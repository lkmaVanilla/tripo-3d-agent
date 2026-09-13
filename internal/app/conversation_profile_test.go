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

// 两个指纹独立采自 c89d7e6 的 git archive：旧工具/Skill 配置与固定旧运行视图。
// 基线没有调用新会话入口或 v3 类型，避免测试与被测协议一起漂移。
func TestConversationUpgradeV2FrozenConfiguration(t *testing.T) {
	profile, err := profileForVersion(CurrentPromptVersion)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := (&Service{}).tools("", &pauseCoordinator{mode: "normal", profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	entries := []any{}
	for _, tool := range tools {
		info, e := tool.Info(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		params, e := info.ParamsOneOf.ToJSONSchema()
		if e != nil {
			t.Fatal(e)
		}
		entries = append(entries, map[string]any{"name": info.Name, "description": info.Desc, "schema": params})
	}
	for _, name := range []string{"intent", "generation", "correction"} {
		content, e := profile.skillContent(name)
		if e != nil {
			t.Fatal(e)
		}
		description, _ := profile.skillDescription(name)
		entries = append(entries, map[string]any{"name": name, "description": description, "content": content, "base_directory": "embedded://skills/" + name})
	}
	got := tokenHash(jsonString(map[string]any{"instruction": profile.Instruction, "description": profile.Description, "entries": entries}))
	const baseline = "6849f833b97ef679c79291ae47d8159903409b9e9584b52b1a44926a1d64929e"
	if got != baseline {
		t.Fatalf("v2 model-visible configuration no longer matches c89d7e6: %s", got)
	}
	v := Session{ID: "frozen-v2-run", Owner: "owner", Request: "产品展示的低模木箱", Status: "awaiting_answer", ExecutionVersion: CurrentPromptVersion, Model: "deepseek-v4-pro", Question: "采用卡通还是写实风格？", WaitID: "frozen-wait", Clarifications: 1, ModelCalls: 2, Limits: Limits{Calls: 20, Submissions: 3, Clarifications: 3}}
	const viewBaseline = "e7591b72c1ecc4cb0bed6ce3d5643e3df8b7c7a2b4d446affc1b32704d856b8c"
	if got = tokenHash(jsonString(v.View())); got != viewBaseline {
		t.Fatalf("v2 runtime_state view no longer matches c89d7e6: %s", got)
	}
}

// prepareFrozenV2Pause 使用真实 Eino 保存旧版 Skill/工具协议；并独立制造提交前草案。
func prepareFrozenV2Pause(t *testing.T, s *Service, production, pending bool) Session {
	t.Helper()
	now := time.Now().UTC()
	v := Session{ID: newID(), Owner: "owner", Request: "产品展示的低模木箱", Status: "understanding", Created: now, LastUser: now, Model: "deepseek-v4-pro", ExecutionVersion: CurrentPromptVersion, RecoverySchemaVersion: recoveryVersion, Limits: Limits{Calls: 20, Submissions: 3, Clarifications: 3, Duration: 30 * time.Minute, Idle: 24 * time.Hour, Retention: 7 * 24 * time.Hour}}
	if production {
		v.Intent = &Intent{Asset: "木箱", Use: "产品展示", MaxTriangles: 5000, MaxBytes: 10 << 20, Plan: []string{"生成并进行技术检查"}}
	}
	if err := s.store.Create(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	if pending {
		if _, err := s.store.db.Exec("CREATE TEMP TRIGGER v2_pause_failure BEFORE INSERT ON checkpoints BEGIN SELECT RAISE(ABORT, 'controlled v2 checkpoint write failure'); END"); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	s.modelFactory = func(context.Context) (model.BaseChatModel, error) {
		return protocolModel{generate: func(_ context.Context, in []*schema.Message) (*schema.Message, error) {
			calls++
			if len(in) == 0 || !strings.HasPrefix(in[0].Content, instructionV2+"\n") {
				return nil, fmt.Errorf("fixture did not use the frozen v2 instruction")
			}
			// 新会话输入、目标种类或回答结局不能出现在旧 Runner 的 runtime_state。
			raw := strings.Split(strings.Split(in[0].Content, "<runtime_state>")[1], "</runtime_state>")[0]
			var state map[string]any
			if err := json.Unmarshal([]byte(raw), &state); err != nil {
				return nil, err
			}
			for _, field := range []string{"conversation_context", "input_version", "input_assessment", "goal_kind", "outcome", "generation"} {
				if _, ok := state[field]; ok {
					return nil, fmt.Errorf("v3 field %s leaked into frozen v2 state", field)
				}
			}
			if calls == 1 {
				name := "intent"
				if production {
					name = "generation"
				}
				return protocolProposal("skill", "v2-skill", map[string]string{"skill": name}), nil
			}
			if calls != 2 {
				return nil, fmt.Errorf("unexpected fresh decision in fixture")
			}
			if production {
				return protocolProposal("generate_asset", "v2-generate", generationInput{Prompt: "wooden crate", TargetTriangles: 5000, TextureQuality: "standard", Reason: "原始 v2 制作计划"}), nil
			}
			return protocolProposal("ask_user", "v2-question", questionInput{Question: "采用卡通还是写实风格？"}), nil
		}}, nil
	}
	coordinator := &pauseCoordinator{s: s, id: v.ID, mode: "normal"}
	interrupted, err := s.executeRunner(context.Background(), v, coordinator, false, []*schema.Message{schema.UserMessage(v.Request)})
	if calls != 2 || !interrupted || (pending && err == nil) || (!pending && err != nil) {
		t.Fatalf("v2 fixture calls=%d interrupted=%t err=%v", calls, interrupted, err)
	}
	if pending {
		if _, err := s.store.db.Exec("DROP TRIGGER v2_pause_failure"); err != nil {
			t.Fatal(err)
		}
	}
	v, err = s.store.Get(context.Background(), v.ID)
	if err != nil || v.ExecutionVersion != CurrentPromptVersion || v.ModelCalls != 2 {
		t.Fatalf("v2 fixture changed profile or budget: %+v %v", v, err)
	}
	if pending && (v.PendingPause == nil || v.PendingPause.Seed == nil || v.PendingPause.Seed.PromptVersion != CurrentPromptVersion) {
		t.Fatal("v2 pending fixture lacks frozen replay seed")
	}
	return v
}

// TestConversationUpgradeV2PauseProtocol 验证包装会话后，五种旧暂停仍使用原执行身份。
// 纯暂停重建不作新决策；只有取到原答案才允许按原 finish_request 协议收尾。
func TestConversationUpgradeV2PauseProtocol(t *testing.T) {
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
			}
			before := prepareFrozenV2Pause(t, s, production, pending)
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
			originalJSON := jsonString(before)
			originalCheckpoint, checkpointErr := s.store.LoadCheckpoint(ctx, before.ID, before.ID)
			seedHash := ""
			if pending {
				seedHash = before.PendingPause.Seed.InputHash
				if err := validateReplaySeed(before.PendingPause.Seed, CurrentPromptVersion); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = testService(t, dir, provider, false)
			defer s.Close()
			if production {
				s.Config.ProductionSlots = 0
			}
			migrated, err := s.store.Get(ctx, before.ID)
			if err != nil {
				t.Fatal(err)
			}
			restoredCheckpoint, restoredErr := s.store.LoadCheckpoint(ctx, before.ID, before.ID)
			if originalJSON != jsonString(migrated) || (checkpointErr == nil) != (restoredErr == nil) || !reflect.DeepEqual(originalCheckpoint, restoredCheckpoint) {
				t.Fatal("conversation migration changed frozen v2 execution or recovery bytes")
			}
			if pending && migrated.PendingPause.Seed.InputHash != seedHash {
				t.Fatal("migration changed the frozen ReplaySeed.InputHash")
			}
			conversation, err := s.store.GetRunConversation(ctx, before.ID)
			if err != nil || conversation.ID == "" {
				t.Fatalf("v2 run lost wrapping conversation: %+v %v", conversation, err)
			}
			_, fresh, err := s.CreateAssetConversation(ctx, "owner", "另一个产品展示资产", "new-v3-"+variant)
			if err != nil || fresh.ExecutionVersion != ConversationPromptVersion {
				t.Fatalf("new conversation did not use a separate v3 execution: %+v %v", fresh, err)
			}
			freshCalls := 0
			s.modelFactory = func(context.Context) (model.BaseChatModel, error) {
				return protocolModel{generate: func(_ context.Context, in []*schema.Message) (*schema.Message, error) {
					freshCalls++
					if len(in) == 0 || !strings.HasPrefix(in[0].Content, instructionV2+"\n") {
						return nil, fmt.Errorf("resuming v2 used another instruction")
					}
					answered := false
					for _, message := range in {
						answered = answered || (message.Role == schema.Tool && strings.Contains(message.Content, `"user_answer":"卡通"`))
					}
					if !answered {
						return nil, fmt.Errorf("original v2 answer missing from restored tool protocol")
					}
					return protocolProposal("finish_request", "v2-finish", json.RawMessage(`{"deliver":false,"artifact_id":"","explanation":"已完成绑定和动画，预算已用完"}`)), nil
				}}, nil
			}
			if err = s.run(ctx, before.ID); err != nil {
				t.Fatal(err)
			}
			after, err := s.store.Get(ctx, before.ID)
			if err != nil || after.ExecutionVersion != CurrentPromptVersion || provider.Count() != 0 || after.Production != before.Production || !after.Deadline.Equal(before.Deadline) {
				t.Fatalf("v2 continuation changed budget/profile/side effects: %+v %v", after, err)
			}
			if variant != "accepted_answer" && (freshCalls != 0 || after.ModelCalls != before.ModelCalls) {
				t.Fatal("pure v2 resume/reconstruction invoked fresh model decisions")
			}
			if production {
				if after.Current == nil || after.Current.ID != ref || after.Current.Params.Prompt != "wooden crate" || after.Current.Params.FaceLimit != 5000 || after.Status != "queued" {
					t.Fatalf("queued v2 proposal changed: %+v", after)
				}
				if !pending && !after.Queued.Equal(before.Queued) {
					t.Fatal("v2 queue position changed")
				}
				assertStoreEventCount(t, s.store, after.ID, "runtime_accepted", 1)
				return
			}
			if variant != "accepted_answer" {
				if after.Status != "awaiting_answer" || after.WaitID != ref || after.Question != "采用卡通还是写实风格？" || after.Clarifications != 1 {
					t.Fatalf("v2 question changed or repeated: %+v", after)
				}
				if err = s.Answer(ctx, after.ID, "卡通"); err != nil {
					t.Fatal(err)
				}
				if err = s.run(ctx, after.ID); err != nil {
					t.Fatal(err)
				}
				after, _ = s.store.Get(ctx, after.ID)
			}
			if freshCalls != 1 || after.ModelCalls != before.ModelCalls+1 || !after.Terminal() || after.Status != "failed" || after.Answers[ref].Text != "卡通" || after.Clarifications != 1 || after.Outcome != nil || strings.Contains(after.Final, "已完成绑定") || strings.Contains(after.Final, "预算已用完") {
				t.Fatalf("v2 finish or evidence protocol changed: calls=%d run=%+v", freshCalls, after)
			}
			events, _ := s.store.Events(ctx, after.ID, 0)
			if !strings.Contains(jsonString(events), "已完成绑定和动画，预算已用完") {
				t.Fatal("v2 original proposal missing from evidence")
			}
		})
	}
}
