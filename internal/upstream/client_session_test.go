package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/testutil"
)

func TestSessionControlCalls(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	client, _ := New("tok", testConfig(mock.URL(), nil))

	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "active" || st.InstanceID != "inst-abc-123" {
		t.Fatalf("create state = %+v", st)
	}
	if st.ExpiresAt.IsZero() {
		t.Error("expiresAt not parsed")
	}

	// poll requires instance header
	polled, err := client.GetSession(context.Background(), "inst-abc-123")
	if err != nil {
		t.Fatal(err)
	}
	if polled.Status != "active" {
		t.Errorf("poll status = %q", polled.Status)
	}

	// end + tolerated 404
	if err := client.EndSession(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestProbeAccount verifies the zero-cost token probe: a GET
// /api/v1/freebuff/session with NO instance header that claims no session
// slot, returns the live per-model quota, and classifies
// auth/ban/region/transport failures through the standard matrix.
func TestProbeAccount(t *testing.T) {
	t.Run("200 with quota", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()

		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		st, err := client.ProbeAccount(context.Background())
		if err != nil {
			t.Fatalf("ProbeAccount: %v", err)
		}
		if st.Status != "active" || st.InstanceID != "inst-abc-123" {
			t.Fatalf("probe state = %+v", st)
		}
		q, ok := st.RateLimitsByModel["deepseek/deepseek-v4-flash"]
		if !ok {
			t.Fatalf("RateLimitsByModel missing flash quota: %+v", st.RateLimitsByModel)
		}
		if q.Limit != 6 || q.RecentCount != 2 {
			t.Errorf("quota limit/recentCount = %v/%v, want 6/2", q.Limit, q.RecentCount)
		}
		if q.Period != "pacific_day" {
			t.Errorf("period = %q, want pacific_day", q.Period)
		}
		if q.ResetAt.IsZero() {
			t.Error("resetAt not parsed")
		}
		// A probe must not claim a session slot (no POST).
		if got := mock.SessionCreatesSnapshot(); got != 0 {
			t.Errorf("session creates = %d, want 0 (probe is zero-cost)", got)
		}
		if got := mock.SessionProbesSnapshot(); got != 1 {
			t.Errorf("session probes = %d, want 1", got)
		}
	})

	t.Run("404 maps to ErrNoActiveSession", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"error":"session not found"}`)
		}

		client, _ := New("tok", testConfig(mock.URL(), nil))
		_, err := client.ProbeAccount(context.Background())
		if !errors.Is(err, ErrNoActiveSession) {
			t.Fatalf("err = %v, want ErrNoActiveSession", err)
		}
	})

	t.Run("200 ended maps to ErrNoActiveSession", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"status":"ended"}`)
		}

		client, _ := New("tok", testConfig(mock.URL(), nil))
		_, err := client.ProbeAccount(context.Background())
		if !errors.Is(err, ErrNoActiveSession) {
			t.Fatalf("err = %v, want ErrNoActiveSession", err)
		}
	})

	t.Run("401 auth rejected", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.AuthReject = true

		client, _ := New("tok", testConfig(mock.URL(), nil))
		_, err := client.ProbeAccount(context.Background())
		if !errors.Is(err, ErrAuthRejected) {
			t.Fatalf("err = %v, want ErrAuthRejected", err)
		}
	})

	t.Run("403 banned", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.Ban = true

		client, _ := New("tok", testConfig(mock.URL(), nil))
		_, err := client.ProbeAccount(context.Background())
		if !errors.Is(err, ErrBanned) {
			t.Fatalf("err = %v, want ErrBanned", err)
		}
	})

	t.Run("403 country blocked", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `{"status":"country_blocked","countryCode":"CN","countryBlockReason":"region_restricted","ipPrivacySignals":["vpn"]}`)
		}

		client, _ := New("tok", testConfig(mock.URL(), nil))
		_, err := client.ProbeAccount(context.Background())
		if !errors.Is(err, ErrCountryBlocked) {
			t.Fatalf("err = %v, want ErrCountryBlocked", err)
		}
		var cbe *CountryBlockedError
		if !errors.As(err, &cbe) {
			t.Fatalf("err = %T, want *CountryBlockedError", err)
		}
		if cbe.CountryCode != "CN" {
			t.Errorf("countryCode = %q, want CN", cbe.CountryCode)
		}
	})

	t.Run("transport error", func(t *testing.T) {
		mock := testutil.NewMock()
		url := mock.URL()
		mock.Close()

		client, _ := New("tok", testConfig(url, nil))
		_, err := client.ProbeAccount(context.Background())
		if err == nil {
			t.Fatal("ProbeAccount returned nil error for closed server")
		}
	})
}

