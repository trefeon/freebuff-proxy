package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

func TestStatusErrorModelIPLimited(t *testing.T) {
	limited := &upstream.SessionState{Message: "model kimi/kimi-k2-0725 is limited on this IP", RetryAfterMs: 30000}
	for _, status := range []string{"session_model_mismatch", "limited_ip"} {
		err := statusError(status, limited)
		var lie *upstream.LimitedIpError
		if !errors.As(err, &lie) {
			t.Fatalf("statusError(%q) = %v, want *upstream.LimitedIpError", status, err)
		}
		if !errors.Is(err, upstream.ErrModelIPLimited) {
			t.Errorf("errors.Is(upstream.ErrModelIPLimited) = false, got %v", err)
		}
		if lie.RetryAfter != 30*time.Second {
			t.Errorf("RetryAfter = %s, want 30s", lie.RetryAfter)
		}
	}

	// Non-limited messages keep today's exact unknown-status error text.
	err := statusError("session_model_mismatch", &upstream.SessionState{Message: "session model mismatch"})
	if err == nil || err.Error() != `session: unknown upstream status "session_model_mismatch"` {
		t.Errorf("non-limited statusError = %v, want unknown-status error", err)
	}
}

// TestStatusErrorClampsCooldown pins the parse-time ceiling on the session
// admission path: an absurd upstream retryAfterMs (int64 max) must clamp to
// upstream.MaxCooldown — not wrap the ms→ns multiply into a multi-year
// positive window — across every status that derives a cooldown from it.
func TestStatusErrorClampsCooldown(t *testing.T) {
	huge := &upstream.SessionState{RetryAfterMs: int64(1<<63 - 1)}

	err := statusError("rate_limited", huge)
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("statusError(rate_limited) = %v, want *upstream.RateLimitError", err)
	}
	if rle.RetryAfter != upstream.MaxCooldown {
		t.Errorf("rate_limited RetryAfter = %v, want %v (clamped)", rle.RetryAfter, upstream.MaxCooldown)
	}

	err = statusError("spend_limited", huge)
	if !errors.As(err, &rle) {
		t.Fatalf("statusError(spend_limited) = %v, want *upstream.RateLimitError", err)
	}
	if rle.RetryAfter != upstream.MaxCooldown {
		t.Errorf("spend_limited RetryAfter = %v, want %v (clamped)", rle.RetryAfter, upstream.MaxCooldown)
	}

	err = statusError("ip_capped", huge)
	var ice *upstream.IpCappedError
	if !errors.As(err, &ice) {
		t.Fatalf("statusError(ip_capped) = %v, want *upstream.IpCappedError", err)
	}
	if ice.RetryAfter != upstream.MaxCooldown {
		t.Errorf("ip_capped RetryAfter = %v, want %v (clamped)", ice.RetryAfter, upstream.MaxCooldown)
	}

	err = statusError("limited_ip", &upstream.SessionState{Message: "model is limited on this IP", RetryAfterMs: int64(1<<63 - 1)})
	var lie *upstream.LimitedIpError
	if !errors.As(err, &lie) {
		t.Fatalf("statusError(limited_ip) = %v, want *upstream.LimitedIpError", err)
	}
	if lie.RetryAfter != upstream.MaxCooldown {
		t.Errorf("limited_ip RetryAfter = %v, want %v (clamped)", lie.RetryAfter, upstream.MaxCooldown)
	}

	// Normal values are untouched.
	err = statusError("rate_limited", &upstream.SessionState{RetryAfterMs: 60000})
	if !errors.As(err, &rle) {
		t.Fatalf("statusError(rate_limited) = %v, want *upstream.RateLimitError", err)
	}
	if rle.RetryAfter != 60*time.Second {
		t.Errorf("rate_limited RetryAfter = %v, want 60s", rle.RetryAfter)
	}
}

