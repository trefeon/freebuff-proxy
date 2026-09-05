package convert

import (
	"encoding/json"
	"testing"
)

// TestComprehensiveToolClassification is the full-coverage tool-call table
// (2026-09-05): every researched client tool name across reference/harnesses
// and reference/agents, classified by the CURRENT behavior of clientToOfficial
// + officialTools:
//
//	official   -> name already IS a codebuff signature tool: unchanged on the
//	             upstream wire, identity restore.
//	mapped     -> renamed upstream to the official signature tool, restored
//	             downstream to the EXACT client casing the client dispatched.
//	passthrough-> no official equivalent: name unchanged on the upstream wire
//	             (the injected end_turn keeps the request first-party — pinned
//	             at the server layer in conformance tests), identity restore.
//
// Sources per row: the client's own tool registry in reference/<repo>, file
// cited in the row comment. Routers (9router, OmniRoute) own no tools — they
// forward whichever client toolset entered them, so coverage for chained
// traffic is inherited from the rows below.

type toolClass string

const (
	classOfficial toolClass = "official"
	classMapped   toolClass = "mapped"
	classPassthru toolClass = "passthrough"
)

type toolRow struct {
	harness string
	tool    string
	class   toolClass
	target  string // official name for classMapped; empty otherwise
}

func TestComprehensiveToolClassification(t *testing.T) {
	rows := []toolRow{
		// ── pi core coding-agent (reference/harnesses/pi
		//    packages/coding-agent/src/core/tools/{bash,edit,edit-diff,find,
		//    grep,ls,powershell,read,write}.ts) ──
		{"Pi", "read", classMapped, "read_files"},
		{"Pi", "bash", classMapped, "run_terminal_command"},
		{"Pi", "edit", classMapped, "str_replace"},
		{"Pi", "write", classMapped, "write_file"},
		{"Pi", "grep", classMapped, "code_search"},
		{"Pi", "ls", classMapped, "list_directory"},
		{"Pi", "find", classMapped, "find_files"},
		{"Pi", "powershell", classMapped, "run_terminal_command"},
		{"Pi", "edit-diff", classMapped, "apply_patch"},

		// ── Oh My Pi (reference/harnesses/oh-my-pi
		//    packages/coding-agent/src/tools/builtin-names.ts) ──
		{"OMP", "todo", classMapped, "write_todos"},
		{"OMP", "glob", classOfficial, ""},
		{"OMP", "web_search", classOfficial, ""},
		{"OMP", "ask", classPassthru, ""},
		{"OMP", "task", classPassthru, ""},
		{"OMP", "hub", classPassthru, ""},
		{"OMP", "eval", classPassthru, ""},
		{"OMP", "lsp", classPassthru, ""},
		{"OMP", "browser", classPassthru, ""},
		{"OMP", "computer", classPassthru, ""},
		{"OMP", "github", classPassthru, ""},
		{"OMP", "ast_grep", classPassthru, ""},
		{"OMP", "ast_edit", classPassthru, ""},
		{"OMP", "checkpoint", classPassthru, ""},
		{"OMP", "rewind", classPassthru, ""},
		{"OMP", "security_scan", classPassthru, ""},
		{"OMP", "memory_edit", classPassthru, ""},
		{"OMP", "retain", classPassthru, ""},
		{"OMP", "recall", classPassthru, ""},
		{"OMP", "reflect", classPassthru, ""},
		{"OMP", "learn", classPassthru, ""},
		{"OMP", "manage_skill", classPassthru, ""},
		{"OMP", "debug", classPassthru, ""},
		{"OMP", "inspect_image", classPassthru, ""},

		// ── Claude Code (reference/agents/claude-code, PascalCase wire names;
		//    mapping is case-insensitive, restore returns exact casing) ──
		{"Claude-Code", "Bash", classMapped, "run_terminal_command"},
		{"Claude-Code", "Read", classMapped, "read_files"},
		{"Claude-Code", "Edit", classMapped, "str_replace"},
		{"Claude-Code", "Write", classMapped, "write_file"},
		{"Claude-Code", "Grep", classMapped, "code_search"},
		{"Claude-Code", "LS", classMapped, "list_directory"},
		{"Claude-Code", "TodoWrite", classMapped, "write_todos"},
		{"Claude-Code", "WebFetch", classMapped, "read_url"},
		{"Claude-Code", "WebSearch", classMapped, "web_search"},
		{"Claude-Code", "Glob", classOfficial, ""},
		{"Claude-Code", "Skill", classOfficial, ""},
		{"Claude-Code", "KillShell", classPassthru, ""},
		{"Claude-Code", "BashOutput", classPassthru, ""},
		{"Claude-Code", "NotebookRead", classPassthru, ""},
		{"Claude-Code", "NotebookEdit", classPassthru, ""},
		{"Claude-Code", "Task", classPassthru, ""},
		{"Claude-Code", "AskUserQuestion", classPassthru, ""},

		// ── kimi-cli (reference/agents/kimi-cli src/kimi_cli/tools/*) ──
		{"Kimi-CLI", "Shell", classMapped, "run_terminal_command"},
		{"Kimi-CLI", "ReadFile", classMapped, "read_files"},
		{"Kimi-CLI", "Grep", classMapped, "code_search"},
		{"Kimi-CLI", "SearchWeb", classMapped, "web_search"},
		{"Kimi-CLI", "Glob", classOfficial, ""},
		{"Kimi-CLI", "WriteFile", classMapped, "write_file"},
		{"Kimi-CLI", "StrReplaceFile", classPassthru, ""},
		{"Kimi-CLI", "ReadMediaFile", classPassthru, ""},
		{"Kimi-CLI", "Think", classPassthru, ""},
		{"Kimi-CLI", "SetTodoList", classMapped, "write_todos"},
		{"Kimi-CLI", "FetchURL", classMapped, "read_url"},
		{"Kimi-CLI", "TaskList", classPassthru, ""},
		{"Kimi-CLI", "TaskOutput", classPassthru, ""},
		{"Kimi-CLI", "TaskStop", classPassthru, ""},
		{"Kimi-CLI", "Agent", classPassthru, ""},
		{"Kimi-CLI", "EnterPlanMode", classPassthru, ""},

		// ── crush (reference/agents/crush internal/agent/tools/*.go) ──
		{"Crush", "bash", classMapped, "run_terminal_command"},
		{"Crush", "view", classMapped, "read_files"},
		{"Crush", "edit", classMapped, "str_replace"},
		{"Crush", "write", classMapped, "write_file"},
		{"Crush", "grep", classMapped, "code_search"},
		{"Crush", "ls", classMapped, "list_directory"},
		{"Crush", "web_fetch", classMapped, "read_url"},
		{"Crush", "web_search", classOfficial, ""},
		{"Crush", "glob", classOfficial, ""},
		{"Crush", "rg", classMapped, "code_search"},
		{"Crush", "todos", classMapped, "write_todos"},
		{"Crush", "multiedit", classPassthru, ""},
		{"Crush", "fetch", classPassthru, ""},
		{"Crush", "download", classPassthru, ""},
		{"Crush", "sourcegraph", classPassthru, ""},
		{"Crush", "question", classPassthru, ""},
		{"Crush", "lsp_definition", classPassthru, ""},
		{"Crush", "job_output", classPassthru, ""},
		{"Crush", "job_kill", classPassthru, ""},
		{"Crush", "read_mcp_resource", classPassthru, ""},
		{"Crush", "crush_logs", classPassthru, ""},
		{"Crush", "safe", classPassthru, ""},

		// ── OpenHands agent-server (reference/harnesses/OpenHands) ──
		{"OpenHands", "terminal", classPassthru, ""},
		{"OpenHands", "file_editor", classPassthru, ""},
		{"OpenHands", "task_tracker", classPassthru, ""},
		{"OpenHands", "browser_navigate", classPassthru, ""},
		{"OpenHands", "canvas_ui", classPassthru, ""},
		{"OpenHands", "finish", classPassthru, ""},

		// ── SWE-agent (reference/harnesses/SWE-agent sweagent/tools) ──
		{"SWE-agent", "bash", classMapped, "run_terminal_command"},
		{"SWE-agent", "str_replace_editor", classPassthru, ""},
		{"SWE-agent", "submit", classPassthru, ""},
		{"SWE-agent", "open", classPassthru, ""},
		{"SWE-agent", "create", classPassthru, ""},
		{"SWE-agent", "goto", classPassthru, ""},
		{"SWE-agent", "scroll_up", classPassthru, ""},
		{"SWE-agent", "scroll_down", classPassthru, ""},
		{"SWE-agent", "find_file", classPassthru, ""},
		{"SWE-agent", "search_dir", classPassthru, ""},
		{"SWE-agent", "search_file", classPassthru, ""},
		{"SWE-agent", "insert", classPassthru, ""},
		{"SWE-agent", "exit_forfeit", classPassthru, ""},

		// ── plandex server (reference/harnesses/plandex): forced
		//    structured-output tools MUST stay unmapped (server-side consume) ──
		{"Plandex", "namePlan", classPassthru, ""},
		{"Plandex", "namePipedData", classPassthru, ""},
		{"Plandex", "nameNote", classPassthru, ""},
		{"Plandex", "describePlan", classPassthru, ""},
		{"Plandex", "didFinishSubtask", classPassthru, ""},

		// ── DeepSeek-Reasonix (reference/harnesses/DeepSeek-Reasonix) ──
		{"Reasonix", "read_file", classMapped, "read_files"},
		{"Reasonix", "edit_file", classMapped, "str_replace"},
		{"Reasonix", "bash", classMapped, "run_terminal_command"},
		{"Reasonix", "grep", classMapped, "code_search"},
		{"Reasonix", "ls", classMapped, "list_directory"},
		{"Reasonix", "todo_write", classMapped, "write_todos"},
		{"Reasonix", "web_fetch", classMapped, "read_url"},
		{"Reasonix", "glob", classOfficial, ""},
		{"Reasonix", "multi_edit", classPassthru, ""},
		{"Reasonix", "fleet", classPassthru, ""},
		{"Reasonix", "run_skill", classPassthru, ""},
		{"Reasonix", "explore", classPassthru, ""},

		// ── jcode (reference/harnesses/jcode crates/jcode-app-core) ──
		{"Jcode", "read", classMapped, "read_files"},
		{"Jcode", "write", classMapped, "write_file"},
		{"Jcode", "edit", classMapped, "str_replace"},
		{"Jcode", "bash", classMapped, "run_terminal_command"},
		{"Jcode", "ls", classMapped, "list_directory"},
		{"Jcode", "todo", classMapped, "write_todos"},
		{"Jcode", "webfetch", classMapped, "read_url"},
		{"Jcode", "websearch", classMapped, "web_search"},
		{"Jcode", "apply_patch", classOfficial, ""},
		{"Jcode", "agentgrep", classPassthru, ""},
		{"Jcode", "multiedit", classPassthru, ""},
		{"Jcode", "patch", classPassthru, ""},
		{"Jcode", "browser", classPassthru, ""},
		{"Jcode", "memory", classPassthru, ""},
		{"Jcode", "initiative", classPassthru, ""},
		{"Jcode", "swarm", classPassthru, ""},
		{"Jcode", "skill_manage", classPassthru, ""},
		{"Jcode", "session_search", classPassthru, ""},

		// ── Codewhale (reference/harnesses/Codewhale crates/tui/src/tools) ──
		{"Codewhale", "read", classMapped, "read_files"},
		{"Codewhale", "write", classMapped, "write_file"},
		{"Codewhale", "edit", classMapped, "str_replace"},
		{"Codewhale", "bash", classMapped, "run_terminal_command"},
		{"Codewhale", "list_dir", classMapped, "list_directory"},
		{"Codewhale", "grep_files", classPassthru, ""},
		{"Codewhale", "file_search", classPassthru, ""},

		// ── Original corpus rows kept for classification continuity ──
		{"Cline", "read_file", classMapped, "read_files"},
		{"Roo-Code", "apply_diff", classMapped, "apply_patch"},
		{"Goose", "developer__shell", classMapped, "run_terminal_command"},
		{"Continue", "readFile", classMapped, "read_files"},
		{"Qwen-Code", "run_shell_command", classMapped, "run_terminal_command"},
		{"Kilocode", "execute_bash", classMapped, "run_terminal_command"},
		{"Aider", "replace_lines", classMapped, "str_replace"},
		{"Gemini-CLI", "read_many_files", classMapped, "read_files"},
	}

	for _, rc := range rows {
		t.Run(rc.harness+"/"+rc.tool, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{
				"model":    "m",
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
				"tools": []any{
					map[string]any{
						"type": "function",
						"function": map[string]any{
							"name":       rc.tool,
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
			got := fn["name"].(string)

			switch rc.class {
			case classMapped:
				if rc.target == "" {
					t.Fatal("mapped row missing target")
				}
				if got != rc.target {
					t.Fatalf("normalized tool name = %q, want mapped target %q", got, rc.target)
				}
				// Downstream restore must return the EXACT client name/casing.
				if restored := mapper.RestoreName(got); restored != rc.tool {
					t.Errorf("RestoreName(%q) = %q, want exact client name %q", got, restored, rc.tool)
				}
			case classOfficial, classPassthru:
				if got != rc.tool {
					t.Fatalf("normalized tool name = %q, want %q unchanged (class %s)", got, rc.tool, rc.class)
				}
				if restored := mapper.RestoreName(got); restored != rc.tool {
					t.Errorf("RestoreName(%q) = %q, want identity %q", got, restored, rc.tool)
				}
			}
		})
	}
}
