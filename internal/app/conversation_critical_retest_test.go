package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestConversationAgentCriticalCaseRetest 默认只复测第三批缺文件因果错误一次。
// 原 product 视觉错误专项已移除；所有新诊断使用非视觉评分，不能与旧批次混用。
// missing-output-final / missing-reference-final 分别对缺文件或未选版本场景独立运行三次，必须显式选择。
// 使用原场景/评分函数，独立保存局部诊断，不能生成84例整批通过结论。
func TestConversationAgentCriticalCaseRetest(t *testing.T) {
	if os.Getenv("RUN_CONVERSATION_CRITICAL_RETEST") != "1" {
		t.Skip("opt-in targeted real DeepSeek retest; no real Tripo")
	}
	mode := "missing-output"
	runsPerTarget, expectedTargets := 1, 1
	rules := []string{"只复测缺文件因果场景一次，已移除视觉错误专项", "编号沿用原记录方便对照，不表示已重复运行三次", "保留全部原始提议，按manifest中的非视觉评分范围审核，Runtime纠正不抵消非视觉错误", "本次局部诊断不能替代84例完整验收或改写旧分数"}
	switch os.Getenv("CONVERSATION_CRITICAL_RETEST_SCOPE") {
	case "":
	case "missing-output-final":
		mode = "missing-output-final"
		runsPerTarget, expectedTargets = 3, 1
		rules = []string{"仅复测第三批原始missing_output场景，独立运行三次，不追加其他案例", "repeat 1/2/3仅表示本批重复序号；original_repeat=3和original_file保留第三批失败记录来源", "保留全部原始提议，按manifest中的非视觉评分范围审核，Runtime纠正不抵消非视觉错误", "仅评估本次三个样本，不能替代84例完整验收或改写旧分数"}
	case "missing-reference-final":
		mode = "missing-reference-final"
		runsPerTarget, expectedTargets = 3, 1
		rules = []string{"仅复测第三批原始missing_reference场景，独立运行三次，不追加其他案例", "repeat 1/2/3仅表示本批重复序号；original_repeat=1和original_file保留第三批原始场景来源", "保留全部原始提议，按manifest中的非视觉评分范围审核，Runtime纠正不抵消非视觉错误", "仅评估本次三个样本，不能替代84例完整验收或改写旧分数"}
	default:
		t.Fatal("未知局部复测范围，停止执行")
	}
	base := os.Getenv("CONVERSATION_CRITICAL_RETEST_EVIDENCE_DIR")
	if !filepath.IsAbs(base) {
		t.Fatal("必须指定局部复测独立证据目录")
	}
	key, err := audienceDeepSeekKey(filepath.Join("..", "..", ".env"))
	if err != nil || key == "" {
		t.Fatal("缺少DeepSeek凭证；复测未执行")
	}
	if err = os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, time.Now().UTC().Format("20060102T150405Z")+"-")
	if err != nil {
		t.Fatal(err)
	}
	type target struct {
		Case           conversationEvalCase `json:"case"`
		OriginalRepeat int                  `json:"original_repeat"`
		OriginalFile   string               `json:"original_file"`
	}
	targets := []target{}
	for _, tc := range conversationEvaluationCasesForProfile("current-v3") {
		repeat := 0
		switch tc.ID {
		case "missing_output":
			if mode != "missing-reference-final" {
				repeat = 3
			}
		case "missing_reference":
			if mode == "missing-reference-final" {
				repeat = 1
			}
		}
		if repeat != 0 {
			name := tc.Suite + "-" + tc.ID + "-" + strconv.Itoa(repeat) + ".json"
			targets = append(targets, target{tc, repeat, "conversation-evidence/20260913T153145Z-3276251327/" + name})
		}
	}
	if len(targets) != expectedTargets {
		t.Fatal("局部复测场景数量与所选模式不一致")
	}
	for _, target := range targets {
		original, err := os.ReadFile(filepath.Join("..", "..", "docs", "evaluations", target.OriginalFile))
		if err != nil {
			t.Fatal("缺少原失败记录，不能核对复测场景")
		}
		var record struct {
			Case conversationEvalCase `json:"case"`
		}
		if json.Unmarshal(original, &record) != nil || !conversationRetestMatchesScenario(record.Case, target.Case) {
			t.Fatal("复测场景与原失败记录不一致，停止执行")
		}
	}
	profile, err := profileForVersion(ConversationPromptVersion)
	if err != nil {
		t.Fatal(err)
	}
	skills := map[string]string{}
	for _, name := range []string{"intent", "generation", "correction", "asset-editing"} {
		content, err := profile.skillContent(name)
		if err != nil {
			t.Fatal(err)
		}
		skills[name] = tokenHash(content)
	}
	manifest := map[string]any{
		"created": time.Now().UTC(), "evaluation_profile": "current-v3-critical-targeted", "scope": "targeted_diagnostic_only_not_full_acceptance",
		"evaluation_rubric": conversationEvaluationRubric, "excluded_semantic_checks": conversationEvaluationExcludedChecks(),
		"scenario_comparison": "输入、用户答复、Provider反馈、允许动作和自动预期均与原场景完全相同；仅ExplanationReview采用本次明确修订的非视觉评分，不继承旧语义分数",
		"mode":                mode,
		"model":               "deepseek-v4-pro", "thinking": "high", "max_completion_tokens": 8192,
		"targets": targets, "runs_per_target": runsPerTarget, "total_runs": len(targets) * runsPerTarget, "max_model_calls_total": len(targets) * runsPerTarget * 20,
		"instruction_hash": tokenHash(profile.Instruction), "skill_hashes": skills,
		"provider":                 "fixed local Provider; synthetic GLB; no real Tripo",
		"rules":                    rules,
		"response_metadata_source": "agent_proposal_events",
		"semantic_criteria":        []string{conversationExplanationReview, "未诊断原因或参数排除作为确定事实属于关键错误", "无依据的面数或文件体积保证仍计解释失败", "局部诊断通过要求自动与语义均通过且关键错误为零，不改变全量验收状态"},
	}
	writeConversationEvalJSON(t, filepath.Join(dir, "manifest.json"), manifest)
	t.Logf("TARGETED_EVIDENCE_DIR=%s", dir)
	for _, target := range targets {
		t.Run(target.Case.ID, func(t *testing.T) {
			if mode == "missing-output-final" || mode == "missing-reference-final" {
				for repeat := 1; repeat <= runsPerTarget; repeat++ {
					t.Run(strconv.Itoa(repeat), func(t *testing.T) {
						runConversationEvaluation(t, key, dir, target.Case, repeat)
					})
				}
				return
			}
			runConversationEvaluation(t, key, dir, target.Case, target.OriginalRepeat)
		})
	}
}

