package pool

// Step-1 smart-routing tests: per-token live-turn slots with FIFO queues,
// the unified scorer, and the ROUTING_SMART master switch. Existing acquire
// tests are untouched (they pin the legacy path with hand-built configs
// where RoutingSmart is false); these tests prove the new path.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// newSmartTestPool wires mocks through newTestPoolCfg with the smart path
// on and explicit routing knobs (hand-built configs bypass Load, so every
// knob under test is set here, never inferred).
func newSmartTestPool(t *testing.T, mut func(*config.Config), mocks ...*testutil.MockUpstream) *Pool {
	t.Helper()
	return newTestPoolCfg(t, func(c *config.Config) {
		c.RoutingSmart = true
		c.TokenMaxConcurrent = 2
		c.QueueWait = 30 * time.Second
		c.QueueDepth = 16
		c.TokenRotation = "drain"
		if mut != nil {
			mut(c)
		}
	}, mocks...)
}

func smartEntry(p *Pool, idx int) *tokenEntry {
	return (*p.roster.Load())[idx]
}

// TestRouteSmartRacingCapWaits proves the cap-2 hard wall on one token:
// three racing acquires never produce a third live turn — the third parks
// until a release wakes it.
func TestRouteSmartRacingCapWaits(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newSmartTestPool(t, nil, mock)
	entry := smartEntry(p, 0)

	first, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.routeSlotLive(entry); got != 2 {
		t.Fatalf("live slots = %d, want 2", got)
	}

	type result struct {
		lease *Lease
		err   error
	}
	thirdCh := make(chan result, 1)
	go func() {
		lease, err := p.Acquire(context.Background(), modelA)
		thirdCh <- result{lease, err}
	}()
	eventually(t, "third acquire parks", func() bool { return p.routeSlotQueued(entry) == 1 })
	if got := p.routeSlotLive(entry); got != 2 {
		t.Fatalf("live slots while parked = %d, want 2 (never a third live turn)", got)
	}
	select {
	case r := <-thirdCh:
		t.Fatalf("third acquire returned early: lease=%v err=%v", r.lease, r.err)
	case <-time.After(150 * time.Millisecond):
	}
	if got := p.routeSlotLive(entry); got != 2 {
		t.Fatalf("live slots after wait = %d, want 2", got)
	}
	p.LeaseRelease(first)
	var third *Lease
	select {
	case r := <-thirdCh:
		if r.err != nil {
			t.Fatalf("third acquire err = %v", r.err)
		}
		if r.lease.Token != 0 {
			t.Errorf("third lease token = %d, want 0", r.lease.Token)
		}
		if r.lease.routeSlot == nil {
			t.Error("third lease carries no live-turn slot")
		}
		third = r.lease
	case <-time.After(5 * time.Second):
		t.Fatal("third acquire never woke after release")
	}
	if got := p.routeSlotLive(entry); got != 2 {
		t.Errorf("live slots after grant = %d, want 2", got)
	}
	p.LeaseRelease(second)
	p.LeaseRelease(third)
	eventually(t, "slots drain", func() bool { return p.routeSlotLive(entry) == 0 })
}

// TestRouteSmartFIFOOrder proves waiters are granted in arrival order. The
// per-model election gate serializes same-model contenders before they
// reach the slot queue (as with the create/chat gates today), so the test
// stages arrivals: A parks, the holder releases, A is granted, then B
// arrives and must wait behind A — B is granted only after A releases.
func TestRouteSmartFIFOOrder(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newSmartTestPool(t, func(c *config.Config) { c.TokenMaxConcurrent = 1 }, mock)
	entry := smartEntry(p, 0)

	holder, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		lease *Lease
		err   error
	}
	aCh := make(chan result, 1)
	go func() {
		lease, err := p.Acquire(context.Background(), modelA)
		aCh <- result{lease, err}
	}()
	eventually(t, "waiter A parks", func() bool { return p.routeSlotQueued(entry) == 1 })

	p.LeaseRelease(holder)
	var leaseA *Lease
	select {
	case r := <-aCh:
		if r.err != nil {
			t.Fatalf("waiter A err = %v", r.err)
		}
		leaseA = r.lease
	case <-time.After(5 * time.Second):
		t.Fatal("waiter A never granted")
	}

	bCh := make(chan result, 1)
	go func() {
		lease, err := p.Acquire(context.Background(), modelA)
		bCh <- result{lease, err}
	}()
	eventually(t, "waiter B parks behind A", func() bool { return p.routeSlotQueued(entry) == 1 })
	select {
	case r := <-bCh:
		t.Fatalf("waiter B granted before A released: %v %v", r.lease, r.err)
	default:
	}
	p.LeaseRelease(leaseA)
	select {
	case r := <-bCh:
		if r.err != nil {
			t.Fatalf("waiter B err = %v", r.err)
		}
		p.LeaseRelease(r.lease)
	case <-time.After(5 * time.Second):
		t.Fatal("waiter B never granted after A released")
	}
	eventually(t, "slots drain", func() bool { return p.routeSlotLive(entry) == 0 })
}

