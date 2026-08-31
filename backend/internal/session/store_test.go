package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	store := NewStore(path)

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

	// A second Store instance over the same file must see the write.
	store2 := NewStore(path)
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
	if got := NewStore(path).Load("key"); got != nil {
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
	// upstream slot but keeps the store entry, so a restart can
	// still probe it via pollPersisted.
	mgr1 := newTestManagerWithStore(t, mock, NewStore(path))
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
	mgr2 := newTestManagerWithStore(t, mock, NewStore(path))
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

func TestStoreFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore(path)
	store.Save("key", &cachedState{status: "active", instanceID: "i", expiresAt: time.Now().Add(time.Hour)})

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Unix enforces 0600; Windows ignores the perm bits (only the read-only
	// attribute is honored), so the mode assertion is Unix-only.
	if runtime.GOOS != "windows" {
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("state file mode = %o, want 600", perm)
		}
	}
}

func TestStoreRemoveCAS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore(path)
	expiry := time.Now().Add(time.Hour).UTC()
	store.Save("key", &cachedState{status: "active", instanceID: "inst-1", expiresAt: expiry})

	// Wrong instance id: the entry must survive untouched.
	store.Remove("key", "inst-other")
	if got := store.Load("key"); got == nil || got.instanceID != "inst-1" {
		t.Fatalf("Remove with wrong instance = %+v, want inst-1", got)
	}
	// A fresh store over the same file must agree (no-op must not flush).
	if got := NewStore(path).Load("key"); got == nil || got.instanceID != "inst-1" {
		t.Fatalf("fresh Load after wrong-instance Remove = %+v, want inst-1", got)
	}

	// Matching instance id: the entry is removed.
	store.Remove("key", "inst-1")
	if got := store.Load("key"); got != nil {
		t.Fatalf("Remove with matching instance = %+v, want nil", got)
	}
	if got := NewStore(path).Load("key"); got != nil {
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
	// The store hammers hundreds of atomic temp+rename writes into this
	// directory; on Windows a transient handle (AV/indexer scan) can make a
	// single-pass RemoveAll fail with "directory is not empty". Retry the
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
	store := NewStore(path)

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

	// A fresh store over the final file must see exactly the keys that were
	// saved but not removed: the odd keys of every worker.
	fresh := NewStore(path)
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

func TestStoreCorruptFileLoadAndOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}

	store := NewStore(path)
	if got := store.Load("key"); got != nil {
		t.Fatalf("Load on corrupt file = %+v, want nil", got)
	}

	// A Save after a corrupt read must succeed and replace the broken file
	// with a valid one carrying the new entry.
	store.Save("key", &cachedState{status: "active", instanceID: "inst-1", expiresAt: time.Now().Add(time.Hour)})
	if got := NewStore(path).Load("key"); got == nil || got.instanceID != "inst-1" {
		t.Fatalf("Load after Save over corrupt file = %+v, want inst-1", got)
	}
}

func TestStoreReadErrorDoesNotClobberFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not enforced on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	seed := NewStore(path)
	seed.Save("a", &cachedState{status: "active", instanceID: "inst-a", expiresAt: time.Now().Add(time.Hour)})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Block access to the directory so both the read and the later write
	// fail: a Save must not clobber a file it could not read.
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Errorf("restoring dir perms: %v", err)
		}
	}()

	store := NewStore(path)
	if got := store.Load("a"); got != nil {
		t.Fatalf("Load on unreadable store = %+v, want nil", got)
	}
	// The Save fails gracefully (temp creation is blocked) and leaves the
	// on-disk file untouched instead of replacing it with an empty view.
	store.Save("b", &cachedState{status: "active", instanceID: "inst-b", expiresAt: time.Now().Add(time.Hour)})

	// Restore access before reading the file back (the deferred restore is
	// only a safety net for temp-dir cleanup).
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("Save clobbered an unreadable file: got %d bytes, want %d", len(after), len(before))
	}

	// Once access is restored the store must re-read the file (the failed
	// read must not have been cached as an empty store) and see the seed.
	if got := store.Load("a"); got == nil || got.instanceID != "inst-a" {
		t.Fatalf("Load after perms restored = %+v, want inst-a", got)
	}
}

