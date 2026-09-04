package server_test

// Conformance replay for Roo Code's strict function tools against the
// OpenAI chat/completions surface. Roo converts every function schema for
// OpenAI strict mode (reference/harnesses/Roo-Code/WIRE-NOTES.md §5:
// base-provider.ts:33-110 — strict:true, all props in required, nullable
// types unwrapped, additionalProperties:false). A proxy whose schema
// normalization strips or rewrites those strict markers silently downgrades
// Roo's structured-output contract, so the round trip must preserve them
// byte-for-byte.

import (
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

func TestConformanceRooStrictSchemaPreserved(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Non-streaming: single JSON completion answering a native tool call.
	mock.ChatBody = `{"id":"chatcmpl-roo1","object":"chat.completion","created":900,"model":"` + modelA + `",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":null,` +
		`"tool_calls":[{"index":0,"id":"call_roo_1","type":"function","function":{"name":"execute_command","arguments":"{\"command\":\"ls\"}"}}]},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":60,"completion_tokens":20,"total_tokens":80}}`
	ts, _ := newTestServer(t, nil, mock)

	// Roo strict shape: strict:true + additionalProperties:false + full
	// required list on the function parameters.
	body := `{"model":"` + modelA + `",` +
		`"messages":[{"role":"user","content":"list files"}],` +
		`"tools":[{"type":"function","function":{"name":"execute_command","description":"Run a command",` +
		`"strict":true,"parameters":{"type":"object","properties":{"command":{"type":"string"}}},` +
		`"required":["command"],"additionalProperties":false}}],` +
		`"tool_choice":"auto"}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}

	// The upstream must see Roo's strict markers untouched. Note
	// execute_command renames to the official run_terminal_command
	// (toolmap), but the SCHEMA around it must survive verbatim.
	recorded := mock.LastChatBody()
	for _, want := range []string{`"strict":true`, `"additionalProperties":false`, `"required":["command"]`} {
		if !strings.Contains(recorded, want) {
			t.Errorf("upstream body missing strict marker %s: %s", want, truncate(recorded, 500))
		}
	}
	if !strings.Contains(recorded, `"run_terminal_command"`) {
		t.Errorf("upstream body missing renamed tool: %s", truncate(recorded, 400))
	}
}
