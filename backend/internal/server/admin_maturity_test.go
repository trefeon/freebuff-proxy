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
