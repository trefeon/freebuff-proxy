package upstream

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestClassifyFreeModeInvalidAgentHierarchy pins the hierarchy gate: a 403
// carrying free_mode_invalid_agent_hierarchy is a dedicated config refusal
// (mirroring free_mode_cli_required), never a generic 502 and never a
// cooldown-bearing RateLimitError. The 403 gate stays tight: the same marker
// on any other status must not match the arm.
func TestClassifyFreeModeInvalidAgentHierarchy(t *testing.T) {
	body := `{"error":"free_mode_invalid_agent_hierarchy","message":"subagent not in root allowlist"}`
	err := classifyError(http.StatusForbidden, body, http.Header{})
	if !errors.Is(err, ErrFreeModeInvalidAgentHierarchy) {
		t.Fatalf("errors.Is(ErrFreeModeInvalidAgentHierarchy) = false, got %T %v", err, err)
	}
	if errors.Is(err, ErrFreeModeCLIRequired) {
		t.Errorf("hierarchy refusal unwraps to ErrFreeModeCLIRequired, want the dedicated sentinel")
	}
	var rle *RateLimitError
	if errors.As(err, &rle) {
		t.Errorf("hierarchy refusal = RateLimitError (%v), want the cooldown-free 403 sentinel", rle)
	}

	// Off-status bodies must not match: the gate is 403-only, so a 429 with
	// the marker stays on the rate-limit path and a 400 stays generic.
	if err := classifyError(http.StatusTooManyRequests, body, http.Header{}); errors.Is(err, ErrFreeModeInvalidAgentHierarchy) {
		t.Errorf("429 hierarchy body matched the 403 arm: %v", err)
	}
	if err := classifyError(http.StatusBadRequest, body, http.Header{}); errors.Is(err, ErrFreeModeInvalidAgentHierarchy) {
		t.Errorf("400 hierarchy body matched the 403 arm: %v", err)
	}

	// A bare 403 without the marker stays generic.
	if err := classifyError(http.StatusForbidden, `{"error":"forbidden"}`, http.Header{}); errors.Is(err, ErrFreeModeInvalidAgentHierarchy) {
		t.Errorf("marker-less 403 matched the hierarchy arm: %v", err)
	}
}

// TestClassifyPeakHoursUnderscore pins the underscore body form: a 429
// carrying peak_hours classifies exactly like the space form ("peak hours") —
// bounded PeakHoursCooldown, distinct peak_hours status, no midnight lock.
func TestClassifyPeakHoursUnderscore(t *testing.T) {
	for _, body := range []string{
		`{"status":"rate_limited","message":"Usage is temporarily limited during peak_hours, prices double"}`,
		`{"error":"peak_hours","message":"peak_hours cap"}`,
	} {
		err := classifyError(http.StatusTooManyRequests, body, http.Header{})
		var rle *RateLimitError
		if !errors.As(err, &rle) {
			t.Fatalf("classifyError(%q) = %T %v, want *RateLimitError", body, err, err)
		}
		if rle.Status != string(WireCodePeakHoursStatus) {
			t.Errorf("Status = %q, want %q", rle.Status, string(WireCodePeakHoursStatus))
		}
		if rle.RetryAfter != PeakHoursCooldown {
			t.Errorf("RetryAfter = %v, want %v (bounded, not midnight)", rle.RetryAfter, PeakHoursCooldown)
		}
		if !rle.ResetAt.IsZero() {
			t.Errorf("ResetAt = %v, want zero (no Pacific-midnight lock)", rle.ResetAt)
		}
	}
}

