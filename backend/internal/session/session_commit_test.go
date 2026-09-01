package session

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// newReviewFixManager builds a manager wired to a temp-file store, mirroring
// newPersistTestManager, for the review-2026-08-31 regression tests. The
// mock upstream is only there so Shutdown's EndSession has somewhere to
// talk to; the tests never drive admission.
func newReviewFixManager(t *testing.T) (*Manager, *Store, string) {
	t.Helper()
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
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
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	mgr := NewManagerWithStore(client, store)
	return mgr, store, client.TokenKey()
}

// reviewFixActiveState returns an active cached state bound to instanceID,
// usable immediately (expiry an hour out).
func reviewFixActiveState(instanceID string) *cachedState {
	expiry := time.Now().Add(time.Hour)
	return &cachedState{
		status:            "active",
		instanceID:        instanceID,
		model:             "deepseek/deepseek-v4-flash",
		expiresAt:         expiry,
		gracePeriodEndsAt: expiry.Add(graceWindow),
	}
}

// waitParkedInStoreSave polls all goroutine stacks until some goroutine is
// parked inside Store.Save. The test holds the store mutex, so once the
// frame appears the goroutine is deterministically stopped at the write —
// no timing-based synchronization involved.
func waitParkedInStoreSave(t *testing.T) {
	t.Helper()
	const marker = "session.(*Store).Save("
	deadline := time.Now().Add(5 * time.Second)
	buf := make([]byte, 1<<20)
	for time.Now().Before(deadline) {
		n := runtime.Stack(buf, true)
		if strings.Contains(string(buf[:n]), marker) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no goroutine reached %s within 5s", marker)
}

// assertSnapshotUnblocked asserts a concurrent Snapshot completes while the
// tested goroutine is parked inside Store.Save: the manager mutex must be
// released across the disk write (review 2026-08-31 P3). Holding mu across
// the flush (the pre-fix shape) deadlocks Snapshot until the timeout.
func assertSnapshotUnblocked(t *testing.T, mgr *Manager, store *Store) {
	t.Helper()
	snapCh := make(chan SessionSnapshot, 1)
	go func() { snapCh <- mgr.Snapshot() }()
	select {
	case <-snapCh:
	case <-time.After(5 * time.Second):
		store.mu.Unlock() // let the parked writer finish before cleanup
		t.Fatal("Snapshot blocked while store.Save is in flight: manager mutex held across disk I/O")
	}
}

// TestCommitSavesOutsideManagerLock pins the review-2026-08-31 P3 fix on
// the admission path: commit's store write runs with the manager mutex
// RELEASED, so a slow flush (temp write + rename inside Store) cannot block
// concurrent EnsureSessionForModel/Snapshot callers, and the committed
// state still lands in the store.
func TestCommitSavesOutsideManagerLock(t *testing.T) {
	mgr, store, key := newReviewFixManager(t)

	mgr.mu.Lock()
	mgr.commit(reviewFixActiveState("inst-seed"))
	mgr.mu.Unlock()

	store.mu.Lock() // gate: the commit's Save parks on the store mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		mgr.mu.Lock()
		mgr.commit(reviewFixActiveState("inst-next"))
		mgr.mu.Unlock()
	}()

	waitParkedInStoreSave(t)
	assertSnapshotUnblocked(t, mgr, store)

	store.mu.Unlock()
	<-done

	if persisted := store.Load(key); persisted == nil || persisted.instanceID != "inst-next" {
		t.Errorf("persisted entry after commit = %+v, want inst-next (commit must still mirror state into the store)", persisted)
	}
}

// TestShutdownSavesAndVerifiesOutsideManagerLock pins the review-2026-08-31
// P3 fix on the shutdown path: Shutdown's flush AND its on-disk verification
// re-read run with the manager mutex released, and the entry survives for
// restart resume.
func TestShutdownSavesAndVerifiesOutsideManagerLock(t *testing.T) {
	mgr, store, key := newReviewFixManager(t)

	mgr.mu.Lock()
	mgr.commit(reviewFixActiveState("inst-shutdown"))
	mgr.mu.Unlock()

	store.mu.Lock() // gate: Shutdown's flush parks on the store mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = mgr.Shutdown(context.Background()) // EndSession's upstream result is irrelevant here
	}()

	waitParkedInStoreSave(t)
	assertSnapshotUnblocked(t, mgr, store)

	store.mu.Unlock()
	<-done

	if persisted := store.Load(key); persisted == nil || persisted.instanceID != "inst-shutdown" {
		t.Errorf("store entry after Shutdown = %+v, want inst-shutdown kept for restart resume", persisted)
	}
}
