package tokenestimate_test

// Golden values in this file were computed with the independent Python
// reference (OpenAI tiktoken 0.13.0, o200k_base, allowed_special="all",
// floor(raw * 1.35)) — see scripts/token_ref/reference.py in the repo. Go
// counts are asserted against those numbers so the estimator's o200k_base
// tokenization is cross-validated against the reference implementation.

import (
	"strings"
	"sync"
	"testing"

	"freebuff-proxy/backend/internal/tokenestimate"
	"github.com/tiktoken-go/tokenizer"
)

func mustNew(t *testing.T) *tokenestimate.Estimator {
	t.Helper()
	e, err := tokenestimate.New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return e
}

func TestCountTextGolden(t *testing.T) {
	// Values from scripts/token_ref/reference.py: floor(raw * 1.35), o200k_base.
	const codeFixture = `func (e *Estimator) CountText(text string) int {
	if text == "" {
		return 0
	}
	n, err := e.codec.Count(text)
	if err != nil {
		units := len(utf16.Encode([]rune(text)))
		return (units + fallbackDivisor - 1) / fallbackDivisor
	}
	return int(math.Floor(float64(n) * fudgeFactor))
}`
	cases := []struct {
		name string
		text string
		want int
	}{
		{"empty", "", 0},
		{"hello", "hello", 1},
		{"hello world", "hello world", 2},
		{"Hello, world!", "Hello, world!", 5},
		{"indonesian", "Saya sedang menguji penghitung token untuk aplikasi ini.", 14},
		{"cjk", "你好世界，这是一个测试。", 8},
		{"emoji combining", "👨\u200d💻🚀 café naïve e\u0301", 16},
		{"go code", codeFixture, 116},
		{"long", strings.Repeat("The quick brown fox jumps over the lazy dog. ", 60), 811},
	}
	e := mustNew(t)
	for _, tc := range cases {
		if got := e.CountText(tc.text); got != tc.want {
			t.Errorf("%s: CountText = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestCountTextSpecialMarkers pins the behavior of OpenAI-style special
// markers. tiktoken-go/tokenizer has no special-token encoder: markers are
// BPE-encoded as regular text instead of collapsing to one token (the Python
// reference with allowed_special="all" yields 1 for <|endoftext|>). The
// exact values below are the deterministic over-count of the local port
// (9 for the endof* markers, 8 for fim_*); a change here means the tokenizer
// behavior changed and the values must be re-derived from the reference.
func TestCountTextSpecialMarkers(t *testing.T) {
	e := mustNew(t)
	cases := map[string]int{
		"<|endoftext|>":   9,
		"<|endofprompt|>": 9,
		"<|endofmask|>":   9,
		"<|fim_prefix|>":  8,
		"<|fim_suffix|>":  8,
		"<|fim_middle|>":  8,
	}
	for s, want := range cases {
		first := e.CountText(s)
		if first != want {
			t.Errorf("CountText(%q) = %d, want %d (documented over-count)", s, first, want)
		}
		if second := e.CountText(s); second != first {
			t.Errorf("CountText(%q) not deterministic: %d then %d", s, first, second)
		}
	}
}

func TestCountJSONGolden(t *testing.T) {
	// Canonical Go json.Marshal output (sorted map keys, no spaces).
	cases := []struct {
		name  string
		value any
		want  int
	}{
		{"tool_input_small", map[string]any{"city": "Jakarta"}, 8},
		{"tool_input_big", map[string]any{
			"city":  "Jakarta",
			"extra": strings.Repeat("a", 64),
			"unit":  "celsius",
		}, 29},
		{"unknown_block", map[string]any{"foo": "bar", "type": "weird"}, 13},
		{"json_ok", "ok", 4},
	}
	e := mustNew(t)
	for _, tc := range cases {
		if got := e.CountJSON(tc.value); got != tc.want {
			t.Errorf("%s: CountJSON = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestCountAnthropicRequestGolden(t *testing.T) {
	// The full mixed request from the reference fixture. Expected total:
	//   system "You are a helpful assistant."                      = 8
	//   user "hello"                                   8 + 1      = 9
	//   assistant thinking "Let me think about this."  8 + 8      = 16
	//   assistant tool_use (name+input)                8 + 2 + 8  = 18
	//   user tool_result [{text "3 files"}]            8 + 2      = 10
	//   tools (tools_minimal JSON)                                = 32
	//                                                             = 93
	req := map[string]any{
		"model":  "z-ai/glm-5.2",
		"system": "You are a helpful assistant.",
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "Let me think about this."},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "t1", "name": "get_weather", "input": map[string]any{"city": "Jakarta"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": []any{
					map[string]any{"type": "text", "text": "3 files"},
				}},
			}},
		},
		"tools": []any{
			map[string]any{"name": "get_weather", "input_schema": map[string]any{
				"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}},
			}},
		},
	}
	e := mustNew(t)
	got, err := e.CountAnthropicRequest(req)
	if err != nil {
		t.Fatalf("CountAnthropicRequest: %v", err)
	}
	if got != 93 {
		t.Errorf("CountAnthropicRequest = %d, want 93", got)
	}
}

func TestCountAnthropicRequestPerMessageOverhead(t *testing.T) {
	e := mustNew(t)
	one := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "hi"},
	}}
	got, err := e.CountAnthropicRequest(one)
	if err != nil {
		t.Fatal(err)
	}
	if got != 9 { // 8 overhead + 1 for "hi"
		t.Errorf("1 message = %d, want 9", got)
	}
	two := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "hi"},
	}}
	got, err = e.CountAnthropicRequest(two)
	if err != nil {
		t.Fatal(err)
	}
	if got != 18 { // 2 * (8 + 1)
		t.Errorf("2 messages = %d, want 18", got)
	}
}

