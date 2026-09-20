package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func conversationHTTP(t *testing.T, c *http.Client, method, url string, payload any) (int, []byte) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		body = strings.NewReader(jsonString(payload))
	}
	req, e := http.NewRequest(method, url, body)
	if e != nil {
		t.Fatal(e)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, e := c.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	b, e := io.ReadAll(res.Body)
	if e != nil {
		t.Fatal(e)
	}
	return res.StatusCode, b
}
func conversationHTTPSnapshot(t *testing.T, b []byte) ConversationSnapshot {
	t.Helper()
	var snap ConversationSnapshot
	if e := json.Unmarshal(b, &snap); e != nil {
		t.Fatalf("bad snapshot %s", b)
	}
	return snap
}

func TestConversationHTTPContractsAndIsolation(t *testing.T) {
	ctx := context.Background()
	s := newConversationTestService(t, &conversationProvider{delay: 200 * time.Millisecond})
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	_, _ = conversationHTTP(t, client, "GET", server.URL+"/api/config", nil)
	first := map[string]any{"request": "产品展示木箱", "client_message_id": "creation-key"}
	var wg sync.WaitGroup
	results := make(chan ConversationSnapshot, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, b := conversationHTTP(t, client, "POST", server.URL+"/api/conversations", first)
			if code != 201 {
				t.Errorf("create %d %s", code, b)
				return
			}
			results <- conversationHTTPSnapshot(t, b)
		}()
	}
	wg.Wait()
	close(results)
	var snap ConversationSnapshot
	for got := range results {
		if snap.Conversation.ID != "" && snap.Conversation.ID != got.Conversation.ID {
			t.Fatal("duplicate created twice")
		}
		snap = got
	}
	id, runID := snap.Conversation.ID, snap.Conversation.ActiveRunID
	if id == "" || runID == "" {
		t.Fatal("missing accepted identity")
	}
	base := server.URL + "/api/conversations/" + id
	post := func(path string, body any, want int) []byte {
		t.Helper()
		code, b := conversationHTTP(t, client, "POST", path, body)
		if code != want {
			t.Fatalf("want %d got %d: %s", want, code, b)
		}
		return b
	}
	post(base+"/messages", conversationCommand{Kind: "message", Text: "另一个目标", ClientMessageID: "busy"}, 409)
	post(server.URL+"/api/conversations", map[string]string{"request": "改内容", "client_message_id": "creation-key"}, 409)
	for _, body := range []any{map[string]any{"kind": "message", "text": "", "client_message_id": "empty"}, map[string]any{"kind": "message", "text": strings.Repeat("a", 8001), "client_message_id": "long"}, map[string]any{"kind": "message", "text": "制作", "client_message_id": "import", "file_path": "/tmp/file"}, map[string]any{"kind": "message", "text": "制作", "client_message_id": "many", "version_id": []string{"a", "b"}}, conversationCommand{Kind: "message", Text: "制作", ClientMessageID: "stale", RunID: runID}} {
		post(base+"/messages", body, 400)
	}
	for _, path := range []string{"", "/versions", "/messages", "/trace", "/events", "/versions/any/file"} {
		code, _ := conversationHTTP(t, http.DefaultClient, "GET", base+path, nil)
		if code != 404 {
			t.Fatalf("foreign read %s: %d", path, code)
		}
	}
	req, _ := http.NewRequest("POST", base+"/runs/"+runID+"/stop", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://elsewhere.invalid")
	res, e := client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal("cross origin accepted")
	}
	waitConversationIdle(t, s, id)
	code, b := conversationHTTP(t, client, "GET", base, nil)
	if code != 200 {
		t.Fatal(code, string(b))
	}
	snap = conversationHTTPSnapshot(t, b)
	if len(snap.Versions) != 1 {
		t.Fatal("missing version")
	}
	version := snap.Versions[0]
	other := conversationHTTPSnapshot(t, post(server.URL+"/api/conversations", map[string]string{"request": "另一个展示模型", "client_message_id": "other"}, 201))
	waitConversationIdle(t, s, other.Conversation.ID)
	post(base+"/messages", conversationCommand{Kind: "message", Text: "加工", ClientMessageID: "cross", VersionID: other.Conversation.ID}, 404)
	_, otherBody := conversationHTTP(t, client, "GET", server.URL+"/api/conversations/"+other.Conversation.ID, nil)
	other = conversationHTTPSnapshot(t, otherBody)
	post(base+"/messages", conversationCommand{Kind: "message", Text: "加工", ClientMessageID: "cross-real", VersionID: other.Versions[0].ID}, 404)
	result := post(base+"/messages", conversationCommand{Kind: "message", Text: "解释报告", ClientMessageID: "answer", VersionID: version.ID}, 200)
	answerID := conversationHTTPSnapshot(t, result).Conversation.ActiveRunID
	waitConversationIdle(t, s, id)
	// 响应丢失后即使Run已完成仍回原身份，不再调用模型。
	before, _ := s.store.Get(ctx, answerID)
	post(base+"/messages", conversationCommand{Kind: "message", Text: "解释报告", ClientMessageID: "answer", VersionID: version.ID}, 200)
	after, _ := s.store.Get(ctx, answerID)
	if before.ModelCalls != after.ModelCalls || before.Production != 0 || !validAnswer(after) {
		t.Fatal("idempotent answer changed execution")
	}
	code, b = conversationHTTP(t, client, "GET", base+"/trace", nil)
	if code != 200 {
		t.Fatal(code)
	}
	for _, forbidden := range []string{s.Config.DataDir, "file_saved", "source_url", "History", "PreparedInput"} {
		if strings.Contains(string(b), forbidden) {
			t.Fatalf("private trace value: %s", forbidden)
		}
	}
	exported := conversationHTTPSnapshot(t, b)
	if len(exported.Runs) != 2 || len(exported.Versions) != 1 {
		t.Fatal("trace conflated goals")
	}
	code, b = conversationHTTP(t, client, "GET", server.URL+"/api/sessions/"+runID, nil)
	if code != 200 || !strings.Contains(string(b), `"id":"`+runID+`"`) {
		t.Fatal("old endpoint points at new run")
	}
	legacy := post(server.URL+"/api/sessions", map[string]string{"request": "legacy"}, 201)
	var view map[string]any
	_ = json.Unmarshal(legacy, &view)
	legacyRun, _ := s.store.Get(ctx, view["id"].(string))
	if legacyRun.ExecutionVersion != CurrentPromptVersion {
		t.Fatal("legacy creation changed profile")
	}
	// 旧Run原Expires可以早于续期后的会话；新旧文件入口使用相同会话有效期。
	_, _ = s.store.Edit(ctx, runID, func(v *Session) error { v.Expires = time.Now().Add(-time.Hour); return nil }, "", nil)
	for _, path := range []string{base + "/versions/" + version.ID + "/file", server.URL + "/api/sessions/" + runID + "/artifacts/" + version.ID} {
		code, b = conversationHTTP(t, client, "GET", path, nil)
		if code != 200 || string(b[:4]) != "glTF" {
			t.Fatal("renewed old download unavailable")
		}
	}
	// 下行连接在凭证失效后停止，即使持有会话ID也无法继续读取。
	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/events?after=" + fmtCursor(exported.Cursor)
	socket, _, e := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: client})
	if e != nil {
		t.Fatal(e)
	}
	defer socket.CloseNow()
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var wsSnap ConversationSnapshot
	if e = wsjson.Read(readCtx, socket, &wsSnap); e != nil {
		t.Fatal(e)
	}
	c, _ := s.store.GetConversation(ctx, id)
	if _, e = s.store.db.Exec("UPDATE visitors SET expires=? WHERE hash=?", time.Now().Add(-time.Hour).Unix(), c.Owner); e != nil {
		t.Fatal(e)
	}
	for e == nil {
		e = wsjson.Read(readCtx, socket, &wsSnap)
	}
	if readCtx.Err() != nil {
		t.Fatal("expired visitor websocket stayed open")
	}
}

