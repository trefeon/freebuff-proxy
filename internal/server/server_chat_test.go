package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/server"
	"freebuff-proxy/internal/testutil"
)

func TestWaitingRoom503ThenRetry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionSequence = []string{"queued", "active"}
	mock.QueuePosition = 3
	mock.QueueDepth = 7
	mock.EstimatedWaitMs = 50
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("first request status = %d, want 503: %s", resp.StatusCode, data)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "1" {
		t.Errorf("Retry-After = %q, want 1 (ceil of ~50ms)", ra)
	}
	if !strings.Contains(string(data), "waiting_room_queued") {
		t.Errorf("body missing waiting_room_queued: %s", data)
	}

	// Wait out the queue window, then the session must advance to active.
	// Poll the retry instead of sleeping: the queued session only advances
	// after its pollAt window, so keep retrying until the queue clears.
	var data2 []byte
	eventually(t, "waiting room to clear", func() bool {
		resp2, d2 := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
		data2 = d2
		return resp2.StatusCode == http.StatusOK
	})
	if !strings.HasSuffix(string(data2), "data: [DONE]\n\n") {
		t.Errorf("retry stream must end with [DONE]: %q", data2)
	}
}

func TestChat401Cooldown(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = 401
	mock.ChatErrorBody = `{"error":{"message":"unauthorized","type":"authentication_error"}}`
	ts, p := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "upstream_auth_rejected") {
		t.Errorf("body missing upstream_auth_rejected: %s", data)
	}

	snap := p.Snapshot()[0]
	if snap.CooldownUntil.Before(time.Now().Add(29 * time.Minute)) {
		t.Errorf("cooldown until = %v, want ~now+30m", snap.CooldownUntil)
	}
}

func TestChatRateLimitSurfaced(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimit = true
	ts, p := newTestServer(t, nil, mock)

	// First request: upstream 429 rate_limited → 429 + Retry-After (the
	// gateway must back off for the exact window, not hammer a 502).
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", resp.StatusCode, data)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "48550" {
		t.Errorf("Retry-After = %q, want 48550 (ceil of 48549499ms)", ra)
	}
	if !strings.Contains(string(data), `"code":"rate_limited"`) {
		t.Errorf("body missing rate_limited code: %s", data)
	}
	if !strings.Contains(string(data), "reset at 2026-08-12T07:00:00") {
		t.Errorf("body missing resetAt: %s", data)
	}

	// The token cooled down for the window; a second request surfaces the
	// remembered 429 + Retry-After, never a 502.
	resp2, data2 := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429: %s", resp2.StatusCode, data2)
	}
	if ra := resp2.Header.Get("Retry-After"); ra != "48550" {
		t.Errorf("second Retry-After = %q, want 48550", ra)
	}

	snap := p.Snapshot()[0]
	if snap.CooldownUntil.Before(time.Now().Add(13 * time.Hour)) {
		t.Errorf("cooldown until = %v, want ~now+13.5h", snap.CooldownUntil)
	}
}

func TestRunInvalidRecovers(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RunIDs = []string{"run-0001", "run-0002"}
	var calls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"runId not found"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-r1", 1, `"choices":[{"index":0,"delta":{"content":"recovered"},"finish_reason":null}]`)))
	}
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after run-invalid retry: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "recovered") {
		t.Errorf("retry stream missing content: %s", data)
	}
	if got := len(mock.StartedRunsSnapshot()); got != 2 {
		t.Errorf("started runs = %d, want 2 (re-START after run-invalid)", got)
	}
	// With the #91 context-pruner child traffic gone there is NO
	// best-effort side-effect traffic at all: the invalidated run-0001 is
	// dropped WITHOUT an upstream FINISH, and run-0002 stays active (lease
	// released) until rotation/shutdown — so zero agent-runs FINISH calls
	// may have been issued.
	if got := mock.FinishesStartedSnapshot(); got != 0 {
		t.Errorf("FINISH attempts = %d, want 0 (invalidated run is not FINISHed; no pruner children exist)", got)
	}
}

