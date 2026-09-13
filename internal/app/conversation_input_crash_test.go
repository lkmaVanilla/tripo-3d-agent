package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lkmaVanilla/tripo-3d-agent/internal/asset"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/testfixture"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
)

// 直接准备已接受的生产操作，隔离上传/提交持久化边界与模型选择。
func conversationInputFixture(t *testing.T) (*Service, Session, AssetVersion) {
	t.Helper()
	s := testService(t, t.TempDir(), &fakeProvider{}, false)
	ctx := context.Background()
	old := s.newRun("owner", "生成木箱", ConversationPromptVersion)
	c, _, _, e := s.store.CreateConversation(ctx, old, "first")
	if e != nil {
		t.Fatal(e)
	}
	b := testfixture.Cube(4500)
	id := newID()
	p := filepath.Join(s.Config.DataDir, "old.glb")
	if e = os.WriteFile(p, b, 0600); e != nil {
		t.Fatal(e)
	}
	_, e = s.store.Edit(ctx, old.ID, func(v *Session) error {
		v.Intent = &Intent{Asset: "木箱", Use: "产品展示", MaxTriangles: 4500, MaxBytes: 10 << 20, Plan: []string{"生成"}}
		v.Current = &Operation{ID: id, Kind: "generate", Stage: "done", TaskID: "original", ArtifactID: id}
		v.Artifacts = []Artifact{{ID: id, TaskID: "original", Path: p, SourceURL: "https://expired.invalid/file", Report: asset.Inspect(b, 4500, 10<<20)}}
		v.Production = 1
		v.SelectedArtifact = id
		return v.finishVerified("completed", "agent", "delivered")
	}, "technical_report", map[string]string{"operation_id": id, "artifact_id": id})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.store.ReleaseConversationRun(ctx, old.ID); e != nil {
		t.Fatal(e)
	}
	version, e := s.store.GetAssetVersion(ctx, c.ID, id)
	if e != nil {
		t.Fatal(e)
	}
	v := s.newRun("owner", "减面到3000", ConversationPromptVersion)
	v, _, e = s.store.AppendConversationRun(ctx, c.ID, "owner", "edit", id, v)
	if e != nil {
		t.Fatal(e)
	}
	v, e = s.store.Edit(ctx, v.ID, func(v *Session) error {
		v.Status = "running"
		v.HasSlot = true
		v.GoalKind = "decimate"
		v.Intent = &Intent{Asset: "木箱", Use: "产品展示", MaxTriangles: 3000, MaxBytes: 10 << 20, Plan: []string{"减面"}}
		v.Current = &Operation{ID: newID(), Kind: "decimate", Stage: "ready", Params: tripo.Params{FaceLimit: 3000, TextureQuality: "standard", Input: "version:" + id}, InputVersionID: id, InputSHA256: version.SHA256}
		return nil
	}, "", nil)
	if e != nil {
		t.Fatal(e)
	}
	return s, v, version
}

type conversationCrashUploader struct{ crashProductionProvider }

func (p conversationCrashUploader) UploadModel(ctx context.Context, b []byte) (string, error) {
	req, e := http.NewRequestWithContext(ctx, "POST", p.base+"/upload", bytes.NewReader(b))
	if e != nil {
		return "", e
	}
	res, e := p.client.Do(req)
	if e != nil {
		return "", e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", fmt.Errorf("controlled upload failure")
	}
	return "file_saved", nil
}

