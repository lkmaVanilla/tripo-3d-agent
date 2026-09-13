package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/middlewares/skill"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
)

// protocolModel 为固定版本 Eino SDK 提供受控响应，保持真实 Run/Resume 协议路径。
// 本文件不手工构造或解码 Eino 私有检查点格式，模型与检查点存储均在本地。
type protocolModel struct {
	generate func(context.Context, []*schema.Message) (*schema.Message, error)
}

func (m protocolModel) Generate(ctx context.Context, in []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return m.generate(ctx, in)
}

func (m protocolModel) Stream(ctx context.Context, in []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.generate(ctx, in)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

// protocolCheckpointStore 记录 SDK 使用的根检查点键和原始字节，也可模拟保存失败。
// 复制读写缓冲区，避免共享切片让测试绕过真正的保存与恢复过程。
type protocolCheckpointStore struct {
	mu     sync.Mutex
	values map[string][]byte
	keys   []string
	setErr error
}

func (s *protocolCheckpointStore) Set(_ context.Context, key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setErr != nil {
		return s.setErr
	}
	if s.values == nil {
		s.values = make(map[string][]byte)
	}
	s.values[key] = append([]byte(nil), value...)
	s.keys = append(s.keys, key)
	return nil
}

func (s *protocolCheckpointStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.values[key]
	return append([]byte(nil), b...), ok, nil
}

// newProtocolRunner 固定 Agent 名称与执行配置，让测试验证应用实际使用的 SDK 协议。
// legacy 模式额外恢复旧版本的 Skill 中间件配置，供旧检查点夹具使用。
func newProtocolRunner(t *testing.T, m model.BaseChatModel, store adk.CheckPointStore, tools []tool.BaseTool, legacy bool) *adk.Runner {
	t.Helper()
	cfg := &adk.ChatModelAgentConfig{
		Name:          "tripo_asset_agent",
		Description:   "静态道具生产与技术纠偏",
		Instruction:   instruction,
		Model:         m,
		MaxIterations: 20,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{Tools: tools, ExecuteSequentially: true},
			ReturnDirectly:  map[string]bool{"finish_request": true},
		},
	}
	if legacy {
		// b91ab83 使用此中间件；列举 Skill 无副作用，夹具不会加载需要 Service 的 Skill。
		handler, err := skill.NewMiddleware(context.Background(), &skill.Config{Backend: skillBackend{}, UseChinese: true})
		if err != nil {
			t.Fatal(err)
		}
		cfg.Handlers = []adk.ChatModelAgentMiddleware{handler}
	}
	agent, err := adk.NewChatModelAgent(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return adk.NewRunner(context.Background(), adk.RunnerConfig{Agent: agent, CheckPointStore: store})
}

// drainProtocolEvents 消费完整事件流并检查 SDK 错误，防止只观察到中断就误判保存成功。
func drainProtocolEvents(t *testing.T, iter *adk.AsyncIterator[*adk.AgentEvent]) (interrupts int) {
	t.Helper()
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			t.Fatalf("real Eino Runner failed: %v", event.Err)
		}
		if event.Action != nil && event.Action.Interrupted != nil {
			interrupts++
		}
	}
	return interrupts
}

// protocolProposal 保留正文、推理内容、扩展字段和工具调用，用于检测恢复时的协议字段丢失。
func protocolProposal(name, callID string, args any) *schema.Message {
	return &schema.Message{
		Role:             schema.Assistant,
		Content:          "需要先确认这一项。",
		ReasoningContent: "这是已保存提议的完整 reasoning_content，不应在恢复时丢失。",
		Extra:            map[string]any{"provider_trace": "controlled-original-response"},
		ToolCalls: []schema.ToolCall{{
			ID: callID, Type: "function", Function: schema.FunctionCall{Name: name, Arguments: jsonString(args)},
		}},
	}
}

