package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// 正文中的提问没有真正暂停，不应宣称用户正在回答，也不能误报为检查点恢复故障。
func TestConversationPlainModelQuestionDoesNotBecomeCheckpointFailure(t *testing.T) {
	s := newConversationTestService(t, &conversationProvider{})
	s.modelFactory = func(context.Context) (model.BaseChatModel, error) {
		return protocolModel{generate: func(context.Context, []*schema.Message) (*schema.Message, error) {
			return schema.AssistantMessage("请说明用途？", nil), nil
		}}, nil
	}
	_, v, err := s.CreateAssetConversation(context.Background(), "owner", "木箱", "plain-question")
	if err != nil {
		t.Fatal(err)
	}
	v = waitSession(t, s, v.ID, func(v Session) bool { return v.Terminal() })
	if v.Result == nil || v.Result.Reason != "model_failed" || v.Clarifications != 0 || v.WaitID != "" || v.Production != 0 {
		t.Fatalf("non-tool question misclassified: %s", jsonString(v.View()))
	}
	assertStoreEventCount(t, s.store, v.ID, "agent_proposal", 2)
	assertStoreEventCount(t, s.store, v.ID, "runtime_blocked", 2)
	assertStoreEventCount(t, s.store, v.ID, "clarification", 0)
}

// 真实 Eino 只能看到纠正后的一个 ask_user；被拒绝的批次不能执行生成或制造两个暂停。
// 使用实际检查点接受答案，证明纠正消息没有破坏 Runner 的暂停协议。
func TestConversationModelActionCorrectionCommitsRealEinoQuestion(t *testing.T) {
	s, v, _, _ := newModelActionFixture(t, ConversationPromptVersion)
	s.provider = &fakeProvider{}
	rejected := invalidModelAction(true)
	accepted := protocolProposal("ask_user", "real-eino-corrected-question", questionInput{Question: "请确认木箱风格。"})
	calls := 0
	s.modelFactory = func(context.Context) (model.BaseChatModel, error) {
		return protocolModel{generate: func(context.Context, []*schema.Message) (*schema.Message, error) {
			calls++
			if calls == 1 {
				return rejected, nil
			}
			if calls == 2 {
				return accepted, nil
			}
			return nil, errors.New("unexpected fresh decision before user answer")
		}}, nil
	}
	ctx := context.Background()
	coordinator := &pauseCoordinator{s: s, id: v.ID, mode: "normal"}
	interrupted, err := s.executeRunner(ctx, v, coordinator, false, []*schema.Message{schema.UserMessage(v.Request)})
	if err != nil || !interrupted || !coordinator.committed || calls != 2 {
		t.Fatalf("real Eino correction did not commit its pause: calls=%d interrupted=%t committed=%t err=%v", calls, interrupted, coordinator.committed, err)
	}
	stored, err := s.store.Get(ctx, v.ID)
	if err != nil || stored.ModelCalls != 2 || stored.Clarifications != 1 || stored.Production != 0 || stored.Current != nil || stored.Status != "awaiting_answer" || stored.WaitID == "" || stored.PendingPause != nil {
		t.Fatalf("invalid corrected pause state: %s err=%v", jsonString(stored.View()), err)
	}
	if stored.ResumePoint == nil || stored.ResumePoint.Kind != "question" || stored.ResumePoint.RefID != stored.WaitID {
		t.Fatal("corrected question lost checkpoint identity")
	}
	seed := coordinator.seed
	if err := validateReplaySeed(seed, ConversationPromptVersion); err != nil {
		t.Fatal(err)
	}
	if seed.ToolCallID != accepted.ToolCalls[0].ID || seed.InputHash != tokenHash(jsonString(stored.History[:len(stored.History)-1])) || strings.Contains(jsonString(seed.Input), "[运行时协议纠正]") || strings.Contains(jsonString(seed.Input), "rejected-generation") {
		t.Fatal("real Eino seed contains provider-only correction messages")
	}
	checkpoint, err := s.store.LoadCheckpoint(ctx, v.ID, v.ID)
	if err != nil || checkpoint.Point == nil || *checkpoint.Point != *stored.ResumePoint {
		t.Fatalf("real Eino checkpoint is not usable: %v", err)
	}
	if err := s.Answer(ctx, v.ID, "卡通低模风格"); err != nil {
		t.Fatalf("could not answer corrected Eino question: %v", err)
	}
	answered, err := s.store.Get(ctx, v.ID)
	if err != nil || answered.Answers[stored.WaitID].Text != "卡通低模风格" || !answered.ResumeRequested || answered.ModelCalls != 2 || answered.Clarifications != 1 || calls != 2 {
		t.Fatalf("accepted answer changed the corrected decision: calls=%d err=%v", calls, err)
	}
	assertStoreEventCount(t, s.store, v.ID, "clarification", 1)
	assertStoreEventCount(t, s.store, v.ID, "checkpoint_committed", 1)
	assertModelActionTrace(t, s, v.ID, []*schema.Message{rejected, accepted}, []string{"tool_batch"})
}

