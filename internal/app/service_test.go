package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino-ext/components/model/deepseek"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/testfixture"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
)

// scriptModel 按运行状态和已有工具记录返回固定决策，驱动真实 Eino 工具循环。
// 它用于验证应用编排、恢复和预算，不评估真实模型的意图理解或纠偏决策能力。
type scriptModel struct{ clarify bool }

func (m scriptModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	var state struct {
		Intent     *Intent    `json:"intent"`
		Artifacts  []Artifact `json:"artifacts"`
		Production int        `json:"production"`
	}
	for _, msg := range in {
		if i := strings.LastIndex(msg.Content, "<runtime_state>"); i >= 0 {
			body := strings.Split(msg.Content[i+len("<runtime_state>"):], "</runtime_state>")[0]
			if err := json.Unmarshal([]byte(body), &state); err != nil {
				return nil, err
			}
		}
	}
	seen := func(name, skillName string) bool {
		for _, msg := range in {
			for _, call := range msg.ToolCalls {
				if call.Function.Name == name && (skillName == "" || strings.Contains(call.Function.Arguments, skillName)) {
					return true
				}
			}
		}
		return false
	}
	call := func(name string, args any) (*schema.Message, error) {
		return &schema.Message{Role: schema.Assistant, ReasoningContent: "测试协议状态，恢复时应完整保留", ToolCalls: []schema.ToolCall{{ID: newID(), Type: "function", Function: schema.FunctionCall{Name: name, Arguments: jsonString(args)}}}}, nil
	}
	if state.Intent == nil {
		if !seen("skill", "intent") {
			return call("skill", map[string]string{"skill": "intent"})
		}
		if m.clarify && !seen("ask_user", "") {
			return call("ask_user", questionInput{Question: "木箱采用卡通还是写实风格？"})
		}
		return call("set_intent", Intent{Asset: "木箱", Use: "俯视角游戏原型", Style: "卡通", Constraints: []string{"静态道具", "卡通木箱"}, MaxTriangles: 5000, MaxBytes: 10 << 20, Assumptions: []string{"默认5000三角面和10 MiB"}, Plan: []string{"生成低模木箱", "检查GLB、面数和体积", "必要时纠偏后交付"}})
	}
	if len(state.Artifacts) == 0 {
		if !seen("skill", "generation") {
			return call("skill", map[string]string{"skill": "generation"})
		}
		return call("generate_asset", generationInput{Prompt: "A stylized low-poly wooden crate for a top-down game prototype", TargetTriangles: 5000, TextureQuality: "standard", Reason: "按已确认需求首次生成"})
	}
	a := state.Artifacts[len(state.Artifacts)-1]
	if a.Report.Passed {
		return call("finish_request", finishInput{Deliver: true, ArtifactID: a.ID, Explanation: "已依据实测报告通过文件有效性、面数和体积检查。"})
	}
	if state.Production >= 3 {
		return call("finish_request", finishInput{Explanation: "生产次数已耗尽，产物仍不满足面数要求。"})
	}
	if !seen("skill", "correction") {
		return call("skill", map[string]string{"skill": "correction"})
	}
	return call("decimate_asset", decimationInput{ArtifactID: a.ID, TargetTriangles: 4000, Reason: "有效模型实测面数超过5000，选择减面"})
}
func (m scriptModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

// fakeProvider 在内存中模拟异步任务；blocked 保持任务运行，over 让生成结果超面数，
// alwaysOver 让纠偏也失败，unknown 模拟无法确认提交结果。锁保护并发会话共用的计数。
type fakeProvider struct {
	mu                     sync.Mutex
	count                  int
	kinds                  map[string]string
	over, blocked, unknown bool
	alwaysOver             bool
}

func (p *fakeProvider) Submit(ctx context.Context, kind string, params tripo.Params) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.count++
	if p.unknown {
		return "", &tripo.APIError{Unknown: true}
	}
	if p.kinds == nil {
		p.kinds = map[string]string{}
	}
	id := fmt.Sprintf("task_%d", p.count)
	p.kinds[id] = kind
	return id, nil
}
func (p *fakeProvider) Query(ctx context.Context, id string) (tripo.Task, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	task := tripo.Task{ID: id, Status: "success", Progress: 100}
	if p.blocked {
		task.Status = "running"
		task.Progress = 40
	}
	task.Output.ModelURL = "https://fixture.example/" + id + ".glb"
	return task, nil
}
func (p *fakeProvider) Download(ctx context.Context, raw string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := strings.TrimSuffix(strings.TrimPrefix(raw, "https://fixture.example/"), ".glb")
	if p.alwaysOver || (p.over && p.kinds[id] == "generate") {
		return testfixture.Cube(6000), nil
	}
	return testfixture.Cube(12), nil
}
func (p *fakeProvider) Count() int { p.mu.Lock(); defer p.mu.Unlock(); return p.count }
func (p *fakeProvider) Unblock()   { p.mu.Lock(); p.blocked = false; p.mu.Unlock() }

