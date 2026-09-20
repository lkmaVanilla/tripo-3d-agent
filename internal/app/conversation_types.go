package app

import (
	"errors"
	"time"

	"github.com/lkmaVanilla/tripo-3d-agent/internal/asset"
)

var (
	ErrConversationBusy    = errors.New("execution_busy：当前执行尚未退出，请保留草稿")
	ErrConversationExpired = errors.New("conversation_expired：会话不存在或已经到期")
	ErrMessageConflict     = errors.New("message_conflict：提交身份已经用于不同内容")
	ErrInvalidVersion      = errors.New("invalid_version：只能引用本会话的模型版本")
)

// Conversation 保存资产会话的生命周期；ActiveRunID 在停止的 worker 退出前仍保留。
// Owner 仅用于服务端访问校验，不进入网页和导出。
type Conversation struct {
	ID          string    `json:"id"`
	Owner       string    `json:"-"`
	Title       string    `json:"title"`
	ActiveRunID string    `json:"active_run_id,omitempty"`
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
	Expires     time.Time `json:"expires"`
	Cleaning    bool      `json:"-"`
}

// Available 不让旧执行的到期时间覆盖整个会话的有效期；清理标记优先于活动指针。
func (c Conversation) Available(now time.Time) bool {
	return !c.Cleaning && (c.ActiveRunID != "" || c.Expires.IsZero() || now.Before(c.Expires))
}

// ConversationMessage 是持久化的公开业务消息，不是 Eino 消息历史。
// Seq 表示初次出现的位置，UpdatedSeq 表示同一制作卡最后一次被事件更新的位置。
type ConversationMessage struct {
	ID             string         `json:"id"`
	ConversationID string         `json:"conversation_id"`
	RunID          string         `json:"run_id"`
	Kind           string         `json:"kind"`
	Text           string         `json:"text,omitempty"`
	VersionID      string         `json:"version_id,omitempty"`
	WaitID         string         `json:"wait_id,omitempty"`
	Seq            int64          `json:"seq"`
	UpdatedSeq     int64          `json:"updated_seq"`
	Created        time.Time      `json:"created"`
	Data           map[string]any `json:"data,omitempty"`
}

// AssetVersion 的身份和来源不可变；Path 与 SourceIntent 使用单独私有数据库列保存。
// 旧记录没有充分父关系证据时标为 historical_unknown，而不按编号猜测加工链。
type AssetVersion struct {
	ID                 string       `json:"id"`
	ConversationID     string       `json:"conversation_id"`
	Number             int          `json:"version_number"`
	SourceRunID        string       `json:"source_run_id"`
	SourceOperationID  string       `json:"source_operation_id"`
	TaskID             string       `json:"task_id"`
	OperationKind      string       `json:"operation_kind"`
	ParentVersionID    string       `json:"parent_version_id,omitempty"`
	ContextReferenceID string       `json:"context_reference_id,omitempty"`
	Path               string       `json:"-"`
	SHA256             string       `json:"sha256"`
	Bytes              int64        `json:"bytes"`
	Format             string       `json:"format"`
	Report             asset.Report `json:"report"`
	SourceIntent       *Intent      `json:"-"`
	Created            time.Time    `json:"created"`
	ProvenanceStatus   string       `json:"provenance_status"`
	URL                string       `json:"url"`
	Processable        bool         `json:"processable"`
}

// ConversationEvent 保留原全局事件序号，并明确它属于哪个执行。
type ConversationEvent struct {
	Event
	ConversationID string `json:"conversation_id"`
	RunID          string `json:"run_id"`
}

// ConversationSnapshot 在同一读取事务中返回消息、版本、执行和事件水位。
type ConversationSnapshot struct {
	Conversation Conversation          `json:"conversation"`
	Messages     []ConversationMessage `json:"messages"`
	Versions     []AssetVersion        `json:"versions"`
	Runs         []map[string]any      `json:"runs"`
	Events       []ConversationEvent   `json:"events"`
	Cursor       int64                 `json:"cursor"`
	HasMore      bool                  `json:"has_more"`
}