// TestCheckpointProtocolRealRunnerReplayAndResume 通过真实 Eino 验证三段过程：
// 用持久化提议重建暂停、无答案时再次暂停、有答案时携带完整协议继续决策。
// 重建阶段的模型适配器只能返回一次已保存提议，不能调用底层模型或改动根检查点键。
func TestCheckpointProtocolRealRunnerReplayAndResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const key = "protocol-session"
	const callID = "proposal-tool-call"
	const pauseToken = `{"version":1,"pause_id":"pause-1","generation":1}`
	proposal := protocolProposal("ask_user", callID, questionInput{Question: "卡通还是写实？"})
	seed, err := newReplaySeed([]*schema.Message{schema.SystemMessage(instruction), schema.UserMessage("为游戏做一个木箱")}, proposal, "deepseek-v4-pro")
	if err != nil {
		t.Fatal(err)
	}
	// 先做 JSON 序列化往返，避免共享原响应指针掩盖持久化时丢失字段的问题。
	var persisted ReplaySeed
	if err := json.Unmarshal([]byte(jsonString(seed)), &persisted); err != nil {
		t.Fatal(err)
	}
	if err := validateReplaySeed(&persisted); err != nil {
		t.Fatal(err)
	}
	restored := *persisted.Response
	store := &protocolCheckpointStore{}
	modelCalls, freshCalls, resumeCalls := 0, 0, 0
	answer := ""
	ask, err := utils.InferTool("ask_user", "需要关键答案时暂停", func(ctx context.Context, in *questionInput) (string, error) {
		if got := compose.GetToolCallID(ctx); got != callID {
			return "", fmt.Errorf("tool call identity changed: %q", got)
		}
		was, has, state := tool.GetInterruptState[string](ctx)
		if was {
			resumeCalls++
			if !has || state != pauseToken {
				return "", fmt.Errorf("resume state changed: %q (has=%v)", state, has)
			}
			if answer != "" {
				return jsonString(map[string]string{"user_answer": answer}), nil
			}
		} else {
			freshCalls++
		}
		return "", tool.StatefulInterrupt(ctx, in.Question, pauseToken)
	})
	if err != nil {
		t.Fatal(err)
	}
	tools := []tool.BaseTool{ask}
	baseCalls := 0
	adapter := &countedModel{
		coordinator: &pauseCoordinator{mode: "rebuild", seed: &persisted},
		BaseChatModel: protocolModel{generate: func(context.Context, []*schema.Message) (*schema.Message, error) {
			baseCalls++
			return nil, fmt.Errorf("rebuild must not call the provider model")
		}},
	}
	replay := protocolModel{generate: func(ctx context.Context, in []*schema.Message) (*schema.Message, error) {
		modelCalls++
		if modelCalls != 1 {
			return nil, fmt.Errorf("replay model requested more than one proposal")
		}
		if len(in) != 2 || in[0].Role != schema.System || in[1].Role != schema.User {
			return nil, fmt.Errorf("rebuild duplicated system prefix or changed input: %s", jsonString(in))
		}
		return adapter.Generate(ctx, in)
	}}
	runner := newProtocolRunner(t, replay, store, tools, false)
	if got := drainProtocolEvents(t, runner.Run(ctx, []*schema.Message{schema.UserMessage("为游戏做一个木箱")}, adk.WithCheckPointID(key))); got != 1 {
		t.Fatalf("rebuild produced %d interrupts, want 1", got)
	}
	if modelCalls != 1 || freshCalls != 1 || resumeCalls != 0 {
		t.Fatalf("unexpected rebuild calls: model=%d fresh=%d resume=%d", modelCalls, freshCalls, resumeCalls)
	}

	// 换一个 Runner 且不给答案：工具应再次暂停，不能重做模型决策。
	// 即使 WithCheckPointID 给出不同值，Resume 的根 ID 仍决定保存位置。
	runner = newProtocolRunner(t, replay, store, tools, false)
	iter, err := runner.Resume(ctx, key, adk.WithCheckPointID("must-not-be-used"))
	if err != nil {
		t.Fatal(err)
	}
	if got := drainProtocolEvents(t, iter); got != 1 {
		t.Fatalf("unanswered resume produced %d interrupts, want 1", got)
	}
	if modelCalls != 1 || freshCalls != 1 || resumeCalls != 1 {
		t.Fatalf("resume repeated the model decision: model=%d fresh=%d resume=%d", modelCalls, freshCalls, resumeCalls)
	}
	if !reflect.DeepEqual(store.keys, []string{key, key}) {
		t.Fatalf("Runner changed root checkpoint key: %v", store.keys)
	}

	// 仅正常续行阶段允许模型决定下一步；检查 Eino 解码后实际交给模型的协议消息。
	answer = "卡通"
	continuationCalls := 0
	continuation := protocolModel{generate: func(_ context.Context, in []*schema.Message) (*schema.Message, error) {
		continuationCalls++
		var foundProposal, foundAnswer bool
		for _, msg := range in {
			if msg.Role == schema.Assistant && len(msg.ToolCalls) > 0 {
				// Eino 接受响应后添加自己的消息 ID；比较时只剔除此字段，
				// 原模型响应的全部字段（包括其他 Extra）必须保留。
				actual := *msg
				actual.Extra = nil
				for name, value := range msg.Extra {
					if name != "_eino_msg_id" {
						if actual.Extra == nil {
							actual.Extra = make(map[string]any)
						}
						actual.Extra[name] = value
					}
				}
				if !reflect.DeepEqual(&actual, &restored) {
					return nil, fmt.Errorf("checkpoint did not preserve the full assistant response: %s", jsonString(msg))
				}
				foundProposal = true
			}
			if msg.Role == schema.Tool && msg.ToolCallID == callID && strings.Contains(msg.Content, answer) {
				foundAnswer = true
			}
		}
		if !foundProposal || !foundAnswer {
			return nil, fmt.Errorf("resume produced incomplete protocol: proposal=%v answer=%v", foundProposal, foundAnswer)
		}
		return schema.AssistantMessage("已确认卡通风格", nil), nil
	}}
	runner = newProtocolRunner(t, continuation, store, tools, false)
	iter, err = runner.Resume(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if got := drainProtocolEvents(t, iter); got != 0 {
		t.Fatalf("answered resume produced %d interrupts", got)
	}
	if continuationCalls != 1 || freshCalls != 1 || resumeCalls != 2 {
		t.Fatalf("unexpected continuation calls: model=%d fresh=%d resume=%d", continuationCalls, freshCalls, resumeCalls)
	}
	if _, err := adapter.Generate(ctx, persisted.Input); err == nil || !strings.Contains(err.Error(), "recovery_seed_invalid") {
		t.Fatalf("the actual replay adapter must reject a second model request: %v", err)
	}
	if baseCalls != 0 {
		t.Fatalf("rebuild made %d provider model calls", baseCalls)
	}
}

