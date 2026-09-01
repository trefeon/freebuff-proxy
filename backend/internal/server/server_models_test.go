package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/logring"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/registry"
	"freebuff-proxy/backend/internal/server"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// flakyFirstRT fails the very first request with a transient transport error
// and delegates everything else to base (mirrors pool_test's helper; drives a
// real retry deterministically across platforms).
type flakyFirstRT struct {
	mu     sync.Mutex
	failed bool
	base   http.RoundTripper
}

func (f *flakyFirstRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	shouldFail := !f.failed
	if shouldFail {
		f.failed = true
	}
	f.mu.Unlock()
	if shouldFail {
		return nil, fmt.Errorf("read tcp 127.0.0.1:443: connection reset by peer")
	}
	return f.base.RoundTrip(req)
}

type quotaEntry struct {
	Limit       float64            `json:"limit"`
	RecentCount float64            `json:"recent_count"`
	Period      string             `json:"period"`
	ResetAt     string             `json:"reset_at"`
	Entitlement map[string]float64 `json:"entitlement"`
}

func TestUnknownModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	for _, tc := range []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"unknown_model", `{"model":"no/such-model","messages":[{"role":"user","content":"hi"}]}`, http.StatusBadRequest, "model_unavailable"},
		{"missing_model", `{"messages":[{"role":"user","content":"hi"}]}`, http.StatusBadRequest, "model_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(tc.body), nil)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.wantStatus, data)
			}
			if !strings.Contains(string(data), tc.wantCode) {
				t.Errorf("body missing %s: %s", tc.wantCode, data)
			}
		})
	}

	// Rejected before the pool: the upstream must be untouched.
	if mock.SessionCreates != 0 {
		t.Errorf("upstream session creates = %d, want 0", mock.SessionCreates)
	}
}

func TestModelsEndpoint(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID        string `json:"id"`
			Object    string `json:"object"`
			Created   int64  `json:"created"`
			OwnedBy   string `json:"owned_by"`
			Available bool   `json:"available"`
			Status    string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, data)
	}
	if out.Object != "list" {
		t.Errorf("object = %q, want list", out.Object)
	}
	// Issue #189 (strict gate); 6→5 on 2026-08-23: luna-es dropped (upstream
	// reclassified it god-only/honeypot-class — vendor snapshot 0603bc1);
	// 5→6 on 2026-08-26: stealth/ox-alpha added (vendor cce4800);
	// 6→5 on 2026-08-28: ox-alpha paused, glm-5.3-flash added (vendor 5951772);
	// 5→6 on 2026-08-29: upstage/solar-pro4 served (vendor 87ef664);
	// 6→5 on 2026-08-31: z-ai/glm-5.2 paused, reward moved to glm-5.3-flash (vendor e557373, a5980e38e).
	if len(out.Data) != 5 {
		t.Errorf("models = %d, want 5", len(out.Data))
	}
	for i, m := range out.Data {
		if m.ID == "" || m.Object != "model" || m.OwnedBy == "" {
			t.Errorf("model %d malformed: %+v", i, m)
		}
		if m.Created != out.Data[0].Created {
			t.Errorf("model %d created = %d, want %d (pinned to server start)", i, m.Created, out.Data[0].Created)
		}
		// Advisory annotation: never hide a working model, so available is
		// true and status "unknown" when no session has reported anything.
		if !m.Available {
			t.Errorf("model %s available = false, want true (advisory default)", m.ID)
		}
		if m.Status == "" {
			t.Errorf("model %s status empty, want a status string", m.ID)
		}
	}
	if out.Data[0].Created <= 0 || out.Data[0].Created > time.Now().Unix() {
		t.Errorf("created = %d, not a plausible server-start timestamp", out.Data[0].Created)
	}
}

// codexModelInfoStrict mirrors the serde-required ModelInfo rows codex-rs
// demands (codex-rs/protocol/src/openai_models.rs:392-483; reference/
// harnesses/codex/WIRE-NOTES.md §8). DisallowUnknownFields decoding proves
// the wire rows are exactly this set — no legacy {id,object,…} fields leak in.
type codexModelInfoStrict struct {
	Slug                     string `json:"slug"`
	DisplayName              string `json:"display_name"`
	SupportedReasoningLevels []struct {
		Effort      string `json:"effort"`
		Description string `json:"description"`
	} `json:"supported_reasoning_levels"`
	ShellType        string `json:"shell_type"`
	Visibility       string `json:"visibility"`
	SupportedInAPI   bool   `json:"supported_in_api"`
	Priority         int    `json:"priority"`
	SupportVerbosity bool   `json:"support_verbosity"`
	TruncationPolicy struct {
		Mode  string `json:"mode"`
		Limit int64  `json:"limit"`
	} `json:"truncation_policy"`
	ExperimentalSupportedTools []string `json:"experimental_supported_tools"`
	InputModalities            []string `json:"input_modalities"`
	BaseInstructions           string   `json:"base_instructions"`
}