// TestRouteSmartQueueWaitExpiry proves a parked waiter fails over with the
// existing 429 shape once QUEUE_WAIT elapses.
func TestRouteSmartQueueWaitExpiry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newSmartTestPool(t, func(c *config.Config) {
		c.TokenMaxConcurrent = 1
		c.QueueWait = 120 * time.Millisecond
	}, mock)
	entry := smartEntry(p, 0)

	holder, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	defer p.LeaseRelease(holder)

	_, err = p.Acquire(context.Background(), modelA)
	if err == nil {
		t.Fatal("waiter acquire succeeded, want queue-wait expiry")
	}
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("expiry err = %T %v, want *upstream.RateLimitError", err, err)
	}
	if !strings.Contains(rle.Body, "queue wait") {
		t.Errorf("expiry body = %q, want queue-wait wording", rle.Body)
	}
	if rle.RetryAfter <= 0 {
		t.Error("expiry 429 carries no Retry-After hint")
	}
	if got := p.routeSlotLive(entry); got != 1 {
		t.Errorf("live slots after expiry = %d, want 1", got)
	}
	p.LeaseRelease(holder)
}

// TestRouteSmartQueueOverflow429 proves a full queue fails over at once
// with the existing 429 shape while the parked waiter still waits.
func TestRouteSmartQueueOverflow429(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newSmartTestPool(t, func(c *config.Config) {
		c.TokenMaxConcurrent = 1
		c.QueueDepth = 1
	}, mock)
	entry := smartEntry(p, 0)

	holder, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		lease *Lease
		err   error
	}
	parkedCh := make(chan result, 1)
	go func() {
		lease, err := p.Acquire(context.Background(), modelA)
		parkedCh <- result{lease, err}
	}()
	eventually(t, "waiter parks", func() bool { return p.routeSlotQueued(entry) == 1 })

	// The overflow contender asks for a DIFFERENT model: the per-model
	// election gate serializes same-model contenders before they reach
	// the slot queue (as with the create/chat gates today), while the
	// live-turn slot itself is per-token — modelB contends the same full
	// slot through its own gate and must fail over at once.
	_, err = p.Acquire(context.Background(), modelB)
	if err == nil {
		t.Fatal("overflow acquire succeeded, want immediate 429")
	}
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("overflow err = %T %v, want *upstream.RateLimitError", err, err)
	}
	if !strings.Contains(rle.Body, "queue full") {
		t.Errorf("overflow body = %q, want queue-full wording", rle.Body)
	}
	select {
	case r := <-parkedCh:
		t.Fatalf("parked waiter returned early: %v %v", r.lease, r.err)
	default:
	}

	p.LeaseRelease(holder)
	select {
	case r := <-parkedCh:
		if r.err != nil {
			t.Fatalf("parked waiter err = %v", r.err)
		}
		p.LeaseRelease(r.lease)
	case <-time.After(5 * time.Second):
		t.Fatal("parked waiter never granted after release")
	}
}

