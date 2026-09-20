package app

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lkmaVanilla/tripo-3d-agent/internal/asset"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/testfixture"
)

func resultFixture() Session {
	r := asset.Inspect(testfixture.Cube(12), 5000, 10<<20)
	return Session{
		ID: "result-session", Status: "completed", SelectedArtifact: "artifact-a",
		Intent:     &Intent{MaxTriangles: 5000, MaxBytes: 10 << 20},
		Current:    &Operation{ID: "artifact-a", ArtifactID: "artifact-a", TaskID: "task-a", Stage: "done"},
		Artifacts:  []Artifact{{ID: "artifact-a", TaskID: "task-a", Path: "/private/result.glb", SourceURL: "https://private.invalid/model?signature=hidden", Report: r}},
		Production: 1, ModelCalls: 5, Limits: Limits{Calls: 20, Submissions: 3},
		Ended: time.Unix(1000, 0).UTC(), Expires: time.Unix(2000, 0).UTC(),
		Final: "虚构说明：只有1000面，绑定和动画已完成。",
	}
}

// 用真实检查器建立报告，随后逐一破坏独立证据，避免只验证 Passed 这个布尔值。
func TestResultDeliverableRequiresCompleteEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Session)
		want   bool
	}{
		{"complete", func(*Session) {}, true},
		{"wrong-reference", func(s *Session) { s.SelectedArtifact = "other-session-asset" }, false},
		{"missing-task", func(s *Session) { s.Artifacts[0].TaskID = "" }, false},
		{"wrong-current-task", func(s *Session) { s.Current.TaskID = "other-task" }, false},
		{"wrong-current-artifact", func(s *Session) { s.Current.ArtifactID = "other-artifact" }, false},
		{"duplicate-artifact", func(s *Session) { s.Artifacts = append(s.Artifacts, s.Artifacts[0]) }, false},
		{"no-intent", func(s *Session) { s.Intent = nil }, false},
		{"changed-limit", func(s *Session) { s.Intent.MaxTriangles = 1000 }, false},
		{"missing-check", func(s *Session) { s.Artifacts[0].Report.Checks = s.Artifacts[0].Report.Checks[:2] }, false},
		{"duplicate-check", func(s *Session) { s.Artifacts[0].Report.Checks[2] = s.Artifacts[0].Report.Checks[0] }, false},
		{"false-passed", func(s *Session) { s.Artifacts[0].Report.Triangles = 6000 }, false},
		{"zero-triangles", func(s *Session) { s.Artifacts[0].Report.Triangles = 0 }, false},
		{"zero-bytes", func(s *Session) { s.Artifacts[0].Report.Bytes = 0 }, false},
		{"failed-check", func(s *Session) { s.Artifacts[0].Report.Checks[1].Status = "failed" }, false},
		{"previous-candidate", func(s *Session) { s.Current = &Operation{ID: "next-op", TaskID: "next-task", Stage: "done"} }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := resultFixture()
			tc.mutate(&s)
			if got := deliverable(s, s.SelectedArtifact); got != tc.want {
				t.Fatalf("deliverable=%v want %v", got, tc.want)
			}
		})
	}
}

func TestResultUsesReportAndNeverFreeExplanation(t *testing.T) {
	s := resultFixture()
	s.Artifacts[0].Report.Checks[0].Detail = "已完成绑定和动画"
	s.Current.Error = "远端已取消，所有目标已满足"
	r, final := buildResult(s, "agent", "delivered")
	if r.Status != "completed" || r.ArtifactID != "artifact-a" || r.Evidence != "verified" || !strings.Contains(final, "实测 12 / 上限 5000") {
		t.Fatalf("unexpected result: %+v %s", r, final)
	}
	for _, fabricated := range []string{"只有1000面", "已完成绑定和动画", "远端已取消", "所有目标已满足"} {
		if strings.Contains(final, fabricated) {
			t.Fatalf("free text became fact: %s", final)
		}
	}
	s.Result, s.Final = r, final
	if got := resultEvidenceCheck(s); got.Status != "passed" {
		t.Fatalf("generated result not consistent: %+v", got)
	}
	s.Final += "已完成绑定"
	if got := resultEvidenceCheck(s); got.Status != "failed" {
		t.Fatalf("modified formal text accepted: %+v", got)
	}
}

