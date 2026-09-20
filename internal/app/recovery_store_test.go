package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// recoveryStoreFixture 使用临时目录中的真实 SQLite，独立验证存储事务和身份约束。
// 本文件的检查点字节是存储占位数据；Eino 编解码兼容性由协议测试单独覆盖。
func recoveryStoreFixture(t *testing.T) (*Store, Session) {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	session := Session{ID: newID(), Owner: "owner", Request: "测试恢复", Status: "understanding", Created: time.Now(), LastUser: time.Now(), Limits: Limits{Calls: 20, Submissions: 3, Clarifications: 3, Duration: time.Hour, Idle: time.Hour, Retention: time.Hour}}
	if err = store.Create(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	return store, session
}

// prepareStorePause 构造指定代次的待提交草案，用于触发存储层的比较并交换校验。
// 这里刻意省略模型重放种子，不经过上层协调器的草案语义校验。
func prepareStorePause(t *testing.T, store *Store, id string, generation int64) ResumePoint {
	t.Helper()
	point := ResumePoint{Version: recoveryVersion, SessionID: id, Key: id, PauseID: newID(), Generation: generation, Kind: "question", RefID: newID()}
	_, err := store.Edit(context.Background(), id, func(v *Session) error {
		v.PendingPause = &PendingPause{Point: point, ExpectedGeneration: generation - 1, Question: "采用什么风格？"}
		return nil
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return point
}

// publishStoreQuestion 将业务发布和事件生成放入 CommitCheckpoint 的事务回调，
// 使测试能观察回调是否被重复执行，以及任一步失败是否完整回滚。
func publishStoreQuestion(v *Session) ([]checkpointEvent, error) {
	v.Question = v.PendingPause.Question
	v.WaitID = v.PendingPause.Point.RefID
	v.Status = "awaiting_answer"
	v.Clarifications++
	return []checkpointEvent{{Kind: "clarification", Data: map[string]string{"question": v.Question}}}, nil
}

// TestCheckpointAtomicCommitAndDuplicate 验证首次提交完整发布，重复确认不重复计数，
// 后继暂停一旦提交，旧代次便不能覆盖新事实。
func TestCheckpointAtomicCommitAndDuplicate(t *testing.T) {
	ctx := context.Background()
	store, initial := recoveryStoreFixture(t)
	point := prepareStorePause(t, store, initial.ID, 1)
	value := []byte("opaque-eino-checkpoint")
	committed, err := store.CommitCheckpoint(ctx, initial.ID, 0, point, value, publishStoreQuestion)
	if err != nil {
		t.Fatal(err)
	}
	if committed.PendingPause != nil || committed.RecoverySchemaVersion != recoveryVersion || committed.Clarifications != 1 || committed.WaitID != point.RefID || committed.ResumePoint.Digest != checkpointDigest(value) {
		t.Fatalf("incomplete commit: %+v", committed)
	}
	record, err := store.LoadCheckpoint(ctx, initial.ID, initial.ID)
	if err != nil || !bytes.Equal(record.Data, value) || *record.Point != *committed.ResumePoint {
		t.Fatalf("load: %+v, %v", record, err)
	}
	_, err = store.Edit(ctx, initial.ID, func(v *Session) error {
		v.Answers = map[string]AnswerRecord{v.WaitID: {Text: "卡通", Accepted: time.Now()}}
		v.Status = "understanding"
		return nil
	}, "answer", "卡通")
	if err != nil {
		t.Fatal(err)
	}
	// 在重复确认前先接受答案并准备下一轮，证明幂等返回不能回写旧会话快照。
	next := prepareStorePause(t, store, initial.ID, 2)
	called := false
	duplicate, err := store.CommitCheckpoint(ctx, initial.ID, 0, point, value, func(v *Session) ([]checkpointEvent, error) {
		called = true
		return publishStoreQuestion(v)
	})
	if err != nil || called || duplicate.Clarifications != 1 || duplicate.Answers[point.RefID].Text != "卡通" || duplicate.PendingPause.Point != next || duplicate.Status != "understanding" {
		t.Fatalf("duplicate changed current facts: %+v, callback=%v, err=%v", duplicate, called, err)
	}
	assertStoreEventCount(t, store, initial.ID, "checkpoint_committed", 1)
	assertStoreEventCount(t, store, initial.ID, "clarification", 1)
	if _, err = store.CommitCheckpoint(ctx, initial.ID, 1, next, []byte("next-checkpoint"), publishStoreQuestion); err != nil {
		t.Fatal(err)
	}
	if _, err = store.CommitCheckpoint(ctx, initial.ID, 0, point, value, publishStoreQuestion); !errors.Is(err, ErrCheckpointMismatch) {
		t.Fatalf("stale generation accepted: %v", err)
	}
	assertStoreEventCount(t, store, initial.ID, "checkpoint_committed", 2)
}

func assertStoreEventCount(t *testing.T, store *Store, id, kind string, want int) {
	t.Helper()
	events, err := store.Events(context.Background(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Kind == kind {
			count++
		}
	}
	if count != want {
		t.Fatalf("event %s: got %d, want %d", kind, count, want)
	}
}

// TestCheckpointCommitRollsBackEveryWrite 在回调、会话写入和事件写入处分别注入失败，
// 验证新检查点与业务状态同进同退，原代次和原事件均完整保留。
func TestCheckpointCommitRollsBackEveryWrite(t *testing.T) {
	for _, boundary := range []string{"callback", "session_write", "event_write"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			store, session := recoveryStoreFixture(t)
			first := prepareStorePause(t, store, session.ID, 1)
			oldValue := []byte("first-checkpoint")
			if _, err := store.CommitCheckpoint(ctx, session.ID, 0, first, oldValue, publishStoreQuestion); err != nil {
				t.Fatal(err)
			}
			next := prepareStorePause(t, store, session.ID, 2)
			before, err := store.Get(ctx, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			callback := publishStoreQuestion
			// 触发器让真实 SQL 写入失败，避免仅用模拟 Store 错误错过事务边界问题。
			switch boundary {
			case "callback":
				callback = func(v *Session) ([]checkpointEvent, error) {
					v.Clarifications++
					return nil, errors.New("injected callback failure")
				}
			case "session_write":
				_, err = store.db.Exec(`CREATE TRIGGER fail_session BEFORE UPDATE ON sessions BEGIN SELECT RAISE(ABORT,'injected session write failure'); END`)
			case "event_write":
				_, err = store.db.Exec(`CREATE TRIGGER fail_event BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'injected event write failure'); END`)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.CommitCheckpoint(ctx, session.ID, 1, next, []byte("second-checkpoint"), callback); err == nil {
				t.Fatal("injected failure was not returned")
			}
			after, err := store.Get(ctx, session.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("session survived failed transaction: before=%+v after=%+v err=%v", before, after, err)
			}
			record, err := store.LoadCheckpoint(ctx, session.ID, session.ID)
			if err != nil || !bytes.Equal(record.Data, oldValue) || record.Point.Generation != 1 {
				t.Fatalf("checkpoint survived failed transaction: %+v, %v", record, err)
			}
			assertStoreEventCount(t, store, session.ID, "checkpoint_committed", 1)
			assertStoreEventCount(t, store, session.ID, "clarification", 1)
		})
	}
}

// TestCheckpointIdentityAndIntegrity 分别破坏字节摘要、元数据及会话绑定，
// 验证加载入口不会把存在一条数据库记录等同于检查点可恢复。
func TestCheckpointIdentityAndIntegrity(t *testing.T) {
	for _, corruption := range []string{"digest", "metadata", "session_binding", "foreign_identity", "missing_metadata"} {
		t.Run(corruption, func(t *testing.T) {
			ctx := context.Background()
			store, session := recoveryStoreFixture(t)
			point := prepareStorePause(t, store, session.ID, 1)
			if _, err := store.CommitCheckpoint(ctx, session.ID, 0, point, []byte("checkpoint"), publishStoreQuestion); err != nil {
				t.Fatal(err)
			}
			var err error
			switch corruption {
			case "digest":
				_, err = store.db.Exec("UPDATE checkpoints SET data=?", []byte("tampered"))
			case "metadata":
				_, err = store.db.Exec("UPDATE checkpoints SET metadata=?", []byte("invalid-json"))
			case "session_binding":
				_, err = store.Edit(ctx, session.ID, func(v *Session) error { v.ResumePoint.Generation++; return nil }, "", nil)
			case "foreign_identity":
				point.SessionID = "another-session"
				point.Digest = checkpointDigest([]byte("checkpoint"))
				var metadata []byte
				metadata, err = json.Marshal(point)
				if err == nil {
					_, err = store.db.Exec("UPDATE checkpoints SET metadata=?", metadata)
				}
			case "missing_metadata":
				_, err = store.db.Exec("UPDATE checkpoints SET metadata=NULL")
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.LoadCheckpoint(ctx, session.ID, session.ID); !errors.Is(err, ErrCheckpointMismatch) {
				t.Fatalf("corruption accepted: %v", err)
			}
		})
	}
}

// TestCheckpointRejectsUnauthorizedOrStaleCommit 验证提交必须匹配当前草案身份，
// 并拒绝跨会话键、同代不同内容以及旧格式写入者覆盖新格式记录。
func TestCheckpointRejectsUnauthorizedOrStaleCommit(t *testing.T) {
	ctx := context.Background()
	store, session := recoveryStoreFixture(t)
	point := prepareStorePause(t, store, session.ID, 1)
	for _, mutation := range []string{"session", "key", "generation", "pause", "digest", "kind"} {
		t.Run(mutation, func(t *testing.T) {
			bad := point
			switch mutation {
			case "session":
				bad.SessionID = "another-session"
			case "key":
				bad.Key = "another-session"
			case "generation":
				bad.Generation++
			case "pause":
				bad.PauseID = newID()
			case "digest":
				bad.Digest = "incorrect-digest"
			case "kind":
				bad.Kind = "unrelated"
			}
			if _, err := store.CommitCheckpoint(ctx, session.ID, 0, bad, []byte("checkpoint"), publishStoreQuestion); !errors.Is(err, ErrCheckpointMismatch) {
				t.Fatalf("invalid commit accepted: %v", err)
			}
		})
	}
	if _, err := store.LoadCheckpoint(ctx, session.ID, "another-session"); !errors.Is(err, ErrCheckpointMismatch) {
		t.Fatalf("cross-session key accepted: %v", err)
	}
	if _, err := store.LoadCheckpoint(ctx, session.ID, session.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing record must retain sql.ErrNoRows: %v", err)
	}
	if _, err := store.CommitCheckpoint(ctx, session.ID, 0, point, []byte("checkpoint"), publishStoreQuestion); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitCheckpoint(ctx, session.ID, 0, point, []byte("different-bytes"), publishStoreQuestion); !errors.Is(err, ErrCheckpointMismatch) {
		t.Fatalf("different bytes at same generation accepted: %v", err)
	}
	if err := (checkpointStore{store: store, sessionID: session.ID}).Set(ctx, session.ID, []byte("legacy-overwrite")); !errors.Is(err, ErrCheckpointMismatch) {
		t.Fatalf("legacy writer replaced new record: %v", err)
	}
	assertStoreEventCount(t, store, session.ID, "checkpoint_committed", 1)
}

// TestCheckpointRejectsStoppedAndExpiredSession 验证终止或到期会话在业务回调前即被拒绝，
// 不留下新检查点，也不发生澄清次数等附带修改。
func TestCheckpointRejectsStoppedAndExpiredSession(t *testing.T) {
	for _, boundary := range []string{"stopped", "deadline", "idle"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			store, session := recoveryStoreFixture(t)
			point := prepareStorePause(t, store, session.ID, 1)
			_, err := store.Edit(ctx, session.ID, func(v *Session) error {
				switch boundary {
				case "stopped":
					v.Finish("stopped", "用户停止")
				case "deadline":
					v.Deadline = time.Now().Add(-time.Second)
				case "idle":
					v.LastUser = time.Now().Add(-2 * v.Limits.Idle)
				}
				return nil
			}, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			called := false
			_, err = store.CommitCheckpoint(ctx, session.ID, 0, point, []byte("checkpoint"), func(*Session) ([]checkpointEvent, error) { called = true; return nil, nil })
			if err == nil || called {
				t.Fatalf("closed session reached callback: called=%v, err=%v", called, err)
			}
			if _, err = store.LoadCheckpoint(ctx, session.ID, session.ID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("rejected commit left a checkpoint: %v", err)
			}
		})
	}
}

// TestCheckpointValidPendingCanReplaceCorruptedOlderBytes 区分确认损坏的旧检查点与提交新代次：
// 前者必须失败，后者在元数据代次匹配时可用有效草案重建的字节完成替换。
func TestCheckpointValidPendingCanReplaceCorruptedOlderBytes(t *testing.T) {
	ctx := context.Background()
	store, session := recoveryStoreFixture(t)
	first := prepareStorePause(t, store, session.ID, 1)
	oldValue := []byte("old-checkpoint")
	if _, err := store.CommitCheckpoint(ctx, session.ID, 0, first, oldValue, publishStoreQuestion); err != nil {
		t.Fatal(err)
	}
	next := prepareStorePause(t, store, session.ID, 2)
	if _, err := store.db.Exec("UPDATE checkpoints SET data=?", []byte("corrupted-old-bytes")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadCheckpoint(ctx, session.ID, session.ID); !errors.Is(err, ErrCheckpointMismatch) {
		t.Fatalf("corrupt old checkpoint readable: %v", err)
	}
	if _, err := store.CommitCheckpoint(ctx, session.ID, 0, first, oldValue, publishStoreQuestion); !errors.Is(err, ErrCheckpointMismatch) {
		t.Fatalf("duplicate must not confirm corrupted storage: %v", err)
	}
	if _, err := store.CommitCheckpoint(ctx, session.ID, 1, next, []byte("rebuilt-next-checkpoint"), publishStoreQuestion); err != nil {
		t.Fatalf("valid pending should replace old blob under matching metadata CAS: %v", err)
	}
	record, err := store.LoadCheckpoint(ctx, session.ID, session.ID)
	if err != nil || record.Point.Generation != 2 || string(record.Data) != "rebuilt-next-checkpoint" {
		t.Fatalf("rebuilt checkpoint: %+v, %v", record, err)
	}
	assertStoreEventCount(t, store, session.ID, "checkpoint_committed", 2)
}

// TestCheckpointNewMetadataSurvivesReopenAndDelete 验证恢复身份可跨数据库重开保存，
// 并随会话删除级联清理，避免留下可误用的孤立检查点。
func TestCheckpointNewMetadataSurvivesReopenAndDelete(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	session := Session{ID: newID(), Owner: "owner", LastUser: time.Now(), Limits: Limits{Idle: time.Hour}}
	if err = store.Create(ctx, session); err != nil {
		t.Fatal(err)
	}
	point := prepareStorePause(t, store, session.ID, 1)
	committed, err := store.CommitCheckpoint(ctx, session.ID, 0, point, []byte("persisted-checkpoint"), publishStoreQuestion)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	record, err := store.LoadCheckpoint(ctx, session.ID, session.ID)
	if err != nil || record.Point == nil || *record.Point != *committed.ResumePoint || string(record.Data) != "persisted-checkpoint" {
		t.Fatalf("new metadata after reopen: %+v, %v", record, err)
	}
	assertStoreEventCount(t, store, session.ID, "checkpoint_committed", 1)
	if err = store.Delete(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"sessions", "checkpoints", "events"} {
		var count int
		// 表名只取本测试的固定白名单，不接收外部输入。
		if err = store.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s not cleaned with session: count=%d, err=%v", table, count, err)
		}
	}
}

// TestCheckpointSchemaMigrationPreservesLegacyAndCascades 验证新增元数据列的迁移可重复执行，
// 保留旧 JSON、检查点原始字节和资产文件，同时维持删除级联关系。
func TestCheckpointSchemaMigrationPreservesLegacyAndCascades(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "tripo.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`PRAGMA foreign_keys=ON;
CREATE TABLE sessions(id TEXT PRIMARY KEY, owner TEXT NOT NULL, data BLOB NOT NULL);
CREATE TABLE checkpoints(session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE, key TEXT NOT NULL, data BLOB NOT NULL, PRIMARY KEY(session_id,key));`)
	if err != nil {
		t.Fatal(err)
	}
	// 固定旧 JSON 字面量，避免当前 Session 新字段渗入迁移前的测试数据。
	legacy := []byte(`{"ID":"legacy-session","Owner":"legacy-owner","Request":"旧请求","Status":"running","Production":2,"ModelCalls":7,"Clarifications":1,"Deadline":"2099-01-01T00:00:00Z","LastUser":"2026-09-01T00:00:00Z","Current":{"id":"old-operation","kind":"generate","stage":"submitted","task_id":"original-task"},"Artifacts":[{"id":"existing-artifact","path":"existing.glb","task_id":"older-task"}],"CheckpointReady":false}`)
	opaque := []byte{0x00, 0x01, 0xff, 0x10}
	if _, err = db.Exec("INSERT INTO sessions(id,owner,data) VALUES(?,?,?)", "legacy-session", "legacy-owner", legacy); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO checkpoints(session_id,key,data) VALUES(?,?,?)", "legacy-session", "legacy-session", opaque); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "existing.glb"), []byte("preserved-model"), 0600); err != nil {
		t.Fatal(err)
	}
	// 先手动回滚 DDL，覆盖迁移重试路径；真实进程退出另由迁移崩溃测试验证。
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("ALTER TABLE checkpoints ADD COLUMN metadata BLOB"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		store, openErr := OpenStore(dir)
		if openErr != nil {
			t.Fatal(openErr)
		}
		var persisted []byte
		if err = store.db.QueryRow("SELECT data FROM sessions WHERE id='legacy-session'").Scan(&persisted); err != nil || !bytes.Equal(persisted, legacy) {
			t.Fatalf("migration changed legacy facts: %s, %v", persisted, err)
		}
		record, loadErr := store.LoadCheckpoint(ctx, "legacy-session", "legacy-session")
		if loadErr != nil || record.Point != nil || !bytes.Equal(record.Data, opaque) {
			t.Fatalf("migration changed legacy checkpoint: %+v, %v", record, loadErr)
		}
		if attempt == 2 {
			if err = store.Delete(ctx, "legacy-session"); err != nil {
				t.Fatal(err)
			}
			var remaining int
			if err = store.db.QueryRow("SELECT count(*) FROM checkpoints").Scan(&remaining); err != nil || remaining != 0 {
				t.Fatalf("checkpoint did not cascade: %d, %v", remaining, err)
			}
		}
		if err = store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	content, err := os.ReadFile(filepath.Join(dir, "existing.glb"))
	if err != nil || string(content) != "preserved-model" {
		t.Fatalf("migration changed model file: %q, %v", content, err)
	}
}
