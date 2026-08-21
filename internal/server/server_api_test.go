package server_test

// Endpoint-surface tests for the wave-4 API additions: the Responses API
// (/v1/responses, #92), the Anthropic Messages API (/v1/messages, #57),
// the structured embeddings rejection (#47), CORS + OPTIONS preflight
// (#78), and the SSE keepalive/grace-flush/late-failure relay (#83/#59).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/logring"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/server"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

// newTestServerWithLogger builds the full stack like newTestServer but with
// a custom logger (logring-wrapped) so tests can assert on trace entries.
func newTestServerWithLogger(t *testing.T, apiKeys []string, logger *slog.Logger, ring *logring.Handler, mocks ...*testutil.MockUpstream) (*httptest.Server, *pool.Pool) {
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
	srv := server.New(cfg, p, reg, logger, ring, "")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, p
}

// --- #78 CORS + OPTIONS preflight ---

func TestCORSPreflight204(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodOptions, ts.URL+"/v1/chat/completions", nil,
		map[string]string{"Origin": "https://app.example.com"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("OPTIONS status = %d, want 204: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
	headers := resp.Header.Get("Access-Control-Allow-Headers")
	for _, want := range []string{"Content-Type", "Authorization", "x-api-key", "anthropic-version"} {
		if !strings.Contains(headers, want) {
			t.Errorf("Access-Control-Allow-Headers missing %q: %q", want, headers)
		}
	}
	for _, want := range []string{"POST", "GET", "OPTIONS"} {
		if !strings.Contains(resp.Header.Get("Access-Control-Allow-Methods"), want) {
			t.Errorf("Access-Control-Allow-Methods missing %q", want)
		}
	}
	if mock.Requests != 0 {
		t.Errorf("upstream requests = %d, want 0 (preflight answered before routing)", mock.Requests)
	}
}

func TestCORSConfiguredOrigin(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.CORSAllowedOrigin = "https://app.example.com"
	}, mock)

	// Preflight echoes the pinned origin and varies on it.
	resp, _ := doJSON(t, http.MethodOptions, ts.URL+"/v1/models", nil,
		map[string]string{"Origin": "https://other.example.com"})
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("Access-Control-Allow-Origin = %q, want configured origin", got)
	}
	if !strings.Contains(resp.Header.Get("Vary"), "Origin") {
		t.Errorf("Vary missing Origin: %q", resp.Header.Get("Vary"))
	}

	// A real /v1/* response carries the CORS headers too.
	resp2, data := doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d: %s", resp2.StatusCode, truncate(string(data), 200))
	}
	if got := resp2.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("GET Access-Control-Allow-Origin = %q, want configured origin", got)
	}
}

func TestCORSPreflightNotAppliedToAdmin(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	// Admin routes are intentionally outside the CORS surface (cookie
	// auth); an OPTIONS there must not gain allow headers.
	resp, _ := doJSON(t, http.MethodOptions, ts.URL+"/admin/login", nil,
		map[string]string{"Origin": "https://evil.example.com"})
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("admin OPTIONS got Access-Control-Allow-Origin %q, want none", got)
	}
	if resp.StatusCode == http.StatusNoContent {
		t.Error("admin OPTIONS answered 204; want the mux's 405/404 (CORS must not intercept admin)")
	}
}

// --- #47 embeddings rejection ---

func TestEmbeddingsUnsupported(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/embeddings",
		[]byte(`{"input":"hello","model":"text-embedding-3"}`), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, truncate(string(data), 200))
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("error body not JSON: %v", err)
	}
	if body.Error.Type != "unsupported_endpoint" || body.Error.Code != "unsupported_endpoint" {
		t.Errorf("error type/code = %q/%q, want unsupported_endpoint", body.Error.Type, body.Error.Code)
	}
	if !strings.Contains(body.Error.Message, "/v1/chat/completions") {
		t.Errorf("message missing chat-completions pointer: %q", body.Error.Message)
	}
	if !strings.Contains(body.Error.Message, modelA) {
		t.Errorf("message missing model list (want %q): %q", modelA, body.Error.Message)
	}
	if mock.Requests != 0 {
		t.Errorf("upstream requests = %d, want 0 (rejected before pool)", mock.Requests)
	}
}

// --- #92 Responses API ---

