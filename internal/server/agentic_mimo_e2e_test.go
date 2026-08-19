package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"freebuff-proxy/internal/testutil"
)

const mimoModelID = "mimo/mimo-v2.5"

// mockAgentWorkspace simulates the local filesystem and execution environment
// of an AI coding agent CLI (e.g. omp / Claude Code / Codex).
type mockAgentWorkspace struct {
	mu    sync.Mutex
	Files map[string]string
}

func newMockAgentWorkspace() *mockAgentWorkspace {
	return &mockAgentWorkspace{
		Files: map[string]string{
			"config.json": "{\n  \"port\": 3000,\n  \"host\": \"127.0.0.1\"\n}",
		},
	}
}

func (w *mockAgentWorkspace) ReadFile(path string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c, ok := w.Files[path]
	if !ok {
		return "", fmt.Errorf("file not found: %s", path)
	}
	return c, nil
}

func (w *mockAgentWorkspace) WriteFile(path, content string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.Files[path] = content
	return fmt.Sprintf(`{"status":"ok","bytes_written":%d}`, len(content)), nil
}

func (w *mockAgentWorkspace) ExecuteBash(command string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if strings.Contains(command, "config.json") {
		c, ok := w.Files["config.json"]
		if ok {
			return c, nil
		}
	}
	if strings.Contains(command, "ls") {
		var names []string
		for k := range w.Files {
			names = append(names, k)
		}
		return strings.Join(names, "\n"), nil
	}
	return fmt.Sprintf("executed: %s", command), nil
}

// openAIToolDefs returns standard OpenAI function declarations for the agent tools.
func openAIToolDefs() []map[string]any {
	return []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        "read_file",
				"description": "Read file contents at the specified path",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type":        "string",
							"description": "Path of the file to read",
						},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "write_file",
				"description": "Write content to the file at the specified path",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type":        "string",
							"description": "Path of the file to write",
						},
						"content": map[string]any{
							"type":        "string",
							"description": "Content to write into the file",
						},
					},
					"required": []string{"path", "content"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "execute_bash",
				"description": "Execute a shell command in the local workspace",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{
							"type":        "string",
							"description": "The command to run",
						},
					},
					"required": []string{"command"},
				},
			},
		},
	}
}

// anthropicToolDefs returns Anthropic input_schema declarations for the agent tools.
func anthropicToolDefs() []map[string]any {
	return []map[string]any{
		{
			"name":        "read_file",
			"description": "Read file contents at the specified path",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Path of the file to read",
					},
				},
				"required": []string{"path"},
			},
		},
		{
			"name":        "write_file",
			"description": "Write content to the file at the specified path",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Path of the file to write",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "Content to write into the file",
					},
				},
				"required": []string{"path", "content"},
			},
		},
		{
			"name":        "execute_bash",
			"description": "Execute a shell command in the local workspace",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "The command to run",
					},
				},
				"required": []string{"command"},
			},
		},
	}
}

func buildSSEChunk(id, model string, delta map[string]any, finishReason any) string {
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": finishReason,
			},
		},
	}
	b, _ := json.Marshal(chunk)
	return "data: " + string(b) + "\n\n"
}

// mimoTurnMockHandler simulates the multi-turn upstream MiMo V2.5 backend.
// It verifies that:
// 1. In Turn 2, incoming assistant messages have content: null and restored reasoning_content.
// 2. In Turn 3, incoming assistant messages have content: null and restored reasoning_content across all prior turns.
type mimoTurnMockHandler struct {
	t    *testing.T
	mock *testutil.MockUpstream
	mu   sync.Mutex

	turnCount int

	turn1Reasoning string
	turn2Reasoning string
	turn3Reasoning string

	turn1ToolCallID string
	turn2ToolCallID string
}

func findMessagesByRole(msgs []any, role string) []map[string]any {
	var res []map[string]any
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok && mm["role"] == role {
			res = append(res, mm)
		}
	}
	return res
}

func newMimoTurnMockHandler(t *testing.T, mock *testutil.MockUpstream, idPrefix string) *mimoTurnMockHandler {
	return &mimoTurnMockHandler{
		t:               t,
		mock:            mock,
		turn1Reasoning:  "MiMo Thinking: I need to inspect config.json first to check the current port configuration.",
		turn2Reasoning:  "MiMo Thinking: The current port is 3000. Now I will update config.json with port 8080.",
		turn3Reasoning:  "MiMo Thinking: The configuration file has been updated with port 8080. Task is complete.",
		turn1ToolCallID: idPrefix + "_read_001",
		turn2ToolCallID: idPrefix + "_write_002",
	}
}

