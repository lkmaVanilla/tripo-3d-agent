package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/asset"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
)

type Service struct {
	Config       Config
	store        *Store
	provider     tripo.Provider
	modelFactory func(context.Context) (model.BaseChatModel, error)
	ctx          context.Context
	cancel       context.CancelFunc
	mu           sync.Mutex
	active       map[string]context.CancelFunc
	wg           sync.WaitGroup
	ready        bool
	source       string
}

func New(c Config) (*Service, error) {
	store, err := OpenStore(c.DataDir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{Config: c, store: store, provider: tripo.New(c.TripoKey), ctx: ctx, cancel: cancel, active: map[string]context.CancelFunc{}, ready: c.DeepSeekKey != "" && c.TripoKey != ""}
	s.modelFactory = s.deepSeekModel
	s.source = "live"
	return s, nil
}
func (s *Service) Start() {
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
}
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
func (s *Service) Create(ctx context.Context, owner, request string) (Session, error) {
	if !s.ready {
		return Session{}, fmt.Errorf("尚未配置 DeepSeek 和 Tripo 凭证")
	}
	now := time.Now().UTC()
	v := Session{ID: newID(), Owner: owner, Request: request, Status: "understanding", Created: now, LastUser: now, Model: "deepseek-v4-pro", Limits: Limits{Calls: s.Config.MaxCalls, Submissions: s.Config.MaxSubmissions, Clarifications: s.Config.MaxClarifications, Duration: s.Config.MaxDuration, Idle: s.Config.IdleTTL, Retention: s.Config.Retention}}
	v.Source = s.source
	err := s.store.Create(ctx, v)
	return v, err
}
func (s *Service) Answer(ctx context.Context, id, answer string) error {
	_, err := s.store.Edit(ctx, id, func(v *Session) error {
		if v.Terminal() {
			return ErrClosed
		}
		if v.Status != "awaiting_answer" || !v.CheckpointReady {
			return fmt.Errorf("当前请求不在等待回答")
		}
		v.Answer = answer
		v.LastUser = time.Now().UTC()
		v.ResumeRequested = true
		v.Status = "understanding"
		return nil
	}, "user_answer", map[string]string{"text": answer})
	return err
}
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
func (s *Service) RetryQueue(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
		if v.Terminal() {
			return ErrClosed
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
func (s *Service) schedule() {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.store.List(s.ctx, "")
	if err != nil {
		return
	}
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
		if v.Terminal() {
			if !v.Expires.IsZero() && now.After(v.Expires) && s.active[v.ID] == nil {
				if err := os.RemoveAll(filepath.Join(s.Config.DataDir, "artifacts", v.ID)); err == nil {
					_ = s.store.Delete(s.ctx, v.ID)
				}
			}
			continue
		}
		expired := !v.Deadline.IsZero() && now.After(v.Deadline)
		idle := v.Deadline.IsZero() && now.Sub(v.LastUser) > v.Limits.Idle
		if expired || idle {
			reason := "生产执行超时，已停止本地执行。"
			if idle {
				reason = "生产前连续无用户操作，等待已到期。"
			}
			_, _ = s.store.Edit(s.ctx, v.ID, func(x *Session) error { x.Finish("failed", reason); return nil }, "expired", map[string]string{"reason": reason})
			if cancel := s.active[v.ID]; cancel != nil {
				cancel()
			}
			v.HasSlot = false
			continue
		}
		if v.HasSlot {
			slots++
		}
	}
	if !s.ready {
		return
	}
	for _, v := range all {
		if v.Terminal() || s.active[v.ID] != nil {
			continue
		}
		if (!v.Deadline.IsZero() && now.After(v.Deadline)) || (v.Deadline.IsZero() && now.Sub(v.LastUser) > v.Limits.Idle) {
			continue
		}
		if v.Status == "queued" && v.CheckpointReady && slots < s.Config.ProductionSlots {
			v, err = s.store.Edit(s.ctx, v.ID, func(x *Session) error { x.HasSlot = true; x.Status = "running"; x.ResumeRequested = true; return nil }, "production_slot", nil)
			if err != nil {
				continue
			}
			slots++
		}
		if v.Status == "understanding" || v.Status == "running" {
			s.launch(v.ID)
		}
	}
}
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
				s.finalize(id, err)
			}
		}
	}()
}

