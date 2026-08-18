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

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

func newTestManager(t *testing.T, mock *testutil.MockUpstream) *Manager {
	t.Helper()
	client, err := upstream.New("tok", &config.Config{
		UpstreamBaseURL:    mock.URL(),
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RotationInterval:   6 * time.Hour,
		RegistryRefresh:    6 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewManager(client)
}

func TestCreateActive(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q", instance)
	}
	if mock.SessionCreates != 1 {
		t.Errorf("creates = %d, want 1", mock.SessionCreates)
	}

	// Second call is served from cache.
	instance, err = mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q", instance)
	}
	if mock.SessionCreates != 1 {
		t.Errorf("creates = %d, want still 1", mock.SessionCreates)
	}
}

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

func TestExpiredCacheRefreshes(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Expiry beyond the 30-min grace window (issue #115): a session that
	// expired more than graceWindow ago must refresh. Expiries inside the
	// window are reusable (TestEnsureSessionRidesGraceFastPath).
	mock.ExpiresIn = -31 * time.Minute
	mgr := newTestManager(t, mock)

	// First call: no cache → one create, state trusted on return.
	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q", instance)
	}
	if mock.SessionCreates != 1 {
		t.Errorf("creates = %d, want 1", mock.SessionCreates)
	}

	// Second call: stale cache past grace → refresh (create #2).
	instance, err = mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q", instance)
	}
	if mock.SessionCreates != 2 {
		t.Errorf("creates = %d, want 2 (stale cache past grace → refresh)", mock.SessionCreates)
	}
}

// TestSingleFlightFailureBounded verifies a failed refresh is NOT amplified:
// N concurrent callers must trigger exactly 1 upstream create and all N must
// surface the retained refresh error (instead of each becoming the next
// refresher and re-running the failing POST).
func TestSingleFlightFailureBounded(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimit = true // every route returns 429 rate_limited
	mgr := newTestManager(t, mock)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = mgr.EnsureSession(context.Background())
		}(i)
	}
	wg.Wait()

	if mock.Requests != 1 {
		t.Errorf("upstream requests = %d, want exactly 1 (single-flight failure must not amplify)", mock.Requests)
	}
	for i, err := range errs {
		if err == nil {
			t.Errorf("caller %d got nil error, want the retained refresh error", i)
			continue
		}
		var rle *upstream.RateLimitError
		if !errors.As(err, &rle) {
			t.Errorf("caller %d error = %T %v, want RateLimitError", i, err, err)
		}
	}
}

// TestPoll404Recreates verifies a poll 404 is treated as ended (recreate
// path) rather than a cached permanent "disabled": the session manager must
// re-create the session after the upstream reports it gone.
func TestPoll404Recreates(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionSequence = []string{"queued", "404"}
	mock.EstimatedWaitMs = 100
	mgr := newTestManager(t, mock)

	_, err := mgr.EnsureSession(context.Background())
	var wr *WaitingRoomError
	if !errors.As(err, &wr) {
		t.Fatalf("want WaitingRoomError from queued create, got %v", err)
	}
	if mock.SessionCreates != 1 {
		t.Errorf("creates = %d, want 1", mock.SessionCreates)
	}

	// Wait for pollAt (queued minimum wait is 1s), then poll → 404 → ended →
	// recreate.
	time.Sleep(1100 * time.Millisecond)
	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q, want inst-abc-123 (recreated after poll 404)", instance)
	}
	if mock.SessionPolls != 1 {
		t.Errorf("polls = %d, want 1", mock.SessionPolls)
	}
	if mock.SessionCreates != 2 {
		t.Errorf("creates = %d, want 2 (poll 404 → recreate)", mock.SessionCreates)
	}
}

