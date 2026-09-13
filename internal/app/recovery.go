package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
)

// pauseCoordinator 由一次 Runner 执行独占，连接工具的 prepare 与 Eino 的检查点回调。
// 工具子 context 中的值不会向上传播到 Runner，因此两端通过同一个协调器共享暂停草案。
// expected 固定本次执行起始代次；业务计数、停止状态等仍以提交时的数据库记录为准。
// mode 中 normal 允许正常决策，rebuild 仅重放种子，verify/legacy 仅验证新/旧检查点并重新暂停。
type pauseCoordinator struct {
	s                             *Service
	id, mode                      string
	expected                      int64
	profile                       executionProfile
	seed                          *ReplaySeed
	pause                         *PendingPause
	resuming, replayed, committed bool
}

// draftHash 将业务提议绑定到暂停身份；Seed 的完整消息另有独立哈希校验。
func draftHash(p *PendingPause) string {
	return tokenHash(jsonString(struct {
		Point            ResumePoint
		Question, Reason string
		Operation        *Operation
		SeedID           string
	}{p.Point, p.Question, p.Reason, p.Operation, func() string {
		if p.Seed != nil {
			return p.Seed.DecisionID
		}
		return ""
	}()}))
}

// validatePending 证明草案属于当前会话的下一代，并与已接受的模型提议相符。
// 已验证暂停的再次编码允许没有 Seed；这种草案只能依靠原检查点恢复，不能凭空重建。
func validatePending(v Session) error {
	if _, err := resolveExecutionProfile(v); err != nil {
		return err
	}
	p := v.PendingPause
	if p == nil || p.Point.SessionID != v.ID || p.Point.Key != v.ID || p.Point.Generation != v.generation()+1 || p.ExpectedGeneration != v.generation() || p.Point.Version != recoveryVersion || p.Point.PauseID == "" || p.Point.RefID == "" || p.DraftHash != draftHash(p) {
		return recoveryError("recovery_seed_invalid")
	}
	if p.Point.Kind != "question" && p.Point.Kind != "production" {
		return recoveryError("recovery_seed_invalid")
	}
	if p.Existing && p.Seed == nil {
		return nil
	}
	if err := validateReplaySeed(p.Seed, executionVersion(v)); err != nil {
		return err
	}
	if p.Seed.Model != v.Model {
		return recoveryError("recovery_seed_invalid")
	}
	if p.Point.Kind == "question" {
		var input questionInput
		if p.Seed.ToolName != "ask_user" || json.Unmarshal([]byte(p.Seed.Response.ToolCalls[0].Function.Arguments), &input) != nil || input.Question != p.Question || p.Question == "" {
			return recoveryError("recovery_seed_invalid")
		}
	} else {
		kind, params, reason, err := seedProduction(v, p.Seed)
		if err != nil {
			return err
		}
		if p.Operation == nil || p.Operation.ID != p.Point.RefID || p.Operation.Kind != kind || p.Operation.Params != params || p.Operation.Stage != "ready" || p.Operation.TaskID != "" || p.Reason != reason {
			return recoveryError("recovery_seed_invalid")
		}
	}
	return nil
}

// hasRecoverablePending 区分可独立重建的草案与仍依赖旧检查点的兼容草案。
func hasRecoverablePending(v Session) bool {
	if v.PendingPause == nil || validatePending(v) != nil {
		return false
	}
	// 没有种子的兼容草案依赖旧检查点；该路径已经失败时，应转向已知任务续查。
	return !(v.PendingPause.Existing && v.PendingPause.Seed == nil && v.RecoveryFailure != "")
}

