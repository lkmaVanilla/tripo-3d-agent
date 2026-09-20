package app

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
)

func TestConversationCorruptInputNeverUploads(t *testing.T) {
	s, v, input := conversationInputFixture(t)
	defer s.Close()
	p := &conversationProvider{}
	s.provider = p
	if !s.store.versionUsable(input) {
		t.Fatal("valid saved file not processable")
	}
	b, e := os.ReadFile(input.Path)
	if e != nil {
		t.Fatal(e)
	}
	b[0] = 'X'
	if e = os.WriteFile(input.Path, b, 0600); e != nil {
		t.Fatal(e)
	}
	// 相同长度损坏也应让公开状态变为不可处理；生产再次独立核对完整摘要。
	snap, e := s.store.ConversationSnapshot(context.Background(), input.ConversationID, "owner", 0, 0, 50)
	if e != nil {
		t.Fatal(e)
	}
	if snap.Versions[0].Processable {
		t.Fatal("corrupt file advertised as processable")
	}
	result, e := s.production(context.Background(), v.ID, v.Current.ID)
	if e != nil || !strings.Contains(result, "input_preparation_failed") {
		t.Fatal("input rejection not explained", e, result)
	}
	saved, _ := s.store.Get(context.Background(), v.ID)
	if saved.Production != 0 || len(saved.Artifacts) != 0 || len(p.uploads) != 0 || len(p.params) != 0 {
		t.Fatal("corrupt input caused side effect")
	}
}

type delayedUploadProvider struct {
	*conversationProvider
	entered, release chan struct{}
}

func (p *delayedUploadProvider) UploadModel(ctx context.Context, b []byte) (string, error) {
	close(p.entered)
	<-p.release
	return p.conversationProvider.UploadModel(ctx, b)
}

func TestConversationStopWaitsForLateUploadExit(t *testing.T) {
	ctx := context.Background()
	p := &conversationProvider{}
	s := newConversationTestService(t, p)
	gate := &delayedUploadProvider{conversationProvider: p, entered: make(chan struct{}), release: make(chan struct{})}
	s.provider = gate
	c, first, e := s.CreateAssetConversation(ctx, "owner", "木箱", "first")
	if e != nil {
		t.Fatal(e)
	}
	first = waitSession(t, s, first.ID, func(v Session) bool { return v.Terminal() })
	waitConversationIdle(t, s, c.ID)
	next, e := s.ContinueConversation(ctx, c.ID, "owner", "减面3000", "edit", first.Artifacts[0].ID)
	if e != nil {
		t.Fatal(e)
	}
	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no upload")
	}
	if e = s.Stop(ctx, next.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.ContinueConversation(ctx, c.ID, "owner", "解释", "still-draining", first.Artifacts[0].ID); e != ErrConversationBusy {
		t.Fatal("accepted during exit", e)
	}
	current, _ := s.store.GetConversation(ctx, c.ID)
	if current.ActiveRunID != next.ID {
		t.Fatal("released before worker exit")
	}
	close(gate.release)
	waitConversationIdle(t, s, c.ID)
	stopped, _ := s.store.Get(ctx, next.ID)
	if stopped.Status != "stopped" || stopped.Production != 0 || len(stopped.Artifacts) != 0 {
		t.Fatal("late upload resurrected old run")
	}
	p.mu.Lock()
	count := len(p.params)
	p.mu.Unlock()
	if count != 1 {
		t.Fatal("late upload submitted production")
	}
	newRun, e := s.ContinueConversation(ctx, c.ID, "owner", "解释", "next", first.Artifacts[0].ID)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.store.ReleaseConversationRun(ctx, next.ID); e != nil {
		t.Fatal(e)
	}
	current, _ = s.store.GetConversation(ctx, c.ID)
	if current.ActiveRunID != newRun.ID {
		t.Fatal("late release removed next run")
	}
}

func TestConversationEmptyModelResponseIsModelFailure(t *testing.T) {
	for _, nilMessage := range []bool{false, true} {
		s := newConversationTestService(t, &conversationProvider{})
		s.modelFactory = func(context.Context) (model.BaseChatModel, error) {
			return protocolModel{generate: func(context.Context, []*schema.Message) (*schema.Message, error) {
				if nilMessage {
					return nil, nil
				}
				return &schema.Message{Role: schema.Assistant}, nil
			}}, nil
		}
		_, v, e := s.CreateAssetConversation(context.Background(), "owner", "解释能力", "first")
		if e != nil {
			t.Fatal(e)
		}
		v = waitSession(t, s, v.ID, func(v Session) bool { return v.Terminal() })
		if v.Result == nil || v.Result.Reason != "model_failed" || v.Production != 0 || v.ModelCalls != 1 {
			t.Fatalf("empty response misclassified %s", jsonString(v.Result))
		}
	}
}

// 保证接口断言随Production Provider适配扩展仍然成立。
var _ tripo.FileUploader = (*delayedUploadProvider)(nil)

func TestConversationProviderInputNeverExposed(t *testing.T) {
	s := &Service{}
	v := map[string]any{"input": "用户原始输入", "params": tripo.Params{Input: "opaque-supplier-secret-without-prefix", FaceLimit: 3000}}
	visible := jsonString(s.sanitize(v))
	if strings.Contains(visible, "opaque-supplier-secret") || !strings.Contains(visible, "用户原始输入") {
		t.Fatal("provider reference leak or user input lost")
	}
}

