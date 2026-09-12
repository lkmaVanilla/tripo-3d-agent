package tripo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProtocol(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Error("missing auth")
		}
		if r.Method == "POST" {
			calls++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if r.URL.Path == "/generation/text-to-model" {
				if body["model"] != Model || body["quad"] != false || body["face_limit"] != float64(5000) {
					t.Errorf("bad generation parameters: %+v", body)
				}
			} else if r.URL.Path == "/mesh/decimate" {
				if body["model"] != "v2.0" || body["input"] != "task_original" {
					t.Errorf("bad decimation: %+v", body)
				}
			}
			io.WriteString(w, `{"code":0,"data":{"task_id":"task_123"}}`)
			return
		}
		if r.URL.Path != "/tasks/task_123" {
			t.Error(r.URL.Path)
		}
		io.WriteString(w, `{"code":0,"data":{"task_id":"task_123","status":"success","progress":100,"output":{"model_url":"https://example.org/model.glb"}}}`)
	}))
	defer server.Close()
	c := New("test")
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	for _, kind := range []string{"generate", "decimate"} {
		id, err := c.Submit(context.Background(), kind, Params{Prompt: "wooden crate", FaceLimit: 5000, TextureQuality: "standard", Input: "task_original"})
		if err != nil || id != "task_123" {
			t.Fatalf("submit %s %v", id, err)
		}
	}
	result, err := c.Query(context.Background(), "task_123")
	if err != nil || result.Status != "success" || calls != 2 {
		t.Fatalf("query %+v %v", result, err)
	}
}
func TestUnknownSubmitNeverRetries(t *testing.T) {
	for _, response := range []string{`{`, `{"code":0,"data":{}}`} {
		count := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count++; io.WriteString(w, response) }))
		c := New("test")
		c.BaseURL = server.URL
		c.HTTP = server.Client()
		_, err := c.Submit(context.Background(), "generate", Params{})
		server.Close()
		if !IsUnknown(err) || count != 1 {
			t.Fatalf("unknown=%v count=%d", err, count)
		}
	}
}
func TestDownloadRejectsPrivateResource(t *testing.T) {
	c := New("test")
	for _, u := range []string{"http://127.0.0.1/model.glb", "https://127.0.0.1/model.glb", "file:///etc/passwd"} {
		_, err := c.Download(context.Background(), u)
		if err == nil {
			t.Fatal("accepted private/local url", u)
		}
		if strings.Contains(err.Error(), "test") {
			t.Fatal("leaked credential")
		}
	}
}