// TestConformanceCodexModelsStrictModelInfo pins the Codex /v1/models wire
// (reference/harnesses/codex/WIRE-NOTES.md §8): with a client_version query
// param (codex always sends it — codex-rs codex-api/src/endpoint/models.rs:
// 31-35) the endpoint must return strict ModelInfo rows under the
// {"models": […]} envelope. A minimal {id,object,…} shape fails codex serde
// and it silently falls back to its bundled catalog, hiding our model ids,
// so every serde-required field is asserted present per row.
func TestConformanceCodexModelsStrictModelInfo(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodGet, ts.URL+"/v1/models?client_version=codex-cli-0.150", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Models []codexModelInfoStrict `json:"models"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("codex ModelInfo decode failed (serde parity): %v: %s", err, data)
	}
	if len(out.Models) == 0 {
		t.Fatal("codex response carried no model rows")
	}
	// Presence check: every serde-required key must be in each raw row
	// (missing keys decode to zero values silently in struct decoding).
	var raw struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("raw decode: %v: %s", err, data)
	}
	required := []string{
		"slug", "display_name", "supported_reasoning_levels", "shell_type",
		"visibility", "supported_in_api", "priority", "support_verbosity",
		"truncation_policy", "experimental_supported_tools", "input_modalities",
		"base_instructions",
	}
	for i, row := range raw.Models {
		for _, k := range required {
			if _, ok := row[k]; !ok {
				t.Errorf("model %d missing serde-required %q", i, k)
			}
		}
	}
	// The codex slug set must be exactly the served ids of the legacy shape.
	resp2, data2 := doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("legacy models status = %d: %s", resp2.StatusCode, data2)
	}
	var legacy struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data2, &legacy); err != nil {
		t.Fatalf("legacy decode: %v: %s", err, data2)
	}
	legacyIDs := make(map[string]bool, len(legacy.Data))
	for _, m := range legacy.Data {
		legacyIDs[m.ID] = true
	}
	if len(out.Models) != len(legacy.Data) {
		t.Fatalf("codex rows = %d, legacy rows = %d, want equal", len(out.Models), len(legacy.Data))
	}
	for i, m := range out.Models {
		if m.Slug == "" {
			t.Errorf("model %d: slug empty", i)
			continue
		}
		if !legacyIDs[m.Slug] {
			t.Errorf("model %d: slug %q not in the legacy served set", i, m.Slug)
		}
		if m.DisplayName == "" {
			t.Errorf("model %s: display_name empty", m.Slug)
		}
		if len(m.SupportedReasoningLevels) == 0 {
			t.Errorf("model %s: supported_reasoning_levels empty (serde-required Vec)", m.Slug)
		}
		for _, lvl := range m.SupportedReasoningLevels {
			if lvl.Effort == "" || lvl.Description == "" {
				t.Errorf("model %s: empty reasoning preset %+v", m.Slug, lvl)
			}
		}
		if m.ShellType != "unified_exec" {
			t.Errorf("model %s: shell_type = %q, want unified_exec", m.Slug, m.ShellType)
		}
		if m.Visibility != "list" {
			t.Errorf("model %s: visibility = %q, want list", m.Slug, m.Visibility)
		}
		if !m.SupportedInAPI {
			t.Errorf("model %s: supported_in_api = false, want true", m.Slug)
		}
		if m.Priority != 0 {
			t.Errorf("model %s: priority = %d, want 0", m.Slug, m.Priority)
		}
		if !m.SupportVerbosity {
			t.Errorf("model %s: support_verbosity = false, want true", m.Slug)
		}
		if m.TruncationPolicy.Mode != "tokens" || m.TruncationPolicy.Limit <= 0 {
			t.Errorf("model %s: truncation_policy = %+v, want tokens with positive limit", m.Slug, m.TruncationPolicy)
		}
		if m.ExperimentalSupportedTools == nil {
			t.Errorf("model %s: experimental_supported_tools missing (deserializes nil)", m.Slug)
		}
		if len(m.InputModalities) != 2 || m.InputModalities[0] != "text" || m.InputModalities[1] != "image" {
			t.Errorf("model %s: input_modalities = %v, want [text image]", m.Slug, m.InputModalities)
		}
	}
}

// TestConformanceModelsWithoutClientVersionKeepsOpenAIShape pins zero
// regression for every non-Codex client: no client_version query param (or an
// empty one — only a real version string switches shapes) means the legacy
// {"object":"list","data":[…]} rows, and never a leaked codex-only field.
func TestConformanceModelsWithoutClientVersionKeepsOpenAIShape(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	for _, url := range []string{ts.URL + "/v1/models", ts.URL + "/v1/models?client_version="} {
		resp, data := doJSON(t, http.MethodGet, url, nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, want 200: %s", url, resp.StatusCode, data)
		}
		var out struct {
			Object string `json:"object"`
			Data   []struct {
				ID        string `json:"id"`
				Object    string `json:"object"`
				Created   int64  `json:"created"`
				OwnedBy   string `json:"owned_by"`
				Available bool   `json:"available"`
				Status    string `json:"status"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("%s not JSON: %v: %s", url, err, data)
		}
		if out.Object != "list" || len(out.Data) == 0 {
			t.Fatalf("%s object = %q, models = %d, want list/non-empty", url, out.Object, len(out.Data))
		}
		for i, m := range out.Data {
			if m.ID == "" || m.Object != "model" || m.OwnedBy == "" || !m.Available || m.Status == "" {
				t.Errorf("%s model %d malformed legacy row: %+v", url, i, m)
			}
			if m.Created != out.Data[0].Created {
				t.Errorf("%s model %d created = %d, want %d (pinned to server start)", url, i, m.Created, out.Data[0].Created)
			}
		}
		var raw struct {
			Object string                       `json:"object"`
			Data   []map[string]json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("%s raw decode: %v", url, err)
		}
		for i, row := range raw.Data {
			if _, ok := row["slug"]; ok {
				t.Errorf("%s model %d leaked codex slug into the OpenAI shape", url, i)
			}
			if _, ok := row["shell_type"]; ok {
				t.Errorf("%s model %d leaked codex shell_type into the OpenAI shape", url, i)
			}
		}
	}
}

