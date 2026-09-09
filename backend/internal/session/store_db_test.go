package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/upstream"
)

// fakeSessionBackend is an in-memory SessionBackend: it stands in for
// sessions_persist without importing the store package (archtest pins
// session to telemetry + upstream only).
type fakeSessionBackend struct {
	mu   sync.Mutex
	rows map[string][2]string // token hash → {session blob, runs blob}
	// saves counts backend writes (SaveSession + SaveSessionRuns +
	// DeleteSession): a Save that never reaches the backend fails the
	// sole-truth contract.
	saves int
}

func newFakeSessionBackend() *fakeSessionBackend {
	return &fakeSessionBackend{rows: make(map[string][2]string)}
}

func (f *fakeSessionBackend) SaveSession(tokenHash, sessionData, runsData string) error {
	if tokenHash == "" {
		return errors.New("fake backend: session token hash cannot be empty")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves++
	if sessionData == "" && runsData == "" {
		delete(f.rows, tokenHash)
		return nil
	}
	f.rows[tokenHash] = [2]string{sessionData, runsData}
	return nil
}

func (f *fakeSessionBackend) LoadSession(tokenHash string) (string, string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[tokenHash]
	if !ok {
		return "", "", false, nil
	}
	return r[0], r[1], true, nil
}

func (f *fakeSessionBackend) SaveSessionRuns(tokenHash, runsData string) error {
	if tokenHash == "" {
		return errors.New("fake backend: session token hash cannot be empty")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves++
	prev := f.rows[tokenHash]
	f.rows[tokenHash] = [2]string{prev[0], runsData}
	return nil
}

func (f *fakeSessionBackend) DeleteSession(tokenHash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves++
	delete(f.rows, tokenHash)
	return nil
}

func (f *fakeSessionBackend) lookup(t *testing.T, key string) (sess, runs string, found bool) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[key]
	return r[0], r[1], ok
}

// TestDBBackedWriteReopenLoadIdentical pins the sole-truth contract: a Save
// (+ SaveRun) reaches the backend, and a fresh store over the same backend
// (a restart) loads byte-identical state.
func TestDBBackedWriteReopenLoadIdentical(t *testing.T) {
	fb := newFakeSessionBackend()
	s1 := NewStoreWithBackend("", fb)

	resetAt := time.Now().Add(12 * time.Hour).Truncate(time.Second)
	slot := activeSlot("inst-reopen-1", "deepseek/deepseek-v4-flash")
	slot.quotaByModel = map[string]upstream.ModelQuota{
		"deepseek/deepseek-v4-flash": {
			Model:       "deepseek/deepseek-v4-flash",
			Limit:       5,
			RecentCount: 2,
			ResetAt:     resetAt,
			Period:      "pacific_day",
			Entitlement: map[string]float64{"base": 5},
		},
	}
	s1.Save("key", slot)
	runAt := time.Now().Add(-time.Minute).Truncate(time.Second)
	s1.SaveRun("key", "agent-x", PersistedRun{RunID: "run-1", AgentID: "agent-x", TraceSessionID: "t", StartedAt: runAt, Requests: 2})

	fb.mu.Lock()
	saves := fb.saves
	fb.mu.Unlock()
	if saves == 0 {
		t.Fatal("Save/SaveRun issued zero backend writes, want sessions_persist as the write target")
	}

	// Restart: a fresh store over the same backend must see everything.
	s2 := NewStoreWithBackend("", fb)
	got := s2.Load("key")
	if got == nil {
		t.Fatal("reopened Load = nil, want inst-reopen-1")
	}
	if got.instanceID != "inst-reopen-1" || got.model != "deepseek/deepseek-v4-flash" {
		t.Errorf("reopened Load = %+v, want inst-reopen-1/flash", got)
	}
	q, ok := got.quotaByModel["deepseek/deepseek-v4-flash"]
	if !ok {
		t.Fatal("reopened quotaByModel missing deepseek/deepseek-v4-flash")
	}
	if q.Limit != 5 || q.RecentCount != 2 || q.Period != "pacific_day" || q.Entitlement["base"] != 5 {
		t.Errorf("reopened quota = %+v, want 5/2/pacific_day/base=5", q)
	}
	if !q.ResetAt.Equal(resetAt) {
		t.Errorf("reopened resetAt = %v, want %v", q.ResetAt, resetAt)
	}
	pr := s2.LoadRun("key", "agent-x")
	if pr == nil || pr.RunID != "run-1" || pr.TraceSessionID != "t" || pr.Requests != 2 {
		t.Fatalf("reopened LoadRun = %+v, want run-1/t/2", pr)
	}
	if !pr.StartedAt.Equal(runAt) {
		t.Errorf("reopened run StartedAt = %v, want %v", pr.StartedAt, runAt)
	}
}

// TestLegacyFileImportsOnceThenArchives pins the import-only fallback: a
// legacy JSON file folds into the backend on first use (store-first on
// collision — the DB row wins with a WARN), the source archives to .bak,
// and later stores serve from the backend without the file.
func TestLegacyFileImportsOnceThenArchives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	grace := expiry.Add(graceWindow)

	fb := newFakeSessionBackend()
	seed := NewStoreWithBackend("", fb)
	seed.Save("db-wins", &cachedState{status: "active", instanceID: "inst-db", model: "m", expiresAt: expiry, gracePeriodEndsAt: grace})

	runAt := time.Now().Add(-time.Minute).Truncate(time.Second)
	file := storeFile{
		Version: storeVersion,
		Sessions: map[string]persistedState{
			"db-wins":  {Status: "active", InstanceID: "inst-file", Model: "m", ExpiresAt: expiry, GracePeriodEndsAt: grace},
			"file-new": {Status: "active", InstanceID: "inst-new", Model: "m", ExpiresAt: expiry, GracePeriodEndsAt: grace},
		},
		Runs: map[string]map[string]PersistedRun{
			"runs-only": {"agent-z": {RunID: "run-z", AgentID: "agent-z", TraceSessionID: "t", StartedAt: runAt}},
		},
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewStoreWithBackend(path, fb)
	if got := s.Load("file-new"); got == nil || got.instanceID != "inst-new" {
		t.Fatalf("Load(file-new) after import = %+v, want inst-new", got)
	}

	// The source archives exactly once: original gone, .bak present.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("legacy file still at %s after import, want archived to .bak: %v", path, err)
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Fatalf("legacy archive missing at %s.bak: %v", path, err)
	}

	// Collision: the backend row wins over the file entry.
	if got := s.Load("db-wins"); got == nil || got.instanceID != "inst-db" {
		t.Fatalf("Load(db-wins) = %+v, want inst-db (dashboard store wins)", got)
	}
	// Runs-only file entries import session-less.
	if pr := s.LoadRun("runs-only", "agent-z"); pr == nil || pr.RunID != "run-z" {
		t.Fatalf("LoadRun(runs-only) = %+v, want run-z", pr)
	}

	// A later store (restart) serves from the backend with no file present.
	s2 := NewStoreWithBackend(path, fb)
	if got := s2.Load("file-new"); got == nil || got.instanceID != "inst-new" {
		t.Fatalf("reopened Load(file-new) = %+v, want inst-new", got)
	}
	if pr := s2.LoadRun("runs-only", "agent-z"); pr == nil || pr.RunID != "run-z" {
		t.Fatalf("reopened LoadRun(runs-only) = %+v, want run-z", pr)
	}

	// The save path never recreates the JSON file.
	s2.Save("file-new", &cachedState{status: "active", instanceID: "inst-new2", model: "m", expiresAt: expiry, gracePeriodEndsAt: grace})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Save recreated the session file at %s, want import-only: %v", path, err)
	}
	if got := NewStoreWithBackend(path, fb).Load("file-new"); got == nil || got.instanceID != "inst-new2" {
		t.Fatalf("Load after Save = %+v, want inst-new2 via backend", got)
	}
}

