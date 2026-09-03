package session

import (
	"path/filepath"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// quotaSlot is a persisted active slot carrying a quota map, as written by
// the last pre-restart admission.
func quotaSlot(pollAt time.Time) *cachedState {
	expiry := time.Now().Add(time.Hour)
	return &cachedState{
		status:            "active",
		instanceID:        "inst-quota-1",
		model:             "mimo/mimo-v2.5",
		expiresAt:         expiry,
		gracePeriodEndsAt: expiry.Add(graceWindow),
		pollAt:            pollAt,
		quotaByModel: map[string]upstream.ModelQuota{
			"mimo/mimo-v2.5": {
				Model:       "mimo/mimo-v2.5",
				Limit:       5,
				RecentCount: 2,
				ResetAt:     expiry,
				Period:      "pacific_day",
			},
		},
		glmPromo: `{"dailySessions":1,"endsAt":"2030-01-01T00:00:00Z"}`,
	}
}

// TestSnapshotRestoresPersistedQuotaStale pins the quota-tracker restart
// view: a fresh manager with no live admission serves the on-disk quota as
// stale (flagged, with the last-poll time) instead of an empty table. The
// first live quota commit clears the mark; a quota-less compact commit keeps
// it (the map is re-applied saved state, not fresh quota).
func TestSnapshotRestoresPersistedQuotaStale(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	_, key := newPersistTestManager(t, mock, store)
	pollAt := time.Now().Add(-90 * time.Minute).Truncate(time.Second)
	store.Save(key, quotaSlot(pollAt))

	// Fresh manager, no admission yet: restored stale quota.
	mgr2, _ := newPersistTestManager(t, mock, store)
	snap := mgr2.Snapshot()
	q, ok := snap.QuotaByModel["mimo/mimo-v2.5"]
	if !ok {
		t.Fatalf("restored quota missing mimo row: %+v", snap.QuotaByModel)
	}
	if q.Limit != 5 || q.RecentCount != 2 {
		t.Errorf("restored quota = %+v, want limit=5 recent=2", q)
	}
	if !snap.QuotaStale {
		t.Errorf("QuotaStale = false, want true for disk-restored quota")
	}
	if !snap.QuotaSavedAt.Equal(pollAt) {
		t.Errorf("QuotaSavedAt = %v, want pollAt %v", snap.QuotaSavedAt, pollAt)
	}
	if snap.GlmPromo == "" {
		t.Errorf("GlmPromo empty, want restored promo block")
	}
	if snap.Status != "" || snap.InstanceID != "" {
		t.Errorf("restored live session fields status=%q instance=%q, want empty (quota only, session stays honest)",
			snap.Status, snap.InstanceID)
	}

	// A quota-less compact commit re-applies the saved map: still stale.
	mgr2.mu.Lock()
	mgr2.commit(&cachedState{status: "active", instanceID: "inst-quota-1"})
	mgr2.mu.Unlock()
	if snap := mgr2.Snapshot(); !snap.QuotaStale {
		t.Errorf("QuotaStale after quota-less commit = false, want true (re-applied saved map is not fresh)")
	}

	// A live quota-carrying commit clears the mark.
	mgr2.mu.Lock()
	mgr2.commit(&cachedState{
		status:      "active",
		instanceID:  "inst-quota-1",
		remainingMs: 42 * 60 * 1000,
		quotaByModel: map[string]upstream.ModelQuota{
			"mimo/mimo-v2.5": {Model: "mimo/mimo-v2.5", Limit: 5, RecentCount: 3},
		},
	})
	mgr2.mu.Unlock()
	if snap := mgr2.Snapshot(); snap.QuotaStale {
		t.Errorf("QuotaStale after live commit = true, want false")
	}
	// Live countdowns survive alongside the quota (dashboard ?view=live
	// carries the session timers every poll).
	if snap := mgr2.Snapshot(); snap.RemainingMs != 42*60*1000 {
		t.Errorf("RemainingMs after live commit = %d, want %d", snap.RemainingMs, 42*60*1000)
	}
}

// TestSnapshotNoStoreNoStale pins the zero path: without persistence the
// tracker keeps its existing empty view (no stale flag, no quota).
func TestSnapshotNoStoreNoStale(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr, _ := newPersistTestManager(t, mock, NewStore(filepath.Join(t.TempDir(), "state.json")))
	snap := mgr.Snapshot()
	if len(snap.QuotaByModel) != 0 {
		t.Fatalf("quota without store/admission = %+v, want empty", snap.QuotaByModel)
	}
	if snap.QuotaStale {
		t.Errorf("QuotaStale = true with no data, want false")
	}
}