// 不启动调度器，确保这里只测模型协议边界，任何提议都不会触发生产或外部请求。
func newModelActionFixture(t *testing.T, version string) (*Service, Session, *countedModel, []*schema.Message) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	v := s.newRun("owner", "为产品展示制作静态木箱", version)
	if err = s.store.Create(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	profile, err := profileForVersion(version)
	if err != nil {
		t.Fatal(err)
	}
	m := &countedModel{s: s, id: v.ID, coordinator: &pauseCoordinator{s: s, id: v.ID, mode: "normal", profile: profile}}
	in := []*schema.Message{schema.SystemMessage(profile.Instruction), schema.UserMessage(v.Request)}
	return s, v, m, in
}

func invalidModelAction(batch bool) *schema.Message {
	msg := protocolProposal("ask_user", "rejected-question", questionInput{Question: "需要什么风格？"})
	msg.Content = "先确认风格，再立即生成。"
	if batch {
		msg.ToolCalls = append(msg.ToolCalls, protocolProposal("generate_asset", "rejected-generation", generationInput{Prompt: "wooden crate", TargetTriangles: 5000}).ToolCalls[0])
	} else {
		msg.ToolCalls = nil
	}
	return msg
}

// 纠正上下文只给提供方：完整保留原响应，批量中的每个调用都有未执行回执，
// 被 Eino 接受的 History 与恢复种子仍然只绑定原输入及最终合法提议。
func TestConversationModelActionCorrectionPreservesReplayProtocol(t *testing.T) {
	for _, batch := range []bool{false, true} {
		name := "plain_question"
		if batch {
			name = "tool_batch"
		}
		t.Run(name, func(t *testing.T) {
			s, v, m, in := newModelActionFixture(t, ConversationPromptVersion)
			original := jsonString(in)
			rejected := invalidModelAction(batch)
			accepted := protocolProposal("ask_user", "accepted-question", questionInput{Question: "需要什么风格？"})
			calls := 0
			m.BaseChatModel = protocolModel{generate: func(_ context.Context, supplied []*schema.Message) (*schema.Message, error) {
				calls++
				current, err := s.store.Get(context.Background(), v.ID)
				if err != nil || current.ModelCalls != calls {
					t.Fatalf("call was not charged before provider invocation: calls=%d state=%d err=%v", calls, current.ModelCalls, err)
				}
				if calls == 1 {
					return rejected, nil
				}
				if calls != 2 {
					t.Fatalf("unexpected provider call %d", calls)
				}
				wantLen := len(in) + 1 + len(rejected.ToolCalls) + 1
				if len(supplied) != wantLen {
					t.Fatalf("correction transcript length=%d want=%d", len(supplied), wantLen)
				}
				if !strings.HasPrefix(supplied[0].Content, in[0].Content) || strings.Count(supplied[0].Content, "<runtime_state>") != 1 || jsonString(supplied[1:len(in)]) != jsonString(in[1:]) {
					t.Fatal("correction lost original input or duplicated runtime state")
				}
				if !reflect.DeepEqual(supplied[len(in)], rejected) || supplied[len(in)].ReasoningContent == "" {
					t.Fatal("provider correction did not preserve the entire original assistant message")
				}
				for i, call := range rejected.ToolCalls {
					reply := supplied[len(in)+1+i]
					if reply.Role != schema.Tool || reply.ToolCallID != call.ID || !strings.Contains(reply.Content, "未执行") {
						t.Fatalf("rejected tool %s has no matching nonexecution receipt", call.ID)
					}
				}
				last := supplied[len(supplied)-1]
				if last.Role != schema.User || strings.TrimSpace(last.Content) == "" || !strings.Contains(last.Content, "工具") {
					t.Fatal("missing explicit user-role protocol correction")
				}
				return accepted, nil
			}}
			got, err := m.Generate(context.Background(), in)
			if err != nil || calls != 2 || !reflect.DeepEqual(got, accepted) {
				t.Fatalf("correction failed: calls=%d err=%v", calls, err)
			}
			if jsonString(in) != original {
				t.Fatal("provider-only messages changed Eino input")
			}
			stored, err := s.store.Get(context.Background(), v.ID)
			if err != nil || stored.ModelCalls != 2 || stored.Production != 0 || stored.Clarifications != 0 {
				t.Fatalf("correction changed business effects or budget: %+v err=%v", stored, err)
			}
			wantHistory := append(append([]*schema.Message(nil), in...), accepted)
			if jsonString(stored.History) != jsonString(wantHistory) {
				t.Fatal("rejected assistant or synthetic correction leaked into authoritative History")
			}
			seed := m.coordinator.seed
			if err := validateReplaySeed(seed, ConversationPromptVersion); err != nil {
				t.Fatal(err)
			}
			if seed.InputHash != tokenHash(original) || jsonString(seed.Input) != original || !reflect.DeepEqual(seed.Response, accepted) {
				t.Fatal("accepted replay seed no longer matches original Eino input")
			}
			code := "invalid_action"
			if batch {
				code = "tool_batch"
			}
			assertModelActionTrace(t, s, v.ID, []*schema.Message{rejected, accepted}, []string{code})
			// 重建只能返回已冻结的合法响应，不能再次纠正、扣费或追加证据。
			m.coordinator = &pauseCoordinator{mode: "rebuild", profile: m.coordinator.profile, seed: seed}
			replayed, err := m.Generate(context.Background(), in)
			if err != nil || calls != 2 || !reflect.DeepEqual(replayed, accepted) {
				t.Fatalf("rebuild made a new decision: calls=%d err=%v", calls, err)
			}
			stored, err = s.store.Get(context.Background(), v.ID)
			if err != nil || stored.ModelCalls != 2 || jsonString(stored.History) != jsonString(wantHistory) {
				t.Fatal("rebuild changed call budget or history")
			}
			assertModelActionTrace(t, s, v.ID, []*schema.Message{rejected, accepted}, []string{code})
		})
	}
}

