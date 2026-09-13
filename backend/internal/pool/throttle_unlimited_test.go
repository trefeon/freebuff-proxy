package pool

// Burst regression tests: no local request/message/spend cap remains --
// upstream quota/429 is the enforcement and the smart-routing live-turn
// slot plus FIFO queue paces bursts. Covers the pooled burst, the bridge
// burst, the per-day display ledger, and the live-turn slot cap.

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

// TestPooledBurstHasNoLocalRefusal proves a pooled burst far past every
// deleted cap scale (per-minute, per-day, daily-message, spend, bridge
// global) is never refused locally: with slot gating off, 50 sequential
// acquires all succeed. Any reintroduced local cap check would refuse
// partway and fail this test.
func TestPooledBurstHasNoLocalRefusal(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.UpstreamBaseURL = mock.URL()
		c.TokenMaxConcurrent = 0
	}, mock)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := range 50 {
		lease, err := p.Acquire(ctx, modelA)
		if err != nil {
			if errors.Is(err, upstream.ErrRateLimited) {
				t.Fatalf("acquire %d rate-limited, want success (no local caps remain)", i)
			}
			t.Fatalf("acquire %d err = %v, want success (no local caps remain)", i, err)
		}
		chatOnce(t, p, lease)
		p.LeaseRelease(lease)
	}
	if got := p.usageCount(0); got != 50 {
		t.Errorf("usageCount = %d, want 50 (display ledger still records)", got)
	}
	if got := p.dayRequestCount(0); got != 50 {
		t.Errorf("dayRequestCount = %d, want 50 (per-day display still records)", got)
	}
	if got := p.requestsServed.Load(); got != 50 {
		t.Errorf("requestsServed = %d, want 50 (lifetime total still records)", got)
	}
}

// TestBridgeBurstHasNoLocalRefusal proves a bridge burst far past every
// deleted cap scale is never refused locally: with slot gating off, 50
// sequential bridge acquires for one client token all succeed. Any
// reintroduced bridge rpm/daily/global cap check would refuse partway.
func TestBridgeBurstHasNoLocalRefusal(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPoolCfg(t, func(c *config.Config) {
		c.UpstreamBaseURL = mock.URL()
		c.TokenMaxConcurrent = 0
	}, mock)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := range 50 {
		lease, err := p.AcquireBridge(ctx, "burst-client", modelA)
		if err != nil {
			if strings.Contains(err.Error(), "limit") || errors.Is(err, upstream.ErrRateLimited) {
				t.Fatalf("bridge acquire %d refused (%v), want success (no local caps remain)", i, err)
			}
			t.Fatalf("bridge acquire %d err = %v, want success (no local caps remain)", i, err)
		}
		p.LeaseRelease(lease)
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
