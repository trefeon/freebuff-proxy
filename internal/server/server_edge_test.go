package server_test

// Server-level edge tests from the AuditServer P1/P2 gap list: malformed
// chat inputs, relay failure paths, model availability annotations, healthz
// field coverage, metrics escaping, auth scheme edges, and the chat-path
// country-block cooldown. Split from server_test.go because that file is
// already 1700+ lines.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/testutil"
)

// TestChatOversizedBody413 pins the 32MiB body cap: a larger payload is
// rejected with 413 content_too_large before any pool/upstream contact.
func TestChatOversizedBody413(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	oversized := bytes.Repeat([]byte("a"), 32<<20+1)
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", oversized, nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if !strings.Contains(string(data), "content_too_large") {
		t.Errorf("body missing content_too_large: %s", truncate(string(data), 200))
	}
	if mock.Requests != 0 {
		t.Errorf("upstream requests = %d, want 0 (oversized body rejected before pool)", mock.Requests)
	}
}

// TestChatInvalidJSONTable pins the malformed-body matrix: non-object JSON
// (`[]`, `"x"`, garbage, empty body) → 400 invalid_json; a JSON `null` body
// unmarshals to a nil map, which falls through to the missing-model check →
// 400 model_not_found (current behavior pinned).
func TestChatInvalidJSONTable(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	cases := []struct {
		name string
		body string
		want string // error code
	}{
		{"array", `[]`, "invalid_json"},
		{"string", `"x"`, "invalid_json"},
		{"garbage", `{not json`, "invalid_json"},
		{"empty body", ``, "invalid_json"},
		{"null body", `null`, "model_not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(tc.body), nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, data)
			}
			if !strings.Contains(string(data), tc.want) {
				t.Errorf("body missing %q: %s", tc.want, data)
			}
		})
	}
	if mock.Requests != 0 {
		t.Errorf("upstream requests = %d, want 0 (malformed bodies rejected before pool)", mock.Requests)
	}
}

// TestChatModelAsNumber pins the model type-assertion: a numeric model field
// fails the string assertion and is treated as missing → 400 model_not_found.
func TestChatModelAsNumber(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions",
		[]byte(`{"model":123,"messages":[{"role":"user","content":"hi"}]}`), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "model_not_found") {
		t.Errorf("body missing model_not_found: %s", data)
	}
}

// TestChatStreamStringCoercedFalse pins the stream type assertion: a string
// `"true"` is NOT a bool, so the request is served non-streaming (relayJSON
// accumulates a chat.completion JSON response) instead of SSE.
func TestChatStreamStringCoercedFalse(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-cf", 1, `"choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-cf", 1, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`))
	ts, _ := newTestServer(t, nil, mock)

	req := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"stream":"true"}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(req), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json (string stream coerced to non-stream)", ct)
	}
	if strings.Contains(string(data), "data: ") {
		t.Errorf("response is SSE despite string stream value: %s", truncate(string(data), 200))
	}
	if !strings.Contains(string(data), "Hello") {
		t.Errorf("accumulated JSON missing content: %s", truncate(string(data), 200))
	}
}

// TestRelayJSONGarbageUpstream502 pins the non-streaming failure path: an
// upstream line that looks like JSON but fails to decode errors the
// accumulator → 502 upstream_unavailable (nothing partial is written).
func TestRelayJSONGarbageUpstream502(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// A '{'-prefixed line that is not valid JSON: parseSSEData accepts
		// it as a payload, the accumulator's decode fails.
		_, _ = io.WriteString(w, `{"choices":[`)
	}
	ts, _ := newTestServer(t, nil, mock)

	req := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"stream":false}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(req), nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "upstream_unavailable") {
		t.Errorf("body missing upstream_unavailable: %s", data)
	}
}

// TestRelayStreamMidstreamDeath pins the streaming failure path: an upstream
// scanner error (a line longer than the 16MiB scanner cap) after a delivered
// chunk writes an upstream_stream_error chunk and then [DONE], preserving the
// 200 status — the client sees a clean stream terminator, not a hang.
func TestRelayStreamMidstreamDeath(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-d1", 1, `"choices":[{"index":0,"delta":{"content":"before"},"finish_reason":null}]`)))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// A single line over maxStreamLine (16MiB) makes bufio.Scanner
		// return ErrTooLong mid-stream — a deterministic "connection died
		// mid-response" stand-in.
		_, _ = io.WriteString(w, strings.Repeat("x", 16<<20+1))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 preserved: %s", resp.StatusCode, truncate(string(data), 200))
	}
	body := string(data)
	if !strings.Contains(body, `"content":"before"`) {
		t.Errorf("stream missing the chunk relayed before the death: %s", truncate(body, 300))
	}
	if !strings.Contains(body, "upstream_stream_error") {
		t.Errorf("stream missing upstream_stream_error chunk: %s", truncate(body, 300))
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("stream must end with [DONE]: %q", truncate(body, 300))
	}
}

