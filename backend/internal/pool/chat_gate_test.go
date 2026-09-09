// chat_gate_test.go — per-(token,model) in-flight chat lease cap (burst
// queue) coverage: metered serialization, unmetered cap, expired-context
// wait-or-503 with zero leaked inflight, cancel release, caps-off passthrough,
// and the bridge-mode mirror. All hermetic (mock upstream only).
package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
)

// newChatGatePool wires a single-token pool with the burst-queue caps set
// and enough mock run IDs for the burst.
func newChatGatePool(t *testing.T, metered, unmetered int) (*Pool, *testutil.MockUpstream) {
	t.Helper()
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	ids := make([]string, 16)
	for i := range ids {
		ids[i] = fmt.Sprintf("chatgate-run-%04d", i)
	}
	mock.RunIDs = ids
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.ChatMaxInflightMetered = metered
		c.ChatMaxInflightUnmetered = unmetered
	}, mock)
	return p, mock
}

// chatLane resolves the gate lane for token idx + model.
func chatLane(p *Pool, idx int, model string) chatGateKey {
	toks := p.roster.Load()
	return chatGateKey{owner: (*toks)[idx], model: model}
}

// seedChatMetered prices modelA on token 0 so it classifies metered.
func seedChatMetered(t *testing.T, p *Pool) {
	t.Helper()
	seedBurstQuota(t, p, 0, 10, time.Now().Add(time.Hour))
}

// runsInflight reports token 0's run inflight count.
func runsInflight(p *Pool) int {
	toks := p.roster.Load()
	return (*toks)[0].runs.InflightCount()
}

// TestChatGateSerializesMeteredBurst pins the core contract: 5 concurrent
// same-token metered acquires with cap 1 all succeed, never hold more than
// one lane slot at once (upstream-parallelism 1), and drain clean.
func TestChatGateSerializesMeteredBurst(t *testing.T) {
	p, _ := newChatGatePool(t, 1, 3)
	seedChatMetered(t, p)
	lane := chatLane(p, 0, modelA)
	first, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	const waiters = 4
	var wg sync.WaitGroup
	errs := make([]error, waiters)
	var chatCur, chatMax atomic.Int32
	for i := range waiters {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			l, err := p.Acquire(ctx, modelA)
			if err != nil {
				errs[i] = err
				return
			}
			// Full path through the leased token while holding the lane.
			n := chatCur.Add(1)
			for {
				m := chatMax.Load()
				if n <= m || chatMax.CompareAndSwap(m, n) {
					break
				}
			}
			chatOnce(t, p, l)
			chatCur.Add(-1)
			p.LeaseRelease(l)
		}(i)
	}

	// The leader parks on the lane (followers queue behind it on the
	// model leader-election gate, not here) — releasing the first lease
	// then lets the grants chain one by one. Waiting for all four here
	// would deadlock: the followers only reach this gate after the leader
	// is granted.
	eventually(t, "leader parked on chat lane", func() bool {
		return p.chatGate.parkedWaiters() >= 1
	})
	p.LeaseRelease(first)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for serialized acquires")
	}

	for i, err := range errs {
		if err != nil {
			t.Errorf("waiter %d: %v", i, err)
		}
	}
	if got := p.chatGate.peakHeld(lane); got != 1 {
		t.Errorf("lane peak = %d, want 1 (metered cap serializes)", got)
	}
	if got := chatMax.Load(); got != 1 {
		t.Errorf("max concurrent chats = %d, want 1", got)
	}
	if got := p.chatGate.totalHeld(); got != 0 {
		t.Errorf("total held after drain = %d, want 0", got)
	}
	if got := runsInflight(p); got != 0 {
		t.Errorf("run inflight after drain = %d, want 0", got)
	}
}

// TestChatGateUnmeteredCapThree pins the unmetered lane: cap 3 holds three
// concurrent leases, and a fourth parks until its deadline (wait-or-503)
// without leaking its run.
func TestChatGateUnmeteredCapThree(t *testing.T) {
	p, _ := newChatGatePool(t, 1, 3)
	lane := chatLane(p, 0, modelA) // unpriced: no quota seed, unmetered

	var held []*Lease
	for i := range 3 {
		l, err := p.Acquire(context.Background(), modelA)
		if err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		held = append(held, l)
	}
	if got := p.chatGate.held(lane); got != 3 {
		t.Fatalf("lane held = %d, want 3", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := p.Acquire(ctx, modelA)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fourth Acquire err = %v, want context.DeadlineExceeded", err)
	}
	// The failed waiter released its run: only the three holders remain.
	if got := runsInflight(p); got != 3 {
		t.Errorf("run inflight after expired wait = %d, want 3 (no leak)", got)
	}
	if got := p.chatGate.peakHeld(lane); got != 3 {
		t.Errorf("lane peak = %d, want 3", got)
	}

	for _, l := range held {
		p.LeaseRelease(l)
	}
	if got := p.chatGate.totalHeld(); got != 0 {
		t.Errorf("total held after drain = %d, want 0", got)
	}
	if got := runsInflight(p); got != 0 {
		t.Errorf("run inflight after drain = %d, want 0", got)
	}
}

// TestChatGateExpiredCtxNoLeak pins wait-or-503 on a pre-cancelled context:
// instant failure, the holder untouched, nothing leaked.
func TestChatGateExpiredCtxNoLeak(t *testing.T) {
	p, _ := newChatGatePool(t, 1, 3)
	seedChatMetered(t, p)
	lane := chatLane(p, 0, modelA)

	first, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = p.Acquire(ctx, modelA)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Acquire err = %v, want context.Canceled", err)
	}
	if got := p.chatGate.held(lane); got != 1 {
		t.Errorf("lane held = %d, want 1 (holder only)", got)
	}
	if got := runsInflight(p); got != 1 {
		t.Errorf("run inflight = %d, want 1 (no leaked run)", got)
	}

	p.LeaseRelease(first)
	if got := p.chatGate.totalHeld(); got != 0 {
		t.Errorf("total held after drain = %d, want 0", got)
	}
}

