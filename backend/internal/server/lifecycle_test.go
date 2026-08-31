// lifecycle_test.go — user-lifecycle end-to-end journey.
//
// One test walks the live HTTP surface through every lifecycle phase a
// fresh operator hits, against an in-process server + testutil.MockUpstream:
// boot → admin login → add a cb_ token → OpenAI stream + non-stream →
// Anthropic stream + count_tokens → config edit (valid, then rejected with
// rollback) → monitor (overview + models quota) → remove token (by index,
// then out-of-range) → reload → metrics counters. Shared state across steps
// is the point: the pool ↔ .env ↔ config invariants can only be pinned on a
// live journey, in the exact order an operator experiences them.
//
// FINDING RECORDED (no prod change made): the dashboard add-token path
// builds the new token's upstream client from the POOL config
// (pool.AddToken → buildTokenEntry → upstream.New(..., p.cfg)), while the
// validity probe (pool.ProbeNewToken) matches the FIRST pooled client's
// base URL when one exists. Where the pool config's UPSTREAM_BASE_URL
// differs from the live clients' base URL (test harnesses; any future
// multi-upstream setup), the probe would validate against one host and the
// runtime-added token would be used against another — a silent
// probe/use asymmetry. Default production deployments are unaffected (both
// are https://www.codebuff.com). The harness below sidesteps it the same
// way server_hybrid_test.go's helper does: the pool config URL is patched
// to the mock so the runtime-added token is built against the mock too.
package server_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
)

// lifecycleToken is a valid-shaped cb_ FreeBuff token (the config validator
// rejects only the well-known placeholders, never "cb_" + a real suffix).
const lifecycleToken = "cb_lifecycle_0123456789abcdef"

