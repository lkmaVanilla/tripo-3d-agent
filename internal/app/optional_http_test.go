package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestOptionalHTTPWebSocketAndExport(t *testing.T) {
	ctx := context.Background()
	s := newConversationTestService(t, &conversationProvider{delay: 100 * time.Millisecond})
	s.modelFactory = func(context.Context) (model.BaseChatModel, error) { return optionalScript{}, nil }
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	code, b := conversationHTTP(t, client, "POST", server.URL+"/api/conversations", map[string]string{"request": "木箱，不限制面数和体积", "client_message_id": "optional"})
	if code != 201 {
		t.Fatal(code, string(b))
	}
	snap := conversationHTTPSnapshot(t, b)
	base := server.URL + "/api/conversations/" + snap.Conversation.ID
	// 运行中订阅，检查实时数据；再按游标重连检查持久化投影。
	check := func(snap ConversationSnapshot) {
		t.Helper()
		if len(snap.Runs) != 1 || len(snap.Versions) != 1 {
			t.Fatal("missing run/version")
		}
		raw := jsonString(snap)
		if !strings.Contains(raw, `"max_triangles":null`) || !strings.Contains(raw, `"max_bytes":null`) || !strings.Contains(raw, `"status":"not_applicable"`) {
			t.Fatal("optional values lost", raw)
		}
		if snap.Versions[0].Report.Triangles != 8000 || !snap.Versions[0].Report.Passed {
			t.Fatal("unexpected report")
		}
	}
	for _, cursor := range []int64{0, -1} {
		if cursor < 0 {
			cursor = snap.Cursor
		}
		conn, _, e := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/events?after="+fmtCursor(cursor), &websocket.DialOptions{HTTPClient: client})
		if e != nil {
			t.Fatal(e)
		}
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		for {
			var next ConversationSnapshot
			e = wsjson.Read(readCtx, conn, &next)
			if e != nil {
				cancel()
				conn.CloseNow()
				t.Fatal(e, jsonString(snap))
			}
			snap = next
			if len(snap.Versions) == 1 && snap.Conversation.ActiveRunID == "" {
				break
			}
		}
		cancel()
		conn.CloseNow()
		check(snap)
	}
	for _, path := range []string{"", "/trace"} {
		code, b = conversationHTTP(t, client, "GET", base+path, nil)
		if code != 200 {
			t.Fatal(code)
		}
		check(conversationHTTPSnapshot(t, b))
	}
	code, b = conversationHTTP(t, client, "GET", base+"/versions", nil)
	if code != 200 || !strings.Contains(string(b), `"max_triangles":null`) {
		t.Fatal("version projection", code, string(b))
	}
	runID := snap.Runs[0]["id"].(string)
	for _, path := range []string{"", "/trace"} {
		code, b = conversationHTTP(t, client, "GET", server.URL+"/api/sessions/"+runID+path, nil)
		var raw any
		if code != 200 || json.Unmarshal(b, &raw) != nil || !strings.Contains(string(b), `"max_bytes":null`) {
			t.Fatal("legacy route projection", code, string(b))
		}
	}
}
