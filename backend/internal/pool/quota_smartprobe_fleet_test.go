// quota_smartprobe_fleet_test.go — issue #484 fleet-overload coverage:
// maintain never blocks on a stalled round, single-flight suppression
// (with kick preservation), overload abort + sparse re-burst damping,
// fresh-roster no-op rounds, warn-only failures, and Shutdown canceling a
// wedged round.
package pool

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
)

// stallProbeHandler blocks token-level probe GETs on release and serves
// everything else instantly. entries counts blocked probe entries. The
// caller must arrange release (returned releaser is idempotent and also
// wired into t.Cleanup).
func stallProbeHandler(t *testing.T, mock *testutil.MockUpstream, release chan struct{}, entries *atomic.Int64) (releaser func()) {
	t.Helper()
	var once sync.Once
	releaser = func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaser)
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.Header.Get("x-freebuff-instance-id") == "" {
			entries.Add(1)
			<-release
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"","rateLimitsByModel":{"deepseek/deepseek-v4-flash":{"model":"deepseek/deepseek-v4-flash","limit":6,"recentCount":1}}}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}
	return releaser
}

// awaitProbeEntries blocks until n probe entries landed (the round is
// provably in flight) or fails the test.
func awaitProbeEntries(t *testing.T, entries *atomic.Int64, n int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for entries.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("probe entries = %d, want %d (round never reached upstream)", entries.Load(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func smartKick(p *Pool) bool {
	p.smartProbe.mu.Lock()
	defer p.smartProbe.mu.Unlock()
	return p.smartProbe.kick
}

func TestSmartProbeMaintainAdvancesDuringStalledRound(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var entries atomic.Int64
	release := make(chan struct{})
	releaser := stallProbeHandler(t, mock, release, &entries)
	p := newTestPool(t, mock)
	setQuotaAutoProbe(p, true)
	markPoolActive(p, time.Now())

	// The dispatching tick returns at once; the round wedges upstream.
	p.smartProbeTickAt(context.Background(), time.Now())
	awaitProbeEntries(t, &entries, 1)

	// maintainTick must advance while the round is stalled, not block
	// behind it (the ~10min fleet round of issue #484).
	done := make(chan struct{})
	go func() {
		p.maintainTick(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("maintainTick blocked on a stalled probe round")
	}
	// A tick during the stalled round suppresses a second dispatch: give
	// a would-be round time to reach upstream, then release and settle.
	p.smartProbeTickAt(context.Background(), time.Now())
	time.Sleep(200 * time.Millisecond)
	if got := entries.Load(); got != 1 {
		t.Errorf("probe entries during stall = %d, want 1 (single-flight suppresses)", got)
	}
	releaser()
	waitSmartProbeIdle(t, p)
	if got := entries.Load(); got != 1 {
		t.Errorf("probe entries after release = %d, want 1 (exactly one round ran)", got)
	}
}

func TestSmartProbeSingleFlightPreservesKick(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var entries atomic.Int64
	release := make(chan struct{})
	releaser := stallProbeHandler(t, mock, release, &entries)
	p := newTestPool(t, mock)
	setQuotaAutoProbe(p, true)
	markPoolActive(p, time.Now())

	p.smartProbeTickAt(context.Background(), time.Now())
	awaitProbeEntries(t, &entries, 1)
	// A membership kick landing mid-round must survive the suppressed
	// tick: the new member is not in this round's snapshot.
	p.smartProbeKick()
	p.smartProbeTickAt(context.Background(), time.Now())
	if !smartKick(p) {
		t.Error("suppressed tick consumed the kick, want it preserved for the next tick")
	}
	releaser()
	waitSmartProbeIdle(t, p)
	if got := entries.Load(); got != 1 {
		t.Errorf("probe entries = %d, want 1 (no overlapping round)", got)
	}
	// The preserved kick fires the next tick (a no-op: the round just
	// refreshed the only token) and is consumed by it.
	p.smartProbeTickAt(context.Background(), time.Now())
	waitSmartProbeIdle(t, p)
	if smartKick(p) {
		t.Error("kick still set after the post-round tick, want consumed")
	}
	if got := entries.Load(); got != 1 {
		t.Errorf("probe entries after kick tick = %d, want 1 (fresh roster, no re-probe)", got)
	}
}

func TestSmartProbeOverloadAbortSparseReburst(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mock2 := testutil.NewMock()
	defer mock2.Close()
	// token0 hits a saturated upstream instantly; the rest park behind a
	// test-owned gate, so the abort deterministically lands first by
	// construction, not by a timing margin (they can only leave via the
	// client's post-abort cancel while the gate stays closed).
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"service_overloaded","message":"upstream saturated, try again later"}`)
	}
	gate := make(chan struct{})
	gatedProbe := func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"","rateLimitsByModel":{"deepseek/deepseek-v4-flash":{"model":"deepseek/deepseek-v4-flash","limit":6,"recentCount":1}}}`)
	}
	mock1.SessionHandler = gatedProbe
	mock2.SessionHandler = gatedProbe
	p := newTestPool(t, mock0, mock1, mock2)
	setQuotaAutoProbe(p, true)
	now := time.Now()
	markPoolActive(p, now)

	ctx := context.Background()
	probeTick(t, p, ctx, now)
	// mock0 is always hit exactly once (nothing else can complete first:
	// the rest park behind the closed gate, so the abort always comes
	// from mock0). Parked-probe arrival before the abort is
	// load-dependent, so mocks1-2 are 0-or-1 here, never more.
	if got := mock0.RequestsSnapshot(); got != 1 {
		t.Fatalf("token0 upstream hits = %d, want 1 (round reached it)", got)
	}
	for i, m := range []*testutil.MockUpstream{mock1, mock2} {
		if got := m.RequestsSnapshot(); got > 1 {
			t.Fatalf("token %d upstream hits = %d, want <= 1 (abort spares the rest)", i+1, got)
		}
	}
	if got := smartBackoff(p); got != 2 {
		t.Fatalf("backoff = %d, want 2 (doubled on overload abort)", got)
	}
	p.smartProbe.mu.Lock()
	overloadedAt := p.smartProbe.overloadedAt
	p.smartProbe.mu.Unlock()
	if overloadedAt.IsZero() {
		t.Fatal("overloadedAt zero after 503 abort, want the abort recorded")
	}
	// The fleet recovers; the kicked round inside the sparse window must
	// sample at most the sparse cap instead of re-bursting the roster.
	// The gate opens first so the canary probes answer at once.
	mock0.SessionHandler = nil
	close(gate)
	before := [...]int{mock0.RequestsSnapshot(), mock1.RequestsSnapshot(), mock2.RequestsSnapshot()}
	p.smartProbeKick()
	probeTick(t, p, ctx, now.Add(30*time.Second))
	if got := mock2.RequestsSnapshot(); got != before[2] {
		t.Errorf("token2 upstream hits = %d, want %d (sparse round spares it)", got, before[2])
	}
	// Eligible is all three (round 1 saved quota nowhere), sparse-capped
	// to the first two in roster order; both serve clean.
	if got, want := mock0.RequestsSnapshot()+mock1.RequestsSnapshot()+mock2.RequestsSnapshot(), before[0]+before[1]+before[2]+2; got != want {
		t.Errorf("roster upstream hits = %d, want %d (sparse canary of exactly 2)", got, want)
	}
	// A sparse canary sample is not fleet-health evidence: the backoff
	// survives the clean sparse round.
	if got := smartBackoff(p); got != 2 {
		t.Errorf("backoff = %d, want 2 (sparse round never resets)", got)
	}
}

func TestSmartProbeFreshRosterNoop(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)
	setQuotaAutoProbe(p, true)
	markPoolActive(p, time.Now())

	// The manual pass refreshes every token and restarts the scheduler
	// timer; the kicked tick then runs a no-op round (cheap, no probes).
	if out := p.ProbeAll(context.Background()); len(out) != 2 {
		t.Fatalf("ProbeAll outcomes = %d, want 2", len(out))
	}
	before := mock0.SessionProbesSnapshot() + mock1.SessionProbesSnapshot()
	p.smartProbeKick()
	probeTick(t, p, context.Background(), time.Now())
	if got := mock0.SessionProbesSnapshot() + mock1.SessionProbesSnapshot(); got != before {
		t.Errorf("probes after kicked tick = %d, want %d (fully-fresh roster short-circuits)", got, before)
	}
}

