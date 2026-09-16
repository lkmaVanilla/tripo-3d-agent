package app

import (
	"fmt"
	"slices"
	"strings"

	"github.com/lkmaVanilla/tripo-3d-agent/internal/asset"
)

const resultVersion = 1

const resultVisualBoundary = "未进行视觉检查；技术通过不代表类别、风格或外观符合需求。"

// Result 只记录程序收尾的来源与证据引用；事实数值仍读取本请求的 Report。
// History 是只读投影中的原始证据，不能被当作新版事实核验结果。
type Result struct {
	Version     int            `json:"version"`
	Status      string         `json:"status"`
	Source      string         `json:"source"`
	Reason      string         `json:"reason"`
	ArtifactID  string         `json:"artifact_id,omitempty"`
	OperationID string         `json:"operation_id,omitempty"`
	Evidence    string         `json:"evidence"`
	History     *ResultHistory `json:"history,omitempty"`
	// StoredConsistency 仅供公开投影记录重建前的一致性，不由 buildResult 持久化。
	// 重建出安全正文后仍保留原记录的失败，避免再次投影掩盖写入错误。
	StoredConsistency string `json:"stored_consistency,omitempty"`
}

// ResultHistory 不含下载地址、本地路径或恢复材料；说明和报告均明确未经新版核验。
type ResultHistory struct {
	Source           string                   `json:"source"`
	Status           string                   `json:"status"`
	Explanation      string                   `json:"explanation"`
	SelectedArtifact string                   `json:"selected_artifact,omitempty"`
	Reports          []ResultHistoricalReport `json:"reports,omitempty"`
}

type ResultHistoricalReport struct {
	ArtifactID string       `json:"artifact_id"`
	TaskID     string       `json:"task_id"`
	Report     asset.Report `json:"report"`
}

// deliverable 核验已保存的资产、任务关联和完整报告；不重新读取文件或调用提供方。
// 早期候选可以交付，但同一资产 ID 出现歧义或与当前操作矛盾时不能通过。
func deliverable(s Session, artifactID string) bool {
	a, ok := resultArtifact(s, artifactID)
	return ok && reportComplete(s, a) && goalSatisfied(s, a.Report)
}

func resultArtifact(s Session, id string) (Artifact, bool) {
	if s.ID == "" || id == "" {
		return Artifact{}, false
	}
	var found Artifact
	count := 0
	for _, a := range s.Artifacts {
		if a.ID == id {
			found = a
			count++
		}
	}
	if count != 1 || found.TaskID == "" {
		return Artifact{}, false
	}
	if op := s.Current; op != nil && (op.ID == id || op.ArtifactID == id) {
		if op.ID != id || op.ArtifactID != id || op.TaskID != found.TaskID || op.Stage != "done" {
			return Artifact{}, false
		}
	}
	return found, true
}

// reportComplete 同时核对数值、三项结论和已保存的验收上限。
// 文件检查失败也可构成完整的失败证据，此时面数必须为无法验证。
func reportComplete(s Session, a Artifact) bool {
	if _, ok := resultArtifact(s, a.ID); !ok || s.Intent == nil {
		return false
	}
	r := a.Report
	if optionalVersion(s) {
		if !validAcceptedIntent(s) || r.Limits == nil || !r.Limits.Equal(*s.Intent.Optional) || r.Bytes < 0 || (r.ResourceLimited && r.Valid) || (r.Valid && (r.Bytes <= 0 || r.Triangles <= 0)) || (!r.Valid && r.Triangles != 0) {
			return false
		}
		checks, passed := asset.OptionalChecks(r)
		if r.Passed != passed || len(r.Checks) != len(checks) {
			return false
		}
		for i, check := range checks {
			if r.Checks[i].Name != check.Name || r.Checks[i].Status != check.Status {
				return false
			}
		}
		return true
	}
	if r.Limits != nil {
		return false
	}
	if r.Bytes < 0 || r.MaxBytes <= 0 || r.MaxTriangles <= 0 || r.MaxBytes != s.Intent.MaxBytes || r.MaxTriangles != s.Intent.MaxTriangles || len(r.Checks) != 3 {
		return false
	}
	checks := map[string]string{}
	for _, c := range r.Checks {
		if _, exists := checks[c.Name]; exists {
			return false
		}
		checks[c.Name] = c.Status
	}
	if checks["文件体积"] != resultVerdict(r.Bytes <= r.MaxBytes) {
		return false
	}
	if r.Valid {
		return r.Bytes > 0 && r.Triangles > 0 && checks["文件有效性"] == "passed" && checks["几何规模"] == resultVerdict(r.Triangles <= r.MaxTriangles) && r.Passed == (r.Triangles <= r.MaxTriangles && r.Bytes <= r.MaxBytes)
	}
	return !r.Passed && r.Triangles == 0 && checks["文件有效性"] == "failed" && checks["几何规模"] == "unverifiable"
}

