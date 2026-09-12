// quota_smartprobe_test.go — activity-aware adaptive prober coverage:
// tier classification, interval backoff, tier-driven cadences (active /
// warm / idle-once-then-sleep), the boot round, 429 abort + doubling +
// reset, per-round skips, probe-not-counted-as-usage, and the event
// triggers (remove kick, manual stamp, first-request-after-idle wake).
package pool

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// setQuotaAutoProbe flips the kill-switch live (the production live-apply
// path: atomic pointer swap, no pool rebuild).
func setQuotaAutoProbe(p *Pool, on bool) {
	cfg := p.cfg.Load()
	cfg.QuotaAutoProbe = on
	p.cfg.Store(cfg)
}

// waitSmartProbeIdle blocks until the dispatched round (if any) completes.
// Ticks dispatch asynchronously (single-flight off the maintain goroutine),
// so every firing tick must settle before the test asserts probe counts; a
// non-firing tick returns immediately.
func waitSmartProbeIdle(t *testing.T, p *Pool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for p.smartProbeInflight() {
		if time.Now().After(deadline) {
			t.Fatal("smart probe round still in flight after 10s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// probeTick runs one scheduler pass and settles the dispatched round.
func probeTick(t *testing.T, p *Pool, ctx context.Context, now time.Time) {
	t.Helper()
	p.smartProbeTickAt(ctx, now)
	waitSmartProbeIdle(t, p)
}

// markPoolActive sets the pool traffic timestamp (what a successful
// Acquire records); zero leaves the pool never-active.
func markPoolActive(p *Pool, at time.Time) {
	p.lastActiveMu.Lock()
	p.lastActive = at
	p.lastActiveMu.Unlock()
}

func smartBackoff(p *Pool) int {
	p.smartProbe.mu.Lock()
	defer p.smartProbe.mu.Unlock()
	return p.smartProbe.mult()
}

func setSmartLastProbe(p *Pool, at time.Time) {
	p.smartProbe.mu.Lock()
	p.smartProbe.lastProbe = at
	p.smartProbe.mu.Unlock()
}

func TestSmartProbeTierBoundaries(t *testing.T) {
	now := time.Now()
	if got := classifyQuotaProbeTier(time.Time{}, now); got != quotaTierIdle {
		t.Error("never-active pool: want IDLE (boot round still fires via Start)")
	}
	if got := classifyQuotaProbeTier(now, now); got != quotaTierActive {
		t.Error("traffic now: want ACTIVE")
	}
	if got := classifyQuotaProbeTier(now.Add(-119*time.Second), now); got != quotaTierActive {
		t.Error("traffic 119s ago: want ACTIVE")
	}
	if got := classifyQuotaProbeTier(now.Add(-121*time.Second), now); got != quotaTierWarm {
		t.Error("traffic 121s ago: want WARM")
	}
	if got := classifyQuotaProbeTier(now.Add(-14*time.Minute), now); got != quotaTierWarm {
		t.Error("traffic 14m ago: want WARM")
	}
	if got := classifyQuotaProbeTier(now.Add(-16*time.Minute), now); got != quotaTierIdle {
		t.Error("traffic 16m ago: want IDLE")
	}
}

func TestSmartProbeIntervalBackoff(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	cfg := p.cfg.Load()
	if got := quotaTierActive.baseInterval(cfg); got != time.Minute {
		t.Errorf("active base = %v, want 1m (zero-value Config normalizes to default)", got)
	}
	if got := quotaTierWarm.baseInterval(cfg); got != 5*time.Minute {
		t.Errorf("warm base = %v, want derived 5m", got)
	}
	if got := quotaTierIdle.baseInterval(cfg); got != 30*time.Minute {
		t.Errorf("idle base = %v, want 30m (zero-value Config normalizes to default)", got)
	}
	if got := effectiveQuotaProbeInterval(time.Minute, 1); got != time.Minute {
		t.Errorf("mult 1 = %v, want base", got)
	}
	if got := effectiveQuotaProbeInterval(time.Minute, 2); got != 2*time.Minute {
		t.Errorf("mult 2 = %v, want doubled", got)
	}
	if got := effectiveQuotaProbeInterval(time.Minute, 20); got != 30*time.Minute {
		t.Errorf("mult 20 = %v, want 30m cap", got)
	}
	if got := effectiveQuotaProbeInterval(30*time.Minute, 2); got != 30*time.Minute {
		t.Errorf("idle doubled = %v, want 30m cap", got)
	}
}

func TestSmartProbeActiveTierCadence(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	setQuotaAutoProbe(p, true)
	now := time.Now()
	markPoolActive(p, now)

	ctx := context.Background()
	probeTick(t, p, ctx, now)
	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Fatalf("SessionProbes = %d, want 1 (active tick probes)", got)
	}
	// 30s later the active interval has not elapsed: silence.
	probeTick(t, p, ctx, now.Add(30*time.Second))
	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Errorf("SessionProbes = %d after 30s re-tick, want 1 (60s cadence)", got)
	}
	// Past the interval the next tick probes again.
	probeTick(t, p, ctx, now.Add(61*time.Second))
	if got := mock.SessionProbesSnapshot(); got != 2 {
		t.Errorf("SessionProbes = %d after 61s tick, want 2 (interval elapsed)", got)
	}
}

func TestSmartProbeWarmTierCadence(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	setQuotaAutoProbe(p, true)
	now := time.Now()
	markPoolActive(p, now.Add(-5*time.Minute)) // WARM window

	ctx := context.Background()
	setSmartLastProbe(p, now.Add(-time.Minute))
	probeTick(t, p, ctx, now)
	if got := mock.SessionProbesSnapshot(); got != 0 {
		t.Fatalf("SessionProbes = %d, want 0 (warm probe 1m old, 5m cadence)", got)
	}
	setSmartLastProbe(p, now.Add(-6*time.Minute))
	probeTick(t, p, ctx, now)
	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Errorf("SessionProbes = %d, want 1 (warm interval elapsed)", got)
	}
}

func TestSmartProbeIdleOnceThenSilent(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	setQuotaAutoProbe(p, true)
	now := time.Now()
	markPoolActive(p, now.Add(-20*time.Minute)) // IDLE window

	ctx := context.Background()
	probeTick(t, p, ctx, now)
	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Fatalf("SessionProbes = %d, want 1 (idle single probe)", got)
	}
	// The next tick sleeps: no traffic, heartbeat far off.
	probeTick(t, p, ctx, now.Add(time.Minute))
	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Errorf("SessionProbes = %d after idle re-tick, want 1 (sleep until heartbeat)", got)
	}
	// Past the heartbeat the pool re-probes.
	probeTick(t, p, ctx, now.Add(31*time.Minute))
	if got := mock.SessionProbesSnapshot(); got != 2 {
		t.Errorf("SessionProbes = %d after heartbeat tick, want 2", got)
	}
}

