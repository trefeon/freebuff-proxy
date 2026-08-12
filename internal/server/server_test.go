package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	t.Helper()
	cfg := &config.Config{
		AuthTokens:         make([]string, len(mocks)),
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
		APIKeys:            apiKeys,
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
	srv := server.New(cfg, p, reg, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, p
}

func chatBody(model string) []byte {
	return []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"ping"}],"stream":true}`)
}

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
	resp, err := http.DefaultClient.Do(req)
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
	if got := h.Get("x-freebuff-model"); got != modelA {
		t.Errorf("x-freebuff-model = %q, want %q", got, modelA)
	}
	if got := h.Get("x-freebuff-instance-id"); got != "inst-abc-123" {
		t.Errorf("x-freebuff-instance-id = %q, want inst-abc-123", got)
	}
	recorded := mock.RecordedChatBodies[0]
	for _, want := range []string{`"codebuff_metadata"`, `"data_collection":"deny"`, `"stream":true`, `"stop":["cb_easp"]`, `"run_id":"run-0001"`} {
		if !strings.Contains(recorded, want) {
			t.Errorf("upstream body missing %s: %s", want, recorded)
		}
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

func TestWaitingRoom503ThenRetry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionSequence = []string{"queued", "active"}
	mock.QueuePosition = 3
	mock.QueueDepth = 7
	mock.EstimatedWaitMs = 2000
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("first request status = %d, want 503: %s", resp.StatusCode, data)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "2" {
		t.Errorf("Retry-After = %q, want 2 (ceil of ~2s)", ra)
	}
	if !strings.Contains(string(data), "waiting_room_queued") {
		t.Errorf("body missing waiting_room_queued: %s", data)
	}

	// Wait out the queue window, then the session must advance to active.
	time.Sleep(2200 * time.Millisecond)

	resp2, data2 := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("retry status = %d, want 200: %s", resp2.StatusCode, data2)
	}
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
	if got := len(mock.StartedRuns); got != 2 {
		t.Errorf("started runs = %d, want 2 (re-START after run-invalid)", got)
	}
	if got := len(mock.FinishedRuns); got != 0 {
		t.Errorf("finished runs = %d, want 0 (invalidated run is not FINISHed)", got)
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
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, data)
	}
	if out.Object != "list" {
		t.Errorf("object = %q, want list", out.Object)
	}
	if len(out.Data) < 15 {
		t.Errorf("models = %d, want >= 15", len(out.Data))
	}
	for i, m := range out.Data {
		if m.ID == "" || m.Object != "model" || m.OwnedBy == "" {
			t.Errorf("model %d malformed: %+v", i, m)
		}
		if m.Created != out.Data[0].Created {
			t.Errorf("model %d created = %d, want %d (pinned to server start)", i, m.Created, out.Data[0].Created)
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
	if out.Models < 15 {
		t.Errorf("models = %d, want >= 15", out.Models)
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

func TestAdminReload(t *testing.T) {
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
	srv := server.New(cfg, p, reg, nil)
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
