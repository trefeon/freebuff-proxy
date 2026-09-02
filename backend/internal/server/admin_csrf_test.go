package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
)

// findCookie returns the named cookie from a response, nil when absent.
func findCookie(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// TestAdminCSRFCookieSetOnLogin pins the double-submit contract: a
// successful login sets the fb_csrf cookie (32-byte hex, SameSite=Strict,
// NOT HttpOnly so the SPA can read it) alongside fb_admin.
func TestAdminCSRFCookieSetOnLogin(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	resp := postLogin(t, ts.URL+"/admin/login", "secret")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login status = %d, want 302", resp.StatusCode)
	}
	csrf := findCookie(resp, "fb_csrf")
	if csrf == nil || csrf.Value == "" {
		t.Fatal("login did not set fb_csrf cookie")
	}
	if csrf.HttpOnly {
		t.Error("fb_csrf is HttpOnly; the SPA must read it from document.cookie")
	}
	if csrf.SameSite != http.SameSiteStrictMode {
		t.Errorf("fb_csrf SameSite = %v, want Strict", csrf.SameSite)
	}
	if csrf.Path != "/" {
		t.Errorf("fb_csrf Path = %q, want /", csrf.Path)
	}
	if len(csrf.Value) != 64 {
		t.Errorf("fb_csrf value length = %d, want 64 (32 random bytes hex)", len(csrf.Value))
	}
	if findCookie(resp, "fb_admin") == nil {
		t.Fatal("login did not set fb_admin session cookie")
	}
}

// TestAdminCSRFHeaderRequired: with the fb_csrf cookie present, a
// state-changing admin POST without a matching X-CSRF-Token header is
// rejected with 403 before reaching the handler.
func TestAdminCSRFHeaderRequired(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	resp := postLogin(t, ts.URL+"/admin/login", "secret")
	csrf := findCookie(resp, "fb_csrf")
	admin := findCookie(resp, "fb_admin")
	_ = resp.Body.Close()
	if csrf == nil || admin == nil {
		t.Fatal("login did not set the expected cookies")
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/admin/diag", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", "fb_admin="+admin.Value+"; fb_csrf="+csrf.Value)
	r, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "Invalid CSRF token.") {
		t.Errorf("POST without X-CSRF-Token = %d, want 403 Invalid CSRF token: %s", r.StatusCode, body)
	}
}

// TestAdminCSRFHeaderAccepted: the same POST with the matching
// X-CSRF-Token header passes the CSRF gate and reaches the handler.
func TestAdminCSRFHeaderAccepted(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	resp := postLogin(t, ts.URL+"/admin/login", "secret")
	csrf := findCookie(resp, "fb_csrf")
	admin := findCookie(resp, "fb_admin")
	_ = resp.Body.Close()
	if csrf == nil || admin == nil {
		t.Fatal("login did not set the expected cookies")
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/admin/diag", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", "fb_admin="+admin.Value+"; fb_csrf="+csrf.Value)
	req.Header.Set("X-CSRF-Token", csrf.Value)
	r, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusOK || !strings.Contains(string(body), `"checks"`) {
		t.Errorf("POST with X-CSRF-Token = %d, want 200 with checks: %s", r.StatusCode, body)
	}
}

// TestAdminCSRFLoginNotBlocked: /admin/login stays origin-checked only —
// with the fb_csrf cookie present but no header, login still authenticates
// (the very first attempt cannot hold a token it never received).
func TestAdminCSRFLoginNotBlocked(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	resp := postLogin(t, ts.URL+"/admin/login", "wrong")
	csrf := findCookie(resp, "fb_csrf")
	_ = resp.Body.Close()

	// A wrong-token login carrying an fb_csrf cookie must reach the token
	// check (401 invalid token), not the CSRF gate (403).
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/admin/login", strings.NewReader("token=wrong"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if csrf != nil {
		req.Header.Set("Cookie", "fb_csrf="+csrf.Value)
	}
	r, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "Invalid admin token") {
		t.Errorf("cookie-carrying login with wrong token = %d, want 401 invalid token: %s", r.StatusCode, body)
	}

	// A correct-token login with the cookie but no header still succeeds.
	req2, err := http.NewRequest(http.MethodPost, ts.URL+"/admin/login", strings.NewReader("token=secret"))
	if err != nil {
		t.Fatal(err)
	}
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if csrf != nil {
		req2.Header.Set("Cookie", "fb_csrf="+csrf.Value)
	}
	r2, err := noRedirectClient().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r2.Body.Close() }()
	if r2.StatusCode != http.StatusFound {
		t.Errorf("cookie-carrying login with correct token = %d, want 302", r2.StatusCode)
	}
}