// TestLegacyImportSkipsBackendBlobs pins the collision comparator: a
// byte-identical re-import (already-archived .bak consulted again) leaves
// the backend row untouched.
func TestLegacyImportIdenticalReimportSilent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	grace := expiry.Add(graceWindow)

	fb := newFakeSessionBackend()
	file := storeFile{
		Version: storeVersion,
		Sessions: map[string]persistedState{
			"k": {Status: "active", InstanceID: "inst-1", Model: "m", ExpiresAt: expiry, GracePeriodEndsAt: grace},
		},
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	first := NewStoreWithBackend(path, fb)
	if got := first.Load("k"); got == nil || got.instanceID != "inst-1" {
		t.Fatalf("first import Load = %+v, want inst-1", got)
	}
	sessBefore, runsBefore, found, _ := fb.LoadSession("k")
	if !found {
		t.Fatal("backend row missing after first import")
	}

	// Point a second store at the archive itself: identical content must
	// not rewrite the row.
	second := NewStoreWithBackend(path+".bak", fb)
	if got := second.Load("k"); got == nil || got.instanceID != "inst-1" {
		t.Fatalf("archive re-import Load = %+v, want inst-1", got)
	}
	sessAfter, runsAfter, found, _ := fb.LoadSession("k")
	if !found || sessAfter != sessBefore || runsAfter != runsBefore {
		t.Fatal("identical re-import rewrote the backend row, want untouched")
	}
}
