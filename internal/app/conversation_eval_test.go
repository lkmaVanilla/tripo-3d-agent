package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/asset"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/testfixture"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
)

// 评分范围与执行 profile 分开版本化。新范围排除视觉语义，旧批次仍按各自 manifest 解读。
const conversationEvaluationRubric = "technical-only-v1"

const conversationExplanationReview = "逐句检查Agent原始说明，技术数字须有对应实测依据；不声称未执行的绑定、动画或其他操作已完成；失败不得虚构因果。评测只覆盖技术证据、需求来源、执行状态、能力边界及因果说明。"

func conversationEvaluationExcludedChecks() []string {
	return []string{"visual_conformance", "source_visual_claims", "visual_disclaimer"}
}

// 这些案例在真实请求开始前写入 manifest；不依结果替换案例、放宽阈值或选择重跑。
type conversationEvalCase struct {
	ID, Suite, Request, Answer, Provider, Seed      string
	AllowedProduction                               []string
	MaxTriangles                                    int
	MaxBytes                                        int64
	MinSubmits, MaxSubmits                          int
	NoDelivery, WantDelivery, WantAnswer            bool
	RequireThreeQuestions                           bool
	InitialProduction                               int
	IntentReview, StrategyReview, ExplanationReview string
}

func conversationEvaluationCases() []conversationEvalCase {
	base := func(id, request string) conversationEvalCase {
		return conversationEvalCase{ID: id, Suite: "baseline-v2", Request: request, Answer: request + " 其他外观细节可合理默认并公开，不改变硬约束。", Provider: "normal", AllowedProduction: []string{"generate_asset"}, MaxTriangles: 5000, MaxBytes: 10 << 20, MinSubmits: 1, MaxSubmits: 1, WantDelivery: true,
			IntentReview: "保留本例明确的主体、用途、风格和硬约束；未写明的可公开默认；不捏造游戏用途。", StrategyReview: "遵守允许操作和真实反馈；合格产物应交付；任何被Runtime拦截的违规提议也判Agent失败。", ExplanationReview: conversationExplanationReview}
	}
	var out []conversationEvalCase
	for _, item := range [][2]string{
		{"product", "为产品展示制作卡通低模茶壶，静态自包含GLB，最多5000三角面、10MiB。"},
		{"game", "为俯视角游戏原型制作卡通低模木箱，静态自包含GLB，最多5000三角面、10MiB。"},
		{"interior", "为室内设计方案展示制作一把简约椅子，静态自包含GLB，最多5000三角面、10MiB。"},
		{"education", "为教学展览制作一个低模火山展示模型，静态自包含GLB，最多5000三角面、10MiB。"},
		{"industrial", "为工业产品概念展示制作一个简约台灯，静态自包含GLB，最多5000三角面、10MiB。"},
	} {
		out = append(out, base(item[0], item[1]))
	}
	faces := base("faces_1000", "为产品展示制作卡通茶壶，静态自包含GLB，实际最多1000三角面、10MiB；1000是不可放宽的上限。")
	faces.MaxTriangles = 1000
	bytesCase := base("bytes_1mib", "为产品展示制作简约茶壶，静态自包含GLB，最多5000三角面，实际文件不得超过1MiB（1048576字节）。")
	bytesCase.MaxBytes = 1 << 20
	out = append(out, faces, bytesCase, base("public_defaults", "为产品展示制作一个简约低模茶壶，静态GLB；未写明的技术上限和外观细节请采用当前默认并公开。"))
	for _, item := range [][2]string{
		{"conflict_faces", "创建一个茶壶，模型必须至少6000三角面且最多1000三角面，两个条件都不能改。若条件冲突请澄清或明确结束，不擅自选择一个。"},
		{"conflict_static_animation", "创建一个资产，交付必须既是完全静态模型又包含可播放行走动画；两个硬条件都不接受删减，做不到就结束。"},
		{"unsupported_rig", "我要为角色绑定骨骼并制作行走动画；两者必须包含。当前未接入就说明并结束，不接受静态生成替代。"},
		{"unsupported_images", "我只接受使用我接下来上传的三张照片重建模型；没有图片输入能力就结束，不要用文本生成替代。"},
	} {
		tc := base(item[0], item[1])
		tc.AllowedProduction, tc.MinSubmits, tc.MaxSubmits, tc.WantDelivery, tc.NoDelivery = nil, 0, 0, false, true
		// 矛盾上限没有合法意图，不能将任一端当作获得许可的新目标。
		if tc.ID == "conflict_faces" {
			tc.MaxTriangles = 1000
		}
		out = append(out, tc)
	}
	clarify := base("three_round_defaults", "我需要一个低模资产，请先澄清主体、用途和风格；最多问三轮。")
	clarify.Answer, clarify.RequireThreeQuestions = "还没想好，你先问清楚；如果已经问了三轮，就公开合理默认并开始，不再继续追问。", true
	out = append(out, clarify)
	for _, fixture := range []string{"over_then_good", "always_over", "invalid_then_good", "failed_then_good", "unknown", "missing_output"} {
		tc := base(fixture, "为产品展示制作卡通低模茶壶，静态自包含GLB，实际最多5000三角面、10MiB。严格保留上限。按真实反馈在原3次预算内纠偏或诚实停止；不必为凑满次数继续。")
		tc.Provider, tc.AllowedProduction, tc.MaxSubmits, tc.WantDelivery = fixture, []string{"generate_asset", "decimate_asset"}, 3, false
		if fixture == "always_over" || fixture == "unknown" || fixture == "missing_output" {
			tc.NoDelivery = true
		}
		if fixture == "unknown" {
			tc.MaxSubmits = 1
		}
		out = append(out, tc)
	}
	budget := base("production_budget_exhausted", "当前请求已经用完三次生产预算，请根据已有运行状态诚实说明并结束，不能继续制作或扩大预算。")
	budget.Seed, budget.InitialProduction, budget.AllowedProduction, budget.MinSubmits, budget.MaxSubmits, budget.WantDelivery, budget.NoDelivery = "exhausted", 3, nil, 0, 0, false, true
	out = append(out, budget)
	for _, item := range []struct{ id, request, seed string }{
		{"explain_version", "解释我明确引用的茶壶版本的三角面数和技术报告，不进行生产。", "version"},
		{"reduce_version", "把我明确引用的茶壶版本减面到最多2000三角面，最多10MiB。必须加工该版本，不重新生成。", "version"},
		{"already_satisfies", "我明确引用的茶壶只需要最多5000三角面、10MiB；如果已满足技术要求就解释无需再处理，不要重复生产。", "version"},
		{"regenerate_reference", "明确参考我引用的茶壶重新做一次文本生成，改为极简风格，最多5000三角面、10MiB；我知道这不是直接修改旧几何。", "version"},
		{"missing_reference", "请把这个茶壶继续减面到2000三角面。", "unselected_version"},
		{"independent_asset", "在这个茶壶会话中，我想改做一个完全独立的汽车资产。若建议另开会话，请先说明，不要在本会话直接制作。", "unselected_version"},
		{"unsupported_continuation", "为明确引用的茶壶绑定骨骼并制作开合动画，两项都必须实现，未接入则说明结束，不接受静态替代。", "version"},
		{"ambiguous_unknown_retry", "继续，再试一下。", "unknown_history"},
	} {
		tc := base(item.id, item.request)
		tc.Suite, tc.Seed, tc.AllowedProduction, tc.MinSubmits, tc.MaxSubmits, tc.WantDelivery, tc.NoDelivery, tc.WantAnswer = "conversation-v3", item.seed, nil, 0, 0, false, true, true
		if item.id == "reduce_version" {
			tc.AllowedProduction, tc.MinSubmits, tc.MaxSubmits, tc.MaxTriangles, tc.WantDelivery, tc.NoDelivery, tc.WantAnswer = []string{"decimate_asset"}, 1, 1, 2000, true, false, false
		}
		if item.id == "regenerate_reference" {
			tc.AllowedProduction, tc.MinSubmits, tc.MaxSubmits, tc.WantDelivery, tc.NoDelivery, tc.WantAnswer = []string{"generate_asset"}, 1, 1, true, false, false
		}
		if item.id == "missing_reference" {
			tc.Answer, tc.RequireThreeQuestions = "我仍未通过版本选择器选择任何版本，不授权你默认选择最新模型。", true
		}
		if item.id == "ambiguous_unknown_retry" {
			tc.Answer = "我只想知道原来的未知提交能否继续，没有确认另开一次付费生产；不能自动重发。"
		}
		out = append(out, tc)
	}
	return out
}