func TestSmartProbeBootFires(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	setQuotaAutoProbe(p, true)
	now := time.Now()
	// Simulate Start without goroutines: a boot time with zero traffic.
	p.quotaBootAt = now.Add(-time.Hour)

	probeTick(t, p, context.Background(), now)
	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Fatalf("SessionProbes = %d, want 1 (boot round bypasses timer)", got)
	}
	// The boot round arms the idle sleep: the next quiet tick is silent.
	probeTick(t, p, context.Background(), now.Add(time.Minute))
	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Errorf("SessionProbes = %d after post-boot tick, want 1 (idle sleep)", got)
	}
}

func TestSmartProbeDisabledSkips(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	now := time.Now()
	// Kill-switch off (zero-value test Config): boot force and traffic
	// alike must not probe — pre-scheduler behavior.
	p.quotaBootAt = now.Add(-time.Hour)
	markPoolActive(p, now)
	probeTick(t, p, context.Background(), now)
	if got := mock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("SessionProbes = %d with kill-switch off, want 0", got)
	}
}

func TestSmartProbeBackoffOn429(t *testing.T) {
	mocks := make([]*testutil.MockUpstream, 5)
	for i := range mocks {
		mocks[i] = testutil.NewMock()
		defer mocks[i].Close()
	}
	p := newTestPool(t, mocks...)
	setQuotaAutoProbe(p, true)
	now := time.Now()
	markPoolActive(p, now)
	mocks[0].SetRateLimit(true)
	// The rest park behind a test-owned gate instead of answering: with
	// the gate closed they can only leave via the client's cancel, which
	// fires after token0's instant 429 records the abort — so the abort
	// deterministically spares the fifth token by construction, not by a
	// timing margin (jobs feed in roster order; only a finished job frees
	// a worker, and every finish happens-after the abort).
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
	for _, m := range mocks[1:] {
		m.SessionHandler = gatedProbe
	}

	ctx := context.Background()
	probeTick(t, p, ctx, now)
	// mock0 is always hit exactly once (nothing else can complete first:
	// the rest park behind the closed gate, so the abort always comes
	// from mock0). How many of the parked probes ARRIVED before the
	// abort is load-dependent — a worker descheduled past the abort
	// never dispatches its job — so mocks1-3 are 0-or-1 here, never more.
	if got := mocks[0].RequestsSnapshot(); got != 1 {
		t.Fatalf("token0 upstream hits = %d, want 1 (round reached it)", got)
	}
	for i, m := range mocks[1:4] {
		if got := m.RequestsSnapshot(); got > 1 {
			t.Fatalf("token %d upstream hits = %d, want <= 1 (abort spares the rest)", i+1, got)
		}
	}
	if got := mocks[4].RequestsSnapshot(); got != 0 {
		t.Errorf("token4 upstream hits = %d, want 0 (fifth job never frees a worker pre-abort)", got)
	}
	if got := smartBackoff(p); got != 2 {
		t.Errorf("backoff = %d, want 2 (doubled on 429)", got)
	}
	// The doubled active interval (2m) holds the next tick quiet.
	mocks[0].SetRateLimit(false)
	probeTick(t, p, ctx, now.Add(61*time.Second))
	if got := mocks[4].RequestsSnapshot(); got != 0 {
		t.Errorf("token4 upstream hits = %d after 61s tick, want 0 (backoff holds)", got)
	}
	before := make([]int, len(mocks))
	for i, m := range mocks {
		before[i] = m.RequestsSnapshot()
	}
	// Past the doubled interval the round completes and resets. Traffic is
	// still flowing (re-marked: without it the pool would have aged into
	// WARM, whose doubled 10m cadence correctly holds much longer). Every
	// token is stale again (quota older than the fresh window), so the
	// full roster probes. The gate opens first so every probe answers at
	// once — no timing involved anywhere in this test.
	close(gate)
	markPoolActive(p, now.Add(130*time.Second))
	probeTick(t, p, ctx, now.Add(130*time.Second))
	for i, m := range mocks {
		// The recovery round is abort-free, so every stale token is fed
		// and hit exactly once more (round 1 saved no quota anywhere:
		// the 429 and the canceled parks carry none).
		if got, want := m.RequestsSnapshot(), before[i]+1; got != want {
			t.Errorf("token %d upstream hits = %d, want %d (recovered full round)", i, got, want)
		}
	}
	if got := smartBackoff(p); got != 1 {
		t.Errorf("backoff = %d, want 1 (reset on 429-free round)", got)
	}
}

