package server_test

// Conformance replay for the opencode CLI (sst/opencode) against the proxy's
// native surfaces, in the exact wire shapes opencode's default AI-SDK runtime
// and its native @opencode-ai/llm runtime both carry (WIRE-NOTES:
// reference/harnesses/opencode/WIRE-NOTES.md):
//
//   - OpenAI provider -> POST /v1/responses (packages/opencode/src/provider/
//     provider.ts:208-212 pins @ai-sdk/openai to sdk.responses(modelID)) with
//     include:["reasoning.encrypted_content"], reasoning:{effort, summary:"auto"},
//     store:false, tools strict:false, stream:true (openAIDefaultOptions,
//     packages/llm/src/providers/openai-options.ts:48-63). The stream MUST end
//     in response.completed / response.incomplete / response.failed — with no
//     such terminal the parser hangs (TERMINAL_TYPES,
//     packages/llm/src/protocols/openai-responses.ts:618-620).
//   - Anthropic provider -> POST /v1/messages with x-api-key +
//     anthropic-version 2023-06-01 + anthropic-beta
//     "interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14"
//     (packages/opencode/src/provider/provider.ts:180-181) — the gateway must
//     never 400 on that beta bundle.
//
// FINDING (recorded, no prod change from this file): the /v1/responses relay
// emits the response.reasoning_text.delta/.done family but never
// response.reasoning_summary_text.delta/.done (the reasoning item carries
// summary:[] instead of summary-part events — see Phase 2 shape pin below).
// The vendor's own golden recording (packages/llm/test/fixtures/recordings/
// openai-responses/openai-responses-gpt-5-5-reasoning.json, real gpt-5.5
// stream with the same include + reasoning{effort,summary:"auto"} request)
// emits the reasoning item as output_item.added → output_item.done with NO
// reasoning_* delta events at all (encrypted_content only), so the client is
// proven tolerant of any delta-less/summary-less reasoning item shape.

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// ocResponsesBody builds the opencode-shaped /v1/responses request body:
// input items (input_text), tools with strict:false, tool_choice "auto",
// store:false, prompt_cache_key, include:["reasoning.encrypted_content"],
// reasoning:{effort, summary:"auto"}, text:{verbosity}, max_output_tokens,
// temperature/top_p, stream:true (OpenAIResponsesBody,
// packages/llm/src/protocols/openai-responses.ts:126-156; openAIDefaultOptions,
// packages/llm/src/providers/openai-options.ts:48-63).
func ocResponsesBody(model, instructions string) string {
	return `{"model":"` + model + `",` +
		`"instructions":"` + instructions + `",` +
		`"input":[{"role":"user","content":[{"type":"input_text","text":"Explain serverless functions in one sentence."}]}],` +
		`"tools":[{"type":"function","name":"read_file","description":"Read a file from the workspace","strict":false,"parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}],` +
		`"tool_choice":"auto",` +
		`"store":false,` +
		`"prompt_cache_key":"sess-opencode-1",` +
		`"include":["reasoning.encrypted_content"],` +
		`"reasoning":{"effort":"medium","summary":"auto"},` +
		`"text":{"verbosity":"low"},` +
		`"max_output_tokens":8192,` +
		`"temperature":0.2,` +
		`"top_p":1,` +
		`"stream":true}`
}