func TestLifecycleFullJourney(t *testing.T) {
	t.Chdir(t.TempDir())

	// The operator's .env is the source of truth: every dashboard save /
	// /admin/reload re-resolves config through config.Load, which also
	// re-applies ADMIN_TOKEN from .env (the in-memory boot config is only
	// the initial state). Seed the realistic first-boot .env so the
	// reloads keep the same admin credential instead of reverting to the
	// factory default.
	seedEnv := []byte("AUTH_TOKENS=tok-0\nADMIN_TOKEN=secret\n")
	if err := os.WriteFile(".env", seedEnv, 0o600); err != nil {
		t.Fatal(err)
	}

	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-lc", 1,
		`"choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-lc", 1,
			`"choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-lc", 1,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`))

	// Hybrid (the production default with AUTH_TOKENS set) with no client
	// API keys: open-pooled, every request served from the token pool.
	// The mut below also points the POOL config at the mock, so a
	// runtime-added token's client is built against the mock (see the
	// finding above).
	ts, p := newTestServerCfg(t, nil, func(c *config.Config) {
		c.BridgeEnabled = true
		c.UpstreamBaseURL = mock.URL()
		c.AdminToken = "secret"
	}, mock)

	t.Run("boot", func(t *testing.T) {
		resp, data := doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
		}
		var hz struct {
			Status string `json:"status"`
			Mode   string `json:"mode"`
			Models int    `json:"models"`
			Tokens []struct {
				Token int `json:"Token"`
			} `json:"tokens"`
		}
		if err := json.Unmarshal(data, &hz); err != nil {
			t.Fatalf("healthz not JSON: %v: %s", err, data)
		}
		if hz.Status != "ok" || hz.Mode != "hybrid" {
			t.Errorf("healthz status/mode = %q/%q, want ok/hybrid (AUTH_TOKENS set + BRIDGE_ENABLED)", hz.Status, hz.Mode)
		}
		if hz.Models != 6 {
			t.Errorf("healthz models = %d, want 6", hz.Models)
		}
		if len(hz.Tokens) != 1 {
			t.Errorf("healthz tokens = %d, want 1 at boot", len(hz.Tokens))
		}

		resp, data = doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("/v1/models status = %d, want 200: %s", resp.StatusCode, data)
		}
		var ml struct {
			Data []struct {
				ID      string `json:"id"`
				Object  string `json:"object"`
				OwnedBy string `json:"owned_by"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &ml); err != nil {
			t.Fatalf("/v1/models not JSON: %v: %s", err, data)
		}
		if len(ml.Data) != 6 {
			t.Fatalf("/v1/models count = %d, want 6", len(ml.Data))
		}
		found := false
		for _, m := range ml.Data {
			if m.ID == modelA {
				found = true
			}
			if m.ID == "" || m.Object != "model" || m.OwnedBy == "" {
				t.Errorf("model row incomplete: %+v", m)
			}
		}
		if !found {
			t.Errorf("/v1/models missing %s", modelA)
		}
	})

	// Admin login: the boot step of dashboard operations.
	cookie := authedCookie(t, ts)

	t.Run("add-token", func(t *testing.T) {
		probesBefore := mock.SessionProbesSnapshot()
		resp := postJSON(t, ts.URL, cookie, "/admin/tokens/add",
			`{"token":"`+lifecycleToken+`"}`)
		body := bodyOf(t, resp)
		if !strings.Contains(body, "Token added at index 1") {
			t.Fatalf("add response = %q, want success at index 1", body)
		}
		if got := p.TokenCount(); got != 2 {
			t.Fatalf("pool TokenCount = %d, want 2", got)
		}
		// The probe ran against the mock (hermetic), not the real upstream.
		if got := mock.SessionProbesSnapshot(); got != probesBefore+1 {
			t.Errorf("probe count = %d, want %d (add must validate via the mock)", got, probesBefore+1)
		}
		env, err := os.ReadFile(".env")
		if err != nil {
			t.Fatalf(".env not written: %v", err)
		}
		if !strings.Contains(string(env), "AUTH_TOKENS=tok-0,"+lifecycleToken) {
			t.Errorf(".env AUTH_TOKENS wrong: %s", env)
		}
	})

	t.Run("use-openai", func(t *testing.T) {
		// Stream: SSE with an injected leading role chunk, content deltas,
		// terminal finish_reason, then [DONE].
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions",
			chatBody(modelA), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream status = %d, want 200: %s", resp.StatusCode, data)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
			t.Errorf("stream Content-Type = %q, want text/event-stream", ct)
		}
		body := string(data)
		if first := firstDataLine(body); !strings.Contains(first, `"role":"assistant"`) {
			t.Errorf("first chunk must carry role assistant: %s", first)
		}
		for _, want := range []string{`"content":"Hello"`, `"content":" world"`, `"finish_reason":"stop"`} {
			if !strings.Contains(body, want) {
				t.Errorf("stream missing %s: %s", want, body)
			}
		}
		if !strings.HasSuffix(body, "data: [DONE]\n\n") {
			t.Errorf("stream must end with [DONE]: %q", body)
		}
		if got := resp.Header.Get("x-freebuff-served-model"); got != modelA {
			t.Errorf("x-freebuff-served-model = %q, want %q", got, modelA)
		}

		// Non-stream: assembled JSON with the joined content.
		req := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"stream":false}`
		resp, data = doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(req), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("non-stream status = %d, want 200: %s", resp.StatusCode, data)
		}
		var out struct {
			Object  string `json:"object"`
			Model   string `json:"model"`
			Choices []struct {
				Message struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("non-stream not JSON: %v: %s", err, data)
		}
		if out.Object != "chat.completion" || out.Model != modelA {
			t.Errorf("object/model = %q/%q, want chat.completion/%s", out.Object, out.Model, modelA)
		}
		if len(out.Choices) != 1 || out.Choices[0].Message.Content != "Hello world" {
			t.Errorf("choices = %+v, want one message with content %q", out.Choices, "Hello world")
		}
	})

	t.Run("use-anthropic", func(t *testing.T) {
		antHeaders := map[string]string{"anthropic-version": "2023-06-01"}

		// Stream: the Anthropic event lifecycle in order, ending with a
		// terminal stop_reason.
		reqBody := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"stream":true}`
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(reqBody), antHeaders)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("anthropic stream status = %d, want 200: %s", resp.StatusCode, data)
		}
		types, stopReasons := anthropicEventTypes(t, string(data))
		want := []string{"message_start", "content_block_start", "content_block_delta",
			"content_block_stop", "message_delta", "message_stop"}
		lastPos := -1
		for _, typ := range want {
			pos := -1
			for i, got := range types {
				if got == typ {
					pos = i
					break
				}
			}
			if pos < 0 {
				t.Fatalf("stream missing %s event: %v", typ, types)
			}
			if pos < lastPos {
				t.Errorf("%s out of order (pos %d after %d): %v", typ, pos, lastPos, types)
			}
			lastPos = pos
		}
		if got := stopReasons["message_delta"]; got != "end_turn" {
			t.Errorf("message_delta stop_reason = %q, want end_turn", got)
		}

		// Non-stream: message object with stop_reason.
		reqNS := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"stream":false}`
		respNS, dataNS := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(reqNS), antHeaders)
		if respNS.StatusCode != http.StatusOK {
			t.Fatalf("anthropic non-stream status = %d, want 200: %s", respNS.StatusCode, dataNS)
		}
		var msg struct {
			Type       string `json:"type"`
			Model      string `json:"model"`
			StopReason string `json:"stop_reason"`
			Content    []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(dataNS, &msg); err != nil {
			t.Fatalf("anthropic non-stream not JSON: %v: %s", err, dataNS)
		}
		if msg.Type != "message" || msg.Model != modelA || msg.StopReason != "end_turn" {
			t.Errorf("message = %+v, want type=message model=%s stop_reason=end_turn", msg, modelA)
		}
		if len(msg.Content) != 1 || msg.Content[0].Type != "text" || msg.Content[0].Text != "Hello world" {
			t.Errorf("content = %+v, want one text block %q", msg.Content, "Hello world")
		}

		// count_tokens: purely local, a positive count, zero upstream contact.
		chatsBefore := len(mock.RecordedChatBodies)
		ctBody := []byte(`{"model":"` + modelA + `","messages":[{"role":"user","content":"hello"}]}`)
		respCT, dataCT := doJSON(t, http.MethodPost, ts.URL+"/v1/messages/count_tokens", ctBody, antHeaders)
		if respCT.StatusCode != http.StatusOK {
			t.Fatalf("count_tokens status = %d, want 200: %s", respCT.StatusCode, dataCT)
		}
		var ct struct {
			InputTokens int `json:"input_tokens"`
		}
		if err := json.Unmarshal(dataCT, &ct); err != nil {
			t.Fatalf("count_tokens not JSON: %v: %s", err, dataCT)
		}
		if ct.InputTokens <= 0 {
			t.Errorf("input_tokens = %d, want > 0", ct.InputTokens)
		}
		// count_tokens must not add an upstream CHAT call (the session
		// maintain loop legitimately polls, so chat bodies are the stable
		// counter here).
		if got := len(mock.RecordedChatBodies); got != chatsBefore {
			t.Errorf("count_tokens added %d upstream chat call(s), want 0 (purely local)", got-chatsBefore)
		}
	})

	t.Run("metrics-after-use", func(t *testing.T) {
		resp, data := doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("metrics status = %d, want 200: %s", resp.StatusCode, data)
		}
		body := string(data)
		for _, want := range []string{
			"freebuff_proxy_models_total 6",
			"freebuff_proxy_tokens_total 2",
			"freebuff_proxy_token_requests_total{token=\"1\"} 4",
			"freebuff_proxy_token_messages_24h{token=\"1\"} 4",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("metrics missing %s in:\n%s", want, body)
			}
		}
	})

	t.Run("config-edit", func(t *testing.T) {
		cfResp := get(t, ts.URL+"/admin/api/config", cookie)
		var cfgData struct {
			EnvContent string `json:"env_content"`
			HasEnvFile bool   `json:"has_env_file"`
		}
		if err := json.Unmarshal([]byte(bodyOf(t, cfResp)), &cfgData); err != nil {
			t.Fatalf("config API not JSON: %v", err)
		}
		if !cfgData.HasEnvFile {
			t.Fatal("config API: no .env file after the token add")
		}
		if !strings.Contains(cfgData.EnvContent, "AUTH_TOKENS=tok-0,"+lifecycleToken) {
			t.Fatalf("config API env_content lost the token: %q", cfgData.EnvContent)
		}

		// Valid edit: LOG_LEVEL change applies and survives.
		valid := strings.TrimRight(cfgData.EnvContent, "\n") + "\nLOG_LEVEL=debug\n"
		resp := postConfig(t, ts.URL, cookie, valid)
		body := bodyOf(t, resp)
		if !strings.Contains(body, "Saved and reloaded") {
			t.Fatalf("valid config save = %q, want Saved and reloaded", body)
		}
		env, err := os.ReadFile(".env")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(env), "LOG_LEVEL=debug") ||
			!strings.Contains(string(env), "AUTH_TOKENS=tok-0,"+lifecycleToken) {
			t.Errorf(".env after valid save wrong: %s", env)
		}
		if got := p.TokenCount(); got != 2 {
			t.Errorf("pool TokenCount after config save = %d, want 2 (tokens preserved)", got)
		}

		// Rejected edit: invalid LOG_LEVEL — the save is refused and the
		// previous .env content is restored byte-exact.
		before, err := os.ReadFile(".env")
		if err != nil {
			t.Fatal(err)
		}
		bad := strings.Replace(string(before), "LOG_LEVEL=debug", "LOG_LEVEL=bogus", 1)
		resp = postConfig(t, ts.URL, cookie, bad)
		body = bodyOf(t, resp)
		if !strings.Contains(body, "Configuration rejected") || !strings.Contains(body, "LOG_LEVEL") {
			t.Fatalf("invalid config save = %q, want rejection naming LOG_LEVEL", body)
		}
		after, err := os.ReadFile(".env")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, before) {
			t.Errorf(".env not rolled back after rejected save:\nbefore: %s\nafter:  %s", before, after)
		}
		if got := p.TokenCount(); got != 2 {
			t.Errorf("pool TokenCount after rejected save = %d, want 2", got)
		}
	})

	t.Run("monitor", func(t *testing.T) {
		ovResp := get(t, ts.URL+"/admin/api/overview", cookie)
		var ov struct {
			Mode       string `json:"mode"`
			ModelCount int    `json:"model_count"`
			HasTokens  bool   `json:"has_tokens"`
			Tokens     []any  `json:"tokens"`
		}
		if err := json.Unmarshal([]byte(bodyOf(t, ovResp)), &ov); err != nil {
			t.Fatalf("overview not JSON: %v", err)
		}
		if ov.Mode != "hybrid" || ov.ModelCount != 6 || !ov.HasTokens || len(ov.Tokens) != 2 {
			t.Errorf("overview = %+v, want mode=hybrid models=6 has_tokens with 2 token cards", ov)
		}

		// The models list carries a per-model quota label.
		mdResp := get(t, ts.URL+"/admin/api/models", cookie)
		var md struct {
			Count  int `json:"count"`
			Models []struct {
				ID    string `json:"id"`
				Agent string `json:"agent"`
				Quota string `json:"quota"`
			} `json:"models"`
		}
		if err := json.Unmarshal([]byte(bodyOf(t, mdResp)), &md); err != nil {
			t.Fatalf("models API not JSON: %v", err)
		}
		if md.Count != 6 || len(md.Models) != 6 {
			t.Fatalf("models API count = %d/%d, want 6", md.Count, len(md.Models))
		}
		allowed := map[string]bool{"unlimited session": true, "5 premium quota": true, "referral +1/day": true, "unmetered": true, "shared premium pool": true, "5/day shared premium": true}
		for _, m := range md.Models {
			if m.ID == "" || m.Agent == "" || !allowed[m.Quota] {
				t.Errorf("model row %+v: id/agent/quota must be populated from the catalog", m)
			}
		}
	})

	t.Run("remove-token", func(t *testing.T) {
		// Index removal: tok-0 leaves the pool and the .env; the added
		// token survives (values stay masked client-side in the SPA, so
		// the operation is by index).
		resp := postTokenAction(t, ts.URL, cookie, "/admin/tokens/remove", "0")
		body := bodyOf(t, resp)
		if !strings.Contains(body, "Token removed") {
			t.Fatalf("remove response = %q, want success", body)
		}
		if got := p.TokenCount(); got != 1 {
			t.Fatalf("pool TokenCount = %d, want 1", got)
		}
		env, err := os.ReadFile(".env")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(env), "tok-0") {
			t.Errorf(".env still contains the removed token: %s", env)
		}
		if !strings.Contains(string(env), "AUTH_TOKENS="+lifecycleToken) {
			t.Errorf(".env missing the remaining token: %s", env)
		}

		// Out-of-range removal: plain rejection, pool and .env untouched.
		resp = postTokenAction(t, ts.URL, cookie, "/admin/tokens/remove", "5")
		body = bodyOf(t, resp)
		if !strings.Contains(body, "Invalid token index") {
			t.Fatalf("out-of-range remove = %q, want index rejection", body)
		}
		if got := p.TokenCount(); got != 1 {
			t.Fatalf("pool TokenCount after bad remove = %d, want 1", got)
		}
		env2, err := os.ReadFile(".env")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(env2, env) {
			t.Errorf(".env changed after rejected remove: %s", env2)
		}
	})

	t.Run("reload-healthy", func(t *testing.T) {
		// /admin/reload is guarded by the ADMIN_TOKEN itself (not the
		// session cookie): Authorization: Bearer <token>.
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/admin/reload", nil,
			map[string]string{"Authorization": "Bearer secret"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("reload status = %d, want 200: %s", resp.StatusCode, data)
		}
		body := string(data)
		if !strings.Contains(body, `"status":"ok"`) || !strings.Contains(body, `"auth_tokens":1`) {
			t.Errorf("reload response = %s, want status ok with auth_tokens 1", body)
		}

		// Still healthy after the journey: hybrid, 6 models, 1 token.
		resp, data = doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("healthz after reload = %d, want 200: %s", resp.StatusCode, data)
		}
		var hz struct {
			Status string `json:"status"`
			Mode   string `json:"mode"`
			Models int    `json:"models"`
			Tokens []any  `json:"tokens"`
		}
		if err := json.Unmarshal(data, &hz); err != nil {
			t.Fatalf("healthz not JSON: %v: %s", err, data)
		}
		if hz.Status != "ok" || hz.Mode != "hybrid" || hz.Models != 6 || len(hz.Tokens) != 1 {
			t.Errorf("healthz = %+v, want ok/hybrid/6/1 token after reload", hz)
		}

		// Final metrics counters: the removed first token's counters are
		// gone with it; the surviving (added) token shows zero use.
		resp, data = doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("metrics status = %d, want 200: %s", resp.StatusCode, data)
		}
		metrics := string(data)
		for _, want := range []string{
			"freebuff_proxy_models_total 6",
			"freebuff_proxy_tokens_total 1",
			"freebuff_proxy_token_requests_total{token=\"1\"} 0",
		} {
			if !strings.Contains(metrics, want) {
				t.Errorf("metrics missing %s in:\n%s", want, metrics)
			}
		}
	})
}

// firstDataLine returns the first SSE "data: " payload of a stream body.
func firstDataLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") {
			return line
		}
	}
	return ""
}

// anthropicEventTypes extracts the ordered event types of an Anthropic SSE
// stream plus the stop_reason carried by any delta event.
func anthropicEventTypes(t *testing.T, body string) ([]string, map[string]string) {
	t.Helper()
	var types []string
	stopReasons := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta *struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("unmarshal anthropic event: %v: %s", err, line)
		}
		types = append(types, ev.Type)
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			stopReasons[ev.Type] = ev.Delta.StopReason
		}
	}
	return types, stopReasons
}
