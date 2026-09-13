package asset

import (
	"encoding/binary"
	"encoding/json"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/testfixture"
	"testing"
)

// mutate 只改 GLB 的 JSON 块并重算长度与四字节对齐，保留原始二进制几何数据。
// 这样可隔离某个结构约束的失败，避免因文件封装损坏而掩盖待测规则。
func mutate(data []byte, fn func(map[string]any)) []byte {
	n := int(binary.LittleEndian.Uint32(data[12:16]))
	var doc map[string]any
	_ = json.Unmarshal(data[20:20+n], &doc)
	fn(doc)
	b, _ := json.Marshal(doc)
	for len(b)%4 != 0 {
		b = append(b, ' ')
	}
	out := append(append([]byte(nil), data[:20]...), b...)
	out = append(out, data[20+n:]...)
	binary.LittleEndian.PutUint32(out[8:12], uint32(len(out)))
	binary.LittleEndian.PutUint32(out[12:16], uint32(len(b)))
	return out
}

// TestInspect 区分文件是否可接受（Valid）和是否满足本次资源限制（Passed）。
// 所有数据均为本地合成或定向破坏的 GLB，仅覆盖技术检查，不包含视觉或语义评测。
func TestInspect(t *testing.T) {
	valid := testfixture.Cube(12)
	tests := []struct {
		name          string
		data          []byte
		faces         int
		size          int64
		valid, passed bool
	}{
		{"valid", valid, 5000, 10 << 20, true, true},
		{"triangles_over_limit", testfixture.Cube(6000), 5000, 10 << 20, true, false},
		{"bytes_over_limit", valid, 5000, 100, true, false},
		{"truncated", valid[:len(valid)-1], 5000, 10 << 20, false, false},
		{"non_glb", []byte(`{"asset":{"version":"2.0"}}`), 5000, 10 << 20, false, false},
		{"external", mutate(valid, func(d map[string]any) { d["images"] = []any{map[string]any{"uri": "https://example.org/x.png"}} }), 5000, 10 << 20, false, false},
		{"compressed", mutate(valid, func(d map[string]any) { d["extensionsUsed"] = []string{"EXT_meshopt_compression"} }), 5000, 10 << 20, false, false},
		{"animated", mutate(valid, func(d map[string]any) { d["animations"] = []any{map[string]any{}} }), 5000, 10 << 20, false, false},
		{"bad_index_reference", mutate(valid, func(d map[string]any) {
			d["meshes"].([]any)[0].(map[string]any)["primitives"].([]any)[0].(map[string]any)["indices"] = 99
		}), 5000, 10 << 20, false, false},
		{"not_triangles", mutate(valid, func(d map[string]any) {
			d["meshes"].([]any)[0].(map[string]any)["primitives"].([]any)[0].(map[string]any)["mode"] = 1
		}), 5000, 10 << 20, false, false},
		{"cyclic_nodes", mutate(valid, func(d map[string]any) {
			d["nodes"].([]any)[0].(map[string]any)["children"] = []int{0}
		}), 5000, 10 << 20, false, false},
		{"empty_scene", mutate(valid, func(d map[string]any) {
			d["scenes"].([]any)[0].(map[string]any)["nodes"] = []int{}
		}), 5000, 10 << 20, false, false},
		{"bad_scene_reference", mutate(valid, func(d map[string]any) { d["scene"] = 100 }), 5000, 10 << 20, false, false},
		{"oversized_accessor", mutate(valid, func(d map[string]any) {
			d["accessors"].([]any)[0].(map[string]any)["count"] = 1 << 30
		}), 5000, 10 << 20, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Inspect(tt.data, tt.faces, tt.size)
			if r.Valid != tt.valid || r.Passed != tt.passed {
				t.Fatalf("unexpected report: %+v", r)
			}
			if len(r.Checks) != 3 {
				t.Fatal("must report all three checks")
			}
			if tt.name == "valid" && r.Triangles != 12 {
				t.Fatalf("measured %d", r.Triangles)
			}
		})
	}
}