func TestChatSessionInvalidBoundedRetry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Every chat returns a session-invalid error. Without a retry budget the
	// recovery loop re-creates the session and re-chats forever, hanging the
	// client; the budget must cap it at one retry (2 chat attempts total).
	// session_superseded is its OWN terminal sentinel (see
	// TestChatSessionSupersededTerminal) — this test uses session_expired to
	// pin the invalidate+reacquire-once budget for ErrSessionInvalid.
	mock.ChatStatus = http.StatusBadRequest
	mock.ChatErrorBody = `{"error":{"message":"session_expired"}}`
	ts, _ := newTestServer(t, nil, mock)

	// A client timeout makes a regression (unbounded loop) fail fast instead
	// of hanging the whole suite.
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(chatBody(modelA)))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "session_invalid") {
		t.Errorf("body missing session_invalid: %s", data)
	}
	if got := len(mock.RecordedChatHeaders); got != 2 {
		t.Errorf("upstream chat attempts = %d, want exactly 2 (bounded retry)", got)
	}
	if got := mock.SessionCreates; got != 2 {
		t.Errorf("upstream session creates = %d, want exactly 2 (bounded retry)", got)
	}
}

// TestChatSessionSupersededTerminal pins #159: 409 session_superseded
// (another instance took over the account, endsTheSession:true) is TERMINAL
// for the current request — the cached session is dropped immediately and
// the error surfaces with NO in-request retry. The success canary proves the
// retry never fires: one chat attempt, one session create, 503
// session_superseded (the #119 re-admit-once behavior wasted a fresh daily
// session slot against the superseding instance and still failed).
func TestChatSessionSupersededTerminal(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// First chat attempt returns session_superseded; a SECOND attempt would
	// succeed (canary) — the response must still be 503 and the canary must
	// never fire, proving the dead instance is not re-attempted.
	callCount := 0
	originalHandler := mock.ChatHandler
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"session_superseded"}}`))
			return
		}
		if originalHandler != nil {
			originalHandler(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + chunk("cmpl-test", 1234567890, `"choices":[{"delta":{"content":"ok"},"index":0}]`) + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "session_superseded") {
		t.Errorf("body missing session_superseded: %s", data)
	}
	if got := callCount; got != 1 {
		t.Errorf("upstream chat attempts = %d, want exactly 1 (no retry on the dead instance)", got)
	}
	if got := len(mock.RecordedChatHeaders); got != 1 {
		t.Errorf("upstream chat attempts (recorded) = %d, want exactly 1", got)
	}
	if got := mock.SessionCreates; got != 1 {
		t.Errorf("session creates = %d, want exactly 1 (no re-admit against the superseding instance)", got)
	}
}

// TestChatSessionSupersededNextRequestReadmits pins #159: a superseded chat
// invalidates the cached session immediately, so the NEXT request re-admits
// fresh instead of reusing the dead row. Two requests: the first surfaces
// 503 session_superseded (one create), the second succeeds on a NEW session
// (second create — proves the cache was dropped, not reused).
func TestChatSessionSupersededNextRequestReadmits(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = http.StatusBadRequest
	mock.ChatErrorBody = `{"error":{"message":"session_superseded"}}`
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "session_superseded") {
		t.Errorf("body missing session_superseded: %s", data)
	}
	if got := mock.SessionCreates; got != 1 {
		t.Fatalf("session creates after superseded request = %d, want 1", got)
	}

	// Upstream heals; the next request must create a FRESH session (the
	// superseded row was invalidated, so no cached instance is reused).
	mock.ChatStatus = http.StatusOK
	mock.ChatErrorBody = ""
	resp2, data2 := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second request status = %d, want 200: %s", resp2.StatusCode, data2)
	}
	if got := mock.SessionCreates; got != 2 {
		t.Errorf("session creates after re-admit request = %d, want 2 (fresh session, not cache reuse)", got)
	}
	if got := len(mock.RecordedChatHeaders); got != 2 {
		t.Errorf("upstream chat attempts = %d, want 2 (1 per request — no in-request retry)", got)
	}
}

func TestChatBanSurfaced(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.Ban = true
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), `"code":"account_banned"`) {
		t.Errorf("body missing account_banned code: %s", data)
	}
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		t.Error("missing Retry-After header")
	}
}

// TestUpstreamRetryableMapsTo503 verifies a Retryable UpstreamError
// (deployment_outside_hours) surfaces as 503 upstream_retryable so clients
// back off and retry later, not a hard 502.
func TestUpstreamRetryableMapsTo503(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = http.StatusServiceUnavailable
	mock.ChatErrorBody = `{"error":"deployment_outside_hours"}`
	ts, _ := newTestServer(t, nil, mock)
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "upstream_retryable") {
		t.Errorf("body = %s, want upstream_retryable code", data)
	}
}

// TestUpstreamRetryableNotBlindRetried verifies chatAttempt does NOT retry a
// Retryable UpstreamError (deployment_outside_hours): the flag means "worth
// retrying later", not "transient", so a blind retry must not burn a second
// lease against the same wall. The mock must see exactly one chat call.
func TestUpstreamRetryableNotBlindRetried(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var chatCalls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"deployment_outside_hours","type":"upstream_error","code":"deployment_outside_hours"}}`)
	}
	ts, _ := newTestServer(t, nil, mock)
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "upstream_retryable") {
		t.Errorf("body = %s, want upstream_retryable code", data)
	}
	if got := chatCalls.Load(); got != 1 {
		t.Errorf("upstream chat calls = %d, want 1 (Retryable errors must not be blind-retried)", got)
	}
}

