package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/store"
)

// attachShadowStore threads a temp settings store into a server built without
// one (newReviewFixServer wires no history): the DB overlay then participates
// in loadConfig exactly like production.
func attachShadowStore(t *testing.T, s *Server) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "shadow.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s.hist = st
	s.admin.settings = st
	return st
}

// shadowLogin performs the dashboard password login and returns the session
// cookie (fb_admin only, so the double-submit CSRF check stays out of the
// way — these tests exercise handler diagnostics, not the CSRF gate).
func shadowLogin(t *testing.T, h http.Handler, password string) *http.Cookie {
	t.Helper()
	form := url.Values{"token": {password}}
	req := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("login status = %d, want 302", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "fb_admin" {
			return c
		}
	}
	t.Fatal("login did not set fb_admin cookie")
	return nil
}

// TestModeSwitchPooledShadowNamesOverlay: with BRIDGE_ENABLED pinned by a DB
// overlay row, the hybrid→pooled .env write cannot take effect — the error
// must name the overlay (with its DELETE reset path), not the environment.
func TestModeSwitchPooledShadowNamesOverlay(t *testing.T) {
	s := newReviewFixServer(t, "AUTH_TOKENS=tok-0\nADMIN_TOKEN=secretPass123\n",
		func(c *config.Config) { c.BridgeEnabled = true })
	st := attachShadowStore(t, s)
	if err := st.SetSetting(config.OverlayRowKey("BRIDGE_ENABLED"), "1"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	h := s.Handler()
	cookie := shadowLogin(t, h, "secretPass123")

	req := httptest.NewRequest(http.MethodPost, "/admin/mode", strings.NewReader(`{"mode":"pooled"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "DB settings overlay") {
		t.Errorf("pooled shadow response = %q, want it to name the DB settings overlay", body)
	}
	if !strings.Contains(body, "DELETE /admin/api/settings/BRIDGE_ENABLED") {
		t.Errorf("pooled shadow response = %q, want the overlay reset path", body)
	}
}

// TestModeSwitchHybridShadowNamesOverlay: with BRIDGE_ENABLED pinned to 0 by
// a DB overlay row, the pooled→hybrid .env write cannot take effect.
func TestModeSwitchHybridShadowNamesOverlay(t *testing.T) {
	s := newReviewFixServer(t, "AUTH_TOKENS=tok-0\nADMIN_TOKEN=secretPass123\n",
		func(c *config.Config) { c.BridgeEnabled = false })
	st := attachShadowStore(t, s)
	if err := st.SetSetting(config.OverlayRowKey("BRIDGE_ENABLED"), "0"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	h := s.Handler()
	cookie := shadowLogin(t, h, "secretPass123")

	req := httptest.NewRequest(http.MethodPost, "/admin/mode", strings.NewReader(`{"mode":"hybrid"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "DB settings overlay") {
		t.Errorf("hybrid shadow response = %q, want it to name the DB settings overlay", body)
	}
}

// TestRequireLoginShadowNamesOverlay: with DASHBOARD_REQUIRE_LOGIN pinned by
// a DB overlay row, the require-login .env write cannot take effect — the
// 409 must name the overlay instead of blaming the environment/JSON.
func TestRequireLoginShadowNamesOverlay(t *testing.T) {
	s := newReviewFixServer(t, "AUTH_TOKENS=tok-0\nADMIN_TOKEN=secretPass123\n", nil)
	st := attachShadowStore(t, s)
	if err := st.SetSetting(config.OverlayRowKey("DASHBOARD_REQUIRE_LOGIN"), "true"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	h := s.Handler()
	cookie := shadowLogin(t, h, "secretPass123")

	req := httptest.NewRequest(http.MethodPost, "/admin/api/require-login",
		strings.NewReader(`{"require_login":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	req.RemoteAddr = "127.0.0.1:4321"
	req.Host = "127.0.0.1:3457"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("require-login shadow status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "DB settings overlay") {
		t.Errorf("require-login shadow response = %q, want it to name the DB settings overlay", body)
	}
}
