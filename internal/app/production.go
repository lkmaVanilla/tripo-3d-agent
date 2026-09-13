package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lkmaVanilla/tripo-3d-agent/internal/asset"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
)

// errOperationRecorded 从 Store.Edit 返回时同时回滚更新和事件，避免重复发布完成事实。
var errOperationRecorded = errors.New("生产结果已保存")

// errMissingModelOutput 只标识查询响应缺少地址这一观测，不诊断供应商或参数原因。
var errMissingModelOutput = errors.New("成功任务缺少模型文件地址，无法检查产物")

// checkOperation 同时校验最新执行边界和操作身份，防止旧工具回调推进另一操作。
func checkOperation(v Session, opID string) error {
	if err := checkExecution(v, time.Now()); err != nil {
		return err
	}
	if v.Current == nil || v.Current.ID != opID {
		return fmt.Errorf("生产操作检查点不匹配")
	}
	return nil
}

// liveOperation 在网络或文件副作用前重新读取数据库，不信任工具恢复时的旧会话快照。
func (s *Service) liveOperation(ctx context.Context, id, opID string) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	v, err := s.store.Get(ctx, id)
	if err != nil {
		return v, err
	}
	return v, checkOperation(v, opID)
}

// production 执行固定的提交、轮询、下载与技术检查流程，并将确定结果交还 Agent。
// 模型选择生产动作，异步状态机由 Go 推进；恢复时始终复用原操作和任务身份。
func (s *Service) production(ctx context.Context, id, opID string) (string, error) {
	v, err := s.liveOperation(ctx, id, opID)
	if err != nil {
		return "", err
	}
	op := *v.Current
	// 工具响应之后、下个检查点之前退出时，重复执行只返回已保存结果。
	if op.Stage == "done" {
		return s.operationResult(v, op), nil
	}
	// 数据库事务无法与远端提交原子完成；缺少任务 ID 时宁可停止，也不能盲目重发。
	if op.Stage == "submitting" && op.TaskID == "" {
		return "", fmt.Errorf("提交结果未知，不能重新提交")
	}
	if op.Stage != "ready" && op.Stage != "submitting" && op.Stage != "submitted" {
		return "", fmt.Errorf("生产操作阶段无效")
	}
	if v.Intent == nil {
		return "", fmt.Errorf("缺少已确认意图，无法进行技术检查")
	}
	// 先扣生产次数并写 submitting，再访问提供方；失败和未知结果都保留这次消耗。
	if op.Stage == "ready" {
		if op.InputVersionID != "" {
			params, e := s.prepareVersionInput(ctx, id, opID)
			if e != nil {
				return s.operationFailure(id, opID, e)
			}
			op.Params = params
		}
		v, err = s.store.Edit(ctx, id, func(x *Session) error {
			if e := checkOperation(*x, opID); e != nil {
				return e
			}
			if x.Current.Stage != "ready" || x.Current.TaskID != "" {
				return fmt.Errorf("生产操作已推进，不能重复提交")
			}
			if !x.HasSlot {
				return fmt.Errorf("尚未获得生产名额")
			}
			if x.Production >= x.Limits.Submissions || x.ModelCalls >= x.Limits.Calls {
				return ErrBudget
			}
			x.Production++
			// 整个请求只在首次提交时开始计时，纠偏与恢复不会获得新的执行窗口。
			if x.Deadline.IsZero() {
				x.Deadline = time.Now().UTC().Add(x.Limits.Duration)
			}
			x.Current.Stage = "submitting"
			if x.Current.InputVersionID != "" {
				p := op.Params
				x.Current.SubmissionParams = &p
			}
			return nil
		}, "tool_submitting", map[string]any{"operation_id": op.ID, "kind": op.Kind, "params": op.Params})
		if err != nil {
			return "", err
		}
	}
	opCtx, cancel := context.WithCancel(ctx)
	if !v.Deadline.IsZero() {
		cancel()
		opCtx, cancel = context.WithDeadline(ctx, v.Deadline)
	}
	defer cancel()
	// op 保留进入函数时的阶段：只有本次从 ready 推进的执行者可以发送首次提交。
	if op.Stage == "ready" {
		if v, err = s.liveOperation(opCtx, id, opID); err != nil {
			return "", err
		}
		if v.Current.Stage != "submitting" || v.Current.TaskID != "" {
			return "", fmt.Errorf("生产操作已推进，不能重复提交")
		}
		taskID, e := s.provider.Submit(opCtx, op.Kind, op.Params)
		if e != nil {
			if tripo.IsUnknown(e) {
				return "", fmt.Errorf("提交结果未知，保留已消耗额度并停止自动执行")
			}
			return s.operationFailure(id, op.ID, e)
		}
		if taskID == "" {
			return "", fmt.Errorf("提交结果未知，未收到任务 ID，不能重新提交")
		}
		// 停止或超时后才到达的任务 ID 仍是远端提交证据，使用独立上下文保存。
		// 只补录操作证据，不重置会话状态、额度、截止时间或生产名额。
		v, err = s.store.Edit(context.Background(), id, func(x *Session) error {
			if x.Current == nil || x.Current.ID != opID {
				return fmt.Errorf("提交响应与当前操作不匹配")
			}
			if x.Current.TaskID == taskID {
				return errOperationRecorded
			}
			if x.Current.Stage != "submitting" || x.Current.TaskID != "" {
				return fmt.Errorf("提交响应不能覆盖已记录的任务")
			}
			x.Current.TaskID = taskID
			x.Current.Stage = "submitted"
			return nil
		}, "tool_submitted", map[string]string{"operation_id": op.ID, "task_id": taskID})
		if err != nil && !errors.Is(err, errOperationRecorded) {
			return "", err
		}
		if v, err = s.liveOperation(opCtx, id, opID); err != nil {
			return "", err
		}
		op = *v.Current
	}
	if op.TaskID == "" {
		return "", fmt.Errorf("已提交操作缺少任务 ID，不能重新提交")
	}
	// 轮询重试只查询原任务；进度去重限定在本次执行，重启后允许再次记录观察事件。
	lastProgress, lastStatus := -1, ""
	for {
		var task tripo.Task
		err = retry(opCtx, func() error {
			if _, e := s.liveTask(opCtx, id, opID, op.TaskID); e != nil {
				return e
			}
			var e error
			task, e = s.provider.Query(opCtx, op.TaskID)
			return e
		})
		if err != nil {
			return "", err
		}
		// 仅接受当前操作的任务结果，避免错误响应被当作本请求的生产证据。
		if task.ID != "" && task.ID != op.TaskID {
			return "", fmt.Errorf("查询结果与原任务 ID 不匹配")
		}
		if v, err = s.liveOperation(opCtx, id, opID); err != nil {
			return "", err
		}
		if v.Current.Stage == "done" {
			return s.operationResult(v, *v.Current), nil
		}
		if task.Progress != lastProgress || task.Status != lastStatus {
			_, err = s.store.Edit(opCtx, id, func(x *Session) error { return checkOperation(*x, opID) }, "tripo_progress", map[string]any{"operation_id": op.ID, "task_id": op.TaskID, "status": task.Status, "progress": task.Progress, "credits": task.Credits})
			if err != nil {
				return "", err
			}
			lastProgress, lastStatus = task.Progress, task.Status
		}
		switch task.Status {
		case "queued", "running":
			if err = wait(opCtx, s.Config.PollInterval); err != nil {
				return "", err
			}
		case "failed", "cancelled":
			return s.operationFailure(id, op.ID, fmt.Errorf("Tripo 任务 %s（code %d）：%s", task.Status, task.ErrorCode, s.redact(task.ErrorMessage)))
		case "success":
			// 远端 success 只代表任务完成；下载并通过技术检查后才有交付依据。
			if task.Output.ModelURL == "" {
				return s.operationFailure(id, op.ID, errMissingModelOutput)
			}
			var data []byte
			err = retry(opCtx, func() error {
				if _, e := s.liveTask(opCtx, id, opID, op.TaskID); e != nil {
					return e
				}
				var e error
				data, e = s.provider.Download(opCtx, task.Output.ModelURL)
				return e
			})
			if err != nil {
				if opCtx.Err() != nil {
					return "", opCtx.Err()
				}
				return s.operationFailure(id, op.ID, err)
			}
			if v, err = s.liveOperation(opCtx, id, opID); err != nil {
				return "", err
			}
			if v.Current.Stage == "done" {
				return s.operationResult(v, *v.Current), nil
			}
			// 检查使用已保存意图的上限，不接受生成参数或模型解释放宽验收标准。
			report := asset.Inspect(data, v.Intent.MaxTriangles, v.Intent.MaxBytes)
			a := Artifact{ID: op.ID, TaskID: op.TaskID, SourceURL: task.Output.ModelURL, Report: report}
			dir := filepath.Join(s.Config.DataDir, "artifacts", id)
			if _, err = s.liveOperation(opCtx, id, opID); err != nil {
				return "", err
			}
			if err = os.MkdirAll(dir, 0700); err != nil {
				return "", err
			}
			// 同一操作总是写同一路径：文件落盘后、数据库提交前退出可安全重新下载。
			a.Path = filepath.Join(dir, a.ID+".glb")
			if _, err = s.liveOperation(opCtx, id, opID); err != nil {
				return "", err
			}
			if err = atomicWrite(a.Path, data); err != nil {
				return "", err
			}
			return s.saveOperationArtifact(opCtx, id, opID, a)
		default:
			return s.operationFailure(id, op.ID, fmt.Errorf("Tripo 返回未知任务状态"))
		}
	}
}

