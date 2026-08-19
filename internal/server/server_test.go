package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/server"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

// modelA must map to an agent with EXCLUSIVE ownership in the registry
// fallback map (see internal/registry/registry_test.go expectedFallback).
const modelA = "z-ai/glm-5.2"

// newTestServer wires the real stack - mock upstream per token, real
// clients/session managers, fallback registry, pool, and the server - behind
// one httptest server.
func newTestServer(t *testing.T, apiKeys []string, mocks ...*testutil.MockUpstream) (*httptest.Server, *pool.Pool) {
	return newTestServerCfg(t, apiKeys, nil, mocks...)
}

// newTestServerCfg is newTestServer with a config mutation hook (e.g.
// enabling TRANSIENT_RETRIES for retry/metrics tests).
func newTestServerCfg(t *testing.T, apiKeys []string, mut func(*config.Config), mocks ...*testutil.MockUpstream) (*httptest.Server, *pool.Pool) {
	t.Helper()
	cfg := &config.Config{
		AuthTokens:         make([]string, len(mocks)),
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
		APIKeys:            apiKeys,
		LogAccess:          true,
	}
	if mut != nil {
		mut(cfg)
	}
	clients := make([]*upstream.Client, 0, len(mocks))
	sessions := make([]*session.Manager, 0, len(mocks))
	for i, mock := range mocks {
		cfg.AuthTokens[i] = fmt.Sprintf("tok-%d", i)
		clientCfg := *cfg
		clientCfg.UpstreamBaseURL = mock.URL()
		client, err := upstream.New(cfg.AuthTokens[i], &clientCfg)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
		sessions = append(sessions, session.NewManager(client))
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, clients, sessions, reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg, p, reg, nil, nil, "")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, p
}

func chatBody(model string) []byte {
	return []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"ping"}],"stream":true}`)
}

// testClient bounds every doJSON request: a handler regression that hangs
// (e.g. an unbounded retry loop) fails the request in 30s instead of hanging
// the whole suite until the go-test timeout.
var testClient = &http.Client{Timeout: 30 * time.Second}

// doJSON performs one request and returns the response plus its full body.
func doJSON(t *testing.T, method, url string, body []byte, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, data
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// chunk renders one OpenAI-style SSE chat chunk with the shared id/model.
func chunk(id string, created int64, payload string) string {
	return `{"id":"` + id + `","object":"chat.completion.chunk","created":` +
		fmt.Sprintf("%d", created) + `,"model":"` + modelA + `",` + payload + `}`
}

func TestChatStream(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	chunks := []string{
		chunk("chatcmpl-s1", 100, `"choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]`),
		chunk("chatcmpl-s1", 100, `"choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]`),
		chunk("chatcmpl-s1", 100, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`),
	}
	for _, c := range chunks {
		mock.ChatBody += testutil.SSEEvent(c)
	}
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	body := string(data)
	for _, want := range []string{`"content":"Hello"`, `"content":" world"`, `"finish_reason":"stop"`} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %s: %s", want, body)
		}
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("stream must end with [DONE]: %q", body)
	}

	// Envelope assertions on the recorded upstream request.
	if len(mock.RecordedChatHeaders) != 1 {
		t.Fatalf("upstream chat calls = %d, want 1", len(mock.RecordedChatHeaders))
	}
	h := mock.RecordedChatHeaders[0]
	// #106: the chat POST carries no model/instance headers — they ride in
	// the body metadata only.
	if got := h.Get("x-freebuff-model"); got != "" {
		t.Errorf("x-freebuff-model = %q on the chat POST, want absent (#106)", got)
	}
	if got := h.Get("x-freebuff-instance-id"); got != "" {
		t.Errorf("x-freebuff-instance-id = %q on the chat POST, want absent (#106)", got)
	}
	recorded := mock.RecordedChatBodies[0]
	for _, want := range []string{`"codebuff_metadata"`, `"data_collection":"deny"`, `"stream":true`, `"stop":["cb_easp"]`, `"run_id":"run-0001"`} {
		if !strings.Contains(recorded, want) {
			t.Errorf("upstream body missing %s: %s", want, recorded)
		}
	}
	// #80+#103: trace_session_id is minted once per run and threaded through
	// the envelope; client_id is a FRESH random 13-char base36 draw per chat
	// call — never the sess:/run:-prefixed shapes the server fingerprints as
	// a proxy.
	if !strings.Contains(recorded, `"trace_session_id":"`) {
		t.Errorf("upstream body missing trace_session_id: %s", recorded)
	}
	if strings.Contains(recorded, `"client_id":"sess:`) || strings.Contains(recorded, `"client_id":"run:`) {
		t.Errorf("upstream body carries a prefixed client_id: %s", recorded)
	}
	if !regexp.MustCompile(`"client_id":"[a-z0-9]{13}"`).MatchString(recorded) {
		t.Errorf("upstream body missing a 13-char base36 client_id: %s", recorded)
	}
}