// quotaExhaustedMock admits a session whose quota for modelA is already at
// the limit (recent == limit), so modelAvailability must annotate
// quota_exhausted.
func quotaExhaustedMock(t *testing.T) *testutil.MockUpstream {
	t.Helper()
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	mock.RateLimitsByModel = map[string]any{
		modelA: map[string]any{
			"model":       modelA,
			"limit":       5,
			"recentCount": 5,
			"period":      "pacific_day",
			"resetAt":     "2026-08-16T07:00:00.000Z",
		},
	}
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-qx", 1,
		`"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-qx", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`))
	return mock
}

// modelRow is the /v1/models entry shape the annotation tests decode.
type modelRow struct {
	ID        string `json:"id"`
	Available bool   `json:"available"`
	Status    string `json:"status"`
}

// modelsByID decodes /v1/models into a map keyed by model id.
func modelsByID(t *testing.T, ts *httptest.Server) map[string]modelRow {
	t.Helper()
	resp, data := doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Data []modelRow `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("models is not JSON: %v: %s", err, data)
	}
	byID := make(map[string]modelRow, len(out.Data))
	for _, m := range out.Data {
		byID[m.ID] = m
	}
	return byID
}

// TestModelsQuotaExhaustedAnnotation pins the quota_exhausted annotation: a
// session whose quota window is full (recent >= limit) marks the model
// status quota_exhausted. available stays true — the annotation is advisory
// and never hides a listed model (current behavior pinned).
func TestModelsQuotaExhaustedAnnotation(t *testing.T) {
	mock := quotaExhaustedMock(t)
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}
	m := modelsByID(t, ts)[modelA]
	if m.Status != "quota_exhausted" {
		t.Errorf("status = %q, want quota_exhausted", m.Status)
	}
	if !m.Available {
		t.Error("available = false, want true (quota exhaustion is advisory, never hides)")
	}
}

// TestModelsLockedAnnotation pins the locked annotation: a session in the
// disabled status marks every model locked.
func TestModelsLockedAnnotation(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "disabled"
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-lk", 1, `"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]`))
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}
	m := modelsByID(t, ts)[modelA]
	if m.Status != "locked" {
		t.Errorf("status = %q, want locked (session disabled)", m.Status)
	}
	if !m.Available {
		t.Error("available = false, want true (a lock is advisory, never hides)")
	}
}

// TestModelsQuotaExhaustedNotHidden pins the MODELS_HIDE_UNAVAILABLE +
// quota_exhausted interaction: hide-unavailable only prunes models whose
// available flag is false (region-limited). A quota-exhausted model keeps
// available=true, so it stays listed even with hide-unavailable on.
func TestModelsQuotaExhaustedNotHidden(t *testing.T) {
	mock := quotaExhaustedMock(t)
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.ModelsHideUnavailable = true }, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}
	byID := modelsByID(t, ts)
	m, ok := byID[modelA]
	if !ok {
		t.Fatalf("model %q pruned from /v1/models — quota exhaustion must not hide it (available stays true)", modelA)
	}
	if m.Status != "quota_exhausted" {
		t.Errorf("status = %q, want quota_exhausted", m.Status)
	}
}

