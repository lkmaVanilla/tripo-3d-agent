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
)

type bindingTestState struct {
	ID             string        `json:"id"`
	Intent         *Intent       `json:"intent"`
	Input          *AssetVersion `json:"input_version"`
	Assessment     *asset.Report `json:"input_assessment"`
	Artifacts      []Artifact    `json:"artifacts"`
	Clarifications int           `json:"clarifications"`
	Binding        struct {
		Scope  string `json:"scope"`
		Status string `json:"status"`
		ID     string `json:"selected_version_id"`
	} `json:"input_binding"`
}

func readBindingTestState(in []*schema.Message) (bindingTestState, error) {
	var state bindingTestState
	if len(in) == 0 {
		return state, fmt.Errorf("missing model messages")
	}
	i := strings.LastIndex(in[0].Content, "<runtime_state>")
	if i < 0 {
		return state, fmt.Errorf("missing provider runtime state")
	}
	raw := strings.Split(in[0].Content[i+len("<runtime_state>"):], "</runtime_state>")[0]
	err := json.Unmarshal([]byte(raw), &state)
	return state, err
}

func checkBindingTestState(state bindingTestState, status, id string) error {
	if state.Binding.Scope != "initial_historical_version" || state.Binding.Status != status || state.Binding.ID != id {
		return fmt.Errorf("input binding=%+v, want %s/%s", state.Binding, status, id)
	}
	return nil
}

