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

var ErrClosed = errors.New("请求已结束")
var ErrBudget = errors.New("请求调用额度已耗尽")

type Config struct {
	Listen, DataDir, DeepSeekKey, TripoKey                                  string
	SecureCookie                                                            bool
	ProductionSlots, QueueSize, MaxCalls, MaxSubmissions, MaxClarifications int
	MaxDuration, PollInterval, IdleTTL, Retention, VisitorTTL               time.Duration
}

func DefaultConfig() Config {
	return Config{Listen: "127.0.0.1:8080", DataDir: "./data", ProductionSlots: 3, QueueSize: 10, MaxCalls: 20, MaxSubmissions: 3, MaxClarifications: 3, MaxDuration: 30 * time.Minute, PollInterval: 3 * time.Second, IdleTTL: 24 * time.Hour, Retention: 7 * 24 * time.Hour, VisitorTTL: 30 * 24 * time.Hour}
}
func ConfigFromEnv() (Config, error) {
	c := DefaultConfig()
	c.DeepSeekKey = os.Getenv("DEEPSEEK_API_KEY")
	c.TripoKey = os.Getenv("TRIPO_API_KEY")
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

type Intent struct {
	Asset        string   `json:"asset" jsonschema:"description=静态道具的名称与关键特征"`
	Use          string   `json:"use" jsonschema:"description=用户的资产用途"`
	Style        string   `json:"style" jsonschema:"description=用户要求的风格"`
	Constraints  []string `json:"constraints" jsonschema:"description=必须保留的全部用户硬约束"`
	MaxTriangles int      `json:"max_triangles" jsonschema:"description=验收面数上限，用户未指定时为5000"`
	MaxBytes     int64    `json:"max_bytes" jsonschema:"description=验收字节上限，用户未指定时为10485760"`
	Assumptions  []string `json:"assumptions" jsonschema:"description=明确列出采用的默认假设"`
	Plan         []string `json:"plan" jsonschema:"description=生产与验收计划"`
}
type Limits struct {
	Calls, Submissions, Clarifications int
	Duration, Idle, Retention          time.Duration
}
type Artifact struct {
	ID        string       `json:"id"`
	TaskID    string       `json:"task_id"`
	Path      string       `json:"path"`
	SourceURL string       `json:"source_url"`
	Report    asset.Report `json:"report"`
}
type Operation struct {
	ID         string       `json:"id"`
	Kind       string       `json:"kind"`
	Params     tripo.Params `json:"params"`
	Stage      string       `json:"stage"`
	TaskID     string       `json:"task_id"`
	Error      string       `json:"error,omitempty"`
	ArtifactID string       `json:"artifact_id,omitempty"`
}
type Session struct {
	ID, Owner, Request, Status                          string
	Intent                                              *Intent
	Question, Answer, WaitID                            string
	Clarifications, Production, ModelCalls              int
	Limits                                              Limits
	Deadline, Created, LastUser, Ended, Expires, Queued time.Time
	HasSlot, CheckpointReady, ResumeRequested           bool
	Current                                             *Operation
	Artifacts                                           []Artifact
	History                                             []*schema.Message
	Final, SelectedArtifact                             string
	Model, Source                                       string
}

func (s Session) Terminal() bool { return !s.Ended.IsZero() }
func (s *Session) Finish(status, reason string) {
	s.Status = status
	s.Final = reason
	s.Ended = time.Now().UTC()
	s.Expires = s.Ended.Add(s.Limits.Retention)
	s.HasSlot = false
	s.ResumeRequested = false
}
func (s Session) View() map[string]any {
	arts := make([]map[string]any, 0, len(s.Artifacts))
	for _, a := range s.Artifacts {
		arts = append(arts, map[string]any{"id": a.ID, "task_id": a.TaskID, "report": a.Report, "url": "/api/sessions/" + s.ID + "/artifacts/" + a.ID})
	}
	return map[string]any{"id": s.ID, "request": s.Request, "status": s.Status, "intent": s.Intent, "question": s.Question, "clarifications": s.Clarifications, "production": s.Production, "model_calls": s.ModelCalls, "max_submissions": s.Limits.Submissions, "max_model_calls": s.Limits.Calls, "deadline": s.Deadline, "created": s.Created, "ended": s.Ended, "expires": s.Expires, "artifacts": arts, "selected_artifact": s.SelectedArtifact, "final": s.Final, "model": s.Model, "evaluation": evaluate(s)}
}
func newID() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

type Evaluation struct {
	Scope  string        `json:"scope"`
	Checks []asset.Check `json:"checks"`
}

func evaluate(s Session) Evaluation {
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
		for _, a := range s.Artifacts {
			if a.ID == s.SelectedArtifact && a.Report.Passed {
				verdict = "passed"
				detail = "交付引用已实测且技术通过的模型"
			}
		}
	} else if s.Terminal() {
		verdict = "not_applicable"
		detail = "本次未宣告交付成功"
	}
	e.Checks = append(e.Checks, asset.Check{Name: "交付证据", Status: verdict, Detail: detail})
	return e
}
