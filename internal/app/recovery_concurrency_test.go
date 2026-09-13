package app

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// TestRecoveryExpirationUsesLatestPersistedFacts 复现调度快照落后于数据库的窗口，
// 验证过期事务依据最新事实判断，不覆盖刚接受的答案、任务 ID 或终止结果。
func TestRecoveryExpirationUsesLatestPersistedFacts(t *testing.T) {
	for _, update := range []string{"stop", "answer-last-user", "production-started", "already-expired"} {
		t.Run(update, func(t *testing.T) {
			s, original, _ := productionRecoveryFixture(t, "ready")
			stale := editProductionFixture(t, s, original.ID, func(v *Session) {
				v.LastUser = time.Now().UTC().Add(-25 * time.Hour)
				v.Limits.Idle = 24 * time.Hour
			})
			if checkExecution(stale, time.Now()) == nil {
				t.Fatal("fixture must reproduce a stale expired List result")
			}
			latest := editProductionFixture(t, s, original.ID, func(v *Session) {
				// 这些更新发生在调度器读取列表之后、过期事务读取会话之前。
				switch update {
				case "stop":
					v.Finish("stopped", "newer user stop")
				case "answer-last-user":
					v.LastUser = time.Now().UTC()
					v.Answers = map[string]AnswerRecord{"wait-a": {Text: "卡通", Accepted: v.LastUser}}
					v.ResumeRequested = true
				case "production-started":
					v.Production = 2
					v.ModelCalls = 17
					v.Deadline = time.Now().UTC().Add(19 * time.Minute)
					v.Current.Stage, v.Current.TaskID = "submitted", "new-known-task"
				case "already-expired":
					v.Finish("failed", "already ended by another executor")
				}
			})
			got, expired, err := s.expireIfDue(context.Background(), stale.ID)
			if err != nil || expired || !reflect.DeepEqual(got, latest) {
				t.Fatalf("stale expiry overwrote newer state: expired=%t err=%v\nlatest=%+v\ngot=%+v", expired, err, latest, got)
			}
			saved := getProductionFixture(t, s, stale.ID)
			if !reflect.DeepEqual(saved, latest) || productionEventCount(t, s, stale.ID, "expired") != 0 {
				t.Fatalf("rejected expiry persisted changes or event: %+v", saved)
			}
		})
	}
}

// TestRecoveryKnownTaskWithUnusableExistingPause 验证旧暂停迁移中断且检查点损坏时，
// 已持久化的远端任务仍可走确定性续查，不依赖模型重做决策或重新提交。
func TestRecoveryKnownTaskWithUnusableExistingPause(t *testing.T) {
	f := newLegacyCheckpointFixture(t, legacyKnownTask)
	p := &startupProvider{}
	var calls atomic.Int64
	s := startupService(t, f.Dir, p, &calls)
	defer s.Close()
	before := editProductionFixture(t, s, f.Key, func(v *Session) {
		// 模拟已核对旧工具身份、但尚未替换 Eino 检查点时执行进程退出。
		v.PendingPause = &PendingPause{Point: ResumePoint{Version: recoveryVersion, SessionID: v.ID, Key: v.ID, Generation: 1, PauseID: "existing-legacy-pause", Kind: "production", RefID: v.Current.ID}, Existing: true, Legacy: true}
		v.PendingPause.DraftHash = draftHash(v.PendingPause)
	})
	if err := validatePending(before); err != nil {
		t.Fatalf("migration prepare must be structurally valid: %v", err)
	}
	if _, err := s.store.db.Exec("UPDATE checkpoints SET data=? WHERE session_id=?", []byte("unreadable-existing-eino-checkpoint"), f.Key); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcile(); err != nil {
		t.Fatal(err)
	}
	if err := s.run(context.Background(), f.Key); err != nil {
		t.Fatalf("known task abandoned because dependent old checkpoint was unusable: %v", err)
	}
	after := getProductionFixture(t, s, f.Key)
	if after.Status != "completed" || after.Current.TaskID != before.Current.TaskID || after.Production != before.Production || !after.Deadline.Equal(before.Deadline) || p.Count() != 0 || p.queries.Load() == 0 || calls.Load() != 0 {
		t.Fatalf("known task lost, resubmitted or re-decided: %+v submits=%d queries=%d model=%d", after, p.Count(), p.queries.Load(), calls.Load())
	}
}

// TestRecoveryExpirationIsRecordedOnceWithoutResettingEvidence 验证重复过期处理只记录一次，
// 且额度、原期限和已知操作继续保留为恢复与追踪证据。
func TestRecoveryExpirationIsRecordedOnceWithoutResettingEvidence(t *testing.T) {
	for _, limit := range []string{"production-deadline", "idle"} {
		t.Run(limit, func(t *testing.T) {
			s, original, _ := productionRecoveryFixture(t, "submitted")
			before := editProductionFixture(t, s, original.ID, func(v *Session) {
				v.Production, v.ModelCalls, v.Clarifications = 2, 18, 2
				if limit == "production-deadline" {
					v.Deadline = time.Now().UTC().Add(-time.Second)
				} else {
					v.Deadline = time.Time{}
					v.LastUser = time.Now().UTC().Add(-v.Limits.Idle - time.Second)
				}
			})
			after, expired, err := s.expireIfDue(context.Background(), before.ID)
			if err != nil || !expired || after.Status != "failed" || !after.Terminal() || after.HasSlot {
				t.Fatalf("expiry not committed: %+v expired=%t err=%v", after, expired, err)
			}
			if after.Production != before.Production || after.ModelCalls != before.ModelCalls || after.Clarifications != before.Clarifications || !after.Deadline.Equal(before.Deadline) || !after.LastUser.Equal(before.LastUser) || !reflect.DeepEqual(after.Current, before.Current) {
				t.Fatalf("expiry reset quota, deadline or operation: %+v", after)
			}
			again, expired, err := s.expireIfDue(context.Background(), before.ID)
			if err != nil || expired || !reflect.DeepEqual(after, again) || productionEventCount(t, s, before.ID, "expired") != 1 {
				t.Fatalf("repeated expiry changed retention/evidence: %+v expired=%t err=%v", again, expired, err)
			}
		})
	}
}
