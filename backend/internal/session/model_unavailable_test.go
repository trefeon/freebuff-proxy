package session

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
)

// unavailableMock returns a mock whose session create 409s with
// model_unavailable for the given model (optionally carrying an
// availableHours window) and admits every other model as active. created
// collects the x-freebuff-model header of each create in order.
func unavailableMock(t *testing.T, unavailable, availableHours string) (*testutil.MockUpstream, *[]string) {
	t.Helper()
	mock := testutil.NewMock()
	var created []string
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		model := r.Header.Get("x-freebuff-model")
		created = append(created, model)
		w.Header().Set("Content-Type", "application/json")
		if model == unavailable {
			w.WriteHeader(http.StatusConflict)
			body := `{"status":"model_unavailable","requestedModel":"` + unavailable + `"`
			if availableHours != "" {
				body += `,"availableHours":"` + availableHours + `"`
			}
			body += `}`
			_, _ = io.WriteString(w, body)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-fb","model":"`+model+`","expiresAt":"2030-01-01T00:00:00Z"}`)
	}
	return mock, &created
}

// TestModelUnavailableCacheSkipsRepeatedAdmissions pins the issue #158 core:
// after the first 409, a second admission for the same off-window model is
// served from the cached fallback session with ZERO additional upstream
// creates.
func TestModelUnavailableCacheSkipsRepeatedAdmissions(t *testing.T) {
	mock, created := unavailableMock(t, "rare/model", "9am ET-5pm PT every day")
	defer mock.Close()
	mgr := newTestManager(t, mock)
	now := time.Date(2026, 8, 21, 12, 30, 0, 0, time.UTC) // 05:30 PDT: off-window; opens 06:00 PDT = 13:00Z
	mgr.now = func() time.Time { return now }
	mgr.SetModelUnavailableCacheTTL(time.Hour)

	instance, err := mgr.EnsureSessionForModel(context.Background(), "rare/model")
	if err != nil {
		t.Fatalf("first admission: %v", err)
	}
	if len(*created) != 2 || (*created)[0] != "rare/model" || (*created)[1] != DefaultFallbackModel() {
		t.Fatalf("creates = %v, want [rare/model, %s]", *created, DefaultFallbackModel())
	}

	// Second admission: cache hit — no new create, fallback session reused.
	instance2, err := mgr.EnsureSessionForModel(context.Background(), "rare/model")
	if err != nil {
		t.Fatalf("second admission: %v", err)
	}
	if instance2 != instance {
		t.Errorf("instance = %q, want %q (fallback reused)", instance2, instance)
	}
	if len(*created) != 2 {
		t.Errorf("creates = %v, want still 2 (skip the 409 roundtrip)", *created)
	}
}

// TestModelUnavailableCacheTTLExpiryReprobes pins window expiry: once the
// cached skip window passes (here: the plain TTL bound), the requested model
// is re-probed and a fresh 409 → fallback cycle happens.
func TestModelUnavailableCacheTTLExpiryReprobes(t *testing.T) {
	mock, created := unavailableMock(t, "rare/model", "") // no window: TTL-only bound
	defer mock.Close()
	mgr := newTestManager(t, mock)
	now := time.Date(2026, 8, 21, 12, 30, 0, 0, time.UTC)
	mgr.now = func() time.Time { return now }
	mgr.SetModelUnavailableCacheTTL(30 * time.Minute)

	if _, err := mgr.EnsureSessionForModel(context.Background(), "rare/model"); err != nil {
		t.Fatalf("first admission: %v", err)
	}
	if len(*created) != 2 {
		t.Fatalf("creates = %v, want 2 after first admission", *created)
	}

	// Within the TTL: skip.
	if _, err := mgr.EnsureSessionForModel(context.Background(), "rare/model"); err != nil {
		t.Fatalf("cached admission: %v", err)
	}
	if len(*created) != 2 {
		t.Fatalf("creates = %v, want 2 (cached skip)", *created)
	}

	// Past the TTL: the cache expired — re-probe (409 again, then fallback).
	now = now.Add(31 * time.Minute)
	if _, err := mgr.EnsureSessionForModel(context.Background(), "rare/model"); err != nil {
		t.Fatalf("post-expiry admission: %v", err)
	}
	if len(*created) != 4 {
		t.Fatalf("creates = %v, want 4 (re-probe + fallback)", *created)
	}
	if (*created)[2] != "rare/model" || (*created)[3] != DefaultFallbackModel() {
		t.Errorf("creates = %v, want re-probe of rare/model then fallback", *created)
	}
}

// TestModelUnavailableCacheWindowBoundaryReprobes pins the window-derived
// bound: with "9am ET-5pm PT every day" (06:00-17:00 PDT) the skip ends at
// the window opening even though the 1h TTL is longer, so the model is
// re-probed the moment it should be available again.
func TestModelUnavailableCacheWindowBoundaryReprobes(t *testing.T) {
	mock, created := unavailableMock(t, "rare/model", "9am ET-5pm PT every day")
	defer mock.Close()
	mgr := newTestManager(t, mock)
	now := time.Date(2026, 8, 21, 12, 30, 0, 0, time.UTC) // 05:30 PDT; window opens 13:00Z (06:00 PDT)
	mgr.now = func() time.Time { return now }
	mgr.SetModelUnavailableCacheTTL(time.Hour)

	if _, err := mgr.EnsureSessionForModel(context.Background(), "rare/model"); err != nil {
		t.Fatalf("first admission: %v", err)
	}
	if len(*created) != 2 {
		t.Fatalf("creates = %v, want 2 after first admission", *created)
	}

	// 12:45Z — still off-window, skip holds (window opens at 13:00Z).
	now = now.Add(15 * time.Minute)
	if _, err := mgr.EnsureSessionForModel(context.Background(), "rare/model"); err != nil {
		t.Fatalf("cached admission: %v", err)
	}
	if len(*created) != 2 {
		t.Fatalf("creates = %v, want 2 (skip before window opens)", *created)
	}

	// 13:05Z — the window has opened: re-probe. The upstream still refuses
	// (the mock always 409s rare/model), so expect another 409 → fallback.
	now = now.Add(20 * time.Minute)
	if _, err := mgr.EnsureSessionForModel(context.Background(), "rare/model"); err != nil {
		t.Fatalf("post-window admission: %v", err)
	}
	if len(*created) != 4 {
		t.Fatalf("creates = %v, want 4 (re-probe + fallback after window opens)", *created)
	}
	if (*created)[2] != "rare/model" || (*created)[3] != DefaultFallbackModel() {
		t.Errorf("creates = %v, want re-probe of rare/model then fallback", *created)
	}
}

// TestModelUnavailableCacheDisabled keeps the pre-#158 behavior when the
// cache TTL is disabled: every admission re-probes (two 409s for two calls).
func TestModelUnavailableCacheDisabled(t *testing.T) {
	mock, created := unavailableMock(t, "rare/model", "9am ET-5pm PT every day")
	defer mock.Close()
	mgr := newTestManager(t, mock)
	mgr.now = func() time.Time { return time.Date(2026, 8, 21, 12, 30, 0, 0, time.UTC) }
	// SetModelUnavailableCacheTTL never called → TTL 0 → cache disabled.

	for i := 0; i < 2; i++ {
		if _, err := mgr.EnsureSessionForModel(context.Background(), "rare/model"); err != nil {
			t.Fatalf("admission %d: %v", i+1, err)
		}
	}
	if len(*created) != 4 {
		t.Fatalf("creates = %v, want 4 (cache disabled: full 409 churn)", *created)
	}
}
