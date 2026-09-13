package app

import (
	"context"
	"testing"
)

// TestExecutionProfileV1Frozen 将 v1 全部模型可见配置绑定到 49a1364 的固定指纹。
// 指纹在本变更修改前从真实工具 schema 和 Skill 字节采集，不引用 v2 配置。
func TestExecutionProfileV1Frozen(t *testing.T) {
	tools, err := (&Service{}).tools("", &pauseCoordinator{mode: "normal"})
	if err != nil {
		t.Fatal(err)
	}
	entries := []any{}
	for _, tool := range tools {
		info, err := tool.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		params, err := info.ParamsOneOf.ToJSONSchema()
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, map[string]any{"name": info.Name, "description": info.Desc, "schema": params})
	}
	for _, name := range []string{"intent", "generation", "correction"} {
		content, err := skillFiles.ReadFile("skills/" + name + ".md")
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, map[string]any{"name": name, "description": skillDescriptions[name], "content": string(content), "base_directory": "embedded://skills/" + name})
	}
	got := tokenHash(jsonString(map[string]any{"instruction": instruction, "description": "静态道具生产与技术纠偏", "entries": entries}))
	const baseline = "5c40a02f7ee2ff6202e45cc1166dbae2fa9600f2909c828b29a13299d181a2ff"
	if got != baseline {
		t.Fatalf("v1 configuration no longer matches 49a1364: %s", got)
	}
}