// TestRouteSlotSignals unit-proves the gate: fast-path grant, timeout and
// overflow reasons, and caller-ctx expiry.
func TestRouteSlotSignals(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newSmartTestPool(t, nil, mock)
	entry := smartEntry(p, 0)
	ctx := context.Background()

	held, parked, err := p.routeSlotAcquire(ctx, entry, 1, 1, 16, 30*time.Second)
	if err != nil || parked || held == nil {
		t.Fatalf("fast path = %v/%v/%v, want permit/no-park/nil", held, parked, err)
	}
	// Timeout signal.
	_, parked, err = p.routeSlotAcquire(ctx, entry, 1, 1, 16, 60*time.Millisecond)
	var qerr *routeQueueExhaustedError
	if !errors.As(err, &qerr) || qerr.Reason != "timeout" {
		t.Fatalf("wait expiry = %v, want timeout queue-exhausted", err)
	}
	if !parked {
		t.Error("expiry parked = false, want true (the caller waited)")
	}
	// Overflow signal: one parked waiter fills depth 1.
	type wresult struct {
		permit *routeSlotPermit
		err    error
	}
	waiting := make(chan wresult, 1)
	go func() {
		permit, _, err := p.routeSlotAcquire(ctx, entry, 1, 1, 1, 30*time.Second)
		waiting <- wresult{permit, err}
	}()
	eventually(t, "gate waiter parks", func() bool { return p.routeSlotQueued(entry) == 1 })
	_, parked, err = p.routeSlotAcquire(ctx, entry, 1, 1, 1, 30*time.Second)
	if !errors.As(err, &qerr) || qerr.Reason != "full" {
		t.Fatalf("overflow = %v, want full queue-exhausted", err)
	}
	if parked {
		t.Error("overflow parked = true, want false (immediate failover)")
	}
	// Caller ctx expiry surfaces ctx.Err, never the queue signal.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, _, err = p.routeSlotAcquire(cancelled, entry, 1, 1, 16, 30*time.Second)
	if !errors.Is(err, context.Canceled) || routeIsQueueExhausted(err) {
		t.Fatalf("cancelled wait = %v, want context.Canceled", err)
	}
	held.Release()
	select {
	case wr := <-waiting:
		if wr.err != nil {
			t.Fatalf("parked waiter err = %v", wr.err)
		}
		if wr.permit == nil {
			t.Fatal("parked waiter granted no permit")
		}
		wr.permit.Release()
	case <-time.After(5 * time.Second):
		t.Fatal("parked waiter never granted")
	}
	eventually(t, "slots drain", func() bool { return p.routeSlotLive(entry) == 0 })
}

// TestRoutingSmartOffByteIdentical proves the master-off path behaves
// exactly like today: cold round-robin order, same admission counts, no
// slot state, and leases carry no slot permit.
func TestRoutingSmartOffByteIdentical(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newTestPoolCfg(t, func(c *config.Config) { c.RoutingSmart = false }, mock0, mock1)

	const n = 6
	got := make([]int, n)
	for i := range n {
		toks := p.roster.Load()
		(*toks)[0].session.Invalidate()
		(*toks)[1].session.Invalidate()
		lease, err := p.Acquire(context.Background(), modelA)
		if err != nil {
			t.Fatal(err)
		}
		got[i] = lease.Token
		if lease.routeSlot != nil {
			t.Errorf("off-path lease carries a slot permit")
		}
		p.LeaseRelease(lease)
	}
	for i, want := range []int{0, 1, 0, 1, 0, 1} {
		if got[i] != want {
			t.Errorf("acquire %d token = %d, want %d", i, got[i], want)
		}
	}
	if mock0.SessionCreates != 3 || mock1.SessionCreates != 3 {
		t.Errorf("session creates = %d/%d, want 3/3", mock0.SessionCreates, mock1.SessionCreates)
	}
	p.routeMu.Lock()
	slots := len(p.routeSlots)
	prev := p.routePrev
	p.routeMu.Unlock()
	if slots != 0 || prev != nil {
		t.Errorf("off-path route state: slots=%d prev=%v, want zero", slots, prev)
	}
}

// TestRouteSmartDrainParityNoPressure proves drain == today under no
// pressure: the smart path reproduces the legacy cold round-robin order
// and admission counts exactly.
func TestRouteSmartDrainParityNoPressure(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newSmartTestPool(t, nil, mock0, mock1)

	const n = 6
	got := make([]int, n)
	for i := range n {
		toks := p.roster.Load()
		(*toks)[0].session.Invalidate()
		(*toks)[1].session.Invalidate()
		lease, err := p.Acquire(context.Background(), modelA)
		if err != nil {
			t.Fatal(err)
		}
		got[i] = lease.Token
		p.LeaseRelease(lease)
	}
	for i, want := range []int{0, 1, 0, 1, 0, 1} {
		if got[i] != want {
			t.Errorf("acquire %d token = %d, want %d", i, got[i], want)
		}
	}
	if mock0.SessionCreates != 3 || mock1.SessionCreates != 3 {
		t.Errorf("session creates = %d/%d, want 3/3", mock0.SessionCreates, mock1.SessionCreates)
	}
}