// TestSessionCallParsesRateLimitsByModel verifies the live per-model quota
// map from an admission response is parsed into SessionState, including the
// nested entitlement breakdown and flex-time resetAt.
func TestSessionCallParsesRateLimitsByModel(t *testing.T) {
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

	client, _ := New("tok", testConfig(mock.URL(), nil))
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	q, ok := st.RateLimitsByModel["z-ai/glm-5.2"]
	if !ok {
		t.Fatalf("RateLimitsByModel missing model z-ai/glm-5.2: %+v", st.RateLimitsByModel)
	}
	if q.Limit != 5 || q.RecentCount != 4 {
		t.Errorf("quota limit/recentCount = %v/%v, want 5/4", q.Limit, q.RecentCount)
	}
	if q.Period != "pacific_day" {
		t.Errorf("period = %q, want pacific_day", q.Period)
	}
	if q.ResetAt.IsZero() {
		t.Error("resetAt not parsed")
	} else if want := "2026-08-16T07:00:00Z"; q.ResetAt.UTC().Format(time.RFC3339) != want {
		t.Errorf("resetAt = %s, want %s", q.ResetAt.UTC().Format(time.RFC3339), want)
	}
	if q.Entitlement["base"] != 1 || q.Entitlement["referral"] != 1 || q.Entitlement["streak"] != 3 {
		t.Errorf("entitlement = %+v, want base=1 referral=1 streak=3", q.Entitlement)
	}
	if q.Model != "z-ai/glm-5.2" {
		t.Errorf("quota model = %q", q.Model)
	}
}

func TestSession404Mapping(t *testing.T) {
	// A create 404 means no session slot exists upstream → disabled.
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "404"

	client, _ := New("tok", testConfig(mock.URL(), nil))
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "disabled" {
		t.Errorf("create 404 status = %q, want disabled", st.Status)
	}

	// A poll 404 means the session vanished upstream (expired/evicted) →
	// ended (recreate path), NOT a permanent disabled (which the session
	// manager would cache with no expiry, disabling the token forever).
	polled, err := client.GetSession(context.Background(), "inst-gone")
	if err != nil {
		t.Fatal(err)
	}
	if polled.Status != "ended" {
		t.Errorf("poll 404 status = %q, want ended", polled.Status)
	}
}

func TestQueuedSessionParsing(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "queued"
	mock.QueuePosition = 4
	mock.QueueDepth = 9
	mock.EstimatedWaitMs = 0

	client, _ := New("tok", testConfig(mock.URL(), nil))
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "queued" || st.Position != 4 || st.QueueDepth != 9 {
		t.Fatalf("queued state = %+v", st)
	}
	if st.PollAt.IsZero() {
		t.Error("pollAt not parsed")
	}
}

func TestStartAndFinishRun(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	client, _ := New("tok", testConfig(mock.URL(), nil))

	runID, err := client.StartRun(context.Background(), "base2-free-deepseek-flash")
	if err != nil {
		t.Fatal(err)
	}
	if runID != "run-0001" {
		t.Errorf("runID = %q", runID)
	}
	if len(mock.StartedRuns) != 1 || mock.StartedRuns[0] != "base2-free-deepseek-flash" {
		t.Errorf("START not recorded: %v", mock.StartedRuns)
	}

	msg1 := "msg-1"
	steps := []RunStep{
		{ID: "step-1", StepNumber: 1, MessageID: &msg1, Status: "completed", StartTime: "2026-08-18T00:00:00.000Z"},
		{ID: "step-2", StepNumber: 2, Status: "completed", StartTime: "2026-08-18T00:00:01.000Z"},
	}
	if err := client.FinishRun(context.Background(), runID, "completed", len(steps), steps, ""); err != nil {
		t.Fatal(err)
	}
	if len(mock.FinishedRuns) != 1 {
		t.Fatalf("FINISH not recorded: %v", mock.FinishedRuns)
	}
	f := mock.FinishedRuns[0]
	if f.RunID != runID || f.Status != "completed" || f.TotalSteps != 2 {
		t.Errorf("FINISH payload = %+v", f)
	}
	// Issue #114: steps ride IN the FINISH payload (the CLI has no /steps
	// endpoint) with the CLI step shape: id, stepNumber, messageId
	// (null-able), status, startTime.
	if len(f.Steps) != 2 || f.Steps[0].StepNumber != 1 || f.Steps[0].MessageID == nil || *f.Steps[0].MessageID != "msg-1" ||
		f.Steps[1].StepNumber != 2 || f.Steps[1].MessageID != nil || f.Steps[1].StartTime == "" {
		t.Errorf("FINISH steps = %+v, want 2 CLI-shaped steps", f.Steps)
	}
}