// liveTask 在操作身份之外核对已知任务 ID，用于每次可重试的查询和下载。
func (s *Service) liveTask(ctx context.Context, id, opID, taskID string) (Session, error) {
	v, err := s.liveOperation(ctx, id, opID)
	if err == nil && (v.Current.TaskID != taskID || taskID == "") {
		err = fmt.Errorf("任务 ID 与当前操作不匹配")
	}
	return v, err
}

// saveOperationArtifact 原子保存产物、操作完成状态和报告事件；重复完成不追加记录。
// 文件已在事务前写好，这里通过操作 ID 与任务 ID 绑定其业务归属。
func (s *Service) saveOperationArtifact(ctx context.Context, id, opID string, a Artifact) (string, error) {
	v, err := s.store.Edit(ctx, id, func(x *Session) error {
		if e := checkOperation(*x, opID); e != nil {
			return e
		}
		if x.Current.Stage == "done" {
			return errOperationRecorded
		}
		if a.ID != opID || a.TaskID == "" || a.TaskID != x.Current.TaskID {
			return fmt.Errorf("产物与原生产操作不匹配")
		}
		for _, saved := range x.Artifacts {
			if saved.ID == a.ID {
				return fmt.Errorf("产物已保存但操作状态不一致")
			}
		}
		x.Artifacts = append(x.Artifacts, a)
		x.Current.Stage = "done"
		x.Current.ArtifactID = a.ID
		return nil
	}, "technical_report", map[string]any{"operation_id": opID, "task_id": a.TaskID, "artifact_id": a.ID, "report": a.Report})
	if err != nil && !errors.Is(err, errOperationRecorded) {
		return "", err
	}
	return s.operationResult(v, *v.Current), nil
}

