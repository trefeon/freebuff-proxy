package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PutPageState stores one dashboard page's UI snapshot (scroll, filters,
// selected cards) as raw JSON so a restart restores the console view.
// Display data only: a missing row degrades to the page default.
func (s *Store) PutPageState(pageID, data string) error {
	if pageID == "" {
		return errors.New("store: page id cannot be empty")
	}
	if _, err := s.db.Exec(
		`INSERT INTO pages_state(page_id, data, updated_at) VALUES(?, ?, ?)
		 ON CONFLICT(page_id) DO UPDATE SET data=excluded.data, updated_at=excluded.updated_at`,
		pageID, data, Millis(time.Now())); err != nil {
		return fmt.Errorf("store: put page %q: %w", pageID, err)
	}
	return nil
}

// GetPageState returns the raw JSON snapshot for pageID. ok is false when
// the page was never persisted.
func (s *Store) GetPageState(pageID string) (data string, ok bool, err error) {
	if pageID == "" {
		return "", false, errors.New("store: page id cannot be empty")
	}
	if err := s.db.QueryRow(`SELECT data FROM pages_state WHERE page_id = ?`, pageID).Scan(&data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("store: get page %q: %w", pageID, err)
	}
	return data, true, nil
}
