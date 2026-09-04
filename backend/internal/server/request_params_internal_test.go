package server

// Request-side feature-translation parity tests (internal): pure mapping
// unit tests for the Anthropic request converter. HTTP-level parity tests
// live in request_params_test.go (package server_test).

import (
	"encoding/json"
	"testing"
)

func TestAnthropicToChatParams_MaxTokensDefault(t *testing.T) {
	raw := map[string]any{
		"model":    "anthropic/claude-sonnet-4",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	out, err := anthropicToChatParams(raw)
	if err != nil {
		t.Fatalf("anthropicToChatParams failed: %v", err)
	}
	var chat map[string]any
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, _ := chat["max_tokens"].(float64); got != anthropicDefaultMaxTokens {
		t.Errorf("max_tokens = %v, want default %d", chat["max_tokens"], anthropicDefaultMaxTokens)
	}

	// A client-supplied max_tokens wins over the default.
	rawWith := map[string]any{
		"model":      "anthropic/claude-sonnet-4",
		"max_tokens": float64(2048),
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
	}
	out2, err := anthropicToChatParams(rawWith)
	if err != nil {
		t.Fatalf("anthropicToChatParams failed: %v", err)
	}
	var chat2 map[string]any
	if err := json.Unmarshal(out2, &chat2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, _ := chat2["max_tokens"].(float64); got != 2048 {
		t.Errorf("max_tokens = %v, want client 2048", chat2["max_tokens"])
	}
}

// TestAnthropicToChatParams_FullMapping pins the complete request-side
// mapping table: mapped params land in the chat envelope, unmappable ones
// (top_k, top-level cache_control, container, inference_geo, output_config,
// service_tier) are documented-ignored and never forwarded.
func TestAnthropicToChatParams_FullMapping(t *testing.T) {
	raw := map[string]any{
		"model":          "anthropic/claude-sonnet-4",
		"max_tokens":     float64(1024),
		"temperature":    float64(0.3),
		"top_p":          float64(0.9),
		"top_k":          float64(40),
		"stop_sequences": []any{"END"},
		"stream":         true,
		"system":         []any{map[string]any{"type": "text", "text": "be brief"}},
		"metadata":       map[string]any{"user_id": "user-7"},
		"thinking":       map[string]any{"type": "enabled", "budget_tokens": float64(2048)},
		"cache_control":  map[string]any{"type": "ephemeral"},
		"container":      "ctr-1",
		"inference_geo":  "us",
		"output_config":  map[string]any{"format": "text"},
		"service_tier":   "standard_only",
		"tool_choice":    map[string]any{"type": "any", "disable_parallel_tool_use": true},
		"tools": []any{
			map[string]any{
				"name":         "get_weather",
				"description":  "weather",
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
			},
		},
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "hello"},
					map[string]any{"type": "tool_result", "tool_use_id": "toolu_abc", "content": "68F"},
				},
			},
		},
	}
	out, err := anthropicToChatParams(raw)
	if err != nil {
		t.Fatalf("anthropicToChatParams failed: %v", err)
	}
	var chat map[string]any
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Mapped.
	if chat["max_tokens"].(float64) != 1024 {
		t.Errorf("max_tokens = %v, want 1024", chat["max_tokens"])
	}
	if chat["temperature"].(float64) != 0.3 || chat["top_p"].(float64) != 0.9 {
		t.Errorf("temperature/top_p = %v/%v, want 0.3/0.9", chat["temperature"], chat["top_p"])
	}
	if got := chat["stop"]; got != "END" {
		t.Errorf("stop = %v, want END", got)
	}
	if chat["user"] != "user-7" {
		t.Errorf("user = %v, want user-7 (metadata.user_id)", chat["user"])
	}
	if chat["reasoning_effort"] != "medium" {
		t.Errorf("reasoning_effort = %v, want medium (2048 budget)", chat["reasoning_effort"])
	}
	if chat["parallel_tool_calls"] != false {
		t.Errorf("parallel_tool_calls = %v, want false", chat["parallel_tool_calls"])
	}
	if chat["tool_choice"] != "required" {
		t.Errorf("tool_choice = %v, want required (any)", chat["tool_choice"])
	}
	tools, _ := chat["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want one", chat["tools"])
	}
	if fn := tools[0].(map[string]any)["function"].(map[string]any); fn["name"] != "get_weather" {
		t.Errorf("tool function name = %v, want get_weather", fn["name"])
	} else if _, hasParams := fn["parameters"]; !hasParams {
		t.Error("tool function missing parameters (input_schema)")
	}
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (system + tool_result + user text)", len(msgs))
	}
	// System first, then the tool result, then the user text turn.
	if sys := msgs[0].(map[string]any); sys["role"] != "system" {
		t.Errorf("first message = %v, want system", sys)
	}
	if tr := msgs[1].(map[string]any); tr["role"] != "tool" || tr["tool_call_id"] != "toolu_abc" {
		t.Errorf("second message = %v, want tool result", tr)
	}
	if usr := msgs[2].(map[string]any); usr["role"] != "user" {
		t.Errorf("third message = %v, want user text", usr)
	}
	// Unmappable: never forwarded.
	for _, k := range []string{"top_k", "cache_control", "container", "inference_geo", "output_config", "service_tier"} {
		if _, ok := chat[k]; ok {
			t.Errorf("unmappable param %q leaked into chat envelope (want documented-ignored)", k)
		}
	}
}

