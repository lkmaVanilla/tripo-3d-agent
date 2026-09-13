package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/asset"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/testfixture"
)

// seedResultArtifact 使用实际检查器构造报告，不用单独的 Passed 标志冒充证据。
func seedResultArtifact(t *testing.T, s *Service, id string) Session {
	t.Helper()
	v, err := s.store.Edit(context.Background(), id, func(v *Session) error {
		v.Intent = &Intent{Asset: "茶壶", Use: "产品展示", MaxTriangles: 5000, MaxBytes: 10 << 20, Plan: []string{"生成", "检查"}}
		v.Artifacts = []Artifact{{ID: "candidate-a", TaskID: "task-a", Path: "/private/hidden-artifact.glb", SourceURL: "https://private.example/file?secret=hidden", Report: asset.Inspect(testfixture.Cube(2645), 5000, 10<<20)}}
		v.Current = &Operation{ID: "candidate-a", ArtifactID: "candidate-a", TaskID: "task-a", Kind: "generate", Stage: "done"}
		v.Production, v.ModelCalls = 1, 6
		v.Deadline = time.Now().UTC().Add(time.Hour)
		return nil
	}, "technical_report", map[string]string{"source": "controlled-test"})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func newResultFixture(t *testing.T) (*Service, Session) {
	t.Helper()
	s := testService(t, t.TempDir(), &fakeProvider{}, false)
	t.Cleanup(func() { s.Close() })
	v, err := s.Create(context.Background(), "owner", "产品展示用的静态茶壶")
	if err != nil {
		t.Fatal(err)
	}
	return s, seedResultArtifact(t, s, v.ID)
}

func TestVerifiedFinishPreservesProposalWithoutAdoptingItsFacts(t *testing.T) {
	for _, deliver := range []bool{true, false} {
		t.Run(map[bool]string{true: "deliver", false: "agent-stop"}[deliver], func(t *testing.T) {
			s, v := newResultFixture(t)
			claim := "共1000面，绑定和动画已完成，全部预算已经耗尽。"
			_, err := s.finishRequest(context.Background(), v.ID, &pauseCoordinator{mode: "normal"}, &finishInput{Deliver: deliver, ArtifactID: "candidate-a", Explanation: claim})
			if err != nil {
				t.Fatal(err)
			}
			got := getProductionFixture(t, s, v.ID)
			if !got.Terminal() || got.Result == nil || strings.Contains(got.Final, "1000") || strings.Contains(got.Final, "绑定和动画已完成") || strings.Contains(got.Final, claim) {
				t.Fatalf("free text became official result: %+v", got.Result)
			}
			if deliver && (got.Status != "completed" || got.SelectedArtifact != "candidate-a" || !strings.Contains(got.Final, "2645")) {
				t.Fatalf("missing measured delivery: %s", got.Final)
			}
			if !deliver && (got.Status != "failed" || got.SelectedArtifact != "" || got.Result.Reason != "agent_stop") {
				t.Fatalf("agent stop adopted unsupported outcome: %+v", got.Result)
			}
			events, _ := s.store.Events(context.Background(), v.ID, 0)
			var found bool
			for _, e := range events {
				if e.Kind != "agent_finished" {
					continue
				}
				var data map[string]any
				if err = json.Unmarshal(e.Data, &data); err != nil {
					t.Fatal(err)
				}
				found = data["explanation"] == claim && data["raw_text_verification"] == "unverifiable" && data["final"] == got.Final && data["result"] != nil
			}
			if !found || resultEvidenceCheck(got).Status != "passed" {
				t.Fatal("missing separate proposal and committed evidence")
			}
		})
	}
}

func TestVerifiedFinishRejectsUnsupportedReferences(t *testing.T) {
	for _, mode := range []string{"missing", "other-session", "failed", "flag-only", "mismatched-limit"} {
		t.Run(mode, func(t *testing.T) {
			s, v := newResultFixture(t)
			id := "candidate-a"
			switch mode {
			case "missing":
				id = "absent"
			case "other-session":
				other, err := s.Create(context.Background(), "other-owner", "另一个资产")
				if err != nil {
					t.Fatal(err)
				}
				seedResultArtifact(t, s, other.ID)
				editProductionFixture(t, s, v.ID, func(v *Session) { v.Artifacts = nil })
			case "failed":
				editProductionFixture(t, s, v.ID, func(v *Session) { v.Artifacts[0].Report = asset.Inspect(testfixture.Cube(6000), 5000, 10<<20) })
			case "flag-only":
				editProductionFixture(t, s, v.ID, func(v *Session) { v.Artifacts[0].Report = asset.Report{Passed: true} })
			case "mismatched-limit":
				editProductionFixture(t, s, v.ID, func(v *Session) { v.Intent.MaxTriangles = 1000 })
			}
			_, err := s.finishRequest(context.Background(), v.ID, &pauseCoordinator{mode: "normal"}, &finishInput{Deliver: true, ArtifactID: id, Explanation: "已经全部成功"})
			if err != nil {
				t.Fatal(err)
			}
			got := getProductionFixture(t, s, v.ID)
			if got.Terminal() || got.Result != nil || got.SelectedArtifact != "" || productionEventCount(t, s, v.ID, "agent_finished") != 0 || productionEventCount(t, s, v.ID, "runtime_blocked") != 1 {
				t.Fatal("invalid delivery became committed result or lost refusal evidence")
			}
		})
	}
}

func TestVerifiedResultCommitRollbackAndIdempotency(t *testing.T) {
	s, v := newResultFixture(t)
	_, err := s.store.db.Exec("CREATE TRIGGER reject_result BEFORE INSERT ON events WHEN NEW.kind = 'agent_finished' BEGIN SELECT RAISE(ABORT, 'injected result event failure'); END")
	if err != nil {
		t.Fatal(err)
	}
	in := &finishInput{Deliver: true, ArtifactID: "candidate-a", Explanation: "模型原始说明"}
	if _, err = s.finishRequest(context.Background(), v.ID, &pauseCoordinator{mode: "normal"}, in); err == nil {
		t.Fatal("storage failure did not propagate")
	}
	got := getProductionFixture(t, s, v.ID)
	if got.Terminal() || got.Result != nil || got.Final != "" || got.SelectedArtifact != "" || productionEventCount(t, s, v.ID, "runtime_blocked") != 0 {
		t.Fatal("event failure committed half a result")
	}
	if _, err = s.store.db.Exec("DROP TRIGGER reject_result"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.finishRequest(context.Background(), v.ID, &pauseCoordinator{mode: "normal"}, in); err != nil {
		t.Fatal(err)
	}
	before := getProductionFixture(t, s, v.ID)
	if err = s.finalize(v.ID, errors.New("late failure")); err != nil {
		t.Fatal(err)
	}
	if err = s.Stop(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	after := getProductionFixture(t, s, v.ID)
	if !reflect.DeepEqual(before, after) || productionEventCount(t, s, v.ID, "agent_finished") != 1 || productionEventCount(t, s, v.ID, "runtime_finished") != 0 || productionEventCount(t, s, v.ID, "stopped") != 0 {
		t.Fatal("duplicate termination changed state or committed another end event")
	}
}

func TestVerifiedResultStopRaceAndRuntimeReasons(t *testing.T) {
	t.Run("late-valid-finish-is-not-agent-violation", func(t *testing.T) {
		s, v := newResultFixture(t)
		if err := s.Stop(context.Background(), v.ID); err != nil {
			t.Fatal(err)
		}
		before := getProductionFixture(t, s, v.ID)
		_, err := s.finishRequest(context.Background(), v.ID, &pauseCoordinator{mode: "normal"}, &finishInput{Deliver: true, ArtifactID: "candidate-a", Explanation: "完成"})
		if !errors.Is(err, ErrClosed) || productionEventCount(t, s, v.ID, "runtime_blocked") != 0 || !reflect.DeepEqual(before, getProductionFixture(t, s, v.ID)) {
			t.Fatal("late valid decision was blamed on Agent or overwrote stop")
		}
	})
	t.Run("stop-versus-delivery", func(t *testing.T) {
		s, v := newResultFixture(t)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = s.Stop(context.Background(), v.ID) }()
		go func() {
			defer wg.Done()
			_, _ = s.finishRequest(context.Background(), v.ID, &pauseCoordinator{mode: "normal"}, &finishInput{Deliver: true, ArtifactID: "candidate-a", Explanation: "完成"})
		}()
		wg.Wait()
		got := getProductionFixture(t, s, v.ID)
		if got.Result == nil || got.Result.Status != got.Status || resultEvidenceCheck(got).Status != "passed" || productionEventCount(t, s, v.ID, "stopped")+productionEventCount(t, s, v.ID, "agent_finished") != 1 {
			t.Fatal("race produced inconsistent terminal evidence")
		}
	})
	for _, mode := range []string{"model_budget", "no-asset-budget", "production_budget", "execution_deadline", "idle_timeout", "submission_unknown", "recovery_failed", "model_failed", "operation_failed", "execution_failed"} {
		t.Run(mode, func(t *testing.T) {
			s, v := newResultFixture(t)
			cause := errors.New("远端已取消，绑定完成")
			want := mode
			editProductionFixture(t, s, v.ID, func(v *Session) {
				switch mode {
				case "model_budget", "no-asset-budget":
					v.ModelCalls = v.Limits.Calls
					cause = ErrBudget
					want = "model_budget"
					if mode == "no-asset-budget" {
						v.Artifacts = nil
					}
				case "production_budget":
					v.Production = v.Limits.Submissions
					cause = ErrBudget
				case "execution_deadline":
					v.Deadline = time.Now().Add(-time.Second)
				case "idle_timeout":
					v.Deadline = time.Time{}
					v.LastUser = time.Now().Add(-25 * time.Hour)
				case "submission_unknown":
					v.Current.Stage, v.Current.TaskID = "submitting", ""
				case "recovery_failed":
					cause = recoveryError("recovery_seed_invalid")
				case "model_failed":
					cause = &modelCallError{cause: cause}
				case "operation_failed":
					v.Current.Error = cause.Error()
				}
			})
			if err := s.finalize(v.ID, cause); err != nil {
				t.Fatal(err)
			}
			got := getProductionFixture(t, s, v.ID)
			if got.Result == nil || got.Result.Reason != want || strings.Contains(got.Final, "远端已取消") || strings.Contains(got.Final, "绑定完成") {
				t.Fatalf("unsupported cause promoted: %s %+v", got.Final, got.Result)
			}
			if (got.Status == "completed") != (mode == "model_budget" || mode == "production_budget") {
				t.Fatalf("wrong delivery boundary: %s", got.Status)
			}
			events, err := s.store.Events(context.Background(), v.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			last := events[len(events)-1]
			var data map[string]any
			if err = json.Unmarshal(last.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data["raw_text_verification"] != "unverifiable" || data["raw_text_source"] != "execution_error" || data["final"] != got.Final {
				t.Fatal("raw runtime error lost source or committed result")
			}
		})
	}
}

func TestVerifiedResultHTTPWebSocketAndExportAgree(t *testing.T) {
	for _, mode := range []string{"current", "history-supported", "history-insufficient"} {
		t.Run(mode, func(t *testing.T) {
			s := testService(t, t.TempDir(), &fakeProvider{}, false)
			defer s.Close()
			server := httptest.NewServer(s.Handler())
			defer server.Close()
			jar, _ := cookiejar.New(nil)
			client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
			res, err := client.Post(server.URL+"/api/sessions", "application/json", strings.NewReader(`{"request":"产品展示静态茶壶"}`))
			if err != nil {
				t.Fatal(err)
			}
			var created struct {
				ID string `json:"id"`
			}
			err = json.NewDecoder(res.Body).Decode(&created)
			res.Body.Close()
			if err != nil || created.ID == "" {
				t.Fatalf("create: %v", err)
			}
			seedResultArtifact(t, s, created.ID)
			if mode == "current" {
				_, err = s.finishRequest(context.Background(), created.ID, &pauseCoordinator{mode: "normal"}, &finishInput{Deliver: true, ArtifactID: "candidate-a", Explanation: "1000面，绑定完成"})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				editProductionFixture(t, s, created.ID, func(v *Session) {
					v.SelectedArtifact = "candidate-a"
					if mode == "history-insufficient" {
						v.Artifacts[0].Report.Checks = nil
					}
					v.Finish("completed", "历史原始错误说明：绑定完成")
				})
			}
			before := getProductionFixture(t, s, created.ID)
			read := func(path string) map[string]any {
				t.Helper()
				res, err := client.Get(server.URL + path)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				var snap map[string]any
				if err = json.NewDecoder(res.Body).Decode(&snap); err != nil {
					t.Fatal(err)
				}
				return snap
			}
			path := "/api/sessions/" + created.ID
			ordinary, exported := read(path), read(path+"/trace")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+path+"/events", &websocket.DialOptions{HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			var streamed map[string]any
			if err = wsjson.Read(ctx, conn, &streamed); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(ordinary["session"], exported["session"]) || !reflect.DeepEqual(ordinary["session"], streamed["session"]) {
				t.Fatal("output surfaces disagree")
			}
			view := ordinary["session"].(map[string]any)
			if mode == "history-insufficient" {
				arts := view["artifacts"].([]any)
				if view["status"] == "completed" || arts[0].(map[string]any)["report"].(map[string]any)["passed"] == true {
					t.Fatal("unsupported historical candidate still passes")
				}
			}
			if strings.Contains(jsonString(exported), "hidden-artifact") || strings.Contains(jsonString(exported), "private.example") {
				t.Fatal("result projection leaked internal artifact locations")
			}
			if !reflect.DeepEqual(before, getProductionFixture(t, s, created.ID)) {
				t.Fatal("read projection mutated original history")
			}
			unowned, err := http.Get(server.URL + path + "/trace")
			if err != nil {
				t.Fatal(err)
			}
			unowned.Body.Close()
			if unowned.StatusCode != http.StatusNotFound {
				t.Fatal("result evidence crossed visitor boundary")
			}
		})
	}
}
