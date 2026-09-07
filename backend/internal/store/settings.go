package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SetSetting stores one dashboard control-state entry (key -> raw JSON
// value), the durable counterpart of the live config the settings console
// edits. Values are opaque to the store: callers marshal/unmarshal shapes.
func (s *Store) SetSetting(key, value string) error {
	if key == "" {
		return errors.New("store: setting key cannot be empty")
	}
	if _, err := s.db.Exec(
		`INSERT INTO settings(key, value, updated_at) VALUES(?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value, Millis(time.Now())); err != nil {
		return fmt.Errorf("store: set setting %q: %w", key, err)
	}
	return nil
}

// GetSetting returns the raw JSON value for key. ok is false when absent.
func (s *Store) GetSetting(key string) (value string, ok bool, err error) {
	if key == "" {
		return "", false, errors.New("store: setting key cannot be empty")
	}
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("store: get setting %q: %w", key, err)
	}
	return value, true, nil
}

// DeleteSetting drops one setting row. A missing row is a no-op (nil
// error), mirroring DeleteSession; callers needing 404 semantics check
// GetSetting first.
func (s *Store) DeleteSetting(key string) error {
	if key == "" {
		return errors.New("store: setting key cannot be empty")
	}
	if _, err := s.db.Exec(`DELETE FROM settings WHERE key = ?`, key); err != nil {
		return fmt.Errorf("store: delete setting %q: %w", key, err)
	}
	return nil
}

// ListSettings returns every setting row (key -> raw value), key-ordered.
// Callers filter their own namespace (e.g. the config overlay prefix).
func (s *Store) ListSettings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("store: list settings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("store: scan setting: %w", err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list settings: %w", err)
	}
	return out, nil
}
