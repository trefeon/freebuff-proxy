package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenEnforces0600 pins the secret-holding posture: since the env-to-DB
// migration the settings table carries AUTH_TOKENS, ADMIN_TOKEN, API_KEYS,
// and WEBHOOK_URL rows, so Open creates the file at 0600 and tightens a
// pre-migration 0644 file on open.
func TestOpenEnforces0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "perms.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("fresh DB mode = %o, want 600", got)
	}
}

func TestOpenTightensExisting0644(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-perms.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Chmod 644: %v", err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("reopened DB mode = %o, want 600", got)
	}
}
