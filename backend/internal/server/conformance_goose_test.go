package server_test

// Conformance replay for goose's declarative OpenAI-compatible path against
// the chat/completions surface. goose sets NO tool_choice at all
// (reference/harnesses/goose/WIRE-NOTES.md: formats/openai.rs:1704-1712 —
// the model may pick zero or many tools), always sends
// stream_options:{include_usage:true} (formats/openai.rs:1766-1768), and
// consumes usage from a trailing choices:[] chunk (1279-1291).
//
// Two goose-critical contracts are pinned:
//   1. intermediate deltas carry finish_reason null — never "" — because an
//      empty string is explicitly NON-terminal for goose (formats/openai.rs
//      :154) and a proxy emitting "" mid-stream corrupts turn assembly;
//   2. a trailing usage-only chunk (choices:[] + usage) is forwarded to the
//      client verbatim so goose's usage reconciliation sees it.

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

func TestConformanceGooseNoToolChoiceUsageTail(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-goose1", 700,
			`"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-goose1", 700,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`)))
		// Trailing usage-only chunk, goose's reconciliation source.
		_, _ = io.WriteString(w, testutil.SSEEvent(`{"id":"chatcmpl-goose1","object":"chat.completion.chunk",`+
			`"created":700,"model":"`+modelA+`","choices":[],`+
			`"usage":{"prompt_tokens":30,"completion_tokens":10,"total_tokens":40}}`))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, nil, mock)

	// goose shape: no tools, no tool_choice, include_usage on.
	body := `{"model":"` + modelA + `",` +
		`"messages":[{"role":"user","content":"hi"}],` +
		`"stream":true,"stream_options":{"include_usage":true}}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	frames, done := collectOpenAIFrames(t, string(data))
	if !done {
		t.Error("stream missing [DONE]")
	}
	sawUsage := false
	for _, f := range frames {
		// Every choices-bearing frame must carry null or a real reason —
		// never "" (goose treats "" as non-terminal noise).
		if ch, ok := f["choices"].([]any); ok && len(ch) > 0 {
			if c0, ok := ch[0].(map[string]any); ok {
				if fr, present := c0["finish_reason"]; present {
					if s, _ := fr.(string); fr != nil && s == "" {
						t.Errorf("frame carries empty-string finish_reason: %v", f)
					}
				}
			}
		}
		if u := openAIUsage(f); u != nil {
			sawUsage = true
		}
	}
	if !sawUsage {
		t.Error("no usage frame reached the client (goose reconciles usage from the trailing chunk)")
	}
	if fr, ok := findTerminalFinish(frames); !ok || fr != "stop" {
		t.Errorf("terminal finish_reason = %q, want stop", fr)
	}
	if strings.Contains(mock.LastChatBody(), `"tool_choice"`) {
		t.Errorf("upstream body carries tool_choice goose never sent: %s", truncate(mock.LastChatBody(), 300))
	}
}
