package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func (s *Service) newRun(owner, request, version string) Session {
	now := time.Now().UTC()
	return Session{ID: newID(), Owner: owner, Request: request, Status: "understanding", Created: now, LastUser: now, Model: "deepseek-v4-pro", Source: s.source, ExecutionVersion: version, RecoverySchemaVersion: recoveryVersion, Limits: Limits{Calls: s.Config.MaxCalls, Submissions: s.Config.MaxSubmissions, Clarifications: s.Config.MaxClarifications, Duration: s.Config.MaxDuration, Idle: s.Config.IdleTTL, Retention: s.Config.Retention}}
}

func (s *Service) acceptReady() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startupErr != nil {
		return fmt.Errorf("恢复对账尚未完成：%w", s.startupErr)
	}
	if !s.ready {
		return fmt.Errorf("尚未配置DeepSeek和Tripo凭证")
	}
	return nil
}

func (s *Service) CreateAssetConversation(ctx context.Context, owner, text, key string) (Conversation, Session, error) {
	if err := s.acceptReady(); err != nil {
		return Conversation{}, Session{}, err
	}
	v := s.newRun(owner, text, OptionalPromptVersion)
	v.ConversationContext = map[string]any{"theme": text, "messages": []any{}, "summaries": []any{}, "truncated": false}
	c, v, _, err := s.store.CreateConversation(ctx, v, key)
	return c, v, err
}

// ContinueConversation 冻结本Run上下文，真正的接受与幂等由存储事务决定。
func (s *Service) ContinueConversation(ctx context.Context, id, owner, text, key, versionID string) (Session, error) {
	if err := s.acceptReady(); err != nil {
		return Session{}, err
	}
	c, err := s.store.GetConversation(ctx, id)
	if err != nil || c.Owner != owner || !c.Available(time.Now()) {
		return Session{}, ErrConversationExpired
	}
	var input *AssetVersion
	if versionID != "" {
		v, e := s.store.GetAssetVersion(ctx, id, versionID)
		if e != nil {
			return Session{}, ErrInvalidVersion
		}
		input = &v
	}
	snap, err := s.store.ConversationSnapshot(ctx, id, owner, 0, 0, 20)
	if err != nil {
		return Session{}, err
	}
	contextData := map[string]any{"theme": c.Title, "messages": []any{}, "summaries": []any{}, "truncated": snap.HasMore}
	if input != nil {
		contextData["input_intent"] = input.SourceIntent
		contextData["input_version"] = input
	}
	// 从最近消息倒序分配正文预算，最后仍按原顺序呈现。硬事实独立于这个窗口。
	budget := 12000
	messages := []any{}
	for i := len(snap.Messages) - 1; i >= 0; i-- {
		m := snap.Messages[i]
		body := []rune(m.Text)
		if len(body) > budget {
			body = body[:budget]
			contextData["truncated"] = true
		}
		budget -= len(body)
		messages = append([]any{map[string]any{"kind": m.Kind, "text": string(body), "version_id": m.VersionID, "run_id": m.RunID}}, messages...)
		if budget == 0 {
			contextData["truncated"] = true
			break
		}
	}
	contextData["messages"] = messages
	summaries := []any{}
	for i := len(snap.Runs) - 1; i >= 0 && len(summaries) < 5; i-- {
		r := snap.Runs[i]
		status, _ := r["status"].(string)
		if status == "completed" || status == "failed" || status == "stopped" || status == "answered" {
			summaries = append([]any{map[string]any{"id": r["id"], "status": status, "result": r["result"], "outcome": r["outcome"], "intent": r["intent"]}}, summaries...)
		}
	}
	contextData["summaries"] = summaries
	v := s.newRun(owner, text, OptionalPromptVersion)
	v.ConversationContext = contextData
	v, _, err = s.store.AppendConversationRun(ctx, id, owner, key, versionID, v)
	return v, err
}

// cleanupConversations 只处理已经整体过期的会话，旧Run.Expires不再单独删文件。
// 调用方持有调度锁；清理标记先持久化，部分失败交给下一轮幂等重试。
func (s *Service) cleanupConversations(now time.Time) {
	all, err := s.store.ListConversations(s.ctx, "")
	if err != nil {
		return
	}
	for _, c := range all {
		if c.ActiveRunID != "" {
			continue
		}
		if !c.Cleaning && (c.Expires.IsZero() || now.Before(c.Expires)) {
			continue
		}
		marked, e := s.store.MarkConversationCleanup(s.ctx, c.ID, now)
		if e != nil || !marked {
			continue
		}
		ids, e := s.store.ConversationRunIDs(s.ctx, c.ID)
		if e != nil {
			continue
		}
		failed := false
		for _, id := range ids {
			if s.active[id] != nil {
				failed = true
				break
			}
			if e = os.RemoveAll(filepath.Join(s.Config.DataDir, "artifacts", id)); e != nil {
				failed = true
				break
			}
		}
		if !failed {
			_ = s.store.DeleteConversation(s.ctx, c.ID)
		}
	}
}