// TestMaxVariantsBlocked pins issue #153: the -max ids are excluded from the
// ServedModels gate, so they are neither listed on /v1/models nor servable —
// upstream's session admission resolves them to mimo/mimo-v2.5 anyway.
func TestMaxVariantsBlocked(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	// /v1/models must not advertise any -max id.
	resp, data := doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("models is not JSON: %v: %s", err, data)
	}
	for _, m := range out.Data {
		if strings.HasSuffix(m.ID, "-max") {
			t.Errorf("/v1/models leaked blocked -max id %q", m.ID)
		}
	}

	// Chat requests for a -max id are rejected before touching upstream.
	for _, model := range []string{
		"deepseek/deepseek-v4-flash-max",
		"deepseek/deepseek-v4-pro-max",
		"openai/gpt-5.6-luna-max",
	} {
		resp, data = doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(model), nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("chat %s status = %d, want 400: %s", model, resp.StatusCode, data)
		}
	}
	if len(mock.RecordedChatHeaders) != 0 {
		t.Error("upstream chat recorded for a blocked -max model, want none")
	}
}

func TestHealthz(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	ts, _ := newTestServer(t, nil, mock0, mock1)

	resp, data := doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		UptimeSeconds float64 `json:"uptime_seconds"`
		Models        int     `json:"models"`
		Tokens        []any   `json:"tokens"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, data)
	}
	// Issue #189 strict count; 6→5 when luna-es was dropped (2026-08-23),
	// 5→6 when stealth/ox-alpha was added (2026-08-26),
	// 5→6 when upstage/solar-pro4 was served (2026-08-29); fable-5 stays
	// out (not actually reachable on free accounts).
	if out.Models != 5 {
		t.Errorf("models = %d, want 5", out.Models)
	}
	if len(out.Tokens) != 2 {
		t.Errorf("tokens = %d, want 2", len(out.Tokens))
	}
}

func TestMetricsEndpoint(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	ts, _ := newTestServer(t, nil, mock0)

	resp, data := doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	body := string(data)
	if !strings.Contains(body, "freebuff_proxy_uptime_seconds") || !strings.Contains(body, "freebuff_proxy_models_total") {
		t.Errorf("metrics missing expected keys: %s", body)
	}
}

// TestHealthzSpend pins the /healthz spend surface (issue #122): the ledger
// buckets fed by the chat feeder, the advisory MAX_SPEND_PER_DAY ceiling
// (SpendLimit), the capped SpendPct, and the SpendLimited refusal counter.
func TestHealthzSpend(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-s1", 1, `"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-s1", 1, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}`))
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.MaxSpendPerDay = 100 }, mock)

	req := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(req), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200: %s", resp.StatusCode, data)
	}

	// The spend feeder runs after the relay flushes the response, so the
	// first healthz read can race it (see waitSpend); poll briefly.
	deadline := time.Now().Add(waitSpendTimeout)
	var tok struct {
		Spend24h     int64 `json:"Spend24h"`
		SpendDay     int64 `json:"SpendDay"`
		SpendLimit   int64 `json:"SpendLimit"`
		SpendPct     int   `json:"SpendPct"`
		SpendLimited int   `json:"SpendLimited"`
	}
	for {
		resp, data = doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
		}
		var out struct {
			Tokens []struct {
				Spend24h     int64 `json:"Spend24h"`
				SpendDay     int64 `json:"SpendDay"`
				SpendLimit   int64 `json:"SpendLimit"`
				SpendPct     int   `json:"SpendPct"`
				SpendLimited int   `json:"SpendLimited"`
			} `json:"tokens"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("healthz is not JSON: %v: %s", err, data)
		}
		if len(out.Tokens) != 1 {
			t.Fatalf("tokens = %d, want 1", len(out.Tokens))
		}
		tok = out.Tokens[0]
		if tok.Spend24h == 13 && tok.SpendDay == 13 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("healthz spend did not reach 13/13 within %s (last: %d/%d)", waitSpendTimeout, tok.SpendDay, tok.Spend24h)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if tok.SpendLimit != 100 {
		t.Errorf("SpendLimit = %d, want 100 (MAX_SPEND_PER_DAY)", tok.SpendLimit)
	}
	if tok.SpendPct != 13 {
		t.Errorf("SpendPct = %d, want 13 (13 of 100)", tok.SpendPct)
	}
	if tok.SpendLimited != 0 {
		t.Errorf("SpendLimited = %d, want 0 (no upstream spend_limited refusals)", tok.SpendLimited)
	}
}

