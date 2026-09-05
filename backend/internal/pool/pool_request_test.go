package pool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// TestAcquirePerMinuteRequestCap pins MAX_REQUESTS_PER_MINUTE on the pooled
// path: one admitted chat fills the 60s window, the next acquire surfaces a
// 429 "per-minute request limit reached" with RetryAfter until the oldest
// admitted request ages out.
func TestAcquirePerMinuteRequestCap(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPoolCfg(t, func(c *config.Config) { c.MaxRequestsPerMinute = 1 }, mock)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	chatOnce(t, p, lease) // admission counted at Acquire; chat records the successful chat
	p.LeaseRelease(lease)
	if got := p.Snapshot()[0].RequestsPerMinute; got != 1 {
		t.Errorf("RequestsPerMinute = %d, want 1", got)
	}

	// Second acquire within the window: capped.
	_, err = p.Acquire(context.Background(), modelA)
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("want *upstream.RateLimitError for capped token, got %v", err)
	}
	if !errors.Is(err, upstream.ErrRateLimited) {
		t.Error("errors.Is(ErrRateLimited) = false")
	}
	if rle.RetryAfter <= 0 || rle.RetryAfter > rpmWindow {
		t.Errorf("RetryAfter = %s, want within (0, 60s]", rle.RetryAfter)
	}
	if rle.Limit != 1 || rle.RecentCount != 1 {
		t.Errorf("quota = %v/%v, want 1/1", rle.RecentCount, rle.Limit)
	}
	if !strings.Contains(err.Error(), "per-minute request limit reached") {
		t.Errorf("error = %q, want per-minute-limit message", err)
	}
}

// TestAcquireDailyRequestCap pins MAX_REQUESTS_PER_DAY on the pooled path:
// one successful chat fills the Pacific-day bucket, the next acquire
// surfaces 429 "daily request limit reached" with RetryAfter until the next
// Pacific midnight (the official daily reset instant) — the unlock the user
// asked to align with the upstream quota reset.
func TestAcquireDailyRequestCap(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPoolCfg(t, func(c *config.Config) { c.MaxRequestsPerDay = 1 }, mock)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	chatOnce(t, p, lease) // records the successful chat → day bucket
	p.LeaseRelease(lease)

	if got := p.Snapshot()[0].RequestsPerDay; got != 1 {
		t.Errorf("RequestsPerDay = %d, want 1", got)
	}
	if got := p.Snapshot()[0].RequestsPerDayLimit; got != 1 {
		t.Errorf("RequestsPerDayLimit = %d, want 1", got)
	}

	// Second acquire: day-capped.
	_, err = p.Acquire(context.Background(), modelA)
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("want *upstream.RateLimitError for capped token, got %v", err)
	}
	if !errors.Is(err, upstream.ErrRateLimited) {
		t.Error("errors.Is(ErrRateLimited) = false")
	}
	// Pacific midnight is 23-25h from the previous midnight; the remaining
	// window is always > 1h and <= 26h, and clearly not the 60s RPM window.
	if rle.RetryAfter <= rpmWindow || rle.RetryAfter > 26*time.Hour {
		t.Errorf("RetryAfter = %s, want ≈ next Pacific midnight (>1h, <=26h)", rle.RetryAfter)
	}
	if rle.Limit != 1 || rle.RecentCount != 1 {
		t.Errorf("quota = %v/%v, want 1/1", rle.RecentCount, rle.Limit)
	}
	if !strings.Contains(err.Error(), "daily request limit reached") {
		t.Errorf("error = %q, want daily-limit message", err)
	}
}

// TestRequestCapFailoverRollsToNextToken pins the rolling behavior behind
// the multi-account setup: a token capped by MAX_REQUESTS_PER_MINUTE is
// skipped and the next uncapped token serves the request; only when every
// token is capped does the client see a 429.
func TestRequestCapFailoverRollsToNextToken(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)
	cfg := p.cfg.Load()
	cfg.MaxRequestsPerMinute = 1
	p.cfg.Store(cfg)

	// Round-robin: first acquire lands on token-1; admit one request on it.
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Token != 0 {
		t.Fatalf("first lease token = %d, want 0", lease.Token)
	}
	chatOnce(t, p, lease)
	p.LeaseRelease(lease)

	// Second acquire fails over to token-2 (token-1 is capped at 1/min).
	lease, err = p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Token != 1 {
		t.Fatalf("second lease token = %d, want 1 (failover to uncapped)", lease.Token)
	}
	chatOnce(t, p, lease)
	p.LeaseRelease(lease)

	// Both tokens capped: the pool surfaces the per-minute 429.
	_, err = p.Acquire(context.Background(), modelA)
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("want *upstream.RateLimitError when every token is capped, got %v", err)
	}
	if rle.RetryAfter <= 0 || rle.RetryAfter > rpmWindow {
		t.Errorf("RetryAfter = %s, want within (0, 60s]", rle.RetryAfter)
	}
}

