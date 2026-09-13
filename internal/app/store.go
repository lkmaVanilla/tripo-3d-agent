package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store 用 SQLite 持久化业务会话、执行事件、Eino 检查点及匿名访客凭证。
// Session 的 JSON 是业务事实来源；检查点只记录框架执行位置，不能替代业务状态。
type Store struct{ db *sql.DB }

// Event 是持久化的执行事件，前端用 Seq 游标断线续读。
// Seq 在整张表中递增，因此同一会话的序号有空隙是正常情况。
type Event struct {
	Seq  int64           `json:"seq"`
	Kind string          `json:"kind"`
	Time time.Time       `json:"time"`
	Data json.RawMessage `json:"data"`
}

// OpenStore 创建数据目录并完成兼容迁移，成功返回后数据库结构即可供恢复对账使用。
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "tripo.db"))
	if err != nil {
		return nil, err
	}
	// 单连接串行化数据库操作，也让连接级 PRAGMA 一致生效。
	// 事务回调中不能再调用 Store 方法取连接，否则会等待自己尚未释放的唯一连接。
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;
CREATE TABLE IF NOT EXISTS sessions(id TEXT PRIMARY KEY, owner TEXT NOT NULL, data BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS session_owner ON sessions(owner);
CREATE TABLE IF NOT EXISTS events(seq INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE, kind TEXT NOT NULL, at TEXT NOT NULL, data BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS event_session ON events(session_id,seq);
CREATE TABLE IF NOT EXISTS checkpoints(session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE, key TEXT NOT NULL, data BLOB NOT NULL, PRIMARY KEY(session_id,key));
CREATE TABLE IF NOT EXISTS visitors(hash TEXT PRIMARY KEY, expires INTEGER NOT NULL);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err = migrateCheckpointMetadata(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close 释放连接；调用方应先停止仍会访问存储的调度器与 Runner。
func (s *Store) Close() error { return s.db.Close() }

// Create 原子保存会话与首个请求事件，避免可见会话缺少执行轨迹起点。
func (s *Store) Create(ctx context.Context, v Session) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO sessions(id,owner,data) VALUES(?,?,?)", v.ID, v.Owner, b); err != nil {
		return err
	}
	if err = insertEvent(ctx, tx, v.ID, "request", map[string]any{"text": v.Request, "model": v.Model, "source": v.Source, "prompt_version": PromptVersion, "tripo_model": "v3.1-20260211"}); err != nil {
		return err
	}
	return tx.Commit()
}

// Get 返回持久化状态的独立快照；读取后内存修改不会自动保存，需要使用 Edit。
// 访客归属校验由调用方完成，Store 同时也服务于跨会话的内部调度。
func (s *Store) Get(ctx context.Context, id string) (Session, error) {
	var v Session
	var b []byte
	err := s.db.QueryRowContext(ctx, "SELECT data FROM sessions WHERE id=?", id).Scan(&b)
	if err == nil {
		err = json.Unmarshal(b, &v)
	}
	return v, err
}

// List 按访客归属读取会话；owner 为空时读取全部，供调度和启动对账使用。
// 查询不保证顺序，FIFO 等业务排序必须由调用方明确处理。
func (s *Store) List(ctx context.Context, owner string) ([]Session, error) {
	query := "SELECT data FROM sessions"
	args := []any{}
	if owner != "" {
		query += " WHERE owner=?"
		args = append(args, owner)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		var b []byte
		var v Session
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Edit 在一个事务中读取最新 Session、应用修改并追加可选事件。
// fn 应在这里核对预算、终态等并发约束，不能用事务外旧快照整体覆盖当前事实。
// fn 不得重入 Store 或执行外部请求；返回错误会连同事件一起回滚，kind 为空则不追加事件。
func (s *Store) Edit(ctx context.Context, id string, fn func(*Session) error, kind string, data any) (Session, error) {
	var v Session
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return v, err
	}
	defer tx.Rollback()
	var b []byte
	if err = tx.QueryRowContext(ctx, "SELECT data FROM sessions WHERE id=?", id).Scan(&b); err != nil {
		return v, err
	}
	if err = json.Unmarshal(b, &v); err != nil {
		return v, err
	}
	if fn != nil {
		if err = fn(&v); err != nil {
			return v, err
		}
	}
	b, err = json.Marshal(v)
	if err != nil {
		return v, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE sessions SET data=? WHERE id=?", b, id); err != nil {
		return v, err
	}
	if kind != "" {
		if err = insertEvent(ctx, tx, id, kind, data); err != nil {
			return v, err
		}
	}
	err = tx.Commit()
	return v, err
}

// insertEvent 使用调用方事务，保证事件只描述同次提交中已经生效的业务事实。
func insertEvent(ctx context.Context, tx *sql.Tx, id, kind string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO events(session_id,kind,at,data) VALUES(?,?,?,?)", id, kind, time.Now().UTC().Format(time.RFC3339Nano), b)
	return err
}

// Events 按序返回指定会话中严格晚于 after 的事件，用于 WebSocket 增量推送和重连补发。
func (s *Store) Events(ctx context.Context, id string, after int64) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT seq,kind,at,data FROM events WHERE session_id=? AND seq>? ORDER BY seq", id, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var at string
		if err = rows.Scan(&e.Seq, &e.Kind, &at, &e.Data); err != nil {
			return nil, err
		}
		e.Time, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// tokenHash 提供稳定摘要：访客凭证只存摘要，恢复协议也用它绑定公开消息和参数。
func tokenHash(token string) string {
	x := sha256.Sum256([]byte(token))
	return hex.EncodeToString(x[:])
}

// Visitor 续期有效匿名访客，或为缺失、未知及过期凭证创建新身份。
// 返回值依次为发给浏览器的凭证和内部归属标识；后者已经是摘要，数据库不保存原始凭证。
func (s *Store) Visitor(ctx context.Context, token string, ttl time.Duration) (string, string, error) {
	hash := tokenHash(token)
	var expires int64
	err := s.db.QueryRowContext(ctx, "SELECT expires FROM visitors WHERE hash=?", hash).Scan(&expires)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}
	if token == "" || err != nil || expires <= time.Now().Unix() {
		token = newID()
		hash = tokenHash(token)
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO visitors(hash,expires) VALUES(?,?) ON CONFLICT(hash) DO UPDATE SET expires=excluded.expires", hash, time.Now().Add(ttl).Unix())
	return token, hash, err
}

// ValidVisitor 接收内部凭证摘要，查询失败或已过期时均视为无效。
func (s *Store) ValidVisitor(ctx context.Context, hash string) bool {
	var t int64
	return s.db.QueryRowContext(ctx, "SELECT expires FROM visitors WHERE hash=?", hash).Scan(&t) == nil && t > time.Now().Unix()
}

// Delete 删除会话，并通过外键级联删除事件和检查点；磁盘资产文件由上层清理。
func (s *Store) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE id=?", id)
	return err
}

// checkpointStore 保留旧协议的读写适配能力，新协议由 pauseCoordinator 原子提交。
// 旧写入者只能更新尚无元数据的记录，不能破坏已经迁移的新恢复点。
type checkpointStore struct {
	store     *Store
	sessionID string
}

// Set 保存属于当前会话的旧检查点；新协议记录存在时返回冲突而非覆盖。
func (c checkpointStore) Set(ctx context.Context, key string, value []byte) error {
	if key != c.sessionID {
		return fmt.Errorf("%w: legacy key does not belong to session", ErrCheckpointMismatch)
	}
	result, err := c.store.db.ExecContext(ctx, "INSERT INTO checkpoints(session_id,key,data) VALUES(?,?,?) ON CONFLICT(session_id,key) DO UPDATE SET data=excluded.data WHERE checkpoints.metadata IS NULL", c.sessionID, key, value)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return fmt.Errorf("%w: legacy writer cannot overwrite a committed recovery point", ErrCheckpointMismatch)
	}
	return err
}

// Get 使用统一的归属与内容校验，记录不存在时遵循 Eino 的 found=false 约定。
func (c checkpointStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	record, err := c.store.LoadCheckpoint(ctx, c.sessionID, key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return record.Data, err == nil, err
}