func TestFinishRunErrorTruncation(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	client, _ := New("tok", testConfig(mock.URL(), nil))

	// errorMessage must be truncated to 5000 runes (CLI parity:
	// truncateString(errorMessage, 5000) in database.ts) — a full Go stack
	// trace must not blow the cap.
	long := strings.Repeat("エ", 6000)
	if err := client.FinishRun(context.Background(), "run-0001", "failed", 0, nil, long); err != nil {
		t.Fatal(err)
	}
	finished := mock.FinishedRunsSnapshot()
	if len(finished) != 1 || finished[0].RunID != "run-0001" || finished[0].Status != "failed" {
		t.Fatalf("finished runs = %+v, want run-0001 failed", finished)
	}
	if got := len([]rune(finished[0].ErrorMessage)); got != 5000 {
		t.Errorf("errorMessage runes = %d, want 5000 (truncated)", got)
	}
}

func TestControlCallTimeout(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Hang the session create; the 50ms control timeout must win even when
	// the caller passes a much longer deadline (the control timeout is the
	// tighter bound and must never be defeated by the caller's context).
	mock.SessionCreateDelay = 10 * time.Second

	client, _ := New("tok", testConfig(mock.URL(), func(c *config.Config) { c.SessionCallTimeout = 50 * time.Millisecond }))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := client.CreateSession(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
}

func TestCreateSessionForModelHeaders(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			model := r.Header.Get("x-freebuff-model")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-1","model":"`+model+`","expiresAt":"2030-01-01T00:00:00Z"}`)
			return
		}
		http.NotFound(w, r)
	}

	client, err := New("tok-a", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}

	st, err := client.CreateSessionForModel(context.Background(), "thudm/glm-5.2")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "active" || st.Model != "thudm/glm-5.2" || st.InstanceID != "inst-1" {
		t.Errorf("got %+v, want active with model thudm/glm-5.2", st)
	}
}

func TestGetSessionWithOptsHeaders(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var gotCompact, gotHeartbeat, gotInstance string
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		gotCompact = r.Header.Get("x-freebuff-compact-session")
		gotHeartbeat = r.Header.Get("x-freebuff-heartbeat")
		gotInstance = r.Header.Get("x-freebuff-instance-id")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-1","expiresAt":"2030-01-01T00:00:00Z"}`)
	}

	client, err := New("tok-a", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}

	st, err := client.GetSessionWithOpts(context.Background(), "inst-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "active" {
		t.Errorf("status = %q, want active", st.Status)
	}
	if gotCompact != "1" || gotInstance != "inst-1" {
		t.Errorf("headers: compact=%q, instance=%q (want 1 / inst-1)", gotCompact, gotInstance)
	}
	// Gap #2: the CLI never beats — x-freebuff-heartbeat is Desktop-only
	// (reference/freebuff freebuff-models.ts:1212-1215), so a compact poll
	// must NOT carry it.
	if gotHeartbeat != "" {
		t.Errorf("x-freebuff-heartbeat = %q, want absent on compact polls", gotHeartbeat)
	}
}

