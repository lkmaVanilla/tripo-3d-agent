package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/asset"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/testfixture"
)

// 以下复制/审计函数仅服务临时副本演练，不是产品的自动备份或回退接口。
// 调用方必须先关闭全部 SQLite 连接及工作进程；未知外部事实不能靠数据库差分排除。
func copyUpgradeTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("backup refuses non-regular file %s", relative)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, 0600)
	})
}

// legacyUpgradeFacts 忽略新增派生表和访客 GET 续期，但逐字覆盖旧执行、事件、checkpoint
// 以及所有模型文件。仅用于已隔离且确认无外部动作的演练，不把缺少事件视为无副作用证明。
func legacyUpgradeFacts(dir string) (string, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "tripo.db")+"?mode=ro")
	if err != nil {
		return "", err
	}
	defer db.Close()
	hash := sha256.New()
	for _, query := range []string{"SELECT * FROM sessions ORDER BY id", "SELECT * FROM events ORDER BY seq", "SELECT * FROM checkpoints ORDER BY session_id,key"} {
		rows, err := db.Query(query)
		if err != nil {
			return "", err
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			return "", err
		}
		fmt.Fprintln(hash, query)
		for rows.Next() {
			values, pointers := make([]any, len(columns)), make([]any, len(columns))
			for index := range values {
				pointers[index] = &values[index]
			}
			if err = rows.Scan(pointers...); err != nil {
				rows.Close()
				return "", err
			}
			if err = json.NewEncoder(hash).Encode(values); err != nil {
				rows.Close()
				return "", err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return "", err
		}
	}
	err = filepath.WalkDir(filepath.Join(dir, "artifacts"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(hash, "%s:%x\n", relative, sha256.Sum256(body))
		return nil
	})
	return hex.EncodeToString(hash.Sum(nil)), err
}

func restoreUpgradeCopy(live, backup string, isolatedWithoutExternalActions bool) (string, error) {
	if !isolatedWithoutExternalActions {
		return "", errors.New("forward-only: external activity cannot be excluded")
	}
	before, err := legacyUpgradeFacts(backup)
	if err != nil {
		return "", err
	}
	after, err := legacyUpgradeFacts(live)
	if err != nil {
		return "", err
	}
	if before != after {
		return "", errors.New("forward-only: accepted message or execution/file facts changed")
	}
	staging, err := os.MkdirTemp(filepath.Dir(live), "upgrade-restoring-")
	if err != nil {
		return "", err
	}
	if err = copyUpgradeTree(backup, staging); err != nil {
		return "", err
	}
	quarantine := live + "-before-restore-" + newID()
	if err = os.Rename(live, quarantine); err != nil {
		return "", err
	}
	if err = os.Rename(staging, live); err != nil {
		return quarantine, errors.Join(err, os.Rename(quarantine, live))
	}
	return quarantine, nil
}

func assertUpgradeDownload(t *testing.T, s *Service, token, runID, artifactID string, body []byte) {
	t.Helper()
	for _, path := range []string{
		"/api/sessions/" + runID + "/artifacts/" + artifactID + "?download=1",
		"/api/conversations/" + runID + "/versions/" + artifactID + "/file?download=1",
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest("GET", path, nil)
		request.AddCookie(&http.Cookie{Name: cookieName, Value: token})
		s.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || !bytes.Equal(recorder.Body.Bytes(), body) {
			t.Fatalf("restored download %s: status=%d bytes=%d", path, recorder.Code, recorder.Body.Len())
		}
	}
}

