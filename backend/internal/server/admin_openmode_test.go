package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/registry"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// newReviewFixServer builds a full single-token server over a fresh temp
// working directory seeded with the given .env content, mirroring the
// external newTestServerCfg wiring so these regression tests exercise the
// real pool/registry/dashboard stack.
func newReviewFixServer(t *testing.T, envContent string, mut func(*config.Config)) *Server {
	t.Helper()
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", []byte(envContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfgPtr := &cfg
	if mut != nil {
		mut(cfgPtr)
	}
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	clientCfg := *cfgPtr
	clientCfg.UpstreamBaseURL = mock.URL()
	client, err := upstream.New(cfg.AuthTokens[0], &clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New(cfgPtr, nil)
	reg.LoadFallback()
	p, err := pool.New(cfgPtr, []*upstream.Client{client}, []*session.Manager{session.NewManager(client)}, reg)
	if err != nil {
		t.Fatal(err)
	}
	return New(cfgPtr, p, reg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, "")
}

// TestAdminDashboardOpenModeRemote403 pins the open-mode dashboard gate:
// with ADMIN_TOKEN unset a non-loopback client gets 403 on the
// AuthDashboard read tier instead of anonymous access, loopback is still
// served, the AuthNone routes (login page) stay reachable remotely, and
// setting ADMIN_TOKEN restores the previous remote behavior.
func TestAdminDashboardOpenModeRemote403(t *testing.T) {
	s := newReviewFixServer(t, "AUTH_TOKENS=tok-0\n", func(c *config.Config) { c.AdminToken = "" })
	h := s.Handler()

	remoteReq := func(path string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "203.0.113.5:4444" // TEST-NET-3: never a loopback peer
		req.Host = "dashboard.example.com"
		return req
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, remoteReq("/admin/api/overview"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("open-mode remote /admin/api/overview status = %d, want 403", rec.Code)
	}

	loopReq := httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
	loopReq.RemoteAddr = "127.0.0.1:5111"
	loopReq.Host = "127.0.0.1:3457"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, loopReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("open-mode loopback /admin/api/overview status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// AuthNone: the login page stays reachable remotely (open-mode GET
	// redirects to /admin rather than 403).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, remoteReq("/admin/login"))
	if rec.Code != http.StatusFound {
		t.Fatalf("open-mode remote /admin/login status = %d, want 302", rec.Code)
	}

	// Setting ADMIN_TOKEN restores the current remote behavior: an
	// unauthenticated remote request is redirected to login, not 403'd.
	secured := newReviewFixServer(t, "AUTH_TOKENS=tok-0\nADMIN_TOKEN=secret123\n", nil)
	rec = httptest.NewRecorder()
	secured.Handler().ServeHTTP(rec, remoteReq("/admin/api/overview"))
	if rec.Code != http.StatusFound {
		t.Fatalf("token-mode remote /admin/api/overview status = %d, want 302", rec.Code)
	}
}

// TestAdminSmokeOversizedFormBodyCapped pins the form-path body cap: an
// oversized urlencoded body must be refused by the MaxBytesReader before
// ParseForm slurps it, surfacing the "request body too large" read failure
// instead of the prompt-length rejection that would prove the body had
// been fully read into memory.
func TestAdminSmokeOversizedFormBodyCapped(t *testing.T) {
	s := newReviewFixServer(t, "AUTH_TOKENS=tok-0\nDEVTOOLS_ENABLED=true\n", nil)
	body := "prompt=" + strings.Repeat("a", 16<<10)
	req := httptest.NewRequest(http.MethodPost, "/admin/smoke", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.admin.handleSmoke(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized smoke form status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if msg := rec.Body.String(); !strings.Contains(msg, "Failed to read request") || !strings.Contains(msg, "request body too large") {
		t.Fatalf("oversized smoke form response = %q, want MaxBytesError surfaced (request body too large)", msg)
	}
}

// TestAdminChangePasswordReloadPropagatesRateLimit pins the reload seam:
// the change-password reload must push the reloaded config into the live
// rate limiter too. A RATE_LIMIT_PER_IP edit made directly in .env (out of
// band) is picked up by the next reload — historically the limiter kept
// the boot value because the change-password path skipped SetRate.
func TestAdminChangePasswordReloadPropagatesRateLimit(t *testing.T) {
	s := newReviewFixServer(t,
		"AUTH_TOKENS=tok-0\nADMIN_TOKEN=secret123\nRATE_LIMIT_PER_IP=1\n", nil)
	if got, burst := s.rateLimiter.Rate(); got != 1 || burst != 2 {
		t.Fatalf("boot limiter rate = (%v, %d), want (1, 2)", got, burst)
	}

	// Out-of-band .env edit the running process has not applied yet.
	env, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(env), "RATE_LIMIT_PER_IP=1", "RATE_LIMIT_PER_IP=9", 1)
	if updated == string(env) {
		t.Fatal("failed to rewrite RATE_LIMIT_PER_IP in .env fixture")
	}
	if err := os.WriteFile(".env", []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}

	form := strings.NewReader("current_password=secret123&new_password=newpass456")
	req := httptest.NewRequest(http.MethodPost, "/admin/api/change-password", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.admin.handleAdminChangePassword(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("change-password status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("change-password response = %s, want ok:true", rec.Body.String())
	}

	// The reload must reach the live limiter (burst defaults to 2x rate).
	if got, burst := s.rateLimiter.Rate(); got != 9 || burst != 18 {
		t.Fatalf("limiter rate after change-password reload = (%v, %d), want (9, 18)", got, burst)
	}
	if cfg := s.cfg.Load(); cfg.RateLimitPerIP != 9 {
		t.Fatalf("cfg.RateLimitPerIP after reload = %v, want 9", cfg.RateLimitPerIP)
	}
}

// TestAdminRunsKnobsRestartOnlyPinned pins the five runs.Options knobs in
// the server's restart-only set: a dashboard save must report them as
// restart-required (changedRestartOnlyKeys drives that response) instead of
// silently no-op'ing on the live runs managers.
func TestAdminRunsKnobsRestartOnlyPinned(t *testing.T) {
	for _, key := range []string{
		"ROTATION_INTERVAL",
		"RUN_FINISH_QUEUE_SIZE",
		"RUN_FINISH_INLINE_TIMEOUT",
		"RUNS_DRAIN_QUEUE_CAP",
		"RUNS_DRAIN_TTL",
	} {
		found := false
		for _, k := range restartOnlyConfigKeys {
			if k == key {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s missing from restartOnlyConfigKeys", key)
		}
	}

	old := &config.Config{
		RotationInterval:       6 * time.Hour,
		RunFinishQueueSize:     64,
		RunFinishInlineTimeout: 250 * time.Millisecond,
		RunsDrainQueueCap:      64,
		RunsDrainTTL:           10 * time.Minute,
	}
	newCfg := *old
	newCfg.RotationInterval = time.Hour
	newCfg.RunFinishQueueSize = 128
	want := map[string]bool{"ROTATION_INTERVAL": true, "RUN_FINISH_QUEUE_SIZE": true}
	for _, k := range changedRestartOnlyKeys(old, &newCfg) {
		if !want[k] {
			t.Errorf("changedRestartOnlyKeys reported unexpected %q", k)
			continue
		}
		delete(want, k)
	}
	for k := range want {
		t.Errorf("changedRestartOnlyKeys missed %q", k)
	}
}