func TestHealthzQuota(t *testing.T) {
	mock := quotaMock(t)
	ts, _ := newTestServer(t, nil, mock)

	// A chat admits the session (which carries rateLimitsByModel).
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Tokens []struct {
			Quota       map[string]quotaEntry `json:"quota"`
			Entitlement map[string]float64    `json:"entitlement"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("healthz is not JSON: %v: %s", err, data)
	}
	if len(out.Tokens) != 1 {
		t.Fatalf("tokens = %d, want 1", len(out.Tokens))
	}
	q, ok := out.Tokens[0].Quota["z-ai/glm-5.2"]
	if !ok {
		t.Fatalf("healthz quota missing z-ai/glm-5.2: %+v", out.Tokens[0].Quota)
	}
	if q.Limit != 5 || q.RecentCount != 4 || q.Period != "pacific_day" {
		t.Errorf("quota = %+v, want limit=5 recent_count=4 period=pacific_day", q)
	}
	if q.ResetAt == "" {
		t.Error("reset_at missing from healthz quota entry")
	}
	if q.Entitlement["referral"] != 1 || q.Entitlement["streak"] != 3 {
		t.Errorf("entitlement = %+v, want referral=1 streak=3", q.Entitlement)
	}
	if len(out.Tokens[0].Entitlement) != 0 {
		t.Errorf("top-level entitlement = %+v, want omitted (empty)", out.Tokens[0].Entitlement)
	}
}

// TestModelsAnnotationWithQuota verifies /v1/models reflects a session that
// admitted a model: status becomes "available" once quotaByModel mentions it.
func TestModelsAnnotationWithQuota(t *testing.T) {
	mock := quotaMock(t)
	mock.CountryCode = "US"
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Data []struct {
			ID        string `json:"id"`
			Available bool   `json:"available"`
			Status    string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("models is not JSON: %v: %s", err, data)
	}
	var found *struct {
		ID        string `json:"id"`
		Available bool   `json:"available"`
		Status    string `json:"status"`
	}
	for i := range out.Data {
		if out.Data[i].ID == modelA {
			found = &out.Data[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("model %q not in /v1/models", modelA)
	}
	if !found.Available {
		t.Errorf("available = false, want true")
	}
	if found.Status != "available" {
		t.Errorf("status = %q, want available (session admitted the model)", found.Status)
	}
}

// TestModelsAllowList verifies MODELS_ALLOW prunes /v1/models to exactly the
// allowlisted ids so picker clients never auto-select a model that 404s.
func TestModelsAllowList(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.ModelsAllow = []string{"deepseek/deepseek-v4-flash", "openai/gpt-5.6-luna"}
	}, mock)

	resp, data := doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("models is not JSON: %v: %s", err, data)
	}
	seen := map[string]bool{}
	for _, m := range out.Data {
		if m.ID != "deepseek/deepseek-v4-flash" && m.ID != "openai/gpt-5.6-luna" {
			t.Errorf("model %q listed outside MODELS_ALLOW", m.ID)
		}
		seen[m.ID] = true
	}
	if !seen["deepseek/deepseek-v4-flash"] || !seen["openai/gpt-5.6-luna"] {
		t.Errorf("allowlisted models missing from /v1/models: %v", seen)
	}
	if len(out.Data) != 2 {
		t.Errorf("model count = %d, want 2 (allowlisted only)", len(out.Data))
	}
}

// TestModelsAllowRejectsChat pins the chat 404: a request whose resolved
// model is outside MODELS_ALLOW is rejected before any upstream call.
func TestModelsAllowRejectsChat(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.ModelsAllow = []string{"deepseek/deepseek-v4-flash"}
	}, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody("deepseek/deepseek-v4-flash-max"), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("chat status = %d, want 400: %s", resp.StatusCode, data)
	}
	var out struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("error body is not JSON: %v: %s", err, data)
	}
	if out.Error.Code != "model_unavailable" {
		t.Errorf("error.code = %q, want model_unavailable", out.Error.Code)
	}
	if !strings.Contains(out.Error.Message, "Supported models: openai") {
		t.Errorf("error.message = %q, want supported models notice", out.Error.Message)
	}
	if len(mock.RecordedChatHeaders) != 0 {
		t.Error("upstream chat recorded for a rejected model, want none")
	}
}

// TestModelsAllowResolvedAlias pins the allowlist contract: it compares
// against the RESOLVED model id (after registry alias resolution), so a
// client alias that resolves outside the list is rejected too.
func TestModelsAllowResolvedAlias(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.ModelAliases = map[string]string{"pro-alias": "deepseek/deepseek-v4-pro"}
		c.ModelsAllow = []string{"deepseek/deepseek-v4-flash"}
	}, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody("pro-alias"), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("chat (alias) status = %d, want 400: %s", resp.StatusCode, data)
	}
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("error body is not JSON: %v: %s", err, data)
	}
	if out.Error.Code != "model_unavailable" {
		t.Errorf("error.code = %q, want model_unavailable", out.Error.Code)
	}
	if len(mock.RecordedChatHeaders) != 0 {
		t.Error("upstream chat recorded for a rejected alias, want none")
	}
}

// TestModelsAllowPassthrough pins the simplified allowlist interaction: no
// auto-upgrade exists, so an allowlisted BASE id serves exactly the base
// model, an allowlisted -max id serves the -max model, and the catalog
// always stays exactly the MODELS_ALLOW list.
func TestModelsAllowPassthrough(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-max", 1, `"choices":[{"index":0,"delta":{"content":"ping"},"finish_reason":"stop"}]`))
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.ModelsAllow = []string{"openai/gpt-5.6-luna"}
	}, mock)

	// The allowlisted base id is served as-is (no -max upgrade).
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody("openai/gpt-5.6-luna"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat (allowlisted base) status = %d, want 200: %s", resp.StatusCode, data)
	}
	// A -max id NOT in the allowlist stays rejected (no implicit expansion).
	resp, data = doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody("deepseek/deepseek-v4-pro-max"), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("chat (unlisted -max) status = %d, want 400: %s", resp.StatusCode, data)
	}
	// A model outside the list stays rejected.
	resp, data = doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("chat (disallowed) status = %d, want 400: %s", resp.StatusCode, data)
	}

	// /v1/models lists exactly the allowlist, nothing else.
	resp, data = doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("models is not JSON: %v: %s", err, data)
	}
	listed := map[string]bool{}
	for _, m := range out.Data {
		listed[m.ID] = true
	}
	if !listed["openai/gpt-5.6-luna"] {
		t.Errorf("/v1/models missing allowlisted base id openai/gpt-5.6-luna: %v", listed)
	}
	if listed["deepseek/deepseek-v4-pro-max"] {
		t.Errorf("/v1/models leaked -max variant under base-only MODELS_ALLOW: %v", listed)
	}
	if len(out.Data) != 1 {
		t.Errorf("model count = %d, want 1 (base allowlist only)", len(out.Data))
	}
}

// TestModelsAllowEmptyIsOpen verifies an empty allowlist keeps current
// behavior: every model is served and listed.
func TestModelsAllowEmptyIsOpen(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-open", 1, `"choices":[{"index":0,"delta":{"content":"ping"},"finish_reason":"stop"}]`))
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}
	resp, data = doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("models is not JSON: %v: %s", err, data)
	}
	if len(out.Data) != 5 {
		t.Errorf("model count = %d, want 5 (all operational models served)", len(out.Data))
	}
	var hasModelA, hasFlash bool
	for _, m := range out.Data {
		hasModelA = hasModelA || m.ID == modelA
		hasFlash = hasFlash || m.ID == "deepseek/deepseek-v4-flash"
	}
	if !hasModelA || !hasFlash {
		t.Errorf("full catalog missing models: modelA=%v flash=%v", hasModelA, hasFlash)
	}
}

// TestSmokeDefaultsToFallbackModel verifies the smoke test with no explicit
// model probes the guaranteed fallback (deepseek-v4-flash), not the
// alphabetical-first catalog model (anthropic/claude-fable-5, a gated offer).
func TestSmokeDefaultsToFallbackModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-sm", 1, `"choices":[{"index":0,"delta":{"content":"ping"},"finish_reason":"stop"}]`))
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" }, mock)

	// Smoke with no model field: server picks the fallback.
	cookie := authedCookie(t, ts)
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/admin/smoke", []byte(`{"prompt":"ping"}`), map[string]string{"Cookie": cookie})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("smoke status = %d, want 200: %s", resp.StatusCode, data)
	}
	if len(mock.RecordedChatHeaders) == 0 {
		t.Fatal("no upstream chat recorded")
	}
	// #106: the smoke probe is a chat POST — the model rides in the body,
	// not an x-freebuff-model header.
	if got := mock.RecordedChatHeaders[0].Get("x-freebuff-model"); got != "" {
		t.Errorf("smoke probe chat POST carries x-freebuff-model %q, want absent (#106)", got)
	}
	if len(mock.RecordedChatBodies) == 0 {
		t.Fatal("no upstream chat body recorded")
	}
	if !strings.Contains(mock.RecordedChatBodies[0], `"model":"deepseek/deepseek-v4-flash"`) {
		t.Errorf("smoke probe body missing model deepseek/deepseek-v4-flash: %s", mock.RecordedChatBodies[0])
	}
}

// TestHealthzModeCountry verifies healthz surfaces the effective routing
// mode plus the per-token country from the session admission.
func TestHealthzModeCountry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.CountryCode = "US"
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Mode   string `json:"mode"`
		Tokens []struct {
			Country string `json:"country"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("healthz is not JSON: %v: %s", err, data)
	}
	if out.Mode != "pooled" {
		t.Errorf("mode = %q, want pooled", out.Mode)
	}
	if len(out.Tokens) != 1 {
		t.Fatalf("tokens = %d, want 1", len(out.Tokens))
	}
	if out.Tokens[0].Country != "US" {
		t.Errorf("country = %q, want US", out.Tokens[0].Country)
	}
}

