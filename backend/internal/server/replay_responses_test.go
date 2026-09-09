package server_test

// Replay tests for the OpenAI Responses API surface (/v1/responses) in the
// exact wire shape the codex CLI (openai/codex) carries: Responses-API-
// ONLY client, Bearer auth, request = model + instructions + input[]
// (input_text / function_call / function_call_output history), tools with
// strict:false, tool_choice "auto", parallel_tool_calls true,
// reasoning:{effort}, include:["reasoning.encrypted_content"], store:false,
// stream:true — and the stream must TERMINATE on response.completed with no
// [DONE] sentinel (codex has no [DONE] handling; an EOF before
// response.completed is a hard client error "stream closed before
// response.completed"). Where the implementation deviates from the
// documented wire contract the test records it as a finding (no prod fixes
// here — this is a replay suite, not a patch box).

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// collectResponsesEvents parses a /v1/responses SSE body into the ordered
// event payloads. Comments (": ...") and data lines that are not JSON
// objects (e.g. "[DONE]") are skipped; each JSON data: line is one event.
func collectResponsesEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rawLine := range strings.Split(body, "\n") {
		line := strings.TrimSpace(rawLine)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var dm map[string]any
		if err := json.Unmarshal([]byte(payload), &dm); err != nil {
			continue
		}
		out = append(out, dm)
	}
	return out
}

// eventTypes extracts the ordered event-type list from parsed Responses
// events.
func eventTypes(events []map[string]any) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		if t, _ := ev["type"].(string); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// indexOfType returns the index of the first event with the wanted type, or
// -1.
func indexOfType(events []map[string]any, typ string) int {
	for i, ev := range events {
		if t, _ := ev["type"].(string); t == typ {
			return i
		}
	}
	return -1
}

// codexResponsesBody builds a codex-shaped /v1/responses request body: full
// input[] history (input_text user turns, a prior function_call item and
// its function_call_output result), function tools with strict:false,
// tool_choice "auto", parallel_tool_calls true, reasoning.effort,
// include ["reasoning.encrypted_content"], store false, stream true.
func codexResponsesBody(model, instructions, effort string) string {
	input := `[
  {"role":"user","content":[{"type":"input_text","text":"What is the weather in San Francisco?"}]},
  {"type":"function_call","call_id":"call_prev_1","name":"get_weather","arguments":"{\"city\":\"SF\"}"},
  {"type":"function_call_output","call_id":"call_prev_1","output":"{\"temp\":68,\"conditions\":\"sunny\"}"},
  {"role":"user","content":[{"type":"input_text","text":"Thanks. Now re-check it right now."}]}
]`
	return `{"model":"` + model + `",` +
		`"instructions":"` + instructions + `",` +
		`"input":` + input + `,` +
		`"tools":[{"type":"function","name":"get_weather","description":"Get current weather for a city","strict":false,"parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],` +
		`"tool_choice":"auto",` +
		`"parallel_tool_calls":true,` +
		`"reasoning":{"effort":"` + effort + `"},` +
		`"include":["reasoning.encrypted_content"],` +
		`"store":false,` +
		`"stream":true,` +
		`"prompt_cache_key":"sess-codex-123",` +
		`"client_metadata":{"x-codex-turn-metadata":"{\"turn\":1}"}}`
}

