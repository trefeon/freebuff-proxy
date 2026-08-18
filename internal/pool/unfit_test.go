package pool

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

// limitedBody is the upstream limited_ip refusal wire shape: a 409
// session_model_mismatch response whose message carries the "limited"
// marker. classifyError maps that pair to upstream.LimitedIpError (issue
// #74 P2); SessionMode cannot express it, so tests script it through
// SessionHandler (see mockupstream.go).
const limitedBody = `{"status":"session_model_mismatch","message":"model limited on this egress IP"}`

// limitedSessionHandler scripts every session POST as a limited_ip refusal
// and every session GET as the (irrelevant) ended state, mirroring the
// handler shape of TestAcquireIpCappedCooldownBounded.
func limitedSessionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, limitedBody)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"status":"ended"}`)
}

// TestModelUnfitRegistry covers the (egress, model) unfit registry unit
// behavior: mark/read/clear, nil-lie tolerance, and TTL expiry with the
// shrunk-window restore.
func TestModelUnfitRegistry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	// Absent entry: zero time, nil error.
	if until, lie := p.ModelUnfit(modelA); !until.IsZero() || lie != nil {
		t.Fatalf("ModelUnfit on fresh pool = %v/%v, want zero/nil", until, lie)
	}

	// Mark: until ≈ now+5m, the lie's Model is set to the marked model and
	// its body is preserved.
	lie := &upstream.LimitedIpError{Body: "model limited on this egress IP"}
	p.MarkModelUnfit(modelA, lie)
	until, got := p.ModelUnfit(modelA)
	if until.IsZero() {
		t.Fatal("ModelUnfit after mark = zero time, want ≈ now+5m")
	}
	want := time.Now().Add(5 * time.Minute)
	if d := until.Sub(want); d < -2*time.Second || d > 2*time.Second {
		t.Errorf("until = %v, want ≈ now+5m (delta %v)", until, d)
	}
	if got == nil {
		t.Fatal("ModelUnfit lie = nil, want the marked error")
	}
	if got.Model != modelA {
		t.Errorf("lie.Model = %q, want %q", got.Model, modelA)
	}
	if got.Body != "model limited on this egress IP" {
		t.Errorf("lie.Body = %q, want preserved body", got.Body)
	}

	// Clear: entry gone.
	p.ClearModelUnfit(modelA)
	if until, lie := p.ModelUnfit(modelA); !until.IsZero() || lie != nil {
		t.Fatalf("ModelUnfit after clear = %v/%v, want zero/nil", until, lie)
	}

	// Nil-lie mark: entry present, error nil.
	p.MarkModelUnfit(modelA, nil)
	until, got = p.ModelUnfit(modelA)
	if until.IsZero() || got != nil {
		t.Fatalf("ModelUnfit with nil lie = %v/%v, want nonzero/nil", until, got)
	}
	p.ClearModelUnfit(modelA)

	// Expiry: shrink the TTL, mark, wait past it — the entry is lazily
	// purged. Restore the package var so later tests see the real window.
	oldTTL := modelUnfitTTL
	modelUnfitTTL = 10 * time.Millisecond
	t.Cleanup(func() { modelUnfitTTL = oldTTL })
	p.MarkModelUnfit(modelA, &upstream.LimitedIpError{Body: "limited"})
	time.Sleep(20 * time.Millisecond)
	if until, lie := p.ModelUnfit(modelA); !until.IsZero() || lie != nil {
		t.Fatalf("ModelUnfit after TTL expiry = %v/%v, want zero/nil", until, lie)
	}
}

// TestAcquireLimitedIpMarksModel covers the admission-path detection: a
// session POST refused with the limited_ip shape surfaces
// *upstream.LimitedIpError from Acquire and marks the (egress, model) pair
// unfit.
func TestAcquireLimitedIpMarksModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = limitedSessionHandler
	p := newTestPool(t, mock)

	_, err := p.Acquire(context.Background(), modelA)
	var lie *upstream.LimitedIpError
	if !errors.As(err, &lie) {
		t.Fatalf("Acquire err = %v, want *upstream.LimitedIpError", err)
	}
	if !errors.Is(err, upstream.ErrModelIPLimited) {
		t.Error("Acquire err not unwrap-able to ErrModelIPLimited")
	}
	if lie.Model != modelA {
		t.Errorf("surfaced lie.Model = %q, want %q", lie.Model, modelA)
	}

	// The refusal marked the (egress, model) pair unfit.
	until, got := p.ModelUnfit(modelA)
	if until.IsZero() {
		t.Fatal("ModelUnfit after limited_ip admission = zero time, want marked")
	}
	if got == nil || got.Model != modelA {
		t.Errorf("ModelUnfit lie = %v, want marked lie for %s", got, modelA)
	}
}

// TestAcquireAllTokensLimitedSurfacesLimitedIp covers failover when every
// token is limited_ip: Acquire surfaces the LimitedIpError bucket, not the
// generic combined error.
func TestAcquireAllTokensLimitedSurfacesLimitedIp(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.SessionHandler = limitedSessionHandler
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mock1.SessionHandler = limitedSessionHandler
	p := newTestPool(t, mock0, mock1)

	_, err := p.Acquire(context.Background(), modelA)
	var lie *upstream.LimitedIpError
	if !errors.As(err, &lie) {
		t.Fatalf("Acquire err = %v, want *upstream.LimitedIpError (not generic combined error)", err)
	}
	if lie.Model != modelA {
		t.Errorf("surfaced lie.Model = %q, want %q", lie.Model, modelA)
	}
	if until, _ := p.ModelUnfit(modelA); until.IsZero() {
		t.Fatal("ModelUnfit after all-tokens-limited acquire = zero time, want marked")
	}
}

// TestAcquireNotBlockedByUnfit pins the retry contract: the registry must
// NOT block Acquire itself. The chat recovery loop re-acquires through the
// plain acquire closure and must reach a DIFFERENT token in mixed-tier
// pools; only the server's new-request guard consults the registry.
func TestAcquireNotBlockedByUnfit(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	p.MarkModelUnfit(modelA, &upstream.LimitedIpError{Body: "limited"})

	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatalf("Acquire blocked by unfit mark: %v", err)
	}
	if lease == nil {
		t.Fatal("Acquire returned nil lease")
	}
	p.LeaseRelease(lease)

	// The mark survives the successful acquire: the pool does not clear it
	// (the server clears on the chat success path).
	if until, _ := p.ModelUnfit(modelA); until.IsZero() {
		t.Fatal("ModelUnfit cleared by Acquire, want mark intact (server clears on chat success)")
	}
}

// TestAcquireBanBeatsLimitedIp covers the failover precedence: when one
// token is banned and another is limited_ip, the ban bucket wins
// (ban > country-blocked > model-IP-limited > rate-limit > ...).
func TestAcquireBanBeatsLimitedIp(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock0.SessionMode = "banned"
	mock1 := testutil.NewMock()
	defer mock1.Close()
	mock1.SessionHandler = limitedSessionHandler
	p := newTestPool(t, mock0, mock1)

	_, err := p.Acquire(context.Background(), modelA)
	var be *upstream.BanError
	if !errors.As(err, &be) {
		t.Fatalf("Acquire err = %v, want *upstream.BanError (ban > model-IP-limited)", err)
	}
	// The limited token still marked the registry during the pass.
	if until, _ := p.ModelUnfit(modelA); until.IsZero() {
		t.Fatal("ModelUnfit not marked by the limited token during the pass")
	}
}
