package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/asset"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/testfixture"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
)

// productionRecoveryProvider 用可替换回调精确控制生产阶段，并记录调用次数。
// 默认返回本地合成 GLB；本文件通过直接调用生产逻辑验证恢复，不访问真实 Tripo。
type productionRecoveryProvider struct {
	submit                      func(context.Context) (string, error)
	query                       func(context.Context, string) (tripo.Task, error)
	download                    func(context.Context) ([]byte, error)
	submits, queries, downloads int
}

func (p *productionRecoveryProvider) Submit(ctx context.Context, _ string, _ tripo.Params) (string, error) {
	p.submits++
	if p.submit != nil {
		return p.submit(ctx)
	}
	return "known-task", nil
}
func (p *productionRecoveryProvider) Query(ctx context.Context, id string) (tripo.Task, error) {
	p.queries++
	if p.query != nil {
		return p.query(ctx, id)
	}
	task := tripo.Task{ID: id, Status: "success", Progress: 100}
	task.Output.ModelURL = "https://fixture.example/model.glb"
	return task, nil
}
func (p *productionRecoveryProvider) Download(ctx context.Context, _ string) ([]byte, error) {
	p.downloads++
	if p.download != nil {
		return p.download(ctx)
	}
	return testfixture.Cube(12), nil
}