func TestWaitingRoomThenActive(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionSequence = []string{"queued", "active"}
	mock.EstimatedWaitMs = 400
	mock.QueuePosition = 2
	mock.QueueDepth = 5
	mgr := newTestManager(t, mock)

	_, err := mgr.EnsureSession(context.Background())
	var wr *WaitingRoomError
	if !errors.As(err, &wr) {
		t.Fatalf("want WaitingRoomError, got %v", err)
	}
	if wr.Position != 2 || wr.QueueDepth != 5 {
		t.Errorf("waiting room position/depth = %d/%d", wr.Position, wr.QueueDepth)
	}
	if wr.RetryAfter <= 0 || wr.RetryAfter > time.Second {
		t.Errorf("RetryAfter = %s, want ~400ms", wr.RetryAfter)
	}

	time.Sleep(500 * time.Millisecond)
	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q", instance)
	}
	if mock.SessionPolls != 1 {
		t.Errorf("polls = %d, want 1", mock.SessionPolls)
	}
}

func TestDisabled(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "disabled"
	mgr := newTestManager(t, mock)

	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "" {
		t.Errorf("instance = %q, want empty for disabled", instance)
	}
}

func TestEndedRecreates(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionSequence = []string{"ended", "active"}
	mgr := newTestManager(t, mock)

	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q", instance)
	}
	if mock.SessionCreates != 2 {
		t.Errorf("creates = %d, want 2 (ended → recreate)", mock.SessionCreates)
	}
}

func TestConcurrentQueuedSharedState(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionSequence = []string{"queued", "active"}
	mock.EstimatedWaitMs = 100
	mgr := newTestManager(t, mock)

	var wg sync.WaitGroup
	waitRooms := make([]bool, 8)
	instances := make([]string, 8)
	for i := range waitRooms {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			instance, err := mgr.EnsureSession(context.Background())
			if err == nil {
				instances[i] = instance
			} else {
				var wr *WaitingRoomError
				waitRooms[i] = errors.As(err, &wr)
			}
		}(i)
	}
	wg.Wait()

	// All callers must either get the waiting room error or the instance;
	// the shared state must not race (exercised under -race).
	gotInstance := false
	for i := range waitRooms {
		if !waitRooms[i] && instances[i] == "" {
			t.Errorf("caller %d: neither waiting room nor instance", i)
		}
		if instances[i] != "" {
			gotInstance = true
		}
	}
	if !gotInstance {
		// All queued is legal; but then no one may hold garbage.
		t.Log("all callers observed the waiting room")
	}
}

func TestCtxCancelPropagates(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionCreateDelay = 2 * time.Second
	mgr := newTestManager(t, mock)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := mgr.EnsureSession(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
}

func TestBannedSessionReturnsError(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "banned"
	mgr := newTestManager(t, mock)

	_, err := mgr.EnsureSession(context.Background())
	if err == nil {
		t.Fatal("want error for banned session")
	}
	if !strings.Contains(err.Error(), "banned") {
		t.Errorf("error = %q, want banned message", err)
	}
}

// TestCountryBlockedSessionReturnsTypedError verifies a country_blocked
// admission surfaces as a CountryBlockedError with the parsed region fields
// (the pre-change code returned a plain fmt.Errorf).
func TestCountryBlockedSessionReturnsTypedError(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"country_blocked","countryCode":"US","countryBlockReason":"Free mode is not available in your country","ipPrivacySignals":["vpn"]}`)
	}
	mgr := newTestManager(t, mock)

	_, err := mgr.EnsureSession(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	var cbe *upstream.CountryBlockedError
	if !errors.As(err, &cbe) {
		t.Fatalf("want *upstream.CountryBlockedError, got %v", err)
	}
	if cbe.CountryCode != "US" || cbe.CountryBlockReason != "Free mode is not available in your country" {
		t.Errorf("country block fields = %+v", cbe)
	}
	if len(cbe.IpPrivacySignals) != 1 || cbe.IpPrivacySignals[0] != "vpn" {
		t.Errorf("ipPrivacySignals = %v", cbe.IpPrivacySignals)
	}
	if !errors.Is(err, upstream.ErrCountryBlocked) {
		t.Error("not unwrap-able to ErrCountryBlocked")
	}
}

func TestModelLockedRecreates(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionSequence = []string{"model_locked", "active"}
	mgr := newTestManager(t, mock)

	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q", instance)
	}
	if mock.SessionCreates != 2 {
		t.Errorf("creates = %d, want 2 (model_locked → recreate)", mock.SessionCreates)
	}
}
func TestModelUnavailableFallback(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var createdModels []string
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			model := r.Header.Get("x-freebuff-model")
			createdModels = append(createdModels, model)
			w.Header().Set("Content-Type", "application/json")
			if model == "rare/model" {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"status":"model_unavailable","requestedModel":"rare/model"}`)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-fallback","model":"`+model+`","expiresAt":"2030-01-01T00:00:00Z"}`)
			return
		}
		http.NotFound(w, r)
	}

	mgr := newTestManager(t, mock)
	instance, err := mgr.EnsureSessionForModel(context.Background(), "rare/model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if instance != "inst-fallback" {
		t.Errorf("instance = %q, want inst-fallback", instance)
	}
	if len(createdModels) != 2 {
		t.Fatalf("createdModels = %v, want 2 attempts", createdModels)
	}
	if createdModels[0] != "rare/model" || createdModels[1] != "deepseek/deepseek-v4-flash" {
		t.Errorf("createdModels = %v, want rare/model then fallback", createdModels)
	}
}

