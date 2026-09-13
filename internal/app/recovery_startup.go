package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/eino/schema"
)

// reconcile 在启动普通调度前核对所有未终止会话，先处理协议不一致，再允许继续执行。
// 单个会话缺少恢复证据时记录失败；存储本身不可用则返回错误，避免无依据地启动调度。
func (s *Service) reconcile() error {
	all, err := s.store.List(s.ctx, "")
	if err != nil {
		return fmt.Errorf("恢复对账无法读取存储：%w", err)
	}
	for _, v := range all {
		if v.Terminal() {
			continue
		}
		if err = s.reconcileSession(s.ctx, v); err != nil {
			if s.ctx.Err() != nil {
				return s.ctx.Err()
			}
			// 已知远端任务独立于 Agent 检查点存在；仍有效时由确定性路径续查并保存结果。
			// RecoveryFailure 阻止继续信任损坏的 Agent 状态，不意味着可以重新提交任务。
			if v.Current != nil && v.Current.TaskID != "" && checkExecution(v, time.Now()) == nil {
				_, saveErr := s.store.Edit(s.ctx, v.ID, func(x *Session) error {
					if err := checkExecution(*x, time.Now()); err != nil {
						return err
					}
					x.RecoveryFailure = s.redact(err.Error())
					x.Status, x.HasSlot = "running", true
					return nil
				}, "recovery_rejected", map[string]string{"reason": s.redact(err.Error()), "action": "query_known_task"})
				if saveErr == nil {
					continue
				}
			}
			if err = s.finalize(v.ID, fmt.Errorf("recovery_rejected: %w", err)); err != nil {
				return err
			}
		}
	}
	return nil
}

// reconcileSession 按终态/期限、未知提交、待提交草案、已提交恢复点和旧记录的次序检查。
// 对无法证明安全的记录返回明确错误，不重新询问模型来猜测原本应该执行什么。
func (s *Service) reconcileSession(ctx context.Context, v Session) error {
	if err := checkExecution(v, time.Now()); err != nil {
		return err
	}
	if v.Current != nil && v.Current.Stage == "submitting" && v.Current.TaskID == "" {
		// 本地事务无法与远端 POST 原子提交；缺少 TaskID 时不能区分未发送和已被受理。
		return recoveryError("submission_outcome_unknown")
	}
	if v.PendingPause != nil {
		// 草案通过验证后交给正常恢复执行重建，此处不覆盖它仍然依赖的上一代检查点。
		return validatePending(v)
	}
	r, err := s.store.LoadCheckpoint(ctx, v.ID, v.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if v.ResumePoint != nil {
		if err != nil || r.Point == nil {
			return recoveryError("checkpoint_identity_mismatch")
		}
		return nil
	}
	if err == nil {
		if r.Point != nil {
			return recoveryError("checkpoint_identity_mismatch")
		}
		// Eino 负责解码旧字节；legacy 模式禁止新模型决策和生产提交，工具仅验证并重新暂停。
		c := &pauseCoordinator{s: s, id: v.ID, mode: "legacy", expected: 0, resuming: true}
		interrupted, e := s.executeRunner(ctx, v, c, true, nil)
		if e != nil {
			return e
		}
		if !interrupted || !c.committed {
			return recoveryError("legacy_recovery_unavailable")
		}
		return nil
	}
	if v.Current != nil && v.Current.TaskID != "" {
		return recoveryError("checkpoint_unavailable")
	}
	if v.RecoverySchemaVersion == 0 && (v.WaitID != "" || v.Current != nil) {
		// 旧代码可能先保存业务状态再写检查点；仅在历史能唯一还原原提议时补建草案。
		p, e := legacyPending(v)
		if e != nil {
			return e
		}
		_, e = s.store.Edit(ctx, v.ID, func(x *Session) error {
			if err := checkExecution(*x, time.Now()); err != nil {
				return err
			}
			if x.generation() != 0 || x.PendingPause != nil {
				return recoveryError("checkpoint_identity_mismatch")
			}
			x.PendingPause = p
			return validatePending(*x)
		}, "", nil)
		return e
	}
	// 没有恢复点的新会话也不能带悬空工具消息继续运行。
	if err := validateProtocol(v.History); err != nil {
		return err
	}
	return nil
}

// legacyAnswer 只从问题文本和 ToolCallID 同时匹配的工具结果中找回答案。
// 恰好一个匹配才算证据充分；多个候选即使文本相同，也不能证明它们属于同一次接受。
func legacyAnswer(history []*schema.Message, question, toolCallID string) string {
	if toolCallID == "" {
		return ""
	}
	answers := []string{}
	for i, m := range history {
		if m == nil || m.Role != schema.Assistant || len(m.ToolCalls) != 1 {
			continue
		}
		call := m.ToolCalls[0]
		var q questionInput
		if call.ID != toolCallID || call.Function.Name != "ask_user" || json.Unmarshal([]byte(call.Function.Arguments), &q) != nil || q.Question != question {
			continue
		}
		if i+1 < len(history) && history[i+1] != nil && history[i+1].Role == schema.Tool && history[i+1].ToolCallID == call.ID {
			var result struct {
				Answer string `json:"user_answer"`
			}
			if json.Unmarshal([]byte(history[i+1].Content), &result) == nil && result.Answer != "" {
				answers = append(answers, result.Answer)
			}
		}
	}
	if len(answers) == 1 {
		return answers[0]
	}
	return ""
}

// legacyPending 为缺少检查点的旧记录提取最后一个完整模型提议。
// Existing 保留旧代码已经增加的计数与队列事实；生产仅接受尚未提交的 ready 操作。
func legacyPending(v Session) (*PendingPause, error) {
	if len(v.History) == 0 {
		return nil, recoveryError("legacy_recovery_unavailable")
	}
	seed, err := newReplaySeed(v.History[:len(v.History)-1], v.History[len(v.History)-1], v.Model)
	if err != nil {
		return nil, recoveryError("legacy_recovery_unavailable")
	}
	p := &PendingPause{Point: ResumePoint{Version: recoveryVersion, SessionID: v.ID, Key: v.ID, PauseID: newID(), Generation: 1}, Existing: true, Legacy: true, Seed: seed}
	switch seed.ToolName {
	case "ask_user":
		var q questionInput
		if json.Unmarshal([]byte(seed.Response.ToolCalls[0].Function.Arguments), &q) != nil || q.Question == "" || q.Question != v.Question || v.WaitID == "" {
			return nil, recoveryError("legacy_recovery_unavailable")
		}
		p.Point.Kind, p.Point.RefID, p.Question = "question", v.WaitID, v.Question
	case "generate_asset", "decimate_asset":
		if v.Current == nil || v.Current.Stage != "ready" || v.Current.TaskID != "" {
			return nil, recoveryError("legacy_recovery_unavailable")
		}
		p.Point.Kind, p.Point.RefID, p.Operation = "production", v.Current.ID, v.Current
		_, _, p.Reason, err = seedProduction(v, seed)
		if err != nil {
			return nil, err
		}
	default:
		return nil, recoveryError("legacy_recovery_unavailable")
	}
	p.DraftHash = draftHash(p)
	v.PendingPause = p
	if err = validatePending(v); err != nil {
		return nil, err
	}
	return p, nil
}
