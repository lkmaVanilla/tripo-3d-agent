package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const conversationSchema = `
CREATE TABLE IF NOT EXISTS conversations (
 id TEXT PRIMARY KEY, owner TEXT NOT NULL, title TEXT NOT NULL,
 active_run_id TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL, updated INTEGER NOT NULL,
 expires INTEGER NOT NULL DEFAULT 0, cleaning INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS conversations_owner ON conversations(owner,updated);
CREATE TABLE IF NOT EXISTS conversation_runs (
 run_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
 conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
 created INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS conversation_runs_parent ON conversation_runs(conversation_id,created);
CREATE TABLE IF NOT EXISTS conversation_messages (
 id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
 run_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 source_key TEXT NOT NULL, seq INTEGER NOT NULL, updated_seq INTEGER NOT NULL, data BLOB NOT NULL,
 UNIQUE(conversation_id,source_key));
CREATE INDEX IF NOT EXISTS conversation_messages_page ON conversation_messages(conversation_id,seq);
CREATE TABLE IF NOT EXISTS conversation_commands (
 scope TEXT NOT NULL, client_id TEXT NOT NULL, payload_hash TEXT NOT NULL,
 run_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 PRIMARY KEY(scope,client_id));
CREATE TABLE IF NOT EXISTS asset_versions (
 id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
 run_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE, operation_id TEXT NOT NULL,
 number INTEGER NOT NULL, data BLOB NOT NULL, path TEXT NOT NULL, source_intent BLOB,
 UNIQUE(run_id,operation_id), UNIQUE(conversation_id,number));
CREATE TABLE IF NOT EXISTS conversation_run_views (
 run_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE, event_seq INTEGER NOT NULL,
 data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS conversation_schema (version INTEGER PRIMARY KEY);
`

type conversationQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func conversationTime(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}
func conversationDate(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

func loadConversation(ctx context.Context, q conversationQuery, id string) (Conversation, error) {
	var c Conversation
	var created, updated, expires int64
	err := q.QueryRowContext(ctx, `SELECT id,owner,title,active_run_id,created,updated,expires,cleaning FROM conversations WHERE id=?`, id).Scan(&c.ID, &c.Owner, &c.Title, &c.ActiveRunID, &created, &updated, &expires, &c.Cleaning)
	c.Created, c.Updated, c.Expires = conversationDate(created), conversationDate(updated), conversationDate(expires)
	return c, err
}

func saveConversation(ctx context.Context, tx *sql.Tx, c Conversation) error {
	_, err := tx.ExecContext(ctx, `UPDATE conversations SET title=?,active_run_id=?,updated=?,expires=?,cleaning=? WHERE id=?`, c.Title, c.ActiveRunID, conversationTime(c.Updated), conversationTime(c.Expires), c.Cleaning, c.ID)
	return err
}

func loadConversationRun(ctx context.Context, q conversationQuery, id string) (Session, error) {
	var body []byte
	var v Session
	err := q.QueryRowContext(ctx, "SELECT data FROM sessions WHERE id=?", id).Scan(&body)
	if err == nil {
		err = json.Unmarshal(body, &v)
	}
	return v, err
}

// migrateConversations 原子建立包装与历史投影，不更新任何旧 Session JSON 或 checkpoint 字节。
func migrateConversations(db *sql.DB) error {
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, conversationSchema); err != nil {
		return err
	}
	var version int
	err = tx.QueryRowContext(ctx, "SELECT version FROM conversation_schema WHERE version=1").Scan(&version)
	if err == nil {
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT data FROM sessions ORDER BY id")
	if err != nil {
		return err
	}
	var sessions []Session
	for rows.Next() {
		var data []byte
		var v Session
		if err = rows.Scan(&data); err == nil {
			err = json.Unmarshal(data, &v)
		}
		if err != nil {
			rows.Close()
			return err
		}
		sessions = append(sessions, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range sessions {
		if err = bindLegacyConversation(ctx, tx, v); err != nil {
			return err
		}
		if err = backfillConversationRun(ctx, tx, v); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO conversation_schema(version) VALUES(1)"); err != nil {
		return err
	}
	return tx.Commit()
}

func bindLegacyConversation(ctx context.Context, tx *sql.Tx, v Session) error {
	var existing string
	err := tx.QueryRowContext(ctx, "SELECT conversation_id FROM conversation_runs WHERE run_id=?", v.ID).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	active := v.ID
	if v.Terminal() {
		active = ""
	}
	updated := v.Created
	if !v.Ended.IsZero() {
		updated = v.Ended
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO conversations(id,owner,title,active_run_id,created,updated,expires) VALUES(?,?,?,?,?,?,?)`, v.ID, v.Owner, v.Request, active, conversationTime(v.Created), conversationTime(updated), conversationTime(v.Expires))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO conversation_runs(run_id,conversation_id,created) VALUES(?,?,?)", v.ID, v.ID, conversationTime(v.Created))
	return err
}

func (s *Store) GetConversation(ctx context.Context, id string) (Conversation, error) {
	return loadConversation(ctx, s.db, id)
}

func (s *Store) GetRunConversation(ctx context.Context, runID string) (Conversation, error) {
	var id string
	if err := s.db.QueryRowContext(ctx, "SELECT conversation_id FROM conversation_runs WHERE run_id=?", runID).Scan(&id); err != nil {
		return Conversation{}, err
	}
	return s.GetConversation(ctx, id)
}

// ListConversations 不因读取续期，服务内部清理可以传空 owner 读取全部。
func (s *Store) ListConversations(ctx context.Context, owner string) ([]Conversation, error) {
	query := "SELECT id FROM conversations"
	var args []any
	if owner != "" {
		query += " WHERE owner=?"
		args = append(args, owner)
	}
	query += " ORDER BY updated DESC,id"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := []Conversation{}
	for _, id := range ids {
		c, e := s.GetConversation(ctx, id)
		if e != nil {
			return nil, e
		}
		if owner == "" || c.Available(time.Now()) {
			out = append(out, c)
		}
	}
	return out, nil
}

func conversationCommandHash(text, version, kind, runID, waitID string, generation int64) string {
	return tokenHash(jsonString([]any{text, version, kind, runID, waitID, generation}))
}

func repeatedConversationCommand(ctx context.Context, tx *sql.Tx, scope, key, hash string) (Session, bool, error) {
	var savedHash, runID string
	err := tx.QueryRowContext(ctx, "SELECT payload_hash,run_id FROM conversation_commands WHERE scope=? AND client_id=?", scope, key).Scan(&savedHash, &runID)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, err
	}
	if hash != savedHash {
		return Session{}, false, ErrMessageConflict
	}
	v, err := loadConversationRun(ctx, tx, runID)
	return v, true, err
}

func insertConversationCommand(ctx context.Context, tx *sql.Tx, scope, key, hash, runID string) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO conversation_commands(scope,client_id,payload_hash,run_id) VALUES(?,?,?,?)", scope, key, hash, runID)
	return err
}

func validConversationCommand(clientID, request string) error {
	if strings.TrimSpace(clientID) == "" || len(clientID) > 200 {
		return fmt.Errorf("需要有效的消息提交身份")
	}
	if strings.TrimSpace(request) == "" || len(request) > 8000 {
		return fmt.Errorf("请输入不超过8000字节的消息")
	}
	return nil
}

func insertConversationRun(ctx context.Context, tx *sql.Tx, v Session) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO sessions(id,owner,data) VALUES(?,?,?)", v.ID, v.Owner, b)
	return err
}

// CreateConversation 的创建幂等键属于访客，首次响应丢失不会重复创建会话。
func (s *Store) CreateConversation(ctx context.Context, v Session, clientID string) (Conversation, Session, bool, error) {
	if err := validConversationCommand(clientID, v.Request); err != nil {
		return Conversation{}, Session{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Conversation{}, Session{}, false, err
	}
	defer tx.Rollback()
	scope, hash := "visitor:"+v.Owner, conversationCommandHash(v.Request, "", "create", "", "", 0)
	if saved, duplicate, e := repeatedConversationCommand(ctx, tx, scope, clientID, hash); e != nil || duplicate {
		if e != nil {
			return Conversation{}, Session{}, false, e
		}
		c, e := loadConversation(ctx, tx, saved.ID)
		if e == nil && !c.Available(time.Now()) {
			e = ErrConversationExpired
		}
		return c, saved, true, e
	}
	if err = insertConversationRun(ctx, tx, v); err == nil {
		err = bindLegacyConversation(ctx, tx, v)
	}
	if err == nil {
		err = insertConversationCommand(ctx, tx, scope, clientID, hash, v.ID)
	}
	if err == nil {
		err = insertEvent(ctx, tx, v.ID, "request", map[string]any{"text": v.Request, "model": v.Model, "source": v.Source, "prompt_version": executionVersion(v), "client_message_id": clientID})
	}
	if err != nil {
		return Conversation{}, Session{}, false, err
	}
	c, err := loadConversation(ctx, tx, v.ID)
	if err == nil {
		err = tx.Commit()
	}
	return c, v, false, err
}

// AppendConversationRun 在同一事务中校验会话、固定合法版本、记录消息并占据活动位置。
func (s *Store) AppendConversationRun(ctx context.Context, id, owner, clientID, versionID string, v Session) (Session, bool, error) {
	if err := validConversationCommand(clientID, v.Request); err != nil {
		return Session{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, false, err
	}
	defer tx.Rollback()
	c, err := loadConversation(ctx, tx, id)
	if err != nil || c.Owner != owner || !c.Available(time.Now()) {
		return Session{}, false, ErrConversationExpired
	}
	scope, hash := "conversation:"+id, conversationCommandHash(v.Request, versionID, "message", "", "", 0)
	if saved, duplicate, e := repeatedConversationCommand(ctx, tx, scope, clientID, hash); e != nil || duplicate {
		return saved, duplicate, e
	}
	if c.ActiveRunID != "" {
		return Session{}, false, ErrConversationBusy
	}
	if v.Owner != owner || v.Terminal() {
		return Session{}, false, fmt.Errorf("新执行身份无效")
	}
	if versionID != "" {
		version, e := loadAssetVersion(ctx, tx, id, versionID)
		if e != nil {
			return Session{}, false, ErrInvalidVersion
		}
		v.InputVersion = &version
		if v.ConversationContext == nil {
			v.ConversationContext = map[string]any{}
		}
		v.ConversationContext["input_intent"] = version.SourceIntent
	} else {
		v.InputVersion = nil
	}
	if err = insertConversationRun(ctx, tx, v); err != nil {
		return Session{}, false, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO conversation_runs(run_id,conversation_id,created) VALUES(?,?,?)", v.ID, id, conversationTime(v.Created)); err != nil {
		return Session{}, false, err
	}
	c.ActiveRunID, c.Updated, c.Expires = v.ID, v.Created, time.Time{}
	if err = saveConversation(ctx, tx, c); err == nil {
		err = insertConversationCommand(ctx, tx, scope, clientID, hash, v.ID)
	}
	if err == nil {
		err = insertEvent(ctx, tx, v.ID, "request", map[string]any{"text": v.Request, "version_id": versionID, "model": v.Model, "source": v.Source, "prompt_version": executionVersion(v), "client_message_id": clientID})
	}
	if err == nil {
		err = tx.Commit()
	}
	return v, false, err
}

// AcceptConversationAnswer 把选模答案与问题身份共同提交，避免答案接受后版本引用丢失。
func (s *Store) AcceptConversationAnswer(ctx context.Context, id, owner, clientID, runID, waitID, versionID string, generation int64, answer string) (Session, bool, error) {
	if err := validConversationCommand(clientID, answer); err != nil {
		return Session{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, false, err
	}
	defer tx.Rollback()
	c, err := loadConversation(ctx, tx, id)
	if err != nil || c.Owner != owner || !c.Available(time.Now()) {
		return Session{}, false, ErrConversationExpired
	}
	scope, hash := "conversation:"+id, conversationCommandHash(answer, versionID, "answer", runID, waitID, generation)
	if saved, duplicate, e := repeatedConversationCommand(ctx, tx, scope, clientID, hash); e != nil || duplicate {
		return saved, duplicate, e
	}
	if c.ActiveRunID != runID {
		return Session{}, false, fmt.Errorf("回答不属于当前执行")
	}
	v, err := loadConversationRun(ctx, tx, runID)
	if err != nil {
		return v, false, err
	}
	if err = checkExecution(v, time.Now()); err != nil {
		return v, false, err
	}
	if v.Status != "awaiting_answer" || v.WaitID != waitID || v.PendingPause != nil || v.ResumePoint == nil || v.ResumePoint.Generation != generation || v.ResumePoint.Kind != "question" || v.ResumePoint.RefID != waitID {
		return v, false, recoveryError("checkpoint_identity_mismatch")
	}
	var data, metadata []byte
	if err = tx.QueryRowContext(ctx, "SELECT data,metadata FROM checkpoints WHERE session_id=? AND key=?", runID, runID).Scan(&data, &metadata); err != nil {
		return v, false, err
	}
	if _, err = decodeCheckpoint(runID, runID, data, metadata, v); err != nil {
		return v, false, err
	}
	if versionID != "" {
		version, e := loadAssetVersion(ctx, tx, id, versionID)
		if e != nil {
			return v, false, ErrInvalidVersion
		}
		if v.InputVersion != nil && v.InputVersion.ID != versionID {
			return v, false, fmt.Errorf("已确认的输入版本不能替换")
		}
		if v.Intent != nil && v.InputVersion == nil {
			return v, false, fmt.Errorf("意图保存后不能补换输入版本")
		}
		v.InputVersion = &version
		if v.ConversationContext == nil {
			v.ConversationContext = map[string]any{}
		}
		v.ConversationContext["input_intent"] = version.SourceIntent
	}
	if v.Answers == nil {
		v.Answers = map[string]AnswerRecord{}
	}
	if _, exists := v.Answers[waitID]; exists {
		return v, false, ErrMessageConflict
	}
	v.Answers[waitID] = AnswerRecord{Text: answer, Accepted: time.Now().UTC()}
	v.Answer = answer
	v.LastUser = time.Now().UTC()
	v.ResumeRequested = true
	v.Status = "understanding"
	body, err := json.Marshal(v)
	if err != nil {
		return v, false, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE sessions SET data=? WHERE id=?", body, runID); err != nil {
		return v, false, err
	}
	if err = insertConversationCommand(ctx, tx, scope, clientID, hash, runID); err == nil {
		err = insertEvent(ctx, tx, runID, "user_answer", map[string]any{"text": answer, "wait_id": waitID, "version_id": versionID, "client_message_id": clientID})
	}
	if err == nil {
		err = tx.Commit()
	}
	return v, false, err
}

// ReleaseConversationRun 仅在该 worker 退出之后调用；不能释放后来创建的另一执行。
func (s *Store) ReleaseConversationRun(ctx context.Context, runID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	if err = tx.QueryRowContext(ctx, "SELECT conversation_id FROM conversation_runs WHERE run_id=?", runID).Scan(&id); err != nil {
		return err
	}
	c, err := loadConversation(ctx, tx, id)
	if err != nil {
		return err
	}
	if c.ActiveRunID != runID {
		return nil
	}
	v, err := loadConversationRun(ctx, tx, runID)
	if err != nil {
		return err
	}
	if !v.Terminal() {
		return nil
	}
	c.ActiveRunID = ""
	c.Updated = v.Ended
	c.Expires = v.Ended.Add(v.Limits.Retention)
	if v.Limits.Retention <= 0 {
		c.Expires = v.Ended.Add(7 * 24 * time.Hour)
	}
	if err = saveConversation(ctx, tx, c); err == nil {
		err = insertEvent(ctx, tx, runID, "conversation_idle", map[string]any{"expires": c.Expires})
	}
	if err == nil {
		err = tx.Commit()
	}
	return err
}

func (s *Store) ConversationRunIDs(ctx context.Context, id string) ([]string, error) {
	return conversationRunIDs(ctx, s.db, id)
}
func conversationRunIDs(ctx context.Context, q conversationQuery, id string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT run_id FROM conversation_runs WHERE conversation_id=? ORDER BY created,run_id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var runID string
		if err = rows.Scan(&runID); err != nil {
			return nil, err
		}
		out = append(out, runID)
	}
	return out, rows.Err()
}

// ReconcileConversations 在启动 worker 之前核对绑定并清理已停止进程遗留的活动指针。
func (s *Store) ReconcileConversations(ctx context.Context) error {
	all, err := s.ListConversations(ctx, "")
	if err != nil {
		return err
	}
	for _, c := range all {
		ids, e := s.ConversationRunIDs(ctx, c.ID)
		if e != nil {
			return e
		}
		var active string
		for _, id := range ids {
			v, e := s.Get(ctx, id)
			if e != nil {
				return e
			}
			if v.Owner != c.Owner {
				return fmt.Errorf("执行与会话归属不一致")
			}
			if !v.Terminal() {
				if active != "" {
					return fmt.Errorf("同会话存在多个活动执行")
				}
				active = id
			}
		}
		if c.Cleaning && active != "" {
			return fmt.Errorf("清理中的会话仍有活动执行")
		}
		if active == "" && c.ActiveRunID != "" {
			if err = s.ReleaseConversationRun(ctx, c.ActiveRunID); err != nil {
				return err
			}
		} else if active != "" && active != c.ActiveRunID {
			if _, err = s.db.ExecContext(ctx, "UPDATE conversations SET active_run_id=?,expires=0 WHERE id=? AND cleaning=0", active, c.ID); err != nil {
				return err
			}
		}
	}
	var missing int
	if err = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions s LEFT JOIN conversation_runs r ON r.run_id=s.id WHERE r.run_id IS NULL").Scan(&missing); err != nil {
		return err
	}
	if missing > 0 {
		return fmt.Errorf("存在缺少会话关联的执行")
	}
	return nil
}

// MarkConversationCleanup 与消息接受串行提交；标记之后所有访问入口必须拒绝该会话。
func (s *Store) MarkConversationCleanup(ctx context.Context, id string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	c, err := loadConversation(ctx, tx, id)
	if err != nil {
		return false, err
	}
	if c.Cleaning {
		return true, nil
	}
	if c.ActiveRunID != "" || c.Expires.IsZero() || now.Before(c.Expires) {
		return false, nil
	}
	c.Cleaning = true
	if err = saveConversation(ctx, tx, c); err == nil {
		err = tx.Commit()
	}
	return err == nil, err
}

// DeleteConversation 在上层成功清理该会话所有模型目录后级联删除数据库记录。
func (s *Store) DeleteConversation(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c, err := loadConversation(ctx, tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !c.Cleaning || c.ActiveRunID != "" {
		return fmt.Errorf("会话尚未进入清理状态")
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE id IN (SELECT run_id FROM conversation_runs WHERE conversation_id=?)", id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM conversations WHERE id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}
