package server_test

// Replay tests for the two Responses-API translation gaps fixed on the
// streaming path: upstream reasoning_content must be forwarded as spec-valid
// reasoning events (never as output text), and function-call argument
// fragments must be emitted under BOTH delta event names
// (response.function_call_arguments.* for legacy clients and
// response.custom_tool_call_input.* for codex).

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// TestReplayResponsesForwardReasoning covers the reasoning_content gap: an
// upstream chat stream whose delta.reasoning_content carries chain-of-thought
// text must relay it as a first-class reasoning output item —
// output_item.added(reasoning) → response.reasoning_text.delta* →
// response.reasoning_text.done → output_item.done(reasoning) — while the
// message item's output_text carries ONLY the answer. response.completed
// stays last and its output includes the reasoning item with the full text.
func TestReplayResponsesForwardReasoning(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-fw1", 301,
			`"choices":[{"index":0,"delta":{"reasoning_content":"Let me reason about "},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-fw1", 301,
			`"choices":[{"index":0,"delta":{"reasoning_content":"this step by step"},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-fw1", 301,
			`"choices":[{"index":0,"delta":{"content":"The answer is 42"},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-fw1", 301,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":90,"completion_tokens":40,"total_tokens":130,"completion_tokens_details":{"reasoning_tokens":13}}`)))
	}
	ts, _ := newTestServer(t, nil, mock)

	body := codexResponsesBody(modelA, "You are a helpful coding agent.", "high")
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	sse := string(data)
	if !strings.Contains(sse, `"type":"response.reasoning_text.delta"`) {
		t.Fatalf("stream missing reasoning_text.delta events: %q", truncate(sse, 400))
	}
	events := collectResponsesEvents(t, sse)
	types := eventTypes(events)
	if last := types[len(types)-1]; last != "response.completed" {
		t.Fatalf("last event = %q, want response.completed; sequence: %v", last, types)
	}
	if strings.Contains(sse, `"type":"response.failed"`) {
		t.Fatal("stream emitted response.failed for a healthy upstream")
	}

	// The reasoning item is announced (reasoning shape) before the first
	// reasoning delta, and the reasoning deltas precede the MESSAGE item's
	// added event.
	reasoningAdded, msgAdded := -1, -1
	for i, ev := range events {
		if typ, _ := ev["type"].(string); typ != "response.output_item.added" {
			continue
		}
		item, _ := ev["item"].(map[string]any)
		if item == nil {
			continue
		}
		switch it, _ := item["type"].(string); it {
		case "reasoning":
			reasoningAdded = i
		case "message":
			msgAdded = i
		}
	}
	if reasoningAdded == -1 || msgAdded == -1 {
		t.Fatalf("output_item.added reasoning=%d message=%d, want both present", reasoningAdded, msgAdded)
	}

	var reasoningDeltas []string
	for _, ev := range events {
		if typ, _ := ev["type"].(string); typ == "response.reasoning_text.delta" {
			if d, ok := ev["delta"].(string); ok {
				reasoningDeltas = append(reasoningDeltas, d)
			}
			if id, _ := ev["item_id"].(string); !strings.HasPrefix(id, "rs_") {
				t.Errorf("reasoning delta item_id = %q, want rs_ prefix", id)
			}
			if ci, ok := ev["content_index"].(float64); ok && ci != 0 {
				t.Errorf("reasoning delta content_index = %v, want 0", ci)
			}
		}
	}
	if len(reasoningDeltas) != 2 || strings.Join(reasoningDeltas, "") != "Let me reason about this step by step" {
		t.Errorf("reasoning deltas = %v, want two fragments joining to the full reasoning text", reasoningDeltas)
	}

	firstReasoningDelta := indexOfType(events, "response.reasoning_text.delta")
	if reasoningAdded >= firstReasoningDelta || firstReasoningDelta >= msgAdded {
		t.Errorf("event order reasoningAdded(%d) < firstReasoningDelta(%d) < msgAdded(%d) violated; sequence: %v", reasoningAdded, firstReasoningDelta, msgAdded, types)
	}

	// Output text carries ONLY the answer; reasoning text never leaks into
	// output_text.
	var textDeltas []string
	for _, ev := range events {
		if typ, _ := ev["type"].(string); typ == "response.output_text.delta" {
			if d, ok := ev["delta"].(string); ok && d != "" {
				textDeltas = append(textDeltas, d)
			}
		}
	}
	if len(textDeltas) != 1 || textDeltas[0] != "The answer is 42" {
		t.Errorf("output_text deltas = %v, want exactly [The answer is 42]", textDeltas)
	}
	for _, d := range textDeltas {
		if strings.Contains(d, "Let me reason") {
			t.Errorf("reasoning leaked into output_text delta: %q", d)
		}
	}

	// reasoning_text.done carries the full text and precedes the terminal
	// completed; the reasoning item's output_item.done carries it too.
	doneIdx := indexOfType(events, "response.reasoning_text.done")
	completedIdx := indexOfType(events, "response.completed")
	if doneIdx == -1 || doneIdx >= completedIdx {
		t.Errorf("reasoning_text.done index = %d, want before response.completed(%d)", doneIdx, completedIdx)
	}
	var reasoningDoneText, reasoningDoneItemText string
	for _, ev := range events {
		if typ, _ := ev["type"].(string); typ == "response.reasoning_text.done" {
			reasoningDoneText, _ = ev["text"].(string)
		}
		if typ, _ := ev["type"].(string); typ == "response.output_item.done" {
			item, _ := ev["item"].(map[string]any)
			if item == nil {
				continue
			}
			if it, _ := item["type"].(string); it == "reasoning" {
				if content, ok := item["content"].([]any); ok && len(content) == 1 {
					if cp, ok := content[0].(map[string]any); ok {
						reasoningDoneItemText, _ = cp["text"].(string)
					}
				}
			}
		}
	}
	if reasoningDoneText != "Let me reason about this step by step" {
		t.Errorf("reasoning_text.done text = %q, want full reasoning text", reasoningDoneText)
	}
	if reasoningDoneItemText != "Let me reason about this step by step" {
		t.Errorf("reasoning output_item.done content text = %q, want full reasoning text", reasoningDoneItemText)
	}

	// response.completed output: reasoning item first (full text in a
	// reasoning_text part), message second — message content has NO
	// reasoning text.
	var completedResp map[string]any
	for _, ev := range events {
		if typ, _ := ev["type"].(string); typ == "response.completed" {
			completedResp, _ = ev["response"].(map[string]any)
		}
	}
	if completedResp == nil {
		t.Fatal("response.completed missing response object")
		return
	}
	out, _ := completedResp["output"].([]any)
	if len(out) != 2 {
		t.Fatalf("completed output len = %d, want 2 (reasoning + message): %v", len(out), out)
	}
	first, _ := out[0].(map[string]any)
	if first == nil || first["type"] != "reasoning" {
		t.Fatalf("completed output[0] = %v, want reasoning item", first)
		return
	}
	if summary, ok := first["summary"].([]any); !ok || len(summary) != 0 {
		t.Errorf("reasoning item summary = %v, want empty array", first["summary"])
	}
	if content, ok := first["content"].([]any); ok && len(content) == 1 {
		if cp, ok := content[0].(map[string]any); ok && cp["text"] != "Let me reason about this step by step" {
			t.Errorf("reasoning item content text = %q, want full reasoning text", cp["text"])
		}
	} else {
		t.Errorf("reasoning item content = %v, want one reasoning_text part", first["content"])
	}
	second, _ := out[1].(map[string]any)
	if second == nil || second["type"] != "message" {
		t.Fatalf("completed output[1] = %v, want message item", second)
		return
	}
	if mcontent, ok := second["content"].([]any); ok && len(mcontent) == 1 {
		if cp, ok := mcontent[0].(map[string]any); ok && cp["text"] != "The answer is 42" {
			t.Errorf("message content text = %q, want The answer is 42", cp["text"])
		}
	}
}

