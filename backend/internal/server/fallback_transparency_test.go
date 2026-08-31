// fallback_transparency_test.go — issue #164: fallback transparency.
//
// The gateway must tell clients what model actually served a request:
//   - x-freebuff-served-model on every successful response (requested model
//     when served directly, the re-routed model after a fallback);
//   - x-freebuff-fallback ONLY when a fallback fired, naming the reason
//     (quota_exhausted | queue_timeout);
//   - the response body's model field (chat.completion chunks/body and the
//     Anthropic message_start/message object) reflecting the served model.
package server_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
)

// chunkModel renders one OpenAI-style SSE chat chunk with a custom model id
// (the shared chunk() helper bakes modelA; these tests need chunks that echo
// a DIFFERENT model to prove the relay rewrites them).
func chunkModel(id string, model string, payload string) string {
	return `{"id":"` + id + `","object":"chat.completion.chunk","created":1,"model":"` + model + `",` + payload + `}`
}

// chatSSE returns a minimal two-chunk streaming chat body (content + stop)
// whose chunks echo the given model id.
func chatSSE(model string) string {
	return testutil.SSEEvent(chunkModel("chatcmpl-t1", model,
		`"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunkModel("chatcmpl-t1", model,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`))
}

// parseChunkModels extracts the "model" field of every data line in an SSE
// body (assumes chat.completion.chunk objects).
func parseChunkModels(t *testing.T, body string) []string {
	t.Helper()
	var models []string
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			continue
		}
		var chunk struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("unmarshal chunk: %v: %s", err, data)
		}
		models = append(models, chunk.Model)
	}
	return models
}

// TestFallbackTransparencyDirectOpenAI pins the direct-serving contract: no
// fallback config, the requested model serves, so x-freebuff-served-model
// equals the requested model, x-freebuff-fallback is ABSENT, and the body
// model field — streaming chunks and the non-streaming body — reflects the
// served model even when the upstream echo lies.
func TestFallbackTransparencyDirectOpenAI(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// The upstream echoes a DIFFERENT model id than the one it was asked to
	// serve: the relay must stamp the proxy's served model regardless.
	mock.ChatBody = chatSSE("upstream/echo-model")
	srv, _ := newTestServer(t, nil, mock)

	// Streaming path.
	resp, data := doJSON(t, http.MethodPost, srv.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200: %s", resp.StatusCode, data)
	}
	if got := resp.Header.Get("x-freebuff-served-model"); got != modelA {
		t.Errorf("x-freebuff-served-model = %q, want %q (direct serving)", got, modelA)
	}
	if got := resp.Header.Get("x-freebuff-fallback"); got != "" {
		t.Errorf("x-freebuff-fallback = %q, want absent (direct serving)", got)
	}
	for i, m := range parseChunkModels(t, string(data)) {
		if m != modelA {
			t.Errorf("stream chunk %d model = %q, want %q (served model stamped)", i, m, modelA)
		}
	}

	// Non-streaming path.
	resp2, data2 := doJSON(t, http.MethodPost, srv.URL+"/v1/chat/completions",
		[]byte(`{"model":"`+modelA+`","messages":[{"role":"user","content":"ping"}],"stream":false}`), nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("non-stream status = %d, want 200: %s", resp2.StatusCode, data2)
	}
	var comp struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(data2, &comp); err != nil {
		t.Fatalf("unmarshal body: %v: %s", err, data2)
	}
	if comp.Model != modelA {
		t.Errorf("non-stream body model = %q, want %q (served model stamped)", comp.Model, modelA)
	}
	if got := resp2.Header.Get("x-freebuff-served-model"); got != modelA {
		t.Errorf("non-stream x-freebuff-served-model = %q, want %q", got, modelA)
	}
	if got := resp2.Header.Get("x-freebuff-fallback"); got != "" {
		t.Errorf("non-stream x-freebuff-fallback = %q, want absent", got)
	}
}

