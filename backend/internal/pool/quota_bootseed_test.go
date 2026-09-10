// quota_bootseed_test.go — boot seed coverage: the seed fills the live
// view from persisted rows, drops bad rows, never downgrades fresher live
// data, and is idempotent.
package pool

import (
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// seededPool builds a one-token pool with a fresh (unprobed) manager.
func seededPool(t *testing.T, mock *testutil.MockUpstream) *Pool {
	t.Helper()
	return newTestPool(t, mock)
}

// seedLiveQuota installs live quota through the same UpdateQuotaFromProbe
// path a real probe uses.
func seedLiveQuota(t *testing.T, p *Pool, token int, reset time.Time) {
	t.Helper()
	toks := p.roster.Load()
	(*toks)[token].session.UpdateQuotaFromProbe(&upstream.SessionState{
		RateLimitsByModel: map[string]upstream.ModelQuota{
			modelA: {Model: modelA, Limit: 5, RecentCount: 1, ResetAt: reset, Period: "pacific_day"},
		},
	})
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
}

func TestQuotaBootSeedNoDowngrade(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := seededPool(t, mock)
	reset := time.Now().Add(3 * time.Hour)
	seedLiveQuota(t, p, 0, reset) // live probe: same path a real probe uses

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
