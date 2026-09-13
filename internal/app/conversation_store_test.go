package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/lkmaVanilla/tripo-3d-agent/internal/asset"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/testfixture"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
)

func conversationTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}
func conversationTestRun(owner, request string) Session {
	now := time.Now().UTC()
	c := DefaultConfig()
	return Session{ID: newID(), Owner: owner, Request: request, Status: "understanding", Created: now, LastUser: now, ExecutionVersion: CurrentPromptVersion, Limits: Limits{Calls: c.MaxCalls, Submissions: c.MaxSubmissions, Clarifications: c.MaxClarifications, Duration: c.MaxDuration, Idle: c.IdleTTL, Retention: c.Retention}}
}
func conversationTestEnd(t *testing.T, s *Store, id string) {
	t.Helper()
	_, err := s.Edit(context.Background(), id, func(v *Session) error { return v.finishVerified("failed", "runtime", "execution_failed") }, "execution_failed", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseConversationRun(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}
func conversationTestOutput(t *testing.T, s *Store, dir string, run Session, parent string) Artifact {
	t.Helper()
	data := testfixture.Cube(12)
	id := newID()
	path := filepath.Join(dir, id+".glb")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	a := Artifact{ID: id, TaskID: "task-" + id, Path: path, SourceURL: "https://fixture.example/" + id, Report: asset.Inspect(data, 5000, 10<<20)}
	kind := "generate"
	if parent != "" {
		kind = "decimate"
	}
	_, err := s.Edit(context.Background(), run.ID, func(v *Session) error {
		v.Intent = &Intent{Asset: "茶壶", Use: "产品展示", MaxTriangles: 5000, MaxBytes: 10 << 20, Plan: []string{"生成"}}
		v.Artifacts = append(v.Artifacts, a)
		v.Current = &Operation{ID: id, Kind: kind, Stage: "done", TaskID: a.TaskID, ArtifactID: id, InputVersionID: parent}
		return nil
	}, "technical_report", map[string]any{"operation_id": id, "artifact_id": id, "task_id": a.TaskID, "report": a.Report})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestConversationCreateAndAppendIdempotency(t *testing.T) {
	s, _ := conversationTestStore(t)
	ctx := context.Background()
	first := conversationTestRun("owner", "茶壶")
	c, run, duplicate, err := s.CreateConversation(ctx, first, "create-1")
	if err != nil || duplicate || run.ID != first.ID {
		t.Fatalf("first creation: %+v %v %v", run, duplicate, err)
	}
	again := conversationTestRun("owner", "茶壶")
	repeated, saved, duplicate, err := s.CreateConversation(ctx, again, "create-1")
	if err != nil || !duplicate || saved.ID != first.ID || repeated.ID != c.ID {
		t.Fatalf("creation retry: %+v %v %v", saved, duplicate, err)
	}
	again.Request = "路灯"
	if _, _, _, err = s.CreateConversation(ctx, again, "create-1"); !errors.Is(err, ErrMessageConflict) {
		t.Fatalf("payload conflict: %v", err)
	}
	next := conversationTestRun("owner", "解释面数")
	if _, _, err = s.AppendConversationRun(ctx, c.ID, "owner", "next", "", next); !errors.Is(err, ErrConversationBusy) {
		t.Fatalf("busy accepted: %v", err)
	}
	conversationTestEnd(t, s, first.ID)
	saved, duplicate, err = s.AppendConversationRun(ctx, c.ID, "owner", "next", "", next)
	if err != nil || duplicate || saved.ID != next.ID {
		t.Fatal(saved, duplicate, err)
	}
	saved, duplicate, err = s.AppendConversationRun(ctx, c.ID, "owner", "next", "", conversationTestRun("owner", "解释面数"))
	if err != nil || !duplicate || saved.ID != next.ID {
		t.Fatal(saved, duplicate, err)
	}
	if _, _, err = s.AppendConversationRun(ctx, c.ID, "other", "x", "", conversationTestRun("other", "解释")); !errors.Is(err, ErrConversationExpired) {
		t.Fatalf("owner isolation: %v", err)
	}
	snapshot, err := s.ConversationSnapshot(ctx, c.ID, "owner", 0, 0, 50)
	if err != nil || len(snapshot.Runs) != 2 {
		t.Fatal(len(snapshot.Runs), err)
	}
	users := 0
	for _, m := range snapshot.Messages {
		if m.Kind == "user" {
			users++
		}
	}
	if users != 2 {
		t.Fatalf("duplicate visible messages: %d", users)
	}
}

func TestConversationConcurrentAdmissionAndDrain(t *testing.T) {
	s, _ := conversationTestStore(t)
	ctx := context.Background()
	first := conversationTestRun("owner", "茶壶")
	c, _, _, err := s.CreateConversation(ctx, first, "create")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Edit(ctx, first.ID, func(v *Session) error { return v.finishVerified("stopped", "runtime", "user_stop") }, "stopped", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.AppendConversationRun(ctx, c.ID, "owner", "drain", "", conversationTestRun("owner", "后续")); !errors.Is(err, ErrConversationBusy) {
		t.Fatalf("terminal worker released before exit: %v", err)
	}
	if err = s.ReleaseConversationRun(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	start := make(chan struct{})
	for _, key := range []string{"a", "b"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			<-start
			_, _, e := s.AppendConversationRun(ctx, c.ID, "owner", key, "", conversationTestRun("owner", key))
			results <- e
		}(key)
	}
	close(start)
	wg.Wait()
	close(results)
	accepted, busy := 0, 0
	for e := range results {
		if e == nil {
			accepted++
		} else if errors.Is(e, ErrConversationBusy) {
			busy++
		} else {
			t.Fatal(e)
		}
	}
	if accepted != 1 || busy != 1 {
		t.Fatalf("concurrent admission accepted=%d busy=%d", accepted, busy)
	}
	before, _ := s.GetConversation(ctx, c.ID)
	if err = s.ReleaseConversationRun(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	after, _ := s.GetConversation(ctx, c.ID)
	if before.ActiveRunID != after.ActiveRunID {
		t.Fatal("late release removed newer worker")
	}
}

func TestConversationVersionsKeepLineageAndPrivateInput(t *testing.T) {
	s, dir := conversationTestStore(t)
	ctx := context.Background()
	first := conversationTestRun("owner", "茶壶")
	c, _, _, err := s.CreateConversation(ctx, first, "create")
	if err != nil {
		t.Fatal(err)
	}
	v1 := conversationTestOutput(t, s, dir, first, "")
	original, _ := s.Get(ctx, first.ID)
	conversationTestEnd(t, s, first.ID)
	second := conversationTestRun("owner", "减面")
	second, _, err = s.AppendConversationRun(ctx, c.ID, "owner", "second", v1.ID, second)
	if err != nil {
		t.Fatal(err)
	}
	if second.InputVersion == nil || second.InputVersion.Path != v1.Path || len(second.Artifacts) != 0 {
		t.Fatal("input copied to output or lost private path")
	}
	reloaded, _ := s.Get(ctx, second.ID)
	if reloaded.ConversationContext["input_intent"] == nil {
		t.Fatal("input intent not persisted")
	}
	v2 := conversationTestOutput(t, s, dir, second, v1.ID)
	conversationTestEnd(t, s, second.ID)
	third := conversationTestRun("owner", "从旧版另一种减面")
	third, _, err = s.AppendConversationRun(ctx, c.ID, "owner", "third", v1.ID, third)
	if err != nil {
		t.Fatal(err)
	}
	v3 := conversationTestOutput(t, s, dir, third, v1.ID)
	_, err = s.Edit(ctx, third.ID, nil, "technical_report", map[string]any{"operation_id": v3.ID, "artifact_id": v3.ID, "task_id": v3.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.ConversationSnapshot(ctx, c.ID, "owner", 0, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Versions) != 3 {
		t.Fatalf("versions duplicated: %d", len(snap.Versions))
	}
	if snap.Versions[1].ID != v2.ID || snap.Versions[1].ParentVersionID != v1.ID || snap.Versions[2].ParentVersionID != v1.ID {
		t.Fatal("parent was inferred from sequence")
	}
	current, _ := s.Get(ctx, first.ID)
	if !reflect.DeepEqual(original.Artifacts, current.Artifacts) {
		t.Fatal("new target changed old report")
	}
	encoded, _ := json.Marshal(snap)
	if bytes.Contains(encoded, []byte(dir)) || bytes.Contains(encoded, []byte("source_url")) {
		t.Fatal("public version leaked private storage")
	}
	for _, v := range snap.Versions {
		if !v.Processable || v.SHA256 == "" || v.Number < 1 {
			t.Fatal("missing verified file identity")
		}
	}
	conversationTestEnd(t, s, third.ID)
	other := conversationTestRun("owner", "路灯")
	oc, _, _, _ := s.CreateConversation(ctx, other, "other")
	conversationTestEnd(t, s, other.ID)
	if _, _, err = s.AppendConversationRun(ctx, oc.ID, "owner", "cross", v1.ID, conversationTestRun("owner", "减面")); !errors.Is(err, ErrInvalidVersion) {
		t.Fatalf("same visitor crossed conversation: %v", err)
	}
}

func TestConversationProjectionIsIncrementalAndCursorStable(t *testing.T) {
	s, _ := conversationTestStore(t)
	ctx := context.Background()
	first := conversationTestRun("owner", "茶壶")
	c, _, _, err := s.CreateConversation(ctx, first, "create")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		_, err = s.Edit(ctx, first.ID, nil, "tripo_progress", map[string]any{"operation_id": "operation", "task_id": "task", "status": "running", "progress": i})
		if err != nil {
			t.Fatal(err)
		}
	}
	snap, err := s.ConversationSnapshot(ctx, c.ID, "owner", 0, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	cards := 0
	for _, m := range snap.Messages {
		if m.Kind == "operation_card" {
			cards++
		}
	}
	if cards != 1 {
		t.Fatalf("polling produced %d cards", cards)
	}
	cursor := snap.Cursor
	conversationTestEnd(t, s, first.ID)
	next := conversationTestRun("owner", "解释")
	if _, _, err = s.AppendConversationRun(ctx, c.ID, "owner", "next", "", next); err != nil {
		t.Fatal(err)
	}
	// 故意让原 Session JSON 不再可反序列化，证明正常快照读取历史投影而非历史内部消息。
	if _, err = s.db.ExecContext(ctx, "UPDATE sessions SET data=? WHERE id=?", []byte("invalid-internal-history"), first.ID); err != nil {
		t.Fatal(err)
	}
	snap, err = s.ConversationSnapshot(ctx, c.ID, "owner", cursor, 0, 50)
	if err != nil || len(snap.Runs) != 2 {
		t.Fatalf("snapshot reread historical Session: %v", err)
	}
	for _, e := range snap.Events {
		if e.Seq <= cursor {
			t.Fatal("event cursor reset between runs")
		}
	}
}

func TestConversationRetentionAndCleanupProtection(t *testing.T) {
	s, _ := conversationTestStore(t)
	ctx := context.Background()
	first := conversationTestRun("owner", "茶壶")
	c, _, _, err := s.CreateConversation(ctx, first, "create")
	if err != nil {
		t.Fatal(err)
	}
	conversationTestEnd(t, s, first.ID)
	old, _ := s.Get(ctx, first.ID)
	before, _ := s.GetConversation(ctx, c.ID)
	if !before.Expires.Equal(old.Ended.Add(7 * 24 * time.Hour)) {
		t.Fatal("retention anchor is not persisted Ended")
	}
	_, _ = s.ConversationSnapshot(ctx, c.ID, "owner", 0, 0, 50)
	after, _ := s.GetConversation(ctx, c.ID)
	if !before.Expires.Equal(after.Expires) {
		t.Fatal("reading renewed conversation")
	}
	next := conversationTestRun("owner", "后续")
	if _, _, err = s.AppendConversationRun(ctx, c.ID, "owner", "next", "", next); err != nil {
		t.Fatal(err)
	}
	if marked, e := s.MarkConversationCleanup(ctx, c.ID, old.Expires.Add(time.Hour)); e != nil || marked {
		t.Fatal("active conversation cleaned", e)
	}
	conversationTestEnd(t, s, next.ID)
	unchanged, _ := s.Get(ctx, first.ID)
	if !old.Ended.Equal(unchanged.Ended) || !old.Expires.Equal(unchanged.Expires) {
		t.Fatal("renewal changed historic expiry")
	}
	if err = s.Delete(ctx, first.ID); err == nil {
		t.Fatal("independent deletion accepted for shared conversation")
	}
	c, _ = s.GetConversation(ctx, c.ID)
	if marked, e := s.MarkConversationCleanup(ctx, c.ID, c.Expires); e != nil || !marked {
		t.Fatal("due cleanup not marked", e)
	}
	if _, err = s.ConversationSnapshot(ctx, c.ID, "owner", 0, 0, 50); !errors.Is(err, ErrConversationExpired) {
		t.Fatal("cleaning conversation remains visible", err)
	}
	if _, _, err = s.AppendConversationRun(ctx, c.ID, "owner", "late", "", conversationTestRun("owner", "后续")); !errors.Is(err, ErrConversationExpired) {
		t.Fatal("accepted while cleaning", err)
	}
	if err = s.DeleteConversation(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteConversation(ctx, c.ID); err != nil {
		t.Fatal("cleanup not idempotent", err)
	}
}

func TestConversationLegacyMigrationPreservesBytes(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	v := conversationTestRun("owner", "旧茶壶")
	v.Status = "failed"
	v.Ended = time.Now().UTC().Add(-24 * time.Hour)
	v.Expires = v.Ended.Add(7 * 24 * time.Hour)
	v.Final = "原始自由解释"
	body, _ := json.Marshal(v)
	if _, err = s.db.ExecContext(ctx, "INSERT INTO sessions(id,owner,data) VALUES(?,?,?)", v.ID, v.Owner, body); err != nil {
		t.Fatal(err)
	}
	checkpoint := []byte("opaque-old-checkpoint")
	if _, err = s.db.ExecContext(ctx, "INSERT INTO checkpoints(session_id,key,data) VALUES(?,?,?)", v.ID, v.ID, checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, "DELETE FROM conversation_schema"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for i := 0; i < 2; i++ {
		s, err = OpenStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		var got, cp []byte
		if err = s.db.QueryRowContext(ctx, "SELECT data FROM sessions WHERE id=?", v.ID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if err = s.db.QueryRowContext(ctx, "SELECT data FROM checkpoints WHERE session_id=?", v.ID).Scan(&cp); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, body) || !bytes.Equal(cp, checkpoint) {
			t.Fatal("migration rewrote legacy protocol")
		}
		c, e := s.GetConversation(ctx, v.ID)
		if e != nil || c.ActiveRunID != "" || !c.Expires.Equal(v.Expires) {
			t.Fatal("migration changed lifecycle", e)
		}
		ids, e := s.ConversationRunIDs(ctx, v.ID)
		if e != nil || len(ids) != 1 {
			t.Fatal("duplicate run mapping", e)
		}
		s.Close()
	}
}

func TestConversationMigrationRollbackAndDrainReconcile(t *testing.T) {
	s, _ := conversationTestStore(t)
	ctx := context.Background()
	v := conversationTestRun("owner", "旧请求")
	b, _ := json.Marshal(v)
	if _, err := s.db.ExecContext(ctx, "INSERT INTO sessions(id,owner,data) VALUES(?,?,?)", v.ID, v.Owner, b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM conversation_schema; CREATE TRIGGER reject_migration BEFORE INSERT ON conversation_run_views BEGIN SELECT RAISE(ABORT,'fixture'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := migrateConversations(s.db); err == nil {
		t.Fatal("migration failure ignored")
	}
	if _, err := s.GetConversation(ctx, v.ID); err == nil {
		t.Fatal("half migrated conversation survived rollback")
	}
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER reject_migration"); err != nil {
		t.Fatal(err)
	}
	if err := migrateConversations(s.db); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Edit(ctx, v.ID, func(v *Session) error { return v.finishVerified("stopped", "runtime", "user_stop") }, "stopped", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileConversations(ctx); err != nil {
		t.Fatal(err)
	}
	c, _ := s.GetConversation(ctx, v.ID)
	if c.ActiveRunID != "" {
		t.Fatal("restart retained stopped worker pointer")
	}
}

func TestConversationLegacyVersionSourceUsesEvidenceNotOrder(t *testing.T) {
	s, dir := conversationTestStore(t)
	ctx := context.Background()
	v := conversationTestRun("owner", "旧资产")
	v.Status = "failed"
	v.Ended, v.Expires = time.Now().UTC(), time.Now().UTC().Add(7*24*time.Hour)
	data := testfixture.Cube(12)
	var artifacts []Artifact
	for i := 0; i < 3; i++ {
		id := newID()
		path := filepath.Join(dir, id+".glb")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		artifacts = append(artifacts, Artifact{ID: id, TaskID: "task-" + id, Path: path, SourceURL: "https://fixture.example/" + id, Report: asset.Inspect(data, 5000, 10<<20)})
	}
	// 子产物位于父产物之前，第三个候选没有可验证操作来源；不能依相邻顺序补链。
	v.Artifacts = []Artifact{artifacts[1], artifacts[0], artifacts[2]}
	body, _ := json.Marshal(v)
	if _, err := s.db.ExecContext(ctx, "INSERT INTO sessions(id,owner,data) VALUES(?,?,?)", v.ID, v.Owner, body); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i, a := range artifacts[:2] {
		kind, params := "generate", tripo.Params{Prompt: "asset"}
		if i == 1 {
			kind, params = "decimate", tripo.Params{Input: artifacts[0].SourceURL, FaceLimit: 1000}
		}
		for _, event := range []struct {
			kind string
			data any
		}{
			{"tool_submitting", map[string]any{"operation_id": a.ID, "kind": kind, "params": params}},
			{"tool_submitted", map[string]any{"operation_id": a.ID, "task_id": a.TaskID}},
			{"technical_report", map[string]any{"operation_id": a.ID, "task_id": a.TaskID, "artifact_id": a.ID}},
		} {
			if err = insertEvent(ctx, tx, v.ID, event.kind, event.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, "DELETE FROM conversation_schema"); err != nil {
		t.Fatal(err)
	}
	if err = migrateConversations(s.db); err != nil {
		t.Fatal(err)
	}
	child, err := s.GetAssetVersion(ctx, v.ID, artifacts[1].ID)
	if err != nil || child.ParentVersionID != artifacts[0].ID || child.ProvenanceStatus != "historical_evidence" {
		t.Fatalf("verifiable old source lost: %+v %v", child, err)
	}
	unknown, err := s.GetAssetVersion(ctx, v.ID, artifacts[2].ID)
	if err != nil || unknown.ParentVersionID != "" || unknown.ProvenanceStatus != "historical_unknown" {
		t.Fatalf("invented old source: %+v %v", unknown, err)
	}
}
