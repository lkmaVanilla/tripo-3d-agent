package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/testfixture"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
)

// crashProductionProvider 仅用于测试，通过父进程 HTTP 服务保存模拟远端任务的证据。
// 强杀执行子进程不会重置提交计数，因此恢复后的重复提交仍能被观测。
type crashProductionProvider struct {
	base, mode string
	client     *http.Client
}

func (p crashProductionProvider) request(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+path, nil)
	if err != nil {
		return nil, err
	}
	res, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("controlled provider HTTP %d", res.StatusCode)
	}
	return io.ReadAll(res.Body)
}

// barrier 让父进程确认已抵达指定边界，并阻塞当前请求直到子进程被杀死。
func (p crashProductionProvider) barrier(ctx context.Context, point string) error {
	_, err := p.request(ctx, "/barrier?point="+point)
	return err
}
func (p crashProductionProvider) Submit(ctx context.Context, _ string, _ tripo.Params) (string, error) {
	if p.mode == "before-submit" {
		if err := p.barrier(ctx, p.mode); err != nil {
			return "", err
		}
	}
	b, err := p.request(ctx, "/submit")
	return string(b), err
}
func (p crashProductionProvider) Query(ctx context.Context, id string) (tripo.Task, error) {
	if p.mode == "known-task" {
		if err := p.barrier(ctx, p.mode); err != nil {
			return tripo.Task{}, err
		}
	}
	b, err := p.request(ctx, "/query")
	var task tripo.Task
	if err == nil {
		err = json.Unmarshal(b, &task)
	}
	return task, err
}
func (p crashProductionProvider) Download(ctx context.Context, _ string) ([]byte, error) {
	return p.request(ctx, "/download")
}

// crashProductionModel 驱动真实 Eino 工具调用，并在工具结果刚进入模型输入时设置屏障。
// 它不请求远端模型，作用是区分结果已落库、已返回工具和已进入下一轮协议的窗口。
type crashProductionModel struct {
	p    crashProductionProvider
	opID string
}

func (m crashProductionModel) Generate(ctx context.Context, in []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	for _, msg := range in {
		if msg.Role != schema.Tool {
			continue
		}
		if msg.ToolCallID != "controlled-production-call" || !strings.Contains(msg.Content, m.opID) {
			return nil, fmt.Errorf("tool return did not contain the saved production result")
		}
		if m.p.mode != "after-tool-return" {
			return nil, fmt.Errorf("before-return barrier did not stop the tool")
		}
		// 第二次真实模型接口调用证明工具结果已到达 Eino；此时尚未产生下一条提议或检查点。
		return nil, m.p.barrier(ctx, m.p.mode)
	}
	return &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "controlled-production-call", Type: "function", Function: schema.FunctionCall{Name: "controlled_production", Arguments: "{}"}}}}, nil
}

func (m crashProductionModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

// runCrashProductionTool 将已持久化操作接入最小真实 Eino Agent，
// 精确定位生产结果返回工具前后；操作执行本身仍调用生产实现。
func runCrashProductionTool(ctx context.Context, s *Service, p crashProductionProvider, v Session) error {
	t, err := utils.InferTool("controlled_production", "controlled existing production operation", func(ctx context.Context, _ *struct{}) (string, error) {
		result, err := s.production(ctx, v.ID, v.Current.ID)
		if err == nil && p.mode == "before-tool-return" {
			err = p.barrier(ctx, p.mode)
		}
		return result, err
	})
	if err != nil {
		return err
	}
	a, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{Name: "controlled_production_agent", Model: crashProductionModel{p: p, opID: v.Current.ID}, MaxIterations: 3, ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: []tool.BaseTool{t}, ExecuteSequentially: true}}})
	if err != nil {
		return err
	}
	r := adk.NewRunner(ctx, adk.RunnerConfig{Agent: a})
	iter := r.Run(ctx, []*schema.Message{schema.UserMessage("recover the persisted operation")})
	for {
		event, ok := iter.Next()
		if !ok {
			return fmt.Errorf("Eino returned without reaching the crash barrier")
		}
		if event.Err != nil {
			return event.Err
		}
	}
}

