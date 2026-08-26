package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/internal/testutil"
)

// TestChatToolNameToleranceE2E pins the issue #140 P2a layer end-to-end: a
// client dispatching on THIRD-PARTY tool names (Cline's read_file /
// write_to_file / execute_command) has them renamed to official signature
// names on the upstream wire (so foreign_toolset / third_party_client never
// fire), and the model's tool_call comes back carrying the CLIENT's name.
func TestChatToolNameToleranceE2E(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Echo the OFFICIAL name, as upstream would: the model saw read_files
	// (the renamed client tool) and calls it.
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := map[string]any{
			"id": "chatcmpl-tol",
			"choices": []any{map[string]any{
				"index": float64(0),
				"delta": map[string]any{
					"tool_calls": []any{map[string]any{
						"index":    float64(0),
						"id":       "call_tol1",
						"type":     "function",
						"function": map[string]any{"name": "read_files", "arguments": `{"path":"config.json"}`},
					}},
				},
				"finish_reason": nil,
			}},
		}
		b, _ := json.Marshal(chunk)
		_, _ = w.Write(append(b, '\n'))
		fin := map[string]any{
			"id": "chatcmpl-tol",
			"choices": []any{map[string]any{
				"index":         float64(0),
				"delta":         map[string]any{},
				"finish_reason": "tool_calls",
			}},
		}
		fb, _ := json.Marshal(fin)
		_, _ = w.Write(append(fb, '\n'))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}
	ts, _ := newTestServer(t, nil, mock)

	// Cline-style third-party toolset.
	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"read config"}],"stream":true,"tools":[
		{"type":"function","function":{"name":"read_file","description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}},
		{"type":"function","function":{"name":"execute_command","description":"Run a command","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}},
		{"type":"function","function":{"name":"ask_followup_question","parameters":{"type":"object","properties":{"question":{"type":"string"}}}}}
	]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}

	// Upstream saw OFFICIAL names (plus end_turn injection), never client ones.
	var upstreamNames []string
	for _, raw := range mock.RecordedChatBodiesSnapshot() {
		var sent struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.Unmarshal([]byte(raw), &sent); err != nil {
			t.Fatalf("recorded body not JSON: %v", err)
		}
		for _, tl := range sent.Tools {
			upstreamNames = append(upstreamNames, tl.Function.Name)
		}
	}
	joined := strings.Join(upstreamNames, ",")
	if !strings.Contains(joined, `"read_files"`) && !strings.Contains(joined, "read_files") {
		t.Errorf("upstream tools missing read_files: %v", upstreamNames)
	}
	if !strings.Contains(joined, "run_terminal_command") {
		t.Errorf("upstream tools missing run_terminal_command: %v", upstreamNames)
	}
	if strings.Contains(joined, "read_file,") || strings.Contains(joined, "execute_command") {
		t.Errorf("client names leaked upstream: %v", upstreamNames)
	}

	// The client sees ITS name back in the stream.
	if !strings.Contains(string(data), `"read_file"`) {
		t.Errorf("response missing client name read_file: %s", data)
	}
	if strings.Contains(string(data), `"read_files"`) {
		t.Errorf("official name leaked to client: %s", data)
	}
}