func TestResultAgentStopDoesNotCertifyClaimedReason(t *testing.T) {
	s := resultFixture()
	s.Status, s.SelectedArtifact = "failed", ""
	s.Final = "预算已耗尽，所有方案都不可行，已经全部制作成功"
	r, final := buildResult(s, "agent", "model_budget")
	if r.Reason != "agent_stop" || r.Status != "failed" || !strings.Contains(final, "没有作出交付选择") || !strings.Contains(final, "模型调用 5 / 20") {
		t.Fatalf("agent stop facts lost: %+v %s", r, final)
	}
	if strings.Contains(final, "额度已耗尽") || strings.Contains(final, "所有方案") || strings.Contains(final, "制作成功") {
		t.Fatalf("agent reason became fact: %s", final)
	}
}

func TestResultFailedReportPreservesUnverifiableGeometry(t *testing.T) {
	s := resultFixture()
	s.Status, s.SelectedArtifact = "failed", ""
	s.Artifacts[0].Report = asset.Inspect([]byte("invalid-glb"), 5000, 10<<20)
	r, final := buildResult(s, "agent", "agent_stop")
	if r.Status != "failed" || !strings.Contains(final, "不能给出可信面数") || strings.Contains(final, "实测 0") {
		t.Fatalf("invalid geometry became a measurement: %+v %s", r, final)
	}
	projected := publicReport(s, s.Artifacts[0])
	if projected.Checks[0].Status != "failed" || projected.Checks[1].Status != "unverifiable" || projected.Checks[2].Status != "passed" {
		t.Fatalf("lost known failure evidence: %+v", projected)
	}
}

func TestResultRuntimeReasonsKeepRemoteUncertainty(t *testing.T) {
	for _, tc := range []struct {
		reason, want string
	}{
		{"user_stop", "用户已停止本地执行"},
		{"execution_deadline", "生产执行期限已到"},
		{"idle_timeout", "生产前等待期限已到"},
		{"submission_unknown", "无法确认远端是否接受"},
		{"operation_failed", "资产生产流程未能完成"},
		{"model_failed", "模型调用未能完成"},
		{"recovery_failed", "执行恢复未能完成"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			s := resultFixture()
			s.Status, s.SelectedArtifact, s.Artifacts = "failed", "", nil
			if tc.reason == "submission_unknown" {
				s.Current.Stage, s.Current.TaskID = "submitting", ""
			}
			_, final := buildResult(s, "runtime", tc.reason)
			if !strings.Contains(final, tc.want) || !strings.Contains(final, "不表示远端任务已取消") {
				t.Fatalf("wrong boundary: %s", final)
			}
		})
	}
	s := resultFixture()
	s.ModelCalls = s.Limits.Calls
	r, final := buildResult(s, "runtime", "model_budget")
	if r.Status != "completed" || !strings.Contains(final, "模型调用额度已耗尽") {
		t.Fatalf("budget lost valid delivery: %+v %s", r, final)
	}
	s.Artifacts[0].Report.Checks = nil
	r, _ = buildResult(s, "runtime", "model_budget")
	if r.Status == "completed" || r.Evidence != "unverifiable" {
		t.Fatalf("budget bypassed delivery evidence: %+v", r)
	}
}

