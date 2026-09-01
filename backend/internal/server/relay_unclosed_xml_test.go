package server

// Regression tests for the 2026-08-31 full-stack review relay fixes:
//   - P2-6: tool calls flushed from an unclosed XML block arrive after the
//     terminal finish_reason "stop"; the relay must append a synthetic
//     empty-delta chunk with finish_reason "tool_calls" before [DONE].
//   - P3: non-streaming /v1/responses drops upstream reasoning_content —
//     it must surface a reasoning output item (stream/non-stream parity).
//   - P3: the end_turn continuation-fragment drop is shared between the
//     chat streaming relay (bytes) and the Responses relay (map): both
//     entry points must stay byte-equivalent.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/logring"
	"freebuff-proxy/backend/internal/upstream"
)

// TestRelayReviewFixUnclosedXMLFinishToolCalls pins P2-6: an XML tool call
// held by the extractor at stream end is flushed AFTER the terminal chunk,
// so the in-loop stop→tool_calls flip (keyed on xmlCallsSeen at terminal
// time) cannot fire. The stream must still end with a synthetic
// empty-delta chunk reading finish_reason "tool_calls" before [DONE].
// Pre-fix, the last finish_reason the client saw was "stop".
func TestRelayReviewFixUnclosedXMLFinishToolCalls(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()

	ss := strings.Join([]string{
		testutilSSE(`{"id":"chatcmpl-rx","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"<tool_call>` + "```json\\n{\\\"name\\\":\\\"bash\\\",\\\"arguments\\\":{\\\"command\\\":\\\"pwd\\\"}}\\n```" + `"},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-rx","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
	}, "")

	s.relayStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now())
	body := rec.Body.String()

	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("terminal stop chunk missing: %q", truncateStr(body, 600))
	}
	if !strings.Contains(body, `"name":"bash"`) {
		t.Fatalf("flushed XML call missing: %q", truncateStr(body, 600))
	}

	frames := collectSSEFrames(t, body)
	stopIdx, callsIdx, finishIdx := -1, -1, -1
	for i, f := range frames {
		choices, _ := f.data["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if choice == nil {
			continue
		}
		if fr, ok := choice["finish_reason"].(string); ok {
			switch fr {
			case "stop":
				stopIdx = i
			case "tool_calls":
				finishIdx = i
			}
		}
		if _, ok := choice["delta"].(map[string]any); ok {
			if _, hasCalls := choice["delta"].(map[string]any)["tool_calls"]; hasCalls {
				callsIdx = i
			}
		}
	}
	if stopIdx < 0 {
		t.Fatal("stop chunk not parsed")
	}
	if callsIdx < 0 {
		t.Fatal("flushed tool_calls chunk not parsed")
	}
	if finishIdx < 0 {
		// The pre-fix failure: the stream ends on "stop" even though fully
		// delivered tool calls followed it.
		t.Fatalf("no synthetic finish_reason tool_calls chunk after flushed calls: %q", truncateStr(body, 600))
	}
	if callsIdx < stopIdx {
		t.Errorf("flushed calls (idx %d) must follow the terminal stop (idx %d)", callsIdx, stopIdx)
	}
	if finishIdx < callsIdx {
		t.Errorf("synthetic finish (idx %d) must follow the flushed calls (idx %d)", finishIdx, callsIdx)
	}
	// The synthetic finish chunk carries an empty delta.
	finChoices, _ := frames[finishIdx].data["choices"].([]any)
	finChoice, _ := finChoices[0].(map[string]any)
	delta, _ := finChoice["delta"].(map[string]any)
	if len(delta) != 0 {
		t.Errorf("synthetic finish delta = %v, want empty", delta)
	}
	// And it lands before the [DONE] terminator.
	doneAt := strings.Index(body, "data: [DONE]")
	finAt := strings.LastIndex(body, `"finish_reason":"tool_calls"`)
	if doneAt < 0 || finAt < 0 || finAt > doneAt {
		t.Errorf("synthetic finish must precede [DONE]: finish@%d done@%d", finAt, doneAt)
	}
}

// TestRelayReviewFixUnclosedXMLLengthReasonIntact pins the guard's scope:
// an upstream "length" terminal is left intact — no synthetic tool_calls
// finish may be appended for a truncated stream.
func TestRelayReviewFixUnclosedXMLLengthReasonIntact(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()

	ss := strings.Join([]string{
		testutilSSE(`{"id":"chatcmpl-rl","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"<tool_call>` + "```json\\n{\\\"name\\\":\\\"bash\\\",\\\"arguments\\\":{\\\"command\\\":\\\"pwd\\\"}}\\n```" + `"},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-rl","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`),
	}, "")

	s.relayStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now())
	body := rec.Body.String()

	if !strings.Contains(body, `"finish_reason":"length"`) {
		t.Errorf("terminal finish_reason length missing: %q", truncateStr(body, 600))
	}
	if strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Errorf("length reason must not be flipped to tool_calls: %q", truncateStr(body, 600))
	}
}