func TestChatNonStream(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-n1", 101, `"choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-n1", 101, `"choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-n1", 101, `"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\""}}]},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-n1", 101, `"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"Paris\"}"}}]},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-n1", 101, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-n1", 101, `"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}`))
	ts, _ := newTestServer(t, nil, mock)

	req := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"stream":false}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(req), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var out struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Created int64  `json:"created"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, data)
	}
	if out.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", out.Object)
	}
	if out.Model != modelA {
		t.Errorf("model = %q, want %q", out.Model, modelA)
	}
	if out.Created != 101 {
		t.Errorf("created = %d, want 101", out.Created)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(out.Choices))
	}
	choice := out.Choices[0]
	if choice.Message.Content != "Hello world" {
		t.Errorf("content = %q, want stitched \"Hello world\"", choice.Message.Content)
	}
	if choice.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(choice.Message.ToolCalls))
	}
	tc := choice.Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Type != "function" || tc.Function.Name != "get_weather" {
		t.Errorf("tool call = %+v, want call_1/function/get_weather", tc)
	}
	if tc.Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("stitched arguments = %q, want %q", tc.Function.Arguments, `{"city":"Paris"}`)
	}
	if out.Usage.TotalTokens != 30 || out.Usage.PromptTokens != 10 || out.Usage.CompletionTokens != 20 {
		t.Errorf("usage = %+v, want 10/20/30", out.Usage)
	}
}

// TestChatFeedsSpendLedger pins the #122 spend feeder: every successful chat
// completion records the upstream usage total into the token's spend ledger
// (streaming and non-stream paths), so SpendDay/Spend24h reflect real usage
// instead of the pre-wiring zeros.
func TestChatFeedsSpendLedger(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-s1", 1, `"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-s1", 1, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}`))
	ts, pool := newTestServer(t, nil, mock)

	req := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(req), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200: %s", resp.StatusCode, data)
	}

	snaps := pool.Snapshot()
	if len(snaps) != 1 {
		t.Fatalf("pool tokens = %d, want 1", len(snaps))
	}
	if snaps[0].SpendDay != 13 || snaps[0].Spend24h != 13 {
		t.Errorf("spend after stream chat = %d/%d, want 13/13 (usage 11+2)", snaps[0].SpendDay, snaps[0].Spend24h)
	}

	// The non-stream path feeds the same ledger: a second completion
	// accumulates on top.
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-s2", 2, `"choices":[{"index":0,"delta":{"content":"yo"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-s2", 2, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}`))
	req2 := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"stream":false}`
	resp2, _ := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(req2), nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("non-stream status = %d, want 200", resp2.StatusCode)
	}
	snaps = pool.Snapshot()
	if snaps[0].SpendDay != 25 || snaps[0].Spend24h != 25 {
		t.Errorf("spend after two chats = %d/%d, want 25/25 (13+12)", snaps[0].SpendDay, snaps[0].Spend24h)
	}
}

// TestHealthzSpend pins the /healthz spend surface (issue #122): the ledger
// buckets fed by the chat feeder, the advisory MAX_SPEND_PER_DAY ceiling
// (SpendLimit), the capped SpendPct, and the SpendLimited refusal counter.
func TestHealthzSpend(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-s1", 1, `"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-s1", 1, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}`))
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.MaxSpendPerDay = 100 }, mock)

	req := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(req), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Tokens []struct {
			Spend24h     int64 `json:"Spend24h"`
			SpendDay     int64 `json:"SpendDay"`
			SpendLimit   int64 `json:"SpendLimit"`
			SpendPct     int   `json:"SpendPct"`
			SpendLimited int   `json:"SpendLimited"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("healthz is not JSON: %v: %s", err, data)
	}
	if len(out.Tokens) != 1 {
		t.Fatalf("tokens = %d, want 1", len(out.Tokens))
	}
	tok := out.Tokens[0]
	if tok.Spend24h != 13 || tok.SpendDay != 13 {
		t.Errorf("healthz spend = %d/%d, want 13/13 (usage 11+2)", tok.Spend24h, tok.SpendDay)
	}
	if tok.SpendLimit != 100 {
		t.Errorf("SpendLimit = %d, want 100 (MAX_SPEND_PER_DAY)", tok.SpendLimit)
	}
	if tok.SpendPct != 13 {
		t.Errorf("SpendPct = %d, want 13 (13 of 100)", tok.SpendPct)
	}
	if tok.SpendLimited != 0 {
		t.Errorf("SpendLimited = %d, want 0 (no upstream spend_limited refusals)", tok.SpendLimited)
	}
}

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
	// Issue #91: every new parent run creates a context-pruner child run that
	// is started and immediately FINISHed as a best-effort side effect
	// through the bounded queue. The invalidated run-0001 contributes its
	// child's FINISH, and the re-STARTed run-0002 contributes its child's
	// FINISH — so exactly 2 upstream FINISH calls (child-run ids only, never
	// a FINISH of the invalidated parent). The queue is async: poll.
	// FinishedRunsSnapshot() is the race-safe accessor (the mock's server
	// goroutine appends to FinishedRuns).
	eventually(t, "both context-pruner children FINISHed", func() bool {
		finished := mock.FinishedRunsSnapshot()
		if len(finished) != 2 {
			return false
		}
		for _, f := range finished {
			if !strings.HasPrefix(f.RunID, "child-run-") {
				return false
			}
		}
		return true
	})
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
	if !strings.Contains(string(data), "upstream_unavailable") {
		t.Errorf("body missing upstream_unavailable: %s", data)
	}
	if got := len(mock.RecordedChatHeaders); got != 2 {
		t.Errorf("upstream chat attempts = %d, want exactly 2 (bounded retry)", got)
	}
	if got := mock.SessionCreates; got != 2 {
		t.Errorf("upstream session creates = %d, want exactly 2 (bounded retry)", got)
	}
}

// TestChatSessionSupersededRetries pins #119: 409 session_superseded (another
// instance took over the account, endsTheSession:true) retries once — the
// cached session is dropped and a fresh session is acquired for the retry.
// This avoids the 30s model lock 9router applies on a 503 response. Two chat
// attempts, two session creates, success on the retry.
func TestChatSessionSupersededRetries(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// First chat attempt returns session_superseded, second succeeds.
	callCount := 0
	originalHandler := mock.ChatHandler
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			// First call: session_superseded
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"session_superseded"}}`))
			return
		}
		// Second call: success
		if originalHandler != nil {
			originalHandler(w, r)
		} else {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: " + chunk("cmpl-test", 1234567890, `"choices":[{"delta":{"content":"ok"},"index":0}]`) + "\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}
	}
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	if got := len(mock.RecordedChatHeaders); got != 2 {
		t.Errorf("upstream chat attempts = %d, want exactly 2 (retry on superseded)", got)
	}
	if got := mock.SessionCreates; got != 2 {
		t.Errorf("session creates = %d, want exactly 2 (invalidated + re-acquired)", got)
	}
}

// TestChatSessionSupersededBoundedRetry pins #119: when the retry ALSO fails
// on session_superseded, the attempt budget caps at 2 attempts and surfaces
// 503 session_superseded with Retry-After: 1.
func TestChatSessionSupersededBoundedRetry(t *testing.T) {
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
	if got := len(mock.RecordedChatHeaders); got != 2 {
		t.Errorf("upstream chat attempts = %d, want exactly 2 (bounded retry on superseded)", got)
	}
	if got := mock.SessionCreates; got != 2 {
		t.Errorf("session creates = %d, want exactly 2 (invalidated + re-acquired once)", got)
	}
}

func TestClientAbortPropagates(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	chatSeen := make(chan struct{})
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-chatSeen:
		default:
			close(chatSeen)
		}
		select {
		case <-r.Context().Done():
			mock.AbortDetected.Store(true)
		case <-time.After(30 * time.Second):
		}
	}
	ts, _ := newTestServer(t, nil, mock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/chat/completions", bytes.NewReader(chatBody(modelA)))
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		errCh <- err
	}()

	select {
	case <-chatSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never saw the chat request")
	}

	cancel()
	eventually(t, "upstream abort detection", func() bool {
		return mock.AbortDetected.Load()
	})
	<-errCh
}

func TestAuth(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, []string{"sk-test"}, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	// No key.
	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key status = %d, want 401", resp.StatusCode)
	}
	if !strings.Contains(string(data), "invalid_api_key") {
		t.Errorf("body missing invalid_api_key: %s", data)
	}

	// Wrong keys, both schemes.
	for _, h := range []map[string]string{
		{"Authorization": "Bearer wrong"},
		{"x-api-key": "wrong"},
	} {
		resp, _ := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), h)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("wrong key %v status = %d, want 401", h, resp.StatusCode)
		}
	}

	// x-api-key ok.
	resp, data = doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"x-api-key": "sk-test"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("x-api-key status = %d, want 200: %s", resp.StatusCode, data)
	}

	// Bearer ok.
	resp, data = doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": "Bearer sk-test"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Bearer status = %d, want 200: %s", resp.StatusCode, data)
	}

	// healthz is exempt from auth.
	resp, _ = doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want 200 (exempt)", resp.StatusCode)
	}

	// models requires auth too.
	resp, _ = doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("models without key status = %d, want 401", resp.StatusCode)
	}

	// The rejected requests must never have reached the pool/upstream; the
	// two accepted chats share one session and one run (same model).
	if mock.SessionCreates != 1 || len(mock.StartedRuns) != 1 {
		t.Errorf("upstream contact = %d session creates / %d started runs, want 1/1 (auth gates before pool)",
			mock.SessionCreates, len(mock.StartedRuns))
	}
}

func TestUnknownModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	for _, reqBody := range []string{
		`{"model":"no/such-model","messages":[{"role":"user","content":"hi"}]}`,
		`{"messages":[{"role":"user","content":"hi"}]}`,
	} {
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(reqBody), nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", resp.StatusCode, data)
		}
		if !strings.Contains(string(data), "model_not_found") {
			t.Errorf("body missing model_not_found: %s", data)
		}
		if !strings.Contains(string(data), "z-ai/glm-5.2") {
			t.Errorf("message missing model list: %s", data)
		}
	}

	// Rejected before the pool: the upstream must be untouched.
	if mock.SessionCreates != 0 {
		t.Errorf("upstream session creates = %d, want 0", mock.SessionCreates)
	}
}

func TestModelsEndpoint(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID                string `json:"id"`
			Object            string `json:"object"`
			Created           int64  `json:"created"`
			OwnedBy           string `json:"owned_by"`
			Available         bool   `json:"available"`
			Status            string `json:"status"`
			CurrentAccessTier string `json:"current_access_tier"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, data)
	}
	if out.Object != "list" {
		t.Errorf("object = %q, want list", out.Object)
	}
	// #121: the offline fallback pruned 5 dead model ids (laguna/ling/greg),
	// 15 -> 10 rows (registry fallbackAgents, free-agents.ts-verified).
	if len(out.Data) < 10 {
		t.Errorf("models = %d, want >= 10", len(out.Data))
	}
	for i, m := range out.Data {
		if m.ID == "" || m.Object != "model" || m.OwnedBy == "" {
			t.Errorf("model %d malformed: %+v", i, m)
		}
		if m.Created != out.Data[0].Created {
			t.Errorf("model %d created = %d, want %d (pinned to server start)", i, m.Created, out.Data[0].Created)
		}
		// Advisory annotation: never hide a working model, so available is
		// true and status "unknown" when no session has reported anything.
		if !m.Available {
			t.Errorf("model %s available = false, want true (advisory default)", m.ID)
		}
		if m.Status == "" {
			t.Errorf("model %s status empty, want a status string", m.ID)
		}
	}
	if out.Data[0].Created <= 0 || out.Data[0].Created > time.Now().Unix() {
		t.Errorf("created = %d, not a plausible server-start timestamp", out.Data[0].Created)
	}
}