func TestMetricsQuotaLines(t *testing.T) {
	mock := quotaMock(t)
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200: %s", resp.StatusCode, data)
	}
	body := string(data)
	for _, want := range []string{
		`freebuff_proxy_quota_recent{token="1",model="z-ai/glm-5.2",period="pacific_day"} 4`,
		`freebuff_proxy_quota_limit{token="1",model="z-ai/glm-5.2",period="pacific_day"} 5`,
		`freebuff_proxy_quota_remaining{token="1",model="z-ai/glm-5.2",period="pacific_day"} 1`,
		fmt.Sprintf(`freebuff_proxy_session_remaining_seconds{token="1",model="%s"}`, modelA),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %s in:\n%s", want, body)
		}
	}
}

// TestMetricsLabelEscaping verifies Prometheus label values (model id and
// period, both upstream-derived) are escaped: quotes become \" so the text
// format stays parseable.
func TestMetricsLabelEscaping(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimitsByModel = map[string]any{
		`weird"model`: map[string]any{
			"model":       `weird"model`,
			"limit":       5,
			"recentCount": 4,
			"period":      `p"d`,
			"resetAt":     "2026-08-16T07:00:00.000Z",
		},
	}
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-qe1", 100,
		`"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-qe1", 100,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`))
	ts, _ := newTestServer(t, nil, mock)

	// A chat admits the session (which carries rateLimitsByModel).
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200: %s", resp.StatusCode, data)
	}
	body := string(data)
	for _, want := range []string{
		`freebuff_proxy_quota_recent{token="1",model="weird\"model",period="p\"d"} 4`,
		`freebuff_proxy_quota_limit{token="1",model="weird\"model",period="p\"d"} 5`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %s in:\n%s", want, body)
		}
	}
}

