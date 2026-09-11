package server_test

import (
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
)

// Maturity lifecycle over the admin API: enable locks the warming token,
// bad params reject, manual touch fires the dry-run probe, disable stops
// automation without unlocking.
func TestTokenMaturityLifecycle(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.StreakBody = map[string]any{"streak": 2, "todayUsed": false, "timeZone": "America/Los_Angeles"}
	ts, p := newTestServerCfg(t, nil, func(c *config.Config) {
		c.AdminToken = "secret"
		c.MaturityEnabled = true
		c.MaturityDryRun = true
		c.MaturityTouchModel = "deepseek/deepseek-v4-flash"
	}, mock)
	cookie := loginCookie(t, ts, "secret")
	post := func(path, body string) (int, string) {
		resp, data := doJSON(t, http.MethodPost, ts.URL+path, []byte(body),
			map[string]string{"Cookie": cookie, "Content-Type": "application/json"})
		return resp.StatusCode, string(data)
	}

	// Bad target rejects.
	if code, _ := post("/admin/tokens/0/maturity", `{"enabled":true,"target":99}`); code != http.StatusBadRequest {
		t.Errorf("target 99 status = %d, want 400", code)
	}
	if code, _ := post("/admin/tokens/0/maturity", `{"enabled":true,"target":-1}`); code != http.StatusBadRequest {
		t.Errorf("target -1 status = %d, want 400", code)
	}
	// Target 0 enrolls with the global MATURITY_TARGET_DAYS default
	// (per-account targets are gone; the dashboard sends target 0).
	if code, body := post("/admin/tokens/0/maturity", `{"enabled":true,"target":0}`); code != http.StatusOK {
		t.Fatalf("target 0 status = %d, want 200: %s", code, body)
	}
	if got := p.Snapshot()[0].Maturity.Target; got != 7 {
		t.Errorf("target-0 snapshot target = %d, want 7 (global default)", got)
	}
	if code, _ := post("/admin/tokens/0/maturity", `{"enabled":false}`); code != http.StatusOK {
		t.Fatalf("disable after target-0 status = %d, want 200", code)
	}
	// Unknown mode rejects.
	if code, _ := post("/admin/tokens/0/maturity", `{"enabled":true,"mode":"turbo"}`); code != http.StatusBadRequest {
		t.Errorf("mode turbo status = %d, want 400", code)
	}
	// Missing enabled rejects.
	if code, _ := post("/admin/tokens/0/maturity", `{"target":7}`); code != http.StatusBadRequest {
		t.Errorf("missing enabled status = %d, want 400", code)
	}

	// Enable: locks the token and arms automation.
	if code, body := post("/admin/tokens/0/maturity", `{"enabled":true,"target":7,"mode":"unmetered"}`); code != http.StatusOK {
		t.Fatalf("enable status = %d, want 200: %s", code, body)
	}
	snap := p.Snapshot()[0]
	if !snap.Locked {
		t.Error("maturity enable did not lock the warming token")
	}
	if snap.Maturity == nil || !snap.Maturity.Enabled || snap.Maturity.Target != 7 {
		t.Errorf("maturity snapshot = %+v, want enabled/7", snap.Maturity)
	}

	// Manual touch fires the dry-run probe (slot/throttle bypassed).
	if code, body := post("/admin/tokens/0/maturity/touch", `{}`); code != http.StatusOK {
		t.Fatalf("touch status = %d, want 200: %s", code, body)
	} else if !strings.Contains(body, "probe") {
		t.Errorf("touch body = %s, want probe action named", body)
	}
	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Errorf("SessionProbes = %d, want 1 manual probe", got)
	}

	// Disable: automation stops, the lock stays for the operator.
	if code, _ := post("/admin/tokens/0/maturity", `{"enabled":false}`); code != http.StatusOK {
		t.Fatalf("disable status = %d, want 200", code)
	}
	snap = p.Snapshot()[0]
	if snap.Maturity == nil || snap.Maturity.Enabled {
		t.Errorf("maturity snapshot = %+v, want disabled", snap.Maturity)
	}
	if !snap.Locked {
		t.Error("maturity disable unlocked the token, want lock unchanged")
	}
}

