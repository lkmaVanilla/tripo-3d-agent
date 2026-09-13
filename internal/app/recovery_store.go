package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrCheckpointMismatch 表示持久化证据互相矛盾，应拒绝恢复而不是盲目重试写入。
var ErrCheckpointMismatch = errors.New("恢复点身份或内容不匹配")

// checkpointRecord 保留 Eino 原始字节和应用元数据；Point 为 nil 仅表示待验证的旧协议。
type checkpointRecord struct {
	Data  []byte
	Point *ResumePoint
}

// checkpointEvent 与检查点和业务状态共用事务，避免回放事件宣称一个未成功提交的事实。
type checkpointEvent struct {
	Kind string
	Data any
}

// migrateCheckpointMetadata 只增加可空元数据列，保留旧检查点字节供后续验证迁移。
// SQLite 的 DDL 也在事务中；中途退出可重新执行，不会留下半迁移结构。
func migrateCheckpointMetadata(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query("PRAGMA table_info(checkpoints)")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err = rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		found = found || name == "metadata"
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if !found {
		if _, err = tx.Exec("ALTER TABLE checkpoints ADD COLUMN metadata BLOB"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// checkpointDigest 对框架原始字节计算完整性摘要，不尝试解释其中的执行状态。
func checkpointDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

// validateCheckpointIdentity 校验会话归属、应用协议和暂停身份，不读取检查点内部内容。
// 根 key 固定为会话 ID；检查点代次由元数据表达，而不是通过更换 Eino key 表达。
func validateCheckpointIdentity(id, key string, point ResumePoint) error {
	if key != id || point.SessionID != id || point.Key != key || point.Version != recoveryVersion || point.Generation < 1 || point.PauseID == "" || (point.Kind != "question" && point.Kind != "production") || point.RefID == "" {
		return fmt.Errorf("%w: invalid session, key or pause identity", ErrCheckpointMismatch)
	}
	if digest, err := hex.DecodeString(point.Digest); err != nil || len(digest) != sha256.Size {
		return fmt.Errorf("%w: invalid digest metadata", ErrCheckpointMismatch)
	}
	return nil
}

// validateCheckpointPoint 在身份校验外确认字节与摘要一致，防止元数据和内容错配。
func validateCheckpointPoint(id, key string, point ResumePoint, value []byte) error {
	if err := validateCheckpointIdentity(id, key, point); err != nil {
		return err
	}
	if len(value) == 0 || point.Digest != checkpointDigest(value) {
		return fmt.Errorf("%w: checkpoint digest", ErrCheckpointMismatch)
	}
	return nil
}

// decodeCheckpointMetadata 仅解码应用元数据，不解析 Eino 的 gob。
// 旧记录双方都无 ResumePoint 才可进入兼容验证；新记录缺失元数据不能降级成旧协议。
func decodeCheckpointMetadata(id, key string, metadata []byte, session Session) (*ResumePoint, error) {
	if session.ID != id || key != id {
		return nil, fmt.Errorf("%w: checkpoint belongs to another session", ErrCheckpointMismatch)
	}
	if metadata == nil {
		if session.ResumePoint != nil {
			return nil, fmt.Errorf("%w: committed point has no metadata", ErrCheckpointMismatch)
		}
		return nil, nil
	}
	var point ResumePoint
	if err := json.Unmarshal(metadata, &point); err != nil {
		return nil, fmt.Errorf("%w: invalid checkpoint metadata", ErrCheckpointMismatch)
	}
	if err := validateCheckpointIdentity(id, key, point); err != nil {
		return nil, err
	}
	if session.RecoverySchemaVersion != recoveryVersion || session.ResumePoint == nil || *session.ResumePoint != point {
		return nil, fmt.Errorf("%w: session and checkpoint disagree", ErrCheckpointMismatch)
	}
	return &point, nil
}

// decodeCheckpoint 将应用绑定校验与字节摘要校验组合起来，原始字节仍留给 Eino 解码。
func decodeCheckpoint(id, key string, value, metadata []byte, session Session) (checkpointRecord, error) {
	record := checkpointRecord{Data: value}
	point, err := decodeCheckpointMetadata(id, key, metadata, session)
	if err != nil {
		return record, err
	}
	if point != nil {
		if err = validateCheckpointPoint(id, key, *point, value); err != nil {
			return record, err
		}
	}
	record.Point = point
	return record, nil
}

// LoadCheckpoint 在同一条查询中读取检查点和 Session，避免分别读取时跨越一次提交。
// 返回旧协议记录不代表已经可安全恢复，还需要通过受限 Runner 验证其业务引用。
func (s *Store) LoadCheckpoint(ctx context.Context, id, key string) (checkpointRecord, error) {
	if key != id {
		return checkpointRecord{}, fmt.Errorf("%w: checkpoint key belongs to another session", ErrCheckpointMismatch)
	}
	var value, metadata, body []byte
	err := s.db.QueryRowContext(ctx, `SELECT c.data,c.metadata,s.data FROM checkpoints c JOIN sessions s ON s.id=c.session_id WHERE c.session_id=? AND c.key=?`, id, key).Scan(&value, &metadata, &body)
	if err != nil {
		return checkpointRecord{}, err
	}
	var session Session
	if err = json.Unmarshal(body, &session); err != nil {
		return checkpointRecord{}, err
	}
	return decodeCheckpoint(id, key, value, metadata, session)
}

// CommitCheckpoint 是新协议唯一的检查点写入入口。
// 它用预期代次拒绝过期写入，并把检查点、最新业务状态和对应事件作为一个事务提交。
// apply 只能修改传入的最新 Session；不能调用 Store 或等待外部服务，以免占住唯一数据库连接。
func (s *Store) CommitCheckpoint(ctx context.Context, id string, expectedGeneration int64, point ResumePoint, value []byte, apply func(*Session) ([]checkpointEvent, error)) (Session, error) {
	var session Session
	if expectedGeneration < 0 || point.Generation != expectedGeneration+1 {
		return session, fmt.Errorf("%w: invalid next generation", ErrCheckpointMismatch)
	}
	digest := checkpointDigest(value)
	if point.Digest != "" && point.Digest != digest {
		return session, fmt.Errorf("%w: supplied digest", ErrCheckpointMismatch)
	}
	point.Digest = digest
	if err := validateCheckpointPoint(id, point.Key, point, value); err != nil {
		return session, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return session, err
	}
	defer tx.Rollback()
	var body []byte
	if err = tx.QueryRowContext(ctx, "SELECT data FROM sessions WHERE id=?", id).Scan(&body); err != nil {
		return session, err
	}
	if err = json.Unmarshal(body, &session); err != nil {
		return session, err
	}
	if session.ID != id {
		return session, fmt.Errorf("%w: stored session identity", ErrCheckpointMismatch)
	}
	// 提交时重新读到的终态与期限优先于旧 Runner 的执行快照，禁止迟到回调重新激活会话。
	if err = checkExecution(session, time.Now()); err != nil {
		return session, err
	}
	var previousData, previousMetadata []byte
	err = tx.QueryRowContext(ctx, "SELECT data,metadata FROM checkpoints WHERE session_id=? AND key=?", id, point.Key).Scan(&previousData, &previousMetadata)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return session, err
	}
	if err == nil {
		previous, decodeErr := decodeCheckpointMetadata(id, point.Key, previousMetadata, session)
		if decodeErr != nil {
			return session, decodeErr
		}
		if previous != nil && *previous == point {
			if err = validateCheckpointPoint(id, point.Key, *previous, previousData); err != nil {
				return session, err
			}
			return session, nil // 同一身份、代次和字节的重复确认不再应用业务变更，保留后来接受的答案。
		}
	} else if session.ResumePoint != nil {
		return session, fmt.Errorf("%w: committed checkpoint missing", ErrCheckpointMismatch)
	}
	// 先识别已经成功提交的幂等重试，再拒绝真正过期的写入者。
	if session.generation() != expectedGeneration {
		return session, fmt.Errorf("%w: stale checkpoint writer", ErrCheckpointMismatch)
	}
	if session.PendingPause == nil || session.PendingPause.ExpectedGeneration != expectedGeneration {
		return session, fmt.Errorf("%w: pending pause missing or stale", ErrCheckpointMismatch)
	}
	pendingPoint := session.PendingPause.Point
	pendingPoint.Digest = digest
	if pendingPoint != point || (session.PendingPause.Point.Digest != "" && session.PendingPause.Point.Digest != digest) {
		return session, fmt.Errorf("%w: pending pause identity", ErrCheckpointMismatch)
	}
	metadata, err := json.Marshal(point)
	if err != nil {
		return session, err
	}
	// 后续业务校验或事件写入失败时，这次检查点替换也随事务回滚。
	if _, err = tx.ExecContext(ctx, `INSERT INTO checkpoints(session_id,key,data,metadata) VALUES(?,?,?,?) ON CONFLICT(session_id,key) DO UPDATE SET data=excluded.data,metadata=excluded.metadata`, id, point.Key, value, metadata); err != nil {
		return session, err
	}
	owner := session.Owner
	var events []checkpointEvent
	if apply != nil {
		if events, err = apply(&session); err != nil {
			return session, err
		}
	}
	if session.ID != id || session.Owner != owner {
		return session, fmt.Errorf("%w: callback changed session ownership", ErrCheckpointMismatch)
	}
	session.ResumePoint = &point
	session.RecoverySchemaVersion = recoveryVersion
	// 草案只在同一事务内清除，保证退出后至少能找到已提交恢复点或尚可重建的草案。
	session.PendingPause = nil
	body, err = json.Marshal(session)
	if err != nil {
		return session, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE sessions SET data=? WHERE id=?", body, id); err != nil {
		return session, err
	}
	events = append(events, checkpointEvent{Kind: "checkpoint_committed", Data: map[string]any{"pause_id": point.PauseID, "generation": point.Generation, "kind": point.Kind, "ref_id": point.RefID}})
	for _, event := range events {
		if event.Kind == "" {
			continue
		}
		if err = insertEvent(ctx, tx, id, event.Kind, event.Data); err != nil {
			return session, err
		}
	}
	if err = tx.Commit(); err != nil {
		return session, err
	}
	return session, nil
}
