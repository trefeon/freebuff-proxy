package convert

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeRequestWhitelist(t *testing.T) {
	body := map[string]any{
		"model":                 "deepseek-v3",
		"messages":              []any{},
		"frequency_penalty":     0.5,
		"logit_bias":            map[string]any{"123": 1.0},
		"logprobs":              true,
		"max_completion_tokens": 100.0,
		"max_tokens":            200.0,
		"metadata":              map[string]any{"k": "v"},
		"modalities":            []any{"text"},
		"parallel_tool_calls":   true,
		"presence_penalty":      -0.2,
		"reasoning_effort":      "high",
		"response_format":       map[string]any{"type": "json_object"},
		"seed":                  42.0,
		"service_tier":          "auto",
		"stop":                  []any{"END"},
		"store":                 false,
		"stream_options":        map[string]any{"include_usage": true},
		"temperature":           0.7,
		"tool_choice":           "auto",
		"tools":                 []any{},
		"top_logprobs":          5.0,
		"top_p":                 0.9,
		"user":                  "u-1",
		// not whitelisted — must be dropped
		"stream":        true,
		"n":             2.0,
		"function_call": "auto",
		"bogus":         "x",
	}
	out, err := NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got := decode(t, out)
	for _, key := range []string{
		"model", "messages", "frequency_penalty", "logit_bias", "max_completion_tokens",
		"max_tokens", "metadata", "modalities", "parallel_tool_calls", "presence_penalty",
		"reasoning_effort", "response_format", "seed", "service_tier", "stop", "store",
		"stream_options", "temperature", "tool_choice", "tools", "top_logprobs", "top_p", "user",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("whitelisted key %q dropped", key)
		}
	}
	for _, key := range []string{"stream", "n", "function_call", "bogus"} {
		if _, ok := got[key]; ok {
			t.Errorf("non-whitelisted key %q kept", key)
		}
	}

	// Null-valued whitelisted keys are dropped (meaningless upstream).
	out, err = NormalizeRequest([]byte(`{"model":"m","messages":[],"temperature":null}`), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got = decode(t, out)
	if _, ok := got["temperature"]; ok {
		t.Error("null-valued whitelisted key kept")
	}
}

func TestNormalizeRequestDeveloperToSystem(t *testing.T) {
	body := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "developer", "content": "be brief"},
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "ok"},
			map[string]any{"role": "system", "content": "sys"},
		},
	}
	out, err := NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got := decode(t, out)
	msgs := got["messages"].([]any)
	for i, role := range []string{"system", "user", "assistant", "system"} {
		if got := msgs[i].(map[string]any)["role"]; got != role {
			t.Errorf("message %d role = %v, want %q", i, got, role)
		}
	}
}

func TestNormalizeRequestInvalidBody(t *testing.T) {
	for name, body := range map[string]string{
		"not json":   "{oops",
		"not object": `"hello"`,
		"null":       `null`,
		"empty":      ``,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NormalizeRequest([]byte(body), ""); err == nil {
				t.Fatalf("expected error for body %q", body)
			}
		})
	}
}

