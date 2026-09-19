package app

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/asset"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
)

// ErrClosed 表示会话已终止，恢复或迟到的回调不能重新推进执行。
var ErrClosed = errors.New("请求已结束")

// ErrBudget 表示新增模型调用或生产提交不能再消耗额度。
var ErrBudget = errors.New("请求调用额度已耗尽")

// Config 定义进程级配置；创建会话时会将执行上限快照到 Limits。
type Config struct {
	// 凭证保留在服务端配置中，不进入 Session.View 的公开数据。
	Listen, DataDir, DeepSeekKey, TripoKey string
	TripoBaseURL                           string
	SecureCookie                           bool
	// 生产名额与队列容量限制全局并发，其余三个值限制单个会话。
	ProductionSlots, QueueSize, MaxCalls, MaxSubmissions, MaxClarifications int
	// MaxDuration 从首次生产提交开始计算；IdleTTL 用于此前的无操作等待。
	MaxDuration, PollInterval, IdleTTL, Retention, VisitorTTL time.Duration
}

// DefaultConfig 给出静态道具 MVP 的并发、预算及数据保留默认值。
func DefaultConfig() Config {
	return Config{Listen: "127.0.0.1:8080", DataDir: "./data", TripoBaseURL: tripo.DefaultBaseURL, ProductionSlots: 3, QueueSize: 10, MaxCalls: 20, MaxSubmissions: 3, MaxClarifications: 3, MaxDuration: 30 * time.Minute, PollInterval: 3 * time.Second, IdleTTL: 24 * time.Hour, Retention: 7 * 24 * time.Hour, VisitorTTL: 30 * 24 * time.Hour}
}

// ConfigFromEnv 仅覆盖已开放的环境变量，其余执行策略沿用默认配置。
func ConfigFromEnv() (Config, error) {
	c := DefaultConfig()
	c.DeepSeekKey = os.Getenv("DEEPSEEK_API_KEY")
	c.TripoKey = os.Getenv("TRIPO_API_KEY")
	base, err := tripo.NormalizeBaseURL(os.Getenv("TRIPO_BASE_URL"))
	if err != nil {
		return c, err
	}
	c.TripoBaseURL = base
	if v := os.Getenv("LISTEN_ADDR"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("COOKIE_SECURE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return c, err
		}
		c.SecureCookie = b
	}
	return c, nil
}

// Intent 是模型提出、Go 校验后保存的需求与验收契约。
// 正常执行中保存后不再重新定义，生产和技术检查都依据同一份上限。
type Intent struct {
	Optional          *asset.AcceptanceLimits     `json:"-"`
	ConstraintSources map[string]ConstraintSource `json:"-"`
	ReductionMode     string                      `json:"-"`
	Asset             string                      `json:"asset" jsonschema:"description=静态道具的名称与关键特征"`
	Use               string                      `json:"use" jsonschema:"description=用户的资产用途"`
	Style             string                      `json:"style" jsonschema:"description=用户要求的风格"`
	Constraints       []string                    `json:"constraints" jsonschema:"description=必须保留的全部用户硬约束"`
	MaxTriangles      int                         `json:"max_triangles" jsonschema:"description=验收面数上限，用户未指定时为5000"`
	MaxBytes          int64                       `json:"max_bytes" jsonschema:"description=验收字节上限，用户未指定时为10485760"`
	Assumptions       []string                    `json:"assumptions" jsonschema:"description=明确列出采用的默认假设"`
	Plan              []string                    `json:"plan" jsonschema:"description=生产与验收计划"`
}

// Limits 固化会话创建时的预算，进程配置变化不会重置已有请求的额度。
type Limits struct {
	Calls, Submissions, Clarifications int
	Duration, Idle, Retention          time.Duration
}

// Artifact 保存已下载产物及实测报告；报告不包含视觉符合性判断。
type Artifact struct {
	ID     string `json:"id"`
	TaskID string `json:"task_id"`
	// Path 是本地文件位置，公开视图改用受会话访问控制的下载地址。
	Path string `json:"path"`
	// SourceURL 保留提供方地址，供后续减面操作引用原模型。
	SourceURL string       `json:"source_url"`
	Report    asset.Report `json:"report"`
}

