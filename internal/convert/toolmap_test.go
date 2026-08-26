package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestToolMapperRequestRename pins the request-side rename (#140 P2a): mapped
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
