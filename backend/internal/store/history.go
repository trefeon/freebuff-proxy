package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// LatestQuotaSnapshots returns the latest row per (token_idx, model),
// newest-first, capped at limit (<=0 defaults to 2000). The quota boot seed
// (ADR-0024) loads the live-view baseline from these without replaying full
// history. Latest = highest rowid per group (inserts are append-only, so the
// last write per group is its freshest sample).
func (s *Store) LatestQuotaSnapshots(limit int) ([]QuotaSnapshot, error) {
	if limit <= 0 {
		limit = 2000
	}
	rows, err := s.db.Query(
		`SELECT id, ts, token_idx, model, quota_limit, recent_count, reset_at, entitlements
		 FROM quota_snapshots
		 WHERE id IN (SELECT MAX(id) FROM quota_snapshots GROUP BY token_idx, model)
		 ORDER BY ts DESC, id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: latest quota: %w", err)
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

// historyCarryTables are the display-history tables copied from a legacy
// history file on first boot with the unified DB. All use INTEGER rowid
// PKs with no UNIQUE constraint, so the copy only runs into empty
// targets (never merges), keeping the import idempotent.
var historyCarryTables = []string{
	"log_entries",
	"quota_snapshots",
	"maturity_events",
	"request_records",
}

// ImportLegacyHistoryDB carries display history forward when the dashboard
// DB path moves: if oldPath exists, differs from newPath, and every
// history table in the open store is empty, it copies all rows from the
// legacy file (ATTACH + INSERT SELECT) and returns the row count. Any
// other state is a no-op (0, nil): missing file, same file, non-empty
// target, or a legacy file without history tables. A corrupt legacy file
// returns an error and copies nothing (callers only warn). The legacy
// file is left in place — history regrows, the old file never deletes.
//
// Attach strategy is two-tier: direct ATTACH first (a live read-write
// legacy file carries with its WAL intact), falling back to a staged temp
// copy of the main+-wal+-shm trio (read-only mounts and locked files fail
// direct ATTACH — SQLite needs write access for WAL/shm). The source is
// never modified by either path.
func ImportLegacyHistoryDB(s *Store, newPath, oldPath string) (int64, error) {
	if oldPath == "" {
		return 0, nil
	}
	newAbs, err := filepath.Abs(newPath)
	if err != nil {
		return 0, fmt.Errorf("store: resolve new db path: %w", err)
	}
	oldAbs, err := filepath.Abs(oldPath)
	if err != nil {
		return 0, fmt.Errorf("store: resolve legacy history path: %w", err)
	}
	if newAbs == oldAbs {
		return 0, nil
	}
	if _, err := os.Stat(oldAbs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("store: stat legacy history: %w", err)
	}
	for _, t := range historyCarryTables {
		var n int64
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + t).Scan(&n); err != nil {
			return 0, fmt.Errorf("store: count %s: %w", t, err)
		}
		if n > 0 {
			return 0, nil
		}
	}
	carry := func() (int64, error) {
		var total int64
		for _, t := range historyCarryTables {
			res, err := s.db.Exec("INSERT INTO main." + t + " SELECT * FROM legacy." + t)
			if err != nil {
				return total, fmt.Errorf("store: carry %s: %w", t, err)
			}
			n, _ := res.RowsAffected()
			total += n
		}
		return total, nil
	}
	attach := func(path string) error {
		_, err := s.db.Exec("ATTACH DATABASE '" + strings.ReplaceAll(path, "'", "''") + "' AS legacy")
		return err
	}
	if err := attach(oldAbs); err == nil {
		defer func() { _, _ = s.db.Exec("DETACH DATABASE legacy") }()
		return carry()
	}
	staged, cleanup, err := stageLegacyTrio(oldAbs)
	if err != nil {
		return 0, err
	}
	defer cleanup()
	if err := attach(staged); err != nil {
		return 0, fmt.Errorf("store: attach legacy history: %w", err)
	}
	defer func() { _, _ = s.db.Exec("DETACH DATABASE legacy") }()
	return carry()
}

// CountLegacyHistoryRows inspects a legacy history file WITHOUT importing
// it: the main+-wal+-shm trio stages into a temp dir (read-only mounts and
// locked files stage fine; the source is never modified), the staged copy
// ATTACHes, and every historyCarryTables entry gets a COUNT(*). Tables the
// file predates count as zero; a missing file is all-zeroes with nil error;
// an unreadable or corrupt file errors. Boot uses it to log multi-era
// skips: ImportLegacyHistoryDB fills empty targets from ONE file only
// (never merges — INTEGER rowid PKs would collide across files), so a later
// era left behind is reported with its counts, never silently covered.
func CountLegacyHistoryRows(s *Store, oldPath string) (map[string]int64, error) {
	counts := make(map[string]int64, len(historyCarryTables))
	for _, t := range historyCarryTables {
		counts[t] = 0
	}
	if oldPath == "" {
		return counts, nil
	}
	oldAbs, err := filepath.Abs(oldPath)
	if err != nil {
		return nil, fmt.Errorf("store: resolve legacy history path: %w", err)
	}
	if _, err := os.Stat(oldAbs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return counts, nil
		}
		return nil, fmt.Errorf("store: stat legacy history: %w", err)
	}
	staged, cleanup, err := stageLegacyTrio(oldAbs)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if _, err := s.db.Exec("ATTACH DATABASE '" + strings.ReplaceAll(staged, "'", "''") + "' AS legacy_count"); err != nil {
		return nil, fmt.Errorf("store: attach legacy history: %w", err)
	}
	defer func() { _, _ = s.db.Exec("DETACH DATABASE legacy_count") }()
	for _, t := range historyCarryTables {
		var exists int64
		if err := s.db.QueryRow("SELECT COUNT(*) FROM legacy_count.sqlite_master WHERE type='table' AND name=?", t).Scan(&exists); err != nil {
			return nil, fmt.Errorf("store: inspect legacy history: %w", err)
		}
		if exists == 0 {
			continue
		}
		var n int64
		if err := s.db.QueryRow("SELECT COUNT(*) FROM legacy_count." + t).Scan(&n); err != nil {
			return nil, fmt.Errorf("store: count legacy %s: %w", t, err)
		}
		counts[t] = n
	}
	return counts, nil
}

// stageLegacyTrio copies the main DB plus its -wal/-shm sidecars (when
// present) into a temp dir and returns the staged main path. Copying the
// trio keeps rows that were never checkpointed out of the main file.
func stageLegacyTrio(oldAbs string) (staged string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "freebuff-legacy-*")
	if err != nil {
		return "", nil, fmt.Errorf("store: stage legacy history: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	for _, suffix := range []string{"", "-wal", "-shm"} {
		src, err := os.Open(oldAbs + suffix)
		if err != nil {
			if suffix != "" && errors.Is(err, os.ErrNotExist) {
				continue
			}
			cleanup()
			return "", nil, fmt.Errorf("store: read legacy history: %w", err)
		}
		dst, err := os.OpenFile(filepath.Join(dir, "legacy.db"+suffix), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			_ = src.Close()
			cleanup()
			return "", nil, fmt.Errorf("store: stage legacy history: %w", err)
		}
		_, cpyErr := dst.ReadFrom(src)
		_ = src.Close()
		closeErr := dst.Close()
		if cpyErr != nil || closeErr != nil {
			cleanup()
			return "", nil, fmt.Errorf("store: stage legacy history: %w", err)
		}
	}
	return filepath.Join(dir, "legacy.db"), cleanup, nil
}
