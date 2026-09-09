package server

// Output-layer completeness tests: first-chunk role injection on the OpenAI
// chat wire, Anthropic stop_reason mapping for content_filter (→ refusal)
// on both stream and JSON paths, the Responses function-call done event and
// its usage detail mapping. These drive the relays directly with scripted
// SSE readers — no network/timing flakiness.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
)

// TestOpenAIStreamFirstChunkRoleInjected pins the first-chunk role
// guarantee: the OpenAI spec stamps "role":"assistant" in the first chunk
// of a chat completion stream, but an upstream that omits it (e.g. when
// the first delta is pure content) would leave clients without a role.
func TestOpenAIStreamFirstChunkRoleInjected(t *testing.T) {
	t.Run("missing role injected on first chunk only", func(t *testing.T) {
		s := testRelayServer()
		rec := httptest.NewRecorder()
		ss := strings.Join([]string{
			testutilSSE(`{"id":"chatcmpl-r1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`),
			testutilSSE(`{"id":"chatcmpl-r1","choices":[{"index":0,"delta":{"content":" there"},"finish_reason":null}]}`),
			testutilSSE(`{"id":"chatcmpl-r1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		}, "")
		s.relayStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now())
		frames := collectSSEFrames(t, rec.Body.String())
		if len(frames) < 3 {
			t.Fatalf("frames = %d, want >= 3", len(frames))
		}
		first, _ := frames[0].data["choices"].([]any)
		firstChoice, _ := first[0].(map[string]any)
		firstDelta, _ := firstChoice["delta"].(map[string]any)
		if firstDelta["role"] != "assistant" {
			t.Errorf("first chunk delta.role = %v, want assistant", firstDelta["role"])
		}
		if firstDelta["content"] != "hi" {
			t.Errorf("first chunk content = %v, want 'hi'", firstDelta["content"])
		}
		// Later chunks must NOT get a role injected (and keep their bytes
		// otherwise).
		second, _ := frames[1].data["choices"].([]any)
		secondChoice, _ := second[0].(map[string]any)
		secondDelta, _ := secondChoice["delta"].(map[string]any)
		if _, has := secondDelta["role"]; has {
			t.Errorf("later chunk got role injected: %v", secondDelta)
		}
	})

	t.Run("existing role untouched", func(t *testing.T) {
		s := testRelayServer()
		rec := httptest.NewRecorder()
		ss := testutilSSE(`{"id":"chatcmpl-r2","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`)
		s.relayStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now())
		frames := collectSSEFrames(t, rec.Body.String())
		first, _ := frames[0].data["choices"].([]any)
		firstChoice, _ := first[0].(map[string]any)
		firstDelta, _ := firstChoice["delta"].(map[string]any)
		if firstDelta["role"] != "assistant" {
			t.Errorf("existing role clobbered: %v", firstDelta["role"])
		}
	})

	t.Run("usage-only first chunk does not consume the role slot", func(t *testing.T) {
		s := testRelayServer()
		rec := httptest.NewRecorder()
		ss := strings.Join([]string{
			testutilSSE(`{"id":"chatcmpl-r3","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`),
			testutilSSE(`{"id":"chatcmpl-r3","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`),
		}, "")
		s.relayStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now())
		frames := collectSSEFrames(t, rec.Body.String())
		if len(frames) != 2 {
			t.Fatalf("frames = %d, want 2", len(frames))
		}
		second, _ := frames[1].data["choices"].([]any)
		secondChoice, _ := second[0].(map[string]any)
		secondDelta, _ := secondChoice["delta"].(map[string]any)
		if secondDelta["role"] != "assistant" {
			t.Errorf("content chunk after usage-only frame missing role: %v", secondDelta)
		}
	})
}

// TestAnthropicStreamInBandErrorChunk pins the in-band upstream error
// path: an OpenAI-shaped error chunk mid-stream must surface as an
// Anthropic error event and terminate — the client must never see a
// successfully completed message for a failed upstream turn.
func TestAnthropicStreamInBandErrorChunk(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()
	ss := strings.Join([]string{
		testutilSSE(`{"id":"chatcmpl-e1","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`),
		testutilSSE(`{"error":{"message":"upstream boom","type":"server_error","code":"e500"}}`),
		testutilSSE(`{"id":"chatcmpl-e1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
	}, "")
	s.relayAnthropicStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now(), "m", 0)
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("stream missing error event: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, "upstream boom") {
		t.Errorf("error event missing upstream message: %q", truncateStr(body, 400))
	}
	// A terminated-by-error stream must not claim a completed message.
	if strings.Contains(body, `"type":"message_stop"`) {
		t.Errorf("message_stop emitted after error: %q", truncateStr(body, 400))
	}
	if strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Errorf("message_delta stop_reason end_turn after error: %q", truncateStr(body, 400))
	}
}

// TestChatStreamUpstreamErrorEnvelope pins the OpenAI chat streaming error
// contract at both failure points, the /v1/chat/completions counterpart of
// TestAnthropicStreamInBandErrorChunk:
//
//   - Pre-stream (upstream refuses admission): the response is a plain HTTP
//     status error carrying the OpenAI JSON error envelope + Retry-After —
//     never SSE-framed (no stream was ever committed, so a 200 envelope
//     would hide the status code and Retry-After from OpenAI SDK clients,
//     which parse the body only after seeing a 2xx) and never the Anthropic
//     envelope on the OpenAI surface.
//   - Mid-stream (upstream dies after committing the stream): the client
//     gets the in-band OpenAI error frame (code "upstream_stream_error",
//     message "upstream stream interrupted: ...") and then [DONE] — the
//     failed turn is never dressed up as a completed one with a synthesized
//     finish_reason, and no Anthropic envelope vocabulary leaks.
func TestChatStreamUpstreamErrorEnvelope(t *testing.T) {
	t.Run("pre-stream 429 is a JSON status error, not SSE", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"status":"rate_limited","message":"Daily session quota exhausted","retryAfterMs":13000}`))
		}
		srv := newServer(t, mock, nil)
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)

		client := &http.Client{Timeout: 10 * time.Second}
		body := `{"model":"` + testModelA + `","messages":[{"role":"user","content":"ping"}],"stream":true}`
		resp, err := client.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		data := string(raw)

		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429: %s", resp.StatusCode, truncateStr(data, 300))
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q, want application/json (the stream never started)", ct)
		}
		if ra := resp.Header.Get("Retry-After"); ra != "13" {
			t.Errorf("Retry-After = %q, want 13 (ceil of retryAfterMs 13000)", ra)
		}
		// No SSE framing may leak: a "data:"-framed error body would parse
		// as garbage for a client that never got a 2xx stream commitment.
		if strings.Contains(data, "data: ") || strings.Contains(data, "[DONE]") {
			t.Errorf("pre-stream error must not be SSE-framed: %s", truncateStr(data, 300))
		}
		var env struct {
			Error struct {
				Message string  `json:"message"`
				Type    string  `json:"type"`
				Param   *string `json:"param"`
				Code    string  `json:"code"`
			} `json:"error"`
			TopLevelType string `json:"type"` // Anthropic envelope sentinel
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("body is not OpenAI error JSON: %v: %s", err, truncateStr(data, 300))
		}
		if env.Error.Code != "rate_limited" {
			t.Errorf("error.code = %q, want rate_limited", env.Error.Code)
		}
		if env.Error.Type != "upstream_error" {
			t.Errorf("error.type = %q, want upstream_error", env.Error.Type)
		}
		if env.Error.Message == "" || env.Error.Param != nil {
			t.Errorf("error message/param wrong: %+v", env.Error)
		}
		if env.TopLevelType == "error" {
			t.Errorf("Anthropic envelope leaked on the OpenAI surface: %s", truncateStr(data, 300))
		}
	})

	t.Run("mid-stream abort emits error frame then DONE, no synthesized turn end", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`data: {"id":"chatcmpl-mid","object":"chat.completion.chunk","created":1,"model":"` + testModelA + `","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}` + "\n\n"))
			w.(http.Flusher).Flush()
			// Abort the connection after committing the stream: the proxy
			// sees a mid-body read failure, not a clean EOF.
			panic(http.ErrAbortHandler)
		}
		srv := newServer(t, mock, nil)
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)

		client := &http.Client{Timeout: 10 * time.Second}
		body := `{"model":"` + testModelA + `","messages":[{"role":"user","content":"ping"}],"stream":true}`
		resp, err := client.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		data := string(raw)

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (the stream was already committed): %s", resp.StatusCode, truncateStr(data, 300))
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
			t.Errorf("Content-Type = %q, want text/event-stream", ct)
		}
		if !strings.Contains(data, "partial") {
			t.Errorf("first chunk lost before the abort: %s", truncateStr(data, 300))
		}
		// The in-band error frame carries the OpenAI error shape with the
		// transport code and the interrupted-stream message.
		var errPayload map[string]any
		for _, f := range collectSSEFrames(t, data) {
			if e, ok := f.data["error"].(map[string]any); ok {
				errPayload = e
				break
			}
		}
		if errPayload == nil {
			t.Fatalf("no error frame in stream: %s", truncateStr(data, 400))
			return
		}
		if errPayload["type"] != "upstream_error" {
			t.Errorf("error.type = %v, want upstream_error", errPayload["type"])
		}
		if errPayload["code"] != "upstream_stream_error" {
			t.Errorf("error.code = %v, want upstream_stream_error", errPayload["code"])
		}
		if msg, _ := errPayload["message"].(string); !strings.HasPrefix(msg, "upstream stream interrupted: ") {
			t.Errorf("error.message = %q, want 'upstream stream interrupted: ...'", msg)
		}
		// [DONE] terminates the stream, after the error frame.
		errIdx := strings.Index(data, `"upstream_stream_error"`)
		doneIdx := strings.Index(data, "data: [DONE]")
		if errIdx < 0 || doneIdx < 0 || errIdx > doneIdx {
			t.Fatalf("error frame must precede [DONE]: errIdx=%d doneIdx=%d body=%s", errIdx, doneIdx, truncateStr(data, 400))
		}
		if !strings.HasSuffix(data, "data: [DONE]\n\n") {
			t.Errorf("stream must terminate with [DONE]: %q", truncateStr(data, 200))
		}
		// A failed turn is never dressed up as a completed one: no
		// synthesized finish_reason (OpenAI vocabulary) and no Anthropic
		// envelope vocabulary on the OpenAI surface.
		if strings.Contains(data, `"finish_reason":"stop"`) || strings.Contains(data, `"finish_reason":"tool_calls"`) {
			t.Errorf("finish_reason synthesized after upstream error: %s", truncateStr(data, 400))
		}
		if strings.Contains(data, `"stop_reason"`) || strings.Contains(data, `"type":"error"`) || strings.Contains(data, "message_stop") {
			t.Errorf("Anthropic envelope vocabulary leaked on the OpenAI surface: %s", truncateStr(data, 400))
		}
	})
}

