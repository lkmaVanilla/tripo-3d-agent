package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/lkmaVanilla/tripo-3d-agent/internal/asset"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/testfixture"
)

// 子进程使用真实迁移与清理操作；屏障只存在于测试，强杀不经过 defer 回滚。
func TestConversationStorageCrashHelper(t *testing.T) {
	dir := os.Getenv("TRIPO_TEST_CONVERSATION_CRASH_DIR")
	if dir == "" {
		return
	}
	phase := os.Getenv("TRIPO_TEST_CONVERSATION_CRASH_PHASE")
	marker := filepath.Join(dir, "conversation-crash-barrier")
	ctx := context.Background()
	switch phase {
	case "migration_before_commit", "migration_after_commit":
		driver := "sqlite"
		if phase == "migration_before_commit" {
			driver = "conversation-migration-barrier-test"
			sql.Register(driver, migrationBarrierDriver{signal: marker})
		}
		db, err := sql.Open(driver, filepath.Join(dir, "tripo.db"))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		defer db.Close()
		if err = migrateConversations(db); err != nil {
			t.Fatal(err)
		}
	case "cleanup_marked", "cleanup_partial":
		s, err := OpenStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		id := os.Getenv("TRIPO_TEST_CONVERSATION_CRASH_ID")
		c, err := s.GetConversation(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if marked, e := s.MarkConversationCleanup(ctx, id, c.Expires); e != nil || !marked {
			t.Fatalf("cleanup marking: %v %v", marked, e)
		}
		if phase == "cleanup_partial" {
			ids, e := s.ConversationRunIDs(ctx, id)
			if e != nil || len(ids) != 2 {
				t.Fatalf("cleanup run mapping: %v %v", ids, e)
			}
			if e = os.RemoveAll(filepath.Join(dir, "artifacts", ids[0])); e != nil {
				t.Fatal(e)
			}
		}
	default:
		t.Fatalf("unknown child phase %q", phase)
	}
	if err := os.WriteFile(marker, []byte(phase), 0600); err != nil {
		t.Fatal(err)
	}
	select {}
}

func killConversationStorageChild(t *testing.T, dir, phase, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConversationStorageCrashHelper$", "-test.timeout=30s")
	cmd.Env = append(os.Environ(), "TRIPO_TEST_CONVERSATION_CRASH_DIR="+dir, "TRIPO_TEST_CONVERSATION_CRASH_PHASE="+phase, "TRIPO_TEST_CONVERSATION_CRASH_ID="+id)
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
		if _, err := os.Stat(filepath.Join(dir, "conversation-crash-barrier")); err == nil {
			break
		}
		if ctx.Err() != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("%s barrier not reached: %s", phase, output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("child returned normally instead of forced exit")
	}
}

func TestConversationMigrationForcedExitPreservesAllLegacyFacts(t *testing.T) {
	for _, phase := range []string{"migration_before_commit", "migration_after_commit"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			s, err := OpenStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			var originals []Session
			var bodies [][]byte
			checkpoint := []byte{0, 3, 9, 255, 'v', '1'}
			for _, state := range []string{"awaiting_answer", "failed", "expired"} {
				v := conversationTestRun("owner", state)
				v.ExecutionVersion = "asset-agent-v2"
				v.ModelCalls, v.Production, v.Clarifications = 9, 2, 2
				v.Status = state
				if state != "awaiting_answer" {
					v.Status = "failed"
					v.Ended = time.Now().UTC().Add(-time.Hour)
					if state == "expired" {
						v.Ended = v.Ended.Add(-8 * 24 * time.Hour)
					}
					v.Expires = v.Ended.Add(7 * 24 * time.Hour)
				}
				data := testfixture.Cube(12)
				file := filepath.Join(dir, v.ID+".glb")
				if err = os.WriteFile(file, data, 0600); err != nil {
					t.Fatal(err)
				}
				v.Artifacts = []Artifact{{ID: newID(), TaskID: "legacy-task", Path: file, Report: asset.Inspect(data, 5000, 10<<20)}}
				body, _ := json.MarshalIndent(v, "", "  ")
				if _, err = s.db.ExecContext(ctx, "INSERT INTO sessions(id,owner,data) VALUES(?,?,?)", v.ID, v.Owner, body); err != nil {
					t.Fatal(err)
				}
				if _, err = s.db.ExecContext(ctx, "INSERT INTO checkpoints(session_id,key,data) VALUES(?,?,?)", v.ID, v.ID, checkpoint); err != nil {
					t.Fatal(err)
				}
				payload, _ := json.Marshal(map[string]string{"text": v.Request})
				if _, err = s.db.ExecContext(ctx, "INSERT INTO events(session_id,kind,at,data) VALUES(?,?,?,?)", v.ID, "request", v.Created.Format(time.RFC3339Nano), payload); err != nil {
					t.Fatal(err)
				}
				originals, bodies = append(originals, v), append(bodies, body)
			}
			if _, err = s.db.ExecContext(ctx, "DELETE FROM conversation_schema"); err != nil {
				t.Fatal(err)
			}
			s.Close()
			killConversationStorageChild(t, dir, phase, "")
			// 先直接观察崩溃后的持久状态，避免恢复迁移掩盖半提交。
			db, err := sql.Open("sqlite", filepath.Join(dir, "tripo.db"))
			if err != nil {
				t.Fatal(err)
			}
			var count int
			err = db.QueryRow("SELECT COUNT(*) FROM conversations").Scan(&count)
			db.Close()
			want := 0
			if phase == "migration_after_commit" {
				want = 3
			}
			if err != nil || count != want {
				t.Fatalf("partial migration after crash: count=%d want=%d err=%v", count, want, err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				s, err = OpenStore(dir)
				if err != nil {
					t.Fatal(err)
				}
				if err = s.ReconcileConversations(ctx); err != nil {
					t.Fatal(err)
				}
				for i, v := range originals {
					var got, cp []byte
					if err = s.db.QueryRowContext(ctx, "SELECT data FROM sessions WHERE id=?", v.ID).Scan(&got); err != nil {
						t.Fatal(err)
					}
					if err = s.db.QueryRowContext(ctx, "SELECT data FROM checkpoints WHERE session_id=?", v.ID).Scan(&cp); err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(got, bodies[i]) || !bytes.Equal(cp, checkpoint) {
						t.Fatal("migration or reconcile rewrote old execution/checkpoint bytes")
					}
					c, e := s.GetConversation(ctx, v.ID)
					if e != nil || !c.Expires.Equal(v.Expires) || (c.ActiveRunID != "") != !v.Terminal() {
						t.Fatalf("old lifecycle changed: %+v %v", c, e)
					}
					ids, e := s.ConversationRunIDs(ctx, v.ID)
					if e != nil || len(ids) != 1 || ids[0] != v.ID {
						t.Fatal("legacy mapping duplicated", e)
					}
					version, e := s.GetAssetVersion(ctx, c.ID, v.Artifacts[0].ID)
					digest, _, hashErr := conversationFileDigest(v.Artifacts[0].Path)
					if e != nil || hashErr != nil || version.SHA256 != digest || version.Number != 1 {
						t.Fatal("legacy file identity changed", e, hashErr)
					}
				}
				s.Close()
			}
		})
	}
}

