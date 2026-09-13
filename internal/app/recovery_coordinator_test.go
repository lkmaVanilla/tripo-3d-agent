package app

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/asset"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/testfixture"
	"modernc.org/sqlite"
)

// coordinatorFixture 仅持久化一个待提交问题草案，尚未发布等待状态或消费澄清次数。
// 后续直接调用协调器，隔离原子发布逻辑与模型、调度器及远端生产。
func coordinatorFixture(t *testing.T) (*Service, *pauseCoordinator, Session) {
	t.Helper()
	s := testService(t, t.TempDir(), &fakeProvider{}, true)
	t.Cleanup(func() { s.Close() })
	v, err := s.Create(context.Background(), "owner", "木箱")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := newReplaySeed([]*schema.Message{schema.UserMessage(v.Request)}, protocolProposal("ask_user", "call", questionInput{Question: "采用什么风格？"}), v.Model, executionVersion(v))
	if err != nil {
		t.Fatal(err)
	}
	p := &PendingPause{Point: ResumePoint{Version: recoveryVersion, SessionID: v.ID, Key: v.ID, PauseID: newID(), RefID: newID(), Kind: "question", Generation: 1}, Question: "采用什么风格？", Seed: seed}
	p.DraftHash = draftHash(p)
	v, err = s.store.Edit(context.Background(), v.ID, func(x *Session) error { x.PendingPause = p; return nil }, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, &pauseCoordinator{s: s, id: v.ID, mode: "rebuild", seed: seed, pause: p}, v
}

// TestRecoveryCoordinatorBoundedWriteRetry 验证短暂写故障可在有限次数内恢复，
// 持续失败则保留草案并明确报错，不能发布没有检查点支撑的问题。
func TestRecoveryCoordinatorBoundedWriteRetry(t *testing.T) {
	for _, failures := range []int{2, 99} {
		t.Run(fmt.Sprint(failures), func(t *testing.T) {
			var calls atomic.Int32
			name := "checkpoint_retry_" + newID()
			// 用真实 SQLite 触发器在写入点失败；唯一函数名避免进程级注册表串扰其他用例。
			if err := sqlite.RegisterScalarFunction(name, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
				if calls.Add(1) <= int32(failures) {
					return nil, errors.New("controlled write failure")
				}
				return int64(1), nil
			}); err != nil {
				t.Fatal(err)
			}
			s, c, before := coordinatorFixture(t)
			if _, err := s.store.db.Exec("CREATE TEMP TRIGGER fail_checkpoint BEFORE INSERT ON checkpoints BEGIN SELECT " + name + "(); END"); err != nil {
				t.Fatal(err)
			}
			if before.Status != "understanding" || before.Clarifications != 0 || before.ResumePoint != nil {
				t.Fatal("pending was published early")
			}
			err := c.Set(context.Background(), before.ID, []byte("opaque bytes from SDK"))
			after, e := s.store.Get(context.Background(), before.ID)
			if e != nil || calls.Load() != 3 {
				t.Fatalf("unbounded retry: calls=%d error=%v", calls.Load(), e)
			}
			if failures == 2 {
				if err != nil || after.PendingPause != nil || after.ResumePoint == nil || after.Status != "awaiting_answer" || after.Clarifications != 1 || !c.committed {
					t.Fatalf("retry did not commit once: %+v %v", after, err)
				}
				assertStoreEventCount(t, s.store, before.ID, "clarification", 1)
			} else {
				if err == nil || !strings.Contains(err.Error(), "checkpoint_write_failed") || after.PendingPause == nil || after.ResumePoint != nil || after.Status != "understanding" || after.Clarifications != 0 || c.committed {
					t.Fatalf("failed write was published: %+v %v", after, err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatal("failed commit changed persisted state")
				}
				if e = s.finalize(before.ID, err); e != nil {
					t.Fatal(e)
				}
				assertStoreEventCount(t, s.store, before.ID, "recovery_rejected", 1)
			}
		})
	}
}

// TestRecoveryCoordinatorStopCommitRace 让停止与检查点提交竞争同一会话，
// 验证两种执行顺序最终均保持停止，迟到回调也不能重新开始保留期限。
func TestRecoveryCoordinatorStopCommitRace(t *testing.T) {
	for i := 0; i < 12; i++ {
		s, c, v := coordinatorFixture(t)
		gate := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-gate; _ = c.Set(context.Background(), v.ID, []byte("opaque bytes")) }()
		go func() { defer wg.Done(); <-gate; _ = s.Stop(context.Background(), v.ID) }()
		close(gate)
		wg.Wait()
		after, err := s.store.Get(context.Background(), v.ID)
		if err != nil || after.Status != "stopped" || !after.Terminal() || after.HasSlot || after.Production != 0 || after.ModelCalls != 0 || after.Clarifications > 1 {
			t.Fatalf("stop lost race: %+v %v", after, err)
		}
		ended, expires := after.Ended, after.Expires
		if err = c.Set(context.Background(), v.ID, []byte("late bytes")); err == nil {
			t.Fatal("late callback accepted")
		}
		after, _ = s.store.Get(context.Background(), v.ID)
		if !after.Ended.Equal(ended) || !after.Expires.Equal(expires) {
			t.Fatal("late callback reset retention")
		}
	}
}