func (h *mimoTurnMockHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.turnCount++
	turn := h.turnCount
	h.mu.Unlock()

	bodyStr := h.mock.LastChatBody()
	var req map[string]any
	if err := json.Unmarshal([]byte(bodyStr), &req); err != nil {
		h.t.Fatalf("turn %d: failed to unmarshal upstream request JSON: %v (raw body: %s)", turn, err, bodyStr)
	}

	msgs, ok := req["messages"].([]any)
	if !ok || len(msgs) == 0 {
		h.t.Fatalf("turn %d: missing messages array in upstream request", turn)
	}

	userMsgs := findMessagesByRole(msgs, "user")
	asstMsgs := findMessagesByRole(msgs, "assistant")
	toolMsgs := findMessagesByRole(msgs, "tool")

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	switch turn {
	case 1:
		if len(userMsgs) == 0 {
			h.t.Fatalf("turn 1: no user message found in upstream request")
		}
		// SSE stream response
		stream := buildSSEChunk("chatcmpl-turn1", mimoModelID, map[string]any{
			"role":              "assistant",
			"reasoning_content": h.turn1Reasoning,
		}, nil) +
			buildSSEChunk("chatcmpl-turn1", mimoModelID, map[string]any{
				"tool_calls": []any{
					map[string]any{
						"index": 0,
						"id":    h.turn1ToolCallID,
						"type":  "function",
						"function": map[string]any{
							"name":      "read_file",
							"arguments": `{"path":"config.json"}`,
						},
					},
				},
			}, nil) +
			buildSSEChunk("chatcmpl-turn1", mimoModelID, map[string]any{}, "tool_calls") +
			"data: [DONE]\n\n"

		_, _ = io.WriteString(w, stream)

	case 2:
		// Turn 2: Agent submitted tool result.
		// Verify Turn 1 assistant message restored reasoning_content and kept content: null.
		if len(asstMsgs) < 1 {
			h.t.Fatalf("turn 2: expected at least 1 assistant message, got %d: %#v", len(asstMsgs), msgs)
		}
		if len(toolMsgs) < 1 {
			h.t.Fatalf("turn 2: expected at least 1 tool message, got %d: %#v", len(toolMsgs), msgs)
		}

		asst1 := asstMsgs[0]

		// INVARIANT 1: reasoning_content must be restored from cache
		rc, _ := asst1["reasoning_content"].(string)
		if rc != h.turn1Reasoning {
			h.t.Errorf("turn 2: assistant reasoning_content = %q, want restored %q", rc, h.turn1Reasoning)
		}

		// INVARIANT 2: content must be explicit nil / null
		if cVal, exists := asst1["content"]; !exists || cVal != nil {
			h.t.Errorf("turn 2: assistant content = %#v, want explicit null", cVal)
		}

		// Verify tool message
		toolMsg := toolMsgs[0]
		if toolMsg["tool_call_id"] != h.turn1ToolCallID {
			h.t.Errorf("turn 2: tool_call_id = %v, want %s", toolMsg["tool_call_id"], h.turn1ToolCallID)
		}

		// SSE stream response for Turn 2: reasoning + tool_call `write_file`
		writeArgs, _ := json.Marshal(map[string]any{
			"path":    "config.json",
			"content": "{\n  \"port\": 8080,\n  \"host\": \"127.0.0.1\"\n}",
		})

		stream := buildSSEChunk("chatcmpl-turn2", mimoModelID, map[string]any{
			"role":              "assistant",
			"reasoning_content": h.turn2Reasoning,
		}, nil) +
			buildSSEChunk("chatcmpl-turn2", mimoModelID, map[string]any{
				"tool_calls": []any{
					map[string]any{
						"index": 0,
						"id":    h.turn2ToolCallID,
						"type":  "function",
						"function": map[string]any{
							"name":      "write_file",
							"arguments": string(writeArgs),
						},
					},
				},
			}, nil) +
			buildSSEChunk("chatcmpl-turn2", mimoModelID, map[string]any{}, "tool_calls") +
			"data: [DONE]\n\n"

		_, _ = io.WriteString(w, stream)

	case 3:
		// Turn 3: Agent submitted second tool result.
		// Verify both assistant turns have restored reasoning_content and content: null.
		if len(asstMsgs) < 2 {
			h.t.Fatalf("turn 3: expected at least 2 assistant messages, got %d: %#v", len(asstMsgs), msgs)
		}
		if len(toolMsgs) < 2 {
			h.t.Fatalf("turn 3: expected at least 2 tool messages, got %d: %#v", len(toolMsgs), msgs)
		}

		// Check Assistant 1
		asst1 := asstMsgs[0]
		if rc, _ := asst1["reasoning_content"].(string); rc != h.turn1Reasoning {
			h.t.Errorf("turn 3: asst1 reasoning_content = %q, want %q", rc, h.turn1Reasoning)
		}
		if cVal, exists := asst1["content"]; !exists || cVal != nil {
			h.t.Errorf("turn 3: asst1 content = %#v, want null", cVal)
		}

		// Check Assistant 2
		asst2 := asstMsgs[1]
		if rc, _ := asst2["reasoning_content"].(string); rc != h.turn2Reasoning {
			h.t.Errorf("turn 3: asst2 reasoning_content = %q, want %q", rc, h.turn2Reasoning)
		}
		if cVal, exists := asst2["content"]; !exists || cVal != nil {
			h.t.Errorf("turn 3: asst2 content = %#v, want null", cVal)
		}

		// Check Tool 2
		toolMsg2 := toolMsgs[1]
		if toolMsg2["tool_call_id"] != h.turn2ToolCallID {
			h.t.Errorf("turn 3: tool_call_id = %v, want %s", toolMsg2["tool_call_id"], h.turn2ToolCallID)
		}

		// SSE stream response for Turn 3: reasoning + final content
		stream := buildSSEChunk("chatcmpl-turn3", mimoModelID, map[string]any{
			"role":              "assistant",
			"reasoning_content": h.turn3Reasoning,
		}, nil) +
			buildSSEChunk("chatcmpl-turn3", mimoModelID, map[string]any{
				"content": "Successfully updated port to 8080.",
			}, nil) +
			buildSSEChunk("chatcmpl-turn3", mimoModelID, map[string]any{}, "stop") +
			"data: [DONE]\n\n"

		_, _ = io.WriteString(w, stream)
	}
}