// responsesChunks renders a text-only upstream chat stream ending with a
// usage block (so relayers surface upstream token accounting).
func responsesChunks() string {
	chunks := []string{
		chunk("chatcmpl-r1", 100, `"choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]`),
		chunk("chatcmpl-r1", 100, `"choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]`),
		chunk("chatcmpl-r1", 100, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}`),
	}
	var sb strings.Builder
	for _, c := range chunks {
		sb.WriteString(testutil.SSEEvent(c))
	}
	return sb.String()
}

func TestResponsesNonStream(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","instructions":"be brief","input":"ping"}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var out struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Model  string `json:"model"`
		Output []struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Status  string `json:"status"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if out.Object != "response" || out.Status != "completed" {
		t.Errorf("object/status = %q/%q, want response/completed", out.Object, out.Status)
	}
	if len(out.Output) != 1 || out.Output[0].Type != "message" {
		t.Fatalf("output = %+v, want one message item", out.Output)
	}
	msg := out.Output[0]
	if msg.Role != "assistant" || msg.Status != "completed" {
		t.Errorf("message role/status = %q/%q, want assistant/completed", msg.Role, msg.Status)
	}
	if len(msg.Content) != 1 || msg.Content[0].Text != "Hello world" {
		t.Errorf("content = %+v, want single output_text 'Hello world'", msg.Content)
	}
	if !strings.HasPrefix(msg.ID, "msg_") {
		t.Errorf("message id = %q, want msg_ prefix", msg.ID)
	}
	if out.Usage.OutputTokens == 0 {
		t.Error("usage.output_tokens = 0, want upstream usage surfaced")
	}
	if !mock.BodyContains(`"role":"system"`) {
		t.Error("upstream chat body missing the instructions system message")
	}
	if !mock.BodyContains("be brief") || !mock.BodyContains("ping") {
		t.Error("upstream chat body missing instructions/input content")
	}
}

func TestResponsesStringInput(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","input":"hello"}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if !mock.BodyContains(`"role":"user"`) || !mock.BodyContains("hello") {
		t.Error("string input not converted to a user message")
	}
}

func TestResponsesToolCallNonStream(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-r2", 101,
		`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-r2", 101, `"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`))
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","input":"weather?","tools":[{"type":"function","name":"get_weather","description":"weather lookup","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}],"tool_choice":{"type":"function","name":"get_weather"}}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var out struct {
		Status string `json:"status"`
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if len(out.Output) != 1 || out.Output[0].Type != "function_call" {
		t.Fatalf("output = %+v, want one function_call", out.Output)
	}
	fc := out.Output[0]
	if fc.Name != "get_weather" || !strings.Contains(fc.Arguments, `"city":"SF"`) {
		t.Errorf("function_call name/arguments = %q/%q, want get_weather with city SF", fc.Name, fc.Arguments)
	}
	if fc.CallID != "call_abc" {
		t.Errorf("call_id = %q, want upstream call_abc", fc.CallID)
	}
	// The upstream body must carry the wrapped function tool + translated
	// tool_choice.
	if !mock.BodyContains(`"type":"function"`) || !mock.BodyContains(`"get_weather"`) {
		t.Error("upstream body missing function-wrapped tool")
	}
	if !mock.BodyContains(`"tool_choice"`) || !mock.BodyContains(`"name":"get_weather"`) {
		t.Error("upstream body missing translated tool_choice")
	}
}

func TestResponsesStream(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","input":"ping","stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	sse := string(data)
	if !strings.Contains(sse, ": connecting") {
		t.Error("stream missing ': connecting' grace-flush comment")
	}
	for _, want := range []string{
		`"type":"response.created"`,
		`"type":"response.in_progress"`,
		`"type":"response.output_item.added"`,
		`"type":"response.content_part.added"`,
		`"type":"response.output_text.delta"`,
		`"delta":"Hello"`,
		`"type":"response.output_text.done"`,
		`"type":"response.content_part.done"`,
		`"type":"response.output_item.done"`,
		`"type":"response.completed"`,
		`"status":"completed"`,
	} {
		if !strings.Contains(sse, want) {
			t.Errorf("stream missing %s", want)
		}
	}
	// Terminal: response.completed carries the assembled text and usage.
	if !strings.Contains(sse, `"text":"Hello world"`) {
		t.Error("response.completed missing assembled output text")
	}
}

func TestResponsesStreamToolCall(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-r3", 102,
		`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"search_web","arguments":"{\"q\":\"freebuff\"}"}}]},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-r3", 102, `"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`))
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","input":"search","tools":[{"type":"function","name":"search_web","description":"s","parameters":{"type":"object","properties":{}}}],"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	sse := string(data)
	for _, want := range []string{
		`"type":"response.output_item.added"`,
		`"type":"function_call"`,
		`"type":"response.function_call_arguments.delta"`,
		`"delta":"{\"q\":\"freebuff\"}"`,
		`"type":"response.output_item.done"`,
		`"type":"response.completed"`,
	} {
		if !strings.Contains(sse, want) {
			t.Errorf("stream missing %s", want)
		}
	}
	if !strings.Contains(sse, `"name":"search_web"`) {
		t.Error("completed function_call missing the tool name")
	}
}