// TestBridgeRequestCaps drives the bridge-mode request caps: per-client
// tokens have independent RPM and RPD counters (mirrors the fixed-token
// path), and each cap surfaces its own 429 body.
func TestBridgeRequestCaps(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(`{"id":"chatcmpl-b1","object":"chat.completion.chunk","created":1,"model":"` + modelA + `","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`)
	p := newBridgePool(t, mock)
	cfg := p.cfg.Load()
	cfg.MaxRequestsPerMinute = 1
	cfg.MaxRequestsPerDay = 2
	p.cfg.Store(cfg)

	// Client A: first chat succeeds (RPM 0→1, day 0→1).
	lease, err := p.AcquireBridge(context.Background(), "client-a", modelA)
	if err != nil {
		t.Fatal(err)
	}
	chatOnce(t, p, lease)
	p.LeaseRelease(lease)

	// Second acquire: RPM capped at 1/min → per-minute 429.
	_, err = p.AcquireBridge(context.Background(), "client-a", modelA)
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("client A RPM cap: want *upstream.RateLimitError, got %v", err)
	}
	if !errors.Is(err, upstream.ErrRateLimited) {
		t.Error("errors.Is(ErrRateLimited) = false")
	}
	if rle.Body != "per-minute request limit reached" {
		t.Errorf("client A body = %q, want per-minute message", rle.Body)
	}

	// Client B's counters are independent: first chat succeeds.
	leaseB, err := p.AcquireBridge(context.Background(), "client-b", modelA)
	if err != nil {
		t.Fatalf("client B acquire failed despite independent counters: %v", err)
	}
	chatOnce(t, p, leaseB)
	p.LeaseRelease(leaseB)

	// Tighten the day cap below client B's usage, and disable the RPM cap
	// so the day cap is the only gate: next acquire is day-capped with the
	// Pacific-midnight RetryAfter.
	cfg = p.cfg.Load()
	cfg.MaxRequestsPerMinute = 0
	cfg.MaxRequestsPerDay = 1
	p.cfg.Store(cfg)
	_, err = p.AcquireBridge(context.Background(), "client-b", modelA)
	if !errors.As(err, &rle) {
		t.Fatalf("client B day cap: want *upstream.RateLimitError, got %v", err)
	}
	if rle.Body != "daily request limit reached" {
		t.Errorf("client B body = %q, want daily message", rle.Body)
	}
	if rle.RetryAfter <= rpmWindow || rle.RetryAfter > 26*time.Hour {
		t.Errorf("client B RetryAfter = %s, want ≈ next Pacific midnight", rle.RetryAfter)
	}

	if got := p.bridgeRpmCount(p.bridgeToken("client-a")); got != 1 {
		t.Errorf("client A rpm = %d, want 1 (unchanged by client B)", got)
	}
}

// TestRequestLedgerRPMWindow pins the ledger's rolling 60s admission window:
// counts prune as timestamps age out, and rpmResetIn tracks the oldest one.
func TestRequestLedgerRPMWindow(t *testing.T) {
	l := newAccountLedger()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	for i := range 5 {
		l.recordRequest(now.Add(time.Duration(i) * time.Second))
	}
	if got := l.rpmCount(now.Add(59 * time.Second)); got != 5 {
		t.Errorf("rpmCount at +59s = %d, want 5", got)
	}
	// Entries recorded at +1s..+4s stay in-window until +61s..+64s.
	if got := l.rpmCount(now.Add(61 * time.Second)); got != 4 {
		t.Errorf("rpmCount at +61s = %d, want 4 (only +0s aged out)", got)
	}
	if got := l.rpmCount(now.Add(65 * time.Second)); got != 0 {
		t.Errorf("rpmCount at +65s = %d, want 0 (window expired)", got)
	}

	l2 := newAccountLedger()
	l2.recordRequest(now)
	if d := l2.rpmResetIn(now.Add(30 * time.Second)); d != 30*time.Second {
		t.Errorf("rpmResetIn at +30s = %s, want 30s", d)
	}
	if d := l2.rpmResetIn(now.Add(61 * time.Second)); d != 0 {
		t.Errorf("rpmResetIn at +61s = %s, want 0 (expired)", d)
	}
}