// TestCheckpointProtocolFailedSetStillEmitsInterrupt 固定 SDK 的保存失败行为：
// 写入报错后仍可能出现中断事件，因此应用不能仅凭该事件发布可恢复状态。
func TestCheckpointProtocolFailedSetStillEmitsInterrupt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := &protocolCheckpointStore{setErr: fmt.Errorf("injected checkpoint write failure")}
	ask, err := utils.InferTool("ask_user", "等待一个答案", func(ctx context.Context, in *questionInput) (string, error) {
		return "", tool.StatefulInterrupt(ctx, in.Question, "pending-wait")
	})
	if err != nil {
		t.Fatal(err)
	}
	m := protocolModel{generate: func(context.Context, []*schema.Message) (*schema.Message, error) {
		return protocolProposal("ask_user", "call-before-failed-set", questionInput{Question: "卡通还是写实？"}), nil
	}}
	runner := newProtocolRunner(t, m, store, []tool.BaseTool{ask}, false)
	iter := runner.Run(ctx, []*schema.Message{schema.UserMessage("木箱")}, adk.WithCheckPointID("failed-set-session"))
	sawWriteError, sawInterrupt := false, false
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			if !strings.Contains(event.Err.Error(), "injected checkpoint write failure") {
				t.Fatalf("unexpected Runner error: %v", event.Err)
			}
			sawWriteError = true
		}
		if event.Action != nil && event.Action.Interrupted != nil {
			sawInterrupt = true
		}
	}
	if !sawWriteError || !sawInterrupt || len(store.values) != 0 {
		t.Fatalf("SDK save-failure contract changed: error=%v interrupt=%v saved=%d", sawWriteError, sawInterrupt, len(store.values))
	}
}