// TestResponsesStreamXMLToolCallInContent verifies that XML tool calls
// emitted inline in delta.content (MiMo/Hermes/Qwen style, split across
// fragments) are extracted into native function_call output items instead
// of leaking as text.
func TestResponsesStreamXMLToolCallInContent(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-r4", 103,
		`"choices":[{"index":0,"delta":{"content":"Let me check.\n<tool_call>"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-r4", 103,
			`"choices":[{"index":0,"delta":{"content":"<function=bash><parameter=command>pwd</parameter></function>"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-r4", 103,
			`"choices":[{"index":0,"delta":{"content":"</tool_call>"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-r4", 103, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`))
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","input":"run pwd","stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	sse := string(data)
	for _, want := range []string{
		`"type":"response.output_item.added"`,
		`"type":"function_call"`,
		`"type":"response.function_call_arguments.delta"`,
		`"delta":"{\"command\":\"pwd\"}"`,
		`"type":"response.output_item.done"`,
		`"type":"response.completed"`,
		`"name":"bash"`,
		`"arguments":"{\"command\":\"pwd\"}"`,
	} {
		if !strings.Contains(sse, want) {
			t.Errorf("stream missing %s", want)
		}
	}
	// Plain text before the XML block survives as an output text delta...
	if !strings.Contains(sse, `"delta":"Let me check.\n"`) {
		t.Error("stream missing leading plain-text delta before the XML block")
	}
	// ...and the raw XML must never reach the client.
	if strings.Contains(sse, "<tool_call>") {
		t.Error("stream leaked raw <tool_call> XML into output")
	}
}

// --- #57 Anthropic Messages API ---

// anthropicTextChunks renders an upstream chat stream with reasoning +
// text + stop + a usage block.
func anthropicTextChunks() string {
	chunks := []string{
		chunk("chatcmpl-a1", 200, `"choices":[{"index":0,"delta":{"reasoning_content":"think step by step"},"finish_reason":null}]`),
		chunk("chatcmpl-a1", 200, `"choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]`),
		chunk("chatcmpl-a1", 200, `"choices":[{"index":0,"delta":{"content":" there"},"finish_reason":null}]`),
		chunk("chatcmpl-a1", 200, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":21,"completion_tokens":4,"total_tokens":25}`),
	}
	var sb strings.Builder
	for _, c := range chunks {
		sb.WriteString(testutil.SSEEvent(c))
	}
	return sb.String()
}

func TestMessagesNonStream(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = anthropicTextChunks()
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","max_tokens":200,"system":"You are helpful.","messages":[{"role":"user","content":"hi"}],"stream":false}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body),
		map[string]string{"x-api-key": "k", "anthropic-version": "2023-06-01"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var msg struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Role       string `json:"role"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content"`
		Usage map[string]int64 `json:"usage"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("message not JSON: %v", err)
	}
	if msg.Type != "message" || msg.Role != "assistant" {
		t.Errorf("type/role = %q/%q, want message/assistant", msg.Type, msg.Role)
	}
	if msg.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", msg.StopReason)
	}
	if len(msg.Content) != 2 || msg.Content[0].Type != "thinking" || msg.Content[1].Type != "text" {
		t.Fatalf("content = %+v, want thinking then text blocks", msg.Content)
	}
	if msg.Content[0].Thinking != "think step by step" {
		t.Errorf("thinking block = %q", msg.Content[0].Thinking)
	}
	if msg.Content[1].Text != "Hello there" {
		t.Errorf("text block = %q, want 'Hello there'", msg.Content[1].Text)
	}
	if msg.Usage["output_tokens"] == 0 {
		t.Error("usage.output_tokens = 0, want upstream usage")
	}
	// Request conversion: the upstream body carries the system message,
	// the user message, and the resolved model.
	if !mock.BodyContains(`"role":"system"`) || !mock.BodyContains("You are helpful.") {
		t.Error("upstream body missing system message")
	}
	if !mock.BodyContains(`"role":"user"`) || !mock.BodyContains("hi") {
		t.Error("upstream body missing user message")
	}
	if !mock.BodyContains(modelA) {
		t.Error("upstream body missing resolved model")
	}
}

func TestMessagesToolUseRoundTrip(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-a2", 201,
		`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"toolu_01","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-a2", 201, `"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`))
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"weather?"}],"tools":[{"name":"get_weather","description":"weather lookup","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],"tool_choice":{"type":"tool","name":"get_weather"}}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var msg struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string         `json:"type"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("message not JSON: %v", err)
	}
	if msg.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", msg.StopReason)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "tool_use" {
		t.Fatalf("content = %+v, want one tool_use block", msg.Content)
	}
	tu := msg.Content[0]
	if tu.Name != "get_weather" || tu.Input["city"] != "SF" {
		t.Errorf("tool_use name/input = %q/%v, want get_weather/SF", tu.Name, tu.Input)
	}
	if tu.ID != "toolu_01" {
		t.Errorf("tool_use id = %q, want toolu_01 preserved", tu.ID)
	}
	// Upstream body: function-wrapped tool + translated tool_choice.
	if !mock.BodyContains(`"type":"function"`) || !mock.BodyContains(`"get_weather"`) {
		t.Error("upstream body missing wrapped tool")
	}
	if !mock.BodyContains(`"tool_choice"`) || !mock.BodyContains(`"name":"get_weather"`) {
		t.Error("upstream body missing translated tool_choice")
	}
}

