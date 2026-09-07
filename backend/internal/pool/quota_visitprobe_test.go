// quota_visitprobe_test.go — visit bulk probe (ADR-0025) coverage: the
// pure due helper (never/fresh/stale/future boundaries) plus pool wiring
// (stale fires through the fake upstream, fresh skips, maxAge re-arms,
// manual force always probes and stamps, visit ignores the scheduler
// kill-switch). Existing scheduler/boot tests are untouched.
package pool

import (
	"context"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
)

func TestQuotaVisitProbeDueBoundaries(t *testing.T) {
	now := time.Now()
	if !quotaVisitProbeDue(time.Time{}, now, time.Hour) {
		t.Error("never probed (zero timestamp): want due")
	}
	if !quotaVisitProbeDue(now.Add(-2*time.Hour), now, time.Hour) {
		t.Error("last probe 2h ago with 1h maxAge: want due")
	}
	if quotaVisitProbeDue(now.Add(-30*time.Minute), now, time.Hour) {
		t.Error("last probe 30m ago with 1h maxAge: want fresh")
	}
	if quotaVisitProbeDue(now.Add(time.Hour), now, time.Hour) {
		t.Error("last probe in the future (clock skew): want fresh")
	}
	if !quotaVisitProbeDue(now.Add(-time.Hour), now, time.Hour) {
		t.Error("last probe exactly maxAge old: want due (boundary)")
	}
}

func TestProbeAllIfStaleNeverProbedFires(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	// Zero-value test Config leaves QUOTA_AUTO_PROBE off: the visit probe
	// is an explicit user-visit action like the manual button, not
	// scheduler work, so the kill-switch must not gate it.
	if !p.ProbeAllIfStale(context.Background(), time.Hour) {
		t.Fatal("ProbeAllIfStale on a never-probed pool: want true")
	}
	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Fatalf("SessionProbes = %d, want 1 (one session-less probe)", got)
	}
}

func TestProbeAllIfStaleFreshSkips(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	if !p.ProbeAllIfStale(context.Background(), time.Hour) {
		t.Fatal("first ProbeAllIfStale: want true (never probed)")
	}
	if p.ProbeAllIfStale(context.Background(), time.Hour) {
		t.Error("second ProbeAllIfStale within maxAge: want false (no upstream touch)")
	}
	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Errorf("SessionProbes = %d after fresh skip, want still 1", got)
	}
}

func TestProbeAllIfStaleRefiresAfterMaxAge(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)
	if !p.ProbeAllIfStale(context.Background(), time.Hour) {
		t.Fatal("first ProbeAllIfStale: want true (never probed)")
	}
	p.bulkProbeMu.Lock()
	p.lastBulkProbe = time.Now().Add(-2 * time.Hour)
	p.bulkProbeMu.Unlock()
	if !p.ProbeAllIfStale(context.Background(), time.Hour) {
		t.Error("ProbeAllIfStale with 2h-old timestamp: want true (stale)")
	}
	if got := mock.SessionProbesSnapshot(); got != 2 {
		t.Errorf("SessionProbes = %d after stale re-fire, want 2", got)
	}
}

func TestProbeAllForcesAndStamps(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)

	results := p.ProbeAll(context.Background())
	if len(results) != 2 {
		t.Fatalf("ProbeAll results = %d, want 2 (one per pooled token)", len(results))
	}
	for i, res := range results {
		if res.Index != i {
			t.Errorf("results[%d].Index = %d, want %d", i, res.Index, i)
		}
		if res.Err != nil {
			t.Errorf("results[%d].Err = %v, want nil against the fake upstream", i, res.Err)
		}
	}
	for i, mock := range []*testutil.MockUpstream{mock0, mock1} {
		if got := mock.SessionProbesSnapshot(); got != 1 {
			t.Errorf("token %d SessionProbes = %d, want 1", i, got)
		}
	}

	// The force pass stamps the pool-scoped timestamp: a visit right
	// after skips without touching upstream.
	if p.ProbeAllIfStale(context.Background(), time.Hour) {
		t.Error("ProbeAllIfStale right after manual ProbeAll: want false")
	}

	// And the timestamp never gates the manual path: forcing again
	// re-probes every token.
	p.ProbeAll(context.Background())
	for i, mock := range []*testutil.MockUpstream{mock0, mock1} {
		if got := mock.SessionProbesSnapshot(); got != 2 {
			t.Errorf("token %d SessionProbes = %d after second force, want 2", i, got)
		}
	}
}