// TestHealthzQueueFields pins the queue position/depth serialization: a
// queued session's snapshot surfaces SessionQueuePosition/SessionQueueDepth
// in /healthz.
func TestHealthzQueueFields(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionSequence = []string{"queued"}
	mock.QueuePosition = 3
	mock.QueueDepth = 7
	// pollAt in the future keeps the first admission queued (a zero wait
	// would advance the refresh loop to the next session mode immediately).
	mock.EstimatedWaitMs = 5000
	ts, _ := newTestServer(t, nil, mock)

	// A queued admission fails the chat with 503 and leaves the token's
	// session in the queued state with position/depth recorded.
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("chat status = %d, want 503 (queued): %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Tokens []struct {
			SessionStatus        string `json:"SessionStatus"`
			SessionQueuePosition int    `json:"SessionQueuePosition"`
			SessionQueueDepth    int    `json:"SessionQueueDepth"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("healthz is not JSON: %v: %s", err, data)
	}
	if len(out.Tokens) != 1 {
		t.Fatalf("tokens = %d, want 1", len(out.Tokens))
	}
	tok := out.Tokens[0]
	if tok.SessionStatus != "queued" {
		t.Errorf("SessionStatus = %q, want queued", tok.SessionStatus)
	}
	if tok.SessionQueuePosition != 3 || tok.SessionQueueDepth != 7 {
		t.Errorf("queue = %d/%d, want 3/7", tok.SessionQueuePosition, tok.SessionQueueDepth)
	}
}

// TestHealthzCooldownFields pins the cooldown serialization: after an
// auth-rejected chat the token's CooldownUntil is surfaced in /healthz.
func TestHealthzCooldownFields(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = 401
	mock.ChatErrorBody = `{"error":{"message":"unauthorized","type":"authentication_error"}}`
	ts, _ := newTestServer(t, nil, mock)

	doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)

	resp, data := doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Tokens []struct {
			CooldownUntil time.Time `json:"CooldownUntil"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("healthz is not JSON: %v: %s", err, data)
	}
	if len(out.Tokens) != 1 {
		t.Fatalf("tokens = %d, want 1", len(out.Tokens))
	}
	cd := out.Tokens[0].CooldownUntil
	if cd.IsZero() {
		t.Fatal("CooldownUntil is zero — cooldown not serialized after a 401")
	}
	if !cd.After(time.Now().Add(29 * time.Minute)) {
		t.Errorf("CooldownUntil = %v, want ~now+30m", cd)
	}
}

// TestMetricsLabelEscapingBackslashNewline extends the label-escaping test to
// backslashes and newlines (only quotes were covered before): both must be
// escaped so the Prometheus text format stays parseable.
func TestMetricsLabelEscapingBackslashNewline(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// The model id carries a literal backslash AND a literal newline
	// (upstream-derived label values).
	modelID := "weird\\mod\nel"
	mock.RateLimitsByModel = map[string]any{
		modelID: map[string]any{
			"model":       modelID,
			"limit":       5,
			"recentCount": 4,
			"period":      "p_day",
			"resetAt":     "2026-08-16T07:00:00.000Z",
		},
	}
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-qs", 1,
		`"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-qs", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`))
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}
	resp, data = doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200: %s", resp.StatusCode, data)
	}
	body := string(data)
	for _, want := range []string{
		`freebuff_proxy_quota_recent{token="1",model="weird\\mod\nel",period="p_day"} 4`,
		`freebuff_proxy_quota_limit{token="1",model="weird\\mod\nel",period="p_day"} 5`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %s in:\n%s", want, body)
		}
	}
}

// TestMetricsEmptyPool pins the zero-token pool: an empty pool still renders
// a 200 with the gauge families and a zero token count, and emits no
// per-token lines.
func TestMetricsEmptyPool(t *testing.T) {
	ts, _ := newTestServer(t, nil) // no mocks → zero tokens
	resp, data := doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200: %s", resp.StatusCode, data)
	}
	body := string(data)
	for _, want := range []string{
		"freebuff_proxy_uptime_seconds",
		"freebuff_proxy_models_total",
		"freebuff_proxy_tokens_total 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %s in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "freebuff_proxy_token_messages_24h{") {
		t.Errorf("empty pool emitted a per-token line:\n%s", body)
	}
}

// TestAuthSchemeCaseInsensitive pins the Authorization scheme check: case-insensitive
// "Bearer " / "bearer " / "BEARER " prefix is recognized (RFC 7235 / RFC 6750),
// while an empty value or missing space is rejected with 401.
func TestAuthSchemeCaseInsensitive(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, []string{"sk-test"}, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	for _, hdr := range []map[string]string{
		{"Authorization": "Bearer "}, // empty value
		{"Authorization": "bearer "}, // lowercase empty value
		{"Authorization": "Bearer"},  // no space after scheme
		{"Authorization": "Basic sk-test"},
	} {
		resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), hdr)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("header %v status = %d, want 401: %s", hdr, resp.StatusCode, data)
		}
	}

	// Case variations of Bearer scheme + value pass.
	for _, hdr := range []map[string]string{
		{"Authorization": "Bearer sk-test"},
		{"Authorization": "bearer sk-test"},
		{"Authorization": "BEARER sk-test"},
		{"Authorization": "bEaReR sk-test"},
	} {
		resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), hdr)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("scheme %v status = %d, want 200: %s", hdr, resp.StatusCode, data)
		}
	}
}