// TestChatCapacityDeferredSurfaced429 verifies #105 (server half): once the
// client-side capacity-deferred budget is exhausted, the gateway surfaces the
// free tier's transient capacity queue as 429 free_mode_capacity_deferred +
// Retry-After (the upstream window) — never the old bare 502 upstream_
// unavailable or a generic 503 upstream_retryable — so downstream clients
// honor the window instead of re-POSTing immediately. The mock sees exactly
// one chat call: the typed error unwraps to a Retryable UpstreamError, so
// chatAttempt must not blind-retry it a second time.
func TestChatCapacityDeferredSurfaced429(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var chatCalls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"free_mode_capacity_deferred","message":"Free mode is at capacity; your request will be retried automatically","retryAfterMs":7000}}`)
	}
	// TRANSIENT_RETRIES=0 = exhausted budget: the client surfaces the typed
	// CapacityDeferredError immediately (no in-place retry, no retry-after
	// sleep), so the server mapping is exercised on the first call.
	ts, _ := newTestServerCfg(t, nil, func(cfg *config.Config) { cfg.TransientRetries = 0 }, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", resp.StatusCode, data)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "7" {
		t.Errorf("Retry-After = %q, want 7 (the upstream window, ceil seconds)", ra)
	}
	if !strings.Contains(string(data), `"code":"free_mode_capacity_deferred"`) {
		t.Errorf("body missing free_mode_capacity_deferred code: %s", data)
	}
	if got := chatCalls.Load(); got != 1 {
		t.Errorf("upstream chat calls = %d, want 1 (no blind retry after budget exhaustion)", got)
	}
}

// TestChatCapacityDeferredDefaultRetryAfter verifies the 10s Retry-After
// fallback when the upstream free_mode_capacity_deferred response carries no
// retry-after window (the AI SDK's default honor window).
func TestChatCapacityDeferredDefaultRetryAfter(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"free_mode_capacity_deferred","message":"Free mode is at capacity; your request will be retried automatically"}}`)
	}
	ts, _ := newTestServerCfg(t, nil, func(cfg *config.Config) { cfg.TransientRetries = 0 }, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", resp.StatusCode, data)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "10" {
		t.Errorf("Retry-After = %q, want 10 (default window)", ra)
	}
	if !strings.Contains(string(data), `"code":"free_mode_capacity_deferred"`) {
		t.Errorf("body missing free_mode_capacity_deferred code: %s", data)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	resp, _ := doJSON(t, http.MethodGet, ts.URL+"/v1/chat/completions", nil, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET chat status = %d, want 405", resp.StatusCode)
	}

	resp, _ = doJSON(t, http.MethodPost, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST models status = %d, want 405", resp.StatusCode)
	}

	resp, _ = doJSON(t, http.MethodGet, ts.URL+"/v1/nope", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path status = %d, want 404", resp.StatusCode)
	}
}

