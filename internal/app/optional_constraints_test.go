package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lkmaVanilla/tripo-3d-agent/internal/asset"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/testfixture"
)

func number(n int64) *int64 { return &n }
func TestOptionalConstraintModesAndPersistence(t *testing.T) {
	v := Session{ID: "new-run", ExecutionVersion: OptionalPromptVersion}
	base := optionalIntentInput{Action: "generate", Intent: optionalIntentFields{Asset: "茶壶", Plan: []string{"制作"}}}
	in, e := normalizeOptionalIntent(v, base)
	if e != nil || in.Optional.MaxTriangles != nil || in.Optional.MaxBytes != nil {
		t.Fatal(in, e)
	}
	raw := jsonString(in)
	if !strings.Contains(raw, `"max_triangles":null`) || !strings.Contains(raw, `"max_bytes":null`) {
		t.Fatal(raw)
	}
	var after Intent
	if e = json.Unmarshal([]byte(raw), &after); e != nil || jsonString(after) != raw {
		t.Fatal(e, jsonString(after), raw)
	}
	v.InputVersion = &AssetVersion{ID: "old", SourceRunID: "old-run", SourceIntent: &Intent{MaxTriangles: 3000, MaxBytes: 10 << 20}}
	base.Action = "regenerate"
	in, e = normalizeOptionalIntent(v, base)
	if e != nil || *in.Optional.MaxTriangles != 3000 || in.ConstraintSources["max_triangles"].Kind != "legacy_inherited" {
		t.Fatal(in, e)
	}
	base.Intent.MaxTriangles = &LimitChange{Mode: "clear"}
	in, e = normalizeOptionalIntent(v, base)
	if e != nil || in.Optional.MaxTriangles != nil || *in.Optional.MaxBytes != 10<<20 {
		t.Fatal(in, e)
	}
	base.Intent.MaxBytes = &LimitChange{Mode: "set", Value: number(1024)}
	in, e = normalizeOptionalIntent(v, base)
	if e != nil || *in.Optional.MaxBytes != 1024 {
		t.Fatal(in, e)
	}
	for _, change := range []*LimitChange{{Mode: "set"}, {Mode: "set", Value: number(0)}, {Mode: "set", Value: number(-1)}, {Mode: "clear", Value: number(100)}, {Mode: "inherit", Value: number(1)}, {Mode: "bogus"}} {
		base.Intent.MaxTriangles = change
		if _, e = normalizeOptionalIntent(v, base); e == nil {
			t.Fatal("accepted invalid change", change)
		}
	}
	for _, raw := range []string{`{"intent":{"max_triangles":{"mode":"set","value":1.5}}}`, `{"intent":{"max_bytes":{"mode":"set","value":9223372036854775808}}}`} {
		var in optionalIntentInput
		if json.Unmarshal([]byte(raw), &in) == nil {
			t.Fatal("invalid integer accepted")
		}
	}
	s := testService(t, t.TempDir(), &fakeProvider{}, false)
	defer s.Close()
	run := s.newRun("owner", "静态茶壶", OptionalPromptVersion)
	run.Intent = &after
	if e = s.store.Create(context.Background(), run); e != nil {
		t.Fatal(e)
	}
	restored, e := s.store.Get(context.Background(), run.ID)
	if e != nil || jsonString(restored.Intent) != jsonString(run.Intent) {
		t.Fatal(e)
	}
}
func TestOptionalTargetReportAndReduction(t *testing.T) {
	in, _ := normalizeOptionalIntent(Session{ID: "run"}, optionalIntentInput{Action: "generate", Intent: optionalIntentFields{Asset: "茶壶", Plan: []string{"制作"}}})
	v := Session{ID: "run", ExecutionVersion: OptionalPromptVersion, Intent: &in, GoalKind: "generate"}
	for _, n := range []int{500, 8000, 20000} {
		if !validTarget(v, n) {
			t.Fatal("valid target rejected", n)
		}
	}
	for _, n := range []int{0, 499, 20001} {
		if validTarget(v, n) {
			t.Fatal("invalid target accepted", n)
		}
	}
	report := inspectIntent(testfixture.Cube(6000), &in)
	a := Artifact{ID: "asset", TaskID: "task", Report: report}
	v.Artifacts = []Artifact{a}
	if !deliverable(v, a.ID) || !reportComplete(v, a) {
		t.Fatal("unconstrained output rejected")
	}
	v.GoalKind = "decimate"
	v.Intent.ReductionMode = "further"
	input := asset.InspectOptional(testfixture.Cube(8000), asset.AcceptanceLimits{})
	v.InputAssessment = &input
	if !deliverable(v, a.ID) || inputSatisfiesGoal(v) {
		t.Fatal("relative reduction rejected or input prematurely satisfied")
	}
	v.Artifacts[0].Report = inspectIntent(testfixture.Cube(8000), &in)
	if deliverable(v, a.ID) || goalSatisfied(v, v.Artifacts[0].Report) {
		t.Fatal("unchanged geometry delivered")
	}
	max := 5000
	v.Intent.Optional.MaxTriangles = &max
	if validTarget(v, 8000) || reportComplete(v, a) {
		t.Fatal("explicit bound bypassed")
	}
}

