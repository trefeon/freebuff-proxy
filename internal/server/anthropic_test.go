package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/convert"
	"freebuff-proxy/internal/testutil"
)

const testModelA = "z-ai/glm-5.2"

func testChunk(id string, created int64, payload string) string {
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","created":%d,"model":%q,%s}`, id, created, testModelA, payload)
}

func doTestJSON(t *testing.T, method, url string, body []byte, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, data
}

func findAssistantMsg(msgs []any) (map[string]any, bool) {
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok && mm["role"] == "assistant" {
			return mm, true
		}
	}
	return nil, false
}

// TestAnthropic_AssistantToolUseProducesContentNull verifies that an assistant
// turn with tool_use and no text produces content: nil (JSON null) in
// normalized chat params, while text-only produces text and empty produces "".
func TestAnthropic_AssistantToolUseProducesContentNull(t *testing.T) {
	// Case 1: Assistant turn with tool_use and NO text -> content: nil (JSON null)
	rawToolOnly := map[string]any{
		"model": "anthropic/claude-3-opus",
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{
						"type":  "tool_use",
						"id":    "toolu_weather_01",
						"name":  "get_weather",
						"input": map[string]any{"city": "Tokyo"},
					},
				},
			},
		},
	}

	chatParams, err := anthropicToChatParams(rawToolOnly)
	if err != nil {
		t.Fatalf("anthropicToChatParams failed: %v", err)
	}

	var chatMap map[string]any
	if err := json.Unmarshal(chatParams, &chatMap); err != nil {
		t.Fatalf("unmarshal chatParams failed: %v", err)
	}

	msgs, ok := chatMap["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("expected 1 message, got: %v", chatMap["messages"])
	}
	firstMsg, ok := msgs[0].(map[string]any)
	if !ok {
		t.Fatalf("expected map message, got: %T", msgs[0])
	}
	if cVal, exists := firstMsg["content"]; !exists || cVal != nil {
		t.Fatalf("expected assistant content to be explicit nil (null), got: %#v", cVal)
	}
	if _, ok := firstMsg["tool_calls"].([]any); !ok {
		t.Fatalf("expected tool_calls on assistant message, got: %#v", firstMsg["tool_calls"])
	}

	// Verify convert.NormalizeRequest preserves content: null
	normalized, err := convert.NormalizeRequest(chatParams, "deepseek/deepseek-v4-pro")
	if err != nil {
		t.Fatalf("NormalizeRequest failed: %v", err)
	}
	var normMap map[string]any
	if err := json.Unmarshal(normalized, &normMap); err != nil {
		t.Fatalf("unmarshal normalized failed: %v", err)
	}
	normMsgs := normMap["messages"].([]any)
	normFirst := normMsgs[0].(map[string]any)
	if cVal, exists := normFirst["content"]; !exists || cVal != nil {
		t.Fatalf("expected normalized assistant content to be explicit nil (null), got: %#v", cVal)
	}

	// Case 2: Assistant turn with text AND tool_use -> content is string/array, not null
	rawTextAndTool := map[string]any{
		"model": "anthropic/claude-3-opus",
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{
						"type": "text",
						"text": "Looking up the weather...",
					},
					map[string]any{
						"type":  "tool_use",
						"id":    "toolu_weather_02",
						"name":  "get_weather",
						"input": map[string]any{"city": "Paris"},
					},
				},
			},
		},
	}
	chatParams2, err := anthropicToChatParams(rawTextAndTool)
	if err != nil {
		t.Fatalf("anthropicToChatParams failed: %v", err)
	}
	var chatMap2 map[string]any
	if err := json.Unmarshal(chatParams2, &chatMap2); err != nil {
		t.Fatalf("unmarshal chatParams2 failed: %v", err)
	}
	msg2 := chatMap2["messages"].([]any)[0].(map[string]any)
	if msg2["content"] != "Looking up the weather..." {
		t.Fatalf("expected assistant content to be text, got: %#v", msg2["content"])
	}

	// Case 3: Assistant turn with NO tool_use and NO text -> content is ""
	rawEmpty := map[string]any{
		"model": "anthropic/claude-3-opus",
		"messages": []any{
			map[string]any{
				"role":    "assistant",
				"content": []any{},
			},
		},
	}
	chatParams3, err := anthropicToChatParams(rawEmpty)
	if err != nil {
		t.Fatalf("anthropicToChatParams failed: %v", err)
	}
	var chatMap3 map[string]any
	if err := json.Unmarshal(chatParams3, &chatMap3); err != nil {
		t.Fatalf("unmarshal chatParams3 failed: %v", err)
	}
	msg3 := chatMap3["messages"].([]any)[0].(map[string]any)
	if msg3["content"] != "" {
		t.Fatalf("expected assistant content to be \"\", got: %#v", msg3["content"])
	}
}