func TestRateLimitedError(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"status":"rate_limited","retryAfterMs":45000,"limit":5,"recentCount":5}`)
	}

	mgr := newTestManager(t, mock)
	_, err := mgr.EnsureSession(context.Background())
	if err == nil {
		t.Fatal("want error on rate limited session")
	}
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("want RateLimitError, got %T: %v", err, err)
	}
	if rle.RetryAfter != 45*time.Second {
		t.Errorf("RetryAfter = %v, want 45s", rle.RetryAfter)
	}
}

// TestEnsureSessionRidesGraceFastPath verifies gap #13 on the fast path: an
// active cache entry whose expiry margin has passed is still reusable while
// its instance id survives the 30-minute grace drain, and once grace closes
// the next EnsureSession re-admits.
func TestEnsureSessionRidesGraceFastPath(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	// Active-but-expired cache, still within the grace drain.
	mgr.mu.Lock()
	mgr.commit(&cachedState{
		status:            "active",
		instanceID:        "inst-grace",
		model:             "deepseek/deepseek-v4-pro",
		expiresAt:         time.Now().Add(-10 * time.Minute),
		gracePeriodEndsAt: time.Now().Add(20 * time.Minute),
	})
	mgr.mu.Unlock()

	creates := mock.SessionCreates
	instance, err := mgr.EnsureSessionForModel(context.Background(), "deepseek/deepseek-v4-pro")
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-grace" {
		t.Errorf("instance = %q, want inst-grace (ride through grace)", instance)
	}
	if mock.SessionCreates != creates {
		t.Errorf("session creates = %d, want %d (no fresh admission inside grace)", mock.SessionCreates, creates)
	}

	// Past the grace window: the fast path falls through and re-admits.
	mgr.mu.Lock()
	mgr.commit(&cachedState{
		status:            "active",
		instanceID:        "inst-grace",
		model:             "deepseek/deepseek-v4-pro",
		expiresAt:         time.Now().Add(-45 * time.Minute),
		gracePeriodEndsAt: time.Now().Add(-10 * time.Minute),
	})
	mgr.mu.Unlock()

	if _, err := mgr.EnsureSessionForModel(context.Background(), "deepseek/deepseek-v4-pro"); err != nil {
		t.Fatal(err)
	}
	if mock.SessionCreates != creates+1 {
		t.Errorf("session creates = %d, want %d (re-admit after grace closes)", mock.SessionCreates, creates+1)
	}
}

// TestRecreateStatusesRecreates pins status-matrix parity for the
// transparently-recreated statuses: "ended" is tested elsewhere, and
// "superseded"/"none" must behave identically (drop the stale slot, create
// again).
func TestRecreateStatusesRecreates(t *testing.T) {
	for _, status := range []string{"superseded", "none"} {
		t.Run(status, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mock.SessionSequence = []string{status, "active"}
			mgr := newTestManager(t, mock)

			instance, err := mgr.EnsureSession(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if instance != "inst-abc-123" {
				t.Errorf("instance = %q", instance)
			}
			if mock.SessionCreates != 2 {
				t.Errorf("creates = %d, want 2 (%s → recreate)", mock.SessionCreates, status)
			}
		})
	}
}

// TestModelLockedReleasesOldSlot verifies the model_locked branch releases
// the OLD upstream slot (SessionEnds == 1) before retrying with the desired
// model: the model-switch invariant that a locked slot is not leaked.
func TestModelLockedReleasesOldSlot(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	var creates, ends atomic.Int32
	var mu sync.Mutex
	bAttempts := 0
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			model := r.Header.Get("x-freebuff-model")
			creates.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if model == "model/A" {
				_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-A","model":"model/A","expiresAt":"2030-01-01T00:00:00Z"}`)
				return
			}
			// model/B: the first attempt is locked, the retry succeeds.
			mu.Lock()
			bAttempts++
			attempt := bAttempts
			mu.Unlock()
			if attempt == 1 {
				_, _ = io.WriteString(w, `{"status":"model_locked","currentModel":"model/A","requestedModel":"model/B"}`)
				return
			}
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-B","model":"model/B","expiresAt":"2030-01-01T00:00:00Z"}`)
		case http.MethodDelete:
			ends.Add(1)
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"status":"ended"}`)
		default:
			http.NotFound(w, r)
		}
	}

	if _, err := mgr.EnsureSessionForModel(context.Background(), "model/A"); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.EnsureSessionForModel(context.Background(), "model/B"); err != nil {
		t.Fatal(err)
	}
	// The model_locked response is itself a create (POST), so the total is:
	// model/A create + model/B locked create + model/B retry create.
	if got := creates.Load(); got != 3 {
		t.Errorf("creates = %d, want 3 (model/A + locked model/B + retry model/B)", got)
	}
	if got := ends.Load(); got != 1 {
		t.Errorf("ends = %d, want 1 (old model/A slot released on model lock)", got)
	}
	if snap := mgr.Snapshot(); snap.InstanceID != "inst-B" {
		t.Errorf("final instance = %q, want inst-B", snap.InstanceID)
	}
	// Issue #160: the release is metered. One model_locked admission for
	// (model/A → model/B) must land exactly one counter event.
	if got := mgr.ModelLocked()["model/A"]["model/B"]; got != 1 {
		t.Errorf("ModelLocked[model/A][model/B] = %d, want 1 (all: %v)", got, mgr.ModelLocked())
	}
}

// TestModelLockedCounter pins the issue #160 counter's accumulation
// semantics: every model_locked admission releases the old slot and
// re-admits with the desired model, and the from → to pair counts each
// release — two switches A→B (each locked once upstream) must read 2, not
// a set/overwrite.
func TestModelLockedCounter(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	var mu sync.Mutex
	bAttempts := 0
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			model := r.Header.Get("x-freebuff-model")
			w.Header().Set("Content-Type", "application/json")
			if model == "model/A" {
				_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-A","model":"model/A","expiresAt":"2030-01-01T00:00:00Z"}`)
				return
			}
			// model/B: alternate locked → active so each A→B switch
			// hits exactly one model_locked admission (the retry in the
			// same refresh loop succeeds on the next iteration).
			mu.Lock()
			bAttempts++
			attempt := bAttempts
			mu.Unlock()
			if attempt%2 == 1 {
				_, _ = io.WriteString(w, `{"status":"model_locked","currentModel":"model/A","requestedModel":"model/B"}`)
				return
			}
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-B","model":"model/B","expiresAt":"2030-01-01T00:00:00Z"}`)
		case http.MethodDelete:
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"status":"ended"}`)
		default:
			http.NotFound(w, r)
		}
	}

	// Switch A→B (locked), back to A (no lock), then A→B again (locked).
	for _, model := range []string{"model/A", "model/B", "model/A", "model/B"} {
		if _, err := mgr.EnsureSessionForModel(context.Background(), model); err != nil {
			t.Fatalf("EnsureSessionForModel(%s): %v", model, err)
		}
	}

	locked := mgr.ModelLocked()
	if got := locked["model/A"]["model/B"]; got != 2 {
		t.Errorf("ModelLocked[model/A][model/B] = %d, want 2 (two locked switches; all: %v)", got, locked)
	}
	if len(locked) != 1 {
		t.Errorf("ModelLocked = %v, want exactly one from→to pair", locked)
	}
	// Sanity: the upstream really saw the four model/B creates and two
	// releases (locked + retry per switch).
	if bAttempts != 4 {
		t.Errorf("model/B creates = %d, want 4 (2 locked + 2 retries)", bAttempts)
	}
}

// TestRefreshBudgetExhaustedAlwaysNone verifies the create/poll loop is
// bounded: an upstream that always returns "none" burns exactly
// maxRefreshIterations creates, then errors out (no infinite loop).
func TestRefreshBudgetExhaustedAlwaysNone(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "none"
	mgr := newTestManager(t, mock)

	_, err := mgr.EnsureSession(context.Background())
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("err = %v, want refresh budget exhaustion error", err)
	}
	if mock.SessionCreates != maxRefreshIterations {
		t.Errorf("creates = %d, want %d (exactly the iteration budget, no infinite loop)", mock.SessionCreates, maxRefreshIterations)
	}
}

// TestEnsureSessionOuterBudgetExhausted pins the outer-cap path: an upstream
// that always returns "queued" with a pollAt in the past makes every refresh
// succeed without progress, so EnsureSession must stop after
// maxOuterIterations attempts with "not ready after repeated refreshes"
// (creates exactly once; every later attempt is a poll).
func TestEnsureSessionOuterBudgetExhausted(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "queued"
	mock.EstimatedWaitMs = 0 // mock formats pollAt with ms precision → always in the past
	mgr := newTestManager(t, mock)

	_, err := mgr.EnsureSession(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not ready after repeated refreshes") {
		t.Fatalf("err = %v, want 'not ready after repeated refreshes'", err)
	}
	if mock.SessionCreates != 1 {
		t.Errorf("creates = %d, want 1 (only the first refresh creates)", mock.SessionCreates)
	}
	if mock.SessionPolls != maxOuterIterations-1 {
		t.Errorf("polls = %d, want %d (one poll per remaining outer iteration)", mock.SessionPolls, maxOuterIterations-1)
	}
}

// TestQueuedZeroPollAtClamp covers the S2 dead path: a queued response with
// a zero pollAt is clamped to max(1s, min(5s, estimatedWaitMs)) instead of
// parking the caller at "now" (the mock always sends pollAt, so the clamp is
// exercised through a custom handler).
func TestQueuedZeroPollAtClamp(t *testing.T) {
	tests := []struct {
		name   string
		waitMs int
		min    time.Duration
		max    time.Duration
	}{
		{"zero wait → 1s floor", 0, 800 * time.Millisecond, 1500 * time.Millisecond},
		{"3s wait kept", 3000, 2800 * time.Millisecond, 3200 * time.Millisecond},
		{"10s wait → 5s cap", 10000, 4500 * time.Millisecond, 5500 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, fmt.Sprintf(`{"status":"queued","instanceId":"inst-q","position":1,"queueDepth":3,"estimatedWaitMs":%d}`, tt.waitMs))
			}
			mgr := newTestManager(t, mock)

			_, err := mgr.EnsureSession(context.Background())
			var wr *WaitingRoomError
			if !errors.As(err, &wr) {
				t.Fatalf("want WaitingRoomError, got %v", err)
			}
			if wr.RetryAfter < tt.min || wr.RetryAfter > tt.max {
				t.Errorf("RetryAfter = %s, want in [%s, %s] (zero-pollAt clamp)", wr.RetryAfter, tt.min, tt.max)
			}
		})
	}
}

// TestLiveModelSwitchDoesNotReleaseOldSlot pins the B2 doc-vs-code
// divergence: EnsureSessionForModel's doc claims a live model switch
// "releases the previous slot", but the live-refresh path only creates for
// the new model — the old upstream slot is never DELETE'd (only the
// model_locked branch releases). The CURRENT behavior (creates==2, ends==0)
// is pinned here; do not change it without also changing the doc.
func TestLiveModelSwitchDoesNotReleaseOldSlot(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	var creates, ends atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			model := r.Header.Get("x-freebuff-model")
			creates.Add(1)
			instance := "inst-B"
			if model == "model/A" {
				instance = "inst-A"
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"`+instance+`","model":"`+model+`","expiresAt":"2030-01-01T00:00:00Z"}`)
		case http.MethodDelete:
			ends.Add(1)
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"status":"ended"}`)
		default:
			http.NotFound(w, r)
		}
	}

	if _, err := mgr.EnsureSessionForModel(context.Background(), "model/A"); err != nil {
		t.Fatal(err)
	}
	if got := creates.Load(); got != 1 {
		t.Fatalf("creates after model/A = %d, want 1", got)
	}

	instance, err := mgr.EnsureSessionForModel(context.Background(), "model/B")
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-B" {
		t.Errorf("instance = %q, want inst-B (fresh create for model/B)", instance)
	}
	if got := creates.Load(); got != 2 {
		t.Errorf("creates = %d, want 2", got)
	}
	if got := ends.Load(); got != 0 {
		t.Errorf("ends = %d, want 0 (current code does NOT release the old slot on live model switch — B2 divergence pinned)", got)
	}
}

// TestCacheRecordsUpstreamServedModel pins the honest-cache contract: when
// upstream binds an admitted session to a different model than requested
// (e.g. a limited-tier token pinned to mimo), the cache stores the SERVED
// model, not what the client asked for. A later request for the coerced
// model then reuses the session instead of pointlessly re-admitting.
func TestCacheRecordsUpstreamServedModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	var creates atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		creates.Add(1)
		// Upstream coercion: whatever was asked, serve model/B's slot.
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-coerced","model":"model/B","expiresAt":"2030-01-01T00:00:00Z"}`)
	}

	if _, err := mgr.EnsureSessionForModel(context.Background(), "model/A"); err != nil {
		t.Fatal(err)
	}
	if snap := mgr.Snapshot(); snap.Model != "model/B" {
		t.Fatalf("cached model = %q, want upstream-served model/B", snap.Model)
	}

	instance, err := mgr.EnsureSessionForModel(context.Background(), "model/B")
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-coerced" {
		t.Errorf("instance = %q, want inst-coerced (cache reused for the coerced model)", instance)
	}
	if got := creates.Load(); got != 1 {
		t.Errorf("creates = %d, want 1 (no re-admission for the served model)", got)
	}
}

