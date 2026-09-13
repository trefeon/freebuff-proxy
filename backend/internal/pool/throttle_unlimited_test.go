package pool

// Unlimited-by-default throttle tests: every local throttle treats 0 as
// unlimited (the upstream quota/429 is the natural brake), while an
// explicit value still enforces. Covers the per-minute gate, the per-day
// gate, the chat gate, the create gate, and the live-turn slot cap.

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

// TestUnlimitedDefaultPerMinuteGate proves a hand-built pool with no caps
// configured (zero values, as Load now produces) never trips the
// per-minute gate: repeated acquires all succeed even past the old cap.
func TestUnlimitedDefaultPerMinuteGate(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := range 5 {
		lease, err := p.Acquire(ctx, modelA)
		if err != nil {
			t.Fatalf("acquire %d err = %v, want success (default unlimited)", i, err)
		}
		p.LeaseRelease(lease)
	}
	if got := p.rpmCount(0); got != 5 {
		t.Errorf("rpmCount = %d, want 5 (admissions still recorded)", got)
	}
}

// TestUnlimitedDefaultPerDayGate proves successful chats never trip the
// daily gate when unconfigured: the day bucket fills but no cap applies.
func TestUnlimitedDefaultPerDayGate(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for range 3 {
		lease, err := p.Acquire(ctx, modelA)
		if err != nil {
			t.Fatal(err)
		}
		chatOnce(t, p, lease)
		p.LeaseRelease(lease)
	}
	if got := p.dayRequestCount(0); got != 3 {
		t.Fatalf("dayRequestCount = %d, want 3", got)
	}
	if _, err := p.Acquire(ctx, modelA); err != nil {
		t.Fatalf("acquire past recorded day usage err = %v, want success (default unlimited)", err)
	}
}

// TestExplicitPerMinuteCapStillEnforces proves opting in still works:
// MAX_REQUESTS_PER_MINUTE=2 refuses the third acquire with the existing
// 429 shape.
func TestExplicitPerMinuteCapStillEnforces(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPoolCfg(t, func(c *config.Config) { c.MaxRequestsPerMinute = 2 }, mock)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := range 2 {
		lease, err := p.Acquire(ctx, modelA)
		if err != nil {
			t.Fatalf("acquire %d err = %v", i, err)
		}
		p.LeaseRelease(lease)
	}
	_, err := p.Acquire(ctx, modelA)
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("third acquire err = %T %v, want *upstream.RateLimitError", err, err)
	}
	if !strings.Contains(err.Error(), "per-minute request limit reached") {
		t.Errorf("error = %q, want per-minute wording", err.Error())
	}
}

// TestExplicitPerDayCapStillEnforces proves opting in still works:
// MAX_REQUESTS_PER_DAY=2 refuses the third acquire after two chats.
func TestExplicitPerDayCapStillEnforces(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPoolCfg(t, func(c *config.Config) { c.MaxRequestsPerDay = 2 }, mock)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := range 2 {
		lease, err := p.Acquire(ctx, modelA)
		if err != nil {
			t.Fatalf("acquire %d err = %v", i, err)
		}
		chatOnce(t, p, lease)
		p.LeaseRelease(lease)
	}
	_, err := p.Acquire(ctx, modelA)
	if !strings.Contains(err.Error(), "daily request limit reached") {
		t.Errorf("third acquire err = %v, want daily-limit refusal", err)
	}
}

// TestChatGateUnlimitedDefault proves 0 caps grant untracked permits at
// once: three simultaneously-held leases on one lane, nothing tracked.
func TestChatGateUnlimitedDefault(t *testing.T) {
	p, _ := newChatGatePool(t, 0, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var leases []*Lease
	for i := range 3 {
		lease, err := p.Acquire(ctx, modelA)
		if err != nil {
			t.Fatalf("held acquire %d err = %v, want immediate grant (unlimited)", i, err)
		}
		leases = append(leases, lease)
	}
	if got := p.chatGate.totalHeld(); got != 0 {
		t.Errorf("tracked chat-lane holds = %d, want 0 (untracked unlimited grants)", got)
	}
	for _, lease := range leases {
		p.LeaseRelease(lease)
	}
}

// TestCreateGateUnlimited proves 0 caps grant immediately: three held
// permits, a loosened gate unblocks, negatives normalize, and a single
// capped dimension still enforces.
func TestCreateGateUnlimited(t *testing.T) {
	t.Run("zero caps grant at once", func(t *testing.T) {
		g := newCreateGate(0, 0)
		var held []*createPermit
		for range 3 {
			permit, err := g.acquire(context.Background(), "m1")
			if err != nil {
				t.Fatalf("acquire err = %v, want immediate grant (unlimited)", err)
			}
			held = append(held, permit)
		}
		for _, permit := range held {
			permit.Release()
		}
	})

	t.Run("loosening to unlimited unblocks", func(t *testing.T) {
		g := newCreateGate(1, 1)
		held, err := g.acquire(context.Background(), "m1")
		if err != nil {
			t.Fatal(err)
		}
		g.setLimits(0, 0)
		permit, err := g.acquire(context.Background(), "m1")
		if err != nil {
			t.Fatalf("acquire after loosening err = %v, want immediate grant", err)
		}
		permit.Release()
		held.Release()
	})

	t.Run("negative normalizes to unlimited", func(t *testing.T) {
		g := newCreateGate(-5, -2)
		permit, err := g.acquire(context.Background(), "m1")
		if err != nil {
			t.Fatalf("acquire err = %v, want immediate grant", err)
		}
		permit.Release()
	})

	t.Run("single capped dimension still enforces", func(t *testing.T) {
		g := newCreateGate(0, 1)
		held, err := g.acquire(context.Background(), "m1")
		if err != nil {
			t.Fatal(err)
		}
		defer held.Release()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
		defer cancel()
		if _, err := g.acquire(ctx, "m1"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("second same-model acquire = %v, want DeadlineExceeded", err)
		}
		// A different model is unaffected by the per-model cap.
		other, err := g.acquire(context.Background(), "m2")
		if err != nil {
			t.Fatalf("other-model acquire err = %v, want success", err)
		}
		other.Release()
	})
}

// TestSlotCapUnlimitedSkipsGating proves TOKEN_MAX_CONCURRENT=0 skips slot
// gating entirely: concurrent live turns pile up with no counter, no queue,
// and no slot permit on the leases.
func TestSlotCapUnlimitedSkipsGating(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newSmartTestPool(t, func(c *config.Config) { c.TokenMaxConcurrent = 0 }, mock)
	entry := smartEntry(p, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var leases []*Lease
	for i := range 4 {
		lease, err := p.Acquire(ctx, modelA)
		if err != nil {
			t.Fatalf("acquire %d err = %v, want success (slot cap unlimited)", i, err)
		}
		if lease.routeSlot != nil {
			t.Errorf("acquire %d lease carries a slot permit, want none (gating skipped)", i)
		}
		leases = append(leases, lease)
	}
	if got := p.routeSlotLive(entry); got != 0 {
		t.Errorf("live slots = %d, want 0 (nothing tracked)", got)
	}
	if got := p.routeSlotQueued(entry); got != 0 {
		t.Errorf("queued = %d, want 0", got)
	}
	for _, lease := range leases {
		p.LeaseRelease(lease)
	}
}