func TestSingleFlight(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatDelay = 150 * time.Millisecond // slow create
	mgr := newTestManager(t, mock)

	const n = 10
	var wg sync.WaitGroup
	results := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = mgr.EnsureSession(context.Background())
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if results[i] != "inst-abc-123" {
			t.Errorf("caller %d instance = %q", i, results[i])
		}
	}
	if mock.SessionCreates != 1 {
		t.Errorf("creates = %d, want 1 (single-flight)", mock.SessionCreates)
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

func TestInvalidateRefreshes(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr.Invalidate()
	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.SessionCreates != 2 {
		t.Errorf("creates = %d, want 2 after Invalidate", mock.SessionCreates)
	}
}

func TestEndSession(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mgr.EndSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.SessionEnds != 1 {
		t.Errorf("ends = %d, want 1", mock.SessionEnds)
	}
	// Cache cleared: next ensure re-creates.
	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mock.SessionCreates != 2 {
		t.Errorf("creates = %d, want 2 after EndSession", mock.SessionCreates)
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

func TestSnapshotModelAndExpiresAt(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	_, err := mgr.EnsureSessionForModel(context.Background(), "thudm/glm-5.2")
	if err != nil {
		t.Fatal(err)
	}

	snap := mgr.Snapshot()
	if snap.Status != "active" {
		t.Errorf("Status = %q, want active", snap.Status)
	}
	if snap.Model != "thudm/glm-5.2" {
		t.Errorf("Model = %q, want thudm/glm-5.2", snap.Model)
	}
	if snap.ExpiresAt.IsZero() {
		t.Error("ExpiresAt should not be zero")
	}
	// GracePeriodEndsAt (write-only cache field) is surfaced in the snapshot.
	if snap.GracePeriodEndsAt.IsZero() {
		t.Error("GracePeriodEndsAt should not be zero")
	}
	if want := snap.ExpiresAt.Add(graceWindow); !snap.GracePeriodEndsAt.Equal(want) {
		t.Errorf("GracePeriodEndsAt = %v, want %v (expiresAt + graceWindow)", snap.GracePeriodEndsAt, want)
	}
}

func TestSnapshotQuotaByModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimitsByModel = map[string]any{
		"z-ai/glm-5.2": map[string]any{
			"model":       "z-ai/glm-5.2",
			"limit":       5,
			"recentCount": 4,
			"period":      "pacific_day",
			"resetAt":     "2026-08-16T07:00:00.000Z",
			"entitlementBreakdown": map[string]any{
				"base":     1,
				"referral": 1,
				"streak":   3,
			},
		},
	}
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	snap := mgr.Snapshot()
	q, ok := snap.QuotaByModel["z-ai/glm-5.2"]
	if !ok {
		t.Fatalf("QuotaByModel missing z-ai/glm-5.2: %+v", snap.QuotaByModel)
	}
	if q.Limit != 5 || q.RecentCount != 4 {
		t.Errorf("quota limit/recentCount = %v/%v, want 5/4", q.Limit, q.RecentCount)
	}
	if q.Period != "pacific_day" {
		t.Errorf("period = %q, want pacific_day", q.Period)
	}
	if q.ResetAt.IsZero() {
		t.Error("resetAt not surfaced")
	}
	if q.Entitlement["referral"] != 1 || q.Entitlement["streak"] != 3 {
		t.Errorf("entitlement = %+v, want referral=1 streak=3", q.Entitlement)
	}
	if len(snap.Entitlement) != 0 {
		t.Errorf("top-level Entitlement = %+v, want empty (nested per model)", snap.Entitlement)
	}
}

func TestPoll(t *testing.T) {
	t.Run("inactive session returns nil", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)

		if err := mgr.Poll(context.Background()); err != nil {
			t.Fatalf("Poll inactive: %v", err)
		}
	})

	t.Run("active session polls compact without heartbeat header", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)

		_, err := mgr.EnsureSession(context.Background())
		if err != nil {
			t.Fatal(err)
		}

		var gotCompact, gotHeartbeat string
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			gotCompact = r.Header.Get("x-freebuff-compact-session")
			gotHeartbeat = r.Header.Get("x-freebuff-heartbeat")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-abc-123"}`)
		}

		if err := mgr.Poll(context.Background()); err != nil {
			t.Fatalf("Poll: %v", err)
		}
		// Gap #2: the CLI never beats — x-freebuff-heartbeat is
		// Desktop-only (reference/freebuff freebuff-models.ts:1212-1215);
		// liveness comes from the recurring compact GET.
		if gotCompact != "1" {
			t.Errorf("x-freebuff-compact-session = %q, want 1", gotCompact)
		}
		if gotHeartbeat != "" {
			t.Errorf("x-freebuff-heartbeat = %q, want absent on polls", gotHeartbeat)
		}
		if snap := mgr.Snapshot(); snap.Status != "active" {
			t.Errorf("status = %q, want active", snap.Status)
		}
	})

	t.Run("ended session status invalidates state", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)

		_, err := mgr.EnsureSession(context.Background())
		if err != nil {
			t.Fatal(err)
		}

		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123"}`)
		}

		if err := mgr.Poll(context.Background()); err != nil {
			t.Fatalf("Poll: %v", err)
		}
		if snap := mgr.Snapshot(); snap.Status != "" {
			t.Errorf("status = %q, want empty after invalidation", snap.Status)
		}
	})
}