// TestReplayCodexResponsesSse mirrors replay-codex-responses-sse: the codex
// wire shape (input[] with function_call + function_call_output history,
// tools strict:false, tool_choice auto, parallel_tool_calls true,
// reasoning.effort, include reasoning.encrypted_content, store false,
// stream true) must stream response.created → output_text.delta →
// function-call argument deltas → output_item.done(function_call) →
// response.completed carrying the usage shape, and MUST NOT require or
// emit a [DONE] sentinel. The upstream mock serves chat chunks with a
// tool call split across fragments plus a final usage chunk, exactly the
// upstream the proxy translates from.
func TestReplayCodexResponsesSse(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-cx1", 200,
			`"choices":[{"index":0,"delta":{"content":"Checking the forecast"},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-cx1", 200,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-cx1", 200,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"SF\"}"}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-cx1", 200,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-cx1", 200,
			`"choices":[],"usage":{"prompt_tokens":1234,"completion_tokens":56,"total_tokens":1290,"completion_tokens_details":{"reasoning_tokens":12}}`)))
		// No upstream [DONE]: the relay must still terminate the client
		// stream with response.completed on upstream EOF — codex has no
		// [DONE] handling and treats EOF-before-completed as a hard error.
	}
	ts, _ := newTestServer(t, nil, mock)

	body := codexResponsesBody(modelA, "You are a helpful coding agent.", "medium")
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	sse := string(data)
	if !strings.Contains(sse, ": connecting") {
		t.Error("stream missing ': connecting' grace-flush comment")
	}
	if strings.Contains(sse, "[DONE]") {
		t.Error("stream must not relay [DONE] to a Responses client (codex would parse it as JSON)")
	}

	events := collectResponsesEvents(t, sse)
	if len(events) == 0 {
		t.Fatalf("no SSE events parsed from body: %q", truncate(sse, 400))
	}
	types := eventTypes(events)

	// Terminal contract: response.completed is emitted, and it is the LAST
	// event — the proxy terminates the stream without a [DONE] sentinel.
	if indexOfType(events, "response.completed") == -1 {
		t.Fatal("stream missing terminal response.completed")
	}
	if last := types[len(types)-1]; last != "response.completed" {
		t.Errorf("last event = %q, want response.completed; full sequence: %v", last, types)
	}
	if strings.Contains(sse, `"type":"response.failed"`) {
		t.Error("stream emitted response.failed for a healthy upstream")
	}

	// Ordered skeleton: response.created first, output_text.delta before
	// the tool-call argument deltas, output_item.done before completed.
	created := indexOfType(events, "response.created")
	textDelta := indexOfType(events, "response.output_text.delta")
	argsDelta := indexOfType(events, "response.function_call_arguments.delta")
	itemDone := indexOfType(events, "response.output_item.done")
	completed := indexOfType(events, "response.completed")
	if created != 0 {
		t.Errorf("response.created index = %d, want 0 (first event)", created)
	}
	if created >= textDelta || textDelta >= argsDelta || argsDelta >= itemDone || itemDone >= completed {
		t.Errorf("event order = %v, want response.created < output_text.delta < function_call_arguments.delta < output_item.done < response.completed", types)
	}

	// Streamed argument fragments must arrive as deltas: the legacy event
	// name (function_call_arguments.*, consumed by non-codex Responses
	// clients) and the codex-consumed name (custom_tool_call_input.*) both
	// carry the same fragments in the same order.
	var argsFragments []string
	for _, ev := range events {
		if t, _ := ev["type"].(string); t == "response.function_call_arguments.delta" {
			if d, ok := ev["delta"].(string); ok {
				argsFragments = append(argsFragments, d)
			}
		}
	}
	if len(argsFragments) != 2 || strings.Join(argsFragments, "") != `{"city":"SF"}` {
		t.Errorf("argument deltas = %v, want two fragments joining to {\"city\":\"SF\"}", argsFragments)
	}

	// Streamed argument fragments MUST also arrive as the codex-consumed
	// event name (response.custom_tool_call_input.delta): codex dispatches
	// tool calls from the completed output_item.done, but its stream parser
	// reads deltas under the custom_tool_call name and ignores the legacy
	// function_call_arguments.* family.
	var customArgsFragments []string
	for _, ev := range events {
		if t, _ := ev["type"].(string); t == "response.custom_tool_call_input.delta" {
			if d, ok := ev["delta"].(string); ok {
				customArgsFragments = append(customArgsFragments, d)
			}
		}
	}
	if len(customArgsFragments) == 0 && len(argsFragments) != 0 {
		t.Error("argument fragments missing custom_tool_call_input.delta events (codex's consumed event name)")
	}
	if len(customArgsFragments) != 2 || strings.Join(customArgsFragments, "") != `{"city":"SF"}` {
		t.Errorf("custom_tool_call_input deltas = %v, want two fragments joining to {\"city\":\"SF\"}", customArgsFragments)
	}

	// output_item.done carries the complete function_call (name restored to
	// the client's dispatch name, call id and the whole arguments string) —
	// this is what codex actually dispatches from.
	var itemDoneFunctionCall bool
	var itemName, itemCallID, itemArgs string
	for _, ev := range events {
		if t, _ := ev["type"].(string); t == "response.output_item.done" {
			item, _ := ev["item"].(map[string]any)
			if item == nil {
				continue
			}
			if it, _ := item["type"].(string); it == "function_call" {
				itemDoneFunctionCall = true
				itemName, _ = item["name"].(string)
				itemCallID, _ = item["call_id"].(string)
				itemArgs, _ = item["arguments"].(string)
			}
		}
	}
	if !itemDoneFunctionCall {
		t.Fatal("output_item.done never carried a function_call item")
	}
	if itemName != "get_weather" || itemCallID != "call_1" || itemArgs != `{"city":"SF"}` {
		t.Errorf("output_item.done function_call = name %q call_id %q arguments %q, want get_weather/call_1/{\"city\":\"SF\"}",
			itemName, itemCallID, itemArgs)
	}

	// response.completed must carry the id, the model, status completed,
	// the output items, and the usage in Responses shape.
	var completedResp map[string]any
	for _, ev := range events {
		if t, _ := ev["type"].(string); t == "response.completed" {
			completedResp, _ = ev["response"].(map[string]any)
		}
	}
	if completedResp == nil {
		t.Fatal("response.completed missing response object")
		return
	}
	id, _ := completedResp["id"].(string)
	if !strings.HasPrefix(id, "resp_") || len(id) < 6 {
		t.Errorf("completed response id = %q, want resp_<random>", id)
	}
	if model, _ := completedResp["model"].(string); model != modelA {
		t.Errorf("completed response model = %q, want %q", model, modelA)
	}
	if status, _ := completedResp["status"].(string); status != "completed" {
		t.Errorf("completed response status = %q, want completed", status)
	}
	out, _ := completedResp["output"].([]any)
	if len(out) != 2 {
		t.Fatalf("completed response output len = %d, want 2 (message + function_call): %v", len(out), out)
	}
	foundFC, foundMsg := false, false
	for _, o := range out {
		item, _ := o.(map[string]any)
		if item == nil {
			continue
		}
		switch item["type"] {
		case "function_call":
			foundFC = true
			if item["name"] != "get_weather" || item["call_id"] != "call_1" || item["arguments"] != `{"city":"SF"}` {
				t.Errorf("completed function_call = %v, want get_weather/call_1/{\"city\":\"SF\"}", item)
			}
		case "message":
			foundMsg = true
			content, _ := item["content"].([]any)
			if len(content) != 1 {
				t.Errorf("completed message content = %v, want one output_text part", content)
			}
		}
	}
	if !foundFC || !foundMsg {
		t.Errorf("completed output missing function_call=%v message=%v", foundFC, foundMsg)
	}
	usage, _ := completedResp["usage"].(map[string]any)
	if usage == nil {
		t.Fatal("response.completed missing usage (codex parses usage from it)")
		return
	}
	if usage["input_tokens"] != float64(1234) || usage["output_tokens"] != float64(56) {
		t.Errorf("usage tokens = %v, want input 1234 output 56", usage)
	}
	od, _ := usage["output_tokens_details"].(map[string]any)
	if od == nil || od["reasoning_tokens"] != float64(12) {
		t.Errorf("usage output_tokens_details = %v, want reasoning_tokens 12", od)
	}

	// The translated upstream chat body must carry the codex request:
	// instructions as system, the function_call_output as a tool message
	// with its call id, the wrapped function tool, tool_choice auto,
	// parallel_tool_calls, reasoning_effort, store.
	if !mock.BodyContains("you are a helpful coding agent") && !mock.BodyContains("You are a helpful coding agent") {
		t.Error("upstream body missing instructions")
	}
	if !mock.BodyContains(`"role":"system"`) {
		t.Error("upstream body missing system message")
	}
	if !mock.BodyContains(`"role":"tool"`) || !mock.BodyContains(`"tool_call_id":"call_prev_1"`) {
		t.Error("upstream body missing function_call_output → tool message")
	}
	if !mock.BodyContains(`"get_weather"`) || !mock.BodyContains(`"tool_choice":"auto"`) {
		t.Error("upstream body missing tool / tool_choice auto")
	}
	if !mock.BodyContains(`"parallel_tool_calls":true`) {
		t.Error("upstream body missing parallel_tool_calls passthrough")
	}
	if !mock.BodyContains(`"reasoning_effort":"high"`) {
		// codex requests effort "medium"; deepseek-v4-flash's effort ladder is
		// {'high'}, so the proxy clamps medium → high on the wire (CLI
		// resolveFreebuffReasoningEffort semantics, #112): the request is
		// honored, the wire carries the nearest allowed rung.
		t.Error("upstream body missing reasoning_effort (want clamped high for deepseek-v4-flash)")
	}
	if !mock.BodyContains(`"store":false`) {
		t.Error("upstream body missing store: false passthrough")
	}
}

// TestReplayCodexResponsesReasoning mirrors replay-codex-responses-reasoning:
// codex with reasoning.effort + include:["reasoning.encrypted_content"] must
// be ACCEPTED (no 400 on unknown include values), the stream must tolerate
// upstream reasoning_content deltas and terminate on response.completed,
// and the usage must carry output_tokens_details.reasoning_tokens.
func TestReplayCodexResponsesReasoning(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-cx2", 201,
			`"choices":[{"index":0,"delta":{"reasoning_content":"Let me reason about this step by step"},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-cx2", 201,
			`"choices":[{"index":0,"delta":{"content":"The answer is 42"},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-cx2", 201,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":90,"completion_tokens":40,"total_tokens":130,"completion_tokens_details":{"reasoning_tokens":13}}`)))
		// No upstream [DONE]: the relay terminates via upstream EOF.
	}
	ts, _ := newTestServer(t, nil, mock)

	body := codexResponsesBody(modelA, "You are a helpful coding agent.", "high")
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (reasoning.effort + include must not 400): %s", resp.StatusCode, truncate(string(data), 300))
	}
	sse := string(data)
	if !strings.Contains(sse, `"type":"response.completed"`) {
		t.Fatalf("stream missing terminal response.completed: %q", truncate(sse, 400))
	}
	events := collectResponsesEvents(t, sse)
	if last := eventTypes(events)[len(events)-1]; last != "response.completed" {
		t.Errorf("last event = %q, want response.completed", last)
	}
	if strings.Contains(sse, `"type":"response.failed"`) {
		t.Error("stream emitted response.failed")
	}
	// Upstream reasoning_content now surfaces as first-class reasoning
	// events (response.reasoning_text.delta) — assert the split: reasoning
	// text lives ONLY in reasoning deltas, output_text carries ONLY the
	// answer, and reasoning precedes the answer.
	var reasoningDeltas, textDeltas []string
	for _, ev := range events {
		switch t, _ := ev["type"].(string); t {
		case "response.reasoning_text.delta":
			if d, ok := ev["delta"].(string); ok {
				reasoningDeltas = append(reasoningDeltas, d)
			}
		case "response.output_text.delta":
			if d, ok := ev["delta"].(string); ok && d != "" {
				textDeltas = append(textDeltas, d)
			}
		}
	}
	if len(reasoningDeltas) != 1 || reasoningDeltas[0] != "Let me reason about this step by step" {
		t.Errorf("reasoning_text deltas = %v, want the full reasoning content", reasoningDeltas)
	}
	if len(textDeltas) != 1 || textDeltas[0] != "The answer is 42" {
		t.Errorf("output_text deltas = %v, want exactly [The answer is 42]", textDeltas)
	}
	firstReasoning := indexOfType(events, "response.reasoning_text.delta")
	firstText := indexOfType(events, "response.output_text.delta")
	if firstReasoning == -1 || firstText == -1 || firstReasoning >= firstText {
		t.Errorf("reasoning delta index = %d, output_text delta index = %d, want reasoning before answer",
			firstReasoning, firstText)
	}
	// Usage carries the reasoning-token accounting in Responses shape.
	var completedResp map[string]any
	for _, ev := range events {
		if t, _ := ev["type"].(string); t == "response.completed" {
			completedResp, _ = ev["response"].(map[string]any)
		}
	}
	if completedResp == nil {
		t.Fatal("response.completed missing response object")
		return
	}
	usage, _ := completedResp["usage"].(map[string]any)
	if usage == nil {
		t.Fatal("response.completed missing usage")
		return
	}
	if usage["input_tokens"] != float64(90) || usage["output_tokens"] != float64(40) {
		t.Errorf("usage = %v, want input 90 output 40", usage)
	}
	od, _ := usage["output_tokens_details"].(map[string]any)
	if od == nil || od["reasoning_tokens"] != float64(13) {
		t.Errorf("usage output_tokens_details = %v, want reasoning_tokens 13", od)
	}
	// The translated chat body must carry reasoning_effort high.
	if !mock.BodyContains(`"reasoning_effort":"high"`) {
		t.Error("upstream body missing reasoning_effort: high")
	}
}