func TestExtractReasoningEffort(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{
			name:    "direct reasoning_effort",
			payload: map[string]any{"reasoning_effort": "max"},
			want:    "max",
		},
		{
			name:    "nested reasoning.effort",
			payload: map[string]any{"reasoning": map[string]any{"effort": "high"}},
			want:    "high",
		},
		{
			name:    "nested thinking.type",
			payload: map[string]any{"thinking": map[string]any{"type": "enabled"}},
			want:    "enabled",
		},
		{
			name:    "empty payload",
			payload: nil,
			want:    "",
		},
		{
			name:    "no effort fields",
			payload: map[string]any{"model": "deepseek-v4-flash"},
			want:    "",
		},
		{
			name:    "model suffix parenthesized max",
			payload: map[string]any{"model": "deepseek/deepseek-v4-pro(max)"},
			want:    "max",
		},
		{
			name:    "model suffix parenthesized high",
			payload: map[string]any{"model": "deepseek/deepseek-v4-pro(high)"},
			want:    "high",
		},
		{
			name:    "model suffix parenthesized medium",
			payload: map[string]any{"model": "deepseek/deepseek-v4-pro(medium)"},
			want:    "medium",
		},
		{
			name:    "model suffix parenthesized low",
			payload: map[string]any{"model": "deepseek/deepseek-v4-pro(low)"},
			want:    "low",
		},
		{
			name:    "model suffix parenthesized minimal",
			payload: map[string]any{"model": "meta/muse-spark(minimal)"},
			want:    "minimal",
		},
		{
			name:    "model suffix parenthesized ultra",
			payload: map[string]any{"model": "openai/gpt-5.6-luna(ultra)"},
			want:    "ultra",
		},
		{
			name:    "model suffix parenthesized xhigh uppercase",
			payload: map[string]any{"model": "openai/gpt-5.6-luna(XHIGH)"},
			want:    "xhigh",
		},
		{
			name:    "model suffix colon max",
			payload: map[string]any{"model": "deepseek/deepseek-v4-pro:max"},
			want:    "max",
		},
		{
			name:    "model suffix colon high",
			payload: map[string]any{"model": "deepseek/deepseek-v4-pro:high"},
			want:    "high",
		},
		{
			name:    "explicit reasoning_effort overrides model suffix",
			payload: map[string]any{"model": "deepseek/deepseek-v4-pro(max)", "reasoning_effort": "low"},
			want:    "low",
		},
		{
			name:    "explicit nested reasoning.effort overrides model suffix",
			payload: map[string]any{"model": "deepseek/deepseek-v4-pro:max", "reasoning": map[string]any{"effort": "medium"}},
			want:    "medium",
		},
		{
			name:    "ox-alpha model suffix parenthesized medium",
			payload: map[string]any{"model": "stealth/ox-alpha(medium)"},
			want:    "medium",
		},
		{
			name:    "ox-alpha model suffix colon max",
			payload: map[string]any{"model": "stealth/ox-alpha:max"},
			want:    "max",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractReasoningEffort(tc.payload); got != tc.want {
				t.Errorf("ExtractReasoningEffort() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNormalizeRequestReasoningEffort(t *testing.T) {
	// DeepSeek models (#111): nested reasoning.effort "max" is clamped (#65)
	// and sent as PLAIN reasoning_effort — no client-side thinking block; the
	// DeepSeek thinking translation is server-side.
	body := map[string]any{
		"model":     "deepseek/deepseek-v4-flash",
		"messages":  []any{map[string]any{"role": "user", "content": "hello"}},
		"reasoning": map[string]any{"effort": "max"},
	}
	out, err := NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got := decode(t, out)
	if gotEff, ok := got["reasoning_effort"].(string); !ok || gotEff != "max" {
		t.Errorf("reasoning_effort = %v, want \"max\"", got["reasoning_effort"])
	}
	if _, ok := got["thinking"]; ok {
		t.Error("thinking block emitted for deepseek model")
	}

	// Non-DeepSeek models keep reasoning_effort (clamped to the model's
	// allowance when the model has one).
	body = map[string]any{
		"model":            "openai/gpt-5.6-luna",
		"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
		"reasoning_effort": "max",
	}
	out, err = NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got = decode(t, out)
	if gotEff, ok := got["reasoning_effort"].(string); !ok || gotEff != "max" {
		t.Errorf("reasoning_effort = %v, want \"max\"", got["reasoning_effort"])
	}
	if _, ok := got["thinking"]; ok {
		t.Error("thinking block added for non-deepseek model")
	}

	// stealth/ox-alpha shares the DeepSeek V4 ladder shape ('low','high',
	// 'max', no medium): nested reasoning.effort is clamped and sent PLAIN,
	// never as a client-side thinking block.
	body = map[string]any{
		"model":     "stealth/ox-alpha",
		"messages":  []any{map[string]any{"role": "user", "content": "hello"}},
		"reasoning": map[string]any{"effort": "max"},
	}
	out, err = NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got = decode(t, out)
	if gotEff, ok := got["reasoning_effort"].(string); !ok || gotEff != "max" {
		t.Errorf("reasoning_effort = %v, want \"max\"", got["reasoning_effort"])
	}
	if _, ok := got["thinking"]; ok {
		t.Error("thinking block emitted for stealth/ox-alpha model")
	}
}

// ---------------------------------------------------------------------------
// Issue #84 — DeepSeek prompt-cache cache_control injection.
// ---------------------------------------------------------------------------
func TestInjectCacheControl(t *testing.T) {
	messages := []any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "user", "content": "u1"},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "u2"},
			map[string]any{"type": "text", "text": "u3", "cache_control": map[string]any{"type": "existing"}},
		}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "u4"}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "u5"}}},
		map[string]any{"role": "user", "content": "plain string"}, // non-block content skipped
	}
	InjectCacheControl(messages)

	blocks := func(i int) []any {
		return messages[i].(map[string]any)["content"].([]any)
	}
	// Indices 2 and 3 (the stable context prefix) get ephemeral hints.
	if cc := blocks(2)[0].(map[string]any)["cache_control"]; cc == nil {
		t.Error("messages[2] block 0 missing cache_control")
	}
	if cc := blocks(2)[1].(map[string]any)["cache_control"].(map[string]any); cc["type"] != "existing" {
		t.Errorf("messages[2] block 1 cache_control overwritten: %v", cc)
	}
	if cc := blocks(3)[0].(map[string]any)["cache_control"]; cc == nil {
		t.Error("messages[3] block 0 missing cache_control")
	}
	// Everything outside indices 2-3 is untouched.
	if _, ok := messages[0].(map[string]any)["cache_control"]; ok {
		t.Error("messages[0] gained cache_control")
	}
	if _, ok := messages[1].(map[string]any)["cache_control"]; ok {
		t.Error("messages[1] gained cache_control")
	}
	if cc := blocks(4)[0].(map[string]any)["cache_control"]; cc != nil {
		t.Error("messages[4] gained cache_control (beyond the prefix window)")
	}
	if got := messages[5].(map[string]any)["content"]; got != "plain string" {
		t.Errorf("non-block content touched: %v", got)
	}
}

