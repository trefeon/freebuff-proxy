package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/server"
	"freebuff-proxy/backend/internal/store"
	"freebuff-proxy/backend/internal/testutil"
)

// settingsTestServer builds a store-backed gateway (ADR-0019): the temp DB
// feeds the settings overlay through server.WithHistory, and the login
// returns both session cookies the mutation endpoints require.
func settingsTestServer(t *testing.T) (*httptest.Server, string, string) {
	t.Helper()
	t.Chdir(t.TempDir())
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv, _ := server.NewTestServerStack(t, nil, []*testutil.MockUpstream{testutil.NewMock()},
		func(c *config.Config) { c.AdminToken = "secret" }, nil, nil, server.WithHistory(st))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp := postLogin(t, ts.URL+"/admin/login", "secret")
	defer func() { _ = resp.Body.Close() }()
	var admin, csrf string
	for _, c := range resp.Cookies() {
		if c.Name == "fb_admin" {
			admin = c.Name + "=" + c.Value
		}
		if c.Name == "fb_csrf" {
			csrf = c.Value
		}
	}
	if admin == "" || csrf == "" {
		t.Fatal("login did not set fb_admin + fb_csrf cookies")
	}
	return ts, admin + "; fb_csrf=" + csrf, csrf
}

// settingsDo performs one settings request and decodes the JSON envelope.
func settingsDo(t *testing.T, method, url, cookie, csrf string, body any) (int, map[string]any) {
	t.Helper()
	var reader *strings.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(raw))
	} else {
		reader = strings.NewReader("")
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s %s: decode: %v", method, url, err)
	}
	return resp.StatusCode, out
}

func settingsSources(t *testing.T, ts *httptest.Server, cookie string) map[string]map[string]any {
	t.Helper()
	resp, data := doJSON(t, http.MethodGet, ts.URL+"/admin/api/settings", nil, map[string]string{"Cookie": cookie})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET settings = %d: %s", resp.StatusCode, data)
	}
	var payload struct {
		Settings []map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	out := make(map[string]map[string]any, len(payload.Settings))
	for _, e := range payload.Settings {
		out[e["key"].(string)] = e
	}
	return out
}

// TestSettingsOverlayCycle proves the write→effective→delete→fallback loop:
// POST persists a DB row and hot-applies it, GET reports source=db, DELETE
// drops the row and the effective value falls back.
func TestSettingsOverlayCycle(t *testing.T) {
	ts, cookie, csrf := settingsTestServer(t)

	// Baseline: LOG_LEVEL ships at its default with no overlay.
	entries := settingsSources(t, ts, cookie)
	if entries["LOG_LEVEL"]["source"] != "default" {
		t.Fatalf("LOG_LEVEL source = %v, want default", entries["LOG_LEVEL"]["source"])
	}
	baseline := entries["LOG_LEVEL"]["value"]

	// Write: a live key hot-applies.
	code, res := settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, csrf,
		map[string]any{"key": "LOG_LEVEL", "value": "debug"})
	if code != http.StatusOK || res["ok"] != true {
		t.Fatalf("POST LOG_LEVEL = %d %v, want 200 ok", code, res)
	}
	if res["code"] != "setting_saved" {
		t.Errorf("POST code = %v, want setting_saved (live key)", res["code"])
	}

	// Effective: the settings view AND the classic config view agree.
	entries = settingsSources(t, ts, cookie)
	if entries["LOG_LEVEL"]["value"] != "debug" || entries["LOG_LEVEL"]["source"] != "db" {
		t.Fatalf("LOG_LEVEL entry = %v, want value=debug source=db", entries["LOG_LEVEL"])
	}
	resp, data := doJSON(t, http.MethodGet, ts.URL+"/admin/api/config", nil, map[string]string{"Cookie": cookie})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), `"key":"LOG_LEVEL"`) {
		t.Fatalf("GET config = %d %s, want the effective view", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), `"value":"debug"`) {
		t.Errorf("config effective view missing debug LOG_LEVEL: %s", data)
	}

	// Restart-only keys persist too, flagged honestly.
	code, res = settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, csrf,
		map[string]any{"key": "TRANSIENT_RETRIES", "value": "3"})
	if code != http.StatusOK || res["ok"] != true {
		t.Fatalf("POST TRANSIENT_RETRIES = %d %v, want 200 ok", code, res)
	}
	if res["code"] != "setting_restart_only" {
		t.Errorf("POST code = %v, want setting_restart_only", res["code"])
	}
	if ro, _ := res["restart_only"].([]any); len(ro) != 1 || ro[0] != "TRANSIENT_RETRIES" {
		t.Errorf("restart_only = %v, want [TRANSIENT_RETRIES]", res["restart_only"])
	}

	// Reset: the row drops and the value falls back to its default tier.
	code, res = settingsDo(t, http.MethodDelete, ts.URL+"/admin/api/settings/LOG_LEVEL", cookie, csrf, nil)
	if code != http.StatusOK || res["ok"] != true {
		t.Fatalf("DELETE LOG_LEVEL = %d %v, want 200 ok", code, res)
	}
	entries = settingsSources(t, ts, cookie)
	if entries["LOG_LEVEL"]["source"] != "default" || entries["LOG_LEVEL"]["value"] != baseline {
		t.Fatalf("LOG_LEVEL after reset = %v, want value=%v source=default", entries["LOG_LEVEL"], baseline)
	}

	// Second delete: nothing left to reset.
	code, res = settingsDo(t, http.MethodDelete, ts.URL+"/admin/api/settings/LOG_LEVEL", cookie, csrf, nil)
	if code != http.StatusNotFound {
		t.Errorf("DELETE missing overlay = %d %v, want 404", code, res)
	}
}

