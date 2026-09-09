package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/store"
)

// dualWritePost is the loopback-authenticated JSON POST helper for the
// dual-layer persist tests (mirrors the require-login flow test).
func dualWritePost(t *testing.T, h http.Handler, cookie *http.Cookie, path, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	req.RemoteAddr = "127.0.0.1:4321"
	req.Host = "127.0.0.1:3457"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func dualWriteStoreRows(t *testing.T, st *store.Store) map[string]string {
	t.Helper()
	rows, err := st.ListSettings()
	if err != nil {
		t.Fatalf("ListSettings: %v", err)
	}
	return rows
}

// TestDualWriteRequireLoginRoundTrip pins the write-through contract: a
// require-login toggle lands in BOTH .env (boot seed/export) and the
// settings overlay (runtime truth), and a simulated reboot
// (ListSettings → OverlayFromRows → LoadOpts) reads the settings value back
// even when the .env seed is later edited underneath.
func TestDualWriteRequireLoginRoundTrip(t *testing.T) {
	s := newReviewFixServer(t, "AUTH_TOKENS=tok-0\nADMIN_TOKEN=secretPass123\n", nil)
	st := attachShadowStore(t, s)
	h := s.Handler()
	cookie := shadowLogin(t, h, "secretPass123")

	rec := dualWritePost(t, h, cookie, "/admin/api/require-login", `{"require_login":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if s.admin.cfgLoad().RequireLogin() {
		t.Fatal("effective RequireLogin not false after toggle")
	}

	envBytes, err := os.ReadFile(filepath.Join(".", ".env"))
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	if !strings.Contains(string(envBytes), "DASHBOARD_REQUIRE_LOGIN=false") {
		t.Errorf(".env missing DASHBOARD_REQUIRE_LOGIN=false export: %q", envBytes)
	}
	rows := dualWriteStoreRows(t, st)
	if rows[config.OverlayRowKey("DASHBOARD_REQUIRE_LOGIN")] != "false" {
		t.Errorf("settings overlay row = %q, want %q (full dump %v)",
			rows[config.OverlayRowKey("DASHBOARD_REQUIRE_LOGIN")], "false", rows)
	}

	// Simulated reboot: the overlay read back through the boot path
	// (cli_serve: ListSettings → OverlayFromRows → LoadOpts).
	ov := config.OverlayFromRows(dualWriteStoreRows(t, st))
	rebooted, err := config.LoadOpts("", config.LoadOptions{Overlay: ov})
	if err != nil {
		t.Fatalf("reboot LoadOpts: %v", err)
	}
	if rebooted.RequireLogin() {
		t.Error("rebooted RequireLogin = true, want false (settings is runtime truth)")
	}

	// The overlay beats a later .env seed edit: rewrite the file underneath
	// and the effective config must not move.
	edited := strings.Replace(string(envBytes), "DASHBOARD_REQUIRE_LOGIN=false", "DASHBOARD_REQUIRE_LOGIN=true", 1)
	if err := os.WriteFile(".env", []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	shadowed, err := s.admin.loadConfig()
	if err != nil {
		t.Fatalf("loadConfig after seed edit: %v", err)
	}
	if shadowed.RequireLogin() {
		t.Error("RequireLogin flipped to true after .env seed edit, want overlay (false) to win")
	}

	// No secret material may ever land in the settings table.
	for k, v := range dualWriteStoreRows(t, st) {
		if k == config.OverlayRowKey("AUTH_TOKENS") || k == config.OverlayRowKey("ADMIN_TOKEN") {
			t.Errorf("secret overlay row %q must never exist", k)
		}
		if strings.Contains(v, "secretPass123") || strings.Contains(v, "tok-0") {
			t.Errorf("settings row %q leaks secret material: %q", k, v)
		}
	}
}

// TestDualWriteRequireLoginRollbackBothLayers pins the dual rollback: when
// the process environment shadows the toggle, the 409 must restore the .env
// bytes AND the settings row — both when the row held a prior value and
// when the row did not exist before the write.
func TestDualWriteRequireLoginRollbackBothLayers(t *testing.T) {
	newSeeded := func(t *testing.T, seedRow bool) (*Server, *store.Store) {
		// Open-mode seed (DASHBOARD_REQUIRE_LOGIN=false clears AdminToken at
		// load): no login cookie exists, so the toggle posts as an
		// unauthenticated loopback client, exactly like the open-mode
		// config-save precedent.
		s := newReviewFixServer(t, "AUTH_TOKENS=tok-0\nADMIN_TOKEN=secretPass123\nDASHBOARD_REQUIRE_LOGIN=false\n", nil)
		st := attachShadowStore(t, s)
		if seedRow {
			if err := st.SetSetting(config.OverlayRowKey("DASHBOARD_REQUIRE_LOGIN"), "false"); err != nil {
				t.Fatalf("SetSetting: %v", err)
			}
		}
		return s, st
	}

	for _, seedRow := range []bool{true, false} {
		func() {
			s, st := newSeeded(t, seedRow)
			h := s.Handler()
			// The environment outranks both layers: the toggle cannot take
			// effect, so both writes must roll back. Scoped to this
			// iteration (not t.Setenv) so nothing leaks across seeds.
			if err := os.Setenv("DASHBOARD_REQUIRE_LOGIN", "false"); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.Unsetenv("DASHBOARD_REQUIRE_LOGIN") }()

			rec := dualWritePost(t, h, nil, "/admin/api/require-login", `{"require_login":true}`)
			if rec.Code != http.StatusConflict {
				t.Fatalf("seedRow=%v toggle status = %d, want 409: %s", seedRow, rec.Code, rec.Body.String())
			}
			var res struct {
				OK      bool   `json:"ok"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if res.OK || !strings.Contains(res.Message, "rolled back") {
				t.Errorf("seedRow=%v response = %+v, want ok=false with rollback text", seedRow, res)
			}

			envBytes, err := os.ReadFile(".env")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(envBytes), "DASHBOARD_REQUIRE_LOGIN=false") {
				t.Errorf("seedRow=%v .env not restored: %q", seedRow, envBytes)
			}
			v, ok, err := st.GetSetting(config.OverlayRowKey("DASHBOARD_REQUIRE_LOGIN"))
			if err != nil {
				t.Fatal(err)
			}
			if seedRow {
				if !ok || v != "false" {
					t.Errorf("seedRow=true settings row = %q,%v, want %q,true (prior value restored)", v, ok, "false")
				}
			} else if ok {
				t.Errorf("seedRow=false settings row = %q, want absent (created row deleted)", v)
			}
		}()
	}
}

