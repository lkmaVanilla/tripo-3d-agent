package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// 不启动调度或清理，文件始终存在；测试必须由归属/生命周期门禁阻断访问。
func expiryHTTPFixture(t *testing.T) (*Service, *httptest.Server, *http.Client, Conversation, Session, Artifact) {
	t.Helper()
	ctx := context.Background()
	s := testService(t, t.TempDir(), &fakeProvider{}, false)
	t.Cleanup(func() { _ = s.Close() })
	token, owner, err := s.store.Visitor(ctx, "", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c, run, err := s.CreateAssetConversation(ctx, owner, "私有历史木箱", "expiry-fixture")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.Config.DataDir, "artifacts", run.ID)
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	a := conversationTestOutput(t, s.store, dir, run, "")
	conversationTestEnd(t, s.store, run.ID)
	run, err = s.store.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(s.Handler())
	t.Cleanup(server.Close)
	jar, _ := cookiejar.New(nil)
	base, _ := url.Parse(server.URL)
	jar.SetCookies(base, []*http.Cookie{{Name: cookieName, Value: token, Path: "/"}})
	return s, server, &http.Client{Jar: jar}, c, run, a
}

func setConversationUnavailable(t *testing.T, s *Service, id, mode string) {
	t.Helper()
	expires, cleaning := time.Now().Add(-time.Hour), false
	if mode == "cleaning" {
		// 即使到期时间还在未来，清理标记也必须先于文件实际删除生效。
		expires, cleaning = time.Now().Add(time.Hour), true
	}
	if _, err := s.store.db.Exec("UPDATE conversations SET expires=?,cleaning=? WHERE id=? AND active_run_id=''", conversationTime(expires), cleaning, id); err != nil {
		t.Fatal(err)
	}
}

func TestConversationExpiredAndCleaningBlockAllHTTPEntrypoints(t *testing.T) {
	for _, mode := range []string{"expired", "cleaning"} {
		t.Run(mode, func(t *testing.T) {
			s, server, client, c, run, artifact := expiryHTTPFixture(t)
			ctx := context.Background()
			conversationBase := server.URL + "/api/conversations/" + c.ID
			sessionBase := server.URL + "/api/sessions/" + run.ID
			original, err := os.ReadFile(artifact.Path)
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{conversationBase + "/versions/" + artifact.ID + "/file", sessionBase + "/artifacts/" + artifact.ID} {
				if code, body := conversationHTTP(t, client, "GET", path, nil); code != 200 || !bytes.Equal(body, original) {
					t.Fatalf("valid fixture download unavailable: %d", code)
				}
			}
			control, _, err := s.CreateAssetConversation(ctx, c.Owner, "仍可访问的另一会话", "unexpired-control")
			if err != nil {
				t.Fatal(err)
			}
			before := jsonString(run)
			setConversationUnavailable(t, s, c.ID, mode)
			requests := []struct {
				method, path string
				payload      any
			}{
				{"GET", conversationBase, nil},
				{"GET", conversationBase + "/messages", nil},
				{"GET", conversationBase + "/versions", nil},
				{"GET", conversationBase + "/trace", nil},
				{"GET", conversationBase + "/versions/" + artifact.ID + "/file?download=1", nil},
				{"GET", sessionBase, nil},
				{"GET", sessionBase + "/trace", nil},
				{"GET", sessionBase + "/artifacts/" + artifact.ID + "?download=1", nil},
				{"POST", conversationBase + "/messages", conversationCommand{Kind: "message", Text: "继续加工", ClientMessageID: "blocked-new", VersionID: artifact.ID}},
				{"POST", conversationBase + "/messages", conversationCommand{Kind: "answer", Text: "澄清答案", ClientMessageID: "blocked-answer", RunID: run.ID, WaitID: "old-wait", Generation: 1}},
				{"POST", conversationBase + "/runs/" + run.ID + "/stop", map[string]any{}},
				{"POST", conversationBase + "/runs/" + run.ID + "/retry", map[string]any{}},
				{"POST", sessionBase + "/answer", map[string]string{"answer": "旧入口答案"}},
				{"POST", sessionBase + "/stop", map[string]any{}},
				{"POST", sessionBase + "/retry", map[string]any{}},
			}
			for _, request := range requests {
				code, body := conversationHTTP(t, client, request.method, request.path, request.payload)
				if code != 404 || strings.Contains(string(body), c.Title) || bytes.Contains(body, original) {
					t.Errorf("%s %s exposed unavailable conversation: %d %s", request.method, request.path, code, body)
				}
			}
			for _, path := range []string{"/api/conversations", "/api/sessions"} {
				code, body := conversationHTTP(t, client, "GET", server.URL+path, nil)
				if code != 200 || strings.Contains(string(body), c.ID) || !strings.Contains(string(body), control.ID) {
					t.Errorf("%s failed to filter only unavailable records: %d %s", path, code, body)
				}
			}
			for _, base := range []string{conversationBase, sessionBase} {
				conn, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/events", &websocket.DialOptions{HTTPClient: client})
				if conn != nil {
					_ = conn.CloseNow()
				}
				if err == nil || response == nil || response.StatusCode != 404 {
					t.Errorf("websocket accepted unavailable conversation: response=%v err=%v", response, err)
				}
			}
			if !s.store.ValidVisitor(ctx, c.Owner) {
				t.Fatal("test accidentally expired the visitor instead of the conversation")
			}
			if code, _ := conversationHTTP(t, client, "GET", server.URL+"/api/conversations/"+control.ID, nil); code != 200 {
				t.Fatal("unrelated conversation was blocked")
			}
			after, err := s.store.Get(ctx, run.ID)
			if err != nil || jsonString(after) != before {
				t.Fatal("rejected commands changed old execution")
			}
			if got, err := os.ReadFile(artifact.Path); err != nil || !bytes.Equal(got, original) {
				t.Fatal("test did not leave the protected model file in place")
			}
		})
	}
}

