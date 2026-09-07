// quota_bootseed_test.go — ADR-0024 coverage: the boot seed fills the live
// view (and teaches the scheduler its reset), never downgrades fresher live
// data, and is idempotent; the recovery boot probe fires exactly once for
// virgin tokens and never for seeded or disabled ones. Existing
// quota_autoprobe_test.go cases are untouched.
package pool

import (
	"context"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/testutil"
)

// seededPool builds a one-token pool with a fresh (unprobed) manager.
func seededPool(t *testing.T, mock *testutil.MockUpstream) *Pool {
	t.Helper()
	return newTestPool(t, mock)
}

func TestQuotaBootSeedFillsViewAndReset(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := seededPool(t, mock)
	reset := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	probed := time.Now().Add(-time.Hour).Truncate(time.Second)

	p.SeedQuotaSnapshot([]QuotaSeedRow{{
		Token: 0, Model: modelA, Limit: 5, Recent: 2,
		ResetAt: reset, ProbedAt: probed,
		Entitlement: map[string]float64{"base": 5},
	}})

	snaps := p.Snapshot()
	q, ok := snaps[0].QuotaByModel[modelA]
	if !ok {
		t.Fatalf("seeded model missing from view: %+v", snaps[0].QuotaByModel)
	}
	if q.Limit != 5 || q.RecentCount != 2 || !q.ResetAt.Equal(reset) {
		t.Errorf("seeded quota = %+v, want limit=5 recent=2 reset=%v", q, reset)
	}
	if !snaps[0].QuotaStale {
		t.Error("QuotaStale = false, want true (seed is last-known)")
	}
	toks := p.roster.Load()
	if !(*toks)[0].quotaSeeded {
		t.Error("quotaSeeded = false, want true (row applied)")
	}
	// The scheduler learns the reset from the seed.
	got, ok := quotaAutoProbeReset(session.SessionSnapshot{QuotaByModel: snaps[0].QuotaByModel}, time.Now())
	if !ok || !got.Equal(reset) {
		t.Errorf("scheduler reset = %v,%v, want %v,true (seed teaches reset)", got, ok, reset)
	}
}

func TestQuotaBootSeedDropsBadRows(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := seededPool(t, mock)
	now := time.Now()
	p.SeedQuotaSnapshot([]QuotaSeedRow{
		{Token: 99, Model: modelA, Limit: 5, ProbedAt: now}, // unknown token
		{Token: 0, Model: "", Limit: 5, ProbedAt: now},      // empty model
		{Token: -1, Model: modelA, Limit: 5, ProbedAt: now}, // negative token
	})
	if len(p.Snapshot()[0].QuotaByModel) != 0 {
		t.Errorf("bad rows polluted the view: %+v", p.Snapshot()[0].QuotaByModel)
	}
	toks := p.roster.Load()
	if (*toks)[0].quotaSeeded {
		t.Error("quotaSeeded = true with zero applied rows, want false")
	}
}

func TestQuotaBootSeedNoDowngrade(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := seededPool(t, mock)
	reset := time.Now().Add(3 * time.Hour)
	seedQuotaReset(t, p, 0, reset) // live probe: same path a real probe uses

	stale := time.Now().Add(-24 * time.Hour)
	p.SeedQuotaSnapshot([]QuotaSeedRow{{
		Token: 0, Model: modelA, Limit: 5, Recent: 4,
		ResetAt: reset.Add(-time.Hour), ProbedAt: stale,
	}})
	q := p.Snapshot()[0].QuotaByModel[modelA]
	if q.RecentCount != 1 || !q.ResetAt.Equal(reset) {
		t.Errorf("live quota downgraded by stale seed: %+v", q)
	}
}

func TestQuotaBootSeedIdempotent(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := seededPool(t, mock)
	rows := []QuotaSeedRow{{
		Token: 0, Model: modelA, Limit: 5, Recent: 2,
		ResetAt: time.Now().Add(3 * time.Hour), ProbedAt: time.Now().Add(-time.Hour),
	}}
	p.SeedQuotaSnapshot(rows)
	before := p.Snapshot()[0].QuotaByModel[modelA]
	p.SeedQuotaSnapshot(rows)
	after := p.Snapshot()[0].QuotaByModel[modelA]
	if before.Limit != after.Limit || before.RecentCount != after.RecentCount ||
		!before.ResetAt.Equal(after.ResetAt) || before.Entitlement["base"] != after.Entitlement["base"] {
		t.Errorf("re-push changed the view: %+v vs %+v", before, after)
	}
}

