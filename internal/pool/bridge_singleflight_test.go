// bridge_singleflight_test.go — tests for per-entry single-flight session
// creation in bridge mode: concurrent requests for the same client token
// share one session creation instead of splitting across multiple creates.
package pool

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/internal/testutil"
)

// TestBridgeSingleFlight_BasicLeaderFollower verifies the fundamental flow:
// the first AcquireBridge becomes the leader and creates a session; the
// second AcquireBridge blocks on the admissionGate, then reuses the session.
func TestBridgeSingleFlight_BasicLeaderFollower(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionCreateDelay = 50 * time.Millisecond

	p := newBridgePool(t, mock)
	const clientToken = "client-tok-sf-1"

	// First acquire: leader, creates session.
	lease1, err := p.AcquireBridge(context.Background(), clientToken, modelA)
	if err != nil {
		t.Fatalf("leader acquire failed: %v", err)
	}
	if lease1.SessionInstanceID != "inst-abc-123" {
		t.Errorf("leader instance = %q, want inst-abc-123", lease1.SessionInstanceID)
	}
	p.LeaseRelease(lease1)

	// Second acquire: should reuse the session (hot path).
	lease2, err := p.AcquireBridge(context.Background(), clientToken, modelA)
	if err != nil {
		t.Fatalf("follower acquire failed: %v", err)
	}
	if lease2.SessionInstanceID != "inst-abc-123" {
		t.Errorf("follower instance = %q, want inst-abc-123", lease2.SessionInstanceID)
	}
	p.LeaseRelease(lease2)

	if mock.SessionCreates != 1 {
		t.Errorf("session creates = %d, want 1 (only leader creates)", mock.SessionCreates)
	}
}

// TestBridgeSingleFlight_ConcurrentRequestsShareSession verifies that N
// concurrent requests for the same client token share a single session
// creation instead of firing duplicate creates.
func TestBridgeSingleFlight_ConcurrentRequestsShareSession(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionCreateDelay = 100 * time.Millisecond

	p := newBridgePool(t, mock)
	const clientToken = "client-tok-sf-2"

	const goroutines = 6
	var wg sync.WaitGroup
	var errs atomic.Int32
	var instances sync.Map

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := p.AcquireBridge(context.Background(), clientToken, modelA)
			if err != nil {
				errs.Add(1)
				return
			}
			instances.Store(lease.SessionInstanceID, true)
			p.LeaseRelease(lease)
		}()
	}
	wg.Wait()

	if errs.Load() > 0 {
		t.Fatalf("%d acquires failed", errs.Load())
	}
	// All goroutines should have received the same instance ID.
	count := 0
	instances.Range(func(_, _ any) bool { count++; return true })
	if count != 1 {
		t.Errorf("distinct instance IDs = %d, want 1 (all share single session)", count)
	}
	if mock.SessionCreates != 1 {
		t.Errorf("session creates = %d, want 1 (single-flight)", mock.SessionCreates)
	}
}

