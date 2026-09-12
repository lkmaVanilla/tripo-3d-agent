package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }
type Event struct {
	Seq  int64           `json:"seq"`
	Kind string          `json:"kind"`
	Time time.Time       `json:"time"`
	Data json.RawMessage `json:"data"`
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "tripo.db"))
	if err != nil {
		return nil, err
	}
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
	return &Store{db: db}, nil
}
func (s *Store) Close() error { return s.db.Close() }
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
func (s *Store) Get(ctx context.Context, id string) (Session, error) {
	var v Session
	var b []byte
	err := s.db.QueryRowContext(ctx, "SELECT data FROM sessions WHERE id=?", id).Scan(&b)
	if err == nil {
		err = json.Unmarshal(b, &v)
	}
	return v, err
}
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
func insertEvent(ctx context.Context, tx *sql.Tx, id, kind string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO events(session_id,kind,at,data) VALUES(?,?,?,?)", id, kind, time.Now().UTC().Format(time.RFC3339Nano), b)
	return err
}
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
func tokenHash(token string) string {
	x := sha256.Sum256([]byte(token))
	return hex.EncodeToString(x[:])
}
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
func (s *Store) ValidVisitor(ctx context.Context, hash string) bool {
	var t int64
	return s.db.QueryRowContext(ctx, "SELECT expires FROM visitors WHERE hash=?", hash).Scan(&t) == nil && t > time.Now().Unix()
}
func (s *Store) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE id=?", id)
	return err
}

type checkpointStore struct {
	store     *Store
	sessionID string
}

func (c checkpointStore) Set(ctx context.Context, key string, value []byte) error {
	_, err := c.store.db.ExecContext(ctx, "INSERT INTO checkpoints(session_id,key,data) VALUES(?,?,?) ON CONFLICT(session_id,key) DO UPDATE SET data=excluded.data", c.sessionID, key, value)
	return err
}
func (c checkpointStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	var b []byte
	err := c.store.db.QueryRowContext(ctx, "SELECT data FROM checkpoints WHERE session_id=? AND key=?", c.sessionID, key).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return b, err == nil, err
}
