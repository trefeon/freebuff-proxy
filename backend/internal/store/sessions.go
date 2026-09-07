package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// Session persistence in the DB mirrors session.Store semantics without
// importing it (leaf package: the blobs stay opaque JSON):
//   - SaveSession(nil-equivalent) removes: two empty blobs delete the row.
//   - SaveSessionRuns updates only the runs column (mirrors SaveRun),
//     creating the row with an empty session blob when absent.
//   - Keys are token SHA-256 hashes computed by the caller; raw tokens never
//     reach this package.
func (s *Store) SaveSession(tokenHash, sessionData, runsData string) error {
	if tokenHash == "" {
		return errors.New("store: session token hash cannot be empty")
	}
	if sessionData == "" && runsData == "" {
		return s.DeleteSession(tokenHash)
	}
	if _, err := s.db.Exec(
		`INSERT INTO sessions_persist(token_hash, session_data, runs_data, updated_at) VALUES(?, ?, ?, ?)
		 ON CONFLICT(token_hash) DO UPDATE SET session_data=excluded.session_data, runs_data=excluded.runs_data, updated_at=excluded.updated_at`,
		tokenHash, sessionData, runsData, Millis(time.Now())); err != nil {
		return fmt.Errorf("store: save session: %w", err)
	}
	return nil
}

// LoadSession returns the raw session + runs blobs for tokenHash.
// found is false when no row exists.
func (s *Store) LoadSession(tokenHash string) (sessionData, runsData string, found bool, err error) {
	if tokenHash == "" {
		return "", "", false, errors.New("store: session token hash cannot be empty")
	}
	if err := s.db.QueryRow(
		`SELECT session_data, runs_data FROM sessions_persist WHERE token_hash = ?`,
		tokenHash).Scan(&sessionData, &runsData); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("store: load session: %w", err)
	}
	return sessionData, runsData, true, nil
}

// SaveSessionRuns replaces only the runs blob, preserving the session blob
// (mirrors session.Store.SaveRun, including create-on-absent).
func (s *Store) SaveSessionRuns(tokenHash, runsData string) error {
	if tokenHash == "" {
		return errors.New("store: session token hash cannot be empty")
	}
	if _, err := s.db.Exec(
		`INSERT INTO sessions_persist(token_hash, session_data, runs_data, updated_at) VALUES(?, '', ?, ?)
		 ON CONFLICT(token_hash) DO UPDATE SET runs_data=excluded.runs_data, updated_at=excluded.updated_at`,
		tokenHash, runsData, Millis(time.Now())); err != nil {
		return fmt.Errorf("store: save session runs: %w", err)
	}
	return nil
}

// DeleteSession drops the row for tokenHash (mirrors Remove).
func (s *Store) DeleteSession(tokenHash string) error {
	if tokenHash == "" {
		return errors.New("store: session token hash cannot be empty")
	}
	if _, err := s.db.Exec(`DELETE FROM sessions_persist WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("store: delete session: %w", err)
	}
	return nil
}

// legacySessionFile is the on-disk shape of .freebuff-session-state.json
// (see session.storeFile). Entries stay raw: the store never interprets
// them, so a newer session schema still imports byte-identically.
type legacySessionFile struct {
	Sessions map[string]json.RawMessage            `json:"sessions"`
	Runs     map[string]map[string]json.RawMessage `json:"runs"`
}

// ImportLegacySessionFile imports a legacy .freebuff-session-state.json into
// sessions_persist, then archives it to path+".bak" (rename, never delete).
// A missing file is a no-op (0, nil); a parse failure returns an error and
// leaves the file in place. Callers gate on SESSION_PERSIST and only warn on
// error — the JSON path keeps working when the DB is unavailable.
func ImportLegacySessionFile(s *Store, path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("store: read legacy session file: %w", err)
	}
	var file legacySessionFile
	if err := json.Unmarshal(data, &file); err != nil {
		return 0, fmt.Errorf("store: parse legacy session file: %w", err)
	}
	imported := 0
	for hash, sess := range file.Sessions {
		if hash == "" || len(sess) == 0 {
			continue
		}
		var runs json.RawMessage
		if agents, ok := file.Runs[hash]; ok && len(agents) > 0 {
			if r, err := json.Marshal(agents); err == nil {
				runs = r
			}
		}
		if err := s.SaveSession(hash, string(sess), string(runs)); err != nil {
			return imported, fmt.Errorf("store: import session: %w", err)
		}
		imported++
	}
	// Runs for hashes with no session entry still carry resume value
	// (a run without cached session state): persist them session-less.
	for hash, agents := range file.Runs {
		if hash == "" || len(agents) == 0 {
			continue
		}
		if _, _, found, err := s.LoadSession(hash); err != nil {
			return imported, err
		} else if found {
			continue
		}
		r, err := json.Marshal(agents)
		if err != nil {
			return imported, fmt.Errorf("store: import runs: %w", err)
		}
		if err := s.SaveSessionRuns(hash, string(r)); err != nil {
			return imported, fmt.Errorf("store: import runs: %w", err)
		}
	}
	bak := path + ".bak"
	// Windows rename fails over an existing target: clear a stale .bak
	// first (the archive is replaced, the legacy source never deleted).
	_ = os.Remove(bak)
	if err := os.Rename(path, bak); err != nil {
		return imported, fmt.Errorf("store: archive legacy session file: %w", err)
	}
	return imported, nil
}