// TestCheckpointProtocolRejectsDamagedReplaySeed 验证损坏或不完整的协议材料被明确拒绝，
// 不能让恢复路径靠新的模型调用猜回丢失字段或补全不成对的工具消息。
func TestCheckpointProtocolRejectsDamagedReplaySeed(t *testing.T) {
	input := []*schema.Message{schema.SystemMessage(instruction), schema.UserMessage("木箱")}
	response := protocolProposal("ask_user", "call-1", questionInput{Question: "卡通还是写实？"})
	tests := []struct {
		name   string
		mutate func(*ReplaySeed)
	}{
		{"reasoning_content_lost", func(seed *ReplaySeed) { seed.Response.ReasoningContent = "" }},
		{"tool_call_id_lost", func(seed *ReplaySeed) { seed.Response.ToolCalls[0].ID = "" }},
		{"response_missing", func(seed *ReplaySeed) { seed.Response = nil }},
		{"changed_question", func(seed *ReplaySeed) {
			seed.Response.ToolCalls[0].Function.Arguments = `{"question":"完全不同的问题"}`
		}},
		{"changed_input", func(seed *ReplaySeed) { seed.Input[len(seed.Input)-1].Content = "不同请求" }},
		{"decision_id_missing", func(seed *ReplaySeed) { seed.DecisionID = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			seed, err := newReplaySeed(input, response, "deepseek-v4-pro")
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(seed)
			if err = validateReplaySeed(seed); err == nil || !strings.Contains(err.Error(), "recovery_seed_invalid") {
				t.Fatalf("damaged replay material must fail explicitly, got %v", err)
			}
			adapter := &countedModel{coordinator: &pauseCoordinator{mode: "rebuild", seed: seed}}
			if _, err := adapter.Generate(context.Background(), input); err == nil || !strings.Contains(err.Error(), "recovery_seed_invalid") {
				t.Fatalf("actual replay entry must reject damaged material before external work, got %v", err)
			}
		})
	}

	for name, in := range map[string][]*schema.Message{
		"dangling_tool_call":    {schema.UserMessage("木箱"), response},
		"orphan_tool_response":  {schema.UserMessage("木箱"), schema.ToolMessage("答案", "unknown-call")},
		"next_user_before_tool": {schema.UserMessage("木箱"), response, schema.UserMessage("回答")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newReplaySeed(in, response, "deepseek-v4-pro"); err == nil || !strings.Contains(err.Error(), "recovery_seed_invalid") {
				t.Fatalf("invalid protocol must be rejected before Runner/model invocation, got %v", err)
			}
		})
	}

	// 后续模型决策可能复用工具调用 ID；种子用服务端决策 ID 区分，不能假设提供方 ID 全局唯一。
	first, err := newReplaySeed(input, response, "deepseek-v4-pro")
	if err != nil {
		t.Fatal(err)
	}
	second, err := newReplaySeed(input, response, "deepseek-v4-pro")
	if err != nil {
		t.Fatal(err)
	}
	if first.DecisionID == second.DecisionID || first.ToolCallID != second.ToolCallID {
		t.Fatal("separate accepted decisions were merged solely by provider call ID")
	}

	// 原模型响应本来没有 reasoning_content 时应保留原协议，而不是补造推理内容。
	withoutReasoning := *response
	withoutReasoning.ReasoningContent = ""
	seed, err := newReplaySeed(input, &withoutReasoning, "controlled-non-thinking")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateReplaySeed(seed); err != nil {
		t.Fatalf("non-thinking mode must retain its original protocol without fabricated reasoning: %v", err)
	}
}

const (
	legacyWaiting               = "waiting"
	legacyReadyMissing          = "ready_missing"
	legacyAnswerCleared         = "answer_cleared"
	legacyKnownTask             = "known_task"
	legacyUnknownSubmission     = "unknown_submission"
	legacyKnownTaskNoCheckpoint = "known_task_missing_checkpoint"
)

// legacyCheckpointFixture 同时保存旧数据库与 SDK 生成的暂停身份，供迁移和崩溃测试复用。
type legacyCheckpointFixture struct {
	Dir        string
	Session    Session
	Checkpoint []byte
	Key        string
	PauseState string
	ToolName   string
	ToolCallID string
}

// legacySessionV0 固定 b91ab83 的 JSON 结构，防止 Session 新字段让旧夹具提前变成已迁移数据。
type legacySessionV0 struct {
	ID, Owner, Request, Status                          string
	Intent                                              *Intent
	Question, Answer, WaitID                            string
	Clarifications, Production, ModelCalls              int
	Limits                                              Limits
	Deadline, Created, LastUser, Ended, Expires, Queued time.Time
	HasSlot, CheckpointReady, ResumeRequested           bool
	Current                                             *Operation
	Artifacts                                           []Artifact
	History                                             []*schema.Message
	Final, SelectedArtifact                             string
	Model, Source                                       string
}

