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

// TestPageStatePutRejectsNonObject pins the object-only gate: every consumer
// merges data as an object (loadPageState's spread, the shell restore), so a
// null/number/string/array snapshot 400s instead of landing verbatim and
// breaking the next load. A rejected write stores nothing.
func TestPageStatePutRejectsNonObject(t *testing.T) {
	ts, cookie, csrf := settingsTestServer(t)
	for _, data := range []any{nil, float64(5), "x", []any{float64(1)}} {
		code, out := settingsDo(t, http.MethodPut, ts.URL+"/admin/api/pages/overview", cookie, csrf,
			map[string]any{"data": data})
		if code != http.StatusBadRequest || out["code"] != "bad_request" {
			t.Errorf("PUT data=%v = %d %v, want 400 bad_request", data, code, out)
		}
	}
	_, out := settingsDo(t, http.MethodGet, ts.URL+"/admin/api/pages/overview", cookie, csrf, nil)
	if data, ok := out["data"].(map[string]any); !ok || len(data) != 0 {
		t.Errorf("GET after rejected PUTs data = %v, want {} (nothing stored)", out["data"])
	}
}

// TestPageStatePutEnvelopeOverflow413 pins the limiter mapping: a body past
// the 72KB limiter (64KB data cap + 8KB envelope slack) 413s as
// page_too_large — the same code as an over-cap data field — so clients key
// truncation on one code instead of a generic 400.
func TestPageStatePutEnvelopeOverflow413(t *testing.T) {
	ts, cookie, csrf := settingsTestServer(t)
	big := map[string]any{"data": map[string]any{"blob": strings.Repeat("x", 80<<10)}}
	code, out := settingsDo(t, http.MethodPut, ts.URL+"/admin/api/pages/logs", cookie, csrf, big)
	if code != http.StatusRequestEntityTooLarge || out["code"] != "page_too_large" {
		t.Errorf("PUT envelope overflow = %d %v, want 413 page_too_large", code, out)
	}
}

// TestPageStateDeepLinkIDsAllowed pins the allowlist/nav-registry parity:
// every NAV_ITEMS id (frontend/src/lib/nav.js) — including the four
// deep-link-only pages (setup/metrics/traces/playground) — plus the shell
// chrome key round-trips instead of 404ing real visits.
func TestPageStateDeepLinkIDsAllowed(t *testing.T) {
	ts, cookie, csrf := settingsTestServer(t)
	for _, id := range []string{"setup", "metrics", "traces", "playground", "shell"} {
		code, out := settingsDo(t, http.MethodPut, ts.URL+"/admin/api/pages/"+id, cookie, csrf,
			map[string]any{"data": map[string]any{"visitedAt": float64(1)}})
		if code != http.StatusOK || out["code"] != "page_saved" {
			t.Errorf("PUT pages/%s = %d %v, want 200 page_saved", id, code, out)
			continue
		}
		code, _ = settingsDo(t, http.MethodGet, ts.URL+"/admin/api/pages/"+id, cookie, csrf, nil)
		if code != http.StatusOK {
			t.Errorf("GET pages/%s = %d, want 200", id, code)
		}
	}
}