// TestRelayReviewFixUnclosedXMLNativeGuard pins the relayJSON guard mirror:
// when native tool-call fragments were seen, a terminal "stop" is
// deliberate and no synthetic tool_calls finish is appended even though the
// flush also releases an extracted call.
func TestRelayReviewFixUnclosedXMLNativeGuard(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()

	ss := strings.Join([]string{
		testutilSSE(`{"id":"chatcmpl-rn","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_n","type":"function","function":{"name":"search","arguments":"{}"}}]},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-rn","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"<tool_call>` + "```json\\n{\\\"name\\\":\\\"bash\\\",\\\"arguments\\\":{\\\"command\\\":\\\"pwd\\\"}}\\n```" + `"},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-rn","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
	}, "")

	s.relayStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now())
	body := rec.Body.String()

	if !strings.Contains(body, `"name":"search"`) || !strings.Contains(body, `"name":"bash"`) {
		t.Errorf("both native and extracted calls must be relayed: %q", truncateStr(body, 600))
	}
	if strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Errorf("native delivery keeps the upstream stop: %q", truncateStr(body, 600))
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("terminal stop missing: %q", truncateStr(body, 600))
	}
}

// TestRelayReviewFixResponsesJSONReasoningItem pins stream/non-stream
// parity: a non-streaming /v1/responses completion whose message carries
// upstream reasoning must surface a reasoning output item with the same
// shape the streaming path emits (type reasoning, empty summary,
// reasoning_text content), preceding the message item.
func TestRelayReviewFixResponsesJSONReasoningItem(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string // delta reasoning alias the upstream sends
	}{
		{"reasoning_content", "reasoning_content"},
		{"reasoning", "reasoning"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testRelayServer()
			rec := httptest.NewRecorder()
			ss := strings.Join([]string{
				testutilSSE(`{"id":"chatcmpl-rs","choices":[{"index":0,"delta":{"role":"assistant","` + tc.key + `":"thinking hard","content":"answer"},"finish_reason":null}]}`),
				testutilSSE(`{"id":"chatcmpl-rs","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
			}, "")
			s.relayResponsesJSON(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now(), "m", "resp_rs")

			var resp struct {
				Output []map[string]any `json:"output"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("response not JSON: %v: %s", err, truncateStr(rec.Body.String(), 400))
			}
			if len(resp.Output) != 2 {
				t.Fatalf("output = %d items, want reasoning + message: %v", len(resp.Output), resp.Output)
			}
			reasoning, message := resp.Output[0], resp.Output[1]
			if reasoning["type"] != "reasoning" {
				t.Errorf("output[0].type = %v, want reasoning (reasoning must precede the message)", reasoning["type"])
			}
			if summary, ok := reasoning["summary"].([]any); !ok || len(summary) != 0 {
				t.Errorf("reasoning.summary = %v, want empty list", reasoning["summary"])
			}
			content, _ := reasoning["content"].([]any)
			if len(content) != 1 {
				t.Fatalf("reasoning.content = %v, want one reasoning_text part", reasoning["content"])
			}
			part, _ := content[0].(map[string]any)
			if part["type"] != "reasoning_text" || part["text"] != "thinking hard" {
				t.Errorf("reasoning content part = %v, want reasoning_text %q", part, "thinking hard")
			}
			if message["type"] != "message" {
				t.Errorf("output[1].type = %v, want message", message["type"])
			}
			mContent, _ := message["content"].([]any)
			if len(mContent) != 1 {
				t.Fatalf("message.content = %v, want one output_text part", message["content"])
			}
			mPart, _ := mContent[0].(map[string]any)
			if mPart["text"] != "answer" {
				t.Errorf("message text = %v, want answer (reasoning must never become output text)", mPart["text"])
			}
		})
	}
}

// TestRelayReviewFixResponsesJSONNoReasoningItem pins the negative case: a
// completion without any reasoning fields must not gain an empty reasoning
// item.
func TestRelayReviewFixResponsesJSONNoReasoningItem(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()
	ss := strings.Join([]string{
		testutilSSE(`{"id":"chatcmpl-pl","choices":[{"index":0,"delta":{"role":"assistant","content":"plain"},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-pl","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
	}, "")
	s.relayResponsesJSON(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now(), "m", "resp_pl")

	var resp struct {
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if len(resp.Output) != 1 || resp.Output[0]["type"] != "message" {
		t.Errorf("output = %v, want exactly one message item", resp.Output)
	}
}

// TestRelayReviewFixContinuationDropHelperEquivalence pins the shared
// map-level core: the bytes-based streaming entry point and the map-level
// helper must agree exactly on the mutated chunk, and both must be no-ops
// (original bytes preserved) when nothing matches.
func TestRelayReviewFixContinuationDropHelperEquivalence(t *testing.T) {
	endTurnIdx := map[int]bool{0: true}
	for _, tc := range []struct {
		name    string
		chunk   string // deliberately unsorted keys to catch re-marshal drift
		indexes map[int]bool
		dropped bool
		emptied bool
	}{
		{
			name:    "continuation dropped, real call kept",
			chunk:   `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}},{"index":1,"id":"call_r","type":"function","function":{"name":"bash","arguments":"{\"command\":\"pwd\"}"}}]},"finish_reason":null}]}`,
			indexes: endTurnIdx,
			dropped: true,
			emptied: false,
		},
		{
			name:    "sole fragment dropped empties the list",
			chunk:   `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":null}]}`,
			indexes: endTurnIdx,
			dropped: true,
			emptied: true,
		},
		{
			name:    "unrelated index untouched",
			chunk:   `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"id":"call_x","type":"function","function":{"name":"ls","arguments":"{}"}}]},"finish_reason":null}]}`,
			indexes: endTurnIdx,
			dropped: false,
			emptied: false,
		},
		{
			name:    "no tracked indexes is a no-op",
			chunk:   `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":null}]}`,
			indexes: map[int]bool{},
			dropped: false,
			emptied: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clean := []byte(tc.chunk)

			// Bytes entry point (chat streaming path).
			got := dropEndTurnContinuations(clean, tc.indexes)

			// Map entry point (Responses path) on an equal map.
			var chunk map[string]any
			if err := json.Unmarshal(clean, &chunk); err != nil {
				t.Fatalf("fixture not JSON: %v", err)
			}
			dropped, emptied := dropEndTurnContinuationsInChunk(chunk, tc.indexes)
			if dropped != tc.dropped || emptied != tc.emptied {
				t.Errorf("map helper = (dropped=%t, emptied=%t), want (%t, %t)", dropped, emptied, tc.dropped, tc.emptied)
			}

			if !tc.dropped {
				// No-op must preserve the original bytes exactly.
				if string(got) != tc.chunk {
					t.Errorf("no-op bytes changed:\n got  %s\n want %s", got, tc.chunk)
				}
				return
			}
			// Both entry points must produce byte-identical output.
			want, err := json.Marshal(chunk)
			if err != nil {
				t.Fatalf("re-marshal failed: %v", err)
			}
			if string(got) != string(want) {
				t.Errorf("bytes and map helpers diverge:\n bytes %s\n map   %s", got, want)
			}
		})
	}
}

// TestRelayReviewFixResponsesStreamEndTurnContinuation pins the Responses
// relay end-to-end on the shared helper: end_turn fragments and their
// argument continuations must never become function_call output items,
// while real calls on other indexes survive with their arguments intact.
func TestRelayReviewFixResponsesStreamEndTurnContinuation(t *testing.T) {
	t.Run("continuation never leaks, real call kept", func(t *testing.T) {
		s := testRelayServer()
		rec := httptest.NewRecorder()
		ss := strings.Join([]string{
			testutilSSE(`{"id":"chatcmpl-ce","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_e","type":"function","function":{"name":"end_turn","arguments":""}}]},"finish_reason":null}]}`),
			testutilSSE(`{"id":"chatcmpl-ce","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":null}]}`),
			testutilSSE(`{"id":"chatcmpl-ce","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_r","type":"function","function":{"name":"bash","arguments":"{\"command\":\"pwd\"}"}}]},"finish_reason":null}]}`),
			testutilSSE(`{"id":"chatcmpl-ce","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		}, "")
		s.relayResponsesStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now(), "m", "resp_ce")

		var completed map[string]any
		for _, ev := range collectSSEFrames(t, rec.Body.String()) {
			if ev.data["type"] == "response.completed" {
				completed, _ = ev.data["response"].(map[string]any)
			}
		}
		if completed == nil {
			t.Fatal("response.completed missing")
		}
		out, _ := completed["output"].([]any)
		var calls []map[string]any
		for _, raw := range out {
			if item, ok := raw.(map[string]any); ok && item["type"] == "function_call" {
				calls = append(calls, item)
			}
		}
		if len(calls) != 1 {
			t.Fatalf("function_call items = %d (%v), want exactly the real call", len(calls), out)
		}
		if calls[0]["name"] != "bash" || calls[0]["arguments"] != `{"command":"pwd"}` {
			t.Errorf("function_call = %v, want bash with its arguments", calls[0])
		}
	})

	t.Run("end_turn-only stream relays no function_call items", func(t *testing.T) {
		s := testRelayServer()
		rec := httptest.NewRecorder()
		ss := strings.Join([]string{
			testutilSSE(`{"id":"chatcmpl-eo","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_e","type":"function","function":{"name":"end_turn","arguments":""}}]},"finish_reason":null}]}`),
			testutilSSE(`{"id":"chatcmpl-eo","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":null}]}`),
			testutilSSE(`{"id":"chatcmpl-eo","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		}, "")
		s.relayResponsesStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now(), "m", "resp_eo")

		var completed map[string]any
		for _, ev := range collectSSEFrames(t, rec.Body.String()) {
			if ev.data["type"] == "response.completed" {
				completed, _ = ev.data["response"].(map[string]any)
			}
		}
		if completed == nil {
			t.Fatal("response.completed missing")
		}
		out, _ := completed["output"].([]any)
		for _, raw := range out {
			item, _ := raw.(map[string]any)
			if item == nil || item["type"] != "function_call" {
				continue
			}
			t.Errorf("end_turn-only stream leaked function_call item: %v", item)
		}
	})
}

