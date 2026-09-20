package tripo

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type failedReuseConn struct {
	net.Conn
	fail *atomic.Bool
}

func TestProductionUsesNonReplayingHTTP1Transport(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			t.Errorf("production negotiated replay-capable HTTP/%d", r.ProtoMajor)
		}
		fmt.Fprint(w, `{"code":0,"data":{"task_id":"task-once"}}`)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	c := New("secret")
	c.BaseURL = server.URL
	c.HTTP.Transport.(*http.Transport).TLSClientConfig.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	if _, err := c.Submit(context.Background(), "generate", Params{}); err != nil {
		t.Fatal(err)
	}
}

func (c failedReuseConn) Write(p []byte) (int, error) {
	if strings.HasPrefix(string(p), "POST ") && c.fail.CompareAndSwap(true, false) {
		return 0, syscall.ECONNRESET
	}
	return c.Conn.Write(p)
}

// 复用连接在写入第一个字节前失败也不新建连接重发生产；不能只数 Submit 调用。
func TestProductionConnectionReuseDoesNotReplay(t *testing.T) {
	var requests, connections atomic.Int32
	var fail atomic.Bool
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		io.WriteString(w, `{"code":0,"data":{"balance":1}}`)
	}))
	defer s.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		connections.Add(1)
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return failedReuseConn{conn, &fail}, nil
	}
	c := New("secret")
	c.BaseURL = s.URL
	c.HTTP.Transport = transport
	if _, err := c.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	_, err := c.Submit(context.Background(), "generate", Params{Prompt: "crate"})
	if !IsUnknown(err) || connections.Load() != 1 || requests.Load() != 1 {
		t.Fatalf("production replayed: connections=%d requests=%d err=%v", connections.Load(), requests.Load(), err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBaseURLConfig(t *testing.T) {
	for raw, want := range map[string]string{"": DefaultBaseURL, DefaultBaseURL + "/": DefaultBaseURL, "https://openapi.tripo3d.ai:443/v3": "https://openapi.tripo3d.ai/v3"} {
		got, err := NormalizeBaseURL(raw)
		if err != nil || got != want {
			t.Fatalf("base=%q err=%v", got, err)
		}
	}
	for _, raw := range []string{"http://openapi.tripo3d.com/v3", "https://attacker.example/v3", "https://openapi.tripo3d.com/v2", "https://secret@openapi.tripo3d.com/v3", DefaultBaseURL + "?key=secret", DefaultBaseURL + "#secret", "https://openapi.tripo3d.com:123/v3", DefaultBaseURL + "//", "https://openapi.tripo3d.com/v%33"} {
		if _, err := NewWithBaseURL("secret", raw); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe base accepted or leaked: %v", err)
		}
	}
}

func TestDiagnosticTransportErrors(t *testing.T) {
	for _, tc := range []struct {
		name, category string
		err            error
	}{
		{"dns", "dns", &net.DNSError{Err: "secret", Name: "secret", IsNotFound: true}},
		{"reset", "connect", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}},
		{"refused", "connect", syscall.ECONNREFUSED},
		{"tls", "tls", tls.RecordHeaderError{Msg: "secret"}},
		{"timeout", "timeout", context.DeadlineExceeded},
		{"canceled", "canceled", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New("secret")
			calls := 0
			c.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.GetBody != nil {
					t.Error("replayable body")
				}
				return nil, tc.err
			})
			_, err := c.Submit(context.Background(), "generate", Params{})
			d := Describe(fmt.Errorf("outer: %w", err), "submit")
			if calls != 1 || !IsUnknown(err) || !errors.Is(err, tc.err) || d.Category != tc.category || d.HTTPStatus != nil || !d.RequestStarted || d.OccurredAt.IsZero() {
				t.Fatalf("%+v %v calls=%d", d, err, calls)
			}
			b, _ := json.Marshal(d)
			if strings.Contains(string(b), "secret") || strings.Contains(err.Error(), "secret") {
				t.Fatal("raw cause leaked")
			}
		})
	}
}

func TestDiagnosticResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body, category string
		status               int
		unknown              bool
	}{
		{"http", `{"code":2}`, "http", 401, false},
		{"provider", `{"code":4}`, "provider", 200, false},
		{"server", `{"code":5}`, "http", 503, true},
		{"bad_json", `secret-not-json`, "decode", 502, true},
		{"missing_code", `{"data":{}}`, "protocol", 200, true},
		{"missing_id", `{"code":0,"data":{}}`, "protocol", 200, true},
		{"null", `{"code":0,"data":null}`, "protocol", 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("X-Tripo-Trace-ID", "trace-123")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer s.Close()
			c := New("secret")
			c.BaseURL = s.URL
			_, err := c.Submit(context.Background(), "generate", Params{})
			d := Describe(err, "submit")
			if err == nil || calls != 1 || d.Category != tc.category || IsUnknown(err) != tc.unknown || d.HTTPStatus == nil || *d.HTTPStatus != tc.status || d.ProviderTraceID != "trace-123" {
				t.Fatalf("%+v %v", d, err)
			}
		})
	}
}

