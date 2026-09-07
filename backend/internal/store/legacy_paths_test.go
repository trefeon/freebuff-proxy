package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyCandidates(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, ".freebuff-session-state.json")
	if err := os.WriteFile(stateFile, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	legacyHist := filepath.Join(dir, "freebuff-history.db")
	if err := os.WriteFile(legacyHist, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	newPath := filepath.Join(dir, "data", "freebuff.db")

	got := LegacyHistoryCandidates(newPath, stateFile)
	if len(got) != 1 || got[0] != legacyHist {
		t.Fatalf("history candidates = %v, want [%s]", got, legacyHist)
	}

	// The unified DB from an old-compose run is a candidate too.
	unified := filepath.Join(dir, "data", "freebuff.db")
	if err := os.MkdirAll(filepath.Dir(unified), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unified, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	got = LegacyHistoryCandidates(filepath.Join(dir, "other.db"), stateFile)
	if len(got) != 2 || got[0] != legacyHist || got[1] != unified {
		t.Fatalf("history candidates = %v, want [legacy unified]", got)
	}
	// The target itself is never a candidate.
	got = LegacyHistoryCandidates(unified, stateFile)
	for _, c := range got {
		if c == unified {
			t.Fatalf("target listed as its own candidate: %v", got)
		}
	}

	sess := LegacySessionCandidates(stateFile)
	if len(sess) != 1 || sess[0] != stateFile {
		t.Fatalf("session candidates = %v, want [%s]", sess, stateFile)
	}
	if got := LegacySessionCandidates(filepath.Join(dir, "missing.json")); len(got) != 0 {
		t.Fatalf("session candidates for missing file = %v, want empty", got)
	}
}
