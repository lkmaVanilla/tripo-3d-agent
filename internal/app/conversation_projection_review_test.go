package app

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// 新工作台与兼容单 Run 导出必须呈现相同的事件核验结论；拦截不抵消违规提议。
func TestConversationCachedEvaluationMatchesSingleRunTrace(t *testing.T) {
	ctx := context.Background()
	s := testService(t, t.TempDir(), &fakeProvider{}, false)
	defer s.Close()
	c, run, err := s.CreateAssetConversation(ctx, "owner", "解释制作能力", "review-first")
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"constraint", "budget", "false_validation", "invalid_artifact"} {
		if _, err = s.store.Edit(ctx, run.ID, nil, "runtime_blocked", map[string]string{"code": code, "detail": "受控违规提议"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, terminal := range []bool{false, true} {
		if terminal {
			if _, err = s.store.Edit(ctx, run.ID, func(v *Session) error { return v.finishAnswer("能力说明") }, "agent_finished", nil); err != nil {
				t.Fatal(err)
			}
		}
		v, err := s.store.Get(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		events, err := s.store.Events(ctx, run.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		legacy := s.snapshot(v, events)["session"].(map[string]any)["evaluation"]
		var expected any
		if err = json.Unmarshal([]byte(jsonString(legacy)), &expected); err != nil {
			t.Fatal(err)
		}
		snapshot, err := s.store.ConversationSnapshot(ctx, c.ID, "owner", 0, 0, 50)
		if err != nil || len(snapshot.Runs) != 1 {
			t.Fatalf("snapshot unavailable: %+v %v", snapshot, err)
		}
		if got := snapshot.Runs[0]["evaluation"]; !reflect.DeepEqual(got, expected) {
			t.Fatalf("terminal=%v cached evaluation differs from single Run trace\nconversation=%s\nsingle_run=%s", terminal, jsonString(got), jsonString(legacy))
		}
	}
}

// 即使每条正文很短，20 条消息窗口裁掉旧记录时也必须明确标记已裁剪。
func TestConversationContextMarksMessageCountTruncation(t *testing.T) {
	ctx := context.Background()
	s := testService(t, t.TempDir(), &fakeProvider{}, false)
	defer s.Close()
	c, run, err := s.CreateAssetConversation(ctx, "owner", "木箱能力说明", "first-short")
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 11; index++ {
		if _, err = s.store.Edit(ctx, run.ID, func(v *Session) error { return v.finishAnswer("简短回答") }, "agent_finished", nil); err != nil {
			t.Fatal(err)
		}
		if err = s.store.ReleaseConversationRun(ctx, run.ID); err != nil {
			t.Fatal(err)
		}
		run, err = s.ContinueConversation(ctx, c.ID, "owner", "继续解释这个木箱", newID(), "")
		if err != nil {
			t.Fatal(err)
		}
	}
	if truncated, ok := run.ConversationContext["truncated"].(bool); !ok || !truncated {
		t.Fatalf("bounded context falsely says all history is present: %s", jsonString(run.ConversationContext))
	}
}
