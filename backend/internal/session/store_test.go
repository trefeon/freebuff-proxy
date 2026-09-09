package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

func newTestManagerWithStore(t *testing.T, mock *testutil.MockUpstream, store *Store) *Manager {
	t.Helper()
	client, err := upstream.New("tok", &config.Config{
		UpstreamBaseURL:    mock.URL(),
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RotationInterval:   6 * time.Hour,
		RegistryRefresh:    6 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewManagerWithStore(client, store)
}

func TestStoreRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	fb := newFakeSessionBackend()
	store := NewStoreWithBackend(path, fb)

	if got := store.Load("key"); got != nil {
		t.Fatalf("Load on empty store = %+v, want nil", got)
	}

	expiry := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	store.Save("key", &cachedState{
		status:            "active",
		instanceID:        "inst-1",
		model:             "m",
		expiresAt:         expiry,
		gracePeriodEndsAt: expiry.Add(graceWindow),
		countryCode:       "US",
	})

	// A second Store instance over the same backend (restart) must see the write.
	store2 := NewStoreWithBackend(path, fb)
	got := store2.Load("key")
	if got == nil {
		t.Fatal("Load after Save = nil")
	}
	if got.instanceID != "inst-1" || got.status != "active" {
		t.Errorf("Load = %+v, want inst-1/active", got)
	}
	if !got.expiresAt.Equal(expiry) {
		t.Errorf("expiresAt = %v, want %v", got.expiresAt, expiry)
	}

	store.Remove("key", "")
	if got := NewStoreWithBackend(path, fb).Load("key"); got != nil {
		t.Errorf("Load after Remove = %+v, want nil", got)
	}
}

func TestStoreDropsExpiredGrace(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	store.Save("key", &cachedState{
		status:            "active",
		instanceID:        "inst-1",
		expiresAt:         time.Now().Add(-40 * time.Minute),
		gracePeriodEndsAt: time.Now().Add(-10 * time.Minute), // grace already closed
	})
	if got := store.Load("key"); got != nil {
		t.Fatalf("Load of expired entry = %+v, want nil", got)
	}
}

// TestShutdownKeepsActiveWhenPersist verifies the session-redesign contract:
// with persistence enabled, shutdown KEEPS the active upstream slot (no
// DELETE) so a restart resumes it via pollPersisted instead of burning a
// fresh premium slot. The store entry survives for the resume.
func TestShutdownKeepsActiveWhenPersist(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	mgr := newTestManagerWithStore(t, mock, store)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.SessionCreates != 1 {
		t.Fatalf("SessionCreates = %d, want 1", mock.SessionCreates)
	}

	if err := mgr.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.SessionEnds != 0 {
		t.Errorf("SessionEnds = %d, want 0 (active session kept for restart-resume when persisting)", mock.SessionEnds)
	}
	if got := store.Load(mgr.key); got == nil || got.instanceID != "inst-abc-123" {
		t.Errorf("store after Shutdown = %+v, want active inst-abc-123", got)
	}
}

func TestShutdownEndsSessionWithoutPersist(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.SessionEnds != 1 {
		t.Errorf("SessionEnds = %d, want 1 (no persistence → DELETE upstream)", mock.SessionEnds)
	}
}

func TestResumePersistedOnRestart(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	path := filepath.Join(t.TempDir(), "state.json")

	// First process: create a session and shut down. Shutdown DELETEs the
	// upstream slot but keeps the backend entry, so a restart can
	// still probe it via pollPersisted. Both processes share one backend
	// (sessions_persist); the legacy file path is import-only.
	fb := newFakeSessionBackend()
	mgr1 := newTestManagerWithStore(t, mock, NewStoreWithBackend(path, fb))
	if _, err := mgr1.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mgr1.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.SessionCreates != 1 {
		t.Fatalf("SessionCreates after first process = %d, want 1", mock.SessionCreates)
	}

	// Second process (same token → same store key): pollPersisted probes the
	// persisted slot. This mock's DELETE is stateless (the instance still
	// answers active), so the slot is resumed and no new quota is burned —
	// the same path that re-POSTs fresh when the DELETE took effect upstream.
	mgr2 := newTestManagerWithStore(t, mock, NewStoreWithBackend(path, fb))
	instance, err := mgr2.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("resumed instance = %q, want inst-abc-123", instance)
	}
	if mock.SessionCreates != 1 {
		t.Errorf("SessionCreates after resume = %d, want still 1 (no new quota burned)", mock.SessionCreates)
	}
	if mock.SessionPolls != 1 {
		t.Errorf("SessionPolls = %d, want 1 (resume poll)", mock.SessionPolls)
	}
}

