package server_test

// Request-side feature-translation parity tests (HTTP level): every
// supported feature param of the three surfaces must map to the upstream
// chat envelope (or error / be documented), and unsupported feature-flagged
// params must produce explicit 400s — never silent drops. These tests pin
// the request-layer contract only; response-side shaping is covered by the
// stream/json relay tests. Pure unit mapping tests live in
// request_params_internal_test.go (package server).

import (
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// TestAnthropic_MaxTokensDefaultEndToEnd drives the full /v1/messages
// handler with an absent max_tokens and asserts the upstream envelope
// carries the 8192 default.
func TestAnthropic_MaxTokensDefaultEndToEnd(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), map[string]string{
		"x-api-key": "test-key", "anthropic-version": "2023-06-01",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	if !mock.BodyContains(`"max_tokens":8192`) {
		t.Error("upstream body missing the max_tokens default 8192")
	}
}

// --- OpenAI /v1/chat/completions ---

func TestChatUnsupportedParamsRejected(t *testing.T) {
	cases := []struct {
		name    string
		extra   string // inserted into a minimal chat body
		wantErr string
	}{
		{"n=2", `,"n":2`, `only n=1 is supported`},
		{"n=0", `,"n":0`, `only n=1 is supported`},
		{"audio", `,"audio":{"voice":"alloy","format":"mp3"}`, `audio output is not supported`},
		{"web_search_options", `,"web_search_options":{"search_context_size":"low"}`, `web_search_options`},
		{"moderation", `,"moderation":"low"`, `request moderation is not supported`},
		{"allowed_tools", `,"allowed_tools":[{"mode":"auto","tools":[{"type":"function","function":{"name":"get_weather"}}]}]`, `allowed_tools`},
		{"custom_tool", `,"tools":[{"type":"custom","custom":{"name":"calc"}}]`, `only function tools translate`},
		{"tool_choice_bad_string", `,"tool_choice":"sometimes"`, `tool_choice`},
		{"tool_choice_allowed", `,"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[]}`, `only named function choices translate`},
		{"tool_choice_custom", `,"tool_choice":{"type":"custom","name":"calc"}`, `only named function choices translate`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mock.ChatBody = responsesChunks()
			ts, _ := newTestServer(t, nil, mock)
			body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"ping"}]` + tc.extra + `}`
			resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, truncate(string(data), 200))
			}
			if !strings.Contains(string(data), tc.wantErr) {
				t.Errorf("error body missing %q: %s", tc.wantErr, truncate(string(data), 200))
			}
			if mock.Requests != 0 {
				t.Errorf("upstream requests = %d, want 0 (rejected before pool)", mock.Requests)
			}
		})
	}
}

// TestChatNAccepted verifies n=1 (the default value) is accepted and the
// gateway still serves the request.
func TestChatNAccepted(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	ts, _ := newTestServer(t, nil, mock)
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"ping"}],"n":1}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
}

// TestChatFunctionToolsAccepted verifies the validator lets real function
// tools and a named function choice through (only the new non-function
// shapes are rejected).
func TestChatFunctionToolsAccepted(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	ts, _ := newTestServer(t, nil, mock)
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"ping"}],` +
		`"tools":[{"type":"function","function":{"name":"get_weather","description":"w","parameters":{"type":"object"}}}],` +
		`"tool_choice":{"type":"function","function":{"name":"get_weather"}}}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if !mock.BodyContains(`"get_weather"`) {
		t.Errorf("upstream body missing function tool: %s", truncate(mock.LastChatBody(), 300))
	}
}

// --- OpenAI /v1/responses ---

func TestResponsesUnsupportedParamsRejected(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			"previous_response_id",
			`{"model":"` + modelA + `","input":"ping","previous_response_id":"resp_123"}`,
			`previous_response_id`,
		},
		{
			"previous_response_id_empty",
			`{"model":"` + modelA + `","input":"ping","previous_response_id":""}`,
			"", // empty id is a no-op, accepted
		},
		{
			"conversation",
			`{"model":"` + modelA + `","input":"ping","conversation":"conv_1"}`,
			`conversation`,
		},
		{
			"background",
			`{"model":"` + modelA + `","input":"ping","background":true}`,
			`background runs are not supported`,
		},
		{
			"moderation",
			`{"model":"` + modelA + `","input":"ping","moderation":"low"}`,
			`request moderation is not supported`,
		},
		{
			"web_search_tool",
			`{"model":"` + modelA + `","input":"ping","tools":[{"type":"web_search"}]}`,
			`built-in tool type `,
		},
		{
			"builtin_tool_choice",
			`{"model":"` + modelA + `","input":"ping","tool_choice":{"type":"web_search"}}`,
			`only function tools translate`,
		},
		{
			"audio_input_part",
			`{"model":"` + modelA + `","input":[{"role":"user","content":[{"type":"input_audio","data":"x"}]}]}`,
			`audio input parts are not supported`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mock.ChatBody = responsesChunks()
			ts, _ := newTestServer(t, nil, mock)
			resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(tc.body), nil)
			if tc.wantErr == "" {
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
				}
				return
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, truncate(string(data), 200))
			}
			if !strings.Contains(string(data), tc.wantErr) {
				t.Errorf("error body missing %q: %s", tc.wantErr, truncate(string(data), 200))
			}
			if mock.Requests != 0 {
				t.Errorf("upstream requests = %d, want 0 (rejected before pool)", mock.Requests)
			}
		})
	}
}

// TestResponsesToolChoiceStrings verifies the string tool_choice forms map
// through (previously they were dropped, silently defaulting to auto).
func TestResponsesToolChoiceStrings(t *testing.T) {
	for _, choice := range []string{"required", "none", "auto"} {
		t.Run(choice, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mock.ChatBody = responsesChunks()
			ts, _ := newTestServer(t, nil, mock)
			body := `{"model":"` + modelA + `","input":"ping","tool_choice":"` + choice + `"}`
			resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 200))
			}
			if !mock.BodyContains(`"tool_choice":"` + choice + `"`) {
				t.Errorf("upstream body missing tool_choice %q: %s", choice, mock.LastChatBody())
			}
		})
	}
}

// TestResponsesReasoningDisabled verifies reasoning.enabled:false suppresses
// reasoning_effort instead of silently shipping the effort anyway.
func TestResponsesReasoningDisabled(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	ts, _ := newTestServer(t, nil, mock)
	body := `{"model":"` + modelA + `","input":"ping","reasoning":{"enabled":false,"effort":"high"}}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if mock.BodyContains("reasoning_effort") {
		t.Errorf("upstream body carries reasoning_effort despite enabled:false: %s", mock.LastChatBody())
	}
}

