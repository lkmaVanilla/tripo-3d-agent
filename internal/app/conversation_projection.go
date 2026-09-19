package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"
)

func loadAssetVersion(ctx context.Context, q conversationQuery, conversationID, id string) (AssetVersion, error) {
	var v AssetVersion
	var body, intent []byte
	err := q.QueryRowContext(ctx, "SELECT data,path,source_intent FROM asset_versions WHERE id=? AND conversation_id=?", id, conversationID).Scan(&body, &v.Path, &intent)
	if err != nil {
		return v, err
	}
	if err = json.Unmarshal(body, &v); err != nil {
		return v, err
	}
	if len(intent) > 0 && string(intent) != "null" {
		err = json.Unmarshal(intent, &v.SourceIntent)
	}
	return v, err
}

// GetAssetVersion 返回私有文件路径，调用者必须先校验会话归属。
func (s *Store) GetAssetVersion(ctx context.Context, conversationID, id string) (AssetVersion, error) {
	return loadAssetVersion(ctx, s.db, conversationID, id)
}

func conversationFileDigest(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("模型不是普通文件")
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func versionUsable(v AssetVersion) bool {
	if !v.Report.Valid || v.SHA256 == "" || v.Path == "" {
		return false
	}
	info, err := os.Stat(v.Path)
	return err == nil && info.Mode().IsRegular() && info.Size() == v.Bytes
}

// registerConversationVersion 只追加真实保存的产物，唯一操作身份使重放不新增版本号。
func registerConversationVersion(ctx context.Context, tx *sql.Tx, c Conversation, run Session, a Artifact, legacy bool, at time.Time) error {
	var existing string
	err := tx.QueryRowContext(ctx, "SELECT id FROM asset_versions WHERE run_id=? AND operation_id=?", run.ID, a.ID).Scan(&existing)
	if err == nil {
		if existing != a.ID {
			return fmt.Errorf("操作产物身份冲突")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if !legacy && (run.Current == nil || run.Current.ID != a.ID || run.Current.ArtifactID != a.ID || run.Current.TaskID != a.TaskID || run.Current.Stage != "done") {
		return fmt.Errorf("版本缺少已完成操作的产物证据")
	}
	digest, size, fileErr := conversationFileDigest(a.Path)
	if fileErr != nil && !legacy {
		return fmt.Errorf("无法保存版本的文件证据：%w", fileErr)
	}
	v := AssetVersion{ID: a.ID, ConversationID: c.ID, SourceRunID: run.ID, SourceOperationID: a.ID, TaskID: a.TaskID, Path: a.Path, SHA256: digest, Bytes: size, Format: "glb", Report: publicReport(run, a), SourceIntent: run.Intent, Created: at, ProvenanceStatus: "verified", URL: "/api/conversations/" + c.ID + "/versions/" + a.ID + "/file"}
	if legacy {
		v.ProvenanceStatus = "historical_unknown"
		if fileErr != nil {
			v.Bytes = a.Report.Bytes
		}
	}
	if op := run.Current; op != nil && op.ID == a.ID && op.ArtifactID == a.ID && op.TaskID == a.TaskID && op.Stage == "done" {
		v.OperationKind = op.Kind
		v.ParentVersionID = op.InputVersionID
		v.ContextReferenceID = op.ContextReferenceID
		if !legacy {
			v.ProvenanceStatus = "verified"
		}
		if legacy && op.Kind == "generate" {
			v.ProvenanceStatus = "historical_evidence"
		}
		if op.Kind == "decimate" && v.ParentVersionID == "" {
			// 旧操作仅保留输入 URL 时，只接受唯一精确匹配，不以产物顺序推断父关系。
			var matches []string
			for _, parent := range run.Artifacts {
				if parent.ID != a.ID && parent.SourceURL != "" && parent.SourceURL == op.Params.Input {
					matches = append(matches, parent.ID)
				}
			}
			if len(matches) == 1 {
				v.ParentVersionID = matches[0]
				v.ProvenanceStatus = "historical_evidence"
			}
		}
	}
	for _, ref := range []string{v.ParentVersionID, v.ContextReferenceID} {
		if ref != "" {
			var parentConversation string
			err = tx.QueryRowContext(ctx, "SELECT conversation_id FROM asset_versions WHERE id=?", ref).Scan(&parentConversation)
			// 旧数组顺序不一定等于生产顺序；同一原 Run 中有明确身份的父候选可随后回填。
			if legacy && errors.Is(err, sql.ErrNoRows) {
				for _, parent := range run.Artifacts {
					if parent.ID == ref && parent.ID != a.ID {
						parentConversation, err = c.ID, nil
						break
					}
				}
			}
			if err != nil || parentConversation != c.ID {
				return fmt.Errorf("版本来源不属于当前会话")
			}
		}
	}
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(number),0)+1 FROM asset_versions WHERE conversation_id=?", c.ID).Scan(&v.Number); err != nil {
		return err
	}
	v.Processable = versionUsable(v)
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	intent, err := json.Marshal(run.Intent)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO asset_versions(id,conversation_id,run_id,operation_id,number,data,path,source_intent) VALUES(?,?,?,?,?,?,?,?)", v.ID, c.ID, run.ID, a.ID, v.Number, body, a.Path, intent)
	return err
}

func publicConversationData(raw json.RawMessage) map[string]any {
	data := map[string]any{}
	_ = json.Unmarshal(raw, &data)
	return data
}
func conversationString(data map[string]any, key string) string { v, _ := data[key].(string); return v }

func saveConversationMessage(ctx context.Context, tx *sql.Tx, key string, m ConversationMessage) error {
	var body []byte
	err := tx.QueryRowContext(ctx, "SELECT data FROM conversation_messages WHERE conversation_id=? AND source_key=?", m.ConversationID, key).Scan(&body)
	if err == nil {
		var old ConversationMessage
		if err = json.Unmarshal(body, &old); err != nil {
			return err
		}
		m.ID, m.Seq, m.Created = old.ID, old.Seq, old.Created
		if old.UpdatedSeq > m.UpdatedSeq {
			return nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if m.ID == "" {
		m.ID = tokenHash(m.ConversationID + ":" + key)
	}
	body, err = json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO conversation_messages(id,conversation_id,run_id,source_key,seq,updated_seq,data) VALUES(?,?,?,?,?,?,?) ON CONFLICT(conversation_id,source_key) DO UPDATE SET updated_seq=excluded.updated_seq,data=excluded.data`, m.ID, m.ConversationID, m.RunID, key, m.Seq, m.UpdatedSeq, body)
	return err
}

func cacheConversationRun(ctx context.Context, tx *sql.Tx, v Session, seq int64) error {
	view := conversationRunView(v)
	var violations int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE session_id=? AND seq<=? AND kind='runtime_blocked' AND json_extract(data,'$.code') IN ('budget','constraint','false_validation')`, v.ID, seq).Scan(&violations)
	if err != nil {
		return err
	}
	e := evaluate(v)
	e.Checks = append(e.Checks, proposalViolationCheck(violations))
	view["evaluation"] = e
	body, err := json.Marshal(view)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO conversation_run_views(run_id,event_seq,data) VALUES(?,?,?) ON CONFLICT(run_id) DO UPDATE SET event_seq=MAX(event_seq,excluded.event_seq),data=excluded.data`, v.ID, seq, body)
	return err
}

func projectConversationResult(ctx context.Context, tx *sql.Tx, c Conversation, v Session, e Event) error {
	// 结束时刷新原操作卡，不能把未知提交永远显示成“正在提交”。
	if v.Current != nil {
		var body []byte
		key := "operation:" + v.Current.ID
		err := tx.QueryRowContext(ctx, "SELECT data FROM conversation_messages WHERE conversation_id=? AND source_key=?", c.ID, key).Scan(&body)
		if err == nil {
			var card ConversationMessage
			if err = json.Unmarshal(body, &card); err != nil {
				return err
			}
			if card.Data == nil {
				card.Data = map[string]any{}
			}
			card.UpdatedSeq = e.Seq
			card.Data["run_status"] = v.Status
			if v.Current.Stage == "submitting" && v.Current.TaskID == "" {
				card.Data["status"] = "submission_unknown"
				card.Data["error_summary"] = "本次本地执行已停止；远端是否创建任务无法确认，系统没有自动重试。"
			}
			if summary := providerFailureText(v); summary != "" {
				card.Data["error_summary"] = summary
			}
			if err = saveConversationMessage(ctx, tx, key, card); err != nil {
				return err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	view := conversationRunView(v)
	text, _ := view["final"].(string)
	kind := "run_result"
	if v.Outcome != nil && v.Outcome.Kind == "answer" {
		kind = "answer"
		text = v.Outcome.Text
	}
	m := ConversationMessage{ConversationID: c.ID, RunID: v.ID, Kind: kind, Text: text, Seq: e.Seq, UpdatedSeq: e.Seq, Created: e.Time, Data: map[string]any{"run_id": v.ID, "result": view["result"], "outcome": view["outcome"], "source": "runtime"}}
	if kind == "answer" {
		m.Data["source"] = "agent"
		m.Data["verification"] = "unverifiable"
	}
	return saveConversationMessage(ctx, tx, "result:"+v.ID, m)
}

// projectConversationEvent 只投影已在当前事务中成立的业务事件，绝不公开模型内部消息。
func projectConversationEvent(ctx context.Context, tx *sql.Tx, v Session, c Conversation, e Event, legacy bool) error {
	data := publicConversationData(e.Data)
	m := ConversationMessage{ConversationID: c.ID, RunID: v.ID, Seq: e.Seq, UpdatedSeq: e.Seq, Created: e.Time}
	key := ""
	switch e.Kind {
	case "request":
		m.Kind, m.Text = "user", conversationString(data, "text")
		if m.Text == "" {
			m.Text = v.Request
		}
		m.VersionID = conversationString(data, "version_id")
		key = "request:" + v.ID
	case "user_answer":
		m.Kind, m.Text = "user", conversationString(data, "text")
		m.WaitID = conversationString(data, "wait_id")
		m.VersionID = conversationString(data, "version_id")
		key = fmt.Sprintf("event:%d", e.Seq)
	case "clarification":
		m.Kind, m.Text = "clarification", conversationString(data, "question")
		if !legacy {
			m.WaitID = v.WaitID
		}
		key = fmt.Sprintf("event:%d", e.Seq)
		m.Data = map[string]any{"generation": v.generation()}
	case "intent_and_plan":
		m.Kind = "accepted_plan"
		m.Data = map[string]any{"intent": data}
		key = "plan:" + v.ID
	case "intent_review":
		m.Kind = "intent_review"
		m.Text = "以下是相对引用版本的需求差异，尚未保存为正式目标；后续已保存的计划才是本次执行依据。"
		m.Data = data
		m.VersionID = conversationString(data, "version_id")
		key = "intent-review:" + v.ID + ":" + conversationString(data, "id")
	case "runtime_accepted", "tool_submitting", "tool_submitted", "tripo_progress", "technical_report", "tool_failed", "production_slot", "queued", "provider_call_failed", "provider_call_recovered", "input_prepared":
		opID := conversationString(data, "operation_id")
		if opID == "" && !legacy && v.Current != nil {
			opID = v.Current.ID
		}
		if opID == "" {
			break
		}
		m.Kind = "operation_card"
		key = "operation:" + opID
		m.Data = map[string]any{"operation_id": opID}
		var prior []byte
		if err := tx.QueryRowContext(ctx, "SELECT data FROM conversation_messages WHERE conversation_id=? AND source_key=?", c.ID, key).Scan(&prior); err == nil {
			var previous ConversationMessage
			if json.Unmarshal(prior, &previous) == nil && previous.Data != nil {
				m.Data = previous.Data
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		for _, field := range []string{"task_id", "status", "progress", "credits", "state", "target_triangles"} {
			if value, ok := data[field]; ok {
				m.Data[field] = value
			}
		}
		if kind := conversationString(data, "kind"); kind != "" {
			m.Data["operation_kind"] = kind
		}
		switch e.Kind {
		case "runtime_accepted", "queued":
			m.Data["stage"] = "ready"
			m.Data["status"] = v.Status
		case "production_slot":
			m.Data["stage"] = "ready"
			m.Data["status"] = "running"
		case "tool_submitting":
			m.Data["stage"] = "submitting"
		case "tool_submitted", "tripo_progress":
			m.Data["stage"] = "submitted"
			if e.Kind == "tool_submitted" {
				delete(m.Data, "diagnostic")
				delete(m.Data, "error_summary")
			}
		case "tool_failed":
			m.Data["error_summary"] = conversationString(data, "error")
			m.Data["stage"] = "done"
			m.Data["status"] = "failed"
		case "provider_call_failed":
			m.Data["diagnostic"] = data
			if unknown, _ := data["submission_unknown"].(bool); unknown {
				m.Data["status"] = "submission_unknown"
			}
		case "provider_call_recovered", "input_prepared":
			delete(m.Data, "diagnostic")
			delete(m.Data, "error_summary")
		case "technical_report":
			delete(m.Data, "diagnostic")
			delete(m.Data, "error_summary")
			m.Data["stage"] = "done"
			m.Data["status"] = "checked"
			artifactID := conversationString(data, "artifact_id")
			for _, a := range v.Artifacts {
				if a.ID == artifactID {
					m.VersionID = a.ID
					m.Data["report"] = publicReport(v, a)
					if !legacy {
						if err := registerConversationVersion(ctx, tx, c, v, a, false, e.Time); err != nil {
							return err
						}
					}
				}
			}
		}
		if !legacy && v.Current != nil && v.Current.ID == opID {
			m.Data["operation_kind"] = v.Current.Kind
			m.Data["stage"] = v.Current.Stage
		}
	}
	if key != "" {
		if err := saveConversationMessage(ctx, tx, key, m); err != nil {
			return err
		}
	}
	if !legacy && v.Terminal() {
		if err := projectConversationResult(ctx, tx, c, v, e); err != nil {
			return err
		}
	}
	if !legacy {
		return cacheConversationRun(ctx, tx, v, e.Seq)
	}
	return nil
}

// conversationEventHook 被 insertEvent 在事务内调用，覆盖普通事件和原子 checkpoint 发布。
func conversationEventHook(ctx context.Context, tx *sql.Tx, runID string, e Event) error {
	var id string
	err := tx.QueryRowContext(ctx, "SELECT conversation_id FROM conversation_runs WHERE run_id=?", runID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	c, err := loadConversation(ctx, tx, id)
	if err != nil {
		return err
	}
	v, err := loadConversationRun(ctx, tx, runID)
	if err != nil {
		return err
	}
	return projectConversationEvent(ctx, tx, v, c, e, false)
}

// legacyArtifactEvidence 只拼接同一操作的提交、TaskID 与产物登记证据。
// 已被后续操作覆盖的 Current 仍可由原事件还原；缺失或矛盾时保留未知来源。
func legacyArtifactEvidence(v Session, a Artifact, events []Event) (Session, time.Time) {
	if op := v.Current; op != nil && op.ID == a.ID && op.ArtifactID == a.ID && op.TaskID == a.TaskID && op.Stage == "done" {
		return v, v.Created
	}
	var op Operation
	var submitted, completed bool
	var at time.Time
	for _, e := range events {
		var record struct {
			OperationID string          `json:"operation_id"`
			TaskID      string          `json:"task_id"`
			ArtifactID  string          `json:"artifact_id"`
			Kind        string          `json:"kind"`
			Params      json.RawMessage `json:"params"`
		}
		if json.Unmarshal(e.Data, &record) != nil || record.OperationID != a.ID {
			continue
		}
		switch e.Kind {
		case "tool_submitting":
			if op.ID != "" || (record.Kind != "generate" && record.Kind != "decimate") || len(record.Params) == 0 || json.Unmarshal(record.Params, &op.Params) != nil {
				return v, v.Created
			}
			op.ID, op.Kind = a.ID, record.Kind
		case "tool_submitted":
			if op.ID == "" || record.TaskID != a.TaskID {
				return v, v.Created
			}
			op.TaskID, submitted = record.TaskID, true
		case "technical_report":
			if !submitted || record.TaskID != a.TaskID || record.ArtifactID != a.ID {
				return v, v.Created
			}
			op.ArtifactID, op.Stage, completed, at = a.ID, "done", true, e.Time
		}
	}
	if completed {
		v.Current = &op
		return v, at
	}
	return v, v.Created
}

// backfillConversationRun 仅在迁移中读取原事件，使用来源事件序号构造可证实的公开历史。
func backfillConversationRun(ctx context.Context, tx *sql.Tx, v Session) error {
	c, err := loadConversation(ctx, tx, v.ID)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT seq,kind,at,data FROM events WHERE session_id=? ORDER BY seq", v.ID)
	if err != nil {
		return err
	}
	events := []Event{}
	for rows.Next() {
		var e Event
		var at string
		if err = rows.Scan(&e.Seq, &e.Kind, &at, &e.Data); err != nil {
			rows.Close()
			return err
		}
		e.Time, _ = time.Parse(time.RFC3339Nano, at)
		events = append(events, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, a := range v.Artifacts {
		evidence, at := legacyArtifactEvidence(v, a, events)
		if err = registerConversationVersion(ctx, tx, c, evidence, a, true, at); err != nil {
			return err
		}
	}
	var last Event
	for _, e := range events {
		if err = projectConversationEvent(ctx, tx, v, c, e, true); err != nil {
			return err
		}
		last = e
	}
	if last.Seq == 0 {
		last.Time = v.Created
	}
	if v.Terminal() {
		if err = projectConversationResult(ctx, tx, c, v, last); err != nil {
			return err
		}
	}
	return cacheConversationRun(ctx, tx, v, last.Seq)
}

// ConversationSnapshot 不逐个重读历史 Session；执行公开视图在原业务事务中增量更新。
// before 为消息历史分页游标，after 为全局事件游标；二者的身份互不替代。
func (s *Store) ConversationSnapshot(ctx context.Context, id, owner string, after, before int64, limit int) (ConversationSnapshot, error) {
	out := ConversationSnapshot{Messages: []ConversationMessage{}, Versions: []AssetVersion{}, Runs: []map[string]any{}, Events: []ConversationEvent{}}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	c, err := loadConversation(ctx, tx, id)
	if err != nil || c.Owner != owner || !c.Available(time.Now()) {
		return out, ErrConversationExpired
	}
	out.Conversation = c
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	query := "SELECT data FROM conversation_messages WHERE conversation_id=?"
	args := []any{id}
	if before > 0 {
		query += " AND seq<?"
		args = append(args, before)
	}
	query += " ORDER BY seq DESC,id DESC LIMIT ?"
	args = append(args, limit+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var b []byte
		var m ConversationMessage
		if err = rows.Scan(&b); err == nil {
			err = json.Unmarshal(b, &m)
		}
		if err != nil {
			rows.Close()
			return out, err
		}
		out.Messages = append(out.Messages, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(out.Messages) > limit {
		out.HasMore = true
		out.Messages = out.Messages[:limit]
	}
	sort.Slice(out.Messages, func(i, j int) bool { return out.Messages[i].Seq < out.Messages[j].Seq })
	rows, err = tx.QueryContext(ctx, "SELECT data,path,source_intent FROM asset_versions WHERE conversation_id=? ORDER BY number", id)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var b, intent []byte
		var v AssetVersion
		if err = rows.Scan(&b, &v.Path, &intent); err == nil {
			err = json.Unmarshal(b, &v)
		}
		if err != nil {
			rows.Close()
			return out, err
		}
		v.Processable = s.versionUsable(v)
		out.Versions = append(out.Versions, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT p.data FROM conversation_run_views p JOIN conversation_runs r ON r.run_id=p.run_id WHERE r.conversation_id=? ORDER BY r.created,r.run_id`, id)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var b []byte
		var view map[string]any
		if err = rows.Scan(&b); err == nil {
			err = json.Unmarshal(b, &view)
		}
		if err != nil {
			rows.Close()
			return out, err
		}
		out.Runs = append(out.Runs, view)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(e.seq),0) FROM events e JOIN conversation_runs r ON r.run_id=e.session_id WHERE r.conversation_id=?`, id).Scan(&out.Cursor); err != nil {
		return out, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT e.seq,e.kind,e.at,e.data,e.session_id FROM events e JOIN conversation_runs r ON r.run_id=e.session_id WHERE r.conversation_id=? AND e.seq>? AND e.seq<=? ORDER BY e.seq`, id, after, out.Cursor)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var e ConversationEvent
		var at string
		e.ConversationID = id
		if err = rows.Scan(&e.Seq, &e.Kind, &at, &e.Data, &e.RunID); err != nil {
			rows.Close()
			return out, err
		}
		e.Time, _ = time.Parse(time.RFC3339Nano, at)
		out.Events = append(out.Events, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	err = tx.Commit()
	return out, err
}