func TestMessagesToolResultConversion(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","messages":[
		{"role":"assistant","content":[{"type":"text","text":"let me check"},{"type":"tool_use","id":"toolu_99","name":"get_weather","input":{"city":"SF"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_99","content":"sunny, 70F"},{"type":"text","text":"thanks"}]}
	]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	recorded := mock.RecordedChatBodies
	if len(recorded) == 0 {
		t.Fatal("no upstream chat body recorded")
	}
	up := recorded[0]
	for _, want := range []string{
		`"tool_call_id":"toolu_99"`,
		`"role":"tool"`,
		"sunny, 70F",
		`"role":"assistant"`,
		`"tool_calls"`,
	} {
		if !strings.Contains(up, want) {
			t.Errorf("upstream body missing %s: %s", want, truncate(up, 500))
		}
	}
}

func TestMessagesStream(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = anthropicTextChunks()
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	sse := string(data)
	// Event sequence: message_start → thinking block → text block →
	// message_delta → message_stop.
	for _, want := range []string{
		`"type":"message_start"`,
		`"type":"content_block_start"`,
		`"type":"thinking"`,
		`"type":"thinking_delta"`,
		`"thinking":"think step by step"`,
		`"type":"text_delta"`,
		`"text":"Hello"`,
		`"type":"signature_delta"`,
		`"type":"content_block_stop"`,
		`"type":"message_delta"`,
		`"stop_reason":"end_turn"`,
		`"type":"message_stop"`,
	} {
		if !strings.Contains(sse, want) {
			t.Errorf("stream missing %s", want)
		}
	}
	if !strings.Contains(sse, `"model":"`+modelA+`"`) {
		t.Error("message_start missing the requested anthropic model")
	}
}

func TestMessagesStreamToolUse(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-a3", 202,
		`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"toolu_07","type":"function","function":{"name":"run_shell","arguments":"{\"cmd\":\"ls\"}"}}]},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-a3", 202, `"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`))
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"ls"}],"tools":[{"name":"run_shell","description":"run a shell command","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}}],"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	sse := string(data)
	for _, want := range []string{
		`"type":"content_block_start"`,
		`"type":"tool_use"`,
		`"name":"run_shell"`,
		`"type":"input_json_delta"`,
		`"partial_json":"{\"cmd\":\"ls\"}"`,
		`"stop_reason":"tool_use"`,
		`"type":"message_stop"`,
	} {
		if !strings.Contains(sse, want) {
			t.Errorf("stream missing %s", want)
		}
	}
}

// TestMessagesStreamXMLToolCall wires the streaming XML tool-call extractor
// into the Anthropic relay: an upstream delta.content carrying a
// fragment-SPLIT <codebuff_tool_call> block must surface as a native
// tool_use block (content_block_start + input_json_delta) with the raw XML
// text absent from content.
func TestMessagesStreamXMLToolCall(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// The codebuff_tool_call block is split across three content deltas so
	// no single fragment contains the complete block.
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-a4", 202,
		`"choices":[{"index":0,"delta":{"content":"<codebuff_tool_call>"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-a4", 202,
			`"choices":[{"index":0,"delta":{"content":"\n<function=bash>\n<parameter=command>pwd</parameter>\n</function>"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-a4", 202,
			`"choices":[{"index":0,"delta":{"content":"\n</codebuff_tool_call>"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-a4", 202, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`))
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"run pwd"}],"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	sse := string(data)
	for _, want := range []string{
		`"type":"content_block_start"`,
		`"type":"tool_use"`,
		`"name":"bash"`,
		`"type":"input_json_delta"`,
		`"partial_json":"{\"command\":\"pwd\"}"`,
		`"stop_reason":"tool_use"`,
		`"type":"message_stop"`,
	} {
		if !strings.Contains(sse, want) {
			t.Errorf("stream missing %s", want)
		}
	}
	if strings.Contains(sse, "<codebuff_tool_call>") {
		t.Error("stream leaked raw XML tool-call text in content")
	}
}

// countTokensResult is the success envelope of /v1/messages/count_tokens.
type countTokensResult struct {
	InputTokens int `json:"input_tokens"`
}

// TestMessagesCountTokens pins the count_tokens contract: a local 200 with
// an Anthropic-shaped {input_tokens} estimate and zero upstream contact.
// The estimate is computed by the golden-reference table in the
// tokenestimate package; here we assert the handler contract end-to-end.
func TestMessagesCountTokens(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hello"}]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages/count_tokens", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
	var out countTokensResult
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	// 8 per-message overhead + 1 for "hello" (o200k_base reference).
	if out.InputTokens != 9 {
		t.Errorf("input_tokens = %d, want 9", out.InputTokens)
	}
	if mock.Requests != 0 {
		t.Errorf("upstream requests = %d, want 0 (local estimate only)", mock.Requests)
	}
}

// TestMessagesCountTokensComplexRequest exercises a mixed request (system,
// thinking, tool_use, tool_result, tools) against the golden total 93 from
// the Python tiktoken reference — proving the estimator's composition, not
// just the trivial one-message case.
func TestMessagesCountTokensComplexRequest(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	body := `{
		"model":"` + modelA + `",
		"system":"You are a helpful assistant.",
		"messages":[
			{"role":"user","content":"hello"},
			{"role":"assistant","content":[{"type":"thinking","thinking":"Let me think about this."}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"get_weather","input":{"city":"Jakarta"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"3 files"}]}]}
		],
		"tools":[{"name":"get_weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]
	}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages/count_tokens", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
	var out countTokensResult
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if out.InputTokens != 93 {
		t.Errorf("input_tokens = %d, want 93 (golden reference)", out.InputTokens)
	}
	if mock.Requests != 0 {
		t.Errorf("upstream requests = %d, want 0", mock.Requests)
	}
}

// TestMessagesCountTokensDeterministic sends the same request twice: the
// estimator must be byte-stable (no random hashing, no cache-dependent
// drift), so repeated calls return identical counts.
func TestMessagesCountTokensDeterministic(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	body := []byte(`{"model":"` + modelA + `","messages":[{"role":"user","content":"hello world"}]}`)
	var first int
	for i := 0; i < 5; i++ {
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages/count_tokens", body, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("iteration %d status = %d: %s", i, resp.StatusCode, truncate(string(data), 200))
		}
		var out countTokensResult
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("iteration %d body not JSON: %v", i, err)
		}
		if i == 0 {
			first = out.InputTokens
			continue
		}
		if out.InputTokens != first {
			t.Fatalf("iteration %d input_tokens = %d, want %d (deterministic)", i, out.InputTokens, first)
		}
	}
	if mock.Requests != 0 {
		t.Errorf("upstream requests = %d, want 0", mock.Requests)
	}
}

func TestMessagesCountTokensMissingModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	body := `{"messages":[{"role":"user","content":"hi"}]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages/count_tokens", []byte(body), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, truncate(string(data), 200))
	}
	var out struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("error body not JSON: %v", err)
	}
	if out.Error.Type != "invalid_request_error" || out.Error.Code != "model_not_found" {
		t.Errorf("type/code = %q/%q, want invalid_request_error/model_not_found", out.Error.Type, out.Error.Code)
	}
	if mock.Requests != 0 {
		t.Errorf("upstream requests = %d, want 0", mock.Requests)
	}
}

func TestMessagesCountTokensUnknownModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages/count_tokens", []byte(body), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, truncate(string(data), 200))
	}
	var out struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("error body not JSON: %v", err)
	}
	if out.Error.Type != "invalid_request_error" {
		t.Errorf("type = %q, want invalid_request_error", out.Error.Type)
	}
	if mock.Requests != 0 {
		t.Errorf("upstream requests = %d, want 0", mock.Requests)
	}
}