func TestProductionRedirectNeverForwarded(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			forwarded, calls := 0, 0
			dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded++ }))
			defer dest.Close()
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.Redirect(w, r, dest.URL, status) }))
			defer s.Close()
			c := New("secret")
			c.BaseURL = s.URL
			c.HTTP = s.Client()
			_, err := c.Submit(context.Background(), "generate", Params{})
			if forwarded != 0 || calls != 1 || !IsUnknown(err) || Describe(err, "submit").Category != "http" {
				t.Fatalf("redirect followed: %d/%d %v", calls, forwarded, err)
			}
		})
	}
}

func TestDiagnosticDownloadReadFailure(t *testing.T) {
	c := New("secret")
	c.DownloadHTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" {
			t.Error("download carried API key")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(&brokenReader{}), ContentLength: -1}, nil
	})
	_, err := c.Download(context.Background(), "https://example.org/model.glb?signature=secret")
	d := Describe(err, "download")
	if err == nil || d.Phase != "download" || d.HTTPStatus == nil || *d.HTTPStatus != 200 || strings.Contains(err.Error(), "secret") || d.OccurredAt.IsZero() {
		t.Fatalf("%+v %v", d, err)
	}
}

type brokenReader struct{}

func (*brokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestSafeDiagnosticDoesNotEchoExternalText(t *testing.T) {
	d := Diagnostic{Phase: "submit", Category: "http", Message: "Bearer secret https://example.org?a=secret file_secret", ProviderTraceID: strings.Repeat("a", 129), CauseCode: "secret"}
	b, _ := json.Marshal(d.Safe("secret"))
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), "https://") || strings.Contains(string(b), strings.Repeat("a", 129)) {
		t.Fatal(string(b))
	}
	d.ProviderTraceID = "prefix-secret"
	if d.Safe("secret").ProviderTraceID != "" {
		t.Fatal("echoed key retained")
	}
	d.Phase, d.Category, d.CauseCode = "submit|query", "dns|connect", "connection_reset|connection_refused"
	d.ProviderTraceID = "trace\ncontrol"
	safe := d.Safe()
	if safe.Phase != "unknown" || safe.Category != "unknown" || safe.CauseCode != "" || safe.ProviderTraceID != "" {
		t.Fatal(safe)
	}
}

func TestDiagnosticDiscardNestedAndOversizedResponse(t *testing.T) {
	for _, size := range []int{64, 3 << 20} {
		c := New("secret-key")
		c.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body := `{"code":400,"message":"sentence https://cdn.example?sign=secret-signed-url Authorization: Bearer secret-key","data":{"nested":{"file_token":"secret-upload-token","text":"` + strings.Repeat("x", size) + `"}}}`
			return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		_, err := c.Submit(context.Background(), "generate", Params{})
		d := Describe(err, "submit")
		b, _ := json.Marshal(d)
		if err == nil || strings.Contains(string(b), "secret-") || strings.Contains(err.Error(), "https://") || len(b) > 1500 {
			t.Fatal("raw response escaped", string(b))
		}
	}
}

func TestPreflightIsReadOnly(t *testing.T) {
	for _, tc := range []struct {
		body   string
		status int
		ok     bool
	}{
		{`{"code":0,"data":{"balance":42}}`, 200, true},
		{`{"code":2,"data":{}}`, 401, false},
		{`{"code":2,"data":{}}`, 200, false},
		{`{"code":0,"data":{}}`, 200, false},
		{`{`, 200, false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" || r.URL.Path != "/v3/account/balance" {
					t.Error("preflight is not read-only")
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer s.Close()
			c := New("secret")
			c.BaseURL = s.URL + "/v3"
			_, err := c.Preflight(context.Background())
			if (err == nil) != tc.ok || calls != 1 {
				t.Fatalf("calls %d error %v", calls, err)
			}
		})
	}
	c := New("secret")
	c.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded })
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := c.Preflight(ctx); err == nil || Describe(err, "preflight").Category != "timeout" {
		t.Fatal(err)
	}
}
