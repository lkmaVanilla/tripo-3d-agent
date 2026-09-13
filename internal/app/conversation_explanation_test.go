package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/asset"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/testfixture"
)

// 用模型实际收到的上下文核对：旧版报告、本目标检查和需求假设不能互相替代。
// 脚本响应仅用于观察消息，不把这项测试当作真实模型已理解事实边界的证明。
func TestConversationModelContextSeparatesTargetsFromEvidence(t *testing.T) {
	ctx := context.Background()
	s := testService(t, t.TempDir(), &fakeProvider{}, false)
	defer s.Close()
	v := s.newRun("owner", "将已引用模型减到2000面", ConversationPromptVersion)
	v.GoalKind = "decimate"
	v.Intent = &Intent{Asset: "茶壶", Use: "产品展示", MaxTriangles: 2000, MaxBytes: 10 << 20,
		Assumptions: []string{"目标2000面不代表实测通过"}}
	data := testfixture.Cube(4500)
	oldReport := asset.Inspect(data, 5000, 10<<20)
	assessment := asset.Inspect(data, 2000, 10<<20)
	v.InputVersion = &AssetVersion{ID: "source", Report: oldReport,
		SourceIntent: &Intent{Asset: "茶壶", MaxTriangles: 5000, MaxBytes: 10 << 20}}
	v.InputAssessment = &assessment
	if err := s.store.Create(ctx, v); err != nil {
		t.Fatal(err)
	}
	original := []*schema.Message{schema.SystemMessage("fixture"), schema.UserMessage(v.Request)}
	originalJSON := jsonString(original)
	m := &countedModel{s: s, id: v.ID, BaseChatModel: protocolModel{generate: func(_ context.Context, in []*schema.Message) (*schema.Message, error) {
		start := strings.LastIndex(in[0].Content, "<runtime_state>")
		if start < 0 {
			t.Fatal("model did not receive runtime state")
		}
		var state struct {
			Basis           map[string]any `json:"explanation_basis"`
			InputVersion    AssetVersion   `json:"input_version"`
			InputAssessment asset.Report   `json:"input_assessment"`
			Intent          Intent         `json:"intent"`
			Artifacts       []any          `json:"artifacts"`
		}
		raw := strings.Split(in[0].Content[start+len("<runtime_state>"):], "</runtime_state>")[0]
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			t.Fatal(err)
		}
		if state.Basis["requirements_role"] != "targets_and_assumptions_not_observations" {
			t.Fatalf("model lacks explicit evidence boundaries: %+v", state.Basis)
		}
		if !state.InputVersion.Report.Passed || state.InputVersion.Report.MaxTriangles != 5000 ||
			state.InputAssessment.Passed || state.InputAssessment.Triangles != 4500 || state.InputAssessment.MaxTriangles != 2000 {
			t.Fatalf("source measurements or independent assessment changed: %+v", state)
		}
		if state.Intent.Asset != "茶壶" || len(state.Intent.Assumptions) != 1 || state.Intent.Assumptions[0] != "目标2000面不代表实测通过" || len(state.Artifacts) != 0 {
			t.Fatalf("requirements were lost or source was promoted to new output: %+v", state)
		}
		return protocolProposal("finish_request", "context-answer", conversationFinishInput{Outcome: "answer", Explanation: "原报告4500面，本次2000面目标尚未满足，未检查外观。"}), nil
	}}}
	if _, err := m.Generate(ctx, original); err != nil {
		t.Fatal(err)
	}
	if jsonString(original) != originalJSON {
		t.Fatal("model-only context mutated Eino replay input")
	}
	saved, err := s.store.Get(ctx, v.ID)
	if err != nil || jsonString(saved.InputVersion.Report) != jsonString(oldReport) || jsonString(saved.InputAssessment) != jsonString(assessment) {
		t.Fatalf("context generation rewrote saved reports: %v", err)
	}
	if _, exists := conversationRunView(saved)["explanation_basis"]; exists {
		t.Fatal("model-only guidance leaked into public result protocol")
	}
}