// openAIParsedResponse holds the accumulated output of an OpenAI turn.
type openAIParsedResponse struct {
	ReasoningContent string
	Content          string
	ToolCalls        []struct {
		ID        string
		Name      string
		Arguments string
	}
	FinishReason string
}

func parseOpenAISSE(sseData []byte) (*openAIParsedResponse, error) {
	scanner := bufio.NewScanner(bytes.NewReader(sseData))
	res := &openAIParsedResponse{}
	toolMap := make(map[int]*struct {
		id   string
		name string
		args string
	})

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		choices, ok := chunk["choices"].([]any)
		if !ok || len(choices) == 0 {
			continue
		}
		choice, ok := choices[0].(map[string]any)
		if !ok {
			continue
		}
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			res.FinishReason = fr
		}
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			continue
		}
		if rc, ok := delta["reasoning_content"].(string); ok {
			res.ReasoningContent += rc
		} else if r, ok := delta["reasoning"].(string); ok {
			res.ReasoningContent += r
		}
		if c, ok := delta["content"].(string); ok {
			res.Content += c
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, tcRaw := range tcs {
				tc, ok := tcRaw.(map[string]any)
				if !ok {
					continue
				}
				idx := 0
				if f, ok := tc["index"].(float64); ok {
					idx = int(f)
				}
				ts, ok := toolMap[idx]
				if !ok {
					ts = &struct {
						id   string
						name string
						args string
					}{}
					toolMap[idx] = ts
				}
				if id, ok := tc["id"].(string); ok && id != "" {
					ts.id = id
				}
				if fn, ok := tc["function"].(map[string]any); ok {
					if name, ok := fn["name"].(string); ok && name != "" {
						ts.name = name
					}
					if args, ok := fn["arguments"].(string); ok {
						ts.args += args
					}
				}
			}
		}
	}
	for i := range len(toolMap) {
		if ts, ok := toolMap[i]; ok {
			res.ToolCalls = append(res.ToolCalls, struct {
				ID        string
				Name      string
				Arguments string
			}{
				ID:        ts.id,
				Name:      ts.name,
				Arguments: ts.args,
			})
		}
	}
	return res, nil
}

// anthropicParsedResponse holds the accumulated output of an Anthropic turn.
type anthropicParsedResponse struct {
	Thinking  string
	Text      string
	ToolCalls []struct {
		ID    string
		Name  string
		Input map[string]any
	}
	StopReason string
}

func parseAnthropicSSE(sseData []byte) (*anthropicParsedResponse, error) {
	scanner := bufio.NewScanner(bytes.NewReader(sseData))
	res := &anthropicParsedResponse{}
	toolMap := make(map[int]*struct {
		id   string
		name string
		args string
	})

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		evType, _ := ev["type"].(string)
		switch evType {
		case "content_block_start":
			idx := int(ev["index"].(float64))
			cb, _ := ev["content_block"].(map[string]any)
			cbType, _ := cb["type"].(string)
			if cbType == "tool_use" {
				id, _ := cb["id"].(string)
				name, _ := cb["name"].(string)
				toolMap[idx] = &struct {
					id   string
					name string
					args string
				}{id: id, name: name}
			}
		case "content_block_delta":
			idx := int(ev["index"].(float64))
			delta, _ := ev["delta"].(map[string]any)
			dType, _ := delta["type"].(string)
			switch dType {
			case "thinking_delta":
				if th, ok := delta["thinking"].(string); ok {
					res.Thinking += th
				}
			case "text_delta":
				if txt, ok := delta["text"].(string); ok {
					res.Text += txt
				}
			case "input_json_delta":
				if pj, ok := delta["partial_json"].(string); ok {
					if ts, ok := toolMap[idx]; ok {
						ts.args += pj
					}
				}
			}
		case "message_delta":
			delta, _ := ev["delta"].(map[string]any)
			if sr, ok := delta["stop_reason"].(string); ok {
				res.StopReason = sr
			}
		}
	}
	for i := range 20 {
		if ts, ok := toolMap[i]; ok {
			var inputMap map[string]any
			_ = json.Unmarshal([]byte(ts.args), &inputMap)
			res.ToolCalls = append(res.ToolCalls, struct {
				ID    string
				Name  string
				Input map[string]any
			}{
				ID:    ts.id,
				Name:  ts.name,
				Input: inputMap,
			})
		}
	}
	return res, nil
}

