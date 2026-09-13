package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
)

// Service 协调单 Agent 执行、会话持久化、生产调度与外部提供方。
// 这里的并发控制面向单进程；持久化 Session 保存跨进程重启所需的事实。
type Service struct {
	Config   Config
	store    *Store
	provider tripo.Provider
	// modelFactory 可注入受控模型，测试仍通过真实 Eino Runner 执行。
	modelFactory func(context.Context) (model.BaseChatModel, error)
	// 服务级上下文控制调度器和所有本地执行，不等价于取消远端 Tripo 任务。
	ctx    context.Context
	cancel context.CancelFunc
	// mu 串行化名额分配和 active 变更；需要同时访问数据库时先获取此锁。
	mu sync.Mutex
	// active 保证一个会话在本进程同时最多有一个执行协程，与生产名额是两个概念。
	active map[string]context.CancelFunc
	wg     sync.WaitGroup
	// ready 表示凭证已配置；启动恢复失败另外通过 startupErr 阻止接纳和调度。
	ready      bool
	source     string
	startupErr error
	// 存储持续不可用的会话暂停进程内重试，避免每个调度周期重复执行失败路径。
	blocked map[string]bool
}

// New 打开存储并组装依赖；Start 完成恢复对账后才开始调度。
func New(c Config) (*Service, error) {
	store, err := OpenStore(c.DataDir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{Config: c, store: store, provider: tripo.New(c.TripoKey), ctx: ctx, cancel: cancel, active: map[string]context.CancelFunc{}, ready: c.DeepSeekKey != "" && c.TripoKey != ""}
	s.modelFactory = s.deepSeekModel
	s.source = "live"
	s.blocked = map[string]bool{}
	return s, nil
}

// Start 先对账检查点与业务状态，成功后启动周期调度；调用方应只启动一次。
func (s *Service) Start() error {
	if err := s.reconcile(); err != nil {
		s.mu.Lock()
		s.startupErr = err
		s.mu.Unlock()
		return err
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				s.schedule()
			}
		}
	}()
	return nil
}