func TestMetricsTransientRetryCounters(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-r", 1, `"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]`)) + "data: [DONE]\n\n"

	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
		DashboardEnabled:   true,
		TransientRetries:   1,
	}
	client, err := upstream.New("tok-0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The first upstream call (agent-runs START during lease acquisition)
	// fails once at the transport level; TRANSIENT_RETRIES replays it.
	client.SetTransport(&flakyFirstRT{base: http.DefaultTransport})

	sess := session.NewManager(client)
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, []*session.Manager{sess}, reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg, p, reg, nil, nil, "")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200: %s", resp.StatusCode, data)
	}
	body := string(data)
	if !strings.Contains(body, `freebuff_proxy_transient_retries_total{token="1"} 1`) {
		t.Errorf("metrics missing transient retry line: %s", body)
	}
	// No TLS fingerprint is pinned in this setup, so no rotation happened
	// and the fingerprint value line must not be emitted (only when > 0).
	if strings.Contains(body, "freebuff_proxy_fingerprint_rotations_total{token=\"1\"}") {
		t.Errorf("metrics emitted a fingerprint rotation value with no rotation: %s", body)
	}
}

// TestStrictServedModelsEnforced pins issue #189 end-to-end:
//  1. GET /v1/models returns strictly the 6 operational models.
//  2. Any request targeting a disabled model on OpenAI chat, Anthropic messages,
//     or OpenAI responses returns immediate fast-fail with model_unavailable.
//  3. /healthz reports models: 6.
func TestStrictServedModelsEnforced(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	// 1. /v1/models check
	resp, data := doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal /v1/models: %v", err)
	}
	if len(out.Data) != 5 {
		t.Fatalf("models count = %d, want exactly 5", len(out.Data))
	}
	wantSet := map[string]bool{
		"deepseek/deepseek-v4-flash": true,
		"openai/gpt-5.6-luna":        true,
		"upstage/solar-pro4":         true,
		"z-ai/glm-5.3-flash":         true,
		"mimo/mimo-v2.5":             true,
	}
	for _, m := range out.Data {
		if !wantSet[m.ID] {
			t.Errorf("/v1/models listed unexpected model %q", m.ID)
		}
	}

	disabledModels := []string{
		"google/gemini-2.5-flash-lite",
		"google/gemini-3.1-flash-lite",
		"google/gemini-3.5-flash-lite",
		"anthropic/claude-fable-5",
		"crof/kimi-k3-eco",
		"meta/muse-spark-1.2-contributor",
	}

	for _, dm := range disabledModels {
		// OpenAI chat completions -> 400 model_unavailable
		body := `{"model":"` + dm + `","messages":[{"role":"user","content":"hi"}]}`
		respChat, dataChat := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
		if respChat.StatusCode != http.StatusBadRequest {
			t.Errorf("chat %s status = %d, want 400: %s", dm, respChat.StatusCode, dataChat)
		}
		var errChat struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(dataChat, &errChat); err != nil {
			t.Errorf("chat %s response not JSON: %v", dm, err)
		}
		if errChat.Error.Code != "model_unavailable" {
			t.Errorf("chat %s error code = %q, want model_unavailable", dm, errChat.Error.Code)
		}
		if !strings.Contains(errChat.Error.Message, "Supported models: openai") {
			t.Errorf("chat %s message = %q, want supported models notice", dm, errChat.Error.Message)
		}

		// Anthropic messages -> 400 invalid_request_error
		respMsg, dataMsg := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), map[string]string{
			"anthropic-version": "2023-06-01",
		})
		if respMsg.StatusCode != http.StatusBadRequest {
			t.Errorf("messages %s status = %d, want 400: %s", dm, respMsg.StatusCode, dataMsg)
		}
		var errAnthropic struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(dataMsg, &errAnthropic); err != nil {
			t.Errorf("messages %s response not JSON: %v", dm, err)
		}
		if errAnthropic.Error.Type != "invalid_request_error" {
			t.Errorf("messages %s error type = %q, want invalid_request_error", dm, errAnthropic.Error.Type)
		}
		if !strings.Contains(errAnthropic.Error.Message, "Supported models: openai") {
			t.Errorf("messages %s message = %q, want supported models notice", dm, errAnthropic.Error.Message)
		}

		// OpenAI responses -> 400 model_unavailable
		bodyResp := `{"model":"` + dm + `","input":"hi"}`
		respResp, dataResp := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(bodyResp), nil)
		if respResp.StatusCode != http.StatusBadRequest {
			t.Errorf("responses %s status = %d, want 400: %s", dm, respResp.StatusCode, dataResp)
		}
	}

	// Health check
	respH, dataH := doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if respH.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", respH.StatusCode)
	}
	var health struct {
		Models int `json:"models"`
	}
	if err := json.Unmarshal(dataH, &health); err != nil {
		t.Fatalf("unmarshal healthz: %v", err)
	}
	if health.Models != 5 {
		t.Errorf("health.Models = %d, want 5", health.Models)
	}
}

// TestPausedModelWithdrawnMessage pins the withdrawn-model flow (issue #140
// drift, vendor cce4800): minimax/minimax-m3 is upstream-recognized but
// admission-refused, so the proxy fast-refuses it with upstream's own copy —
// naming the replacement — instead of the generic supported-list dump. The
// refusal must land BEFORE any lease acquisition (no doomed admissions), on
// all three chat surfaces.
func TestPausedModelWithdrawnMessage(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	const wantMsg = "MiniMax M3 is no longer available in Freebuff. We recommend using GLM 5.3 Flash instead."

	t.Run("openai chat", func(t *testing.T) {
		body := `{"model":"minimax/minimax-m3","messages":[{"role":"user","content":"hi"}]}`
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(body), nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", resp.StatusCode, data)
		}
		var out struct {
			Error struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("not JSON: %v", err)
		}
		if out.Error.Code != "model_unavailable" {
			t.Errorf("code = %q, want model_unavailable", out.Error.Code)
		}
		if out.Error.Message != wantMsg {
			t.Errorf("message = %q, want %q (mirror freebuffWithdrawnModelMessage)", out.Error.Message, wantMsg)
		}
	})

	t.Run("anthropic messages", func(t *testing.T) {
		body := `{"model":"minimax/minimax-m3","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), map[string]string{
			"anthropic-version": "2023-06-01",
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", resp.StatusCode, data)
		}
		var out struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("not JSON: %v", err)
		}
		if out.Error.Message != wantMsg {
			t.Errorf("message = %q, want %q", out.Error.Message, wantMsg)
		}
	})

	t.Run("responses", func(t *testing.T) {
		body := `{"model":"minimax/minimax-m3","input":"hi"}`
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", resp.StatusCode, data)
		}
		if !strings.Contains(string(data), "GLM 5.3 Flash") {
			t.Errorf("body missing replacement model: %s", data)
		}
	})

	// No doomed admission may reach the mock: zero session/chat traffic.
	if got := len(mock.RecordedChatBodies); got != 0 {
		t.Errorf("upstream chat calls = %d, want 0 (refusal precedes any lease)", got)
	}

	// count_tokens still resolves the paused id (recognition preserved).
	ctBody := []byte(`{"model":"minimax/minimax-m3","messages":[{"role":"user","content":"hello"}]}`)
	respCT, _ := doJSON(t, http.MethodPost, ts.URL+"/v1/messages/count_tokens", ctBody, map[string]string{
		"anthropic-version": "2023-06-01",
	})
	if respCT.StatusCode != http.StatusOK {
		t.Errorf("count_tokens status = %d, want 200 (paused ids stay recognized)", respCT.StatusCode)
	}
}