// TestCountAnthropicRequestImageFlat pins the flat per-image cost: base64
// length must never influence the count (images are never tokenized), and
// both base64 and URL sources bill the same 1600/image.
func TestCountAnthropicRequestImageFlat(t *testing.T) {
	e := mustNew(t)
	short := "aGVsbG8="
	long := strings.Repeat("QUJD", 4096) // 16KiB of base64
	for _, tc := range []struct {
		name   string
		req    map[string]any
		images int
	}{
		{"short base64", imageRequest("base64", short), 1},
		{"long base64", imageRequest("base64", long), 1},
		{"url", imageRequest("url", "https://example.com/pic.png"), 1},
		{"two images", map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": []any{
				imagePart("base64", short),
				imagePart("base64", short),
			}},
		}}, 2},
	} {
		got, err := e.CountAnthropicRequest(tc.req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if want := 8 + tc.images*1600; got != want {
			t.Errorf("%s: count = %d, want %d", tc.name, got, want)
		}
	}
}

// TestCountAnthropicRequestSystemImageFlat pins that image parts inside the
// top-level system array bill the same flat 1600 as every other image path —
// and, critically, that the base64 payload is never tokenized: the count must
// be identical for a 24-byte and a 128KiB payload (regression: this used to
// fall through to the JSON fallback and BPE-count the base64 as text,
// quadratic in payload size — ~87s and ~236k tokens for 256KiB).
func TestCountAnthropicRequestSystemImageFlat(t *testing.T) {
	e := mustNew(t)
	systemReq := func(data string) map[string]any {
		return map[string]any{
			"system": []any{imagePart("base64", data)},
			"messages": []any{
				map[string]any{"role": "user", "content": "hi"},
			},
		}
	}
	short := systemReq("aGVsbG8=")
	long := systemReq(strings.Repeat("QUJD", (128<<10)/3)) // 128KiB of base64
	for _, tc := range []struct {
		name string
		req  map[string]any
	}{
		{"short base64", short},
		{"long base64", long},
	} {
		got, err := e.CountAnthropicRequest(tc.req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if want := 1600 + 8 + 1; got != want { // system image + message overhead + "hi"
			t.Errorf("%s: count = %d, want %d (flat 1600, base64 never tokenized)", tc.name, got, want)
		}
	}
}

func imageRequest(sourceType, data string) map[string]any {
	return map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{imagePart(sourceType, data)}},
	}}
}

func imagePart(sourceType, data string) map[string]any {
	return map[string]any{"type": "image", "source": map[string]any{
		"type": sourceType, "media_type": "image/png", "data": data,
	}}
}