// TestChatGateAbandonReleasesSlot pins the cancel path: LeaseAbandon frees
// the lane so the next acquire grants immediately.
func TestChatGateAbandonReleasesSlot(t *testing.T) {
	p, _ := newChatGatePool(t, 1, 3)
	seedChatMetered(t, p)
	lane := chatLane(p, 0, modelA)

	first, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	p.LeaseAbandon(first)
	if got := p.chatGate.held(lane); got != 0 {
		t.Fatalf("lane held after abandon = %d, want 0", got)
	}

	second, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("Acquire after abandon: %v", err)
	}
	p.LeaseRelease(second)
	if got := p.chatGate.totalHeld(); got != 0 {
		t.Errorf("total held after drain = %d, want 0", got)
	}
}

// TestChatGateZeroCapsUnlimited pins the knobs-off contract: caps 0 grant
// untracked permits — everything succeeds with nothing held on the gate.
func TestChatGateZeroCapsUnlimited(t *testing.T) {
	p, _ := newChatGatePool(t, 0, 0)
	seedChatMetered(t, p)
	lane := chatLane(p, 0, modelA)

	var held []*Lease
	for i := range 5 {
		l, err := p.Acquire(context.Background(), modelA)
		if err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		held = append(held, l)
	}
	if got := p.chatGate.totalHeld(); got != 0 {
		t.Errorf("total held with caps off = %d, want 0 (untracked)", got)
	}
	if got := p.chatGate.peakHeld(lane); got != 0 {
		t.Errorf("lane peak with caps off = %d, want 0 (untracked)", got)
	}
	for _, l := range held {
		p.LeaseRelease(l)
	}
	if got := runsInflight(p); got != 0 {
		t.Errorf("run inflight after drain = %d, want 0", got)
	}
}

// TestChatGateWakeupGrantsWaiter pins the broadcast wakeup at gate level:
// a parked waiter is granted exactly the freed slot, flagged as waited.
func TestChatGateWakeupGrantsWaiter(t *testing.T) {
	g := newChatGate()
	key := chatGateKey{owner: "tok", model: "m"}

	first, waited, err := g.acquire(context.Background(), key.owner, key.model, 1)
	if err != nil || waited {
		t.Fatalf("first acquire = (%v, waited=%v), want (permit, false)", err, waited)
	}

	type result struct {
		permit *chatPermit
		waited bool
		err    error
	}
	resCh := make(chan result, 1)
	go func() {
		permit, waited, err := g.acquire(context.Background(), key.owner, key.model, 1)
		resCh <- result{permit, waited, err}
	}()
	eventually(t, "waiter parked", func() bool { return g.parkedWaiters() == 1 })

	first.Release()

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("waiter err = %v", res.err)
		}
		if !res.waited {
			t.Error("waiter granted without parking flag (dispatch jitter skipped)")
		}
		if got := g.held(key); got != 1 {
			t.Errorf("lane held = %d, want 1", got)
		}
		res.permit.Release()
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never granted after release")
	}
	if got := g.totalHeld(); got != 0 {
		t.Errorf("total held after drain = %d, want 0", got)
	}
}

// TestChatGateBridgeUnmeteredCap pins the bridge-mode mirror: three
// concurrent leases on one client entry (unmetered) hold, a fourth waits out
// its deadline, and everything drains clean.
func TestChatGateBridgeUnmeteredCap(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ids := make([]string, 8)
	for i := range ids {
		ids[i] = fmt.Sprintf("bridge-chatgate-run-%04d", i)
	}
	mock.RunIDs = ids
	p := newBridgePool(t, mock)
	cfg := p.cfg.Load()
	cfg.ChatMaxInflightMetered = 1
	cfg.ChatMaxInflightUnmetered = 3
	p.cfg.Store(cfg)

	const client = "bridge-chatgate-client"
	var held []*Lease
	for i := range 3 {
		l, err := p.AcquireBridge(context.Background(), client, modelA)
		if err != nil {
			t.Fatalf("AcquireBridge %d: %v", i, err)
		}
		held = append(held, l)
	}
	if got := p.chatGate.totalHeld(); got != 3 {
		t.Fatalf("total held = %d, want 3", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := p.AcquireBridge(ctx, client, modelA); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fourth AcquireBridge err = %v, want context.DeadlineExceeded", err)
	}

	for _, l := range held {
		p.LeaseRelease(l)
	}
	if got := p.chatGate.totalHeld(); got != 0 {
		t.Errorf("total held after drain = %d, want 0", got)
	}
}