// TestResponsesReasoningEnabled verifies the enabled (or omitted) path still
// maps effort (clamped per model by NormalizeRequest).
func TestResponsesReasoningEnabled(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	ts, _ := newTestServer(t, nil, mock)
	body := `{"model":"` + modelA + `","input":"ping","reasoning":{"enabled":true,"effort":"high"}}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if !mock.BodyContains(`"reasoning_effort":"high"`) {
		t.Errorf("upstream body missing reasoning_effort high: %s", mock.LastChatBody())
	}
}

// TestResponsesInputImageMapped verifies input_image parts translate to chat
// image_url parts instead of being silently dropped.
func TestResponsesInputImageMapped(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	ts, _ := newTestServer(t, nil, mock)
	body := `{"model":"` + modelA + `","input":[{"role":"user","content":[{"type":"input_text","text":"what is this?"},{"type":"input_image","image_url":"https://example.com/cat.png"}]}]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if !mock.BodyContains(`"type":"image_url"`) || !mock.BodyContains(`"url":"https://example.com/cat.png"`) {
		t.Errorf("upstream body missing mapped image_url part: %s", mock.LastChatBody())
	}
}

// TestResponsesFunctionCallOutputReplay verifies function_call_output items
// become role-"tool" messages with the matching tool_call_id.
func TestResponsesFunctionCallOutputReplay(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	ts, _ := newTestServer(t, nil, mock)
	body := `{"model":"` + modelA + `","input":[{"type":"function_call_output","call_id":"call_1","output":"{\"temp\":68}"},{"role":"user","content":"done"}]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if !mock.BodyContains(`"role":"tool"`) || !mock.BodyContains(`"tool_call_id":"call_1"`) ||
		!mock.BodyContains(`{\"temp\":68}`) {
		t.Errorf("upstream body missing replayed tool message: %s", mock.LastChatBody())
	}
}

// --- Anthropic /v1/messages ---

// TestAnthropicServerToolsRejected verifies server-side tool declarations
// (typed blocks, nameless entries) and the container param fail with an
// explicit 400 instead of mistranslating into an empty-named function tool
// upstream. Plain client function tools still pass.
func TestAnthropicServerToolsRejected(t *testing.T) {
	cases := []struct {
		name    string
		extra   string
		wantErr string
	}{
		{"nameless_tool", `,"tools":[{"description":"no name here"}]`, `every tool needs a name`},
		{"server_tool_type", `,"tools":[{"type":"web_search_20260222","name":"web_search"}]`, `only client function tools`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := testutil.NewMock()
			defer mock.Close()
			mock.ChatBody = responsesChunks()
			ts, _ := newTestServer(t, nil, mock)
			body := `{"model":"` + modelA + `","max_tokens":100,"messages":[{"role":"user","content":"ping"}]` + tc.extra + `}`
			resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), map[string]string{
				"anthropic-version": "2023-06-01",
			})
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, truncate(string(data), 200))
			}
			if !strings.Contains(string(data), tc.wantErr) {
				t.Errorf("error body missing %q: %s", tc.wantErr, truncate(string(data), 200))
			}
			if mock.Requests != 0 {
				t.Errorf("upstream requests = %d, want 0 (rejected before pool)", mock.Requests)
			}
		})
	}

	// Plain client function tools are unaffected by the server-tool gate.
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	ts, _ := newTestServer(t, nil, mock)
	body := `{"model":"` + modelA + `","max_tokens":100,"messages":[{"role":"user","content":"ping"}],` +
		`"tools":[{"name":"get_weather","description":"w","input_schema":{"type":"object"}}]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), map[string]string{
		"anthropic-version": "2023-06-01",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if !mock.BodyContains(`"get_weather"`) {
		t.Errorf("upstream body missing function tool: %s", truncate(mock.LastChatBody(), 300))
	}
}

// TestTrailingSlashTolerance verifies harness baseURL joins with a trailing
// slash resolve to the same handlers (goose derive_base_path, LibreChat
// custom endpoints, SillyTavern all vary here): a 404 on a trailing slash
// is a pure client-config artifact, never a missing route.
func TestTrailingSlashTolerance(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	ts, _ := newTestServer(t, nil, mock)
	chatBody := `{"model":"` + modelA + `","messages":[{"role":"user","content":"ping"}]}`
	for _, path := range []string{"/v1/chat/completions/", "/v1/messages/"} {
		headers := map[string]string(nil)
		if path == "/v1/messages/" {
			headers = map[string]string{"anthropic-version": "2023-06-01"}
		}
		resp, data := doJSON(t, http.MethodPost, ts.URL+path, []byte(chatBody), headers)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("POST %s status = %d, want 200: %s", path, resp.StatusCode, truncate(string(data), 200))
		}
	}
	// Note: GET /v1/models/ stays claimed by the /v1/models/{model...}
	// wildcard (stdlib mux rejects the overlap), so only the POST twins
	// are asserted here.
}
