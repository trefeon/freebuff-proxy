// smartprobe_persist_test.go — smart-probe restart persistence: the
// scheduler timer (lastProbe, backoff, overloadedAt, idleSlept) and the
// live per-token quota cache (QuotaByModel + QuotaSavedAt) ride pool_state
// under pool/probe/* keys (SHA-keyed, never positional), so a restart
// resumes warm — the boot round still fires as an event but probes
// nothing while the cache is fresh — and in-use tokens sit out rounds
// until their lease drains.
package pool

import (
	"context"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
)

func (m *memPoolPersist) saveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saves
}

// TestSmartProbeSchedulerPersistRoundTrip pins the persisted timer shape:
// lastProbe/backoff/overloadedAt/idleSlept survive a flush + fresh-pool
// restore, while bootDone is never persisted (the restart re-arms the
// boot round) and the transient kick/inflight flags hard-reset to false
// (a persisted inflight would suppress ticks forever).
func TestSmartProbeSchedulerPersistRoundTrip(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mem := newMemPoolPersist()

	p1 := newTestPool(t, mock)
	p1.SetPoolPersist(mem)
	lastProbe := time.Now().Add(-time.Minute)
	overloadedAt := time.Now().Add(-30 * time.Second)
	p1.smartProbe.mu.Lock()
	p1.smartProbe.lastProbe = lastProbe
	p1.smartProbe.backoff = 4
	p1.smartProbe.idleSlept = true
	p1.smartProbe.overloadedAt = overloadedAt
	p1.smartProbe.bootDone = true
	p1.smartProbe.kick = true
	p1.smartProbe.inflight = true
	p1.smartProbe.mu.Unlock()
	p1.markPersistDirty()
	if err := p1.FlushPoolPersist(); err != nil {
		t.Fatalf("FlushPoolPersist: %v", err)
	}
	found := false
	for _, k := range mem.keys() {
		if k == poolProbeScheduler {
			found = true
		}
	}
	if !found {
		t.Fatalf("scheduler row %q missing after flush: %v", poolProbeScheduler, mem.keys())
	}

	p2 := newTestPool(t, mock)
	p2.SetPoolPersist(mem)
	p2.RestorePoolPersist()

	p2.smartProbe.mu.Lock()
	defer p2.smartProbe.mu.Unlock()
	if got := p2.smartProbe.lastProbe.UnixMilli(); got != lastProbe.UnixMilli() {
		t.Errorf("restored lastProbe = %v, want %v", got, lastProbe.UnixMilli())
	}
	if p2.smartProbe.backoff != 4 {
		t.Errorf("restored backoff = %d, want 4", p2.smartProbe.backoff)
	}
	if !p2.smartProbe.idleSlept {
		t.Error("restored idleSlept = false, want true")
	}
	if got := p2.smartProbe.overloadedAt.UnixMilli(); got != overloadedAt.UnixMilli() {
		t.Errorf("restored overloadedAt = %v, want %v", got, overloadedAt.UnixMilli())
	}
	if p2.smartProbe.bootDone {
		t.Error("restored bootDone = true, want false (restart re-arms the boot round)")
	}
	if p2.smartProbe.kick {
		t.Error("restored kick = true, want false (transient)")
	}
	if p2.smartProbe.inflight {
		t.Error("restored inflight = true, want false (would suppress ticks forever)")
	}
}

// TestSmartProbeRestartSkipsFreshCache is the restart simulation:
// populate the cache with a live round, flush, rebuild a fresh pool over
// the same store, and prove the first tick sends zero probes while the
// restored cache is fresh — with the dashboard view warm immediately.
func TestSmartProbeRestartSkipsFreshCache(t *testing.T) {
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mem := newMemPoolPersist()

	p1 := newTestPool(t, mock1)
	p1.SetPoolPersist(mem)
	setQuotaAutoProbe(p1, true)
	probeTick(t, p1, context.Background(), time.Now())
	if got := mock1.SessionProbesSnapshot(); got != 1 {
		t.Fatalf("p1 probes = %d, want 1 (live round populates the cache)", got)
	}
	if err := p1.FlushPoolPersist(); err != nil {
		t.Fatalf("FlushPoolPersist: %v", err)
	}
	// Raw tokens never reach the store (SHA-only rule).
	for _, k := range mem.keys() {
		raw, _, _ := mem.LoadPoolState(k)
		if strings.Contains(k, "tok-0") || strings.Contains(string(raw), "tok-0") {
			t.Fatalf("raw token leaked into pool_state row %q", k)
		}
	}

	mock2 := testutil.NewMock()
	defer mock2.Close()
	p2 := newTestPool(t, mock2)
	p2.SetPoolPersist(mem)
	setQuotaAutoProbe(p2, true)
	p2.RestorePoolPersist()

	// Warm view before any new probe traffic.
	snap := p2.Snapshot()[0]
	if len(snap.QuotaByModel) == 0 {
		t.Fatal("restored QuotaByModel empty, want the warm cache")
	}
	if snap.QuotaSavedAt.IsZero() {
		t.Error("restored QuotaSavedAt zero, want the flush-time write")
	}
	if !snap.QuotaStale {
		t.Error("restored QuotaStale = false, want true (last-known until live contact)")
	}

	// The boot round fires as an event but probes nothing: every token
	// is fresh from the restore.
	p2.quotaBootAt = time.Now()
	probeTick(t, p2, context.Background(), time.Now())
	if got := mock2.SessionProbesSnapshot(); got != 0 {
		t.Errorf("p2 probes after restart = %d, want 0 (fresh cache skips the round)", got)
	}
	// Probe traffic stays out of the usage accounts on the new process too.
	if got := p2.usageCount(0); got != 0 {
		t.Errorf("p2 usageCount = %d, want 0 (probes never touch the ledgers)", got)
	}
	if got := p2.dayRequestCount(0); got != 0 {
		t.Errorf("p2 dayRequestCount = %d, want 0 (probes never feed the activity ledger)", got)
	}
	if got := p2.requestsServed.Load(); got != 0 {
		t.Errorf("p2 requestsServed = %d, want 0 (probes are not client traffic)", got)
	}
}

