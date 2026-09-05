package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestToolMapperRequestRename pins the request-side rename (#140): mapped
// client names become official signature names on the wire; schemas pass
// through untouched; unmapped and already-official names are unchanged.
func TestToolMapperRequestRename(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[
		{"type":"function","function":{"name":"read_file","description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}},
		{"type":"function","function":{"name":"bash","description":"Run a command","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}},
		{"type":"function","function":{"name":"my_custom_tool","parameters":{"type":"object","properties":{}}}},
		{"type":"function","function":{"name":"write_file","description":"Official already","parameters":{"type":"object","properties":{}}}}
	],"tool_choice":{"type":"function","function":{"name":"read_file"}}}`)

	out, mapper, err := NormalizeRequestMapped(body, "")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}

	names := map[string]map[string]any{}
	tools := payload["tools"].([]any)
	for _, tl := range tools {
		fn := tl.(map[string]any)["function"].(map[string]any)
		names[fn["name"].(string)] = fn
	}

	if _, ok := names["read_files"]; !ok {
		t.Errorf("read_file not renamed to read_files: tools = %v", payload["tools"])
	}
	if _, ok := names["run_terminal_command"]; !ok {
		t.Errorf("bash not renamed to run_terminal_command")
	}
	if _, ok := names["my_custom_tool"]; !ok {
		t.Error("unmapped custom tool was altered")
	}
	if _, ok := names["read_file"]; ok {
		t.Error("original client name still present after rename")
	}

	// Schema passes through untouched (the model fills args per this shape).
	rf := names["read_files"]
	params := rf["parameters"].(map[string]any)
	reqd := params["required"].([]any)
	if len(reqd) != 1 || reqd[0] != "path" {
		t.Errorf("client schema mutated: required = %v", reqd)
	}

	// tool_choice follows the rename.
	tc := payload["tool_choice"].(map[string]any)
	tcfn := tc["function"].(map[string]any)
	if tcfn["name"] != "read_files" {
		t.Errorf("tool_choice name = %v, want read_files", tcfn["name"])
	}

	// The mapper restores both directions.
	if got := mapper.RestoreName("read_files"); got != "read_file" {
		t.Errorf("RestoreName(read_files) = %q, want read_file", got)
	}
	if got := mapper.RestoreName("end_turn"); got != "end_turn" {
		t.Errorf("RestoreName(end_turn) = %q, want identity", got)
	}
}

// TestToolMapperResponseRestore pins the response-side restore on both chunk
// shapes: streaming delta.tool_calls and non-streaming message.tool_calls.
func TestToolMapperResponseRestore(t *testing.T) {
	mapper := NewToolMapper([]byte(`{"tools":[
		{"type":"function","function":{"name":"bash"}},
		{"type":"function","function":{"name":"write_to_file"}}
	]}`))
	if mapper.Len() != 2 {
		t.Fatalf("mapper entries = %d, want 2", mapper.Len())
	}

	streamChunk := map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{
				"tool_calls": []any{
					map[string]any{"index": float64(0), "function": map[string]any{"name": "run_terminal_command", "arguments": "{}"}},
				},
			},
		}},
	}
	if !mapper.FromUpstreamChunk(streamChunk) {
		t.Fatal("stream chunk unchanged by restore")
	}
	delta := streamChunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	fn := delta["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "bash" {
		t.Errorf("stream restored name = %v, want bash", fn["name"])
	}

	jsonChunk := map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"tool_calls": []any{
					map[string]any{"id": "c1", "function": map[string]any{"name": "write_file"}},
				},
			},
		}},
	}
	if !mapper.FromUpstreamChunk(jsonChunk) {
		t.Fatal("json chunk unchanged by restore")
	}
	msg := jsonChunk["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	jfn := msg["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if jfn["name"] != "write_to_file" {
		t.Errorf("json restored name = %v, want write_to_file", jfn["name"])
	}

	// Identity mapper never reports change.
	empty := ToolMapper{}
	if empty.FromUpstreamChunk(streamChunk) {
		t.Error("identity mapper changed a chunk")
	}
}

// TestToolMapperSignatureCoverage guards the layer's whole point: every
// mapping target must be a signature tool upstream recognizes, so renamed
// requests classify first-party under detectForeignFreebuffClient.
func TestToolMapperSignatureCoverage(t *testing.T) {
	for client, official := range clientToOfficial {
		if official == "" {
			continue
		}
		if !officialTools[official] {
			t.Errorf("%q maps to %q which is not in officialTools", client, official)
		}
		if strings.ToLower(client) != client {
			t.Errorf("mapping key %q is not lowercase; lookups always lowercase", client)
		}
	}
}

// TestToolMapperCounts pins the console MSG/TOOL segments: NewToolMapper
// retains the client message and tool counts from its single body parse
// (Responses input[] counts as messages). Zero value and garbage bodies
// report 0 so the console omits the segments.
func TestToolMapperCounts(t *testing.T) {
	chat := NewToolMapper([]byte(`{"model":"m","messages":[{"role":"u"},{"role":"a"},{"role":"u"}],"tools":[{"function":{"name":"a"}},{"function":{"name":"b"}}]}`))
	if chat.MsgCount() != 3 {
		t.Errorf("chat msgs = %d, want 3", chat.MsgCount())
	}
	if chat.ToolCount() != 2 {
		t.Errorf("chat tools = %d, want 2", chat.ToolCount())
	}
	resp := NewToolMapper([]byte(`{"model":"m","input":[{"role":"u"},{"role":"u"}],"tools":[]}`))
	if resp.MsgCount() != 2 {
		t.Errorf("responses msgs = %d, want 2 (input[])", resp.MsgCount())
	}
	if resp.ToolCount() != 0 {
		t.Errorf("responses tools = %d, want 0", resp.ToolCount())
	}
	var zero ToolMapper
	if zero.MsgCount() != 0 || zero.ToolCount() != 0 {
		t.Error("zero mapper counts nonzero")
	}
	bad := NewToolMapper([]byte(`not json`))
	if bad.MsgCount() != 0 || bad.ToolCount() != 0 {
		t.Error("garbage body counts nonzero")
	}
}

// TestAllHarnessToolsBidirectionalMapping verifies that every tool from the
// client harnesses in reference/harnesses renames to an official signature tool for upstream
// and restores cleanly back to the client's original casing/name downstream.
func TestAllHarnessToolsBidirectionalMapping(t *testing.T) {
	testCases := []struct {
		harness      string
		clientTool   string
		wantOfficial string
	}{
		// Qwen-Code
		{"Qwen-Code", "run_shell_command", "run_terminal_command"},
		{"Qwen-Code", "grep_search", "code_search"},
		{"Qwen-Code", "todo_write", "write_todos"},
		{"Qwen-Code", "web_fetch", "read_url"},
		// Goose
		{"Goose", "developer__shell", "run_terminal_command"},
		{"Goose", "developer__bash", "run_terminal_command"},
		{"Goose", "developer__text_editor", "str_replace"},
		{"Goose", "developer__read", "read_files"},
		{"Goose", "developer__write", "write_file"},
		{"Goose", "developer__edit", "str_replace"},
		{"Goose", "computer__execute", "run_terminal_command"},
		// Continue (camelCase client tools)
		{"Continue", "readFile", "read_files"},
		{"Continue", "editFile", "str_replace"},
		{"Continue", "createNewFile", "write_file"},
		{"Continue", "runTerminalCommand", "run_terminal_command"},
		{"Continue", "grepSearch", "code_search"},
		{"Continue", "globSearch", "glob"},
		{"Continue", "fetchUrlContent", "read_url"},
		{"Continue", "searchWeb", "web_search"},
		{"Continue", "viewSubdirectory", "list_directory"},
		{"Continue", "singleFindAndReplace", "str_replace"},
		// Roo-Code / Cline
		{"Roo-Code", "apply_diff", "apply_patch"},
		{"Roo-Code", "edit_file", "str_replace"},
		{"Roo-Code", "search_replace", "str_replace"},
		{"Roo-Code", "search_and_replace", "str_replace"},
		{"Roo-Code", "codebase_search", "code_search"},
		{"Roo-Code", "update_todo_list", "write_todos"},
		{"Roo-Code", "read_command_output", "run_terminal_command"},
		{"Cline", "editor", "str_replace"},
		{"Cline", "fetch_web", "read_url"},
		{"Cline", "search", "code_search"},
		// Aider
		{"Aider", "replace_lines", "str_replace"},
		// Codex
		{"Codex", "exec_command", "run_terminal_command"},
		{"Codex", "exec", "run_terminal_command"},
		// Pi / OMP
		{"Pi", "powershell", "run_terminal_command"},
		{"Pi", "find", "find_files"},
		{"Pi", "edit-diff", "apply_patch"},
		// Kilocode / OpenCode
		{"Kilocode", "execute_bash", "run_terminal_command"},
		{"Kilocode", "fuzzy_search", "code_search"},
		{"Kilocode", "list_dir", "list_directory"},
		{"Kilocode", "websearch", "web_search"},
		{"Kilocode", "webfetch", "read_url"},
		// Gemini-CLI
		{"Gemini-CLI", "read_many_files", "read_files"},
		// Claude Code (PascalCase names; mapping is case-insensitive and
		// restore must return the EXACT client casing)
		{"Claude-Code", "Bash", "run_terminal_command"},
		{"Claude-Code", "Read", "read_files"},
		{"Claude-Code", "Edit", "str_replace"},
		{"Claude-Code", "Write", "write_file"},
		{"Claude-Code", "Grep", "code_search"},
		{"Claude-Code", "LS", "list_directory"},
		{"Claude-Code", "TodoWrite", "write_todos"},
		{"Claude-Code", "WebFetch", "read_url"},
		{"Claude-Code", "WebSearch", "web_search"},
	}

	for _, tc := range testCases {
		t.Run(tc.harness+"/"+tc.clientTool, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{
				"model":    "m",
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
				"tools": []any{
					map[string]any{
						"type": "function",
						"function": map[string]any{
							"name":       tc.clientTool,
							"parameters": map[string]any{"type": "object"},
						},
					},
				},
			})

			norm, mapper, err := NormalizeRequestMapped(body, "")
			if err != nil {
				t.Fatalf("NormalizeRequestMapped failed: %v", err)
			}

			var parsed map[string]any
			if err := json.Unmarshal(norm, &parsed); err != nil {
				t.Fatalf("unmarshal normalized failed: %v", err)
			}

			tools := parsed["tools"].([]any)
			fn := tools[0].(map[string]any)["function"].(map[string]any)
			gotOfficial := fn["name"].(string)
			if gotOfficial != tc.wantOfficial {
				t.Errorf("normalized tool name = %q, want %q", gotOfficial, tc.wantOfficial)
			}

			// Downstream restoration must restore the client's exact name
			restored := mapper.RestoreName(gotOfficial)
			if restored != tc.clientTool {
				t.Errorf("RestoreName(%q) = %q, want %q", gotOfficial, restored, tc.clientTool)
			}
		})
	}
}