// testService 保留真实持久化、Eino Runtime 和应用工具，仅替换模型与 Tripo Provider。
// 返回前不启动调度器，便于用例先配置额度或写入边界状态；关闭责任由调用方承担。
func testService(t *testing.T, dir string, p *fakeProvider, clarify bool) *Service {
	t.Helper()
	c := DefaultConfig()
	c.DataDir = dir
	c.PollInterval = 10 * time.Millisecond
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	s.provider = p
	s.ready = true
	s.source = "controlled-test"
	s.modelFactory = func(context.Context) (model.BaseChatModel, error) { return scriptModel{clarify: clarify}, nil }
	return s
}

// waitSession 以持久化状态作为异步完成依据；提前终止时输出追踪，避免单纯超时掩盖原因。
func waitSession(t *testing.T, s *Service, id string, predicate func(Session) bool) Session {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		v, err := s.store.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if predicate(v) {
			return v
		}
		if v.Terminal() {
			events, _ := s.store.Events(context.Background(), id, 0)
			t.Fatalf("unexpected terminal state %s: %s\nevents=%s", v.Status, v.Final, jsonString(events))
		}
		time.Sleep(20 * time.Millisecond)
	}
	v, _ := s.store.Get(context.Background(), id)
	t.Fatalf("timed out: %+v", v)
	return v
}

// TestEinoEndToEndCorrectionAndClarification 验收澄清、生成、实测超限、减面和交付的完整受控链路。
// 决策来自脚本模型，产物来自合成 GLB，因此通过不等于真实 Agent 评测或视觉验收通过。
func TestEinoEndToEndCorrectionAndClarification(t *testing.T) {
	p := &fakeProvider{over: true}
	s := testService(t, t.TempDir(), p, true)
	s.Start()
	defer s.Close()
	v, err := s.Create(context.Background(), "visitor-a", "给我的俯视角游戏原型做一个低模木箱。")
	if err != nil {
		t.Fatal(err)
	}
	waitSession(t, s, v.ID, func(v Session) bool { return v.Status == "awaiting_answer" && (v.ResumePoint != nil) })
	if err = s.Answer(context.Background(), v.ID, "卡通风格"); err != nil {
		t.Fatal(err)
	}
	v = waitSession(t, s, v.ID, func(v Session) bool { return v.Terminal() })
	if v.Status != "completed" || v.Production != 2 || p.Count() != 2 || v.Clarifications != 1 || len(v.Artifacts) != 2 {
		t.Fatalf("bad outcome: %+v", v)
	}
	if v.Artifacts[0].Report.Triangles != 6000 || v.Artifacts[1].Report.Triangles != 12 {
		t.Fatalf("not measuring actual geometry: %+v", v.Artifacts)
	}
	events, _ := s.store.Events(context.Background(), v.ID, 0)
	found := map[string]bool{}
	for _, event := range events {
		found[event.Kind] = true
	}
	for _, kind := range []string{"skill_loaded", "agent_proposal", "runtime_accepted", "tool_submitted", "technical_report", "agent_finished", "checkpoint_committed"} {
		if !found[kind] {
			t.Errorf("missing trace event %s", kind)
		}
	}
}

// TestRestartReusesKnownTask 验证服务正常关闭后重启沿用远端任务 ID 和生产期限，
// 后续模型可继续决策，但已提交生产不能重做。
func TestRestartReusesKnownTask(t *testing.T) {
	dir := t.TempDir()
	p := &fakeProvider{blocked: true}
	s := testService(t, dir, p, false)
	s.Start()
	v, err := s.Create(context.Background(), "owner", "卡通木箱，GLB，5000面，10 MiB")
	if err != nil {
		t.Fatal(err)
	}
	v = waitSession(t, s, v.ID, func(v Session) bool { return v.Current != nil && v.Current.TaskID != "" })
	calls := v.ModelCalls
	deadline := v.Deadline
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	p.Unblock()
	s = testService(t, dir, p, false)
	s.Start()
	defer s.Close()
	v = waitSession(t, s, v.ID, func(v Session) bool { return v.Terminal() })
	if v.Status != "completed" || p.Count() != 1 || v.Production != 1 || v.ModelCalls <= calls || !v.Deadline.Equal(deadline) {
		t.Fatalf("bad recovery: %+v count=%d", v, p.Count())
	}
}