func TestHealthz(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	ts, _ := newTestServer(t, nil, mock0, mock1)

	resp, data := doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		UptimeSeconds float64 `json:"uptime_seconds"`
		Models        int     `json:"models"`
		Tokens        []any   `json:"tokens"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, data)
	}
	if out.UptimeSeconds < 0 {
		t.Errorf("uptime_seconds = %v, want >= 0", out.UptimeSeconds)
	}
	// #121: fallback registry 15 -> 10 rows after pruning dead model ids.
	if out.Models < 10 {
		t.Errorf("models = %d, want >= 10", out.Models)
	}
	if len(out.Tokens) != 2 {
		t.Errorf("tokens = %d, want 2", len(out.Tokens))
	}
}
func TestMetricsEndpoint(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	ts, _ := newTestServer(t, nil, mock0)

	resp, data := doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	body := string(data)
	if !strings.Contains(body, "freebuff_proxy_uptime_seconds") || !strings.Contains(body, "freebuff_proxy_models_total") {
		t.Errorf("metrics missing expected keys: %s", body)
	}
}

// quotaMock wires a mock whose admission response carries rateLimitsByModel
// for z-ai/glm-5.2, so a chat drives session creation with quota data.
func quotaMock(t *testing.T) *testutil.MockUpstream {
	t.Helper()
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
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
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-q1", 100,
		`"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-q1", 100,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`))
	return mock
}

func TestHealthzQuota(t *testing.T) {
	mock := quotaMock(t)
	ts, _ := newTestServer(t, nil, mock)

	// A chat admits the session (which carries rateLimitsByModel).
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Tokens []struct {
			Quota       map[string]quotaEntry `json:"quota"`
			Entitlement map[string]float64    `json:"entitlement"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("healthz is not JSON: %v: %s", err, data)
	}
	if len(out.Tokens) != 1 {
		t.Fatalf("tokens = %d, want 1", len(out.Tokens))
	}
	q, ok := out.Tokens[0].Quota["z-ai/glm-5.2"]
	if !ok {
		t.Fatalf("healthz quota missing z-ai/glm-5.2: %+v", out.Tokens[0].Quota)
	}
	if q.Limit != 5 || q.RecentCount != 4 || q.Period != "pacific_day" {
		t.Errorf("quota = %+v, want limit=5 recent_count=4 period=pacific_day", q)
	}
	if q.ResetAt == "" {
		t.Error("reset_at missing from healthz quota entry")
	}
	if q.Entitlement["referral"] != 1 || q.Entitlement["streak"] != 3 {
		t.Errorf("entitlement = %+v, want referral=1 streak=3", q.Entitlement)
	}
	if len(out.Tokens[0].Entitlement) != 0 {
		t.Errorf("top-level entitlement = %+v, want omitted (empty)", out.Tokens[0].Entitlement)
	}
}

