// burst_test.go — burst load-balance (ADR-0023) coverage: disabled
// byte-identical selection, trip + per-model isolation, max-tokens cap,
// window-expiry recovery, grant-path wiring, and maintain-tick pruning.
// Existing acquire tests are untouched; trip/recovery use direct window
// seeding (burstRecordAt/burstPruneAt) so no clock advance is needed.
package pool

import (
	"context"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// setBurst flips the burst knobs live (the production live-apply path:
// atomic pointer swap, no pool rebuild).
func setBurst(p *Pool, mut func(*config.Config)) {
	cfg := p.cfg.Load()
	mut(cfg)
	p.cfg.Store(cfg)
}

// newBurstPool wires a 3-token pool with the burst kill-switch on and a
// small threshold so tests trip deterministically.
func newBurstPool(t *testing.T, mocks ...*testutil.MockUpstream) *Pool {
	t.Helper()
	return newTestPoolCfg(t, func(c *config.Config) {
		c.BurstBalanceEnabled = true
		c.BurstWindow = time.Minute
		c.BurstThreshold = 2
		c.BurstMaxTokens = 2
	}, mocks...)
}

// seedBurstQuota installs a known session quota (as a manual probe would)
// with remaining = Limit - RecentCount and a future reset, so least_used
// ranks deterministically.
func seedBurstQuota(t *testing.T, p *Pool, token, remaining int, reset time.Time) {
	t.Helper()
	toks := p.roster.Load()
	(*toks)[token].session.UpdateQuotaFromProbe(&upstream.SessionState{
		RateLimitsByModel: map[string]upstream.ModelQuota{
			modelA: {Model: modelA, Limit: 5, RecentCount: float64(5 - remaining), ResetAt: reset, Period: "pacific_day"},
		},
	})
}

func seedBurstQuotas(t *testing.T, p *Pool, remaining ...int) {
	t.Helper()
	reset := time.Now().Add(time.Hour)
	for i, rem := range remaining {
		seedBurstQuota(t, p, i, rem, reset)
	}
}

func equalOrder(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestBurstDisabledByteIdentical pins the ADR-0023 default-off contract: a
// disabled pool records no window state and selects exactly like an
// enabled-but-steady pool across starts, quota shapes, and cooldowns.
func TestBurstDisabledByteIdentical(t *testing.T) {
	mocks := []*testutil.MockUpstream{testutil.NewMock(), testutil.NewMock(), testutil.NewMock()}
	for _, m := range mocks {
		defer m.Close()
	}
	off := newTestPool(t, mocks...)
	toks := off.roster.Load()
	seedBurstQuotas(t, off, 1, 3, 4)
	// One token cooling: exercises the availability-demotion path too.
	(*toks)[0].runs.CooldownRateLimit(&upstream.RateLimitError{
		Status: "rate_limited", Model: modelA, RetryAfter: 15 * time.Minute,
	})

	// Recording while disabled must keep zero burst state.
	now := time.Now()
	for range 10 {
		off.burstRecordAt(modelA, 0, now)
	}
	if len(off.burstHits) != 0 {
		t.Fatalf("disabled burstHits = %d models, want 0 (no state while off)", len(off.burstHits))
	}

	steady := newTestPoolCfg(t, func(c *config.Config) {
		c.BurstBalanceEnabled = true
		c.BurstWindow = time.Minute
		c.BurstThreshold = 1000
		c.BurstMaxTokens = 2
	}, mocks...)
	stoks := steady.roster.Load()
	seedBurstQuotas(t, steady, 1, 3, 4)
	(*stoks)[0].runs.CooldownRateLimit(&upstream.RateLimitError{
		Status: "rate_limited", Model: modelA, RetryAfter: 15 * time.Minute,
	})
	// Below-threshold admissions: still steady.
	for range 10 {
		steady.burstRecordAt(modelA, 0, now)
	}

	for _, start := range []int{0, 1, 2} {
		want, wantLimited := off.acquireOrder(toks, start, modelA)
		got, gotLimited := steady.acquireOrder(stoks, start, modelA)
		if !equalOrder(got, want) {
			t.Errorf("start %d: enabled-steady order = %v, want disabled order %v", start, got, want)
		}
		if len(gotLimited) != len(wantLimited) {
			t.Errorf("start %d: enabled-steady quotaLimited = %d, want %d", start, len(gotLimited), len(wantLimited))
		}
	}
}

// TestBurstTripSwitchesOrder pins the trip: once same-model admissions
// exceed the threshold, THAT model's order switches from drain to capped
// least_used, while other models keep the configured strategy.
func TestBurstTripSwitchesOrder(t *testing.T) {
	mocks := []*testutil.MockUpstream{testutil.NewMock(), testutil.NewMock(), testutil.NewMock()}
	for _, m := range mocks {
		defer m.Close()
	}
	p := newBurstPool(t, mocks...)
	toks := p.roster.Load()
	// Least_used ranks tok2 (rem 4) > tok1 (rem 3) > tok0 (rem 1).
	seedBurstQuotas(t, p, 1, 3, 4)

	// Steady: cold drain from the round-robin start.
	before, _ := p.acquireOrder(toks, 0, modelA)
	if !equalOrder(before, []int{0, 1, 2}) {
		t.Fatalf("steady order = %v, want [0 1 2] (cold drain)", before)
	}

	now := time.Now()
	p.burstRecordAt(modelA, 0, now)
	p.burstRecordAt(modelA, 0, now)
	if plan := p.burstPlanForModel(p.cfg.Load(), modelA, now); plan.active {
		t.Fatal("burst active at threshold (2/2), want steady until EXCEEDED")
	}
	p.burstRecordAt(modelA, 0, now)
	if !p.burstOn[modelA] {
		t.Fatal("burstOn = false after 3 admissions over threshold 2, want entry edge")
	}

	// Tripped: least_used [2 1 0], spread set {0} → preferred [0] + one
	// newcomer [2], over-cap [1] demoted last.
	got, _ := p.acquireOrder(toks, 0, modelA)
	if !equalOrder(got, []int{0, 2, 1}) {
		t.Errorf("burst order = %v, want [0 2 1] (member + capped newcomer + demoted)", got)
	}

	// Per-model isolation: modelB never tripped.
	if plan := p.burstPlanForModel(p.cfg.Load(), modelB, now); plan.active {
		t.Error("modelB burst active while only modelA tripped, want per-model isolation")
	}
}

// TestBurstMaxTokensCap pins the spread cap: once the window's distinct
// tokens fill BURST_MAX_TOKENS, further accounts stay demoted.
func TestBurstMaxTokensCap(t *testing.T) {
	mocks := []*testutil.MockUpstream{testutil.NewMock(), testutil.NewMock(), testutil.NewMock()}
	for _, m := range mocks {
		defer m.Close()
	}
	p := newBurstPool(t, mocks...)
	toks := p.roster.Load()
	seedBurstQuotas(t, p, 1, 3, 4)

	now := time.Now()
	for range 3 {
		p.burstRecordAt(modelA, 0, now)
	}
	// A grant to the newcomer fills the spread set {0, 2}.
	p.burstRecordAt(modelA, 2, now)

	got, _ := p.acquireOrder(toks, 0, modelA)
	if !equalOrder(got, []int{2, 0, 1}) {
		t.Fatalf("capped burst order = %v, want [2 0 1] (full set first, tok1 demoted)", got)
	}
	// Sustained hammering on the spread set never promotes the third account.
	for i := range 10 {
		p.burstRecordAt(modelA, []int{0, 2}[i%2], now)
	}
	got, _ = p.acquireOrder(toks, 0, modelA)
	if got[len(got)-1] != 1 {
		t.Errorf("capped burst order = %v, want token 1 demoted last", got)
	}
}

// TestBurstWindowExpiryRecovery pins the recovery: once the window slides
// past the burst (pruned on the maintain-tick path), selection returns to
// the configured strategy and the episode flag clears.
func TestBurstWindowExpiryRecovery(t *testing.T) {
	mocks := []*testutil.MockUpstream{testutil.NewMock(), testutil.NewMock(), testutil.NewMock()}
	for _, m := range mocks {
		defer m.Close()
	}
	p := newBurstPool(t, mocks...)
	toks := p.roster.Load()
	seedBurstQuotas(t, p, 1, 3, 4)

	now := time.Now()
	for range 3 {
		p.burstRecordAt(modelA, 0, now)
	}
	if !p.burstOn[modelA] {
		t.Fatal("burstOn = false after trip, want entry edge")
	}

	p.burstPruneAt(now.Add(time.Minute + time.Second))
	if p.burstOn[modelA] {
		t.Error("burstOn = true after the window expired, want exit edge")
	}
	if len(p.burstHits) != 0 {
		t.Errorf("burstHits retains %d models after expiry, want pruned", len(p.burstHits))
	}
	got, _ := p.acquireOrder(toks, 0, modelA)
	if !equalOrder(got, []int{0, 1, 2}) {
		t.Errorf("recovered order = %v, want [0 1 2] (cold drain again)", got)
	}
}

// TestBurstGrantsDriveTrip wires the record site to real Acquires: two
// grants over threshold 1 engage the burst, and the next order shows the
// capped least_used switch (drain would serve [0 1 2]).
func TestBurstGrantsDriveTrip(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mock2 := testutil.NewMock()
	defer mock2.Close()
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.BurstBalanceEnabled = true
		c.BurstWindow = time.Minute
		c.BurstThreshold = 1
		c.BurstMaxTokens = 2
	}, mock0, mock1, mock2)

	ctx := context.Background()
	for i := range 2 {
		lease, err := p.Acquire(ctx, modelA)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		if lease.Token != 0 {
			t.Fatalf("acquire %d landed token %d, want 0 (drain sticks to first)", i, lease.Token)
		}
		p.LeaseRelease(lease)
	}
	if !p.burstOn[modelA] {
		t.Fatal("burstOn = false after 2 grants over threshold 1, want grant-path recording")
	}

	// Seed differing quotas AFTER the grants (a live admission would
	// refresh them from upstream): tok2 fullest, tok0 thinnest.
	seedBurstQuotas(t, p, 1, 2, 4)
	toks := p.roster.Load()
	got, _ := p.acquireOrder(toks, 0, modelA)
	if !equalOrder(got, []int{0, 2, 1}) {
		t.Errorf("post-grant burst order = %v, want [0 2 1] (member + capped newcomer + demoted)", got)
	}

	// The next real grant still lands the preferred member.
	lease, err := p.Acquire(ctx, modelA)
	if err != nil {
		t.Fatalf("burst acquire: %v", err)
	}
	if lease.Token != 0 {
		t.Errorf("burst acquire landed token %d, want 0 (spread-set member first)", lease.Token)
	}
	p.LeaseRelease(lease)
}

