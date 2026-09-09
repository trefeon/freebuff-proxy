package server_test

// Hermetic "real client" conformance tests for the codex CLI harness
// (reference/harnesses/codex/WIRE-NOTES.md §1-§7) against the proxy's
// /v1/responses surface. The codex CLI speaks ONLY the OpenAI Responses
// API: wire_api "chat" is a hard config error (WIRE-NOTES.md §1), requests
// carry Authorization: Bearer, tool_choice "auto", store:false,
// include:["reasoning.encrypted_content"], reasoning.effort, stream:true
// (WIRE-NOTES.md §3-§4), tool calls arrive as response.output_item.done
// function_call items (never assistant tool_calls, WIRE-NOTES.md §6), the
// stream parser consumes response.custom_tool_call_input.delta (arguments
// stream) plus the completed function_call item while IGNORING
// response.function_call_arguments.* (WIRE-NOTES.md §5), and
// response.completed REQUIRES response.id plus, when usage is present,
// input_tokens/output_tokens/total_tokens (WIRE-NOTES.md §5). Each test
// drives the live HTTP surface with testutil.MockUpstream as the fake
// codebuff.com upstream and asserts the byte shape the client parses.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// codexTurnBody builds the Turn-1 codex request: a single user input_text
// message, one function tool (bash), tool_choice "auto", store:false,
// stream:true, include ["reasoning.encrypted_content"], reasoning.effort
// "high" — the exact field set codex emits per WIRE-NOTES.md §4.
func codexTurnBody(model, instructions string) string {
	return `{"model":"` + model + `",` +
		`"instructions":"` + instructions + `",` +
		`"input":[{"role":"user","content":[{"type":"input_text","text":"Run pwd and report the result."}]}],` +
		`"tools":[{"type":"function","name":"bash","description":"Run a command in the shell","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}],` +
		`"tool_choice":"auto",` +
		`"store":false,` +
		`"stream":true,` +
		`"include":["reasoning.encrypted_content"],` +
		`"reasoning":{"effort":"high"}}`
}

// codexRoundTripBody builds the Turn-2 request: the prior turn's function
// call and its result round-trip as input items (function_call +
// function_call_output, WIRE-NOTES.md §6) next to the new user message.
func codexRoundTripBody(model, instructions string) string {
	return `{"model":"` + model + `",` +
		`"instructions":"` + instructions + `",` +
		`"input":[` +
		`{"role":"user","content":[{"type":"input_text","text":"Run pwd and report the result."}]},` +
		`{"type":"function_call","call_id":"call_2a","name":"bash","arguments":"{\"command\":\"pwd\"}"},` +
		`{"type":"function_call_output","call_id":"call_2a","output":"{\"cwd\":\"/home\"}"}` +
		`],` +
		`"tools":[{"type":"function","name":"bash","description":"Run a command in the shell","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}],` +
		`"tool_choice":"auto",` +
		`"store":false,` +
		`"stream":true,` +
		`"include":["reasoning.encrypted_content"]}`
}

// codexEventByType returns the first event with the wanted type, or nil.
func codexEventByType(events []map[string]any, typ string) map[string]any {
	for _, ev := range events {
		if t, _ := ev["type"].(string); t == typ {
			return ev
		}
	}
	return nil
}

// codexItemOfType returns the "item" object of the first eventType event
// whose item.type == itemType (e.g. output_item.added + function_call), or
// nil.
func codexItemOfType(events []map[string]any, eventType, itemType string) map[string]any {
	for _, ev := range events {
		if t, _ := ev["type"].(string); t != eventType {
			continue
		}
		item, _ := ev["item"].(map[string]any)
		if it, _ := item["type"].(string); it == itemType {
			return item
		}
	}
	return nil
}