func TestSmartProbeFailuresKeepHealth(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)
	setQuotaAutoProbe(p, true)
	markPoolActive(p, time.Now())
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	}

	ctx := context.Background()
	probeTick(t, p, ctx, time.Now())
	if got := mock0.RequestsSnapshot(); got != 1 {
		t.Fatalf("failing token upstream hits = %d, want 1 (round reached it)", got)
	}
	// The round continues past a warn-only failure.
	if got := mock1.SessionProbesSnapshot(); got != 1 {
		t.Errorf("healthy token probes = %d, want 1 (failure does not abort the round)", got)
	}
	// A probe failure never mutates account health.
	tok0 := (*p.roster.Load())[0]
	if tok0.quarantine.Load() != nil {
		t.Error("failing token quarantined by a probe, want health untouched")
	}
	rs := tok0.runs.Snapshot()
	if rs.BanError != nil {
		t.Errorf("failing token BanError = %v, want nil (probes never ban)", rs.BanError)
	}
	if !rs.CooldownUntil.IsZero() {
		t.Errorf("failing token CooldownUntil = %v, want zero (probes never cool down)", rs.CooldownUntil)
	}
	if got := smartBackoff(p); got != 1 {
		t.Errorf("backoff = %d, want 1 (generic failure is not a 429)", got)
	}
}

func TestSmartProbeShutdownCancelsStalledRound(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var entries atomic.Int64
	release := make(chan struct{})
	releaser := stallProbeHandler(t, mock, release, &entries)
	p := newTestPool(t, mock)
	setQuotaAutoProbe(p, true)
	markPoolActive(p, time.Now())

	p.smartProbeTickAt(context.Background(), time.Now())
	awaitProbeEntries(t, &entries, 1)
	// Shutdown cancels the wedged round and waits it out instead of
	// hanging behind it.
	done := make(chan struct{})
	go func() {
		p.Shutdown(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Shutdown hung on a stalled probe round")
	}
	// Unblock the server-side handler before the deferred mock.Close:
	// Close waits for outstanding handlers, which still park on release
	// (the client already went away with the round cancel).
	releaser()
	if p.smartProbeInflight() {
		t.Error("probe round still in flight after Shutdown, want it canceled and waited")
	}
}
