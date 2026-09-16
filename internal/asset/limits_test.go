package asset

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lkmaVanilla/tripo-3d-agent/internal/testfixture"
)

func TestOptionalLimitsInspection(t *testing.T) {
	faces, size := 5000, int64(10<<20)
	for _, tc := range []struct {
		name       string
		l          AcceptanceLimits
		passed     bool
		face, size string
	}{
		{"none", AcceptanceLimits{}, true, "not_applicable", "not_applicable"},
		{"faces", AcceptanceLimits{MaxTriangles: &faces}, false, "failed", "not_applicable"},
		{"bytes", AcceptanceLimits{MaxBytes: &size}, true, "not_applicable", "passed"},
		{"both", AcceptanceLimits{&faces, &size}, false, "failed", "passed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := InspectOptional(testfixture.Cube(6000), tc.l)
			if !r.Valid || r.Passed != tc.passed || r.Triangles != 6000 || r.Checks[1].Status != tc.face || r.Checks[2].Status != tc.size {
				t.Fatalf("%+v", r)
			}
			raw, e := json.Marshal(r)
			if e != nil {
				t.Fatal(e)
			}
			var restored Report
			if e = json.Unmarshal(raw, &restored); e != nil || restored.Limits == nil || !restored.Limits.Equal(tc.l) || restored.Passed != r.Passed {
				t.Fatalf("roundtrip %s %v", raw, e)
			}
		})
	}
	data := testfixture.Cube(6000)
	// An ignored JSON extras payload keeps the GLB valid while exceeding the old size default.
	data = mutate(data, func(d map[string]any) { d["extras"] = map[string]any{"padding": strings.Repeat("x", 12<<20)} })
	r := InspectOptional(data, AcceptanceLimits{})
	if !r.Passed || r.Bytes <= 10<<20 || r.Triangles != 6000 {
		t.Fatalf("large unconstrained: %+v", r)
	}
	r = InspectOptional([]byte("broken"), AcceptanceLimits{})
	if r.Passed || r.Valid || r.Checks[1].Status != "unverifiable" || r.Checks[2].Status != "not_applicable" {
		t.Fatal(r)
	}
}
func TestOptionalLimitsResourceAndEncoding(t *testing.T) {
	data := mutate(testfixture.Cube(12), func(d map[string]any) { d["accessors"].([]any)[0].(map[string]any)["count"] = 1 << 30 })
	r := InspectOptional(data, AcceptanceLimits{})
	if r.Passed || !r.ResourceLimited || r.Checks[0].Status != "unverifiable" {
		t.Fatal(r)
	}
	for _, raw := range []string{
		`{"constraint_version":"optional-v1","max_triangles":0,"max_bytes":null}`,
		`{"constraint_version":"optional-v1","max_triangles":null}`,
		`{"constraint_version":"unknown","max_triangles":null,"max_bytes":null}`,
		`{"constraint_version":"optional-v1","max_triangles":2.5,"max_bytes":null}`,
	} {
		var r Report
		if json.Unmarshal([]byte(raw), &r) == nil {
			t.Fatal("accepted corrupt evidence", raw)
		}
	}
}

func TestOptionalExactBoundaries(t *testing.T) {
	data := testfixture.Cube(6000)
	faces, size := 6000, int64(len(data))
	r := InspectOptional(data, AcceptanceLimits{MaxTriangles: &faces, MaxBytes: &size})
	if !r.Passed || r.Checks[1].Status != "passed" || r.Checks[2].Status != "passed" {
		t.Fatal("exact boundary rejected", r)
	}
	size--
	r = InspectOptional(data, AcceptanceLimits{MaxBytes: &size})
	if r.Passed || r.Checks[1].Status != "not_applicable" || r.Checks[2].Status != "failed" {
		t.Fatal("single byte bound ignored", r)
	}
}
