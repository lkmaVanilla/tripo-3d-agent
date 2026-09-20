package app

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
	"modernc.org/sqlite"
)

// checkpointCrashMarker 标识子进程当前到达的会话、代次和故障边界。
// 屏障只存在于测试二进制中；父进程在真实持久化或 Runtime 操作中强杀子进程，
// 不使用 Service.Close、手动回滚或返回模拟错误替代崩溃。
type checkpointCrashMarker struct {
	SessionID string
	Mode      string
	Round     int
}

// checkpointCrashBarrier 原子发布到达标记，避免父进程读到半个 JSON 后误判边界。
func checkpointCrashBarrier(path string, marker checkpointCrashMarker) {
	if err := atomicWrite(path, []byte(jsonString(marker))); err != nil {
		panic(err)
	}
	select {} // 仅由父进程的 SIGKILL 结束，保持当前调用和事务悬停。
}

// checkpointCrashStore 包装真实协调器，仅在指定代次的 Set 前后阻塞，
// 让后续轮次故障保留前一轮真实检查点而不是人工拼接状态。
type checkpointCrashStore struct {
	*pauseCoordinator
	mode, markerPath string
	round            int
}

func (s checkpointCrashStore) Set(ctx context.Context, key string, data []byte) error {
	target := s.pause != nil && s.pause.Point.Generation == int64(s.round)
	marker := checkpointCrashMarker{SessionID: key, Mode: s.mode, Round: s.round}
	if target && (s.mode == "before_set" || s.mode == "correction_before_set") {
		checkpointCrashBarrier(s.markerPath, marker)
	}
	if err := s.pauseCoordinator.Set(ctx, key, data); err != nil {
		return err
	}
	if target && (s.mode == "after_commit" || s.mode == "queue_after_commit" || s.mode == "queue_full_after_commit") {
		// 业务事务已持久化，但外层 Eino Runner 尚未收到 Set 返回值或发出中断事件。
		checkpointCrashBarrier(s.markerPath, marker)
	}
	return nil
}

// checkpointCrashAnswerTool 在真实 ask_user 返回后阻塞，隔离答案已消费但下一检查点未生成的窗口。
type checkpointCrashAnswerTool struct {
	tool.InvokableTool
	markerPath string
	marker     checkpointCrashMarker
}

func (t checkpointCrashAnswerTool) InvokableRun(ctx context.Context, args string, opts ...tool.Option) (string, error) {
	result, err := t.InvokableTool.InvokableRun(ctx, args, opts...)
	if err == nil && strings.Contains(result, "user_answer") {
		// 真实 ask_user 已返回并清空显示字段，但结果尚未进入 ToolsNode 或下一次模型调用。
		checkpointCrashBarrier(t.markerPath, t.marker)
	}
	return result, err
}

// checkpointCrashQuestionModel 按协议中的工具答案数推进澄清，最多走完指定轮次后生产。
// 这样可验证第二、第三轮恢复确实沿用原答案，而不是只让最终状态碰巧成功。
type checkpointCrashQuestionModel struct{ rounds int }

// checkpointCrashGenerationModel 直接驱动现有意图与生产工具；暂停恢复前不能提交 Provider 任务。
type checkpointCrashGenerationModel struct{}

// checkpointCrashCorrectionModel 对首次实测超面数产物给出固定减面提议，用于验证纠偏草案恢复。
type checkpointCrashCorrectionModel struct{}

func (checkpointCrashCorrectionModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	var state struct {
		Artifacts []Artifact `json:"artifacts"`
	}
	for _, msg := range in {
		if i := strings.LastIndex(msg.Content, "<runtime_state>"); i >= 0 {
			body := strings.Split(msg.Content[i+len("<runtime_state>"):], "</runtime_state>")[0]
			if err := json.Unmarshal([]byte(body), &state); err != nil {
				return nil, err
			}
		}
	}
	if len(state.Artifacts) > 0 {
		a := state.Artifacts[len(state.Artifacts)-1]
		return protocolProposal("decimate_asset", "accepted-correction-call", decimationInput{ArtifactID: a.ID, TargetTriangles: 4000, Reason: "实测6000三角面超过5000，选择减面"}), nil
	}
	return (checkpointCrashGenerationModel{}).Generate(ctx, in, opts...)
}

func (m checkpointCrashCorrectionModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

func (checkpointCrashGenerationModel) Generate(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	for _, msg := range in {
		for _, call := range msg.ToolCalls {
			if call.Function.Name == "set_intent" {
				return protocolProposal("generate_asset", "queued-production-call", generationInput{Prompt: "A stylized static wooden crate", TargetTriangles: 5000, TextureQuality: "standard", Reason: "已明确静态卡通木箱需求"}), nil
			}
		}
	}
	return protocolProposal("set_intent", "queued-intent-call", Intent{Asset: "木箱", Use: "游戏原型", Style: "卡通", MaxTriangles: 5000, MaxBytes: 10 << 20, Constraints: []string{"静态", "卡通木箱"}, Plan: []string{"生成", "技术检查", "交付"}}), nil
}

func (m checkpointCrashGenerationModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

func (m checkpointCrashQuestionModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	answers := 0
	for _, msg := range in {
		if msg.Role == schema.Tool && strings.Contains(msg.Content, "user_answer") {
			answers++
		}
	}
	if answers < m.rounds {
		return protocolProposal("ask_user", fmt.Sprintf("crash-question-%d", answers+1), questionInput{Question: fmt.Sprintf("第%d个关键问题：卡通木箱是否保持静态？", answers+1)}), nil
	}
	return (scriptModel{}).Generate(ctx, in, opts...)
}

func (m checkpointCrashQuestionModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

// checkpointCrashProvider 访问父进程托管的受控 Provider，子进程与恢复后的 Service 共用该服务。
// 提交次数留在父进程中，SIGKILL 不会擦掉重复生产的证据。
type checkpointCrashProvider struct{ endpoint string }

func (p checkpointCrashProvider) request(ctx context.Context, operation string, input, output any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+"/"+operation, strings.NewReader(jsonString(input)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("controlled provider failed with HTTP %d", resp.StatusCode)
	}
	if b, ok := output.(*[]byte); ok {
		*b, err = io.ReadAll(resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(output)
}

func (p checkpointCrashProvider) Submit(ctx context.Context, kind string, params tripo.Params) (string, error) {
	var id string
	err := p.request(ctx, "submit", struct {
		Kind   string
		Params tripo.Params
	}{kind, params}, &id)
	return id, err
}
func (p checkpointCrashProvider) Query(ctx context.Context, id string) (tripo.Task, error) {
	var task tripo.Task
	err := p.request(ctx, "query", id, &task)
	return task, err
}
func (p checkpointCrashProvider) Download(ctx context.Context, url string) ([]byte, error) {
	var b []byte
	err := p.request(ctx, "download", url, &b)
	return b, err
}

// checkpointCrashProviderServer 将内存任务夹具暴露为本地 HTTP 服务，延长证据生命周期至父用例结束。
// 返回的 GLB 均为合成数据，覆盖恢复和技术检查，不构成真实 API 或完整 Agent 评测。
func checkpointCrashProviderServer(t *testing.T) (*httptest.Server, *fakeProvider) {
	t.Helper()
	p := &fakeProvider{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var output any
		var err error
		switch r.URL.Path {
		case "/submit":
			var input struct {
				Kind   string
				Params tripo.Params
			}
			if err = json.NewDecoder(r.Body).Decode(&input); err == nil {
				output, err = p.Submit(r.Context(), input.Kind, input.Params)
			}
		case "/query":
			var id string
			if err = json.NewDecoder(r.Body).Decode(&id); err == nil {
				output, err = p.Query(r.Context(), id)
			}
		case "/download":
			var url string
			if err = json.NewDecoder(r.Body).Decode(&url); err == nil {
				var b []byte
				b, err = p.Download(r.Context(), url)
				if err == nil {
					_, _ = w.Write(b)
					return
				}
			}
		default:
			err = fmt.Errorf("unexpected controlled provider operation")
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(output)
	}))
	t.Cleanup(server.Close)
	return server, p
}

// TestCheckpointCrashHelper 由父用例启动独立测试进程，在真实 Eino 暂停流程中走到指定屏障。
// 所有故障模式均由测试环境变量选择，不向生产入口增加配置项。
func TestCheckpointCrashHelper(t *testing.T) {
	mode := os.Getenv("TRIPO_TEST_CRASH_MODE")
	if mode == "" {
		t.Skip("only launched by the crash-test parent")
	}
	dir, markerPath := os.Getenv("TRIPO_TEST_CRASH_DIR"), os.Getenv("TRIPO_TEST_CRASH_MARKER")
	round, err := strconv.Atoi(os.Getenv("TRIPO_TEST_CRASH_ROUND"))
	if err != nil || round < 1 || round > 3 {
		t.Fatal("invalid test crash round")
	}
	marker := checkpointCrashMarker{Mode: mode, Round: round}
	if mode == "inside_transaction" {
		// 函数仅在此子进程打开 SQLite 前注册；TEMP 触发器随连接消失，
		// 恢复进程不会继承故障触发器，也不需要生产代码识别测试标记。
		err = sqlite.RegisterScalarFunction("tripo_checkpoint_crash_barrier", 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
			checkpointCrashBarrier(markerPath, marker)
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg := DefaultConfig()
	cfg.DataDir = dir
	queueMode := mode == "queue_after_commit" || mode == "queue_full_after_commit"
	if queueMode {
		cfg.ProductionSlots, cfg.QueueSize = 0, 1
		if mode == "queue_full_after_commit" {
			cfg.QueueSize = 0
		}
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// 故意不 defer s.Close()：到达屏障时保留活跃事务和 Runtime，由 SIGKILL 直接结束。
	s.ready, s.source = true, "controlled-crash-test"
	s.provider = checkpointCrashProvider{endpoint: os.Getenv("TRIPO_TEST_CRASH_PROVIDER")}
	s.modelFactory = func(context.Context) (model.BaseChatModel, error) {
		if mode == "correction_before_set" {
			return checkpointCrashCorrectionModel{}, nil
		}
		if queueMode {
			return checkpointCrashGenerationModel{}, nil
		}
		return checkpointCrashQuestionModel{rounds: round}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	v, err := s.Create(ctx, "crash-test-owner", "为游戏生成一个卡通静态木箱")
	if err != nil {
		t.Fatal(err)
	}
	marker.SessionID = v.ID
	if mode == "inside_transaction" {
		// 在检查点 INSERT/UPDATE 已执行、业务状态仍未提交时阻塞，检验 SQLite 整体回滚。
		_, err = s.store.db.ExecContext(ctx, fmt.Sprintf(`
CREATE TEMP TRIGGER checkpoint_crash_after_insert AFTER INSERT ON main.checkpoints WHEN json_extract(NEW.metadata, '$.Generation') = %d BEGIN SELECT tripo_checkpoint_crash_barrier(); END;
CREATE TEMP TRIGGER checkpoint_crash_after_update AFTER UPDATE ON main.checkpoints WHEN json_extract(NEW.metadata, '$.Generation') = %d BEGIN SELECT tripo_checkpoint_crash_barrier(); END;`, round, round))
		if err != nil {
			t.Fatal(err)
		}
	}
	for step := 1; step <= round; step++ {
		v, err = s.store.Get(ctx, v.ID)
		if err != nil {
			t.Fatal(err)
		}
		c := &pauseCoordinator{s: s, id: v.ID, mode: "normal", expected: v.generation(), resuming: v.ResumePoint != nil}
		tools, err := s.tools(v.ID, c)
		if err != nil {
			t.Fatal(err)
		}
		store := checkpointCrashStore{pauseCoordinator: c, mode: mode, markerPath: markerPath, round: round}
		base, _ := s.modelFactory(ctx)
		runner := newProtocolRunner(t, &countedModel{BaseChatModel: base, s: s, id: v.ID, coordinator: c}, store, tools, true)
		var iter *adk.AsyncIterator[*adk.AgentEvent]
		if v.ResumePoint == nil {
			iter = runner.Run(ctx, []*schema.Message{schema.UserMessage(v.Request)}, adk.WithCheckPointID(v.ID))
		} else {
			iter, err = runner.Resume(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
		}
		if n := drainProtocolEvents(t, iter); n != 1 {
			t.Fatalf("expected one prepared pause, got %d", n)
		}
		if mode == "correction_before_set" {
			continue // 先恢复生成操作，再推进到对应纠偏操作的下一次暂停。
		}
		if err := s.Answer(ctx, v.ID, fmt.Sprintf("第%d轮答案：保持卡通静态", step)); err != nil {
			t.Fatal(err)
		}
		if step == round && mode == "after_answer" {
			checkpointCrashBarrier(markerPath, marker)
		}
		if step == round && mode == "after_answer_tool" {
			v, err = s.store.Get(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			c = &pauseCoordinator{s: s, id: v.ID, mode: "normal", expected: v.generation(), resuming: true}
			tools, err = s.tools(v.ID, c)
			if err != nil {
				t.Fatal(err)
			}
			for i, candidate := range tools {
				info, err := candidate.Info(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if info.Name == "ask_user" {
					tools[i] = checkpointCrashAnswerTool{InvokableTool: candidate.(tool.InvokableTool), markerPath: markerPath, marker: marker}
				}
			}
			runner = newProtocolRunner(t, &countedModel{BaseChatModel: base, s: s, id: v.ID, coordinator: c}, c, tools, true)
			iter, err = runner.Resume(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			drainProtocolEvents(t, iter)
		}
	}
	t.Fatal("child missed its crash barrier")
}

// killCheckpointChildAtBarrier 等到带身份的屏障标记后才强杀子进程，并核对真实 SIGKILL 退出状态。
// 超时或提前退出均判失败，避免用固定延时杀进程却没有命中目标窗口。
func killCheckpointChildAtBarrier(t *testing.T, dir, endpoint, mode string, round int) checkpointCrashMarker {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(dir, "crash-barrier.json")
	cmd := exec.Command(executable, "-test.run=^TestCheckpointCrashHelper$", "-test.timeout=40s")
	cmd.Env = append(os.Environ(), "TRIPO_TEST_CRASH_MODE="+mode, "TRIPO_TEST_CRASH_DIR="+dir, "TRIPO_TEST_CRASH_MARKER="+markerPath, "TRIPO_TEST_CRASH_ROUND="+strconv.Itoa(round), "TRIPO_TEST_CRASH_PROVIDER="+endpoint)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = cmd.Process.Kill()
			<-done
		}
	})
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			stopped = true
			t.Fatalf("child exited before SIGKILL barrier: %v\n%s", err, output.String())
		case <-deadline.C:
			_ = cmd.Process.Kill()
			<-done
			stopped = true
			t.Fatalf("child did not reach crash barrier\n%s", output.String())
		case <-tick.C:
			b, err := os.ReadFile(markerPath)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			var marker checkpointCrashMarker
			if err := json.Unmarshal(b, &marker); err != nil {
				t.Fatal(err)
			}
			if marker.SessionID == "" || marker.Mode != mode || marker.Round != round {
				t.Fatalf("unexpected crash barrier identity: %+v", marker)
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			err = <-done
			stopped = true
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("child was not killed: %v\n%s", err, output.String())
			}
			status, ok := exit.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("expected SIGKILL, got %v\n%s", exit.ProcessState, output.String())
			}
			return marker
		}
	}
}

// TestCheckpointCrashRecoveryBoundaries 覆盖首次和后继暂停、事务内写入、已提交未通知及答案消费窗口。
// 每个用例从强杀后真实数据库恢复到受控交付，核对原身份、额度、答案和业务事件均不重复。
func TestCheckpointCrashRecoveryBoundaries(t *testing.T) {
	cases := []struct {
		name, mode string
		round      int
	}{
		{"F01_first_pending_before_set", "before_set", 1},
		{"F02_row_inserted_before_transaction_commit", "inside_transaction", 1},
		{"F02_replacement_before_transaction_commit", "inside_transaction", 2},
		{"F03_commit_before_interrupt_notification", "after_commit", 1},
		{"F04_second_pending_before_set", "before_set", 2},
		{"F04_third_pending_before_set", "before_set", 3},
		{"F05_answer_accepted_before_resume", "after_answer", 1},
		{"F05_answer_tool_returned_before_next_model", "after_answer_tool", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			server, provider := checkpointCrashProviderServer(t)
			marker := killCheckpointChildAtBarrier(t, dir, server.URL, tc.mode, tc.round)
			if provider.Count() != 0 {
				t.Fatal("a pending clarification caused remote production before the crash")
			}
			cfg := DefaultConfig()
			cfg.DataDir, cfg.PollInterval = dir, 5*time.Millisecond
			s, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			s.ready, s.source = true, "controlled-crash-test"
			s.provider = checkpointCrashProvider{endpoint: server.URL}
			s.modelFactory = func(context.Context) (model.BaseChatModel, error) {
				return checkpointCrashQuestionModel{rounds: tc.round}, nil
			}
			ctx := context.Background()
			before, err := s.store.Get(ctx, marker.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			pendingCrash := tc.mode == "before_set" || tc.mode == "inside_transaction"
			answeredCrash := tc.mode == "after_answer" || tc.mode == "after_answer_tool"
			wantWait := before.WaitID
			if pendingCrash {
				if before.PendingPause == nil || before.PendingPause.Point.Generation != int64(tc.round) || before.Clarifications != tc.round-1 || before.generation() != int64(tc.round-1) || before.Status == "awaiting_answer" {
					t.Fatalf("crash exposed half-committed waiting state: %+v", before)
				}
				wantWait = before.PendingPause.Point.RefID
				if tc.round == 1 {
					if _, err := s.store.LoadCheckpoint(ctx, before.ID, before.ID); !errors.Is(err, sql.ErrNoRows) {
						t.Fatalf("uncommitted checkpoint row survived SIGKILL: %v", err)
					}
				} else if record, err := s.store.LoadCheckpoint(ctx, before.ID, before.ID); err != nil || record.Point.Generation != int64(tc.round-1) {
					t.Fatalf("previous checkpoint lost during pending crash: point=%+v err=%v", record.Point, err)
				}
			} else if before.PendingPause != nil || before.ResumePoint == nil || before.generation() != int64(tc.round) || before.Clarifications != tc.round {
				t.Fatalf("committed pause did not survive SIGKILL: %+v", before)
			}
			if before.ModelCalls != tc.round || before.Production != 0 || before.HasSlot || !before.Deadline.IsZero() {
				t.Fatalf("pause crash changed execution budget or started production: %+v", before)
			}
			if answeredCrash && (before.Answers[wantWait].Text != "第1轮答案：保持卡通静态" || before.Status != "understanding") {
				t.Fatalf("accepted answer did not survive SIGKILL: %+v", before)
			}
			if tc.mode == "after_answer_tool" && before.Question != "" {
				t.Fatal("tool-return barrier was reached before ask_user consumed its answer")
			}
			// 显式对账后再启动（启动还会对账），验证重复执行不会创建另一个逻辑暂停。
			if err := s.reconcile(); err != nil {
				t.Fatal(err)
			}
			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			if !answeredCrash {
				waiting := waitSession(t, s, before.ID, func(v Session) bool { return v.Status == "awaiting_answer" && v.PendingPause == nil })
				if waiting.WaitID != wantWait || waiting.Clarifications != tc.round || waiting.ModelCalls != before.ModelCalls || waiting.generation() != int64(tc.round) {
					t.Fatalf("recovery repeated/changed the accepted decision: %+v", waiting)
				}
				if err := s.Answer(ctx, before.ID, fmt.Sprintf("第%d轮答案：保持卡通静态", tc.round)); err != nil {
					t.Fatal(err)
				}
			}
			final := waitSession(t, s, before.ID, func(v Session) bool { return v.Terminal() })
			if final.Status != "completed" || final.Clarifications != tc.round || len(final.Answers) != tc.round || final.Production != 1 || len(final.Artifacts) != 1 || provider.Count() != 1 {
				t.Fatalf("restart repeated questions, lost answers or duplicated production: %+v provider_submissions=%d", final, provider.Count())
			}
			for waitID, answer := range before.Answers {
				if final.Answers[waitID] != answer {
					t.Fatalf("previous accepted answer changed during resume: %s", waitID)
				}
			}
			if productionEventCount(t, s, before.ID, "clarification") != tc.round || productionEventCount(t, s, before.ID, "user_answer") != tc.round || productionEventCount(t, s, before.ID, "technical_report") != 1 {
				t.Fatal("SIGKILL recovery duplicated business fact events")
			}
			if !final.Artifacts[0].Report.Passed {
				t.Fatal("recovered execution delivered without a passing technical report")
			}
			if _, err := os.Stat(final.Artifacts[0].Path); err != nil {
				t.Fatalf("recovered execution did not persist the real controlled GLB: %v", err)
			}
		})
	}
}

// TestCheckpointCrashQueueRecovery 覆盖入队和队满状态已提交、Runner 尚未通知时的强杀窗口，
// 验证恢复不改变原操作和 FIFO 时间，队满必须由用户重试后才能进入生产。
func TestCheckpointCrashQueueRecovery(t *testing.T) {
	for _, mode := range []string{"queue_after_commit", "queue_full_after_commit"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			server, provider := checkpointCrashProviderServer(t)
			marker := killCheckpointChildAtBarrier(t, dir, server.URL, mode, 1)
			cfg := DefaultConfig()
			cfg.DataDir, cfg.ProductionSlots, cfg.QueueSize, cfg.PollInterval = dir, 0, 1, 5*time.Millisecond
			if mode == "queue_full_after_commit" {
				cfg.QueueSize = 0
			}
			s, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			s.ready, s.source = true, "controlled-crash-test"
			s.provider = checkpointCrashProvider{endpoint: server.URL}
			s.modelFactory = func(context.Context) (model.BaseChatModel, error) { return scriptModel{}, nil }
			ctx := context.Background()
			before, err := s.store.Get(ctx, marker.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus := "queued"
			if mode == "queue_full_after_commit" {
				wantStatus = "queue_full"
			}
			if before.Status != wantStatus || before.Current == nil || before.Current.Stage != "ready" || before.ResumePoint == nil || before.ResumePoint.RefID != before.Current.ID || before.PendingPause != nil || before.Queued.IsZero() {
				t.Fatalf("production pause was not committed before SIGKILL: %+v", before)
			}
			if before.HasSlot || before.Production != 0 || !before.Deadline.IsZero() || provider.Count() != 0 {
				t.Fatalf("queue pause started production: %+v submissions=%d", before, provider.Count())
			}
			operationID, queueTime := before.Current.ID, before.Queued
			if err := s.reconcile(); err != nil {
				t.Fatal(err)
			}
			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			// 显式执行多次调度，证明恢复本身不会自动重试队满请求，或提前消费槽位、次数和期限。
			for i := 0; i < 3; i++ {
				s.schedule()
			}
			after, err := s.store.Get(ctx, before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != wantStatus || after.Current.ID != operationID || !after.Queued.Equal(queueTime) || after.generation() != before.generation() || after.ModelCalls != before.ModelCalls || after.HasSlot || after.Production != 0 || !after.Deadline.IsZero() || provider.Count() != 0 {
				t.Fatalf("restart changed queue identity or advanced production: %+v", after)
			}
			if mode == "queue_full_after_commit" {
				if err := s.RetryQueue(ctx, before.ID); err == nil {
					t.Fatal("queue_full entered a queue with zero capacity")
				}
				s.mu.Lock()
				s.Config.QueueSize = 1
				s.mu.Unlock()
				if err := s.RetryQueue(ctx, before.ID); err != nil {
					t.Fatal(err)
				}
				retried, err := s.store.Get(ctx, before.ID)
				if err != nil {
					t.Fatal(err)
				}
				if retried.Status != "queued" || retried.Current.ID != operationID || retried.generation() != before.generation() || retried.Queued.Before(queueTime) || retried.Production != 0 || !retried.Deadline.IsZero() {
					t.Fatalf("user queue retry changed the original operation: %+v", retried)
				}
			}
			s.mu.Lock()
			s.Config.ProductionSlots = 1
			s.mu.Unlock()
			s.schedule()
			final := waitSession(t, s, before.ID, func(v Session) bool { return v.Terminal() })
			if final.Status != "completed" || final.Current.ID != operationID || final.Current.ArtifactID != operationID || final.Production != 1 || provider.Count() != 1 || len(final.Artifacts) != 1 {
				t.Fatalf("resumed queue duplicated operation or submission: %+v provider=%d", final, provider.Count())
			}
			if productionEventCount(t, s, before.ID, "runtime_accepted") != 1 || productionEventCount(t, s, before.ID, "production_slot") != 1 || productionEventCount(t, s, before.ID, "tool_submitted") != 1 {
				t.Fatal("queue restart duplicated acceptance, slot or submission events")
			}
			if mode == "queue_after_commit" && !final.Queued.Equal(queueTime) {
				t.Fatal("accepted FIFO timestamp changed after recovery")
			}
		})
	}
}

// TestCheckpointCrashCorrectionPending 在下一次纠偏检查点提交前强杀进程，
// 验证 Current 仍保留已完成原操作，草案独立保存纠偏身份、源资产与完整提议，
// 恢复后沿用原决策只提交一次减面，并保留首次失败与纠偏成功的两份实测报告。
func TestCheckpointCrashCorrectionPending(t *testing.T) {
	dir := t.TempDir()
	server, provider := checkpointCrashProviderServer(t)
	provider.mu.Lock()
	provider.over = true
	provider.mu.Unlock()
	marker := killCheckpointChildAtBarrier(t, dir, server.URL, "correction_before_set", 2)
	if provider.Count() != 1 {
		t.Fatalf("expected only initial generation before SIGKILL, got %d submissions", provider.Count())
	}
	cfg := DefaultConfig()
	cfg.DataDir, cfg.PollInterval = dir, 5*time.Millisecond
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.ready, s.source = true, "controlled-crash-test"
	s.provider = checkpointCrashProvider{endpoint: server.URL}
	s.modelFactory = func(context.Context) (model.BaseChatModel, error) { return scriptModel{}, nil }
	ctx := context.Background()
	before, err := s.store.Get(ctx, marker.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Current == nil || before.Current.Kind != "generate" || before.Current.Stage != "done" || before.Production != 1 || !before.HasSlot || before.Deadline.IsZero() || len(before.Artifacts) != 1 {
		t.Fatalf("pending correction overwrote the completed original operation: %+v", before)
	}
	original := before.Artifacts[0]
	if original.ID != before.Current.ID || original.Report.Passed || original.Report.Triangles != 6000 || original.SourceURL == "" {
		t.Fatalf("initial measured failure evidence was not preserved: %+v", original)
	}
	pending := before.PendingPause
	if pending == nil || pending.Operation == nil || pending.Operation.Kind != "decimate" || pending.Operation.Stage != "ready" || pending.Operation.Params.Input != original.SourceURL || pending.Operation.Params.FaceLimit != 4000 || pending.Point.Generation != 2 || before.generation() != 1 {
		t.Fatalf("pending correction lost its accepted operation/source: %+v", pending)
	}
	if pending.Seed == nil || pending.Seed.ToolName != "decimate_asset" || pending.Seed.ToolCallID != "accepted-correction-call" || pending.Seed.Response.ReasoningContent == "" {
		t.Fatal("pending correction lost its full original model proposal")
	}
	var proposed decimationInput
	if err := json.Unmarshal([]byte(pending.Seed.Response.ToolCalls[0].Function.Arguments), &proposed); err != nil {
		t.Fatal(err)
	}
	if proposed.ArtifactID != original.ID || proposed.TargetTriangles != 4000 {
		t.Fatalf("correction proposal no longer references original evidence: %+v", proposed)
	}
	if err := validatePending(before); err != nil {
		t.Fatalf("valid decimate pending seed cannot be reconstructed: %v", err)
	}
	oldCheckpoint, err := s.store.LoadCheckpoint(ctx, before.ID, before.ID)
	if err != nil || oldCheckpoint.Point.RefID != original.ID || oldCheckpoint.Point.Generation != 1 {
		t.Fatalf("old checkpoint no longer matches preserved Current: %+v err=%v", oldCheckpoint.Point, err)
	}
	correctionID, seedDecisionID := pending.Operation.ID, pending.Seed.DecisionID
	if correctionID == original.ID || seedDecisionID == "" {
		t.Fatal("correction identities were not established before the crash")
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	final := waitSession(t, s, before.ID, func(v Session) bool { return v.Terminal() })
	if final.Status != "completed" || final.Production != 2 || len(final.Artifacts) != 2 || provider.Count() != 2 || final.Current.ID != correctionID || final.SelectedArtifact != correctionID || final.Current.Params.Input != original.SourceURL {
		t.Fatalf("correction recovery changed or repeated production: %+v submissions=%d", final, provider.Count())
	}
	if final.PendingPause != nil || final.generation() != 2 || final.ResumePoint.PauseID != pending.Point.PauseID || final.ModelCalls != before.ModelCalls+1 || !final.Deadline.Equal(before.Deadline) {
		t.Fatalf("pending rebuild re-decided correction or reset execution facts: %+v", final)
	}
	if jsonString(final.Artifacts[0]) != jsonString(original) || final.Artifacts[1].ID != correctionID || !final.Artifacts[1].Report.Passed || final.Artifacts[1].Report.Triangles != 12 {
		t.Fatal("original failure report changed or corrected asset was not measured")
	}
	provider.mu.Lock()
	kinds := make(map[string]int)
	for _, kind := range provider.kinds {
		kinds[kind]++
	}
	provider.mu.Unlock()
	if kinds["generate"] != 1 || kinds["decimate"] != 1 {
		t.Fatalf("expected one remote generation and one decimation: %+v", kinds)
	}
	events, err := s.store.Events(ctx, before.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	reports := make(map[string]int)
	rebuilt := 0
	for _, event := range events {
		if event.Kind == "technical_report" {
			var data struct {
				OperationID string `json:"operation_id"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatal(err)
			}
			reports[data.OperationID]++
		}
		if event.Kind == "checkpoint_rebuilt" {
			rebuilt++
		}
	}
	if reports[original.ID] != 1 || reports[correctionID] != 1 || len(reports) != 2 || rebuilt != 1 {
		t.Fatalf("recovery duplicated a measured report or rebuilt another decision: reports=%v rebuilt=%d", reports, rebuilt)
	}
}