// TestRecoveryQueueFIFOAfterRestart 验证重启沿用已入队操作和 FIFO 时间，
// 队满请求需显式重试，等待阶段不消费生产额度或启动生产期限。
func TestRecoveryQueueFIFOAfterRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	p := &fakeProvider{blocked: true}
	s := testService(t, dir, p, false)
	// 先关闭所有生产槽，使三个请求稳定停在队列边界，再以单槽恢复验证先后顺序。
	s.Config.ProductionSlots = 0
	s.Config.QueueSize = 2
	var queued []Session
	for i := 0; i < 3; i++ {
		v, err := s.Create(ctx, "owner", fmt.Sprintf("木箱%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if err = s.run(ctx, v.ID); err != nil {
			t.Fatal(err)
		}
		v, _ = s.store.Get(ctx, v.ID)
		queued = append(queued, v)
	}
	if queued[0].Status != "queued" || queued[1].Status != "queued" || queued[2].Status != "queue_full" {
		t.Fatalf("bad queue: %+v", queued)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = testService(t, dir, p, false)
	s.Config.ProductionSlots = 1
	s.Config.QueueSize = 2
	defer s.Close()
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	first := waitSession(t, s, queued[0].ID, func(v Session) bool { return v.Current.TaskID != "" })
	second, _ := s.store.Get(ctx, queued[1].ID)
	if first.Current.ID != queued[0].Current.ID || !first.Queued.Equal(queued[0].Queued) || second.Status != "queued" || !second.Queued.Equal(queued[1].Queued) || !second.Deadline.IsZero() || second.Production != 0 {
		t.Fatal("restart reordered queue or consumed waiting budget")
	}
	if err := s.RetryQueue(ctx, queued[2].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RetryQueue(ctx, queued[2].ID); err == nil {
		t.Fatal("duplicate retry accepted")
	}
	if err := s.Stop(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	second = waitSession(t, s, queued[1].ID, func(v Session) bool { return v.Current.TaskID != "" })
	third, _ := s.store.Get(ctx, queued[2].ID)
	if second.Current.ID != queued[1].Current.ID || third.Status != "queued" || third.Production != 0 || third.Current.ID != queued[2].Current.ID || p.Count() != 2 {
		t.Fatal("FIFO or operation identity lost")
	}
	assertStoreEventCount(t, s.store, third.ID, "queued", 1)
}

// TestRecoveryStartupStorageFailureStopsAdmission 验证恢复对账无法读取存储时不接收新请求。
func TestRecoveryStartupStorageFailureStopsAdmission(t *testing.T) {
	s := testService(t, t.TempDir(), &fakeProvider{}, false)
	if err := s.store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err == nil {
		t.Fatal("unavailable storage reported healthy startup")
	}
	if _, err := s.Create(context.Background(), "owner", "木箱"); err == nil || !strings.Contains(err.Error(), "恢复对账") {
		t.Fatalf("admission not gated: %v", err)
	}
	s.cancel()
}

// TestRecoveryBudgetFinalizationCannotDeliverAfterDeadline 验证期限优先于预算耗尽后的兜底交付，
// 即使数据库已有合格报告，过期请求也不能再被标成成功。
func TestRecoveryBudgetFinalizationCannotDeliverAfterDeadline(t *testing.T) {
	s, c, v := coordinatorFixture(t)
	_ = c
	deadline := time.Now().Add(-time.Second)
	_, err := s.store.Edit(context.Background(), v.ID, func(x *Session) error {
		x.PendingPause = nil
		x.ModelCalls = x.Limits.Calls
		x.Deadline = deadline
		x.Intent = &Intent{Asset: "木箱", MaxTriangles: 5000, MaxBytes: 10 << 20}
		x.Artifacts = []Artifact{{ID: "result", TaskID: "saved-task", Report: asset.Inspect(testfixture.Cube(12), 5000, 10<<20)}}
		return nil
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.store.Get(context.Background(), v.ID)
	if !deliverable(before, "result") {
		t.Fatal("fixture must have complete delivery evidence before applying the expired deadline")
	}
	if err = s.finalize(v.ID, ErrBudget); err != nil {
		t.Fatal(err)
	}
	after, _ := s.store.Get(context.Background(), v.ID)
	if after.Status != "failed" || after.SelectedArtifact != "" || after.Result == nil || after.Result.Reason != "execution_deadline" || !after.Deadline.Equal(deadline) {
		t.Fatalf("delivered expired result: %+v", after)
	}
}

// TestRecoveryIntentReplayPreservesConstraints 仅允许恢复时重放完全相同的已接受意图，
// 防止普通调用、暂停重建或篡改后的参数借幂等入口放宽硬约束。
func TestRecoveryIntentReplayPreservesConstraints(t *testing.T) {
	for _, mode := range []string{"same-resume", "different-resume", "normal-call", "rebuild"} {
		t.Run(mode, func(t *testing.T) {
			s, _, v := coordinatorFixture(t)
			intent := Intent{Asset: "木箱", Use: "游戏原型", Constraints: []string{"静态道具"}, MaxTriangles: 5000, MaxBytes: 10 << 20, Plan: []string{"生成并技术检查"}}
			_, err := s.store.Edit(context.Background(), v.ID, func(x *Session) error { x.Intent = &intent; x.PendingPause = nil; return nil }, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			c := &pauseCoordinator{s: s, id: v.ID, mode: "normal", resuming: mode != "normal-call"}
			if mode == "rebuild" {
				c.mode = "rebuild"
			}
			args := intent
			if mode == "different-resume" {
				args.MaxTriangles = 10000
			}
			tools, err := s.tools(v.ID, c)
			if err != nil {
				t.Fatal(err)
			}
			for _, base := range tools {
				info, e := base.Info(context.Background())
				if e != nil {
					t.Fatal(e)
				}
				if info.Name != "set_intent" {
					continue
				}
				result, e := base.(tool.InvokableTool).InvokableRun(context.Background(), jsonString(args))
				if mode == "same-resume" {
					if e != nil || result != jsonString(intent) {
						t.Fatalf("same accepted intent not replayed: %s %v", result, e)
					}
				} else if e == nil && !strings.Contains(result, "error") {
					t.Fatal("invalid intent replay accepted")
				}
			}
			after, _ := s.store.Get(context.Background(), v.ID)
			if !reflect.DeepEqual(after.Intent, &intent) {
				t.Fatal("replay changed hard constraints")
			}
			assertStoreEventCount(t, s.store, v.ID, "intent_and_plan", 0)
		})
	}
}