func resultVerdict(passed bool) string {
	if passed {
		return "passed"
	}
	return "failed"
}

// publicReport 不透传原始明细中的自然语言事实，而是根据相互一致的结构化证据生成明细。
// 不完整报告的数值不作为可信测量输出，原报告另保存在历史审计字段或原技术事件中。
func publicReport(s Session, a Artifact) asset.Report {
	if !reportComplete(s, a) {
		return asset.Report{Visual: resultVisualBoundary, Checks: []asset.Check{
			{Name: "文件有效性", Status: "unverifiable", Detail: "已存报告证据不完整或相互矛盾，不能确认文件有效性。"},
			{Name: "几何规模", Status: "unverifiable", Detail: "缺少可信的面数检查证据。"},
			{Name: "文件体积", Status: "unverifiable", Detail: "缺少完整且一致的体积检查证据。"},
		}}
	}
	r := a.Report
	r.Visual = resultVisualBoundary
	if optionalVersion(s) {
		r.Checks, r.Passed = asset.OptionalChecks(r)
		return r
	}
	r.Checks = nil
	if r.Valid {
		r.Checks = append(r.Checks,
			asset.Check{Name: "文件有效性", Status: "passed", Detail: "静态、自包含、非空三角面 GLB"},
			asset.Check{Name: "几何规模", Status: resultVerdict(r.Triangles <= r.MaxTriangles), Detail: fmt.Sprintf("实测 %d / 上限 %d 个三角面", r.Triangles, r.MaxTriangles)})
	} else {
		r.Checks = append(r.Checks,
			asset.Check{Name: "文件有效性", Status: "failed", Detail: "已存技术报告记录文件有效性检查未通过。"},
			asset.Check{Name: "几何规模", Status: "unverifiable", Detail: "文件未通过有效性检查，不能给出可信面数。"})
	}
	r.Checks = append(r.Checks, asset.Check{Name: "文件体积", Status: resultVerdict(r.Bytes <= r.MaxBytes), Detail: fmt.Sprintf("实测 %d / 上限 %d 字节", r.Bytes, r.MaxBytes)})
	return r
}

// buildResult 只接受调用方给出的程序来源/原因代码，不读取 Final、原始模型说明或错误正文。
// 调用方负责在同一事务中校验时间边界并保存终态；这里不读取当前时间或产生副作用。
func buildResult(s Session, source, reason string) (*Result, string) {
	if validAnswer(s) {
		return buildAnswerResult(s)
	}
	r := &Result{Version: resultVersion, Status: s.Status, Source: source, Reason: reason, Evidence: "verified"}
	switch source {
	case "agent", "runtime", "recovery", "history":
	default:
		r.Source, r.Reason, r.Evidence = "runtime", "execution_failed", "unverifiable"
	}
	switch r.Status {
	case "completed", "failed", "stopped":
	default:
		r.Status, r.Evidence = "failed", "unverifiable"
	}
	if s.Current != nil {
		r.OperationID = s.Current.ID
	}
	var lines []string
	if r.Status == "completed" {
		if !deliverable(s, s.SelectedArtifact) || (source == "recovery" && (s.Current == nil || s.Current.ArtifactID != s.SelectedArtifact)) {
			r.Status, r.Reason, r.Evidence = "failed", "evidence_unavailable", "unverifiable"
			lines = append(lines, "交付证据不足，不能确认技术通过；本地执行已结束。")
		} else {
			r.ArtifactID = s.SelectedArtifact
			if optionalVersion(s) {
				lines = append(lines, "已交付通过文件有效性与适用技术约束检查的静态 GLB。")
			} else {
				lines = append(lines, "已交付通过文件有效性、几何规模和文件体积检查的静态 GLB。")
			}
			if r.Source == "agent" {
				r.Reason = "delivered"
			}
		}
	}
	if len(lines) == 0 {
		if r.Source == "agent" && r.Reason != "evidence_unavailable" {
			// 模型结束决定不包含关于预算耗尽或方案不可行的程序证明。
			r.Reason = "agent_stop"
		}
		lines = append(lines, resultReasonText(s, r))
	} else if r.Status == "completed" && r.Source != "agent" {
		lines = append(lines, resultReasonText(s, r))
	}
	if a, ok := resultEvidenceArtifact(s, r); ok {
		r.ArtifactID = a.ID
		if r.Status != "completed" {
			lines = append(lines, "已有候选未被选作本次交付，检查结果如下：")
		}
		if optionalVersion(s) && s.GoalKind == "decimate" && s.Intent != nil && s.Intent.ReductionMode == "further" && s.InputAssessment != nil && reportComplete(s, a) && a.Report.Valid {
			lines = append(lines, fmt.Sprintf("减面目标：初始输入 %d 面，输出 %d 面；目标完成：%t。", s.InputAssessment.Triangles, a.Report.Triangles, goalSatisfied(s, a.Report)))
		}
		projected := publicReport(s, a)
		for _, check := range projected.Checks {
			lines = append(lines, check.Name+"："+check.Detail)
		}
	}
	if optionalVersion(s) && s.Current != nil && s.Current.ErrorCode == "download_resource_limit" {
		lines = append(lines, "文件超出系统 150 MiB 下载保护上限，未取得完整文件，面数和实际体积未核验；这不是用户验收上限失败。")
	}
	if s.Production >= 0 && s.ModelCalls >= 0 && s.Limits.Submissions > 0 && s.Limits.Calls > 0 {
		lines = append(lines, fmt.Sprintf("累计生产提交 %d / %d 次，模型调用 %d / %d 次。", s.Production, s.Limits.Submissions, s.ModelCalls, s.Limits.Calls))
	}
	if r.Status != "completed" && s.Current != nil && (s.Current.TaskID != "" || s.Current.Stage == "submitting") {
		lines = append(lines, "本地执行结束不表示远端任务已取消。")
	}
	lines = append(lines, resultVisualBoundary)
	return r, strings.Join(lines, "\n")
}

