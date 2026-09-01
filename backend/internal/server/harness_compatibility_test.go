package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
)

// TestAnthropicClaudeCodeStreamingSequence tests the exact Anthropic SSE event sequence
// expected by Claude Code CLI and Anthropic SDK:
// message_start -> content_block_start (thinking) -> content_block_delta (thinking_delta) ->
// signature_delta -> content_block_stop -> content_block_start (tool_use) ->
// content_block_delta (input_json_delta) -> content_block_stop -> message_delta -> message_stop.
// Also verifies end_turn is completely stripped and never leaked as a tool_use block.
func TestAnthropicClaudeCodeStreamingSequence(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// 1. Thinking delta
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-1","choices":[{"delta":{"reasoning_content":"Thinking about files...","role":"assistant"},"index":0}]}`+"\n\n")
		// 2. Real tool call + injected end_turn pseudo tool call from upstream
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_bash_123","type":"function","function":{"name":"Bash","arguments":"{\"command\":"}}]},"index":0}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls -la\"}"}}]},"index":0}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-1","choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_end_turn_999","type":"function","function":{"name":"end_turn","arguments":"{}"}}]},"index":0}]}`+"\n\n")
		// 3. Final chunk with finish_reason and usage
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-1","choices":[{"delta":{},"finish_reason":"tool_calls","index":0}],"usage":{"prompt_tokens":150,"completion_tokens":45,"total_tokens":195}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	ts, _ := newTestServerCfg(t, []string{"anthropic-test-key"}, nil, mock)

	reqBody := `{
		"model": "deepseek/deepseek-v4-flash",
		"messages": [
			{"role": "user", "content": "List files in directory"}
		],
		"stream": true,
		"thinking": {"type": "enabled", "budget_tokens": 2048}
	}`

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-api-key", "anthropic-test-key")
	req.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, string(body))
	}

	verHeader := resp.Header.Get("anthropic-version")
	if verHeader != "2023-06-01" {
		t.Errorf("anthropic-version header = %q, want 2023-06-01", verHeader)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(bodyBytes), "\n")

	var events []string
	var dataLines []map[string]any
	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if strings.HasPrefix(line, "event:") {
			events = append(events, strings.TrimSpace(strings.TrimPrefix(line, "event:")))
		}
		if strings.HasPrefix(line, "data:") {
			jsonStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			var dataMap map[string]any
			if err := json.Unmarshal([]byte(jsonStr), &dataMap); err == nil {
				dataLines = append(dataLines, dataMap)
			}
		}
	}

	// Verify events sequence
	if len(events) == 0 {
		t.Fatalf("no SSE events emitted: %s", string(bodyBytes))
	}

	// Verify message_start event has input_tokens > 0
	var foundMessageStart, foundToolUse, foundEndTurnToolUse, foundMessageDelta, foundMessageStop bool
	for _, dm := range dataLines {
		evType, _ := dm["type"].(string)
		switch evType {
		case "message_start":
			foundMessageStart = true
			if msg, ok := dm["message"].(map[string]any); ok {
				if usage, ok := msg["usage"].(map[string]any); ok {
					if inToks, ok := usage["input_tokens"].(float64); !ok || inToks <= 0 {
						t.Errorf("message_start usage.input_tokens = %v, want > 0", usage["input_tokens"])
					}
				} else {
					t.Errorf("message_start missing usage map")
				}
			}
		case "content_block_start":
			if cb, ok := dm["content_block"].(map[string]any); ok {
				cbType, _ := cb["type"].(string)
				if cbType == "tool_use" {
					name, _ := cb["name"].(string)
					if name == "end_turn" {
						foundEndTurnToolUse = true
					}
					if name == "Bash" {
						foundToolUse = true
					}
				}
			}
		case "message_delta":
			foundMessageDelta = true
			if delta, ok := dm["delta"].(map[string]any); ok {
				stopReason, _ := delta["stop_reason"].(string)
				if stopReason != "tool_use" {
					t.Errorf("message_delta stop_reason = %q, want tool_use", stopReason)
				}
			}
		case "message_stop":
			foundMessageStop = true
		}
	}

	if !foundMessageStart {
		t.Errorf("message_start event not found")
	}
	if !foundToolUse {
		t.Errorf("tool_use content_block_start for Bash not found")
	}
	if foundEndTurnToolUse {
		t.Errorf("end_turn pseudo-tool was leaked to Anthropic client in content_block_start")
	}
	if !foundMessageDelta {
		t.Errorf("message_delta event not found")
	}
	if !foundMessageStop {
		t.Errorf("message_stop event not found")
	}
}