// 正式事实不能仅凭 Passed；每个适用状态及约束版本必须与持久化意图一致。
func TestOptionalRejectsContradictoryReports(t *testing.T) {
	for _, mutate := range []func(*asset.Report){
		func(r *asset.Report) { r.Checks[1].Status = "passed" },
		func(r *asset.Report) { r.Triangles = 0 },
		func(r *asset.Report) { r.Bytes = -1 },
		func(r *asset.Report) { r.Valid = false },
		func(r *asset.Report) { r.Limits = nil },
		func(r *asset.Report) { r.Passed = false },
	} {
		in, _ := normalizeOptionalIntent(Session{}, optionalIntentInput{Action: "generate", Intent: optionalIntentFields{Asset: "木箱", Plan: []string{"制作"}}})
		a := Artifact{ID: "asset", TaskID: "task", Report: inspectIntent(testfixture.Cube(6000), &in)}
		mutate(&a.Report)
		v := Session{ID: "run", ExecutionVersion: OptionalPromptVersion, Intent: &in, Artifacts: []Artifact{a}}
		if reportComplete(v, a) || deliverable(v, a.ID) {
			t.Fatal("contradictory evidence accepted", jsonString(a.Report))
		}
		projected := publicReport(v, a)
		if projected.Valid || projected.Passed || projected.Triangles != 0 || projected.Checks[0].Status != "unverifiable" {
			t.Fatal("untrusted measurements leaked")
		}
	}
}

func TestOptionalClearDoesNotSilentlyInheritTextLimits(t *testing.T) {
	v := Session{ID: "new", InputVersion: &AssetVersion{ID: "source", SourceIntent: &Intent{MaxTriangles: 5000, MaxBytes: 10 << 20, Constraints: []string{"最多5000面", "静态GLB"}}}}
	args := optionalIntentInput{Action: "regenerate", Intent: optionalIntentFields{Asset: "木箱", Plan: []string{"重新制作"}, MaxTriangles: &LimitChange{Mode: "clear"}}}
	if _, e := normalizeOptionalIntent(v, args); e == nil {
		t.Fatal("implicit inheritance could restore cancelled textual limit")
	}
	args.Intent.Constraints = []string{"静态GLB"}
	in, e := normalizeOptionalIntent(v, args)
	if e != nil || in.Optional.MaxTriangles != nil || len(in.Constraints) != 1 || in.Constraints[0] != "静态GLB" {
		t.Fatal(in, e)
	}
}

func TestOptionalInheritanceAndAbsoluteReduction(t *testing.T) {
	faces := 3000
	prior := Intent{Optional: &asset.AcceptanceLimits{MaxTriangles: &faces}}
	v := Session{ID: "next", ExecutionVersion: OptionalPromptVersion, InputVersion: &AssetVersion{ID: "source", SourceIntent: &prior, Report: asset.InspectOptional(testfixture.Cube(2500), asset.AcceptanceLimits{})}}
	in, e := normalizeOptionalIntent(v, optionalIntentInput{Action: "decimate", Intent: optionalIntentFields{Asset: "木箱", Plan: []string{"仅达上限"}, ReductionMode: "within_limit"}})
	if e != nil || in.Optional.MaxBytes != nil || *in.Optional.MaxTriangles != 3000 || in.ConstraintSources["max_triangles"].Kind != "inherited" {
		t.Fatal(in, e)
	}
	v.Intent = &in
	v.GoalKind = "decimate"
	r := inspectIntent(testfixture.Cube(2500), &in)
	v.InputAssessment = &r
	if !inputSatisfiesGoal(v) {
		t.Fatal("satisfied absolute goal must avoid production")
	}
	v.Intent.ReductionMode = "further"
	if inputSatisfiesGoal(v) {
		t.Fatal("further reduction incorrectly satisfied by existing upper bound")
	}
	minimum := 100
	v.Intent.Optional.MaxTriangles = &minimum
	if validTarget(v, 500) {
		t.Fatal("user bound widened to tool minimum")
	}
}
