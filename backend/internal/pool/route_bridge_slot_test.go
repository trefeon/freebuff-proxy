package pool

// Bridge-mode live-turn slot tests: bridge entries are paced by the same
// per-lane slot semaphore + FIFO queue the pooled path uses
// (route_smart.go), keyed off the *bridgeEntry. The three legacy knobs and
// their gates (session-create, chat-lane) are gone, so these tests are the
// only proof that bridge concurrency is bounded at all: cap holds, grant
// order is arrival order, overflow/timeout return the pooled 429 shape,
// every error path returns the slot, and ROUTING_SMART=false restores the
// per-entry admission single-flight as the only pacing.

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

// newBridgeSmartPool is newBridgePool with the smart-routing knobs applied
// through the config pointer (newBridgePool loads no knobs of its own, so
// the smart values must be set explicitly; the mock base URL is carried
// over from the pool's own config). Every knob under test is set here,
// never inferred.
func newBridgeSmartPool(t *testing.T, mut func(*config.Config), mock *testutil.MockUpstream) *Pool {
	t.Helper()
	p := newBridgePool(t, mock)
	cfg := *p.cfg.Load()
	cfg.RoutingSmart = true
	cfg.TokenMaxConcurrent = 2
	cfg.QueueWait = 30 * time.Second
	cfg.QueueDepth = 16
	cfg.TokenRotation = "drain"
	if mut != nil {
		mut(&cfg)
	}
	p.cfg.Store(&cfg)
	return p
}

// bridgeLane resolves the cached bridge entry for token, the same pointer
// the slot map is keyed by.
func bridgeLane(t *testing.T, p *Pool, token string) *bridgeEntry {
	t.Helper()
	entry, err := p.bridgeEntryFor(token)
	if err != nil {
		t.Fatalf("bridgeEntryFor: %v", err)
	}
	return entry
}