func TestConversationModelActionCorrectionIsBounded(t *testing.T) {
	for _, batch := range []bool{false, true} {
		name := "plain_twice"
		if batch {
			name = "batch_twice"
		}
		t.Run(name, func(t *testing.T) {
			s, v, m, in := newModelActionFixture(t, ConversationPromptVersion)
			calls := 0
			rejected := invalidModelAction(batch)
			m.BaseChatModel = protocolModel{generate: func(context.Context, []*schema.Message) (*schema.Message, error) {
				calls++
				return rejected, nil
			}}
			got, err := m.Generate(context.Background(), in)
			var modelErr *modelCallError
			if got != nil || !errors.As(err, &modelErr) || calls != 2 || m.coordinator.seed != nil {
				t.Fatalf("repeated invalid action escaped bounded correction: calls=%d err=%v", calls, err)
			}
			code := "invalid_action"
			if batch {
				code = "tool_batch"
			}
			assertModelActionTrace(t, s, v.ID, []*schema.Message{rejected, rejected}, []string{code, code})
		})
	}
}

// 空响应与供应商错误不是可纠正的动作，不能自动花费另一次模型预算。
func TestConversationModelActionDoesNotCorrectMissingResponse(t *testing.T) {
	providerErr := errors.New("controlled provider error")
	for _, tc := range []struct {
		name string
		msg  *schema.Message
		err  error
	}{
		{name: "nil"},
		{name: "empty", msg: schema.AssistantMessage(" \n\t", nil)},
		{name: "provider_error", err: providerErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, v, m, in := newModelActionFixture(t, ConversationPromptVersion)
			calls := 0
			m.BaseChatModel = protocolModel{generate: func(context.Context, []*schema.Message) (*schema.Message, error) {
				calls++
				return tc.msg, tc.err
			}}
			got, err := m.Generate(context.Background(), in)
			var modelErr *modelCallError
			if got != nil || !errors.As(err, &modelErr) || calls != 1 || m.coordinator.seed != nil {
				t.Fatalf("unusable response was retried or accepted: calls=%d err=%v", calls, err)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatal("provider error cause was lost")
			}
			assertStoreEventCount(t, s.store, v.ID, "model_call", 1)
			assertStoreEventCount(t, s.store, v.ID, "runtime_blocked", 0)
			assertStoreEventCount(t, s.store, v.ID, "model_error", 1)
			proposals := 0
			if tc.msg != nil {
				proposals = 1
			}
			assertStoreEventCount(t, s.store, v.ID, "agent_proposal", proposals)
		})
	}
}

