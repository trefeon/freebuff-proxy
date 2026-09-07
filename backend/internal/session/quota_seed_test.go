package session

import (
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// seedRow builds a live-shaped quota sample for the seed tests.
func seedRow(model string, limit, recent float64, reset time.Time) upstream.ModelQuota {
	return upstream.ModelQuota{
		Model:       model,
		Limit:       limit,
		RecentCount: recent,
		ResetAt:     reset,
		Period:      "pacific_day",
		Entitlement: map[string]float64{"base": limit},
	}
}

func TestSeedQuotaFillsEmptyView(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)
	reset := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	probed := time.Now().Add(-time.Hour).Truncate(time.Second)

	if !mgr.SeedQuota(seedRow("mimo/mimo-v2.5", 5, 2, reset), probed) {
		t.Fatal("SeedQuota on empty manager = false, want true (applied)")
	}
	snap := mgr.Snapshot()
	q, ok := snap.QuotaByModel["mimo/mimo-v2.5"]
	if !ok {
		t.Fatalf("seeded model missing from view: %+v", snap.QuotaByModel)
	}
	if q.Limit != 5 || q.RecentCount != 2 || !q.ResetAt.Equal(reset) {
		t.Errorf("seeded quota = %+v, want limit=5 recent=2 reset=%v", q, reset)
	}
	if q.Entitlement["base"] != 5 {
		t.Errorf("seeded entitlement = %v, want base=5", q.Entitlement)
	}
	if !snap.QuotaStale {
		t.Error("QuotaStale = false, want true (seed is last-known, not live)")
	}
	if !snap.QuotaSavedAt.Equal(probed) {
		t.Errorf("QuotaSavedAt = %v, want probe time %v", snap.QuotaSavedAt, probed)
	}
}

func TestSeedQuotaRejects(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)
	now := time.Now()
	if mgr.SeedQuota(seedRow("", 5, 1, now), now) {
		t.Error("SeedQuota empty model = true, want false")
	}
	if mgr.SeedQuota(seedRow("m", 5, 1, now), time.Time{}) {
		t.Error("SeedQuota zero probe time = true, want false")
	}
	if len(mgr.Snapshot().QuotaByModel) != 0 {
		t.Error("rejected seeds polluted the view")
	}
}

func TestSeedQuotaNeverDowngradesLiveProbe(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)
	reset := time.Now().Add(3 * time.Hour)
	// Live probe lands first (the same path a real probe uses).
	mgr.UpdateQuotaFromProbe(&upstream.SessionState{
		RateLimitsByModel: map[string]upstream.ModelQuota{
			"mimo/mimo-v2.5": seedRow("mimo/mimo-v2.5", 5, 1, reset),
		},
	})
	// Stale persisted row must not clobber it.
	stale := time.Now().Add(-24 * time.Hour)
	if mgr.SeedQuota(seedRow("mimo/mimo-v2.5", 5, 4, reset.Add(-time.Hour)), stale) {
		t.Error("stale SeedQuota over live probe = true, want false")
	}
	q := mgr.Snapshot().QuotaByModel["mimo/mimo-v2.5"]
	if q.RecentCount != 1 || !q.ResetAt.Equal(reset) {
		t.Errorf("live quota downgraded by stale seed: %+v", q)
	}
	if mgr.Snapshot().QuotaStale {
		t.Error("QuotaStale = true after live probe, want false")
	}
}

func TestSeedQuotaNewerWinsAndIdempotent(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)
	reset := time.Now().Add(3 * time.Hour)
	old := time.Now().Add(-48 * time.Hour)
	newer := time.Now().Add(-time.Hour)

	if !mgr.SeedQuota(seedRow("m", 5, 4, reset), old) {
		t.Fatal("first seed = false, want true")
	}
	if !mgr.SeedQuota(seedRow("m", 5, 1, reset.Add(time.Hour)), newer) {
		t.Error("newer seed = false, want true (newer wins)")
	}
	if got := mgr.Snapshot().QuotaByModel["m"].RecentCount; got != 1 {
		t.Errorf("recent after newer seed = %v, want 1", got)
	}
	// Re-pushing the same row is a no-op.
	if mgr.SeedQuota(seedRow("m", 5, 1, reset.Add(time.Hour)), newer) {
		t.Error("identical re-push = true, want false (idempotent)")
	}
	if got := mgr.Snapshot().QuotaByModel["m"].RecentCount; got != 1 {
		t.Errorf("recent after re-push = %v, want 1", got)
	}
}

func TestSeedQuotaLiveStateOwnsView(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)
	mgr.SetSessionStateForTest("active", "inst-1", "m", time.Now().Add(time.Hour), time.Now().Add(2*time.Hour))
	if mgr.SeedQuota(seedRow("m", 5, 1, time.Now()), time.Now()) {
		t.Error("SeedQuota with live state = true, want false (live owns the view)")
	}
}

func TestSeedQuotaRespectsDiskRestoreTime(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	store := NewStore(t.TempDir() + "/state.json")
	_, key := newPersistTestManager(t, mock, store)
	pollAt := time.Now().Add(-90 * time.Minute).Truncate(time.Second)
	store.Save(key, quotaSlot(pollAt))

	mgr2, _ := newPersistTestManager(t, mock, store)
	_ = mgr2.Snapshot() // lazy restore stamps the poll time baseline
	// A persisted row older than the restore poll must not win.
	if mgr2.SeedQuota(seedRow("mimo/mimo-v2.5", 5, 9, pollAt.Add(time.Hour)), pollAt.Add(-time.Hour)) {
		t.Error("seed older than disk restore = true, want false")
	}
	if got := mgr2.Snapshot().QuotaByModel["mimo/mimo-v2.5"].RecentCount; got != 2 {
		t.Errorf("restored recent = %v, want 2 (seed must not downgrade restore)", got)
	}
	// A newer row upgrades the restore.
	freshReset := time.Now().Add(5 * time.Hour).Truncate(time.Second)
	if !mgr2.SeedQuota(seedRow("mimo/mimo-v2.5", 5, 3, freshReset), pollAt.Add(time.Hour)) {
		t.Error("seed newer than disk restore = false, want true")
	}
	if got := mgr2.Snapshot().QuotaByModel["mimo/mimo-v2.5"].RecentCount; got != 3 {
		t.Errorf("recent after newer seed = %v, want 3", got)
	}
}
