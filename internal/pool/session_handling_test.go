// session_handling_test.go — regression tests for Issue #191 (better session handling,
// model stickiness, concurrent admission deduplication, and grace drain reuse).
package pool

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/internal/testutil"
)

// TestAcquireScarceModelSessionStickiness verifies Issue #191:
// When multiple tokens are configured, multi-turn requests for a scarce model
// (e.g. deepseek/deepseek-v4-pro) must stick to the active session on Token 0
// and must NOT trigger a second session creation on Token 1.
func TestAcquireScarceModelSessionStickiness(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()

	p := newTestPool(t, mock0, mock1)
	const scarceModel = "deepseek/deepseek-v4-pro"

	// Request 1: cold pool admits scarce session on Token 0.
	lease1, err := p.Acquire(context.Background(), scarceModel)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	if lease1.Token != 0 {
		t.Fatalf("first lease token = %d, want 0", lease1.Token)
	}
	p.LeaseRelease(lease1)

	if mock0.SessionCreates != 1 {
		t.Fatalf("mock0 session creates = %d, want 1", mock0.SessionCreates)
	}
	if mock1.SessionCreates != 0 {
		t.Fatalf("mock1 session creates = %d, want 0", mock1.SessionCreates)
	}

	// Requests 2..6: successive turns for the same scarce model must reuse Token 0.
	for i := range 5 {
		lease, err := p.Acquire(context.Background(), scarceModel)
		if err != nil {
			t.Fatalf("turn %d acquire failed: %v", i+2, err)
		}
		if lease.Token != 0 {
			t.Errorf("turn %d landed on token %d, want token 0 (model stickiness)", i+2, lease.Token)
		}
		p.LeaseRelease(lease)
	}

	// Token 1 must never have had a session created.
	if mock1.SessionCreates != 0 {
		t.Errorf("mock1 session creates = %d, want 0 (scarce session must not leak to second token)", mock1.SessionCreates)
	}
	if mock0.SessionCreates != 1 {
		t.Errorf("mock0 session creates = %d, want 1 (session reused across all turns)", mock0.SessionCreates)
	}
}

// TestAcquireMatchingHotSessionStickinessEqualQuota verifies that when multiple tokens
// both hold active matching sessions with equal/unknown quota, successive requests stick
// to the last used token instead of round-robin ping-ponging across accounts on every turn.
func TestAcquireMatchingHotSessionStickinessEqualQuota(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()

	p := newTestPool(t, mock0, mock1)

	// Pre-admit active sessions for modelA on BOTH tokens.
	toks := p.toks.Load()
	if _, err := (*toks)[0].session.EnsureSessionForModel(context.Background(), modelA); err != nil {
		t.Fatal(err)
	}
	if _, err := (*toks)[1].session.EnsureSessionForModel(context.Background(), modelA); err != nil {
		t.Fatal(err)
	}

	// First acquire lands on a token and records it as last-used.
	first, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	chosenToken := first.Token
	p.LeaseRelease(first)

	// Successive acquires must stick to the chosen token (no ping-pong).
	for i := range 6 {
		lease, err := p.Acquire(context.Background(), modelA)
		if err != nil {
			t.Fatalf("acquire %d failed: %v", i+1, err)
		}
		if lease.Token != chosenToken {
			t.Errorf("acquire %d landed on token %d, want %d (stickiness with equal quota)", i+1, lease.Token, chosenToken)
		}
		p.LeaseRelease(lease)
	}
}

// TestAcquireConcurrentColdAdmissionSharesSingleFlight verifies that when multiple
// concurrent requests arrive for the same model on a cold pool, they share the single
// in-flight session admission on the leader token rather than firing duplicate session creates
// across multiple tokens.
func TestAcquireConcurrentColdAdmissionSharesSingleFlight(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()

	// Add a slight delay to mock0 session create so the concurrent request lands while refreshing.
	mock0.SessionCreateDelay = 100 * time.Millisecond

	p := newTestPool(t, mock0, mock1)
	const scarceModel = "deepseek/deepseek-v4-pro"

	var wg sync.WaitGroup
	var errors atomic.Int32
	var token0Count atomic.Int32
	var token1Count atomic.Int32

	// Launch 4 concurrent acquires for the same scarce model.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := p.Acquire(context.Background(), scarceModel)
			if err != nil {
				errors.Add(1)
				return
			}
			switch lease.Token {
			case 0:
				token0Count.Add(1)
			case 1:
				token1Count.Add(1)
			}
			p.LeaseRelease(lease)
		}()
	}

	wg.Wait()

	if errors.Load() > 0 {
		t.Fatalf("%d concurrent acquires failed", errors.Load())
	}
	if mock0.SessionCreates != 1 {
		t.Errorf("mock0 session creates = %d, want 1 (single-flight admission)", mock0.SessionCreates)
	}
	if mock1.SessionCreates != 0 {
		t.Errorf("mock1 session creates = %d, want 0 (competing session must not be created)", mock1.SessionCreates)
	}
	if token1Count.Load() != 0 {
		t.Errorf("token 1 leases = %d, want 0", token1Count.Load())
	}
	if token0Count.Load() != 4 {
		t.Errorf("token 0 leases = %d, want 4", token0Count.Load())
	}
}

// TestAcquireReusesGraceDrainSession verifies that a token whose session is past its
// nominal expiresAt but still within the 30-minute grace drain window is recognized as
// matchingHot and reused, avoiding unnecessary fresh session admissions on other tokens.
func TestAcquireReusesGraceDrainSession(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()

	p := newTestPool(t, mock0, mock1)

	// Admit an initial session on Token 0.
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)

	if mock0.SessionCreates != 1 {
		t.Fatalf("mock0 session creates = %d, want 1", mock0.SessionCreates)
	}

	// Manually set Token 0's session to be in grace drain (past expiresAt, but before gracePeriodEndsAt).
	toks := p.toks.Load()
	now := time.Now()
	(*toks)[0].session.SetSessionStateForTest(
		"active",
		"inst-grace-test",
		modelA,
		now.Add(-2*time.Minute), // expired 2m ago
		now.Add(25*time.Minute), // 25m grace remaining
	)

	// Acquire again for modelA: Token 0's grace session should be reused.
	lease2, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("acquire during grace drain failed: %v", err)
	}
	if lease2.Token != 0 {
		t.Errorf("lease token = %d, want 0 (grace drain session must be prioritized)", lease2.Token)
	}
	if lease2.SessionInstanceID != "inst-grace-test" {
		t.Errorf("lease instance = %q, want inst-grace-test", lease2.SessionInstanceID)
	}
	p.LeaseRelease(lease2)

	// Token 1 must remain untouched (0 creates).
	if mock1.SessionCreates != 0 {
		t.Errorf("mock1 session creates = %d, want 0", mock1.SessionCreates)
	}
}
