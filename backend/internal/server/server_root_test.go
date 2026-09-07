package server_test

import (
	"net/http"
	"testing"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/testutil"
)

// TestRootRedirectsToAdmin pins GET / -> 302 /admin when the dashboard is
// enabled. The redirect carries no auth gate: /admin itself owns auth.
func TestRootRedirectsToAdmin(t *testing.T) {
	testutil.UnsetConfigEnv(t)
	ts := dashboardServer(t, "", nil)
	defer ts.Close()

	resp := get(t, ts.URL+"/", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("GET / status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin" {
		t.Fatalf("GET / Location = %q, want /admin", loc)
	}
}

// TestRootNotFoundWhenDashboardDisabled pins GET / -> 404 when
// DASHBOARD_ENABLED=false (the pre-change behavior is preserved).
func TestRootNotFoundWhenDashboardDisabled(t *testing.T) {
	testutil.UnsetConfigEnv(t)
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.DashboardEnabled = false
	}, testutil.NewMock())
	defer ts.Close()

	resp := get(t, ts.URL+"/", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET / with dashboard disabled status = %d, want 404", resp.StatusCode)
	}
}
