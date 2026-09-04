package server_test

// Conformance replay for qwen-code's OpenAI-compatible path against the
// chat/completions surface. qwen-code is strict about two transport-level
// contracts (reference/harnesses/qwen-code/WIRE-NOTES.md §10):
//   1. SSE content-type gate (pipeline.ts:197-205): an HTTP 200 streaming
//      response must be text/event-stream (or ndjson); anything else
//      throws NonSSEResponseError.
//   2. thinking-tag leak guard (pipeline.ts:648-651): literal <think> tags
//      inside content are rejected as PROTOCOL_TAG_LEAK — reasoning must
//      travel the reasoning_content channel, never flattened into content
//      by the proxy. qwen sends stream_options:{include_usage:true} on
//      EVERY streaming request (pipeline.ts:816).

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

func TestConformanceQwenStreamGates(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Reasoning on its own channel + clean content (no think tags).
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-qwen1", 750,
			`"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"need to check the weather"},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-qwen1", 750,
			`"choices":[{"index":0,"delta":{"content":"Checking now."},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-qwen1", 750,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(`{"id":"chatcmpl-qwen1","object":"chat.completion.chunk",`+
			`"created":750,"model":"`+modelA+`","choices":[],`+
			`"usage":{"prompt_tokens":40,"completion_tokens":15,"total_tokens":55}}`))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, nil, mock)

	// qwen shape: stream + include_usage on every streaming request.
	body := `{"model":"` + modelA + `",` +
		`"messages":[{"role":"user","content":"weather?"}],` +
		`"stream":true,"stream_options":{"include_usage":true}}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream (qwen NonSSEResponseError gate)", ct)
	}

	frames, done := collectOpenAIFrames(t, string(data))
	if !done {
		t.Error("stream missing [DONE]")
	}
	sawReasoning, sawUsage := false, false
	for _, f := range frames {
		if d := openAIDelta(f); d != nil {
			if rc, _ := d["reasoning_content"].(string); rc != "" {
				sawReasoning = true
			}
			// The proxy must never flatten reasoning into content with
			// thinking tags (qwen PROTOCOL_TAG_LEAK guard).
			if c, _ := d["content"].(string); strings.Contains(c, "<think>") || strings.Contains(c, "<thinking>") {
				t.Errorf("content delta carries thinking tags: %q", c)
			}
		}
		if u := openAIUsage(f); u != nil {
			sawUsage = true
		}
	}
	if !sawReasoning {
		t.Error("no reasoning_content delta reached the client (qwen thinking channel)")
	}
	if !sawUsage {
		t.Error("no usage frame reached the client (qwen sends include_usage always)")
	}
	if fr, ok := findTerminalFinish(frames); !ok || fr != "stop" {
		t.Errorf("terminal finish_reason = %q, want stop", fr)
	}
}