func TestConversationModelActionStopsWhenCorrectionHasNoResponse(t *testing.T) {
	providerErr := errors.New("controlled correction provider error")
	for _, tc := range []struct {
		name string
		msg  *schema.Message
		err  error
	}{
		{name: "nil"},
		{name: "empty", msg: schema.AssistantMessage("", nil)},
		{name: "provider_error", err: providerErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, v, m, in := newModelActionFixture(t, ConversationPromptVersion)
			calls := 0
			m.BaseChatModel = protocolModel{generate: func(context.Context, []*schema.Message) (*schema.Message, error) {
				calls++
				if calls == 1 {
					return invalidModelAction(false), nil
				}
				return tc.msg, tc.err
			}}
			got, err := m.Generate(context.Background(), in)
			var modelErr *modelCallError
			if got != nil || !errors.As(err, &modelErr) || calls != 2 || m.coordinator.seed != nil {
				t.Fatalf("unusable correction triggered another attempt: calls=%d err=%v", calls, err)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatal("correction provider error cause was lost")
			}
			assertStoreEventCount(t, s.store, v.ID, "model_call", 2)
			assertStoreEventCount(t, s.store, v.ID, "runtime_blocked", 1)
			assertStoreEventCount(t, s.store, v.ID, "model_error", 1)
			proposals := 1
			if tc.msg != nil {
				proposals++
			}
			assertStoreEventCount(t, s.store, v.ID, "agent_proposal", proposals)
		})
	}
}

// 缺少唯一调用身份或合法参数时无法生成可信的未执行回执，直接失败而非伪造修复。
func TestConversationModelActionDoesNotRetryUnclosableToolBatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*schema.Message)
	}{
		{"duplicate_id", func(m *schema.Message) { m.ToolCalls[1].ID = m.ToolCalls[0].ID }},
		{"empty_id", func(m *schema.Message) { m.ToolCalls[0].ID = "" }},
		{"invalid_arguments", func(m *schema.Message) { m.ToolCalls[0].Function.Arguments = "{" }},
		{"missing_name", func(m *schema.Message) { m.ToolCalls[0].Function.Name = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, v, m, in := newModelActionFixture(t, ConversationPromptVersion)
			rejected := invalidModelAction(true)
			tc.mutate(rejected)
			calls := 0
			m.BaseChatModel = protocolModel{generate: func(context.Context, []*schema.Message) (*schema.Message, error) {
				calls++
				return rejected, nil
			}}
			got, err := m.Generate(context.Background(), in)
			var modelErr *modelCallError
			if got != nil || !errors.As(err, &modelErr) || calls != 1 || m.coordinator.seed != nil {
				t.Fatalf("invalid batch was retried with invented protocol: calls=%d err=%v", calls, err)
			}
			assertModelActionTrace(t, s, v.ID, []*schema.Message{rejected}, []string{"tool_batch"})
			assertStoreEventCount(t, s.store, v.ID, "model_protocol_retry", 0)
		})
	}
}