// TestAnthropicToChatParams_SystemAndToolBounds pins system normalization
// (string and array-of-text shapes) and tool cache_control passthrough
// (harmless extra key on the schema, kept — no upstream cache marker exists).
func TestAnthropicToChatParams_SystemNormalization(t *testing.T) {
	for name, system := range map[string]any{
		"string":  "be brief",
		"array":   []any{map[string]any{"type": "text", "text": "be brief"}, map[string]any{"type": "text", "text": "and focused"}},
		"empty":   "",
		"nil":     nil,
		"notText": []any{map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://x/y.png"}}},
	} {
		t.Run(name, func(t *testing.T) {
			raw := map[string]any{"model": "anthropic/claude-sonnet-4", "system": system,
				"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
			out, err := anthropicToChatParams(raw)
			if err != nil {
				t.Fatalf("anthropicToChatParams failed: %v", err)
			}
			var chat map[string]any
			if err := json.Unmarshal(out, &chat); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			msgs, _ := chat["messages"].([]any)
			first := msgs[0].(map[string]any)
			if name == "empty" || name == "nil" || name == "notText" {
				if first["role"] == "system" {
					t.Errorf("system message emitted for %s system: %v", name, first)
				}
			} else if first["role"] != "system" {
				t.Errorf("system message missing for %s system: %v", name, first)
			}
		})
	}
}

// TestResponsesEcho mirrors OpenAI's echo semantics on the response object
// skeleton: request params (store/tools/tool_choice/...) are reflected,
// OpenAI defaults apply when absent. Regression: the skeleton hardcoded
// store:true/tools:[] regardless of the request (live S13 showed
// store:true on a store:false request).
func TestResponsesEcho(t *testing.T) {
	// Nil request → OpenAI defaults.
	d := responsesEcho(nil)
	if d["store"] != true {
		t.Errorf("default store = %v, want true", d["store"])
	}
	if tools, _ := d["tools"].([]any); len(tools) != 0 {
		t.Errorf("default tools = %v, want []", d["tools"])
	}
	if d["tool_choice"] != "auto" {
		t.Errorf("default tool_choice = %v, want auto", d["tool_choice"])
	}

	// Client params are echoed verbatim.
	tools := []any{map[string]any{"type": "function", "name": "get_weather"}}
	raw := map[string]any{
		"store":               false,
		"tools":               tools,
		"tool_choice":         "required",
		"parallel_tool_calls": false,
		"temperature":         0.2,
		"instructions":        "Be concise.",
		"max_output_tokens":   float64(300),
		"reasoning":           map[string]any{"effort": "low", "summary": "auto"},
	}
	e := responsesEcho(raw)
	if e["store"] != false {
		t.Errorf("store = %v, want false", e["store"])
	}
	if e["tool_choice"] != "required" {
		t.Errorf("tool_choice = %v, want required", e["tool_choice"])
	}
	if e["parallel_tool_calls"] != false {
		t.Errorf("parallel_tool_calls = %v, want false", e["parallel_tool_calls"])
	}
	if e["instructions"] != "Be concise." {
		t.Errorf("instructions = %v, want echo", e["instructions"])
	}
	if e["max_output_tokens"] != float64(300) {
		t.Errorf("max_output_tokens = %v, want 300", e["max_output_tokens"])
	}

	// The skeleton carries the echo (created/in_progress/completed share it).
	base := responsesBase("m", "resp_x", 1, "in_progress", e)
	for k, want := range map[string]any{"store": false, "tool_choice": "required", "instructions": "Be concise."} {
		if base[k] != want {
			t.Errorf("skeleton[%s] = %v, want %v", k, base[k], want)
		}
	}
	if len(base["tools"].([]any)) != 1 {
		t.Errorf("skeleton tools = %v, want 1 echoed tool", base["tools"])
	}

	// Nil echo → defaults (test-driven relays pass &relayStats{}).
	def := responsesBase("m", "resp_y", 1, "completed", nil)
	if def["store"] != true || def["tool_choice"] != "auto" {
		t.Errorf("nil-echo skeleton = store %v choice %v, want true/auto", def["store"], def["tool_choice"])
	}
}

// TestAnthropicOutputConfigEffort pins the Claude 4.6+ effort path:
// thinking adaptive carries no budget, so output_config.effort must win
// over the thinking-derived default (else every adaptive turn inflates
// to high). output_format json_schema maps to response_format.
func TestAnthropicOutputConfigEffort(t *testing.T) {
	raw := map[string]any{
		"model":         "anthropic/claude-opus-4-6",
		"max_tokens":    float64(1024),
		"thinking":      map[string]any{"type": "adaptive"},
		"output_config": map[string]any{"effort": "low"},
		"output_format": map[string]any{
			"type":   "json_schema",
			"schema": map[string]any{"type": "object"},
		},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	out, err := anthropicToChatParams(raw)
	if err != nil {
		t.Fatalf("anthropicToChatParams failed: %v", err)
	}
	var chat map[string]any
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if chat["reasoning_effort"] != "low" {
		t.Errorf("reasoning_effort = %v, want low (output_config wins over adaptive default high)", chat["reasoning_effort"])
	}
	rf, _ := chat["response_format"].(map[string]any)
	if rf["type"] != "json_schema" {
		t.Errorf("response_format = %v, want json_schema mapping", chat["response_format"])
	}

	// Without output_config, adaptive still defaults to high and no
	// response_format appears.
	raw2 := map[string]any{
		"model":      "anthropic/claude-opus-4-6",
		"max_tokens": float64(1024),
		"thinking":   map[string]any{"type": "adaptive"},
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
	}
	out2, err := anthropicToChatParams(raw2)
	if err != nil {
		t.Fatalf("anthropicToChatParams failed: %v", err)
	}
	var chat2 map[string]any
	if err := json.Unmarshal(out2, &chat2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if chat2["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v, want high (adaptive default)", chat2["reasoning_effort"])
	}
	if _, ok := chat2["response_format"]; ok {
		t.Errorf("response_format = %v, want absent without output_format", chat2["response_format"])
	}
}