// newLegacyCheckpointFixture 只写入 t.TempDir()，由真实 Eino v0.9.19 生成根检查点，
// 使用 b91ab83 的 Agent/Tool 名称和旧字符串状态；不启动当前 Service，也不调用 Provider。
func newLegacyCheckpointFixture(t *testing.T, variant string) legacyCheckpointFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fixture := legacyCheckpointFixture{Dir: t.TempDir(), Key: "legacy-session", PauseState: "legacy-wait", ToolName: "ask_user", ToolCallID: "legacy-call"}
	production := false
	switch variant {
	case legacyWaiting, legacyReadyMissing, legacyAnswerCleared:
	case legacyKnownTask, legacyUnknownSubmission, legacyKnownTaskNoCheckpoint:
		production = true
		fixture.PauseState = "legacy-operation"
		fixture.ToolName = "generate_asset"
	default:
		t.Fatalf("unknown legacy fixture variant: %s", variant)
	}
	now := time.Now().UTC()
	v := legacySessionV0{
		ID: fixture.Key, Owner: "legacy-owner", Request: "为俯视角游戏做一个卡通木箱", Status: "awaiting_answer",
		Question: "木箱采用卡通还是写实风格？", WaitID: fixture.PauseState, Clarifications: 1, ModelCalls: 2,
		Limits:  Limits{Calls: 20, Submissions: 3, Clarifications: 3, Duration: 30 * time.Minute, Idle: 24 * time.Hour, Retention: 7 * 24 * time.Hour},
		Created: now.Add(-time.Minute), LastUser: now, CheckpointReady: true,
		Model: "deepseek-v4-pro", Source: "controlled-test-legacy-b91ab83",
	}
	args := any(questionInput{Question: v.Question})
	if production {
		v.Status, v.Question, v.WaitID, v.Clarifications = "running", "", "", 0
		v.Production, v.ModelCalls, v.HasSlot = 1, 4, true
		v.Deadline = now.Add(29 * time.Minute)
		v.Intent = &Intent{Asset: "木箱", Use: "俯视角游戏原型", Style: "卡通", MaxTriangles: 5000, MaxBytes: 10 << 20, Plan: []string{"生成", "技术检查"}}
		v.Current = &Operation{ID: fixture.PauseState, Kind: "generate", Stage: "submitted", TaskID: "legacy-task", Params: tripo.Params{Prompt: "A stylized wooden crate", FaceLimit: 5000, TextureQuality: "standard"}}
		args = generationInput{Prompt: v.Current.Params.Prompt, TargetTriangles: 5000, TextureQuality: "standard", Reason: "首次生成"}
	}
	proposal := protocolProposal(fixture.ToolName, fixture.ToolCallID, args)
	store := &protocolCheckpointStore{}
	m := protocolModel{generate: func(_ context.Context, in []*schema.Message) (*schema.Message, error) {
		v.History = append(append([]*schema.Message(nil), in...), proposal)
		return proposal, nil
	}}
	pause, err := utils.InferTool(fixture.ToolName, "旧协议暂停夹具", func(ctx context.Context, _ *json.RawMessage) (string, error) {
		return "", tool.StatefulInterrupt(ctx, "旧协议待继续", fixture.PauseState)
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := newProtocolRunner(t, m, store, []tool.BaseTool{pause}, true)
	if got := drainProtocolEvents(t, runner.Run(ctx, []*schema.Message{schema.UserMessage(v.Request)}, adk.WithCheckPointID(fixture.Key))); got != 1 {
		t.Fatalf("legacy fixture produced %d interrupts, want 1", got)
	}
	bytes, ok, err := store.Get(ctx, fixture.Key)
	if err != nil || !ok || len(bytes) == 0 {
		t.Fatalf("legacy checkpoint not generated: exists=%v err=%v", ok, err)
	}
	fixture.Checkpoint = bytes
	switch variant {
	case legacyReadyMissing:
		v.CheckpointReady = false
	case legacyAnswerCleared:
		// 旧 ask_user 在下一次模型输入持久化前清空显示字段，但恢复旧检查点仍需原答案。
		v.Status, v.Question, v.Answer = "understanding", "", ""
	case legacyUnknownSubmission:
		v.Current.Stage, v.Current.TaskID = "submitting", ""
	case legacyKnownTaskNoCheckpoint:
		fixture.Checkpoint = nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixture.Session); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(fixture.Dir, "tripo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// 直接建立 b91ab83 的旧表结构，避免 OpenStore 提前加列，失去检验真实迁移的前提。
	_, err = db.ExecContext(ctx, `PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;
CREATE TABLE sessions(id TEXT PRIMARY KEY, owner TEXT NOT NULL, data BLOB NOT NULL);
CREATE INDEX session_owner ON sessions(owner);
CREATE TABLE events(seq INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE, kind TEXT NOT NULL, at TEXT NOT NULL, data BLOB NOT NULL);
CREATE INDEX event_session ON events(session_id,seq);
CREATE TABLE checkpoints(session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE, key TEXT NOT NULL, data BLOB NOT NULL, PRIMARY KEY(session_id,key));
CREATE TABLE visitors(hash TEXT PRIMARY KEY, expires INTEGER NOT NULL);`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "INSERT INTO sessions(id,owner,data) VALUES(?,?,?)", v.ID, v.Owner, raw); err != nil {
		t.Fatal(err)
	}
	if fixture.Checkpoint != nil {
		if _, err = db.ExecContext(ctx, "INSERT INTO checkpoints(session_id,key,data) VALUES(?,?,?)", v.ID, fixture.Key, fixture.Checkpoint); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

// TestLegacyCheckpointFixturesPreserveOldFailureStates 先验证迁移夹具确实保留旧版本故障形态，
// 再用受限 Runner 恢复到原工具，防止后续迁移测试建立在伪造或已修复的输入上。
func TestLegacyCheckpointFixturesPreserveOldFailureStates(t *testing.T) {
	for _, variant := range []string{legacyWaiting, legacyReadyMissing, legacyAnswerCleared, legacyKnownTask, legacyUnknownSubmission, legacyKnownTaskNoCheckpoint} {
		t.Run(variant, func(t *testing.T) {
			f := newLegacyCheckpointFixture(t, variant)
			db, err := sql.Open("sqlite", filepath.Join(f.Dir, "tripo.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var raw []byte
			if err := db.QueryRow("SELECT data FROM sessions WHERE id=?", f.Key).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"RecoverySchemaVersion", "ResumePoint", "PendingPause", "ReplaySeed", "Answers"} {
				if _, exists := fields[name]; exists {
					t.Fatalf("legacy JSON unexpectedly contains new field %q", name)
				}
			}
			if variant == legacyReadyMissing && (f.Session.CheckpointReady || f.Session.Status != "awaiting_answer" || len(f.Checkpoint) == 0) {
				t.Fatal("fixture did not reproduce checkpoint saved before Ready")
			}
			if variant == legacyAnswerCleared && (f.Session.Question != "" || f.Session.Answer != "" || f.Session.WaitID == "" || f.Session.History[len(f.Session.History)-1].Role != schema.Assistant) {
				t.Fatal("fixture did not reproduce answer erased before durable protocol history")
			}
			if variant == legacyUnknownSubmission && (f.Session.Current.TaskID != "" || f.Session.Current.Stage != "submitting" || f.Session.Production != 1) {
				t.Fatal("fixture did not retain unknown submission and spent budget")
			}
			if variant == legacyKnownTaskNoCheckpoint {
				if f.Session.Current.TaskID == "" || f.Checkpoint != nil {
					t.Fatal("fixture did not retain known task without checkpoint")
				}
				return
			}

			// 禁止模型工作，只通过 SDK Resume 解码，再用公开工具状态核对旧字符串身份。
			// 这证明夹具是 SDK 可读的旧检查点，而非测试自行构造的 JSON 标记。
			resumed := 0
			guard, err := utils.InferTool(f.ToolName, "只检查旧身份并再次暂停", func(ctx context.Context, _ *json.RawMessage) (string, error) {
				was, has, state := tool.GetInterruptState[string](ctx)
				if !was || !has || state != f.PauseState || compose.GetToolCallID(ctx) != f.ToolCallID {
					return "", fmt.Errorf("legacy identity mismatch: was=%v has=%v state=%q", was, has, state)
				}
				resumed++
				return "", tool.StatefulInterrupt(ctx, "已证明旧身份", state)
			})
			if err != nil {
				t.Fatal(err)
			}
			store := &protocolCheckpointStore{values: map[string][]byte{f.Key: f.Checkpoint}}
			blockedModel := protocolModel{generate: func(context.Context, []*schema.Message) (*schema.Message, error) {
				return nil, fmt.Errorf("legacy guard must not call the model")
			}}
			runner := newProtocolRunner(t, blockedModel, store, []tool.BaseTool{guard}, true)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			iter, err := runner.Resume(ctx, f.Key)
			if err != nil {
				t.Fatal(err)
			}
			if got := drainProtocolEvents(t, iter); got != 1 || resumed != 1 {
				t.Fatalf("legacy guard did not reach old tool: interrupts=%d resumed=%d", got, resumed)
			}
		})
	}
}
