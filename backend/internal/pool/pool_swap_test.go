package pool

import (
	"context"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

func TestPoolSwapTokens(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	p := sizedPool(t, mock) // initial token at index 0
	addTokens(t, p, "cb_alpha", "cb_beta", "cb_gamma") // indices 1, 2, 3

	if p.TokenCount() != 4 {
		t.Fatalf("TokenCount = %d, want 4", p.TokenCount())
	}

	toks := *p.toks.Load()
	if toks[1].token != "cb_alpha" || toks[2].token != "cb_beta" || toks[3].token != "cb_gamma" {
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

	// Swap 1 and 2 (promote cb_beta to #1)
	if err := p.SwapTokens(1, 2); err != nil {
		t.Fatalf("SwapTokens(1, 2) error: %v", err)
	}

	toks = *p.toks.Load()
	if toks[1].token != "cb_beta" || toks[2].token != "cb_alpha" || toks[3].token != "cb_gamma" {
		t.Fatalf("swapped order wrong: %v, %v, %v", toks[1].token, toks[2].token, toks[3].token)
	}

	// In-flight refusal test
	lease, err := p.Acquire(context.Background(), "deepseek/deepseek-v4-flash")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer p.LeaseRelease(lease)

	if err := p.SwapTokens(1, 3); err == nil {
		t.Error("SwapTokens with in-flight lease want error, got nil")
	}
}
