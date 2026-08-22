package pool

// Wave-3 pool tests: quota-aware token ordering (#85), the session-create
// admission gate (#86), the local spend ledger (#87), run pre-create at
// admission (#90a), abandoned-lease finish (#53/#114), and step recording
// (#114: steps ride the FINISH payload).

import (
	"context"
	"errors"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

// futureReset is a ResetAt ~1h out for quota fixtures.
func futureReset() time.Time { return time.Now().Add(time.Hour) }

func quotaFor(model string, limit, recent float64, reset time.Time) map[string]any {
	return map[string]any{
		model: map[string]any{
			"model":       model,
			"limit":       limit,
			"recentCount": recent,
			"period":      "pacific_day",
			"resetAt":     reset.UTC().Format(time.RFC3339),
		},
	}
}

// admitBoth admits sessions for token 0 and 1 on modelA so both are "hot".
func admitBoth(t *testing.T, p *Pool, model string) {
	t.Helper()
	toks := p.toks.Load()
	ctx := context.Background()
	for i := 0; i < len(*toks); i++ {
		if _, err := (*toks)[i].session.EnsureSessionForModel(ctx, model); err != nil {
			t.Fatalf("admit token %d: %v", i, err)
		}
	}
}

func TestAcquireQuotaAwareOrdering(t *testing.T) {
	reset := futureReset()
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.RateLimitsByModel = quotaFor(modelA, 10, 8, reset) // remaining 2
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mock1.RateLimitsByModel = quotaFor(modelA, 10, 2, reset) // remaining 8
	p := newTestPool(t, mock0, mock1)
	admitBoth(t, p, modelA)

	// Both tokens hot with KNOWN positive remaining quota: smallest
	// remaining (token 0, rem 2) must be tried first.
	toks := p.toks.Load()
	order, limited := p.acquireOrder(toks, 0, modelA)
	if len(limited) != 0 {
		t.Fatalf("unexpected quota-limited errors: %v", limited)
	}
	if len(order) < 2 || order[0] != 0 {
		t.Fatalf("order = %v, want token 0 (smallest remaining) first", order)
	}
	// Token 1 must follow (larger remaining) before any cold token.
	if order[1] != 1 {
		t.Errorf("order[1] = %d, want 1", order[1])
	}
}

func TestAcquireKnownQuotaBeforeUnknown(t *testing.T) {
	reset := futureReset()
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.RateLimitsByModel = quotaFor(modelA, 10, 8, reset) // known, rem 2
	mock1 := testutil.NewMock()
	defer mock1.Close() // no quota → unknown
	p := newTestPool(t, mock0, mock1)
	admitBoth(t, p, modelA)

	toks := p.toks.Load()
	order, _ := p.acquireOrder(toks, 0, modelA)
	if len(order) < 2 || order[0] != 0 {
		t.Fatalf("order = %v, want known-quota token 0 first", order)
	}
}

func TestAcquireSkipsQuotaCappedAndSurfaces429(t *testing.T) {
	reset := futureReset()
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.RateLimitsByModel = quotaFor(modelA, 5, 5, reset) // capped
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mock1.RateLimitsByModel = quotaFor(modelA, 5, 5, reset) // capped
	p := newTestPool(t, mock0, mock1)
	admitBoth(t, p, modelA)

	// Both tokens are capped for modelA: Acquire must surface a 429
	// (RateLimitError) with the earliest window reset, not a generic error.
	_, err := p.Acquire(context.Background(), modelA)
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("Acquire err = %v, want *RateLimitError", err)
	}
	if rle.Limit != 5 || rle.RecentCount != 5 {
		t.Errorf("rate limit = %g/%g, want 5/5", rle.RecentCount, rle.Limit)
	}
	if rle.RetryAfter <= 0 || rle.RetryAfter > time.Hour {
		t.Errorf("RetryAfter = %v, want ~1h window", rle.RetryAfter)
	}
	// No session was created for the capped tokens.
	if mock0.SessionCreates != 1 || mock1.SessionCreates != 1 {
		t.Errorf("session creates = %d/%d, want 1/1 (only the admits)", mock0.SessionCreates, mock1.SessionCreates)
	}
}

func TestAcquireStaleQuotaNotCapped(t *testing.T) {
	// RecentCount >= Limit but the window already rolled (past ResetAt):
	// not capped, treated as unknown — never skipped.
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.RateLimitsByModel = quotaFor(modelA, 5, 5, time.Now().Add(-time.Minute))
	p := newTestPool(t, mock0)
	admitBoth(t, p, modelA)

	toks := p.toks.Load()
	order, limited := p.acquireOrder(toks, 0, modelA)
	if len(limited) != 0 {
		t.Fatalf("stale-quota token wrongly capped: %v", limited)
	}
	if len(order) != 1 || order[0] != 0 {
		t.Errorf("order = %v, want [0]", order)
	}
}

