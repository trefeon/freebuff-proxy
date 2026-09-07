package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionPersistRoundTrip(t *testing.T) {
	s := openTest(t)
	if _, _, ok, err := s.LoadSession("tokhash"); err != nil {
		t.Fatalf("LoadSession missing: %v", err)
	} else if ok {
		t.Fatal("LoadSession on empty store returned ok=true")
	}
	if err := s.SaveSession("tokhash", `{"instance_id":"i1"}`, `{"agent-1":{"run_id":"r1"}}`); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	sess, runs, ok, err := s.LoadSession("tokhash")
	if err != nil || !ok || sess != `{"instance_id":"i1"}` || runs != `{"agent-1":{"run_id":"r1"}}` {
		t.Fatalf("LoadSession = %q,%q,%v,%v, want blobs back", sess, runs, ok, err)
	}
	// Runs-only update must preserve the session blob (mirrors SaveRun).
	if err := s.SaveSessionRuns("tokhash", `{"agent-2":{"run_id":"r2"}}`); err != nil {
		t.Fatalf("SaveSessionRuns: %v", err)
	}
	if sess, runs, _, _ := s.LoadSession("tokhash"); sess != `{"instance_id":"i1"}` || runs != `{"agent-2":{"run_id":"r2"}}` {
		t.Fatalf("runs update clobbered session: %q,%q", sess, runs)
	}
	// Runs-only update on an unknown hash creates the row with empty session.
	if err := s.SaveSessionRuns("fresh", `{"a":{"run_id":"x"}}`); err != nil {
		t.Fatalf("SaveSessionRuns fresh: %v", err)
	}
	if sess, runs, ok, _ := s.LoadSession("fresh"); !ok || sess != "" || runs != `{"a":{"run_id":"x"}}` {
		t.Fatalf("fresh runs row = %q,%q,%v, want empty session + runs", sess, runs, ok)
	}
	// Saving two empty blobs removes the row (mirrors Save(nil)).
	if err := s.SaveSession("tokhash", "", ""); err != nil {
		t.Fatalf("SaveSession empty: %v", err)
	}
	if _, _, ok, _ := s.LoadSession("tokhash"); ok {
		t.Fatal("empty SaveSession did not remove the row")
	}
	if err := s.DeleteSession("fresh"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, _, ok, _ := s.LoadSession("fresh"); ok {
		t.Fatal("deleted session still returned")
	}
}

func TestSessionPersistRejectsEmptyHash(t *testing.T) {
	s := openTest(t)
	if err := s.SaveSession("", "a", "b"); err == nil {
		t.Fatal("SaveSession(\"\") accepted, want an error")
	}
	if _, _, _, err := s.LoadSession(""); err == nil {
		t.Fatal("LoadSession(\"\") accepted, want an error")
	}
	if err := s.SaveSessionRuns("", "b"); err == nil {
		t.Fatal("SaveSessionRuns(\"\") accepted, want an error")
	}
}

func TestImportLegacySessionFile(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, ".freebuff-session-state.json")
	fixture := `{"version":1,"sessions":{"abc123":{"instance_id":"inst-1","model":"m","status":"active"}},"runs":{"abc123":{"agent-1":{"run_id":"run-9","agent_id":"agent-1"}}}}`
	if err := os.WriteFile(legacy, []byte(fixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	s := openTest(t)
	n, err := ImportLegacySessionFile(s, legacy)
	if err != nil {
		t.Fatalf("ImportLegacySessionFile: %v", err)
	}
	if n != 1 {
		t.Fatalf("imported = %d, want 1", n)
	}
	sess, runs, ok, err := s.LoadSession("abc123")
	if err != nil || !ok {
		t.Fatalf("LoadSession after import = %v,%v", ok, err)
	}
	if !strings.Contains(sess, "inst-1") {
		t.Fatalf("session blob = %q, want inst-1 inside", sess)
	}
	if !strings.Contains(runs, "run-9") {
		t.Fatalf("runs blob = %q, want run-9 inside", runs)
	}
	// Archive, never delete: the original path is gone, the .bak holds it.
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("legacy file still in place, want it renamed away")
	}
	bak, err := os.ReadFile(legacy + ".bak")
	if err != nil {
		t.Fatalf("read .bak: %v", err)
	}
	if string(bak) != fixture {
		t.Fatalf(".bak = %q, want the original bytes", bak)
	}
}

func TestImportLegacySessionFileMissingIsNoop(t *testing.T) {
	s := openTest(t)
	missing := filepath.Join(t.TempDir(), "no-such-state.json")
	n, err := ImportLegacySessionFile(s, missing)
	if err != nil || n != 0 {
		t.Fatalf("missing import = %d,%v, want 0,nil", n, err)
	}
	if _, err := os.Stat(missing + ".bak"); !os.IsNotExist(err) {
		t.Fatal("noop import created a .bak, want nothing")
	}
}

func TestImportLegacySessionFileBadJSONKeepsFile(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, ".freebuff-session-state.json")
	if err := os.WriteFile(legacy, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	s := openTest(t)
	if _, err := ImportLegacySessionFile(s, legacy); err == nil {
		t.Fatal("garbage import accepted, want an error")
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("garbage legacy file renamed away on failure: %v", err)
	}
}

func TestSessionsEmptyGate(t *testing.T) {
	s := openTest(t)
	if empty, err := s.SessionsEmpty(); err != nil || !empty {
		t.Fatalf("SessionsEmpty fresh = %v,%v, want true,nil", empty, err)
	}
	if err := s.SaveSession("h1", `{"a":1}`, ""); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	if empty, err := s.SessionsEmpty(); err != nil || empty {
		t.Fatalf("SessionsEmpty after save = %v,%v, want false,nil", empty, err)
	}
	if err := s.DeleteSession("h1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if empty, err := s.SessionsEmpty(); err != nil || !empty {
		t.Fatalf("SessionsEmpty after delete = %v,%v, want true,nil", empty, err)
	}
}

// TestImportLegacySessionCollisions pins the split-brain report: a later
// candidate overwriting an earlier-imported hash with different content (in
// either blob) fires onCollision once per hash and the incoming row wins;
// byte-identical re-imports stay silent.
func TestImportLegacySessionCollisions(t *testing.T) {
	dir := t.TempDir()
	writeLegacy := func(name, fixture string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(fixture), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}
	first := writeLegacy("first.json",
		`{"version":1,"sessions":{"hdiff":{"instance_id":"old"},"hsame":{"instance_id":"kept"}},"runs":{"hsame":{"agent-1":{"run_id":"r1"}}}}`)
	second := writeLegacy("second.json",
		`{"version":1,"sessions":{"hdiff":{"instance_id":"new"},"hsame":{"instance_id":"kept"}},"runs":{"hsame":{"agent-1":{"run_id":"r1"}}}}`)
	s := openTest(t)
	var got []string
	collect := func(h string) { got = append(got, h) }
	if _, err := ImportLegacySessionFileWithCollisions(s, first, collect); err != nil {
		t.Fatalf("first import: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("first import collisions = %v, want none (empty store)", got)
	}
	if _, err := ImportLegacySessionFileWithCollisions(s, second, collect); err != nil {
		t.Fatalf("second import: %v", err)
	}
	if len(got) != 1 || got[0] != "hdiff" {
		t.Fatalf("second import collisions = %v, want [hdiff]", got)
	}
	if sess, _, _, _ := s.LoadSession("hdiff"); !strings.Contains(sess, `"new"`) {
		t.Fatalf("hdiff after overwrite = %q, want the incoming row to win", sess)
	}
	// A third file changing only the runs blob still counts as different
	// content; a byte-identical file stays silent.
	third := writeLegacy("third.json",
		`{"version":1,"sessions":{"hdiff":{"instance_id":"new"},"hsame":{"instance_id":"kept"}},"runs":{"hsame":{"agent-1":{"run_id":"r2"}}}}`)
	fourth := writeLegacy("fourth.json",
		`{"version":1,"sessions":{"hdiff":{"instance_id":"new"},"hsame":{"instance_id":"kept"}},"runs":{"hsame":{"agent-1":{"run_id":"r2"}}}}`)
	got = nil
	if _, err := ImportLegacySessionFileWithCollisions(s, third, collect); err != nil {
		t.Fatalf("third import: %v", err)
	}
	if len(got) != 1 || got[0] != "hsame" {
		t.Fatalf("third import collisions = %v, want [hsame] (runs changed)", got)
	}
	got = nil
	if _, err := ImportLegacySessionFileWithCollisions(s, fourth, collect); err != nil {
		t.Fatalf("fourth import: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("identical re-import collisions = %v, want none", got)
	}
}

// TestImportLegacySessionBackupNoArchive pins the .bak re-consult shape: a
// path already ending in .bak imports WITHOUT renaming (the archive is the
// only copy — a second rename would orphan it as .bak.bak).
func TestImportLegacySessionBackupNoArchive(t *testing.T) {
	dir := t.TempDir()
	bak := filepath.Join(dir, ".freebuff-session-state.json.bak")
	fixture := `{"version":1,"sessions":{"abc123":{"instance_id":"inst-9"}},"runs":{}}`
	if err := os.WriteFile(bak, []byte(fixture), 0o600); err != nil {
		t.Fatalf("write .bak: %v", err)
	}
	s := openTest(t)
	n, err := ImportLegacySessionBackup(s, bak, nil)
	if err != nil {
		t.Fatalf("ImportLegacySessionBackup: %v", err)
	}
	if n != 1 {
		t.Fatalf("imported = %d, want 1", n)
	}
	if _, _, ok, err := s.LoadSession("abc123"); err != nil || !ok {
		t.Fatalf("LoadSession after backup import = %v,%v, want the row", ok, err)
	}
	if _, err := os.Stat(bak); err != nil {
		t.Fatalf(".bak archive moved away by a no-archive import: %v", err)
	}
	if _, err := os.Stat(bak + ".bak"); !os.IsNotExist(err) {
		t.Fatal("no-archive import created a .bak.bak orphan")
	}
}

// TestImportLegacySessionFileNeverOrphansBak pins the suffix guard on the
// archiving entry point: even archive=true must not rename a .bak path.
func TestImportLegacySessionFileNeverOrphansBak(t *testing.T) {
	dir := t.TempDir()
	bak := filepath.Join(dir, "state.json.bak")
	if err := os.WriteFile(bak, []byte(`{"version":1,"sessions":{"h1":{"x":1}},"runs":{}}`), 0o600); err != nil {
		t.Fatalf("write .bak: %v", err)
	}
	s := openTest(t)
	if _, err := ImportLegacySessionFile(s, bak); err != nil {
		t.Fatalf("ImportLegacySessionFile(.bak): %v", err)
	}
	if _, err := os.Stat(bak); err != nil {
		t.Fatalf(".bak path renamed away: %v", err)
	}
	if _, err := os.Stat(bak + ".bak"); !os.IsNotExist(err) {
		t.Fatal("archiving import created a .bak.bak orphan")
	}
}