// Close 取消并等待本地执行退出，再关闭数据库，避免执行协程写入已关闭存储。
func (s *Service) Close() error {
	s.cancel()
	s.mu.Lock()
	for _, cancel := range s.active {
		cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return s.store.Close()
}

// Create 保存新的独立资产请求，冻结当时的预算；后续由调度器启动需求理解。
func (s *Service) Create(ctx context.Context, owner, request string) (Session, error) {
	s.mu.Lock()
	startupErr := s.startupErr
	s.mu.Unlock()
	if startupErr != nil {
		return Session{}, fmt.Errorf("恢复对账尚未完成：%w", startupErr)
	}
	if !s.ready {
		return Session{}, fmt.Errorf("尚未配置 DeepSeek 和 Tripo 凭证")
	}
	now := time.Now().UTC()
	v := Session{RecoverySchemaVersion: recoveryVersion, ID: newID(), Owner: owner, Request: request, Status: "understanding", Created: now, LastUser: now, Model: "deepseek-v4-pro", Limits: Limits{Calls: s.Config.MaxCalls, Submissions: s.Config.MaxSubmissions, Clarifications: s.Config.MaxClarifications, Duration: s.Config.MaxDuration, Idle: s.Config.IdleTTL, Retention: s.Config.Retention}}
	v.Source = s.source
	err := s.store.Create(ctx, v)
	return v, err
}

// Answer 仅向已提交的问题暂停写入答案；恢复点身份必须与当前 WaitID 一致。
// 先接受并持久化答案，再唤醒执行，保证崩溃后无需用户重复回答。
func (s *Service) Answer(ctx context.Context, id, answer string) error {
	if strings.TrimSpace(answer) == "" {
		return fmt.Errorf("回答不能为空")
	}
	record, err := s.store.LoadCheckpoint(ctx, id, id)
	if err != nil {
		return err
	}
	if record.Point == nil {
		return recoveryError("checkpoint_identity_mismatch")
	}
	_, err = s.store.Edit(ctx, id, func(v *Session) error {
		if v.Terminal() {
			return ErrClosed
		}
		if v.Status != "awaiting_answer" || v.PendingPause != nil || v.ResumePoint == nil || v.ResumePoint.Kind != "question" || v.ResumePoint.RefID != v.WaitID {
			return fmt.Errorf("当前请求不在等待回答")
		}
		// 检查点在事务外读取，事务中再次比对可拒绝读取后已被替换的旧恢复点。
		if *record.Point != *v.ResumePoint {
			return recoveryError("checkpoint_identity_mismatch")
		}
		if err := checkExecution(*v, time.Now()); err != nil {
			return err
		}
		if v.Answers == nil {
			v.Answers = map[string]AnswerRecord{}
		}
		v.Answers[v.WaitID] = AnswerRecord{Text: answer, Accepted: time.Now().UTC()}
		v.Answer = answer
		v.LastUser = time.Now().UTC()
		v.ResumeRequested = true
		v.Status = "understanding"
		return nil
	}, "user_answer", map[string]string{"text": answer})
	return err
}

// Stop 先尝试持久化终态，再取消执行上下文；后续回调应以最新终态为准。
// 本地取消无法撤销已被 Tripo 接受的任务。
func (s *Service) Stop(ctx context.Context, id string) error {
	_, err := s.store.Edit(ctx, id, func(v *Session) error {
		if !v.Terminal() {
			v.Finish("stopped", "用户停止了本地执行；已提交的远端任务可能仍在运行。")
		}
		return nil
	}, "stopped", map[string]string{"reason": "user"})
	s.mu.Lock()
	if cancel := s.active[id]; cancel != nil {
		cancel()
	}
	s.mu.Unlock()
	return err
}

// RetryQueue 处理用户对 queue_full 的显式重试，复用原操作并按本次时间重新入队。
// 仅检查点身份仍匹配的已提交暂停可以入队，不生成新的生产提议。
func (s *Service) RetryQueue(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.store.LoadCheckpoint(ctx, id, id)
	if err != nil {
		return err
	}
	if record.Point == nil {
		return recoveryError("checkpoint_identity_mismatch")
	}
	all, err := s.store.List(ctx, "")
	if err != nil {
		return err
	}
	n := 0
	for _, v := range all {
		if v.Status == "queued" {
			n++
		}
	}
	if n >= s.Config.QueueSize {
		return fmt.Errorf("队列仍然繁忙，请稍后重试")
	}
	_, err = s.store.Edit(ctx, id, func(v *Session) error {
		if err := checkExecution(*v, time.Now()); err != nil {
			return err
		}
		if v.PendingPause != nil || v.ResumePoint == nil || *v.ResumePoint != *record.Point {
			return recoveryError("checkpoint_identity_mismatch")
		}
		if v.Status != "queue_full" {
			return fmt.Errorf("无需重新入队")
		}
		v.Status = "queued"
		v.Queued = time.Now().UTC()
		v.LastUser = v.Queued
		return nil
	}, "queued", nil)
	return err
}

// schedule 先清理或终止到期会话，再按入队时间分配名额、启动可推进的会话。
// 锁保护的是进程内调度决策；每次状态更新仍在事务中复核最新边界。
func (s *Service) schedule() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startupErr != nil {
		return
	}
	all, err := s.store.List(s.ctx, "")
	if err != nil {
		return
	}
	// 队列按入队时间排序；时间相同时用创建时间确定先后。
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i].Queued, all[j].Queued
		if a.Equal(b) {
			return all[i].Created.Before(all[j].Created)
		}
		return a.Before(b)
	})
	slots := 0
	now := time.Now().UTC()
	for i := range all {
		v := &all[i]
		// 等执行协程退出后再删除过期数据，避免清理与迟到写入竞争。
		if v.Terminal() {
			if !v.Expires.IsZero() && now.After(v.Expires) && s.active[v.ID] == nil {
				if err := os.RemoveAll(filepath.Join(s.Config.DataDir, "artifacts", v.ID)); err == nil {
					_ = s.store.Delete(s.ctx, v.ID)
				}
			}
			continue
		}
		// 首次生产前按用户空闲时间过期；生产开始后只使用固定执行截止时间。
		expired := !v.Deadline.IsZero() && now.After(v.Deadline)
		idle := v.Deadline.IsZero() && now.Sub(v.LastUser) > v.Limits.Idle
		if expired || idle {
			latest, ended, e := s.expireIfDue(s.ctx, v.ID)
			if e != nil {
				continue
			}
			*v = latest
			if ended {
				if cancel := s.active[v.ID]; cancel != nil {
					cancel()
				}
			}
			if v.Terminal() {
				continue
			}
		}
		if v.HasSlot {
			slots++
		}
	}
	if !s.ready {
		return
	}
	for _, v := range all {
		if v.Terminal() || s.active[v.ID] != nil || s.blocked[v.ID] {
			continue
		}
		if (!v.Deadline.IsZero() && now.After(v.Deadline)) || (v.Deadline.IsZero() && now.Sub(v.LastUser) > v.Limits.Idle) {
			continue
		}
		// 名额只授予已提交的生产暂停；准备中的草案不能提前消耗队列执行能力。
		if v.Status == "queued" && v.PendingPause == nil && v.ResumePoint != nil && slots < s.Config.ProductionSlots {
			v, err = s.store.Edit(s.ctx, v.ID, func(x *Session) error {
				if err := checkExecution(*x, time.Now()); err != nil {
					return err
				}
				if x.Status != "queued" || x.PendingPause != nil || x.ResumePoint == nil {
					return recoveryError("checkpoint_identity_mismatch")
				}
				x.HasSlot = true
				x.Status = "running"
				x.ResumeRequested = true
				return nil
			}, "production_slot", nil)
			if err != nil {
				continue
			}
			slots++
		}
		// 理解和暂停重建无需先占生产名额；等待回答与队列满由用户操作继续推进。
		if v.PendingPause != nil || v.Status == "understanding" || v.Status == "running" {
			s.launch(v.ID)
		}
	}
}