// TestStoreReadErrorDoesNotClobberFileUnreadableFile is the read-failure regression:
// the on-disk file itself is unreadable (chmod 000) while the DIRECTORY stays
// writable, so a Save could silently replace the file with the in-memory
// partial view — destroying every OTHER token's persisted entries. The store
// must skip the flush while the file is unreadable (keep the in-memory
// update only), leaving the on-disk file byte-identical, and re-read it once
// access is restored.
func TestStoreReadErrorDoesNotClobberFileUnreadableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not enforced on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	seed := NewStore(path)
	seed.Save("a", &cachedState{status: "active", instanceID: "inst-a", expiresAt: time.Now().Add(time.Hour)})
	seed.Save("other", &cachedState{status: "active", instanceID: "inst-other", expiresAt: time.Now().Add(time.Hour)})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Make the FILE unreadable but leave the directory writable: this is the
	// dangerous window where the old code flushed the partial view over the
	// file (the both-fail dir test never hit it).
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(path, 0o600); err != nil {
			t.Errorf("restoring file perms: %v", err)
		}
	}()

	store := NewStore(path)
	if got := store.Load("a"); got != nil {
		t.Fatalf("Load on unreadable store = %+v, want nil", got)
	}
	// Save during the failure window must NOT replace the file.
	store.Save("b", &cachedState{status: "active", instanceID: "inst-b", expiresAt: time.Now().Add(time.Hour)})
	store.Remove("a", "") // remove must also not flush the partial view

	// Restore access before reading the file back (the deferred restore is
	// only a safety net for temp-dir cleanup).
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("Save/Remove clobbered an unreadable file: got %d bytes, want %d (other tokens' entries destroyed)", len(after), len(before))
	}

	// Once access is restored the store re-reads the file (the failed read
	// must not have been cached as an empty store) and sees the seeds.
	if got := store.Load("a"); got == nil || got.instanceID != "inst-a" {
		t.Fatalf("Load('a') after perms restored = %+v, want inst-a", got)
	}
	if got := store.Load("other"); got == nil || got.instanceID != "inst-other" {
		t.Fatalf("Load('other') after perms restored = %+v, want inst-other (other token survived)", got)
	}

	// A healed Save must now flush the merged map and work normally.
	store.Save("c", &cachedState{status: "active", instanceID: "inst-c", expiresAt: time.Now().Add(time.Hour)})
	if got := NewStore(path).Load("c"); got == nil || got.instanceID != "inst-c" {
		t.Fatalf("Load('c') after healed Save = %+v, want inst-c", got)
	}
	if got := NewStore(path).Load("a"); got == nil || got.instanceID != "inst-a" {
		t.Fatalf("Load('a') after healed Save = %+v, want inst-a (existing entry preserved on flush)", got)
	}
}