// TestModelsAnnotationWithQuota verifies /v1/models reflects a session that
// admitted a model: status becomes "available" once quotaByModel mentions it,
// and current_access_tier carries the admission's tier.
func TestModelsAnnotationWithQuota(t *testing.T) {
	mock := quotaMock(t)
	mock.AccessTier = "limited"
	mock.CountryCode = "US"
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Data []struct {
			ID                string `json:"id"`
			Available         bool   `json:"available"`
			Status            string `json:"status"`
			CurrentAccessTier string `json:"current_access_tier"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("models is not JSON: %v: %s", err, data)
	}
	var found *struct {
		ID                string `json:"id"`
		Available         bool   `json:"available"`
		Status            string `json:"status"`
		CurrentAccessTier string `json:"current_access_tier"`
	}
	for i := range out.Data {
		if out.Data[i].ID == modelA {
			found = &out.Data[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("model %q not in /v1/models", modelA)
	}
	if !found.Available {
		t.Errorf("available = false, want true")
	}
	if found.Status != "available" {
		t.Errorf("status = %q, want available (session admitted the model)", found.Status)
	}
	if found.CurrentAccessTier != "limited" {
		t.Errorf("current_access_tier = %q, want limited (from session admission)", found.CurrentAccessTier)
	}
}

// TestModelsRegionLimited verifies the tier-aware annotation: with a token in
// the 'limited' tier (region/privacy demotion), a model outside the limited
// allowlist with no admission signal is marked available=false +
// status=region_limited, while an allowlisted model (mimo-v2.5) stays
// available. An admitted model keeps its available status (admission is
// ground truth).
func TestModelsRegionLimited(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.AccessTier = "limited"
	mock.CountryCode = "US"
	ts, _ := newTestServer(t, nil, mock)

	// Admit a session so the token's snapshot carries tier=limited. The
	// admitted model (modelA) becomes available by admission.
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Data []struct {
			ID        string `json:"id"`
			Available bool   `json:"available"`
			Status    string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("models is not JSON: %v: %s", err, data)
	}
	byID := map[string]struct {
		Available bool
		Status    string
	}{}
	for _, m := range out.Data {
		byID[m.ID] = struct {
			Available bool
			Status    string
		}{m.Available, m.Status}
	}
	// mimo/mimo-v2.5 is on the limited allowlist -> available.
	if m, ok := byID["mimo/mimo-v2.5"]; !ok || !m.Available {
		t.Errorf("mimo/mimo-v2.5 = %+v, want available:true on limited tier", m)
	}
	// deepseek-v4-flash is NOT on the limited allowlist anymore (disabled upstream)
	// and was never admitted -> available:false + region_limited.
	if m, ok := byID["deepseek/deepseek-v4-flash"]; !ok {
		t.Errorf("deepseek-v4-flash missing from /v1/models")
	} else if m.Available {
		t.Errorf("deepseek-v4-flash available = true, want false on limited tier")
	} else if m.Status != "region_limited" {
		t.Errorf("deepseek-v4-flash status = %q, want region_limited", m.Status)
	}
	// anthropic/claude-fable-5 is NOT on the limited allowlist and was never
	// admitted -> available:false + region_limited.
	if m, ok := byID["anthropic/claude-fable-5"]; !ok {
		t.Errorf("fable-5 missing from /v1/models")
	} else if m.Available {
		t.Errorf("fable-5 available = true, want false on limited tier")
	} else if m.Status != "region_limited" {
		t.Errorf("fable-5 status = %q, want region_limited", m.Status)
	}
}

// TestModelsHideUnavailable verifies MODELS_HIDE_UNAVAILABLE=true prunes
// region-limited models from the list so picker clients cannot select them.
func TestModelsHideUnavailable(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.AccessTier = "limited"
	mock.CountryCode = "US"
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.ModelsHideUnavailable = true
	}, mock)

	// Admit a session so the token snapshot carries tier=limited.
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("models is not JSON: %v: %s", err, data)
	}
	for _, m := range out.Data {
		if m.ID == "anthropic/claude-fable-5" {
			t.Errorf("fable-5 present in /v1/models with MODELS_HIDE_UNAVAILABLE=true")
		}
		if m.ID == "deepseek/deepseek-v4-flash" {
			t.Errorf("deepseek-v4-flash present in /v1/models with MODELS_HIDE_UNAVAILABLE=true (disabled on limited tier)")
		}
	}
	found := false
	for _, m := range out.Data {
		if m.ID == "mimo/mimo-v2.5" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("mimo/mimo-v2.5 pruned, want kept (limited allowlist)")
	}
}

// TestModelsAllowList verifies MODELS_ALLOW prunes /v1/models to exactly the
// allowlisted ids so picker clients never auto-select a model that 404s.
func TestModelsAllowList(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.ModelsAllow = []string{"deepseek/deepseek-v4-flash", modelA}
	}, mock)

	resp, data := doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("models is not JSON: %v: %s", err, data)
	}
	seen := map[string]bool{}
	for _, m := range out.Data {
		if m.ID != "deepseek/deepseek-v4-flash" && m.ID != modelA {
			t.Errorf("model %q listed outside MODELS_ALLOW", m.ID)
		}
		seen[m.ID] = true
	}
	if !seen["deepseek/deepseek-v4-flash"] || !seen[modelA] {
		t.Errorf("allowlisted models missing from /v1/models: %v", seen)
	}
	if len(out.Data) != 2 {
		t.Errorf("model count = %d, want 2 (allowlisted only)", len(out.Data))
	}
}

// TestModelsAllowRejectsChat pins the chat 404: a request whose resolved
// model is outside MODELS_ALLOW is rejected before any upstream call.
func TestModelsAllowRejectsChat(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.ModelsAllow = []string{"deepseek/deepseek-v4-flash"}
	}, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("chat status = %d, want 404: %s", resp.StatusCode, data)
	}
	var out struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("error body is not JSON: %v: %s", err, data)
	}
	if out.Error.Code != "model_not_found" {
		t.Errorf("error.code = %q, want model_not_found", out.Error.Code)
	}
	if !strings.Contains(out.Error.Message, "MODELS_ALLOW") {
		t.Errorf("error.message = %q, want a mention of MODELS_ALLOW", out.Error.Message)
	}
	if len(mock.RecordedChatHeaders) != 0 {
		t.Error("upstream chat recorded for a rejected model, want none")
	}
}

// TestModelsAllowResolvedAlias pins the allowlist contract: it compares
// against the RESOLVED model id (after registry alias resolution), so a
// client alias that resolves outside the list is rejected too.
func TestModelsAllowResolvedAlias(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.ModelAliases = map[string]string{"glm-alias": modelA}
		c.ModelsAllow = []string{"deepseek/deepseek-v4-flash"}
	}, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody("glm-alias"), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("chat (alias) status = %d, want 404: %s", resp.StatusCode, data)
	}
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("error body is not JSON: %v: %s", err, data)
	}
	if out.Error.Code != "model_not_found" {
		t.Errorf("error.code = %q, want model_not_found", out.Error.Code)
	}
	if len(mock.RecordedChatHeaders) != 0 {
		t.Error("upstream chat recorded for a rejected alias, want none")
	}
}

// TestModelsAllowMaxUpgrade pins the PREFER_MAX_MODELS interaction: the
// allowlist accepts both the -max UPGRADED id directly AND a base-model id
// whose -max variant is the resolved target (auto-upgrade + base-only
// allowlist coexist), while anything outside the list stays rejected.
func TestModelsAllowMaxUpgrade(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-max", 1, `"choices":[{"index":0,"delta":{"content":"ping"},"finish_reason":"stop"}]`))
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.PreferMaxModels = true
		c.ModelsAllow = []string{"deepseek/deepseek-v4-pro-max"}
	}, mock)

	// A base client id upgrades to the allowlisted -max variant → served.
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody("deepseek/deepseek-v4-pro"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat (max-upgraded, max listed) status = %d, want 200: %s", resp.StatusCode, data)
	}
	// A model outside the list stays rejected.
	resp, data = doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("chat (disallowed) status = %d, want 404: %s", resp.StatusCode, data)
	}

	// Base-only allowlist + auto-upgrade: the resolved -max id is accepted
	// through the allowlisted base id, so clients may keep requesting the
	// base id while the proxy serves the extended-context variant.
	mock2 := testutil.NewMock()
	defer mock2.Close()
	mock2.ChatBody = testutil.SSEEvent(chunk("chatcmpl-max2", 1, `"choices":[{"index":0,"delta":{"content":"ping"},"finish_reason":"stop"}]`))
	ts2, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.PreferMaxModels = true
		c.ModelsAllow = []string{"deepseek/deepseek-v4-pro"}
	}, mock2)

	resp, data = doJSON(t, http.MethodPost, ts2.URL+"/v1/chat/completions", chatBody("deepseek/deepseek-v4-pro"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat (base allowlist + upgrade) status = %d, want 200: %s", resp.StatusCode, data)
	}
	// The -max id is also accepted when derived from an allowlisted base.
	resp, data = doJSON(t, http.MethodPost, ts2.URL+"/v1/chat/completions", chatBody("deepseek/deepseek-v4-pro-max"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat (direct -max id of allowed base) status = %d, want 200: %s", resp.StatusCode, data)
	}
}

// TestModelsAllowEmptyIsOpen verifies an empty allowlist keeps current
// behavior: every model is served and listed.
func TestModelsAllowEmptyIsOpen(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-open", 1, `"choices":[{"index":0,"delta":{"content":"ping"},"finish_reason":"stop"}]`))
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}
	resp, data = doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("models is not JSON: %v: %s", err, data)
	}
	if len(out.Data) < 2 {
		t.Fatalf("model count = %d, want full catalog (no allowlist)", len(out.Data))
	}
	var hasModelA, hasFlash bool
	for _, m := range out.Data {
		hasModelA = hasModelA || m.ID == modelA
		hasFlash = hasFlash || m.ID == "deepseek/deepseek-v4-flash"
	}
	if !hasModelA || !hasFlash {
		t.Errorf("full catalog missing models: modelA=%v flash=%v", hasModelA, hasFlash)
	}
}

// TestSmokeDefaultsToFallbackModel verifies the smoke test with no explicit
// model probes the guaranteed fallback (deepseek-v4-flash), not the
// alphabetical-first catalog model (anthropic/claude-fable-5, a gated offer).
func TestSmokeDefaultsToFallbackModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-sm", 1, `"choices":[{"index":0,"delta":{"content":"ping"},"finish_reason":"stop"}]`))
	ts, _ := newTestServer(t, nil, mock)

	// Smoke with no model field: server picks the fallback.
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/admin/smoke", []byte(`{"prompt":"ping"}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("smoke status = %d, want 200: %s", resp.StatusCode, data)
	}
	if len(mock.RecordedChatHeaders) == 0 {
		t.Fatal("no upstream chat recorded")
	}
	// #106: the smoke probe is a chat POST — the model rides in the body,
	// not an x-freebuff-model header.
	if got := mock.RecordedChatHeaders[0].Get("x-freebuff-model"); got != "" {
		t.Errorf("smoke probe chat POST carries x-freebuff-model %q, want absent (#106)", got)
	}
	if len(mock.RecordedChatBodies) == 0 {
		t.Fatal("no upstream chat body recorded")
	}
	if !strings.Contains(mock.RecordedChatBodies[0], `"model":"deepseek/deepseek-v4-flash"`) {
		t.Errorf("smoke probe body missing model deepseek/deepseek-v4-flash: %s", mock.RecordedChatBodies[0])
	}
}

