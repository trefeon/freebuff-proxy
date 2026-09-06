package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
)

func TestAdminDefaultPasswordAndChangeFlow(t *testing.T) {
	t.Chdir(t.TempDir())
	mock := testutil.NewMock()
	defer mock.Close()

	if err := os.WriteFile(".env", []byte("AUTH_TOKENS=tok-0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IsDefaultAdminToken() || cfg.AdminToken != config.DefaultAdminToken {
		t.Fatalf("expected default admin token %q, got %q", config.DefaultAdminToken, cfg.AdminToken)
	}

	ts, _ := newTestServerCfg(t, nil, nil, mock)
	defer ts.Close()

	// 1. Unauthenticated request to /admin/api/overview redirects to /admin/login (302)
	httpClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := httpClient.Get(ts.URL + "/admin/api/overview")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("unauthenticated /admin/api/overview status = %d, want 302", resp.StatusCode)
	}

	// 2. Login with wrong password fails
	loginForm := url.Values{"token": {"wrong-password"}}
	resp, err = httpClient.PostForm(ts.URL+"/admin/login", loginForm)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "Invalid password") {
		t.Fatalf("wrong password body missing 'Invalid password': %s", body)
	}

	// 3. Login with default password "123456" succeeds and sets cookie
	loginForm = url.Values{"token": {config.DefaultAdminToken}}
	resp, err = httpClient.PostForm(ts.URL+"/admin/login", loginForm)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login with default password status = %d, want 302", resp.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "fb_admin" {
			cookie = c
			break
		}
	}
	if cookie == nil || cookie.Value == "" {
		t.Fatal("login did not set fb_admin cookie")
	}

	// 4. Authenticated request to /admin/api/overview shows is_default_admin_token = true
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/api/overview", nil)
	req.AddCookie(cookie)
	resp, err = httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/admin/api/overview status = %d, want 200: %s", resp.StatusCode, data)
	}
	var overview struct {
		IsDefaultAdminToken bool `json:"is_default_admin_token"`
	}
	if err := json.Unmarshal(data, &overview); err != nil {
		t.Fatalf("overview JSON unmarshal: %v", err)
	}
	if !overview.IsDefaultAdminToken {
		t.Errorf("overview is_default_admin_token = false, want true")
	}

	// 5. Check /admin/api/auth/status
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/admin/api/auth/status", nil)
	req.AddCookie(cookie)
	resp, err = httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/admin/api/auth/status status = %d, want 200: %s", resp.StatusCode, data)
	}
	var authStatus struct {
		Authenticated       bool `json:"authenticated"`
		IsDefaultAdminToken bool `json:"is_default_admin_token"`
	}
	if err := json.Unmarshal(data, &authStatus); err != nil {
		t.Fatalf("authStatus JSON unmarshal: %v", err)
	}
	if !authStatus.Authenticated || !authStatus.IsDefaultAdminToken {
		t.Errorf("authStatus = %+v, want authenticated=true, is_default_admin_token=true", authStatus)
	}

	// 6. Test change password validations
	// 6a. Wrong current password
	changeReq := func(curr, next string) *http.Response {
		payload := `{"current_password":` + strconvQuote(curr) + `,"new_password":` + strconvQuote(next) + `}`
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/api/change-password", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		r, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	resp = changeReq("wrong", "securepass123")
	data, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "Current password is incorrect") {
		t.Errorf("wrong current password status = %d, want 400: %s", resp.StatusCode, data)
	}

	// 6b. New password too short (<6 chars)
	resp = changeReq(config.DefaultAdminToken, "123")
	data, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "at least 6 characters") {
		t.Errorf("short password status = %d, want 400: %s", resp.StatusCode, data)
	}

	// 6c. New password same as default
	resp = changeReq(config.DefaultAdminToken, config.DefaultAdminToken)
	data, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "factory default") {
		t.Errorf("default new password status = %d, want 400: %s", resp.StatusCode, data)
	}

	// 6d. Valid new password
	newPass := "SuperSecretAdmin2026!"
	resp = changeReq(config.DefaultAdminToken, newPass)
	data, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid change-password status = %d, want 200: %s", resp.StatusCode, data)
	}
	var changeRes struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &changeRes); err != nil {
		t.Fatalf("change password unmarshal: %v", err)
	}
	if !changeRes.OK {
		t.Errorf("changeRes.OK = false, want true: %s", data)
	}

	// Update cookie from response if present
	for _, c := range resp.Cookies() {
		if c.Name == "fb_admin" {
			cookie = c
			break
		}
	}

	// 7. Verify .env was updated
	envBytes, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(envBytes), "ADMIN_TOKEN="+newPass) {
		t.Errorf(".env does not contain updated ADMIN_TOKEN: %s", envBytes)
	}

	// 8. Verify overview now reports is_default_admin_token = false
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/admin/api/overview", nil)
	req.AddCookie(cookie)
	resp, err = httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/admin/api/overview after change status = %d, want 200", resp.StatusCode)
	}
	if err := json.Unmarshal(data, &overview); err != nil {
		t.Fatalf("overview JSON unmarshal: %v", err)
	}
	if overview.IsDefaultAdminToken {
		t.Errorf("overview is_default_admin_token after change = true, want false")
	}

	// 9. Verify subsequent login requires new password
	loginForm = url.Values{"token": {config.DefaultAdminToken}}
	resp, err = httpClient.PostForm(ts.URL+"/admin/login", loginForm)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "Invalid password") {
		t.Fatalf("old default password still accepted after change: %s", body)
	}

	loginForm = url.Values{"token": {newPass}}
	resp, err = httpClient.PostForm(ts.URL+"/admin/login", loginForm)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("new password login status = %d, want 302", resp.StatusCode)
	}
}