func TestSmartProbeSkipsUnhealthy(t *testing.T) {
	cases := map[string]func(t *testing.T, p *Pool){
		"locked": func(t *testing.T, p *Pool) {
			(*p.roster.Load())[0].locked.Store(true)
		},
		"cooling": func(t *testing.T, p *Pool) {
			t.Helper()
			p.CooldownTokenRateLimit(0, &upstream.RateLimitError{Body: "rate limit", RetryAfter: 10 * time.Minute})
		},
		"banned": func(t *testing.T, p *Pool) {
			t.Helper()
			p.CooldownTokenBan(0, &upstream.BanError{Body: "banned", ResumesAt: time.Now().Add(time.Hour)})
		},
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			mock0 := testutil.NewMock()
			defer mock0.Close()
			mock1 := testutil.NewMock()
			defer mock1.Close()
			p := newTestPool(t, mock0, mock1)
			setQuotaAutoProbe(p, true)
			now := time.Now()
			markPoolActive(p, now)
			bad(t, p)

			probeTick(t, p, context.Background(), now)
			if got := mock0.RequestsSnapshot(); got != 0 {
				t.Errorf("flagged token upstream hits = %d, want 0 (skipped)", got)
			}
			if got := mock1.SessionProbesSnapshot(); got != 1 {
				t.Errorf("healthy token probes = %d, want 1 (round still covers it)", got)
			}
		})
	}
}

func TestSmartProbeNotCountedAsUsage(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	setQuotaAutoProbe(p, true)
	now := time.Now()
	p.quotaBootAt = now.Add(-time.Hour)

	probeTick(t, p, context.Background(), now)
	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Fatalf("SessionProbes = %d, want 1 (probe fired)", got)
	}
	if got := p.usageCount(0); got != 0 {
		t.Errorf("usageCount = %d, want 0 (probes never touch the ledgers)", got)
	}
	if got := p.requestsServed.Load(); got != 0 {
		t.Errorf("requestsServed = %d, want 0 (probes are not client traffic)", got)
	}
	p.lastActiveMu.Lock()
	active := p.lastActive
	p.lastActiveMu.Unlock()
	if !active.IsZero() {
		t.Errorf("lastActive = %v, want zero (probes must not feed the tier classifier)", active)
	}
}