// TestConformanceOpencodeResponsesStream mirrors opencode's Responses turn:
// include + reasoning{effort, summary:"auto"} + store:false + strict:false
// tools must be ACCEPTED (no 400 on the include value or the summary field),
// and the stream must terminate on response.completed — opencode's parser
// hangs without a terminal event (TERMINAL_TYPES,
// openai-responses.ts:618-620). Upstream reasoning (scripted reasoning THEN
// text) surfaces as the reasoning_text delta/done family ahead of the answer,
// the reasoning output_item.done carries the spec-shaped summary field, and
// the response body NEVER contains the "encrypted_content" key (the ignored
// include contract, devdocs/compatibility-roadmap.md Phase 0/2).
func TestConformanceOpencodeResponsesStream(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-oc1", 301,
			`"choices":[{"index":0,"delta":{"reasoning_content":"Serverless functions run on demand."},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-oc1", 301,
			`"choices":[{"index":0,"delta":{"content":"They spin up per request and scale to zero."},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-oc1", 301,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":200,"completion_tokens":50,"total_tokens":250,"completion_tokens_details":{"reasoning_tokens":30}}`)))
		// No upstream [DONE]: the relay must still terminate the client
		// stream with response.completed on upstream EOF.
	}
	ts, _ := newTestServer(t, nil, mock)

	body := ocResponsesBody(modelA, "You are opencode, a coding agent.")
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (include + reasoning.summary must not 400): %s", resp.StatusCode, truncate(string(data), 300))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	sse := string(data)
	if !strings.Contains(sse, ": connecting") {
		t.Error("stream missing ': connecting' grace-flush comment")
	}
	// Ignored-include contract: the response body never carries the requested
	// encrypted_content key, and no [DONE] sentinel leaks (Responses clients
	// parse [DONE] as JSON).
	if strings.Contains(sse, "encrypted_content") {
		t.Error("response body contains the ignored-include key encrypted_content; the contract omits it")
	}
	if strings.Contains(sse, "[DONE]") {
		t.Error("stream must not emit [DONE] to a Responses client")
	}

	events := collectResponsesEvents(t, sse)
	if len(events) == 0 {
		t.Fatalf("no SSE events parsed from body: %q", truncate(sse, 400))
	}
	if created := indexOfType(events, "response.created"); created != 0 {
		t.Errorf("response.created index = %d, want 0 (first event)", created)
	}
	completed := indexOfType(events, "response.completed")
	types := eventTypes(events)
	if completed == -1 {
		t.Fatal("stream missing terminal response.completed (opencode's parser hangs without it)")
	}
	if last := types[len(types)-1]; last != "response.completed" {
		t.Errorf("last event = %q, want response.completed; sequence: %v", last, types)
	}
	if strings.Contains(sse, `"type":"response.failed"`) {
		t.Error("stream emitted response.failed for a healthy upstream")
	}

	// Reasoning channel: the scripted upstream reasoning arrives as
	// response.reasoning_text.delta (the family opencode consumes) ahead of
	// the answer, and reasoning_text.done closes it before the terminal event.
	// See the file-header FINDING for why response.reasoning_summary_text.*
	// events are not asserted: the relay does not emit them, and opencode
	// consumes any of the three delta/done names interchangeably
	// (packages/llm/src/protocols/openai-responses.ts:913-934).
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
	if len(reasoningDeltas) != 1 || reasoningDeltas[0] != "Serverless functions run on demand." {
		t.Errorf("reasoning_text deltas = %v, want the full reasoning text", reasoningDeltas)
	}
	if len(textDeltas) != 1 || textDeltas[0] != "They spin up per request and scale to zero." {
		t.Errorf("output_text deltas = %v, want exactly [the answer]", textDeltas)
	}
	firstReasoning := indexOfType(events, "response.reasoning_text.delta")
	firstText := indexOfType(events, "response.output_text.delta")
	doneIdx := indexOfType(events, "response.reasoning_text.done")
	if firstReasoning == -1 || firstReasoning >= firstText {
		t.Errorf("reasoning delta index = %d, output_text delta index = %d, want reasoning before the answer", firstReasoning, firstText)
	}
	if doneIdx == -1 || doneIdx >= completed {
		t.Errorf("reasoning_text.done index = %d, want before response.completed(%d)", doneIdx, completed)
	}

	// opencode starts a reasoning item from output_item.added (needs the id —
	// openai-responses.ts:688-703) and keys deltas on it.
	var sawReasoningAdded bool
	for _, ev := range events {
		if t, _ := ev["type"].(string); t != "response.output_item.added" {
			continue
		}
		item, _ := ev["item"].(map[string]any)
		if item == nil || item["type"] != "reasoning" {
			continue
		}
		sawReasoningAdded = true
		if id, _ := item["id"].(string); !strings.HasPrefix(id, "rs_") {
			t.Errorf("reasoning output_item.added id = %q, want rs_<random>", id)
		}
	}
	if !sawReasoningAdded {
		t.Error("missing output_item.added for the reasoning item (opencode starts reasoning from it)")
	}

	// output_item.done(reasoning) must be spec-shaped: id, type, status,
	// summary and content[{type:"reasoning_text",text}] — the Phase 2 pin
	// (devdocs/compatibility-roadmap.md). The summary field is pinned here; the
	// summary EVENTS are the file-header FINDING.
	var reasonDoneItem map[string]any
	for _, ev := range events {
		if t, _ := ev["type"].(string); t != "response.output_item.done" {
			continue
		}
		item, _ := ev["item"].(map[string]any)
		if item != nil && item["type"] == "reasoning" {
			reasonDoneItem = item
		}
	}
	if reasonDoneItem == nil {
		t.Fatal("reasoning output_item.done never emitted")
		return
	}
	if id, _ := reasonDoneItem["id"].(string); !strings.HasPrefix(id, "rs_") {
		t.Errorf("reasoning output_item.done id = %q, want rs_<random>", id)
	}
	if status, _ := reasonDoneItem["status"].(string); status != "completed" {
		t.Errorf("reasoning item status = %q, want completed", status)
	}
	if summary, ok := reasonDoneItem["summary"].([]any); !ok || len(summary) != 0 {
		t.Errorf("reasoning item summary = %v, want empty array (spec-shaped; Phase 2)", reasonDoneItem["summary"])
	}
	content, ok := reasonDoneItem["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("reasoning item content = %v, want one reasoning_text part", reasonDoneItem["content"])
	}
	if cp, _ := content[0].(map[string]any); cp["type"] != "reasoning_text" || cp["text"] != "Serverless functions run on demand." {
		t.Errorf("reasoning content part = %v, want reasoning_text with the full text", content[0])
	}

	// response.completed carries the resp_ id, served model, status and the
	// Responses usage shape (input_tokens + output_tokens with reasoning).
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
	if model, _ := completedResp["model"].(string); model != modelA {
		t.Errorf("completed response model = %q, want %q", model, modelA)
	}
	if status, _ := completedResp["status"].(string); status != "completed" {
		t.Errorf("completed response status = %q, want completed", status)
	}
	usage, _ := completedResp["usage"].(map[string]any)
	if usage == nil {
		t.Fatal("response.completed missing usage")
		return
	}
	if usage["input_tokens"] != float64(200) || usage["output_tokens"] != float64(50) || usage["total_tokens"] != float64(250) {
		t.Errorf("usage tokens = %v, want input 200 output 50 total 250", usage)
	}
	od, _ := usage["output_tokens_details"].(map[string]any)
	if od == nil || od["reasoning_tokens"] != float64(30) {
		t.Errorf("usage output_tokens_details = %v, want reasoning_tokens 30", od)
	}
	out, _ := completedResp["output"].([]any)
	if len(out) != 2 {
		t.Fatalf("completed output len = %d, want 2 (reasoning + message): %v", len(out), out)
	}
	if first, _ := out[0].(map[string]any); first == nil || first["type"] != "reasoning" {
		t.Errorf("completed output[0] = %v, want the reasoning item first", out[0])
	}
	if second, _ := out[1].(map[string]any); second == nil || second["type"] != "message" {
		t.Errorf("completed output[1] = %v, want the message item", out[1])
	}

	// The translated upstream chat body: instructions → system, wrapped tool,
	// tool_choice auto, store passthrough, max_output_tokens →
	// max_completion_tokens, and the reasoning effort — opencode requests
	// "medium"; deepseek-v4-flash's effort ladder is {'high'}, so the proxy
	// clamps medium → high on the wire (resolveFreebuffReasoningEffort
	// semantics, #112 — same rule TestReplayCodexResponsesSse pins).
	if !mock.BodyContains(`"role":"system"`) || !mock.BodyContains("You are opencode, a coding agent.") {
		t.Error("upstream body missing instructions → system message")
	}
	if !mock.BodyContains(`"read_files"`) || !mock.BodyContains(`"tool_choice":"auto"`) {
		t.Error("upstream body missing wrapped tool / tool_choice auto")
	}
	if !mock.BodyContains(`"store":false`) {
		t.Error("upstream body missing store:false passthrough")
	}
	if !mock.BodyContains(`"max_completion_tokens":8192`) {
		t.Error("upstream body missing max_output_tokens → max_completion_tokens")
	}
	if !mock.BodyContains(`"reasoning_effort":"high"`) {
		t.Error("upstream body missing reasoning_effort (want clamped high for deepseek-v4-flash)")
	}
}

// TestConformanceOpencodeAnthropicBetaBundle pins the opencode Anthropic
// provider wire: x-api-key + anthropic-version 2023-06-01 + anthropic-beta
// "interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14"
// (packages/opencode/src/provider/provider.ts:180-181) on /v1/messages with
// thinking enabled. The beta bundle must NEVER 400 (the gateway is liberal —
// anthropic.go: "does not validate the version header"), the thinking block
// lifecycle must be message_start → content_block_start(thinking) →
// thinking_delta → signature_delta → content_block_stop → text block →
// message_delta → message_stop, thinking must translate to a reasoning effort
// upstream, and no [DONE] terminator leaks into the Anthropic stream.
func TestConformanceOpencodeAnthropicBetaBundle(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-oca", 302,
			`"choices":[{"index":0,"delta":{"reasoning_content":"Let me think about serverless functions."},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-oca", 302,
			`"choices":[{"index":0,"delta":{"content":"Serverless functions scale to zero."},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-oca", 302,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":80,"completion_tokens":30,"total_tokens":110}`)))
		// No upstream [DONE]: finalize on upstream EOF.
	}
	ts, _ := newTestServer(t, nil, mock)
	headers := map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         "opencode-key",
		"anthropic-version": "2023-06-01",
		"anthropic-beta":    "interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14",
	}
	body := `{"model":"` + modelA + `","max_tokens":4096,` +
		`"thinking":{"type":"enabled","budget_tokens":4096},` +
		`"messages":[{"role":"user","content":"Explain serverless functions."}],"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (opencode beta bundle must never 400): %s", resp.StatusCode, truncate(string(data), 300))
	}
	if ver := resp.Header.Get("anthropic-version"); ver != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", ver)
	}
	sse := string(data)
	if strings.Contains(sse, "[DONE]") {
		t.Error("stream leaked an OpenAI [DONE] terminator into the Anthropic surface")
	}
	events := collectAnthropicEvents(t, sse)
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if got := replayEventTypes(events); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence = %v, want %v", got, want)
	}

	// Thinking block: type thinking at index 0, thinking_delta then the
	// signature_delta that closes it (empty signature — the chat upstream
	// never emits signatures) before the text block opens.
	var thinkingIdx = -1
	for _, ev := range events {
		if ev["type"] != "content_block_start" {
			continue
		}
		cb, _ := ev["content_block"].(map[string]any)
		if cb == nil || cb["type"] != "thinking" {
			continue
		}
		thinkingIdx = replayEventIndex(ev)
		if sig, _ := cb["signature"].(string); sig != "" {
			t.Errorf("thinking block signature = %q, want empty (upstream provides none)", sig)
		}
	}
	if thinkingIdx != 0 {
		t.Fatalf("thinking block index = %d, want 0", thinkingIdx)
	}
	var sawThinkingDelta, sawSignatureDelta bool
	for _, ev := range events {
		if ev["type"] != "content_block_delta" || replayEventIndex(ev) != thinkingIdx {
			continue
		}
		d, _ := ev["delta"].(map[string]any)
		switch d["type"] {
		case "thinking_delta":
			if d["thinking"] != "Let me think about serverless functions." {
				t.Errorf("thinking_delta = %v, want the full reasoning text", d["thinking"])
			}
			sawThinkingDelta = true
		case "signature_delta":
			sawSignatureDelta = true
		}
	}
	if !sawThinkingDelta || !sawSignatureDelta {
		t.Errorf("thinking lifecycle incomplete: thinking_delta=%v signature_delta=%v", sawThinkingDelta, sawSignatureDelta)
	}
	// The answer follows in the text block only; reasoning never becomes text.
	if got := replayTextJoin(events); got != "Serverless functions scale to zero." {
		t.Errorf("assembled text = %q, want only the answer", got)
	}
	if stop, _ := replayMessageDelta(events); stop != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", stop)
	}
	// thinking → reasoning effort reaches the upstream (budget 4096 → medium,
	// clamped to high for deepseek-v4-flash — #112 ladder rule).
	if !mock.BodyContains(`"reasoning_effort":"high"`) {
		t.Error("upstream body missing reasoning_effort (want clamped high)")
	}
}