// TestServer_ReasoningCacheReplayAcrossTurns verifies that an assistant response
// with reasoning and tool calls saves reasoning into the cache, and a subsequent
// chat request replaying that tool call without reasoning_content restores it
// via NormalizeRequest and upstream forwarding.
func TestServer_ReasoningCacheReplayAcrossTurns(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	// Turn 1 Mock Response: emits reasoning_content + tool_calls
	toolCallID := "call_weather_sf_123"
	reasoningText := "Thinking: I must check San Francisco weather using get_weather tool."
	mock.ChatBody = testutil.SSEEvent(testChunk("chatcmpl-turn1", 100,
		`"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"`+reasoningText+`","tool_calls":[{"index":0,"id":"`+toolCallID+`","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},"finish_reason":null}]`) +
		"\n" +
		testChunk("chatcmpl-turn1", 100, `"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`) +
		"\n" +
		testChunk("chatcmpl-turn1", 100, `"usage":{"prompt_tokens":15,"completion_tokens":25,"total_tokens":40}`))

	srv := newServer(t, mock, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Turn 1 Request (non-streaming chat completions)
	turn1Req := `{"model":"` + testModelA + `","messages":[{"role":"user","content":"How is the weather in SF?"}],"stream":false}`
	resp1, data1 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(turn1Req), nil)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("Turn 1 failed with status %d: %s", resp1.StatusCode, data1)
	}

	var turn1Resp map[string]any
	if err := json.Unmarshal(data1, &turn1Resp); err != nil {
		t.Fatalf("unmarshal Turn 1 response failed: %v", err)
	}

	// Verify server reasoningCache recorded the tool call reasoning
	if srv.reasoningCache == nil {
		t.Fatalf("srv.reasoningCache is nil")
	}
	rCached, _, ok := srv.reasoningCache.GetByToolID(toolCallID)
	if !ok || rCached != reasoningText {
		t.Fatalf("expected cached reasoning %q for %s, got (%q, %v)", reasoningText, toolCallID, rCached, ok)
	}

	// Turn 2 Mock Response: simple final answer
	mock.ChatBody = testutil.SSEEvent(testChunk("chatcmpl-turn2", 101,
		`"choices":[{"index":0,"delta":{"content":"The weather in SF is 68F and sunny."},"finish_reason":"stop"}]`) +
		"\n" +
		testChunk("chatcmpl-turn2", 101, `"usage":{"prompt_tokens":50,"completion_tokens":10,"total_tokens":60}`))

	// Turn 2 Request: Replays tool call WITHOUT reasoning_content
	turn2Req := `{
		"model":"` + testModelA + `",
		"messages":[
			{"role":"user","content":"How is the weather in SF?"},
			{
				"role":"assistant",
				"content":null,
				"tool_calls":[
					{
						"id":"` + toolCallID + `",
						"type":"function",
						"function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}
					}
				]
			},
			{
				"role":"tool",
				"tool_call_id":"` + toolCallID + `",
				"content":"68F and sunny"
			}
		],
		"stream":false
	}`

	resp2, data2 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(turn2Req), nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("Turn 2 failed with status %d: %s", resp2.StatusCode, data2)
	}

	// Verify that NormalizeRequest restored reasoning_content before sending upstream
	lastReq := mock.LastChatBody()
	if len(lastReq) == 0 {
		t.Fatalf("mock recorded empty request body for Turn 2")
	}

	var upReq map[string]any
	if err := json.Unmarshal([]byte(lastReq), &upReq); err != nil {
		t.Fatalf("unmarshal upstream request failed: %v; body: %s", err, lastReq)
	}

	upMsgs, ok := upReq["messages"].([]any)
	if !ok {
		t.Fatalf("expected messages slice upstream, got: %v", upReq["messages"])
	}

	asstMsg, ok := findAssistantMsg(upMsgs)
	if !ok {
		t.Fatalf("expected assistant message upstream, got: %v", upMsgs)
	}

	rcVal, hasRC := asstMsg["reasoning_content"]
	if !hasRC || rcVal != reasoningText {
		t.Fatalf("expected restored reasoning_content %q, got: %#v (full msg: %#v)", reasoningText, rcVal, asstMsg)
	}
}