// TestClassifyTurnSpendLimitAnyStatus pins the loop-protection breaker as
// status-agnostic: the turn_spend_limit literal is terminal (a retry re-trips
// instantly) on whatever status carries it, so it must surface as
// *TurnSpendLimitError — never a RateLimitError cooldown, never the generic
// 502 — with the incoming status preserved for telemetry.
func TestClassifyTurnSpendLimitAnyStatus(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		body := `{"error":"turn_spend_limit","message":"Something went wrong with this turn.","retryAfterMs":60000}`
		err := classifyError(status, body, http.Header{})
		var tsle *TurnSpendLimitError
		if !errors.As(err, &tsle) {
			t.Fatalf("status %d: classifyError = %T %v, want *TurnSpendLimitError", status, err, err)
		}
		if !errors.Is(err, ErrTurnSpendLimited) {
			t.Errorf("status %d: does not unwrap to ErrTurnSpendLimited", status)
		}
		if tsle.Status != status {
			t.Errorf("status %d: TurnSpendLimitError.Status = %d, want the incoming status preserved", status, tsle.Status)
		}
		if !strings.Contains(tsle.Body, "Something went wrong with this turn") {
			t.Errorf("status %d: Body = %q, want the upstream loop warning intact", status, tsle.Body)
		}
		var rle *RateLimitError
		if errors.As(err, &rle) {
			t.Errorf("status %d: = RateLimitError (%v), want the terminal type (no cooldown/backoff)", status, rle)
		}
	}
}

// TestClassifyVendorUICopyStaysDefault pins the no-literal rule: purchase /
// consent / terms / purchasesPaused strings are vendor-UI copy (availability
// labels, Desktop updateRequired/purchasesPaused flags, wallet-consent prose)
// with no wire status literal — the wiregen guard fails loud on any new
// snapshot literal — so bodies carrying only those strings must stay the
// default *UpstreamError (502 upstream_unavailable), never a typed refusal.
func TestClassifyVendorUICopyStaysDefault(t *testing.T) {
	bodies := []struct {
		status int
		body   string
	}{
		{405, `{"error":"purchase_required","message":"No purchase was made"}`},
		{http.StatusBadRequest, `{"error":"consent_required","message":"re-confirm 2 wallet Freebucks to admit"}`},
		{http.StatusForbidden, `{"error":"terms_not_accepted","message":"accept the terms first"}`},
		{http.StatusConflict, `{"status":"model_unavailable","availableHours":"Purchased sessions are paused.","purchasesPaused":true}`},
	}
	for _, tc := range bodies {
		err := classifyError(tc.status, tc.body, http.Header{})
		var ue *UpstreamError
		if !errors.As(err, &ue) {
			t.Fatalf("status %d body %q: classifyError = %T %v, want default *UpstreamError", tc.status, tc.body, err, err)
		}
		if ue.Retryable {
			t.Errorf("status %d body %q: Retryable = true, want the non-retryable default", tc.status, tc.body)
		}
		for _, typed := range []error{ErrBanned, ErrCountryBlocked, ErrRateLimited, ErrSessionInvalid, ErrRunInvalid, ErrTurnSpendLimited, ErrFreeModeCLIRequired, ErrFreeModeInvalidAgentHierarchy} {
			if errors.Is(err, typed) {
				t.Errorf("status %d body %q: unwraps to %v, want no typed refusal", tc.status, tc.body, typed)
			}
		}
	}
}

// TestClassifyRunIDGateStays400 pins the existing contract: the run-id
// markers only match on 400. Any other status carrying them stays off the
// run-invalid path.
func TestClassifyRunIDGateStays400(t *testing.T) {
	err := classifyError(http.StatusBadRequest, `{"error":"runid not found"}`, http.Header{})
	if !errors.Is(err, ErrRunInvalid) {
		t.Fatalf("400 runid body = %T %v, want ErrRunInvalid", err, err)
	}
	for _, status := range []int{http.StatusConflict, http.StatusNotFound, http.StatusInternalServerError} {
		for _, body := range []string{`{"error":"runid not found"}`, `{"error":"runid not running"}`} {
			if err := classifyError(status, body, http.Header{}); errors.Is(err, ErrRunInvalid) {
				t.Errorf("status %d body %q matched the 400-gated run arm: %v", status, body, err)
			}
		}
	}
}