func TestResumeSkipsDeadPersistedSession(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore(path)

	// The upstream reports the persisted slot as ended, then serves a fresh
	// active session for the re-create.
	mock.SessionSequence = []string{"ended", "active"}

	client, err := upstream.New("tok", &config.Config{
		UpstreamBaseURL:    mock.URL(),
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RotationInterval:   6 * time.Hour,
		RegistryRefresh:    6 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewManagerWithStore(client, store)
	store.Save(mgr.key, &cachedState{
		status:            "active",
		instanceID:        "inst-dead",
		expiresAt:         time.Now().Add(time.Hour),
		gracePeriodEndsAt: time.Now().Add(time.Hour + graceWindow),
	})

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The dead slot must have been discarded and a fresh session created.
	if mock.SessionCreates != 1 {
		t.Errorf("SessionCreates = %d, want 1 (dead slot re-created)", mock.SessionCreates)
	}
	got := store.Load(mgr.key)
	if got == nil || got.instanceID != "inst-abc-123" {
		t.Errorf("store after re-create = %+v, want fresh active inst-abc-123", got)
	}
}

// TestStoreSaveCreatesNoFile pins the DB-unified contract: the legacy JSON
// path is import-only — Save/SaveRun/Remove never create or modify the file,
// with or without a backend.
func TestStoreSaveCreatesNoFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	stores := []*Store{NewStore(path), NewStoreWithBackend(path, newFakeSessionBackend())}
	for i, store := range stores {
		store.Save("key", &cachedState{status: "active", instanceID: "inst-1", expiresAt: time.Now().Add(time.Hour), gracePeriodEndsAt: time.Now().Add(2 * time.Hour)})
		store.SaveRun("key", "agent-x", PersistedRun{RunID: "run-1", AgentID: "agent-x"})
		store.Remove("key", "")
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("store %d created the session file, want import-only: %v", i, err)
		}
		if got := store.Load("key"); got != nil {
			t.Fatalf("store %d Load after Remove = %+v, want nil", i, got)
		}
	}
}

func TestStoreRemoveCAS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	fb := newFakeSessionBackend()
	store := NewStoreWithBackend(path, fb)
	expiry := time.Now().Add(time.Hour).UTC()
	store.Save("key", &cachedState{status: "active", instanceID: "inst-1", expiresAt: expiry})

	// Wrong instance id: the entry must survive untouched.
	store.Remove("key", "inst-other")
	if got := store.Load("key"); got == nil || got.instanceID != "inst-1" {
		t.Fatalf("Remove with wrong instance = %+v, want inst-1", got)
	}
	// A fresh store over the same backend must agree (no-op persists nothing new).
	if got := NewStoreWithBackend(path, fb).Load("key"); got == nil || got.instanceID != "inst-1" {
		t.Fatalf("fresh Load after wrong-instance Remove = %+v, want inst-1", got)
	}

	// Matching instance id: the entry is removed.
	store.Remove("key", "inst-1")
	if got := store.Load("key"); got != nil {
		t.Fatalf("Remove with matching instance = %+v, want nil", got)
	}
	if got := NewStoreWithBackend(path, fb).Load("key"); got != nil {
		t.Fatalf("fresh Load after matching Remove = %+v, want nil", got)
	}

	// Empty expected instance id removes unconditionally.
	store.Save("key2", &cachedState{status: "active", instanceID: "inst-2", expiresAt: expiry})
	store.Remove("key2", "")
	if got := store.Load("key2"); got != nil {
		t.Fatalf("unconditional Remove = %+v, want nil", got)
	}

	// Removing an absent key is a no-op, not an error.
	store.Remove("absent", "anything")
}

