package app

import (
	"context"
	"encoding/json"
)

// schedulableRuns 仅加载活动指针指向的执行，持续会话的历史History不进入周期扫描。
// 停止后尚未释放的Run仍会被返回，供调度器重试空闲状态的持久化。
func (s *Store) schedulableRuns(ctx context.Context) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.data FROM sessions s JOIN conversations c ON c.active_run_id=s.id WHERE c.cleaning=0`)
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