// TestMetricsFamiliesContract pins the /metrics exposition contract: the
// EXACT set of Prometheus families — presence AND absence of unknown
// additions, so a new family forces a conscious update here (the review
// found families drifting untracked). The expected list mirrors
// handleMetrics (server/health.go) plus the package counter it reads
// (telemetry.ModelUnavailableSkips). freebuff_proxy_log_events_total is
// emitted only when the dashboard log ring is wired, so it is pinned in the
// ring variant below and must be ABSENT without one.
func TestMetricsFamiliesContract(t *testing.T) {
	// metricsFamilies maps family name -> TYPE value (the full contract
	// minus the ring-conditional log_events_total).
	metricsFamilies := map[string]string{
		"freebuff_proxy_uptime_seconds":                "gauge",
		"freebuff_proxy_models_total":                  "gauge",
		"freebuff_proxy_tokens_total":                  "gauge",
		"freebuff_proxy_rate_limit_rejected_total":     "counter",
		"freebuff_proxy_model_unavailable_skips_total": "counter",
		"freebuff_proxy_token_messages_24h":            "gauge",
		"freebuff_proxy_token_requests_total":          "counter",
		"freebuff_proxy_token_active_runs":             "gauge",
		"freebuff_proxy_token_cooldown_active":         "gauge",
		"freebuff_proxy_quota_recent":                  "gauge",
		"freebuff_proxy_quota_limit":                   "gauge",
		"freebuff_proxy_quota_remaining":               "gauge",
		"freebuff_proxy_session_remaining_seconds":     "gauge",
		"freebuff_proxy_transient_retries_total":       "counter",
		"freebuff_proxy_fingerprint_rotations_total":   "counter",
		"freebuff_proxy_rate_limit_events_total":       "counter",
		"freebuff_proxy_model_locked_total":            "counter",
		"freebuff_proxy_premium_quota_limit":           "gauge",
		"freebuff_proxy_premium_quota_used":            "gauge",
		"freebuff_proxy_premium_quota_remaining":       "gauge",
		"freebuff_proxy_premium_quota_percent":         "gauge",
	}

	// assertFamilies checks every expected family has a HELP and a TYPE
	// line with the pinned type, and fails on ANY family not in want —
	// additions must be a conscious contract update.
	assertFamilies := func(t *testing.T, body string, want map[string]string) {
		t.Helper()
		help := map[string]bool{}
		typ := map[string]string{}
		for _, line := range strings.Split(body, "\n") {
			switch {
			case strings.HasPrefix(line, "# HELP "):
				name := strings.TrimSpace(strings.TrimPrefix(line, "# HELP "))
				if i := strings.IndexByte(name, ' '); i >= 0 {
					name = name[:i]
				}
				help[name] = true
			case strings.HasPrefix(line, "# TYPE "):
				rest := strings.TrimSpace(strings.TrimPrefix(line, "# TYPE "))
				name, tval, ok := strings.Cut(rest, " ")
				if !ok {
					t.Fatalf("malformed TYPE line %q", line)
				}
				typ[name] = tval
			}
		}
		for name, wantType := range want {
			if !help[name] {
				t.Errorf("family %s missing from /metrics HELP set", name)
			}
			gotType, ok := typ[name]
			if !ok {
				t.Errorf("family %s missing from /metrics TYPE set", name)
			} else if gotType != wantType {
				t.Errorf("family %s TYPE = %q, want %q", name, gotType, wantType)
			}
		}
		for name := range help {
			if _, ok := want[name]; !ok {
				t.Errorf("unknown family %s added to /metrics HELP (update TestMetricsFamiliesContract consciously)", name)
			}
		}
		for name := range typ {
			if _, ok := want[name]; !ok {
				t.Errorf("unknown family %s has a /metrics TYPE (update TestMetricsFamiliesContract consciously)", name)
			}
		}
	}

	populatedChatBody := func(mock *testutil.MockUpstream) {
		mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-fam", 1, `"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]`)) +
			testutil.SSEEvent(chunk("chatcmpl-fam", 1, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`)) +
			"data: [DONE]\n\n"
	}

	t.Run("exact families on a populated server", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		populatedChatBody(mock)
		ts, _ := newTestServer(t, nil, mock)

		// Populate: one successful streamed chat so the per-token families
		// carry rows, not just HELP/TYPE headers.
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
		}

		resp, data = doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("metrics status = %d, want 200", resp.StatusCode)
		}
		body := string(data)
		assertFamilies(t, body, metricsFamilies)
		// The populated server's request counter must carry a real row.
		if !strings.Contains(body, `freebuff_proxy_token_requests_total{token="1"} 1`) {
			t.Errorf("populated server missing token_requests_total{token=\"1\"} 1 row:\n%s", body)
		}
		// No ring wired: the log-ring family must be absent.
		if strings.Contains(body, "freebuff_proxy_log_events_total") {
			t.Error("log_events_total emitted without a dashboard log ring")
		}
	})

	t.Run("dashboard log ring adds exactly the log_events family", func(t *testing.T) {
		mock := testutil.NewMock()
		defer mock.Close()
		populatedChatBody(mock)
		ring := logring.NewHandler(slog.NewTextHandler(io.Discard, nil), 500)
		ts, _ := newTestServerWithLogger(t, nil, slog.New(ring), ring, mock)

		// One chat so the ring holds records (chat request/routing/done).
		resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
		}

		resp, data = doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("metrics status = %d, want 200", resp.StatusCode)
		}
		body := string(data)
		want := map[string]string{"freebuff_proxy_log_events_total": "counter"}
		for name, tval := range metricsFamilies {
			want[name] = tval
		}
		assertFamilies(t, body, want)
		if !strings.Contains(body, `freebuff_proxy_log_events_total{level="info",msg="chat request"}`) {
			t.Errorf("log_events_total missing the chat request row:\n%s", body)
		}
	})
}