// codexIndexOfItem returns the index of the first eventType event whose
// item.type == itemType, or -1 — the ordering primitive for the
// output_item.added -> custom_tool_call_input.* -> output_item.done
// sequence (WIRE-NOTES.md §5).
func codexIndexOfItem(events []map[string]any, eventType, itemType string) int {
	for i, ev := range events {
		if t, _ := ev["type"].(string); t != eventType {
			continue
		}
		item, _ := ev["item"].(map[string]any)
		if it, _ := item["type"].(string); it == itemType {
			return i
		}
	}
	return -1
}

// codexDeltasOf collects the string "key" field from every event of typ
// (streamed fragments, e.g. arguments under response.custom_tool_call_input.delta).
func codexDeltasOf(events []map[string]any, typ, key string) []string {
	var out []string
	for _, ev := range events {
		if t, _ := ev["type"].(string); t == typ {
			if s, ok := ev[key].(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// TestConformanceCodexResponsesTurn replays a codex Turn-1 streaming turn
// end-to-end: POST /v1/responses with Bearer auth and the full codex field
// set, upstream mock serving reasoning + text + a function-call turn split
// over fragments plus a final usage chunk, no upstream [DONE] sentinel
// (codex has no [DONE] handling — WIRE-NOTES.md §5/§12). The downstream
// stream must open response.created → response.in_progress (response.id
// non-empty), surface reasoning as output_item.added + reasoning_text.delta
// (with content_index), emit the tool call as output_item.added
// (function_call, in_progress) BEFORE response.custom_tool_call_input.delta,
// then custom_tool_call_input.done, then output_item.done carrying the
// complete function_call item — and terminate on response.completed with
// response.id plus a usage carrying input_tokens/output_tokens/total_tokens
// and no response.failed.
func TestConformanceCodexResponsesTurn(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// The proxy renames client tool "bash" to the official upstream
	// signature "run_terminal_command" (internal/convert/toolmap.go
	// clientToOfficial), so the upstream turn carries the official name and
	// the relay must restore "bash" for codex to dispatch (WIRE-NOTES.md §6).
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-codex1", 1,
			`"choices":[{"index":0,"delta":{"reasoning_content":"Let me check the right command."},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-codex1", 1,
			`"choices":[{"index":0,"delta":{"content":"Running the command"},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-codex1", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_2a","type":"function","function":{"name":"run_terminal_command","arguments":"{\"command\":"}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-codex1", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"pwd\"}"}}]},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-codex1", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-codex1", 1,
			`"choices":[],"usage":{"prompt_tokens":321,"completion_tokens":45,"total_tokens":366,"completion_tokens_details":{"reasoning_tokens":18}}`)))
		// No upstream [DONE]: upstream EOF is the terminator (codex treats
		// [DONE] as unparseable JSON at best, WIRE-NOTES.md §5).
	}
	ts, _ := newTestServer(t, []string{"sk-codex"}, mock)

	body := codexTurnBody(modelA, "You are a helpful coding agent.")
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body),
		map[string]string{"Authorization": "Bearer sk-codex"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	sse := string(data)
	if strings.Contains(sse, "[DONE]") {
		t.Error("stream must not relay [DONE] to codex (it parses data: JSON only)")
	}
	if strings.Contains(sse, `"type":"response.failed"`) {
		t.Error("stream emitted response.failed for a healthy upstream")
	}

	events := collectResponsesEvents(t, sse)
	if len(events) == 0 {
		t.Fatalf("no SSE events parsed from body: %q", truncate(sse, 400))
	}
	types := eventTypes(events)

	// Terminal contract: response.completed is the LAST event, carrying a
	// non-empty response.id and the usage triple codex deserializes
	// REQUIRED (WIRE-NOTES.md §5: EOF before response.completed is a hard
	// client error and a malformed usage errors the whole event).
	completed := indexOfType(events, "response.completed")
	if completed == -1 {
		t.Fatal("stream missing terminal response.completed")
	}
	if last := types[len(types)-1]; last != "response.completed" {
		t.Errorf("last event = %q, want response.completed; full sequence: %v", last, types)
	}
	completedEv := codexEventByType(events, "response.completed")
	completedResp, _ := completedEv["response"].(map[string]any)
	if completedResp == nil {
		t.Fatal("response.completed missing response object")
		return
	}
	if cid, _ := completedResp["id"].(string); !strings.HasPrefix(cid, "resp_") || len(cid) <= len("resp_") {
		t.Errorf("completed response id = %q, want resp_<random> (codex requires it, WIRE-NOTES.md §5)", cid)
	}
	if status, _ := completedResp["status"].(string); status != "completed" {
		t.Errorf("completed response status = %q, want completed", status)
	}
	if model, _ := completedResp["model"].(string); model != modelA {
		t.Errorf("completed response model = %q, want %q", model, modelA)
	}
	usage, _ := completedResp["usage"].(map[string]any)
	if usage == nil {
		t.Fatal("response.completed missing usage")
		return
	}
	for _, key := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		if _, ok := usage[key]; !ok {
			t.Errorf("completed usage missing %s: %v", key, usage)
		}
	}
	if u, ok := usage["input_tokens"].(float64); !ok || u != 321 {
		t.Errorf("usage.input_tokens = %#v, want 321", usage["input_tokens"])
	}
	if u, ok := usage["output_tokens"].(float64); !ok || u != 45 {
		t.Errorf("usage.output_tokens = %#v, want 45", usage["output_tokens"])
	}
	if u, ok := usage["total_tokens"].(float64); !ok || u != 366 {
		t.Errorf("usage.total_tokens = %#v, want 366", usage["total_tokens"])
	}

	// Open: response.created first, then response.in_progress, sharing the
	// same non-empty response.id.
	created := indexOfType(events, "response.created")
	inProgress := indexOfType(events, "response.in_progress")
	if created != 0 {
		t.Errorf("response.created index = %d, want 0 (first event)", created)
	}
	if inProgress != 1 {
		t.Errorf("response.in_progress index = %d, want 1", inProgress)
	}
	createdResp, _ := codexEventByType(events, "response.created")["response"].(map[string]any)
	inProgResp, _ := codexEventByType(events, "response.in_progress")["response"].(map[string]any)
	if createdID, _ := createdResp["id"].(string); createdID == "" {
		t.Error("response.created missing response.id")
	} else if inProgID, _ := inProgResp["id"].(string); inProgID != createdID {
		t.Errorf("response.in_progress id = %q, want same as created %q", inProgID, createdID)
	}

	// Reasoning: upstream reasoning_content must surface as a reasoning
	// output_item (added) followed by reasoning_text.delta carrying
	// content_index (WIRE-NOTES.md §5 hard requirement).
	rAdded := codexIndexOfItem(events, "response.output_item.added", "reasoning")
	rDelta := indexOfType(events, "response.reasoning_text.delta")
	if rAdded < 0 || rDelta < 0 || rDelta <= rAdded {
		t.Fatalf("reasoning event order: added=%d delta=%d, want added before delta", rAdded, rDelta)
	}
	rItem := codexItemOfType(events, "response.output_item.added", "reasoning")
	if st, _ := rItem["status"].(string); st != "in_progress" {
		t.Errorf("reasoning output_item.added status = %q, want in_progress", st)
	}
	rDeltaEv := codexEventByType(events, "response.reasoning_text.delta")
	if ci, ok := rDeltaEv["content_index"]; !ok {
		t.Error("response.reasoning_text.delta missing content_index (codex requires it, WIRE-NOTES.md §5)")
	} else if f, ok := ci.(float64); !ok || f != 0 {
		t.Errorf("reasoning_text.delta content_index = %#v, want 0", ci)
	}
	if d, _ := rDeltaEv["delta"].(string); d != "Let me check the right command." {
		t.Errorf("reasoning_text.delta = %q", d)
	}

	// Tool call: output_item.added (function_call, in_progress) BEFORE the
	// custom_tool_call_input.delta fragments, then custom_tool_call_input.done,
	// then output_item.done with the complete function_call item — the exact
	// order codex's Responses stream parser walks (WIRE-NOTES.md §5-§6;
	// function_call_arguments.* is ignored by codex, so the custom_* pair is
	// the only streamed arguments channel it consumes).
	fcAdded := codexIndexOfItem(events, "response.output_item.added", "function_call")
	customDelta := indexOfType(events, "response.custom_tool_call_input.delta")
	customDone := indexOfType(events, "response.custom_tool_call_input.done")
	fcDone := codexIndexOfItem(events, "response.output_item.done", "function_call")
	if fcAdded < 0 || customDelta < 0 || customDone < 0 || fcDone < 0 {
		t.Fatalf("tool-call events missing: added=%d customDelta=%d customDone=%d done=%d", fcAdded, customDelta, customDone, fcDone)
	}
	if fcAdded >= customDelta || customDelta >= customDone || customDone >= fcDone {
		t.Errorf("tool-call order = added:%d < customDelta:%d < customDone:%d < done:%d violated: %v",
			fcAdded, customDelta, customDone, fcDone, types)
	}
	fcAddedItem := codexItemOfType(events, "response.output_item.added", "function_call")
	if st, _ := fcAddedItem["status"].(string); st != "in_progress" {
		t.Errorf("function_call output_item.added status = %q, want in_progress", st)
	}
	deltas := codexDeltasOf(events, "response.custom_tool_call_input.delta", "delta")
	if len(deltas) != 2 || strings.Join(deltas, "") != `{"command":"pwd"}` {
		t.Errorf("custom_tool_call_input deltas = %v, want two fragments joining to {\"command\":\"pwd\"}", deltas)
	}
	customDoneEv := codexEventByType(events, "response.custom_tool_call_input.done")
	if in, _ := customDoneEv["input"].(string); in != `{"command":"pwd"}` {
		t.Errorf("custom_tool_call_input.done input = %q, want {\"command\":\"pwd\"}", in)
	}
	fcDoneItem := codexItemOfType(events, "response.output_item.done", "function_call")
	if st, _ := fcDoneItem["status"].(string); st != "completed" {
		t.Errorf("function_call output_item.done status = %q, want completed", st)
	}
	if id, _ := fcDoneItem["call_id"].(string); id != "call_2a" {
		t.Errorf("function_call item call_id = %q, want call_2a", id)
	}
	if name, _ := fcDoneItem["name"].(string); name != "bash" {
		t.Errorf("function_call item name = %q, want bash (client dispatch name restored, toolmap.go clientToOfficial)", name)
	}
	if args, _ := fcDoneItem["arguments"].(string); args != `{"command":"pwd"}` {
		t.Errorf("function_call item arguments = %q, want {\"command\":\"pwd\"} (JSON string, WIRE-NOTES.md §6)", args)
	}
}