// TestRestartClarification 验证已发布的问题跨服务重启仍能接受答案并恢复 Eino，
// 同一问题不增加第二次澄清计数。
func TestRestartClarification(t *testing.T) {
	dir := t.TempDir()
	p := &fakeProvider{}
	s := testService(t, dir, p, true)
	s.Start()
	v, _ := s.Create(context.Background(), "owner", "木箱")
	v = waitSession(t, s, v.ID, func(v Session) bool { return v.Status == "awaiting_answer" && (v.ResumePoint != nil) })
	calls := v.ModelCalls
	_ = s.Close()
	s = testService(t, dir, p, true)
	s.Start()
	defer s.Close()
	if err := s.Answer(context.Background(), v.ID, "卡通"); err != nil {
		t.Fatal(err)
	}
	v = waitSession(t, s, v.ID, func(v Session) bool { return v.Terminal() })
	if v.Status != "completed" || v.Clarifications != 1 || v.ModelCalls <= calls {
		t.Fatalf("bad clarification recovery %+v", v)
	}
}

// TestUnknownSubmissionStops 验证未知提交结果沿完整服务链路收敛为明确失败，且只提交一次。
func TestUnknownSubmissionStops(t *testing.T) {
	p := &fakeProvider{unknown: true}
	s := testService(t, t.TempDir(), p, false)
	s.Start()
	defer s.Close()
	v, _ := s.Create(context.Background(), "owner", "木箱")
	v = waitSession(t, s, v.ID, func(v Session) bool { return v.Terminal() })
	if v.Status != "failed" || v.Production != 1 || p.Count() != 1 || !strings.Contains(v.Final, "未知") {
		t.Fatalf("bad unknown handling %+v", v)
	}
}

// TestModelBudgetIsPersistent 验证模型调用上限记录在会话中，触顶后不继续进入生产。
func TestModelBudgetIsPersistent(t *testing.T) {
	p := &fakeProvider{}
	s := testService(t, t.TempDir(), p, false)
	s.Config.MaxCalls = 2
	s.Start()
	defer s.Close()
	v, _ := s.Create(context.Background(), "owner", "木箱")
	v = waitSession(t, s, v.ID, func(v Session) bool { return v.Terminal() })
	if v.ModelCalls != 2 || p.Count() != 0 || v.Status != "failed" {
		t.Fatalf("bad model budget %+v", v)
	}
}