// TestReplayResponsesForwardCustomToolCall covers the dual-delta gap: every
// streamed function-call argument fragment must be emitted BOTH as
// response.function_call_arguments.delta (legacy consumers) and as
// response.custom_tool_call_input.delta (codex), and the done pair
// (function_call_arguments.done + custom_tool_call_input.done) must carry
// the complete arguments before output_item.done.
func TestReplayResponsesForwardCustomToolCall(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-fw2", 302,
			`"choices":[{"index":0,"delta":{"content":"Checking the forecast"},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-fw2", 302,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-fw2", 302,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"SF\"}"}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-fw2", 302,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-fw2", 302,
			`"choices":[],"usage":{"prompt_tokens":1234,"completion_tokens":56,"total_tokens":1290,"completion_tokens_details":{"reasoning_tokens":12}}`)))
	}
	ts, _ := newTestServer(t, nil, mock)

	body := codexResponsesBody(modelA, "You are a helpful coding agent.", "medium")
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	events := collectResponsesEvents(t, string(data))
	types := eventTypes(events)
	if last := types[len(types)-1]; last != "response.completed" {
		t.Fatalf("last event = %q, want response.completed; sequence: %v", last, types)
	}

	// Both delta event types carry the same fragments, in the same order,
	// with the same item_id/output_index.
	var legacyDeltas, customDeltas []string
	var legacyID, customID string
	var legacyIdx, customIdx float64
	for _, ev := range events {
		switch typ, _ := ev["type"].(string); typ {
		case "response.function_call_arguments.delta":
			if d, ok := ev["delta"].(string); ok {
				legacyDeltas = append(legacyDeltas, d)
			}
			legacyID, _ = ev["item_id"].(string)
			legacyIdx, _ = ev["output_index"].(float64)
		case "response.custom_tool_call_input.delta":
			if d, ok := ev["delta"].(string); ok {
				customDeltas = append(customDeltas, d)
			}
			customID, _ = ev["item_id"].(string)
			customIdx, _ = ev["output_index"].(float64)
		}
	}
	if len(legacyDeltas) != 2 || len(customDeltas) != 2 {
		t.Fatalf("delta counts = legacy %d / custom %d, want 2/2: %v", len(legacyDeltas), len(customDeltas), types)
	}
	for i := range legacyDeltas {
		if legacyDeltas[i] != customDeltas[i] {
			t.Errorf("delta fragment %d differs: function_call_arguments=%q custom_tool_call_input=%q", i, legacyDeltas[i], customDeltas[i])
		}
	}
	if joined := strings.Join(legacyDeltas, ""); joined != `{"city":"SF"}` {
		t.Errorf("joined argument deltas = %q, want {\"city\":\"SF\"}", joined)
	}
	if customID == "" || customID != legacyID {
		t.Errorf("custom delta item_id = %q, want = %q (same item as function_call_arguments)", customID, legacyID)
	}
	if customIdx != legacyIdx || customIdx == 0 {
		t.Errorf("custom delta output_index = %v, want = %v (same item as function_call_arguments)", customIdx, legacyIdx)
	}

	// Done pair: function_call_arguments.done then custom_tool_call_input.done,
	// both before the function_call's output_item.done, carrying the full args.
	fcArgsDone := indexOfType(events, "response.function_call_arguments.done")
	customDone := indexOfType(events, "response.custom_tool_call_input.done")
	itemDoneFC := -1
	for i, ev := range events {
		if typ, _ := ev["type"].(string); typ != "response.output_item.done" {
			continue
		}
		item, _ := ev["item"].(map[string]any)
		if item == nil {
			continue
		}
		if it, _ := item["type"].(string); it == "function_call" {
			itemDoneFC = i
		}
	}
	if fcArgsDone >= customDone || customDone >= itemDoneFC || itemDoneFC >= indexOfType(events, "response.completed") {
		t.Errorf("done ordering = argsDone(%d) < customDone(%d) < outputItemDone(%d) violated; sequence: %v", fcArgsDone, customDone, itemDoneFC, types)
	}
	var customInput string
	for _, ev := range events {
		if typ, _ := ev["type"].(string); typ == "response.custom_tool_call_input.done" {
			customInput, _ = ev["input"].(string)
		}
	}
	if customInput != `{"city":"SF"}` {
		t.Errorf("custom_tool_call_input.done input = %q, want {\"city\":\"SF\"}", customInput)
	}

	// The complete call in output_item.done stays authoritative.
	var fcArgs string
	var fcName, fcCallID string
	for _, ev := range events {
		if typ, _ := ev["type"].(string); typ != "response.output_item.done" {
			continue
		}
		item, _ := ev["item"].(map[string]any)
		if item == nil {
			continue
		}
		if it, _ := item["type"].(string); it == "function_call" {
			fcArgs, _ = item["arguments"].(string)
			fcName, _ = item["name"].(string)
			fcCallID, _ = item["call_id"].(string)
		}
	}
	if fcName != "get_weather" || fcCallID != "call_1" || fcArgs != `{"city":"SF"}` {
		t.Errorf("output_item.done function_call = name %q call_id %q arguments %q, want get_weather/call_1/{\"city\":\"SF\"}", fcName, fcCallID, fcArgs)
	}
}
