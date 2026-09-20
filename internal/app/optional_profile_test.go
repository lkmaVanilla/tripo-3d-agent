package app

import (
	"context"
	"strings"
	"testing"
)

// 指纹独立采自 80dd8dc 的 git archive，冻结旧 v3 工具 schema、提示和四份 Skill。
func TestOptionalV3FrozenConfiguration(t *testing.T) {
	profile, err := profileForVersion(ConversationPromptVersion)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := (&Service{}).tools("", &pauseCoordinator{mode: "normal", profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	entries := []any{}
	for _, tool := range tools {
		info, e := tool.Info(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		params, e := info.ParamsOneOf.ToJSONSchema()
		if e != nil {
			t.Fatal(e)
		}
		entries = append(entries, map[string]any{"name": info.Name, "description": info.Desc, "schema": params})
	}
	for _, name := range []string{"intent", "generation", "correction", "asset-editing"} {
		content, e := profile.skillContent(name)
		if e != nil {
			t.Fatal(e)
		}
		description, _ := profile.skillDescription(name)
		entries = append(entries, map[string]any{"name": name, "description": description, "content": content, "base_directory": "embedded://skills/" + name})
	}
	got := tokenHash(jsonString(map[string]any{"instruction": profile.Instruction, "description": profile.Description, "entries": entries}))
	if got != "439e14b784d384645168b2991bdf7110c836c04c32ae94363a4c55c7f21f42f8" {
		t.Fatalf("v3 configuration changed: %s", got)
	}
}

func TestOptionalProfileLoadsAllSkillsAndToolSchema(t *testing.T) {
	p, e := profileForVersion(OptionalPromptVersion)
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"intent", "generation", "correction", "asset-editing"} {
		content, e := p.skillContent(name)
		if e != nil || content == "" {
			t.Fatalf("missing embedded v4 skill %s: %v", name, e)
		}
	}
	tools, e := (&Service{}).tools("", &pauseCoordinator{mode: "normal", profile: p})
	if e != nil {
		t.Fatal(e)
	}
	for _, tool := range tools {
		info, e := tool.Info(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		if info.Name != "set_intent" {
			continue
		}
		schema, e := info.ParamsOneOf.ToJSONSchema()
		if e != nil {
			t.Fatal(e)
		}
		raw := jsonString(schema)
		for _, expected := range []string{"inherit", "clear", "set", "reduction_mode", "within_limit", "further"} {
			if !strings.Contains(raw, expected) {
				t.Fatalf("v4 schema missing %s", expected)
			}
		}
		return
	}
	t.Fatal("no v4 intent tool")
}