func TestConversationExistingSocketsCloseOnExpiryOrCleanup(t *testing.T) {
	for _, mode := range []string{"expired", "cleaning"} {
		for _, protocol := range []string{"conversations", "sessions"} {
			t.Run(mode+"/"+protocol, func(t *testing.T) {
				s, server, client, c, run, _ := expiryHTTPFixture(t)
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				defer cancel()
				conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/api/"+protocol+"/"+c.ID+"/events", &websocket.DialOptions{HTTPClient: client})
				if err != nil {
					t.Fatal(err)
				}
				defer conn.CloseNow()
				var snapshot map[string]any
				if err = wsjson.Read(ctx, conn, &snapshot); err != nil {
					t.Fatal(err)
				}
				setConversationUnavailable(t, s, c.ID, mode)
				const privateMarker = "MUST_NOT_BE_STREAMED_AFTER_UNAVAILABLE"
				if _, err = s.store.Edit(context.Background(), run.ID, nil, "expiry_test_evidence", map[string]string{"text": privateMarker}); err != nil {
					t.Fatal(err)
				}
				// 已缓冲的旧快照可以读完，但新持久化证据不得在失效后继续下发。
				for {
					err = wsjson.Read(ctx, conn, &snapshot)
					if err != nil {
						break
					}
					if strings.Contains(jsonString(snapshot), privateMarker) {
						t.Fatal("socket streamed new evidence after conversation became unavailable")
					}
				}
				// 服务端生命周期取消可能先关闭TCP而未发关闭帧；EOF同样证明已断开。
				closed := errors.Is(err, io.EOF) || websocket.CloseStatus(err) == websocket.StatusNormalClosure
				if ctx.Err() != nil || !closed {
					t.Fatalf("unavailable conversation socket did not close promptly: %v", err)
				}
			})
		}
	}
}