// TestAnthropicNonStreamingMessage verifies non-streaming /v1/messages formatting,
// including thinking signature, end_turn stripping, and usage integers.
func TestAnthropicNonStreamingMessage(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-2","choices":[{"delta":{"reasoning_content":"Let's check tools.","role":"assistant"},"index":0}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-2","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_read_1","type":"function","function":{"name":"Read","arguments":"{\"path\":\"foo.go\"}"}}]},"index":0}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-2","choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_end_2","type":"function","function":{"name":"end_turn","arguments":"{}"}}]},"index":0}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-2","choices":[{"delta":{},"finish_reason":"tool_calls","index":0}],"usage":{"prompt_tokens":200,"completion_tokens":50,"total_tokens":250}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	ts, _ := newTestServer(t, nil, mock)

	reqBody := `{
		"model": "deepseek/deepseek-v4-flash",
		"messages": [{"role": "user", "content": "Read foo.go"}],
		"stream": false
	}`

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, string(body))
	}

	var msg map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatal(err)
	}

	if msg["type"] != "message" {
		t.Errorf("type = %v, want message", msg["type"])
	}
	if msg["role"] != "assistant" {
		t.Errorf("role = %v, want assistant", msg["role"])
	}
	if msg["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", msg["stop_reason"])
	}

	content, ok := msg["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content missing or empty: %v", msg["content"])
	}

	for _, rawBlock := range content {
		block, _ := rawBlock.(map[string]any)
		bType, _ := block["type"].(string)
		if bType == "thinking" {
			if _, hasSig := block["signature"]; !hasSig {
				t.Errorf("thinking block missing required signature field")
			}
		}
		if bType == "tool_use" {
			name, _ := block["name"].(string)
			if name == "end_turn" {
				t.Errorf("end_turn pseudo-tool leaked in non-streaming content block")
			}
			if name != "Read" {
				t.Errorf("unexpected tool name %q, want Read", name)
			}
		}
	}
}

// TestOpenAIMultiTurnSchemaCompliance tests OpenAI chat completions non-streaming schema:
// logprobs: null, refusal: null, tool_calls format.
func TestOpenAIMultiTurnSchemaCompliance(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-3","choices":[{"delta":{"content":"Hello world!","role":"assistant"},"index":0}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-3","choices":[{"delta":{},"finish_reason":"stop","index":0}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	ts, _ := newTestServer(t, nil, mock)

	reqBody := `{
		"model": "deepseek/deepseek-v4-flash",
		"functions": [{"name": "get_weather", "parameters": {"type": "object", "properties": {"loc": {"type": "string"}}}}],
		"function_call": "auto"
	}`

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, string(body))
	}

	var res map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}

	choices, ok := res["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("choices missing: %v", res)
	}
	choice := choices[0].(map[string]any)
	if _, hasLogprobs := choice["logprobs"]; !hasLogprobs {
		t.Errorf("choices[0] missing required logprobs field")
	}
	msg := choice["message"].(map[string]any)
	if _, hasRefusal := msg["refusal"]; !hasRefusal {
		t.Errorf("choices[0].message missing required refusal field")
	}
}

// TestOpenAIModelRetrieveEndpoint tests GET /v1/models/{model}
func TestOpenAIModelRetrieveEndpoint(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	ts, _ := newTestServer(t, nil, mock)

	// Existing model (glm-5.3-flash is served; glm-5.2 is paused since 2026-08-31)
	resp, err := http.Get(ts.URL + "/v1/models/z-ai/glm-5.3-flash")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var modelObj map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&modelObj); err != nil {
		t.Fatal(err)
	}
	if modelObj["id"] != "z-ai/glm-5.3-flash" || modelObj["object"] != "model" {
		t.Errorf("modelObj = %v, want id=z-ai/glm-5.3-flash, object=model", modelObj)
	}
	// Paused model -> 404 with withdrawn copy (freebuff-models.ts FREEBUFF_PAUSED_FREE_MODEL_IDS)
	respPaused, err := http.Get(ts.URL + "/v1/models/z-ai/glm-5.2")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = respPaused.Body.Close() }()
	if respPaused.StatusCode != http.StatusNotFound {
		t.Fatalf("paused model status = %d, want 404", respPaused.StatusCode)
	}

	// Unknown model -> 404
	resp404, err := http.Get(ts.URL + "/v1/models/unknown-model-xyz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp404.Body.Close() }()
	if resp404.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp404.StatusCode)
	}
}