// 历史投影必须同时保守处理状态与候选报告，并保持原记录、切片和时间不变。
func TestResultHistoricalProjectionIsConservativeAndReadOnly(t *testing.T) {
	s := resultFixture()
	good := s.Artifacts[0]
	good.ID, good.TaskID = "complete-candidate", "complete-task"
	s.Artifacts[0].Report.Checks = nil
	s.Artifacts = append([]Artifact{good}, s.Artifacts...)
	before, _ := json.Marshal(s)
	projected := publicResult(s)
	if projected.Status != "failed" || projected.SelectedArtifact != "" || projected.Result.Evidence != "unverifiable" || !strings.Contains(projected.Final, "历史交付证据不足") {
		t.Fatalf("legacy false completion leaked: %+v", projected)
	}
	if !projected.Artifacts[0].Report.Passed || projected.Artifacts[1].Report.Passed || projected.Artifacts[1].Report.Valid {
		t.Fatalf("wrong per-candidate projection: %+v", projected.Artifacts)
	}
	if projected.Result.History.Status != "completed" || projected.Result.History.Explanation != s.Final || !projected.Result.History.Reports[1].Report.Passed {
		t.Fatal("original audit evidence was lost")
	}
	if !projected.Ended.Equal(s.Ended) || !projected.Expires.Equal(s.Expires) || projected.Production != s.Production {
		t.Fatal("projection changed execution facts")
	}
	if got := resultEvidenceCheck(projected); got.Status != "unverifiable" {
		t.Fatalf("missing history evidence marked passed: %+v", got)
	}
	audit, _ := json.Marshal(projected.Result)
	if strings.Contains(string(audit), "/private/result.glb") || strings.Contains(string(audit), "private.invalid") {
		t.Fatal("private artifact locations exposed through result audit")
	}
	if again := publicResult(projected); !reflect.DeepEqual(again, projected) {
		t.Fatal("result projection is not idempotent")
	}
	projected.Artifacts[0].Report.Checks[0].Detail = "mutated public report"
	projected.Result.History.Reports[0].Report.Checks[0].Detail = "mutated public audit"
	after, _ := json.Marshal(s)
	if string(before) != string(after) {
		t.Fatal("read-only projection mutated persisted session")
	}
}

func TestResultHistoricalCompleteAndActiveSession(t *testing.T) {
	s := resultFixture()
	projected := publicResult(s)
	if projected.Status != "completed" || projected.Result.Source != "history" || strings.Contains(projected.Final, "绑定和动画已完成") || !strings.Contains(projected.Final, "不代表当时运行过新版") {
		t.Fatalf("history source not preserved: %+v", projected)
	}
	if got := resultEvidenceCheck(projected); got.Status != "passed" {
		t.Fatalf("complete historical evidence rejected: %+v", got)
	}
	s.Ended, s.Status = time.Time{}, "running"
	s.Artifacts[0].Report.Checks = nil
	if got := publicResult(s); !reflect.DeepEqual(got, s) {
		t.Fatal("historical projection changed active runtime state")
	}
	if got := resultEvidenceCheck(s); got.Status != "pending" {
		t.Fatalf("active request marked finished: %+v", got)
	}
}

func TestResultRecoveryAndStoredCompletionRecheckEvidence(t *testing.T) {
	s := resultFixture()
	r, final := buildResult(s, "recovery", "known_task_recovery")
	if r.Status != "completed" || r.ArtifactID != s.Current.ArtifactID || !strings.Contains(final, "原 Tripo 任务") {
		t.Fatalf("known task evidence lost: %+v %s", r, final)
	}
	s.Current = &Operation{ID: "different-op", TaskID: "different-task", Stage: "done"}
	r, _ = buildResult(s, "recovery", "known_task_recovery")
	if r.Status == "completed" || r.Evidence != "unverifiable" {
		t.Fatalf("old candidate presented as recovered current task: %+v", r)
	}
	s = resultFixture()
	s.Result, s.Final = buildResult(s, "agent", "delivered")
	s.Artifacts[0].Report.Checks = nil
	projected := publicResult(s)
	if projected.Status != "failed" || projected.Result.Reason != "evidence_unavailable" || projected.Result.Evidence != "unverifiable" || strings.Contains(projected.Final, "Agent 已选择结束") {
		t.Fatalf("missing saved evidence became an Agent stop choice: %+v", projected)
	}
	if got := resultEvidenceCheck(projected); got.Status != "failed" {
		t.Fatalf("inconsistent saved completion was hidden: %+v", got)
	}
	// 空文件的面数不可验证，但读取零字节本身是检查器能够记录的失败证据。
	s.Artifacts[0].Report = asset.Inspect(nil, 5000, 10<<20)
	if report := publicReport(s, s.Artifacts[0]); report.Checks[0].Status != "failed" || report.Checks[1].Status != "unverifiable" || report.Checks[2].Status != "passed" {
		t.Fatalf("empty file lost known byte evidence: %+v", report)
	}
}