// TestBridgeSlotCapHoldsUnderContention proves the cap-2 hard wall on one
// bridge entry: three racing acquires never produce a third live turn —
// the third parks on the FIFO queue until a release wakes it, and every
// granted lease carries its slot permit.
func TestBridgeSlotCapHoldsUnderContention(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgeSmartPool(t, nil, mock)
	const token = "bridge-slot-cap"
	entry := bridgeLane(t, p, token)

	first, err := p.AcquireBridge(context.Background(), token, modelA)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.AcquireBridge(context.Background(), token, modelA)
	if err != nil {
		t.Fatal(err)
	}
	if first.routeSlot == nil || second.routeSlot == nil {
		t.Fatal("granted bridge lease carries no live-turn slot")
	}
	if got := p.routeSlotLive(entry); got != 2 {
		t.Fatalf("live slots = %d, want 2", got)
	}

	thirdCh := make(chan acquireResult, 1)
	go func() {
		lease, err := p.AcquireBridge(context.Background(), token, modelA)
		thirdCh <- acquireResult{lease, err}
	}()
	eventually(t, "third bridge acquire parks", func() bool { return p.routeSlotQueued(entry) == 1 })
	if got := p.routeSlotLive(entry); got != 2 {
		t.Fatalf("live slots while parked = %d, want 2 (never a third live turn)", got)
	}
	select {
	case r := <-thirdCh:
		t.Fatalf("third acquire returned early: lease=%v err=%v", r.lease, r.err)
	case <-time.After(150 * time.Millisecond):
	}

	p.LeaseRelease(first)
	var third *Lease
	select {
	case r := <-thirdCh:
		if r.err != nil {
			t.Fatalf("third acquire err = %v", r.err)
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
	p.routeMu.Lock()
	lanes := len(p.routeSlots)
	p.routeMu.Unlock()
	if lanes != 0 {
		t.Errorf("slot lanes after full release = %d, want 0 (state deleted)", lanes)
	}
}

// TestBridgeSlotFIFOOrder proves waiters are granted in arrival order: with
// the cap at 1 the holder, then waiter A, then waiter B park in sequence,
// and each release hands the slot to the head — B is never granted before A
// releases.
func TestBridgeSlotFIFOOrder(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgeSmartPool(t, func(c *config.Config) { c.TokenMaxConcurrent = 1 }, mock)
	const token = "bridge-slot-fifo"
	entry := bridgeLane(t, p, token)

	holder, err := p.AcquireBridge(context.Background(), token, modelA)
	if err != nil {
		t.Fatal(err)
	}
	aCh := make(chan acquireResult, 1)
	go func() {
		lease, err := p.AcquireBridge(context.Background(), token, modelA)
		aCh <- acquireResult{lease, err}
	}()
	eventually(t, "waiter A parks", func() bool { return p.routeSlotQueued(entry) == 1 })
	bCh := make(chan acquireResult, 1)
	go func() {
		lease, err := p.AcquireBridge(context.Background(), token, modelA)
		bCh <- acquireResult{lease, err}
	}()
	eventually(t, "waiter B parks behind A", func() bool { return p.routeSlotQueued(entry) == 2 })
	if got := p.routeSlotLive(entry); got != 1 {
		t.Fatalf("live slots with two waiters = %d, want 1", got)
	}

	p.LeaseRelease(holder)
	var leaseA *Lease
	select {
	case r := <-aCh:
		if r.err != nil {
			t.Fatalf("waiter A err = %v", r.err)
		}
		leaseA = r.lease
	case <-time.After(5 * time.Second):
		t.Fatal("waiter A never granted (FIFO head)")
	}
	select {
	case r := <-bCh:
		t.Fatalf("waiter B granted before A released: %v %v", r.lease, r.err)
	default:
	}
	if got := p.routeSlotQueued(entry); got != 1 {
		t.Fatalf("queued after A's grant = %d, want 1 (B still parked)", got)
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

// TestBridgeSlotQueueWaitExpiry proves a parked bridge waiter surfaces the
// pooled 429 rate-limit shape once QUEUE_WAIT elapses (bridge has no
// failover, so the client sees it directly), and the holder keeps its slot.
func TestBridgeSlotQueueWaitExpiry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgeSmartPool(t, func(c *config.Config) {
		c.TokenMaxConcurrent = 1
		c.QueueWait = 120 * time.Millisecond
	}, mock)
	const token = "bridge-slot-wait"
	entry := bridgeLane(t, p, token)

	holder, err := p.AcquireBridge(context.Background(), token, modelA)
	if err != nil {
		t.Fatal(err)
	}
	defer p.LeaseRelease(holder)

	_, err = p.AcquireBridge(context.Background(), token, modelA)
	if err == nil {
		t.Fatal("bridge waiter succeeded, want queue-wait expiry")
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
		t.Errorf("live slots after expiry = %d, want 1 (holder keeps its slot)", got)
	}
	if got := p.routeSlotQueued(entry); got != 0 {
		t.Errorf("queued after expiry = %d, want 0 (the waiter dequeued)", got)
	}
}

// TestBridgeSlotQueueOverflow429 proves a full queue fails the newcomer at
// once with the same 429 shape while the parked waiter still waits, and the
// loser leaves no slot behind.
func TestBridgeSlotQueueOverflow429(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgeSmartPool(t, func(c *config.Config) {
		c.TokenMaxConcurrent = 1
		c.QueueDepth = 1
	}, mock)
	const token = "bridge-slot-overflow"
	entry := bridgeLane(t, p, token)

	holder, err := p.AcquireBridge(context.Background(), token, modelA)
	if err != nil {
		t.Fatal(err)
	}
	parkedCh := make(chan acquireResult, 1)
	go func() {
		lease, err := p.AcquireBridge(context.Background(), token, modelA)
		parkedCh <- acquireResult{lease, err}
	}()
	eventually(t, "waiter parks", func() bool { return p.routeSlotQueued(entry) == 1 })

	_, err = p.AcquireBridge(context.Background(), token, modelA)
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
	if got := p.routeSlotLive(entry); got != 1 {
		t.Errorf("live slots after overflow = %d, want 1 (the loser took nothing)", got)
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
	eventually(t, "slots drain", func() bool { return p.routeSlotLive(entry) == 0 })
}

// TestBridgeSlotReleaseOnErrorPaths proves the slot never leaks when a
// granted acquire aborts before returning a lease: the admission failure
// path (upstream 500) and the per-minute refusal path both hand the slot
// back.
func TestBridgeSlotReleaseOnErrorPaths(t *testing.T) {
	t.Run("admission error", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		p := newBridgeSmartPool(t, nil, mock)
		const token = "bridge-slot-admission-err"
		// Cache the entry against the healthy mock (the token probe is a
		// zero-cost GET), then fail every later admission so the slot is
		// taken and the acquire aborts inside the session create.
		entry := bridgeLane(t, p, token)
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
		}

		if _, err := p.AcquireBridge(context.Background(), token, modelA); err == nil {
			t.Fatal("acquire succeeded against a failing admission, want an error")
		}
		if got := p.routeSlotLive(entry); got != 0 {
			t.Errorf("live slots after admission error = %d, want 0 (slot released)", got)
		}
		if got := p.routeSlotQueued(entry); got != 0 {
			t.Errorf("queued after admission error = %d, want 0", got)
		}
	})

	t.Run("per-minute refusal", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		p := newBridgeSmartPool(t, func(c *config.Config) {
			c.TokenMaxConcurrent = 1
			c.MaxRequestsPerMinute = 1
		}, mock)
		const token = "bridge-slot-rpm"
		entry := bridgeLane(t, p, token)

		lease, err := p.AcquireBridge(context.Background(), token, modelA)
		if err != nil {
			t.Fatal(err)
		}
		p.LeaseRelease(lease)
		if got := p.routeSlotLive(entry); got != 0 {
			t.Fatalf("live slots after release = %d, want 0", got)
		}
		_, err = p.AcquireBridge(context.Background(), token, modelA)
		var rle *upstream.RateLimitError
		if !errors.As(err, &rle) {
			t.Fatalf("refusal err = %T %v, want *upstream.RateLimitError", err, err)
		}
		if got := p.routeSlotLive(entry); got != 0 {
			t.Errorf("live slots after per-minute refusal = %d, want 0 (slot released)", got)
		}
	})

	t.Run("cancelled ctx while parked", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		p := newBridgeSmartPool(t, func(c *config.Config) { c.TokenMaxConcurrent = 1 }, mock)
		const token = "bridge-slot-cancel"
		entry := bridgeLane(t, p, token)

		holder, err := p.AcquireBridge(context.Background(), token, modelA)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		waitErr := make(chan error, 1)
		go func() {
			_, err := p.AcquireBridge(ctx, token, modelA)
			waitErr <- err
		}()
		eventually(t, "waiter parks", func() bool { return p.routeSlotQueued(entry) == 1 })
		cancel()
		select {
		case err := <-waitErr:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled wait err = %v, want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("cancelled waiter never returned")
		}
		if got := p.routeSlotQueued(entry); got != 0 {
			t.Errorf("queued after cancel = %d, want 0", got)
		}
		if got := p.routeSlotLive(entry); got != 1 {
			t.Errorf("live slots after cancel = %d, want 1 (holder untouched)", got)
		}
		p.LeaseRelease(holder)
		eventually(t, "slots drain", func() bool { return p.routeSlotLive(entry) == 0 })
	})
}