// TestRequestLedgerDayBucketRollsAtPacificMidnight pins the RPD bucket
// boundary: the day bucket rolls at Pacific midnight
// (America/Los_Angeles), NOT UTC midnight — the same instant upstream resets
// its daily quota windows. July exercises PDT (07:00Z), January PST
// (08:00Z), mirroring TestSpendDayBucketRollsAtPacificMidnight.
func TestRequestLedgerDayBucketRollsAtPacificMidnight(t *testing.T) {
	// PDT July: 06:59Z is still the PREVIOUS Pacific day.
	before := time.Date(2026, 7, 16, 6, 59, 0, 0, time.UTC)
	at := time.Date(2026, 7, 16, 7, 0, 0, 0, time.UTC) // 00:00 PDT 07-16
	l := newAccountLedger()
	l.recordDayRequest(before)
	if got := l.dayRequestCount(before); got != 1 {
		t.Errorf("PDT count before midnight = %d, want 1", got)
	}
	l.recordDayRequest(at)
	if got := l.dayRequestCount(at); got != 1 {
		t.Errorf("PDT count at midnight = %d, want 1 (bucket rolled, then +1)", got)
	}
	l.recordDayRequest(at.Add(time.Minute))
	if got := l.dayRequestCount(at.Add(time.Minute)); got != 2 {
		t.Errorf("PDT count after +1m = %d, want 2", got)
	}

	// PST January: the boundary is 08:00Z.
	winter := time.Date(2026, 1, 16, 8, 0, 0, 0, time.UTC) // 00:00 PST 01-16
	lw := newAccountLedger()
	lw.recordDayRequest(winter.Add(-time.Minute))
	if got := lw.dayRequestCount(winter.Add(-time.Minute)); got != 1 {
		t.Errorf("PST count before midnight = %d, want 1", got)
	}
	lw.recordDayRequest(winter)
	if got := lw.dayRequestCount(winter); got != 1 {
		t.Errorf("PST count at midnight = %d, want 1 (rolled)", got)
	}

	// dayRequestResetIn is wall-clock (time.Until): always the remaining
	// time to the next Pacific midnight — within (0, 26h] (the 23/24/25h
	// DST day length) no matter when it runs.
	l3 := newAccountLedger()
	if d := l3.dayRequestResetIn(time.Now()); d <= 0 || d > 26*time.Hour {
		t.Errorf("dayRequestResetIn = %s, want (0, 26h]", d)
	}
}

// TestAcquireRPMAdmitBurstIsAtomic pins that RPM admission counting is
// atomic with the lease grant: two concurrent acquires against cap=1 must
// admit exactly one (the other 429s) — never both. Regression: the old
// check-in-Acquire / record-in-Chat split let a concurrent burst (agent
// spawn batches) pass the cap before any record landed, since Chat records
// after an upstream call the test path never makes.
func TestAcquireRPMAdmitBurstIsAtomic(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPoolCfg(t, func(c *config.Config) { c.MaxRequestsPerMinute = 1 }, mock)

	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := p.Acquire(context.Background(), modelA)
			results <- err
		}()
	}
	close(start)
	var okCount, cappedCount int
	for range 2 {
		if err := <-results; err == nil {
			okCount++
		} else if errors.Is(err, upstream.ErrRateLimited) {
			cappedCount++
		} else {
			t.Fatalf("unexpected acquire error: %v", err)
		}
	}
	if okCount != 1 || cappedCount != 1 {
		t.Errorf("burst admit = %d ok / %d capped, want 1/1 (admission counting must be atomic with the grant)", okCount, cappedCount)
	}
}

// TestAcquireCountsAdmissionAtGrant pins that an admitted request is
// counted the moment the lease is granted — before any chat. A lease with
// no follow-up chat still consumed its admission slot.
func TestAcquireCountsAdmissionAtGrant(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock) // RPM cap 0 (unlimited) — counting still happens

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	defer p.LeaseRelease(lease)
	if got := p.Snapshot()[0].RequestsPerMinute; got != 1 {
		t.Errorf("RequestsPerMinute right after Acquire = %d, want 1 (admission counted at lease grant)", got)
	}
}

// TestBridgeRPMAdmitBurstIsAtomic is the bridge-mode mirror: two
// concurrent AcquireBridge calls for one client token against cap=1 must
// admit exactly one.
func TestBridgeRPMAdmitBurstIsAtomic(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgePool(t, mock)
	cfg := p.cfg.Load()
	cfg.MaxRequestsPerMinute = 1
	p.cfg.Store(cfg)

	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := p.AcquireBridge(context.Background(), "client-a", modelA)
			results <- err
		}()
	}
	close(start)
	var okCount, cappedCount int
	for range 2 {
		if err := <-results; err == nil {
			okCount++
		} else if errors.Is(err, upstream.ErrRateLimited) {
			cappedCount++
		} else {
			t.Fatalf("unexpected bridge acquire error: %v", err)
		}
	}
	if okCount != 1 || cappedCount != 1 {
		t.Errorf("bridge burst admit = %d ok / %d capped, want 1/1", okCount, cappedCount)
	}
}