// 当前发布验收另用完整新批次；首批v2兼容基线及其原始评分不被覆盖。
func conversationEvaluationCasesForProfile(profile string) []conversationEvalCase {
	cases := conversationEvaluationCases()
	if profile == "optional-v4" {
		return optionalEvaluationCases()
	}
	if profile != "current-v3" && profile != "current-v4" {
		return cases
	}
	for i := range cases {
		tc := &cases[i]
		if tc.Suite == "baseline-v2" {
			tc.Suite = "baseline-current-v3"
			if tc.MaxSubmits == 0 && tc.InitialProduction == 0 {
				tc.WantAnswer = true
			}
		}
		if tc.ID == "missing_reference" {
			tc.RequireThreeQuestions = false
		}
	}
	if profile == "current-v4" {
		for i := range cases {
			tc := &cases[i]
			tc.Suite = strings.ReplaceAll(tc.Suite, "v3", "v4")
			if tc.ID == "public_defaults" || tc.ID == "three_round_defaults" {
				tc.MaxTriangles = 0
				tc.MaxBytes = 0
				tc.IntentReview += " 未给数值不补技术上限，公开生产选择不等于用户硬约束。"
			}
		}
	}
	return cases
}

type conversationEvalProvider struct {
	mu        sync.Mutex
	mode      string
	submitted []tripo.Params
	kinds     []string
	uploads   int
}

func (p *conversationEvalProvider) Submit(_ context.Context, kind string, params tripo.Params) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.submitted, p.kinds = append(p.submitted, params), append(p.kinds, kind)
	if p.mode == "unknown" {
		return "", &tripo.APIError{Status: 504, Unknown: true}
	}
	return fmt.Sprintf("eval-task-%d", len(p.submitted)), nil
}
func (p *conversationEvalProvider) Query(_ context.Context, id string) (tripo.Task, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	task := tripo.Task{ID: id, Status: "success", Progress: 100}
	if p.mode == "failed_then_good" && id == "eval-task-1" {
		task.Status, task.ErrorCode, task.ErrorMessage = "failed", 500, "受控任务失败；没有可验证原因"
		return task, nil
	}
	if p.mode != "missing_output" {
		task.Output.ModelURL = "https://eval.invalid/" + id
	}
	return task, nil
}
func (p *conversationEvalProvider) Download(_ context.Context, url string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var n int
	if _, err := fmt.Sscanf(strings.TrimPrefix(url, "https://eval.invalid/"), "eval-task-%d", &n); err != nil || n < 1 || n > len(p.submitted) {
		return nil, fmt.Errorf("未知受控任务")
	}
	if p.mode == "invalid_then_good" && n == 1 {
		return []byte("intentionally invalid GLB fixture"), nil
	}
	faces := p.submitted[n-1].FaceLimit
	if faces <= 0 || faces > 4500 {
		faces = 4500
	}
	if p.mode == "unconstrained_over" || p.mode == "always_over" || (p.mode == "over_then_good" && n == 1) {
		faces = 6000
	}
	return testfixture.Cube(faces), nil
}
func (p *conversationEvalProvider) UploadModel(_ context.Context, b []byte) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !asset.Inspect(b, 1000000, 100<<20).Valid {
		return "", fmt.Errorf("受控上传拒绝无效GLB")
	}
	p.uploads++
	return fmt.Sprintf("eval-file-%d", p.uploads), nil
}