// 没有产物时仅声明技术检查范围，不制造已检查或失败原因结论。
func TestConversationMissingOutputDoesNotInventDiagnosis(t *testing.T) {
	v := Session{ExecutionVersion: ConversationPromptVersion, GoalKind: "generate",
		Intent:  &Intent{Asset: "茶壶", MaxTriangles: 5000, MaxBytes: 10 << 20},
		Current: &Operation{ID: "failed-op", Kind: "generate", Stage: "done", TaskID: "known-task", Error: "供应商错误正文声称修改参数无效"}}
	before := jsonString(v)
	view := conversationModelView(v)
	basis := view["explanation_basis"].(map[string]any)
	if basis["provider_failure_cause"] != "not_diagnosed" || basis["parameter_effects"] != "not_guaranteed" {
		t.Fatalf("external error text became a diagnosis: %+v", basis)
	}
	if len(view["artifacts"].([]map[string]any)) != 0 || view["goal_kind"] != "generate" || before != jsonString(v) {
		t.Fatal("missing output context created evidence or changed the goal")
	}
}

// 用真实Eino和固定供应商走实际路径：缺文件允许有界再次生成，纠偏减面不改原目标，
// 未知提交仍由Runtime直接终止，不能进入同一套再次生成路径。
func TestConversationCorrectionPathsMatchAcceptedGoal(t *testing.T) {
	for _, mode := range []string{"missing_output", "always_over", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			p := &conversationEvalProvider{mode: mode}
			s := newConversationTestService(t, &conversationProvider{})
			s.provider = p
			s.modelFactory = func(context.Context) (model.BaseChatModel, error) {
				return protocolModel{generate: func(ctx context.Context, in []*schema.Message) (*schema.Message, error) {
					var state struct {
						Intent     *Intent        `json:"intent"`
						Goal       string         `json:"goal_kind"`
						Current    *Operation     `json:"current_operation"`
						Production int            `json:"production"`
						Artifacts  []Artifact     `json:"artifacts"`
						Basis      map[string]any `json:"explanation_basis"`
						Failure    map[string]any `json:"failure_evidence"`
					}
					start := strings.LastIndex(in[0].Content, "<runtime_state>")
					if start < 0 {
						t.Fatal("missing runtime facts")
					}
					raw := strings.Split(in[0].Content[start+len("<runtime_state>"):], "</runtime_state>")[0]
					if err := json.Unmarshal([]byte(raw), &state); err != nil {
						t.Fatal(err)
					}
					if state.Intent == nil {
						return (conversationScript{}).Generate(ctx, in)
					}
					if state.Goal != "generate" || state.Basis["provider_failure_cause"] != "not_diagnosed" {
						t.Fatalf("operation or external feedback changed goal/evidence: %+v", state)
					}
					if mode == "missing_output" && state.Production > 0 {
						if state.Current == nil || state.Current.ErrorCode != "missing_model_output" || state.Failure["cause"] != "unknown" || state.Failure["task_id"] != state.Current.TaskID {
							t.Fatalf("missing-output observation was lost on the real Eino resume path: %+v", state)
						}
					}
					if (mode == "missing_output" && state.Production == 2) || state.Production == 3 {
						return protocolProposal("finish_request", newID(), conversationFinishInput{Outcome: "ended", Explanation: "依据实际反馈结束，未交付符合技术目标的文件；未做视觉检查，不推断供应商原因或参数效果。"}), nil
					}
					if mode == "always_over" && state.Production == 1 {
						return protocolProposal("decimate_asset", newID(), decimationInput{ArtifactID: state.Artifacts[0].ID, TargetTriangles: 3500, Reason: "根据实测6000面选择减面，保留原生成目标"}), nil
					}
					if mode == "always_over" && state.Production == 2 && (state.Current == nil || state.Current.Kind != "decimate") {
						t.Fatal("fixture did not complete a decimation correction")
					}
					return protocolProposal("generate_asset", newID(), generationInput{Prompt: "A wooden crate for product presentation", TargetTriangles: state.Intent.MaxTriangles, Reason: "在原生成目标和剩余预算内尝试，不保证参数效果"}), nil
				}}, nil
			}
			_, v, err := s.CreateAssetConversation(ctx, "owner", "为产品展示生成木箱", "correction-boundary")
			if err != nil {
				t.Fatal(err)
			}
			v = waitSession(t, s, v.ID, func(v Session) bool { return v.Terminal() })
			want := 2
			if mode == "always_over" {
				want = 3
			} else if mode == "unknown" {
				want = 1
			}
			if v.Production != want || v.GoalKind != "generate" || v.Status != "failed" || v.Result == nil {
				t.Fatalf("incorrect correction result: %s", jsonString(v.View()))
			}
			if mode == "unknown" && v.Result.Reason != "submission_unknown" {
				t.Fatal("unknown submission was retried or misclassified")
			}
			assertStoreEventCount(t, s.store, v.ID, "runtime_blocked", 0)
			p.mu.Lock()
			defer p.mu.Unlock()
			if len(p.submitted) != want || (mode == "always_over" && strings.Join(p.kinds, ",") != "generate,decimate,generate") {
				t.Fatalf("wrong actual production chain: %v", p.kinds)
			}
		})
	}
}

