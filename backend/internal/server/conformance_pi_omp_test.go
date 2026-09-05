package server_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// Hermetic "real user usage" conformance tests for the pi / Oh My Pi (OMP)
// coding agents (reference/harnesses/pi, reference/harnesses/oh-my-pi)
// against all three proxy surfaces.
//
// pi's core coding-agent tool registry
// (packages/coding-agent/src/core/tools/{bash,edit,edit-diff,find,grep,ls,
// powershell,read,write}.ts) emits the CLASSIC tool names (bash, edit, read,
// ...), and OMP's BUILTIN_TOOL_NAMES (packages/coding-agent/src/tools/
// builtin-names.ts) adds todo/web_search plus first-class loop tools with NO
// official codebuff equivalent (ask, task, hub, eval, lsp, browser, computer,
// github, ast_grep, ast_edit, checkpoint, rewind, security_scan, memory_edit,
// learn, manage_skill, debug, inspect_image).
//
// Contract under test (issue #140 + foreign_toolset gate):
//   1. Every classic pi/OMP tool with an official signature equivalent is
//      renamed upstream and restored to the EXACT client name downstream on
//      ALL surfaces (chat, responses, anthropic).
//   2. Tools with no equivalent pass through VERBATIM both ways, and the
//      injected end_turn keeps the request first-party regardless.
//   3. A tool_choice pinned to a mapped name is re-pointed at the official
//      name upstream.

// tool renders one OpenAI function tool entry for a request body.
func tool(name, params string) string {
	return `{"type":"function","function":{"name":"` + name + `","description":"` + name + ` tool","parameters":` + params + `}}`
}