// TestFallbackTransparencyQueueTimeout pins the queue-time fallback (issue
// #100): with FALLBACK_AFTER_MS + FALLBACK_MODEL configured and a waiting
// room beyond the threshold, x-freebuff-served-model names the fallback
// model, x-freebuff-fallback = queue_timeout, the legacy
// X-FreeBuff-Fallback-Model header stays, and streamed chunks carry the
// fallback model.
func TestFallbackTransparencyQueueTimeout(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionMode = "queued"
	mock.SessionSequence = []string{"queued", "active"} // first create queues; the fallback model's create succeeds
	mock.EstimatedWaitMs = 20000                        // > FALLBACK_AFTER_MS (10s)
	// The fallback model's upstream chat echoes the REQUESTED model: the
	// relay must stamp the actually-served fallback model onto the chunks.
	mock.ChatBody = chatSSE("openai/gpt-5.6-luna")
	srv, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.FallbackAfter = 10 * time.Second
		c.FallbackModels = map[string]string{"openai/gpt-5.6-luna": "deepseek/deepseek-v4-flash"}
	}, mock)
	body := `{"model":"openai/gpt-5.6-luna","messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, data := doJSON(t, http.MethodPost, srv.URL+"/v1/chat/completions", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fallback served): %s", resp.StatusCode, data)
	}
	if got := resp.Header.Get("x-freebuff-served-model"); got != "deepseek/deepseek-v4-flash" {
		t.Errorf("x-freebuff-served-model = %q, want deepseek/deepseek-v4-flash", got)
	}
	if got := resp.Header.Get("x-freebuff-fallback"); got != "queue_timeout" {
		t.Errorf("x-freebuff-fallback = %q, want queue_timeout", got)
	}
	// Legacy #100 header remains for existing clients.
	if got := resp.Header.Get("X-FreeBuff-Fallback-Model"); got != "deepseek/deepseek-v4-flash" {
		t.Errorf("X-FreeBuff-Fallback-Model = %q, want deepseek/deepseek-v4-flash", got)
	}
	for i, m := range parseChunkModels(t, string(data)) {
		if m != "deepseek/deepseek-v4-flash" {
			t.Errorf("stream chunk %d model = %q, want deepseek/deepseek-v4-flash (served fallback stamped)", i, m)
		}
	}
}

// quotaExhaustionMock wires a mock whose session create 429s with a
// quota-exhausted body for the requested model and succeeds for the
// QUOTA_FALLBACK_MODELS target (mirrors the pool-level quota setup,
// exercised end-to-end through the HTTP surface here).
func quotaExhaustionMock(t *testing.T) *testutil.MockUpstream {
	t.Helper()
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"none"}`)
			return
		}
		if r.Method == http.MethodPost {
			switch r.Header.Get("x-freebuff-model") {
			case "deepseek/deepseek-v4-flash":
				// The fallback model admits fine.
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, fmt.Sprintf(`{
					"status": "active",
					"instanceId": "inst-fallback",
					"model": %q,
					"expiresAt": %q
				}`, "deepseek/deepseek-v4-flash", time.Now().Add(time.Hour).Format(time.RFC3339)))
			default:
				// Requested model is quota exhausted.
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, fmt.Sprintf(`{
					"status": "rate_limited",
					"model": %q,
					"limit": 5,
					"recentCount": 5,
					"period": "pacific_day",
					"resetAt": %q,
					"retryAfterMs": 3600000
				}`, r.Header.Get("x-freebuff-model"), time.Now().Add(time.Hour).Format(time.RFC3339)))
			}
			return
		}
		http.NotFound(w, r)
	}
	// relay must stamp the served fallback model.
	mock.ChatBody = chatSSE("z-ai/glm-5.2")
	return mock
}