// TestHealthzModeTierCountry verifies healthz surfaces the effective routing
// mode plus the per-token tier/country from the session admission.
func TestHealthzModeTierCountry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.AccessTier = "limited"
	mock.CountryCode = "US"
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Mode   string `json:"mode"`
		Tokens []struct {
			Tier    string `json:"tier"`
			Country string `json:"country"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("healthz is not JSON: %v: %s", err, data)
	}
	if out.Mode != "pooled" {
		t.Errorf("mode = %q, want pooled", out.Mode)
	}
	if len(out.Tokens) != 1 {
		t.Fatalf("tokens = %d, want 1", len(out.Tokens))
	}
	if out.Tokens[0].Tier != "limited" {
		t.Errorf("tier = %q, want limited", out.Tokens[0].Tier)
	}
	if out.Tokens[0].Country != "US" {
		t.Errorf("country = %q, want US", out.Tokens[0].Country)
	}
}

func TestMetricsQuotaLines(t *testing.T) {
	mock := quotaMock(t)
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
		`freebuff_proxy_quota_recent{token="1",model="z-ai/glm-5.2",period="pacific_day"} 4`,
		`freebuff_proxy_quota_limit{token="1",model="z-ai/glm-5.2",period="pacific_day"} 5`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %s in:\n%s", want, body)
		}
	}
}

// TestMetricsLabelEscaping verifies Prometheus label values (model id and
// period, both upstream-derived) are escaped: quotes become \" so the text
// format stays parseable.
func TestMetricsLabelEscaping(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimitsByModel = map[string]any{
		`weird"model`: map[string]any{
			"model":       `weird"model`,
			"limit":       5,
			"recentCount": 4,
			"period":      `p"d`,
			"resetAt":     "2026-08-16T07:00:00.000Z",
		},
	}
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-qe1", 100,
		`"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-qe1", 100,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`))
	ts, _ := newTestServer(t, nil, mock)

	// A chat admits the session (which carries rateLimitsByModel).
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
		`freebuff_proxy_quota_recent{token="1",model="weird\"model",period="p\"d"} 4`,
		`freebuff_proxy_quota_limit{token="1",model="weird\"model",period="p\"d"} 5`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %s in:\n%s", want, body)
		}
	}
}

type quotaEntry struct {
	Limit       float64            `json:"limit"`
	RecentCount float64            `json:"recent_count"`
	Period      string             `json:"period"`
	ResetAt     string             `json:"reset_at"`
	Entitlement map[string]float64 `json:"entitlement"`
}

func TestAdminReload(t *testing.T) {
	// Isolate cwd: handleReload runs config.Load("") which reads ./.env.
	// Without a temp dir the test would silently pick up a developer's .env
	// dropped into internal/server.
	t.Chdir(t.TempDir())
	mock0 := testutil.NewMock()
	defer mock0.Close()
	ts, _ := newTestServer(t, nil, mock0)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/admin/reload", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), `"status":"ok"`) {
		t.Errorf("reload response missing ok status: %s", data)
	}
}