// TestModelsEndpointLimitedTier verifies that when upstream reports accessTier: "limited",
// /v1/models annotates each row with current_access_tier: "limited", marks mimo/mimo-v2.5
// available: true, and marks models outside the limited-tier allowlist available: false,
// status: "region_limited".
func TestModelsEndpointLimitedTier(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.AccessTier = "limited"
	ts, _ := newTestServer(t, nil, mock)

	// Execute a chat to admit a session and populate the pool snapshot with AccessTier: "limited".
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody("mimo/mimo-v2.5"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Data []struct {
			ID                string `json:"id"`
			Available         bool   `json:"available"`
			Status            string `json:"status"`
			CurrentAccessTier string `json:"current_access_tier"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal /v1/models: %v", err)
	}
	if len(out.Data) == 0 {
		t.Fatal("empty models data")
	}
	for _, m := range out.Data {
		if m.CurrentAccessTier != "limited" {
			t.Errorf("model %s current_access_tier = %q, want limited", m.ID, m.CurrentAccessTier)
		}
		if m.ID == "mimo/mimo-v2.5" {
			if !m.Available {
				t.Errorf("mimo available = false, want true on limited tier")
			}
		} else {
			if m.Available {
				t.Errorf("model %s available = true, want false on limited tier", m.ID)
			}
			if m.Status != "region_limited" {
				t.Errorf("model %s status = %q, want region_limited", m.ID, m.Status)
			}
		}
	}
}

// TestModelsEndpointLimitedTierHideUnavailable verifies that MODELS_HIDE_UNAVAILABLE=true
// prunes region_limited models on the limited tier, returning only the limited-allowed models.
func TestModelsEndpointLimitedTierHideUnavailable(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.AccessTier = "limited"
	ts, _ := newTestServerCfg(t, nil, func(cfg *config.Config) {
		cfg.ModelsHideUnavailable = true
	}, mock)

	// Admit session so AccessTier is known.
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody("mimo/mimo-v2.5"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal /v1/models: %v", err)
	}
	if len(out.Data) != 1 || out.Data[0].ID != "mimo/mimo-v2.5" {
		t.Errorf("got models %+v, want only [mimo/mimo-v2.5]", out.Data)
	}
}

// TestModelRetrieveLimitedTier verifies that single model retrieval /v1/models/{model...}
// returns the current_access_tier and region_limited annotations.
func TestModelRetrieveLimitedTier(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.AccessTier = "limited"
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody("mimo/mimo-v2.5"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	// Non-limited model
	resp, data = doJSON(t, http.MethodGet, ts.URL+"/v1/models/z-ai/glm-5.3-flash", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get glm-5.3-flash status = %d, want 200: %s", resp.StatusCode, data)
	}
	var glm struct {
		ID                string `json:"id"`
		Available         bool   `json:"available"`
		Status            string `json:"status"`
		CurrentAccessTier string `json:"current_access_tier"`
	}
	if err := json.Unmarshal(data, &glm); err != nil {
		t.Fatalf("unmarshal glm: %v", err)
	}
	if glm.Available || glm.Status != "region_limited" || glm.CurrentAccessTier != "limited" {
		t.Errorf("glm row = %+v, want available=false, status=region_limited, current_access_tier=limited", glm)
	}

	// Limited model
	resp, data = doJSON(t, http.MethodGet, ts.URL+"/v1/models/mimo/mimo-v2.5", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get mimo status = %d, want 200: %s", resp.StatusCode, data)
	}
	var mimo struct {
		ID                string `json:"id"`
		Available         bool   `json:"available"`
		Status            string `json:"status"`
		CurrentAccessTier string `json:"current_access_tier"`
	}
	if err := json.Unmarshal(data, &mimo); err != nil {
		t.Fatalf("unmarshal mimo: %v", err)
	}
	if !mimo.Available || mimo.CurrentAccessTier != "limited" {
		t.Errorf("mimo row = %+v, want available=true, current_access_tier=limited", mimo)
	}
}

// TestChatRoutingLogsServedModelOnCoercion verifies issue #230: when upstream coerces
// the session to a different model (e.g. mimo), the INFO routing log line explicitly
// includes served_model and the response carries X-FreeBuff-Served-Model.
func TestChatRoutingLogsServedModelOnCoercion(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-coerced","accessTier":"limited","model":"mimo/mimo-v2.5","expiresAt":"2030-01-01T00:00:00Z"}`)
	}

	ring := logring.NewHandler(slog.NewTextHandler(io.Discard, nil), 500)
	ts, _ := newTestServerWithLogger(t, nil, slog.New(ring), ring, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody("z-ai/glm-5.3-flash"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	if servedHeader := resp.Header.Get("X-FreeBuff-Served-Model"); servedHeader != "mimo/mimo-v2.5" {
		t.Errorf("X-FreeBuff-Served-Model header = %q, want mimo/mimo-v2.5", servedHeader)
	}

	var routingEntry *logring.Entry
	for _, e := range ring.Recent(500) {
		if e.Message == "chat routing" {
			routingEntry = &e
			break
		}
	}
	if routingEntry == nil {
		t.Fatal("missing 'chat routing' entry in log ring")
	}
	if gotModel := entryField(*routingEntry, "model"); gotModel != "z-ai/glm-5.3-flash" {
		t.Errorf("routing model = %q, want z-ai/glm-5.3-flash", gotModel)
	}
	if gotServed := entryField(*routingEntry, "served_model"); gotServed != "mimo/mimo-v2.5" {
		t.Errorf("routing served_model = %q, want mimo/mimo-v2.5 (upstream coercion logged)", gotServed)
	}
}