func TestChatModelAliasesAndReasoningEffort(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	ts, p := newTestServer(t, nil, mock)
	_ = ts

	reg := registry.New(&config.Config{
		ModelAliases: map[string]string{
			"gpt-4o": modelA,
		},
	}, nil)
	reg.LoadFallback()

	srv := server.New(&config.Config{
		AuthTokens: []string{"tok-0"},
		ModelAliases: map[string]string{
			"gpt-4o": modelA,
		},
		DashboardEnabled: true,
	}, p, reg, nil, nil, "")
	tsAlias := httptest.NewServer(srv.Handler())
	t.Cleanup(tsAlias.Close)

	bodyBytes, _ := json.Marshal(map[string]any{
		"model":     "gpt-4o",
		"messages":  []any{map[string]any{"role": "user", "content": "hi"}},
		"reasoning": map[string]any{"effort": "max"},
		"stream":    true,
	})

	resp, data := doJSON(t, http.MethodPost, tsAlias.URL+"/v1/chat/completions", bodyBytes, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}

	if len(mock.RecordedChatBodies) == 0 {
		t.Fatal("no chat requests recorded upstream")
	}
	var upstreamPayload map[string]any
	if err := json.Unmarshal([]byte(mock.RecordedChatBodies[len(mock.RecordedChatBodies)-1]), &upstreamPayload); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	if gotModel := upstreamPayload["model"]; gotModel != modelA {
		t.Errorf("upstream model = %v, want %v (resolved alias)", gotModel, modelA)
	}
	if gotEffort := upstreamPayload["reasoning_effort"]; gotEffort != "max" {
		t.Errorf("upstream reasoning_effort = %v, want \"max\"", gotEffort)
	}
}

// TestChatModelIPLimitedMarked pins the chat-level limited_ip flow: a 409
// session_model_mismatch+limited chat error surfaces as 409 model_ip_limited
// (never session-invalid, never a session invalidation) and marks the
// (egress, model) pairing unfit.
func TestChatModelIPLimitedMarked(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = http.StatusConflict
	mock.ChatErrorBody = limitedChatBody()
	ts, p := newTestServer(t, nil, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, data)
	}
	if got := errorCode(t, data); got != "model_ip_limited" {
		t.Errorf("code = %q, want model_ip_limited", got)
	}
	until, _ := p.ModelUnfit(modelA)
	if until.IsZero() {
		t.Error("pool unfit not set after limited chat")
	}
}

// TestChatModelIPLimitedFastRefusal pins the fast-refusal guard: while
// (egress, model) is marked unfit, a new request is refused at the entry
// guard with 409 model_ip_limited and NO new upstream chat call. The first
// (marking) request retries once inside chatAttempt, so it hits upstream
// twice.
func TestChatModelIPLimitedFastRefusal(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var chatCalls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		writeRawJSON(w, http.StatusConflict, limitedChatBody())
	}
	ts, p := newTestServer(t, nil, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	// First request: the limited error marks unfit and chatAttempt retries
	// once through a fresh acquire (a different token may still serve the
	// model) before surfacing the 409 — exactly two upstream chat calls.
	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("first request status = %d, want 409: %s", resp.StatusCode, data)
	}
	if got := chatCalls.Load(); got != 2 {
		t.Errorf("first request upstream chat calls = %d, want 2 (retry-once)", got)
	}
	if until, _ := p.ModelUnfit(modelA); until.IsZero() {
		t.Fatal("unfit not marked after first request")
	}

	// Second request: refused at the entry guard — no upstream chat hit.
	resp2, data2 := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), nil)
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("second request status = %d, want 409: %s", resp2.StatusCode, data2)
	}
	if got := errorCode(t, data2); got != "model_ip_limited" {
		t.Errorf("second request code = %q, want model_ip_limited", got)
	}
	if got := chatCalls.Load(); got != 2 {
		t.Errorf("second request upstream chat calls = %d, want 2 (fast-refused, no new chat)", got)
	}
}