// TestRelayReviewFixContinuationDropMapSemantics pins the helper's map
// contract directly: the stripped index's delta key is deleted when the
// list empties and surviving entries keep their original element type.
func TestRelayReviewFixContinuationDropMapSemantics(t *testing.T) {
	t.Run("sole fragment dropped empties the list", func(t *testing.T) {
		chunk := map[string]any{
			"choices": []any{map[string]any{
				"index": float64(0),
				"delta": map[string]any{
					"role": "assistant",
					"tool_calls": []any{
						map[string]any{"index": float64(0), "function": map[string]any{"arguments": "{}"}},
					},
				},
			}},
		}
		dropped, emptied := dropEndTurnContinuationsInChunk(chunk, map[int]bool{0: true})
		if !dropped || !emptied {
			t.Fatalf("(dropped, emptied) = (%t, %t), want (true, true)", dropped, emptied)
		}
		choice := chunk["choices"].([]any)[0].(map[string]any)
		delta := choice["delta"].(map[string]any)
		if _, ok := delta["tool_calls"]; ok {
			t.Errorf("emptied tool_calls key must be deleted, got %v", delta["tool_calls"])
		}
		if delta["role"] != "assistant" {
			t.Errorf("sibling delta fields must survive, got %v", delta)
		}
		if !reflect.DeepEqual(choice["index"], float64(0)) {
			t.Errorf("choice fields must survive untouched")
		}
	})

	t.Run("non-map entries pass through and keep the list alive", func(t *testing.T) {
		chunk := map[string]any{
			"choices": []any{map[string]any{
				"index": float64(0),
				"delta": map[string]any{
					"tool_calls": []any{
						map[string]any{"index": float64(0), "function": map[string]any{"arguments": "{}"}},
						"kept-as-is", // non-map entries pass through untouched
					},
				},
			}},
		}
		dropped, emptied := dropEndTurnContinuationsInChunk(chunk, map[int]bool{0: true})
		if !dropped || emptied {
			t.Fatalf("(dropped, emptied) = (%t, %t), want (true, false)", dropped, emptied)
		}
		delta := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		tcs, _ := delta["tool_calls"].([]any)
		if len(tcs) != 1 || tcs[0] != "kept-as-is" {
			t.Errorf("tool_calls = %v, want the untouched non-map entry", tcs)
		}
	})
}

