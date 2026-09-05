package server_test

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
)

// TestDashboardTokenRemoveSpecificIndex pins the by-index removal: the SPA
// sends the row index (values stay masked client-side), and the chosen token
// — never the last one — leaves the pool and the .env.
func TestDashboardTokenRemoveSpecificIndex(t *testing.T) {
	t.Chdir(t.TempDir())
	ts, p := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" },
		testutil.NewMock(), testutil.NewMock())
	cookie := authedCookie(t, ts)
	// The harness names the tokens tok-0 / tok-1; remove index 0.
	resp := postTokenAction(t, ts.URL, cookie, "/admin/tokens/remove", "0")
	body := bodyOf(t, resp)
	if !strings.Contains(body, "Token removed") {
		t.Fatalf("remove response = %q, want success", body)
	}
	if got := p.TokenCount(); got != 1 {
		t.Fatalf("pool TokenCount = %d, want 1 (only tok-1 remains)", got)
	}
	env, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env), "tok-0") {
		t.Errorf(".env still contains the removed token: %s", env)
	}
	if !strings.Contains(string(env), "tok-1") {
		t.Errorf(".env missing the remaining token: %s", env)
	}
}

// TestDashboardTokenRemoveBadIndex pins the rejection: an out-of-range index
// gets a plain error message and the pool is untouched.
func TestDashboardTokenRemoveBadIndex(t *testing.T) {
	t.Chdir(t.TempDir())
	ts, p := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" },
		testutil.NewMock(), testutil.NewMock())
	cookie := authedCookie(t, ts)
	resp := postTokenAction(t, ts.URL, cookie, "/admin/tokens/remove", "5")
	body := bodyOf(t, resp)
	if !strings.Contains(body, "Invalid token index") {
		t.Fatalf("remove response = %q, want index rejection", body)
	}
	if got := p.TokenCount(); got != 2 {
		t.Fatalf("pool TokenCount = %d, want 2 (untouched)", got)
	}
}

// TestDashboardTokenRemoveJSONIndex pins the SPA wire format: Tokens.svelte
// posts application/json {token: <0-based index>} via postAPI, which
// FormValue never parses. The indexed token — never the last one — must
// leave the pool and the .env.
func TestDashboardTokenRemoveJSONIndex(t *testing.T) {
	t.Chdir(t.TempDir())
	ts, p := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" },
		testutil.NewMock(), testutil.NewMock())
	cookie := authedCookie(t, ts)
	// Remove index 0 of tok-0/tok-1 via JSON, exactly like the SPA.
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/admin/tokens/remove",
		strings.NewReader(`{"token":0}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", "application/json")
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := bodyOf(t, resp)
	if !strings.Contains(body, "Token removed") {
		t.Fatalf("remove response = %q, want success", body)
	}
	if got := p.TokenCount(); got != 1 {
		t.Fatalf("pool TokenCount = %d, want 1 (only tok-1 remains)", got)
	}
	env, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env), "tok-0") {
		t.Errorf(".env still contains the removed token: %s", env)
	}
	if !strings.Contains(string(env), "tok-1") {
		t.Errorf(".env missing the remaining token: %s", env)
	}
}

func postTokenAction(t *testing.T, base, cookie, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+path,
		strings.NewReader(url.Values{"token": []string{token}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
