package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/asset"
)

const OptionalPromptVersion = "asset-agent-v4"

func conversationVersion(version string) bool {
	return version == ConversationPromptVersion || version == OptionalPromptVersion
}
func optionalVersion(v Session) bool { return executionVersion(v) == OptionalPromptVersion }

// ConstraintSource 只证明提议引用了哪个请求/历史版本，不认证自然语言理解正确。
type ConstraintSource struct {
	Kind      string `json:"kind"`
	RunID     string `json:"run_id,omitempty"`
	VersionID string `json:"version_id,omitempty"`
}
type intentJSON Intent

func (in Intent) MarshalJSON() ([]byte, error) {
	if in.Optional == nil {
		return json.Marshal(intentJSON(in))
	}
	return asset.OptionalJSON(intentJSON(in), *in.Optional, map[string]any{"constraint_sources": in.ConstraintSources, "reduction_mode": in.ReductionMode})
}
func (in *Intent) UnmarshalJSON(data []byte) error {
	var old intentJSON
	if err := json.Unmarshal(data, &old); err != nil {
		return err
	}
	limits, err := asset.DecodeOptional(data)
	if err != nil {
		return err
	}
	var extra struct {
		Sources       map[string]ConstraintSource `json:"constraint_sources"`
		ReductionMode string                      `json:"reduction_mode"`
	}
	if err = json.Unmarshal(data, &extra); err != nil {
		return err
	}
	*in = Intent(old)
	in.Optional = limits
	in.ConstraintSources = extra.Sources
	in.ReductionMode = extra.ReductionMode
	return nil
}

// LimitChange 的缺失表示继承；取消必须明确使用 clear，不能与零数值混用。
type LimitChange struct {
	Mode  string `json:"mode" jsonschema:"enum=inherit,enum=set,enum=clear,description=inherit未给新要求时继承或留空；set仅用户明确数值；clear仅用户明确取消"`
	Value *int64 `json:"value,omitempty" jsonschema:"description=set时必填正整数，inherit和clear不提供"`
}
type optionalIntentFields struct {
	Asset         string       `json:"asset"`
	Use           string       `json:"use"`
	Style         string       `json:"style"`
	Constraints   []string     `json:"constraints" jsonschema:"description=完整的当前用户硬约束；已明确取消的旧数值限制不得留在这里"`
	MaxTriangles  *LimitChange `json:"max_triangles,omitempty"`
	MaxBytes      *LimitChange `json:"max_bytes,omitempty"`
	Assumptions   []string     `json:"assumptions"`
	Plan          []string     `json:"plan"`
	ReductionMode string       `json:"reduction_mode,omitempty" jsonschema:"enum=within_limit,enum=further,description=仅decimate必填：within_limit仅达到指定上限；further明确继续降低，可无绝对上限"`
}
type optionalIntentInput struct {
	Action string               `json:"action" jsonschema:"enum=generate,enum=regenerate,enum=decimate"`
	Intent optionalIntentFields `json:"intent"`
}