// operationFailure 保存一次操作的确定失败，作为工具反馈供 Agent 判断能否纠偏。
// 取消和超时直接交给 Runtime 收尾，不能伪装成允许继续生产的普通失败。
func (s *Service) operationFailure(id, opID string, cause error) (string, error) {
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return "", cause
	}
	v, err := s.store.Edit(context.Background(), id, func(x *Session) error {
		if e := checkOperation(*x, opID); e != nil {
			return e
		}
		if x.Current.Stage == "done" {
			return errOperationRecorded
		}
		if executionVersion(*x) == ConversationPromptVersion && x.Current.Stage == "ready" && x.Current.InputVersionID != "" {
			x.Current.ErrorCode = "input_preparation_failed"
		}
		if executionVersion(*x) == ConversationPromptVersion && errors.Is(cause, errMissingModelOutput) {
			x.Current.ErrorCode = "missing_model_output"
		}
		x.Current.Stage = "done"
		x.Current.Error = s.redact(cause.Error())
		return nil
	}, "tool_failed", map[string]string{"operation_id": opID, "error": s.redact(cause.Error())})
	if err != nil && !errors.Is(err, errOperationRecorded) {
		return "", err
	}
	return s.operationResult(v, *v.Current), nil
}

// operationResult 重用固定产物或错误证据，但始终根据最新 Session 计算剩余预算。
// 不能缓存带旧余额的整段工具响应，否则恢复后模型会看到已过期的额度。
func (s *Service) operationResult(v Session, op Operation) string {
	for _, a := range v.Artifacts {
		if a.ID == op.ArtifactID && a.ID == op.ID && a.TaskID == op.TaskID {
			return jsonString(map[string]any{"artifact_id": a.ID, "task_id": a.TaskID, "report": a.Report, "remaining_submissions": v.Limits.Submissions - v.Production})
		}
	}
	message := op.Error
	if message == "" {
		message = "操作缺少可验证的产物报告"
	}
	result := map[string]any{"error": message, "remaining_submissions": v.Limits.Submissions - v.Production}
	if executionVersion(v) == ConversationPromptVersion && op.ErrorCode != "" {
		result["error_code"], result["input_version_id"] = op.ErrorCode, op.InputVersionID
		if evidence := conversationFailureEvidence(op); evidence != nil {
			result["failure_evidence"] = evidence
		}
	}
	return jsonString(result)
}