// seedProduction 用已保存的提议还原生产参数，并重新核对当前意图和可用资产。
// 此函数只做确定性转换与检查，不调用模型或 Tripo，也不消耗生产预算。
func seedProduction(v Session, seed *ReplaySeed) (string, tripo.Params, string, error) {
	var params tripo.Params
	var reason, kind string
	if v.Intent == nil {
		return kind, params, reason, recoveryError("recovery_seed_invalid")
	}
	switch seed.ToolName {
	case "generate_asset":
		var in generationInput
		if json.Unmarshal([]byte(seed.Response.ToolCalls[0].Function.Arguments), &in) != nil {
			return kind, params, reason, recoveryError("recovery_seed_invalid")
		}
		kind, reason = "generate", in.Reason
		params = tripo.Params{Prompt: in.Prompt, FaceLimit: in.TargetTriangles, TextureQuality: in.TextureQuality}
		if len(v.Intent.Constraints) > 0 {
			params.Prompt += "\nRequired: " + strings.Join(v.Intent.Constraints, "; ")
		}
	case "decimate_asset":
		var in decimationInput
		if json.Unmarshal([]byte(seed.Response.ToolCalls[0].Function.Arguments), &in) != nil {
			return kind, params, reason, recoveryError("recovery_seed_invalid")
		}
		kind, reason = "decimate", in.Reason
		params.FaceLimit = in.TargetTriangles
		for _, a := range v.Artifacts {
			if a.ID == in.ArtifactID && a.Report.Valid && in.TargetTriangles < a.Report.Triangles {
				params.Input = a.SourceURL
			}
		}
		if params.Input == "" {
			return kind, params, reason, recoveryError("recovery_seed_invalid")
		}
	default:
		return kind, params, reason, recoveryError("recovery_seed_invalid")
	}
	if params.TextureQuality == "" {
		params.TextureQuality = "standard"
	}
	if params.FaceLimit < 500 || params.FaceLimit > 20000 || params.FaceLimit > v.Intent.MaxTriangles {
		return kind, params, reason, recoveryError("recovery_seed_invalid")
	}
	return kind, params, reason, nil
}

// prepare 先保存可恢复的草案，暂不发布澄清问题、替换 Current 或分配生产名额。
// 即使首次 Eino 检查点尚未生成就退出，也能通过种子重建同一提议。
func (c *pauseCoordinator) prepare(ctx context.Context, p *PendingPause) error {
	if c.mode == "rebuild" {
		// 重建工具只能复用已保存草案，不接受本次工具临时生成的新问题或操作 ID。
		v, err := c.s.store.Get(ctx, c.id)
		if err != nil {
			return err
		}
		if err = validatePending(v); err != nil {
			return err
		}
		if v.PendingPause.Point.Kind != p.Point.Kind || c.seed == nil || compose.GetToolCallID(ctx) != c.seed.ToolCallID {
			return recoveryError("recovery_seed_invalid")
		}
		c.pause = v.PendingPause
		return nil
	}
	if !p.Existing {
		if err := validateReplaySeed(c.seed); err != nil {
			return err
		}
		if compose.GetToolCallID(ctx) != c.seed.ToolCallID {
			return recoveryError("recovery_seed_invalid")
		}
		p.Seed = c.seed
	}
	p.ExpectedGeneration = c.expected
	p.Point.Version, p.Point.SessionID, p.Point.Key, p.Point.Generation = recoveryVersion, c.id, c.id, c.expected+1
	if p.Point.PauseID == "" {
		p.Point.PauseID = newID()
	}
	p.DraftHash = draftHash(p)
	_, err := c.s.store.Edit(ctx, c.id, func(v *Session) error {
		if err := checkExecution(*v, time.Now()); err != nil {
			return err
		}
		// 一个会话同一时刻只允许一个待提交暂停，避免旧 Runner 覆盖新提议。
		if v.generation() != c.expected || v.PendingPause != nil {
			return recoveryError("checkpoint_identity_mismatch")
		}
		v.PendingPause = p
		return validatePending(*v)
	}, "", nil)
	if err == nil {
		c.pause = p
	}
	return err
}

// interrupt 只把公开的暂停身份编码为字符串交给 Eino 保存。
// 应用不依赖框架私有序列化结构，恢复时由 Eino 解码，再由 restored 核对身份。
func (c *pauseCoordinator) interrupt(ctx context.Context, info string) error {
	if c.pause == nil {
		return recoveryError("checkpoint_identity_mismatch")
	}
	return tool.StatefulInterrupt(ctx, info, jsonString(c.pause.Point))
}

