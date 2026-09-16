package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/asset"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/testfixture"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
)

// 可复用的确定性决策驱动真实 Runtime；不把它计作真实模型评测。
type optionalScript struct{}

func (optionalScript) Generate(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	var state struct {
		Intent     *Intent       `json:"intent"`
		Input      *AssetVersion `json:"input_version"`
		Artifacts  []Artifact    `json:"artifacts"`
		Production int           `json:"production"`
	}
	start := strings.LastIndex(in[0].Content, "<runtime_state>")
	if err := json.Unmarshal([]byte(strings.Split(in[0].Content[start+15:], "</runtime_state>")[0]), &state); err != nil {
		return nil, err
	}
	call := func(name string, args any) (*schema.Message, error) {
		return protocolProposal(name, newID(), args), nil
	}
	if state.Intent == nil {
		action := "generate"
		f := optionalIntentFields{Asset: "木箱", Use: "产品展示", Constraints: []string{}, Plan: []string{"制作并测量实际指标"}, MaxTriangles: &LimitChange{Mode: "clear"}, MaxBytes: &LimitChange{Mode: "clear"}}
		if state.Input != nil {
			action = "decimate"
			f.ReductionMode = "further"
		}
		return call("set_intent", optionalIntentInput{Action: action, Intent: f})
	}
	if len(state.Artifacts) > 0 {
		a := state.Artifacts[len(state.Artifacts)-1]
		passed := a.Report.Passed
		if state.Input != nil {
			passed = passed && a.Report.Triangles < state.Input.Report.Triangles
		}
		return call("finish_request", conversationFinishInput{Outcome: testConversationOutcome(passed), ArtifactID: a.ID, Explanation: "按实际文件技术检查与加工结果结束。"})
	}
	if state.Production > 0 {
		return call("finish_request", conversationFinishInput{Outcome: "ended", Explanation: "未取得可交付文件，记录操作失败。"})
	}
	if state.Input != nil {
		target := state.Input.Report.Triangles / 2
		if state.Input.Report.Triangles <= 500 {
			return call("finish_request", conversationFinishInput{Outcome: "answer", Explanation: "当前工具目标最少500面，输入没有可行减面空间。"})
		}
		if target < 500 {
			target = 500
		}
		if target > 20000 {
			target = 20000
		}
		return call("decimate_asset", decimationInput{ArtifactID: state.Input.ID, TargetTriangles: target, Reason: "按用户进一步减面目标选择较低参数"})
	}
	return call("generate_asset", generationInput{Prompt: "A wooden crate for product display", TargetTriangles: 8000, Reason: "制作参数选择，不作为用户验收上限"})
}
func (m optionalScript) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	r, e := m.Generate(ctx, in, opts...)
	if e != nil {
		return nil, e
	}
	return schema.StreamReaderFromArray([]*schema.Message{r}), nil
}

func TestOptionalConversationGenerationAndReduction(t *testing.T) {
	ctx := context.Background()
	s := newConversationTestService(t, &conversationProvider{})
	s.modelFactory = func(context.Context) (model.BaseChatModel, error) { return optionalScript{}, nil }
	c, run, e := s.CreateAssetConversation(ctx, "owner", "做产品展示木箱，不限制面数和体积", "one")
	if e != nil {
		t.Fatal(e)
	}
	first := waitSession(t, s, run.ID, func(v Session) bool { return v.Terminal() })
	waitConversationIdle(t, s, c.ID)
	if first.Status != "completed" || first.Production != 1 || first.Intent.Optional == nil || first.Intent.Optional.MaxTriangles != nil || first.Artifacts[0].Report.Triangles != 8000 {
		t.Fatalf("first: %s", jsonString(first.View()))
	}
	old := jsonString(first.Artifacts[0].Report)
	next, e := s.ContinueConversation(ctx, c.ID, "owner", "继续减面，不限制绝对面数和体积", "two", first.Artifacts[0].ID)
	if e != nil {
		t.Fatal(e)
	}
	after := waitSession(t, s, next.ID, func(v Session) bool { return v.Terminal() })
	waitConversationIdle(t, s, c.ID)
	if after.Status != "completed" || after.Production != 1 || after.Artifacts[0].Report.Triangles != 4000 {
		t.Fatalf("next: %s", jsonString(after.View()))
	}
	snap, e := s.store.ConversationSnapshot(ctx, c.ID, "owner", 0, 0, 100)
	if e != nil || len(snap.Versions) != 2 || snap.Versions[1].ParentVersionID != first.Artifacts[0].ID || jsonString(snap.Versions[0].Report) != old {
		t.Fatal("version lineage/history", e)
	}
	if !strings.Contains(jsonString(snap), `"max_triangles":null`) || strings.Contains(after.Final, "上限 0") {
		t.Fatal("projection lost optional values")
	}
	assertStoreEventCount(t, s.store, after.ID, "runtime_blocked", 0)
}

