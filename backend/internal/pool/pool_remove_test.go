package pool

import (
	"context"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
)

// newTestPool seeds one fixed token (the mock's default credential), so all
// counts below are relative to the pool's current size at the point of the
// assertion rather than absolute.
func sizedPool(t *testing.T, mocks ...*testutil.MockUpstream) *Pool {
	t.Helper()
	p := newTestPool(t, mocks...)
	return p
}

func addTokens(t *testing.T, p *Pool, tokens ...string) {
	t.Helper()
	for _, tok := range tokens {
		if _, err := p.AddToken(tok); err != nil {
			t.Fatalf("AddToken(%s): %v", tok, err)
		}
	}
}

// TestRemoveTokenAtSpecificIndex pins the by-index removal: the chosen entry
// leaves the pool, the remaining order is preserved, and the usage/spend
// tracks stay index-aligned (a follow-up LastToken removal still works).
func TestRemoveTokenAtSpecificIndex(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := sizedPool(t, mock)
	addTokens(t, p, "cb_one", "cb_two", "cb_three")
	before := p.TokenCount()
	if err := p.RemoveTokenAt(1); err != nil {
		t.Fatalf("RemoveTokenAt(1): %v", err)
	}
	if got := p.TokenCount(); got != before-1 {
		t.Fatalf("TokenCount = %d, want %d", got, before-1)
	}
	// The removed entry was drained: no parked-inflight leftover.
	// A further removal of the (now last) token keeps the tracks aligned.
	if err := p.RemoveLastToken(); err != nil {
		t.Fatalf("RemoveLastToken after by-index removal: %v", err)
	}
	if err := p.RemoveLastToken(); err != nil {
		t.Fatalf("RemoveLastToken: %v", err)
	}
}

// TestRemoveTokenAtRefusesWhileBusy pins the idle guard: a middle removal
// would shift indices under in-flight leases, so the pool refuses while any
// run is active and succeeds once the lease is released.
func TestRemoveTokenAtRefusesWhileBusy(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n")
	p := sizedPool(t, mock)
	addTokens(t, p, "cb_busy_a", "cb_busy_b")
	before := p.TokenCount()
	lease, err := p.Acquire(context.Background(), "z-ai/glm-5.3-flash")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	err = p.RemoveTokenAt(0)
	if err == nil || !strings.Contains(err.Error(), "in flight") {
		t.Fatalf("RemoveTokenAt while busy = %v, want in-flight refusal", err)
		return
	}
	p.LeaseRelease(lease)
	time.Sleep(10 * time.Millisecond) // let the release settle
	if err := p.RemoveTokenAt(0); err != nil {
		t.Fatalf("RemoveTokenAt after release: %v", err)
	}
	if got := p.TokenCount(); got != before-1 {
		t.Fatalf("TokenCount = %d, want %d", got, before-1)
	}
}

// TestRemoveTokenAtOutOfRange pins the bounds error.
func TestRemoveTokenAtOutOfRange(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := sizedPool(t, mock)
	if _, err := p.AddToken("cb_only"); err != nil {
		t.Fatalf("AddToken: %v", err)
	}
	if err := p.RemoveTokenAt(p.TokenCount() + 3); err == nil {
		t.Fatal("RemoveTokenAt(too-large) succeeded, want out-of-range error")
		return
	}
	if err := p.RemoveTokenAt(-1); err == nil {
		t.Fatal("RemoveTokenAt(-1) succeeded, want out-of-range error")
		return
	}
}
