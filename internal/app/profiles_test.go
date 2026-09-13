package app

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// createFrozenV1Session 保存升级前缺少 ExecutionVersion 的 JSON；所有指引来自固定 v1。
// 不调用最新的 Service.Create 后再改版本，否则 request 事件会伪装成旧配置。
func createFrozenV1Session(t *testing.T, s *Service, production bool) Session {
	t.Helper()
	now := time.Now().UTC()
	v := Session{ID: newID(), Owner: "owner", Request: "游戏原型的低模木箱", Status: "understanding", Created: now, LastUser: now, Model: "deepseek-v4-pro", RecoverySchemaVersion: recoveryVersion,
		Limits: Limits{Calls: 20, Submissions: 3, Clarifications: 3, Duration: 30 * time.Minute, Idle: 24 * time.Hour, Retention: 7 * 24 * time.Hour}}
	if production {
		v.Intent = &Intent{Asset: "木箱", Use: "游戏原型", MaxTriangles: 5000, MaxBytes: 10 << 20, Plan: []string{"生成并进行技术检查"}}
	}
	if err := s.store.Create(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec("UPDATE sessions SET data=json_remove(data, '$.ExecutionVersion') WHERE id=?", v.ID); err != nil {
		t.Fatal(err)
	}
	return v
}

// prepareFrozenV1Pause 用真实 Eino 先加载 v1 Skill 再执行原 JSON 工具提议。
// 写失败场景只阻断 checkpoint 提交，已持久化 Seed 包含原系统前缀与 Skill 消息。
func prepareFrozenV1Pause(t *testing.T, s *Service, production, pending bool) Session {
	t.Helper()
	v := createFrozenV1Session(t, s, production)
	if pending {
		if _, err := s.store.db.Exec("CREATE TEMP TRIGGER profile_pause_failure BEFORE INSERT ON checkpoints BEGIN SELECT RAISE(ABORT, 'controlled profile upgrade checkpoint failure'); END"); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	s.modelFactory = func(context.Context) (model.BaseChatModel, error) {
		return protocolModel{generate: func(_ context.Context, in []*schema.Message) (*schema.Message, error) {
			calls++
			if len(in) == 0 || !strings.HasPrefix(in[0].Content, instruction+"\n") {
				return nil, fmt.Errorf("frozen fixture did not use v1 instruction")
			}
			if calls == 1 {
				name := "intent"
				if production {
					name = "generation"
				}
				return protocolProposal("skill", "v1-skill", map[string]string{"skill": name}), nil
			}
			if calls != 2 {
				return nil, fmt.Errorf("unexpected fresh model decision")
			}
			if production {
				return protocolProposal("generate_asset", "v1-generate", generationInput{Prompt: "wooden crate", TargetTriangles: 5000, TextureQuality: "standard", Reason: "原始 v1 制作计划"}), nil
			}
			return protocolProposal("ask_user", "v1-question", questionInput{Question: "采用卡通还是写实风格？"}), nil
		}}, nil
	}
	c := &pauseCoordinator{s: s, id: v.ID, mode: "normal"}
	interrupted, err := s.executeRunner(context.Background(), v, c, false, []*schema.Message{schema.UserMessage(v.Request)})
	if calls != 2 || !interrupted || (pending && err == nil) || (!pending && err != nil) {
		t.Fatalf("v1 pause fixture failed: calls=%d interrupted=%t error=%v", calls, interrupted, err)
	}
	if pending {
		if _, err := s.store.db.Exec("DROP TRIGGER profile_pause_failure"); err != nil {
			t.Fatal(err)
		}
	}
	v, err = s.store.Get(context.Background(), v.ID)
	if err != nil || v.ExecutionVersion != "" || v.ModelCalls != 2 {
		t.Fatalf("fixture lost original version or counters: %+v %v", v, err)
	}
	if pending && (v.PendingPause == nil || v.ResumePoint != nil || v.PendingPause.Seed.PromptVersion != PromptVersion) {
		t.Fatal("missing independent v1 replay seed")
	}
	return v
}

func TestExecutionProfileSelectionAndSkillEvidence(t *testing.T) {
	s := testService(t, t.TempDir(), &fakeProvider{}, false)
	defer s.Close()
	old := createFrozenV1Session(t, s, false)
	fresh, err := s.Create(context.Background(), "owner", "产品展示的低模茶壶")
	if err != nil || fresh.ExecutionVersion != CurrentPromptVersion || executionVersion(old) != PromptVersion {
		t.Fatalf("new or historical version selection failed: %+v %v", fresh, err)
	}
	for _, v := range []Session{old, fresh} {
		backend := skillBackend{s: s, id: v.ID}
		loaded, err := backend.Get(context.Background(), "intent")
		if err != nil {
			t.Fatal(err)
		}
		profile, _ := resolveExecutionProfile(v)
		content, _ := profile.skillContent("intent")
		if loaded.Content != content || loaded.BaseDirectory != "embedded://skills/intent" {
			t.Fatal("skill content or logical path differs from the selected profile")
		}
		if err := s.validateExecutionVersion(context.Background(), v); err != nil {
			t.Fatal(err)
		}
		events, _ := s.store.Events(context.Background(), v.ID, 0)
		last := events[len(events)-1]
		var evidence map[string]string
		if last.Kind != "skill_loaded" || json.Unmarshal(last.Data, &evidence) != nil || evidence["version"] != tokenHash(content) || evidence["prompt_version"] != profile.Version {
			t.Fatalf("skill trace does not identify actual bytes: %+v", last)
		}
	}
}

// TestExecutionProfileUpgradePauseAndOldFinish 覆盖有/无检查点、已接受答案和排队四类暂停。
// 恢复只重放原提议；真正读到用户答案之后才允许一次新的模型决策，仍使用旧 finish JSON。
func TestExecutionProfileUpgradePauseAndOldFinish(t *testing.T) {
	for _, variant := range []string{"waiting", "accepted_answer", "pending_question", "queued", "pending_production"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			provider := &fakeProvider{}
			s := testService(t, dir, provider, false)
			production := variant == "queued" || variant == "pending_production"
			pending := strings.HasPrefix(variant, "pending_")
			if production {
				s.Config.ProductionSlots = 0
			} // 固定无生产名额，隔离暂停重建与实际执行。
			before := prepareFrozenV1Pause(t, s, production, pending)
			if variant == "accepted_answer" {
				if err := s.Answer(ctx, before.ID, "卡通"); err != nil {
					t.Fatal(err)
				}
				before, _ = s.store.Get(ctx, before.ID)
			}
			ref := before.WaitID
			if before.PendingPause != nil {
				ref = before.PendingPause.Point.RefID
			} else if production {
				ref = before.Current.ID
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = testService(t, dir, provider, false)
			if production {
				s.Config.ProductionSlots = 0
			}
			defer s.Close()
			fresh, err := s.Create(ctx, "owner", "产品展示的茶壶")
			if err != nil || fresh.ExecutionVersion != CurrentPromptVersion {
				t.Fatal("upgraded application does not default new sessions to v2")
			}
			freshCalls := 0
			s.modelFactory = func(context.Context) (model.BaseChatModel, error) {
				return protocolModel{generate: func(_ context.Context, in []*schema.Message) (*schema.Message, error) {
					freshCalls++
					if len(in) == 0 || !strings.HasPrefix(in[0].Content, instruction+"\n") {
						return nil, fmt.Errorf("old request switched to v2 during normal continuation")
					}
					found := false
					for _, msg := range in {
						found = found || (msg.Role == schema.Tool && strings.Contains(msg.Content, `"user_answer":"卡通"`))
					}
					if !found {
						return nil, fmt.Errorf("accepted answer disappeared from old message protocol")
					}
					return protocolProposal("finish_request", "v1-finish", json.RawMessage(`{"deliver":false,"explanation":"已完成绑定和动画，预算已用完"}`)), nil
				}}, nil
			}
			if err := s.run(ctx, before.ID); err != nil {
				t.Fatal(err)
			}
			after, err := s.store.Get(ctx, before.ID)
			if err != nil || executionVersion(after) != PromptVersion || provider.Count() != 0 || after.Production != before.Production || !after.Deadline.Equal(before.Deadline) {
				t.Fatalf("upgrade changed production, version or budget: %+v %v", after, err)
			}
			if variant != "accepted_answer" && (freshCalls != 0 || after.ModelCalls != before.ModelCalls) {
				t.Fatal("pure resume or reconstruction consumed a new model decision")
			}
			if production {
				if after.Current == nil || after.Current.ID != ref || after.Current.Params.Prompt != "wooden crate" || after.Current.Params.FaceLimit != 5000 || after.Status != "queued" {
					t.Fatalf("old queued operation changed: %+v", after)
				}
				if !pending && !after.Queued.Equal(before.Queued) {
					t.Fatal("resume changed original queue order")
				}
				assertStoreEventCount(t, s.store, after.ID, "runtime_accepted", 1)
				return
			}
			if variant != "accepted_answer" {
				if after.Status != "awaiting_answer" || after.WaitID != ref || after.Question != "采用卡通还是写实风格？" || after.Clarifications != 1 {
					t.Fatalf("old question was changed or repeated: %+v", after)
				}
				if err := s.Answer(ctx, after.ID, "卡通"); err != nil {
					t.Fatal(err)
				}
				if err := s.run(ctx, after.ID); err != nil {
					t.Fatal(err)
				}
				after, _ = s.store.Get(ctx, after.ID)
			}
			if freshCalls != 1 || after.ModelCalls != before.ModelCalls+1 || !after.Terminal() || after.Status != "failed" || after.Answers[ref].Text != "卡通" || after.Clarifications != 1 || strings.Contains(after.Final, "已完成绑定") || strings.Contains(after.Final, "预算已用完") {
				t.Fatalf("old finish protocol or result evidence failed: calls=%d session=%+v", freshCalls, after)
			}
			events, _ := s.store.Events(ctx, after.ID, 0)
			if !strings.Contains(jsonString(events), "已完成绑定和动画，预算已用完") {
				t.Fatal("original model explanation was removed from evidence")
			}
		})
	}
}

// TestExecutionProfileRejectsConflictingEvidence 检查完整 checkpoint 无 Seed 时也拒绝版本矛盾。
// 校验失败不能创建模型客户端、触碰原检查点或重置已持久化预算。
func TestExecutionProfileRejectsConflictingEvidence(t *testing.T) {
	for _, variant := range []string{"unknown_session", "conflicting_event", "conflicting_skill", "seed_profile", "damaged_input"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			s := testService(t, t.TempDir(), &fakeProvider{}, false)
			defer s.Close()
			v := prepareFrozenV1Pause(t, s, false, variant == "seed_profile" || variant == "damaged_input")
			switch variant {
			case "unknown_session":
				v, _ = s.store.Edit(ctx, v.ID, func(x *Session) error { x.ExecutionVersion = "asset-agent-future"; return nil }, "", nil)
			case "conflicting_event":
				if err := s.event(v.ID, "model_call", map[string]string{"prompt_version": CurrentPromptVersion}); err != nil {
					t.Fatal(err)
				}
			case "conflicting_skill":
				if err := s.event(v.ID, "skill_loaded", map[string]string{"name": "intent", "version": "unknown-content"}); err != nil {
					t.Fatal(err)
				}
			case "seed_profile":
				v, _ = s.store.Edit(ctx, v.ID, func(x *Session) error { x.PendingPause.Seed.PromptVersion = CurrentPromptVersion; return nil }, "", nil)
			case "damaged_input":
				v, _ = s.store.Edit(ctx, v.ID, func(x *Session) error { x.PendingPause.Seed.Input[0].Content += "改写"; return nil }, "", nil)
			}
			before := jsonString(v)
			checkpoint, checkpointErr := s.store.LoadCheckpoint(ctx, v.ID, v.ID)
			factories := 0
			s.modelFactory = func(context.Context) (model.BaseChatModel, error) { factories++; return scriptModel{}, nil }
			if err := s.run(ctx, v.ID); err == nil {
				t.Fatal("unprovable execution profile was accepted")
			}
			after, _ := s.store.Get(ctx, v.ID)
			newCheckpoint, newErr := s.store.LoadCheckpoint(ctx, v.ID, v.ID)
			if factories != 0 || before != jsonString(after) || !reflect.DeepEqual(checkpoint, newCheckpoint) || (checkpointErr == nil) != (newErr == nil) {
				t.Fatal("rejection mutated original evidence or invoked model factory")
			}
		})
	}
}