func TestConversationInputCrashChild(t *testing.T) {
	if os.Getenv("CONVERSATION_INPUT_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	cfg := DefaultConfig()
	cfg.DataDir = os.Getenv("CONVERSATION_INPUT_DIR")
	cfg.PollInterval = time.Millisecond
	s, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	p := conversationCrashUploader{crashProductionProvider{base: os.Getenv("CONVERSATION_INPUT_URL"), mode: os.Getenv("CONVERSATION_INPUT_POINT"), client: http.DefaultClient}}
	s.provider = p
	if p.mode == "token-saved" {
		_, e = s.store.db.Exec(`CREATE TEMP TRIGGER stop_before_production BEFORE UPDATE ON sessions WHEN json_extract(NEW.data,'$.Current.stage')='submitting' BEGIN SELECT RAISE(ABORT,'controlled token boundary'); END`)
		if e != nil {
			t.Fatal(e)
		}
	}
	v, e := s.store.Get(context.Background(), os.Getenv("CONVERSATION_INPUT_ID"))
	if e != nil {
		t.Fatal(e)
	}
	_, _ = s.production(context.Background(), v.ID, v.Current.ID)
	if p.mode == "token-saved" {
		_ = p.barrier(context.Background(), p.mode)
	}
	t.Fatal("expected force-kill boundary")
}

func TestConversationInputForcedExit(t *testing.T) {
	for _, point := range []string{"upload-response", "token-saved", "before-submit", "known-task"} {
		t.Run(point, func(t *testing.T) {
			s, before, version := conversationInputFixture(t)
			defer s.Close()
			reached := make(chan string, 4)
			var recovering atomic.Bool
			var uploads, submits, queries atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/upload":
					uploads.Add(1)
					var got bytes.Buffer
					_, _ = got.ReadFrom(r.Body)
					want, _ := versionBytes(version)
					if !bytes.Equal(got.Bytes(), want) {
						http.Error(w, "wrong input", 400)
						return
					}
					if point == "upload-response" && !recovering.Load() {
						reached <- point
						<-r.Context().Done()
						return
					}
				case "/submit":
					submits.Add(1)
					_, _ = w.Write([]byte("known-task"))
				case "/query":
					queries.Add(1)
					task := tripo.Task{ID: "known-task", Status: "success", Progress: 100}
					task.Output.ModelURL = "https://fixture.example/output"
					_ = json.NewEncoder(w).Encode(task)
				case "/download":
					_, _ = w.Write(testfixture.Cube(3000))
				case "/barrier":
					reached <- r.URL.Query().Get("point")
					<-r.Context().Done()
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			cmd := exec.Command(os.Args[0], "-test.run=^TestConversationInputCrashChild$", "-test.count=1")
			cmd.Env = append(os.Environ(), "CONVERSATION_INPUT_CHILD=1", "CONVERSATION_INPUT_DIR="+s.Config.DataDir, "CONVERSATION_INPUT_URL="+server.URL, "CONVERSATION_INPUT_ID="+before.ID, "CONVERSATION_INPUT_POINT="+point)
			var output bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &output
			if e := cmd.Start(); e != nil {
				t.Fatal(e)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			killed := false
			defer func() {
				if !killed {
					_ = cmd.Process.Kill()
					<-done
				}
			}()
			select {
			case got := <-reached:
				if got != point {
					t.Fatal(got)
				}
			case e := <-done:
				killed = true
				t.Fatalf("child exited %v: %s", e, output.String())
			case <-time.After(20 * time.Second):
				t.Fatal("no boundary")
			}
			if e := cmd.Process.Kill(); e != nil {
				t.Fatal(e)
			}
			<-done
			killed = true
			interrupted, e := s.store.Get(context.Background(), before.ID)
			if e != nil {
				t.Fatal(e)
			}
			if interrupted.Current.PreparedInput == nil || interrupted.Current.PreparedInput.Attempts != 1 || interrupted.Current.InputSHA256 != version.SHA256 {
				t.Fatal("input identity or attempts lost")
			}
			if point == "upload-response" && interrupted.Current.PreparedInput.Token != "" {
				t.Fatal("invented token")
			}
			if point != "upload-response" && interrupted.Current.PreparedInput.Token != "file_saved" {
				t.Fatal("saved token lost")
			}
			recovering.Store(true)
			s.provider = conversationCrashUploader{crashProductionProvider{base: server.URL, client: server.Client()}}
			_, e = s.production(context.Background(), before.ID, before.Current.ID)
			after, _ := s.store.Get(context.Background(), before.ID)
			if point == "before-submit" {
				if e == nil || submits.Load() != 0 || queries.Load() != 0 || after.Production != 1 {
					t.Fatal("unknown submission resent")
				}
			} else {
				if e != nil || submits.Load() != 1 || len(after.Artifacts) != 1 || after.Production != 1 || !after.Artifacts[0].Report.Passed {
					t.Fatalf("recovery failed %v: %s", e, jsonString(after))
				}
				outputVersion, e := s.store.GetAssetVersion(context.Background(), version.ConversationID, after.Artifacts[0].ID)
				if e != nil || outputVersion.ParentVersionID != version.ID {
					t.Fatal("lineage changed")
				}
			}
			if !interrupted.Deadline.IsZero() && !after.Deadline.Equal(interrupted.Deadline) {
				t.Fatal("deadline reset")
			}
			expected := int32(1)
			if point == "upload-response" {
				expected = 2
			}
			if uploads.Load() != expected {
				t.Fatal("unexpected reupload", uploads.Load())
			}
			if after.Current.InputSHA256 != version.SHA256 || after.Current.Params.Input != "version:"+version.ID {
				t.Fatal("semantic input changed")
			}
			if point != "upload-response" && after.Current.PreparedInput.Attempts != 1 {
				t.Fatal("token reused incorrectly")
			}
		})
	}
}

func TestConversationInputUploadBudgetAndFailure(t *testing.T) {
	s, v, version := conversationInputFixture(t)
	dir := s.Config.DataDir
	p := &conversationProvider{failUploads: true}
	s.provider = p
	// 前两次在发送前已持久化且进程退出，第三次失败后不得重新取得额度。
	for attempts := 1; attempts <= 2; attempts++ {
		_, e := s.store.Edit(context.Background(), v.ID, func(x *Session) error { x.Current.PreparedInput = &PreparedInput{Attempts: attempts}; return nil }, "", nil)
		if e != nil {
			t.Fatal(e)
		}
		_ = s.Close()
		s = testService(t, dir, &fakeProvider{}, false)
		s.provider = p
	}
	defer s.Close()
	result, e := s.production(context.Background(), v.ID, v.Current.ID)
	if e != nil || !strings.Contains(result, "input_preparation_failed") {
		t.Fatalf("failure not structured: %s %v", result, e)
	}
	saved, _ := s.store.Get(context.Background(), v.ID)
	if saved.Production != 0 || !saved.Deadline.IsZero() || saved.Current.PreparedInput.Attempts != 3 || len(saved.Artifacts) != 0 || len(p.uploads) != 1 || len(p.params) != 0 {
		t.Fatal("upload budget or production mutated")
	}
	if _, e = versionBytes(version); e != nil {
		t.Fatal("old version lost")
	}
	_, _ = s.production(context.Background(), v.ID, v.Current.ID)
	if len(p.uploads) != 1 {
		t.Fatal("finished preparation retried")
	}
}