func TestCreateGateBlocksAtCapAndReleases(t *testing.T) {
	// Per-model cap: a second acquire on the same model waits until the
	// holder releases (the global cap leaves room, so only the model cap
	// gates it).
	g := newCreateGate(4, 1)
	p1, err := g.acquire(context.Background(), "m1")
	if err != nil {
		t.Fatal(err)
	}
	blocked := make(chan *createPermit, 1)
	go func() {
		p, _ := g.acquire(context.Background(), "m1")
		blocked <- p
	}()
	select {
	case <-blocked:
		t.Fatal("per-model cap not enforced")
	case <-time.After(100 * time.Millisecond):
	}
	p1.Release()
	select {
	case got := <-blocked:
		if got == nil {
			t.Fatal("waiter got nil permit")
		}
		got.Release()
	case <-time.After(2 * time.Second):
		t.Fatal("waiter not woken after release")
	}

	// Global cap: with every model cap free, the second acquire still waits
	// until the global holder releases.
	g2 := newCreateGate(1, 4)
	p2, err := g2.acquire(context.Background(), "m1")
	if err != nil {
		t.Fatal(err)
	}
	blocked2 := make(chan *createPermit, 1)
	go func() {
		p, _ := g2.acquire(context.Background(), "m2")
		blocked2 <- p
	}()
	select {
	case <-blocked2:
		t.Fatal("global cap not enforced")
	case <-time.After(100 * time.Millisecond):
	}
	p2.Release()
	select {
	case got := <-blocked2:
		got.Release()
	case <-time.After(2 * time.Second):
		t.Fatal("global waiter not woken after release")
	}
}

func TestCreateGateWaitExpiresWithCtx(t *testing.T) {
	g := newCreateGate(1, 1)
	p1, err := g.acquire(context.Background(), "m1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = g.acquire(ctx, "m1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire at cap with expiring ctx = %v, want DeadlineExceeded", err)
	}
	p1.Release()
}

func TestAcquireCreateGateWaits(t *testing.T) {
	// Global cap 1: while one admission holds the slot, a second Acquire
	// must wait (its ctx deadline surfaces the wait-or-503 behavior).
	mock0 := testutil.NewMock()
	defer mock0.Close()
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.SessionCreateMaxParallelGlobal = 1
		c.SessionCreateMaxParallelPerModel = 1
	}, mock0)

	// Hold the gate slot.
	permit, err := p.gate.acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err = p.Acquire(ctx, modelA)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire under gate cap = %v, want DeadlineExceeded (wait-or-503)", err)
	}
	permit.Release()

	// After the release, Acquire succeeds.
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)
}

func TestSpendLedgerRollover(t *testing.T) {
	p := newTestPool(t, testutil.NewMock())
	p.spendMu.Lock()
	l := p.spendPerToken[0]
	p.spendMu.Unlock()

	now := time.Now()
	l.add(100, now)
	v := ledgerView(l)
	if v.Rolling24h != 100 || v.Day != 100 || v.Week != 100 || v.Month != 100 {
		t.Fatalf("ledger after 100: %+v", v)
	}
	if v.DayStart.IsZero() || v.WeekStart.IsZero() || v.MonthStart.IsZero() {
		t.Fatal("period starts not set")
	}

	// Day rollover: force a start from the previous Pacific day (26h is
	// safely past the previous midnight even on the 25h fall-back DST day),
	// add → resets then accumulates.
	l.dayStart = now.Add(-26 * time.Hour).Unix()
	l.add(50, now)
	v = ledgerView(l)
	if v.Day != 50 {
		t.Errorf("Day = %d, want 50 after rollover+add", v.Day)
	}
	if v.Rolling24h != 150 {
		t.Errorf("Rolling24h = %d, want 150", v.Rolling24h)
	}
	if v.Week != 150 {
		t.Errorf("Week = %d, want 150 (week window still open)", v.Week)
	}
}