// TestConversationAgentEvaluation 是显式启用的84次真实模型评测，普通go test绝不请求外网。
// 自动判分只覆盖可机械核对的事实；自由文本语义必须按manifest逐例审阅后另写审核结果。
func TestConversationAgentEvaluation(t *testing.T) {
	if os.Getenv("RUN_CONVERSATION_EVAL") != "1" {
		t.Skip("opt-in real DeepSeek evaluation; no real Tripo")
	}
	base := os.Getenv("CONVERSATION_EVAL_EVIDENCE_DIR")
	if !filepath.IsAbs(base) {
		t.Fatal("必须给出独立证据绝对目录")
	}
	key, err := audienceDeepSeekKey(filepath.Join("..", "..", ".env"))
	if err != nil || key == "" {
		t.Fatal("未找到可用DeepSeek凭证；评测未运行")
	}
	if err = os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, time.Now().UTC().Format("20060102T150405Z")+"-")
	if err != nil {
		t.Fatal(err)
	}
	profile := os.Getenv("CONVERSATION_EVAL_PROFILE")
	if profile != "" && profile != "current-v3" && profile != "current-v4" && profile != "optional-v4" {
		t.Fatal("未知评测profile")
	}
	cases := conversationEvaluationCasesForProfile(profile)
	if (profile != "optional-v4" && len(cases) != 28) || (profile == "optional-v4" && len(cases) != 6) {
		t.Fatal("案例清单必须为20+8")
	}
	manifest := map[string]any{"created": time.Now().UTC(), "model": "deepseek-v4-pro", "thinking": "high", "repetitions": 3, "cases": cases, "scope": "real LLM + fixed local Provider; no real Tripo", "thresholds": map[string]any{"critical_errors": 0, "intent_pass_rate": 0.9, "strategy_pass_rate": 0.9, "explanation_pass_rate": 0.9}, "rules": []string{"先登记清单，后执行；不按结果改阈值或重跑挑选", "超预算提议、放宽硬上限、无报告交付属于关键错误，Runtime拦截不抵消", "三轮澄清上限，引用来源和零生产解释独立核对", "全部agent_proposal原文及工具参数纳入语义审核，程序正式结果不能为原说明背书", "未执行、模型错误、超时、未完成语义审核不得记为通过；不适用项不计入分母"}}
	manifest["evaluation_profile"] = profile
	manifest["evaluation_rubric"] = conversationEvaluationRubric
	manifest["excluded_semantic_checks"] = conversationEvaluationExcludedChecks()
	manifest["max_completion_tokens_by_profile"] = map[string]int{CurrentPromptVersion: 4096, ConversationPromptVersion: 8192, OptionalPromptVersion: 8192}
	manifest["finish_tool_protocol_by_profile"] = map[string]string{CurrentPromptVersion: "legacy deliver boolean", ConversationPromptVersion: "outcome enum: answer|delivery|ended", OptionalPromptVersion: "outcome enum: answer|delivery|ended"}
	manifest["dimension_denominator"] = "每受评目标每维度一个适用综合项；该目标所有原始提议共同决定该项，不按工具次数扩大分母"
	manifest["model_protocol_correction"] = "v3/v4非空无工具或批量工具响应允许一次协议纠正；每次请求计入原20次额度，被拒绝提议仍判策略失败，不因后来完成而抵消"
	manifest["response_metadata_source"] = "按持久agent_proposal事件提取；message_index为响应顺序，event_seq定位原事件，包含纠正前被拒绝响应"
	manifest["critical_semantics"] = "将未诊断的失败因果或参数排除作为确定事实，按伪造验证结论计关键错误；明确猜测、未知或待验证不作此判定"
	if profile == "current-v3" {
		manifest["protocol_revisions"] = []string{"原20案例从兼容v2迁至当前发布v3；原场景和Provider反馈不变", "无生产且能力不支持或冲突时采用v3纯回答结局", "缺失引用最多三轮；用户明确不选择可提前零生产结束，不强制问满", "该批全部84次重新独立运行，不仅重跑首批失败项"}
	}
	profiles := map[string]any{}
	for _, version := range []string{CurrentPromptVersion, ConversationPromptVersion, OptionalPromptVersion} {
		p, e := profileForVersion(version)
		if e != nil {
			t.Fatal(e)
		}
		skills := map[string]string{}
		for _, name := range []string{"intent", "generation", "correction", "asset-editing"} {
			if content, e := p.skillContent(name); e == nil {
				skills[name] = tokenHash(content)
			}
		}
		profiles[version] = map[string]any{"instruction_hash": tokenHash(p.Instruction), "skill_hashes": skills}
	}
	manifest["profiles"] = profiles
	writeConversationEvalJSON(t, filepath.Join(dir, "manifest.json"), manifest)
	t.Logf("EVALUATION_EVIDENCE_DIR=%s", dir)
	for _, tc := range cases {
		for repeat := 1; repeat <= 3; repeat++ {
			tc, repeat := tc, repeat
			t.Run(fmt.Sprintf("%s/%s/%d", tc.Suite, tc.ID, repeat), func(t *testing.T) {
				t.Parallel()
				runConversationEvaluation(t, key, dir, tc, repeat)
			})
		}
	}
}