// Operation 表示当前一次生产操作，ID 同时作为产物 ID，便于恢复时去重。
// Stage 按 ready → submitting → submitted → done 推进；明确失败也会进入 done。
type Operation struct {
	ID     string       `json:"id"`
	Kind   string       `json:"kind"`
	Params tripo.Params `json:"params"`
	Stage  string       `json:"stage"`
	// submitting 且 TaskID 为空代表提交结果未知，不能通过重发来猜测结果。
	TaskID    string `json:"task_id"`
	Error     string `json:"error,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
	// LastFailure 是当前未解决的安全摘要，逐次历史保存在执行事件中。
	LastFailure *tripo.Diagnostic `json:"last_failure,omitempty"`
	ArtifactID  string            `json:"artifact_id,omitempty"`
	// v3 的语义输入保持固定，临时上传凭证单独保存，避免重放时改变加工对象。
	InputVersionID     string         `json:"input_version_id,omitempty"`
	ContextReferenceID string         `json:"context_reference_id,omitempty"`
	InputSHA256        string         `json:"input_sha256,omitempty"`
	PreparedInput      *PreparedInput `json:"prepared_input,omitempty"`
	SubmissionParams   *tripo.Params  `json:"submission_params,omitempty"`
}

// Session 是一个资产请求的持久化业务事实，包含对话、执行和恢复状态。
// Eino 检查点恢复控制流，不能用其中的旧快照覆盖这里的预算与终止事实。
type Session struct {
	// Owner 关联匿名访客身份；Status 供界面展示，终止判断以 Ended 为准。
	ID, Owner, Request, Status string
	Intent                     *Intent
	// Question/Answer 为当前展示及兼容字段；可靠重放读取 Answers[WaitID]。
	Question, Answer, WaitID string
	// 已消耗计数不会随重启重置；生产明确失败或结果未知也保留已扣次数。
	Clarifications, Production, ModelCalls int
	Limits                                 Limits
	// Deadline 首次生产时固定，Queued 保留排队顺序，Expires 控制终止后的清理。
	Deadline, Created, LastUser, Ended, Expires, Queued time.Time
	// HasSlot 是请求持有的生产名额，纠偏期间保留；CheckpointReady 不能单独证明可恢复。
	HasSlot, CheckpointReady, ResumeRequested bool
	// 新暂停提交前保留旧 Current，避免恢复中的旧工具引用被新提议提前覆盖。
	Current   *Operation
	Artifacts []Artifact
	// History 保留完整 Eino 消息协议（含推理字段），不直接暴露到公开视图。
	History                 []*schema.Message
	Final, SelectedArtifact string
	Model, Source           string
	// 空版本仅兼容升级前会话；新请求固定创建时的提示与 Skill 配置。
	ExecutionVersion string
	// Result 保存程序收尾依据；历史自由 Final 不因缺省字段自动获得核验资格。
	Result *Result
	// 恢复版本、已提交恢复点和待提交草案共同区分“已发布暂停”与“准备中的暂停”。
	RecoverySchemaVersion int
	ResumePoint           *ResumePoint
	PendingPause          *PendingPause
	// 答案按问题身份保留至会话清理，工具读取一次后仍可从旧检查点重放。
	Answers         map[string]AnswerRecord
	RecoveryFailure string
	// 连续会话给 v3/v4 Run 增加上下文；v1/v2 View 和框架恢复输入不变。
	InputVersion        *AssetVersion  `json:",omitempty"`
	InputAssessment     *asset.Report  `json:",omitempty"`
	IntentDraft         *IntentDraft   `json:",omitempty"`
	ConversationContext map[string]any `json:",omitempty"`
	GoalKind            string         `json:",omitempty"`
	Outcome             *RunOutcome    `json:",omitempty"`
}

// Terminal 使用持久化结束时间判断终态，避免依赖可能扩展的状态字符串集合。
func (s Session) Terminal() bool { return !s.Ended.IsZero() }

// Finish 统一设置终态与保留期限、释放名额；调用方负责事务及幂等终止检查。
func (s *Session) Finish(status, reason string) {
	s.Status = status
	s.Final = reason
	s.Ended = time.Now().UTC()
	s.Expires = s.Ended.Add(s.Limits.Retention)
	s.HasSlot = false
	s.ResumeRequested = false
}

// View 显式构造供 HTTP/WebSocket 使用的业务视图，避免泄露本地路径和恢复材料。
func (s Session) View() map[string]any {
	s = publicResult(s)
	arts := make([]map[string]any, 0, len(s.Artifacts))
	for _, a := range s.Artifacts {
		arts = append(arts, map[string]any{"id": a.ID, "task_id": a.TaskID, "report": a.Report, "url": "/api/sessions/" + s.ID + "/artifacts/" + a.ID})
	}
	return map[string]any{"id": s.ID, "request": s.Request, "status": s.Status, "intent": s.Intent, "question": s.Question, "clarifications": s.Clarifications, "production": s.Production, "model_calls": s.ModelCalls, "max_submissions": s.Limits.Submissions, "max_model_calls": s.Limits.Calls, "deadline": s.Deadline, "created": s.Created, "ended": s.Ended, "expires": s.Expires, "artifacts": arts, "selected_artifact": s.SelectedArtifact, "final": s.Final, "result": s.Result, "model": s.Model, "prompt_version": executionVersion(s), "evaluation": evaluate(s)}
}

// newID 使用随机字节生成不含业务含义的标识，供会话、暂停和操作使用。
func newID() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Evaluation 记录单次执行的程序证据核验，不代表正式 Agent 评测结果。
type Evaluation struct {
	Scope  string        `json:"scope"`
	Checks []asset.Check `json:"checks"`
}

// evaluate 核验硬预算和交付证据；只有宣告交付时才要求选中产物检查通过。
func evaluate(s Session) Evaluation {
	s = publicResult(s)
	status := func(b bool) string {
		if b {
			return "passed"
		}
		return "failed"
	}
	e := Evaluation{Scope: "仅核验本次执行的程序证据；不代替 20 案例 × 3 次 Agent 评测。", Checks: []asset.Check{{Name: "生产次数", Status: status(s.Production <= s.Limits.Submissions), Detail: strconv.Itoa(s.Production)}, {Name: "模型调用额度", Status: status(s.ModelCalls <= s.Limits.Calls), Detail: strconv.Itoa(s.ModelCalls)}, {Name: "澄清轮数", Status: status(s.Clarifications <= s.Limits.Clarifications), Detail: strconv.Itoa(s.Clarifications)}}}
	verdict := "pending"
	detail := "尚未交付"
	if s.Status == "completed" {
		verdict = "failed"
		detail = "缺少通过检查的交付证据"
		if deliverable(s, s.SelectedArtifact) {
			verdict = "passed"
			detail = "交付引用已实测且技术通过的模型"
		}
	} else if s.Terminal() {
		verdict = "not_applicable"
		detail = "本次未宣告交付成功"
	}
	e.Checks = append(e.Checks, asset.Check{Name: "交付证据", Status: verdict, Detail: detail})
	e.Checks = append(e.Checks, resultEvidenceCheck(s), asset.Check{Name: "原始 Agent 解释事实核验", Status: "unverifiable", Detail: "未自动核验自由说明的语义；程序结果正确不代表模型解释正确，需独立案例评测。"})
	return e
}