// TestModelsHEAD pins ServeMux's GET-pattern behavior: a HEAD /v1/models is
// matched by the "GET /v1/models" route and answers 200 with no body.
func TestModelsHEAD(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodHead, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD /v1/models status = %d, want 200: %s", resp.StatusCode, data)
	}
	if len(data) != 0 {
		t.Errorf("HEAD returned a body (%d bytes), want none", len(data))
	}
}

// TestChatCountryBlockCooldown pins the chat-path country-block cooldown:
// a chat that hits 403 country_blocked cools the token down, and the next
// request surfaces the remembered 403 without re-hitting the upstream.
func TestChatCountryBlockCooldown(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = 403
	mock.ChatErrorBody = `{"status":"country_blocked","countryCode":"US","countryBlockReason":"region_restricted"}`
	ts, p := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "country_blocked") {
		t.Errorf("body missing country_blocked: %s", data)
	}
	snap := p.Snapshot()[0]
	if snap.CooldownUntil.Before(time.Now().Add(14 * time.Minute)) {
		t.Errorf("cooldown until = %v, want ~now+15m (chat-path country block must cooldown)", snap.CooldownUntil)
	}
	if snap.CountryBlockReason != "region_restricted" {
		t.Errorf("countryBlockReason = %q, want region_restricted", snap.CountryBlockReason)
	}

	// The upstream heals, but the remembered block surfaces without a re-hit.
	mock.ChatStatus = 200
	resp2, data2 := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("second request status = %d, want 403 (remembered block): %s", resp2.StatusCode, data2)
	}
	if !strings.Contains(string(data2), "country_blocked") {
		t.Errorf("second body missing country_blocked: %s", data2)
	}
	if got := len(mock.RecordedChatHeaders); got != 1 {
		t.Errorf("upstream chat calls = %d, want 1 (cooldown skipped the second request)", got)
	}
}

// TestBridgeChatSessionInvalidBoundedRetry pins the bridge-path recovery
// budget: a session-expired chat error recreates the session once and
// retries, then fails with 502 — never an unbounded recreate loop.
// (session_superseded is terminal since #119 — see
// TestBridgeChatSessionSupersededTerminal.)
func TestBridgeChatSessionInvalidBoundedRetry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = http.StatusBadRequest
	mock.ChatErrorBody = `{"error":{"message":"session_expired"}}`
	ts, _ := newBridgeTestServer(t, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA),
		map[string]string{"Authorization": "Bearer client-tok-ss"})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "upstream_unavailable") {
		t.Errorf("body missing upstream_unavailable: %s", data)
	}
	if got := len(mock.RecordedChatHeaders); got != 2 {
		t.Errorf("upstream chat attempts = %d, want exactly 2 (bounded retry)", got)
	}
	if got := mock.SessionCreates; got != 2 {
		t.Errorf("upstream session creates = %d, want exactly 2 (session recreated once)", got)
	}
}

// TestBridgeChatSessionSupersededTerminal pins #119 on the bridge path: a
// 409 session_superseded chat gate is terminal — exactly one upstream chat
// call and no session recreate, surfaced as 409 session_superseded.
func TestBridgeChatSessionSupersededTerminal(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = http.StatusConflict
	mock.ChatErrorBody = `{"error":{"message":"session_superseded"}}`
	ts, _ := newBridgeTestServer(t, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA),
		map[string]string{"Authorization": "Bearer client-tok-ss"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "session_superseded") {
		t.Errorf("body missing session_superseded code: %s", data)
	}
	if got := len(mock.RecordedChatHeaders); got != 1 {
		t.Errorf("upstream chat attempts = %d, want 1 (no auto-POST)", got)
	}
	if got := mock.SessionCreates; got != 1 {
		t.Errorf("upstream session creates = %d, want exactly 1 (initial admission only)", got)
	}
}

// truncate shortens a string for error messages.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return fmt.Sprintf("%s…(%d bytes)", s[:n], len(s))
}
