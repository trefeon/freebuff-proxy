package server_test

// Conformance replay for aider against the OpenAI chat/completions surface.
// aider declares exactly ONE function tool and FORCES it via tool_choice
// (reference/harnesses/aider/WIRE-NOTES.md §5: tools=[{type:"function",
// function:{...}}] + tool_choice={"type":"function","function":{"name":…}},
// models.py:1006-1009). Multi-tool / auto tool_choice is never used.
//
// Two aider-critical contracts are pinned here:
//   1. the forced named choice passes through to the upstream intact;
//   2. finish_reason "length" is relayed VERBATIM — aider raises
//      FinishReasonLength on it (base_coder.py:1863-1866) and may resend
//      with an assistant prefill; a proxy that swallows or flips "length"
//      breaks aider's context-exhaustion handling.

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

func TestConformanceAiderForcedFunction(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-aider1", 800,
			`"choices":[{"index":0,"delta":{"role":"assistant","content":"partial edit"},"finish_reason":null}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("chatcmpl-aider1", 800,
			`"choices":[{"index":0,"delta":{},"finish_reason":"length"}]`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, nil, mock)

	// aider shape: single function tool + hard named tool_choice.
	body := `{"model":"` + modelA + `",` +
		`"messages":[{"role":"user","content":"fix the bug"}],` +
		`"tools":[{"type":"function","function":{"name":"write_file","description":"Write a file","parameters":{"type":"object"}}}],` +
		`"tool_choice":{"type":"function","function":{"name":"write_file"}},` +
		`"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}

	// Request translation: the forced named choice reaches the upstream.
	recorded := mock.LastChatBody()
	if !strings.Contains(recorded, `"name":"write_file"`) {
		t.Errorf("upstream body missing forced tool name: %s", truncate(recorded, 400))
	}
	if !strings.Contains(recorded, `"tool_choice"`) {
		t.Errorf("upstream body missing forced tool_choice: %s", truncate(recorded, 400))
	}

	// Response relay: "length" arrives verbatim so aider can raise.
	frames, done := collectOpenAIFrames(t, string(data))
	if !done {
		t.Error("stream missing [DONE]")
	}
	fr, ok := findTerminalFinish(frames)
	if !ok {
		t.Fatal("no terminal finish_reason")
	}
	if fr != "length" {
		t.Errorf("terminal finish_reason = %q, want length (aider drives FinishReasonLength off it)", fr)
	}
}