// TestSmartProbeSkipsInUseToken pins the true-smart refresh: a token with
// in-flight runs sits out the round (same InflightCount gate as
// sessionPollTick/maintainTick — no new busy signal) while the idle token
// is still covered, and rejoins once the lease drains.
func TestSmartProbeSkipsInUseToken(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)
	setQuotaAutoProbe(p, true)
	now := time.Now()
	p.quotaBootAt = now.Add(-time.Hour)
	markPoolActive(p, now)

	ctx := context.Background()
	lease, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatal(err)
	}
	toks := p.roster.Load()
	held := -1
	for i, e := range *toks {
		if e.runs.InflightCount() > 0 {
			if held != -1 {
				t.Fatal("multiple tokens in flight after one Acquire")
			}
			held = i
		}
	}
	if held == -1 {
		t.Fatal("no token in flight after Acquire")
	}
	if !smartProbeSkipToken((*toks)[held], now) {
		t.Errorf("held token skipped = false, want true (in-use)")
	}

	// The round covers only the idle token.
	probeTick(t, p, ctx, now)
	heldMock, otherMock := mock0, mock1
	if held == 1 {
		heldMock, otherMock = mock1, mock0
	}
	if got := heldMock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("held token probes = %d, want 0 (in-use skip)", got)
	}
	if got := otherMock.SessionProbesSnapshot(); got != 1 {
		t.Errorf("idle token probes = %d, want 1 (round still covers it)", got)
	}

	// Released, the token rejoins: no longer skipped, and probed on the
	// next due tick (its quota is still empty — the default mock
	// admission carries none — so staleness, not freshness, decides).
	p.LeaseRelease(lease)
	if smartProbeSkipToken((*toks)[held], now) {
		t.Error("released token skipped = true, want false")
	}
	probeTick(t, p, ctx, now.Add(61*time.Second))
	if got := heldMock.SessionProbesSnapshot(); got != 1 {
		t.Errorf("released token probes = %d, want 1 (probed once the lease drains)", got)
	}
}

// TestSmartProbeStartRestoresAndTickFlushes is the boot-level proof: Start
// restores the timer + quota cache synchronously (warm view before any
// tick), the maintain tick then runs the boot round with zero probes on
// the warm cache, and the following tick flushes the re-stamped timer.
func TestSmartProbeStartRestoresAndTickFlushes(t *testing.T) {
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mem := newMemPoolPersist()

	p1 := newTestPool(t, mock1)
	p1.SetPoolPersist(mem)
	setQuotaAutoProbe(p1, true)
	probeTick(t, p1, context.Background(), time.Now())
	if got := mock1.SessionProbesSnapshot(); got != 1 {
		t.Fatalf("p1 probes = %d, want 1", got)
	}
	if err := p1.FlushPoolPersist(); err != nil {
		t.Fatalf("FlushPoolPersist: %v", err)
	}

	mock2 := testutil.NewMock()
	defer mock2.Close()
	p2 := newTestPool(t, mock2)
	p2.SetPoolPersist(mem)
	setQuotaAutoProbe(p2, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p2.Start(ctx)

	// Start restored synchronously: warm view + timer before any tick.
	if got := len(p2.Snapshot()[0].QuotaByModel); got == 0 {
		t.Fatal("post-Start QuotaByModel empty, want the restored cache")
	}
	p2.smartProbe.mu.Lock()
	restoredLastProbe := p2.smartProbe.lastProbe
	p2.smartProbe.mu.Unlock()
	if restoredLastProbe.IsZero() {
		t.Fatal("post-Start lastProbe zero, want the restored timer")
	}

	// The maintain tick runs the boot round (zero probes on the warm
	// cache); the round completion re-arms the dirty flag, so the next
	// tick flushes the timer back to the store.
	savesBefore := mem.saveCount()
	p2.maintainTick(ctx)
	waitSmartProbeIdle(t, p2)
	if got := mock2.SessionProbesSnapshot(); got != 0 {
		t.Errorf("post-restart probes = %d, want 0 (warm cache)", got)
	}
	p2.maintainTick(ctx)
	if got := mem.saveCount(); got <= savesBefore {
		t.Errorf("store saves = %d, want > %d (tick flushes the timer)", got, savesBefore)
	}
}
