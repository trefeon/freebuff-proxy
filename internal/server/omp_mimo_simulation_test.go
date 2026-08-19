package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"freebuff-proxy/internal/convert"
	"freebuff-proxy/internal/reasoningcache"
	"freebuff-proxy/internal/testutil"
)

// ompWorkspace simulates the local filesystem and tool runner of the Oh My Pi (omp) harness.
type ompWorkspace struct {
	mu    sync.Mutex
	Files map[string]string
}

func newOmpWorkspace() *ompWorkspace {
	return &ompWorkspace{
		Files: map[string]string{
			"src/main.go": "package main\n\nconst Port = 8080\n\nfunc main() {}\n",
		},
	}
}

func (w *ompWorkspace) Glob(pattern string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var matches []string
	for p := range w.Files {
		if strings.HasPrefix(p, "src/") && strings.HasSuffix(p, ".go") {
			matches = append(matches, p)
		}
	}
	if len(matches) == 0 {
		return "", nil
	}
	return strings.Join(matches, "\n"), nil
}

func (w *ompWorkspace) Read(path string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	content, ok := w.Files[path]
	if !ok {
		return "", fmt.Errorf("file not found: %s", path)
	}
	return content, nil
}

func (w *ompWorkspace) Edit(input string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	// Line-anchored patch language simulation:
	// If input contains replacement for Port, apply it to src/main.go
	if strings.Contains(input, "Port = 9090") {
		if content, ok := w.Files["src/main.go"]; ok {
			w.Files["src/main.go"] = strings.Replace(content, "const Port = 8080", "const Port = 9090", 1)
			return "[main.go#1A2B] updated", nil
		}
	}
	return "[main.go#1A2B] ok", nil
}

func (w *ompWorkspace) Write(path, content string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.Files[path] = content
	return fmt.Sprintf("wrote %d bytes to %s", len(content), path), nil
}

func (w *ompWorkspace) Bash(command string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return fmt.Sprintf("executed: %s", command), nil
}

func (w *ompWorkspace) Grep(pattern, path string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return fmt.Sprintf("matches for %s in %s", pattern, path), nil
}

func (w *ompWorkspace) Todo(op, task, phase string, list []string) (string, error) {
	return fmt.Sprintf("todo %s ok", op), nil
}

// ompOpenAIToolDefs returns standard OpenAI function declarations for the 7 omp tools:
// read, bash, write, edit, grep, glob, todo.
func ompOpenAIToolDefs() []map[string]any {
	return []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        "read",
				"description": "Read files, directories, archives, SQLite, images, documents, internal resources, and web URLs via path.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type":        "string",
							"description": "Local path, internal URI, or URL.",
						},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "bash",
				"description": "Runs commands in a persistent shell.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{
							"type":        "string",
							"description": "The command to run.",
						},
						"cwd": map[string]any{
							"type":        "string",
							"description": "Working directory.",
						},
						"env": map[string]any{
							"type":        "object",
							"description": "Environment variables.",
						},
						"timeout": map[string]any{
							"type":        "number",
							"description": "Timeout in seconds.",
						},
					},
					"required": []string{"command"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "write",
				"description": "Creates or overwrites file at specified path.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type":        "string",
							"description": "File path.",
						},
						"content": map[string]any{
							"type":        "string",
							"description": "File content.",
						},
					},
					"required": []string{"path", "content"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "edit",
				"description": "Line-anchored patch language.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"input": map[string]any{
							"type":        "string",
							"description": "Patch input string.",
						},
					},
					"required": []string{"input"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "grep",
				"description": "Searches files/internal URLs with regex pattern.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"pattern": map[string]any{
							"type":        "string",
							"description": "Regex search pattern.",
						},
						"path": map[string]any{
							"type":        "string",
							"description": "File, directory or glob to search.",
						},
					},
					"required": []string{"pattern"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "glob",
				"description": "Globs files and directories.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type":        "string",
							"description": "Glob pattern or directory path.",
						},
						"gitignore": map[string]any{
							"type":        "boolean",
							"description": "Respect gitignore.",
						},
					},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "todo",
				"description": "Task and todo list manager.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"op": map[string]any{
							"type":        "string",
							"description": "Operation: get, set, add, done.",
						},
						"task": map[string]any{
							"type":        "string",
							"description": "Task description.",
						},
						"phase": map[string]any{
							"type":        "string",
							"description": "Phase name.",
						},
						"list": map[string]any{
							"type":        "array",
							"description": "List of tasks.",
							"items": map[string]any{
								"type": "string",
							},
						},
					},
					"required": []string{"op"},
				},
			},
		},
	}
}