// TestBurstPruneRidesMaintainTick pins the ADR-0023 prune site: one
// maintain pass drops expired window hits (no upstream traffic asserted —
// the pass is shared with maturity/autoprobe, covered by their tests).
func TestBurstPruneRidesMaintainTick(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBurstPool(t, mock)

	stale := time.Now().Add(-2 * time.Minute)
	p.burstRecordAt(modelA, 0, stale)
	if len(p.burstHits[modelA]) != 1 {
		t.Fatalf("burstHits = %d, want 1 staged stale hit", len(p.burstHits[modelA]))
	}
	p.maintainTick(context.Background())
	if len(p.burstHits) != 0 {
		t.Errorf("burstHits retains %d models after maintainTick, want expired hits pruned", len(p.burstHits))
	}
	if p.burstOn[modelA] {
		t.Error("burstOn = true for a never-tripped stale window, want false")
	}
}

// TestBurstLiveDisableRestoresDrain pins live-apply off: flipping the
// kill-switch mid-burst returns selection to the configured strategy on
// the next pass (the episode flag clears on the following prune).
func TestBurstLiveDisableRestoresDrain(t *testing.T) {
	mocks := []*testutil.MockUpstream{testutil.NewMock(), testutil.NewMock(), testutil.NewMock()}
	for _, m := range mocks {
		defer m.Close()
	}
	p := newBurstPool(t, mocks...)
	toks := p.roster.Load()
	seedBurstQuotas(t, p, 1, 3, 4)

	now := time.Now()
	for range 3 {
		p.burstRecordAt(modelA, 0, now)
	}
	if !p.burstOn[modelA] {
		t.Fatal("precondition: burst not engaged")
	}

	setBurst(p, func(c *config.Config) { c.BurstBalanceEnabled = false })
	got, _ := p.acquireOrder(toks, 0, modelA)
	if !equalOrder(got, []int{0, 1, 2}) {
		t.Errorf("disabled order = %v, want [0 1 2] (drain restored immediately)", got)
	}
	p.burstPruneAt(now.Add(time.Minute + time.Second))
	if p.burstOn[modelA] {
		t.Error("burstOn = true after disable + prune, want exit edge")
	}
}