// TestAnthropicStreamContentFilterMapsToRefusal pins the stop_reason
// mapping: an upstream OpenAI "content_filter" finish reason must surface
// as the Anthropic "refusal" StopReason (there is no "content_filter" in
// the Anthropic vocabulary), with output_tokens usage still attached.
func TestAnthropicStreamContentFilterMapsToRefusal(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()
	ss := strings.Join([]string{
		testutilSSE(`{"id":"chatcmpl-cf","choices":[{"index":0,"delta":{"content":"I can't help with that"},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-cf","choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}}`),
	}, "")
	s.relayAnthropicStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now(), "m", 0)
	var delta map[string]any
	found := false
	for _, ev := range collectSSEFrames(t, rec.Body.String()) {
		if ev.data["type"] == "message_delta" {
			delta, _ = ev.data["delta"].(map[string]any)
			found = true
		}
	}
	if !found {
		t.Fatal("message_delta not emitted")
	}
	if delta["stop_reason"] != "refusal" {
		t.Errorf("stop_reason = %v, want refusal", delta["stop_reason"])
	}
}

// TestAnthropicJSONContentFilterMapsToRefusal pins the same mapping on the
// non-streaming /v1/messages path.
func TestAnthropicJSONContentFilterMapsToRefusal(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	ss := strings.Join([]string{
		testutilSSE(`{"id":"chatcmpl-cj","choices":[{"index":0,"delta":{"content":"I can't help with that"},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-cj","choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}`),
	}, "")
	s.relayAnthropicJSON(context.Background(), rec, r, strings.NewReader(ss), &relayStats{}, time.Now(), "m")
	var msg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("response not JSON: %v: %s", err, truncateStr(rec.Body.String(), 400))
	}
	if msg["stop_reason"] != "refusal" {
		t.Errorf("stop_reason = %v, want refusal", msg["stop_reason"])
	}
}