func TestSmartProbeRemoveKicks(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)
	setQuotaAutoProbe(p, true)
	now := time.Now()
	markPoolActive(p, now)

	ctx := context.Background()
	probeTick(t, p, ctx, now)
	if got := mock0.SessionProbesSnapshot() + mock1.SessionProbesSnapshot(); got != 2 {
		t.Fatalf("probes = %d, want 2 (one per token)", got)
	}
	if err := p.RemoveLastToken(); err != nil {
		t.Fatal(err)
	}
	// 10s later the active interval has not elapsed, but the membership
	// change bypasses the timer: the kicked round still runs, finds the
	// remaining token's quota fresh, and probes nothing new.
	probeTick(t, p, ctx, now.Add(10*time.Second))
	if got := mock0.SessionProbesSnapshot(); got != 1 {
		t.Errorf("remaining token probes = %d, want 1 (fresh quota skips the kicked round)", got)
	}
	// A newly added token has no quota data (stale): the next kicked
	// round probes exactly it and still spares the fresh member.
	mock2 := testutil.NewMock()
	defer mock2.Close()
	cfg := p.cfg.Load()
	cfg.UpstreamBaseURL = mock2.URL()
	p.cfg.Store(cfg)
	if _, err := p.AddToken("tok-new"); err != nil {
		t.Fatal(err)
	}
	probeTick(t, p, ctx, now.Add(20*time.Second))
	if got := mock0.SessionProbesSnapshot(); got != 1 {
		t.Errorf("remaining token probes = %d, want still 1 (fresh member spared)", got)
	}
	if got := mock2.SessionProbesSnapshot(); got != 1 {
		t.Errorf("new token probes = %d, want 1 (stale newcomer probed on kick)", got)
	}
}

func TestSmartProbeManualStamps(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	setQuotaAutoProbe(p, true)
	markPoolActive(p, time.Now())

	if out := p.ProbeAll(context.Background()); len(out) != 1 {
		t.Fatalf("ProbeAll outcomes = %d, want 1", len(out))
	}
	before := mock.SessionProbesSnapshot()
	// The manual pass restarts the scheduler timer: the next tick inside
	// the active interval stays quiet.
	probeTick(t, p, context.Background(), time.Now().Add(10*time.Second))
	if got := mock.SessionProbesSnapshot(); got != before {
		t.Errorf("SessionProbes = %d, want %d (manual pass counts as a round)", got, before)
	}
}

func TestSmartProbeFirstRequestAfterIdleWakes(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	setQuotaAutoProbe(p, true)
	// A slow active cadence proves the wake is the event trigger, not the
	// interval: 2m after the idle probe the 5m timer is nowhere near due.
	cfg := p.cfg.Load()
	cfg.QuotaProbeActiveInterval = 5 * time.Minute
	p.cfg.Store(cfg)

	now := time.Now()
	markPoolActive(p, now.Add(-20*time.Minute))
	ctx := context.Background()
	probeTick(t, p, ctx, now)
	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Fatalf("SessionProbes = %d, want 1 (idle single probe)", got)
	}
	// Traffic resumes a minute later; the next tick wakes past the timer.
	markPoolActive(p, now.Add(time.Minute))
	probeTick(t, p, ctx, now.Add(2*time.Minute))
	if got := mock.SessionProbesSnapshot(); got != 2 {
		t.Errorf("SessionProbes = %d, want 2 (first request after idle bypasses timer)", got)
	}
	// With traffic flowing the wake is spent: the cadence holds again.
	probeTick(t, p, ctx, now.Add(3*time.Minute))
	if got := mock.SessionProbesSnapshot(); got != 2 {
		t.Errorf("SessionProbes = %d after follow-up tick, want 2 (wake fires once)", got)
	}
}

func TestSmartProbeRidesMaintainTick(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	setQuotaAutoProbe(p, true)
	markPoolActive(p, time.Now())

	before := mock.SessionProbesSnapshot()
	p.maintainTick(context.Background())
	waitSmartProbeIdle(t, p)
	if got := mock.SessionProbesSnapshot(); got != before+1 {
		t.Fatalf("maintainTick probes delta = %d, want 1 (scheduler rides the tick)", got-before)
	}
	// The round stamps the timer: the immediate next pass is quiet.
	before = mock.SessionProbesSnapshot()
	p.maintainTick(context.Background())
	waitSmartProbeIdle(t, p)
	if got := mock.SessionProbesSnapshot(); got != before {
		t.Errorf("maintainTick probes delta = %d, want 0 (timer holds)", got-before)
	}
}