func TestStoreConcurrentSaveLoadRemove(t *testing.T) {
	dir := t.TempDir()
	// The memory-only store issues no file writes here, but TempDir cleanup
	// on Windows can still hit a transient handle (AV/indexer scan) and fail
	// a single-pass RemoveAll with "directory is not empty". Retry the
	// removal so the flake cannot fail the suite.
	t.Cleanup(func() {
		for attempt := range 5 {
			if err := os.RemoveAll(dir); err == nil {
				return
			} else if attempt == 4 {
				t.Logf("could not remove temp dir %s: %v", dir, err)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	path := filepath.Join(dir, "state.json")
	fb := newFakeSessionBackend()
	store := NewStoreWithBackend(path, fb)

	const workers = 16
	const keysPerWorker = 8

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for k := 0; k < keysPerWorker; k++ {
				key := fmt.Sprintf("w%d-k%d", w, k)
				store.Save(key, &cachedState{
					status:     "active",
					instanceID: "inst-" + key,
					expiresAt:  time.Now().Add(time.Hour),
				})
				if got := store.Load(key); got == nil || got.instanceID != "inst-"+key {
					t.Errorf("Load(%q) after Save = %+v", key, got)
				}
				// Even keys are removed again by their own writer; the CAS
				// matches, so the removal must stick.
				if k%2 == 0 {
					store.Remove(key, "inst-"+key)
					if got := store.Load(key); got != nil {
						t.Errorf("Load(%q) after Remove = %+v, want nil", key, got)
					}
				}
			}
		}(w)
	}
	wg.Wait()

	// A fresh store over the same backend must see exactly the keys that were
	// saved but not removed: the odd keys of every worker.
	fresh := NewStoreWithBackend(path, fb)
	for w := 0; w < workers; w++ {
		for k := 0; k < keysPerWorker; k++ {
			key := fmt.Sprintf("w%d-k%d", w, k)
			got := fresh.Load(key)
			if k%2 == 0 {
				if got != nil {
					t.Errorf("fresh Load(%q) = %+v, want nil (removed)", key, got)
				}
			} else if got == nil || got.instanceID != "inst-"+key {
				t.Errorf("fresh Load(%q) = %+v, want inst-%s", key, got, key)
			}
		}
	}
}

func TestStoreCorruptLegacyFileIgnoredNeverRewritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	store := NewStore(path)
	if got := store.Load("key"); got != nil {
		t.Fatalf("Load on corrupt file = %+v, want nil", got)
	}

	// A Save after a corrupt read succeeds in memory and never touches the
	// broken file: the legacy path is import-only.
	store.Save("key", &cachedState{status: "active", instanceID: "inst-1", expiresAt: time.Now().Add(time.Hour), gracePeriodEndsAt: time.Now().Add(2 * time.Hour)})
	if got := store.Load("key"); got == nil || got.instanceID != "inst-1" {
		t.Fatalf("Load after Save over corrupt file = %+v, want inst-1", got)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("Save rewrote the corrupt legacy file, want import-only (byte-identical)")
	}
}

// TestStoreUnreadableLegacyFileStaysUsable: the legacy file is only a
// fallback seed — when it cannot be read (here a directory at the path, so
// the failure holds portably on every platform) the store stays fully
// usable on memory + backend, and nothing is ever written to the file path.
// The old read-failure/pending-merge machinery is gone with the file write
// target: there is no partial view left that a save could clobber.
func TestStoreUnreadableLegacyFileStaysUsable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	fb := newFakeSessionBackend()
	store := NewStoreWithBackend(path, fb)
	if got := store.Load("a"); got != nil {
		t.Fatalf("Load on unreadable legacy file = %+v, want nil", got)
	}
	store.Save("b", &cachedState{status: "active", instanceID: "inst-b", expiresAt: time.Now().Add(time.Hour), gracePeriodEndsAt: time.Now().Add(2 * time.Hour)})
	if got := store.Load("b"); got == nil || got.instanceID != "inst-b" {
		t.Fatalf("Load('b') = %+v, want inst-b (memory + backend usable despite unreadable file)", got)
	}
	// A fresh store over the same backend resumes 'b' without the file.
	if got := NewStoreWithBackend(path, fb).Load("b"); got == nil || got.instanceID != "inst-b" {
		t.Fatalf("fresh Load('b') = %+v, want inst-b", got)
	}
}

