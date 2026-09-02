package pool

import (
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// TestAcquireOrderPrefersAvailableToken pins the rate-limit smart selection
// (issue: auto-switch + search): when one token is cooling down and another
// is available, the ORDER runs the available token first so the failover
// loop reaches it immediately instead of burning an attempt on the cooled
// account. The unavailable token is still in the order (its error is
// recorded when every available token fails), just demoted.
func TestAcquireOrderPrefersAvailableToken(t *testing.T) {
	mock0 := testutil.NewMock() // token 0: cooling down (rate limited)
	defer mock0.Close()
	mock1 := testutil.NewMock() // token 1: available
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)

	// Cool token 0 for the requested model.
	toks := p.roster.Load()
	(*toks)[0].runs.CooldownRateLimit(&upstream.RateLimitError{
		Status:     "rate_limited",
		Model:      modelA,
		RetryAfter: 15 * time.Minute,
	})

	order, _ := p.acquireOrder(toks, 0, modelA)
	if len(order) != 2 {
		t.Fatalf("order len = %d, want 2 (both tokens visited; errors recorded)", len(order))
	}
	if order[0] != 1 {
		t.Errorf("order[0] = %d, want 1 (available token first)", order[0])
	}
	if order[1] != 0 {
		t.Errorf("order[1] = %d, want 0 (cooled token demoted, still visited)", order[1])
	}
}

// TestAcquireOrderDemotesBannedToken pins the same demotion for a banned
// token: it loses to an available one in the visit order.
func TestAcquireOrderDemotesBannedToken(t *testing.T) {
	mock0 := testutil.NewMock() // token 0: banned
	defer mock0.Close()
	mock1 := testutil.NewMock() // token 1: available
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)

	toks := p.roster.Load()
	(*toks)[0].runs.CooldownBan(&upstream.BanError{ResumesAt: time.Now().Add(time.Hour)})

	order, _ := p.acquireOrder(toks, 0, modelA)
	if len(order) != 2 {
		t.Fatalf("order len = %d, want 2", len(order))
	}
	if order[0] != 1 {
		t.Errorf("order[0] = %d, want 1 (available token first)", order[0])
	}
}

// TestAcquireOrderEarliestUnblockFirst pins the tie-break among unavailable
// tokens: the one whose cooldown unblocks earliest is visited first, so a
// fully-limited pool surfaces the shortest retry window.
func TestAcquireOrderEarliestUnblockFirst(t *testing.T) {
	mock0 := testutil.NewMock() // token 0: long cooldown (30 min)
	defer mock0.Close()
	mock1 := testutil.NewMock() // token 1: short cooldown (5 min)
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)

	toks := p.roster.Load()
	(*toks)[0].runs.Cooldown(30 * time.Minute)
	(*toks)[1].runs.Cooldown(5 * time.Minute)

	order, _ := p.acquireOrder(toks, 0, modelA)
	if len(order) != 2 {
		t.Fatalf("order len = %d, want 2", len(order))
	}
	if order[0] != 1 {
		t.Errorf("order[0] = %d, want 1 (earliest unblock first)", order[0])
	}
}

// TestAcquireOrderModelExemptionKeepsTokenAvailable pins the per-model
// exemption (mirrors leaseFromOrder): a cooldown caused by a DIFFERENT
// model's quota exhaustion leaves the token available for the requested
// model, so it is NOT demoted.
func TestAcquireOrderModelExemptionKeepsTokenAvailable(t *testing.T) {
	mock0 := testutil.NewMock() // token 0: quota-capped on a different model
	defer mock0.Close()
	mock1 := testutil.NewMock() // token 1: also cold
	defer mock1.Close()
	p := newTestPool(t, mock0, mock1)

	toks := p.roster.Load()
	// Quota-shaped refusal (pacific_day period + recent>=limit) on a
	// DIFFERENT model: exactly the case leaseFromOrder exempts.
	(*toks)[0].runs.CooldownRateLimit(&upstream.RateLimitError{
		Status:      "rate_limited",
		Model:       "another/model",
		Period:      "pacific_day",
		Limit:       4,
		RecentCount: 4,
	})

	order, _ := p.acquireOrder(toks, 0, modelA)
	if len(order) != 2 {
		t.Fatalf("order len = %d, want 2", len(order))
	}
	if order[0] != 0 {
		t.Errorf("order[0] = %d, want 0 (exempt for requested model — keeps hot-first rank)", order[0])
	}
}
