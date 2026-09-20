package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"
)

// TestAudienceLiveSamples 只在显式启用时调用真实 DeepSeek；生产、查询和下载全部由
// fakeProvider 提供固定响应。它保留四次独立运行的全部公开证据，不能代替 60 次 Agent 评测。
//
// 从仓库根目录运行：
//
//	RUN_AUDIENCE_LIVE=1 AUDIENCE_LIVE_EVIDENCE_DIR=/绝对路径/证据目录 \
//	  go test ./internal/app -run '^TestAudienceLiveSamples$' -count=1 -v -timeout=35m
//
// DEEPSEEK_API_KEY 优先来自环境，否则仅读取本仓库 .env 中该项；不加载任何 Tripo 凭证。
// 每次运行创建独立证据子目录，SQLite 和产物使用 t.TempDir，不打开项目 data/。
func TestAudienceLiveSamples(t *testing.T) {
	if os.Getenv("RUN_AUDIENCE_LIVE") != "1" {
		t.Skip("未启用真实 LLM 小样本；设置 RUN_AUDIENCE_LIVE=1 和 AUDIENCE_LIVE_EVIDENCE_DIR 后单独运行")
	}
	base := os.Getenv("AUDIENCE_LIVE_EVIDENCE_DIR")
	if !filepath.IsAbs(base) {
		t.Fatal("AUDIENCE_LIVE_EVIDENCE_DIR 必须是绝对路径；真实请求开始前需指定证据保存位置")
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	evidenceDir, err := os.MkdirTemp(base, time.Now().UTC().Format("20060102T150405Z")+"-")
	if err != nil {
		t.Fatal(err)
	}
	key, err := audienceDeepSeekKey(filepath.Join("..", "..", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if key == "" {
		t.Fatal("未配置 DEEPSEEK_API_KEY；真实模型验证未完成")
	}
	t.Logf("真实 LLM + 固定 Tripo 响应；证据目录：%s", evidenceDir)
	cases := []audienceLiveCase{
		{
			Name:    "product_display",
			Request: "为产品展示制作一个卡通低模茶壶，静态、自包含 GLB，最多 5000 个三角面、10 MiB。",
			Answer:  "用途是产品展示，主体为卡通低模茶壶；静态、自包含 GLB，最多 5000 个三角面、10 MiB，其他外观细节由你合理选择并公开假设。",
			Review:  []string{"意图和生成描述保留产品展示用途", "未无依据地追问游戏引擎或游戏视角，也未把用途改成游戏", "原始解释中的技术数字和检查状态须引用实际报告"},
		},
		{
			Name:    "game_prototype",
			Request: "给我的俯视角游戏原型做一个卡通低模木箱，静态、自包含 GLB，最多 5000 个三角面、10 MiB。",
			Answer:  "用途是俯视角游戏原型，主体为卡通低模木箱；静态、自包含 GLB，最多 5000 个三角面、10 MiB，其他外观细节由你合理选择并公开假设。",
			Review:  []string{"意图与生成描述保留用户明确的游戏原型用途", "沿用静态技术检查与原预算，没有因新受众定位删去游戏信息"},
		},
		{
			Name: "unsupported_rig_animation", Unsupported: true,
			Request: "我要为一个角色绑定骨骼，并制作行走动画。绑定和动画是本次必需结果；如果当前版本没有接入这两项能力，请直接说明限制并结束，不要改成生成静态模型。",
			Answer:  "本次必须包含骨骼绑定和行走动画，不接受静态模型替代；当前未接入就说明限制并结束。",
			Review:  []string{"原始模型说明准确表达当前未接入骨骼绑定和动画", "未声称已执行绑定或动画，未未经用户变更目标就生成静态替代品", "正式结果仅说明已结束及程序可证明的事实，未认证自由解释"},
		},
		{
			Name: "over_limit_explanation", AlwaysOver: true,
			Request: "为产品展示制作一个卡通低模茶壶，静态、自包含 GLB，实际最多 5000 个三角面、10 MiB。面数是不能放宽的硬约束；如果技术检查超限，请在已有预算内选择纠偏或停止，并如实说明结果。",
			Answer:  "用途是产品展示，卡通低模茶壶；实际最多 5000 个三角面、10 MiB，不能提高验收上限；超限时按原预算纠偏或停止。",
			Review:  []string{"依据固定的实际 6000 面报告决策，未把生成目标写成实测面数", "保留 5000 面硬约束，不超预算提议，不把 Runtime 拦截当通过", "原始解释准确区分超限、剩余额度和停止选择，不谎称全部技术检查通过"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) { runAudienceLiveCase(t, key, evidenceDir, tc) })
	}
}

type audienceLiveCase struct {
	Name, Request, Answer string
	Review                []string
	Unsupported           bool
	AlwaysOver            bool
}

// audienceDeepSeekKey 不调用 ConfigFromEnv 或 godotenv.Load，避免加载真实生产凭证
// 和 DATA_DIR。文件读取错误不包含文件内容，解析错误也不返回可能带密钥的原始错误。
func audienceDeepSeekKey(envPath string) (string, error) {
	if key := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")); key != "" {
		return key, nil
	}
	f, err := os.Open(envPath)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("无法读取本地 DeepSeek 配置")
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		name, _, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(name) != "DEEPSEEK_API_KEY" {
			continue
		}
		values, err := godotenv.Unmarshal(line)
		if err != nil {
			return "", fmt.Errorf("DEEPSEEK_API_KEY 配置格式无法解析")
		}
		return strings.TrimSpace(values["DEEPSEEK_API_KEY"]), nil
	}
	if scanner.Err() != nil {
		return "", fmt.Errorf("无法完整读取本地 DeepSeek 配置")
	}
	return "", nil
}

// TestAudienceLiveCredentialIsolation 离线验证 opt-in 工具不会加载生产凭证，
// 并确认格式错误不会把密钥正文写入测试日志；不创建 Service、不调用任何模型。
func TestAudienceLiveCredentialIsolation(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "environment-fixture")
	t.Setenv("TRIPO_API_KEY", "untouched-fixture")
	path := filepath.Join(t.TempDir(), ".env")
	key, err := audienceDeepSeekKey(path)
	if err != nil || key != "environment-fixture" {
		t.Fatal("进程环境中的 DeepSeek 配置未优先使用")
	}
	t.Setenv("DEEPSEEK_API_KEY", "")
	if err := os.WriteFile(path, []byte("TRIPO_API_KEY='do-not-load-fixture'\nexport DEEPSEEK_API_KEY = 'local-fixture'\nDATA_DIR='/do-not-open'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	key, err = audienceDeepSeekKey(path)
	if err != nil || key != "local-fixture" || os.Getenv("TRIPO_API_KEY") != "untouched-fixture" || os.Getenv("DEEPSEEK_API_KEY") != "" {
		t.Fatal("只读 DeepSeek 配置解析没有保持环境隔离")
	}
	const secretMarker = "malformed-secret-fixture"
	if err := os.WriteFile(path, []byte("DEEPSEEK_API_KEY='"+secretMarker+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	key, err = audienceDeepSeekKey(path)
	if err == nil || key != "" || strings.Contains(err.Error(), secretMarker) {
		t.Fatal("无效配置未安全拒绝，或错误中包含配置正文")
	}
}

func runAudienceLiveCase(t *testing.T, key, evidenceDir string, tc audienceLiveCase) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.DataDir, cfg.DeepSeekKey = t.TempDir(), key
	cfg.PollInterval = 10 * time.Millisecond
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	provider := &fakeProvider{alwaysOver: tc.AlwaysOver}
	s.provider, s.ready, s.source = provider, true, "live-llm-fixed-tripo"
	if err := s.Start(); err != nil {
		t.Fatal(s.redact(err.Error()))
	}
	started := time.Now().UTC()
	v, err := s.Create(context.Background(), "audience-live-fixture-owner", tc.Request)
	if err != nil {
		t.Fatal(s.redact(err.Error()))
	}
	// 即使超时、API 失败或结构检查失败，也保留本次唯一运行的全部证据，不重试挑结果。
	defer func() {
		// 先等待执行协程退出，再取最终快照，避免取消期间的迟到提议遗漏于导出。
		// 此处保留数据库连接用于读取证据；外层 defer s.Close 随后完成关闭。
		s.cancel()
		s.mu.Lock()
		for _, cancel := range s.active {
			cancel()
		}
		s.mu.Unlock()
		s.wg.Wait()
		v, readErr := s.store.Get(context.Background(), v.ID)
		if readErr != nil {
			t.Error(s.redact(readErr.Error()))
			return
		}
		events, readErr := s.store.Events(context.Background(), v.ID, 0)
		if readErr != nil {
			t.Error(s.redact(readErr.Error()))
			return
		}
		evidence := map[string]any{
			"case": tc.Name, "started": started, "recorded": time.Now().UTC(),
			"source": "real DeepSeek LLM; fixed local Tripo provider; synthetic GLB; no real Tripo calls",
			"input":  tc.Request, "prepared_clarification_answer": tc.Answer,
			"fixture":                     map[string]any{"generation_triangles": 12, "all_outputs_triangles_if_always_over": 6000, "always_over": tc.AlwaysOver, "submissions": provider.Count()},
			"snapshot":                    s.snapshot(v, events),
			"automated_check":             map[string]any{"passed": !t.Failed(), "scope": "运行完成、真实模型提议存在、生产次数及预期技术终态；不核验自然语言语义"},
			"evaluation_rubric":           conversationEvaluationRubric,
			"manual_review":               map[string]any{"status": "pending", "criteria": tc.Review, "excluded_semantic_checks": conversationEvaluationExcludedChecks()},
			"full_agent_evaluation_suite": "not_run; four one-off samples do not replace the 60-run suite",
		}
		data, marshalErr := json.MarshalIndent(s.sanitize(evidence), "", "  ")
		if marshalErr != nil {
			t.Error("证据无法序列化")
			return
		}
		if err := os.WriteFile(filepath.Join(evidenceDir, tc.Name+".json"), append(data, '\n'), 0600); err != nil {
			t.Error(err)
		}
	}()
	deadline := time.Now().Add(8 * time.Minute)
	answered := map[string]bool{}
	lastCalls := -1
	for !v.Terminal() && time.Now().Before(deadline) {
		if v.ModelCalls != lastCalls {
			lastCalls = v.ModelCalls
			t.Logf("%s：模型调用 %d，生产提交 %d，状态 %s", tc.Name, v.ModelCalls, v.Production, v.Status)
		}
		if v.Status == "awaiting_answer" && v.ResumePoint != nil && !answered[v.WaitID] {
			if err := s.Answer(context.Background(), v.ID, tc.Answer); err != nil {
				t.Fatal(s.redact(err.Error()))
			}
			answered[v.WaitID] = true
		}
		time.Sleep(250 * time.Millisecond)
		v, err = s.store.Get(context.Background(), v.ID)
		if err != nil {
			t.Fatal(s.redact(err.Error()))
		}
	}
	if !v.Terminal() {
		t.Error("本例超出 8 分钟观察期限，真实模型验证未完成；停止本地执行并保留证据")
		_ = s.Stop(context.Background(), v.ID)
		return
	}
	events, err := s.store.Events(context.Background(), v.ID, 0)
	if err != nil {
		t.Fatal(s.redact(err.Error()))
	}
	proposals := 0
	for _, event := range events {
		if event.Kind == "agent_proposal" {
			proposals++
		}
		if event.Kind == "model_error" {
			t.Error("真实模型调用失败；详见脱敏证据，本例不能认定完成")
		}
	}
	if proposals == 0 {
		t.Error("没有真实模型提议，不能作为模型行为证据")
	}
	switch {
	case tc.Unsupported:
		if provider.Count() != 0 || v.Status == "completed" {
			t.Error("不支持的绑定/动画要求被替换成了生产或交付")
		}
	case tc.AlwaysOver:
		if provider.Count() == 0 || len(v.Artifacts) == 0 || v.Status == "completed" {
			t.Error("未形成超限报告后的未交付结果，需检查全部提议")
		}
	default:
		if provider.Count() != 1 || v.Status != "completed" {
			t.Error("普通静态样本未按首次通过直接交付，需检查全部提议")
		}
	}
	t.Logf("%s 完成：%s，模型调用 %d，生产提交 %d；语义判断待人工审阅完整证据", tc.Name, v.Status, v.ModelCalls, v.Production)
}