// TestStoreVersionMismatchIgnoredNeverReplaced is the version-mismatch case:
// a legacy file with a version other than storeVersion is ignored (empty
// view), and the next Save leaves it byte-identical: the legacy path is
// import-only, never replaced wholesale.
func TestStoreVersionMismatchIgnoredNeverReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	file := storeFile{
		Version: storeVersion + 1,
		Sessions: map[string]persistedState{
			"old": {Status: "active", InstanceID: "inst-old", ExpiresAt: time.Now().Add(time.Hour)},
		},
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	store := NewStore(path)
	if got := store.Load("old"); got != nil {
		t.Fatalf("Load of version-mismatched entry = %+v, want nil (ignored)", got)
	}

	store.Save("new", &cachedState{status: "active", instanceID: "inst-new", expiresAt: time.Now().Add(time.Hour), gracePeriodEndsAt: time.Now().Add(2 * time.Hour)})
	if got := store.Load("new"); got == nil || got.instanceID != "inst-new" {
		t.Fatalf("Load('new') after Save = %+v, want inst-new (memory)", got)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, data) {
		t.Fatal("Save replaced the version-mismatched legacy file, want import-only (byte-identical)")
	}
	// Without a backend there is no cross-instance durability: a fresh
	// memory-only store over the same path sees neither entry.
	fresh := NewStore(path)
	if got := fresh.Load("new"); got != nil {
		t.Errorf("fresh Load('new') = %+v, want nil (no backend, no durability)", got)
	}
	if got := fresh.Load("old"); got != nil {
		t.Errorf("fresh Load('old') = %+v, want nil (version-mismatched file stays ignored)", got)
	}
}

// TestStoreEmptyKeyNoop verifies Save/Remove with an empty key are no-ops
// that do not even create the store file.
func TestStoreEmptyKeyNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore(path)

	store.Save("", &cachedState{status: "active", instanceID: "inst-1", expiresAt: time.Now().Add(time.Hour)})
	store.Remove("", "")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("state file exists after empty-key Save/Remove, want not created: %v", err)
	}

	// A real key still works afterwards.
	store.Save("key", &cachedState{status: "active", instanceID: "inst-1", expiresAt: time.Now().Add(time.Hour)})
	if got := store.Load("key"); got == nil || got.instanceID != "inst-1" {
		t.Fatalf("Load('key') after Save = %+v, want inst-1", got)
	}
}

func TestStoreIgnoresOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, make([]byte, maxStoreFileSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path)
	if got := store.Load("key"); got != nil {
		t.Fatalf("Load on oversized file = %+v, want nil", got)
	}
}

func TestStoreDropsActiveEntryWithoutInstanceID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	file := storeFile{
		Version: storeVersion,
		Sessions: map[string]persistedState{
			"bad":  {Status: "active", InstanceID: "", ExpiresAt: time.Now().Add(time.Hour)},
			"good": {Status: "active", InstanceID: "inst-1", ExpiresAt: time.Now().Add(time.Hour)},
		},
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	store := NewStore(path)
	if got := store.Load("bad"); got != nil {
		t.Errorf("Load of invalid active entry = %+v, want nil", got)
	}
	if got := store.Load("good"); got == nil || got.instanceID != "inst-1" {
		t.Errorf("Load of valid entry = %+v, want inst-1", got)
	}
}