// TestAdminChangePasswordEnvShadowed covers the divergence guard in
// handleAdminChangePassword: when ADMIN_TOKEN is set in the real process
// environment, config.Load keeps resolving it from there even after the
// handler writes the new password into ./.env. The handler must detect
// that the reload did not move the effective credential, roll the .env
// write back byte-exact, and answer 409 instead of reporting success.
func TestAdminChangePasswordEnvShadowed(t *testing.T) {
	t.Chdir(t.TempDir())
	mock := testutil.NewMock()
	defer mock.Close()

	if err := os.WriteFile(".env", []byte("AUTH_TOKENS=tok-0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ADMIN_TOKEN", "env-shadow-token")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminToken != "env-shadow-token" {
		t.Fatalf("expected process env ADMIN_TOKEN to win over .env, got %q", cfg.AdminToken)
	}

	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "env-shadow-token" }, mock)
	defer ts.Close()

	httpClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	loginForm := url.Values{"token": {"env-shadow-token"}}
	resp, err := httpClient.PostForm(ts.URL+"/admin/login", loginForm)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login with env admin token status = %d, want 302", resp.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "fb_admin" {
			cookie = c
			break
		}
	}
	if cookie == nil || cookie.Value == "" {
		t.Fatal("login did not set fb_admin cookie")
	}

	envBefore, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}

	payload := `{"current_password":"env-shadow-token","new_password":"BrandNewPass2026!"}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/api/change-password", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	resp, err = httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("env-shadowed change-password status = %d, want 409: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "admin_token_overridden") {
		t.Errorf("error body missing admin_token_overridden code: %s", data)
	}

	envAfter, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if string(envBefore) != string(envAfter) {
		t.Errorf(".env mutated despite rollback:\nbefore: %q\nafter:  %q", envBefore, envAfter)
	}

	// The running credential must be unchanged.
	resp, err = httpClient.PostForm(ts.URL+"/admin/login", url.Values{"token": {"env-shadow-token"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("original env token rejected after failed change: %d", resp.StatusCode)
	}
}

// TestAdminChangePasswordUnsafeCharset pins the pre-write validation:
// parseDotenv trims unquoted values at '#' and strips leading quote pairs,
// so such a password could never reload losslessly. It must be rejected
// with 400 before any .env mutation rather than tripping the divergence
// guard with a misleading environment-shadowing error.
func TestAdminChangePasswordUnsafeCharset(t *testing.T) {
	t.Chdir(t.TempDir())
	mock := testutil.NewMock()
	defer mock.Close()

	if err := os.WriteFile(".env", []byte("AUTH_TOKENS=tok-0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ts, _ := newTestServerCfg(t, nil, nil, mock)
	defer ts.Close()

	httpClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := httpClient.PostForm(ts.URL+"/admin/login", url.Values{"token": {config.DefaultAdminToken}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "fb_admin" {
			cookie = c
			break
		}
	}
	if cookie == nil {
		t.Fatal("login did not set fb_admin cookie")
	}

	envBefore, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}

	for _, pw := range []string{"hash#inside", "#leader", `"quoted"`, "'seven1'", "comma,inside"} {
		payload := `{"current_password":` + strconvQuote(config.DefaultAdminToken) + `,"new_password":` + strconvQuote(pw) + `}`
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/api/change-password", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		r, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if r.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "password_unsafe_for_env") {
			t.Errorf("password %q status = %d, want 400 password_unsafe_for_env: %s", pw, r.StatusCode, data)
		}
	}

	// S-14 over-long passwords are rejected before any .env write.
	long := strings.Repeat("x", 300)
	payload := `{"current_password":` + strconvQuote(config.DefaultAdminToken) + `,"new_password":` + strconvQuote(long) + `}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/api/change-password", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	r, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if r.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "password_too_long") {
		t.Errorf("overlong password status = %d, want 400 password_too_long: %s", r.StatusCode, data)
	}

	envAfter, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if string(envBefore) != string(envAfter) {
		t.Errorf(".env mutated by rejected change:\nbefore: %q\nafter:  %q", envBefore, envAfter)
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
