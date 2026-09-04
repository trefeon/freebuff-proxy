package pool

// Restart-resume tests: a container restart wipes in-memory sessions while
// the on-disk store keeps the instance handle + last-seen quota. The next
// Acquire must resume the persisted session (one zero-cost GET) instead of
// 429ing on the restored (stale-marked) quota or burning a fresh admission.

import (
	"context"
	"path/filepath"
	"testing"

	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/testutil"
)

func TestAcquireResumesPersistedSessionAfterRestart(t *testing.T) {
	reset := futureReset()
	mock := testutil.NewMock()
	defer mock.Close()
	// Capped per last-seen quota: without the stale exemption the pool
	// would 429 here and the persisted session would never be resumed.
	mock.RateLimitsByModel = quotaFor(modelA, 5, 5, reset)
	p := newTestPool(t, mock)
	toks := p.roster.Load()
	client := (*toks)[0].client
	store := session.NewStore(filepath.Join(t.TempDir(), "state.json"))

	// Pre-restart: admit through a store-backed manager (persists the
	// instance handle + quota to disk).
	mgr1 := session.NewManagerWithStore(client, store)
	(*toks)[0].session = mgr1
	if _, err := mgr1.EnsureSessionForModel(context.Background(), modelA); err != nil {
		t.Fatalf("pre-restart admit: %v", err)
	}
	if mock.SessionCreates != 1 {
		t.Fatalf("session creates = %d, want 1 (the admit)", mock.SessionCreates)
	}

	// Simulate a container restart: fresh manager, same store + client.
	// Memory is empty; disk holds the live instance + capped quota.
	mgr2 := session.NewManagerWithStore(client, store)
	(*toks)[0].session = mgr2
	if snap := mgr2.Snapshot(); !snap.QuotaStale {
		t.Fatal("restored snapshot QuotaStale = false, want true")
	}

	// The next Acquire must resume (zero new admission), not 429.
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("post-restart Acquire = %v, want resume success", err)
	}
	if lease.SessionInstanceID == "" {
		t.Error("resumed lease has no session instance id")
	}
	p.LeaseRelease(lease)
	if mock.SessionCreates != 1 {
		t.Errorf("session creates = %d, want 1 (resume posts no admission)", mock.SessionCreates)
	}
}