func fmtCursor(n int64) string { return jsonString(n) }

func TestConversationHTTPAnswersStayOnOriginalRun(t *testing.T) {
	s := newConversationTestService(t, &conversationProvider{})
	s.modelFactory = func(context.Context) (model.BaseChatModel, error) { return conversationBrowserModel{}, nil }
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	code, b := conversationHTTP(t, client, "POST", server.URL+"/api/conversations", map[string]string{"request": "澄清木箱用途", "client_message_id": "first"})
	if code != 201 {
		t.Fatal(code, string(b))
	}
	snap := conversationHTTPSnapshot(t, b)
	v := waitSession(t, s, snap.Conversation.ActiveRunID, func(v Session) bool { return v.Status == "awaiting_answer" })
	cmd := conversationCommand{Kind: "answer", Text: "产品展示，卡通", ClientMessageID: "answer", RunID: v.ID, WaitID: v.WaitID, Generation: v.generation()}
	bad := cmd
	bad.Generation++
	code, _ = conversationHTTP(t, client, "POST", server.URL+"/api/conversations/"+snap.Conversation.ID+"/messages", bad)
	if code != 409 {
		t.Fatal("stale answer accepted")
	}
	for i := 0; i < 2; i++ {
		code, b = conversationHTTP(t, client, "POST", server.URL+"/api/conversations/"+snap.Conversation.ID+"/messages", cmd)
		if code != 200 {
			t.Fatal(code, string(b))
		}
	}
	waitConversationIdle(t, s, snap.Conversation.ID)
	saved, _ := s.store.Get(context.Background(), v.ID)
	if saved.Clarifications != 1 || saved.Status != "completed" {
		t.Fatal("answer created repeated question or wrong run", saved.Final)
	}
	final, _ := s.store.ConversationSnapshot(context.Background(), snap.Conversation.ID, saved.Owner, 0, 0, 50)
	if len(final.Runs) != 1 {
		t.Fatal("answer created new run")
	}
}
