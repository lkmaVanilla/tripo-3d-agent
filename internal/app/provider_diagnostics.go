package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
)

// recordedProviderError 只在本次调用栈去重；每次真正的新尝试都有新的身份。
type recordedProviderError struct{ error }

func (e *recordedProviderError) Unwrap() error { return e.error }

// recordProviderFailure 不向调用方返回诊断存储错误，避免重试器将日志故障当成网络故障。
// 记录失败不能撤销先前提交事实，也不能阻止稍后保存已知 TaskID。
func (s *Service) recordProviderFailure(id, opID, taskID, phase, group string, attempt int, started time.Time, cause error) error {
	var recorded *recordedProviderError
	if errors.As(cause, &recorded) {
		return cause
	}
	d := tripo.Describe(cause, phase)
	if d.OccurredAt.IsZero() {
		d.OccurredAt, d.DurationMS = time.Now().UTC(), time.Since(started).Milliseconds()
		var api *tripo.APIError
		d.RequestStarted = !errors.As(cause, &api)
	}
	d.RunID, d.OperationID, d.TaskID = id, opID, taskID
	d.AttemptID, d.RetryGroupID, d.Attempt = newID(), group, attempt
	d = d.Safe(s.Config.DeepSeekKey, s.Config.TripoKey)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.store.Edit(ctx, id, func(v *Session) error {
		if v.Current == nil || v.Current.ID != opID {
			return ErrClosed
		}
		if v.Current.PreparedInput != nil {
			d = d.Safe(v.Current.PreparedInput.Token)
		}
		// 已结束记录的正式结果不能被迟到诊断改写；事件仍可追加证据。
		if !v.Terminal() {
			v.Current.LastFailure = &d
		}
		return nil
	}, "provider_call_failed", &d)
	slog.Warn("Tripo 调用失败", "diagnostic", d, "recorded", err == nil)
	if err != nil {
		slog.Error("保存 Tripo 诊断失败，生产状态未回退", "run_id", id, "operation_id", opID, "attempt_id", d.AttemptID)
	}
	return &recordedProviderError{tripo.WithDiagnostic(cause, d)}
}

// resolveProviderFailure 只清理当前错误摘要，历史事件保持不变。
func (s *Service) resolveProviderFailure(ctx context.Context, id, opID, phase string) {
	v, err := s.store.Get(ctx, id)
	if err != nil || v.Terminal() || v.Current == nil || v.Current.ID != opID || v.Current.LastFailure == nil || v.Current.LastFailure.Phase != phase {
		return
	}
	_, err = s.store.Edit(ctx, id, func(x *Session) error {
		if x.Terminal() || x.Current == nil || x.Current.ID != opID {
			return ErrClosed
		}
		if x.Current.LastFailure != nil && x.Current.LastFailure.Phase == phase {
			x.Current.LastFailure = nil
		}
		return nil
	}, "provider_call_recovered", map[string]string{"operation_id": opID, "phase": phase})
	if err != nil {
		slog.Error("保存调用恢复诊断失败", "run_id", id, "operation_id", opID)
	}
}

// providerFailureText 只根据尚未解决的结构化错误生成说明，不读取外部错误正文。
func providerFailureText(s Session) string {
	if s.Status == "completed" || s.Current == nil || s.Current.LastFailure == nil {
		return ""
	}
	d := s.Current.LastFailure.Safe()
	if d.OperationID != s.Current.ID || d.RunID != s.ID {
		return ""
	}
	if s.Current.ArtifactID != "" {
		return ""
	}
	text := d.Message
	if s.Current.Stage == "submitting" && s.Current.TaskID == "" {
		return text + "本次本地执行已停止；无法确认远端是否已经创建任务，系统没有自动重试。"
	}
	if d.Phase == "download" {
		return "远端任务已成功；" + text + "尚未完成本地交付。"
	}
	return text
}
