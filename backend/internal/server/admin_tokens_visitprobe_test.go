// admin_tokens_visitprobe_test.go — visit auto-probe (ADR-0025) handler
// coverage: ?auto=1 probes when the pool-scoped timestamp is stale and
// skips upstream untouched when fresh (single-note ok envelope either
// way), while the manual path always forces and refreshes the timestamp.
// Existing test-all tests are untouched.
package server_test

import (
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
)

func TestDashboardTokenTestAllAutoStaleProbes(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" }, mock0, mock1)
	cookie := authedCookie(t, ts)

	resp := doTokenAction(t, ts.URL, cookie, "/admin/tokens/test-all?auto=1")
	body := bodyOf(t, resp)
	if !strings.Contains(body, `"ok":true`) {
		t.Errorf("stale auto response = %q, want ok:true note", body)
	}
	if !strings.Contains(body, "refreshed") {
		t.Errorf("stale auto response = %q, want refreshed note", body)
	}
	// Stale visit probes every pooled token exactly once, session-less.
	for i, mock := range []*testutil.MockUpstream{mock0, mock1} {
		if got := mock.SessionCreatesSnapshot(); got != 0 {
			t.Errorf("token %d session creates = %d, want 0 (probe claims no session)", i, got)
		}
		if got := mock.SessionProbesSnapshot(); got != 1 {
			t.Errorf("token %d session probes = %d, want 1", i, got)
		}
	}
}

func TestDashboardTokenTestAllAutoFreshSkips(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" }, mock0, mock1)
	cookie := authedCookie(t, ts)

	// First visit is stale (never probed): probes once.
	resp := doTokenAction(t, ts.URL, cookie, "/admin/tokens/test-all?auto=1")
	_ = bodyOf(t, resp)

	// Second visit is fresh: no upstream touch, current view untouched
	// with a skip note in the same ok envelope.
	resp = doTokenAction(t, ts.URL, cookie, "/admin/tokens/test-all?auto=1")
	body := bodyOf(t, resp)
	if !strings.Contains(body, `"ok":true`) {
		t.Errorf("fresh auto response = %q, want ok:true note", body)
	}
	if !strings.Contains(body, "skipping") {
		t.Errorf("fresh auto response = %q, want skipping note", body)
	}
	for i, mock := range []*testutil.MockUpstream{mock0, mock1} {
		if got := mock.SessionProbesSnapshot(); got != 1 {
			t.Errorf("token %d session probes = %d after fresh visit, want still 1", i, got)
		}
	}
}

func TestDashboardTokenTestAllManualForcesDespiteFresh(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.AdminToken = "secret" }, mock0, mock1)
	cookie := authedCookie(t, ts)

	// Stale visit probes once and stamps the pool-scoped timestamp.
	resp := doTokenAction(t, ts.URL, cookie, "/admin/tokens/test-all?auto=1")
	_ = bodyOf(t, resp)

	// Manual (no param) always forces: per-token rows, probes again.
	resp = doTokenAction(t, ts.URL, cookie, "/admin/tokens/test-all")
	body := bodyOf(t, resp)
	if !strings.Contains(body, `"token":0`) || !strings.Contains(body, `"token":1`) {
		t.Errorf("manual test-all missing per-token results: %s", body)
	}
	for i, mock := range []*testutil.MockUpstream{mock0, mock1} {
		if got := mock.SessionProbesSnapshot(); got != 2 {
			t.Errorf("token %d session probes = %d after manual force, want 2", i, got)
		}
	}

	// The manual pass refreshed the timestamp: the next visit skips.
	resp = doTokenAction(t, ts.URL, cookie, "/admin/tokens/test-all?auto=1")
	if body := bodyOf(t, resp); !strings.Contains(body, "skipping") {
		t.Errorf("visit after manual response = %q, want skipping note", body)
	}
	for i, mock := range []*testutil.MockUpstream{mock0, mock1} {
		if got := mock.SessionProbesSnapshot(); got != 2 {
			t.Errorf("token %d session probes = %d after visit, want still 2", i, got)
		}
	}
}