// TestServer_ReasoningCacheStreamReplay verifies streaming turn ingestion and replay.
func TestServer_ReasoningCacheStreamReplay(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	toolCallID := "call_stream_tool_456"
	reasoningChunk1 := "Thinking step 1: "
	reasoningChunk2 := "Calling shell command."
	expectedReasoning := reasoningChunk1 + reasoningChunk2

	// Streaming Turn 1
	mock.ChatBody = testutil.SSEEvent(testChunk("chatcmpl-s1", 200,
		`"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"`+reasoningChunk1+`"},"finish_reason":null}]`) +
		"\n" +
		testChunk("chatcmpl-s1", 200,
			`"choices":[{"index":0,"delta":{"reason_content":"`+reasoningChunk2+`","reasoning":"`+reasoningChunk2+`","tool_calls":[{"index":0,"id":"`+toolCallID+`","type":"function","function":{"name":"run_cmd","arguments":"{\"cmd\":\"ls\"}"}}]},"finish_reason":null}]`) +
		"\n" +
		testChunk("chatcmpl-s1", 200, `"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`) +
		"\n" +
		testChunk("chatcmpl-s1", 200, `"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}`))

	srv := newServer(t, mock, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	turn1Req := `{"model":"` + testModelA + `","messages":[{"role":"user","content":"run ls"}],"stream":true}`
	resp1, data1 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(turn1Req), nil)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("Turn 1 stream failed with status %d: %s", resp1.StatusCode, data1)
	}

	time.Sleep(10 * time.Millisecond)

	// Turn 2 Mock Response
	mock.ChatBody = testutil.SSEEvent(testChunk("chatcmpl-s2", 201,
		`"choices":[{"index":0,"delta":{"content":"files listed"},"finish_reason":"stop"}]`))

	turn2Req := `{
		"model":"` + testModelA + `",
		"messages":[
			{"role":"user","content":"run ls"},
			{
				"role":"assistant",
				"content":null,
				"tool_calls":[
					{
						"id":"` + toolCallID + `",
						"type":"function",
						"function":{"name":"run_cmd","arguments":"{\"cmd\":\"ls\"}"}
					}
				]
			},
			{
				"role":"tool",
				"tool_call_id":"` + toolCallID + `",
				"content":"file1.txt\nfile2.txt"
			}
		],
		"stream":false
	}`

	resp2, data2 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(turn2Req), nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("Turn 2 failed with status %d: %s", resp2.StatusCode, data2)
	}

	lastReq := mock.LastChatBody()
	var upReq map[string]any
	if err := json.Unmarshal([]byte(lastReq), &upReq); err != nil {
		t.Fatalf("unmarshal upstream request failed: %v", err)
	}
	upMsgs := upReq["messages"].([]any)
	asstMsg, ok := findAssistantMsg(upMsgs)
	if !ok {
		t.Fatalf("expected assistant message upstream, got: %v", upMsgs)
	}
	rcVal, hasRC := asstMsg["reasoning_content"]
	if !hasRC || !strings.Contains(rcVal.(string), "Thinking step 1") {
		t.Fatalf("expected restored reasoning containing %q, got: %#v", expectedReasoning, rcVal)
	}
}

