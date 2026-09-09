package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Pool runtime persistence (schema v4, pool_state table).
//
// The pool keeps its hot-path state in memory and persists it as opaque
// blobs keyed by stable string keys, write-through from a background flush
// the pool package owns. This file is the SOLE schema owner for pool_state;
// no other pool-runtime table may be added elsewhere (pool slices propose
// new keys via hub instead). Keys are namespaced pool/<area>/<id>, e.g.
// pool/ledger/<sha256hex>, pool/admissions, pool/burst,
// pool/bridge/usage, pool/bridge/survivors. Token keys are SHA-256 hex —
// raw tokens never reach the store. Values are opaque to the store: the
// pool marshals/unmarshals its own shapes (leaf package: stdlib + the
// sqlite driver only, zero internal imports).
//
// Every method degrades to live-only on DB error: callers (the pool flush)
// warn and keep serving from memory, so persistence never blocks the
// request hot path.

// SavePoolState upserts one opaque pool-runtime blob under key.
func (s *Store) SavePoolState(key string, value []byte) error {
	if key == "" {
		return errors.New("store: pool state key cannot be empty")
	}
	if _, err := s.db.Exec(
		`INSERT INTO pool_state(key, value, updated_at) VALUES(?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value, Millis(time.Now())); err != nil {
		return fmt.Errorf("store: save pool state %q: %w", key, err)
	}
	return nil
}

// LoadPoolState returns the opaque blob for key. ok is false when absent
// (sql.ErrNoRows maps to ok=false, never an error).
func (s *Store) LoadPoolState(key string) (value []byte, ok bool, err error) {
	if key == "" {
		return nil, false, errors.New("store: pool state key cannot be empty")
	}
	var raw []byte
	if err := s.db.QueryRow(`SELECT value FROM pool_state WHERE key = ?`, key).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("store: load pool state %q: %w", key, err)
	}
	return raw, true, nil
}

// DeletePoolState drops one pool-state row. A missing row is a no-op (nil
// error), mirroring DeleteSetting.
func (s *Store) DeletePoolState(key string) error {
	if key == "" {
		return errors.New("store: pool state key cannot be empty")
	}
	if _, err := s.db.Exec(`DELETE FROM pool_state WHERE key = ?`, key); err != nil {
		return fmt.Errorf("store: delete pool state %q: %w", key, err)
	}
	return nil
}

// ListPoolState returns every pool-state row under prefix (key -> raw
// value), key-ordered. Callers use it for boot restore sweeps (e.g. ledger
// orphan pruning). Prefix LIKE metacharacters (\, %, _) are escaped so the
// match is a literal prefix.
func (s *Store) ListPoolState(prefix string) (map[string][]byte, error) {
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	rows, err := s.db.Query(`SELECT key, value FROM pool_state WHERE key LIKE ? ESCAPE '\' ORDER BY key`, esc+`%`)
	if err != nil {
		return nil, fmt.Errorf("store: list pool state: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string][]byte)
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("store: scan pool state: %w", err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list pool state: %w", err)
	}
	return out, nil
}