// TestActiveSessionWithoutModelServesAnyModel pins the S1 leniency: a
// session created via the default-model path (cached model "") is reused for
// ANY requested model — the cache-hit check treats "" as a wildcard. The
// upstream may later reject with session_model_mismatch; the leniency is
// current behavior and must not silently change.
func TestActiveSessionWithoutModelServesAnyModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	// The mock's active body carries no model field → cached model stays "".
	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snap := mgr.Snapshot(); snap.Model != "" {
		t.Fatalf("cached model = %q, want empty (default-model create)", snap.Model)
	}

	instance, err := mgr.EnsureSessionForModel(context.Background(), "anything/model")
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q, want inst-abc-123 (cache reused)", instance)
	}
	if mock.SessionCreates != 1 {
		t.Errorf("creates = %d, want 1 (model-less session reused for any model)", mock.SessionCreates)
	}
}

func TestLeaderCancellationDecoupling(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	leaderBlockCh := make(chan struct{})
	leaderStartedCh := make(chan struct{})
	var createCount atomic.Int32

	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			count := createCount.Add(1)
			if count == 1 {
				close(leaderStartedCh)
				<-leaderBlockCh
				// The leader's create must deterministically observe the
				// cancellation: wait for the request context to be done
				// before returning, so the mock's response cannot race the
				// cancel and let the leader "succeed" (a -race timing
				// flake). A bounded fallback prevents a hang if the cancel
				// never arrives.
				select {
				case <-r.Context().Done():
					return // canceled: no response, client sees context.Canceled
				case <-time.After(2 * time.Second):
				}
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"status":"active","instanceId":"leader-inst","model":"model/A"}`)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"waiter-inst","model":"model/A"}`)
			return
		}
	}

	mgr := newTestManager(t, mock)

	leaderCtx, leaderCancel := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := mgr.EnsureSessionForModel(leaderCtx, "model/A")
		leaderDone <- err
	}()

	// Wait until leader starts refresh
	<-leaderStartedCh

	// Waiter starts with active context
	waiterDone := make(chan struct {
		inst string
		err  error
	}, 1)
	go func() {
		inst, err := mgr.EnsureSessionForModel(context.Background(), "model/A")
		waiterDone <- struct {
			inst string
			err  error
		}{inst, err}
	}()

	// Give waiter time to park on leader's refreshCh
	time.Sleep(50 * time.Millisecond)

	// Cancel leader context and unblock mock handler
	leaderCancel()
	close(leaderBlockCh)

	leaderErr := <-leaderDone
	if !errors.Is(leaderErr, context.Canceled) {
		t.Fatalf("leader err = %v, want context.Canceled", leaderErr)
	}

	// Waiter should recover, become candidate leader, and succeed
	select {
	case res := <-waiterDone:
		if res.err != nil {
			t.Fatalf("waiter err = %v, want nil", res.err)
		}
		if res.inst != "waiter-inst" {
			t.Errorf("waiter inst = %q, want waiter-inst", res.inst)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for waiter to complete")
	}
}

