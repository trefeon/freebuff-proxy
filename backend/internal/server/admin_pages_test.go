package server_test

import (
	"net/http"
	"strings"
	"testing"
)

// TestPageStateRoundTrip proves the per-page persist loop: an absent page
// reads as {} (never 404), PUT upserts opaque JSON, and a second PUT
// overwrites. Unknown ids 404 against the page allowlist.
func TestPageStateRoundTrip(t *testing.T) {
	ts, cookie, csrf := settingsTestServer(t)

	code, out := settingsDo(t, http.MethodGet, ts.URL+"/admin/api/pages/tokens", cookie, csrf, nil)
	if code != http.StatusOK {
		t.Fatalf("GET absent page = %d %v, want 200", code, out)
	}
	if data, ok := out["data"].(map[string]any); !ok || len(data) != 0 {
		t.Fatalf("GET absent page data = %v, want {}", out["data"])
	}

	code, out = settingsDo(t, http.MethodPut, ts.URL+"/admin/api/pages/tokens", cookie, csrf,
		map[string]any{"data": map[string]any{"expanded": float64(2)}})
	if code != http.StatusOK || out["code"] != "page_saved" {
		t.Fatalf("PUT page = %d %v, want 200 page_saved", code, out)
	}

	code, out = settingsDo(t, http.MethodGet, ts.URL+"/admin/api/pages/tokens", cookie, csrf, nil)
	if code != http.StatusOK {
		t.Fatalf("GET stored page = %d %v, want 200", code, out)
	}
	if data, ok := out["data"].(map[string]any); !ok || data["expanded"] != float64(2) {
		t.Fatalf("GET stored page data = %v, want {expanded:2}", out["data"])
	}

	code, _ = settingsDo(t, http.MethodPut, ts.URL+"/admin/api/pages/tokens", cookie, csrf,
		map[string]any{"data": map[string]any{"filter": "x"}})
	if code != http.StatusOK {
		t.Fatalf("PUT overwrite = %d, want 200", code)
	}
	_, out = settingsDo(t, http.MethodGet, ts.URL+"/admin/api/pages/tokens", cookie, csrf, nil)
	if data, ok := out["data"].(map[string]any); !ok || data["filter"] != "x" || len(data) != 1 {
		t.Fatalf("GET overwritten page data = %v, want {filter:x}", out["data"])
	}

	for _, tc := range []struct{ method, url string }{
		{http.MethodGet, ts.URL + "/admin/api/pages/nope"},
		{http.MethodPut, ts.URL + "/admin/api/pages/nope"},
	} {
		code, out := settingsDo(t, tc.method, tc.url, cookie, csrf, map[string]any{"data": map[string]any{}})
		if code != http.StatusNotFound || out["code"] != "unknown_page" {
			t.Errorf("%s unknown page = %d %v, want 404 unknown_page", tc.method, code, out)
		}
	}
}

// TestPageStatePutGuards pins the write gates: missing data 400s, payloads
// over the 64KB cap 413, and a PUT without the double-submit token 403s.
func TestPageStatePutGuards(t *testing.T) {
	ts, cookie, csrf := settingsTestServer(t)

	code, out := settingsDo(t, http.MethodPut, ts.URL+"/admin/api/pages/logs", cookie, csrf,
		map[string]any{"other": 1})
	if code != http.StatusBadRequest {
		t.Errorf("PUT missing data = %d %v, want 400", code, out)
	}

	big := map[string]any{"data": map[string]any{"blob": strings.Repeat("x", 70<<10)}}
	code, out = settingsDo(t, http.MethodPut, ts.URL+"/admin/api/pages/logs", cookie, csrf, big)
	if code != http.StatusRequestEntityTooLarge || out["code"] != "page_too_large" {
		t.Errorf("PUT oversize = %d %v, want 413 page_too_large", code, out)
	}

	code, _ = settingsDo(t, http.MethodPut, ts.URL+"/admin/api/pages/logs", cookie, "",
		map[string]any{"data": map[string]any{}})
	if code != http.StatusForbidden {
		t.Errorf("PUT without X-CSRF-Token = %d, want 403", code)
	}
}

// TestPageStateWithoutStore: a live-only gateway (DB failed at boot) still
// serves the empty view; the upsert 503s instead of landing nowhere.
func TestPageStateWithoutStore(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := dashboardServer(t, "secret", nil)
	cookie := authedCookie(t, ts)

	code, out := settingsDo(t, http.MethodGet, ts.URL+"/admin/api/pages/shell", cookie, "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET pages live-only = %d %v, want 200", code, out)
	}
	if data, ok := out["data"].(map[string]any); !ok || len(data) != 0 {
		t.Fatalf("GET pages live-only data = %v, want {}", out["data"])
	}

	code, out = settingsDo(t, http.MethodPut, ts.URL+"/admin/api/pages/shell", cookie, "csrf",
		map[string]any{"data": map[string]any{"lastHash": "tokens"}})
	if code != http.StatusServiceUnavailable {
		t.Errorf("PUT live-only = %d %v, want 503", code, out)
	}
}