func TestAgenticMiMoE2E(t *testing.T) {
	t.Run("OpenAI_NonStreaming", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()

		handler := newMimoTurnMockHandler(t, mock, "call_openai_ns")
		mock.ChatHandler = handler.ServeHTTP

		srv := newServer(t, mock, nil)
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)

		ws := newMockAgentWorkspace()
		tools := openAIToolDefs()

		// Conversation state maintained by the agent CLI
		var messages []map[string]any
		messages = append(messages, map[string]any{
			"role":    "user",
			"content": "Inspect config.json and update the port to 8080",
		})

		// --- TURN 1 ---
		req1 := map[string]any{
			"model":    mimoModelID,
			"messages": messages,
			"tools":    tools,
			"stream":   false,
		}
		body1, _ := json.Marshal(req1)
		resp1, data1 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", body1, nil)
		if resp1.StatusCode != http.StatusOK {
			t.Fatalf("Turn 1 failed: %d: %s", resp1.StatusCode, data1)
		}

		var comp1 map[string]any
		if err := json.Unmarshal(data1, &comp1); err != nil {
			t.Fatalf("unmarshal Turn 1 response: %v", err)
		}

		choices1 := comp1["choices"].([]any)
		msg1 := choices1[0].(map[string]any)["message"].(map[string]any)
		tcs1, ok := msg1["tool_calls"].([]any)
		if !ok || len(tcs1) == 0 {
			t.Fatalf("Turn 1: expected tool_calls in response, got %#v", msg1)
		}
		tc1 := tcs1[0].(map[string]any)
		fn1 := tc1["function"].(map[string]any)
		if fn1["name"] != "read_file" {
			t.Fatalf("Turn 1: expected function read_file, got %v", fn1["name"])
		}

		// Agent executes read_file locally
		var fn1Args map[string]any
		_ = json.Unmarshal([]byte(fn1["arguments"].(string)), &fn1Args)
		readResult, err := ws.ReadFile(fn1Args["path"].(string))
		if err != nil {
			t.Fatalf("agent read_file failed: %v", err)
		}

		// --- TURN 2 ---
		// Agent appends assistant message (with reasoning_content STRIPPED, content: null)
		messages = append(messages, map[string]any{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": tcs1,
		})
		// Agent appends tool result
		messages = append(messages, map[string]any{
			"role":         "tool",
			"tool_call_id": tc1["id"],
			"content":      readResult,
		})

		req2 := map[string]any{
			"model":    mimoModelID,
			"messages": messages,
			"tools":    tools,
			"stream":   false,
		}
		body2, _ := json.Marshal(req2)
		resp2, data2 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", body2, nil)
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("Turn 2 failed: %d: %s", resp2.StatusCode, data2)
		}

		var comp2 map[string]any
		if err := json.Unmarshal(data2, &comp2); err != nil {
			t.Fatalf("unmarshal Turn 2 response: %v", err)
		}

		choices2 := comp2["choices"].([]any)
		msg2 := choices2[0].(map[string]any)["message"].(map[string]any)
		tcs2, ok := msg2["tool_calls"].([]any)
		if !ok || len(tcs2) == 0 {
			t.Fatalf("Turn 2: expected tool_calls in response, got %#v", msg2)
		}
		tc2 := tcs2[0].(map[string]any)
		fn2 := tc2["function"].(map[string]any)
		if fn2["name"] != "write_file" {
			t.Fatalf("Turn 2: expected function write_file, got %v", fn2["name"])
		}

		// Agent executes write_file locally
		var fn2Args map[string]any
		_ = json.Unmarshal([]byte(fn2["arguments"].(string)), &fn2Args)
		writeResult, err := ws.WriteFile(fn2Args["path"].(string), fn2Args["content"].(string))
		if err != nil {
			t.Fatalf("agent write_file failed: %v", err)
		}

		// --- TURN 3 ---
		// Agent appends assistant message (with reasoning_content STRIPPED, content: null)
		messages = append(messages, map[string]any{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": tcs2,
		})
		// Agent appends tool result
		messages = append(messages, map[string]any{
			"role":         "tool",
			"tool_call_id": tc2["id"],
			"content":      writeResult,
		})

		req3 := map[string]any{
			"model":    mimoModelID,
			"messages": messages,
			"tools":    tools,
			"stream":   false,
		}
		body3, _ := json.Marshal(req3)
		resp3, data3 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", body3, nil)
		if resp3.StatusCode != http.StatusOK {
			t.Fatalf("Turn 3 failed: %d: %s", resp3.StatusCode, data3)
		}

		var comp3 map[string]any
		if err := json.Unmarshal(data3, &comp3); err != nil {
			t.Fatalf("unmarshal Turn 3 response: %v", err)
		}

		choices3 := comp3["choices"].([]any)
		msg3 := choices3[0].(map[string]any)["message"].(map[string]any)
		finalContent, _ := msg3["content"].(string)
		if !strings.Contains(finalContent, "Successfully updated port to 8080.") {
			t.Fatalf("Turn 3: expected completion message, got %q", finalContent)
		}

		// Verify workspace updated
		updatedFile, _ := ws.ReadFile("config.json")
		if !strings.Contains(updatedFile, "8080") {
			t.Fatalf("workspace config.json was not updated: %s", updatedFile)
		}
	})

	t.Run("OpenAI_Streaming", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()

		handler := newMimoTurnMockHandler(t, mock, "call_openai_stream")
		mock.ChatHandler = handler.ServeHTTP

		srv := newServer(t, mock, nil)
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)

		ws := newMockAgentWorkspace()
		tools := openAIToolDefs()

		var messages []map[string]any
		messages = append(messages, map[string]any{
			"role":    "user",
			"content": "Inspect config.json and update the port to 8080",
		})

		// --- TURN 1 (Streaming) ---
		req1 := map[string]any{
			"model":    mimoModelID,
			"messages": messages,
			"tools":    tools,
			"stream":   true,
		}
		body1, _ := json.Marshal(req1)
		resp1, data1 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", body1, nil)
		if resp1.StatusCode != http.StatusOK {
			t.Fatalf("Turn 1 stream failed: %d: %s", resp1.StatusCode, data1)
		}

		parsed1, err := parseOpenAISSE(data1)
		if err != nil {
			t.Fatalf("parse Turn 1 SSE: %v", err)
		}
		if len(parsed1.ToolCalls) == 0 || parsed1.ToolCalls[0].Name != "read_file" {
			t.Fatalf("Turn 1: expected read_file tool call, got %#v", parsed1.ToolCalls)
		}
		if parsed1.FinishReason != "tool_calls" {
			t.Fatalf("Turn 1: expected finish_reason tool_calls, got %q", parsed1.FinishReason)
		}

		// Execute read_file
		var fn1Args map[string]any
		_ = json.Unmarshal([]byte(parsed1.ToolCalls[0].Arguments), &fn1Args)
		readResult, err := ws.ReadFile(fn1Args["path"].(string))
		if err != nil {
			t.Fatalf("agent read_file failed: %v", err)
		}

		// --- TURN 2 (Streaming) ---
		// Replay Turn 1 assistant tool call with reasoning_content stripped and content: null
		messages = append(messages, map[string]any{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []map[string]any{
				{
					"id":   parsed1.ToolCalls[0].ID,
					"type": "function",
					"function": map[string]any{
						"name":      parsed1.ToolCalls[0].Name,
						"arguments": parsed1.ToolCalls[0].Arguments,
					},
				},
			},
		})
		messages = append(messages, map[string]any{
			"role":         "tool",
			"tool_call_id": parsed1.ToolCalls[0].ID,
			"content":      readResult,
		})

		req2 := map[string]any{
			"model":    mimoModelID,
			"messages": messages,
			"tools":    tools,
			"stream":   true,
		}
		body2, _ := json.Marshal(req2)
		resp2, data2 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", body2, nil)
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("Turn 2 stream failed: %d: %s", resp2.StatusCode, data2)
		}

		parsed2, err := parseOpenAISSE(data2)
		if err != nil {
			t.Fatalf("parse Turn 2 SSE: %v", err)
		}
		if len(parsed2.ToolCalls) == 0 || parsed2.ToolCalls[0].Name != "write_file" {
			t.Fatalf("Turn 2: expected write_file tool call, got %#v", parsed2.ToolCalls)
		}

		// Execute write_file
		var fn2Args map[string]any
		_ = json.Unmarshal([]byte(parsed2.ToolCalls[0].Arguments), &fn2Args)
		writeResult, err := ws.WriteFile(fn2Args["path"].(string), fn2Args["content"].(string))
		if err != nil {
			t.Fatalf("agent write_file failed: %v", err)
		}

		// --- TURN 3 (Streaming) ---
		messages = append(messages, map[string]any{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []map[string]any{
				{
					"id":   parsed2.ToolCalls[0].ID,
					"type": "function",
					"function": map[string]any{
						"name":      parsed2.ToolCalls[0].Name,
						"arguments": parsed2.ToolCalls[0].Arguments,
					},
				},
			},
		})
		messages = append(messages, map[string]any{
			"role":         "tool",
			"tool_call_id": parsed2.ToolCalls[0].ID,
			"content":      writeResult,
		})

		req3 := map[string]any{
			"model":    mimoModelID,
			"messages": messages,
			"tools":    tools,
			"stream":   true,
		}
		body3, _ := json.Marshal(req3)
		resp3, data3 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", body3, nil)
		if resp3.StatusCode != http.StatusOK {
			t.Fatalf("Turn 3 stream failed: %d: %s", resp3.StatusCode, data3)
		}

		parsed3, err := parseOpenAISSE(data3)
		if err != nil {
			t.Fatalf("parse Turn 3 SSE: %v", err)
		}
		if !strings.Contains(parsed3.Content, "Successfully updated port to 8080.") {
			t.Fatalf("Turn 3: expected final message, got %q", parsed3.Content)
		}
		if parsed3.FinishReason != "stop" {
			t.Fatalf("Turn 3: expected finish_reason stop, got %q", parsed3.FinishReason)
		}

		// Verify workspace updated
		updatedFile, _ := ws.ReadFile("config.json")
		if !strings.Contains(updatedFile, "8080") {
			t.Fatalf("workspace config.json was not updated: %s", updatedFile)
		}
	})

	t.Run("Anthropic_NonStreaming", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()

		handler := newMimoTurnMockHandler(t, mock, "toolu_anthropic_ns")
		mock.ChatHandler = handler.ServeHTTP

		srv := newServer(t, mock, nil)
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)

		ws := newMockAgentWorkspace()
		tools := anthropicToolDefs()
		headers := map[string]string{
			"x-api-key":         "claude-code-test-key",
			"anthropic-version": "2023-06-01",
		}

		var messages []map[string]any
		messages = append(messages, map[string]any{
			"role":    "user",
			"content": "Inspect config.json and update the port to 8080",
		})

		// --- TURN 1 ---
		req1 := map[string]any{
			"model":      mimoModelID,
			"max_tokens": 4096,
			"messages":   messages,
			"tools":      tools,
			"stream":     false,
		}
		body1, _ := json.Marshal(req1)
		resp1, data1 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/messages", body1, headers)
		if resp1.StatusCode != http.StatusOK {
			t.Fatalf("Anthropic Turn 1 failed: %d: %s", resp1.StatusCode, data1)
		}

		var antResp1 map[string]any
		if err := json.Unmarshal(data1, &antResp1); err != nil {
			t.Fatalf("unmarshal Anthropic Turn 1: %v", err)
		}

		contentBlocks1 := antResp1["content"].([]any)
		var toolUseBlock1 map[string]any
		for _, b := range contentBlocks1 {
			bm := b.(map[string]any)
			if bm["type"] == "tool_use" {
				toolUseBlock1 = bm
				break
			}
		}
		if toolUseBlock1 == nil || toolUseBlock1["name"] != "read_file" {
			t.Fatalf("Anthropic Turn 1: expected read_file tool_use, got %#v", contentBlocks1)
		}

		input1 := toolUseBlock1["input"].(map[string]any)
		readResult, err := ws.ReadFile(input1["path"].(string))
		if err != nil {
			t.Fatalf("agent read_file failed: %v", err)
		}

		// --- TURN 2 ---
		// Agent appends assistant tool_use turn (thinking block stripped/omitted)
		messages = append(messages, map[string]any{
			"role": "assistant",
			"content": []any{
				toolUseBlock1,
			},
		})
		// Agent appends user tool_result turn
		messages = append(messages, map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": toolUseBlock1["id"],
					"content":     readResult,
				},
			},
		})

		req2 := map[string]any{
			"model":      mimoModelID,
			"max_tokens": 4096,
			"messages":   messages,
			"tools":      tools,
			"stream":     false,
		}
		body2, _ := json.Marshal(req2)
		resp2, data2 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/messages", body2, headers)
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("Anthropic Turn 2 failed: %d: %s", resp2.StatusCode, data2)
		}

		var antResp2 map[string]any
		if err := json.Unmarshal(data2, &antResp2); err != nil {
			t.Fatalf("unmarshal Anthropic Turn 2: %v", err)
		}

		contentBlocks2 := antResp2["content"].([]any)
		var toolUseBlock2 map[string]any
		for _, b := range contentBlocks2 {
			bm := b.(map[string]any)
			if bm["type"] == "tool_use" {
				toolUseBlock2 = bm
				break
			}
		}
		if toolUseBlock2 == nil || toolUseBlock2["name"] != "write_file" {
			t.Fatalf("Anthropic Turn 2: expected write_file tool_use, got %#v", contentBlocks2)
		}

		input2 := toolUseBlock2["input"].(map[string]any)
		writeResult, err := ws.WriteFile(input2["path"].(string), input2["content"].(string))
		if err != nil {
			t.Fatalf("agent write_file failed: %v", err)
		}

		// --- TURN 3 ---
		messages = append(messages, map[string]any{
			"role": "assistant",
			"content": []any{
				toolUseBlock2,
			},
		})
		messages = append(messages, map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": toolUseBlock2["id"],
					"content":     writeResult,
				},
			},
		})

		req3 := map[string]any{
			"model":      mimoModelID,
			"max_tokens": 4096,
			"messages":   messages,
			"tools":      tools,
			"stream":     false,
		}
		body3, _ := json.Marshal(req3)
		resp3, data3 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/messages", body3, headers)
		if resp3.StatusCode != http.StatusOK {
			t.Fatalf("Anthropic Turn 3 failed: %d: %s", resp3.StatusCode, data3)
		}

		var antResp3 map[string]any
		if err := json.Unmarshal(data3, &antResp3); err != nil {
			t.Fatalf("unmarshal Anthropic Turn 3: %v", err)
		}

		contentBlocks3 := antResp3["content"].([]any)
		var textBlock3 string
		for _, b := range contentBlocks3 {
			bm := b.(map[string]any)
			if bm["type"] == "text" {
				textBlock3, _ = bm["text"].(string)
				break
			}
		}
		if !strings.Contains(textBlock3, "Successfully updated port to 8080.") {
			t.Fatalf("Anthropic Turn 3: expected final text message, got %#v", contentBlocks3)
		}

		// Verify workspace updated
		updatedFile, _ := ws.ReadFile("config.json")
		if !strings.Contains(updatedFile, "8080") {
			t.Fatalf("workspace config.json was not updated: %s", updatedFile)
		}
	})

	t.Run("Anthropic_Streaming", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()

		handler := newMimoTurnMockHandler(t, mock, "toolu_anthropic_stream")
		mock.ChatHandler = handler.ServeHTTP

		srv := newServer(t, mock, nil)
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)

		ws := newMockAgentWorkspace()
		tools := anthropicToolDefs()
		headers := map[string]string{
			"x-api-key":         "claude-code-test-key",
			"anthropic-version": "2023-06-01",
		}

		var messages []map[string]any
		messages = append(messages, map[string]any{
			"role":    "user",
			"content": "Inspect config.json and update the port to 8080",
		})

		// --- TURN 1 (Streaming) ---
		req1 := map[string]any{
			"model":      mimoModelID,
			"max_tokens": 4096,
			"messages":   messages,
			"tools":      tools,
			"stream":     true,
		}
		body1, _ := json.Marshal(req1)
		resp1, data1 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/messages", body1, headers)
		if resp1.StatusCode != http.StatusOK {
			t.Fatalf("Anthropic Turn 1 stream failed: %d: %s", resp1.StatusCode, data1)
		}

		parsed1, err := parseAnthropicSSE(data1)
		if err != nil {
			t.Fatalf("parse Anthropic Turn 1 SSE: %v", err)
		}
		if len(parsed1.ToolCalls) == 0 || parsed1.ToolCalls[0].Name != "read_file" {
			t.Fatalf("Anthropic Turn 1: expected read_file tool_use, got %#v", parsed1.ToolCalls)
		}
		if parsed1.StopReason != "tool_use" {
			t.Fatalf("Anthropic Turn 1: expected stop_reason tool_use, got %q", parsed1.StopReason)
		}

		readResult, err := ws.ReadFile(parsed1.ToolCalls[0].Input["path"].(string))
		if err != nil {
			t.Fatalf("agent read_file failed: %v", err)
		}

		// --- TURN 2 (Streaming) ---
		messages = append(messages, map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type":  "tool_use",
					"id":    parsed1.ToolCalls[0].ID,
					"name":  parsed1.ToolCalls[0].Name,
					"input": parsed1.ToolCalls[0].Input,
				},
			},
		})
		messages = append(messages, map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": parsed1.ToolCalls[0].ID,
					"content":     readResult,
				},
			},
		})

		req2 := map[string]any{
			"model":      mimoModelID,
			"max_tokens": 4096,
			"messages":   messages,
			"tools":      tools,
			"stream":     true,
		}
		body2, _ := json.Marshal(req2)
		resp2, data2 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/messages", body2, headers)
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("Anthropic Turn 2 stream failed: %d: %s", resp2.StatusCode, data2)
		}

		parsed2, err := parseAnthropicSSE(data2)
		if err != nil {
			t.Fatalf("parse Anthropic Turn 2 SSE: %v", err)
		}
		if len(parsed2.ToolCalls) == 0 || parsed2.ToolCalls[0].Name != "write_file" {
			t.Fatalf("Anthropic Turn 2: expected write_file tool_use, got %#v", parsed2.ToolCalls)
		}

		writeResult, err := ws.WriteFile(parsed2.ToolCalls[0].Input["path"].(string), parsed2.ToolCalls[0].Input["content"].(string))
		if err != nil {
			t.Fatalf("agent write_file failed: %v", err)
		}

		// --- TURN 3 (Streaming) ---
		messages = append(messages, map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type":  "tool_use",
					"id":    parsed2.ToolCalls[0].ID,
					"name":  parsed2.ToolCalls[0].Name,
					"input": parsed2.ToolCalls[0].Input,
				},
			},
		})
		messages = append(messages, map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": parsed2.ToolCalls[0].ID,
					"content":     writeResult,
				},
			},
		})

		req3 := map[string]any{
			"model":      mimoModelID,
			"max_tokens": 4096,
			"messages":   messages,
			"tools":      tools,
			"stream":     true,
		}
		body3, _ := json.Marshal(req3)
		resp3, data3 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/messages", body3, headers)
		if resp3.StatusCode != http.StatusOK {
			t.Fatalf("Anthropic Turn 3 stream failed: %d: %s", resp3.StatusCode, data3)
		}

		parsed3, err := parseAnthropicSSE(data3)
		if err != nil {
			t.Fatalf("parse Anthropic Turn 3 SSE: %v", err)
		}
		if !strings.Contains(parsed3.Text, "Successfully updated port to 8080.") {
			t.Fatalf("Anthropic Turn 3: expected final text, got %q", parsed3.Text)
		}
		if parsed3.StopReason != "end_turn" {
			t.Fatalf("Anthropic Turn 3: expected stop_reason end_turn, got %q", parsed3.StopReason)
		}

		// Verify workspace updated
		updatedFile, _ := ws.ReadFile("config.json")
		if !strings.Contains(updatedFile, "8080") {
			t.Fatalf("workspace config.json was not updated: %s", updatedFile)
		}
	})

	t.Run("ExecuteBash_ToolCall", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()

		ws := newMockAgentWorkspace()
		ws.Files["main.go"] = "package main\nfunc main() {}\n"

		toolCallID := "call_bash_001"
		reasoning1 := "MiMo Thinking: I will list the files in the directory using execute_bash."
		reasoning2 := "MiMo Thinking: I see config.json and main.go. Task complete."

		turnCount := 0
		mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
			turnCount++
			bodyStr := mock.LastChatBody()
			var upReq map[string]any
			_ = json.Unmarshal([]byte(bodyStr), &upReq)

			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)

			switch turnCount {
			case 1:
				stream := buildSSEChunk("chatcmpl-b1", mimoModelID, map[string]any{
					"role":              "assistant",
					"reasoning_content": reasoning1,
				}, nil) +
					buildSSEChunk("chatcmpl-b1", mimoModelID, map[string]any{
						"tool_calls": []any{
							map[string]any{
								"index": 0,
								"id":    toolCallID,
								"type":  "function",
								"function": map[string]any{
									"name":      "execute_bash",
									"arguments": `{"command":"ls -la"}`,
								},
							},
						},
					}, nil) +
					buildSSEChunk("chatcmpl-b1", mimoModelID, map[string]any{}, "tool_calls") +
					"data: [DONE]\n\n"
				_, _ = io.WriteString(w, stream)
			case 2:
				// Verify restored reasoning and content: null
				msgs := upReq["messages"].([]any)
				asstMsgs := findMessagesByRole(msgs, "assistant")
				if len(asstMsgs) == 0 {
					t.Fatalf("bash test turn 2: no assistant message found")
				}
				asst := asstMsgs[0]
				if rc, _ := asst["reasoning_content"].(string); rc != reasoning1 {
					t.Errorf("bash test turn 2: reasoning_content = %q, want %q", rc, reasoning1)
				}
				if asst["content"] != nil {
					t.Errorf("bash test turn 2: assistant content = %v, want null", asst["content"])
				}
				stream := buildSSEChunk("chatcmpl-b2", mimoModelID, map[string]any{
					"role":              "assistant",
					"reasoning_content": reasoning2,
				}, nil) +
					buildSSEChunk("chatcmpl-b2", mimoModelID, map[string]any{
						"content": "Directory contains config.json and main.go.",
					}, nil) +
					buildSSEChunk("chatcmpl-b2", mimoModelID, map[string]any{}, "stop") +
					"data: [DONE]\n\n"
				_, _ = io.WriteString(w, stream)
			}
		}

		srv := newServer(t, mock, nil)
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)

		// Turn 1
		req1 := map[string]any{
			"model": mimoModelID,
			"messages": []map[string]any{
				{"role": "user", "content": "List files in workspace"},
			},
			"tools":  openAIToolDefs(),
			"stream": false,
		}
		body1, _ := json.Marshal(req1)
		resp1, data1 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", body1, nil)
		if resp1.StatusCode != http.StatusOK {
			t.Fatalf("Turn 1 failed: %d: %s", resp1.StatusCode, data1)
		}

		var comp1 map[string]any
		_ = json.Unmarshal(data1, &comp1)
		tc := comp1["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
		cmd := tc["function"].(map[string]any)["arguments"].(string)

		var cmdArgs map[string]any
		_ = json.Unmarshal([]byte(cmd), &cmdArgs)
		bashOutput, err := ws.ExecuteBash(cmdArgs["command"].(string))
		if err != nil {
			t.Fatalf("execute_bash failed: %v", err)
		}

		// Turn 2
		req2 := map[string]any{
			"model": mimoModelID,
			"messages": []map[string]any{
				{"role": "user", "content": "List files in workspace"},
				{
					"role":       "assistant",
					"content":    nil,
					"tool_calls": []any{tc},
				},
				{
					"role":         "tool",
					"tool_call_id": tc["id"],
					"content":      bashOutput,
				},
			},
			"tools":  openAIToolDefs(),
			"stream": false,
		}
		body2, _ := json.Marshal(req2)
		resp2, data2 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", body2, nil)
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("Turn 2 failed: %d: %s", resp2.StatusCode, data2)
		}

		var comp2 map[string]any
		_ = json.Unmarshal(data2, &comp2)
		finalMsg := comp2["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
		if !strings.Contains(finalMsg, "Directory contains config.json and main.go.") {
			t.Fatalf("expected bash final message, got %q", finalMsg)
		}
	})
}
