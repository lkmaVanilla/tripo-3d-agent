package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

// IntentDraft 是尚未授权生产的完整需求及来源差异。ID 固定其内容，支持重放去重。
type IntentDraft struct {
	ID        string         `json:"id"`
	VersionID string         `json:"version_id"`
	Action    string         `json:"action"`
	Intent    Intent         `json:"intent"`
	Changes   []IntentChange `json:"changes"`
	Inherited []string       `json:"inherited"`
}

type IntentChange struct {
	Field  string `json:"field"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

// prepareConversationIntent 只补回未提供的来源字段。明确给出的新值由 Agent 从用户语义提取，
// Go 记录真实字段差异而不声称已验证语义；空约束数组是明确值，nil 才表示未提供。
// 草案事务先提交，正式 Intent 在下一事务中按草案身份冻结。中途崩溃不会提前授权生产。
func (s *Service) prepareConversationIntent(ctx context.Context, v Session, input AssetVersion, action string, intent Intent) (Intent, string, error) {
	if prior := input.SourceIntent; prior != nil {
		if strings.TrimSpace(intent.Asset) == "" {
			intent.Asset = prior.Asset
		}
		if strings.TrimSpace(intent.Use) == "" {
			intent.Use = prior.Use
		}
		if strings.TrimSpace(intent.Style) == "" {
			intent.Style = prior.Style
		}
		if intent.Constraints == nil {
			intent.Constraints = append([]string(nil), prior.Constraints...)
		}
		if intent.MaxTriangles == 0 {
			intent.MaxTriangles = prior.MaxTriangles
		}
		if intent.MaxBytes == 0 {
			intent.MaxBytes = prior.MaxBytes
		}
	}
	if intent.MaxTriangles == 0 {
		intent.MaxTriangles = 5000
		intent.Assumptions = append(intent.Assumptions, "未指定面数上限，默认5,000个三角面")
	}
	if intent.MaxBytes == 0 {
		intent.MaxBytes = 10 << 20
		intent.Assumptions = append(intent.Assumptions, "未指定文件体积上限，默认10 MiB")
	}
	if strings.TrimSpace(intent.Asset) == "" || len(intent.Plan) == 0 || intent.MaxTriangles < 1 || intent.MaxBytes < 1 {
		return intent, "", fmt.Errorf("需要明确资产、计划和正数技术上限")
	}
	draft := IntentDraft{VersionID: input.ID, Action: action, Intent: intent, Changes: []IntentChange{}, Inherited: []string{}}
	// 使用 JSON 字段名，投影前后值与实际冻结的 Intent 保持一致。
	before, after := map[string]any{}, map[string]any{}
	if input.SourceIntent != nil {
		_ = json.Unmarshal([]byte(jsonString(input.SourceIntent)), &before)
	}
	_ = json.Unmarshal([]byte(jsonString(intent)), &after)
	fields := make([]string, 0, len(after))
	for field := range after {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	for _, field := range fields {
		if reflect.DeepEqual(before[field], after[field]) {
			draft.Inherited = append(draft.Inherited, field)
		} else {
			draft.Changes = append(draft.Changes, IntentChange{Field: field, Before: before[field], After: after[field]})
		}
	}
	hash := sha256.Sum256([]byte(jsonString(draft)))
	draft.ID = hex.EncodeToString(hash[:])
	if v.Intent != nil || (v.IntentDraft != nil && v.IntentDraft.ID == draft.ID) {
		return intent, draft.ID, nil
	}
	_, err := s.store.Edit(ctx, v.ID, func(current *Session) error {
		if err := checkExecution(*current, time.Now()); err != nil {
			return err
		}
		if current.Intent != nil || current.InputVersion == nil || current.InputVersion.ID != input.ID {
			return fmt.Errorf("本次目标或输入已经改变")
		}
		current.IntentDraft = &draft
		return nil
	}, "intent_review", draft)
	return intent, draft.ID, err
}