// TestSettingsPostRejects pins the validation gate: secrets, unknown keys,
// and unparseable values never reach the DB.
func TestSettingsPostRejects(t *testing.T) {
	ts, cookie, csrf := settingsTestServer(t)

	for _, key := range []string{"AUTH_TOKENS", "ADMIN_TOKEN", "API_KEYS", "WEBHOOK_URL", "UPSTREAM_BASE_URL", "DB_PATH"} {
		code, res := settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, csrf,
			map[string]any{"key": key, "value": "x"})
		if code != http.StatusBadRequest {
			t.Errorf("POST %s = %d %v, want 400 (never in DB)", key, code, res)
		}
	}
	for _, kv := range [][2]string{
		{"NOPE_NOT_A_KEY", "x"},
		{"SAFE_MODE", "banana"},
		{"MAX_REQUESTS_PER_MINUTE", "lots"},
		{"RATE_LIMIT_PER_IP", "fast"},
		{"HTTP_READ_TIMEOUT", "soon"},
		{"LOG_LEVEL", ""},
		{"LOG_LEVEL", "   "},
	} {
		code, res := settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, csrf,
			map[string]any{"key": kv[0], "value": kv[1]})
		if code != http.StatusBadRequest {
			t.Errorf("POST %v = %d %v, want 400", kv, code, res)
		}
	}

	// Rejections store nothing: the source map stays clean.
	entries := settingsSources(t, ts, cookie)
	for _, key := range []string{"SAFE_MODE", "MAX_REQUESTS_PER_MINUTE", "RATE_LIMIT_PER_IP", "HTTP_READ_TIMEOUT"} {
		if entries[key]["source"] == "db" {
			t.Errorf("%s source = db after rejected POSTs, want no overlay row", key)
		}
	}
}

// TestSettingsDeleteCSRF: the DELETE row carries the same double-submit
// gate as every other state-changing admin route.
func TestSettingsDeleteCSRF(t *testing.T) {
	ts, cookie, _ := settingsTestServer(t)
	code, _ := settingsDo(t, http.MethodDelete, ts.URL+"/admin/api/settings/LOG_LEVEL", cookie, "", nil)
	if code != http.StatusForbidden {
		t.Errorf("DELETE without X-CSRF-Token = %d, want 403", code)
	}
}

// TestSettingsWithoutStore: a live-only gateway (DB failed at boot) still
// serves the effective view; mutations 503 instead of landing nowhere.
func TestSettingsWithoutStore(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	resp, data := doJSON(t, http.MethodGet, ts.URL+"/admin/api/settings", nil, map[string]string{"Cookie": cookie})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET settings live-only = %d: %s", resp.StatusCode, data)
	}
	var payload struct {
		Settings []map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || len(payload.Settings) == 0 {
		t.Fatalf("GET settings payload = %s, want a non-empty settings array", data)
	}

	code, res := settingsDo(t, http.MethodPost, ts.URL+"/admin/api/settings", cookie, "csrf",
		map[string]any{"key": "LOG_LEVEL", "value": "debug"})
	if code != http.StatusServiceUnavailable {
		t.Errorf("POST live-only = %d %v, want 503", code, res)
	}
}
