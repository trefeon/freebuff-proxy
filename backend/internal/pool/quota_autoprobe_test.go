// quota_autoprobe_test.go — scheduler (ADR-0022) coverage: the pure slot/due
// helpers (determinism, window spread, day-gate, disabled-skip,
// unknown-reset-skip) plus pool wiring (fires once per Pacific day through
// the fake upstream, rides maintainTick, disabled restores pre-scheduler
// behavior). Existing acquire/probe tests are untouched.
package pool

import (
	"context"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// laNoon anchors an injected tick at Pacific midday so same-day re-ticks
// stay on one Pacific calendar day (no midnight flake).
func laNoon(t time.Time) time.Time {
	loc := maturityLocation("America/Los_Angeles")
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 12, 0, 0, 0, loc)
}

// dueResetAt returns a reset instant that makes token idx due at now: the
// slot trails now while the reset stays future. Deterministic (pure hash
// scan, no clock advance), so the pool tests pin wiring — not luck.
func dueResetAt(t *testing.T, idx int, now time.Time) time.Time {
	t.Helper()
	day := laDay(now)
	for d := range 120 {
		if reset := now.Add(time.Duration(d+1) * time.Minute); !now.Before(quotaAutoProbeSlot(idx, day, reset)) {
			return reset
		}
	}
	t.Fatal("no due reset found in 120m scan (hash spread failure)")
	return time.Time{}
}

// seedQuotaReset installs a known cached quota (as a manual probe would)
// with a single future reset.
func seedQuotaReset(t *testing.T, p *Pool, token int, reset time.Time) {
	t.Helper()
	toks := p.roster.Load()
	(*toks)[token].session.UpdateQuotaFromProbe(&upstream.SessionState{
		RateLimitsByModel: map[string]upstream.ModelQuota{
			modelA: {Model: modelA, Limit: 5, RecentCount: 1, ResetAt: reset, Period: "pacific_day"},
		},
	})
}

// setQuotaAutoProbe flips the kill-switch live (the production live-apply
// path: atomic pointer swap, no pool rebuild).
func setQuotaAutoProbe(p *Pool, on bool) {
	cfg := p.cfg.Load()
	cfg.QuotaAutoProbe = on
	p.cfg.Store(cfg)
}

func TestQuotaAutoProbeSlotDeterministic(t *testing.T) {
	reset := time.Date(2026, 9, 8, 7, 0, 0, 0, time.UTC)
	a := quotaAutoProbeSlot(0, "2026-09-07", reset)
	if b := quotaAutoProbeSlot(0, "2026-09-07", reset); !b.Equal(a) {
		t.Errorf("slot not deterministic: %v vs %v", a, b)
	}
	if b := quotaAutoProbeSlot(1, "2026-09-07", reset); b.Equal(a) {
		t.Errorf("slot identical for idx 0 and 1 (%v): no per-token spread", a)
	}
	if b := quotaAutoProbeSlot(0, "2026-09-08", reset); b.Equal(a) {
		t.Errorf("slot identical across Pacific days (%v): day input ignored", a)
	}
}

func TestQuotaAutoProbeSlotWithinWindowAndSpread(t *testing.T) {
	reset := time.Date(2026, 9, 8, 7, 0, 0, 0, time.UTC)
	start := reset.Add(-quotaAutoProbeWindow)
	seen := map[time.Time]bool{}
	for i := range 8 {
		s := quotaAutoProbeSlot(i, "2026-09-07", reset)
		if s.Before(start) || !s.Before(reset) {
			t.Errorf("token %d slot %v outside [reset-2h, reset) = [%v, %v)", i, s, start, reset)
		}
		seen[s] = true
	}
	if len(seen) < 2 {
		t.Error("8 tokens share one slot: no stagger, restarts would herd")
	}
}

func TestQuotaAutoProbeDueGates(t *testing.T) {
	tick := laNoon(time.Now())
	reset := dueResetAt(t, 0, tick)
	day := laDay(tick)
	if !quotaAutoProbeDue(true, 0, "", tick, reset) {
		t.Fatal("due case: want true")
	}
	if quotaAutoProbeDue(false, 0, "", tick, reset) {
		t.Error("disabled kill-switch: want false")
	}
	if quotaAutoProbeDue(true, 0, "", tick, time.Time{}) {
		t.Error("unknown (zero) reset: want false")
	}
	if quotaAutoProbeDue(true, 0, day, tick, reset) {
		t.Error("already probed today: want false")
	}
	early := tick.Add(-3 * time.Hour)
	if quotaAutoProbeDue(true, 0, "", early, early.Add(3*time.Hour)) {
		t.Error("slot still in the future: want false")
	}
}

func TestQuotaAutoProbeStaleResetCatchesUp(t *testing.T) {
	now := laNoon(time.Now())
	if !quotaAutoProbeDue(true, 0, "", now, now.Add(-time.Hour)) {
		t.Error("stale reset (window rolled unprobed): want catch-up probe")
	}
}