// touch_model round-trips through the maturity endpoint: stored on the
// snapshot, bad shapes 400, empty clears back to the global fallback.
func TestTokenMaturityTouchModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.StreakBody = map[string]any{"streak": 2, "todayUsed": false, "timeZone": "America/Los_Angeles"}
	ts, p := newTestServerCfg(t, nil, func(c *config.Config) {
		c.AdminToken = "secret"
		c.MaturityEnabled = true
		c.MaturityDryRun = true
		c.MaturityTouchModel = "deepseek/deepseek-v4-flash"
	}, mock)
	cookie := loginCookie(t, ts, "secret")
	post := func(path, body string) (int, string) {
		resp, data := doJSON(t, http.MethodPost, ts.URL+path, []byte(body),
			map[string]string{"Cookie": cookie, "Content-Type": "application/json"})
		return resp.StatusCode, string(data)
	}

	// Bad shape rejects.
	if code, _ := post("/admin/tokens/0/maturity", `{"enabled":true,"touch_model":"not-a-model"}`); code != http.StatusBadRequest {
		t.Errorf("touch_model without provider/ status = %d, want 400", code)
	}
	// Override stores + surfaces on the snapshot.
	if code, body := post("/admin/tokens/0/maturity", `{"enabled":true,"target":7,"mode":"unmetered","touch_model":"mimo/mimo-v2.5"}`); code != http.StatusOK {
		t.Fatalf("override enable status = %d, want 200: %s", code, body)
	}
	if got := p.Snapshot()[0].Maturity.TouchModel; got != "mimo/mimo-v2.5" {
		t.Errorf("snapshot touch_model = %q, want mimo/mimo-v2.5", got)
	}
	// Empty clears back to the global fallback.
	if code, body := post("/admin/tokens/0/maturity", `{"enabled":true,"target":7,"touch_model":""}`); code != http.StatusOK {
		t.Fatalf("override clear status = %d, want 200: %s", code, body)
	}
	if got := p.Snapshot()[0].Maturity.TouchModel; got != "" {
		t.Errorf("cleared touch_model = %q, want fallback empty", got)
	}
}

// warn-reset is additive: it clears only the warning loop, never the
// config. Warning behavior itself is proven at the pool level
// (TestClearMaturityWarnRearms); here the route, idempotency, and range
// validation are pinned.
func TestTokenMaturityWarnReset(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.StreakBody = map[string]any{"streak": 2, "todayUsed": false, "timeZone": "America/Los_Angeles"}
	ts, p := newTestServerCfg(t, nil, func(c *config.Config) {
		c.AdminToken = "secret"
		c.MaturityEnabled = true
		c.MaturityDryRun = true
		c.MaturityTouchModel = "deepseek/deepseek-v4-flash"
	}, mock)
	cookie := loginCookie(t, ts, "secret")
	post := func(path, body string) (int, string) {
		resp, data := doJSON(t, http.MethodPost, ts.URL+path, []byte(body),
			map[string]string{"Cookie": cookie, "Content-Type": "application/json"})
		return resp.StatusCode, string(data)
	}
	if code, body := post("/admin/tokens/0/maturity", `{"enabled":true,"target":7,"mode":"unmetered"}`); code != http.StatusOK {
		t.Fatalf("enable status = %d, want 200: %s", code, body)
	}
	// No warning set: idempotent success, config untouched.
	if code, body := post("/admin/tokens/0/maturity/warn-reset", `{}`); code != http.StatusOK {
		t.Fatalf("warn-reset status = %d, want 200: %s", code, body)
	}
	snap := p.Snapshot()[0]
	if snap.Maturity == nil || !snap.Maturity.Enabled || snap.Maturity.Target != 7 {
		t.Errorf("snapshot after reset = %+v, want enabled/7 kept", snap.Maturity)
	}
	// Out-of-range token rejects.
	if code, _ := post("/admin/tokens/99/maturity/warn-reset", `{}`); code != http.StatusBadRequest {
		t.Errorf("warn-reset token 99 status = %d, want 400", code)
	}
}