// ompAnthropicToolDefs returns Anthropic input_schema declarations for the 7 omp tools.
func ompAnthropicToolDefs() []map[string]any {
	return []map[string]any{
		{
			"name":        "read",
			"description": "Read files, directories, archives, SQLite, images, documents, internal resources, and web URLs via path.",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Local path, internal URI, or URL.",
					},
				},
				"required": []string{"path"},
			},
		},
		{
			"name":        "bash",
			"description": "Runs commands in a persistent shell.",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "The command to run.",
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Working directory.",
					},
					"env": map[string]any{
						"type":        "object",
						"description": "Environment variables.",
					},
					"timeout": map[string]any{
						"type":        "number",
						"description": "Timeout in seconds.",
					},
				},
				"required": []string{"command"},
			},
		},
		{
			"name":        "write",
			"description": "Creates or overwrites file at specified path.",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "File path.",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "File content.",
					},
				},
				"required": []string{"path", "content"},
			},
		},
		{
			"name":        "edit",
			"description": "Line-anchored patch language.",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"input": map[string]any{
						"type":        "string",
						"description": "Patch input string.",
					},
				},
				"required": []string{"input"},
			},
		},
		{
			"name":        "grep",
			"description": "Searches files/internal URLs with regex pattern.",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{
						"type":        "string",
						"description": "Regex search pattern.",
					},
					"path": map[string]any{
						"type":        "string",
						"description": "File, directory or glob to search.",
					},
				},
				"required": []string{"pattern"},
			},
		},
		{
			"name":        "glob",
			"description": "Globs files and directories.",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Glob pattern or directory path.",
					},
					"gitignore": map[string]any{
						"type":        "boolean",
						"description": "Respect gitignore.",
					},
				},
			},
		},
		{
			"name":        "todo",
			"description": "Task and todo list manager.",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"op": map[string]any{
						"type":        "string",
						"description": "Operation: get, set, add, done.",
					},
					"task": map[string]any{
						"type":        "string",
						"description": "Task description.",
					},
					"phase": map[string]any{
						"type":        "string",
						"description": "Phase name.",
					},
					"list": map[string]any{
						"type":        "array",
						"description": "List of tasks.",
						"items": map[string]any{
							"type": "string",
						},
					},
				},
				"required": []string{"op"},
			},
		},
	}
}

// ompMimoMockHandler implements the 4-turn MiMo V2.5 simulation with strict schema checking.
type ompMimoMockHandler struct {
	t    *testing.T
	mock *testutil.MockUpstream
	mu   sync.Mutex

	turnCount int

	turn1Reasoning string
	turn2Reasoning string
	turn3Reasoning string
	turn4Reasoning string

	turn1ToolCallID string
	turn2ToolCallID string
	turn3ToolCallID string
}

func newOmpMimoMockHandler(t *testing.T, mock *testutil.MockUpstream, idPrefix string) *ompMimoMockHandler {
	return &ompMimoMockHandler{
		t:               t,
		mock:            mock,
		turn1Reasoning:  "MiMo Thinking: I need to locate files first. Let me find the entrypoint in src/ using glob.",
		turn2Reasoning:  "MiMo Thinking: Let me read src/main.go to inspect the port configuration.",
		turn3Reasoning:  "MiMo Thinking: The current port is 8080. I will edit Port to 9090 in src/main.go.",
		turn4Reasoning:  "MiMo Thinking: The port was successfully updated to 9090. Task complete.",
		turn1ToolCallID: idPrefix + "_glob_001",
		turn2ToolCallID: idPrefix + "_read_002",
		turn3ToolCallID: idPrefix + "_edit_003",
	}
}

