// Mock-9router bridge tests: 9router speaks to the gateway as an
// Anthropic-compatible client (anthropic-version header, /v1/messages) while
// the gateway runs in bridge mode (no AUTH_TOKENS). Each test pins one
// observable of that pairing so a 9router-shaped failure stays diagnosable:
//
//   - passthrough: a client FreeBuff token in Authorization reaches upstream
//     untouched, and the bridge entry is reused across requests.
//   - key swap: 9router replacing Authorization with its own key is NOT
//     validated locally; the garbage token goes upstream and the upstream
//     rejection surfaces. NOTE: today that surfaces as 502/api_error with
//     code upstream_auth_rejected, not 401: the rejection is classified as
//     an upstream failure, not a client credential error. If that status is
//     ever deemed wrong it is a contract change (needs ADR), not a test fix.
//   - routed name: 9router's internal routing prefix (fb/...) is not a
//     served model id. The gateway answers 400 before touching upstream, so
//     9router must strip its prefix before forwarding (which it does).
//   - upstream 429: the #351 shape. A per-minute upstream throttle relays
//     as 429 + rate_limit_error with the Retry-After preserved; the bridge
//     entry is not poisoned by it.
package server_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

const nineRouterMessagesBody = `{"model":"deepseek/deepseek-v4-flash","max_tokens":256,"messages":[{"role":"user","content":"ping"}],"stream":true}`

const nineRouterRoutedModelBody = `{"model":"fb/deepseek/deepseek-v4-flash","max_tokens":256,"messages":[{"role":"user","content":"ping"}],"stream":true}`

// nineRouterHeaders emulates what 9router forwards on its Anthropic-compatible
// provider route: JSON body, the configured credential as Bearer, and the
// Anthropic version header (which selects our Anthropic error envelope).
func nineRouterHeaders(token string) map[string]string {
	return map[string]string{
		"Content-Type":      "application/json",
		"Authorization":     "Bearer " + token,
		"anthropic-version": "2023-06-01",
	}
}

func TestBridgeNineRouterPassthrough(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-9r1", 1, `"choices":[{"index":0,"delta":{"content":"nine","role":"assistant"},"finish_reason":null}]`))
	ts, _ := newBridgeTestServer(t, mock)
	url := ts.URL + "/v1/messages"

	resp, data := doJSON(t, http.MethodPost, url, []byte(nineRouterMessagesBody), nineRouterHeaders("fb-live-token-1"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "nine") {
		t.Errorf("stream missing relayed content: %s", data)
	}
	if len(mock.RecordedChatHeaders) != 1 {
		t.Fatalf("upstream chat calls = %d, want 1", len(mock.RecordedChatHeaders))
	}
	if got := mock.RecordedChatHeaders[0].Get("Authorization"); got != "Bearer fb-live-token-1" {
		t.Errorf("upstream Authorization = %q, want client token relayed untouched", got)
	}
	if got := mock.SessionCreatesSnapshot(); got != 1 {
		t.Fatalf("session creates = %d, want 1", got)
	}

	// Same token again: the bridge entry is reused, no new upstream session.
	resp2, data2 := doJSON(t, http.MethodPost, url, []byte(nineRouterMessagesBody), nineRouterHeaders("fb-live-token-1"))
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second request status = %d, want 200: %s", resp2.StatusCode, data2)
	}
	if got := mock.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("session creates after reuse = %d, want 1 (entry cached per token)", got)
	}
}

func TestBridgeNineRouterKeySwap(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Upstream rejects the unknown credential at session admission.
	var attempts int
	var upstreamAuth string
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		attempts++
		upstreamAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"invalid_token","message":"unknown credential"}}`)
	}
	ts, _ := newBridgeTestServer(t, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(nineRouterMessagesBody), nineRouterHeaders("9r-sk-garbage"))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (upstream rejection, not client 401): %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "upstream_auth_rejected") {
		t.Errorf("body missing upstream_auth_rejected: %s", data)
	}
	if !strings.Contains(string(data), "api_error") {
		t.Errorf("body missing Anthropic api_error type: %s", data)
	}
	// Lazy bridge: exactly one admission attempt, and the presented
	// credential went upstream verbatim (no local validation, no rewrite).
	if attempts != 1 {
		t.Errorf("session attempts = %d, want 1", attempts)
	}
	if upstreamAuth != "Bearer 9r-sk-garbage" {
		t.Errorf("upstream Authorization = %q, want garbage bearer forwarded untouched", upstreamAuth)
	}
}

func TestBridgeNineRouterRoutedModelName(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newBridgeTestServer(t, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(nineRouterRoutedModelBody), nineRouterHeaders("fb-live-token-1"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (fb/ prefix is not a served id): %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "fb/deepseek/deepseek-v4-flash") {
		t.Errorf("body should echo the rejected id: %s", data)
	}
	if got := mock.SessionCreatesSnapshot(); got != 0 {
		t.Errorf("session creates = %d, want 0 (rejected before pool)", got)
	}
}

func TestBridgeNineRouterUpstream429Relay(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// The #351 shape: upstream answers the chat relay with a per-minute
	// throttle carrying an explicit Retry-After.
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limited","message":"upstream rate limited (retry after 1m0s): quota exhausted"}}`)
	}
	ts, _ := newBridgeTestServer(t, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(nineRouterMessagesBody), nineRouterHeaders("fb-live-token-1"))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "rate_limit_error") {
		t.Errorf("body missing Anthropic rate_limit_error type: %s", data)
	}
	if !strings.Contains(string(data), "rate_limited") {
		t.Errorf("body missing upstream rate_limited code: %s", data)
	}
	if got := resp.Header.Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q, want upstream value preserved", got)
	}
}