// TestAdminReloadToken verifies ADMIN_TOKEN guards POST /admin/reload: 401
// without the bearer token, 200 with it; unset keeps the legacy open
// behavior.
func TestAdminReloadToken(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServerCfg(t, nil, func(cfg *config.Config) { cfg.AdminToken = "admin-secret" }, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/admin/reload", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reload without token status = %d, want 401: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodPost, ts.URL+"/admin/reload", nil, map[string]string{"Authorization": "Bearer wrong"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reload with wrong token status = %d, want 401: %s", resp.StatusCode, data)
	}

	// The successful reload executes LAST: it swaps s.cfg for a fresh
	// config.Load("") (no ADMIN_TOKEN in the test environment), so nothing
	// after it may rely on the old gate.
	resp, data = doJSON(t, http.MethodPost, ts.URL+"/admin/reload", nil, map[string]string{"Authorization": "Bearer admin-secret"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reload with token status = %d, want 200: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), `"status":"ok"`) {
		t.Errorf("reload response missing ok status: %s", data)
	}

	// Unset: legacy behavior (open), still works.
	mockLegacy := testutil.NewMock()
	defer mockLegacy.Close()
	tsLegacy, _ := newTestServer(t, nil, mockLegacy)
	resp, data = doJSON(t, http.MethodPost, tsLegacy.URL+"/admin/reload", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reload without ADMIN_TOKEN status = %d, want 200 (legacy): %s", resp.StatusCode, data)
	}
}

func TestConcurrentReloadAndChat(t *testing.T) {
	// Isolate cwd: the reload workers run config.Load("") which reads
	// ./.env (see TestAdminReload).
	t.Chdir(t.TempDir())
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-c1", 1, `"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]`))
	ts, _ := newTestServer(t, nil, mock)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	// Hammer chat and models while handleReload swaps s.cfg. The local run
	// exercises the concurrent paths without panicking; the -race build in CI
	// is the real data-race gate.
	worker := func(method, url string, body []byte) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				var reader io.Reader
				if body != nil {
					reader = bytes.NewReader(body)
				}
				req, err := http.NewRequest(method, url, reader)
				if err != nil {
					t.Errorf("worker request build: %v", err)
					return
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Errorf("worker request: %v", err)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}()
	}
	for i := 0; i < 8; i++ {
		worker(http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA))
	}
	for i := 0; i < 4; i++ {
		worker(http.MethodGet, ts.URL+"/v1/models", nil)
	}
	for i := 0; i < 20; i++ {
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/admin/reload", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("reload %d status = %d, want 200: %s", i, resp.StatusCode, data)
		}
	}
	close(stop)
	wg.Wait()
}

func TestAllTokensDead502(t *testing.T) {
	bad0 := testutil.NewMock()
	defer bad0.Close()
	bad0.AuthReject = true
	bad1 := testutil.NewMock()
	defer bad1.Close()
	bad1.AuthReject = true
	ts, _ := newTestServer(t, nil, bad0, bad1)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "upstream_unavailable") {
		t.Errorf("body missing upstream_unavailable: %s", data)
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

// --- bridge mode ---

// newBridgeTestServer wires the server in bridge mode (no AUTH_TOKENS): the
// pool has no fixed tokens and lazily-created per-client-token clients talk
// to the given mock upstream.
func newBridgeTestServer(t *testing.T, mock *testutil.MockUpstream) (*httptest.Server, *pool.Pool) {
	t.Helper()
	cfg := &config.Config{
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg, p, reg, nil, nil, "")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, p
}

func TestBridgeModeRequiresClientToken(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newBridgeTestServer(t, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	// No Authorization header: 401 missing_bearer_token, nothing upstream.
	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "missing_bearer_token") {
		t.Errorf("body missing missing_bearer_token: %s", data)
	}
	if mock.SessionCreates != 0 || len(mock.StartedRuns) != 0 {
		t.Errorf("upstream contact = %d creates / %d runs, want 0/0 (missing token rejected before pool)",
			mock.SessionCreates, len(mock.StartedRuns))
	}

	// An empty Bearer value is also rejected.
	resp, data = doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": "Bearer  "})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("empty bearer status = %d, want 401: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "missing_bearer_token") {
		t.Errorf("empty bearer body missing missing_bearer_token: %s", data)
	}
}

func TestBridgeModeRelaysClientToken(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-b1", 1, `"choices":[{"index":0,"delta":{"content":"bridged"},"finish_reason":null}]`))
	ts, _ := newBridgeTestServer(t, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": "Bearer client-tok-abc"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "bridged") {
		t.Errorf("stream missing content: %s", data)
	}

	// The upstream saw the CLIENT's token, not a proxy-configured one.
	if len(mock.RecordedChatHeaders) != 1 {
		t.Fatalf("upstream chat calls = %d, want 1", len(mock.RecordedChatHeaders))
	}
	if got := mock.RecordedChatHeaders[0].Get("Authorization"); got != "Bearer client-tok-abc" {
		t.Errorf("upstream Authorization = %q, want %q", got, "Bearer client-tok-abc")
	}

	// A second request with the same token reuses the entry: no new run.
	resp2, data2 := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": "Bearer client-tok-abc"})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second request status = %d, want 200: %s", resp2.StatusCode, data2)
	}
	if got := len(mock.StartedRuns); got != 1 {
		t.Errorf("started runs = %d, want 1 (entry reused across requests)", got)
	}
}

func TestBridgeModeXAPIKey(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-b2", 2, `"choices":[{"index":0,"delta":{"content":"keyed"},"finish_reason":null}]`))
	ts, _ := newBridgeTestServer(t, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), map[string]string{"x-api-key": "client-tok-xyz"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "keyed") {
		t.Errorf("stream missing content: %s", data)
	}
	if got := mock.RecordedChatHeaders[0].Get("Authorization"); got != "Bearer client-tok-xyz" {
		t.Errorf("upstream Authorization = %q, want %q (x-api-key relayed as bearer)", got, "Bearer client-tok-xyz")
	}
}

func TestBridgeModeModelsAndHealthz(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newBridgeTestServer(t, mock)

	// /v1/models and /healthz need no header in bridge mode.
	resp, data := doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200: %s", resp.StatusCode, data)
	}
	resp, data = doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		BridgeTokens int `json:"bridge_tokens"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("healthz is not JSON: %v: %s", err, data)
	}
	if out.BridgeTokens != 0 {
		t.Errorf("bridge_tokens = %d, want 0 (no chat requests yet)", out.BridgeTokens)
	}

	// After a bridged chat the healthz counter reflects the cached entry.
	doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), map[string]string{"Authorization": "Bearer client-tok-h"})
	resp, data = doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("healthz is not JSON: %v: %s", err, data)
	}
	if out.BridgeTokens != 1 {
		t.Errorf("bridge_tokens = %d, want 1 after a chat", out.BridgeTokens)
	}
}