// 读取时重建出的安全正文不能洗掉已持久化 Result/Final 的不一致。
func TestResultProjectionPreservesStoredInconsistency(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Session)
	}{
		{"formal-text", func(s *Session) { s.Final = "已经完成绑定和动画，实测只有1000面" }},
		{"wrong-result-status", func(s *Session) { s.Result.Status = "failed" }},
		{"wrong-asset-reference", func(s *Session) { s.Result.ArtifactID = "foreign-asset" }},
		{"wrong-operation-reference", func(s *Session) { s.Result.OperationID = "foreign-operation" }},
		{"wrong-evidence-state", func(s *Session) { s.Result.Evidence = "unverifiable" }},
		{"wrong-reason", func(s *Session) { s.Result.Reason = "model_budget" }},
		{"unknown-version", func(s *Session) { s.Result.Version = resultVersion + 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := resultFixture()
			s.Result, s.Final = buildResult(s, "agent", "delivered")
			tc.mutate(&s)
			before, _ := json.Marshal(s)
			for _, input := range []Session{s, publicResult(s), publicResult(publicResult(s))} {
				if got := resultEvidenceCheck(input); got.Status != "failed" {
					t.Fatalf("stored inconsistency disappeared: %+v", got)
				}
			}
			projected := publicResult(s)
			if strings.Contains(projected.Final, "已经完成绑定和动画") || !strings.Contains(projected.Final, "实测 12 / 上限 5000") || projected.Result.StoredConsistency != "failed" {
				t.Fatalf("unsafe or unmarked projection: %+v", projected)
			}
			for _, evaluation := range []Evaluation{evaluate(s), s.View()["evaluation"].(Evaluation)} {
				found := false
				for _, check := range evaluation.Checks {
					if check.Name == "正式结果证据一致性" {
						found = check.Status == "failed"
					}
				}
				if !found {
					t.Fatalf("public evaluation lost stored failure: %+v", evaluation)
				}
			}
			after, _ := json.Marshal(s)
			if string(before) != string(after) {
				t.Fatal("read-only check changed stored record")
			}
		})
	}
	s := resultFixture()
	s.Result, s.Final = buildResult(s, "agent", "delivered")
	if s.Result.StoredConsistency != "" {
		t.Fatal("projection marker became required persisted state")
	}
	projected := publicResult(s)
	if projected.Result.StoredConsistency != "passed" || resultEvidenceCheck(projected).Status != "passed" || !reflect.DeepEqual(publicResult(projected), projected) {
		t.Fatal("consistent new result does not survive repeated projection")
	}
	legacy := resultFixture()
	projected = publicResult(legacy)
	if projected.Result.StoredConsistency != "not_applicable" || resultEvidenceCheck(projected).Status != "passed" || !reflect.DeepEqual(publicResult(projected), projected) {
		t.Fatal("historical reconstruction was treated as a stored new result")
	}
	legacy.Artifacts[0].Report.Checks = nil
	view := legacy.View()
	if view["result"].(*Result).StoredConsistency != "not_applicable" {
		t.Fatal("missing legacy evidence was classified as a new persisted result")
	}
	for _, check := range view["evaluation"].(Evaluation).Checks {
		if check.Name == "正式结果证据一致性" && check.Status != "unverifiable" {
			t.Fatalf("legacy evidence no longer remains uncertain: %+v", check)
		}
	}
}
