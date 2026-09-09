// glm_guard_test.go — issue #183: pre-admission entitlement guard for
// referral-gated z-ai/glm-5.2 model. The session manager probes unadmitted
// accounts before sending POST /api/sessions/create with x-freebuff-model: z-ai/glm-5.2
// so unentitled accounts are rejected locally rather than banned upstream.
package session

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// TestEnsureSessionForModelGlmGuardUnentitled verifies that an account with no
// referral credits is probed with a safe GET and rejected with 429 without ever
// sending a POST create for GLM 5.2.
func TestEnsureSessionForModelGlmGuardUnentitled(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	var getProbes atomic.Int32
	var postCreates atomic.Int32

	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			getProbes.Add(1)
			// Return account state with standard models but NO GLM entitlement
			writeJSON(w, http.StatusOK, map[string]any{
				"status": "none",
				"rateLimitsByModel": map[string]any{
					"deepseek/deepseek-v4-flash": map[string]any{
						"model":       "deepseek/deepseek-v4-flash",
						"limit":       5,
						"recentCount": 0,
						"period":      "pacific_day",
					},
				},
			})
			return
		}
		if r.Method == http.MethodPost {
			postCreates.Add(1)
			writeJSON(w, http.StatusOK, map[string]any{
				"status":     "active",
				"instanceId": "inst-glm-danger",
				"model":      r.Header.Get("x-freebuff-model"),
			})
		}
	}

	mgr := newTestSession(t, mock)

	_, err := mgr.EnsureSessionForModel(context.Background(), "z-ai/glm-5.2")
	if err == nil {
		t.Fatal("EnsureSessionForModel(z-ai/glm-5.2) succeeded, want 429 rate limit refusal")
		return
	}

	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("err = %v, want *upstream.RateLimitError", err)
	}
	if rle.Model != "z-ai/glm-5.2" {
		t.Errorf("rle.Model = %q, want z-ai/glm-5.2", rle.Model)
	}

	if getProbes.Load() == 0 {
		t.Error("getProbes = 0, want zero-cost probe GET before admission")
	}
	if postCreates.Load() != 0 {
		t.Errorf("postCreates = %d, want 0 (POST create must never be sent for unentitled GLM request)", postCreates.Load())
	}
}

// TestEnsureSessionForModelGlmGuardEntitled verifies that an account whose
// probe response reveals a live glmPromo is permitted to proceed with session
// admission for GLM 5.2.
func TestEnsureSessionForModelGlmGuardEntitled(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	var getProbes atomic.Int32
	var postCreates atomic.Int32

	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			getProbes.Add(1)
			// Return account state with active glmPromo
			writeJSON(w, http.StatusOK, map[string]any{
				"status": "none",
				"glmPromo": map[string]any{
					"dailySessions": 2,
					"endsAt":        "2099-01-01T00:00:00Z",
				},
			})
			return
		}
		if r.Method == http.MethodPost {
			postCreates.Add(1)
			writeJSON(w, http.StatusOK, map[string]any{
				"status":     "active",
				"instanceId": "inst-glm-success",
				"model":      "z-ai/glm-5.2",
				"expiresAt":  time.Now().Add(30 * time.Minute).Format(time.RFC3339),
			})
		}
	}

	mgr := newTestSession(t, mock)

	instance, err := mgr.EnsureSessionForModel(context.Background(), "z-ai/glm-5.2")
	if err != nil {
		t.Fatalf("EnsureSessionForModel(z-ai/glm-5.2) failed: %v", err)
	}
	if instance != "inst-glm-success" {
		t.Errorf("instance = %q, want inst-glm-success", instance)
	}
	if postCreates.Load() == 0 {
		t.Error("postCreates = 0, want session create for entitled GLM request")
	}
}