// errNotExpired 让事务回滚无效的过期尝试，避免重复生成 expired 事件。
var errNotExpired = errors.New("请求尚未到期或已经终止")

// expireIfDue 在事务内重读期限，避免调度快照覆盖用户刚提交的答案或停止状态。
func (s *Service) expireIfDue(ctx context.Context, id string) (Session, bool, error) {
	data := map[string]string{}
	v, err := s.store.Edit(ctx, id, func(x *Session) error {
		if x.Terminal() {
			return errNotExpired
		}
		now := time.Now()
		reason := ""
		if !x.Deadline.IsZero() && !now.Before(x.Deadline) {
			reason = "生产执行超时，已停止本地执行。"
		}
		if x.Deadline.IsZero() && now.Sub(x.LastUser) >= x.Limits.Idle {
			reason = "生产前连续无用户操作，等待已到期。"
		}
		if reason == "" {
			return errNotExpired
		}
		x.Finish("failed", reason)
		data["reason"] = reason
		return nil
	}, "expired", data)
	if errors.Is(err, errNotExpired) {
		return v, false, nil
	}
	return v, err == nil, err
}

// launch 在调用方持有 mu 时登记执行协程，协程结束后释放 active 标记。
// 服务关闭只保留当前持久化状态供下次恢复，不把正常停机当作请求失败。
func (s *Service) launch(id string) {
	ctx, cancel := context.WithCancel(s.ctx)
	s.active[id] = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { cancel(); s.mu.Lock(); delete(s.active, id); s.mu.Unlock() }()
		if err := s.run(ctx, id); err != nil && s.ctx.Err() == nil {
			v, e := s.store.Get(context.Background(), id)
			if e == nil && !v.Terminal() {
				e = s.finalize(id, err)
			}
			// 终止事实也无法写入时隔离此会话，直到服务重启后重新对账。
			if e != nil {
				s.mu.Lock()
				s.blocked[id] = true
				s.mu.Unlock()
				slog.Error("恢复存储不可用，停止重试至服务重新启动", "session", id, "error", e)
			}
		}
	}()
}