// TestFallbackTransparencyQuotaExhaustedOpenAI pins the quota-exhaustion
// fallback (issue #155): when the requested model's session quota is
// exhausted on every token, the pool re-routes to QUOTA_FALLBACK_MODELS and
// the response reports served-model + x-freebuff-fallback: quota_exhausted.
func TestFallbackTransparencyQuotaExhaustedOpenAI(t *testing.T) {
	mock := quotaExhaustionMock(t)
	srv, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.QuotaFallbackModels = map[string]string{"z-ai/glm-5.2": "deepseek/deepseek-v4-flash"}
	}, mock)

	// Streaming: the fallback model serves; headers + chunks must agree.
	body := `{"model":"z-ai/glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, data := doJSON(t, http.MethodPost, srv.URL+"/v1/chat/completions", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200 (fallback served): %s", resp.StatusCode, data)
	}
	if got := resp.Header.Get("x-freebuff-served-model"); got != "deepseek/deepseek-v4-flash" {
		t.Errorf("x-freebuff-served-model = %q, want deepseek/deepseek-v4-flash", got)
	}
	if got := resp.Header.Get("x-freebuff-fallback"); got != "quota_exhausted" {
		t.Errorf("x-freebuff-fallback = %q, want quota_exhausted", got)
	}
	for i, m := range parseChunkModels(t, string(data)) {
		if m != "deepseek/deepseek-v4-flash" {
			t.Errorf("stream chunk %d model = %q, want deepseek/deepseek-v4-flash (served fallback stamped)", i, m)
		}
	}

	// Non-streaming: body model must also name the served fallback model.
	bodyNS := `{"model":"z-ai/glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":false}`
	resp2, data2 := doJSON(t, http.MethodPost, srv.URL+"/v1/chat/completions", []byte(bodyNS), nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("non-stream status = %d, want 200: %s", resp2.StatusCode, data2)
	}
	var comp struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(data2, &comp); err != nil {
		t.Fatalf("unmarshal body: %v: %s", err, data2)
	}
	if comp.Model != "deepseek/deepseek-v4-flash" {
		t.Errorf("non-stream body model = %q, want deepseek/deepseek-v4-flash", comp.Model)
	}
	if got := resp2.Header.Get("x-freebuff-served-model"); got != "deepseek/deepseek-v4-flash" {
		t.Errorf("non-stream x-freebuff-served-model = %q, want deepseek/deepseek-v4-flash", got)
	}
	if got := resp2.Header.Get("x-freebuff-fallback"); got != "quota_exhausted" {
		t.Errorf("non-stream x-freebuff-fallback = %q, want quota_exhausted", got)
	}
}

// anthropicHeaders sends one /v1/messages request with the given headers and
// returns the response, the raw body, and the parsed SSE events when stream.
type anthropicMessageEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Model string `json:"model"`
	} `json:"message"`
}

func parseAnthropicEvents(t *testing.T, body string) []anthropicMessageEvent {
	t.Helper()
	var events []anthropicMessageEvent
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev anthropicMessageEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("unmarshal anthropic event: %v: %s", err, line)
		}
		events = append(events, ev)
	}
	return events
}

// TestFallbackTransparencyAnthropic pins the Anthropic surface: message_start
// (streaming) and the message object (non-streaming) name the served model,
// and the fallback headers fire on both when a quota fallback serves.
func TestFallbackTransparencyAnthropic(t *testing.T) {
	direct := testutil.NewMock()
	defer direct.Close()
	direct.ChatBody = chatSSE("upstream/echo-model")
	srvDirect, _ := newTestServer(t, []string{"anthropic-key"}, direct)

	// Direct streaming: message_start names the requested model.
	reqBody := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, data := doJSON(t, http.MethodPost, srvDirect.URL+"/v1/messages", []byte(reqBody),
		map[string]string{"anthropic-api-key": "anthropic-key", "anthropic-version": "2023-06-01"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("direct stream status = %d, want 200: %s", resp.StatusCode, data)
	}
	if got := resp.Header.Get("x-freebuff-served-model"); got != modelA {
		t.Errorf("direct x-freebuff-served-model = %q, want %q", got, modelA)
	}
	if got := resp.Header.Get("x-freebuff-fallback"); got != "" {
		t.Errorf("direct x-freebuff-fallback = %q, want absent", got)
	}
	foundStart := false
	for _, ev := range parseAnthropicEvents(t, string(data)) {
		if ev.Type == "message_start" && ev.Message != nil {
			foundStart = true
			if ev.Message.Model != modelA {
				t.Errorf("message_start model = %q, want %q (served model)", ev.Message.Model, modelA)
			}
		}
	}
	if !foundStart {
		t.Error("message_start event not found in direct stream")
	}

	// Direct non-streaming: message object model names the served model.
	reqBodyNS := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"stream":false}`
	respNS, dataNS := doJSON(t, http.MethodPost, srvDirect.URL+"/v1/messages", []byte(reqBodyNS),
		map[string]string{"anthropic-api-key": "anthropic-key", "anthropic-version": "2023-06-01"})
	if respNS.StatusCode != http.StatusOK {
		t.Fatalf("direct non-stream status = %d, want 200: %s", respNS.StatusCode, dataNS)
	}
	var msg struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(dataNS, &msg); err != nil {
		t.Fatalf("unmarshal message: %v: %s", err, dataNS)
	}
	if msg.Model != modelA {
		t.Errorf("non-stream message model = %q, want %q", msg.Model, modelA)
	}

	// Quota fallback on the Anthropic surface.
	fb := quotaExhaustionMock(t)
	srvFB, _ := newTestServerCfg(t, []string{"anthropic-key"}, func(c *config.Config) {
		c.QuotaFallbackModels = map[string]string{"z-ai/glm-5.2": "deepseek/deepseek-v4-flash"}
	}, fb)
	reqBodyGlm := `{"model":"z-ai/glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`
	respFB, dataFB := doJSON(t, http.MethodPost, srvFB.URL+"/v1/messages", []byte(reqBodyGlm),
		map[string]string{"anthropic-api-key": "anthropic-key", "anthropic-version": "2023-06-01"})
	if respFB.StatusCode != http.StatusOK {
		t.Fatalf("fallback stream status = %d, want 200: %s", respFB.StatusCode, dataFB)
	}
	if got := respFB.Header.Get("x-freebuff-served-model"); got != "deepseek/deepseek-v4-flash" {
		t.Errorf("fallback x-freebuff-served-model = %q, want deepseek/deepseek-v4-flash", got)
	}
	if got := respFB.Header.Get("x-freebuff-fallback"); got != "quota_exhausted" {
		t.Errorf("fallback x-freebuff-fallback = %q, want quota_exhausted", got)
	}
	foundFbStart := false
	for _, ev := range parseAnthropicEvents(t, string(dataFB)) {
		if ev.Type == "message_start" && ev.Message != nil {
			foundFbStart = true
			if ev.Message.Model != "deepseek/deepseek-v4-flash" {
				t.Errorf("fallback message_start model = %q, want deepseek/deepseek-v4-flash", ev.Message.Model)
			}
		}
	}
	if !foundFbStart {
		t.Error("message_start event not found in fallback stream")
	}

	// Non-streaming fallback: message object model names the served model.
	reqBodyGlmNS := `{"model":"z-ai/glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":false}`
	respFBNS, dataFBNS := doJSON(t, http.MethodPost, srvFB.URL+"/v1/messages", []byte(reqBodyGlmNS),
		map[string]string{"anthropic-api-key": "anthropic-key", "anthropic-version": "2023-06-01"})
	if respFBNS.StatusCode != http.StatusOK {
		t.Fatalf("fallback non-stream status = %d, want 200: %s", respFBNS.StatusCode, dataFBNS)
	}
	var msgFB struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(dataFBNS, &msgFB); err != nil {
		t.Fatalf("unmarshal fallback message: %v: %s", err, dataFBNS)
	}
	if msgFB.Model != "deepseek/deepseek-v4-flash" {
		t.Errorf("fallback non-stream message model = %q, want deepseek/deepseek-v4-flash", msgFB.Model)
	}
	if got := respFBNS.Header.Get("x-freebuff-fallback"); got != "quota_exhausted" {
		t.Errorf("fallback non-stream x-freebuff-fallback = %q, want quota_exhausted", got)
	}
}
