package convert

import (
	"encoding/json"
	"strings"
)

// Tool-name tolerance mapping (issue #140 P2a).
//
// Upstream's free-mode gate classifies a request offering tools with NO
// signature tool as third-party (foreign_toolset → downgrade to
// ling-3.0-tiny:free, reference/freebuff common/src/constants/
// foreign-client-signals.ts) and the trust system permanently caps any
// account seen sending a foreign tool schema (third_party_client sticky cap).
// The proxy neutralizes both by renaming common third-party harness tool
// names to the official signature equivalents for the upstream wire and
// renaming them BACK on every response path — the client only ever sees its
// own names.
//
// Design constraint that keeps this safe: the client's PARAMETER SCHEMA is
// forwarded untouched (after structural normalization). The model fills
// arguments per the schema it was shown, so arguments come back in client
// shape and only the NAME needs restoring downstream. No argument translation,
// no schema substitution, no invented structure.

// clientToOfficial maps well-known third-party harness tool names to the
// official codebuff signature tool they behave as. Keys are lowercase.
// Only names whose behavior genuinely matches are mapped; everything else
// passes through untouched (an unknown rename would break the client's
// dispatcher). Derived from reference/freebuff common/src/tools/params/tool/*
// schemas:
//
//	read_files{paths[]}   write_file{path,instructions,content}
//	run_terminal_command{command,...}   glob{pattern,...}
//	write_todos{todos[{task,completed}]}
var clientToOfficial = map[string]string{
	// Claude Code / generic agentic CLIs
	"read":      "read_files",
	"view":      "read_files",
	"edit":      "str_replace",
	"write":     "write_file",
	"bash":      "run_terminal_command",
	"execute":   "run_terminal_command",
	"ls":        "list_directory",
	"grep":      "code_search",
	"todo":      "write_todos",
	"todowrite": "write_todos",

	// Cline / Roo Code
	"read_file":       "read_files",
	"write_to_file":   "write_file",
	"replace_in_file": "str_replace",
	"execute_command": "run_terminal_command",
	"list_files":      "list_directory",
	"search_files":    "code_search",

	// Codex / OpenAI harnesses
	"shell":          "run_terminal_command",
	"local_shell":    "run_terminal_command",
	"container_exec": "run_terminal_command",

	// Aider / misc
	"command": "run_terminal_command",
}

// officialTools is the set of official codebuff signature tool names a
// mapping target must be in. Guards against typos introducing a name
// upstream does not recognize (which would itself be foreign).
var officialTools = map[string]bool{
	"apply_patch":          true,
	"code_search":          true,
	"end_turn":             true,
	"glob":                 true,
	"list_directory":       true,
	"read_files":           true,
	"read_subtree":         true,
	"run_terminal_command": true,
	"skill":                true,
	"str_replace":          true,
	"web_search":           true,
	"write_file":           true,
	"write_todos":          true,
	"find_files":           true,
	"read_url":             true,
}

func init() {
	for _, official := range clientToOfficial {
		if official == "" {
			continue
		}
		if !officialTools[official] {
			panic("convert: clientToOfficial maps to non-official tool " + official)
		}
	}
}

// ToolMapper is one request's bidirectional tool-name map. Compute it from
// the request body BEFORE normalization (ToUpstream rewrites the request's
// tools array), then thread it to the response relays (FromUpstream restores
// client names). Zero value is valid: nothing maps, relays pass through.
type ToolMapper struct {
	upstreamToClient map[string]string // response path: official → original
}

// NewToolMapper scans a raw request body for function-tool names and returns
// the mapper for it. Names that are already official (or unknown) produce no
// entry — they round-trip unchanged. body may be nil/invalid (returns empty).
func NewToolMapper(body []byte) ToolMapper {
	var payload struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ToolMapper{}
	}
	m := ToolMapper{upstreamToClient: make(map[string]string)}
	for _, t := range payload.Tools {
		name := t.Function.Name
		if name == "" {
			continue
		}
		if official, ok := clientToOfficial[strings.ToLower(name)]; ok && official != "" && official != name {
			m.upstreamToClient[official] = name
		}
	}
	return m
}

// ToUpstream renames mapped client tool entries in the request payload IN
// PLACE (payload["tools"]) so the upstream wire carries official signature
// names. Idempotent.
func (m ToolMapper) ToUpstream(payload map[string]any) {
	tools, ok := payload["tools"].([]any)
	if !ok {
		return
	}
	for _, t := range tools {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if official, hit := clientToOfficial[strings.ToLower(name)]; hit && official != "" {
			fn["name"] = official
			// Preserve the original where the model can see it: some models
			// echo the description when choosing between similar tools.
			if desc, _ := fn["description"].(string); desc != "" && !strings.Contains(desc, "(client tool: "+name+")") {
				fn["description"] = strings.TrimSpace(desc) + " (client tool: " + name + ")"
			}
		}
	}
}

// RenameRequestToolChoice points tool_choice at the renamed official tool
// when the client pinned a mapped name. Handles both string form ("auto")
// and structured form {"type":"function","function":{"name":...}}.
func (m ToolMapper) RenameRequestToolChoice(payload map[string]any) {
	tc, ok := payload["tool_choice"]
	if !ok || tc == nil {
		return
	}
	switch v := tc.(type) {
	case string:
		if official, hit := clientToOfficial[strings.ToLower(v)]; hit && official != "" {
			payload["tool_choice"] = official
		}
	case map[string]any:
		if fn, ok := v["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				if official, hit := clientToOfficial[strings.ToLower(name)]; hit && official != "" {
					fn["name"] = official
				}
			}
		}
	}
}

// FromUpstreamChunk restores client tool names in one upstream SSE chunk or
// completion object, IN PLACE: walks choices[].delta.tool_calls[] and
// choices[].message.tool_calls[], rewriting function.name. Returns the chunk
// unchanged (same semantics as callers' other in-place mutators). raw is the
// already-marshaled JSON; callers re-marshal only when this returns true.
func (m ToolMapper) FromUpstreamChunk(chunk map[string]any) bool {
	changed := false
	restore := func(fn map[string]any) {
		if fn == nil {
			return
		}
		name, _ := fn["name"].(string)
		if name == "" {
			return
		}
		if orig, ok := m.upstreamToClient[name]; ok {
			fn["name"] = orig
			changed = true
		}
	}
	choices, ok := chunk["choices"].([]any)
	if !ok {
		return false
	}
	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if delta, ok := choice["delta"].(map[string]any); ok {
			if tcs, ok := delta["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					tcMap, _ := tc.(map[string]any)
					if tcMap == nil {
						continue
					}
					fn, _ := tcMap["function"].(map[string]any)
					restore(fn)
				}
			}
		}
		if msg, ok := choice["message"].(map[string]any); ok {
			if tcs, ok := msg["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					tcMap, _ := tc.(map[string]any)
					if tcMap == nil {
						continue
					}
					fn, _ := tcMap["function"].(map[string]any)
					restore(fn)
				}
			}
		}
	}
	return changed
}

// RestoreName maps one upstream tool name back to the client's original
// (identity when unmapped). Used by the Anthropic stream state machine,
// which tracks names outside the chunk-JSON shapes above.
func (m ToolMapper) RestoreName(name string) string {
	if orig, ok := m.upstreamToClient[name]; ok {
		return orig
	}
	return name
}

// Len reports how many names the mapper restores. Zero = identity mapper;
// relays use it to skip their restore pass entirely.
func (m ToolMapper) Len() int { return len(m.upstreamToClient) }