func (h *ompMimoMockHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.turnCount++
	turn := h.turnCount
	h.mu.Unlock()

	bodyStr := h.mock.LastChatBody()
	var req map[string]any
	if err := json.Unmarshal([]byte(bodyStr), &req); err != nil {
		h.t.Fatalf("turn %d: failed to unmarshal upstream request JSON: %v (raw body: %s)", turn, err, bodyStr)
	}

	// 1. Strict Schema Assertion: Model must be mimo/mimo-v2.5
	if req["model"] != mimoModelID {
		h.t.Errorf("turn %d: model = %v, want %q", turn, req["model"], mimoModelID)
	}

	// 2. Strict Schema Assertion: Tools must be present
	tools, ok := req["tools"].([]any)
	if !ok || len(tools) == 0 {
		h.t.Fatalf("turn %d: missing or empty tools array", turn)
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
		// Turn 1: Initial prompt
		if len(userMsgs) == 0 {
			h.t.Fatalf("turn 1: no user message found in upstream request")
		}
		if len(asstMsgs) != 0 {
			h.t.Fatalf("turn 1: expected 0 assistant messages, got %d", len(asstMsgs))
		}

		// Emit reasoning + tool_call `glob(path="src/**/*.go")`
		globArgs, _ := json.Marshal(map[string]any{"path": "src/**/*.go"})
		stream := buildSSEChunk("chatcmpl-omp-1", mimoModelID, map[string]any{
			"role":              "assistant",
			"reasoning_content": h.turn1Reasoning,
		}, nil) +
			buildSSEChunk("chatcmpl-omp-1", mimoModelID, map[string]any{
				"tool_calls": []any{
					map[string]any{
						"index": 0,
						"id":    h.turn1ToolCallID,
						"type":  "function",
						"function": map[string]any{
							"name":      "glob",
							"arguments": string(globArgs),
						},
					},
				},
			}, nil) +
			buildSSEChunk("chatcmpl-omp-1", mimoModelID, map[string]any{}, "tool_calls") +
			"data: [DONE]\n\n"

		_, _ = io.WriteString(w, stream)

	case 2:
		// Turn 2: omp returns glob result ("src/main.go")
		if len(asstMsgs) < 1 {
			h.t.Fatalf("turn 2: expected at least 1 assistant message, got %d", len(asstMsgs))
		}
		if len(toolMsgs) < 1 {
			h.t.Fatalf("turn 2: expected at least 1 tool message, got %d", len(toolMsgs))
		}

		// Verify Turn 1 assistant message has restored reasoning_content and content: null
		asst1 := asstMsgs[0]
		rc1, _ := asst1["reasoning_content"].(string)
		if rc1 != h.turn1Reasoning {
			h.t.Errorf("turn 2: asst1 reasoning_content = %q, want restored %q", rc1, h.turn1Reasoning)
		}
		if cVal, exists := asst1["content"]; !exists || cVal != nil {
			h.t.Errorf("turn 2: asst1 content = %#v, want explicit null", cVal)
		}

		// Verify Tool 1 response
		tool1 := toolMsgs[0]
		if tool1["tool_call_id"] != h.turn1ToolCallID {
			h.t.Errorf("turn 2: tool 1 tool_call_id = %v, want %s", tool1["tool_call_id"], h.turn1ToolCallID)
		}

		// Emit reasoning + tool_call `read(path="src/main.go")`
		readArgs, _ := json.Marshal(map[string]any{"path": "src/main.go"})
		stream := buildSSEChunk("chatcmpl-omp-2", mimoModelID, map[string]any{
			"role":              "assistant",
			"reasoning_content": h.turn2Reasoning,
		}, nil) +
			buildSSEChunk("chatcmpl-omp-2", mimoModelID, map[string]any{
				"tool_calls": []any{
					map[string]any{
						"index": 0,
						"id":    h.turn2ToolCallID,
						"type":  "function",
						"function": map[string]any{
							"name":      "read",
							"arguments": string(readArgs),
						},
					},
				},
			}, nil) +
			buildSSEChunk("chatcmpl-omp-2", mimoModelID, map[string]any{}, "tool_calls") +
			"data: [DONE]\n\n"

		_, _ = io.WriteString(w, stream)

	case 3:
		// Turn 3: omp returns read result ("package main\n\nconst Port = 8080...")
		if len(asstMsgs) < 2 {
			h.t.Fatalf("turn 3: expected at least 2 assistant messages, got %d", len(asstMsgs))
		}
		if len(toolMsgs) < 2 {
			h.t.Fatalf("turn 3: expected at least 2 tool messages, got %d", len(toolMsgs))
		}

		// Check Asst 1
		asst1 := asstMsgs[0]
		if rc, _ := asst1["reasoning_content"].(string); rc != h.turn1Reasoning {
			h.t.Errorf("turn 3: asst1 reasoning_content = %q, want %q", rc, h.turn1Reasoning)
		}
		if cVal, exists := asst1["content"]; !exists || cVal != nil {
			h.t.Errorf("turn 3: asst1 content = %#v, want explicit null", cVal)
		}

		// Check Asst 2
		asst2 := asstMsgs[1]
		if rc, _ := asst2["reasoning_content"].(string); rc != h.turn2Reasoning {
			h.t.Errorf("turn 3: asst2 reasoning_content = %q, want %q", rc, h.turn2Reasoning)
		}
		if cVal, exists := asst2["content"]; !exists || cVal != nil {
			h.t.Errorf("turn 3: asst2 content = %#v, want explicit null", cVal)
		}

		// Check Tool 2
		tool2 := toolMsgs[1]
		if tool2["tool_call_id"] != h.turn2ToolCallID {
			h.t.Errorf("turn 3: tool 2 tool_call_id = %v, want %s", tool2["tool_call_id"], h.turn2ToolCallID)
		}

		// Emit reasoning + tool_call `edit(input="PUT 2.=2:\n+const Port = 9090")`
		editArgs, _ := json.Marshal(map[string]any{"input": "PUT 2.=2:\n+const Port = 9090"})
		stream := buildSSEChunk("chatcmpl-omp-3", mimoModelID, map[string]any{
			"role":              "assistant",
			"reasoning_content": h.turn3Reasoning,
		}, nil) +
			buildSSEChunk("chatcmpl-omp-3", mimoModelID, map[string]any{
				"tool_calls": []any{
					map[string]any{
						"index": 0,
						"id":    h.turn3ToolCallID,
						"type":  "function",
						"function": map[string]any{
							"name":      "edit",
							"arguments": string(editArgs),
						},
					},
				},
			}, nil) +
			buildSSEChunk("chatcmpl-omp-3", mimoModelID, map[string]any{}, "tool_calls") +
			"data: [DONE]\n\n"

		_, _ = io.WriteString(w, stream)

	case 4:
		// Turn 4: omp returns edit result ("[main.go#1A2B] updated")
		if len(asstMsgs) < 3 {
			h.t.Fatalf("turn 4: expected at least 3 assistant messages, got %d", len(asstMsgs))
		}
		if len(toolMsgs) < 3 {
			h.t.Fatalf("turn 4: expected at least 3 tool messages, got %d", len(toolMsgs))
		}

		// Check all 3 prior assistant messages for restored reasoning_content and content: null
		for i, asst := range asstMsgs[:3] {
			var expectedRC string
			switch i {
			case 0:
				expectedRC = h.turn1Reasoning
			case 1:
				expectedRC = h.turn2Reasoning
			case 2:
				expectedRC = h.turn3Reasoning
			}
			if rc, _ := asst["reasoning_content"].(string); rc != expectedRC {
				h.t.Errorf("turn 4: asst %d reasoning_content = %q, want %q", i+1, rc, expectedRC)
			}
			if cVal, exists := asst["content"]; !exists || cVal != nil {
				h.t.Errorf("turn 4: asst %d content = %#v, want explicit null", i+1, cVal)
			}
		}

		// Check Tool 3
		tool3 := toolMsgs[2]
		if tool3["tool_call_id"] != h.turn3ToolCallID {
			h.t.Errorf("turn 4: tool 3 tool_call_id = %v, want %s", tool3["tool_call_id"], h.turn3ToolCallID)
		}

		// Emit final reasoning + completion text
		stream := buildSSEChunk("chatcmpl-omp-4", mimoModelID, map[string]any{
			"role":              "assistant",
			"reasoning_content": h.turn4Reasoning,
		}, nil) +
			buildSSEChunk("chatcmpl-omp-4", mimoModelID, map[string]any{
				"content": "Successfully updated server port to 9090 in src/main.go.",
			}, nil) +
			buildSSEChunk("chatcmpl-omp-4", mimoModelID, map[string]any{}, "stop") +
			"data: [DONE]\n\n"

		_, _ = io.WriteString(w, stream)
	}
}

