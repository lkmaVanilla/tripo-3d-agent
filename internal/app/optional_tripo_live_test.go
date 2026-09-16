package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/joho/godotenv"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
)

// 显式 opt-in；最多一个生成和一个减面，保存实际 GLB 和技术证据，不调用真实 LLM。
func TestOptionalTripoLive(t *testing.T) {
	if os.Getenv("RUN_OPTIONAL_TRIPO") != "1" {
		t.Skip("opt-in real Tripo optional-constraint smoke")
	}
	base := os.Getenv("OPTIONAL_TRIPO_EVIDENCE_DIR")
	if !filepath.IsAbs(base) {
		t.Fatal("必须提供绝对证据目录")
	}
	values, e := godotenv.Read(filepath.Join("..", "..", ".env"))
	if e != nil {
		t.Fatal("无法读取本地供应商配置")
	}
	key := strings.TrimSpace(os.Getenv("TRIPO_API_KEY"))
	if key == "" {
		key = strings.TrimSpace(values["TRIPO_API_KEY"])
	}
	if key == "" {
		t.Fatal("缺少 Tripo 凭证，未执行")
	}
	if e = os.MkdirAll(base, 0700); e != nil {
		t.Fatal(e)
	}
	dir, e := os.MkdirTemp(base, time.Now().UTC().Format("20060102T150405Z")+"-")
	if e != nil {
		t.Fatal(e)
	}
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.TripoKey = key
	cfg.MaxSubmissions = 1
	cfg.PollInterval = 2 * time.Second
	s, e := New(cfg)
	if e != nil {
		t.Fatal("服务初始化失败")
	}
	defer s.Close()
	s.provider = tripo.New(key)
	s.ready = true
	s.source = "real-tripo-optional-controlled-agent"
	s.modelFactory = func(context.Context) (model.BaseChatModel, error) { return optionalScript{}, nil }
	if e = s.Start(); e != nil {
		t.Fatal(s.redact(e.Error()))
	}
	ctx := context.Background()
	c, run, e := s.CreateAssetConversation(ctx, "optional-live-fixture", "为产品展示生成木箱，不限制面数和文件体积", "first")
	if e != nil {
		t.Fatal(s.redact(e.Error()))
	}
	t.Log("OPTIONAL_TRIPO_EVIDENCE_DIR=" + dir)
	defer func() {
		s.cancel()
		s.mu.Lock()
		for _, cancel := range s.active {
			cancel()
		}
		s.mu.Unlock()
		s.wg.Wait()
		snap, e := s.store.ConversationSnapshot(ctx, c.ID, "optional-live-fixture", 0, 0, 100)
		if e != nil {
			t.Error(e)
			return
		}
		// 实际输入就是第一份输出；文件保存为测试证据，正式用户数据目录始终不参与。
		files := map[string]any{}
		for _, version := range snap.Versions {
			v, e := s.store.Get(ctx, version.SourceRunID)
			if e != nil {
				t.Error(e)
				continue
			}
			for _, a := range v.Artifacts {
				if a.ID != version.ID {
					continue
				}
				b, e := os.ReadFile(a.Path)
				if e != nil {
					t.Error(e)
					continue
				}
				name := a.ID + ".glb"
				if e = os.WriteFile(filepath.Join(dir, name), b, 0600); e != nil {
					t.Error(e)
					continue
				}
				sum := sha256.Sum256(b)
				files[a.ID] = map[string]any{"file": name, "sha256": hex.EncodeToString(sum[:]), "bytes": len(b), "parent_version_id": version.ParentVersionID}
			}
		}
		writeConversationEvalJSON(t, filepath.Join(dir, "evidence.json"), s.sanitize(map[string]any{"source": "real Tripo + deterministic Eino decisions; not Agent evaluation", "recorded": time.Now().UTC(), "passed": !t.Failed(), "snapshot": snap, "files": files, "submission_limit": "one generation plus one decimation; no automatic extra production"}))
	}()
	await := func(run Session) Session {
		until := time.Now().Add(15 * time.Minute)
		for time.Now().Before(until) {
			v, e := s.store.Get(ctx, run.ID)
			if e != nil {
				t.Fatal(s.redact(e.Error()))
			}
			if v.Terminal() {
				waitConversationIdle(t, s, c.ID)
				if v.Status != "completed" || v.Production != 1 || len(v.Artifacts) != 1 {
					t.Fatalf("真实验证本步失败：%s", v.Final)
				}
				r := v.Artifacts[0].Report
				if v.Intent.Optional == nil || v.Intent.Optional.MaxTriangles != nil || v.Intent.Optional.MaxBytes != nil || !r.Passed || r.Limits == nil {
					t.Fatal("可选约束证据错误")
				}
				return v
			}
			time.Sleep(time.Second)
		}
		_ = s.Stop(ctx, run.ID)
		t.Fatal("真实步骤超过15分钟，保留失败证据")
		return Session{}
	}
	first := await(run)
	input := first.Artifacts[0]
	if input.Report.Triangles <= 500 {
		t.Fatal("真实输入不足以在现有工具范围进一步减面，未提交第二步")
	}
	run, e = s.ContinueConversation(ctx, c.ID, "optional-live-fixture", "引用这个版本继续降低面数，不设置绝对面数或体积上限", "second", input.ID)
	if e != nil {
		t.Fatal(s.redact(e.Error()))
	}
	second := await(run)
	output := second.Artifacts[0]
	v, e := s.store.GetAssetVersion(ctx, c.ID, output.ID)
	if e != nil || v.ParentVersionID != input.ID || output.Report.Triangles >= input.Report.Triangles || second.Current.PreparedInput == nil || second.Current.SubmissionParams == nil {
		t.Fatal("缺少实际减面、上传输入或父版本证据")
	}
}