func TestMessagesCountTokensInvalidJSON(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	for _, body := range []string{`{not json`, `[]`, `"x"`, ``, `{"model":"x"} trailing`} {
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages/count_tokens", []byte(body), nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %q status = %d, want 400: %s", body, resp.StatusCode, truncate(string(data), 200))
		}
		var out struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("body %q error not JSON: %v", body, err)
		}
		if out.Error.Code != "invalid_json" {
			t.Errorf("body %q code = %q, want invalid_json", body, out.Error.Code)
		}
	}
	if mock.Requests != 0 {
		t.Errorf("upstream requests = %d, want 0", mock.Requests)
	}
}

// TestMessagesCountTokensOversizedBody pins the 32MiB body cap on
// count_tokens: an oversized payload is rejected with 413 content_too_large
// before any counting or upstream contact.
func TestMessagesCountTokensOversizedBody(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	oversized := bytes.Repeat([]byte("a"), 32<<20+1)
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages/count_tokens", oversized, nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if !strings.Contains(string(data), "content_too_large") {
		t.Errorf("body missing content_too_large: %s", truncate(string(data), 200))
	}
	if mock.Requests != 0 {
		t.Errorf("upstream requests = %d, want 0 (oversized body rejected before counting)", mock.Requests)
	}
}

// TestMessagesCountTokensDocumentRejected verifies the estimator refuses to
// guess a token count for PDF/document blocks: the proxy's /v1/messages
// conversion does not consume documents, so the request is rejected with
// 400 instead of faking accuracy with a base64 character count.
func TestMessagesCountTokensDocumentRejected(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}}]}]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages/count_tokens", []byte(body), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, truncate(string(data), 200))
	}
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("error body not JSON: %v", err)
	}
	if out.Error.Code != "unsupported_content" {
		t.Errorf("code = %q, want unsupported_content (valid JSON, so invalid_json would mislead)", out.Error.Code)
	}
	if mock.Requests != 0 {
		t.Errorf("upstream requests = %d, want 0", mock.Requests)
	}
}