func TestBridgeModeChat401Cooldown(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = 401
	mock.ChatErrorBody = `{"error":{"message":"unauthorized","type":"authentication_error"}}`
	ts, _ := newBridgeTestServer(t, mock)
	chatURL := ts.URL + "/v1/chat/completions"
	hdr := map[string]string{"Authorization": "Bearer client-tok-401"}

	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), hdr)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "upstream_auth_rejected") {
		t.Errorf("body missing upstream_auth_rejected: %s", data)
	}

	// The entry's token went on cooldown; the next request surfaces the
	// cooldown without re-hitting upstream.
	mock.ChatStatus = 200
	resp2, data2 := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), hdr)
	if resp2.StatusCode != http.StatusBadGateway {
		t.Fatalf("second request status = %d, want 502 (cooldown): %s", resp2.StatusCode, data2)
	}
	if !strings.Contains(string(data2), "cooling down") {
		t.Errorf("second request body = %q, want cooldown error", data2)
	}
	if got := len(mock.RecordedChatHeaders); got != 1 {
		t.Errorf("upstream chat calls = %d, want 1 (cooldown skipped upstream)", got)
	}
}

// TestHybridModeChatRouting verifies hybrid mode routes per request: a
// client-supplied token is relayed upstream like bridge, and a token-less
// request falls back to the pooled AUTH_TOKENS.
func TestHybridModeChatRouting(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-h1", 1, `"choices":[{"index":0,"delta":{"content":"hybrid"},"finish_reason":null}]`))
	ts, p := newTestServerCfg(t, nil, func(c *config.Config) {
		c.HybridMode = true
		c.UpstreamBaseURL = mock.URL() // bridge leases build clients from the pool cfg
	}, mock)
	_ = p
	chatURL := ts.URL + "/v1/chat/completions"

	// Client token present → bridge relay: the upstream sees the client token.
	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": "Bearer client-hybrid-1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token request status = %d, want 200: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "hybrid") {
		t.Errorf("stream missing content: %s", data)
	}

	// Token-less → pooled: the upstream sees the configured pool token.
	resp, data = doJSON(t, http.MethodPost, chatURL, chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token-less request status = %d, want 200: %s", resp.StatusCode, data)
	}

	if len(mock.RecordedChatHeaders) != 2 {
		t.Fatalf("upstream chat calls = %d, want 2", len(mock.RecordedChatHeaders))
	}
	if got := mock.RecordedChatHeaders[0].Get("Authorization"); got != "Bearer client-hybrid-1" {
		t.Errorf("token request upstream Authorization = %q, want %q", got, "Bearer client-hybrid-1")
	}
	if got := mock.RecordedChatHeaders[1].Get("Authorization"); got != "Bearer tok-0" {
		t.Errorf("token-less request upstream Authorization = %q, want %q (pooled)", got, "Bearer tok-0")
	}

	// healthz reports the hybrid mode.
	resp, data = doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("healthz is not JSON: %v: %s", err, data)
	}
	if out.Mode != "hybrid" {
		t.Errorf("mode = %q, want hybrid", out.Mode)
	}
}

// TestHybridModeNoTokensRequiresPooledFallback verifies hybrid without any
// AUTH_TOKENS: a client token still relays (bridge path), while a token-less
// request fails with the pooled no-tokens error instead of the bridge 401 —
// the mode-switch warning ("token-less requests will 502 until a token is
// added") is what the operator sees.
func TestHybridModeNoTokensRequiresPooledFallback(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-h2", 2, `"choices":[{"index":0,"delta":{"content":"relayed"},"finish_reason":null}]`))
	// Mirrors newBridgeTestServer (no AUTH_TOKENS) plus HYBRID_MODE.
	cfg := &config.Config{
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
		HybridMode:         true,
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg, p, reg, nil, nil, "")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Client token relays fine.
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), map[string]string{"Authorization": "Bearer client-hybrid-2"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token request status = %d, want 200: %s", resp.StatusCode, data)
	}
	// Token-less request fails as pooled-no-tokens (not the bridge 401).
	resp, data = doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("token-less hybrid status = 401, want a pooled no-tokens failure (not bridge 401): %s", data)
	}
	if strings.Contains(string(data), "missing_bearer_token") {
		t.Errorf("token-less hybrid body = %s, must not claim bridge mode", data)
	}
}

// TestHybridModeXAPIKeyStaysPooled verifies hybrid routing never relays an
// x-api-key upstream: x-api-key is the API_KEYS scheme for pooled clients,
// and a Bearer token is the only bridge discriminator. A pooled client using
// x-api-key must hit the pool (and fail API_KEYS auth when the key is not
// configured) instead of having its key sent to the upstream service.
func TestHybridModeXAPIKeyStaysPooled(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-h3", 3, `"choices":[{"index":0,"delta":{"content":"pooled"},"finish_reason":null}]`))
	// API_KEYS configured: pooled requests must authenticate with them.
	ts, _ := newTestServerCfg(t, []string{"sk-pooled"}, func(c *config.Config) {
		c.HybridMode = true
		c.UpstreamBaseURL = mock.URL()
	}, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	// x-api-key with the configured pool key -> pooled path, 200.
	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"x-api-key": "sk-pooled"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("x-api-key pooled status = %d, want 200: %s", resp.StatusCode, data)
	}
	// x-api-key without a valid pool key -> 401 (API_KEYS gate), and the
	// key must never reach the upstream as a bridge relay.
	resp, data = doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"x-api-key": "sk-any"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("x-api-key invalid status = %d, want 401: %s", resp.StatusCode, data)
	}
	// Bearer still relays as bridge.
	resp, data = doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": "Bearer client-hybrid-3"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bearer status = %d, want 200: %s", resp.StatusCode, data)
	}
	if len(mock.RecordedChatHeaders) != 2 {
		t.Fatalf("upstream chat calls = %d, want 2 (pooled + bearer bridge)", len(mock.RecordedChatHeaders))
	}
	if got := mock.RecordedChatHeaders[0].Get("Authorization"); got != "Bearer tok-0" {
		t.Errorf("x-api-key pooled upstream Authorization = %q, want %q", got, "Bearer tok-0")
	}
	for i, h := range mock.RecordedChatHeaders {
		if v := h.Get("x-api-key"); v == "sk-pooled" || v == "sk-any" {
			t.Errorf("upstream call %d carried an operator x-api-key %q upstream", i, v)
		}
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

// TestBridgeModeHealthzReportsMode pins the healthz "mode" field in pure
// bridge mode.
func TestBridgeModeHealthzReportsMode(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newBridgeTestServer(t, mock)
	resp, data := doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("healthz is not JSON: %v: %s", err, data)
	}
	if out.Mode != "bridge" {
		t.Errorf("mode = %q, want bridge", out.Mode)
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

// flakyFirstRT fails the very first request with a transient transport error
// and delegates everything else to base (mirrors pool_test's helper; drives a
// real retry deterministically across platforms).
type flakyFirstRT struct {
	mu     sync.Mutex
	failed bool
	base   http.RoundTripper
}

func (f *flakyFirstRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	shouldFail := !f.failed
	if shouldFail {
		f.failed = true
	}
	f.mu.Unlock()
	if shouldFail {
		return nil, fmt.Errorf("read tcp 127.0.0.1:443: connection reset by peer")
	}
	return f.base.RoundTrip(req)
}

func TestMetricsTransientRetryCounters(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-r", 1, `"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]`)) + "data: [DONE]\n\n"

	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
		TransientRetries:   1,
	}
	client, err := upstream.New("tok-0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The first upstream call (agent-runs START during lease acquisition)
	// fails once at the transport level; TRANSIENT_RETRIES replays it.
	client.SetTransport(&flakyFirstRT{base: http.DefaultTransport})

	sess := session.NewManager(client)
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, []*session.Manager{sess}, reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg, p, reg, nil, nil, "")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200: %s", resp.StatusCode, data)
	}
	body := string(data)
	if !strings.Contains(body, `freebuff_proxy_transient_retries_total{token="1"} 1`) {
		t.Errorf("metrics missing transient retry line: %s", body)
	}
	// No TLS fingerprint is pinned in this setup, so no rotation happened
	// and the fingerprint value line must not be emitted (only when > 0).
	if strings.Contains(body, "freebuff_proxy_fingerprint_rotations_total{token=\"1\"}") {
		t.Errorf("metrics emitted a fingerprint rotation value with no rotation: %s", body)
	}
}

// RequestsServed counts successful upstream chats in bridge mode too (the
// metrics page reads it, so it must not stay 0 when no AUTH_TOKENS exist).
func TestBridgeRequestsServedCounter(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-b2", 1, `"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]`))
	ts, p := newBridgeTestServer(t, mock)
	chatURL := ts.URL + "/v1/chat/completions"
	for range 3 {
		resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": "Bearer client-tok"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d: %s", resp.StatusCode, data)
		}
	}
	if got := p.PoolSnapshot().RequestsServed; got != 3 {
		t.Fatalf("RequestsServed = %d, want 3 (bridge chats must count)", got)
	}
}