// TestBridgeSingleFlight_LeaderFailureResetsGate verifies that when the
// leader's session creation fails, the gate resets so the next request
// retries instead of being stuck on a failed creation.
func TestBridgeSingleFlight_LeaderFailureResetsGate(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	// First attempt: fail session creation.
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"status":"rate_limited","limit":1,"recentCount":1,"period":"pacific_day","resetAt":"2099-01-01T00:00:00Z","retryAfterMs":3600000}`)
	}

	p := newBridgePool(t, mock)
	const clientToken = "client-tok-sf-3"

	// First acquire: should fail.
	_, err := p.AcquireBridge(context.Background(), clientToken, modelA)
	if err == nil {
		t.Fatal("expected error on first acquire")
	}

	// Verify gate was reset (entry still cached, not evicted).
	p.bridgeMu.Lock()
	entry, ok := p.bridge[tokenKey(clientToken)]
	p.bridgeMu.Unlock()
	if !ok {
		t.Fatal("entry should still be cached after failure")
	}

	// Check gate is reset (closed channel would mean stuck).
	select {
	case <-entry.admissionGate:
		t.Fatal("admissionGate should be open (reset after failure)")
	default:
		// Gate is open — correct.
	}

	// Fix the handler and clear cooldown before retry.
	mock.SessionHandler = nil // back to default
	mock.SessionCreateDelay = 0
	entry.runs.ClearCooldowns()

	lease, err := p.AcquireBridge(context.Background(), clientToken, modelA)
	if err != nil {
		t.Fatalf("retry acquire failed: %v", err)
	}
	p.LeaseRelease(lease)

	if mock.SessionCreates != 1 {
		t.Errorf("session creates = %d, want 1 (retry succeeded)", mock.SessionCreates)
	}
}

// TestBridgeSingleFlight_DifferentTokensIndependent verifies that single-flight
// is per-entry: two different client tokens elect independent leaders and
// do not block each other.
func TestBridgeSingleFlight_DifferentTokensIndependent(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionCreateDelay = 100 * time.Millisecond

	p := newBridgePool(t, mock)

	var wg sync.WaitGroup
	var lease1, lease2 *Lease
	var err1, err2 error

	wg.Add(2)
	go func() {
		defer wg.Done()
		lease1, err1 = p.AcquireBridge(context.Background(), "client-tok-A", modelA)
	}()
	go func() {
		defer wg.Done()
		lease2, err2 = p.AcquireBridge(context.Background(), "client-tok-B", modelA)
	}()
	wg.Wait()

	if err1 != nil {
		t.Fatalf("token A acquire failed: %v", err1)
	}
	if err2 != nil {
		t.Fatalf("token B acquire failed: %v", err2)
	}
	p.LeaseRelease(lease1)
	p.LeaseRelease(lease2)

	// Each token should have its own bridge entry (independent single-flight gates).
	if lease1.Bridge == lease2.Bridge {
		t.Error("both tokens share the same bridge entry (should be independent)")
	}
	// Mock returns same instance for all requests — that's expected.
	// Verify 2 session creates (one per entry).
	if mock.SessionCreates != 2 {
		t.Errorf("session creates = %d, want 2 (one per token)", mock.SessionCreates)
	}
}

// TestBridgeSingleFlight_RapidSequentialRequests verifies that after a leader
// exits and the gate is closed, rapid sequential requests each get the cached
// session without creating new ones.
func TestBridgeSingleFlight_RapidSequentialRequests(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	p := newBridgePool(t, mock)
	const clientToken = "client-tok-sf-5"

	for i := range 10 {
		lease, err := p.AcquireBridge(context.Background(), clientToken, modelA)
		if err != nil {
			t.Fatalf("acquire %d failed: %v", i, err)
		}
		if lease.SessionInstanceID != "inst-abc-123" {
			t.Errorf("acquire %d instance = %q, want inst-abc-123", i, lease.SessionInstanceID)
		}
		p.LeaseRelease(lease)
	}

	// Exactly 1 session created (first cold admit, rest hot reuse).
	if mock.SessionCreates != 1 {
		t.Errorf("session creates = %d, want 1", mock.SessionCreates)
	}
}

// TestBridgeValidation_InvalidTokenRejected verifies that an invalid token
// (rejected by ProbeAccount) is never cached as a bridge entry.
func TestBridgeValidation_InvalidTokenRejected(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	// Token validation fails (401).
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.Header.Get("x-freebuff-instance-id") == "" {
			// Probe (no instance-id) → reject.
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"message":"unauthorized"}`)
			return
		}
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-abc-123","model":"mimo/mimo-v2.5","expiresAt":"2099-01-01T00:00:00Z"}`)
	}

	p := newBridgePool(t, mock)

	_, err := p.AcquireBridge(context.Background(), "bad-token", modelA)
	if err == nil {
		t.Fatal("expected error for invalid token")
	}

	// Entry should NOT be cached.
	p.bridgeMu.Lock()
	_, cached := p.bridge[tokenKey("bad-token")]
	p.bridgeMu.Unlock()
	if cached {
		t.Error("invalid token was cached as bridge entry")
	}
}

// TestBridgeValidation_ValidTokenAccepted verifies that a valid token passes
// ProbeAccount and is cached as a bridge entry.
func TestBridgeValidation_ValidTokenAccepted(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	p := newBridgePool(t, mock)
	const clientToken = "valid-token"

	lease, err := p.AcquireBridge(context.Background(), clientToken, modelA)
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	p.LeaseRelease(lease)

	// Entry should be cached.
	p.bridgeMu.Lock()
	_, cached := p.bridge[tokenKey(clientToken)]
	p.bridgeMu.Unlock()
	if !cached {
		t.Error("valid token not cached as bridge entry")
	}
	if mock.SessionCreates != 1 {
		t.Errorf("session creates = %d, want 1", mock.SessionCreates)
	}
}

// TestBridgeStickiness_LastModelTracked verifies that after a successful
// lease, the entry's lastModel is set to the effective model.
func TestBridgeStickiness_LastModelTracked(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	p := newBridgePool(t, mock)
	const clientToken = "client-tok-stick-1"

	lease, err := p.AcquireBridge(context.Background(), clientToken, modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)

	p.bridgeMu.Lock()
	entry := p.bridge[tokenKey(clientToken)]
	p.bridgeMu.Unlock()

	if entry == nil {
		t.Fatal("bridge entry not found")
	}
	got, _ := entry.lastModel.Load().(string)
	if got != modelA {
		t.Errorf("lastModel = %q, want %q", got, modelA)
	}
}

// TestBridgeStickiness_MultiTurnReusesSession verifies that multi-turn
// requests for the same model on the same bridge entry reuse the session
// without creating new ones.
func TestBridgeStickiness_MultiTurnReusesSession(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	p := newBridgePool(t, mock)
	const clientToken = "client-tok-stick-2"

	for i := range 5 {
		lease, err := p.AcquireBridge(context.Background(), clientToken, modelA)
		if err != nil {
			t.Fatalf("turn %d failed: %v", i, err)
		}
		if lease.SessionInstanceID != "inst-abc-123" {
			t.Errorf("turn %d instance = %q, want inst-abc-123", i, lease.SessionInstanceID)
		}
		p.LeaseRelease(lease)
	}

	if mock.SessionCreates != 1 {
		t.Errorf("session creates = %d, want 1 (multi-turn reuse)", mock.SessionCreates)
	}
}