// 用户只调整了解释评分范围；场景输入、技术阈值和操作预期仍必须逐字段一致。
func conversationRetestMatchesScenario(original, current conversationEvalCase) bool {
	original.ExplanationReview = current.ExplanationReview
	return jsonString(original) == jsonString(current)
}

// 更换解释评分不能顺带修改旧场景的硬约束或供应商反馈；离线验证，不请求模型。
func TestConversationRetestKeepsScenario(t *testing.T) {
	for _, current := range conversationEvaluationCasesForProfile("current-v3") {
		originalFile := ""
		switch current.ID {
		case "missing_output":
			originalFile = "baseline-current-v3-missing_output-3.json"
		case "missing_reference":
			originalFile = "conversation-v3-missing_reference-1.json"
		default:
			continue
		}
		t.Run(current.ID, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", "docs", "evaluations", "conversation-evidence", "20260913T153145Z-3276251327", originalFile))
			if err != nil {
				t.Fatal(err)
			}
			var record struct {
				Case conversationEvalCase `json:"case"`
			}
			if err := json.Unmarshal(data, &record); err != nil {
				t.Fatal(err)
			}
			if !conversationRetestMatchesScenario(record.Case, current) {
				t.Fatal("new rubric changed the original execution scenario")
			}
			changed := current
			changed.MaxTriangles++
			if conversationRetestMatchesScenario(record.Case, changed) {
				t.Fatal("rubric revision silently changed the triangle limit")
			}
			changed = current
			changed.Provider = "different-feedback"
			if conversationRetestMatchesScenario(record.Case, changed) {
				t.Fatal("rubric revision silently changed provider feedback")
			}
		})
	}
}