// 第一次响应与纠正请求之间重新读取边界，不能沿用第一次调用前的额度或终态。
func TestConversationModelActionCorrectionRechecksExecutionLimits(t *testing.T) {
	for _, boundary := range []string{"calls_exhausted", "stopped", "deadline", "context_cancelled"} {
		t.Run(boundary, func(t *testing.T) {
			s, v, m, in := newModelActionFixture(t, ConversationPromptVersion)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wantErr := ErrBudget
			if boundary == "calls_exhausted" {
				if _, err := s.store.Edit(ctx, v.ID, func(v *Session) error { v.Limits.Calls = 1; return nil }, "", nil); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			rejected := invalidModelAction(false)
			m.BaseChatModel = protocolModel{generate: func(context.Context, []*schema.Message) (*schema.Message, error) {
				calls++
				if calls != 1 {
					t.Fatalf("second provider call crossed %s boundary", boundary)
				}
				switch boundary {
				case "stopped", "deadline":
					_, err := s.store.Edit(context.Background(), v.ID, func(v *Session) error {
						if boundary == "stopped" {
							v.Ended = time.Now().UTC()
							v.Status = "stopped"
						} else {
							v.Deadline = time.Now().Add(-time.Second)
						}
						return nil
					}, "", nil)
					if err != nil {
						t.Fatal(err)
					}
					wantErr = ErrClosed
					if boundary == "deadline" {
						wantErr = context.DeadlineExceeded
					}
				case "context_cancelled":
					wantErr = context.Canceled
					cancel()
				}
				return rejected, nil
			}}
			got, err := m.Generate(ctx, in)
			var modelErr *modelCallError
			if got != nil || !errors.Is(err, wantErr) || errors.As(err, &modelErr) || calls != 1 {
				t.Fatalf("execution boundary was misclassified: calls=%d err=%v want=%v", calls, err, wantErr)
			}
			stored, err := s.store.Get(context.Background(), v.ID)
			if err != nil || stored.ModelCalls != 1 || stored.Production != 0 || m.coordinator.seed != nil {
				t.Fatal("blocked correction consumed extra budget or accepted a tool")
			}
			assertModelActionTrace(t, s, v.ID, []*schema.Message{rejected}, []string{"invalid_action"})
		})
	}
}

func TestConversationModelActionCorrectionDoesNotChangeLegacyProfiles(t *testing.T) {
	for _, version := range []string{PromptVersion, CurrentPromptVersion} {
		for _, batch := range []bool{false, true} {
			name := version + "/plain"
			if batch {
				name = version + "/batch"
			}
			t.Run(name, func(t *testing.T) {
				s, v, m, in := newModelActionFixture(t, version)
				proposal := invalidModelAction(batch)
				calls := 0
				m.BaseChatModel = protocolModel{generate: func(context.Context, []*schema.Message) (*schema.Message, error) {
					calls++
					return proposal, nil
				}}
				got, err := m.Generate(context.Background(), in)
				if calls != 1 {
					t.Fatalf("legacy profile retried %d calls", calls)
				}
				if batch {
					var modelErr *modelCallError
					if got != nil || err == nil || errors.As(err, &modelErr) {
						t.Fatalf("legacy batch error changed: %v", err)
					}
				} else if err != nil || !reflect.DeepEqual(got, proposal) {
					t.Fatalf("legacy plain response was no longer returned: %v", err)
				}
				assertStoreEventCount(t, s.store, v.ID, "model_call", 1)
				assertStoreEventCount(t, s.store, v.ID, "agent_proposal", 1)
			})
		}
	}
}

// 原始 content/tool_calls 必须分别留痕；纠正成功也不能隐藏先前被拒绝的提议。
func assertModelActionTrace(t *testing.T, s *Service, id string, proposals []*schema.Message, codes []string) {
	t.Helper()
	events, err := s.store.Events(context.Background(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	var gotProposals []*schema.Message
	var gotCodes []string
	var order []string
	for _, event := range events {
		switch event.Kind {
		case "model_call":
			order = append(order, event.Kind)
		case "agent_proposal":
			var evidence struct {
				Content   string            `json:"content"`
				ToolCalls []schema.ToolCall `json:"tool_calls"`
			}
			if err := json.Unmarshal(event.Data, &evidence); err != nil {
				t.Fatal(err)
			}
			gotProposals = append(gotProposals, &schema.Message{Content: evidence.Content, ToolCalls: evidence.ToolCalls})
			order = append(order, event.Kind)
		case "runtime_blocked":
			var evidence struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(event.Data, &evidence); err != nil {
				t.Fatal(err)
			}
			gotCodes = append(gotCodes, evidence.Code)
			order = append(order, event.Kind)
		}
	}
	if len(gotProposals) != len(proposals) || !reflect.DeepEqual(gotCodes, codes) {
		t.Fatalf("missing original proposal/block evidence: proposals=%d want=%d codes=%v want=%v", len(gotProposals), len(proposals), gotCodes, codes)
	}
	wantOrder := []string{}
	for i, proposal := range proposals {
		if gotProposals[i].Content != proposal.Content || jsonString(gotProposals[i].ToolCalls) != jsonString(proposal.ToolCalls) {
			t.Fatalf("proposal %d content or tool parameters changed", i)
		}
		wantOrder = append(wantOrder, "model_call", "agent_proposal")
		if i < len(codes) {
			wantOrder = append(wantOrder, "runtime_blocked")
		}
	}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("proposal/block/correction evidence order=%v want=%v", order, wantOrder)
	}
}