func TestOptionalHistoricalInheritanceClearAndFreeze(t *testing.T) {
	ctx := context.Background()
	s, v, input := conversationInputFixture(t)
	defer s.Close()
	v = editProductionFixture(t, s, v.ID, func(v *Session) {
		v.ExecutionVersion = OptionalPromptVersion
		v.Intent = nil
		v.Current = nil
		v.GoalKind = ""
		v.HasSlot = false
	})
	invoke := func(args optionalIntentInput) string {
		all, e := s.tools(v.ID, &pauseCoordinator{mode: "normal"})
		if e != nil {
			t.Fatal(e)
		}
		for _, x := range all {
			info, _ := x.Info(ctx)
			if info.Name == "set_intent" {
				r, e := x.(tool.InvokableTool).InvokableRun(ctx, jsonString(args))
				if e != nil {
					t.Fatal(e)
				}
				return r
			}
		}
		t.Fatal("missing intent tool")
		return ""
	}
	args := optionalIntentInput{Action: "decimate", Intent: optionalIntentFields{MaxTriangles: &LimitChange{Mode: "clear"}, Plan: []string{"进一步减面"}, ReductionMode: "further"}}
	invoke(args)
	saved := getProductionFixture(t, s, v.ID)
	if saved.Intent == nil || saved.Intent.Optional.MaxTriangles != nil || saved.Intent.Optional.MaxBytes == nil || *saved.Intent.Optional.MaxBytes != input.SourceIntent.MaxBytes || saved.Intent.ConstraintSources["max_bytes"].Kind != "legacy_inherited" {
		t.Fatalf("inheritance lost: %s", jsonString(saved))
	}
	if saved.IntentDraft == nil || saved.InputAssessment == nil || !saved.InputAssessment.Passed || inputSatisfiesGoal(saved) {
		t.Fatal("missing draft or wrong reduction gate")
	}
	before := jsonString(saved.Intent)
	args.Intent.MaxBytes = &LimitChange{Mode: "clear"}
	invoke(args)
	if jsonString(getProductionFixture(t, s, v.ID).Intent) != before {
		t.Fatal("accepted intent changed")
	}
	assertStoreEventCount(t, s.store, v.ID, "intent_review", 1)
	assertStoreEventCount(t, s.store, v.ID, "intent_and_plan", 1)
}

func TestOptionalKnownTaskRecoveryPreservesLimits(t *testing.T) {
	s, v, p := productionRecoveryFixture(t, "submitted")
	defer s.Close()
	intent, _ := normalizeOptionalIntent(v, optionalIntentInput{Action: "generate", Intent: optionalIntentFields{Asset: "木箱", Plan: []string{"生成"}}})
	v = editProductionFixture(t, s, v.ID, func(v *Session) {
		v.ExecutionVersion = OptionalPromptVersion
		v.Intent = &intent
		v.GoalKind = "generate"
		v.Current.Params.FaceLimit = 8000
	})
	deadline := v.Deadline
	p.download = func(context.Context) ([]byte, error) { return testfixture.Cube(8000), nil }
	if e := s.recoverKnownTask(context.Background(), v.ID); e != nil {
		t.Fatal(e)
	}
	after := getProductionFixture(t, s, v.ID)
	if p.submits != 0 || after.Production != 1 || !after.Deadline.Equal(deadline) || !after.Artifacts[0].Report.Passed || after.Intent.Optional.MaxTriangles != nil {
		t.Fatalf("recovery changed contract: %s", jsonString(after))
	}
}