// TestStorePendingMutationSurvivesReadFailure is the read-failure regression: a
// Save/Remove made while the file was unreadable was kept in memory but
// never flushed; when the file became readable again the reload rebuilt
// s.data from disk, silently discarding the in-window update — the
// following flush persisted WITHOUT it, so a restart could not resume that
// session and burned a daily slot. Pending mutations must be merged back
// over the disk content on the successful reload and flushed.
func TestStorePendingMutationSurvivesReadFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not enforced on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	seed := NewStore(path)
	seed.Save("a", &cachedState{status: "active", instanceID: "inst-a", expiresAt: time.Now().Add(time.Hour)})

	// Make the FILE unreadable but leave the directory writable.
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(path, 0o600); err != nil {
			t.Errorf("restoring file perms: %v", err)
		}
	}()

	store := NewStore(path)
	if got := store.Load("a"); got != nil {
		t.Fatalf("Load on unreadable store = %+v, want nil", got)
	}
	// A mutation made while the file is unreadable cannot flush.
	store.Save("b", &cachedState{status: "active", instanceID: "inst-b", expiresAt: time.Now().Add(time.Hour)})

	// Restore access before reloading.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}

	// The successful reload must merge the in-window mutation back over the
	// disk content: 'b' survives alongside the pre-existing 'a'.
	if got := store.Load("b"); got == nil || got.instanceID != "inst-b" {
		t.Fatalf("Load('b') after reload = %+v, want inst-b (in-window update lost)", got)
	}
	if got := store.Load("a"); got == nil || got.instanceID != "inst-a" {
		t.Fatalf("Load('a') after reload = %+v, want inst-a (disk content preserved)", got)
	}

	// The merged map is flushed: a fresh store over the same file resumes
	// 'b' — a restart would not have burned a daily slot.
	if got := NewStore(path).Load("b"); got == nil || got.instanceID != "inst-b" {
		t.Fatalf("fresh Load('b') = %+v, want inst-b (merge not persisted)", got)
	}
	if got := NewStore(path).Load("a"); got == nil || got.instanceID != "inst-a" {
		t.Fatalf("fresh Load('a') = %+v, want inst-a", got)
	}
}

// TestStorePendingMutationSurvivesReadFailurePortable is the same read-failure
// regression as TestStorePendingMutationSurvivesReadFailure but forces the
// read failure PORTABLY — the store file is replaced by a directory, so
// os.ReadFile fails with a non-ErrNotExist error on every platform (chmod
// 000 is not enforced on Windows). This keeps the pending-merge path
// exercised on Windows boxes where the chmod-based test skips.
func TestStorePendingMutationSurvivesReadFailurePortable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	seed := NewStore(path)
	seed.Save("a", &cachedState{status: "active", instanceID: "inst-a", expiresAt: time.Now().Add(time.Hour)})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Replace the file with a directory: reads fail, and a flush would also
	// fail (temp creation is blocked by the path being a directory), so the
	// read-failure window holds on every platform.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	store := NewStore(path)
	if got := store.Load("a"); got != nil {
		t.Fatalf("Load on unreadable store = %+v, want nil", got)
	}
	// A mutation made while the store is unreadable cannot flush.
	store.Save("b", &cachedState{status: "active", instanceID: "inst-b", expiresAt: time.Now().Add(time.Hour)})

	// Restore the original file.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}

	// The successful reload must merge the in-window mutation back over the
	// disk content, and the merged map must be flushed (fresh store sees it).
	if got := store.Load("b"); got == nil || got.instanceID != "inst-b" {
		t.Fatalf("Load('b') after reload = %+v, want inst-b (in-window update lost)", got)
	}
	if got := store.Load("a"); got == nil || got.instanceID != "inst-a" {
		t.Fatalf("Load('a') after reload = %+v, want inst-a (disk content preserved)", got)
	}
	if got := NewStore(path).Load("b"); got == nil || got.instanceID != "inst-b" {
		t.Fatalf("fresh Load('b') = %+v, want inst-b (merge not persisted)", got)
	}
}

// TestStoreVersionMismatchIgnoredThenReplaced is the version-mismatch case: a store file with a
// version other than storeVersion is ignored (empty view), and the next Save
// replaces it wholesale with the current version.
func TestStoreVersionMismatchIgnoredThenReplaced(t *testing.T) {
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

	store.Save("new", &cachedState{status: "active", instanceID: "inst-new", expiresAt: time.Now().Add(time.Hour)})
	fresh := NewStore(path)
	if got := fresh.Load("new"); got == nil || got.instanceID != "inst-new" {
		t.Fatalf("Load('new') after Save = %+v, want inst-new", got)
	}
	if got := fresh.Load("old"); got != nil {
		t.Errorf("Load('old') after Save = %+v, want nil (version-mismatched file replaced wholesale)", got)
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
