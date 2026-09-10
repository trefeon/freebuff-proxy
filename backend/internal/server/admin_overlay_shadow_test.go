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

// TestModeSwitchPooledConvergesOverlay: with BRIDGE_ENABLED pinned to 1 by a
// stale DB overlay row, the hybrid→pooled switch converges the row instead
// of failing — the mode switch is write-through (DB-unified storage), so the
// explicit UI action wins over the stale pin on both layers.
func TestModeSwitchPooledConvergesOverlay(t *testing.T) {
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
	if rec.Code != http.StatusOK {
		t.Fatalf("pooled switch status = %d, want 200 (stale overlay converges): %s", rec.Code, rec.Body.String())
	}
	if v, _, _ := st.GetSetting(config.OverlayRowKey("BRIDGE_ENABLED")); v != "0" {
		t.Errorf("overlay BRIDGE_ENABLED = %q, want converged %q", v, "0")
	}
	if s.admin.cfgLoad().HybridBridgeMode() {
		t.Error("effective config still hybrid after pooled switch")
	}
}

// TestModeSwitchHybridConvergesOverlay: with BRIDGE_ENABLED pinned to 0 by a
// stale DB overlay row, the pooled→hybrid switch converges the row instead
// of failing (write-through, like the pooled direction above).
func TestModeSwitchHybridConvergesOverlay(t *testing.T) {
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
	if rec.Code != http.StatusOK {
		t.Fatalf("hybrid switch status = %d, want 200 (stale overlay converges): %s", rec.Code, rec.Body.String())
	}
	if v, _, _ := st.GetSetting(config.OverlayRowKey("BRIDGE_ENABLED")); v != "1" {
		t.Errorf("overlay BRIDGE_ENABLED = %q, want converged %q", v, "1")
	}
	if !s.admin.cfgLoad().HybridBridgeMode() {
		t.Error("effective config not hybrid after switch")
	}
}

// TestRequireLoginConvergesOverlay: with DASHBOARD_REQUIRE_LOGIN pinned to
// true by a stale DB overlay row, the require-login toggle converges the row
// instead of 409ing — the toggle is write-through (DB-unified storage).
func TestRequireLoginConvergesOverlay(t *testing.T) {
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
	if rec.Code != http.StatusOK {
		t.Fatalf("require-login toggle status = %d, want 200 (stale overlay converges): %s", rec.Code, rec.Body.String())
	}
	if v, _, _ := st.GetSetting(config.OverlayRowKey("DASHBOARD_REQUIRE_LOGIN")); v != "false" {
		t.Errorf("overlay DASHBOARD_REQUIRE_LOGIN = %q, want converged %q", v, "false")
	}
	if s.admin.cfgLoad().RequireLogin() {
		t.Error("effective RequireLogin still true after toggle to false")
	}
}

// TestModeSwitchBridgeConvergesAuthTokensOverlay: with a stale migrated
// config:AUTH_TOKENS row pinning a pool the .env no longer carries, the
// pooled→bridge switch converges the row to empty instead of failing — the
// switch is write-through (DB-unified storage), so token management keeps
// working after the env-to-DB migration instead of tripping its own
// divergence guard on the migrated row.
func TestModeSwitchBridgeConvergesAuthTokensOverlay(t *testing.T) {
	s := newReviewFixServer(t, "AUTH_TOKENS=tok-0\nADMIN_TOKEN=secretPass123\n", nil)
	st := attachShadowStore(t, s)
	if err := st.SetSetting(config.OverlayRowKey("AUTH_TOKENS"), "tok-stale"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	h := s.Handler()
	cookie := shadowLogin(t, h, "secretPass123")

	req := httptest.NewRequest(http.MethodPost, "/admin/mode", strings.NewReader(`{"mode":"bridge"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bridge switch status = %d, want 200 (stale overlay converges): %s", rec.Code, rec.Body.String())
	}
	if v, _, _ := st.GetSetting(config.OverlayRowKey("AUTH_TOKENS")); v != "" {
		t.Errorf("overlay AUTH_TOKENS = %q, want converged empty (bridge pin)", v)
	}
	if !s.admin.cfgLoad().BridgeMode() {
		t.Error("effective config not in bridge mode after switch")
	}
}

func TestChangePasswordConvergesAdminTokenOverlay(t *testing.T) {
	s := newReviewFixServer(t, "AUTH_TOKENS=tok-0\nADMIN_TOKEN=secretPass123\n", nil)
	st := attachShadowStore(t, s)
	h := s.Handler()
	cookie := shadowLogin(t, h, "secretPass123")
	// Seed the stale migrated row after login: it would shadow the file on
	// the next reload, so the change must converge it instead of failing
	// its divergence guard on the migrated row.
	if err := st.SetSetting(config.OverlayRowKey("ADMIN_TOKEN"), "stale-pass"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/change-password",
		strings.NewReader(`{"current_password":"secretPass123","new_password":"rotatedPass789"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("change-password status = %d, want 200 (stale overlay converges): %s", rec.Code, rec.Body.String())
	}
	if v, _, _ := st.GetSetting(config.OverlayRowKey("ADMIN_TOKEN")); v != "rotatedPass789" {
		t.Error("overlay ADMIN_TOKEN not converged to the new credential")
	}
	if got := s.admin.cfgLoad().AdminToken; got != "rotatedPass789" {
		t.Error("effective ADMIN_TOKEN not rotated")
	}
}