// TestReplayCodexResponses401 mirrors replay-codex-responses-401: a missing
// or blank bearer on /v1/responses must yield a CLEAN 401 JSON error (codex
// maps 401 to a recoverable class and would otherwise fall into the ChatGPT
// login flow) — never a login redirect, never an HTML page — and must not
// touch the upstream at all.
func TestReplayCodexResponses401(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newBridgeTestServer(t, mock)

	body := `{"model":"` + modelA + `","input":"ping","stream":true}`
	// 1. No Authorization header.
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no bearer: status = %d, want 401: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Errorf("no bearer: Location = %q, want empty (401 is JSON, not a login redirect)", loc)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("no bearer: Content-Type = %q, want application/json", ct)
	}
	var errResp struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &errResp); err != nil {
		t.Fatalf("no bearer: body is not JSON: %v (%q)", err, truncate(string(data), 200))
	}
	if errResp.Error.Code != "missing_bearer_token" {
		t.Errorf("no bearer: error.code = %q, want missing_bearer_token", errResp.Error.Code)
	}
	if errResp.Error.Message == "" {
		t.Error("no bearer: error.message empty")
	}

	// 2. Blank bearer ("Bearer " with no token).
	resp2, data2 := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body),
		map[string]string{"Authorization": "Bearer "})
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("blank bearer: status = %d, want 401: %s", resp2.StatusCode, truncate(string(data2), 200))
	}
	if loc := resp2.Header.Get("Location"); loc != "" {
		t.Errorf("blank bearer: Location = %q, want empty", loc)
	}
	if !strings.Contains(string(data2), "missing_bearer_token") {
		t.Errorf("blank bearer: body missing missing_bearer_token: %s", truncate(string(data2), 200))
	}

	// The gateway must reject before any upstream contact.
	if mock.RequestsSnapshot() != 0 {
		t.Errorf("upstream requests = %d, want 0 (401 rejected before pool/bridge)", mock.RequestsSnapshot())
	}
}