func resultReasonText(s Session, r *Result) string {
	if r.Source == "history" {
		if r.Reason == "evidence_unavailable" {
			r.Evidence = "unverifiable"
			return "历史交付证据不足，不能确认技术通过；原始状态和说明保留为未核验的历史证据。"
		}
		r.Reason = "historical_state"
		if r.Status == "completed" {
			return "以上结论依据历史记录中的技术报告整理，不代表当时运行过新版结果核验。"
		}
		if r.Status == "stopped" {
			return "历史记录表明本地执行已停止；无法仅凭原始说明核实更具体的停止原因。"
		}
		return "历史记录表明本地执行已结束，未形成可确认的交付；具体结束原因未核验。"
	}
	switch r.Reason {
	case "delivered":
		return "程序已根据本请求的实测技术证据完成收尾。"
	case "agent_stop":
		return "Agent 已选择结束本次请求，没有作出交付选择；原始说明保留为决策记录。"
	case "user_stop":
		return "用户已停止本地执行。"
	case "execution_deadline":
		return "生产执行期限已到，已停止本地执行。"
	case "idle_timeout":
		return "生产前等待期限已到，已结束本地执行。"
	case "model_budget":
		if s.Limits.Calls > 0 && s.ModelCalls >= s.Limits.Calls {
			return "模型调用额度已耗尽，由程序依据现有证据完成收尾。"
		}
	case "production_budget":
		if s.Limits.Submissions > 0 && s.Production >= s.Limits.Submissions {
			return "生产提交额度已耗尽，由程序依据现有证据完成收尾。"
		}
	case "submission_unknown":
		if s.Current != nil && s.Current.Stage == "submitting" && s.Current.TaskID == "" {
			return "生产提交结果未知，无法确认远端是否接受；不会自动重复提交。"
		}
	case "known_task_recovery":
		if s.Current != nil && s.Current.TaskID != "" && s.Current.Stage == "done" {
			return "程序依据原 Tripo 任务的执行结果完成恢复收尾，未新增模型决策或生产操作。"
		}
	case "operation_failed":
		return "资产生产流程未能完成本地交付。"
	case "model_failed":
		return "模型调用未能完成，已结束本地执行。"
	case "recovery_failed":
		return "原请求的执行恢复未能完成，已结束本地执行。"
	case "execution_failed":
		return "本地执行未能继续，已结束本次请求。"
	case "evidence_unavailable":
		r.Evidence = "unverifiable"
		return "现有证据不足，不能确认交付结论；本地执行已结束。"
	}
	r.Reason, r.Evidence = "evidence_unavailable", "unverifiable"
	return "本地执行已结束；现有证据不足以确认更具体的结束原因。"
}