// TestRouteSmartHotStickiness proves the smart path keeps hot-session-first
// reuse: successive acquires land on the live session.
func TestRouteSmartHotStickiness(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newSmartTestPool(t, nil, mock0, mock1)

	for i := range 4 {
		lease, err := p.Acquire(context.Background(), modelA)
		if err != nil {
			t.Fatal(err)
		}
		if lease.Token != 0 {
			t.Errorf("acquire %d token = %d, want 0 (hot reuse)", i, lease.Token)
		}
		p.LeaseRelease(lease)
	}
	if mock1.SessionCreates != 0 {
		t.Errorf("cold token session creates = %d, want 0", mock1.SessionCreates)
	}
}

// TestRouteScorerWeights pins the approved weights on controlled state:
// affinity +1000 (drain-only, hot-gated), hot +200, warm +50, 429 backoff
// -200, transient -50 decaying, anti-clump -25, plus every disqualifier.
func TestRouteScorerWeights(t *testing.T) {
	newPool := func(t *testing.T, mut func(*config.Config)) *Pool {
		t.Helper()
		mock0 := testutil.NewMock()
		t.Cleanup(mock0.Close)
		mock1 := testutil.NewMock()
		t.Cleanup(mock1.Close)
		return newSmartTestPool(t, mut, mock0, mock1)
	}
	scoreOf := func(p *Pool, idx int, model string) (int, bool) {
		t.Helper()
		toks := p.roster.Load()
		s, ok, _ := p.routeScore(p.cfg.Load(), toks, idx, model, time.Now())
		return s, ok
	}

	t.Run("cold fresh pool scores zero", func(t *testing.T) {
		p := newPool(t, nil)
		for i := range 2 {
			if s, ok := scoreOf(p, i, modelA); !ok || s != 0 {
				t.Errorf("token %d = (%d,%v), want (0,true)", i, s, ok)
			}
		}
	})

	t.Run("hot last-used token earns affinity+hot+warm minus anti-clump", func(t *testing.T) {
		p := newPool(t, nil)
		lease, err := p.Acquire(context.Background(), modelA)
		if err != nil {
			t.Fatal(err)
		}
		p.LeaseRelease(lease)
		// 1000 affinity + 200 hot + 50 warm - 25 anti-clump (prev turn).
		if s, ok := scoreOf(p, 0, modelA); !ok || s != 1225 {
			t.Errorf("hot token = (%d,%v), want (1225,true)", s, ok)
		}
		if s, ok := scoreOf(p, 1, modelA); !ok || s != 0 {
			t.Errorf("cold token = (%d,%v), want (0,true)", s, ok)
		}
	})

	t.Run("mismatched hot session earns only anti-clump", func(t *testing.T) {
		p := newPool(t, nil)
		lease, err := p.Acquire(context.Background(), modelA)
		if err != nil {
			t.Fatal(err)
		}
		p.LeaseRelease(lease)
		// Token 0 is hot for modelA and served the previous turn: for a
		// different model it is neither hot nor warm — only anti-clump.
		if s, ok := scoreOf(p, 0, modelB); !ok || s != routeWeightAntiClump {
			t.Errorf("mismatched token = (%d,%v), want (%d,true)", s, ok, routeWeightAntiClump)
		}
	})

	t.Run("cooling token disqualified", func(t *testing.T) {
		p := newPool(t, nil)
		p.CooldownToken(0, time.Minute)
		if _, ok := scoreOf(p, 0, modelA); ok {
			t.Error("cooling token eligible, want disqualified")
		}
		if err := p.UnlockToken(0); err != nil {
			t.Fatal(err)
		}
		if _, ok := scoreOf(p, 0, modelA); !ok {
			t.Error("unlocked token disqualified, want eligible")
		}
	})

	t.Run("backed-off token stays eligible with minus 200", func(t *testing.T) {
		p := newPool(t, nil)
		before, ok := scoreOf(p, 0, modelA)
		if !ok {
			t.Fatal("token ineligible before backoff")
		}
		rle := &upstream.RateLimitError{
			Status: "rate_limited", Model: modelB, RetryAfter: time.Minute,
			Limit: 10, RecentCount: 10, Body: "session quota exhausted for model",
		}
		p.CooldownTokenRateLimit(0, rle)
		after, ok := scoreOf(p, 0, modelA)
		if !ok {
			t.Fatal("exempted backed-off token disqualified, want eligible with -200")
		}
		if after != before+routeWeightBackoff429 {
			t.Errorf("backed-off score = %d, want %d", after, before+routeWeightBackoff429)
		}
		// Same-model quota exhaustion is NOT exempt: disqualified.
		rleSame := &upstream.RateLimitError{
			Status: "rate_limited", Model: modelA, RetryAfter: time.Minute,
			Limit: 10, RecentCount: 10, Body: "session quota exhausted for model",
		}
		p.CooldownTokenRateLimit(0, rleSame)
		if _, ok := scoreOf(p, 0, modelA); ok {
			t.Error("same-model capped token eligible, want disqualified")
		}
	})

	t.Run("transient penalty decays", func(t *testing.T) {
		p := newPool(t, nil)
		entry := smartEntry(p, 1)
		p.routeNoteTransient(entry)
		if s, _ := scoreOf(p, 1, modelA); s != routeWeightTransient {
			t.Errorf("fresh transient = %d, want %d", s, routeWeightTransient)
		}
		entry.routeTransientAt.Store(time.Now().Add(-2 * time.Minute).UnixNano())
		if s, _ := scoreOf(p, 1, modelA); s != routeWeightTransient/2 {
			t.Errorf("aged transient = %d, want %d", s, routeWeightTransient/2)
		}
		entry.routeTransientAt.Store(time.Now().Add(-6 * time.Minute).UnixNano())
		if s, _ := scoreOf(p, 1, modelA); s != 0 {
			t.Errorf("expired transient = %d, want 0", s)
		}
	})

	t.Run("locked and quarantined tokens disqualified", func(t *testing.T) {
		p := newPool(t, nil)
		if err := p.LockToken(1); err != nil {
			t.Fatal(err)
		}
		if _, ok := scoreOf(p, 1, modelA); ok {
			t.Error("locked token eligible, want disqualified")
		}
		if err := p.UnlockLockToken(1); err != nil {
			t.Fatal(err)
		}
		p.quarantineToken(smartEntry(p, 0), "banned", &upstream.BanError{Body: "test"})
		if _, ok := scoreOf(p, 0, modelA); ok {
			t.Error("quarantined token eligible, want disqualified")
		}
		if err := p.UnlockToken(0); err != nil {
			t.Fatal(err)
		}
		if _, ok := scoreOf(p, 0, modelA); !ok {
			t.Error("unlocked token disqualified, want eligible")
		}
	})

	t.Run("daily cap disqualifies", func(t *testing.T) {
		p := newPool(t, func(c *config.Config) { c.MaxMessagesPerDay = 1 })
		p.recordChat(0)
		if _, ok := scoreOf(p, 0, modelA); ok {
			t.Error("capped token eligible, want disqualified")
		}
		if _, ok := scoreOf(p, 1, modelA); !ok {
			t.Error("uncapped token disqualified, want eligible")
		}
	})
}