// TestOmpMiMoSimulation runs the end-to-end 4-turn agentic coding loop simulation for
// the Oh My Pi (omp) harness interacting with mimo/mimo-v2.5 through freebuff-proxy.
func TestOmpMiMoSimulation(t *testing.T) {
	t.Run("OpenAI_NonStreaming", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()

		handler := newOmpMimoMockHandler(t, mock, "call_omp_ns")
		mock.ChatHandler = handler.ServeHTTP

		srv := newServer(t, mock, nil)
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)

		ws := newOmpWorkspace()
		tools := ompOpenAIToolDefs()

		// Conversation state maintained by omp
		var messages []map[string]any
		messages = append(messages, map[string]any{
			"role":    "user",
			"content": "Find the entrypoint in src/, inspect it, and update the server port to 9090.",
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
			t.Fatalf("unmarshal Turn 1: %v", err)
		}

		msg1 := comp1["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		tcs1, ok := msg1["tool_calls"].([]any)
		if !ok || len(tcs1) == 0 {
			t.Fatalf("Turn 1: expected tool_calls in response, got %#v", msg1)
		}
		tc1 := tcs1[0].(map[string]any)
		fn1 := tc1["function"].(map[string]any)
		if fn1["name"] != "glob" {
			t.Fatalf("Turn 1: expected function glob, got %v", fn1["name"])
		}

		// omp executes glob
		var fn1Args map[string]any
		_ = json.Unmarshal([]byte(fn1["arguments"].(string)), &fn1Args)
		globResult, err := ws.Glob(fn1Args["path"].(string))
		if err != nil {
			t.Fatalf("omp glob failed: %v", err)
		}
		if globResult != "src/main.go" {
			t.Fatalf("expected glob output src/main.go, got %q", globResult)
		}

		// --- TURN 2 ---
		// omp appends assistant turn (stripping reasoning_content, content: null)
		messages = append(messages, map[string]any{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": []any{tc1},
		})
		// omp appends tool response
		messages = append(messages, map[string]any{
			"role":         "tool",
			"tool_call_id": tc1["id"],
			"content":      globResult,
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
			t.Fatalf("unmarshal Turn 2: %v", err)
		}

		msg2 := comp2["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		tcs2, ok := msg2["tool_calls"].([]any)
		if !ok || len(tcs2) == 0 {
			t.Fatalf("Turn 2: expected tool_calls in response, got %#v", msg2)
		}
		tc2 := tcs2[0].(map[string]any)
		fn2 := tc2["function"].(map[string]any)
		if fn2["name"] != "read" {
			t.Fatalf("Turn 2: expected function read, got %v", fn2["name"])
		}

		// omp executes read
		var fn2Args map[string]any
		_ = json.Unmarshal([]byte(fn2["arguments"].(string)), &fn2Args)
		readResult, err := ws.Read(fn2Args["path"].(string))
		if err != nil {
			t.Fatalf("omp read failed: %v", err)
		}
		if !strings.Contains(readResult, "Port = 8080") {
			t.Fatalf("expected read output to contain Port = 8080, got %q", readResult)
		}

		// --- TURN 3 ---
		messages = append(messages, map[string]any{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": []any{tc2},
		})
		messages = append(messages, map[string]any{
			"role":         "tool",
			"tool_call_id": tc2["id"],
			"content":      readResult,
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
			t.Fatalf("unmarshal Turn 3: %v", err)
		}

		msg3 := comp3["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		tcs3, ok := msg3["tool_calls"].([]any)
		if !ok || len(tcs3) == 0 {
			t.Fatalf("Turn 3: expected tool_calls in response, got %#v", msg3)
		}
		tc3 := tcs3[0].(map[string]any)
		fn3 := tc3["function"].(map[string]any)
		if fn3["name"] != "edit" {
			t.Fatalf("Turn 3: expected function edit, got %v", fn3["name"])
		}

		// omp executes edit
		var fn3Args map[string]any
		_ = json.Unmarshal([]byte(fn3["arguments"].(string)), &fn3Args)
		editResult, err := ws.Edit(fn3Args["input"].(string))
		if err != nil {
			t.Fatalf("omp edit failed: %v", err)
		}
		if editResult != "[main.go#1A2B] updated" {
			t.Fatalf("expected edit result [main.go#1A2B] updated, got %q", editResult)
		}

		// --- TURN 4 ---
		messages = append(messages, map[string]any{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": []any{tc3},
		})
		messages = append(messages, map[string]any{
			"role":         "tool",
			"tool_call_id": tc3["id"],
			"content":      editResult,
		})

		req4 := map[string]any{
			"model":    mimoModelID,
			"messages": messages,
			"tools":    tools,
			"stream":   false,
		}
		body4, _ := json.Marshal(req4)
		resp4, data4 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", body4, nil)
		if resp4.StatusCode != http.StatusOK {
			t.Fatalf("Turn 4 failed: %d: %s", resp4.StatusCode, data4)
		}

		var comp4 map[string]any
		if err := json.Unmarshal(data4, &comp4); err != nil {
			t.Fatalf("unmarshal Turn 4: %v", err)
		}

		msg4 := comp4["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		content4, _ := msg4["content"].(string)
		if !strings.Contains(content4, "Successfully updated server port to 9090 in src/main.go.") {
			t.Fatalf("Turn 4: expected final completion text, got %q", content4)
		}

		// Verify workspace updated
		updatedFile, _ := ws.Read("src/main.go")
		if !strings.Contains(updatedFile, "const Port = 9090") {
			t.Fatalf("workspace src/main.go was not updated to 9090: %s", updatedFile)
		}
	})

	t.Run("OpenAI_Streaming", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()

		handler := newOmpMimoMockHandler(t, mock, "call_omp_stream")
		mock.ChatHandler = handler.ServeHTTP

		srv := newServer(t, mock, nil)
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)

		ws := newOmpWorkspace()
		tools := ompOpenAIToolDefs()

		var messages []map[string]any
		messages = append(messages, map[string]any{
			"role":    "user",
			"content": "Find the entrypoint in src/, inspect it, and update the server port to 9090.",
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
		if len(parsed1.ToolCalls) == 0 || parsed1.ToolCalls[0].Name != "glob" {
			t.Fatalf("Turn 1: expected glob tool call, got %#v", parsed1.ToolCalls)
		}
		if !strings.Contains(parsed1.ReasoningContent, "locate files first") {
			t.Fatalf("Turn 1: expected reasoning content, got %q", parsed1.ReasoningContent)
		}

		var fn1Args map[string]any
		_ = json.Unmarshal([]byte(parsed1.ToolCalls[0].Arguments), &fn1Args)
		globResult, err := ws.Glob(fn1Args["path"].(string))
		if err != nil {
			t.Fatalf("omp glob failed: %v", err)
		}

		// --- TURN 2 (Streaming) ---
		// Replay assistant message with content: null and stripped reasoning
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
			"content":      globResult,
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
		if len(parsed2.ToolCalls) == 0 || parsed2.ToolCalls[0].Name != "read" {
			t.Fatalf("Turn 2: expected read tool call, got %#v", parsed2.ToolCalls)
		}
		if !strings.Contains(parsed2.ReasoningContent, "read src/main.go") {
			t.Fatalf("Turn 2: expected reasoning content, got %q", parsed2.ReasoningContent)
		}

		var fn2Args map[string]any
		_ = json.Unmarshal([]byte(parsed2.ToolCalls[0].Arguments), &fn2Args)
		readResult, err := ws.Read(fn2Args["path"].(string))
		if err != nil {
			t.Fatalf("omp read failed: %v", err)
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
			"content":      readResult,
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
		if len(parsed3.ToolCalls) == 0 || parsed3.ToolCalls[0].Name != "edit" {
			t.Fatalf("Turn 3: expected edit tool call, got %#v", parsed3.ToolCalls)
		}
		if !strings.Contains(parsed3.ReasoningContent, "edit Port to 9090") {
			t.Fatalf("Turn 3: expected reasoning content, got %q", parsed3.ReasoningContent)
		}

		var fn3Args map[string]any
		_ = json.Unmarshal([]byte(parsed3.ToolCalls[0].Arguments), &fn3Args)
		editResult, err := ws.Edit(fn3Args["input"].(string))
		if err != nil {
			t.Fatalf("omp edit failed: %v", err)
		}

		// --- TURN 4 (Streaming) ---
		messages = append(messages, map[string]any{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []map[string]any{
				{
					"id":   parsed3.ToolCalls[0].ID,
					"type": "function",
					"function": map[string]any{
						"name":      parsed3.ToolCalls[0].Name,
						"arguments": parsed3.ToolCalls[0].Arguments,
					},
				},
			},
		})
		messages = append(messages, map[string]any{
			"role":         "tool",
			"tool_call_id": parsed3.ToolCalls[0].ID,
			"content":      editResult,
		})

		req4 := map[string]any{
			"model":    mimoModelID,
			"messages": messages,
			"tools":    tools,
			"stream":   true,
		}
		body4, _ := json.Marshal(req4)
		resp4, data4 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", body4, nil)
		if resp4.StatusCode != http.StatusOK {
			t.Fatalf("Turn 4 stream failed: %d: %s", resp4.StatusCode, data4)
		}

		parsed4, err := parseOpenAISSE(data4)
		if err != nil {
			t.Fatalf("parse Turn 4 SSE: %v", err)
		}
		if !strings.Contains(parsed4.Content, "Successfully updated server port to 9090 in src/main.go.") {
			t.Fatalf("Turn 4: expected final content, got %q", parsed4.Content)
		}
		if parsed4.FinishReason != "stop" {
			t.Fatalf("Turn 4: expected finish_reason stop, got %q", parsed4.FinishReason)
		}

		// Verify workspace updated
		updatedFile, _ := ws.Read("src/main.go")
		if !strings.Contains(updatedFile, "const Port = 9090") {
			t.Fatalf("workspace src/main.go was not updated: %s", updatedFile)
		}
	})

	t.Run("Anthropic_NonStreaming", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()

		handler := newOmpMimoMockHandler(t, mock, "toolu_omp_ant_ns")
		mock.ChatHandler = handler.ServeHTTP

		srv := newServer(t, mock, nil)
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)

		ws := newOmpWorkspace()
		tools := ompAnthropicToolDefs()
		headers := map[string]string{
			"x-api-key":         "omp-claude-test-key",
			"anthropic-version": "2023-06-01",
		}

		var messages []map[string]any
		messages = append(messages, map[string]any{
			"role":    "user",
			"content": "Find the entrypoint in src/, inspect it, and update the server port to 9090.",
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
		if toolUseBlock1 == nil || toolUseBlock1["name"] != "glob" {
			t.Fatalf("Anthropic Turn 1: expected glob tool_use, got %#v", contentBlocks1)
		}

		input1 := toolUseBlock1["input"].(map[string]any)
		globResult, err := ws.Glob(input1["path"].(string))
		if err != nil {
			t.Fatalf("omp glob failed: %v", err)
		}

		// --- TURN 2 ---
		// omp appends assistant tool_use turn (thinking block stripped/omitted)
		messages = append(messages, map[string]any{
			"role": "assistant",
			"content": []any{
				toolUseBlock1,
			},
		})
		// omp appends user tool_result turn
		messages = append(messages, map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": toolUseBlock1["id"],
					"content":     globResult,
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
		if toolUseBlock2 == nil || toolUseBlock2["name"] != "read" {
			t.Fatalf("Anthropic Turn 2: expected read tool_use, got %#v", contentBlocks2)
		}

		input2 := toolUseBlock2["input"].(map[string]any)
		readResult, err := ws.Read(input2["path"].(string))
		if err != nil {
			t.Fatalf("omp read failed: %v", err)
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
					"content":     readResult,
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
		var toolUseBlock3 map[string]any
		for _, b := range contentBlocks3 {
			bm := b.(map[string]any)
			if bm["type"] == "tool_use" {
				toolUseBlock3 = bm
				break
			}
		}
		if toolUseBlock3 == nil || toolUseBlock3["name"] != "edit" {
			t.Fatalf("Anthropic Turn 3: expected edit tool_use, got %#v", contentBlocks3)
		}

		input3 := toolUseBlock3["input"].(map[string]any)
		editResult, err := ws.Edit(input3["input"].(string))
		if err != nil {
			t.Fatalf("omp edit failed: %v", err)
		}

		// --- TURN 4 ---
		messages = append(messages, map[string]any{
			"role": "assistant",
			"content": []any{
				toolUseBlock3,
			},
		})
		messages = append(messages, map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": toolUseBlock3["id"],
					"content":     editResult,
				},
			},
		})

		req4 := map[string]any{
			"model":      mimoModelID,
			"max_tokens": 4096,
			"messages":   messages,
			"tools":      tools,
			"stream":     false,
		}
		body4, _ := json.Marshal(req4)
		resp4, data4 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/messages", body4, headers)
		if resp4.StatusCode != http.StatusOK {
			t.Fatalf("Anthropic Turn 4 failed: %d: %s", resp4.StatusCode, data4)
		}

		var antResp4 map[string]any
		if err := json.Unmarshal(data4, &antResp4); err != nil {
			t.Fatalf("unmarshal Anthropic Turn 4: %v", err)
		}

		contentBlocks4 := antResp4["content"].([]any)
		var textBlock4 string
		for _, b := range contentBlocks4 {
			bm := b.(map[string]any)
			if bm["type"] == "text" {
				textBlock4, _ = bm["text"].(string)
				break
			}
		}
		if !strings.Contains(textBlock4, "Successfully updated server port to 9090 in src/main.go.") {
			t.Fatalf("Anthropic Turn 4: expected final text, got %#v", contentBlocks4)
		}

		// Verify workspace updated
		updatedFile, _ := ws.Read("src/main.go")
		if !strings.Contains(updatedFile, "const Port = 9090") {
			t.Fatalf("workspace src/main.go was not updated: %s", updatedFile)
		}
	})

	t.Run("Anthropic_Streaming", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()

		handler := newOmpMimoMockHandler(t, mock, "toolu_omp_ant_stream")
		mock.ChatHandler = handler.ServeHTTP

		srv := newServer(t, mock, nil)
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)

		ws := newOmpWorkspace()
		tools := ompAnthropicToolDefs()
		headers := map[string]string{
			"x-api-key":         "omp-claude-test-key",
			"anthropic-version": "2023-06-01",
		}

		var messages []map[string]any
		messages = append(messages, map[string]any{
			"role":    "user",
			"content": "Find the entrypoint in src/, inspect it, and update the server port to 9090.",
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
		if len(parsed1.ToolCalls) == 0 || parsed1.ToolCalls[0].Name != "glob" {
			t.Fatalf("Anthropic Turn 1: expected glob tool_use, got %#v", parsed1.ToolCalls)
		}
		if parsed1.StopReason != "tool_use" {
			t.Fatalf("Anthropic Turn 1: expected stop_reason tool_use, got %q", parsed1.StopReason)
		}

		globResult, err := ws.Glob(parsed1.ToolCalls[0].Input["path"].(string))
		if err != nil {
			t.Fatalf("omp glob failed: %v", err)
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
					"content":     globResult,
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
		if len(parsed2.ToolCalls) == 0 || parsed2.ToolCalls[0].Name != "read" {
			t.Fatalf("Anthropic Turn 2: expected read tool_use, got %#v", parsed2.ToolCalls)
		}
		if parsed2.StopReason != "tool_use" {
			t.Fatalf("Anthropic Turn 2: expected stop_reason tool_use, got %q", parsed2.StopReason)
		}

		readResult, err := ws.Read(parsed2.ToolCalls[0].Input["path"].(string))
		if err != nil {
			t.Fatalf("omp read failed: %v", err)
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
					"content":     readResult,
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
		if len(parsed3.ToolCalls) == 0 || parsed3.ToolCalls[0].Name != "edit" {
			t.Fatalf("Anthropic Turn 3: expected edit tool_use, got %#v", parsed3.ToolCalls)
		}
		if parsed3.StopReason != "tool_use" {
			t.Fatalf("Anthropic Turn 3: expected stop_reason tool_use, got %q", parsed3.StopReason)
		}

		editResult, err := ws.Edit(parsed3.ToolCalls[0].Input["input"].(string))
		if err != nil {
			t.Fatalf("omp edit failed: %v", err)
		}

		// --- TURN 4 (Streaming) ---
		messages = append(messages, map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type":  "tool_use",
					"id":    parsed3.ToolCalls[0].ID,
					"name":  parsed3.ToolCalls[0].Name,
					"input": parsed3.ToolCalls[0].Input,
				},
			},
		})
		messages = append(messages, map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": parsed3.ToolCalls[0].ID,
					"content":     editResult,
				},
			},
		})

		req4 := map[string]any{
			"model":      mimoModelID,
			"max_tokens": 4096,
			"messages":   messages,
			"tools":      tools,
			"stream":     true,
		}
		body4, _ := json.Marshal(req4)
		resp4, data4 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/messages", body4, headers)
		if resp4.StatusCode != http.StatusOK {
			t.Fatalf("Anthropic Turn 4 stream failed: %d: %s", resp4.StatusCode, data4)
		}

		parsed4, err := parseAnthropicSSE(data4)
		if err != nil {
			t.Fatalf("parse Anthropic Turn 4 SSE: %v", err)
		}
		if !strings.Contains(parsed4.Text, "Successfully updated server port to 9090 in src/main.go.") {
			t.Fatalf("Anthropic Turn 4: expected final text, got %q", parsed4.Text)
		}
		if parsed4.StopReason != "end_turn" {
			t.Fatalf("Anthropic Turn 4: expected stop_reason end_turn, got %q", parsed4.StopReason)
		}

		// Verify workspace updated
		updatedFile, _ := ws.Read("src/main.go")
		if !strings.Contains(updatedFile, "const Port = 9090") {
			t.Fatalf("workspace src/main.go was not updated: %s", updatedFile)
		}
	})

	t.Run("OmpToolSchemaCompliance", func(t *testing.T) {
		// Verify that all 7 omp tools pass through NormalizeRequest without error and
		// retain their function definitions.
		reqPayload := map[string]any{
			"model": mimoModelID,
			"messages": []map[string]any{
				{"role": "user", "content": "hello"},
			},
			"tools": ompOpenAIToolDefs(),
		}
		reqBytes, _ := json.Marshal(reqPayload)
		normalizedBytes, err := convert.NormalizeRequest(reqBytes, "")
		if err != nil {
			t.Fatalf("NormalizeRequest failed on omp tools: %v", err)
		}

		var normalized map[string]any
		if err := json.Unmarshal(normalizedBytes, &normalized); err != nil {
			t.Fatalf("unmarshal normalized JSON: %v", err)
		}

		normTools, ok := normalized["tools"].([]any)
		if !ok || len(normTools) < 7 {
			t.Fatalf("expected at least 7 normalized tools, got %d", len(normTools))
		}

		toolNames := make(map[string]bool)
		for _, tRaw := range normTools {
			tm := tRaw.(map[string]any)
			fn := tm["function"].(map[string]any)
			name := fn["name"].(string)
			toolNames[name] = true
		}

		expectedNames := []string{"read", "bash", "write", "edit", "grep", "glob", "todo", "end_turn"}
		for _, name := range expectedNames {
			if !toolNames[name] {
				t.Errorf("missing expected tool %q in normalized output", name)
			}
		}
	})

	t.Run("ReasoningCache_MultiTurnRestorationAndContentNull", func(t *testing.T) {
		// Unit check: Verify that when reasoningCache is populated with tool call reasoning,
		// NormalizeRequest restores reasoning_content and forces content: null on assistant tool turns.
		cache := reasoningcache.New(100, time.Hour)
		cache.Put([]string{"call_glob_test"}, "", "", "Thinking about globbing...", "", mimoModelID)
		cache.Put([]string{"call_read_test"}, "", "", "Thinking about reading...", "", mimoModelID)
		cache.Put([]string{"call_edit_test"}, "", "", "Thinking about editing...", "", mimoModelID)

		convert.SetReasoningLookup(func(toolID string, content, toolCallsJSON string) (string, string, bool) {
			return cache.Get(toolID, content, toolCallsJSON)
		})
		defer convert.SetReasoningLookup(nil)

		// Simulate client sending multi-turn conversation where reasoning_content is stripped
		// and content is empty string or nil
		clientReq := map[string]any{
			"model": mimoModelID,
			"messages": []map[string]any{
				{"role": "user", "content": "start"},
				{
					"role":    "assistant",
					"content": "", // empty string from client
					"tool_calls": []map[string]any{
						{
							"id":   "call_glob_test",
							"type": "function",
							"function": map[string]any{
								"name":      "glob",
								"arguments": `{"path":"src/**/*.go"}`,
							},
						},
					},
				},
				{"role": "tool", "tool_call_id": "call_glob_test", "content": "src/main.go"},
				{
					"role":    "assistant",
					"content": nil, // nil from client
					"tool_calls": []map[string]any{
						{
							"id":   "call_read_test",
							"type": "function",
							"function": map[string]any{
								"name":      "read",
								"arguments": `{"path":"src/main.go"}`,
							},
						},
					},
				},
				{"role": "tool", "tool_call_id": "call_read_test", "content": "package main..."},
			},
		}

		rawBytes, _ := json.Marshal(clientReq)
		normBytes, err := convert.NormalizeRequest(rawBytes, "")
		if err != nil {
			t.Fatalf("NormalizeRequest failed: %v", err)
		}

		var normReq map[string]any
		_ = json.Unmarshal(normBytes, &normReq)
		normMsgs := normReq["messages"].([]any)

		asstMsgs := findMessagesByRole(normMsgs, "assistant")
		if len(asstMsgs) != 2 {
			t.Fatalf("expected 2 assistant messages, got %d", len(asstMsgs))
		}

		// Verify Assistant 1
		asst1 := asstMsgs[0]
		if asst1["content"] != nil {
			t.Errorf("asst 1 content = %#v, want null", asst1["content"])
		}
		if rc, _ := asst1["reasoning_content"].(string); rc != "Thinking about globbing..." {
			t.Errorf("asst 1 reasoning_content = %q, want %q", rc, "Thinking about globbing...")
		}

		// Verify Assistant 2
		asst2 := asstMsgs[1]
		if asst2["content"] != nil {
			t.Errorf("asst 2 content = %#v, want null", asst2["content"])
		}
		if rc, _ := asst2["reasoning_content"].(string); rc != "Thinking about reading..." {
			t.Errorf("asst 2 reasoning_content = %q, want %q", rc, "Thinking about reading...")
		}
	})
}