// TestServer_AnthropicMessages_ReasoningCacheReplay verifies that Anthropic /v1/messages
// responses ingest thinking blocks into reasoningCache and restore them across turns.
func TestServer_AnthropicMessages_ReasoningCacheReplay(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	toolCallID := "toolu_anthropic_calc_789"
	thinkingText := "Calculating the square root of 144."

	// Mock upstream returns OpenAI chat chunks
	mock.ChatBody = testutil.SSEEvent(testChunk("chatcmpl-ant1", 300,
		`"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"`+thinkingText+`","tool_calls":[{"index":0,"id":"`+toolCallID+`","type":"function","function":{"name":"calc","arguments":"{\"num\":144}"}}]},"finish_reason":null}]`) +
		"\n" +
		testChunk("chatcmpl-ant1", 300, `"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`) +
		"\n" +
		testChunk("chatcmpl-ant1", 300, `"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}`))

	srv := newServer(t, mock, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Anthropic Turn 1 (non-streaming)
	turn1Req := `{
		"model":"` + testModelA + `",
		"max_tokens": 100,
		"messages":[
			{"role":"user","content":"calculate sqrt(144)"}
		],
		"stream":false
	}`
	resp1, data1 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(turn1Req),
		map[string]string{"x-api-key": "test-key", "anthropic-version": "2023-06-01"})
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("Anthropic Turn 1 failed status %d: %s", resp1.StatusCode, data1)
	}

	// Turn 2 Mock Response
	mock.ChatBody = testutil.SSEEvent(testChunk("chatcmpl-ant2", 301,
		`"choices":[{"index":0,"delta":{"content":"The answer is 12."},"finish_reason":"stop"}]`))

	// Anthropic Turn 2 (assistant turn with tool_use and no text)
	turn2Req := `{
		"model":"` + testModelA + `",
		"max_tokens": 100,
		"messages":[
			{"role":"user","content":"calculate sqrt(144)"},
			{
				"role":"assistant",
				"content":[
					{
						"type":"tool_use",
						"id":"` + toolCallID + `",
						"name":"calc",
						"input":{"num":144}
					}
				]
			},
			{
				"role":"user",
				"content":[
					{
						"type":"tool_result",
						"tool_use_id":"` + toolCallID + `",
						"content":"12"
					}
				]
			}
		],
		"stream":false
	}`

	resp2, data2 := doTestJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(turn2Req),
		map[string]string{"x-api-key": "test-key", "anthropic-version": "2023-06-01"})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("Anthropic Turn 2 failed status %d: %s", resp2.StatusCode, data2)
	}

	lastReq := mock.LastChatBody()
	var upReq map[string]any
	if err := json.Unmarshal([]byte(lastReq), &upReq); err != nil {
		t.Fatalf("unmarshal upstream request failed: %v", err)
	}
	upMsgs := upReq["messages"].([]any)
	asstMsg, ok := findAssistantMsg(upMsgs)
	if !ok {
		t.Fatalf("expected assistant message upstream, got: %v", upMsgs)
	}
	rcVal, hasRC := asstMsg["reasoning_content"]
	if !hasRC || rcVal != thinkingText {
		t.Fatalf("expected restored reasoning_content %q, got: %#v", thinkingText, rcVal)
	}
	// Also assert content is null/nil on the assistant message
	if asstMsg["content"] != nil {
		t.Fatalf("expected assistant content null, got: %#v", asstMsg["content"])
	}
}