// run 在正常执行、已提交检查点恢复和待提交暂停重建之间选择路径。
// 每轮重新读取 Session，确保旧 Runner 的控制流不能恢复过期或已停止的请求。
func (s *Service) run(ctx context.Context, id string) error {
	for {
		v, err := s.store.Get(ctx, id)
		if err != nil {
			return err
		}
		if err = checkExecution(v, time.Now()); err != nil {
			return err
		}
		// 本地没有任务 ID 时不能判断远端是否接受过提交，保留额度并停止自动重发。
		if v.Current != nil && v.Current.Stage == "submitting" && v.Current.TaskID == "" {
			return recoveryError("submission_outcome_unknown")
		}
		// 模型额度或检查点不可用不应丢弃已知远端任务；可转为 Go 确定性查询和检查。
		if v.Current != nil && v.Current.TaskID != "" && ((v.RecoveryFailure != "" && !hasRecoverablePending(v)) || ((v.ModelCalls >= v.Limits.Calls || v.ResumePoint == nil) && v.PendingPause == nil)) {
			return s.recoverKnownTask(ctx, id)
		}
		c := &pauseCoordinator{s: s, id: id, mode: "normal", expected: v.generation(), resuming: v.ResumePoint != nil}
		resume := v.ResumePoint != nil
		input := []*schema.Message{schema.UserMessage(v.Request)}
		// 新提议有完整种子时从原输入重建；旧暂停只有经验证的检查点时走受限 Resume。
		if v.PendingPause != nil {
			if err = validatePending(v); err != nil {
				return err
			}
			c.pause = v.PendingPause
			if v.PendingPause.Seed != nil {
				c.mode, c.seed, resume = "rebuild", v.PendingPause.Seed, false
				input = recoveryInput(c.seed.Input)
			} else {
				c.mode = "verify"
				if v.ResumePoint == nil {
					c.mode = "legacy"
				}
				resume = true
			}
		} else if !resume && len(v.History) > 0 {
			// 没有检查点时仅接受完整消息协议，不能带悬空工具调用直接继续问模型。
			if err = validateProtocol(v.History); err != nil {
				return err
			}
			input = recoveryInput(v.History)
		}
		interrupted, err := s.executeRunner(ctx, v, c, resume, input)
		if err != nil {
			latest, e := s.store.Get(ctx, id)
			if e == nil && !latest.Terminal() && latest.Current != nil && latest.Current.TaskID != "" && (latest.PendingPause == nil || (latest.PendingPause.Existing && latest.PendingPause.Seed == nil)) {
				_, e = s.store.Edit(ctx, id, func(x *Session) error {
					if err := checkExecution(*x, time.Now()); err != nil {
						return err
					}
					x.RecoveryFailure = s.redact(err.Error())
					return nil
				}, "recovery_rejected", map[string]string{"reason": s.redact(err.Error()), "action": "query_known_task"})
				if e != nil {
					return e
				}
				return s.recoverKnownTask(ctx, id)
			}
			return err
		}
		v, err = s.store.Get(ctx, id)
		if err != nil || v.Terminal() {
			return err
		}
		// Eino 的中断事件不是持久化成功证明，还必须确认协调器已原子提交检查点。
		if !interrupted || !c.committed {
			return recoveryError("checkpoint_not_committed")
		}
		// 提交后可能已接到答案或已持有名额，直接继续；其余暂停把控制权交回调度器。
		if v.Status == "understanding" || (v.Status == "running" && v.HasSlot) {
			continue
		}
		return nil
	}
}

