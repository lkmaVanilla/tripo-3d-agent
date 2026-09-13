package app

import (
	"context"
	"reflect"
	"testing"

	"github.com/cloudwego/eino/components/tool"
)

// 草案已发布而正式保存失败时，既不能生产，也不能在重启后丢失来源约束或重复差异卡。
func TestConversationIntentReviewPrecedesAcceptanceAndSurvivesFailure(t *testing.T) {
	ctx := context.Background()
	s, v, input := conversationInputFixture(t)
	defer func() { _ = s.Close() }()
	prior := *input.SourceIntent
	prior.Style = "工业写实"
	prior.Constraints = []string{"仅外观展示", "单个独立模型"}
	prior.MaxBytes = 3 << 20
	if _, err := s.store.db.ExecContext(ctx, "UPDATE asset_versions SET source_intent=? WHERE id=?", jsonString(prior), input.ID); err != nil {
		t.Fatal(err)
	}
	var err error
	v, err = s.store.Edit(ctx, v.ID, func(v *Session) error {
		v.Intent, v.Current, v.InputAssessment = nil, nil, nil
		v.GoalKind, v.Status, v.HasSlot = "", "understanding", false
		return nil
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	call := func(name string, args any) {
		t.Helper()
		all, err := s.tools(v.ID, &pauseCoordinator{mode: "normal"})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range all {
			info, _ := item.Info(ctx)
			if info.Name == name {
				if _, err = item.(tool.InvokableTool).InvokableRun(ctx, jsonString(args)); err != nil {
					t.Fatal(err)
				}
				return
			}
		}
		t.Fatal("tool not found")
	}
	args := conversationIntentInput{Action: "decimate", Intent: Intent{MaxTriangles: 2000, Plan: []string{"对引用模型减面并技术检查"}}}
	if _, err = s.store.db.Exec(`CREATE TRIGGER reject_intent BEFORE INSERT ON events WHEN NEW.kind='intent_and_plan' BEGIN SELECT RAISE(ABORT,'controlled intent failure'); END`); err != nil {
		t.Fatal(err)
	}
	call("set_intent", args)
	saved, _ := s.store.Get(ctx, v.ID)
	if saved.Intent != nil || saved.IntentDraft == nil || saved.Production != 0 || saved.GoalKind != "" {
		t.Fatal("draft authorized production or was lost")
	}
	if saved.IntentDraft.Intent.MaxBytes != prior.MaxBytes || saved.IntentDraft.Intent.Style != prior.Style || !reflect.DeepEqual(saved.IntentDraft.Intent.Constraints, prior.Constraints) {
		t.Fatal("omitted source constraints not inherited")
	}
	if saved.IntentDraft.Intent.MaxTriangles != 2000 {
		t.Fatal("explicit new limit not applied")
	}
	call("decimate_asset", decimationInput{ArtifactID: input.ID, TargetTriangles: 2000, Reason: "草案不能生产"})
	saved, _ = s.store.Get(ctx, v.ID)
	if saved.Production != 0 || saved.Current != nil {
		t.Fatal("unaccepted draft produced a task")
	}
	assertStoreEventCount(t, s.store, v.ID, "intent_review", 1)
	assertStoreEventCount(t, s.store, v.ID, "intent_and_plan", 0)
	if _, err = s.store.db.Exec("DROP TRIGGER reject_intent"); err != nil {
		t.Fatal(err)
	}
	dir := s.Config.DataDir
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s = testService(t, dir, &fakeProvider{}, false)
	call("set_intent", args)
	saved, err = s.store.Get(ctx, v.ID)
	if err != nil || saved.Intent == nil || saved.Intent.MaxBytes != prior.MaxBytes || saved.Intent.MaxTriangles != 2000 || saved.GoalKind != "decimate" {
		t.Fatal("restart did not accept frozen complete intent", err)
	}
	assertStoreEventCount(t, s.store, v.ID, "intent_review", 1)
	assertStoreEventCount(t, s.store, v.ID, "intent_and_plan", 1)
	snap, err := s.store.ConversationSnapshot(ctx, input.ConversationID, "owner", 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var reviewSeq, acceptedSeq int64
	for _, m := range snap.Messages {
		if m.RunID != v.ID {
			continue
		}
		if m.Kind == "intent_review" {
			reviewSeq = m.Seq
		}
		if m.Kind == "accepted_plan" {
			acceptedSeq = m.Seq
		}
	}
	if reviewSeq == 0 || acceptedSeq <= reviewSeq {
		t.Fatal("review not published before acceptance")
	}
	unchanged, err := s.store.GetAssetVersion(ctx, input.ConversationID, input.ID)
	if err != nil || unchanged.SHA256 != input.SHA256 || !reflect.DeepEqual(unchanged.Report, input.Report) {
		t.Fatal("old version evidence changed")
	}
}