// 在实际 provider 输入上检查：历史里的唯一版本不能变成选择，选择身份也不证明可加工。
func TestConversationInputBindingUsesAcceptedSelection(t *testing.T) {
	for _, selected := range []bool{false, true} {
		name := "visible_history_is_not_selection"
		if selected {
			name = "selected_unusable_version_keeps_identity"
		}
		t.Run(name, func(t *testing.T) {
			s, v, m, in := newModelActionFixture(t, ConversationPromptVersion)
			_, err := s.store.Edit(context.Background(), v.ID, func(v *Session) error {
				v.ConversationContext = map[string]any{
					"messages":      []any{map[string]any{"version_id": "history-B", "text": "唯一历史版本B"}},
					"input_version": map[string]any{"id": "history-B"},
				}
				if selected {
					v.InputVersion = &AssetVersion{ID: "selected-A", Processable: false, Report: asset.Report{Valid: false, Passed: false}}
				}
				return nil
			}, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			m.BaseChatModel = protocolModel{generate: func(_ context.Context, supplied []*schema.Message) (*schema.Message, error) {
				state, err := readBindingTestState(supplied)
				if err != nil {
					return nil, err
				}
				status, id := "not_selected", ""
				if selected {
					status, id = "selected", "selected-A"
					if state.Input == nil || state.Input.ID != id || state.Input.Processable || state.Input.Report.Valid || state.Input.Report.Passed {
						return nil, fmt.Errorf("selection was promoted to usable evidence: %+v", state.Input)
					}
				} else if state.Input != nil {
					return nil, fmt.Errorf("history became bound input: %+v", state.Input)
				}
				if err := checkBindingTestState(state, status, id); err != nil {
					return nil, err
				}
				return protocolProposal("finish_request", newID(), conversationFinishInput{Outcome: "answer", Explanation: "输入身份与可加工性分开核对。"}), nil
			}}
			if _, err = m.Generate(context.Background(), in); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// 真实 Eino 先提交澄清检查点，再接受带身份的答案；脚本模型只用于核验输入投影与路径。
func TestConversationInputBindingClarificationResumesSameRun(t *testing.T) {
	for _, choose := range []bool{false, true} {
		name := "decline_without_production"
		if choose {
			name = "choose_older_A_in_same_run"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			provider := &fakeProvider{}
			s := testService(t, t.TempDir(), provider, false)
			defer s.Close()
			c, old, err := s.CreateAssetConversation(ctx, "owner", "产品展示茶壶", "first")
			if err != nil {
				t.Fatal(err)
			}
			older := conversationTestOutput(t, s.store, s.Config.DataDir, old, "")
			conversationTestOutput(t, s.store, s.Config.DataDir, old, "")
			conversationTestEnd(t, s.store, old.ID)
			run, err := s.ContinueConversation(ctx, c.ID, "owner", "把这个茶壶减到最多2000面", "followup", "")
			if err != nil {
				t.Fatal(err)
			}
			s.modelFactory = func(context.Context) (model.BaseChatModel, error) {
				return protocolModel{generate: func(_ context.Context, in []*schema.Message) (*schema.Message, error) {
					state, err := readBindingTestState(in)
					if err != nil {
						return nil, err
					}
					if state.ID != run.ID {
						return nil, fmt.Errorf("answer created a different run: %s", state.ID)
					}
					if state.Clarifications == 0 {
						if err = checkBindingTestState(state, "not_selected", ""); err != nil {
							return nil, err
						}
						return protocolProposal("ask_user", newID(), questionInput{Question: "请通过版本选择器指定茶壶版本。"}), nil
					}
					if !choose {
						if err = checkBindingTestState(state, "not_selected", ""); err != nil {
							return nil, err
						}
						return protocolProposal("finish_request", newID(), conversationFinishInput{Outcome: "answer", Explanation: "没有明确选择输入，本次没有加工或新版本。"}), nil
					}
					if err = checkBindingTestState(state, "selected", older.ID); err != nil {
						return nil, err
					}
					if state.Intent == nil {
						return protocolProposal("set_intent", newID(), conversationIntentInput{Action: "decimate", Intent: Intent{Asset: "茶壶", Use: "产品展示", MaxTriangles: 2000, MaxBytes: 10 << 20, Plan: []string{"先核对所选输入是否需要减面"}}}), nil
					}
					if state.Assessment == nil || !state.Assessment.Passed || state.Assessment.Triangles != 12 {
						return nil, fmt.Errorf("selected input assessment was not evaluated: %+v", state.Assessment)
					}
					return protocolProposal("finish_request", newID(), conversationFinishInput{Outcome: "answer", Explanation: "已明确选择旧版本A，实测12面满足2000面上限，无需加工；视觉未验。"}), nil
				}}, nil
			}
			if err = s.run(ctx, run.ID); err != nil {
				t.Fatal(err)
			}
			paused, err := s.store.Get(ctx, run.ID)
			if err != nil || paused.Status != "awaiting_answer" || paused.InputVersion != nil || paused.Intent != nil || paused.Current != nil || paused.ModelCalls != 1 || paused.Clarifications != 1 {
				t.Fatalf("first action did not wait for a selection: %s err=%v", jsonString(conversationRunView(paused)), err)
			}
			versionID, answer := "", "我不选择任何版本，不授权默认选择。"
			if choose {
				versionID, answer = older.ID, "明确选择旧版本A。"
			}
			accepted, duplicate, err := s.store.AcceptConversationAnswer(ctx, c.ID, "owner", "answer", run.ID, paused.WaitID, versionID, paused.generation(), answer)
			if err != nil || duplicate || accepted.ID != run.ID {
				t.Fatalf("answer changed run identity: duplicate=%t err=%v", duplicate, err)
			}
			if err = s.run(ctx, run.ID); err != nil {
				t.Fatal(err)
			}
			done, err := s.store.Get(ctx, run.ID)
			if err != nil || !validAnswer(done) || done.Production != 0 || done.Current != nil || len(done.Artifacts) != 0 || done.Answers[paused.WaitID].Text != answer || provider.Count() != 0 {
				t.Fatalf("wrong resumed outcome: %s err=%v", jsonString(conversationRunView(done)), err)
			}
			if choose && (done.InputVersion == nil || done.InputVersion.ID != older.ID) {
				t.Fatal("resumed answer switched to the latest history version")
			}
			assertStoreEventCount(t, s.store, run.ID, "runtime_blocked", 0)
			assertStoreEventCount(t, s.store, run.ID, "clarification", 1)
			ids, err := s.store.ConversationRunIDs(ctx, c.ID)
			if err != nil || len(ids) != 2 {
				t.Fatalf("clarification created another run: %v %v", ids, err)
			}
		})
	}
}

// 首次生成及对本 Run 新候选纠偏都没有历史输入绑定，不能被新指引误限为缺输入。
func TestConversationInputBindingAllowsCurrentRunCorrection(t *testing.T) {
	provider := &conversationProvider{overGeneration: true}
	s := newConversationTestService(t, provider)
	s.modelFactory = func(context.Context) (model.BaseChatModel, error) {
		return protocolModel{generate: func(ctx context.Context, in []*schema.Message) (*schema.Message, error) {
			state, err := readBindingTestState(in)
			if err != nil {
				return nil, err
			}
			if err = checkBindingTestState(state, "not_selected", ""); err != nil || state.Input != nil {
				return nil, fmt.Errorf("current output became historical selection: %+v: %v", state.Input, err)
			}
			if len(state.Artifacts) > 0 && !state.Artifacts[len(state.Artifacts)-1].Report.Passed {
				return protocolProposal("decimate_asset", newID(), decimationInput{ArtifactID: state.Artifacts[len(state.Artifacts)-1].ID, TargetTriangles: 4000, Reason: "当前候选实测超限，在原生成目标内减面。"}), nil
			}
			return (conversationScript{}).Generate(ctx, in)
		}}, nil
	}
	_, run, err := s.CreateAssetConversation(context.Background(), "owner", "产品展示木箱", "first")
	if err != nil {
		t.Fatal(err)
	}
	done := waitSession(t, s, run.ID, func(v Session) bool { return v.Terminal() })
	if done.Status != "completed" || done.GoalKind != "generate" || done.InputVersion != nil || done.Clarifications != 0 || done.Production != 2 || len(done.Artifacts) != 2 || done.Artifacts[1].Report.Triangles != 4000 {
		t.Fatalf("unselected initial input blocked legal generation/correction: %s", jsonString(conversationRunView(done)))
	}
	assertStoreEventCount(t, s.store, run.ID, "runtime_blocked", 0)
}
