package pool

import (
	"context"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

func TestPoolSwapTokens(t *testing.T) {
	mocks := []*testutil.MockUpstream{
		testutil.NewMock(),
		testutil.NewMock(),
		testutil.NewMock(),
		testutil.NewMock(),
	}
	for _, m := range mocks {
		defer m.Close()
	}

	// One mock per token: every entry must admit against hermetic
	// upstream (AddToken-built entries would point at production).
	p := newTestPool(t, mocks[0], mocks[1], mocks[2], mocks[3])
	if p.TokenCount() != 4 {
		t.Fatalf("TokenCount = %d, want 4", p.TokenCount())
	}

	toks := *p.roster.Load()
	if toks[1].token != "tok-1" || toks[2].token != "tok-2" || toks[3].token != "tok-3" {
		t.Fatalf("initial order wrong: %v, %v, %v", toks[1].token, toks[2].token, toks[3].token)
	}

	// Out of bounds checks
	if err := p.SwapTokens(-1, 0); err == nil {
		t.Error("SwapTokens(-1, 0) want error, got nil")
	}
	if err := p.SwapTokens(0, 10); err == nil {
		t.Error("SwapTokens(0, 10) want error, got nil")
	}

	// Idempotent self-swap
	if err := p.SwapTokens(1, 1); err != nil {
		t.Errorf("SwapTokens(1, 1) error: %v", err)
	}

	// Swap 1 and 2 (promote tok-2 to #2)
	if err := p.SwapTokens(1, 2); err != nil {
		t.Fatalf("SwapTokens(1, 2) error: %v", err)
	}

	toks = *p.roster.Load()
	if toks[1].token != "tok-2" || toks[2].token != "tok-1" || toks[3].token != "tok-3" {
		t.Fatalf("swapped order wrong: %v, %v, %v", toks[1].token, toks[2].token, toks[3].token)
	}

	// In-flight seamless test: hold a lease, swap under it, and prove every
	// post-acquire lease path still targets the lease's own entry (previously
	// refused with "active requests in flight").
	ctx := context.Background()
	lease, err := p.Acquire(ctx, modelB)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	origEntry := lease.entry
	origIdx := lease.Token
	if origEntry == nil {
		t.Fatal("lease has no backing entry")
	}
	other := (origIdx + 1) % 4
	otherEntry := (*p.roster.Load())[other]
	// Give the swapped-in entry its own live session so the
	// cross-invalidation check below is meaningful (cold is trivially dead).
	if _, err := otherEntry.session.EnsureSessionForModel(ctx, modelB); err != nil {
		t.Fatalf("admit swapped-in entry: %v", err)
	}
	if err := p.SwapTokens(origIdx, other); err != nil {
		t.Fatalf("SwapTokens with in-flight lease = %v, want seamless success", err)
	}

	// Order actually swapped.
	toks = *p.roster.Load()
	if toks[other] != origEntry || toks[origIdx] != otherEntry {
		t.Fatal("post-swap occupants wrong: entries did not exchange positions")
	}

	// Cooldown via lease lands on the ORIGINAL entry, not the swapped-in one.
	rle := &upstream.RateLimitError{Status: "rate_limited", Model: modelB, RetryAfter: time.Minute}
	p.CooldownLeaseRateLimit(lease, rle)
	if origEntry.runs.RateLimitError() == nil {
		t.Error("CooldownLeaseRateLimit missed the lease's own entry")
	}
	if otherEntry.runs.RateLimitError() != nil {
		t.Error("CooldownLeaseRateLimit hit the swapped-in entry")
	}

	// Session invalidate via lease drops the ORIGIN session only.
	p.InvalidateLeaseSession(lease)
	if origEntry.session.Snapshot().Usable() {
		t.Error("origin session still usable after InvalidateLeaseSession")
	}
	if !otherEntry.session.Snapshot().Usable() {
		t.Error("swapped-in entry session wrongly invalidated")
	}

	// Release drains through the own entry; pool serves post-swap (the
	// cooled origin is skipped, the hot swapped-in entry serves via reuse).
	p.LeaseRelease(lease)
	if origEntry.runs.InflightCount() != 0 {
		t.Errorf("origin inflight = %d, want 0 after release", origEntry.runs.InflightCount())
	}
	lease2, err := p.Acquire(ctx, modelB)
	if err != nil {
		t.Fatalf("post-swap Acquire: %v", err)
	}
	p.LeaseRelease(lease2)
}