// recoveryInput 去除将由 ChatModelAgent 重新构造的指令与 Skill 系统前缀。
func recoveryInput(messages []*schema.Message) []*schema.Message {
	if len(messages) > 0 && messages[0].Role == schema.System {
		return messages[1:]
	}
	return messages
}

// executeRunner 运行并耗尽 Eino 事件迭代器，保留首个错误以及是否发生中断。
// 暂停状态由检查点存储提交，消费事件时不再补写可能过时的 Ready 标记。
func (s *Service) executeRunner(ctx context.Context, v Session, c *pauseCoordinator, resume bool, input []*schema.Message) (bool, error) {
	runner, err := s.runner(ctx, v.ID, c)
	if err != nil {
		return false, err
	}
	var iter *adk.AsyncIterator[*adk.AgentEvent]
	// Eino 的根检查点键始终为会话 ID，暂停代次由应用侧恢复元数据校验。
	if resume {
		iter, err = runner.Resume(ctx, v.ID)
	} else {
		iter = runner.Run(ctx, input, adk.WithCheckPointID(v.ID))
	}
	if err != nil {
		return false, err
	}
	interrupted := false
	var firstErr error
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil && firstErr == nil {
			firstErr = event.Err
		}
		if event.Action != nil && event.Action.Interrupted != nil {
			interrupted = true
		}
	}
	if ctx.Err() != nil {
		return interrupted, ctx.Err()
	}
	return interrupted, firstErr
}

// finalize 将执行错误收敛为持久化结论，尊重已有终态和当前时间边界。
// 模型额度用完但已有实测合格产物时，可由程序基于证据完成交付。
func (s *Service) finalize(id string, cause error) error {
	kind := "runtime_finished"
	if strings.Contains(cause.Error(), "recovery_") || strings.Contains(cause.Error(), "checkpoint_") || errors.Is(cause, ErrCheckpointMismatch) {
		kind = "recovery_rejected"
	}
	data := map[string]string{"reason": s.redact(cause.Error())}
	_, err := s.store.Edit(context.Background(), id, func(v *Session) error {
		if v.Terminal() {
			return nil
		}
		if boundary := checkExecution(*v, time.Now()); boundary != nil {
			v.Finish("failed", s.redact(boundary.Error()))
			data["reason"] = s.redact(boundary.Error())
			return nil
		}
		if errors.Is(cause, ErrBudget) && len(v.Artifacts) > 0 {
			a := v.Artifacts[len(v.Artifacts)-1]
			if a.Report.Passed {
				v.SelectedArtifact = a.ID
				v.Finish("completed", "模型调用额度已耗尽；程序已依据实测技术报告交付结果。未进行视觉检查。")
				return nil
			}
		}
		v.Finish("failed", s.redact(cause.Error()))
		return nil
	}, kind, data)
	if err != nil {
		slog.Error("保存终止状态失败", "session", id, "error", err)
	}
	return err
}

// event 通过统一存储入口追加轨迹，复用事件序号与持久化规则。
func (s *Service) event(id, kind string, data any) error {
	_, err := s.store.Edit(context.Background(), id, nil, kind, data)
	return err
}

// retry 最多尝试三次，间隔一秒、两秒；调用方仅应传入允许重试的操作。
// 生产提交不能使用此帮助函数，以免响应丢失后重复产生远端任务。
func retry(ctx context.Context, fn func() error) error {
	var err error
	for i := 0; i < 3; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err = fn(); err == nil {
			return nil
		}
		if i < 2 {
			if e := wait(ctx, time.Duration(1<<i)*time.Second); e != nil {
				return e
			}
		}
	}
	return err
}

// wait 让轮询或退避等待响应取消，避免停止请求后仍等完整个间隔。
func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// atomicWrite 在同目录写临时文件、同步并重命名，避免下载中断暴露半个模型文件。
// 文件与数据库不是同一个事务，调用方按操作 ID 处理重复下载和产物记录。
func atomicWrite(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