func TestCountAnthropicRequestToolsGolden(t *testing.T) {
	// tools_minimal / tools_rich / tools_two canonical JSON from
	// token_ref/reference.py; each request has one empty user message
	// (8 overhead, 0 content) plus the tool definitions.
	emptyMsg := []any{map[string]any{"role": "user"}}
	cases := []struct {
		name string
		req  map[string]any
		want int
	}{
		{"no tools", map[string]any{"messages": emptyMsg}, 8},
		{"tools_minimal", map[string]any{"messages": emptyMsg, "tools": []any{
			map[string]any{"name": "get_weather", "input_schema": map[string]any{
				"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}},
			}},
		}}, 8 + 32},
		{"tools_rich", map[string]any{"messages": emptyMsg, "tools": []any{
			map[string]any{
				"name":        "get_weather",
				"description": "Get current weather",
				"input_schema": map[string]any{
					"type":     "object",
					"required": []any{"city"},
					"properties": map[string]any{
						"city": map[string]any{"description": "City name", "type": "string"},
						"unit": map[string]any{"enum": []any{"celsius", "fahrenheit"}, "type": "string"},
					},
				},
			},
		}}, 8 + 75},
		{"tools_two", map[string]any{"messages": emptyMsg, "tools": []any{
			map[string]any{
				"name":        "get_weather",
				"description": "Get current weather",
				"input_schema": map[string]any{
					"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}},
				},
			},
			map[string]any{"name": "search_docs", "input_schema": map[string]any{
				"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}},
			}},
		}}, 8 + 70},
	}
	e := mustNew(t)
	for _, tc := range cases {
		got, err := e.CountAnthropicRequest(tc.req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: count = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestCountAnthropicRequestToolKeyOrder proves map-key insertion order does
// not change the count: Go's json.Marshal sorts keys, so the same tool
// object counted in any construction order is byte-stable.
func TestCountAnthropicRequestToolKeyOrder(t *testing.T) {
	e := mustNew(t)
	base := map[string]any{"messages": []any{map[string]any{"role": "user"}}}
	a := map[string]any{"messages": base["messages"], "tools": []any{
		map[string]any{
			"name":        "get_weather",
			"description": "Get current weather",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"city": map[string]any{"type": "string", "description": "City name"},
					"unit": map[string]any{"type": "string", "enum": []any{"celsius", "fahrenheit"}},
				},
			},
		},
	}}
	b := map[string]any{"messages": base["messages"], "tools": []any{
		map[string]any{
			"description": "Get current weather",
			"input_schema": map[string]any{
				"properties": map[string]any{
					"unit": map[string]any{"enum": []any{"celsius", "fahrenheit"}, "type": "string"},
					"city": map[string]any{"description": "City name", "type": "string"},
				},
				"type": "object",
			},
			"name": "get_weather",
		},
	}}
	gotA, err := e.CountAnthropicRequest(a)
	if err != nil {
		t.Fatal(err)
	}
	gotB, err := e.CountAnthropicRequest(b)
	if err != nil {
		t.Fatal(err)
	}
	if gotA != gotB {
		t.Errorf("key order changed count: %d vs %d", gotA, gotB)
	}
}

func TestCountAnthropicRequestErrors(t *testing.T) {
	e := mustNew(t)
	doc := map[string]any{"type": "document", "source": map[string]any{
		"type": "base64", "media_type": "application/pdf", "data": "JVBERi0=",
	}}
	cases := []struct {
		name string
		req  map[string]any
	}{
		{"missing messages", map[string]any{"model": "x"}},
		{"messages not array", map[string]any{"messages": "hi"}},
		{"document in system array", map[string]any{
			"system":   []any{doc},
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}},
		{"document in messages", map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": []any{doc}},
		}}},
		{"document in tool_result", map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "content": []any{doc}},
			}},
		}}},
	}
	for _, tc := range cases {
		if _, err := e.CountAnthropicRequest(tc.req); err == nil {
			t.Errorf("%s: want error, got nil", tc.name)
		}
	}
}