// TestAdminCSRFNoCookieFallback: a state-changing POST without the fb_csrf
// cookie keeps working through the origin/Sec-Fetch-Site checks (no token
// can be required before it has ever been issued) and the response issues
// the cookie for the next request.
func TestAdminCSRFNoCookieFallback(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	resp := postLogin(t, ts.URL+"/admin/login", "secret")
	admin := findCookie(resp, "fb_admin")
	_ = resp.Body.Close()
	if admin == nil {
		t.Fatal("login did not set fb_admin cookie")
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/admin/diag", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", "fb_admin="+admin.Value)
	r, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusOK || !strings.Contains(string(body), `"checks"`) {
		t.Errorf("cookie-less CSRF POST = %d, want 200 fallback: %s", r.StatusCode, body)
	}
	if findCookie(r, "fb_csrf") == nil {
		t.Error("fallback response did not issue fb_csrf for the next request")
	}
}

// TestAdminCSRFCookieOnPageIssued pins the "when absent on an admin page
// response" half of the contract: the dashboard page response carries
// fb_csrf even when the request had none.
func TestAdminCSRFCookieOnPageIssued(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)
	resp := get(t, ts.URL+"/admin", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("unauthenticated /admin status = %d, want 302", resp.StatusCode)
	}
	if findCookie(resp, "fb_csrf") == nil {
		t.Error("page response did not issue fb_csrf while absent")
	}
}

// TestDevToolsDisabledGate pins the dev-tools gate: with DEVTOOLS_ENABLED=false the
// smoke and playground POST handlers are gated server-side (404), even for
// an authenticated dashboard client.
func TestDevToolsDisabledGate(t *testing.T) {
	ts := dashboardServer(t, "secret", func(c *config.Config) {
		c.DevToolsEnabled = false
	})
	defer ts.Close()
	cookie := authedCookie(t, ts)

	resp := postJSON(t, ts.URL, cookie, "/admin/smoke", `{"model":"deepseek/deepseek-v4-flash","prompt":"ping"}`)
	body := bodyOf(t, resp)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "Dev tools are disabled") {
		t.Errorf("smoke with devtools off = %d, want 404: %s", resp.StatusCode, body)
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/admin/playground/chat",
		strings.NewReader(`{"model":"deepseek/deepseek-v4-flash","prompt":"ping","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", cookie)
	r, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	pb, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusNotFound || !strings.Contains(string(pb), "devtools_disabled") {
		t.Errorf("playground with devtools off = %d, want 404 devtools_disabled: %s", r.StatusCode, pb)
	}
}

// TestAdminLoginTrimsAndBoundsToken pins the login-token bounds: the credential is trimmed
// exactly like the change-password form (padded tokens must authenticate),
// and over-long tokens are rejected as invalid without a compare.
func TestAdminLoginTrimsAndBoundsToken(t *testing.T) {
	ts := dashboardServer(t, "secret", nil)

	post := func(token string) (*http.Response, string) {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/admin/login",
			strings.NewReader("token="+url.QueryEscape(token)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r, err := noRedirectClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Body.Close() }()
		b, _ := io.ReadAll(r.Body)
		return r, string(b)
	}

	r, body := post(" secret ")
	if r.StatusCode != http.StatusFound {
		t.Errorf("whitespace-padded valid token login = %d, want 302: %s", r.StatusCode, body)
	}
	if findCookie(r, "fb_admin") == nil {
		t.Error("trimmed login did not set fb_admin")
	}

	// Over-long: 400 chars, rejected without counting toward lockout
	// semantics beyond a failed attempt.
	long := strings.Repeat("x", 400)
	r, body = post(long)
	if r.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "Invalid admin token") {
		t.Errorf("over-long token login = %d, want 401: %s", r.StatusCode, body)
	}
}

// TestAdminCookieAlwaysSecureBehindSpoofedHeader pins the unconditional
// Secure rule (#318): even an X-Forwarded-Proto: https header from a
// PUBLIC peer (a spoofable value) cannot make the login cookie non-Secure.
func TestAdminCookieAlwaysSecureBehindSpoofedHeader(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" }, mock)
	defer ts.Close()

	req := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader("token=secret"))
	req.RemoteAddr = "203.0.113.9:1234"
	req.Host = "proxy.example.com"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("login status = %d, want 302", rec.Code)
	}
	var admin *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "fb_admin" {
			admin = c
			break
		}
	}
	if admin == nil {
		t.Fatal("login did not set fb_admin")
	}
	if !admin.Secure {
		t.Error("fb_admin Secure = false; Secure must be unconditional (#318)")
	}
}