// TestConformanceCodexFunctionCallRoundTrip replays codex Turn 2: the prior
// turn's function_call + function_call_output input items must round-trip
// through the proxy into the upstream chat body as an assistant message
// carrying tool_calls (id/name/arguments) immediately followed by the
// role:"tool" message with the matching tool_call_id — the PR #226 contract
// that keeps codex from looping forever on a re-issued call. The request
// translation must also forward tool_choice "auto" and keep "include"
// client-side (it has no chat-completions analogue and must never surface
// as a chat field).
func TestConformanceCodexFunctionCallRoundTrip(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-codex2", 2,
			`"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-codex2", 2,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`)))
	}
	ts, _ := newTestServer(t, []string{"sk-codex"}, mock)

	body := codexRoundTripBody(modelA, "You are a helpful coding agent.")
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body),
		map[string]string{"Authorization": "Bearer sk-codex"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}

	// The recorded upstream body is the chat wire the mock received: the
	// function_call item must emerge as assistant tool_calls BEFORE its
	// function_call_output as role:"tool" (PR #226 / responses.go
	// responsesInputToMessages), with the arguments kept as the full JSON
	// string (WIRE-NOTES.md §6: arguments is a JSON string, not an object).
	recorded := mock.RecordedChatBodiesSnapshot()
	if len(recorded) != 1 {
		t.Fatalf("recorded chat bodies = %d, want 1: %v", len(recorded), recorded)
	}
	var up map[string]any
	if err := json.Unmarshal([]byte(recorded[0]), &up); err != nil {
		t.Fatalf("recorded upstream body not JSON: %v: %s", err, recorded[0])
	}
	if tc, _ := up["tool_choice"].(string); tc != "auto" {
		t.Errorf("upstream tool_choice = %#v, want \"auto\" (codex always sends the string form, WIRE-NOTES.md §4)", up["tool_choice"])
	}
	if _, leaked := up["include"]; leaked {
		t.Error("upstream body carries the Responses-only \"include\" field (codex's include must stay client-side)")
	}
	if strings.Contains(recorded[0], "reasoning.encrypted_content") {
		t.Errorf("upstream body leaks include value reasoning.encrypted_content: %s", recorded[0])
	}

	msgs, _ := up["messages"].([]any)
	assistantIdx, toolIdx := -1, -1
	var assistant, tool map[string]any
	for i, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		switch mm["role"] {
		case "assistant":
			if assistantIdx == -1 {
				assistantIdx, assistant = i, mm
			}
		case "tool":
			if toolIdx == -1 {
				toolIdx, tool = i, mm
			}
		}
	}
	if assistantIdx == -1 || toolIdx == -1 {
		t.Fatalf("messages missing assistant/tool pair: %v", msgs)
	}
	if assistantIdx >= toolIdx {
		t.Errorf("assistant tool_calls message (index %d) must precede its tool reply (index %d)", assistantIdx, toolIdx)
	}
	tcs, _ := assistant["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("assistant tool_calls = %#v, want exactly one entry", assistant["tool_calls"])
	}
	tc, _ := tcs[0].(map[string]any)
	if id, _ := tc["id"].(string); id != "call_2a" {
		t.Errorf("tool_calls[0].id = %q, want call_2a", id)
	}
	fn, _ := tc["function"].(map[string]any)
	if name, _ := fn["name"].(string); name != "bash" {
		t.Errorf("tool_calls[0].function.name = %q, want bash", name)
	}
	if args, _ := fn["arguments"].(string); args != `{"command":"pwd"}` {
		t.Errorf("tool_calls[0].function.arguments = %q, want {\"command\":\"pwd\"} as a JSON string", args)
	}
	if tcid, _ := tool["tool_call_id"].(string); tcid != "call_2a" {
		t.Errorf("tool message tool_call_id = %q, want call_2a (matching the replayed call)", tcid)
	}
	if content, _ := tool["content"].(string); content != `{"cwd":"/home"}` {
		t.Errorf("tool message content = %q, want the output string", content)
	}

	// The request-side rename (#140, internal/convert/toolmap.go
	// clientToOfficial["bash"] = "run_terminal_command"): the upstream sees
	// the official signature name so it executes the call, and the
	// response relays restore "bash" (asserted in the turn test).
	var toolsUp []any
	if tv, ok := up["tools"].([]any); ok {
		toolsUp = tv
	}
	foundOfficial := false
	for _, toolRaw := range toolsUp {
		tm, _ := toolRaw.(map[string]any)
		wrapped, _ := tm["function"].(map[string]any)
		if name, _ := wrapped["name"].(string); name == "run_terminal_command" {
			foundOfficial = true
			if desc, _ := wrapped["description"].(string); !strings.Contains(desc, "(client tool: bash)") {
				t.Errorf("renamed tool description = %q, want it to carry the client tool name for model selection", desc)
			}
		}
	}
	if !foundOfficial {
		t.Errorf("upstream tools missing renamed run_terminal_command: %v", toolsUp)
	}
}