// TestPollRidesGraceEndedWithInstance verifies gap #13 on the poll path: an
// "ended" response that still carries the instance id (with a future grace
// end) is kept as a usable ended-with-instance row — the fast path keeps
// serving it until grace closes, with no fresh admission.
func TestPollRidesGraceEndedWithInstance(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	graceEnd := time.Now().Add(20 * time.Minute).UTC().Format(time.RFC3339)
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ended","instanceId":"inst-abc-123","gracePeriodEndsAt":"`+graceEnd+`"}`)
	}

	if err := mgr.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	snap := mgr.Snapshot()
	if snap.Status != "ended" || snap.InstanceID != "inst-abc-123" {
		t.Fatalf("snapshot = %+v, want ended inst-abc-123 (in-grace row kept)", snap)
	}

	// The fast path reuses the in-grace slot: no upstream create.
	creates := mock.SessionCreates
	instance, err := mgr.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance != "inst-abc-123" {
		t.Errorf("instance = %q, want inst-abc-123 (ride through grace)", instance)
	}
	if mock.SessionCreates != creates {
		t.Errorf("session creates = %d, want %d (no fresh admission inside grace)", mock.SessionCreates, creates)
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

// TestPollEndedPastGraceInvalidates verifies an "ended" poll response whose
// grace window has already closed (or that carries no instance id) drops the
// cached slot so the next EnsureSession re-creates fresh.
func TestPollEndedPastGraceInvalidates(t *testing.T) {
	for name, body := range map[string]string{
		"past grace":  `{"status":"ended","instanceId":"inst-abc-123","gracePeriodEndsAt":"2020-01-01T00:00:00Z"}`,
		"no instance": `{"status":"ended"}`,
	} {
		t.Run(name, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mgr := newTestManager(t, mock)

			if _, err := mgr.EnsureSession(context.Background()); err != nil {
				t.Fatal(err)
			}

			mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			}

			if err := mgr.Poll(context.Background()); err != nil {
				t.Fatalf("Poll: %v", err)
			}
			if snap := mgr.Snapshot(); snap.Status != "" {
				t.Errorf("status = %q, want empty after %s ended poll", snap.Status, name)
			}
		})
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

// TestPollInvalidatesRecreateStatuses verifies Poll invalidates the cached
// admission for "superseded"/"none" polls exactly like "ended" (status
// parity for the poll path).
func TestPollInvalidatesRecreateStatuses(t *testing.T) {
	for _, status := range []string{"superseded", "none"} {
		t.Run(status, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mgr := newTestManager(t, mock)

			if _, err := mgr.EnsureSession(context.Background()); err != nil {
				t.Fatal(err)
			}

			mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"`+status+`","instanceId":"inst-abc-123"}`)
			}

			if err := mgr.Poll(context.Background()); err != nil {
				t.Fatalf("Poll: %v", err)
			}
			if snap := mgr.Snapshot(); snap.Status != "" {
				t.Errorf("status = %q, want empty after %s invalidation", snap.Status, status)
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

// TestPollTransportErrorKeepsCachedState verifies a transport error on the
// session poll surfaces as an error (pool backoff path) while the cached
// active admission stays intact — the transport failure did not prove the
// session dead.
func TestPollTransportErrorKeepsCachedState(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Subsequent poll GET hangs up the connection (transport error).
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("mock server does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack failed: %v", err)
			return
		}
		_ = conn.Close()
	}

	if err := mgr.Poll(context.Background()); err == nil {
		t.Fatal("poll transport error must surface, got nil")
	}
	snap := mgr.Snapshot()
	if snap.Status != "active" {
		t.Errorf("status = %q, want active kept after transport error", snap.Status)
	}
	if snap.InstanceID != "inst-abc-123" {
		t.Errorf("instance = %q, want inst-abc-123 kept", snap.InstanceID)
	}
}

// TestEndSessionSwallowsSessionInvalid verifies EndSession returns nil when
// the upstream DELETE fails with a 400 session_superseded (ErrSessionInvalid
// — the slot is already gone, nothing to do).
func TestEndSessionSwallowsSessionInvalid(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"session_superseded"}`)
			return
		}
		http.NotFound(w, r)
	}

	if err := mgr.EndSession(context.Background()); err != nil {
		t.Fatalf("EndSession with 400 session_superseded = %v, want nil (ErrSessionInvalid swallowed)", err)
	}
}