func TestNormalizeRequestCacheControlInjection(t *testing.T) {
	mkBody := func(model string) map[string]any {
		content := func(text string) []any {
			return []any{map[string]any{"type": "text", "text": text}}
		}
		return map[string]any{
			"model": model,
			"messages": []any{
				map[string]any{"role": "system", "content": "sys"},
				map[string]any{"role": "user", "content": "u1"},
				map[string]any{"role": "user", "content": content("u2")},
				map[string]any{"role": "user", "content": content("u3")},
			},
		}
	}
	hasHints := func(out []byte) bool {
		got := decode(t, out)
		for i, m := range got["messages"].([]any) {
			if i < 2 || i > 3 {
				continue
			}
			blocks, _ := m.(map[string]any)["content"].([]any)
			for _, b := range blocks {
				if _, ok := b.(map[string]any)["cache_control"]; ok {
					return true
				}
			}
		}
		return false
	}

	t.Run("default on for deepseek", func(t *testing.T) {
		t.Setenv("CACHE_CONTROL_INJECTION", "")
		out, err := NormalizeRequest(mustJSON(t, mkBody("deepseek/deepseek-v4-flash")), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		if !hasHints(out) {
			t.Error("deepseek request without cache_control hints")
		}
	})

	t.Run("on for deepseek pro", func(t *testing.T) {
		t.Setenv("CACHE_CONTROL_INJECTION", "")
		out, err := NormalizeRequest(mustJSON(t, mkBody("deepseek/deepseek-v4-pro")), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		if !hasHints(out) {
			t.Error("deepseek pro request without cache_control hints")
		}
	})

	t.Run("disabled via env", func(t *testing.T) {
		t.Setenv("CACHE_CONTROL_INJECTION", "false")
		out, err := NormalizeRequest(mustJSON(t, mkBody("deepseek/deepseek-v4-flash")), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		if hasHints(out) {
			t.Error("cache_control injected with CACHE_CONTROL_INJECTION=false")
		}
	})

	t.Run("non-deepseek model untouched", func(t *testing.T) {
		t.Setenv("CACHE_CONTROL_INJECTION", "")
		out, err := NormalizeRequest(mustJSON(t, mkBody("minimax/minimax-m3")), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		if hasHints(out) {
			t.Error("cache_control injected for non-deepseek model")
		}
	})
}

// ---------------------------------------------------------------------------
// Issue #111 — plain reasoning_effort on DeepSeek routes (no thinking block).
// ---------------------------------------------------------------------------
func TestDeepSeekPlainReasoningEffort(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		effort string
		want   string
	}{
		{"flash low stays low", "deepseek/deepseek-v4-flash", "low", "low"},
		{"flash medium rewrites to high", "deepseek/deepseek-v4-flash", "medium", "high"},
		{"flash high stays high", "deepseek/deepseek-v4-flash", "high", "high"},
		{"flash max stays max", "deepseek/deepseek-v4-flash", "max", "max"},
		{"flash xhigh clamps down to high", "deepseek/deepseek-v4-flash", "xhigh", "high"},
		{"pro low stays low", "deepseek/deepseek-v4-pro", "low", "low"},
		{"ox-alpha low stays low", "stealth/ox-alpha", "low", "low"},
		{"ox-alpha medium rewrites to high", "stealth/ox-alpha", "medium", "high"},
		{"ox-alpha high stays high", "stealth/ox-alpha", "high", "high"},
		{"ox-alpha max stays max", "stealth/ox-alpha", "max", "max"},
		{"bare ox-alpha id tolerated", "ox-alpha", "medium", "high"},
		{"pro medium rewrites to high", "deepseek/deepseek-v4-pro", "medium", "high"},
		{"pro max stays max", "deepseek/deepseek-v4-pro", "max", "max"},
		{"bare model id tolerated", "deepseek-v4-flash", "max", "max"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{
				"model":            tc.model,
				"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
				"reasoning_effort": tc.effort,
			}
			out, err := NormalizeRequest(mustJSON(t, body), "")
			if err != nil {
				t.Fatalf("NormalizeRequest: %v", err)
			}
			got := decode(t, out)
			if gotEff, ok := got["reasoning_effort"].(string); !ok || gotEff != tc.want {
				t.Errorf("reasoning_effort = %v, want %q", got["reasoning_effort"], tc.want)
			}
			if _, ok := got["thinking"]; ok {
				t.Errorf("thinking block emitted for deepseek model: %v", got["thinking"])
			}
		})
	}

	t.Run("reasoning.enabled=false suppresses effort", func(t *testing.T) {
		body := map[string]any{
			"model":     "deepseek/deepseek-v4-flash",
			"messages":  []any{map[string]any{"role": "user", "content": "hi"}},
			"reasoning": map[string]any{"enabled": false},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		if _, ok := got["reasoning_effort"]; ok {
			t.Error("reasoning_effort kept alongside reasoning.enabled=false")
		}
		if _, ok := got["thinking"]; ok {
			t.Error("thinking block emitted alongside reasoning.enabled=false")
		}
	})

	t.Run("thinking.type=disabled suppresses effort", func(t *testing.T) {
		body := map[string]any{
			"model":            "deepseek/deepseek-v4-flash",
			"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
			"reasoning_effort": "max",
			"thinking":         map[string]any{"type": "disabled"},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		if _, ok := got["reasoning_effort"]; ok {
			t.Error("reasoning_effort kept alongside thinking.type=disabled")
		}
		if _, ok := got["thinking"]; ok {
			t.Error("thinking block emitted alongside thinking.type=disabled")
		}
	})

	t.Run("no effort requested adds no thinking", func(t *testing.T) {
		body := map[string]any{
			"model":    "deepseek/deepseek-v4-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		if _, ok := got["thinking"]; ok {
			t.Errorf("thinking block added without an effort request: %v", got["thinking"])
		}
	})
}

// ---------------------------------------------------------------------------
// Issue #65 — per-model reasoning effort clamping.
// ---------------------------------------------------------------------------
func TestClampReasoningEffort(t *testing.T) {
	cases := []struct {
		name      string
		requested string
		allowed   []string
		want      string
	}{
		{"max within allowance", "max", []string{"low", "high", "max"}, "max"},
		{"high within allowance", "high", []string{"low", "high", "max"}, "high"},
		{"medium clamps down to low", "medium", []string{"low", "high", "max"}, "low"},
		{"all above requested gives lowest", "low", []string{"high", "max"}, "high"},
		{"below everything gives lowest", "minimal", []string{"low", "high", "max"}, "low"},
		{"unknown effort falls back", "banana", []string{"low", "high", "max"}, defaultReasoningEffort},
		{"empty requested falls back", "", []string{"low"}, defaultReasoningEffort},
		{"nil allowance falls back", "high", nil, defaultReasoningEffort},
		{"empty allowance falls back", "high", []string{}, defaultReasoningEffort},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampReasoningEffort(tc.requested, tc.allowed, defaultReasoningEffort); got != tc.want {
				t.Errorf("clampReasoningEffort(%q, %v) = %q, want %q", tc.requested, tc.allowed, got, tc.want)
			}
		})
	}
}

func TestEffortsForModel(t *testing.T) {
	// Every id the ServedModels gate serves, with its upstream-verified
	// ladder (reference/freebuff/common/src/constants/freebuff-models.ts,
	// modelcat catalog, pinned 5951772).
	for model, want := range map[string][]string{
		"deepseek/deepseek-v4-flash": {"low", "high", "max"},
		"deepseek/deepseek-v4-pro":   {"low", "high", "max"},
		"stealth/ox-alpha":           {"low", "high", "max"},
		"mimo/mimo-v2.5":             {"high"}, // Xiaomi: disabled/high only
		"minimax/minimax-m3":         {"high"}, // adaptive/disabled only
		"anthropic/claude-fable-5":   {"low", "medium", "high", "xhigh", "max"},
		"openai/gpt-5.6-luna":        {"low", "medium", "high", "xhigh", "max"},
	} {
		if got := effortsForModel(model); !reflect.DeepEqual(got, want) {
			t.Errorf("effortsForModel(%s) = %v, want %v", model, got, want)
		}
	}

	// Ignore-routes (CrofAI accepts but ignores reasoning_effort, including
	// invalid values: GLM/Kimi) and helper models (gemini lite rows) get the
	// full ladder — nothing to clamp to.
	for _, model := range []string{
		"z-ai/glm-5.2",
		"crof/kimi-k3-eco",
		"meta/muse-spark-1.2-contributor", // god-only, gate-blocked; ladder dropped with the 5951772 catalog sync
		"google/gemini-2.5-flash-lite",
		"google/gemini-3.1-flash-lite",
		"google/gemini-3.5-flash-lite",
	} {
		if got := effortsForModel(model); !reflect.DeepEqual(got, reasoningLadder[:]) {
			t.Errorf("effortsForModel(%s) = %v, want full ladder", model, got)
		}
	}
	// Blocked -max variants are gone from the table (issue #153); a stray id
	// falls back to the full ladder, never a hidden row.
	if got := effortsForModel("deepseek/deepseek-v4-flash-max"); !reflect.DeepEqual(got, reasoningLadder[:]) {
		t.Errorf("flash-max efforts = %v, want full ladder (blocked model)", got)
	}
	// mimo-v2.5-pro was removed from free mode 2026-08-04 (paid-only in
	// model-config.ts) — it must NOT pretend to be a free-catalog row.
	if got := effortsForModel("mimo/mimo-v2.5-pro"); !reflect.DeepEqual(got, reasoningLadder[:]) {
		t.Errorf("mimo-v2.5-pro efforts = %v, want full ladder (removed model)", got)
	}
}

func TestNormalizeRequestEffortClamp(t *testing.T) {
	effortFor := func(model, effort string) string {
		body := map[string]any{
			"model":            model,
			"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
			"reasoning_effort": effort,
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		return decode(t, out)["reasoning_effort"].(string)
	}

	// gpt-5.6-luna EFFORTS_THROUGH_MAX includes xhigh: it passes through.
	if got := effortFor("openai/gpt-5.6-luna", "xhigh"); got != "xhigh" {
		t.Errorf("gpt-5.6-luna xhigh = %q, want xhigh", got)
	}
	// muse-spark EFFORTS_THROUGH_XHIGH includes minimal: it passes through.
	if got := effortFor("meta/muse-spark-1.2-contributor", "minimal"); got != "minimal" {
		t.Errorf("muse-spark minimal = %q, want minimal", got)
	}
	// deepseek-v4-pro gained low on 08/13: it passes through unclamped.
	if got := effortFor("deepseek/deepseek-v4-pro", "low"); got != "low" {
		t.Errorf("deepseek-v4-pro low = %q, want low", got)
	}
	// medium on a DeepSeek route rewrites to high (resolveFreebuffReasoningEffort),
	// never down to low.
	if got := effortFor("deepseek/deepseek-v4-flash", "medium"); got != "high" {
		t.Errorf("deepseek-v4-flash medium = %q, want high", got)
	}
	// stealth/ox-alpha shares the DeepSeek V4 ladder ('low','high','max'):
	// medium likewise rewrites to high, never down to low.
	if got := effortFor("stealth/ox-alpha", "medium"); got != "high" {
		t.Errorf("stealth/ox-alpha medium = %q, want high", got)
	}
	// mimo/mimo-v2.5 exposes only disabled/high: every depth rung is a
	// compatibility alias and clamps to high.
	if got := effortFor("mimo/mimo-v2.5", "low"); got != "high" {
		t.Errorf("mimo low = %q, want high", got)
	}
	if got := effortFor("mimo/mimo-v2.5", "max"); got != "high" {
		t.Errorf("mimo max = %q, want high", got)
	}
	// minimax/mimax-m3 likewise has no effort levels (adaptive or disabled
	// thinking only) — every rung clamps to high.
	if got := effortFor("minimax/minimax-m3", "minimal"); got != "high" {
		t.Errorf("minimax-m3 minimal = %q, want high", got)
	}
	if got := effortFor("minimax/minimax-m3", "max"); got != "high" {
		t.Errorf("minimax-m3 max = %q, want high", got)
	}
	// Ignore-routes pass every rung through untouched (CrofAI ignores them).
	if got := effortFor("z-ai/glm-5.2", "ultra"); got != "ultra" {
		t.Errorf("glm-5.2 ultra = %q, want ultra", got)
	}
	if got := effortFor("crof/kimi-k3-eco", "max"); got != "max" {
		t.Errorf("kimi-k3-eco max = %q, want max", got)
	}
	// Unrecognized effort falls back to the default.
	if got := effortFor("openai/gpt-5.6-luna", "banana"); got != defaultReasoningEffort {
		t.Errorf("unknown effort = %q, want %q", got, defaultReasoningEffort)
	}
}

// ---------------------------------------------------------------------------
// Issue #58 — optional prompt & context compression.
// ---------------------------------------------------------------------------
func TestCompressMessages(t *testing.T) {
	msg := func(role, content string) map[string]any {
		return map[string]any{"role": role, "content": content}
	}

	t.Run("budget enforcement with marker", func(t *testing.T) {
		opts := Options{CompressKeepLast: 4}
		msgs := []any{
			msg("system", "sys"),
			msg("user", "u1"),
			msg("assistant", "a1"),
			msg("user", "u2"),
			msg("assistant", "a2"),
			msg("user", "u3"),
			msg("assistant", "a3"),
			msg("user", "u4"),
		}
		got, dropped := compressMessages(msgs, opts)
		if dropped != 3 {
			t.Fatalf("dropped = %d, want 3 (u1, a1, u2)", dropped)
		}
		// 8 messages - 3 dropped + 1 marker = 6.
		if len(got) != 6 {
			t.Fatalf("len = %d, want 6", len(got))
		}
		marker := got[1].(map[string]any)
		if marker["role"] != "system" {
			t.Errorf("marker role = %v, want system", marker["role"])
		}
		if marker["content"] != "[truncated by freebuff-proxy compression; 3 earlier messages omitted]" {
			t.Errorf("marker content = %v", marker["content"])
		}
		// The trailing window (last 4 messages) is preserved verbatim.
		for i, want := range []string{"a2", "u3", "a3", "u4"} {
			if got[2+i].(map[string]any)["content"] != want {
				t.Errorf("trailing message %d = %v, want %q", i, got[2+i], want)
			}
		}
	})

	t.Run("tool messages never dropped", func(t *testing.T) {
		opts := Options{CompressKeepLast: 4}
		toolMsg := msg("tool", "result")
		assistantCall := map[string]any{
			"role":       "assistant",
			"content":    "",
			"tool_calls": []any{map[string]any{"id": "call_1", "type": "function"}},
		}
		msgs := []any{
			msg("system", "sys"),
			msg("user", "u1"),
			assistantCall,
			toolMsg,
			msg("user", "u2"),
			msg("assistant", "a1"),
			msg("user", "u3"),
		}
		got, dropped := compressMessages(msgs, opts)
		if dropped != 1 {
			t.Fatalf("dropped = %d, want 1 (only u1; tool call/result survive)", dropped)
		}
		seenCall, seenTool := false, false
		for _, m := range got {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			if _, has := mm["tool_calls"]; has {
				seenCall = true
			}
			if mm["role"] == "tool" {
				seenTool = true
			}
		}
		if !seenCall || !seenTool {
			t.Errorf("tool call/result lost: seenCall=%v seenTool=%v", seenCall, seenTool)
		}
		// The tool result's content is never truncated.
		if got[len(got)-1].(map[string]any)["role"] == "tool" {
			t.Error("last message should be the user's current turn")
		}
	})

	t.Run("last message never dropped or truncated", func(t *testing.T) {
		opts := Options{CompressKeepLast: 3}
		long := strings.Repeat("x", 4096)
		msgs := []any{
			msg("system", "sys"),
			msg("user", "u1"),
			msg("assistant", "a1"),
			msg("user", long),
			msg("user", long),
		}
		got, _ := compressMessages(msgs, opts)
		last := got[len(got)-1].(map[string]any)
		if last["content"] != long {
			t.Error("last (current) message was truncated")
		}
	})

	t.Run("content cap with marker", func(t *testing.T) {
		opts := Options{CompressKeepLast: 10, CompressMaxContentBytes: 16}
		long := strings.Repeat("x", 100)
		msgs := []any{
			msg("system", "sys"),
			msg("user", long),
			msg("user", long), // the last message: never truncated
		}
		got, dropped := compressMessages(msgs, opts)
		if dropped != 0 {
			t.Fatalf("dropped = %d, want 0", dropped)
		}
		capped := got[1].(map[string]any)["content"].(string)
		if !strings.HasPrefix(capped, "xxxxxxxx") || !strings.HasSuffix(capped, compressContentMarker) {
			t.Errorf("capped content = %q, want prefix + marker", capped)
		}
		if len(capped) >= 100 {
			t.Errorf("capped content not truncated: len %d", len(capped))
		}
		if got[2].(map[string]any)["content"] != long {
			t.Error("last message truncated")
		}
	})

	t.Run("short conversation untouched", func(t *testing.T) {
		opts := Options{CompressKeepLast: 4}
		msgs := []any{msg("system", "sys"), msg("user", "u1"), msg("assistant", "a1")}
		got, dropped := compressMessages(msgs, opts)
		if dropped != 0 || len(got) != len(msgs) {
			t.Fatalf("short conversation changed: dropped=%d len=%d", dropped, len(got))
		}
		for i := range msgs {
			if got[i].(map[string]any)["content"] != msgs[i].(map[string]any)["content"] {
				t.Errorf("message %d not identical", i)
			}
		}
	})
}

func TestNormalizeRequestCompression(t *testing.T) {
	opts := Options{CompressKeepLast: 4}
	body := map[string]any{
		"model":    "m",
		"messages": []any{},
	}
	for i := 0; i < 8; i++ {
		role := "user"
		if i%2 == 0 {
			role = "assistant"
		}
		body["messages"] = append(body["messages"].([]any),
			map[string]any{"role": role, "content": fmt.Sprintf("msg-%d", i)})
	}

	opts.CompressPrompt = true
	out, err := NormalizeRequestOpts(mustJSON(t, body), "", opts)
	if err != nil {
		t.Fatalf("NormalizeRequestOpts: %v", err)
	}
	got := decode(t, out)
	msgs := got["messages"].([]any)
	// keepLast=4 with 8 messages: indices 0-3 are all plain user/assistant
	// turns (the first message is NOT the system prompt, so it is droppable)
	// → 4 dropped, replaced by one marker: 8 - 4 + 1 = 5.
	if len(msgs) != 5 {
		t.Fatalf("compressed messages = %d, want 5 (8 - 4 dropped + 1 marker)", len(msgs))
	}
	if !strings.Contains(msgs[0].(map[string]any)["content"].(string), "4 earlier messages omitted") {
		t.Errorf("marker missing: %v", msgs[0])
	}

	opts.CompressPrompt = false
	out, err = NormalizeRequestOpts(mustJSON(t, body), "", opts)
	if err != nil {
		t.Fatalf("NormalizeRequestOpts: %v", err)
	}
	if got := decode(t, out); len(got["messages"].([]any)) != 8 {
		t.Errorf("compression on=false still compressed: %d messages", len(got["messages"].([]any)))
	}
}

func TestNormalizeRequest_AssistantToolCallContentNull(t *testing.T) {
	t.Run("empty string content with tool_calls becomes null", func(t *testing.T) {
		body := map[string]any{
			"model": "gpt-4o",
			"messages": []any{
				map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []any{
						map[string]any{
							"id":   "call_1",
							"type": "function",
							"function": map[string]any{
								"name":      "get_weather",
								"arguments": `{"city":"Tokyo"}`,
							},
						},
					},
				},
			},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		if !bytes.Contains(out, []byte(`"content":null`)) {
			t.Fatalf("expected output to contain '\"content\":null', got: %s", string(out))
		}
		got := decode(t, out)
		msgs := got["messages"].([]any)
		msg := msgs[0].(map[string]any)
		if msg["content"] != nil {
			t.Errorf("msg.content = %v, want nil", msg["content"])
		}
	})

	t.Run("nil content with tool_calls remains null", func(t *testing.T) {
		body := map[string]any{
			"model": "gpt-4o",
			"messages": []any{
				map[string]any{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []any{
						map[string]any{
							"id":   "call_2",
							"type": "function",
							"function": map[string]any{
								"name":      "calc",
								"arguments": `{}`,
							},
						},
					},
				},
			},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		if !bytes.Contains(out, []byte(`"content":null`)) {
			t.Fatalf("expected output to contain '\"content\":null', got: %s", string(out))
		}
		got := decode(t, out)
		msgs := got["messages"].([]any)
		msg := msgs[0].(map[string]any)
		if msg["content"] != nil {
			t.Errorf("msg.content = %v, want nil", msg["content"])
		}
	})

	t.Run("non-empty content with tool_calls is preserved", func(t *testing.T) {
		body := map[string]any{
			"model": "gpt-4o",
			"messages": []any{
				map[string]any{
					"role":    "assistant",
					"content": "calling tool now",
					"tool_calls": []any{
						map[string]any{
							"id":   "call_3",
							"type": "function",
							"function": map[string]any{
								"name":      "calc",
								"arguments": `{}`,
							},
						},
					},
				},
			},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		msgs := got["messages"].([]any)
		msg := msgs[0].(map[string]any)
		if msg["content"] != "calling tool now" {
			t.Errorf("msg.content = %v, want 'calling tool now'", msg["content"])
		}
	})

	t.Run("empty content without tool_calls remains empty string", func(t *testing.T) {
		body := map[string]any{
			"model": "gpt-4o",
			"messages": []any{
				map[string]any{
					"role":    "assistant",
					"content": "",
				},
			},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		msgs := got["messages"].([]any)
		msg := msgs[0].(map[string]any)
		if msg["content"] != "" {
			t.Errorf("msg.content = %v, want empty string", msg["content"])
		}
	})
}

func TestNormalizeRequest_LeakedThinkTagExtraction(t *testing.T) {
	t.Run("leaked think tag extracted to reasoning_content with tool_calls", func(t *testing.T) {
		body := map[string]any{
			"model": "mimo/mimo-v2.5",
			"messages": []any{
				map[string]any{
					"role":    "assistant",
					"content": "<think>foo</think>",
					"tool_calls": []any{
						map[string]any{
							"id":   "call_1",
							"type": "function",
							"function": map[string]any{
								"name":      "bash",
								"arguments": `{"command":"ls"}`,
							},
						},
					},
				},
			},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		msgs := got["messages"].([]any)
		msg := msgs[0].(map[string]any)
		if msg["reasoning_content"] != "foo" {
			t.Errorf("reasoning_content = %v, want 'foo'", msg["reasoning_content"])
		}
		if msg["content"] != nil {
			t.Errorf("content = %v, want nil", msg["content"])
		}
	})

	t.Run("leaked think tag extracted leaving remaining content", func(t *testing.T) {
		body := map[string]any{
			"model": "mimo/mimo-v2.5",
			"messages": []any{
				map[string]any{
					"role":    "assistant",
					"content": "<think>planning steps</think>Here is my plan",
					"tool_calls": []any{
						map[string]any{
							"id":   "call_2",
							"type": "function",
							"function": map[string]any{
								"name":      "bash",
								"arguments": `{"command":"pwd"}`,
							},
						},
					},
				},
			},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		msgs := got["messages"].([]any)
		msg := msgs[0].(map[string]any)
		if msg["reasoning_content"] != "planning steps" {
			t.Errorf("reasoning_content = %v, want 'planning steps'", msg["reasoning_content"])
		}
		if msg["content"] != "Here is my plan" {
			t.Errorf("content = %v, want 'Here is my plan'", msg["content"])
		}
	})

	t.Run("other think tags variants", func(t *testing.T) {
		for tag, raw := range map[string]string{
			"thinking":       "<thinking>reason A</thinking>",
			"reasoning":      "<reasoning>reason B</reasoning>",
			"antml:thinking": "<antml:thinking>reason C</antml:thinking>",
		} {
			body := map[string]any{
				"model": "gpt-4o",
				"messages": []any{
					map[string]any{
						"role":    "assistant",
						"content": raw,
					},
				},
			}
			out, err := NormalizeRequest(mustJSON(t, body), "")
			if err != nil {
				t.Fatalf("NormalizeRequest (%s): %v", tag, err)
			}
			got := decode(t, out)
			msgs := got["messages"].([]any)
			msg := msgs[0].(map[string]any)
			if msg["reasoning_content"] == "" {
				t.Errorf("tag %s: expected reasoning_content to be extracted, got empty", tag)
			}
		}
	})
}

func TestNormalizeRequest_ReasoningLookupRestoration(t *testing.T) {
	lookup := func(toolID string, content, toolCallsJSON string) (string, string, bool) {
		if toolID == "call_abc123" {
			return "restored reasoning for call_abc123", "sig_123", true
		}
		if strings.Contains(toolCallsJSON, "fallback_tool") {
			return "restored reasoning via json", "sig_json", true
		}
		return "", "", false
	}

	t.Run("restores reasoning_content by tool call id", func(t *testing.T) {
		body := map[string]any{
			"model": "mimo/mimo-v2.5",
			"messages": []any{
				map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []any{
						map[string]any{
							"id":   "call_abc123",
							"type": "function",
							"function": map[string]any{
								"name":      "bash",
								"arguments": `{"command":"whoami"}`,
							},
						},
					},
				},
			},
		}
		out, err := NormalizeRequestOpts(mustJSON(t, body), "", Options{ReasoningLookup: lookup})
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		msgs := got["messages"].([]any)
		msg := msgs[0].(map[string]any)
		if msg["reasoning_content"] != "restored reasoning for call_abc123" {
			t.Errorf("reasoning_content = %v, want 'restored reasoning for call_abc123'", msg["reasoning_content"])
		}
		if msg["content"] != nil {
			t.Errorf("content = %v, want nil", msg["content"])
		}
	})

	t.Run("restores reasoning_content by toolCalls JSON fallback", func(t *testing.T) {
		body := map[string]any{
			"model": "mimo/mimo-v2.5",
			"messages": []any{
				map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []any{
						map[string]any{
							"id":   "unknown_id",
							"type": "function",
							"function": map[string]any{
								"name":      "fallback_tool",
								"arguments": `{}`,
							},
						},
					},
				},
			},
		}
		out, err := NormalizeRequestOpts(mustJSON(t, body), "", Options{ReasoningLookup: lookup})
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		msgs := got["messages"].([]any)
		msg := msgs[0].(map[string]any)
		if msg["reasoning_content"] != "restored reasoning via json" {
			t.Errorf("reasoning_content = %v, want 'restored reasoning via json'", msg["reasoning_content"])
		}
	})

	t.Run("strict reasoning model sets empty reasoning_content when lookup misses", func(t *testing.T) {
		body := map[string]any{
			"model": "mimo/mimo-v2.5",
			"messages": []any{
				map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []any{
						map[string]any{
							"id":   "not_cached_id",
							"type": "function",
							"function": map[string]any{
								"name":      "other_tool",
								"arguments": `{}`,
							},
						},
					},
				},
			},
		}
		out, err := NormalizeRequestOpts(mustJSON(t, body), "", Options{ReasoningLookup: lookup})
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		msgs := got["messages"].([]any)
		msg := msgs[0].(map[string]any)
		rc, ok := msg["reasoning_content"].(string)
		if !ok || rc != "" {
			t.Errorf("reasoning_content = %v (ok=%v), want empty string \"\"", msg["reasoning_content"], ok)
		}
	})
}

func TestNormalizeRequest_MiMoReasoningLadder(t *testing.T) {
	for _, model := range []string{"mimo/mimo-v2.5"} {
		for _, reqEffort := range []string{"low", "medium", "high", "max"} {
			body := map[string]any{
				"model":            model,
				"reasoning_effort": reqEffort,
				"messages": []any{
					map[string]any{"role": "user", "content": "hi"},
				},
			}
			out, err := NormalizeRequest(mustJSON(t, body), "")
			if err != nil {
				t.Fatalf("NormalizeRequest(%s, %s): %v", model, reqEffort, err)
			}
			got := decode(t, out)
			if got["reasoning_effort"] != "high" {
				t.Errorf("model %s with requested effort %s got %v, want 'high'", model, reqEffort, got["reasoning_effort"])
			}
		}
	}

	for _, eff := range []string{"low", "medium", "high", "xhigh", "max"} {
		body := map[string]any{
			"model":            "anthropic/claude-fable-5",
			"reasoning_effort": eff,
			"messages": []any{
				map[string]any{"role": "user", "content": "hi"},
			},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest(claude-fable-5, %s): %v", eff, err)
		}
		got := decode(t, out)
		if got["reasoning_effort"] != eff {
			t.Errorf("model anthropic/claude-fable-5 with requested effort %s got %v, want %s", eff, got["reasoning_effort"], eff)
		}
	}
}