func TestSessionCallStructured4xx(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		body       string
		wantStatus string
	}{
		{
			name:       "model_locked 409",
			statusCode: http.StatusConflict,
			body:       `{"status":"model_locked","currentModel":"deepseek/deepseek-v4-flash","requestedModel":"thudm/glm-5.2"}`,
			wantStatus: "model_locked",
		},
		{
			name:       "model_unavailable 409",
			statusCode: http.StatusConflict,
			body:       `{"status":"model_unavailable","requestedModel":"thudm/glm-5.2","availableHours":"08:00-20:00"}`,
			wantStatus: "model_unavailable",
		},
		{
			name:       "ip_capped 429",
			statusCode: http.StatusTooManyRequests,
			body:       `{"status":"ip_capped","activeUsersForIp":5,"limit":4,"retryAfterMs":30000}`,
			wantStatus: "ip_capped",
		},
		{
			name:       "spend_limited 429",
			statusCode: http.StatusTooManyRequests,
			body:       `{"status":"spend_limited","message":"Daily budget reached","retryAfterMs":60000}`,
			wantStatus: "spend_limited",
		},
		{
			name:       "country_blocked 403",
			statusCode: http.StatusForbidden,
			body:       `{"status":"country_blocked","countryCode":"CN","countryBlockReason":"country_not_allowed"}`,
			wantStatus: "country_blocked",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.statusCode)
				_, _ = io.WriteString(w, tc.body)
			}

			client, err := New("tok-a", testConfig(mock.URL(), nil))
			if err != nil {
				t.Fatal(err)
			}

			st, err := client.CreateSession(context.Background())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if st.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", st.Status, tc.wantStatus)
			}
		})
	}
}

// TestSessionCallUnknownStatus5xx pins current sessionCall behavior (G10):
// any status code with a parseable body carrying a non-empty status field
// yields a SessionState, not an error — even a 5xx with an unknown status.
func TestSessionCallUnknownStatus5xx(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"status":"weird","message":"unknown status"}`)
	}
	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.CreateSession(context.Background())
	if err != nil {
		t.Fatalf("unexpected error for a parseable 5xx body: %v", err)
	}
	if st.Status != "weird" {
		t.Errorf("status = %q, want weird", st.Status)
	}
}

// TestEndSession404Tolerated guards the EndSession 404 contract (E2E flow
// 10): a 404 DELETE is "nothing to end", not an error, while a 5xx is.
func TestEndSession404Tolerated(t *testing.T) {
	t.Run("404 tolerated", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"session not found"}`)
		}
		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		if err := client.EndSession(context.Background()); err != nil {
			t.Errorf("EndSession 404 = %v, want nil", err)
		}
	})

	t.Run("5xx surfaces error", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"boom"}`)
		}
		client, err := New("tok", testConfig(mock.URL(), nil))
		if err != nil {
			t.Fatal(err)
		}
		if err := client.EndSession(context.Background()); err == nil {
			t.Error("EndSession 500 succeeded, want error")
		}
	})
}

// TestCompactPollAbsentTolerant is E2E flow 8: a compact poll without quota/
// offer fields parses cleanly with nil maps, and carries no heartbeat header.
func TestCompactPollAbsentTolerant(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	gotCompact := make(chan string, 1)
	gotHeartbeat := make(chan string, 1)
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		gotCompact <- r.Header.Get("x-freebuff-compact-session")
		gotHeartbeat <- r.Header.Get("x-freebuff-heartbeat")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-1","expiresAt":"2026-08-17T10:00:00.000Z"}`)
	}

	client, err := New("tok", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.GetSessionWithOpts(context.Background(), "inst-1", true)
	if err != nil {
		t.Fatalf("compact poll: %v", err)
	}
	if st.Status != "active" || st.InstanceID != "inst-1" {
		t.Errorf("state = %+v, want active inst-1", st)
	}
	if st.RateLimitsByModel != nil {
		t.Errorf("RateLimitsByModel = %v, want nil on a compact poll without quotas", st.RateLimitsByModel)
	}
	if got := <-gotCompact; got != "1" {
		t.Errorf("compact header = %q, want 1", got)
	}
	if got := <-gotHeartbeat; got != "" {
		t.Errorf("heartbeat header = %q, want absent (CLI never beats)", got)
	}
}

// TestClassifyCapacityDeferred verifies #75: a free_mode_capacity_deferred
// response classifies as the distinct CapacityDeferredError (retryable
// same-session condition), never a token cooldown or session invalidation.
func TestClassifyCapacityDeferred(t *testing.T) {
	err := classifyError(http.StatusTooManyRequests, `{"error":{"code":"free_mode_capacity_deferred","message":"Free mode is at capacity; your request will be retried automatically"}}`, http.Header{})
	var cde *CapacityDeferredError
	if !errors.As(err, &cde) {
		t.Fatalf("err = %v, want *CapacityDeferredError", err)
	}
	if !errors.Is(err, ErrCapacityDeferred) {
		t.Errorf("err = %v, want ErrCapacityDeferred", err)
	}
	// Unwraps to a Retryable UpstreamError (errors.As finds it), but
	// writeError surfaces 429 free_mode_capacity_deferred + Retry-After
	// via its dedicated CapacityDeferredError branch (#105).
	var ue *UpstreamError
	if !errors.As(err, &ue) || !ue.Retryable {
		t.Errorf("err = %v, want unwrap to Retryable UpstreamError", err)
	}
	if cde.Status != http.StatusTooManyRequests {
		t.Errorf("Status = %d, want 429", cde.Status)
	}
}