// TestRouteSmartFreeSlotDeferral proves a full token sorts behind free
// tokens yet stays waitable: the head is the free token, and it returns to
// the front once its slot frees.
func TestRouteSmartFreeSlotDeferral(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newSmartTestPool(t, func(c *config.Config) { c.TokenMaxConcurrent = 1 }, mock0, mock1)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Token != 0 {
		t.Fatalf("first lease token = %d, want 0", lease.Token)
	}
	toks := p.roster.Load()
	base, _ := p.acquireOrder(toks, 0, modelA)
	ranked := p.routeSmartRank(p.cfg.Load(), toks, base, modelA)
	if len(ranked) != 2 || ranked[0] != 1 || ranked[1] != 0 {
		t.Fatalf("ranked full-first = %v, want [1 0]", ranked)
	}
	p.LeaseRelease(lease)
	ranked = p.routeSmartRank(p.cfg.Load(), toks, base, modelA)
	if len(ranked) != 2 || ranked[0] != 0 {
		t.Fatalf("ranked freed = %v, want head 0", ranked)
	}
}

// TestRouteSmartNonDrainKeepsOrder proves non-drain strategies keep today's
// order in step 1 even with hot sessions and last-used state (strategy
// reduction lands in step 2).
func TestRouteSmartNonDrainKeepsOrder(t *testing.T) {
	for _, rot := range []string{"round_robin", "least_used", "random"} {
		t.Run(rot, func(t *testing.T) {
			mock0 := testutil.NewMock()
			defer mock0.Close()
			mock1 := testutil.NewMock()
			defer mock1.Close()
			p := newSmartTestPool(t, func(c *config.Config) { c.TokenRotation = rot }, mock0, mock1)
			lease, err := p.Acquire(context.Background(), modelA)
			if err != nil {
				t.Fatal(err)
			}
			p.LeaseRelease(lease)
			toks := p.roster.Load()
			cfg := p.cfg.Load()
			for _, start := range []int{0, 1} {
				base, _ := p.acquireOrder(toks, start, modelA)
				ranked := p.routeSmartRank(cfg, toks, base, modelA)
				if len(ranked) != len(base) {
					t.Fatalf("ranked len = %d, want %d", len(ranked), len(base))
				}
				for i := range base {
					if ranked[i] != base[i] {
						t.Fatalf("ranked = %v, want base %v", ranked, base)
					}
				}
			}
		})
	}
}

