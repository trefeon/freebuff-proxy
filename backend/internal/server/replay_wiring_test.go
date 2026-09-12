package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// Wiring replays: every new classify arm must surface the same client code on
// both the OpenAI envelope (/v1/chat/completions) and the Anthropic envelope
// (/v1/messages). Each test drives a real upstream refusal through the mock
// into the client response.

// TestChatHierarchyGateSurfaced403 replays the hierarchy-gate refusal: a 403
// free_mode_invalid_agent_hierarchy body must reach the OpenAI client as 403
// free_mode_invalid_agent_hierarchy with an actionable hint — never the dead
// 502 the default branch used to write.
func TestChatHierarchyGateSurfaced403(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"free_mode_invalid_agent_hierarchy","message":"subagent inline not in root allowlist"}`)
	}
	ts, _ := newTestServerCfg(t, nil, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", resp.StatusCode, data)
	}
	if got := errorCode(t, data); got != "free_mode_invalid_agent_hierarchy" {
		t.Errorf("code = %q, want free_mode_invalid_agent_hierarchy: %s", got, data)
	}
	if !strings.Contains(string(data), "allowlist") {
		t.Errorf("body missing the hierarchy hint: %s", data)
	}
	if !strings.Contains(string(data), "subagent inline not in root allowlist") {
		t.Errorf("body missing the upstream message: %s", data)
	}
}

// TestReplayMessagesHierarchyGate replays the same refusal on the Anthropic
// surface: identical 403 + code, in the Anthropic envelope
// (permission_error / free_mode_invalid_agent_hierarchy).
func TestReplayMessagesHierarchyGate(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"free_mode_invalid_agent_hierarchy","message":"subagent inline not in root allowlist"}`)
	}
	ts, _ := newTestServerCfg(t, []string{"replay-key"}, nil, mock)
	headers := map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         "replay-key",
		"anthropic-version": "2023-06-01",
	}
	body := `{"model":"deepseek/deepseek-v4-flash","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), headers)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("403 response is not parseable: %v: %s", err, data)
	}
	if envelope.Type != "error" {
		t.Errorf("top-level type = %q, want error (Anthropic envelope)", envelope.Type)
	}
	if envelope.Error.Type != "permission_error" {
		t.Errorf("error.type = %q, want permission_error", envelope.Error.Type)
	}
	if envelope.Error.Code != "free_mode_invalid_agent_hierarchy" {
		t.Errorf("error.code = %q, want free_mode_invalid_agent_hierarchy", envelope.Error.Code)
	}
}

// TestChatPeakHoursUnderscoreSurfaced429 replays the underscore body form: a
// 429 carrying peak_hours must surface exactly like the space form — 429
// peak_hours with the bounded 30m Retry-After, never generic rate_limited.
func TestChatPeakHoursUnderscoreSurfaced429(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"status":"rate_limited","message":"Usage is temporarily limited during peak_hours, prices double"}`)
	}
	ts, _ := newTestServerCfg(t, nil, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", resp.StatusCode, data)
	}
	if got := errorCode(t, data); got != "peak_hours" {
		t.Errorf("code = %q, want peak_hours: %s", got, data)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "1800" {
		t.Errorf("Retry-After = %q, want 1800 (bounded 30m, not midnight)", ra)
	}
}

// TestReplayMessagesPeakHoursUnderscore replays the same body on the
// Anthropic surface: 429 + rate_limit_error / peak_hours.
func TestReplayMessagesPeakHoursUnderscore(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"status":"rate_limited","message":"Usage is temporarily limited during peak_hours, prices double"}`)
	}
	ts, _ := newTestServerCfg(t, []string{"replay-key"}, nil, mock)
	headers := map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         "replay-key",
		"anthropic-version": "2023-06-01",
	}
	body := `{"model":"deepseek/deepseek-v4-flash","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), headers)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("429 response is not parseable: %v: %s", err, data)
	}
	if envelope.Error.Type != "rate_limit_error" {
		t.Errorf("error.type = %q, want rate_limit_error", envelope.Error.Type)
	}
	if envelope.Error.Code != "peak_hours" {
		t.Errorf("error.code = %q, want peak_hours", envelope.Error.Code)
	}
}

// TestChatTurnSpendOffStatusSurfaced429 replays the breaker on a non-429
// status: the turn_spend_limit literal is terminal whatever carries it, so a
// 403 carrying it must still surface as 429 turn_spend_limited with the loop
// warning and NO Retry-After — never the 502 the default branch wrote.
func TestChatTurnSpendOffStatusSurfaced429(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var chatCalls atomic.Int32
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"turn_spend_limit","message":"Something went wrong with this turn.","retryAfterMs":60000}`)
	}
	ts, _ := newTestServerCfg(t, nil, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", resp.StatusCode, data)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		t.Errorf("Retry-After = %q, want none (no retry drumbeat for a killed turn)", ra)
	}
	if !strings.Contains(string(data), `"code":"turn_spend_limited"`) {
		t.Errorf("body missing turn_spend_limited code: %s", data)
	}
	if !strings.Contains(string(data), "Something went wrong with this turn") {
		t.Errorf("body missing the upstream loop warning: %s", data)
	}
	if got := chatCalls.Load(); got != 1 {
		t.Errorf("upstream chat calls = %d, want 1 (never re-POST into a turn-spend refusal)", got)
	}
}

// TestReplayMessagesTurnSpendOffStatus replays the same off-status breaker on
// the Anthropic surface: identical terminal framing in the Anthropic
// envelope, with NO Retry-After.
func TestReplayMessagesTurnSpendOffStatus(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var mu sync.Mutex
	chatCalls := 0
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		chatCalls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"turn_spend_limit","message":"Something went wrong with this turn.","retryAfterMs":60000}`)
	}
	ts, _ := newTestServerCfg(t, []string{"replay-key"}, nil, mock)
	headers := map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         "replay-key",
		"anthropic-version": "2023-06-01",
	}
	body := `{"model":"deepseek/deepseek-v4-flash","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), headers)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("429 response is not parseable: %v: %s", err, data)
	}
	if envelope.Error.Type != "rate_limit_error" {
		t.Errorf("error.type = %q, want rate_limit_error", envelope.Error.Type)
	}
	if envelope.Error.Code != "turn_spend_limited" {
		t.Errorf("error.code = %q, want turn_spend_limited", envelope.Error.Code)
	}
	if !strings.Contains(envelope.Error.Message, "Something went wrong with this turn") {
		t.Errorf("error.message missing the upstream loop warning: %q", envelope.Error.Message)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		t.Errorf("Retry-After = %q, want none (no retry drumbeat for a killed turn)", ra)
	}
	mu.Lock()
	defer mu.Unlock()
	if chatCalls != 1 {
		t.Errorf("upstream chat calls = %d, want 1 (never re-POST into a turn-spend refusal)", chatCalls)
	}
}

// TestChatVendorUICopyStaysDefault502 pins the no-literal rule end to end: a
// purchase-flavored refusal with no wire status literal must surface as the
// default 502 upstream_unavailable — never a typed code.
func TestChatVendorUICopyStaysDefault502(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = io.WriteString(w, `{"error":"purchase_required","message":"No purchase was made"}`)
	}
	ts, _ := newTestServerCfg(t, nil, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", resp.StatusCode, data)
	}
	if got := errorCode(t, data); got != "upstream_unavailable" {
		t.Errorf("code = %q, want upstream_unavailable: %s", got, data)
	}
}
