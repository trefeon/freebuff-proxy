// glm_referral_test.go — issue #183: referral-gated model (z-ai/glm-5.2)
// entitlement gating and quota fallback. Only tokens with verified GLM
// entitlement are permitted to admit GLM 5.2 sessions, preventing unentitled
// accounts from getting permanently hard-banned by upstream fraud checks.
package pool

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// TestUnentitledPoolTokenGlmQuotaFallback verifies that requesting z-ai/glm-5.2
// on a pool where no token holds referral entitlement automatically quota-falls
// back to deepseek/deepseek-v4-flash without sending any upstream session create
// for GLM 5.2 (which upstream punishes with 403 account_banned).
func TestUnentitledPoolTokenGlmQuotaFallback(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	var glmCreates atomic.Int32
	var flashCreates atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("x-freebuff-model") {
		case "z-ai/glm-5.2":
			glmCreates.Add(1)
		case "deepseek/deepseek-v4-flash":
			flashCreates.Add(1)
		}
		expiresAt := time.Now().Add(30 * time.Minute).UTC().Format("2006-01-02T15:04:05.000Z07:00")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-flash-123","model":"deepseek/deepseek-v4-flash","expiresAt":"`+expiresAt+`"}`)
	}

	p := newTestPoolCfg(t, func(c *config.Config) {
		c.QuotaFallbackModels = map[string]string{"z-ai/glm-5.2": "deepseek/deepseek-v4-flash"}
	}, mock)

	lease, err := p.Acquire(context.Background(), "z-ai/glm-5.2")
	if err != nil {
		t.Fatalf("Acquire(z-ai/glm-5.2) failed: %v", err)
	}
	defer p.LeaseRelease(lease)

	if glmCreates.Load() != 0 {
		t.Errorf("glmCreates = %d, want 0 (unentitled token must never send session create for GLM 5.2)", glmCreates.Load())
	}
	if flashCreates.Load() == 0 {
		t.Error("flashCreates = 0, want fallback session created for deepseek/deepseek-v4-flash")
	}
	if lease.Model != "deepseek/deepseek-v4-flash" {
		t.Errorf("lease.Model = %q, want deepseek/deepseek-v4-flash", lease.Model)
	}
	if lease.FallbackReason != "quota_exhausted" {
		t.Errorf("lease.FallbackReason = %q, want quota_exhausted", lease.FallbackReason)
	}
}

// TestUnentitledPoolTokenGlmRefusalWithoutFallback verifies that when
// QUOTA_FALLBACK_MODELS is explicitly empty, an unentitled request for
// z-ai/glm-5.2 returns a clean 429 rate limit error without sending any
// upstream session create.
func TestUnentitledPoolTokenGlmRefusalWithoutFallback(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	var glmCreates atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-freebuff-model") == "z-ai/glm-5.2" {
			glmCreates.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-1"}`)
	}

	p := newTestPoolCfg(t, func(c *config.Config) {
		c.QuotaFallbackModels = map[string]string{} // disable quota fallback
	}, mock)

	_, err := p.Acquire(context.Background(), "z-ai/glm-5.2")
	if err == nil {
		t.Fatal("Acquire(z-ai/glm-5.2) succeeded, want 429 rate-limit error")
	}
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("err = %v, want *upstream.RateLimitError", err)
	}
	if !strings.Contains(rle.Body, "referral entitlement required") && !strings.Contains(rle.Body, "no referral quota") {
		t.Errorf("rle.Body = %q, want referral entitlement notice", rle.Body)
	}
	if glmCreates.Load() != 0 {
		t.Errorf("glmCreates = %d, want 0", glmCreates.Load())
	}
}

// TestEntitledPoolTokenWithGlmPromo verifies that a token with an active
// GlmPromo block is permitted to open a z-ai/glm-5.2 session without falling back.
func TestEntitledPoolTokenWithGlmPromo(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	var glmCreates atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-freebuff-model") == "z-ai/glm-5.2" {
			glmCreates.Add(1)
		}
		expiresAt := time.Now().Add(30 * time.Minute).UTC().Format("2006-01-02T15:04:05.000Z07:00")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-glm-123","model":"z-ai/glm-5.2","expiresAt":"`+expiresAt+`","glmPromo":{"dailySessions":2,"endsAt":"2099-01-01T00:00:00Z"}}`)
	}

	p := newTestPool(t, mock)

	// Seed token 0 with active GlmPromo
	toks := p.roster.Load()
	(*toks)[0].session.Invalidate()
	// Run a probe that populates GlmPromo
	mock.GlmPromo = map[string]any{
		"dailySessions": 2,
		"endsAt":        "2099-01-01T00:00:00Z",
	}
	_, _ = p.ProbeToken(context.Background(), 0)

	lease, err := p.Acquire(context.Background(), "z-ai/glm-5.2")
	if err != nil {
		t.Fatalf("Acquire(z-ai/glm-5.2) failed: %v", err)
	}
	defer p.LeaseRelease(lease)

	if lease.Model != "z-ai/glm-5.2" {
		t.Errorf("lease.Model = %q, want z-ai/glm-5.2", lease.Model)
	}
	if lease.FallbackReason != "" {
		t.Errorf("lease.FallbackReason = %q, want empty (no fallback)", lease.FallbackReason)
	}
	if glmCreates.Load() == 0 {
		t.Error("glmCreates = 0, want GLM 5.2 session create for entitled token")
	}
}

// TestBridgeUnentitledGlmFallback verifies that in bridge mode an unentitled
// client token falls back to deepseek/deepseek-v4-flash via QuotaFallbackModels
// without attempting an unentitled upstream GLM create.
func TestBridgeUnentitledGlmFallback(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	var glmCreates atomic.Int32
	var flashCreates atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("x-freebuff-model") {
		case "z-ai/glm-5.2":
			glmCreates.Add(1)
		case "deepseek/deepseek-v4-flash":
			flashCreates.Add(1)
		}
		expiresAt := time.Now().Add(30 * time.Minute).UTC().Format("2006-01-02T15:04:05.000Z07:00")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-bridge-flash","model":"deepseek/deepseek-v4-flash","expiresAt":"`+expiresAt+`"}`)
	}

	p := newBridgePool(t, mock)
	cfg := *p.cfg.Load()
	cfg.QuotaFallbackModels = map[string]string{
		"z-ai/glm-5.2": "deepseek/deepseek-v4-flash",
	}
	p.SetConfig(&cfg)

	lease, err := p.AcquireBridge(context.Background(), "user-bridge-token", "z-ai/glm-5.2")
	if err != nil {
		t.Fatalf("AcquireBridge failed: %v", err)
	}
	defer p.LeaseRelease(lease)

	if glmCreates.Load() != 0 {
		t.Errorf("glmCreates = %d, want 0", glmCreates.Load())
	}
	if flashCreates.Load() == 0 {
		t.Error("flashCreates = 0, want fallback session create for flash")
	}
	if lease.Model != "deepseek/deepseek-v4-flash" {
		t.Errorf("lease.Model = %q, want deepseek/deepseek-v4-flash", lease.Model)
	}
	if lease.FallbackReason != "quota_exhausted" {
		t.Errorf("lease.FallbackReason = %q, want quota_exhausted", lease.FallbackReason)
	}
}