// TestBridgeSlotSmartOffKeepsSingleFlight proves ROUTING_SMART=false leaves
// bridge exactly as it was: no slot gating (a second concurrent acquire is
// not parked by TOKEN_MAX_CONCURRENT=1), no slot state, no permit on the
// lease, and the per-entry admission single-flight still admits the session
// once for both callers.
func TestBridgeSlotSmartOffKeepsSingleFlight(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newBridgeSmartPool(t, func(c *config.Config) {
		c.RoutingSmart = false
		c.TokenMaxConcurrent = 1
	}, mock)
	const token = "bridge-slot-off"
	entry := bridgeLane(t, p, token)

	type result struct {
		lease *Lease
		err   error
	}
	const n = 3
	results := make(chan result, n)
	for range n {
		go func() {
			lease, err := p.AcquireBridge(context.Background(), token, modelA)
			results <- result{lease, err}
		}()
	}
	leases := make([]*Lease, 0, n)
	for range n {
		select {
		case r := <-results:
			if r.err != nil {
				t.Fatalf("off-path acquire err = %v, want success (no slot gating)", r.err)
			}
			if r.lease.routeSlot != nil {
				t.Error("off-path bridge lease carries a slot permit")
			}
			leases = append(leases, r.lease)
		case <-time.After(5 * time.Second):
			t.Fatal("off-path acquire never returned (slot gating still active)")
		}
	}
	if got := p.routeSlotLive(entry); got != 0 {
		t.Errorf("off-path live slots = %d, want 0 (nothing tracked)", got)
	}
	p.routeMu.Lock()
	lanes := len(p.routeSlots)
	p.routeMu.Unlock()
	if lanes != 0 {
		t.Errorf("off-path slot lanes = %d, want 0", lanes)
	}
	if mock.SessionCreates != 1 {
		t.Errorf("session creates = %d, want 1 (per-entry single-flight preserved)", mock.SessionCreates)
	}
	for _, lease := range leases {
		p.LeaseRelease(lease)
	}
}