// productionRecoveryFixture 直接持久化给定阶段的操作，跳过模型决策和调度过程。
// 非 ready 阶段预先记录已消费的次数与期限；模型入口一旦被调用就让测试失败。
func productionRecoveryFixture(t *testing.T, stage string) (*Service, Session, *productionRecoveryProvider) {
	t.Helper()
	c := DefaultConfig()
	c.DataDir = t.TempDir()
	c.PollInterval = time.Millisecond
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	p := &productionRecoveryProvider{}
	s.provider = p
	s.modelFactory = func(context.Context) (model.BaseChatModel, error) {
		t.Error("deterministic production recovery called the model")
		return nil, errors.New("model must not be called")
	}
	now := time.Now().UTC()
	v := Session{ID: newID(), Owner: "owner", Request: "卡通木箱", Status: "running", Created: now.Add(-time.Minute), LastUser: now, HasSlot: true,
		Intent:  &Intent{Asset: "木箱", MaxTriangles: 5000, MaxBytes: 10 << 20},
		Limits:  Limits{Calls: 20, Submissions: 3, Clarifications: 3, Duration: 30 * time.Minute, Idle: 24 * time.Hour, Retention: 7 * 24 * time.Hour},
		Current: &Operation{ID: newID(), Kind: "generate", Stage: stage, Params: tripo.Params{Prompt: "wooden crate", FaceLimit: 5000, TextureQuality: "standard"}},
		Model:   "controlled-test", Source: "controlled-test"}
	if stage != "ready" {
		v.Production = 1
		v.Deadline = now.Add(20 * time.Minute)
	}
	if stage == "submitted" || stage == "done" {
		v.Current.TaskID = "known-task"
	}
	if err = s.store.Create(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	return s, v, p
}

func editProductionFixture(t *testing.T, s *Service, id string, fn func(*Session)) Session {
	t.Helper()
	v, err := s.store.Edit(context.Background(), id, func(v *Session) error { fn(v); return nil }, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func getProductionFixture(t *testing.T, s *Service, id string) Session {
	t.Helper()
	v, err := s.store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func productionEventCount(t *testing.T, s *Service, id, kind string) int {
	t.Helper()
	events, err := s.store.Events(context.Background(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// TestProductionRecoveryStages 核对各持久化阶段允许的恢复动作：ready 可提交，
// submitted 仅续查，submitting 或非法阶段不得尝试推测并继续生产。
func TestProductionRecoveryStages(t *testing.T) {
	for _, stage := range []string{"ready", "submitting", "submitted", "invalid"} {
		t.Run(stage, func(t *testing.T) {
			s, before, p := productionRecoveryFixture(t, stage)
			result, err := s.production(context.Background(), before.ID, before.Current.ID)
			after := getProductionFixture(t, s, before.ID)
			switch stage {
			case "ready":
				if err != nil || p.submits != 1 || after.Production != 1 || after.Deadline.IsZero() || !strings.Contains(result, "artifact_id") {
					t.Fatalf("ready: %+v err=%v provider=%+v", after, err, p)
				}
			case "submitted":
				if err != nil || p.submits != 0 || p.queries != 1 || after.Production != before.Production || !after.Deadline.Equal(before.Deadline) {
					t.Fatalf("known: %+v err=%v provider=%+v", after, err, p)
				}
			default:
				if err == nil || p.submits != 0 || p.queries != 0 || after.Production != before.Production || !after.Deadline.Equal(before.Deadline) {
					t.Fatalf("unsafe stage advanced: %+v err=%v", after, err)
				}
			}
		})
	}
}

// TestProductionRecoveryCompletedResultUsesLiveBudget 验证已完成结果的业务证据稳定，
// 但返回的剩余额度来自当前数据库，不能因重放历史结果而恢复已消耗次数。
func TestProductionRecoveryCompletedResultUsesLiveBudget(t *testing.T) {
	s, before, p := productionRecoveryFixture(t, "submitted")
	first, err := s.production(context.Background(), before.ID, before.Current.ID)
	if err != nil {
		t.Fatal(err)
	}
	completed := getProductionFixture(t, s, before.ID)
	if len(completed.Artifacts) != 1 || completed.Artifacts[0].Report.Triangles != 12 {
		t.Fatalf("missing measured result: %+v", completed)
	}
	editProductionFixture(t, s, before.ID, func(v *Session) { v.Production = 2; v.ModelCalls = 20 })
	second, err := s.production(context.Background(), before.ID, before.Current.ID)
	if err != nil {
		t.Fatal(err)
	}
	var a, b map[string]json.RawMessage
	if err = json.Unmarshal([]byte(first), &a); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal([]byte(second), &b); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"artifact_id", "task_id", "report"} {
		if !reflect.DeepEqual(a[field], b[field]) {
			t.Errorf("business evidence changed: %s", field)
		}
	}
	if string(a["remaining_submissions"]) != "2" || string(b["remaining_submissions"]) != "1" {
		t.Errorf("budget was replayed: %s / %s", first, second)
	}
	if p.submits != 0 || p.queries != 1 || p.downloads != 1 || productionEventCount(t, s, before.ID, "technical_report") != 1 {
		t.Fatalf("replay repeated effects: %+v", p)
	}
}

// TestProductionRecoveryArtifactCommitIsIdempotent 并发提交同一操作结果，
// 验证产物与技术报告事件只保存一份，迟到失败不能推翻已完成证据。
func TestProductionRecoveryArtifactCommitIsIdempotent(t *testing.T) {
	s, before, _ := productionRecoveryFixture(t, "submitted")
	a := Artifact{ID: before.Current.ID, TaskID: before.Current.TaskID, Report: asset.Inspect(testfixture.Cube(12), 5000, 10<<20)}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.saveOperationArtifact(context.Background(), before.ID, before.Current.ID, a); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	after := getProductionFixture(t, s, before.ID)
	if len(after.Artifacts) != 1 || productionEventCount(t, s, before.ID, "technical_report") != 1 {
		t.Fatalf("duplicate completion: %+v", after)
	}
	if _, err := s.operationFailure(before.ID, before.Current.ID, errors.New("late failure")); err != nil {
		t.Fatal(err)
	}
	if productionEventCount(t, s, before.ID, "tool_failed") != 0 || getProductionFixture(t, s, before.ID).Current.Error != "" {
		t.Fatal("late failure overwrote completed evidence")
	}
}

// TestProductionRecoveryFileAlreadyWritten 复现文件已落盘、数据库尚未记录产物的窗口，
// 验证恢复沿用操作对应路径，允许重新下载但不能重复生产或追加重复记录。
func TestProductionRecoveryFileAlreadyWritten(t *testing.T) {
	s, before, p := productionRecoveryFixture(t, "submitted")
	dir := filepath.Join(s.Config.DataDir, "artifacts", before.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, before.Current.ID+".glb")
	if err := atomicWrite(path, testfixture.Cube(12)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.production(context.Background(), before.ID, before.Current.ID); err != nil {
		t.Fatal(err)
	}
	after := getProductionFixture(t, s, before.ID)
	if p.submits != 0 || len(after.Artifacts) != 1 || after.Artifacts[0].Path != path || productionEventCount(t, s, before.ID, "technical_report") != 1 {
		t.Fatalf("file recovery duplicated production: %+v", after)
	}
}

// TestProductionRecoveryLateTaskIDPreservesTerminalState 模拟提交响应晚于停止或过期，
// 验证任务 ID 仍作为远端已接受的证据保存，但会话不能因此重新进入执行状态。
func TestProductionRecoveryLateTaskIDPreservesTerminalState(t *testing.T) {
	for _, condition := range []string{"stop", "deadline"} {
		t.Run(condition, func(t *testing.T) {
			s, before, p := productionRecoveryFixture(t, "ready")
			var terminal Session
			p.submit = func(context.Context) (string, error) {
				terminal = editProductionFixture(t, s, before.ID, func(v *Session) {
					if condition == "stop" {
						v.Finish("stopped", "user stopped")
					} else {
						v.Deadline = time.Now().Add(-time.Second)
						v.Finish("failed", "expired")
					}
				})
				return "accepted-before-stop", nil
			}
			_, err := s.production(context.Background(), before.ID, before.Current.ID)
			after := getProductionFixture(t, s, before.ID)
			if err == nil || after.Current.TaskID != "accepted-before-stop" || after.Current.Stage != "submitted" || p.submits != 1 || p.queries != 0 || after.HasSlot || after.Production != 1 {
				t.Fatalf("late response: %+v err=%v", after, err)
			}
			if after.Status != terminal.Status || after.Final != terminal.Final || !after.Ended.Equal(terminal.Ended) || !after.Expires.Equal(terminal.Expires) || !after.Deadline.Equal(terminal.Deadline) {
				t.Fatalf("late response revived terminal facts: %+v", after)
			}
		})
	}
}

// TestProductionRecoveryRechecksBeforeEffects 在查询、下载等边界更新数据库事实，
// 验证后续副作用重新核对终止、期限和操作身份，不继续使用入口处的旧快照。
func TestProductionRecoveryRechecksBeforeEffects(t *testing.T) {
	for _, boundary := range []string{"entry-stop", "entry-deadline", "after-query-stop", "after-query-deadline", "after-query-operation", "after-download-stop"} {
		t.Run(boundary, func(t *testing.T) {
			s, before, p := productionRecoveryFixture(t, "submitted")
			mutate := func() {
				editProductionFixture(t, s, before.ID, func(v *Session) {
					switch {
					case strings.Contains(boundary, "deadline"):
						v.Deadline = time.Now().Add(-time.Second)
					case strings.Contains(boundary, "operation"):
						v.Current = &Operation{ID: "new-operation", Stage: "ready"}
					default:
						v.Finish("stopped", "stop during recovery")
					}
				})
			}
			if strings.HasPrefix(boundary, "entry") {
				mutate()
			}
			if strings.HasPrefix(boundary, "after-query") {
				p.query = func(_ context.Context, id string) (tripo.Task, error) {
					mutate()
					task := tripo.Task{ID: id, Status: "success"}
					task.Output.ModelURL = "https://fixture.example/model.glb"
					return task, nil
				}
			}
			if boundary == "after-download-stop" {
				p.download = func(context.Context) ([]byte, error) { mutate(); return testfixture.Cube(12), nil }
			}
			_, err := s.production(context.Background(), before.ID, before.Current.ID)
			after := getProductionFixture(t, s, before.ID)
			if err == nil || p.submits != 0 || len(after.Artifacts) != 0 || productionEventCount(t, s, before.ID, "technical_report") != 0 {
				t.Fatalf("invalid execution advanced: %+v err=%v", after, err)
			}
			if strings.HasPrefix(boundary, "entry") && p.queries != 0 {
				t.Fatal("queried after prior termination")
			}
			if strings.HasPrefix(boundary, "after-query") && p.downloads != 0 {
				t.Fatal("downloaded after newer terminal state")
			}
			if _, e := os.Stat(filepath.Join(s.Config.DataDir, "artifacts", before.ID, before.Current.ID+".glb")); !os.IsNotExist(e) {
				t.Fatalf("file written after rejection: %v", e)
			}
		})
	}
}

// TestProductionRecoveryFailureIsBoundToOperation 验证失败只能写入匹配的当前操作，
// 且重复上报保留首次失败原因与单一事件。
func TestProductionRecoveryFailureIsBoundToOperation(t *testing.T) {
	s, before, _ := productionRecoveryFixture(t, "submitted")
	if _, err := s.operationFailure(before.ID, "old-operation", errors.New("old failure")); err == nil {
		t.Fatal("old operation mutated current failure")
	}
	if _, err := s.operationFailure(before.ID, before.Current.ID, errors.New("provider failure")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.operationFailure(before.ID, before.Current.ID, errors.New("different replay failure")); err != nil {
		t.Fatal(err)
	}
	after := getProductionFixture(t, s, before.ID)
	if after.Current.Error != "provider failure" || productionEventCount(t, s, before.ID, "tool_failed") != 1 {
		t.Fatalf("failure evidence changed: %+v", after)
	}
}

// TestRecoverKnownTaskNeedsRealPassingEvidence 验证确定性恢复只能凭实际文件检查报告交付，
// 不能仅凭任务成功、旧完成标记或剩余预算耗尽就推定资产合格。
// 这里的“实际”指检查合成文件的真实字节，不是视觉检查或真实 API 的生成评测。
func TestRecoverKnownTaskNeedsRealPassingEvidence(t *testing.T) {
	for _, outcome := range []string{"valid", "over-limit", "invalid-file", "remote-failure", "missing-result", "missing-url", "unknown-submit", "ready", "pending", "corrupt-pending"} {
		t.Run(outcome, func(t *testing.T) {
			stage := "submitted"
			if outcome == "unknown-submit" {
				stage = "submitting"
			}
			if outcome == "ready" {
				stage = "ready"
			}
			if outcome == "missing-result" {
				stage = "done"
			}
			s, before, p := productionRecoveryFixture(t, stage)
			editProductionFixture(t, s, before.ID, func(v *Session) {
				v.ModelCalls = v.Limits.Calls
				if outcome == "pending" {
					v.PendingPause = &PendingPause{Point: ResumePoint{Version: recoveryVersion, SessionID: v.ID, Key: v.ID, Generation: 1, PauseID: "pending-pause", RefID: v.Current.ID, Kind: "production"}, Existing: true, Operation: v.Current}
					v.PendingPause.DraftHash = draftHash(v.PendingPause)
				}
				if outcome == "corrupt-pending" {
					v.PendingPause = &PendingPause{}
				}
			})
			switch outcome {
			case "over-limit":
				p.download = func(context.Context) ([]byte, error) { return testfixture.Cube(6000), nil }
			case "invalid-file":
				p.download = func(context.Context) ([]byte, error) { return []byte("broken GLB"), nil }
			case "remote-failure":
				p.query = func(_ context.Context, id string) (tripo.Task, error) {
					return tripo.Task{ID: id, Status: "failed", ErrorMessage: "provider failed"}, nil
				}
			case "missing-url":
				p.query = func(_ context.Context, id string) (tripo.Task, error) {
					return tripo.Task{ID: id, Status: "success"}, nil
				}
			}
			err := s.recoverKnownTask(context.Background(), before.ID)
			after := getProductionFixture(t, s, before.ID)
			if p.submits != 0 || after.ModelCalls != before.Limits.Calls || after.Production != before.Production || !after.Deadline.Equal(before.Deadline) || !after.LastUser.Equal(before.LastUser) {
				t.Fatalf("deterministic recovery reset facts: %+v", after)
			}
			switch outcome {
			case "valid", "corrupt-pending":
				if err != nil || after.Status != "completed" || len(after.Artifacts) != 1 || after.SelectedArtifact != before.Current.ID || !strings.Contains(after.Final, "未进行视觉检查") {
					t.Fatalf("valid evidence not delivered: %+v err=%v", after, err)
				}
			case "unknown-submit", "ready":
				if err == nil || p.queries != 0 || p.downloads != 0 || after.Terminal() {
					t.Fatalf("unknown/ready state advanced: %+v err=%v", after, err)
				}
			case "pending":
				if err == nil || after.Terminal() || after.PendingPause == nil || len(after.Artifacts) != 1 {
					t.Fatalf("lost pending follow-up pause: %+v err=%v", after, err)
				}
			default:
				if err != nil || after.Status != "failed" || after.SelectedArtifact != "" {
					t.Fatalf("unproven result delivered: %+v err=%v", after, err)
				}
			}
		})
	}
}

// TestProductionRecoveryReadyCannotBypassModelBudget 验证恢复入口不会让尚未提交的操作绕过预算，
// 拒绝后也不能消费生产次数或启动期限。
func TestProductionRecoveryReadyCannotBypassModelBudget(t *testing.T) {
	s, before, p := productionRecoveryFixture(t, "ready")
	editProductionFixture(t, s, before.ID, func(v *Session) { v.ModelCalls = v.Limits.Calls })
	if _, err := s.production(context.Background(), before.ID, before.Current.ID); !errors.Is(err, ErrBudget) {
		t.Fatalf("expected budget failure: %v", err)
	}
	after := getProductionFixture(t, s, before.ID)
	if p.submits != 0 || after.Production != 0 || !after.Deadline.IsZero() {
		t.Fatalf("budget check consumed production: %+v", after)
	}
}

// TestProductionRecoveryUnknownSubmissionNeverResends 验证未知响应或空任务 ID 均保留提交中证据，
// 再次进入生产逻辑时仍不能自动重发。
func TestProductionRecoveryUnknownSubmissionNeverResends(t *testing.T) {
	for _, outcome := range []string{"unknown-response", "empty-task-id"} {
		t.Run(outcome, func(t *testing.T) {
			s, before, p := productionRecoveryFixture(t, "ready")
			p.submit = func(context.Context) (string, error) {
				if outcome == "unknown-response" {
					return "", &tripo.APIError{Unknown: true}
				}
				return "", nil
			}
			if _, err := s.production(context.Background(), before.ID, before.Current.ID); err == nil {
				t.Fatal("unknown remote acceptance was treated as successful")
			}
			once := getProductionFixture(t, s, before.ID)
			if _, err := s.production(context.Background(), before.ID, before.Current.ID); err == nil {
				t.Fatal("unknown operation was replayed")
			}
			twice := getProductionFixture(t, s, before.ID)
			if p.submits != 1 || p.queries != 0 || twice.Current.Stage != "submitting" || twice.Current.TaskID != "" || twice.Production != 1 || !twice.Deadline.Equal(once.Deadline) {
				t.Fatalf("unknown submission was not preserved: %+v provider=%+v", twice, p)
			}
		})
	}
}

// TestProductionRecoveryCanceledDownloadKeepsKnownTask 区分服务关闭导致的取消与操作失败，
// 保留已知任务供下次续查，避免一次取消永久消耗可恢复的结果。
func TestProductionRecoveryCanceledDownloadKeepsKnownTask(t *testing.T) {
	s, before, p := productionRecoveryFixture(t, "submitted")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.download = func(context.Context) ([]byte, error) {
		cancel()
		return nil, errors.New("download connection closed during service shutdown")
	}
	if _, err := s.production(ctx, before.ID, before.Current.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation: %v", err)
	}
	after := getProductionFixture(t, s, before.ID)
	if after.Current.Stage != "submitted" || after.Current.TaskID != before.Current.TaskID || after.Current.Error != "" || after.Production != before.Production || productionEventCount(t, s, before.ID, "tool_failed") != 0 {
		t.Fatalf("shutdown consumed recoverable known task: %+v", after)
	}
	p.download = nil
	if err := s.recoverKnownTask(context.Background(), before.ID); err != nil {
		t.Fatal(err)
	}
	if p.submits != 0 || getProductionFixture(t, s, before.ID).Status != "completed" {
		t.Fatal("known task could not resume after canceled download")
	}
}
