package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/testfixture"
)

// 在真实 Eino checkpoint 写入前后断开服务，证明新版并非只在普通执行路径支持空上限。
func TestOptionalPauseRecovery(t *testing.T) {
	for _, kind := range []string{"question", "generation", "reduction"} {
		for _, pending := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/pending=%t", kind, pending), func(t *testing.T) {
				ctx := context.Background()
				s := testService(t, t.TempDir(), &fakeProvider{}, false)
				var v Session
				if kind == "reduction" {
					s.Close()
					var input AssetVersion
					s, v, input = conversationInputFixture(t)
					c, e := s.store.GetRunConversation(ctx, v.ID)
					if e != nil {
						t.Fatal(e)
					}
					editProductionFixture(t, s, v.ID, func(v *Session) { v.Finish("stopped", "fixture") })
					if e = s.store.ReleaseConversationRun(ctx, v.ID); e != nil {
						t.Fatal(e)
					}
					v = s.newRun("owner", "继续降低面数，取消两个上限", OptionalPromptVersion)
					v, _, e = s.store.AppendConversationRun(ctx, c.ID, "owner", "v4-edit", input.ID, v)
					if e != nil {
						t.Fatal(e)
					}
					v.InputVersion = &input
					intent, e := normalizeOptionalIntent(v, optionalIntentInput{Action: "decimate", Intent: optionalIntentFields{Asset: "木箱", Plan: []string{"继续降低"}, ReductionMode: "further", MaxTriangles: &LimitChange{Mode: "clear"}, MaxBytes: &LimitChange{Mode: "clear"}}})
					if e != nil {
						t.Fatal(e)
					}
					assessment := inspectIntent(testfixture.Cube(4500), &intent)
					v = editProductionFixture(t, s, v.ID, func(v *Session) { v.Intent = &intent; v.GoalKind = "decimate"; v.InputAssessment = &assessment })
				} else {
					v = s.newRun("owner", "产品展示木箱", OptionalPromptVersion)
					if kind == "generation" {
						intent, e := normalizeOptionalIntent(v, optionalIntentInput{Action: "generate", Intent: optionalIntentFields{Asset: "木箱", Plan: []string{"生成"}}})
						if e != nil {
							t.Fatal(e)
						}
						v.Intent = &intent
						v.GoalKind = "generate"
					}
					if _, _, _, e := s.store.CreateConversation(ctx, v, "first"); e != nil {
						t.Fatal(e)
					}
				}
				s.Config.ProductionSlots = 0
				if pending {
					if _, e := s.store.db.Exec("CREATE TEMP TRIGGER optional_pause_failure BEFORE INSERT ON checkpoints BEGIN SELECT RAISE(ABORT, 'controlled optional checkpoint failure'); END"); e != nil {
						t.Fatal(e)
					}
				}
				calls := 0
				s.modelFactory = func(context.Context) (model.BaseChatModel, error) {
					return protocolModel{generate: func(_ context.Context, in []*schema.Message) (*schema.Message, error) {
						calls++
						if !strings.HasPrefix(in[0].Content, instructionV4+"\n") {
							return nil, fmt.Errorf("wrong profile")
						}
						if calls == 1 {
							return protocolProposal("skill", "v4-skill", map[string]string{"skill": "intent"}), nil
						}
						switch kind {
						case "question":
							return protocolProposal("ask_user", "v4-question", questionInput{Question: "展示用途是什么？"}), nil
						case "generation":
							return protocolProposal("generate_asset", "v4-generate", generationInput{Prompt: "crate", TargetTriangles: 8000, Reason: "制作选择"}), nil
						default:
							return protocolProposal("decimate_asset", "v4-decimate", decimationInput{ArtifactID: v.InputVersion.ID, TargetTriangles: 2000, Reason: "进一步降低"}), nil
						}
					}}, nil
				}
				interrupted, e := s.executeRunner(ctx, v, &pauseCoordinator{s: s, id: v.ID, mode: "normal"}, false, []*schema.Message{schema.UserMessage(v.Request)})
				if !interrupted || calls != 2 || (pending && e == nil) || (!pending && e != nil) {
					t.Fatalf("pause calls=%d interrupted=%t err=%v", calls, interrupted, e)
				}
				before := getProductionFixture(t, s, v.ID)
				if pending && before.PendingPause == nil {
					t.Fatal("missing pending pause")
				}
				dir := s.Config.DataDir
				if e = s.Close(); e != nil {
					t.Fatal(e)
				}
				p := &fakeProvider{}
				s = testService(t, dir, p, false)
				defer s.Close()
				s.Config.ProductionSlots = 0
				s.modelFactory = func(context.Context) (model.BaseChatModel, error) {
					return protocolModel{generate: func(context.Context, []*schema.Message) (*schema.Message, error) {
						t.Error("pure recovery called new model decision")
						return nil, fmt.Errorf("unexpected")
					}}, nil
				}
				if e = s.run(ctx, v.ID); e != nil {
					t.Fatal(e)
				}
				after := getProductionFixture(t, s, v.ID)
				if after.ExecutionVersion != OptionalPromptVersion || after.ModelCalls != before.ModelCalls || after.Production != before.Production || p.Count() != 0 || !after.Deadline.Equal(before.Deadline) || jsonString(after.Intent) != jsonString(before.Intent) || jsonString(after.InputVersion) != jsonString(before.InputVersion) || jsonString(after.InputAssessment) != jsonString(before.InputAssessment) {
					t.Fatal("recovery changed saved identity or contract")
				}
				if kind == "question" {
					ref := before.WaitID
					if pending {
						ref = before.PendingPause.Point.RefID
					}
					if after.Status != "awaiting_answer" || after.WaitID != ref || after.Clarifications != 1 {
						t.Fatalf("question lost: %+v", after)
					}
				} else {
					op := before.Current
					if pending {
						op = before.PendingPause.Operation
					}
					if after.Status != "queued" || after.Current == nil || after.Current.ID != op.ID || jsonString(after.Current.Params) != jsonString(op.Params) || after.Current.InputVersionID != op.InputVersionID || after.Current.InputSHA256 != op.InputSHA256 {
						t.Fatalf("production proposal lost: %+v", after)
					}
				}
			})
		}
	}
}

func TestOptionalUnknownSubmissionNeverResends(t *testing.T) {
	s, v, p := productionRecoveryFixture(t, "submitting")
	defer s.Close()
	in, _ := normalizeOptionalIntent(v, optionalIntentInput{Action: "generate", Intent: optionalIntentFields{Asset: "木箱", Plan: []string{"生成"}}})
	v = editProductionFixture(t, s, v.ID, func(v *Session) { v.ExecutionVersion = OptionalPromptVersion; v.Intent = &in; v.GoalKind = "generate" })
	_, e := s.production(context.Background(), v.ID, v.Current.ID)
	after := getProductionFixture(t, s, v.ID)
	if e == nil || p.submits != 0 || p.queries != 0 || after.Production != v.Production || !after.Deadline.Equal(v.Deadline) {
		t.Fatal("unknown submission was retried")
	}
}