func TestConversationCleanupForcedExitStaysInaccessibleAndRetries(t *testing.T) {
	for _, phase := range []string{"cleanup_marked", "cleanup_partial"} {
		t.Run(phase, func(t *testing.T) {
			s, dir := conversationTestStore(t)
			ctx := context.Background()
			first := conversationTestRun("owner", "过期资产")
			c, _, _, err := s.CreateConversation(ctx, first, "create")
			if err != nil {
				t.Fatal(err)
			}
			conversationTestEnd(t, s, first.ID)
			second := conversationTestRun("owner", "第二个目标")
			if _, _, err = s.AppendConversationRun(ctx, c.ID, "owner", "next", "", second); err != nil {
				t.Fatal(err)
			}
			conversationTestEnd(t, s, second.ID)
			other := conversationTestRun("owner", "活动会话")
			if _, _, _, err = s.CreateConversation(ctx, other, "other"); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{first.ID, second.ID, other.ID} {
				path := filepath.Join(dir, "artifacts", id)
				if err = os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(path, "asset.glb"), testfixture.Cube(12), 0600); err != nil {
					t.Fatal(err)
				}
			}
			s.Close()
			killConversationStorageChild(t, dir, phase, c.ID)
			s, err = OpenStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err = s.ReconcileConversations(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err = s.ConversationSnapshot(ctx, c.ID, "owner", 0, 0, 50); !errors.Is(err, ErrConversationExpired) {
				t.Fatalf("half-cleaned snapshot accessible: %v", err)
			}
			if _, _, err = s.AppendConversationRun(ctx, c.ID, "owner", "late", "", conversationTestRun("owner", "续期")); !errors.Is(err, ErrConversationExpired) {
				t.Fatalf("half-cleaned conversation renewed: %v", err)
			}
			// 直接调用实际清理流程：重启后部分目录已经不存在也应完成全会话清理。
			service := &Service{store: s, ctx: ctx, Config: Config{DataDir: dir}}
			service.cleanupConversations(time.Now().Add(8 * 24 * time.Hour))
			service.cleanupConversations(time.Now().Add(8 * 24 * time.Hour))
			if _, err = s.GetConversation(ctx, c.ID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("cleanup did not finish: %v", err)
			}
			for _, id := range []string{first.ID, second.ID} {
				if _, err = os.Stat(filepath.Join(dir, "artifacts", id)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("run files retained after cleanup: %v", err)
				}
				if _, err = s.Get(ctx, id); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("run records retained after cleanup: %v", err)
				}
			}
			if _, err = s.Get(ctx, other.ID); err != nil {
				t.Fatal("cleanup deleted unrelated active conversation", err)
			}
			if _, err = os.Stat(filepath.Join(dir, "artifacts", other.ID, "asset.glb")); err != nil {
				t.Fatal("cleanup deleted unrelated active asset", err)
			}
		})
	}
}