// recoverKnownTask 在模型额度或检查点不可用时，直接查询并检查已知任务，不新增 Agent 决策。
// 调用方必须先协调后续待提交暂停；此路径仍受生产证据、终态和原截止时间约束。
func (s *Service) recoverKnownTask(ctx context.Context, id string) error {
	v, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if v.Current == nil || v.Current.TaskID == "" || v.Current.Stage == "ready" {
		return fmt.Errorf("确定性恢复需要已经提交的任务 ID")
	}
	opID := v.Current.ID
	if _, err = s.production(ctx, id, opID); err != nil {
		return err
	}
	_, err = s.store.Edit(ctx, id, func(x *Session) error {
		if e := checkOperation(*x, opID); e != nil {
			return e
		}
		// 旧任务完成不代表后续已接受提议可以被丢弃，仍可恢复的草案必须先处理。
		if hasRecoverablePending(*x) {
			return fmt.Errorf("原任务已检查，仍需处理待提交暂停")
		}
		if x.Current.Stage != "done" {
			return fmt.Errorf("原任务尚无确定结果")
		}
		// 只能依据原任务的实测报告交付；技术未通过时结束，不再自动追加纠偏。
		for _, a := range x.Artifacts {
			if a.ID == x.Current.ArtifactID && a.ID == opID && a.TaskID == x.Current.TaskID {
				if deliverable(*x, a.ID) {
					x.SelectedArtifact = a.ID
					return x.finishVerified("completed", "recovery", "known_task_recovery")
				} else {
					return x.finishVerified("failed", "recovery", "known_task_recovery")
				}
			}
		}
		reason := "evidence_unavailable"
		if x.Current.Error != "" {
			reason = "operation_failed"
		}
		return x.finishVerified("failed", "recovery", reason)
	}, "runtime_finished", map[string]string{"reason": "known_task_recovery", "operation_id": opID})
	return err
}