func TestConversationCorrectionKeepsRealParentWithinRun(t *testing.T) {
	ctx := context.Background()
	s := newConversationTestService(t, &conversationProvider{overGeneration: true})
	s.modelFactory = func(context.Context) (model.BaseChatModel, error) {
		return protocolModel{generate: func(ctx context.Context, in []*schema.Message) (*schema.Message, error) {
			var state struct {
				Artifacts []Artifact `json:"artifacts"`
			}
			for _, msg := range in {
				if i := strings.LastIndex(msg.Content, "<runtime_state>"); i >= 0 {
					_ = json.Unmarshal([]byte(strings.Split(msg.Content[i+15:], "</runtime_state>")[0]), &state)
				}
			}
			if len(state.Artifacts) > 0 {
				a := state.Artifacts[len(state.Artifacts)-1]
				if !a.Report.Passed {
					return protocolProposal("decimate_asset", newID(), decimationInput{ArtifactID: a.ID, TargetTriangles: 3500, Reason: "实测超限，使用本Run候选纠偏"}), nil
				}
			}
			return (conversationScript{}).Generate(ctx, in)
		}}, nil
	}
	c, v, e := s.CreateAssetConversation(ctx, "owner", "产品展示木箱", "first")
	if e != nil {
		t.Fatal(e)
	}
	v = waitSession(t, s, v.ID, func(v Session) bool { return v.Terminal() })
	if v.Status != "completed" || v.Production != 2 || len(v.Artifacts) != 2 {
		t.Fatalf("v3 correction failed %s", v.Final)
	}
	snap, e := s.store.ConversationSnapshot(ctx, c.ID, "owner", 0, 0, 50)
	if e != nil {
		t.Fatal(e)
	}
	if len(snap.Versions) != 2 || snap.Versions[1].ParentVersionID != snap.Versions[0].ID || snap.Versions[0].Report.Passed || !snap.Versions[1].Report.Passed {
		t.Fatal("candidate provenance/report changed")
	}
}

func TestConversationContextWindowPreservesExplicitOldVersion(t *testing.T) {
	s, prior, input := conversationInputFixture(t)
	defer s.Close()
	ctx := context.Background()
	conversationTestEnd(t, s.store, prior.ID)
	for i := 0; i < 12; i++ {
		v := s.newRun("owner", strings.Repeat("最近对话", 300), ConversationPromptVersion)
		v, _, e := s.store.AppendConversationRun(ctx, input.ConversationID, "owner", newID(), "", v)
		if e != nil {
			t.Fatal(e)
		}
		_, e = s.store.Edit(ctx, v.ID, func(x *Session) error { return x.finishAnswer(strings.Repeat("有来源的说明", 600)) }, "agent_finished", nil)
		if e != nil {
			t.Fatal(e)
		}
		if e = s.store.ReleaseConversationRun(ctx, v.ID); e != nil {
			t.Fatal(e)
		}
	}
	v, e := s.ContinueConversation(ctx, input.ConversationID, "owner", "引用最早版本减面至2000", "last", input.ID)
	if e != nil {
		t.Fatal(e)
	}
	if v.InputVersion == nil || v.InputVersion.ID != input.ID || v.InputVersion.Report.Triangles != 4500 || v.ConversationContext["input_intent"] == nil {
		t.Fatal("old explicit facts lost outside window")
	}
	b, _ := json.Marshal(v.ConversationContext)
	var state struct {
		Messages []struct {
			Text string `json:"text"`
		} `json:"messages"`
		Summaries []any `json:"summaries"`
		Truncated bool  `json:"truncated"`
	}
	_ = json.Unmarshal(b, &state)
	size := 0
	for _, m := range state.Messages {
		size += len([]rune(m.Text))
	}
	if len(state.Messages) > 20 || len(state.Summaries) != 5 || size > 12000 || !state.Truncated {
		t.Fatalf("unbounded context: messages=%d summaries=%d chars=%d truncated=%v", len(state.Messages), len(state.Summaries), size, state.Truncated)
	}
	old, e := s.store.GetAssetVersion(ctx, input.ConversationID, input.ID)
	if e != nil || old.Report.MaxTriangles != input.Report.MaxTriangles {
		t.Fatal("old report rewritten")
	}
}

func TestConversationIdleFailureRecoversWithoutRevivingRun(t *testing.T) {
	s := testService(t, t.TempDir(), &fakeProvider{}, false)
	defer s.Close()
	ctx := context.Background()
	c, v, e := s.CreateAssetConversation(ctx, "owner", "目标", "first")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Stop(ctx, v.ID); e != nil {
		t.Fatal(e)
	}
	_, e = s.store.db.Exec(`CREATE TEMP TRIGGER idle_event_failure BEFORE INSERT ON events WHEN NEW.kind='conversation_idle' BEGIN SELECT RAISE(ABORT,'controlled idle event failure'); END`)
	if e != nil {
		t.Fatal(e)
	}
	s.schedule()
	busy, _ := s.store.GetConversation(ctx, c.ID)
	if busy.ActiveRunID != v.ID {
		t.Fatal("failed idle event detached active identity")
	}
	_, e = s.store.db.Exec("DROP TRIGGER idle_event_failure")
	if e != nil {
		t.Fatal(e)
	}
	// 下一次调度重试同一释放，不运行已停止的Runner，不消耗模型或生产。
	s.schedule()
	idle, _ := s.store.GetConversation(ctx, c.ID)
	saved, _ := s.store.Get(ctx, v.ID)
	if idle.ActiveRunID != "" || saved.ModelCalls != 0 || saved.Production != 0 || saved.Status != "stopped" {
		t.Fatal("idle retry revived stopped run")
	}
	assertStoreEventCount(t, s.store, v.ID, "conversation_idle", 1)
}
