package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/joho/godotenv"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
)

// 真Tripo协议验收与真实LLM决策评测分开。本测试使用真实Eino、确定性提议和临时数据库。
func TestConversationTripoLive(t *testing.T) {
	if os.Getenv("RUN_CONVERSATION_TRIPO") != "1" {
		t.Skip("opt-in real Tripo integration")
	}
	base := os.Getenv("CONVERSATION_TRIPO_EVIDENCE_DIR")
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
		t.Fatal("未配置Tripo凭证")
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
	cfg.PollInterval = 2 * time.Second
	s, e := New(cfg)
	if e != nil {
		t.Fatal("供应商验证服务初始化失败")
	}
	defer s.Close()
	s.provider = tripo.New(key)
	s.ready = true
	s.source = "real-tripo-controlled-agent"
	s.modelFactory = func(context.Context) (model.BaseChatModel, error) { return tripoLiveInputModel{}, nil }
	if e = s.Start(); e != nil {
		t.Fatal(s.redact(e.Error()))
	}
	ctx := context.Background()
	c, run, e := s.CreateAssetConversation(ctx, "live-input-fixture", "为产品展示生成一个木箱，静态GLB，最多20000三角面，10 MiB。", "first")
	if e != nil {
		t.Fatal(s.redact(e.Error()))
	}
	t.Log("真实Tripo受控验收证据：" + dir)
	defer func() {
		s.cancel()
		s.mu.Lock()
		for _, cancel := range s.active {
			cancel()
		}
		s.mu.Unlock()
		s.wg.Wait()
		snap, e := s.store.ConversationSnapshot(ctx, c.ID, "live-input-fixture", 0, 0, 100)
		if e != nil {
			t.Error("无法保存供应商验证证据")
			return
		}
		data, e := json.MarshalIndent(s.sanitize(map[string]any{"source": "real Tripo + deterministic Eino decisions; not Agent evaluation", "recorded": time.Now().UTC(), "passed": !t.Failed(), "snapshot": snap, "credits_note": "tripo_progress.credits为任务累计额度，同一TaskID不能重复求和；没有法币账单接口证据时不推算费用。"}), "", "  ")
		if e != nil {
			t.Error(e)
			return
		}
		if e = os.WriteFile(filepath.Join(dir, "evidence.json"), data, 0600); e != nil {
			t.Error(e)
		}
	}()
	await := func(run Session) Session {
		until := time.Now().Add(15 * time.Minute)
		last := ""
		for time.Now().Before(until) {
			v, e := s.store.Get(ctx, run.ID)
			if e != nil {
				t.Fatal(s.redact(e.Error()))
			}
			taskID := ""
			if v.Current != nil {
				taskID = v.Current.TaskID
			}
			state := v.Status + ":" + taskID
			if state != last {
				t.Log("执行 " + v.ID + " " + state)
				last = state
			}
			if v.Terminal() {
				if v.Status != "completed" || len(v.Artifacts) == 0 {
					t.Fatalf("真实供应商本步未通过：%s", v.Final)
				}
				waitConversationIdle(t, s, c.ID)
				return v
			}
			time.Sleep(time.Second)
		}
		_ = s.Stop(ctx, run.ID)
		t.Fatal("真实供应商步骤超过15分钟，保留真实失败证据")
		return Session{}
	}
	first := await(run)
	v1 := first.Artifacts[0].ID
	if first.Artifacts[0].Report.Triangles <= 3000 {
		t.Fatal("生成实测面数不足以完成预定3000→1500加工链，不能伪造减面目标")
	}
	var v2 string
	for i, text := range []string{"明确引用版本减面至3000三角面", "明确引用版本减面至1500三角面", "回到原始版本减面至2500三角面"} {
		input := v1
		if i == 1 {
			input = v2
		}
		run, e = s.ContinueConversation(ctx, c.ID, "live-input-fixture", text, fmt.Sprintf("edit-%d", i), input)
		if e != nil {
			t.Fatal(s.redact(e.Error()))
		}
		done := await(run)
		if i == 0 {
			v2 = done.SelectedArtifact
		}
		version, e := s.store.GetAssetVersion(ctx, c.ID, done.Artifacts[0].ID)
		if e != nil || version.ParentVersionID != input {
			t.Fatal("真实加工父关系不匹配")
		}
		if done.Production < 1 || done.Production > 3 || done.Current.PreparedInput == nil || done.Current.PreparedInput.Token == "" || done.Current.SubmissionParams == nil {
			t.Fatal("真实上传/提交证据不完整")
		}
	}
}

type tripoLiveInputModel struct{}

func (m tripoLiveInputModel) Generate(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	var state struct {
		Request    string        `json:"request"`
		Intent     *Intent       `json:"intent"`
		Input      *AssetVersion `json:"input_version"`
		Artifacts  []Artifact    `json:"artifacts"`
		Current    *Operation    `json:"current_operation"`
		Production int           `json:"production"`
	}
	for _, msg := range in {
		if i := strings.LastIndex(msg.Content, "<runtime_state>"); i >= 0 {
			if e := json.Unmarshal([]byte(strings.Split(msg.Content[i+15:], "</runtime_state>")[0]), &state); e != nil {
				return nil, e
			}
		}
	}
	call := func(name string, args any) (*schema.Message, error) {
		return protocolProposal(name, newID(), args), nil
	}
	if state.Intent == nil {
		target, action := 20000, "generate"
		if state.Input != nil {
			action = "decimate"
			switch {
			case strings.Contains(state.Request, "1500"):
				target = 1500
			case strings.Contains(state.Request, "2500"):
				target = 2500
			default:
				target = 3000
			}
		}
		return call("set_intent", conversationIntentInput{Action: action, Intent: Intent{Asset: "木箱", Use: "产品展示", MaxTriangles: target, MaxBytes: 10 << 20, Plan: []string{"按明确输入制作并技术检查"}}})
	}
	if len(state.Artifacts) > 0 {
		a := state.Artifacts[len(state.Artifacts)-1]
		if a.Report.Valid && !a.Report.Passed && state.Production < 3 {
			// 供应商目标是近似值，实际超限时按原上限继续合法纠偏，不放宽验收。
			target := state.Intent.MaxTriangles * 3 / 4
			if target >= 500 && target < a.Report.Triangles {
				return call("decimate_asset", decimationInput{ArtifactID: a.ID, TargetTriangles: target, Reason: "实测面数超过原上限，使用本Run真实候选继续减面"})
			}
		}
		return call("finish_request", conversationFinishInput{Outcome: testConversationOutcome(a.Report.Passed), ArtifactID: a.ID, Explanation: "只依据实际GLB技术检查，不认证外观。"})
	}
	if state.Current != nil && state.Current.Stage == "done" && state.Current.Error != "" {
		return call("finish_request", conversationFinishInput{Outcome: "ended", Explanation: "真实供应商操作失败，保留原始事实。"})
	}
	if state.Input != nil {
		return call("decimate_asset", decimationInput{ArtifactID: state.Input.ID, TargetTriangles: state.Intent.MaxTriangles, Reason: "按指定版本降低面数"})
	}
	return call("generate_asset", generationInput{Prompt: "A single low-poly wooden crate for product presentation, static mesh, no skeleton, no animation", TargetTriangles: 6000, Reason: "验证实际Tripo生成及后续历史文件加工协议"})
}
func (m tripoLiveInputModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, e := m.Generate(ctx, in, opts...)
	if e != nil {
		return nil, e
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}
