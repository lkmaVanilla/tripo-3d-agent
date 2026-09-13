package app

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
	"modernc.org/sqlite"
)

// startupProvider 在受控 Provider 上增加原子查询计数，区分确定性续查与重复提交。
type startupProvider struct {
	fakeProvider
	queries atomic.Int64
}

func (p *startupProvider) Query(ctx context.Context, id string) (tripo.Task, error) {
	p.queries.Add(1)
	return p.fakeProvider.Query(ctx, id)
}

// startupService 复用指定目录中的数据库并统计模型调用，便于核对多次重启的累计副作用。
// 恢复与调度走真实 Service，外部依赖均被替换为本地夹具。
func startupService(t *testing.T, dir string, p *startupProvider, calls *atomic.Int64) *Service {
	t.Helper()
	c := DefaultConfig()
	c.DataDir, c.PollInterval = dir, time.Millisecond
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	s.provider, s.ready, s.source = p, true, "controlled-recovery-test"
	s.modelFactory = func(context.Context) (model.BaseChatModel, error) {
		return protocolModel{generate: func(ctx context.Context, in []*schema.Message) (*schema.Message, error) {
			calls.Add(1)
			return scriptModel{}.Generate(ctx, in)
		}}, nil
	}
	return s
}

// TestRecoveryStartupLegacyReadyMissingAndRepeatedRestart 覆盖旧检查点已保存但 Ready 未写入的窗口，
// 验证问题可重新开放作答，重复启动不创建新暂停或重复计算澄清次数。
func TestRecoveryStartupLegacyReadyMissingAndRepeatedRestart(t *testing.T) {
	f := newLegacyCheckpointFixture(t, legacyReadyMissing)
	p := &startupProvider{}
	var calls atomic.Int64
	var first ResumePoint
	for attempt := 0; attempt < 3; attempt++ {
		s := startupService(t, f.Dir, p, &calls)
		s.Start()
		v, err := s.store.Get(context.Background(), f.Key)
		if err != nil {
			t.Fatal(err)
		}
		if v.Status != "awaiting_answer" || v.ResumePoint == nil || v.Clarifications != f.Session.Clarifications || v.ModelCalls != f.Session.ModelCalls || v.WaitID != f.PauseState || v.Question != f.Session.Question || v.PendingPause != nil {
			t.Fatalf("legacy wait not restored: %+v", v)
		}
		if attempt == 0 {
			first = *v.ResumePoint
		} else if *v.ResumePoint != first {
			t.Fatalf("restart generated another pause: first=%+v current=%+v", first, v.ResumePoint)
		}
		assertStoreEventCount(t, s.store, f.Key, "checkpoint_committed", 1)
		assertStoreEventCount(t, s.store, f.Key, "checkpoint_rebuilt", 1)
		assertStoreEventCount(t, s.store, f.Key, "clarification", 0)
		if attempt == 2 {
			if err = s.Answer(context.Background(), f.Key, "卡通"); err != nil {
				t.Fatalf("Ready=false legacy question cannot be answered: %v", err)
			}
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 0 || p.Count() != 0 || p.queries.Load() != 0 {
		t.Fatalf("migration used external work: model=%d submit=%d query=%d", calls.Load(), p.Count(), p.queries.Load())
	}
}

// TestRecoveryStartupLegacyAnswerEvidence 验证迁移只采用明确属于旧等待点的答案，
// 可用证据必须真实进入恢复后的工具协议，丢失或歧义证据必须明确失败。
func TestRecoveryStartupLegacyAnswerEvidence(t *testing.T) {
	for _, evidence := range []string{"answer_field", "unique_history", "lost", "ambiguous_history", "answer_for_other_call"} {
		t.Run(evidence, func(t *testing.T) {
			f := newLegacyCheckpointFixture(t, legacyAnswerCleared)
			p := &startupProvider{}
			var calls atomic.Int64
			s := startupService(t, f.Dir, p, &calls)
			defer s.Close()
			_, err := s.store.Edit(context.Background(), f.Key, func(v *Session) error {
				switch evidence {
				case "answer_field":
					v.Answer = "卡通"
				case "unique_history", "ambiguous_history":
					v.History = append(v.History, schema.ToolMessage(`{"user_answer":"卡通"}`, f.ToolCallID))
					if evidence == "ambiguous_history" {
						// 同一工具调用 ID 对应两份完整答案时，无法证明哪份属于旧检查点中的等待 ID。
						var q questionInput
						_ = json.Unmarshal([]byte(f.Session.History[len(f.Session.History)-1].ToolCalls[0].Function.Arguments), &q)
						v.History = append(v.History, protocolProposal("ask_user", f.ToolCallID, q), schema.ToolMessage(`{"user_answer":"写实"}`, f.ToolCallID))
					}
				case "answer_for_other_call":
					current := v.History[len(v.History)-1]
					var q questionInput
					_ = json.Unmarshal([]byte(current.ToolCalls[0].Function.Arguments), &q)
					v.History = append(v.History[:len(v.History)-1], protocolProposal("ask_user", "older-call", q), schema.ToolMessage(`{"user_answer":"上一轮的答案"}`, "older-call"), current)
				}
				return nil
			}, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			s.reconcile()
			v, err := s.store.Get(context.Background(), f.Key)
			if err != nil {
				t.Fatal(err)
			}
			if evidence == "lost" || evidence == "ambiguous_history" || evidence == "answer_for_other_call" {
				if !v.Terminal() || v.Status != "failed" || !strings.Contains(v.Final, "legacy_recovery_unavailable") {
					t.Fatalf("unprovable answer did not fail clearly: status=%s final=%q answers=%v", v.Status, v.Final, v.Answers)
				}
			} else {
				if v.Terminal() || v.ResumePoint == nil || v.Answers[f.PauseState].Text != "卡通" || v.Clarifications != f.Session.Clarifications || v.ModelCalls != f.Session.ModelCalls {
					t.Fatalf("accepted answer was not migrated: %+v", v)
				}
				var sawAnswer atomic.Bool
				s.modelFactory = func(context.Context) (model.BaseChatModel, error) {
					return protocolModel{generate: func(_ context.Context, in []*schema.Message) (*schema.Message, error) {
						for _, m := range in {
							if m.Role == schema.Tool && m.ToolCallID == f.ToolCallID && strings.Contains(m.Content, "卡通") {
								sawAnswer.Store(true)
							}
						}
						return protocolProposal("finish_request", "finish-after-answer", finishInput{Explanation: "受控恢复验证已读取原答案"}), nil
					}}, nil
				}
				if err = s.run(context.Background(), f.Key); err != nil {
					t.Fatal(err)
				}
				if !sawAnswer.Load() {
					t.Fatal("resumed Runner never received the original accepted answer")
				}
			}
			if calls.Load() != 0 || p.Count() != 0 || p.queries.Load() != 0 {
				t.Fatalf("legacy reconciliation performed external work: model=%d submit=%d query=%d", calls.Load(), p.Count(), p.queries.Load())
			}
		})
	}
}

// TestRecoveryStartupKnownTaskWithoutUsableCheckpoint 验证检查点缺失或损坏时，
// 已知任务仍可完成续查、下载和技术验收，即使模型预算耗尽也不丢弃已付费结果。
func TestRecoveryStartupKnownTaskWithoutUsableCheckpoint(t *testing.T) {
	for _, variant := range []string{"missing", "corrupt", "missing_model_budget_exhausted", "corrupt_model_budget_exhausted"} {
		t.Run(variant, func(t *testing.T) {
			fixtureKind := legacyKnownTask
			if strings.HasPrefix(variant, "missing") {
				fixtureKind = legacyKnownTaskNoCheckpoint
			}
			f := newLegacyCheckpointFixture(t, fixtureKind)
			p := &startupProvider{}
			var calls atomic.Int64
			s := startupService(t, f.Dir, p, &calls)
			if strings.HasPrefix(variant, "corrupt") {
				if _, err := s.store.db.Exec("UPDATE checkpoints SET data=? WHERE session_id=?", []byte("not-an-eino-checkpoint"), f.Key); err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.store.Edit(context.Background(), f.Key, func(v *Session) error {
				if strings.HasSuffix(variant, "exhausted") {
					v.ModelCalls = v.Limits.Calls
				}
				return nil
			}, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			s.Start()
			v := waitSession(t, s, f.Key, func(v Session) bool { return v.Terminal() })
			if v.Status != "completed" || len(v.Artifacts) != 1 || !v.Artifacts[0].Report.Passed || v.SelectedArtifact != f.PauseState || v.Current.TaskID != f.Session.Current.TaskID || v.Production != before.Production || v.ModelCalls != before.ModelCalls || !v.Deadline.Equal(before.Deadline) || calls.Load() != 0 || p.Count() != 0 || p.queries.Load() == 0 {
				t.Fatalf("known task recovery changed constraints or skipped validation: %+v, model=%d submit=%d query=%d", v, calls.Load(), p.Count(), p.queries.Load())
			}
			assertStoreEventCount(t, s.store, f.Key, "technical_report", 1)
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s = startupService(t, f.Dir, p, &calls)
			queries := p.queries.Load()
			s.Start()
			after, err := s.store.Get(context.Background(), f.Key)
			if err != nil || !reflect.DeepEqual(v, after) || queries != p.queries.Load() {
				t.Fatalf("completed recovery repeated after restart: %+v, %v", after, err)
			}
			assertStoreEventCount(t, s.store, f.Key, "technical_report", 1)
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestRecoveryStartupUnknownSubmissionNeverResends 验证启动对账保留未知提交和已消费额度，
// 明确终止后即使再次启动也不查询不存在的任务 ID 或重发生产。
func TestRecoveryStartupUnknownSubmissionNeverResends(t *testing.T) {
	f := newLegacyCheckpointFixture(t, legacyUnknownSubmission)
	p := &startupProvider{}
	var calls atomic.Int64
	for attempt := 0; attempt < 2; attempt++ {
		s := startupService(t, f.Dir, p, &calls)
		s.Start()
		v, err := s.store.Get(context.Background(), f.Key)
		if err != nil || !v.Terminal() || v.Status != "failed" || !strings.Contains(v.Final, "submission_outcome_unknown") || v.Production != f.Session.Production || v.ModelCalls != f.Session.ModelCalls || !v.Deadline.Equal(f.Session.Deadline) || v.Current.Stage != "submitting" || v.Current.TaskID != "" {
			t.Fatalf("unknown submission not preserved and stopped: %+v, %v", v, err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 0 || p.Count() != 0 || p.queries.Load() != 0 {
		t.Fatalf("unknown submission replayed: model=%d submit=%d query=%d", calls.Load(), p.Count(), p.queries.Load())
	}
}

// TestRecoveryStartupLegacyCompleteSeedWithoutCheckpoint 删除旧检查点但保留完整已接受提议，
// 验证仅凭原协议重建同一问题，不重新调用模型或重复计数。
func TestRecoveryStartupLegacyCompleteSeedWithoutCheckpoint(t *testing.T) {
	f := newLegacyCheckpointFixture(t, legacyReadyMissing)
	p := &startupProvider{}
	var calls atomic.Int64
	s := startupService(t, f.Dir, p, &calls)
	defer s.Close()
	if _, err := s.store.db.Exec("DELETE FROM checkpoints WHERE session_id=?", f.Key); err != nil {
		t.Fatal(err)
	}
	s.Start()
	v := waitSession(t, s, f.Key, func(v Session) bool { return v.ResumePoint != nil && v.PendingPause == nil })
	if v.Status != "awaiting_answer" || v.Clarifications != f.Session.Clarifications || v.ModelCalls != f.Session.ModelCalls || v.WaitID != f.PauseState || v.Question != f.Session.Question || calls.Load() != 0 || p.Count() != 0 {
		t.Fatalf("complete legacy seed not rebuilt without duplicate work: %+v, calls=%d, submits=%d", v, calls.Load(), p.Count())
	}
	if err := s.Answer(context.Background(), f.Key, "卡通"); err != nil {
		t.Fatal(err)
	}
}

// TestRecoveryStartupKnownTaskWithInvalidPendingStillChecksResult 验证损坏的 Agent 草案不会阻断
// 已知远端任务的确定性续查；取回结果后仍必须执行真实文件字节的技术检查。
func TestRecoveryStartupKnownTaskWithInvalidPendingStillChecksResult(t *testing.T) {
	f := newLegacyCheckpointFixture(t, legacyKnownTask)
	p := &startupProvider{}
	var calls atomic.Int64
	s := startupService(t, f.Dir, p, &calls)
	defer s.Close()
	_, err := s.store.Edit(context.Background(), f.Key, func(v *Session) error {
		v.PendingPause = &PendingPause{Point: ResumePoint{Version: recoveryVersion, SessionID: v.ID, Key: v.ID, Kind: "production", RefID: "broken-operation", PauseID: "broken-pause", Generation: 1}}
		return nil
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	v := waitSession(t, s, f.Key, func(v Session) bool { return v.Terminal() })
	if p.queries.Load() == 0 || len(v.Artifacts) != 1 || !v.Artifacts[0].Report.Passed || v.Current.TaskID != f.Session.Current.TaskID || calls.Load() != 0 || p.Count() != 0 || v.Production != f.Session.Production {
		t.Fatalf("invalid Agent draft prevented deterministic known-task recovery: %+v, queries=%d model=%d submit=%d", v, p.queries.Load(), calls.Load(), p.Count())
	}
}

// TestRecoveryPendingProductionMatchesAcceptedParameters 验证草案摘要自洽仍不足以授权生产，
// 操作类型和参数还必须与已接受的模型提议逐项一致。
func TestRecoveryPendingProductionMatchesAcceptedParameters(t *testing.T) {
	for _, field := range []string{"valid", "prompt", "target_triangles", "texture_quality", "kind"} {
		t.Run(field, func(t *testing.T) {
			args := generationInput{Prompt: "A stylized wooden crate", TargetTriangles: 5000, TextureQuality: "standard", Reason: "首次生成"}
			seed, err := newReplaySeed([]*schema.Message{schema.UserMessage("生成木箱")}, protocolProposal("generate_asset", "generation-call", args), "deepseek-v4-pro")
			if err != nil {
				t.Fatal(err)
			}
			v := Session{ID: "session", Model: "deepseek-v4-pro", Intent: &Intent{MaxTriangles: 5000, MaxBytes: 10 << 20}}
			p := &PendingPause{Point: ResumePoint{Version: recoveryVersion, SessionID: v.ID, Key: v.ID, PauseID: "pause", RefID: "operation", Kind: "production", Generation: 1}, Seed: seed, Reason: args.Reason, Operation: &Operation{ID: "operation", Kind: "generate", Stage: "ready", Params: tripo.Params{Prompt: args.Prompt, FaceLimit: args.TargetTriangles, TextureQuality: args.TextureQuality}}}
			switch field {
			case "prompt":
				p.Operation.Params.Prompt = "A different character"
			case "target_triangles":
				p.Operation.Params.FaceLimit = 1000
			case "texture_quality":
				p.Operation.Params.TextureQuality = "extreme"
			case "kind":
				p.Operation.Kind = "decimate"
			}
			p.DraftHash = draftHash(p)
			v.PendingPause = p
			err = validatePending(v)
			if field == "valid" && err != nil {
				t.Fatalf("valid bound proposal rejected: %v", err)
			}
			if field != "valid" && err == nil {
				t.Fatal("draft is internally hashed but contradicts the accepted proposal")
			}
		})
	}
}

// TestRecoveryStartupTerminalAndDeadlineTakePriority 验证终止、生产期限与闲置期限先于恢复处理，
// 不能因迁移检查点而重置额度或复活已关闭请求。
func TestRecoveryStartupTerminalAndDeadlineTakePriority(t *testing.T) {
	for _, boundary := range []string{"terminal", "deadline", "idle"} {
		t.Run(boundary, func(t *testing.T) {
			variant := legacyKnownTask
			if boundary == "idle" {
				variant = legacyReadyMissing
			}
			f := newLegacyCheckpointFixture(t, variant)
			p := &startupProvider{}
			var calls atomic.Int64
			s := startupService(t, f.Dir, p, &calls)
			defer s.Close()
			before, err := s.store.Edit(context.Background(), f.Key, func(v *Session) error {
				switch boundary {
				case "terminal":
					v.Finish("stopped", "既有停止原因")
				case "deadline":
					v.Deadline = time.Now().Add(-time.Minute)
				case "idle":
					v.LastUser = time.Now().Add(-25 * time.Hour)
				}
				return nil
			}, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			s.Start()
			after, err := s.store.Get(context.Background(), f.Key)
			if err != nil || !after.Terminal() || after.ResumePoint != nil || after.Production != before.Production || after.ModelCalls != before.ModelCalls || !after.Deadline.Equal(before.Deadline) || !after.LastUser.Equal(before.LastUser) || calls.Load() != 0 || p.Count() != 0 || p.queries.Load() != 0 {
				t.Fatalf("expired or stopped request resumed: %+v, %v", after, err)
			}
			if boundary == "terminal" && !reflect.DeepEqual(before, after) {
				t.Fatal("startup rewrote an existing terminal outcome")
			}
		})
	}
}

// TestRecoveryStartupCorruptSessionIsolated 同时放入坏检查点与可恢复旧会话，
// 验证单会话恢复失败有明确记录，且不阻塞其他会话迁移和作答。
func TestRecoveryStartupCorruptSessionIsolated(t *testing.T) {
	f := newLegacyCheckpointFixture(t, legacyReadyMissing)
	p := &startupProvider{}
	var calls atomic.Int64
	s := startupService(t, f.Dir, p, &calls)
	defer s.Close()
	bad := Session{ID: "corrupt-session", Owner: "another-owner", Status: "understanding", LastUser: time.Now(), Limits: f.Session.Limits}
	if err := s.store.Create(context.Background(), bad); err != nil {
		t.Fatal(err)
	}
	point := prepareStorePause(t, s.store, bad.ID, 1)
	if _, err := s.store.CommitCheckpoint(context.Background(), bad.ID, 0, point, []byte("checkpoint-placeholder"), publishStoreQuestion); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec("UPDATE checkpoints SET data=? WHERE session_id=?", []byte("tampered"), bad.ID); err != nil {
		t.Fatal(err)
	}
	s.Start()
	broken, err := s.store.Get(context.Background(), bad.ID)
	if err != nil || !broken.Terminal() || broken.Status != "failed" {
		t.Fatalf("corrupt session did not fail: %+v, %v", broken, err)
	}
	healthy, err := s.store.Get(context.Background(), f.Key)
	if err != nil || healthy.Terminal() || healthy.ResumePoint == nil || healthy.Status != "awaiting_answer" {
		t.Fatalf("corrupt session blocked valid migration: %+v, %v", healthy, err)
	}
	if err = s.Answer(context.Background(), f.Key, "卡通"); err != nil {
		t.Fatalf("healthy session cannot accept answer: %v", err)
	}
	assertStoreEventCount(t, s.store, bad.ID, "recovery_rejected", 1)
}

// TestRecoveryFieldsRoundTripAndPublicViews 验证完整恢复材料可持久化往返，
// 但不会出现在会话视图、WebSocket/导出快照中；旧 JSON 仍保留零值兼容语义。
func TestRecoveryFieldsRoundTripAndPublicViews(t *testing.T) {
	input := []*schema.Message{schema.UserMessage("内部恢复输入标记")}
	response := protocolProposal("ask_user", "private-tool-call", questionInput{Question: "内部待发布问题标记"})
	response.ReasoningContent = "内部完整推理标记"
	seed, err := newReplaySeed(input, response, "deepseek-v4-pro")
	if err != nil {
		t.Fatal(err)
	}
	point := ResumePoint{Version: recoveryVersion, SessionID: "session", Key: "session", PauseID: "private-pause", Kind: "question", RefID: "private-wait", Generation: 1, Digest: "private-digest"}
	v := Session{ID: "session", Owner: "private-owner", RecoverySchemaVersion: recoveryVersion, ResumePoint: &point, PendingPause: &PendingPause{Point: point, Seed: seed, Question: "内部待发布问题标记"}, Answers: map[string]AnswerRecord{"private-wait": {Text: "内部答案证据标记", Accepted: time.Now().UTC()}}, History: append(input, response)}
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Session
	if err = json.Unmarshal(body, &decoded); err != nil || !reflect.DeepEqual(v, decoded) {
		t.Fatalf("recovery identity or protocol fields lost in round trip: %v", err)
	}
	if err = validateReplaySeed(decoded.PendingPause.Seed); err != nil {
		t.Fatal(err)
	}
	s := &Service{}
	for name, view := range map[string]any{"view": decoded.View(), "socket_and_export_snapshot": s.snapshot(decoded, nil)} {
		public, marshalErr := json.Marshal(view)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		for _, private := range []string{"RecoverySchemaVersion", "ResumePoint", "PendingPause", "Answers", "History", "private-owner", "private-tool-call", "private-digest", "内部恢复输入标记", "内部完整推理标记", "内部答案证据标记", "内部待发布问题标记"} {
			if strings.Contains(string(public), private) {
				t.Errorf("%s leaked %q", name, private)
			}
		}
	}
	var legacy Session
	if err = json.Unmarshal([]byte(`{"ID":"old","Status":"awaiting_answer","CheckpointReady":true,"WaitID":"old-wait","Answer":"原答案"}`), &legacy); err != nil || legacy.RecoverySchemaVersion != 0 || legacy.ResumePoint != nil || legacy.PendingPause != nil || legacy.Answers != nil || legacy.Answer != "原答案" {
		t.Fatal(fmt.Sprintf("legacy JSON defaults changed: %+v, %v", legacy, err))
	}
}

// migrationBarrierDriver 只在测试中包装 SQLite 驱动，将真实迁移阻塞在 ALTER TABLE 后、Commit 前。
// 父进程强杀该子进程以验证 SQLite 恢复，无需给生产 Store 增加故障开关。
type migrationBarrierDriver struct{ signal string }

func (d migrationBarrierDriver) Open(name string) (driver.Conn, error) {
	conn, err := (&sqlite.Driver{}).Open(name)
	if err != nil {
		return nil, err
	}
	return migrationBarrierConn{Conn: conn, signal: d.signal}, nil
}

type migrationBarrierConn struct {
	driver.Conn
	signal string
}

func (c migrationBarrierConn) Begin() (driver.Tx, error) {
	tx, err := c.Conn.Begin()
	if err != nil {
		return nil, err
	}
	return migrationBarrierTx{Tx: tx, signal: c.signal}, nil
}

type migrationBarrierTx struct {
	driver.Tx
	signal string
}

func (tx migrationBarrierTx) Commit() error {
	if err := os.WriteFile(tx.signal, []byte("migration-reached-commit"), 0600); err != nil {
		return err
	}
	select {} // 此屏障只能由父进程结束子进程，不能靠正常返回触发自动清理。
}

// TestRecoveryMigrationCrashHelper 由父用例通过环境变量启动，仅对临时旧数据库执行迁移。
// 独立子进程隔离测试专用驱动的注册和未提交事务。
func TestRecoveryMigrationCrashHelper(t *testing.T) {
	dir := os.Getenv("TRIPO_TEST_MIGRATION_CRASH_DIR")
	if dir == "" {
		return
	}
	sql.Register("migration-barrier-test", migrationBarrierDriver{signal: filepath.Join(dir, "migration-barrier")})
	db, err := sql.Open("migration-barrier-test", filepath.Join(dir, "tripo.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if err = migrateCheckpointMetadata(db); err != nil {
		t.Fatal(err)
	}
	t.Fatal("parent did not kill the paused migration")
}

// TestRecoveryMigrationForcedExitPreservesLegacy 在真实 DDL 提交前强杀子进程，
// 验证重开数据库并重复迁移后，旧会话事实和 Eino 检查点字节均未变化。
func TestRecoveryMigrationForcedExitPreservesLegacy(t *testing.T) {
	f := newLegacyCheckpointFixture(t, legacyKnownTask)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRecoveryMigrationCrashHelper$", "-test.timeout=20s")
	cmd.Env = append(os.Environ(), "TRIPO_TEST_MIGRATION_CRASH_DIR="+f.Dir)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	for {
		// 等待明确提交屏障，不用固定睡眠猜测迁移执行到了哪一步。
		if _, err := os.Stat(filepath.Join(f.Dir, "migration-barrier")); err == nil {
			break
		}
		if ctx.Err() != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("migration did not reach commit barrier: %s", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("migration child exited normally instead of being killed")
	}
	for attempt := 0; attempt < 2; attempt++ {
		store, err := OpenStore(f.Dir)
		if err != nil {
			t.Fatalf("migration after forced exit: %v", err)
		}
		v, err := store.Get(context.Background(), f.Key)
		if err != nil || !reflect.DeepEqual(v, f.Session) {
			t.Fatalf("migration changed old session facts: %+v, %v", v, err)
		}
		record, err := store.LoadCheckpoint(context.Background(), f.Key, f.Key)
		if err != nil || record.Point != nil || !bytes.Equal(record.Data, f.Checkpoint) {
			t.Fatalf("migration changed original Eino checkpoint: %+v, %v", record, err)
		}
		if err = store.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
