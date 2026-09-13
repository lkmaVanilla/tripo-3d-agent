package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/testfixture"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
)

// conversationProvider 返回不同规模的真实GLB，上传绑定原字节，不冒充视觉验收。
type conversationProvider struct {
	mu             sync.Mutex
	params         map[string]tripo.Params
	uploads        [][]byte
	unknown        bool
	failUploads    bool
	delay          time.Duration
	overGeneration bool
}

func (p *conversationProvider) Submit(_ context.Context, kind string, params tripo.Params) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.params == nil {
		p.params = map[string]tripo.Params{}
	}
	id := fmt.Sprintf("task_%d", len(p.params)+1)
	p.params[id] = params
	if p.unknown {
		return "", &tripo.APIError{Unknown: true}
	}
	return id, nil
}
func (p *conversationProvider) Query(ctx context.Context, id string) (tripo.Task, error) {
	if p.delay > 0 {
		if err := wait(ctx, p.delay); err != nil {
			return tripo.Task{}, err
		}
	}
	t := tripo.Task{ID: id, Status: "success", Progress: 100}
	t.Output.ModelURL = "https://fixture.example/" + id
	return t, nil
}
func (p *conversationProvider) Download(_ context.Context, url string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := strings.TrimPrefix(url, "https://fixture.example/")
	params := p.params[id]
	faces := params.FaceLimit
	if p.overGeneration && params.Input == "" {
		faces = 6000
	}
	if faces < 1 {
		faces = 4500
	}
	return testfixture.Cube(faces), nil
}
func (p *conversationProvider) UploadModel(_ context.Context, b []byte) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.uploads = append(p.uploads, append([]byte(nil), b...))
	if p.failUploads {
		return "", fmt.Errorf("controlled upload failure")
	}
	return fmt.Sprintf("file_%d", len(p.uploads)), nil
}

type conversationScript struct{}

func (conversationScript) Generate(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	var state struct {
		Request    string        `json:"request"`
		Intent     *Intent       `json:"intent"`
		Artifacts  []Artifact    `json:"artifacts"`
		Input      *AssetVersion `json:"input_version"`
		Assessment *struct {
			Passed bool `json:"passed"`
		} `json:"input_assessment"`
	}
	for _, msg := range in {
		if i := strings.LastIndex(msg.Content, "<runtime_state>"); i >= 0 {
			raw := strings.Split(msg.Content[i+15:], "</runtime_state>")[0]
			if err := json.Unmarshal([]byte(raw), &state); err != nil {
				return nil, err
			}
		}
	}
	call := func(name string, args any) (*schema.Message, error) {
		return &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: newID(), Type: "function", Function: schema.FunctionCall{Name: name, Arguments: jsonString(args)}}}}, nil
	}
	if strings.Contains(state.Request, "解释") {
		return call("finish_request", conversationFinishInput{Outcome: "answer", Explanation: "依据版本实测报告解释技术数据，未进行视觉检查。"})
	}
	if state.Intent == nil {
		action, faces := "generate", 4500
		if state.Input != nil {
			action, faces = "decimate", 3000
			if strings.Contains(state.Request, "2000") {
				faces = 2000
			}
		}
		return call("set_intent", conversationIntentInput{Action: action, Intent: Intent{Asset: "木箱", Use: "产品展示", MaxTriangles: faces, MaxBytes: 10 << 20, Plan: []string{"按明确输入制作并技术检查"}}})
	}
	if state.Assessment != nil && state.Assessment.Passed && len(state.Artifacts) == 0 {
		return call("finish_request", conversationFinishInput{Outcome: "answer", Explanation: "现有输入满足本次技术要求，无需加工。"})
	}
	if len(state.Artifacts) > 0 {
		a := state.Artifacts[len(state.Artifacts)-1]
		return call("finish_request", conversationFinishInput{Outcome: testConversationOutcome(a.Report.Passed), ArtifactID: a.ID, Explanation: "按实测报告决定交付。"})
	}
	if state.Input != nil {
		return call("decimate_asset", decimationInput{ArtifactID: state.Input.ID, TargetTriangles: state.Intent.MaxTriangles, Reason: "用户明确要求降低面数"})
	}
	return call("generate_asset", generationInput{Prompt: "A wooden crate for product presentation", TargetTriangles: state.Intent.MaxTriangles, Reason: "用户要求生成"})
}
func (m conversationScript) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	r, e := m.Generate(ctx, in, opts...)
	if e != nil {
		return nil, e
	}
	return schema.StreamReaderFromArray([]*schema.Message{r}), nil
}