// TestDualWriteModeSwitchHybridPersistsBothLayers pins the BRIDGE_ENABLED
// write-through on the pooled→hybrid switch: .env export, overlay row, and
// the zero-secret token marker all land, and the reboot path reads hybrid
// back from settings.
func TestDualWriteModeSwitchHybridPersistsBothLayers(t *testing.T) {
	s := newReviewFixServer(t, "AUTH_TOKENS=tok-0\nADMIN_TOKEN=secretPass123\nBRIDGE_ENABLED=0\n", nil)
	st := attachShadowStore(t, s)
	h := s.Handler()
	cookie := shadowLogin(t, h, "secretPass123")

	rec := dualWritePost(t, h, cookie, "/admin/mode", `{"mode":"hybrid"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("mode switch status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	envBytes, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(envBytes), "BRIDGE_ENABLED=1") {
		t.Errorf(".env missing BRIDGE_ENABLED=1 export: %q", envBytes)
	}
	rows := dualWriteStoreRows(t, st)
	if rows[config.OverlayRowKey("BRIDGE_ENABLED")] != "1" {
		t.Errorf("overlay BRIDGE_ENABLED = %q, want \"1\" (dump %v)", rows[config.OverlayRowKey("BRIDGE_ENABLED")], rows)
	}
	if rows[tokenMarkerKey] != "true" {
		t.Errorf("token marker row = %q, want \"true\" (dump %v)", rows[tokenMarkerKey], rows)
	}
	if !s.admin.cfgLoad().HybridBridgeMode() {
		t.Error("effective config not hybrid after switch")
	}

	ov := config.OverlayFromRows(dualWriteStoreRows(t, st))
	rebooted, err := config.LoadOpts("", config.LoadOptions{Overlay: ov})
	if err != nil {
		t.Fatalf("reboot LoadOpts: %v", err)
	}
	if !rebooted.HybridBridgeMode() {
		t.Error("rebooted config not hybrid, want settings overlay to carry the switch")
	}
}

// TestDualWriteModeSwitchBridgeDropsMarker pins the marker delete path: the
// hybrid→bridge switch clears AUTH_TOKENS in .env and drops the presence
// marker from settings (bridge = no pooled tokens).
func TestDualWriteModeSwitchBridgeDropsMarker(t *testing.T) {
	s := newReviewFixServer(t, "AUTH_TOKENS=tok-0\nADMIN_TOKEN=secretPass123\n", nil)
	st := attachShadowStore(t, s)
	if err := st.SetSetting(tokenMarkerKey, "true"); err != nil {
		t.Fatalf("SetSetting marker: %v", err)
	}
	h := s.Handler()
	cookie := shadowLogin(t, h, "secretPass123")

	rec := dualWritePost(t, h, cookie, "/admin/mode", `{"mode":"bridge"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("mode switch status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !s.admin.cfgLoad().BridgeMode() {
		t.Error("effective config not bridge after switch")
	}
	if _, ok, err := st.GetSetting(tokenMarkerKey); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Error("token marker still present after bridge switch, want deleted")
	}
	for k, v := range dualWriteStoreRows(t, st) {
		if strings.Contains(v, "tok-0") {
			t.Errorf("settings row %q leaks token material after bridge switch: %q", k, v)
		}
	}
}

// TestTokenMarkerDelta pins the zero-secret marker mapping: pooled lists set
// the presence flag, an emptied pool drops the row.
func TestTokenMarkerDelta(t *testing.T) {
	set, del := tokenMarkerDelta([]string{"a", "b"})
	if set[tokenMarkerKey] != "true" || len(del) != 0 {
		t.Errorf("pooled delta = (%v, %v), want marker set", set, del)
	}
	set, del = tokenMarkerDelta(nil)
	if len(set) != 0 || len(del) != 1 || del[0] != tokenMarkerKey {
		t.Errorf("bridge delta = (%v, %v), want marker deleted", set, del)
	}
}
