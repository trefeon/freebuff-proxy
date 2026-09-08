package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SaveTokenMaturity persists per-token maturity automation state (Account
// Maturity rev 2) in the tokens table keyed by the SHA-256 hash of the
// value (raw tokens never touch disk). maturity_json is the opaque automation blob
// the pool marshals (config, slot, counters, warning); streak_blob is the
// opaque upstream streak JSON for the dashboard. CreatedAt is Unix millis
// UTC, stamped once on first save and preserved afterwards.
func (s *Store) SaveTokenMaturity(valueHash, maturityJSON string, streakBlob []byte) error {
	if valueHash == "" {
		return errors.New("store: token hash cannot be empty")
	}
	if _, err := s.db.Exec(
		`INSERT INTO tokens(value_hash, maturity_json, streak_blob, created_at) VALUES(?, ?, ?, ?)
		 ON CONFLICT(value_hash) DO UPDATE SET maturity_json=excluded.maturity_json, streak_blob=excluded.streak_blob`,
		valueHash, maturityJSON, streakBlob, Millis(time.Now())); err != nil {
		return fmt.Errorf("store: save token maturity: %w", err)
	}
	return nil
}

// LoadTokenMaturity returns one token's maturity blobs. ok is false when the
// token has no row yet (never enabled); a migrated v2 row loads as empty
// state with a nil blob.
func (s *Store) LoadTokenMaturity(valueHash string) (maturityJSON string, streakBlob []byte, ok bool, err error) {
	if valueHash == "" {
		return "", nil, false, errors.New("store: token hash cannot be empty")
	}
	var blob sql.Null[[]byte]
	if err := s.db.QueryRow(
		`SELECT maturity_json, streak_blob FROM tokens WHERE value_hash = ?`,
		valueHash).Scan(&maturityJSON, &blob); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, false, nil
		}
		return "", nil, false, fmt.Errorf("store: load token maturity: %w", err)
	}
	return maturityJSON, blob.V, true, nil
}