func resolveLimit(change *LimitChange, prior *int64, v Session, version *AssetVersion) (*int64, ConstraintSource, error) {
	mode := "inherit"
	if change != nil {
		mode = change.Mode
	}
	switch mode {
	case "set":
		if change.Value == nil || *change.Value <= 0 {
			return nil, ConstraintSource{}, fmt.Errorf("set需要正整数上限")
		}
		n := *change.Value
		return &n, ConstraintSource{Kind: "user_request", RunID: v.ID}, nil
	case "clear", "inherit":
		if change != nil && change.Value != nil {
			return nil, ConstraintSource{}, fmt.Errorf("取消或继承不能携带数值")
		}
		if mode == "clear" {
			return nil, ConstraintSource{Kind: "cleared", RunID: v.ID}, nil
		}
		if version != nil && prior != nil {
			kind := "inherited"
			if version.SourceIntent == nil || version.SourceIntent.Optional == nil {
				kind = "legacy_inherited"
			}
			n := *prior
			return &n, ConstraintSource{Kind: kind, VersionID: version.ID, RunID: version.SourceRunID}, nil
		}
		source := ConstraintSource{Kind: "unset"}
		if version != nil {
			source.VersionID = version.ID
		}
		return nil, source, nil
	default:
		return nil, ConstraintSource{}, fmt.Errorf("未知约束变更模式")
	}
}
func intentLimits(in *Intent) asset.AcceptanceLimits {
	if in == nil {
		return asset.AcceptanceLimits{}
	}
	if in.Optional != nil {
		return *in.Optional
	}
	return asset.AcceptanceLimits{MaxTriangles: &in.MaxTriangles, MaxBytes: &in.MaxBytes}
}
func normalizeOptionalIntent(v Session, in optionalIntentInput) (Intent, error) {
	f := in.Intent
	out := Intent{Asset: f.Asset, Use: f.Use, Style: f.Style, Constraints: f.Constraints, Assumptions: f.Assumptions, Plan: f.Plan, ReductionMode: f.ReductionMode, ConstraintSources: map[string]ConstraintSource{}}
	var prior asset.AcceptanceLimits
	if v.InputVersion != nil {
		prior = intentLimits(v.InputVersion.SourceIntent)
	}
	var faces *int64
	if prior.MaxTriangles != nil {
		n := int64(*prior.MaxTriangles)
		faces = &n
	}
	face, fs, err := resolveLimit(f.MaxTriangles, faces, v, v.InputVersion)
	if err != nil {
		return out, err
	}
	size, bs, err := resolveLimit(f.MaxBytes, prior.MaxBytes, v, v.InputVersion)
	if err != nil {
		return out, err
	}
	out.Optional = &asset.AcceptanceLimits{MaxBytes: size}
	if face != nil {
		n := int(*face)
		if int64(n) != *face {
			return out, fmt.Errorf("面数上限溢出")
		}
		out.Optional.MaxTriangles = &n
		out.MaxTriangles = n
	}
	if size != nil {
		out.MaxBytes = *size
	}
	out.ConstraintSources["max_triangles"] = fs
	out.ConstraintSources["max_bytes"] = bs
	// 旧自由文本可能同时记录数值与其他硬约束。取消时要求完整的新清单，
	// 不猜测删除哪些文字，也不让下层的 nil 继承把已取消条款自动补回来。
	if v.InputVersion != nil && v.InputVersion.SourceIntent != nil && len(v.InputVersion.SourceIntent.Constraints) > 0 && f.Constraints == nil && (fs.Kind == "cleared" || bs.Kind == "cleared") {
		return out, fmt.Errorf("取消上限时须提供完整constraints列表：移除已取消数值，保留其他硬约束；没有剩余条款则提供空数组")
	}
	if in.Action == "decimate" {
		if f.ReductionMode != "within_limit" && f.ReductionMode != "further" {
			return out, fmt.Errorf("减面需要明确within_limit或further目标")
		}
		if f.ReductionMode == "within_limit" && out.Optional.MaxTriangles == nil {
			return out, fmt.Errorf("达到上限目标必须有明确面数上限；继续降低使用further")
		}
	} else if f.ReductionMode != "" {
		return out, fmt.Errorf("生成不能设置减面目标模式")
	}
	return out, nil
}

// optionalIntentTool 只替换新版 schema；下层仍使用原事务和输入绑定保护。
func (s *Service) optionalIntentTool(id string, original tool.InvokableTool) (tool.InvokableTool, error) {
	return utils.InferTool("set_intent", "保存当前目标。未给数值不补默认；历史加工先选择版本。上限使用inherit/set/clear；明确降低面数用further，单纯达到上限用within_limit。", func(ctx context.Context, in *optionalIntentInput) (string, error) {
		v, err := s.store.Get(ctx, id)
		if err != nil {
			return "", err
		}
		if !optionalVersion(v) {
			return "", recoveryError("execution_version_mismatch")
		}
		if v.InputVersion != nil {
			source, e := s.productionVersion(ctx, v, v.InputVersion.ID)
			if e != nil {
				return s.block(id, "invalid_artifact", e.Error())
			}
			v.InputVersion = &source
		}
		intent, err := normalizeOptionalIntent(v, *in)
		if err != nil {
			return s.block(id, "invalid_intent", err.Error())
		}
		return original.InvokableRun(ctx, jsonString(conversationIntentInput{Action: in.Action, Intent: intent}))
	})
}
func validAcceptedIntent(v Session) bool {
	if v.Intent == nil {
		return false
	}
	if optionalVersion(v) {
		return v.Intent.Optional != nil && v.Intent.Optional.Valid()
	}
	return v.Intent.Optional == nil && v.Intent.MaxTriangles > 0 && v.Intent.MaxBytes > 0
}
func validTarget(v Session, target int) bool {
	if !validAcceptedIntent(v) || target < 500 || target > 20000 {
		return false
	}
	bound := intentLimits(v.Intent).MaxTriangles
	return bound == nil || target <= *bound
}
func inspectIntent(data []byte, in *Intent) asset.Report {
	if in.Optional != nil {
		return asset.InspectOptional(data, *in.Optional)
	}
	return asset.Inspect(data, in.MaxTriangles, in.MaxBytes)
}
func goalSatisfied(v Session, r asset.Report) bool {
	if !r.Valid || !r.Passed {
		return false
	}
	if optionalVersion(v) && v.GoalKind == "decimate" && v.Intent != nil && v.Intent.ReductionMode == "further" {
		return v.InputAssessment != nil && v.InputAssessment.Valid && r.Triangles < v.InputAssessment.Triangles
	}
	return true
}
func inputSatisfiesGoal(v Session) bool {
	if v.InputAssessment == nil || !v.InputAssessment.Passed {
		return false
	}
	return !optionalVersion(v) || (v.GoalKind == "decimate" && v.Intent != nil && v.Intent.ReductionMode == "within_limit")
}
func optionalIntentValid(in *Intent) bool {
	return in.Optional != nil && in.Optional.Valid() && len(in.ConstraintSources) == 2 && strings.TrimSpace(in.Asset) != "" && len(in.Plan) > 0
}