// TestStopDoesNotResubmit 在远端任务运行时停止会话，验证后台调度不会重新提交或占住槽位。
func TestStopDoesNotResubmit(t *testing.T) {
	p := &fakeProvider{blocked: true}
	s := testService(t, t.TempDir(), p, false)
	s.Start()
	defer s.Close()
	v, _ := s.Create(context.Background(), "owner", "木箱")
	waitSession(t, s, v.ID, func(v Session) bool { return v.Current != nil && v.Current.TaskID != "" })
	if err := s.Stop(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	v, _ = s.store.Get(context.Background(), v.ID)
	if v.Status != "stopped" || v.HasSlot || p.Count() != 1 {
		t.Fatalf("bad stop %+v", v)
	}
}

// TestAnonymousIsolation 用有无访问者 Cookie 的两个客户端验证会话与追踪隔离，
// 并确认即使携带所属 Cookie，跨站修改请求仍被拒绝。
func TestAnonymousIsolation(t *testing.T) {
	s := testService(t, t.TempDir(), &fakeProvider{}, false)
	defer s.Close()
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	a := &http.Client{Jar: jar}
	res, err := a.Post(server.URL+"/api/sessions", "application/json", strings.NewReader(`{"request":"木箱"}`))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	_ = json.NewDecoder(res.Body).Decode(&v)
	res.Body.Close()
	if res.StatusCode != 201 {
		t.Fatal(v)
	}
	id := v["id"].(string)
	for _, path := range []string{"", "/trace", "/events", "/artifacts/random"} {
		res, err = http.Get(server.URL + "/api/sessions/" + id + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 404 {
			t.Fatalf("foreign visitor accessed %s: %d", path, res.StatusCode)
		}
	}
	res, err = a.Get(server.URL + "/api/sessions/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal("owner lost access")
	}
	req, _ := http.NewRequest("POST", server.URL+"/api/sessions/"+id+"/stop", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://attacker.example")
	res, err = a.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal("cross-site mutation accepted")
	}
}

// TestProductionBudgetAndConcurrentQueue 分别验收生产次数上限和并发容量边界，
// 等待队列不提前消费次数或期限，槽位释放后应只放行最早请求。
func TestProductionBudgetAndConcurrentQueue(t *testing.T) {
	t.Run("three_attempts", func(t *testing.T) {
		p := &fakeProvider{alwaysOver: true}
		s := testService(t, t.TempDir(), p, false)
		s.Start()
		defer s.Close()
		v, _ := s.Create(context.Background(), "owner", "木箱")
		v = waitSession(t, s, v.ID, func(v Session) bool { return v.Terminal() })
		if v.Status != "failed" || v.Production != 3 || p.Count() != 3 {
			t.Fatalf("bad production budget %+v", v)
		}
	})
	t.Run("three_slots_ten_queue", func(t *testing.T) {
		// 固定远端任务为运行中，使 14 个请求稳定分成 3 个生产、10 个等待和 1 个队满。
		p := &fakeProvider{blocked: true}
		s := testService(t, t.TempDir(), p, false)
		s.Start()
		defer s.Close()
		for i := 0; i < 14; i++ {
			if _, err := s.Create(context.Background(), "owner", fmt.Sprintf("木箱%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		var queued []Session
		var running []Session
		full := 0
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			all, _ := s.store.List(context.Background(), "")
			queued = nil
			running = nil
			full = 0
			for _, v := range all {
				if v.Status == "queued" && (v.ResumePoint != nil) {
					queued = append(queued, v)
				}
				if v.HasSlot && v.Current != nil && v.Current.TaskID != "" {
					running = append(running, v)
				}
				if v.Status == "queue_full" {
					full++
				}
			}
			if len(queued) == 10 && len(running) == 3 && full == 1 {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		if len(queued) != 10 || len(running) != 3 || full != 1 || p.Count() != 3 {
			t.Fatalf("slots=%d queued=%d full=%d submits=%d", len(running), len(queued), full, p.Count())
		}
		for _, v := range queued {
			if v.Production != 0 || !v.Deadline.IsZero() {
				t.Fatal("queue consumes production or deadline")
			}
		}
		sort.Slice(queued, func(i, j int) bool { return queued[i].Queued.Before(queued[j].Queued) })
		if err := s.Stop(context.Background(), running[0].ID); err != nil {
			t.Fatal(err)
		}
		waitSession(t, s, queued[0].ID, func(v Session) bool { return v.Current.TaskID != "" })
		if p.Count() != 4 {
			t.Fatal("FIFO should start exactly one request")
		}
	})
}

// TestDeepSeekProtocolWithEinoResume 使用真实 DeepSeek SDK 和 Eino，对接本地 HTTP 响应器，
// 核对模型选项、调用计数及恢复后 reasoning_content 的传递；不会请求真实 DeepSeek。
func TestDeepSeekProtocolWithEinoResume(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	historyWithReasoning := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string            `json:"model"`
			Messages []*schema.Message `json:"messages"`
			Tools    []any             `json:"tools"`
			Thinking struct {
				Type string `json:"type"`
			} `json:"thinking"`
			Effort string `json:"reasoning_effort"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "bad JSON", 400)
			return
		}
		if body.Model != "deepseek-v4-pro" || body.Thinking.Type != "enabled" || body.Effort != "high" || len(body.Tools) == 0 {
			t.Errorf("missing model options: %+v", body)
		}
		mu.Lock()
		calls++
		for _, msg := range body.Messages {
			if msg.Role == schema.Assistant {
				if msg.ReasoningContent == "" {
					t.Error("lost assistant reasoning_content")
				}
				historyWithReasoning = true
			}
		}
		mu.Unlock()
		msg, err := (scriptModel{clarify: true}).Generate(r.Context(), body.Messages)
		if err != nil {
			t.Error(err)
			http.Error(w, "test response error", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "test-completion", "model": "deepseek-v4-pro", "choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}})
	}))
	defer server.Close()
	s := testService(t, t.TempDir(), &fakeProvider{}, true)
	s.modelFactory = func(ctx context.Context) (model.BaseChatModel, error) {
		return deepseek.NewChatModel(ctx, &deepseek.ChatModelConfig{APIKey: "test-key", BaseURL: server.URL, HTTPClient: server.Client(), Model: "deepseek-v4-pro", ThinkingConfig: &deepseek.ThinkingConfig{Type: "enabled"}})
	}
	s.Start()
	defer s.Close()
	v, _ := s.Create(context.Background(), "owner", "木箱")
	waitSession(t, s, v.ID, func(v Session) bool { return v.Status == "awaiting_answer" && (v.ResumePoint != nil) })
	if err := s.Answer(context.Background(), v.ID, "卡通"); err != nil {
		t.Fatal(err)
	}
	v = waitSession(t, s, v.ID, func(v Session) bool { return v.Terminal() })
	mu.Lock()
	defer mu.Unlock()
	if v.Status != "completed" || calls != v.ModelCalls || !historyWithReasoning {
		t.Fatalf("protocol or call count mismatch: %s calls=%d persisted=%d", v.Final, calls, v.ModelCalls)
	}
}

// TestExpiryAndTraceRedaction 验证闲置过期、保留期清理以及公开追踪中的凭证和签名脱敏。
func TestExpiryAndTraceRedaction(t *testing.T) {
	s := testService(t, t.TempDir(), &fakeProvider{}, false)
	defer s.Close()
	s.Config.DeepSeekKey = "secret-test-api-key"
	v, _ := s.Create(context.Background(), "private-owner", "木箱")
	_, _ = s.store.Edit(context.Background(), v.ID, func(v *Session) error { v.LastUser = time.Now().Add(-25 * time.Hour); return nil }, "test", map[string]string{"error": "secret-test-api-key", "input": "https://cdn.example/model.glb?signature=private"})
	s.schedule()
	v, _ = s.store.Get(context.Background(), v.ID)
	if !v.Terminal() || v.HasSlot {
		t.Fatal("idle request did not expire")
	}
	events, _ := s.store.Events(context.Background(), v.ID, 0)
	data := jsonString(s.sanitize(s.snapshot(v, events)))
	for _, private := range []string{"secret-test-api-key", "private-owner", "signature=private"} {
		if strings.Contains(data, private) {
			t.Fatalf("trace leaked %s", private)
		}
	}
	_, _ = s.store.Edit(context.Background(), v.ID, func(v *Session) error { v.Expires = time.Now().Add(-time.Second); return nil }, "", nil)
	s.schedule()
	if _, err := s.store.Get(context.Background(), v.ID); err == nil {
		t.Fatal("expired session data retained")
	}
}

// TestBrowserHarness 是需显式启用的浏览器夹具服务，cmd/server 不会启动它。
// 服务标识明确注明受控测试来源，页面操作结果不能当作真实 Provider 的验证证据。
func TestBrowserHarness(t *testing.T) {
	if os.Getenv("TRIPO_BROWSER_TEST") != "1" {
		t.Skip("opt-in browser fixture")
	}
	s := testService(t, t.TempDir(), &fakeProvider{over: true}, true)
	s.Start()
	listener, err := net.Listen("tcp", "127.0.0.1:48089")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(s.Handler())
	server.Listener = listener
	server.Start()
	defer func() { s.Close(); server.Close() }()
	t.Log("CONTROLLED TEST FIXTURE ONLY: " + server.URL)
	select {
	case <-time.After(5 * time.Minute):
	case <-s.ctx.Done():
	}
}

// TestMissingCredentials 验证未配置凭证时页面仍可读取服务就绪状态，
// 但创建请求会被拒绝，不能留下会话或触发生产副作用。
func TestMissingCredentials(t *testing.T) {
	c := DefaultConfig()
	c.DataDir = t.TempDir()
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := &fakeProvider{}
	s.provider = p
	s.Start()
	if _, err = s.Create(context.Background(), "visitor", "木箱"); err == nil {
		t.Fatal("accepted production without credentials")
	}
	all, _ := s.store.List(context.Background(), "")
	if len(all) != 0 || p.Count() != 0 {
		t.Fatal("missing credentials caused side effects")
	}
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/api/config", nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"ready":false`) {
		t.Fatalf("missing readiness: %s", response.Body.String())
	}
}

// batchedModel 故意在一次响应中返回两个工具调用，用于验证单步执行约束。
type batchedModel struct{ scriptModel }

func (m batchedModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	msg, err := m.scriptModel.Generate(ctx, in, opts...)
	if err == nil {
		second := msg.ToolCalls[0]
		second.ID = newID()
		msg.ToolCalls = append(msg.ToolCalls, second)
	}
	return msg, err
}

// TestBatchedToolsBlocked 验证批量工具提议在任何工具执行前被拒绝，不产生远端提交。
func TestBatchedToolsBlocked(t *testing.T) {
	p := &fakeProvider{}
	s := testService(t, t.TempDir(), p, false)
	s.modelFactory = func(context.Context) (model.BaseChatModel, error) { return batchedModel{}, nil }
	s.Start()
	defer s.Close()
	v, _ := s.Create(context.Background(), "visitor", "木箱")
	v = waitSession(t, s, v.ID, func(v Session) bool { return v.Terminal() })
	if v.ModelCalls != 1 || v.Status != "failed" || p.Count() != 0 {
		t.Fatalf("batch executed: %+v", v)
	}
}