func TestQuotaAutoProbeResetEarliestFuture(t *testing.T) {
	now := time.Now()
	far := now.Add(5 * time.Hour)
	near := now.Add(30 * time.Minute)
	snap := session.SessionSnapshot{QuotaByModel: map[string]session.QuotaSnapshot{
		modelA:        {Model: modelA, Limit: 5, RecentCount: 1, ResetAt: far},
		modelB:        {Model: modelB, Limit: 5, RecentCount: 1, ResetAt: near},
		"stale/model": {Model: "stale/model", Limit: 5, RecentCount: 5, ResetAt: now.Add(-time.Hour)},
		"zero/model":  {Model: "zero/model", Limit: 5, RecentCount: 1},
	}}
	got, ok := quotaAutoProbeReset(snap, now)
	if !ok || !got.Equal(near) {
		t.Errorf("reset = %v,%v, want earliest future %v", got, ok, near)
	}
	if _, ok := quotaAutoProbeReset(session.SessionSnapshot{}, now); ok {
		t.Error("empty quota map: want unknown reset")
	}
	past := session.SessionSnapshot{QuotaByModel: map[string]session.QuotaSnapshot{
		modelA: {Model: modelA, Limit: 5, RecentCount: 5, ResetAt: now.Add(-time.Hour)},
	}}
	if _, ok := quotaAutoProbeReset(past, now); ok {
		t.Error("all resets past: want unknown reset (window rolled, manual probe seeds fresh)")
	}
}

func TestQuotaAutoProbeTickFiresOncePerDay(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	setQuotaAutoProbe(p, true)
	tick := laNoon(time.Now())
	seedQuotaReset(t, p, 0, dueResetAt(t, 0, tick))

	ctx := context.Background()
	p.quotaAutoProbeTickAt(ctx, tick)
	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Fatalf("SessionProbes = %d, want 1 (one session-less probe)", got)
	}
	toks := p.roster.Load()
	if got := (*toks)[0].quotaProbeDay; got != laDay(tick) {
		t.Errorf("lastProbeDay = %q, want %q", got, laDay(tick))
	}

	// Same Pacific day, later tick: day gate holds, no second probe.
	p.quotaAutoProbeTickAt(ctx, tick.Add(time.Hour))
	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Errorf("SessionProbes = %d after same-day re-tick, want 1 (day gate)", got)
	}

	// A stale day mark re-arms: the next due tick probes again.
	(*toks)[0].quotaProbeDay = "2000-01-01"
	tick2 := tick.Add(2 * time.Hour)
	seedQuotaReset(t, p, 0, dueResetAt(t, 0, tick2))
	p.quotaAutoProbeTickAt(ctx, tick2)
	if got := mock.SessionProbesSnapshot(); got != 2 {
		t.Errorf("SessionProbes = %d after new-day tick, want 2 (day rolled)", got)
	}
}

func TestQuotaAutoProbeTickDisabledSkips(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	// Zero-value test Config leaves the knob off until explicitly enabled,
	// so a due slot alone must not fire.
	tick := laNoon(time.Now())
	seedQuotaReset(t, p, 0, dueResetAt(t, 0, tick))
	p.quotaAutoProbeTickAt(context.Background(), tick)
	if got := mock.SessionProbesSnapshot(); got != 0 {
		t.Fatalf("SessionProbes = %d with kill-switch off, want 0 (pre-scheduler behavior)", got)
	}
	// Live-apply: flipping the knob on takes effect on the next pass.
	setQuotaAutoProbe(p, true)
	p.quotaAutoProbeTickAt(context.Background(), tick)
	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Errorf("SessionProbes = %d after enabling, want 1 (live-apply)", got)
	}
}

func TestQuotaAutoProbeTickUnknownResetSkips(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	setQuotaAutoProbe(p, true)
	// Never manually probed: no cached quota, no known reset.
	p.quotaAutoProbeTickAt(context.Background(), time.Now())
	if got := mock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("SessionProbes = %d with unknown reset, want 0 (first probe stays manual)", got)
	}
}

func TestQuotaAutoProbeRidesMaintainTick(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	setQuotaAutoProbe(p, true)
	seedQuotaReset(t, p, 0, dueResetAt(t, 0, time.Now()))

	before := mock.SessionProbesSnapshot()
	p.maintainTick(context.Background())
	if got := mock.SessionProbesSnapshot(); got != before+1 {
		t.Fatalf("maintainTick probes delta = %d, want 1 (scheduler rides the tick)", got-before)
	}

	// Disabled: the same pass issues no auto-probe — pre-scheduler behavior.
	setQuotaAutoProbe(p, false)
	toks := p.roster.Load()
	(*toks)[0].quotaProbeDay = ""
	seedQuotaReset(t, p, 0, dueResetAt(t, 0, time.Now()))
	before = mock.SessionProbesSnapshot()
	p.maintainTick(context.Background())
	if got := mock.SessionProbesSnapshot(); got != before {
		t.Errorf("maintainTick probes delta = %d with kill-switch off, want 0", got-before)
	}
}
