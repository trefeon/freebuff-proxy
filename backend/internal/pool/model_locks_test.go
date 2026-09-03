package pool

import (
	"context"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
)

// TestAcquireModelLocksRouteBySlot pins issue #325: slot 0 locked to modelA
// and slot 1 locked to modelB route strictly — a modelA request leases slot
// 0 (never slot 1) and vice versa, with no session churn on the wrong slot.
func TestAcquireModelLocksRouteBySlot(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.ModelLocks = map[int][]string{0: {modelA}, 1: {modelB}}
	}, mock0, mock1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	leaseA, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire %s: %v", modelA, err)
	}
	if leaseA.Token != 0 {
		t.Errorf("acquire %s leased token %d, want slot 0 (locked)", modelA, leaseA.Token)
	}
	// The wrong slot never admitted a session for the other model.
	if n := mock1.SessionCreatesSnapshot(); n != 0 {
		t.Errorf("slot 1 session creates during modelA acquire = %d, want 0", n)
	}
	p.LeaseRelease(leaseA)

	leaseB, err := p.Acquire(ctx, modelB)
	if err != nil {
		t.Fatalf("acquire %s: %v", modelB, err)
	}
	if leaseB.Token != 1 {
		t.Errorf("acquire %s leased token %d, want slot 1 (locked)", modelB, leaseB.Token)
	}
	p.LeaseRelease(leaseB)
}

// TestAcquireModelLocksFailFast pins the all-locked-out path: when no slot
// is locked to the requested model, Acquire fails with a routing error
// naming the model and attempts no upstream admission at all.
func TestAcquireModelLocksFailFast(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.ModelLocks = map[int][]string{0: {modelA}, 1: {modelA}}
	}, mock0, mock1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := p.Acquire(ctx, modelB)
	if err == nil {
		t.Fatalf("acquire %s with all slots locked away succeeded, want fail-fast", modelB)
	}
	if !strings.Contains(err.Error(), modelB) || !strings.Contains(err.Error(), "no account locked to model") {
		t.Errorf("fail-fast error = %q, want it to name the model and the lock", err)
	}
	if n := mock0.SessionCreatesSnapshot() + mock1.SessionCreatesSnapshot(); n != 0 {
		t.Errorf("session creates during locked-out acquire = %d, want 0 (no admission attempted)", n)
	}
	if n := len(mock0.StartedRunsSnapshot()) + len(mock1.StartedRunsSnapshot()); n != 0 {
		t.Errorf("run STARTs during locked-out acquire = %d, want 0", n)
	}
}

// TestAcquireModelLocksUnlockedFallback pins the mixed pool: a request for a
// model no lock covers falls back to unlocked slots when any exist.
func TestAcquireModelLocksUnlockedFallback(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.ModelLocks = map[int][]string{0: {modelA}}
	}, mock0, mock1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	lease, err := p.Acquire(ctx, modelB)
	if err != nil {
		t.Fatalf("acquire %s with unlocked fallback: %v", modelB, err)
	}
	if lease.Token != 1 {
		t.Errorf("acquire %s leased token %d, want unlocked slot 1", modelB, lease.Token)
	}
	p.LeaseRelease(lease)
}

// TestAcquireModelLocksSkipsCounted pins the observability counter: every
// lock-skip decision increments the slot's allowlistSkips, surfaced on the
// snapshot for cards and metrics.
func TestAcquireModelLocksSkipsCounted(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.ModelLocks = map[int][]string{0: {modelA}, 1: {modelA}}
	}, mock0, mock1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	lease, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("acquire %s: %v", modelA, err)
	}
	p.LeaseRelease(lease)

	snaps := p.Snapshot()
	if len(snaps) != 2 {
		t.Fatalf("snapshots = %d, want 2", len(snaps))
	}
	if len(snaps[0].AllowedModels) != 1 || snaps[0].AllowedModels[0] != modelA {
		t.Errorf("slot 0 AllowedModels = %v, want [%s]", snaps[0].AllowedModels, modelA)
	}
	// Slot 1 was skipped at least once while serving modelA traffic... but
	// slot 0 is hot for modelA now, so force a cold sweep: release and
	// re-acquire after invalidating? Simpler: the fail-fast probe below
	// exercises the loop gate on both slots.
	if _, err := p.Acquire(ctx, modelB); err == nil {
		t.Fatalf("acquire %s with all slots locked away succeeded, want fail-fast", modelB)
	}
	snaps = p.Snapshot()
	if snaps[0].AllowlistSkips == 0 || snaps[1].AllowlistSkips == 0 {
		t.Errorf("AllowlistSkips = [%d %d], want both >0 after locked-out acquire",
			snaps[0].AllowlistSkips, snaps[1].AllowlistSkips)
	}
}