// TestEndSessionSurfacesServerError verifies EndSession surfaces a generic
// upstream error (500) instead of swallowing it.
func TestEndSessionSurfacesServerError(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"boom"}`)
			return
		}
		http.NotFound(w, r)
	}

	if err := mgr.EndSession(context.Background()); err == nil {
		t.Fatal("EndSession with 500 = nil, want error surfaced")
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
func TestPollStatusErrors(t *testing.T) {
	t.Run("banned returns BanError and clears cached admission", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)

		if _, err := mgr.EnsureSession(context.Background()); err != nil {
			t.Fatal(err)
		}

		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"banned","resumes_at":"2026-08-16T12:00:00Z"}`)
		}

		err := mgr.Poll(context.Background())
		var be *upstream.BanError
		if !errors.As(err, &be) {
			t.Fatalf("want *upstream.BanError, got %v", err)
		}
		if !errors.Is(err, upstream.ErrBanned) {
			t.Error("not unwrap-able to ErrBanned")
		}
		if be.ResumesAt.IsZero() {
			t.Error("resumes_at not parsed into BanError")
		}
		if snap := mgr.Snapshot(); snap.Status != "" {
			t.Errorf("status = %q, want cleared after ban cooldown", snap.Status)
		}
	})

	t.Run("country_blocked returns CountryBlockedError", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)

		if _, err := mgr.EnsureSession(context.Background()); err != nil {
			t.Fatal(err)
		}

		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"country_blocked","countryCode":"US","countryBlockReason":"region restricted","ipPrivacySignals":["proxy"]}`)
		}

		err := mgr.Poll(context.Background())
		var cbe *upstream.CountryBlockedError
		if !errors.As(err, &cbe) {
			t.Fatalf("want *upstream.CountryBlockedError, got %v", err)
		}
		if cbe.CountryCode != "US" || cbe.CountryBlockReason != "region restricted" {
			t.Errorf("country block fields = %+v", cbe)
		}
		if !errors.Is(err, upstream.ErrCountryBlocked) {
			t.Error("not unwrap-able to ErrCountryBlocked")
		}
	})

	t.Run("rate_limited returns RateLimitError", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mgr := newTestManager(t, mock)

		if _, err := mgr.EnsureSession(context.Background()); err != nil {
			t.Fatal(err)
		}

		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"rate_limited","retryAfterMs":45000,"limit":5,"recentCount":5}`)
		}

		err := mgr.Poll(context.Background())
		var rle *upstream.RateLimitError
		if !errors.As(err, &rle) {
			t.Fatalf("want *upstream.RateLimitError, got %v", err)
		}
		if !errors.Is(err, upstream.ErrRateLimited) {
			t.Error("not unwrap-able to ErrRateLimited")
		}
		if rle.RetryAfter != 45*time.Second {
			t.Errorf("RetryAfter = %s, want 45s", rle.RetryAfter)
		}
	})
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
	if deleteInstanceIDs[0] != "locked-inst-123" {
		t.Errorf("EndSession instanceID = %q, want locked-inst-123", deleteInstanceIDs[0])
	}
}

// ── Wave 1 issue tests (#81) ─────────────────────────────────────────────

// TestPollIpCapped verifies #81: an ip_capped session status maps to
// the distinct upstream.IpCappedError (admission-only, bounded to
// retryAfterMs — never the Pacific-midnight quota lock), NOT a
// RateLimitError.
func TestPollIpCapped(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ip_capped","activeUsersForIp":5,"limit":4,"retryAfterMs":30000}`)
	}

	err := mgr.Poll(context.Background())
	if errors.Is(err, upstream.ErrRateLimited) {
		t.Fatal("ip_capped mapped to ErrRateLimited, want distinct ErrIpCapped")
	}
	var ice *upstream.IpCappedError
	if !errors.As(err, &ice) {
		t.Fatalf("want *upstream.IpCappedError, got %v", err)
	}
	if !errors.Is(err, upstream.ErrIpCapped) {
		t.Error("not unwrap-able to ErrIpCapped")
	}
	if ice.ActiveUsersForIP != 5 || ice.Limit != 4 {
		t.Errorf("IpCappedError = %+v, want ActiveUsersForIP 5 limit 4", ice)
	}
	if ice.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %s, want 30s (bounded to retryAfterMs only)", ice.RetryAfter)
	}
}

// TestSnapshotActiveUsersForIP verifies the admission response's
// activeUsersForIp is cached and exposed through SessionSnapshot for the
// pool snapshot (issue #81 "if cheap").
func TestSnapshotActiveUsersForIP(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mgr := newTestManager(t, mock)

	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-abc-123","activeUsersForIp":3}`)
	}

	if _, err := mgr.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := mgr.Snapshot()
	if snap.ActiveUsersForIP != 3 {
		t.Errorf("Snapshot.ActiveUsersForIP = %d, want 3", snap.ActiveUsersForIP)
	}
	if snap.Status != "active" {
		t.Errorf("Status = %q, want active", snap.Status)
	}
}