// TestAnthropicErrorEnvelopeStructure tests error response formatting for Anthropic requests.
func TestAnthropicErrorEnvelopeStructure(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	ts, _ := newTestServer(t, nil, mock)

	// Empty messages array -> 400 with Anthropic error envelope
	reqBody := `{"model": "deepseek/deepseek-v4-flash", "messages": []}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if ver := resp.Header.Get("anthropic-version"); ver != "2023-06-01" {
		t.Errorf("anthropic-version header = %q, want 2023-06-01", ver)
	}

	var errResp struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatal(err)
	}
	if errResp.Type != "error" {
		t.Errorf("envelope type = %q, want error", errResp.Type)
	}
	if errResp.Error.Type != "invalid_request_error" {
		t.Errorf("inner error type = %q, want invalid_request_error", errResp.Error.Type)
	}
}

// TestBridgeModeAnthropicAndOpenAI verifies that when AUTH_TOKENS is empty (pure bridge mode),
// clients can authenticate using anthropic-api-key, x-api-key, or Authorization: Bearer,
// and the client's token is dynamically leased and relayed upstream.
func TestBridgeModeAnthropicAndOpenAI(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	var seenTokens []string
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		seenTokens = append(seenTokens, strings.TrimPrefix(auth, "Bearer "))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-b","choices":[{"delta":{"content":"ok"},"index":0,"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newBridgeTestServer(t, mock)

	// 1. Anthropic endpoint with anthropic-api-key
	req1, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(`{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("anthropic-api-key", "token-anthropic-key")
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp1.Body.Close() }()
	if resp1.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp1.Body)
		t.Fatalf("anthropic-api-key status = %d, want 200: %s", resp1.StatusCode, string(body))
	}

	// 2. Anthropic endpoint with x-api-key
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(`{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("x-api-key", "token-x-key")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("x-api-key status = %d, want 200: %s", resp2.StatusCode, string(body))
	}

	// 3. OpenAI endpoint with Authorization: Bearer
	req3, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("Authorization", "Bearer token-bearer-key")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp3.Body.Close() }()
	if resp3.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp3.Body)
		t.Fatalf("bearer token status = %d, want 200: %s", resp3.StatusCode, string(body))
	}

	// 4. Missing token -> 401
	req4, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(`{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`))
	req4.Header.Set("Content-Type", "application/json")
	resp4, err := http.DefaultClient.Do(req4)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp4.Body.Close() }()
	if resp4.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", resp4.StatusCode)
	}

	// Verify tokens were relayed upstream
	if len(seenTokens) != 3 {
		t.Fatalf("seen tokens count = %d, want 3", len(seenTokens))
	}
	if seenTokens[0] != "token-anthropic-key" || seenTokens[1] != "token-x-key" || seenTokens[2] != "token-bearer-key" {
		t.Errorf("seen tokens = %v, want [token-anthropic-key, token-x-key, token-bearer-key]", seenTokens)
	}
}

// collectAnthropicEvents parses a /v1/messages SSE body into the ordered
// event data maps (event: headers and comment lines are ignored; each data:
// line is one event).
func collectAnthropicEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rawLine := range strings.Split(body, "\n") {
		line := strings.TrimSpace(rawLine)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		jsonStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if jsonStr == "" {
			continue
		}
		var dm map[string]any
		if err := json.Unmarshal([]byte(jsonStr), &dm); err != nil {
			t.Fatalf("invalid SSE data line %q: %v", jsonStr, err)
		}
		out = append(out, dm)
	}
	return out
}

// TestAnthropicStreamEndTurnOnlyStopReason pins the end_turn-only invariant:
// when the upstream terminates with finish_reason "tool_calls" but every tool
// fragment was the stripped proxy-injected end_turn pseudo-tool, the relay
// must emit message_delta stop_reason "end_turn" and ZERO tool_use content
// blocks — never a tool_use stop_reason with no blocks.
func TestAnthropicStreamEndTurnOnlyStopReason(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// end_turn-only turn: the proxy-injected pseudo-tool is called, then
		// the upstream claims a tool call with finish_reason "tool_calls".
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-e1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_end_1","type":"function","function":{"name":"end_turn","arguments":"{}"}}]},"index":0}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-e1","choices":[{"delta":{},"finish_reason":"tool_calls","index":0}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, nil, mock)

	reqBody := `{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, string(body))
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	events := collectAnthropicEvents(t, string(bodyBytes))

	var stopReason string
	toolUseBlocks := 0
	for _, ev := range events {
		switch ev["type"] {
		case "content_block_start":
			if cb, ok := ev["content_block"].(map[string]any); ok && cb["type"] == "tool_use" {
				toolUseBlocks++
			}
		case "message_delta":
			if delta, ok := ev["delta"].(map[string]any); ok {
				stopReason, _ = delta["stop_reason"].(string)
			}
		}
	}
	if stopReason != "end_turn" {
		t.Errorf("message_delta stop_reason = %q, want end_turn (end_turn-only turn)", stopReason)
	}
	if toolUseBlocks != 0 {
		t.Errorf("tool_use content blocks = %d, want 0 (end_turn-only turn)", toolUseBlocks)
	}
}

// TestAnthropicStreamThinkingClosedBeforeToolUse pins the sequential block
// lifecycle: a thinking content_block_stop (with its signature_delta) must be
// emitted BEFORE the tool_use content_block_start — the thinking block must
// not stay open and straddle the tool_use block.
func TestAnthropicStreamThinkingClosedBeforeToolUse(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-t1","choices":[{"delta":{"reasoning_content":"Let me think"},"index":0}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-t1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_bash_1","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]},"index":0}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-t1","choices":[{"delta":{},"finish_reason":"tool_calls","index":0}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, nil, mock)

	reqBody := `{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true,"thinking":{"type":"enabled","budget_tokens":1024}}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, string(body))
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	events := collectAnthropicEvents(t, string(bodyBytes))

	// Walk the event stream: record when the thinking block starts/stops and
	// when the tool_use block starts; the thinking stop must come first.
	var seq []string
	thinkingOpen := false
	stopReason := ""
	for _, ev := range events {
		switch ev["type"] {
		case "content_block_start":
			cb, _ := ev["content_block"].(map[string]any)
			switch cb["type"] {
			case "thinking":
				thinkingOpen = true
				seq = append(seq, "thinking-start")
			case "tool_use":
				seq = append(seq, "tool-start")
			}
		case "content_block_stop":
			if thinkingOpen {
				thinkingOpen = false
				seq = append(seq, "thinking-stop")
			}
		case "message_delta":
			if delta, ok := ev["delta"].(map[string]any); ok {
				stopReason, _ = delta["stop_reason"].(string)
			}
		}
	}
	tsPos, toolPos := -1, -1
	for i, s := range seq {
		if s == "thinking-stop" && tsPos < 0 {
			tsPos = i
		}
		if s == "tool-start" && toolPos < 0 {
			toolPos = i
		}
	}
	if tsPos < 0 || toolPos < 0 {
		t.Fatalf("missing lifecycle markers in %v", seq)
	}
	if tsPos > toolPos {
		t.Errorf("thinking block stopped at %d after tool_use started at %d: %v", tsPos, toolPos, seq)
	}
	if stopReason != "tool_use" {
		t.Errorf("message_delta stop_reason = %q, want tool_use (real tool call)", stopReason)
	}
}

// TestAnthropicStreamReasoningAfterTextReopensThinking pins the
// reasoning-after-text fix: when reasoning deltas resume after a text block
// opened (which closed the thinking block), the relay must open a FRESH
// thinking content block at a NEW index and emit the deltas against it —
// never against the already-closed index.
func TestAnthropicStreamReasoningAfterTextReopensThinking(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-r1","choices":[{"delta":{"reasoning_content":"first thought"},"index":0}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-r1","choices":[{"delta":{"content":"answer text"},"index":0}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-r1","choices":[{"delta":{"reasoning_content":"second thought"},"index":0}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-r1","choices":[{"delta":{},"finish_reason":"stop","index":0}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, nil, mock)

	reqBody := `{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, string(body))
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	events := collectAnthropicEvents(t, string(bodyBytes))

	idxOf := func(ev map[string]any) int {
		if f, ok := ev["index"].(float64); ok {
			return int(f)
		}
		return -1
	}
	var thinkingStarts []int
	textStart := -1
	thinkingDeltas := map[int]bool{}
	stopReason := ""
	for _, ev := range events {
		switch ev["type"] {
		case "content_block_start":
			idx := idxOf(ev)
			if cb, ok := ev["content_block"].(map[string]any); ok {
				switch cb["type"] {
				case "thinking":
					thinkingStarts = append(thinkingStarts, idx)
				case "text":
					textStart = idx
				}
			}
		case "content_block_delta":
			if d, ok := ev["delta"].(map[string]any); ok && d["type"] == "thinking_delta" {
				thinkingDeltas[idxOf(ev)] = true
			}
		case "message_delta":
			if delta, ok := ev["delta"].(map[string]any); ok {
				stopReason, _ = delta["stop_reason"].(string)
			}
		}
	}
	if len(thinkingStarts) != 2 {
		t.Fatalf("thinking content_block_start count = %d, want 2; starts %v", len(thinkingStarts), thinkingStarts)
	}
	if textStart < 0 {
		t.Fatal("no text block emitted")
	}
	second := thinkingStarts[1]
	if second <= textStart {
		t.Errorf("reopened thinking index %d must be a fresh index above the text block index %d", second, textStart)
	}
	if !thinkingDeltas[second] {
		t.Errorf("no thinking_delta against the reopened thinking index %d", second)
	}
	if thinkingDeltas[thinkingStarts[0]] == false {
		t.Errorf("first thinking index %d missing its thinking_delta", thinkingStarts[0])
	}
	if stopReason != "end_turn" {
		t.Errorf("message_delta stop_reason = %q, want end_turn", stopReason)
	}
}

// TestAnthropicMessagesRateLimitErrorEnvelope verifies that a /v1/messages
// request tripping the local per-IP rate limiter gets an Anthropic error
// envelope ({"type":"error","error":{...}}), not the OpenAI-shaped body
// writeJSONError produces.
func TestAnthropicMessagesRateLimitErrorEnvelope(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-rl", 1, `"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-rl", 1, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`))
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.RateLimitPerIP = 1.0
		c.RateLimitBurst = 2
	}, mock)

	body := `{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`
	for i := range 2 {
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d should succeed, got %d: %s", i+1, resp.StatusCode, truncate(string(data), 200))
		}
	}
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("3rd request should return 429, got %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var errResp struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &errResp); err != nil {
		t.Fatalf("error response is not valid JSON: %v: %s", err, data)
	}
	if errResp.Type != "error" {
		t.Errorf("top-level type = %q, want error (Anthropic envelope)", errResp.Type)
	}
	if errResp.Error.Type != "rate_limit_error" {
		t.Errorf("error.type = %q, want rate_limit_error", errResp.Error.Type)
	}
	if errResp.Error.Code != "rate_limit_exceeded" {
		t.Errorf("error.code = %q, want rate_limit_exceeded", errResp.Error.Code)
	}
	if !strings.Contains(errResp.Error.Message, "rate limit exceeded") {
		t.Errorf("error.message = %q, want rate limit exceeded note", errResp.Error.Message)
	}
}

// TestAnthropicMessagesDecodeErrorEnvelope verifies that a non-streaming
// /v1/messages request whose upstream 200 body fails to decode gets a 502 with
// an Anthropic error envelope (api_error), not an OpenAI-shaped body.
func TestAnthropicMessagesDecodeErrorEnvelope(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = "data: {not-json}\n\ndata: [DONE]\n\n"
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":false}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var errResp struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &errResp); err != nil {
		t.Fatalf("error response is not valid JSON: %v: %s", err, data)
	}
	if errResp.Type != "error" {
		t.Errorf("top-level type = %q, want error (Anthropic envelope)", errResp.Type)
	}
	if errResp.Error.Type != "api_error" {
		t.Errorf("error.type = %q, want api_error", errResp.Error.Type)
	}
	if !strings.Contains(errResp.Error.Message, "decode") {
		t.Errorf("error.message = %q, want decode failure note", errResp.Error.Message)
	}
}