func TestOptionalProductionSeedUsesSavedParameterRules(t *testing.T) {
	v := Session{ID: "run", ExecutionVersion: OptionalPromptVersion, Model: "controlled"}
	intent, _ := normalizeOptionalIntent(v, optionalIntentInput{Action: "generate", Intent: optionalIntentFields{Asset: "木箱", Plan: []string{"生成"}}})
	v.Intent = &intent
	seed, e := newReplaySeed([]*schema.Message{schema.UserMessage("木箱")}, protocolProposal("generate_asset", "call", generationInput{Prompt: "crate", TargetTriangles: 8000, Reason: "制作选择"}), v.Model, OptionalPromptVersion)
	if e != nil {
		t.Fatal(e)
	}
	_, params, _, e := seedProduction(v, seed)
	if e != nil || params.FaceLimit != 8000 {
		t.Fatal(params, e)
	}
	bound := 5000
	v.Intent.Optional = &asset.AcceptanceLimits{MaxTriangles: &bound}
	if _, _, _, e = seedProduction(v, seed); e == nil {
		t.Fatal("rebuild ignored changed bound")
	}
}

// 正常关闭再打开验证真实 SQLite 序列化；新旧协议不做批量重写。
func TestOptionalReopenMixedRecords(t *testing.T) {
	dir := t.TempDir()
	s := testService(t, dir, &fakeProvider{}, false)
	ctx := context.Background()
	old := s.newRun("owner", "旧请求", ConversationPromptVersion)
	old.Intent = &Intent{Asset: "旧模型", MaxTriangles: 5000, MaxBytes: 10 << 20}
	old.Finish("failed", "fixture")
	fresh := s.newRun("owner", "新请求", OptionalPromptVersion)
	in, _ := normalizeOptionalIntent(fresh, optionalIntentInput{Action: "generate", Intent: optionalIntentFields{Asset: "新模型", Plan: []string{"制作"}}})
	fresh.Intent = &in
	fresh.Finish("failed", "fixture")
	for _, v := range []Session{old, fresh} {
		if e := s.store.Create(ctx, v); e != nil {
			t.Fatal(e)
		}
	}
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	// 在关闭后的隔离备份副本打开；原数据文件保持不变。
	backup := t.TempDir()
	original, e := os.ReadFile(filepath.Join(dir, "tripo.db"))
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(backup, "tripo.db"), original, 0600); e != nil {
		t.Fatal(e)
	}
	s = testService(t, backup, &fakeProvider{}, false)
	defer s.Close()
	unchanged, e := os.ReadFile(filepath.Join(dir, "tripo.db"))
	if e != nil || string(unchanged) != string(original) {
		t.Fatal("backup validation modified original")
	}
	for _, v := range []Session{old, fresh} {
		after, e := s.store.Get(ctx, v.ID)
		if e != nil || jsonString(after.Intent) != jsonString(v.Intent) || !after.Ended.Equal(v.Ended) || !after.Expires.Equal(v.Expires) {
			t.Fatal("record changed", e)
		}
	}
}

func TestOptionalDownloadLimitExplanation(t *testing.T) {
	s, v, p := productionRecoveryFixture(t, "submitted")
	defer s.Close()
	in, _ := normalizeOptionalIntent(v, optionalIntentInput{Action: "generate", Intent: optionalIntentFields{Asset: "木箱", Plan: []string{"生成"}}})
	v = editProductionFixture(t, s, v.ID, func(v *Session) { v.ExecutionVersion = OptionalPromptVersion; v.Intent = &in; v.GoalKind = "generate" })
	p.download = func(context.Context) ([]byte, error) { return nil, tripo.ErrDownloadLimit }
	if _, e := s.production(context.Background(), v.ID, v.Current.ID); e != nil {
		t.Fatal(e)
	}
	after := getProductionFixture(t, s, v.ID)
	if after.Current.ErrorCode != "download_resource_limit" || len(after.Artifacts) != 0 {
		t.Fatal("resource failure lost typed evidence")
	}
	evidence := conversationFailureEvidence(*after.Current)
	if evidence["cause"] != "system_download_limit" {
		t.Fatal(evidence)
	}
	after.Finish("failed", "fixture")
	if !strings.Contains(after.View()["final"].(string), "系统 150 MiB") {
		t.Fatal("system protection missing from formal result", after.View())
	}
}
