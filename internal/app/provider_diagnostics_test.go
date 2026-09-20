package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/lkmaVanilla/tripo-3d-agent/internal/testfixture"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
)

func TestProviderUploadDiagnosticsKeepPreparationBudget(t *testing.T) {
	s, v, _ := conversationInputFixture(t)
	defer s.Close()
	p := &conversationProvider{failUploads: true}
	s.provider = p
	if _, err := s.production(context.Background(), v.ID, v.Current.ID); err != nil {
		t.Fatal(err)
	}
	ds := diagnosticsFor(t, s, v.ID)
	got := getProductionFixture(t, s, v.ID)
	if len(ds) != 3 || got.Current.PreparedInput.Attempts != 3 || got.Production != 0 || len(p.uploads) != 3 || len(p.params) != 0 {
		t.Fatal("upload diagnostics changed budget", len(ds), got.Current)
	}
	for i, d := range ds {
		if d.Phase != "upload" || d.Attempt != i+1 || !d.RequestStarted {
			t.Fatal(d)
		}
	}
	if _, err := s.production(context.Background(), v.ID, v.Current.ID); err != nil || len(p.uploads) != 3 {
		t.Fatal("upload failure replayed", err)
	}
}

type diagnosticConversationProvider struct{ *conversationProvider }

func (p diagnosticConversationProvider) Submit(ctx context.Context, kind string, params tripo.Params) (string, error) {
	p.conversationProvider.Submit(ctx, kind, params)
	return "", diagnosticFixtureError("submit", "timeout", true)
}