// TestClassifyWaitingRoomQueued verifies #81: a 429 waiting_room_queued body
// is a transient admission race (endsTheSession:false) — surfaced as a
// WaitingRoomError, never session-invalid (no session refresh/recreate).
func TestClassifyWaitingRoomQueued(t *testing.T) {
	err := classifyError(http.StatusTooManyRequests, `{"error":{"code":"waiting_room_queued","message":"row caught mid-admit"}}`, http.Header{})
	if errors.Is(err, ErrSessionInvalid) {
		t.Fatal("waiting_room_queued classified as session-invalid, want transient WaitingRoomError")
	}
	var wr *WaitingRoomError
	if !errors.As(err, &wr) {
		t.Fatalf("err = %v, want *WaitingRoomError", err)
	}
}

// TestClassifySessionLimitReached verifies #82: a 409 session_limit_reached
// response is a distinct non-invalid error carrying the code — the ACCOUNT
// is over its concurrent-tab budget but the session row is fine
// (endsTheSession:false), so no session refresh/recreate may trigger.
func TestClassifySessionLimitReached(t *testing.T) {
	err := classifyError(http.StatusConflict, `{"error":{"code":"session_limit_reached","message":"Concurrent tab limit reached"}}`, http.Header{})
	if errors.Is(err, ErrSessionInvalid) {
		t.Fatal("session_limit_reached classified as session-invalid; the row is fine")
	}
	var sle *SessionLimitError
	if !errors.As(err, &sle) {
		t.Fatalf("err = %v, want *SessionLimitError", err)
	}
	if !errors.Is(err, ErrSessionLimitReached) {
		t.Errorf("err = %v, want ErrSessionLimitReached", err)
	}
	if sle.Status != http.StatusConflict {
		t.Errorf("Status = %d, want 409", sle.Status)
	}
}

// TestProbeAccountDoesNotSendIncludeUnusedRateLimits verifies #140: the
// zero-cost GET probe sends NO x-freebuff-include-unused-rate-limits header
// (a third-party-proxy fingerprint the vendored CLI never sends; its session
// GET returns the same response shape without it). The probe carries only
// the standard Authorization + the plain Bun fetch UA (audit G5: session
// paths are bare Bun traffic in the real CLI), and sessionCall still parses
// glmPromo/rateLimitsByModel when the response includes them.
func TestProbeAccountDoesNotSendIncludeUnusedRateLimits(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var gotHeader, gotAuth, gotUA string
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("x-freebuff-include-unused-rate-limits")
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-1","glmPromo":{"dailySessions":2,"endsAt":"2026-08-20T07:00:00.000Z"},"rateLimitsByModel":{"deepseek/deepseek-v4-flash":{"model":"deepseek/deepseek-v4-flash","limit":6,"recentCount":2,"period":"pacific_day","resetAt":"2026-08-18T07:00:00.000Z"}}}`)
	}
	client, err := New("tok-a", testConfig(mock.URL(), nil))
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.ProbeAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotHeader != "" {
		t.Errorf("x-freebuff-include-unused-rate-limits = %q, want absent", gotHeader)
	}
	if gotAuth != "Bearer tok-a" {
		t.Errorf("Authorization = %q, want Bearer tok-a", gotAuth)
	}
	if gotUA != bunUserAgent {
		t.Errorf("User-Agent = %q, want %q (Bun fetch default on session paths, no browser persona)", gotUA, bunUserAgent)
	}
	if st.GlmPromo == "" || !strings.Contains(st.GlmPromo, "dailySessions") {
		t.Errorf("GlmPromo = %q, want raw glmPromo JSON", st.GlmPromo)
	}
	if st.RateLimitsByModel == nil || st.RateLimitsByModel["deepseek/deepseek-v4-flash"].Limit != 6 {
		t.Errorf("RateLimitsByModel = %+v, want parsed per-model quota", st.RateLimitsByModel)
	}
}