func newConversationTestService(t *testing.T, p *conversationProvider) *Service {
	t.Helper()
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.PollInterval = 5 * time.Millisecond
	s, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	s.provider = p
	s.ready = true
	s.source = "controlled-conversation"
	s.modelFactory = func(context.Context) (model.BaseChatModel, error) { return conversationScript{}, nil }
	if e = s.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func waitConversationIdle(t *testing.T, s *Service, id string) {
	t.Helper()
	until := time.Now().Add(15 * time.Second)
	for time.Now().Before(until) {
		c, e := s.store.GetConversation(context.Background(), id)
		if e != nil {
			t.Fatal(e)
		}
		if c.ActiveRunID == "" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("conversation remained busy")
}

func TestConversationRuntimeLineageAndAnswer(t *testing.T) {
	ctx := context.Background()
	p := &conversationProvider{}
	s := newConversationTestService(t, p)
	c, v, e := s.CreateAssetConversation(ctx, "owner", "产品展示木箱", "first")
	if e != nil {
		t.Fatal(e)
	}
	first := waitSession(t, s, v.ID, func(v Session) bool { return v.Terminal() })
	if first.Status != "completed" {
		t.Fatalf("first: %s", first.Final)
	}
	waitConversationIdle(t, s, c.ID)
	a := first.Artifacts[0]
	for i, text := range []string{"减面至3000", "减面至2000"} {
		v, e = s.ContinueConversation(ctx, c.ID, "owner", text, fmt.Sprintf("next%d", i), a.ID)
		if e != nil {
			t.Fatal(e)
		}
		done := waitSession(t, s, v.ID, func(v Session) bool { return v.Terminal() })
		if done.Status != "completed" {
			events, _ := s.store.Events(ctx, v.ID, 0)
			t.Fatalf("followup: %s %s", done.Final, jsonString(events))
		}
		waitConversationIdle(t, s, c.ID)
	}
	snap, e := s.store.ConversationSnapshot(ctx, c.ID, "owner", 0, 0, 50)
	if e != nil {
		t.Fatal(e)
	}
	if len(snap.Versions) != 3 {
		t.Fatalf("versions=%d", len(snap.Versions))
	}
	for _, ver := range snap.Versions[1:] {
		if ver.ParentVersionID != a.ID {
			t.Fatal("wrong parent")
		}
	}
	v, e = s.ContinueConversation(ctx, c.ID, "owner", "解释这个版本的报告", "explain", a.ID)
	if e != nil {
		t.Fatal(e)
	}
	answer := waitSession(t, s, v.ID, func(v Session) bool { return v.Terminal() })
	if !validAnswer(answer) || answer.Production != 0 || !resultMatchesEvidence(answer) {
		t.Fatalf("invalid answer outcome: %s", jsonString(conversationRunView(answer)))
	}
	old, e := s.store.Get(ctx, first.ID)
	if e != nil || old.Final != first.Final || old.Intent.MaxTriangles != 4500 {
		t.Fatal("historical result changed")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.params) != 3 || len(p.uploads) != 2 {
		t.Fatal("incorrect production/upload count")
	}
	if string(p.uploads[0]) != string(p.uploads[1]) {
		t.Fatal("second branch used wrong version bytes")
	}
}

func testConversationOutcome(deliver bool) string {
	if deliver {
		return "delivery"
	}
	return "ended"
}