// TestExecutionProfileUpgradeProductionEvidence 使用 v1 的真实生产暂停和原任务证据。
// 已知任务仅续查，未知提交保守收尾；两个分支都不能以升级为由重新发送 POST。
func TestExecutionProfileUpgradeProductionEvidence(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(fmt.Sprintf("known_task_%t", known), func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			s := testService(t, dir, &fakeProvider{}, false)
			before := prepareFrozenV1Pause(t, s, true, false)
			deadline := time.Now().UTC().Add(10 * time.Minute)
			before, err := s.store.Edit(ctx, before.ID, func(v *Session) error {
				v.Production, v.Deadline = 1, deadline
				v.Current.Stage = "submitting"
				if known {
					v.Current.Stage, v.Current.TaskID = "submitted", "original-v1-task"
				}
				return nil
			}, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = testService(t, dir, &fakeProvider{}, false)
			defer s.Close()
			provider := &productionRecoveryProvider{}
			s.provider = provider
			calls := 0
			s.modelFactory = func(context.Context) (model.BaseChatModel, error) {
				return protocolModel{generate: func(_ context.Context, in []*schema.Message) (*schema.Message, error) {
					calls++
					if !strings.HasPrefix(in[0].Content, instruction+"\n") {
						return nil, fmt.Errorf("known task continuation changed prompt version")
					}
					v, err := s.store.Get(ctx, before.ID)
					if err != nil || len(v.Artifacts) != 1 {
						return nil, fmt.Errorf("known task result not saved before finish")
					}
					args := `{"deliver":true,"artifact_id":"` + v.Artifacts[0].ID + `","explanation":"已完成绑定和动画，模型只有1000面"}`
					return protocolProposal("finish_request", "old-json-delivery", json.RawMessage(args)), nil
				}}, nil
			}
			err = s.run(ctx, before.ID)
			if known && err != nil {
				t.Fatal(err)
			}
			if !known {
				if err == nil || !strings.Contains(err.Error(), "submission_outcome_unknown") {
					t.Fatalf("unknown submission was allowed to continue: %v", err)
				}
				if err := s.finalize(before.ID, err); err != nil {
					t.Fatal(err)
				}
			}
			after, err := s.store.Get(ctx, before.ID)
			if err != nil || !after.Terminal() || provider.submits != 0 || after.Production != 1 || after.Current.ID != before.Current.ID || !after.Deadline.Equal(deadline) || executionVersion(after) != PromptVersion {
				t.Fatal("upgrade changed original operation, deadline or submission budget")
			}
			if known {
				if calls != 1 || provider.queries != 1 || provider.downloads != 1 || after.Current.TaskID != "original-v1-task" || after.Status != "completed" || after.SelectedArtifact != before.Current.ID || strings.Contains(after.Final, "已完成绑定") || strings.Contains(after.Final, "1000面") {
					t.Fatal("known task or original finish JSON did not preserve truthful delivery")
				}
			} else if calls != 0 || provider.queries != 0 || provider.downloads != 0 || after.ModelCalls != before.ModelCalls || after.Current.TaskID != "" || after.Status != "failed" {
				t.Fatal("unknown submission was queried, retried or re-decided")
			}
		})
	}
}