// TestRelayReviewFixRateLimitedLogLevel pins the log-level split: a routine
// client-caused 429 rate_limited (daily session quota) logs at Info, while
// upstream-class failures (bans) keep Warn. Pre-fix, both were Warn.
func TestRelayReviewFixRateLimitedLogLevel(t *testing.T) {
	logEntry := func(t *testing.T, code string) *logring.Entry {
		t.Helper()
		ring := logring.NewHandler(slog.NewTextHandler(io.Discard, nil), 500)
		s := &Server{logger: slog.New(ring)}
		var err error
		switch code {
		case "rate_limited":
			err = &upstream.RateLimitError{RetryAfter: time.Minute, Window: "reset", Body: "daily quota exhausted"}
		case "account_banned":
			err = &upstream.BanError{ResumesAt: time.Now().Add(time.Hour), Body: `{"status":"banned"}`}
		default:
			t.Fatalf("unknown fixture code %q", code)
		}
		s.writeError(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), err, "", nil)
		for i, e := range ring.Recent(500) {
			if e.Message != "request failed" {
				continue
			}
			for _, f := range e.Fields {
				if f == "code="+code {
					return &ring.Recent(500)[i]
				}
			}
		}
		return nil
	}

	t.Run("rate_limited logs at Info", func(t *testing.T) {
		e := logEntry(t, "rate_limited")
		if e == nil {
			t.Fatal("request failed entry missing for rate_limited")
		}
		if e.Level != "INFO" {
			t.Errorf("rate_limited log level = %s, want INFO (routine client-caused 429)", e.Level)
		}
	})
	t.Run("banned keeps Warn", func(t *testing.T) {
		e := logEntry(t, "account_banned")
		if e == nil {
			t.Fatal("request failed entry missing for account_banned")
		}
		if e.Level != "WARN" {
			t.Errorf("account_banned log level = %s, want WARN (upstream-class failure)", e.Level)
		}
	})
}

// TestRelayReviewFixCostModeHintMatchesValidator pins the out_of_credits
// hint against the actual COST_MODE startup validation: valid values are
// free or unset; any other value fails startup (the old text claimed a
// typo routes requests as PAID, which startup validation now rejects).
func TestRelayReviewFixCostModeHintMatchesValidator(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()
	s.writeError(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		&upstream.CreditsError{Body: "Out of credits"}, "", nil)

	var body struct {
		Error struct {
			Code string `json:"code"`
			Hint string `json:"hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body not JSON: %v: %s", err, truncateStr(rec.Body.String(), 400))
	}
	if body.Error.Code != "out_of_credits" {
		t.Fatalf("code = %q, want out_of_credits", body.Error.Code)
	}
	if !strings.Contains(body.Error.Hint, "free or unset") || !strings.Contains(body.Error.Hint, "fails startup validation") {
		t.Errorf("hint = %q, want the validator-accurate COST_MODE wording", body.Error.Hint)
	}
	if strings.Contains(body.Error.Hint, "routes requests as PAID") {
		t.Errorf("hint = %q, still carries the stale typo-routes-as-PAID wording", body.Error.Hint)
	}
}