// TestProductionRecoveryCrashHelper 是强杀用例的子进程入口，只在显式测试环境变量下运行。
// 子进程复用父进程准备的临时数据库与本地 Provider 服务。
func TestProductionRecoveryCrashHelper(t *testing.T) {
	if os.Getenv("TRIPO_PRODUCTION_CRASH_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	c := DefaultConfig()
	c.DataDir = os.Getenv("TRIPO_PRODUCTION_CRASH_DIR")
	c.PollInterval = time.Millisecond
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	p := crashProductionProvider{base: os.Getenv("TRIPO_PRODUCTION_CRASH_URL"), mode: os.Getenv("TRIPO_PRODUCTION_CRASH_POINT"), client: http.DefaultClient}
	s.provider = p
	id := os.Getenv("TRIPO_PRODUCTION_CRASH_ID")
	v, err := s.store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if p.mode == "before-tool-return" || p.mode == "after-tool-return" {
		err = runCrashProductionTool(context.Background(), s, p, v)
	} else {
		_, err = s.production(context.Background(), id, v.Current.ID)
	}
	if p.mode == "file-written" {
		_ = p.barrier(context.Background(), p.mode)
	}
	if err != nil {
		t.Fatal(err)
	}
	// 故意不调用 Service.Close，确保退出不会先做优雅清理或取消恢复状态。
	t.Fatal("crash boundary did not block the child")
}

// TestProductionRecoveryForcedProcessExit 覆盖提交前后、已知任务、文件落盘和工具返回边界。
// 使用真实 Process.Kill 验证未清理退出后的数据库事实；所有 Provider 响应均为本地受控数据。
func TestProductionRecoveryForcedProcessExit(t *testing.T) {
	for _, point := range []string{"before-submit", "after-submit", "known-task", "file-written", "before-tool-return", "after-tool-return"} {
		t.Run(point, func(t *testing.T) {
			s, before, _ := productionRecoveryFixture(t, "ready")
			reached := make(chan string, 2)
			var submissions, queries, downloads atomic.Int32
			var isRecovering atomic.Bool
			writeLock := make(chan *sql.Tx, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/barrier":
					reached <- r.URL.Query().Get("point")
					<-r.Context().Done()
				case "/submit":
					submissions.Add(1)
					if point == "after-submit" && !isRecovering.Load() {
						// 模拟远端已接受但响应尚未返回：本地没有任务 ID，恢复只能保守停止。
						reached <- point
						<-r.Context().Done()
						return
					}
					_, _ = w.Write([]byte("known-task"))
				case "/query":
					queries.Add(1)
					task := tripo.Task{ID: "known-task", Status: "success", Progress: 100}
					task.Output.ModelURL = "https://fixture.example/model.glb"
					_ = json.NewEncoder(w).Encode(task)
				case "/download":
					downloads.Add(1)
					if point == "file-written" && !isRecovering.Load() {
						// 父连接持有写事务，允许子进程落盘 GLB，但阻止它提交产物记录。
						// 子进程到达屏障并被强杀后才释放锁，稳定复现文件与数据库之间的窗口。
						tx, err := s.store.db.BeginTx(context.Background(), nil)
						if err == nil {
							_, err = tx.ExecContext(context.Background(), "UPDATE sessions SET owner=owner WHERE id=?", before.ID)
						}
						if err != nil {
							if tx != nil {
								_ = tx.Rollback()
							}
							http.Error(w, err.Error(), 500)
							return
						}
						writeLock <- tx
					}
					_, _ = w.Write(testfixture.Cube(12))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			cmd := exec.Command(os.Args[0], "-test.run=^TestProductionRecoveryCrashHelper$", "-test.count=1")
			cmd.Env = append(os.Environ(), "TRIPO_PRODUCTION_CRASH_CHILD=1", "TRIPO_PRODUCTION_CRASH_DIR="+s.Config.DataDir, "TRIPO_PRODUCTION_CRASH_ID="+before.ID, "TRIPO_PRODUCTION_CRASH_URL="+server.URL, "TRIPO_PRODUCTION_CRASH_POINT="+point)
			var output bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := make(chan error, 1)
			go func() { waited <- cmd.Wait() }()
			killed := false
			defer func() {
				if !killed {
					_ = cmd.Process.Kill()
					<-waited
				}
				select {
				case tx := <-writeLock:
					_ = tx.Rollback()
				default:
				}
			}()
			select {
			case seen := <-reached:
				if seen != point {
					t.Fatalf("wrong crash point: %s", seen)
				}
			case err := <-waited:
				killed = true
				t.Fatalf("child exited before fault: %v\n%s", err, output.String())
			case <-time.After(20 * time.Second):
				t.Fatal("child did not reach crash boundary")
			}
			// 不能改成 Service.Close 或返回模拟错误，否则会绕过真正的进程退出恢复。
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := <-waited; err == nil {
				t.Fatal("child exited normally, not by forced termination")
			}
			killed = true
			select {
			case tx := <-writeLock:
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
			default:
			}
			interrupted := getProductionFixture(t, s, before.ID)
			if interrupted.Production != 1 || interrupted.Deadline.IsZero() || interrupted.Terminal() {
				t.Fatalf("crash lost original quota or deadline: %+v", interrupted)
			}
			if point == "file-written" {
				if len(interrupted.Artifacts) != 0 || interrupted.Current.Stage != "submitted" {
					t.Fatalf("file boundary already committed: %+v", interrupted)
				}
				if _, err := os.Stat(filepath.Join(s.Config.DataDir, "artifacts", before.ID, before.Current.ID+".glb")); err != nil {
					t.Fatalf("missing pre-crash file: %v", err)
				}
			}
			isRecovering.Store(true)
			s.provider = crashProductionProvider{base: server.URL, client: server.Client()}
			_, err := s.production(context.Background(), before.ID, before.Current.ID)
			after := getProductionFixture(t, s, before.ID)
			if after.Production != 1 || !after.Deadline.Equal(interrupted.Deadline) {
				t.Fatalf("recovery reset quota or deadline: %+v", after)
			}
			if point == "before-submit" || point == "after-submit" {
				want := int32(0)
				if point == "after-submit" {
					want = 1
				}
				if err == nil || submissions.Load() != want || queries.Load() != 0 || len(after.Artifacts) != 0 {
					t.Fatalf("unknown submission was repeated: err=%v submits=%d", err, submissions.Load())
				}
				return
			}
			if err != nil || submissions.Load() != 1 || len(after.Artifacts) != 1 || !after.Artifacts[0].Report.Passed || productionEventCount(t, s, before.ID, "technical_report") != 1 {
				t.Fatalf("production crash recovery: %+v err=%v submits=%d", after, err, submissions.Load())
			}
			if (point == "before-tool-return" || point == "after-tool-return") && (queries.Load() != 1 || downloads.Load() != 1) {
				t.Fatalf("completed result triggered new calls: queries=%d downloads=%d", queries.Load(), downloads.Load())
			}
		})
	}
}