// TestConversationUpgradeBackupRestoreDrill 覆盖停写副本、仅迁移回退、同路径模型恢复，
// 随后证明原 v1 checkpoint 能在兼容 Runner 中继续；没有真实 LLM/Tripo 或用户数据。
func TestConversationUpgradeBackupRestoreDrill(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	live, backup := filepath.Join(root, "live"), filepath.Join(root, "backup")
	provider := &fakeProvider{}
	s := testService(t, live, provider, false)
	paused := prepareFrozenV1Pause(t, s, false, false)
	checkpoint, err := s.store.LoadCheckpoint(ctx, paused.ID, paused.ID)
	if err != nil {
		t.Fatal(err)
	}
	var originalRunJSON []byte
	if err = s.store.db.QueryRow("SELECT data FROM sessions WHERE id=?", paused.ID).Scan(&originalRunJSON); err != nil {
		t.Fatal(err)
	}
	token, owner, err := s.store.Visitor(ctx, "", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	completed := conversationTestRun(owner, "旧版木箱成品")
	completed.ExecutionVersion = ""
	completed.Status, completed.Ended = "completed", time.Now().UTC()
	completed.Expires = completed.Ended.Add(7 * 24 * time.Hour)
	completed.Intent = &Intent{Asset: "木箱", Use: "产品展示", MaxTriangles: 5000, MaxBytes: 10 << 20}
	modelBytes := testfixture.Cube(12)
	artifactID := newID()
	modelPath := filepath.Join(live, "artifacts", completed.ID, artifactID+".glb")
	if err = os.MkdirAll(filepath.Dir(modelPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(modelPath, modelBytes, 0600); err != nil {
		t.Fatal(err)
	}
	completed.Artifacts = []Artifact{{ID: artifactID, TaskID: "old-known-task", Path: modelPath, Report: asset.Inspect(modelBytes, 5000, 10<<20)}}
	completed.SelectedArtifact = artifactID
	if _, err = s.store.db.Exec("INSERT INTO sessions(id,owner,data) VALUES(?,?,?)", completed.ID, owner, jsonString(completed)); err != nil {
		t.Fatal(err)
	}
	// 移除新会话派生结构得到真正的迁移前副本，原 Session 和 checkpoint 字节不动。
	for _, table := range []string{"conversation_run_views", "conversation_commands", "conversation_messages", "asset_versions", "conversation_runs", "conversations", "conversation_schema"} {
		if _, err = s.store.db.Exec("DROP TABLE " + table); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if err = copyUpgradeTree(live, backup); err != nil {
		t.Fatal(err)
	}
	baseline, err := legacyUpgradeFacts(backup)
	if err != nil {
		t.Fatal(err)
	}
	// 仅 New/OpenStore 完成迁移，整个核对窗口都不调用 Start。
	s = testService(t, live, provider, false)
	var integrity string
	if err = s.store.db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("migrated SQLite integrity: %s %v", integrity, err)
	}
	var mappingCount int
	if err = s.store.db.QueryRow("SELECT COUNT(*) FROM conversation_runs").Scan(&mappingCount); err != nil || mappingCount != 2 {
		t.Fatalf("legacy wrapping count=%d err=%v", mappingCount, err)
	}
	assertUpgradeDownload(t, s, token, completed.ID, artifactID, modelBytes)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if got, e := legacyUpgradeFacts(live); e != nil || got != baseline {
		t.Fatalf("migration-only preflight changed old facts: %s %v", got, e)
	}
	quarantine, err := restoreUpgradeCopy(live, backup, true)
	if err != nil || quarantine == "" {
		t.Fatalf("whole-directory restore failed: %v", err)
	}
	if got, e := os.ReadFile(modelPath); e != nil || !bytes.Equal(got, modelBytes) || !filepath.IsAbs(modelPath) {
		t.Fatalf("restoration did not preserve original absolute file path: %v", e)
	}
	s = testService(t, live, provider, false)
	defer s.Close()
	restoredCheckpoint, err := s.store.LoadCheckpoint(ctx, paused.ID, paused.ID)
	var restoredRunJSON []byte
	if e := s.store.db.QueryRow("SELECT data FROM sessions WHERE id=?", paused.ID).Scan(&restoredRunJSON); e != nil {
		t.Fatal(e)
	}
	if err != nil || !reflect.DeepEqual(checkpoint, restoredCheckpoint) || !bytes.Equal(originalRunJSON, restoredRunJSON) {
		t.Fatal("restore changed old checkpoint or Session bytes")
	}
	assertUpgradeDownload(t, s, token, completed.ID, artifactID, modelBytes)
	// 新消息一经持久接受就越过回退边界，即使调度尚未调用模型或 Provider。
	_, accepted, err := s.CreateAssetConversation(ctx, owner, "新的产品展示木箱", "after-upgrade-message")
	if err != nil || accepted.ExecutionVersion != ConversationPromptVersion || accepted.ModelCalls != 0 || provider.Count() != 0 {
		t.Fatalf("new message fixture: %+v %v", accepted, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = restoreUpgradeCopy(live, backup, true); err == nil || !strings.Contains(err.Error(), "forward-only") {
		t.Fatalf("new message accepted before execution allowed rollback: %v", err)
	}
	s = testService(t, live, provider, false)
	defer s.Close()
	if preserved, e := s.store.Get(ctx, accepted.ID); e != nil || preserved.Request != accepted.Request {
		t.Fatal("refused rollback lost the newly accepted message")
	}
	newCalls := 0
	s.modelFactory = func(context.Context) (model.BaseChatModel, error) {
		return protocolModel{generate: func(_ context.Context, in []*schema.Message) (*schema.Message, error) {
			newCalls++
			if len(in) == 0 || !strings.HasPrefix(in[0].Content, instruction+"\n") {
				return nil, errors.New("restored checkpoint lost frozen v1 instruction")
			}
			for _, message := range in {
				if message.Role == schema.Tool && strings.Contains(message.Content, `"user_answer":"卡通"`) {
					return protocolProposal("finish_request", "restored-v1-finish", json.RawMessage(`{"deliver":false,"artifact_id":"","explanation":"不再制作"}`)), nil
				}
			}
			return nil, errors.New("restored v1 question answer missing")
		}}, nil
	}
	if err = s.Answer(ctx, paused.ID, "卡通"); err != nil {
		t.Fatal(err)
	}
	if err = s.run(ctx, paused.ID); err != nil {
		t.Fatal(err)
	}
	after, err := s.store.Get(ctx, paused.ID)
	if err != nil || !after.Terminal() || newCalls != 1 || after.ModelCalls != paused.ModelCalls+1 || provider.Count() != 0 || after.Answers[paused.WaitID].Text != "卡通" {
		t.Fatalf("restored original checkpoint cannot continue: calls=%d run=%+v err=%v", newCalls, after, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	// 已接受答案且调用过模型，旧快照不能再覆盖这些新事实。
	factsBeforeRefusal, _ := legacyUpgradeFacts(live)
	if _, err = restoreUpgradeCopy(live, backup, true); err == nil || !strings.Contains(err.Error(), "forward-only") {
		t.Fatalf("accepted facts allowed destructive rollback: %v", err)
	}
	if factsAfterRefusal, e := legacyUpgradeFacts(live); e != nil || factsBeforeRefusal != factsAfterRefusal {
		t.Fatal("refused rollback mutated current facts")
	}
	// 即使本地尚无新增记录，不能排除外部动作时仍拒绝；数据库沉默不是恢复授权。
	if _, err = restoreUpgradeCopy(backup, backup, false); err == nil || !strings.Contains(err.Error(), "external activity") {
		t.Fatalf("unknown external actions allowed snapshot restore: %v", err)
	}
}