// TestSpendDayBucketRollsAtPacificMidnight pins the #122 day-bucket
// boundary: the daily bucket rolls at Pacific midnight
// (America/Los_Angeles), NOT UTC midnight. In summer (PDT, UTC-7) the
// boundary is 07:00Z: the UTC day turns over at 00:00Z hours earlier, so a
// UTC-midnight bucket would reset at the wrong instant.
func TestSpendDayBucketRollsAtPacificMidnight(t *testing.T) {
	// Summer: Pacific midnight = 07:00Z (PDT).
	before := time.Date(2026, 8, 17, 6, 59, 0, 0, time.UTC) // 23:59 PDT 08-16
	after := time.Date(2026, 8, 17, 7, 0, 0, 0, time.UTC)   // 00:00 PDT 08-17

	if got := bucketStart(before, "day"); got != time.Date(2026, 8, 16, 7, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("bucketStart(%v) = %v, want 08-16T07:00Z (Pacific midnight)", before, time.Unix(got, 0).UTC())
	}
	if got := bucketStart(after, "day"); got != time.Date(2026, 8, 17, 7, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("bucketStart(%v) = %v, want 08-17T07:00Z (Pacific midnight)", after, time.Unix(got, 0).UTC())
	}
	// Winter (PST, UTC-8): the boundary is 08:00Z.
	winter := time.Date(2026, 1, 5, 8, 30, 0, 0, time.UTC) // 00:30 PST 01-05
	if got := bucketStart(winter, "day"); got != time.Date(2026, 1, 5, 8, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("bucketStart(%v) = %v, want 01-05T08:00Z (Pacific midnight)", winter, time.Unix(got, 0).UTC())
	}

	p := newTestPool(t, testutil.NewMock())
	p.spendMu.Lock()
	l := p.spendPerToken[0]
	p.spendMu.Unlock()

	l.add(100, before)
	v := ledgerView(l)
	if v.Day != 100 {
		t.Fatalf("Day after 100 at %v = %d, want 100", before, v.Day)
	}
	// 00:00Z is 00:00 PDT the same Pacific day: the UTC date rolled, the
	// Pacific day has not — no reset.
	l.add(50, time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC))
	v = ledgerView(l)
	if v.Day != 150 {
		t.Errorf("Day after add at 00:00Z = %d, want 150 (no Pacific rollover yet)", v.Day)
	}
	// 07:00Z = 00:00 PDT: the Pacific day rolled — reset then accumulate.
	l.add(75, after)
	v = ledgerView(l)
	if v.Day != 75 {
		t.Errorf("Day after add at 07:00Z = %d, want 75 (rolled at Pacific midnight)", v.Day)
	}
	if want := time.Date(2026, 8, 17, 7, 0, 0, 0, time.UTC); !v.DayStart.Equal(want) {
		t.Errorf("DayStart = %v, want %v (Pacific midnight)", v.DayStart, want)
	}
}

func TestRecordSpendSurfacesInSnapshot(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.RecordSpend(lease, 1234)
	p.LeaseRelease(lease)

	snaps := p.Snapshot()
	if len(snaps) != 1 || snaps[0].Spend24h != 1234 {
		t.Fatalf("snapshot spend = %+v, want Spend24h 1234", snaps)
	}
	if snaps[0].SpendDay != 1234 || snaps[0].SpendWeek != 1234 || snaps[0].SpendMonth != 1234 {
		t.Errorf("period spend = %d/%d/%d, want 1234 each", snaps[0].SpendDay, snaps[0].SpendWeek, snaps[0].SpendMonth)
	}
}

func TestRecordSpendBridge(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgePool(t, mock) // bridge-mode pool (client tokens, no AUTH_TOKENS)

	lease, err := p.AcquireBridge(context.Background(), "client-tok", modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.RecordSpend(lease, 99)
	v := p.bridgeSpendSnapshot(lease.Bridge)
	if v.Rolling24h != 99 {
		t.Errorf("bridge spend = %d, want 99", v.Rolling24h)
	}
	p.LeaseRelease(lease)
}

func TestLeaseAbandonFinishesRun(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	runID := lease.Run.RunID
	p.LeaseAbandon(lease) // client disconnect

	// Issue #114: an abandoned run must FINISH as cancelled, not completed —
	// a gateway with zero cancelled runs looks synthetic.
	eventually(t, "abandoned run FINISH", func() bool {
		for _, f := range mock.FinishedRunsSnapshot() {
			if f.RunID == runID && f.Status == "cancelled" {
				return true
			}
		}
		return false
	})
}

func TestPrecreateAtAdmissionStartsRun(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	// First Acquire admits the session AND pre-creates the run; the lease
	// then rides it. The mock must see exactly one START for the agent.
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Run.RunID == "" {
		t.Fatal("lease without run")
	}
	if got := mock.StartedRunsSnapshot(); len(got) != 1 || got[0] != agentA {
		t.Fatalf("started runs = %v, want [%s]", got, agentA)
	}
	p.LeaseRelease(lease)
}

func TestRecordRunStepThroughPool(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	// Issue #114: steps are appended in memory and batched with FINISH —
	// the pool path must never hit the (removed) /steps endpoint.
	p.RecordRunStep(lease, "chatcmpl-9")
	p.LeaseRelease(lease)
	p.Shutdown(context.Background())

	finished := mock.FinishedRunsSnapshot()
	if len(finished) != 1 {
		t.Fatalf("finished runs = %d, want 1", len(finished))
	}
	f := finished[0]
	if f.Status != "completed" || f.TotalSteps != 1 {
		t.Errorf("FINISH = %+v, want completed with 1 total step", f)
	}
	if len(f.Steps) != 1 || f.Steps[0].StepNumber != 1 || f.Steps[0].MessageID == nil || *f.Steps[0].MessageID != "chatcmpl-9" {
		t.Errorf("FINISH steps = %+v, want step 1 with message chatcmpl-9", f.Steps)
	}
}
