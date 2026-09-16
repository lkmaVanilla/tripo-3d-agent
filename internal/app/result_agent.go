package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var errInvalidDelivery = errors.New("没有可支持交付结论的技术检查证据")

// finishVerified 只在持久化修改回调内使用；正式结论不接受模型或外部错误正文。
// 已结束的请求拒绝再次收尾，使原终态、保留期限和结束事件保持幂等。
func (s *Session) finishVerified(status, source, reason string) error {
	if s.Terminal() {
		return ErrClosed
	}
	if status != "completed" && status != "failed" && status != "stopped" {
		return fmt.Errorf("不支持的终止状态")
	}
	if status == "completed" && !deliverable(*s, s.SelectedArtifact) {
		return errInvalidDelivery
	}
	s.Status = status
	if status != "completed" {
		s.SelectedArtifact = ""
	}
	s.Result, s.Final = buildResult(*s, source, reason)
	s.Ended = time.Now().UTC()
	s.Expires = s.Ended.Add(s.Limits.Retention)
	s.HasSlot, s.ResumeRequested = false, false
	if conversationVersion(executionVersion(*s)) {
		kind := "ended"
		if status == "completed" {
			kind = "delivery"
		}
		s.Outcome = &RunOutcome{Kind: kind, Source: source}
	}
	return nil
}

// finishRequest 保留既有工具参数和 Agent 选择；Explanation 仅进入原始提议证据。
func (s *Service) finishRequest(ctx context.Context, id string, c *pauseCoordinator, in *finishInput) (string, error) {
	if c.mode != "normal" {
		return "", recoveryError("recovery_seed_invalid")
	}
	if strings.TrimSpace(in.Explanation) == "" {
		return s.block(id, "invalid_explanation", "必须解释交付或停止依据")
	}
	v, err := s.store.Edit(ctx, id, func(v *Session) error {
		if err := checkExecution(*v, time.Now()); err != nil {
			return err
		}
		if in.Deliver {
			if !deliverable(*v, in.ArtifactID) {
				return errInvalidDelivery
			}
			v.SelectedArtifact = in.ArtifactID
			return v.finishVerified("completed", "agent", "delivered")
		}
		return v.finishVerified("failed", "agent", "agent_stop")
	}, "agent_finished", in)
	if errors.Is(err, errInvalidDelivery) {
		return s.block(id, "false_validation", err.Error())
	}
	if err != nil {
		// 停止、期限和存储错误不是模型伪造证据，不计入 Agent 违规。
		return "", err
	}
	return jsonString(v.View()), nil
}

// resultReason 从程序类型和最新状态分类，不根据错误字符串中的自然语言猜测事实。
func resultReason(v Session, cause error) string {
	now := time.Now()
	if !v.Deadline.IsZero() && !now.Before(v.Deadline) {
		return "execution_deadline"
	}
	if v.Deadline.IsZero() && now.Sub(v.LastUser) >= v.Limits.Idle {
		return "idle_timeout"
	}
	if v.Current != nil && v.Current.Stage == "submitting" && v.Current.TaskID == "" {
		return "submission_unknown"
	}
	if errors.Is(cause, ErrBudget) {
		if v.ModelCalls >= v.Limits.Calls {
			return "model_budget"
		}
		if v.Production >= v.Limits.Submissions {
			return "production_budget"
		}
	}
	var fault *recoveryFault
	if errors.As(cause, &fault) || errors.Is(cause, ErrCheckpointMismatch) {
		return "recovery_failed"
	}
	var modelFailure *modelCallError
	if errors.As(cause, &modelFailure) {
		return "model_failed"
	}
	if v.Current != nil && v.Current.Stage == "done" && v.Current.Error != "" {
		return "operation_failed"
	}
	return "execution_failed"
}