func TestCountAnthropicRequestContentShapes(t *testing.T) {
	e := mustNew(t)
	msg := func(content any) map[string]any {
		return map[string]any{"messages": []any{map[string]any{"role": "user", "content": content}}}
	}
	cases := []struct {
		name string
		req  map[string]any
		want int
	}{
		{"nil content", msg(nil), 8},
		{"empty content", msg(""), 8},
		{"string content", msg("hello"), 9},
		{"unknown block JSON fallback", msg([]any{map[string]any{"foo": "bar", "type": "weird"}}), 8 + 13},
		{"text block", msg([]any{map[string]any{"type": "text", "text": "hello"}}), 9},
		{"thinking uses thinking field", msg([]any{map[string]any{"type": "thinking", "thinking": "Let me think about this."}}), 8 + 8},
		{"thinking falls back to text", msg([]any{map[string]any{"type": "thinking", "text": "Let me think about this."}}), 8 + 8},
		{"tool_use name and input", msg([]any{map[string]any{"type": "tool_use", "name": "get_weather", "input": map[string]any{"city": "Jakarta"}}}), 8 + 2 + 8},
		{"tool_result string", msg([]any{map[string]any{"type": "tool_result", "content": "3 files"}}), 8 + 2},
		{"tool_result structured text", msg([]any{map[string]any{"type": "tool_result", "content": []any{
			map[string]any{"type": "text", "text": "3 files"},
		}}}), 8 + 2},
		{"tool_result structured image", msg([]any{map[string]any{"type": "tool_result", "content": []any{
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "aGVsbG8="}},
		}}}), 8 + 1600},
	}
	for _, tc := range cases {
		got, err := e.CountAnthropicRequest(tc.req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: count = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestCountAnthropicRequestSystemShapes(t *testing.T) {
	e := mustNew(t)
	sysReq := func(system any) map[string]any {
		return map[string]any{"system": system, "messages": []any{map[string]any{"role": "user", "content": ""}}}
	}
	cases := []struct {
		name   string
		system any
		want   int
	}{
		{"string", "You are a helpful assistant.", 8 + 8},
		{"text block array", []any{map[string]any{"type": "text", "text": "You are a helpful assistant."}}, 8 + 8},
		{"null", nil, 8},
		{"non-string non-array dropped", 42, 8},
	}
	for _, tc := range cases {
		got, err := e.CountAnthropicRequest(sysReq(tc.system))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: count = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestCountAnthropicRequestNonObjectMessage pins that non-object entries in
// the messages array are skipped (the /v1/messages conversion drops them),
// matching the one-message overhead arithmetic for the valid entry only.
func TestCountAnthropicRequestNonObjectMessage(t *testing.T) {
	e := mustNew(t)
	req := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "hi"},
		42,
		"not an object",
	}}
	got, err := e.CountAnthropicRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if got != 9 {
		t.Errorf("count = %d, want 9 (only the object message counts)", got)
	}
}

func TestDeterminism(t *testing.T) {
	e := mustNew(t)
	req := map[string]any{
		"system": "You are a helpful assistant.",
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "name": "get_weather", "input": map[string]any{"city": "Jakarta"}},
			}},
		},
		"tools": []any{map[string]any{"name": "get_weather", "input_schema": map[string]any{
			"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}},
		}}},
	}
	first, err := e.CountAnthropicRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		again, err := e.CountAnthropicRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("iteration %d: count = %d, want %d", i, again, first)
		}
	}
}

// TestConcurrentUse runs CountText and CountAnthropicRequest from many
// goroutines against the shared codec; the -race detector validates the
// estimator is safe for concurrent use (Count/Encode are read-only).
func TestConcurrentUse(t *testing.T) {
	e := mustNew(t)
	req := map[string]any{
		"system": "You are a helpful assistant.",
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "name": "get_weather", "input": map[string]any{"city": "Jakarta"}},
			}},
		},
		"tools": []any{map[string]any{"name": "get_weather", "input_schema": map[string]any{
			"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}},
		}}},
	}
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if _, err := e.CountAnthropicRequest(req); err != nil {
					t.Errorf("CountAnthropicRequest: %v", err)
					return
				}
				if got := e.CountText("hello world"); got != 2 {
					t.Errorf("CountText = %d, want 2", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestDecodeRoundTrip pins the Decode API (issue #243): ids produced by
// Encode decode back to the text, and concurrent Decode calls do not race
// (the shared codec's reverse-vocabulary map is built lazily and would
// crash without the internal lock).
func TestDecodeRoundTrip(t *testing.T) {
	est, err := tokenestimate.New()
	if err != nil {
		t.Fatal(err)
	}
	text := "hello, world — token estimate round trip"
	codec, err := tokenizer.Get(tokenizer.O200kBase)
	if err != nil {
		t.Fatal(err)
	}
	ids, _, err := codec.Encode(text)
	if err != nil {
		t.Fatal(err)
	}
	got, err := est.Decode(ids)
	if err != nil {
		t.Fatal(err)
	}
	if got != text {
		t.Errorf("decoded = %q, want %q", got, text)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := est.Decode(ids); err != nil {
				t.Errorf("concurrent Decode: %v", err)
			}
		}()
	}
	wg.Wait()
}