// TestBearerCaseInsensitiveVariants verifies lowercase bearer and mixed-case BEARER
// work for API authentication, admin endpoints, and bridge token extraction.
func TestBearerCaseInsensitiveVariants(t *testing.T) {
	t.Run("API auth accepts case variations", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		ts, _ := newTestServer(t, []string{"sk-test"}, mock)
		chatURL := ts.URL + "/v1/chat/completions"

		for _, auth := range []string{
			"Bearer sk-test",
			"bearer sk-test",
			"BEARER sk-test",
			"bEaReR sk-test",
		} {
			resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": auth})
			if resp.StatusCode != http.StatusOK {
				t.Errorf("auth %q status = %d, want 200: %s", auth, resp.StatusCode, data)
			}
		}
	})

	t.Run("admin reload accepts case variations", func(t *testing.T) {
		for _, auth := range []string{
			"bearer admin-secret",
			"BEARER admin-secret",
		} {
			mock := testutil.NewMock()
			ts, _ := newTestServerCfg(t, nil, func(cfg *config.Config) { cfg.AdminToken = "admin-secret" }, mock)
			resp, data := doJSON(t, http.MethodPost, ts.URL+"/admin/reload", nil, map[string]string{"Authorization": auth})
			mock.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("admin auth %q status = %d, want 200: %s", auth, resp.StatusCode, data)
			}
		}
	})

	t.Run("bridge mode token extraction accepts case variations", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-b3", 1, `"choices":[{"index":0,"delta":{"content":"bridged"},"finish_reason":null}]`))
		ts, _ := newBridgeTestServer(t, mock)
		chatURL := ts.URL + "/v1/chat/completions"

		for _, auth := range []string{
			"bearer client-tok-lower",
			"BEARER client-tok-upper",
		} {
			resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": auth})
			if resp.StatusCode != http.StatusOK {
				t.Errorf("bridge auth %q status = %d, want 200: %s", auth, resp.StatusCode, data)
			}
		}
	})
}

// --- Issue #74 P2: per-(egress, model) unfit registry ---

// limitedChatBody renders the upstream 409 body classifyError maps to
// upstream.LimitedIpError (session_model_mismatch + "limited" marker).
func limitedChatBody() string {
	return `{"status":"session_model_mismatch","message":"model ` + modelA + ` is limited on this IP"}`
}

// errorCode extracts the OpenAI error.code from a response body.
func errorCode(t *testing.T, data []byte) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("body is not the OpenAI error shape: %v: %s", err, data)
	}
	return body.Error.Code
}

// writeRawJSON writes a pre-formatted JSON body (the mock's writeJSON helper
// is unexported; scripted ChatHandler/SessionHandler tests write raw bodies).
func writeRawJSON(w http.ResponseWriter, status int, raw string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, raw)
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

// TestBridgeModelUnfitNotGated pins the bridge exemption: bridge clients
// relay their own token (their account may serve the model on this egress
// and their session slots are theirs to spend), so the registry never gates
// them even when (egress, model) is marked unfit.
func TestBridgeModelUnfitNotGated(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-bg1", 1, `"choices":[{"index":0,"delta":{"content":"bridged"},"finish_reason":null}]`))
	ts, p := newBridgeTestServer(t, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	p.MarkModelUnfit(modelA, &upstream.LimitedIpError{Body: "pre-marked unfit"})
	if until, _ := p.ModelUnfit(modelA); until.IsZero() {
		t.Fatal("pre-mark not set")
	}

	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": "Bearer client-tok-abc"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bridge status = %d, want 200 (bridge never gated): %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "bridged") {
		t.Errorf("stream missing bridged content: %s", data)
	}
	if got := len(mock.RecordedChatHeaders); got != 1 {
		t.Errorf("upstream chat calls = %d, want 1 (bridge ignored the unfit mark)", got)
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

// TestChatSpendLedgerIgnoresUsageNull pins the relayStream usage guard: a
// trailing chunk carrying "usage":null must not zero the spend ledger — only
// chunks that actually carry a usage block may update it.
func TestChatSpendLedgerIgnoresUsageNull(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-u1", 1, `"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-u1", 1, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}`)) +
		testutil.SSEEvent(chunk("chatcmpl-u1", 1, `"choices":[{"index":0,"delta":{},"finish_reason":null}],"usage":null`))
	ts, pool := newTestServer(t, nil, mock)
	req := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(req), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200: %s", resp.StatusCode, data)
	}
	snaps := pool.Snapshot()
	if len(snaps) != 1 {
		t.Fatalf("pool tokens = %d, want 1", len(snaps))
	}
	if snaps[0].SpendDay != 13 || snaps[0].Spend24h != 13 {
		t.Errorf("spend after usage-null trailing chunk = %d/%d, want 13/13 (not zeroed)", snaps[0].SpendDay, snaps[0].Spend24h)
	}
}

// TestAdminSensitiveOpenMode verifies that in open mode (ADMIN_TOKEN unset / optional),
// admin routes are accessible without 403.
func TestAdminSensitiveOpenMode(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/config", nil)
	req.Host = "127.0.0.1:3457"
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