// TestChatModelIPLimitedSuccessClears pins the success-side clear: a
// successful chat is egress-level proof the model is servable again, so the
// unfit mark is dropped. The mark is cleared between requests (simulating
// the window lapsing) so the second request passes the entry guard and
// reaches chatAttempt, where its retry lands on the 200.
func TestChatModelIPLimitedSuccessClears(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var chatCalls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		// Calls 1-2: request 1's two chatAttempt attempts both see the
		// limited 409 (marking the pair unfit). Call 3: request 2's first
		// attempt (re-marks). Call 4+: the upstream serves the model again,
		// so request 2's retry lands on the 200 and clears the mark.
		if chatCalls.Add(1) <= 3 {
			writeRawJSON(w, http.StatusConflict, limitedChatBody())
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-u1", 1, `"choices":[{"index":0,"delta":{"content":"recovered"},"finish_reason":null}]`)))
	}
	ts, p := newTestServer(t, nil, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	// First request: limited 409 (both retry attempts limited).
	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("first request status = %d, want 409: %s", resp.StatusCode, data)
	}
	if until, _ := p.ModelUnfit(modelA); until.IsZero() {
		t.Fatal("unfit not marked after limited response")
	}

	// Simulate the unfit window lapsing so the second request is not
	// fast-refused at the entry guard — it must reach chatAttempt, where
	// the retry lands on the 200 and the success path clears the mark.
	p.ClearModelUnfit(modelA)

	resp2, data2 := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second request status = %d, want 200: %s", resp2.StatusCode, data2)
	}
	if !strings.Contains(string(data2), "recovered") {
		t.Errorf("stream missing recovered content: %s", data2)
	}
	if until, _ := p.ModelUnfit(modelA); !until.IsZero() {
		t.Errorf("unfit not cleared after successful chat (until = %v)", until)
	}
}

// TestChatModelIPLimitedAdmissionPath covers the admission-path end-to-end:
// the session create itself returns 409 limited, the pool marks (egress,
// model) unfit and surfaces the LimitedIpError, and the chat surfaces 409
// model_ip_limited. The session is never admitted, so no chat call fires.
func TestChatModelIPLimitedAdmissionPath(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writeRawJSON(w, http.StatusConflict, limitedChatBody())
			return
		}
		writeRawJSON(w, http.StatusNotFound, `{"error":"not found"}`)
	}
	ts, p := newTestServer(t, nil, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, data)
	}
	if got := errorCode(t, data); got != "model_ip_limited" {
		t.Errorf("code = %q, want model_ip_limited", got)
	}
	until, _ := p.ModelUnfit(modelA)
	if until.IsZero() {
		t.Error("pool unfit not set after admission-path limited refusal")
	}
}

// TestChatModelIPLimitedConcurrentRefusals pins the unfit-guard race fix
// (SEC-1): concurrent requests to an unfit model are all fast-refused at the
// entry guard with 409 and never reach the upstream. CI runs the suite with
// -race, which the pre-fix in-place RetryAfter mutation of the shared
// registry error would flag.
func TestChatModelIPLimitedConcurrentRefusals(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		writeRawJSON(w, http.StatusConflict, limitedChatBody())
	}
	ts, p := newTestServer(t, nil, mock)
	chatURL := ts.URL + "/v1/chat/completions"
	body := chatBody(modelA)

	// Prime the unfit mark with one limited response.
	resp, _ := doJSON(t, http.MethodPost, chatURL, body, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("prime status = %d, want 409", resp.StatusCode)
	}
	if until, _ := p.ModelUnfit(modelA); until.IsZero() {
		t.Fatal("unfit not marked after prime")
	}
	// Let any tail-end upstream activity from the prime's retry flow settle,
	// then baseline: the entry-guard refusals must add ZERO upstream calls.
	time.Sleep(300 * time.Millisecond)
	before := mock.RequestsSnapshot()

	// Now the entry guard fast-refuses; hammer it concurrently. Every
	// refusal must be 409 and the upstream must see no new calls.
	const n = 16
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := http.Post(chatURL, "application/json", bytes.NewReader(body))
			if err != nil {
				codes[i] = -1
				return
			}
			defer func() { _ = r.Body.Close() }()
			_, _ = io.Copy(io.Discard, r.Body)
			codes[i] = r.StatusCode
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusConflict {
			t.Errorf("request %d status = %d, want 409", i, c)
		}
	}
	if got := mock.RequestsSnapshot(); got != before {
		t.Errorf("upstream requests = %d, want %d (prime baseline; guard refusals never reach upstream)", got, before)
	}
}
