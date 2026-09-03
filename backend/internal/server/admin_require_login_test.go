package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/config"
)

func TestAdminRequireLoginToggleFlow(t *testing.T) {
	s := newReviewFixServer(t, "AUTH_TOKENS=tok-0\nADMIN_TOKEN=secretPass123\n", nil)
	h := s.Handler()

	// 1. Log in with custom password
	loginForm := url.Values{"token": {"secretPass123"}}
	loginReq := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(loginForm.Encode()))
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loginReq)
	if rec.Code != http.StatusFound {
		t.Fatalf("login status = %d, want 302", rec.Code)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "fb_admin" {
			cookie = c
			break
		}
	}
	if cookie == nil {
		t.Fatal("login did not set fb_admin cookie")
	}

	// 2. Check auth status initially: require_login=true, has_password=true
	statusReq := httptest.NewRequest(http.MethodGet, "/admin/api/auth/status", nil)
	statusReq.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, statusReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth status code = %d, want 200", rec.Code)
	}
	var status struct {
		Authenticated       bool `json:"authenticated"`
		IsDefaultAdminToken bool `json:"is_default_admin_token"`
		RequireLogin        bool `json:"require_login"`
		HasPassword         bool `json:"has_password"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("auth status unmarshal: %v", err)
	}
	if !status.RequireLogin || !status.HasPassword {
		t.Errorf("initial status = %+v, want require_login=true, has_password=true", status)
	}

	toggleReq := func(enable bool) *httptest.ResponseRecorder {
		payload := `{"require_login":` + strconvBool(enable) + `}`
		req := httptest.NewRequest(http.MethodPost, "/admin/api/require-login", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		req.RemoteAddr = "127.0.0.1:4321"
		req.Host = "127.0.0.1:3457"
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		return res
	}

	rec = toggleReq(false)
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle require_login=false status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resData struct {
		OK           bool   `json:"ok"`
		RequireLogin bool   `json:"require_login"`
		Message      string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resData); err != nil {
		t.Fatalf("unmarshal toggle response: %v", err)
	}
	if !resData.OK || resData.RequireLogin {
		t.Errorf("toggle response = %+v, want ok=true, require_login=false", resData)
	}

	// 4. Verify unauthenticated request to /admin/api/overview succeeds on loopback (open mode)
	overviewReq := httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
	overviewReq.RemoteAddr = "127.0.0.1:4321"
	overviewReq.Host = "127.0.0.1:3457"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, overviewReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("open-mode loopback /admin/api/overview status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// 5. Toggle require_login = true
	rec = toggleReq(true)
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle require_login=true status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resData); err != nil {
		t.Fatalf("unmarshal toggle response: %v", err)
	}
	if !resData.OK || !resData.RequireLogin {
		t.Errorf("toggle response = %+v, want ok=true, require_login=true", resData)
	}

	// 6. Verify unauthenticated request to /admin/api/overview now redirects to /admin/login (302)
	overviewReq = httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
	overviewReq.RemoteAddr = "127.0.0.1:4321"
	overviewReq.Host = "127.0.0.1:3457"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, overviewReq)
	if rec.Code != http.StatusFound {
		t.Fatalf("token-mode /admin/api/overview unauthenticated status = %d, want 302", rec.Code)
	}
}

func TestAdminRequireLoginRemoteRejected(t *testing.T) {
	s := newReviewFixServer(t, "AUTH_TOKENS=tok-0\nADMIN_TOKEN=secretPass123\n", nil)
	h := s.Handler()

	// Log in to obtain cookie
	loginForm := url.Values{"token": {"secretPass123"}}
	loginReq := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(loginForm.Encode()))
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loginReq)
	if rec.Code != http.StatusFound {
		t.Fatalf("login status = %d, want 302", rec.Code)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "fb_admin" {
			cookie = c
			break
		}
	}
	if cookie == nil {
		t.Fatal("login did not set fb_admin cookie")
	}

	// Make remote request (simulate remote client with non-loopback Host or RemoteAddr)
	payload := `{"require_login": false}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/require-login", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	req.RemoteAddr = "203.0.113.5:1234"
	req.Host = "proxy.example.com"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("remote require_login=false status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "remote_open_mode_forbidden") {
		t.Errorf("expected remote_open_mode_forbidden in response, got: %s", rec.Body.String())
	}
}

func TestAdminChangePasswordWhenNoPasswordConfigured(t *testing.T) {
	// Start with DASHBOARD_REQUIRE_LOGIN=false (open mode)
	s := newReviewFixServer(t, "AUTH_TOKENS=tok-0\nDASHBOARD_REQUIRE_LOGIN=false\n", nil)
	h := s.Handler()

	// Change password with empty current_password on loopback
	payload := `{"current_password":"","new_password":"BrandNewPassword2026!"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/change-password", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:5555"
	req.Host = "127.0.0.1:3457"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("change password when open status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// Verify new configuration enforces password and login requirement
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminToken != "BrandNewPassword2026!" {
		t.Errorf("AdminToken = %q, want BrandNewPassword2026!", cfg.AdminToken)
	}
	if !cfg.RequireLogin() {
		t.Errorf("RequireLogin() = false, want true after password change")
	}
}

func strconvBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