// TestResponsesIncludeIgnoredContract pins the include-key contract around
// reasoning.encrypted_content: a /v1/responses request carrying
// include:["reasoning.encrypted_content"] + store:false must be ACCEPTED
// (no 400 — the include value is an ignored, documented no-op) and must
// NEVER leak the encrypted_content wire concern downstream: the SSE stream
// must not contain the substring "encrypted_content", response.completed
// must terminate the stream, and the reasoning item's output_item.done must
// be spec-shaped (id + summary + content[{type:reasoning_text,text}]) with
// no encrypted_content field. Grounded in
// reference/harnesses/codex/WIRE-NOTES.md:158 ("include:
// ['reasoning.encrypted_content'] is always sent; the encrypted_content is
// optional on the Reasoning item, so a proxy may omit it.") and the
// reference reasoning output_item.done shape
// (codex protocol/src/models.rs:1937-1945).
func TestResponsesIncludeIgnoredContract(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-cx3", 200,
			`"choices":[{"index":0,"delta":{"reasoning_content":"We reason here before answering"},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-cx3", 200,
			`"choices":[{"index":0,"delta":{"content":"The answer is 42"},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-cx3", 200,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"completion_tokens_details":{"reasoning_tokens":20}}`)))
		// No upstream [DONE]: the relay terminates via upstream EOF.
	}
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","input":"What is 2+2?","include":["reasoning.encrypted_content"],"store":false,"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (include reasoning.encrypted_content must not 400): %s", resp.StatusCode, truncate(string(data), 300))
	}
	sse := string(data)

	// (a) The encrypted_content concern never reaches the SSE wire.
	if strings.Contains(sse, "encrypted_content") {
		t.Error("downstream SSE leaked 'encrypted_content' — the include key must be an ignored no-op")
	}
	if strings.Contains(sse, "[DONE]") {
		t.Error("stream must not relay [DONE] to a Responses client")
	}

	events := collectResponsesEvents(t, sse)
	if len(events) == 0 {
		t.Fatalf("no SSE events parsed from body: %q", truncate(sse, 400))
	}

	// (b) The request succeeds and terminates on response.completed with id.
	if !strings.Contains(sse, `"type":"response.completed"`) {
		t.Fatalf("stream missing terminal response.completed: %q", truncate(sse, 400))
	}
	if last := eventTypes(events)[len(events)-1]; last != "response.completed" {
		t.Errorf("last event = %q, want response.completed", last)
	}
	var completedResp map[string]any
	for _, ev := range events {
		if t, _ := ev["type"].(string); t == "response.completed" {
			completedResp, _ = ev["response"].(map[string]any)
		}
	}
	if completedResp == nil {
		t.Fatal("response.completed missing response object")
		return
	}
	if id, _ := completedResp["id"].(string); !strings.HasPrefix(id, "resp_") || len(id) < 6 {
		t.Errorf("completed response id = %q, want resp_<random>", id)
	}

	// (c) The reasoning item's output_item.done is spec-shaped (id +
	// summary + content[{type:reasoning_text,text}]) with no
	// encrypted_content field — a proxy may omit encrypted_content
	// (reference/harnesses/codex/WIRE-NOTES.md:158).
	var foundReasoning bool
	for _, ev := range events {
		if t, _ := ev["type"].(string); t != "response.output_item.done" {
			continue
		}
		item, _ := ev["item"].(map[string]any)
		if item == nil {
			continue
		}
		if it, _ := item["type"].(string); it != "reasoning" {
			continue
		}
		foundReasoning = true
		if id, _ := item["id"].(string); id == "" {
			t.Error("reasoning output_item.done missing id")
		}
		summary, ok := item["summary"].([]any)
		if !ok || len(summary) != 0 {
			t.Errorf("reasoning summary = %v, want an empty array", item["summary"])
		}
		content, ok := item["content"].([]any)
		if !ok || len(content) != 1 {
			t.Errorf("reasoning content = %v, want one reasoning_text part", item["content"])
			continue
		}
		part, _ := content[0].(map[string]any)
		if part == nil || part["type"] != "reasoning_text" {
			t.Errorf("reasoning content part = %v, want type reasoning_text", part)
			continue
		}
		if text, _ := part["text"].(string); text != "We reason here before answering" {
			t.Errorf("reasoning text = %q, want the full reasoning content", text)
		}
		if _, has := item["encrypted_content"]; has {
			t.Error("reasoning output_item.done must not carry encrypted_content")
		}
	}
	if !foundReasoning {
		t.Fatal("no reasoning output_item.done in stream")
	}
}