func TestModelLockedFallbackInstance(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	var deleteInstanceIDs []string
	var mu sync.Mutex
	var callCount atomic.Int32

	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			count := callCount.Add(1)
			if count == 1 {
				// Return model_locked with instanceId
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"status":"model_locked","currentModel":"model/old","instanceId":"locked-inst-123"}`)
				return
			}
			// Second call succeeds
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"active-inst-456","model":"model/new"}`)
		case http.MethodDelete:
			mu.Lock()
			// #120: the CLI DELETEs with Bearer only — no instance header
			// (reference/freebuff freebuff-session-api.ts releaseFreebuffSlot).
			deleteInstanceIDs = append(deleteInstanceIDs, r.Header.Get("x-freebuff-instance-id"))
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"ended"}`)
		}
	}

	mgr := newTestManager(t, mock)

	// Initial ensure with model/new when mgr has no cached state
	instance, err := mgr.EnsureSessionForModel(context.Background(), "model/new")
	if err != nil {
		t.Fatalf("EnsureSessionForModel failed: %v", err)
	}
	if instance != "active-inst-456" {
		t.Errorf("instance = %q, want active-inst-456", instance)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(deleteInstanceIDs) != 1 {
		t.Fatalf("EndSession calls = %d, want 1", len(deleteInstanceIDs))
	}
	if deleteInstanceIDs[0] != "" {
		t.Errorf("DELETE x-freebuff-instance-id = %q, want absent (#120: session DELETE is Bearer-only)", deleteInstanceIDs[0])
	}
}
