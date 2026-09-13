package app

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/schema"
)

// recoveryVersion 标识应用层恢复协议；与 Eino 自身检查点的编码版本无关。
const recoveryVersion = 1

// ResumePoint 将 Eino 检查点字节绑定到一次已提交的业务暂停。
// SessionID/Key 隔离会话，PauseID 标识逻辑暂停，RefID 指向问题或生产操作。
// Generation 在新检查点提交时递增，用于拒绝旧 Runner 的写入；重复确认同一提交不推进代次。
// Digest 校验检查点内容，只有 Eino 完成编码后才能计算，不放入编码前的中断令牌。
type ResumePoint struct {
	Version                                      int
	SessionID, PauseID, Kind, RefID, Key, Digest string
	Generation                                   int64
}

// PendingPause 是 prepare 阶段持久化的草案，不代表问题已发布或生产已获准执行。
// 只有检查点与业务状态在同一事务内提交后，草案才转为 Session.ResumePoint。
type PendingPause struct {
	Point ResumePoint
	// ExpectedGeneration 是准备草案时观察到的已提交代次，提交时必须仍然匹配。
	ExpectedGeneration int64
	Question, Reason   string
	// DraftHash 绑定暂停身份、问题、操作与 Seed 决策 ID，防止混用不同草案。
	DraftHash string
	Operation *Operation
	// Seed 足以重建首次检查点；已有暂停重新编码时可为空，但必须保留旧检查点。
	Seed *ReplaySeed
	// Existing 表示业务暂停早已生效，重新编码不得再次计数、入队或清除已接受答案。
	// Legacy 标记旧记录迁移，用于区分恢复来源，不赋予绕过身份校验的权限。
	Existing, Legacy bool
}

// ReplaySeed 冻结一次已经接受的模型提议，用于重建暂停，而非让模型重新做决定。
// Input/Response 保留完整公开消息（包括推理内容），哈希防止恢复时悄悄改变消息或参数。
// DecisionID 由服务端生成；ToolCallID 只关联本次决策内的工具调用，不单独充当全局身份。
// Model/PromptVersion 约束重放环境，避免用不同模型或提示词解释旧提议。
type ReplaySeed struct {
	Version                                         int
	DecisionID, ToolCallID, ToolName, ArgumentsHash string
	InputHash, ResponseHash, Model, PromptVersion   string
	Input                                           []*schema.Message
	Response                                        *schema.Message
}

// AnswerRecord 是按问题 ID 留存的已接受答案；展示字段清空后仍可供旧检查点重放。
type AnswerRecord struct {
	Text     string
	Accepted time.Time
}

// generation 将尚无已提交恢复点的会话视为第 0 代，首个检查点从第 1 代开始。
func (v Session) generation() int64 {
	if v.ResumePoint == nil {
		return 0
	}
	return v.ResumePoint.Generation
}

// checkExecution 检查终态和有效期，不负责预算扣减。
// 调用方必须传入最新持久化状态，不能用检查点内的旧快照覆盖用户停止或到期事实。
func checkExecution(v Session, now time.Time) error {
	if v.Terminal() {
		return ErrClosed
	}
	if !v.Deadline.IsZero() && !now.Before(v.Deadline) {
		return context.DeadlineExceeded
	}
	if v.Deadline.IsZero() && now.Sub(v.LastUser) >= v.Limits.Idle {
		return fmt.Errorf("生产前连续无用户操作，等待已到期")
	}
	return nil
}