func resultEvidenceArtifact(s Session, r *Result) (Artifact, bool) {
	if r.Status == "completed" {
		return resultArtifact(s, r.ArtifactID)
	}
	if r.Source == "recovery" {
		if s.Current == nil {
			return Artifact{}, false
		}
		return resultArtifact(s, s.Current.ArtifactID)
	}
	if len(s.Artifacts) == 0 {
		return Artifact{}, false
	}
	return resultArtifact(s, s.Artifacts[len(s.Artifacts)-1].ID)
}

// publicResult 仅对终态构建只读投影；在途状态继续原样参与模型输入和恢复。
// 所有被替换的报告均为新值，不修改原始 Session、报告切片或时间字段。
func publicResult(s Session) Session {
	if !s.Terminal() {
		return s
	}
	out := s
	legacy := s.Result == nil || s.Result.Version != resultVersion
	storedConsistency := "not_applicable"
	if s.Result != nil {
		switch {
		case s.Result.StoredConsistency == "failed", s.Result.Version != resultVersion, !resultMatchesEvidence(s):
			storedConsistency = "failed"
		case s.Result.StoredConsistency == "not_applicable" && s.Result.Source == "history" && s.Result.History != nil:
			// 已投影的历史记录没有新版原始结果，不能伪装成曾持久化过新版结论。
		default:
			storedConsistency = "passed"
		}
	}
	source, reason := "history", "historical_state"
	if !legacy {
		source, reason = s.Result.Source, s.Result.Reason
	}
	if s.Status == "completed" && !deliverable(s, s.SelectedArtifact) {
		out.Status, out.SelectedArtifact, reason = "failed", "", "evidence_unavailable"
	} else if s.Status != "completed" {
		out.SelectedArtifact = ""
	}
	out.Artifacts = make([]Artifact, len(s.Artifacts))
	for i, a := range s.Artifacts {
		out.Artifacts[i] = a
		out.Artifacts[i].Report = publicReport(s, a)
	}
	out.Result, out.Final = buildResult(out, source, reason)
	out.Result.StoredConsistency = storedConsistency
	if legacy {
		history := &ResultHistory{Source: "historical_unverified", Status: s.Status, Explanation: s.Final, SelectedArtifact: s.SelectedArtifact}
		for _, a := range s.Artifacts {
			r := a.Report
			r.Checks = slices.Clone(r.Checks)
			history.Reports = append(history.Reports, ResultHistoricalReport{ArtifactID: a.ID, TaskID: a.TaskID, Report: r})
		}
		out.Result.History = history
	} else if s.Result.History != nil {
		history := *s.Result.History
		history.Reports = slices.Clone(history.Reports)
		for i := range history.Reports {
			history.Reports[i].Report.Checks = slices.Clone(history.Reports[i].Report.Checks)
		}
		out.Result.History = &history
	}
	return out
}

// resultMatchesEvidence 比较正式结果的已存依据与正文，忽略只读投影附加的审计字段。
// 原始模型说明不属于此比较对象；它是否真实仍由独立 Agent 评测判断。
func resultMatchesEvidence(s Session) bool {
	if s.Result == nil || s.Result.Version != resultVersion {
		return false
	}
	expected, final := buildResult(s, s.Result.Source, s.Result.Reason)
	actual := *s.Result
	actual.History, actual.StoredConsistency = nil, ""
	return actual == *expected && s.Final == final
}

// resultEvidenceCheck 只验证正式结果的构造依据，不判断原始模型说明的语义真伪。
// 历史缺证据投影不能借一个正确的保守模板被标记为已有完整交付证据。
func resultEvidenceCheck(s Session) asset.Check {
	check := asset.Check{Name: "正式结果证据一致性", Status: "pending", Detail: "请求尚未结束。"}
	if !s.Terminal() {
		return check
	}
	if s.Result == nil || s.Result.Version != resultVersion {
		s = publicResult(s)
	}
	if s.Result.StoredConsistency == "failed" || !resultMatchesEvidence(s) {
		check.Status, check.Detail = "failed", "原已保存的正式结果与状态、引用或技术证据不一致；公开正文已保守重建，原记录未被修改。"
		return check
	}
	if s.Result.Evidence == "unverifiable" {
		check.Status, check.Detail = "unverifiable", "现有结果依据不足；公开结论保留不确定性，不认证原始说明。"
		return check
	}
	check.Status, check.Detail = "passed", "正式结果由本请求的状态和相应技术证据生成；未核验原始自由解释。"
	return check
}