// frameSetHasToolCall reports whether any collected SSE frame carries a
// tool_calls segment whose function name equals want.
func frameSetHasToolCall(frames []map[string]any, want string) bool {
	for _, f := range frames {
		choices, _ := f["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
		tcs, _ := delta["tool_calls"].([]any)
		for _, tc := range tcs {
			fn, _ := tc.(map[string]any)["function"].(map[string]any)
			if n, _ := fn["name"].(string); n == want {
				return true
			}
		}
	}
	return false
}

// TestConformancePiChatToolRenameRestore replays pi's OpenAI-completions
// surface (pi config api:"openai-completions"): the classic tool set in one
// turn. The upstream wire must carry the official signature names + the
// injected end_turn; the model's calls arrive back under the CLIENT names.
func TestConformancePiChatToolRenameRestore(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Upstream model calls three tools by their OFFICIAL names.
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-pi", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_p0","type":"function","function":{"name":"read_files","arguments":"{\"paths\":[\"a.go\"]}"}}]},"index":0}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-pi", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_p1","type":"function","function":{"name":"str_replace","arguments":"{\"path\":\"a.go\",\"old_string\":\"x\",\"new_string\":\"y\"}"}}]},"index":0}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-pi", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_p2","type":"function","function":{"name":"code_search","arguments":"{\"pattern\":\"TODO\",\"path\":\".\"}"}}]},"index":0}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-pi", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":30,"completion_tokens":20,"total_tokens":50}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	ts, _ := newTestServer(t, []string{"pi-key"}, mock)
	// pi core tools, minus powershell (bash-flavored turn).
	piTools := `"tools":[` +
		tool("read", `{"type":"object","properties":{"paths":{"type":"array","items":{"type":"string"}}},"required":["paths"]}`) + `,` +
		tool("bash", `{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`) + `,` +
		tool("edit", `{"type":"object","properties":{"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"}},"required":["path","old_string","new_string"]}`) + `,` +
		tool("write", `{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`) + `,` +
		tool("grep", `{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"]}`) + `,` +
		tool("ls", `{"type":"object","properties":{"path":{"type":"string"}},"required":[]}`) + `,` +
		tool("find", `{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"]}`) + `,` +
		tool("edit-diff", `{"type":"object","properties":{"file_path":{"type":"string"},"diff":{"type":"string"}},"required":["file_path","diff"]}`) + `]`

	reqBody := `{"model":"` + modelA + `","messages":[{"role":"user","content":"fix it"}],"stream":true,"stream_options":{"include_usage":true},` + piTools + `}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(reqBody), map[string]string{"Authorization": "Bearer pi-key"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}

	// Upstream wire carries official names + the first-party end_turn pin.
	recorded := mock.RecordedChatBodies[0]
	for _, want := range []string{
		`"name":"read_files"`, `"name":"run_terminal_command"`, `"name":"str_replace"`,
		`"name":"write_file"`, `"name":"code_search"`, `"name":"list_directory"`,
		`"name":"find_files"`, `"name":"apply_patch"`, `"end_turn"`,
	} {
		if !strings.Contains(recorded, want) {
			t.Errorf("upstream body missing %s: %s", want, recorded)
		}
	}
	for _, gone := range []string{`"name":"read"`, `"name":"bash"`, `"name":"edit"`, `"name":"grep"`} {
		if strings.Contains(recorded, gone) {
			t.Errorf("upstream body still has client tool name %s: %s", gone, recorded)
		}
	}

	// Downstream: the model's official-named calls arrive under pi names.
	frames, done := collectOpenAIFrames(t, string(data))
	if !done {
		t.Error("stream missing [DONE]")
	}
	for _, want := range []string{"read", "edit", "grep"} {
		if !frameSetHasToolCall(frames, want) {
			t.Errorf("client stream missing restored tool call %q", want)
		}
	}
}

// TestConformancePiPowershellWindowsTurn replays the Windows-flavored pi turn
// (pi registers powershell.ts on win32; bash.ts on unix — a session never
// sends both, which is what keeps the shared run_terminal_command target
// unambiguous). Assert powershell renames upstream and restores downstream.
func TestConformancePiPowershellWindowsTurn(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-ps", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_ps1","type":"function","function":{"name":"run_terminal_command","arguments":"{\"command\":\"dir\"}"}}]},"index":0}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-ps", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, []string{"pi-key"}, mock)
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"dir"}],"stream":true,` +
		`"tools":[` + tool("powershell", `{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`) + `]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), map[string]string{"Authorization": "Bearer pi-key"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	if !mock.BodyContains(`"name":"run_terminal_command"`) {
		t.Errorf("upstream body missing renamed powershell: %s", mock.RecordedChatBodies[0])
	}
	frames, _ := collectOpenAIFrames(t, string(data))
	if !frameSetHasToolCall(frames, "powershell") {
		t.Error("client stream missing restored powershell tool call")
	}
}

// TestConformanceOmpUnmappedLoopToolsPassthrough replays an OMP turn carrying
// the first-class loop tools that have NO official signature equivalent
// (ask/task/hub/eval/lsp/browser/computer/github/ast_grep/ast_edit/
// checkpoint/rewind/security_scan/memory_edit/learn/manage_skill/debug/
// inspect_image) plus the mapped todo and the official web_search. The
// unmapped names must round-trip VERBATIM (never renamed to a wrong target,
// never dropped), the mapped todo renamed, and end_turn still injected so
// the foreign_toolset gate never fires.
func TestConformanceOmpUnmappedLoopToolsPassthrough(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Model calls the unmapped ask tool by its own name.
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-omp", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_ask1","type":"function","function":{"name":"ask","arguments":"{\"question\":\"ok?\"}"}}]},"index":0}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-omp", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, []string{"omp-key"}, mock)

	var tools []string
	for _, name := range []string{"ask", "task", "hub", "eval", "lsp", "browser", "computer", "github", "ast_grep", "ast_edit", "checkpoint", "rewind", "security_scan", "memory_edit", "learn", "manage_skill", "debug", "inspect_image", "todo", "web_search"} {
		tools = append(tools, tool(name, `{"type":"object","properties":{},"required":[]}`))
	}
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"stream":true,"tools":[` + strings.Join(tools, ",") + `]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), map[string]string{"Authorization": "Bearer omp-key"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	recorded := mock.RecordedChatBodies[0]
	for _, keep := range []string{"ask", "task", "hub", "eval", "lsp", "ast_grep", "security_scan", "web_search"} {
		if !strings.Contains(recorded, `"name":"`+keep+`"`) {
			t.Errorf("upstream body missing verbatim tool %q: %s", keep, recorded)
		}
	}
	if !strings.Contains(recorded, `"name":"write_todos"`) {
		t.Error("upstream body missing renamed todo → write_todos")
	}
	if !strings.Contains(recorded, "end_turn") {
		t.Error("upstream body missing injected end_turn (foreign_toolset gate would downgrade)")
	}
	frames, done := collectOpenAIFrames(t, string(data))
	if !done {
		t.Error("stream missing [DONE]")
	}
	if !frameSetHasToolCall(frames, "ask") {
		t.Error("client stream missing restored ask call")
	}
}

// TestConformancePiAnthropicToolRenameRestore replays pi's Anthropic Messages
// surface (api:"anthropic-messages", x-api-key auth): flat tools[].name
// entries are renamed on the translated upstream chat wire, and the tool_use
// content block the client parses opens with the CLIENT name.
func TestConformancePiAnthropicToolRenameRestore(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-anth", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a1","type":"function","function":{"name":"run_terminal_command","arguments":"{\"command\":\"ls\"}"}}]},"index":0}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-anth", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":15,"completion_tokens":8,"total_tokens":23}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, []string{"pi-key"}, mock)
	headers := map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         "pi-key",
		"anthropic-version": "2023-06-01",
	}
	body := `{"model":"` + modelA + `","max_tokens":4096,"messages":[{"role":"user","content":"ls"}],` +
		`"tools":[{"name":"bash","description":"Run a command","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}],` +
		`"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	if !mock.BodyContains(`"name":"run_terminal_command"`) || !mock.BodyContains(`"end_turn"`) {
		t.Error("upstream chat body missing renamed tool / end_turn pin")
	}
	events := collectAnthropicEvents(t, string(data))
	idx, name, id := continueToolUseBlock(events)
	if name != "bash" || id != "call_a1" {
		t.Errorf("tool_use name/id = %q/%q, want bash/call_a1 (client dispatch name must survive)", name, id)
	}
	if args := replayInputFragments(events, idx); args != `{"command":"ls"}` {
		t.Errorf("assembled tool_use input = %q", args)
	}
	stop, _ := replayMessageDelta(events)
	if stop != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", stop)
	}
	if last := events[len(events)-1]; last["type"] != "message_stop" {
		t.Errorf("last event = %v, want message_stop", last["type"])
	}
}

// TestConformanceResponsesSurfaceToolRename replays a flat-tools Responses
// request (the pi openai-responses api shape): the flat function name is
// renamed on the upstream chat wire and the function_call item streamed back
// carries the CLIENT name.
func TestConformanceResponsesSurfaceToolRename(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-resp", 1,
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_r1","type":"function","function":{"name":"run_terminal_command","arguments":"{\"command\":\"pwd\"}"}}]},"index":0}]`)))
		_, _ = io.WriteString(w, testutil.SSEEvent(chunk("cmpl-resp", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13}`)))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newTestServer(t, []string{"pi-key"}, mock)
	body := `{"model":"` + modelA + `","input":[{"role":"user","content":[{"type":"input_text","text":"pwd"}]}],"stream":true,` +
		`"tools":[{"type":"function","name":"bash","description":"Run a command","strict":false,"parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}],"tool_choice":"auto"}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), map[string]string{"Authorization": "Bearer pi-key"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	if !mock.BodyContains(`"name":"run_terminal_command"`) || !mock.BodyContains(`"end_turn"`) {
		t.Error("upstream body missing renamed tool / end_turn pin")
	}
	if !strings.Contains(string(data), `"name":"bash"`) {
		t.Errorf("responses stream missing client tool name bash: %s", truncate(string(data), 400))
	}
}

// TestConformanceToolChoicePinnedRename replays a chat request whose
// tool_choice pins a mapped client tool: the choice must be re-pointed at
// the official name upstream.
func TestConformanceToolChoicePinnedRename(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}
	ts, _ := newTestServer(t, []string{"pi-key"}, mock)
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"run pwd"}],` +
		`"tools":[` + tool("bash", `{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`) + `],` +
		`"tool_choice":{"type":"function","function":{"name":"bash"}}}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), map[string]string{"Authorization": "Bearer pi-key"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	recorded := mock.RecordedChatBodies[0]
	if !strings.Contains(recorded, `"tool_choice":{"function":{"name":"run_terminal_command"}`) {
		t.Errorf("upstream tool_choice not re-pointed at official name: %s", recorded)
	}
}