// 错误类型是证据来源，供应商错误正文相同不等于程序观察到了缺失输出。
// 重复执行只重放已保存的观察；旧 profile 的反馈字节不因 v3 增强而改变。
func TestConversationFailureEvidenceIsTypedAndReplayable(t *testing.T) {
	for _, version := range []string{"asset-agent-v1", "asset-agent-v2", ConversationPromptVersion} {
		for _, typed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/typed=%t", version, typed), func(t *testing.T) {
				ctx := context.Background()
				s := testService(t, t.TempDir(), &fakeProvider{}, false)
				defer s.Close()
				v := s.newRun("owner", "生成茶壶", version)
				v.Production = 1
				v.Current = &Operation{ID: "op", Kind: "generate", Stage: "submitted", TaskID: "task"}
				if err := s.store.Create(ctx, v); err != nil {
					t.Fatal(err)
				}
				cause := fmt.Errorf("%s", errMissingModelOutput)
				if typed {
					cause = fmt.Errorf("%w", errMissingModelOutput)
				}
				result, err := s.operationFailure(v.ID, "op", cause)
				if err != nil {
					t.Fatal(err)
				}
				saved, err := s.store.Get(ctx, v.ID)
				if err != nil {
					t.Fatal(err)
				}
				var feedback map[string]any
				if err := json.Unmarshal([]byte(result), &feedback); err != nil {
					t.Fatal(err)
				}
				if typed && version == ConversationPromptVersion {
					if saved.Current.ErrorCode != "missing_model_output" || jsonString(feedback["failure_evidence"]) != jsonString(conversationModelView(saved)["failure_evidence"]) {
						t.Fatal("stored observation, tool result and model context diverged")
					}
				} else if saved.Current.ErrorCode != "" || feedback["failure_evidence"] != nil || result != jsonString(map[string]any{"error": cause.Error(), "remaining_submissions": v.Limits.Submissions - 1}) {
					t.Fatalf("untyped text or legacy result was promoted to new evidence: %s", result)
				}
				replayed, err := s.production(ctx, v.ID, "op")
				if err != nil || replayed != result {
					t.Fatalf("replay changed the saved evidence: %s %v", replayed, err)
				}
				saved.Production++
				var updated map[string]any
				if err := json.Unmarshal([]byte(s.operationResult(saved, *saved.Current)), &updated); err != nil || updated["remaining_submissions"] != float64(saved.Limits.Submissions-2) || jsonString(updated["failure_evidence"]) != jsonString(feedback["failure_evidence"]) {
					t.Fatal("result reused stale budget or changed persisted observation")
				}
				assertStoreEventCount(t, s.store, v.ID, "tool_failed", 1)
			})
		}
	}
}