// TestRouteSmartAllCappedDegrades proves total exhaustion still surfaces
// the honest 429 bucket (the rank degrades to the legacy order so the loop
// records every reason).
func TestRouteSmartAllCappedDegrades(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newSmartTestPool(t, nil, mock0, mock1)

	rle := &upstream.RateLimitError{
		Status: "rate_limited", Model: modelA, RetryAfter: time.Minute,
		Limit: 10, RecentCount: 10, Body: "session quota exhausted for model",
	}
	p.CooldownTokenRateLimit(0, rle)
	p.CooldownTokenRateLimit(1, rle)
	_, err := p.Acquire(context.Background(), modelA)
	if err == nil {
		t.Fatal("acquire succeeded on a fully capped pool")
	}
	var got *upstream.RateLimitError
	if !errors.As(err, &got) {
		t.Fatalf("exhausted err = %T %v, want *upstream.RateLimitError", err, err)
	}
}

// TestRouteSmartTransientHookFires proves a transport-transient admission
// failure is remembered for the decaying penalty while failover still
// serves the request on the next token.
func TestRouteSmartTransientHookFires(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}
	mock1 := testutil.NewMock()
	defer mock1.Close()
	p := newSmartTestPool(t, nil, mock0, mock1)

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	defer p.LeaseRelease(lease)
	if lease.Token != 1 {
		t.Fatalf("lease token = %d, want 1 (failover past the failing token)", lease.Token)
	}
	if got := smartEntry(p, 0).routeTransientCount.Load(); got < 1 {
		t.Errorf("transient count = %d, want >= 1", got)
	}
}

// TestRouteSlotParamsFloors pins the defensive floors for hand-built
// configs that bypass Load (nil and zero configs).
func TestRouteSlotParamsFloors(t *testing.T) {
	cap, depth, wait := routeSlotParams(nil)
	if cap != 2 || depth != 16 || wait != 30*time.Second {
		t.Errorf("nil params = %d/%d/%v, want 2/16/30s", cap, depth, wait)
	}
	cap, depth, wait = routeSlotParams(&config.Config{})
	if cap != 1 || depth != 0 || wait != 30*time.Second {
		t.Errorf("zero params = %d/%d/%v, want 1/0/30s", cap, depth, wait)
	}
	cap, depth, wait = routeSlotParams(&config.Config{TokenMaxConcurrent: 3, QueueDepth: 5, QueueWait: 7 * time.Second})
	if cap != 3 || depth != 5 || wait != 7*time.Second {
		t.Errorf("explicit params = %d/%d/%v, want 3/5/7s", cap, depth, wait)
	}
}
