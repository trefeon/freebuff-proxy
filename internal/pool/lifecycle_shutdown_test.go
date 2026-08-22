package pool

// Regression guards for the P1/P2 shutdown fixes:
//
//   - Pool.Shutdown must not hold bridgeMu across the per-entry upstream
//     drain (FinishAllRuns + session shutdown for every cached bridge
//     entry): with the lock held for the whole drain, every other bridge
//     operation (AcquireBridge, bridgeRecordChat/bridgeRecordSpend,
//     BridgeCount) stalls behind sequential upstream calls (the same rule
//     bridgeEvictLocked / bridgeMaintain already follow).
//   - A bridge entry with an outstanding lease at shutdown must not lose
//     its FINISH: FinishAllRuns defers in-flight runs, and the last lease
//     release re-queues the FINISH (P1).

import (
	"context"
	"fmt"
	"testing"
	"time"

	"freebuff-proxy/internal/testutil"
)

// TestShutdownBridgeDrainOutsideBridgeMu is the regression guard for the
// P2 bridgeMu stall: Pool.Shutdown used to hold bridgeMu across the whole
// per-entry drain. Here a slow FINISH (mock FinishDelay) holds the drain in
// flight while a concurrent bridgeMu acquisition must still complete — with
// the bug it would block for the rest of the drain.
func TestShutdownBridgeDrainOutsideBridgeMu(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgePool(t, mock)

	// Arm the slow FINISH BEFORE the releases: with the #91 context-pruner
	// child traffic gone, the release-path drain FINISHes are the ONLY
	// upstream work, and a slow parent FINISH is what holds Shutdown inside
	// the bridge drain for the assertion below.
	mock.SetFinishDelay(time.Second)
	for i := 0; i < 2; i++ {
		lease, err := p.AcquireBridge(context.Background(), fmt.Sprintf("shutdown-tok-%d", i), modelA)
		if err != nil {
			t.Fatalf("AcquireBridge token %d: %v", i, err)
		}
		p.LeaseRelease(lease)
	}

	done := make(chan struct{})
	go func() {
		p.Shutdown(context.Background())
		close(done)
	}()

	// A parent FINISH (the first entry's release-path drain) is now in
	// flight — Shutdown waits on it inside the bridge drain. bridgeMu must
	// be acquirable while that wait runs: the drain runs OUTSIDE the lock.
	eventually(t, "drain FINISH in flight", func() bool {
		return mock.FinishesStartedSnapshot() >= 1
	})
	locked := make(chan struct{})
	go func() {
		p.bridgeMu.Lock()
		close(locked)
	}()
	select {
	case <-locked:
		p.bridgeMu.Unlock()
	case <-time.After(200 * time.Millisecond):
		t.Fatal("bridgeMu held during bridge drain (FinishAllRuns ran under the lock)")
	}

	<-done

	finished := mock.FinishedRunsSnapshot()
	if len(finished) != 2 {
		t.Errorf("finished runs = %d, want 2 (both bridge entries drained)", len(finished))
	}
	for _, f := range finished {
		if f.Status != "completed" {
			t.Errorf("run %s finished with status %q, want completed", f.RunID, f.Status)
		}
	}
	if mock.SessionEnds != 2 {
		t.Errorf("session ends = %d, want 2 (both bridge sessions ended)", mock.SessionEnds)
	}
}

// TestShutdownBridgeInflightRunFinishedOnRelease is the pool-level half of
// the P1 fix: a bridge entry with an outstanding lease at shutdown must not
// lose its FINISH. FinishAllRuns defers the in-flight run (it stays
// tracked); the last lease release re-queues the FINISH, so the upstream
// agent run is not orphaned. Previously FinishAllRuns deleted the run from
// both sets, leaving it unreachable and never FINISHed.
func TestShutdownBridgeInflightRunFinishedOnRelease(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgePool(t, mock)

	lease, err := p.AcquireBridge(context.Background(), "inflight-tok", modelA)
	if err != nil {
		t.Fatal(err)
	}
	// Hold the lease across shutdown: the bridge drain sees inflight > 0.

	p.Shutdown(context.Background())

	entry := p.bridgeToken("inflight-tok")
	if entry == nil {
		t.Fatal("bridge entry missing after shutdown")
	}
	// The run is deferred, not orphaned: it must stay tracked so the last
	// lease release can re-queue its FINISH.
	if snap := entry.runs.Snapshot(); snap.ActiveRuns != 1 {
		t.Fatalf("ActiveRuns after shutdown with in-flight lease = %d, want 1 (run deferred, not dropped)", snap.ActiveRuns)
	}

	p.LeaseRelease(lease)

	eventually(t, "FINISH of deferred in-flight bridge run after release", func() bool {
		finished := mock.FinishedRunsSnapshot()
		return len(finished) == 1 && finished[0].Status == "completed"
	})
	if snap := entry.runs.Snapshot(); snap.ActiveRuns != 0 {
		t.Errorf("ActiveRuns after release + FINISH = %d, want 0", snap.ActiveRuns)
	}
}