// Set 实现 Eino 的检查点写入接口，是暂停真正对业务生效的提交入口。
// 编码已由 Eino 完成；这里在同一事务中保存字节、业务状态和事件，不执行外部请求。
func (c *pauseCoordinator) Set(ctx context.Context, key string, value []byte) error {
	if c.pause == nil || key != c.id {
		return recoveryError("checkpoint_identity_mismatch")
	}
	err := retryCheckpoint(ctx, func() error {
		// 固定按 Service.mu → 数据库的顺序获取资源，串行决定全局名额和排队状态。
		// CommitCheckpoint 的回调只能改事务内 Session，不能再进入 Store 获取单一连接。
		c.s.mu.Lock()
		defer c.s.mu.Unlock()
		all, err := c.s.store.List(ctx, "")
		if err != nil {
			return err
		}
		slots, queued := 0, 0
		for _, v := range all {
			if !v.Terminal() && v.HasSlot {
				slots++
			}
			if !v.Terminal() && v.Status == "queued" {
				queued++
			}
		}
		_, err = c.s.store.CommitCheckpoint(ctx, c.id, c.expected, c.pause.Point, value, func(v *Session) ([]checkpointEvent, error) {
			if err := validatePending(*v); err != nil {
				return nil, err
			}
			p := v.PendingPause
			if p.Point != c.pause.Point {
				return nil, recoveryError("checkpoint_identity_mismatch")
			}
			events := []checkpointEvent{}
			if p.Existing {
				// 已生效暂停仅更新恢复协议，保留并发接受的答案、计数和原排队位置。
				if p.Legacy {
					events = append(events, checkpointEvent{"checkpoint_rebuilt", map[string]any{"pause_id": p.Point.PauseID, "generation": p.Point.Generation, "source": "legacy"}})
				}
			} else if p.Point.Kind == "question" {
				if v.Current != nil || v.Clarifications >= v.Limits.Clarifications {
					return nil, recoveryError("checkpoint_constraint")
				}
				v.Question, v.WaitID, v.Answer = p.Question, p.Point.RefID, ""
				v.Clarifications++
				v.Status, v.ResumeRequested = "awaiting_answer", false
				events = append(events, checkpointEvent{"clarification", map[string]string{"question": p.Question}})
			} else {
				// 用事务内最新预算决定能否接受生产；旧检查点不能恢复已经花掉的额度。
				if v.Intent == nil || v.Production >= v.Limits.Submissions || v.ModelCalls >= v.Limits.Calls {
					return nil, ErrBudget
				}
				v.Current = p.Operation
				// 纠错操作沿用已有名额；新操作不能越过已有队列直接抢占空闲名额。
				v.HasSlot = v.HasSlot || (slots < c.s.Config.ProductionSlots && queued == 0)
				v.Status = "running"
				if !v.HasSlot {
					v.Status = "queued"
					if queued >= c.s.Config.QueueSize {
						v.Status = "queue_full"
					}
				}
				v.Queued = time.Now().UTC()
				v.ResumeRequested = v.HasSlot
				events = append(events, checkpointEvent{"runtime_accepted", map[string]any{"operation_id": p.Point.RefID, "kind": p.Operation.Kind, "reason": p.Reason, "state": v.Status, "target_triangles": p.Operation.Params.FaceLimit}})
			}
			if c.mode == "rebuild" {
				events = append(events, checkpointEvent{"checkpoint_rebuilt", map[string]any{"pause_id": p.Point.PauseID, "generation": p.Point.Generation, "source": "pending"}})
			}
			return events, nil
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("checkpoint_write_failed: %w", err)
	}
	c.committed = true
	return nil
}

// Get 在 Eino 解码前验证会话、内容及预期代次，防止恢复到其他会话或更旧的暂停。
// legacy 模式仅放宽旧记录缺少元数据这一点，工具恢复后仍须核对旧问题或操作 ID。
func (c *pauseCoordinator) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if key != c.id {
		return nil, false, recoveryError("checkpoint_identity_mismatch")
	}
	v, err := c.s.store.Get(ctx, c.id)
	if err != nil {
		return nil, false, err
	}
	if err = checkExecution(v, time.Now()); err != nil {
		return nil, false, err
	}
	r, err := c.s.store.LoadCheckpoint(ctx, c.id, key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if c.mode != "legacy" && (r.Point == nil || v.ResumePoint == nil || *r.Point != *v.ResumePoint || r.Point.Generation != c.expected) {
		return nil, false, recoveryError("checkpoint_identity_mismatch")
	}
	return r.Data, true, nil
}

// restored 在 Eino 恢复工具状态后，用最新业务记录核对中断令牌。
// 旧协议必须先证明原问题/操作身份，再重新中断并原子保存新协议，不能直接继续产生副作用。
func (c *pauseCoordinator) restored(ctx context.Context, state, kind, question string) (Session, string, error) {
	v, err := c.s.store.Get(ctx, c.id)
	if err != nil {
		return v, "", err
	}
	if err = checkExecution(v, time.Now()); err != nil {
		return v, "", err
	}
	if c.mode == "legacy" {
		ref := v.WaitID
		if kind == "production" && v.Current != nil {
			ref = v.Current.ID
		}
		if ref == "" || state != ref {
			return v, "", recoveryError("legacy_recovery_unavailable")
		}
		if v.PendingPause != nil && v.PendingPause.Existing {
			if err = validatePending(v); err != nil {
				return v, "", err
			}
			c.pause = v.PendingPause
			return v, "", c.interrupt(ctx, "重新保存已验证恢复点")
		}
		if kind == "question" {
			if v.Question != "" && v.Question != question {
				return v, "", recoveryError("legacy_recovery_unavailable")
			}
			if v.Answers == nil {
				v.Answers = map[string]AnswerRecord{}
			}
			if v.Answer != "" {
				v.Answers[ref] = AnswerRecord{Text: v.Answer, Accepted: v.LastUser}
			}
			if v.Answers[ref].Text == "" {
				// 只接受与工具调用和问题唯一匹配的历史结果，无法证明原答案时拒绝猜测。
				if answer := legacyAnswer(v.History, question, compose.GetToolCallID(ctx)); answer != "" {
					v.Answers[ref] = AnswerRecord{Text: answer, Accepted: v.LastUser}
				}
			}
			if v.Status != "awaiting_answer" && v.Answers[ref].Text == "" {
				return v, "", recoveryError("legacy_recovery_unavailable")
			}
			_, err = c.s.store.Edit(ctx, c.id, func(x *Session) error { x.Answers = v.Answers; return nil }, "", nil)
			if err != nil {
				return v, "", err
			}
		}
		p := &PendingPause{Point: ResumePoint{Kind: kind, RefID: ref}, Question: v.Question, Existing: true, Legacy: true}
		if err = c.prepare(ctx, p); err != nil {
			return v, "", err
		}
		return v, "", c.interrupt(ctx, "已验证旧恢复点，保存恢复协议")
	}
	var point ResumePoint
	if json.Unmarshal([]byte(state), &point) != nil || v.ResumePoint == nil {
		return v, "", recoveryError("checkpoint_identity_mismatch")
	}
	committed := *v.ResumePoint
	committed.Digest = "" // 中断令牌先于外层检查点编码生成，当时尚无完整字节可计算摘要。
	if point != committed || point.Kind != kind || point.Generation != c.expected {
		return v, "", recoveryError("checkpoint_identity_mismatch")
	}
	if kind == "question" && v.WaitID != point.RefID {
		return v, "", recoveryError("checkpoint_identity_mismatch")
	}
	if v.PendingPause != nil && v.PendingPause.Existing {
		if err = validatePending(v); err != nil {
			return v, "", err
		}
		c.pause = v.PendingPause
		return v, "", c.interrupt(ctx, "重新保存已验证恢复点")
	}
	return v, point.RefID, nil
}

// repause 为仍在等待答案或名额的同一逻辑暂停重新保存检查点。
// PauseID 保持不变、Generation 推进，但不会再次消耗澄清次数或改变排队时间。
func (c *pauseCoordinator) repause(ctx context.Context, v Session, info string) error {
	p := &PendingPause{Point: *v.ResumePoint, Question: v.Question, Existing: true}
	p.Point.Digest = ""
	if err := c.prepare(ctx, p); err != nil {
		return err
	}
	return c.interrupt(ctx, info)
}

// retryCheckpoint 仅对可能短暂的存储错误做最多三次有界尝试，间隔为 1 秒、2 秒。
// 身份冲突、硬约束和取消立即退出；继续重试不能使不安全的恢复变安全。
// save 必须具备幂等性，因为提交成功后收到错误也可能触发下一次尝试。
func retryCheckpoint(ctx context.Context, save func() error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = save(); err == nil {
			return nil
		}
		var rejected *recoveryFault
		if errors.As(err, &rejected) || errors.Is(err, ErrClosed) || errors.Is(err, ErrBudget) || errors.Is(err, ErrCheckpointMismatch) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if attempt < 2 {
			if e := wait(ctx, time.Duration(1<<attempt)*time.Second); e != nil {
				return e
			}
		}
	}
	return err
}
