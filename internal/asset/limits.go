package asset

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

const OptionalConstraintVersion = "optional-v1"

// AcceptanceLimits 是验收条件；nil 表示没有对应要求，不限制测量与资源保护。
type AcceptanceLimits struct {
	MaxTriangles *int   `json:"max_triangles"`
	MaxBytes     *int64 `json:"max_bytes"`
}

func (l AcceptanceLimits) Valid() bool {
	return (l.MaxTriangles == nil || *l.MaxTriangles > 0) && (l.MaxBytes == nil || *l.MaxBytes > 0)
}
func (l AcceptanceLimits) Equal(other AcceptanceLimits) bool { return reflect.DeepEqual(l, other) }

// OptionalJSON 仅改新版字段；旧版 JSON 字节由原结构直接编码，保留恢复证据。
func OptionalJSON(base any, l AcceptanceLimits, extra map[string]any) ([]byte, error) {
	b, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}
	var fields map[string]any
	if err = json.Unmarshal(b, &fields); err != nil {
		return nil, err
	}
	fields["constraint_version"] = OptionalConstraintVersion
	fields["max_triangles"] = l.MaxTriangles
	fields["max_bytes"] = l.MaxBytes
	for k, v := range extra {
		fields[k] = v
	}
	return json.Marshal(fields)
}

// DecodeOptional 要求新版显式保存两个字段；缺失字段是损坏证据，不等于取消约束。
func DecodeOptional(data []byte) (*AcceptanceLimits, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	version, ok := fields["constraint_version"]
	if !ok {
		return nil, nil
	}
	var name string
	if json.Unmarshal(version, &name) != nil || name != OptionalConstraintVersion {
		return nil, errors.New("unknown constraint version")
	}
	faces, fok := fields["max_triangles"]
	size, sok := fields["max_bytes"]
	if !fok || !sok {
		return nil, errors.New("optional constraint fields missing")
	}
	l := new(AcceptanceLimits)
	if err := json.Unmarshal(faces, &l.MaxTriangles); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(size, &l.MaxBytes); err != nil {
		return nil, err
	}
	if !l.Valid() {
		return nil, errors.New("constraint must be a positive integer or null")
	}
	return l, nil
}

type reportJSON Report

func (r Report) MarshalJSON() ([]byte, error) {
	if r.Limits == nil {
		return json.Marshal(reportJSON(r))
	}
	return OptionalJSON(reportJSON(r), *r.Limits, nil)
}
func (r *Report) UnmarshalJSON(data []byte) error {
	var old reportJSON
	if err := json.Unmarshal(data, &old); err != nil {
		return err
	}
	l, err := DecodeOptional(data)
	if err != nil {
		return err
	}
	*r = Report(old)
	r.Limits = l
	return nil
}

func comparison(value int64, limit *int64, unit string) Check {
	if limit == nil {
		return Check{Status: "not_applicable", Detail: fmt.Sprintf("实测 %d %s；未设置验收上限", value, unit)}
	}
	return Check{Status: verdict(value <= *limit), Detail: fmt.Sprintf("实测 %d / 上限 %d %s", value, *limit, unit)}
}

// OptionalChecks 由测量事实生成检查结论，检查器和结果一致性校验共用。
func OptionalChecks(r Report) ([]Check, bool) {
	if r.Limits == nil || !r.Limits.Valid() {
		return nil, false
	}
	file := Check{Name: "文件有效性", Status: "failed", Detail: "文件未通过有效性检查"}
	geometry := Check{Name: "几何规模", Status: "unverifiable", Detail: "文件未通过有效性检查，不能给出可信面数"}
	if r.ResourceLimited {
		file.Status = "unverifiable"
		file.Detail = "超过系统解析资源限制，无法安全验证"
	}
	if r.Valid {
		file.Status = "passed"
		file.Detail = "静态、自包含、非空三角面 GLB"
		var limit *int64
		if r.Limits.MaxTriangles != nil {
			n := int64(*r.Limits.MaxTriangles)
			limit = &n
		}
		geometry = comparison(int64(r.Triangles), limit, "个三角面")
		geometry.Name = "几何规模"
	}
	size := comparison(r.Bytes, r.Limits.MaxBytes, "字节")
	size.Name = "文件体积"
	passed := r.Valid && !r.ResourceLimited && r.Triangles > 0 && r.Bytes > 0 && geometry.Status != "failed" && size.Status != "failed"
	return []Check{file, geometry, size}, passed
}

func InspectOptional(data []byte, limits AcceptanceLimits) Report {
	r := Report{Bytes: int64(len(data)), Limits: &limits, Visual: "未进行视觉检查；技术通过不代表类别、风格或外观符合需求。"}
	if !limits.Valid() {
		r.Checks = []Check{{Name: "验收条件", Status: "unverifiable", Detail: "非法验收上限"}}
		return r
	}
	triangles, err := geometry(data)
	r.Valid = err == nil
	if r.Valid {
		r.Triangles = triangles
	}
	r.ResourceLimited = errors.Is(err, ErrInspectionResourceLimit)
	r.Checks, r.Passed = OptionalChecks(r)
	if err != nil {
		r.Checks[0].Detail = err.Error()
	}
	return r
}

var ErrInspectionResourceLimit = errors.New("展开后的网格数据超过 256 MiB 检查资源上限，无法验证")
