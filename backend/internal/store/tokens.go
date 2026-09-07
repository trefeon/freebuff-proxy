package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// TokenMeta is the durable side of one gateway token: the SHA-256 hash of
// the value (raw tokens never touch disk), an operator label, a status, and
// an opaque quota JSON blob the dashboard renders. CreatedAt is Unix millis
// UTC, stamped once on first upsert and preserved afterwards.
type TokenMeta struct {
	ValueHash string
	Label     string
	Status    string
	QuotaData string
	CreatedAt int64
}

// UpsertTokenMeta inserts or refreshes one token row. CreatedAt survives
// refreshes; label/status/quota are replaced wholesale.
func (s *Store) UpsertTokenMeta(valueHash, label, status, quotaData string) error {
	if valueHash == "" {
		return errors.New("store: token hash cannot be empty")
	}
	if _, err := s.db.Exec(
		`INSERT INTO tokens(value_hash, label, status, quota_data, created_at) VALUES(?, ?, ?, ?, ?)
		 ON CONFLICT(value_hash) DO UPDATE SET label=excluded.label, status=excluded.status, quota_data=excluded.quota_data`,
		valueHash, label, status, quotaData, Millis(time.Now())); err != nil {
		return fmt.Errorf("store: upsert token: %w", err)
	}
	return nil
}

// GetTokenMeta returns one token row. ok is false when absent.
func (s *Store) GetTokenMeta(valueHash string) (meta TokenMeta, ok bool, err error) {
	if valueHash == "" {
		return TokenMeta{}, false, errors.New("store: token hash cannot be empty")
	}
	meta.ValueHash = valueHash
	if err := s.db.QueryRow(
		`SELECT label, status, quota_data, created_at FROM tokens WHERE value_hash = ?`,
		valueHash).Scan(&meta.Label, &meta.Status, &meta.QuotaData, &meta.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TokenMeta{}, false, nil
		}
		return TokenMeta{}, false, fmt.Errorf("store: get token: %w", err)
	}
	return meta, true, nil
}

// ListTokenMetas returns every token row oldest-first.
func (s *Store) ListTokenMetas() ([]TokenMeta, error) {
	rows, err := s.db.Query(
		`SELECT value_hash, label, status, quota_data, created_at FROM tokens ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TokenMeta
	for rows.Next() {
		var m TokenMeta
		if err := rows.Scan(&m.ValueHash, &m.Label, &m.Status, &m.QuotaData, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan token: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list tokens: %w", err)
	}
	return out, nil
}

// DeleteTokenMeta drops one token row (token removed from the pool).
func (s *Store) DeleteTokenMeta(valueHash string) error {
	if valueHash == "" {
		return errors.New("store: token hash cannot be empty")
	}
	if _, err := s.db.Exec(`DELETE FROM tokens WHERE value_hash = ?`, valueHash); err != nil {
		return fmt.Errorf("store: delete token: %w", err)
	}
	return nil
}