// TestMessagesCountTokensSystemImageFlat pins the flat image cost through the
// handler for image parts in the top-level system array: 1600/image, never a
// function of base64 length (regression: the system array used to fall back
// to JSON counting and tokenize the base64 as text, quadratically — a 256KiB
// payload cost ~87s of CPU).
func TestMessagesCountTokensSystemImageFlat(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","system":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + strings.Repeat("QUJD", (128<<10)/3) + `"}}],"messages":[{"role":"user","content":"hi"}]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages/count_tokens", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
	var out countTokensResult
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if out.InputTokens != 1600+8+1 { // system image + message overhead + "hi"
		t.Errorf("input_tokens = %d, want %d (flat 1600, base64 never tokenized)", out.InputTokens, 1600+8+1)
	}
	if mock.Requests != 0 {
		t.Errorf("upstream requests = %d, want 0", mock.Requests)
	}
}

// TestMessagesCountTokensAuth pins that count_tokens sits behind the same
// requireAuth gate as the rest of the API surface (no key → 401, key → 200).
func TestMessagesCountTokensAuth(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, []string{"sk-test"}, mock)
	url := ts.URL + "/v1/messages/count_tokens"
	body := []byte(`{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}]}`)

	resp, data := doJSON(t, http.MethodPost, url, body, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key status = %d, want 401: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if !strings.Contains(string(data), "invalid_api_key") {
		t.Errorf("body missing invalid_api_key: %s", data)
	}

	resp, data = doJSON(t, http.MethodPost, url, body, map[string]string{"x-api-key": "sk-test"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("x-api-key status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}

	resp, data = doJSON(t, http.MethodPost, url, body, map[string]string{"Authorization": "Bearer sk-test"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Bearer status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}

	if mock.Requests != 0 {
		t.Errorf("upstream requests = %d, want 0", mock.Requests)
	}
}

// --- #83/#59 SSE keepalive, grace flush, late failure ---

// TestRelayStreamInBandErrorFrame verifies an upstream error chunk is
// relayed in-band (type upstream_error + provider message) followed by
// [DONE] — the reference late-failure contract.
func TestRelayStreamInBandErrorFrame(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(`{"id":"chatcmpl-e1","object":"chat.completion.chunk","created":400,"model":"`+modelA+`","choices":[{"index":0,"delta":{},"finish_reason":null}]}`) +
		testutil.SSEEvent(`{"error":{"message":"Upstream provider error (429): Model is at capacity.","type":"upstream_error"}}`)
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions",
		chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (error delivered in-band): %s", resp.StatusCode, truncate(string(data), 200))
	}
	body := string(data)
	if !strings.Contains(body, `"type":"upstream_error"`) {
		t.Error("error frame missing type upstream_error")
	}
	if !strings.Contains(body, "Upstream provider error (429): Model is at capacity.") {
		t.Error("error frame missing the provider message")
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Error("stream missing the [DONE] terminator after the error frame")
	}
}

// --- #89 phase timing in traces ---

// TestTracePhasesRecorded verifies the chat trace carries the per-request
// latency phases (acquire_ms/upstream_ttfb_ms/total_ms; session/run phases
// come from the pool when it opts into phasetiming).
func TestTracePhasesRecorded(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	var sink bytes.Buffer
	ring := logring.NewHandler(slog.NewTextHandler(&sink, nil), 200)
	ts, _ := newTestServerWithLogger(t, nil, slog.New(ring), ring, mock)

	_, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if !strings.Contains(string(data), "Hello") {
		t.Fatalf("chat stream unexpected: %s", truncate(string(data), 200))
	}
	entries := ring.Recent(50)
	var trace *logring.Entry
	for i := range entries {
		if entries[i].Message == "chat trace" {
			trace = &entries[i]
			break
		}
	}
	if trace == nil {
		t.Fatal("no 'chat trace' entry in the log ring")
	}
	joined := strings.Join(trace.Fields, " ")
	for _, phase := range []string{"acquire_ms", "upstream_ttfb_ms", "total_ms"} {
		re := regexp.MustCompile(phase + `=\d+`)
		if !re.MatchString(joined) {
			t.Errorf("trace fields missing %s: %s", phase, joined)
		}
	}
	if !strings.Contains(joined, "status=ok") {
		t.Errorf("trace missing status=ok: %s", joined)
	}
}

// --- D1 correlation ids + T2/T3/T8/T12/T13 ---

// entryField extracts a "key=value" field from a logring entry, or "".
func entryField(e logring.Entry, key string) string {
	for _, f := range e.Fields {
		if v, ok := strings.CutPrefix(f, key+"="); ok {
			return v
		}
	}
	return ""
}

// debugRing builds a logring-wrapped Debug-level logger so tests can assert
// on Debug lines (upstream do/retry, runs lifecycle, transient retry).
func debugRing(t *testing.T) (*bytes.Buffer, *logring.Handler, *slog.Logger) {
	t.Helper()
	var sink bytes.Buffer
	ring := logring.NewHandler(slog.NewTextHandler(&sink, &slog.HandlerOptions{Level: slog.LevelDebug}), 400)
	logger := slog.New(ring)
	// runs.go and upstream/client.go log via the package-level default
	// logger (slog.Debug), mirroring production where main.go calls
	// slog.SetDefault. Route the default at the ring so those lines are
	// assertable, and restore on test end.
	oldDefault := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(oldDefault) })
	return &sink, ring, logger
}

// TestRequestCorrelationIDs verifies D1 (T2): one request with
// X-Request-Id: abc produces the SAME req_id on access → chat routing →
// chat done → chat trace, a client_request_id=abc passthrough on access +
// trace, and a header-less request carries no client_request_id and a
// fresh req_id.
func TestRequestCorrelationIDs(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	_, ring, logger := debugRing(t)
	ts, _ := newTestServerWithLogger(t, nil, logger, ring, mock)

	_, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA),
		map[string]string{"X-Request-Id": "abc"})
	if !strings.Contains(string(data), "Hello") {
		t.Fatalf("chat stream unexpected: %s", truncate(string(data), 200))
	}
	entries := ring.Recent(100)
	byMsg := map[string]*logring.Entry{}
	for i := range entries {
		e := &entries[i]
		switch e.Message {
		case "access", "chat routing", "chat done", "chat trace":
			byMsg[e.Message] = e
		}
	}
	for _, want := range []string{"access", "chat routing", "chat done", "chat trace"} {
		if byMsg[want] == nil {
			t.Fatalf("missing %q entry in the log ring", want)
		}
	}
	reqID := entryField(*byMsg["access"], "req_id")
	if reqID == "" {
		t.Fatal("access entry missing req_id")
	}
	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidRe.MatchString(reqID) {
		t.Errorf("access req_id = %q, want UUIDv4 shape", reqID)
	}
	for _, m := range []string{"chat routing", "chat done", "chat trace"} {
		if got := entryField(*byMsg[m], "req_id"); got != reqID {
			t.Errorf("%s req_id = %q, want the access req_id %q", m, got, reqID)
		}
	}
	for _, m := range []string{"access", "chat trace"} {
		if got := entryField(*byMsg[m], "client_request_id"); got != "abc" {
			t.Errorf("%s client_request_id = %q, want abc", m, got)
		}
	}

	// A second request without X-Request-Id: no client_request_id anywhere
	// and a fresh req_id (not the previous request's).
	_, data2 := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if !strings.Contains(string(data2), "Hello") {
		t.Fatalf("second chat stream unexpected: %s", truncate(string(data2), 200))
	}
	entries2 := ring.Recent(200)
	var access2, trace2 *logring.Entry
	for i := range entries2 {
		e := &entries2[i]
		if e.Message == "access" && entryField(*e, "req_id") != reqID && access2 == nil {
			access2 = e
		}
		if e.Message == "chat trace" && entryField(*e, "req_id") != reqID && trace2 == nil {
			trace2 = e
		}
	}
	if access2 == nil {
		t.Fatal("no access entry for the second request")
	}
	if got := entryField(*access2, "client_request_id"); got != "" {
		t.Errorf("header-less request access client_request_id = %q, want absent", got)
	}
	if trace2 == nil {
		t.Fatal("no chat trace entry for the second request")
	}
	if got := entryField(*trace2, "client_request_id"); got != "" {
		t.Errorf("header-less request trace client_request_id = %q, want absent", got)
	}
}