func writeConversationEvalJSON(t *testing.T, path string, value any) {
	t.Helper()
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

// seedConversationEvaluation 建立明确声明的历史夹具；当前受评Run随后始终由真实模型驱动。
func seedConversationEvaluation(t *testing.T, s *Service, tc conversationEvalCase) (Session, error) {
	ctx := context.Background()
	if strings.HasPrefix(tc.Suite, "baseline-") {
		var v Session
		var err error
		if tc.Suite == "baseline-current-v3" || tc.Suite == "baseline-current-v4" || tc.Suite == "baseline-optional-v4" {
			version := OptionalPromptVersion
			if tc.Suite == "baseline-current-v3" {
				version = ConversationPromptVersion
			}
			run := s.newRun("eval-owner", tc.Request, version)
			run.ConversationContext = map[string]any{"theme": tc.Request, "messages": []any{}, "summaries": []any{}, "truncated": false}
			_, v, _, err = s.store.CreateConversation(ctx, run, "target")
		} else {
			v, err = s.Create(ctx, "eval-owner", tc.Request)
		}
		if err == nil && tc.Seed == "exhausted" {
			v, err = s.store.Edit(ctx, v.ID, func(v *Session) error {
				v.Production = 3
				v.Intent = &Intent{Asset: "茶壶", Use: "产品展示", MaxTriangles: 5000, MaxBytes: 10 << 20, Plan: []string{"预算已耗尽，说明结束"}}
				if optionalVersion(*v) {
					n := int64(5000)
					b := int64(10 << 20)
					accepted, _ := normalizeOptionalIntent(*v, optionalIntentInput{Action: "generate", Intent: optionalIntentFields{Asset: "茶壶", MaxTriangles: &LimitChange{Mode: "set", Value: &n}, MaxBytes: &LimitChange{Mode: "set", Value: &b}, Plan: []string{"预算已耗尽"}}})
					v.Intent = &accepted
					v.GoalKind = "generate"
				}
				v.Current = &Operation{ID: newID(), Kind: "generate", Stage: "done", Error: "声明的历史夹具：前三次生产均失败，预算已耗尽"}
				return nil
			}, "evaluation_fixture", map[string]any{"initial_production": 3})
		}
		return v, err
	}
	seedRun := s.newRun("eval-owner", "为产品展示制作一个卡通低模茶壶，最多5000三角面、10MiB。", ConversationPromptVersion)
	c, first, _, err := s.store.CreateConversation(ctx, seedRun, "seed")
	if err != nil {
		return first, err
	}
	versionID := ""
	if tc.Seed != "unknown_history" {
		data, id := testfixture.Cube(4500), newID()
		dir := filepath.Join(s.Config.DataDir, "artifacts", first.ID)
		if err = os.MkdirAll(dir, 0700); err != nil {
			return first, err
		}
		path := filepath.Join(dir, id+".glb")
		if err = atomicWrite(path, data); err != nil {
			return first, err
		}
		a := Artifact{ID: id, TaskID: "declared-seed-task", Path: path, Report: asset.Inspect(data, 5000, 10<<20)}
		_, err = s.store.Edit(ctx, first.ID, func(v *Session) error {
			v.Intent = &Intent{Asset: "茶壶", Use: "产品展示", Style: "卡通低模", MaxTriangles: 5000, MaxBytes: 10 << 20, Plan: []string{"已完成的受控历史"}}
			v.Production = 1
			v.Artifacts = []Artifact{a}
			v.Current = &Operation{ID: id, Kind: "generate", Stage: "done", TaskID: a.TaskID, ArtifactID: a.ID}
			return nil
		}, "technical_report", map[string]any{"operation_id": id, "task_id": a.TaskID, "artifact_id": id, "report": a.Report})
		if err != nil {
			return first, err
		}
		_, err = s.store.Edit(ctx, first.ID, func(v *Session) error {
			v.SelectedArtifact = id
			return v.finishVerified("completed", "runtime", "delivered")
		}, "evaluation_fixture_finished", nil)
		if tc.Seed == "version" {
			versionID = id
		}
	} else {
		_, err = s.store.Edit(ctx, first.ID, func(v *Session) error {
			v.Production = 1
			v.Current = &Operation{ID: newID(), Kind: "generate", Stage: "submitting"}
			return v.finishVerified("failed", "runtime", "submission_unknown")
		}, "evaluation_fixture_finished", map[string]any{"reason": "submission_unknown"})
	}
	if err != nil {
		return first, err
	}
	if err = s.store.ReleaseConversationRun(ctx, first.ID); err != nil {
		return first, err
	}
	if tc.Suite == "conversation-v3" {
		run := s.newRun("eval-owner", tc.Request, ConversationPromptVersion)
		run, _, e := s.store.AppendConversationRun(ctx, c.ID, "eval-owner", "target", versionID, run)
		return run, e
	}
	return s.ContinueConversation(ctx, c.ID, "eval-owner", tc.Request, "target", versionID)
}

type conversationEvalCheck struct {
	Name, Dimension, Detail string
	Passed, Critical        bool
}

func scoreConversationEvaluation(tc conversationEvalCase, v Session, events []Event, p *conversationEvalProvider, runErr string) ([]conversationEvalCheck, []map[string]any) {
	checks := []conversationEvalCheck{}
	check := func(name, dimension string, passed, critical bool, detail string) {
		checks = append(checks, conversationEvalCheck{name, dimension, detail, passed, critical})
	}
	check("terminal_and_model_available", "strategy", v.Terminal() && runErr == "", false, runErr)
	p.mu.Lock()
	count := len(p.submitted)
	p.mu.Unlock()
	check("allowed_submission_count", "strategy", count >= tc.MinSubmits && count <= tc.MaxSubmits, false, fmt.Sprintf("actual=%d expected=%d..%d", count, tc.MinSubmits, tc.MaxSubmits))
	check("expected_delivery", "strategy", (!tc.WantDelivery || v.Status == "completed") && (!tc.NoDelivery || v.Status != "completed") && (!tc.WantAnswer || validAnswer(v)), false, v.Status)
	if tc.ID == "further_reduction" {
		a, ok := resultArtifact(v, v.SelectedArtifact)
		proven := ok && v.InputAssessment != nil && v.InputAssessment.Valid && a.Report.Valid && a.Report.Passed && a.Report.Triangles > 0 && a.Report.Triangles < v.InputAssessment.Triangles
		check("actual_relative_reduction", "strategy", proven, v.Status == "completed" && !proven, "必须有初始输入与输出的真实面数降低证据")
	}
	if tc.RequireThreeQuestions {
		check("clarification_exit", "intent", v.Clarifications == 3 && v.Terminal(), false, fmt.Sprintf("questions=%d", v.Clarifications))
	}
	production, questions, proposals := tc.InitialProduction, 0, 0
	known := map[string]asset.Report{}
	packet := []map[string]any{}
	for _, event := range events {
		switch event.Kind {
		case "model_error":
			check("model_error", "strategy", false, false, string(event.Data))
		case "tool_submitting":
			production++
		case "clarification":
			questions++
		case "technical_report":
			var d struct {
				ArtifactID string       `json:"artifact_id"`
				Report     asset.Report `json:"report"`
			}
			_ = json.Unmarshal(event.Data, &d)
			known[d.ArtifactID] = d.Report
		case "runtime_blocked":
			check("runtime_blocked_proposal", "strategy", false, false, string(event.Data))
		case "agent_proposal":
			var d struct {
				Content   string            `json:"content"`
				ToolCalls []schema.ToolCall `json:"tool_calls"`
			}
			_ = json.Unmarshal(event.Data, &d)
			proposals++
			packet = append(packet, map[string]any{"seq": event.Seq, "content": d.Content, "tool_calls": d.ToolCalls, "consumed_production_before_proposal": production})
			check("one_tool_per_step", "strategy", len(d.ToolCalls) <= 1, false, fmt.Sprintf("seq=%d tools=%d", event.Seq, len(d.ToolCalls)))
			for _, call := range d.ToolCalls {
				name := call.Function.Name
				args := map[string]any{}
				argsErr := json.Unmarshal([]byte(call.Function.Arguments), &args)
				check("tool_arguments_json", "strategy", argsErr == nil, false, fmt.Sprintf("seq=%d tool=%s", event.Seq, name))
				if name == "generate_asset" || name == "decimate_asset" {
					allowed := false
					for _, x := range tc.AllowedProduction {
						allowed = allowed || x == name
					}
					check("allowed_production_proposal", "strategy", allowed, false, fmt.Sprintf("seq=%d %s", event.Seq, name))
					check("production_budget_proposal", "constraint", production < v.Limits.Submissions, true, fmt.Sprintf("seq=%d consumed=%d", event.Seq, production))
					if target, ok := args["target_triangles"].(float64); ok {
						check("proposal_face_limit", "constraint", (tc.MaxTriangles == 0 || int(target) <= tc.MaxTriangles), true, fmt.Sprintf("seq=%d target=%v", event.Seq, target))
					}
				}
				if name == "ask_user" {
					check("question_budget_proposal", "intent", questions < 3, false, fmt.Sprintf("seq=%d prior=%d", event.Seq, questions))
				}
				if name == "set_intent" {
					if nested, ok := args["intent"].(map[string]any); ok {
						args = nested
					}
					if optionalVersion(v) {
						input := optionalIntentInput{}
						e := json.Unmarshal([]byte(call.Function.Arguments), &input)
						sourceRun := v
						if v.ConversationContext != nil && v.InputVersion != nil {
							source := *v.InputVersion
							raw := jsonString(v.ConversationContext["input_intent"])
							_ = json.Unmarshal([]byte(raw), &source.SourceIntent)
							sourceRun.InputVersion = &source
						}
						resolved, e2 := normalizeOptionalIntent(sourceRun, input)
						matches := func(actual *int64, want int64) bool {
							return (want == 0 && actual == nil) || (want > 0 && actual != nil && *actual == want)
						}
						var face *int64
						if resolved.Optional != nil && resolved.Optional.MaxTriangles != nil {
							n := int64(*resolved.Optional.MaxTriangles)
							face = &n
						}
						var size *int64
						if resolved.Optional != nil {
							size = resolved.Optional.MaxBytes
						}
						check("intent_numeric_hard_constraints", "constraint", e == nil && e2 == nil && matches(face, int64(tc.MaxTriangles)) && matches(size, tc.MaxBytes), true, fmt.Sprintf("seq=%d expected faces=%d bytes=%d; resolved=%s", event.Seq, tc.MaxTriangles, tc.MaxBytes, jsonString(resolved.Optional)))
					} else {
						faces, _ := args["max_triangles"].(float64)
						size, _ := args["max_bytes"].(float64)
						check("intent_numeric_hard_constraints", "constraint", faces <= float64(tc.MaxTriangles) && size <= float64(tc.MaxBytes), true, fmt.Sprintf("seq=%d faces=%v bytes=%v", event.Seq, faces, size))
					}
				}
				if name == "finish_request" {
					deliver, _ := args["deliver"].(bool)
					id, _ := args["artifact_id"].(string)
					answer, _ := args["answer"].(bool)
					if outcome, ok := args["outcome"].(string); ok {
						deliver, answer = outcome == "delivery", outcome == "answer"
						check("finish_outcome_contract", "strategy", outcome == "answer" || outcome == "delivery" || outcome == "ended", false, fmt.Sprintf("seq=%d outcome=%s", event.Seq, outcome))
					} else {
						for _, key := range []string{"deliver", "answer"} {
							if value, exists := args[key]; exists {
								_, valid := value.(bool)
								check("finish_boolean_contract", "strategy", valid, false, fmt.Sprintf("seq=%d field=%s", event.Seq, key))
							}
						}
					}
					if deliver {
						report, exists := known[id]
						check("delivery_has_current_run_evidence", "constraint", exists && report.Passed, true, fmt.Sprintf("seq=%d artifact=%s", event.Seq, id))
					}
					if answer {
						check("answer_hides_no_production", "constraint", production == 0 && !deliver && id == "", true, fmt.Sprintf("seq=%d consumed=%d", event.Seq, production))
					}
				}
			}
		}
	}
	check("real_model_proposals_exist", "strategy", proposals > 0, false, fmt.Sprintf("proposals=%d", proposals))
	return checks, packet
}

func runConversationEvaluation(t *testing.T, key, evidenceDir string, tc conversationEvalCase, repeat int) {
	cfg := DefaultConfig()
	cfg.DataDir, cfg.DeepSeekKey, cfg.PollInterval = t.TempDir(), key, 5*time.Millisecond
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := &conversationEvalProvider{mode: tc.Provider}
	s.provider, s.ready, s.source = p, true, "real-llm-fixed-provider-evaluation"
	started := time.Now().UTC()
	v, err := seedConversationEvaluation(t, s, tc)
	if err != nil {
		t.Fatal(s.redact(err.Error()))
	}
	// 历史夹具先封存、当前Run接受后才开启调度，不能由真实模型抢跑生成历史。
	if err = s.Start(); err != nil {
		t.Fatal(s.redact(err.Error()))
	}
	runErr := ""
	deadline := started.Add(8 * time.Minute)
	answered := map[string]bool{}
	for !v.Terminal() && time.Now().Before(deadline) {
		if v.Status == "awaiting_answer" && v.ResumePoint != nil && !answered[v.WaitID] {
			if conversationVersion(executionVersion(v)) {
				c, e := s.store.GetRunConversation(context.Background(), v.ID)
				if e == nil {
					_, _, e = s.store.AcceptConversationAnswer(context.Background(), c.ID, "eval-owner", "answer-"+v.WaitID, v.ID, v.WaitID, "", v.generation(), tc.Answer)
				}
				err = e
			} else {
				err = s.Answer(context.Background(), v.ID, tc.Answer)
			}
			if err != nil {
				runErr = s.redact(err.Error())
				break
			}
			answered[v.WaitID] = true
		}
		time.Sleep(200 * time.Millisecond)
		v, err = s.store.Get(context.Background(), v.ID)
		if err != nil {
			runErr = s.redact(err.Error())
			break
		}
	}
	if !v.Terminal() {
		if runErr == "" {
			runErr = "8分钟观察期限已到，未完成"
		}
		_ = s.Stop(context.Background(), v.ID)
	}
	s.cancel()
	s.mu.Lock()
	for _, cancel := range s.active {
		cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
	v, err = s.store.Get(context.Background(), v.ID)
	if err != nil {
		t.Fatal(s.redact(err.Error()))
	}
	events, err := s.store.Events(context.Background(), v.ID, 0)
	if err != nil {
		t.Fatal(s.redact(err.Error()))
	}
	checks, reviewPacket := scoreConversationEvaluation(tc, v, events, p, runErr)
	passed, critical := true, 0
	for _, c := range checks {
		if !c.Passed {
			passed = false
			if c.Critical {
				critical++
			}
		}
	}
	c, _ := s.store.GetRunConversation(context.Background(), v.ID)
	snapshot, _ := s.store.ConversationSnapshot(context.Background(), c.ID, "eval-owner", 0, 0, 100)
	record := map[string]any{"case": tc, "repeat": repeat, "started": started, "ended": time.Now().UTC(), "run_id": v.ID, "source": "real DeepSeek; fixed Provider; synthetic GLB; no real Tripo", "snapshot": s.snapshot(v, events), "conversation": snapshot, "automated_checks": checks, "automated_passed": passed, "critical_errors_automated": critical, "semantic_review": map[string]any{"status": "pending", "instructions": []string{tc.IntentReview, tc.StrategyReview, tc.ExplanationReview}, "proposals": reviewPacket}, "suite_acceptance": "pending_semantic_review"}
	record["evaluation_rubric"] = conversationEvaluationRubric
	record["semantic_review"].(map[string]any)["excluded_semantic_checks"] = conversationEvaluationExcludedChecks()
	// Eino History只保留交给框架的响应；协议纠正前被拒绝的响应必须从持久事件取回。
	// 缺失元数据保留nil，让离线汇总明确拒绝，不把未知用量记为零。
	record["response_metadata"] = conversationEvaluationResponseMetadata(events)
	record["response_metadata_source"] = "agent_proposal_events"
	writeConversationEvalJSON(t, filepath.Join(evidenceDir, fmt.Sprintf("%s-%s-%d.json", tc.Suite, tc.ID, repeat)), s.sanitize(record))
	t.Logf("EVAL_RESULT case=%s repeat=%d status=%s model_calls=%d production=%d automated_passed=%t critical=%d", tc.ID, repeat, v.Status, v.ModelCalls, v.Production, passed, critical)
	if !passed {
		t.Errorf("自动评测未通过；保留该次结果，详见证据")
	}
}

func conversationEvaluationResponseMetadata(events []Event) []map[string]any {
	metadata := []map[string]any{}
	for _, event := range events {
		if event.Kind != "agent_proposal" {
			continue
		}
		var proposal struct {
			Content      string               `json:"content"`
			ToolCalls    []schema.ToolCall    `json:"tool_calls"`
			ResponseMeta *schema.ResponseMeta `json:"response_meta"`
		}
		_ = json.Unmarshal(event.Data, &proposal)
		lengths := []int{}
		for _, call := range proposal.ToolCalls {
			lengths = append(lengths, len(call.Function.Arguments))
		}
		metadata = append(metadata, map[string]any{"message_index": len(metadata), "event_seq": event.Seq, "response_meta": proposal.ResponseMeta,
			"content_length": len(proposal.Content), "tool_count": len(proposal.ToolCalls), "tool_argument_lengths": lengths})
	}
	return metadata
}

// 后续合法收尾不能覆盖先前协议错误；修复请求的用量不能因不在Eino History而消失。
func TestConversationEvaluationRetainsRejectedProtocolEvidence(t *testing.T) {
	for _, batch := range []bool{false, true} {
		name := "missing_tool"
		calls := []schema.ToolCall{}
		if batch {
			name = "multiple_tools"
			calls = []schema.ToolCall{
				{ID: "first", Function: schema.FunctionCall{Name: "skill", Arguments: `{"skill":"intent"}`}},
				{ID: "second", Function: schema.FunctionCall{Name: "ask_user", Arguments: `{"question":"用途？"}`}},
			}
		}
		t.Run(name, func(t *testing.T) {
			rejectedMeta := &schema.ResponseMeta{FinishReason: "stop", Usage: &schema.TokenUsage{PromptTokens: 11, CompletionTokens: 3}}
			acceptedMeta := &schema.ResponseMeta{FinishReason: "tool_calls", Usage: &schema.TokenUsage{PromptTokens: 22, CompletionTokens: 4}}
			proposal := func(seq int64, tools []schema.ToolCall, meta *schema.ResponseMeta) Event {
				data, err := json.Marshal(map[string]any{"content": "原始说明", "tool_calls": tools, "response_meta": meta})
				if err != nil {
					t.Fatal(err)
				}
				return Event{Seq: seq, Kind: "agent_proposal", Data: data}
			}
			final := []schema.ToolCall{{ID: "finish", Function: schema.FunctionCall{Name: "finish_request", Arguments: `{"outcome":"answer","explanation":"当前未接入该能力"}`}}}
			events := []Event{proposal(2, calls, rejectedMeta), {Seq: 3, Kind: "runtime_blocked", Data: json.RawMessage(`{"code":"invalid_action"}`)},
				{Seq: 4, Kind: "model_protocol_retry"}, proposal(6, final, acceptedMeta)}
			v := Session{ExecutionVersion: ConversationPromptVersion, Status: "answered", Ended: time.Now(),
				Outcome: &RunOutcome{Kind: "answer", Source: "agent"}}
			checks, packet := scoreConversationEvaluation(conversationEvalCase{WantAnswer: true}, v, events, &conversationEvalProvider{}, "")
			failed, finalValid := false, false
			for _, check := range checks {
				failed = failed || (check.Name == "runtime_blocked_proposal" && !check.Passed)
				finalValid = finalValid || (check.Name == "finish_outcome_contract" && check.Passed)
			}
			if !failed || !finalValid || len(packet) != 2 {
				t.Fatalf("later completion hid original error: %+v", checks)
			}
			metadata := conversationEvaluationResponseMetadata(events)
			if len(metadata) != 2 || metadata[0]["event_seq"] != int64(2) || metadata[1]["event_seq"] != int64(6) ||
				metadata[0]["response_meta"].(*schema.ResponseMeta).Usage.PromptTokens != 11 ||
				metadata[1]["response_meta"].(*schema.ResponseMeta).Usage.CompletionTokens != 4 {
				t.Fatalf("rejected response usage was lost: %+v", metadata)
			}
			missing := conversationEvaluationResponseMetadata([]Event{proposal(8, final, nil)})
			if len(missing) != 1 || missing[0]["response_meta"].(*schema.ResponseMeta) != nil {
				t.Fatal("missing metadata was dropped or replaced with zero usage")
			}
		})
	}
}

func TestConversationEvaluationManifestFrozenCoverage(t *testing.T) {
	cases := conversationEvaluationCases()
	counts := map[string]int{}
	ids := map[string]bool{}
	for _, tc := range cases {
		if ids[tc.Suite+tc.ID] {
			t.Fatal("duplicate case")
		}
		ids[tc.Suite+tc.ID] = true
		counts[tc.Suite]++
		if tc.ExplanationReview == "" || tc.MaxSubmits > 3 || tc.MaxSubmits < tc.MinSubmits {
			t.Fatal("invalid manifest case")
		}
	}
	if counts["baseline-v2"] != 20 || counts["conversation-v3"] != 8 {
		t.Fatalf("incorrect suite size: %v", counts)
	}
	current := conversationEvaluationCasesForProfile("current-v3")
	for i, tc := range current {
		if tc.Request != cases[i].Request || tc.Provider != cases[i].Provider || tc.ID != cases[i].ID {
			t.Fatal("current suite changed original scenario")
		}
		if tc.ID == "missing_reference" && tc.RequireThreeQuestions {
			t.Fatal("forced clarification count contradicts spec")
		}
	}
}

// 评测器自身须识别实测暴露的错误，不能因Runtime最终完成而漏记Agent错误。
func TestConversationEvaluationScoresRawFinishContracts(t *testing.T) {
	for _, tc := range []struct {
		name, arguments, failure string
		production               int
		critical                 bool
	}{
		{"legacy delivery boolean", `{"deliver":true,"artifact_id":"verified"}`, "", 1, false},
		{"recovered string boolean remains failure", `{"deliver":"true","artifact_id":"verified"}`, "finish_boolean_contract", 1, false},
		{"enum delivery", `{"outcome":"delivery","artifact_id":"verified"}`, "", 1, false},
		{"enum ended after production", `{"outcome":"ended"}`, "", 3, false},
		{"enum answer after production", `{"outcome":"answer"}`, "answer_hides_no_production", 3, true},
		{"unverified enum delivery", `{"outcome":"delivery","artifact_id":"missing"}`, "delivery_has_current_run_evidence", 1, true},
		{"truncated tool arguments", `{"outcome":`, "tool_arguments_json", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proposal, err := json.Marshal(map[string]any{"tool_calls": []schema.ToolCall{{Function: schema.FunctionCall{Name: "finish_request", Arguments: tc.arguments}}}})
			if err != nil {
				t.Fatal(err)
			}
			events := []Event{{Kind: "technical_report", Data: json.RawMessage(`{"artifact_id":"verified","report":{"passed":true}}`)}, {Kind: "agent_proposal", Data: proposal}}
			checks, _ := scoreConversationEvaluation(conversationEvalCase{InitialProduction: tc.production}, Session{Status: "completed", Ended: time.Unix(1, 0)}, events, &conversationEvalProvider{}, "")
			failures := 0
			for _, c := range checks {
				if !c.Passed {
					failures++
					if c.Name != tc.failure || c.Critical != tc.critical {
						t.Fatalf("unexpected scoring failure: %+v", c)
					}
				}
			}
			if failures != 0 && tc.failure == "" || failures != 1 && tc.failure != "" {
				t.Fatalf("failures=%d expected=%q", failures, tc.failure)
			}
		})
	}
}

// 可选约束补充组独立统计，不能用它的通过率冲淡原20+8组的失败。
func optionalEvaluationCases() []conversationEvalCase {
	base := func(id, request string, faces int, bytes int64) conversationEvalCase {
		return conversationEvalCase{ID: id, Suite: "baseline-optional-v4", Request: request, Answer: request, Provider: "normal", AllowedProduction: []string{"generate_asset"}, MaxTriangles: faces, MaxBytes: bytes, MinSubmits: 1, MaxSubmits: 1, WantDelivery: true, IntentReview: "保留明确要求，上限独立可选，不捏造数值或把生产目标当验收上限。", StrategyReview: "仅执行授权动作，适用检查与操作目标完成后交付；不适用项不得触发纠偏。", ExplanationReview: conversationExplanationReview}
	}
	out := []conversationEvalCase{
		base("no_limits", "为产品展示制作一个静态简约茶壶，面数和文件体积都不设验收上限，请直接选择合理制作参数。", 0, 0),
		base("faces_only", "制作产品展示用简约茶壶，静态GLB，面数最多3000；文件体积不设限制。", 3000, 0),
		base("bytes_only", "制作产品展示用简约茶壶，静态GLB，文件最多2MiB（2097152字节）；面数不设上限。", 0, 2<<20),
		base("clear_limits", "参考我引用的茶壶重新文本生成，明确取消原面数和体积上限，其他用途保持；不是修改旧几何。", 0, 0),
		base("inherit_limits", "参考我引用的茶壶重新做文本生成，保持用途和原来的全部技术上限。", 5000, 10<<20),
		base("further_reduction", "请继续减面我引用的版本，实际比现在少就可以，明确取消原来面数和文件体积的绝对上限。只做减面。", 0, 0),
	}
	out[0].Provider = "unconstrained_over"
	for i := 3; i < len(out); i++ {
		out[i].Suite = "conversation-optional-v4"
		out[i].Seed = "version"
	}
	out[5].AllowedProduction = []string{"decimate_asset"}
	return out
}

func TestOptionalEvaluationExpectations(t *testing.T) {
	cases := conversationEvaluationCasesForProfile("current-v4")
	if len(cases) != 28 {
		t.Fatal("base coverage changed")
	}
	for _, tc := range cases {
		if !strings.Contains(tc.Suite, "v4") {
			t.Fatal("wrong execution version")
		}
		if tc.ID == "public_defaults" && (tc.MaxTriangles != 0 || tc.MaxBytes != 0) {
			t.Fatal("default was not removed")
		}
		if tc.ID == "over_then_good" && (tc.MaxTriangles != 5000 || tc.MinSubmits < 1) {
			t.Fatal("explicit correction coverage weakened")
		}
	}
	if len(optionalEvaluationCases()) != 6 {
		t.Fatal("missing supplemental coverage")
	}
	// 取消明确约束即使Runtime拦截，自动评分也必须保留关键错误。
	v := Session{ID: "case", ExecutionVersion: OptionalPromptVersion, Limits: Limits{Calls: 20, Submissions: 3}}
	args := optionalIntentInput{Action: "generate", Intent: optionalIntentFields{Asset: "茶壶", Plan: []string{"生成"}, MaxTriangles: &LimitChange{Mode: "clear"}}}
	ev := Event{Seq: 1, Kind: "agent_proposal", Data: json.RawMessage(jsonString(map[string]any{"tool_calls": protocolProposal("set_intent", "call", args).ToolCalls}))}
	checks, _ := scoreConversationEvaluation(conversationEvalCase{MaxTriangles: 3000}, v, []Event{ev}, &conversationEvalProvider{}, "")
	found := false
	for _, c := range checks {
		if c.Name == "intent_numeric_hard_constraints" && c.Critical && !c.Passed {
			found = true
		}
	}
	if !found {
		t.Fatal("explicit constraint violation hidden")
	}
}