func TestQuotaBootProbeSlotSpread(t *testing.T) {
	boot := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	end := boot.Add(quotaBootProbeWindow)
	seen := map[time.Time]bool{}
	for i := range 8 {
		s := quotaBootProbeSlot(i, boot)
		if s.Before(boot) || !s.Before(end) {
			t.Errorf("token %d slot %v outside [boot, boot+5m)", i, s)
		}
		if b := quotaBootProbeSlot(i, boot); !b.Equal(s) {
			t.Errorf("token %d slot not deterministic: %v vs %v", i, s, b)
		}
		seen[s] = true
	}
	if len(seen) < 2 {
		t.Error("all 8 token slots identical: no per-token spread")
	}
}

func TestQuotaBootProbeDueGates(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Hour) // every slot (boot+0..5m) is past
	if !quotaBootProbeDue(0, false, old, now) {
		t.Error("due case: want true (slot long past, never probed)")
	}
	if quotaBootProbeDue(0, true, old, now) {
		t.Error("already boot-probed: want false (once per process)")
	}
	if quotaBootProbeDue(0, false, time.Time{}, now) {
		t.Error("zero boot (pool never Started): want false")
	}
	if quotaBootProbeDue(0, false, now.Add(time.Hour), now) {
		t.Error("slot still in the future: want false")
	}
}

func TestQuotaBootProbeVirginFiresOnce(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := seededPool(t, mock)
	setQuotaAutoProbe(p, true)
	// Boot long ago: every staggered slot is due on the next tick.
	p.quotaBootAt = time.Now().Add(-time.Hour)
	tick := laNoon(time.Now())

	p.quotaAutoProbeTickAt(context.Background(), tick)
	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Fatalf("SessionProbes = %d, want 1 (one session-less boot probe)", got)
	}
	toks := p.roster.Load()
	if !(*toks)[0].quotaBootProbed {
		t.Error("quotaBootProbed = false, want true (fired)")
	}
	if got := (*toks)[0].quotaProbeDay; got != laDay(tick) {
		t.Errorf("quotaProbeDay = %q, want %q (day marked, no same-day double)", got, laDay(tick))
	}
	// Second tick: once per process, no second probe.
	p.quotaAutoProbeTickAt(context.Background(), tick.Add(time.Minute))
	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Errorf("SessionProbes = %d after re-tick, want 1 (once per process)", got)
	}
}

func TestQuotaBootProbeSeededSkips(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := seededPool(t, mock)
	setQuotaAutoProbe(p, true)
	// Seed with a reset whose slot stays future (offset >= 0 puts the slot
	// at/after reset-2h = now+1h): known reset, nothing due.
	p.SeedQuotaSnapshot([]QuotaSeedRow{{
		Token: 0, Model: modelA, Limit: 5, Recent: 1,
		ResetAt: time.Now().Add(3 * time.Hour), ProbedAt: time.Now(),
	}})
	p.quotaBootAt = time.Now().Add(-time.Hour)

	p.quotaAutoProbeTickAt(context.Background(), laNoon(time.Now()))
	if got := mock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("SessionProbes = %d for seeded token, want 0 (normal slots own it)", got)
	}
}

func TestQuotaBootProbeDisabledSkips(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := seededPool(t, mock)
	// Kill-switch off (zero-value test config): virgin token, due slot.
	p.quotaBootAt = time.Now().Add(-time.Hour)

	p.quotaAutoProbeTickAt(context.Background(), laNoon(time.Now()))
	if got := mock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("SessionProbes = %d with kill-switch off, want 0 (seed display only)", got)
	}
}

func TestQuotaBootProbeUnstartedSkips(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := seededPool(t, mock)
	setQuotaAutoProbe(p, true)
	// quotaBootAt zero: pool never Started — no boot probe, exactly the
	// pre-ADR-0024 unknown-reset behavior.
	p.quotaAutoProbeTickAt(context.Background(), laNoon(time.Now()))
	if got := mock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("SessionProbes = %d with zero boot time, want 0", got)
	}
}