// TestTransientRetrySkippedOnCanceledContext verifies T8: a chat that fails
// because the request context was canceled must NOT fire the retry-once
// recovery (a retry cannot succeed on a canceled context) — no
// "transient chat error, retrying once", no "chat retry succeeded".
func TestTransientRetrySkippedOnCanceledContext(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	chatSeen := make(chan struct{})
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-chatSeen:
		default:
			close(chatSeen)
		}
		// Hold the chat POST open until the request context dies, so the
		// upstream call fails with a context error at the retry decision
		// point (the exact log-watch scenario: retry on "context canceled").
		select {
		case <-r.Context().Done():
			mock.AbortDetected.Store(true)
		case <-time.After(30 * time.Second):
		}
	}
	_, ring, logger := debugRing(t)
	ts, _ := newTestServerWithLogger(t, nil, logger, ring, mock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/chat/completions", bytes.NewReader(chatBody(modelA)))
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() {
		resp, err := testClient.Do(req)
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

	// Wait for the handler to finish logging the canceled request.
	eventually(t, "request canceled by client entry", func() bool {
		for _, e := range ring.Recent(400) {
			if e.Message == "request canceled by client" {
				return true
			}
		}
		return false
	})
	for _, e := range ring.Recent(400) {
		if strings.Contains(e.Message, "transient chat error") {
			t.Errorf("canceled request logged a retry announcement: %s", e.Message)
		}
		if e.Message == "chat retry succeeded" {
			t.Error("canceled request logged chat retry succeeded")
		}
	}
	<-errCh
}

// TestChatRetryTelemetry verifies T12/T13: a retried request logs the
// structured "transient chat error, retrying once" (reason/backoff_ms/
// attempt/req_id), "chat retry succeeded" (attempts=2), a chat trace with
// attempts=2/retried=true/statuses_seen=500,200, and the SAME req_id on
// both upstream attempt lines (D1 threading to the client do() logs).
func TestChatRetryTelemetry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var calls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// Generic 5xx: not a classified error and not Retryable, so the
			// server's retry-once recovery fires (the UpstreamError carries
			// status 500 into statuses_seen).
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"internal boom"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, responsesChunks())
	}
	_, ring, logger := debugRing(t)
	ts, _ := newTestServerWithLogger(t, nil, logger, ring, mock)

	_, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA),
		map[string]string{"X-Request-Id": "req-1"})
	if !strings.Contains(string(data), "Hello") {
		t.Fatalf("retried chat stream unexpected: %s", truncate(string(data), 200))
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream chat attempts = %d, want 2", got)
	}

	var transient, retriedOK, trace, ok1, ok2 *logring.Entry
	recent := ring.Recent(400)
	for i := range recent {
		e := &recent[i]
		switch e.Message {
		case "transient chat error, retrying once":
			transient = e
		case "chat retry succeeded":
			retriedOK = e
		case "chat trace":
			trace = e
		case "upstream ok", "upstream response":
			// Session/run management calls also log these; only the
			// two /api/v1/chat/completions attempts carry the chat req_id.
			// T5: the failed attempt logs "upstream response", the
			// successful retry logs "upstream ok".
			if entryField(*e, "path") != "/api/v1/chat/completions" {
				continue
			}
			if ok1 == nil {
				ok1 = e
			} else if ok2 == nil {
				ok2 = e
			}
		}
	}
	if transient == nil {
		t.Fatal("no 'transient chat error, retrying once' entry")
	}
	if got := entryField(*transient, "attempt"); got != "1" {
		t.Errorf("transient entry attempt = %q, want 1", got)
	}
	if entryField(*transient, "reason") == "" {
		t.Error("transient entry missing reason")
	}
	if entryField(*transient, "backoff_ms") == "" {
		t.Error("transient entry missing backoff_ms")
	}
	if retriedOK == nil {
		t.Fatal("no 'chat retry succeeded' entry")
	}
	if got := entryField(*retriedOK, "attempts"); got != "2" {
		t.Errorf("retry succeeded attempts = %q, want 2", got)
	}
	if entryField(*retriedOK, "ms") == "" {
		t.Error("retry succeeded missing ms")
	}
	if trace == nil {
		t.Fatal("no chat trace entry")
	}
	reqID := entryField(*trace, "req_id")
	if reqID == "" {
		t.Fatal("chat trace missing req_id")
	}
	for _, f := range []struct{ key, want string }{
		{"attempts", "2"},
		{"retried", "true"},
		{"statuses_seen", "500,200"},
		{"client_request_id", "req-1"},
	} {
		if got := entryField(*trace, f.key); got != f.want {
			t.Errorf("chat trace %s = %q, want %q", f.key, got, f.want)
		}
	}
	if got := entryField(*trace, "backoff_ms"); got == "" {
		t.Error("chat trace missing backoff_ms")
	}
	// D1: the same req_id must appear on both upstream attempt lines, and
	// on the server-side retry lines.
	if ok1 == nil || ok2 == nil {
		t.Fatal("expected two upstream attempt entries (one per chat attempt)")
	}
	if got := entryField(*ok1, "req_id"); got != reqID {
		t.Errorf("first upstream attempt req_id = %q, want %q", got, reqID)
	}
	if got := entryField(*ok2, "req_id"); got != reqID {
		t.Errorf("second upstream attempt req_id = %q, want %q", got, reqID)
	}
	if got := entryField(*transient, "req_id"); got != reqID {
		t.Errorf("transient entry req_id = %q, want %q", got, reqID)
	}
	if got := entryField(*retriedOK, "req_id"); got != reqID {
		t.Errorf("retry succeeded req_id = %q, want %q", got, reqID)
	}
}

// TestTraceSessionIDThreaded verifies T3: the run's trace_session_id (the
// value threaded into codebuff_metadata) appears on "runs: run started"
// and on the chat trace of the request that acquired the run, with the
// same value.
func TestTraceSessionIDThreaded(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	_, ring, logger := debugRing(t)
	ts, _ := newTestServerWithLogger(t, nil, logger, ring, mock)

	_, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if !strings.Contains(string(data), "Hello") {
		t.Fatalf("chat stream unexpected: %s", truncate(string(data), 200))
	}
	var startedTS, traceTS string
	recent := ring.Recent(400)
	for i := range recent {
		e := &recent[i]
		switch e.Message {
		case "runs: run started":
			startedTS = entryField(*e, "trace_session_id")
		case "chat trace":
			traceTS = entryField(*e, "trace_session_id")
		}
	}
	if startedTS == "" {
		t.Fatal("runs: run started entry missing trace_session_id")
	}
	if traceTS == "" {
		t.Fatal("chat trace entry missing trace_session_id")
	}
	if traceTS != startedTS {
		t.Errorf("chat trace trace_session_id = %q, want the run started value %q", traceTS, startedTS)
	}
}