func (s *Service) run(ctx context.Context, id string) error {
	v, err := s.store.Get(ctx, id)
	if err != nil || v.Terminal() {
		return err
	}
	// A persisted pre-submission checkpoint lets the tool reuse a known task ID.
	if v.Current != nil && v.Current.Stage == "submitting" {
		return fmt.Errorf("生产提交结果未知；为避免重复生产，已停止自动执行")
	}
	runner, err := s.runner(ctx, id)
	if err != nil {
		return err
	}
	for {
		v, err = s.store.Get(ctx, id)
		if err != nil || v.Terminal() {
			return err
		}
		var iter *adk.AsyncIterator[*adk.AgentEvent]
		if v.CheckpointReady {
			iter, err = runner.Resume(ctx, id)
		} else {
			messages := []*schema.Message{schema.UserMessage(v.Request)}
			if len(v.History) > 0 {
				messages = v.History
			}
			iter = runner.Run(ctx, messages, adk.WithCheckPointID(id))
		}
		if err != nil {
			return err
		}
		interrupted := false
		for {
			event, ok := iter.Next()
			if !ok {
				break
			}
			if event.Err != nil {
				return event.Err
			}
			if event.Action != nil && event.Action.Interrupted != nil {
				interrupted = true
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		v, err = s.store.Get(ctx, id)
		if err != nil || v.Terminal() {
			return err
		}
		if !interrupted {
			return fmt.Errorf("Agent 已结束，但没有提供明确的交付或停止动作")
		}
		v, err = s.store.Edit(ctx, id, func(x *Session) error { x.CheckpointReady = true; x.ResumeRequested = false; return nil }, "checkpoint", map[string]string{"state": v.Status})
		if err != nil {
			return err
		}
		if v.Status == "running" && v.HasSlot {
			continue
		}
		return nil
	}
}
func (s *Service) finalize(id string, cause error) {
	_, err := s.store.Edit(context.Background(), id, func(v *Session) error {
		if v.Terminal() {
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
	}, "runtime_finished", map[string]string{"reason": s.redact(cause.Error())})
	if err != nil {
		slog.Error("保存终止状态失败", "session", id, "error", err)
	}
}
func (s *Service) event(id, kind string, data any) error {
	_, err := s.store.Edit(context.Background(), id, nil, kind, data)
	return err
}
func (s *Service) production(ctx context.Context, id, opID string) (string, error) {
	v, err := s.store.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if v.Terminal() {
		return "", ErrClosed
	}
	if v.Current == nil || v.Current.ID != opID {
		return "", fmt.Errorf("生产操作检查点不匹配")
	}
	op := *v.Current
	if op.Stage == "done" {
		return s.operationResult(v, op), nil
	}
	if op.Stage == "submitting" {
		return "", fmt.Errorf("提交结果未知，不能重新提交")
	}
	if op.Stage == "ready" {
		v, err = s.store.Edit(ctx, id, func(x *Session) error {
			if x.Terminal() {
				return ErrClosed
			}
			if !x.HasSlot {
				return fmt.Errorf("尚未获得生产名额")
			}
			if x.Production >= x.Limits.Submissions || x.ModelCalls >= x.Limits.Calls {
				return ErrBudget
			}
			x.Production++
			if x.Deadline.IsZero() {
				x.Deadline = time.Now().UTC().Add(x.Limits.Duration)
			}
			x.Current.Stage = "submitting"
			return nil
		}, "tool_submitting", map[string]any{"operation_id": op.ID, "kind": op.Kind, "params": op.Params})
		if err != nil {
			return "", err
		}
	}
	opCtx, cancel := context.WithDeadline(ctx, v.Deadline)
	defer cancel()
	if op.Stage == "ready" {
		taskID, e := s.provider.Submit(opCtx, op.Kind, op.Params)
		if e != nil {
			if tripo.IsUnknown(e) {
				return "", fmt.Errorf("提交结果未知，保留已消耗额度并停止自动执行")
			}
			return s.operationFailure(id, op.ID, e)
		}
		v, err = s.store.Edit(context.Background(), id, func(x *Session) error { x.Current.TaskID = taskID; x.Current.Stage = "submitted"; return nil }, "tool_submitted", map[string]string{"operation_id": op.ID, "task_id": taskID})
		if err != nil {
			return "", err
		}
		op = *v.Current
	}
	lastProgress := -1
	lastStatus := ""
	for {
		var task tripo.Task
		err = retry(opCtx, func() error { var e error; task, e = s.provider.Query(opCtx, op.TaskID); return e })
		if err != nil {
			return "", err
		}
		if task.Progress != lastProgress || task.Status != lastStatus {
			if err = s.event(id, "tripo_progress", map[string]any{"operation_id": op.ID, "task_id": op.TaskID, "status": task.Status, "progress": task.Progress, "credits": task.Credits}); err != nil {
				return "", err
			}
			lastProgress, lastStatus = task.Progress, task.Status
		}
		switch task.Status {
		case "queued", "running":
			if err = wait(opCtx, s.Config.PollInterval); err != nil {
				return "", err
			}
			continue
		case "failed", "cancelled":
			return s.operationFailure(id, op.ID, fmt.Errorf("Tripo 任务 %s（code %d）：%s", task.Status, task.ErrorCode, s.redact(task.ErrorMessage)))
		case "success":
			var data []byte
			err = retry(opCtx, func() error { var e error; data, e = s.provider.Download(opCtx, task.Output.ModelURL); return e })
			if err != nil {
				return s.operationFailure(id, op.ID, err)
			}
			if opCtx.Err() != nil {
				return "", opCtx.Err()
			}
			report := asset.Inspect(data, v.Intent.MaxTriangles, v.Intent.MaxBytes)
			a := Artifact{ID: op.ID, TaskID: op.TaskID, SourceURL: task.Output.ModelURL, Report: report}
			dir := filepath.Join(s.Config.DataDir, "artifacts", id)
			if err = os.MkdirAll(dir, 0700); err != nil {
				return "", err
			}
			a.Path = filepath.Join(dir, a.ID+".glb")
			if err = atomicWrite(a.Path, data); err != nil {
				return "", err
			}
			v, err = s.store.Edit(context.Background(), id, func(x *Session) error {
				if x.Terminal() {
					return ErrClosed
				}
				x.Artifacts = append(x.Artifacts, a)
				x.Current.Stage = "done"
				x.Current.ArtifactID = a.ID
				return nil
			}, "technical_report", map[string]any{"operation_id": op.ID, "task_id": op.TaskID, "artifact_id": a.ID, "report": report})
			if err != nil {
				return "", err
			}
			return s.operationResult(v, *v.Current), nil
		default:
			return s.operationFailure(id, op.ID, fmt.Errorf("Tripo 返回未知任务状态"))
		}
	}
}
func (s *Service) operationFailure(id, opID string, cause error) (string, error) {
	v, err := s.store.Edit(context.Background(), id, func(x *Session) error {
		if x.Terminal() {
			return ErrClosed
		}
		x.Current.Stage = "done"
		x.Current.Error = s.redact(cause.Error())
		return nil
	}, "tool_failed", map[string]string{"operation_id": opID, "error": s.redact(cause.Error())})
	if err != nil {
		return "", err
	}
	return s.operationResult(v, *v.Current), nil
}
func (s *Service) operationResult(v Session, op Operation) string {
	for _, a := range v.Artifacts {
		if a.ID == op.ArtifactID {
			return jsonString(map[string]any{"artifact_id": a.ID, "task_id": a.TaskID, "report": a.Report, "remaining_submissions": v.Limits.Submissions - v.Production})
		}
	}
	return jsonString(map[string]any{"error": op.Error, "remaining_submissions": v.Limits.Submissions - v.Production})
}
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
