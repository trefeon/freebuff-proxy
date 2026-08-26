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
const modelA = "deepseek/deepseek-v4-flash"

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
		DashboardEnabled:   true,
		AdminToken:         config.DefaultAdminToken,
		QuotaFallbackModels: map[string]string{
			"deepseek/deepseek-v4-flash": "mimo/mimo-v2.5",
			"z-ai/glm-5.2":               "deepseek/deepseek-v4-flash",
		},
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

// waitSpendTimeout bounds the spend-ledger polling below.
const waitSpendTimeout = 2 * time.Second

// waitSpend polls the pool snapshot until the first token's spend reaches
// want (both Pacific-day and rolling-24h buckets), or fails. The spend
// feeder runs after the relay writes the response (chatCore: relay →
// RecordSpend), so a client that has already read the full response can
// snapshot the pool before the ledger lands; polling makes the assertion
// robust to scheduler preemption instead of depending on write ordering.
func waitSpend(t *testing.T, pool *pool.Pool, want int64) []pool.TokenSnapshot {
	t.Helper()
	deadline := time.Now().Add(waitSpendTimeout)
	for {
		snaps := pool.Snapshot()
		if len(snaps) > 0 && snaps[0].SpendDay == want && snaps[0].Spend24h == want {
			return snaps
		}
		if time.Now().After(deadline) {
			t.Fatalf("spend did not reach %d/%d within %s (last: %v)", want, want, waitSpendTimeout, snaps)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// quotaMock wires a mock whose admission response carries rateLimitsByModel
// for z-ai/glm-5.2, so a chat drives session creation with quota data.
func quotaMock(t *testing.T) *testutil.MockUpstream {
	t.Helper()
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	mock.RateLimitsByModel = map[string]any{
		modelA: map[string]any{
			"model":       modelA,
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
	for _, want := range []string{`"codebuff_metadata"`, `"data_collection":"deny"`, `"stream":true`, `"stop":["\"cb_easp\""]`, `"run_id":"run-0001"`} {
		if !strings.Contains(recorded, want) {
			t.Errorf("upstream body missing %s: %s", want, recorded)
		}
	}
	// #80+#103: trace_session_id and client_id are BOTH minted once per run
	// and threaded through the envelope (a per-call client_id draw is the
	// free_mode_run_fanout shape — see TestChatClientIDStableAcrossRun).
	// client_id keeps the SDK-faithful 13-char base36 form, never the
	// sess:/run:-prefixed shapes the server fingerprints as a proxy.
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

	snaps := waitSpend(t, pool, 13)
	if len(snaps) != 1 {
		t.Fatalf("pool tokens = %d, want 1", len(snaps))
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
	waitSpend(t, pool, 25)
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
	snaps := waitSpend(t, pool, 13)
	if len(snaps) != 1 {
		t.Fatalf("pool tokens = %d, want 1", len(snaps))
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

// TestClientCancelMidRelayAbandonsRun pins issue #157: when the client
// disconnects mid-relay (stream already running), the proxy must release
// the upstream slot and FINISH the run as "cancelled" PROMPTLY — without
// waiting for upstream EOF. The mock holds the upstream stream open with no
// further data, so the FINISH can only arrive via the abandon path
// (LeaseAbandon → ReleaseAbandoned), never from a natural stream end.
func TestClientCancelMidRelayAbandonsRun(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	chatSeen := make(chan struct{})
	firstChunk := make(chan struct{})
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-chatSeen:
		default:
			close(chatSeen)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-cancel", 100,
			`"choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]`)))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(firstChunk)
		// Hold the stream open with NO further data and NO EOF until the
		// request context dies.
		select {
		case <-r.Context().Done():
			mock.AbortDetected.Store(true)
		case <-time.After(30 * time.Second):
		}
	}
	ts, _ := newTestServer(t, nil, mock)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/chat/completions", bytes.NewReader(chatBody(modelA)))
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		errCh <- err
	}()

	select {
	case <-chatSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never saw the chat request")
	}
	select {
	case <-firstChunk:
	case <-time.After(5 * time.Second):
		t.Fatal("first chunk never relayed (relay not mid-stream)")
	}

	// Client cancel mid-relay: the slot must be released and the run
	// FINISHed as cancelled WITHOUT waiting for upstream EOF (the mock
	// never sends one).
	cancel()
	eventually(t, "run FINISHed cancelled on mid-relay client cancel", func() bool {
		for _, f := range mock.FinishedRunsSnapshot() {
			if f.Status == "cancelled" {
				return true
			}
		}
		return false
	})
	// The proxy must have aborted the held-open upstream stream (context
	// propagation to the wire), not merely stopped relaying.
	eventually(t, "upstream abort detection", func() bool {
		return mock.AbortDetected.Load()
	})
	<-errCh
}

// TestClientCancelBeforeFirstByteAbandonsRun pins issue #157's observed
// failure mode: the client cancels while the upstream request is still in
// flight (long reasoning before the first byte — the ~59s harness-timeout
// pattern). The lease must be ABANDONED, not plain-released: the run
// FINISHes as "cancelled" promptly instead of lingering active (and
// upstream-alive) until the 6h rotation.
func TestClientCancelBeforeFirstByteAbandonsRun(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	chatSeen := make(chan struct{})
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-chatSeen:
		default:
			close(chatSeen)
		}
		// Hold the request open before writing any response headers: the
		// upstream call is still in flight when the client cancels.
		select {
		case <-r.Context().Done():
			mock.AbortDetected.Store(true)
		case <-time.After(30 * time.Second):
		}
	}
	ts, _ := newTestServer(t, nil, mock)

	ctx, cancel := context.WithCancel(context.Background())
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
	eventually(t, "run FINISHed cancelled on pre-first-byte client cancel", func() bool {
		for _, f := range mock.FinishedRunsSnapshot() {
			if f.Status == "cancelled" {
				return true
			}
		}
		return false
	})
	// The upstream HTTP request must have been canceled on the wire (the
	// request context propagation), not left running until the mock's
	// 30s fallback.
	eventually(t, "upstream abort detection", func() bool {
		return mock.AbortDetected.Load()
	})
	<-errCh
}

// TestChatClientIDStableAcrossRun is the wiring guard for the
// free_mode_run_fanout fix: two chat requests served by the same lease/run
// must carry the SAME codebuff_metadata.client_id, because the CLI mints it
// once per prompt and repeats it on every LLM step (run.ts:722/822,
// llm.ts:117). A fresh draw per call made one run_id fan out across N client
// ids, which upstream refuses.
func TestChatClientIDStableAcrossRun(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	body := []byte(`{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	for i := 0; i < 2; i++ {
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", body, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200: %s", i+1, resp.StatusCode, data)
		}
	}

	recorded := mock.RecordedChatBodies
	if len(recorded) < 2 {
		t.Fatalf("recorded chat bodies = %d, want 2", len(recorded))
	}
	meta := func(raw string) (clientID, runID string) {
		var sent struct {
			Metadata struct {
				ClientID string `json:"client_id"`
				RunID    string `json:"run_id"`
			} `json:"codebuff_metadata"`
		}
		if err := json.Unmarshal([]byte(raw), &sent); err != nil {
			t.Fatalf("body not JSON: %v", err)
		}
		return sent.Metadata.ClientID, sent.Metadata.RunID
	}
	id1, run1 := meta(recorded[0])
	id2, run2 := meta(recorded[1])
	if run1 != run2 {
		t.Fatalf("run_id = %q then %q; this test needs both calls on one run", run1, run2)
	}
	if !regexp.MustCompile(`^[a-z0-9]{13}$`).MatchString(id1) {
		t.Errorf("client_id = %q, want 13-char base36", id1)
	}
	if id1 != id2 {
		t.Errorf("client_id = %q then %q on run %q, want one id per run (fanout shape otherwise)", id1, id2, run1)
	}
}