func TestProviderConversationHTTPWebSocketAndExportAgree(t *testing.T) {
	p := &conversationProvider{}
	s := newConversationTestService(t, p)
	s.provider = diagnosticConversationProvider{p}
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	conversationHTTP(t, client, "GET", server.URL+"/api/config", nil)
	status, b := conversationHTTP(t, client, "POST", server.URL+"/api/conversations", map[string]string{"request": "产品展示木箱", "client_message_id": "diagnostic-chat"})
	if status != 201 {
		t.Fatal(status, string(b))
	}
	created := conversationHTTPSnapshot(t, b)
	id := created.Conversation.ID
	runID := created.Conversation.ActiveRunID
	waitConversationIdle(t, s, id)
	base := server.URL + "/api/conversations/" + id
	var first tripo.Diagnostic
	var final string
	for _, path := range []string{base, base + "/trace", base} {
		status, b = conversationHTTP(t, client, "GET", path, nil)
		if status != 200 {
			t.Fatal(status, string(b))
		}
		if strings.Contains(string(b), "provider-secret") || strings.Contains(string(b), "sign=") {
			t.Fatal("HTTP leaked error")
		}
		snap := conversationHTTPSnapshot(t, b)
		n := 0
		cards := 0
		for _, event := range snap.Events {
			if event.Kind == "provider_call_failed" {
				var d tripo.Diagnostic
				json.Unmarshal(event.Data, &d)
				n++
				if first.AttemptID == "" {
					first = d
				}
				if !reflect.DeepEqual(d, first) {
					t.Fatal("different diagnosis across reads")
				}
			}
		}
		for _, m := range snap.Messages {
			if m.Kind == "operation_card" {
				cards++
				if m.Data["status"] != "submission_unknown" || !strings.Contains(m.Data["error_summary"].(string), "请求超时") {
					t.Fatal(m.Data)
				}
			}
		}
		if n != 1 || cards != 1 || len(snap.Runs) != 1 {
			t.Fatal("missing or duplicated evidence", n, cards)
		}
		text := snap.Runs[0]["final"].(string)
		if final == "" {
			final = text
		}
		if final != text || !strings.Contains(text, "没有自动重试") {
			t.Fatal(text)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		socket, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/events", &websocket.DialOptions{HTTPClient: client})
		if err != nil {
			t.Fatal(err)
		}
		var snap ConversationSnapshot
		err = wsjson.Read(ctx, socket, &snap)
		socket.CloseNow()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, event := range snap.Events {
			if event.Kind == "provider_call_failed" {
				var d tripo.Diagnostic
				json.Unmarshal(event.Data, &d)
				if !reflect.DeepEqual(first, d) {
					t.Fatal("WebSocket changed diagnosis")
				}
				found = true
			}
		}
		if !found || strings.Contains(jsonString(snap), "provider-secret") {
			t.Fatal("WebSocket missing or leaking diagnosis")
		}
	}
	for _, path := range []string{base, base + "/trace", base + "/events", server.URL + "/api/sessions/" + runID + "/trace"} {
		status, _ = conversationHTTP(t, http.DefaultClient, "GET", path, nil)
		if status != 404 {
			t.Fatal("foreign access allowed", status)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.params) != 1 {
		t.Fatal("read/reconnect caused production", len(p.params))
	}
}

func diagnosticFixtureError(phase, category string, unknown bool) error {
	return &tripo.APIError{Unknown: unknown, Detail: tripo.Diagnostic{Phase: phase, Category: category, RequestStarted: true, ProviderTraceID: "trace-123", Message: "Authorization: Bearer provider-secret; https://cdn.example/model?sign=provider-secret file_secret"}}
}

func diagnosticsFor(t *testing.T, s *Service, id string) []tripo.Diagnostic {
	t.Helper()
	events, err := s.store.Events(context.Background(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []tripo.Diagnostic
	for _, e := range events {
		if e.Kind == "provider_call_failed" {
			var d tripo.Diagnostic
			if err = json.Unmarshal(e.Data, &d); err != nil {
				t.Fatal(err)
			}
			out = append(out, d)
		}
	}
	return out
}

func TestProviderUnknownDiagnosticAndRecovery(t *testing.T) {
	for _, failDiagnostic := range []bool{false, true} {
		t.Run(map[bool]string{false: "recorded", true: "record_failure"}[failDiagnostic], func(t *testing.T) {
			s, v, p := productionRecoveryFixture(t, "ready")
			if failDiagnostic {
				if _, err := s.store.db.Exec(`CREATE TRIGGER fail_diagnostic BEFORE INSERT ON events WHEN NEW.kind='provider_call_failed' BEGIN SELECT RAISE(ABORT,'injected diagnostic failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			p.submit = func(context.Context) (string, error) { return "", diagnosticFixtureError("submit", "timeout", true) }
			_, err := s.production(context.Background(), v.ID, v.Current.ID)
			if err == nil || !tripo.IsUnknown(err) {
				t.Fatal(err)
			}
			got := getProductionFixture(t, s, v.ID)
			if got.Current.Stage != "submitting" || got.Current.TaskID != "" || got.Production != 1 || p.submits != 1 {
				t.Fatal("unknown state lost", got.Current, p.submits)
			}
			ds := diagnosticsFor(t, s, v.ID)
			if !failDiagnostic && (len(ds) != 1 || ds[0].Phase != "submit" || ds[0].RunID != v.ID || ds[0].DurationMS < 0 || ds[0].HTTPStatus != nil || !ds[0].SubmissionUnknown) {
				t.Fatal(ds)
			}
			if failDiagnostic && (len(ds) != 0 || got.Current.LastFailure != nil) {
				t.Fatal("failed transaction leaked state")
			}
			if _, err = s.production(context.Background(), v.ID, v.Current.ID); err == nil || p.submits != 1 {
				t.Fatal("tool re-entry resubmitted")
			}
			s2, err := New(s.Config)
			if err != nil {
				t.Fatal(err)
			}
			defer s2.Close()
			s2.provider = p
			if err = s2.Start(); err != nil {
				t.Fatal(err)
			}
			after := getProductionFixture(t, s2, v.ID)
			if !after.Terminal() || p.submits != 1 || after.Production != 1 || after.Result.Reason != "submission_unknown" {
				t.Fatal("restart changed unknown submission", after.Result)
			}
			if !failDiagnostic && !strings.Contains(after.Final, "请求超时") {
				t.Fatal(after.Final)
			}
		})
	}
}

func TestProviderDiagnosticDoesNotRollbackSubmission(t *testing.T) {
	for _, kind := range []string{"tool_submitting", "tool_submitted", "provider_call_failed"} {
		t.Run(kind, func(t *testing.T) {
			s, v, p := productionRecoveryFixture(t, "ready")
			_, err := s.store.db.Exec(`CREATE TRIGGER fail_write BEFORE INSERT ON events WHEN NEW.kind='` + kind + `' BEGIN SELECT RAISE(ABORT,'injected failure'); END`)
			if err != nil {
				t.Fatal(err)
			}
			p.query = func(_ context.Context, id string) (tripo.Task, error) {
				return tripo.Task{}, diagnosticFixtureError("query", "http", false)
			}
			_, _ = s.production(context.Background(), v.ID, v.Current.ID)
			got := getProductionFixture(t, s, v.ID)
			switch kind {
			case "tool_submitting":
				if p.submits != 0 || got.Production != 0 || got.Current.Stage != "ready" {
					t.Fatal("submitted without durable intent")
				}
			case "tool_submitted":
				if p.submits != 1 || got.Current.TaskID != "" || got.Current.Stage != "submitting" {
					t.Fatal("bad identity save failure")
				}
				_, _ = s.production(context.Background(), v.ID, v.Current.ID)
				if p.submits != 1 {
					t.Fatal("resubmitted after task save failed")
				}
			case "provider_call_failed":
				if p.submits != 1 || got.Current.TaskID != "known-task" || got.Current.Stage != "submitted" || p.queries != 3 {
					t.Fatal("diagnostic failure changed task or retries", got.Current, p.queries)
				}
			}
		})
	}
}

func TestProviderIntermediateFailuresPersistAndResolve(t *testing.T) {
	for _, phase := range []string{"query", "download"} {
		t.Run(phase, func(t *testing.T) {
			s, v, p := productionRecoveryFixture(t, "ready")
			if phase == "query" {
				p.query = func(_ context.Context, id string) (tripo.Task, error) {
					if p.queries < 3 {
						return tripo.Task{}, diagnosticFixtureError("query", "connect", false)
					}
					task := tripo.Task{ID: id, Status: "success", Progress: 100}
					task.Output.ModelURL = "https://fixture.example/model.glb"
					return task, nil
				}
			} else {
				p.download = func(context.Context) ([]byte, error) {
					if p.downloads < 3 {
						return nil, diagnosticFixtureError("download", "connect", false)
					}
					return testfixture.Cube(12), nil
				}
			}
			if _, err := s.production(context.Background(), v.ID, v.Current.ID); err != nil {
				t.Fatal(err)
			}
			ds := diagnosticsFor(t, s, v.ID)
			if len(ds) != 2 || ds[0].Phase != phase || ds[0].Attempt != 1 || ds[1].Attempt != 2 || ds[0].RetryGroupID != ds[1].RetryGroupID || ds[0].AttemptID == ds[1].AttemptID {
				t.Fatal(ds)
			}
			after := getProductionFixture(t, s, v.ID)
			if after.Current.LastFailure != nil || after.Current.Stage != "done" || p.submits != 1 {
				t.Fatal("resolved failure overrides success")
			}
			after.SelectedArtifact = after.Current.ArtifactID
			if err := after.finishVerified("completed", "runtime", "delivered"); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(after.Final, "连接失败") || strings.Contains(after.Final, "provider-secret") {
				t.Fatal(after.Final)
			}
		})
	}
}

func TestProviderTaskFailureAndSafeLogs(t *testing.T) {
	s, v, p := productionRecoveryFixture(t, "ready")
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)
	status, code := 200, 0
	p.query = func(_ context.Context, id string) (tripo.Task, error) {
		return tripo.Task{ID: id, Status: "failed", ErrorCode: 42, ErrorMessage: "provider-secret https://cdn.example?sig=provider-secret", Call: tripo.Diagnostic{HTTPStatus: &status, ProviderCode: &code, ProviderTraceID: "trace-123"}}, nil
	}
	if _, err := s.production(context.Background(), v.ID, v.Current.ID); err != nil {
		t.Fatal(err)
	}
	got := getProductionFixture(t, s, v.ID)
	ds := diagnosticsFor(t, s, v.ID)
	if len(ds) != 1 || ds[0].Category != "task_failed" || ds[0].TaskErrorCode == nil || *ds[0].TaskErrorCode != 42 || ds[0].TaskID != "known-task" || *ds[0].HTTPStatus != 200 {
		t.Fatal(ds)
	}
	if got.Current.Stage != "done" || got.Production != 1 || got.Current.LastFailure == nil {
		t.Fatal(got.Current)
	}
	if err := s.finalize(v.ID, errors.New("operation failed")); err != nil {
		t.Fatal(err)
	}
	events, _ := s.store.Events(context.Background(), v.ID, 0)
	for _, text := range []string{logs.String(), jsonString(events), jsonString(getProductionFixture(t, s, v.ID)), jsonString(s.sanitize(s.snapshot(getProductionFixture(t, s, v.ID), events)))} {
		if strings.Contains(text, "provider-secret") || strings.Contains(text, "sig=") {
			t.Fatal("raw error escaped", text)
		}
	}
}

func TestProviderDownloadExhaustionExplainsDeliveryFailure(t *testing.T) {
	s, v, p := productionRecoveryFixture(t, "ready")
	p.download = func(context.Context) ([]byte, error) {
		return nil, diagnosticFixtureError("download", "connect", false)
	}
	_, err := s.production(context.Background(), v.ID, v.Current.ID)
	if err != nil || p.downloads != 3 || p.submits != 1 {
		t.Fatal("download budget changed", err, p.downloads, p.submits)
	}
	if err = s.finalize(v.ID, errors.New("download failed")); err != nil {
		t.Fatal(err)
	}
	ds := diagnosticsFor(t, s, v.ID)
	if len(ds) != 3 || ds[2].Attempt != 3 || ds[0].RetryGroupID != ds[2].RetryGroupID {
		t.Fatal(ds)
	}
	after := getProductionFixture(t, s, v.ID)
	if !strings.Contains(after.Final, "远端任务已成功") || !strings.Contains(after.Final, "尚未完成本地交付") || strings.Contains(after.Final, "供应商拒绝") {
		t.Fatal(after.Final)
	}
}

func TestProviderKnownTaskNewAttemptsAfterRestart(t *testing.T) {
	s, v, p := productionRecoveryFixture(t, "submitted")
	first := diagnosticFixtureError("query", "http", false)
	s.recordProviderFailure(v.ID, v.Current.ID, v.Current.TaskID, "query", newID(), 1, time.Now(), first)
	s2, err := New(s.Config)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	s2.provider = p
	p.query = func(_ context.Context, id string) (tripo.Task, error) {
		return tripo.Task{ID: id, Status: "failed", ErrorCode: 77}, nil
	}
	if _, err = s2.production(context.Background(), v.ID, v.Current.ID); err != nil {
		t.Fatal(err)
	}
	ds := diagnosticsFor(t, s2, v.ID)
	if len(ds) != 2 || ds[0].RetryGroupID == ds[1].RetryGroupID || ds[0].AttemptID == ds[1].AttemptID || ds[1].TaskID != v.Current.TaskID || p.submits != 0 {
		t.Fatal(ds, p.submits)
	}
}