// TestResponsesStreamFunctionCallArgumentsDoneSequence pins the spec's
// function-call output-item sequence: output_item.added,
// function_call_arguments.delta (per fragment), function_call_arguments.done
// with the final assembled arguments, then output_item.done.
func TestResponsesStreamFunctionCallArgumentsDoneSequence(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()
	ss := strings.Join([]string{
		testutilSSE(`{"id":"chatcmpl-fc","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":"}}]},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-fc","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"pwd\"}"}}]},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-fc","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
	}, "")
	s.relayResponsesStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now(), "m", "resp_fc")
	var seq []string
	var done map[string]any
	for _, ev := range collectSSEFrames(t, rec.Body.String()) {
		seq = append(seq, ev.data["type"].(string))
		if ev.data["type"] == "response.function_call_arguments.done" {
			done = ev.data
		}
	}
	// The done event must land before the item's output_item.done.
	doneIdx, itemIdx := -1, -1
	for i, typ := range seq {
		if typ == "response.function_call_arguments.done" {
			doneIdx = i
		}
		if typ == "response.output_item.done" {
			itemIdx = i
		}
	}
	if doneIdx < 0 {
		t.Fatal("function_call_arguments.done not emitted")
	}
	if itemIdx < 0 || doneIdx > itemIdx {
		t.Errorf("args.done (idx %d) must precede output_item.done (idx %d)", doneIdx, itemIdx)
	}
	if done == nil || done["call_id"] != "call_1" || done["name"] != "bash" {
		t.Errorf("args.done = %v, want call_id call_1 name bash", done)
	}
	if done["arguments"] != `{"command":"pwd"}` {
		t.Errorf("args.done arguments = %v, want assembled %q", done["arguments"], `{"command":"pwd"}`)
	}
}

// TestResponsesJSONUsageCarriesDetails pins the usage translation on the
// non-streaming Responses path: OpenAI-shaped detail keys
// (prompt_tokens_details / completion_tokens_details) must surface in the
// Responses-native input/output details.
func TestResponsesJSONUsageCarriesDetails(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()
	ss := strings.Join([]string{
		testutilSSE(`{"id":"chatcmpl-ud","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-ud","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":8,"total_tokens":28,"prompt_tokens_details":{"cached_tokens":7},"completion_tokens_details":{"reasoning_tokens":3}}}`),
	}, "")
	s.relayResponsesJSON(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now(), "m", "resp_ud")
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage == nil {
		t.Fatal("usage missing")
		return
	}
	inDetails, _ := usage["input_tokens_details"].(map[string]any)
	if inDetails == nil || inDetails["cached_tokens"].(float64) != 7 {
		t.Errorf("input_tokens_details = %v, want cached_tokens 7", inDetails)
	}
	outDetails, _ := usage["output_tokens_details"].(map[string]any)
	if outDetails == nil || outDetails["reasoning_tokens"].(float64) != 3 {
		t.Errorf("output_tokens_details = %v, want reasoning_tokens 3", outDetails)
	}
	if usage["total_tokens"].(float64) != 28 {
		t.Errorf("total_tokens = %v, want 28", usage["total_tokens"])
	}
}
