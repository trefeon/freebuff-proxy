package store

import (
	"fmt"
	"strings"
)

// AppendLogs batch-inserts log records in one transaction (the logring spill
// path). Empty input is a no-op.
func (s *Store) AppendLogs(entries []LogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: logs begin: %w", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO log_entries(ts, level, msg, fields, req_id) VALUES(?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: logs prepare: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, e := range entries {
		if _, err := stmt.Exec(e.TS, e.Level, e.Msg, e.Fields, e.ReqID); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: logs insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: logs commit: %w", err)
	}
	return nil
}

// RecordQuota stores one per-model quota sample.
func (s *Store) RecordQuota(q QuotaSnapshot) error {
	_, err := s.db.Exec(
		`INSERT INTO quota_snapshots(ts, token_idx, model, quota_limit, recent_count, reset_at, entitlements)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		q.TS, q.TokenIdx, q.Model, q.Limit, q.Recent, q.ResetAt, q.Entitlements,
	)
	if err != nil {
		return fmt.Errorf("store: quota insert: %w", err)
	}
	return nil
}

// RecordMaturity stores one streak/standing event.
func (s *Store) RecordMaturity(e MaturityEvent) error {
	_, err := s.db.Exec(
		`INSERT INTO maturity_events(ts, token_idx, kind, detail) VALUES(?, ?, ?, ?)`,
		e.TS, e.TokenIdx, e.Kind, e.Detail,
	)
	if err != nil {
		return fmt.Errorf("store: maturity insert: %w", err)
	}
	return nil
}

// RecordRequest upserts one /v1 inference outcome. OR REPLACE keeps the
// latest row when a retried request reports twice under one req_id.
func (s *Store) RecordRequest(r RequestRecord) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO request_records(req_id, ts, endpoint, model, token_idx, status, ttfb_ms, error)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ReqID, r.TS, r.Endpoint, r.Model, r.TokenIdx, r.Status, r.TTFBms, r.Err,
	)
	if err != nil {
		return fmt.Errorf("store: request upsert: %w", err)
	}
	return nil
}

const (
	defaultLogLimit = 500
	maxLogLimit     = 5000
)

// QueryLogs returns newest-first rows matching the filter.
func (s *Store) QueryLogs(f LogFilter) ([]LogEntry, error) {
	var where []string
	var args []any
	if f.Since > 0 {
		where = append(where, "ts >= ?")
		args = append(args, f.Since)
	}
	if f.Until > 0 {
		where = append(where, "ts <= ?")
		args = append(args, f.Until)
	}
	if f.Level != "" {
		where = append(where, "level = ?")
		args = append(args, strings.ToUpper(f.Level))
	}
	if f.Contains != "" {
		where = append(where, "(msg LIKE ? ESCAPE '\\' OR fields LIKE ? ESCAPE '\\')")
		like := "%" + escapeLike(f.Contains) + "%"
		args = append(args, like, like)
	}
	if f.ReqID != "" {
		where = append(where, "req_id = ?")
		args = append(args, f.ReqID)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultLogLimit
	}
	if limit > maxLogLimit {
		limit = maxLogLimit
	}
	q := "SELECT id, ts, level, msg, fields, req_id FROM log_entries"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY ts DESC, id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query logs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []LogEntry{}
	for rows.Next() {
		var e LogEntry
		if err := rows.Scan(&e.ID, &e.TS, &e.Level, &e.Msg, &e.Fields, &e.ReqID); err != nil {
			return nil, fmt.Errorf("store: scan log: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// QuotaHistory returns oldest-first samples for one token+model since the
// given Unix-millis cutoff (0 = all), capped at limit (<=0 defaults to 2000).
func (s *Store) QuotaHistory(tokenIdx int, model string, since int64, limit int) ([]QuotaSnapshot, error) {
	if limit <= 0 {
		limit = 2000
	}
	rows, err := s.db.Query(
		`SELECT id, ts, token_idx, model, quota_limit, recent_count, reset_at, entitlements
		 FROM quota_snapshots WHERE token_idx = ? AND model = ? AND ts >= ?
		 ORDER BY ts ASC, id ASC LIMIT ?`,
		tokenIdx, model, since, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: quota history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []QuotaSnapshot{}
	for rows.Next() {
		var q QuotaSnapshot
		if err := rows.Scan(&q.ID, &q.TS, &q.TokenIdx, &q.Model, &q.Limit, &q.Recent, &q.ResetAt, &q.Entitlements); err != nil {
			return nil, fmt.Errorf("store: scan quota: %w", err)
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// MaturityHistory returns oldest-first events for one token since the cutoff.
func (s *Store) MaturityHistory(tokenIdx int, since int64, limit int) ([]MaturityEvent, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(
		`SELECT id, ts, token_idx, kind, detail FROM maturity_events
		 WHERE token_idx = ? AND ts >= ? ORDER BY ts ASC, id ASC LIMIT ?`,
		tokenIdx, since, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: maturity history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []MaturityEvent{}
	for rows.Next() {
		var e MaturityEvent
		if err := rows.Scan(&e.ID, &e.TS, &e.TokenIdx, &e.Kind, &e.Detail); err != nil {
			return nil, fmt.Errorf("store: scan maturity: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// escapeLike shields LIKE metacharacters in user filter text.
func escapeLike(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_")
	return r.Replace(s)
}
