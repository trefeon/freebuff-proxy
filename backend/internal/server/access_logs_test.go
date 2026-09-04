package server

// Wave-2 observability tests (T15-T17): admin audit trail, silent-endpoint
// coverage, and access-log hygiene. Internal package so the tests can reach
// the unexported access gate, adminAuth snapshots, and config-diff helpers.

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/registry"
	"freebuff-proxy/backend/internal/testutil"
)

// newLoggingServer builds a full test server (one mock token, fallback
// registry) whose logger writes to a capture buffer at Debug level, so the
// T15-T17 log lines are assertable.
func newLoggingServer(t *testing.T, mock *testutil.MockUpstream, mut func(*config.Config)) (*Server, *bytes.Buffer) {
	t.Helper()
	srv := newServerOpts(t, mock, mut)
	var sink bytes.Buffer
	srv.logger = slog.New(slog.NewTextHandler(&sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return srv, &sink
}

// TestAccessLogGate pins the quiet-path gate (T17): the first request for a
// path logs, requests within the window are suppressed, a different path is
// its own gate, and a request after the window logs again.
func TestAccessLogGate(t *testing.T) {
	g := newAccessGates()
	t.Cleanup(g.reset)

	t0 := time.Now()
	if !g.accessLogDue("/healthz", t0) {
		t.Fatal("first /healthz request must log")
	}
	if g.accessLogDue("/healthz", t0.Add(time.Second)) {
		t.Error("second /healthz within the window must be suppressed")
	}
	if !g.accessLogDue("/metrics", t0.Add(time.Second)) {
		t.Error("a different quiet path has its own gate")
	}
	if !g.accessLogDue("/healthz", t0.Add(61*time.Second)) {
		t.Error("/healthz after the window must log again (a new minute)")
	}
}

// TestAccessQuietEndpointsRateLimited verifies end-to-end that two /healthz
// requests in the same window produce one access line, and a request after
// the window produces a second (T17).
func TestAccessQuietEndpointsRateLimited(t *testing.T) {
	testutil.UnsetConfigEnv(t)
	mock := testutil.NewMock()
	defer mock.Close()
	srv, sink := newLoggingServer(t, mock, nil)
	srv.gates = newAccessGates()
	h := srv.Handler()

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	}
	if got := strings.Count(sink.String(), "msg=access"); got != 1 {
		t.Fatalf("access lines for two same-window /healthz = %d, want 1", got)
	}

	// Shrink the per-Server window to zero: the next request logs again
	// (deterministic stand-in for "two requests in different minutes").
	srv.gates.window = 0
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if got := strings.Count(sink.String(), "msg=access"); got != 2 {
		t.Fatalf("access lines after window expiry = %d, want 2", got)
	}
}

// TestAccessLogsEndpointRateLimited pins the console self-pollution fix: the
// Logs page polls GET /admin/api/logs every second, and each poll used to
// emit its own access line into the same 200-entry window the console reads.
// At one line per second an idle dashboard evicted its own inference history
// in ~3 minutes ("entries appear then vanish with no new requests"). The
// endpoint is quiet-gated like /healthz: at most one access line per window.
func TestAccessLogsEndpointRateLimited(t *testing.T) {
	testutil.UnsetConfigEnv(t)
	mock := testutil.NewMock()
	defer mock.Close()
	srv, sink := newLoggingServer(t, mock, nil)
	srv.gates = newAccessGates()
	h := srv.Handler()

	for range 2 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/api/logs", nil))
	}
	if got := strings.Count(sink.String(), "msg=access"); got != 1 {
		t.Fatalf("access lines for two same-window GET /admin/api/logs = %d, want 1", got)
	}

	// A normal /v1 access line must still fire every time (only the
	// self-observing logs poll is quiet-gated).
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if got := strings.Count(sink.String(), "msg=access"); got != 2 {
		t.Fatalf("access lines after one /v1/models = %d, want 2", got)
	}
}

// TestAccessLogDisabledSuppressesLines verifies LOG_ACCESS=false turns the
// access lines off entirely (normal paths included), and flipping the
// effective config back on restores them (T17).
func TestAccessLogDisabledSuppressesLines(t *testing.T) {
	testutil.UnsetConfigEnv(t)
	mock := testutil.NewMock()
	defer mock.Close()
	srv, sink := newLoggingServer(t, mock, func(c *config.Config) { c.LogAccess = false })
	h := srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("models status = %d, want 200", rec.Code)
	}
	if strings.Contains(sink.String(), "msg=access") {
		t.Fatal("access line logged with LOG_ACCESS=false")
	}

	// Runtime toggle back on (config reload semantics).
	cfg := *srv.cfg.Load()
	cfg.LogAccess = true
	srv.cfg.Store(&cfg)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if !strings.Contains(sink.String(), "msg=access") {
		t.Fatal("no access line after LOG_ACCESS re-enabled")
	}
}

// TestAdminReloadLogsAudit verifies T15: /admin/reload success logs INFO and
// failure logs WARN, both with remote and path. The server carries an
// ADMIN_TOKEN so the non-loopback test client passes the adminSensitive gate
// via the bearer token (the open-mode loopback requirement is pinned by
// TestAdminReloadAdminSensitiveGate).
func TestAdminReloadLogsAudit(t *testing.T) {
	testutil.UnsetConfigEnv(t)
	t.Chdir(t.TempDir())
	mock := testutil.NewMock()
	defer mock.Close()
	srv, sink := newLoggingServer(t, mock, func(c *config.Config) { c.AdminToken = "secret-admin-token" })
	h := srv.Handler()

	// The .env carries ADMIN_TOKEN too: a successful reload swaps s.cfg for
	// config.Load(""), so the bearer gate must survive the swap for the
	// non-loopback test client (the adminSensitive loopback gate would
	// otherwise 403 the audit requests).
	if err := os.WriteFile(".env", []byte("ADMIN_TOKEN=secret-admin-token\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reload := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
		req.RemoteAddr = "198.51.100.7:1234"
		req.Header.Set("Authorization", "Bearer secret-admin-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := reload()
	if rec.Code != http.StatusOK {
		t.Fatalf("reload status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	logs := sink.String()
	for _, want := range []string{"admin reload requested", "config reloaded successfully", "remote=198.51.100.7", "path=/admin/reload"} {
		if !strings.Contains(logs, want) {
			t.Errorf("reload success logs missing %q", want)
		}
	}

	// Failure path: an invalid .env makes config.Load reject the reload
	// (ADMIN_TOKEN line kept so the gate still passes on the retry).
	if err := os.WriteFile(".env", []byte("ROTATION_INTERVAL=not-a-duration\nADMIN_TOKEN=secret-admin-token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec = reload()
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("reload failure status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	logs = sink.String()
	for _, want := range []string{"admin reload failed", "remote=198.51.100.7", "path=/admin/reload"} {
		if !strings.Contains(logs, want) {
			t.Errorf("reload failure logs missing %q", want)
		}
	}
}

// TestAdminLoginFailureLogsNoCredential verifies T15: a failed /admin/login
// logs a WARN with remote, running attempt count, and reason — never the
// submitted credential or the configured token.
func TestAdminLoginFailureLogsNoCredential(t *testing.T) {
	testutil.UnsetConfigEnv(t)
	mock := testutil.NewMock()
	defer mock.Close()
	srv, sink := newLoggingServer(t, mock, func(c *config.Config) { c.AdminToken = "secret-admin-token" })
	h := srv.Handler()

	post := func(cred string) {
		t.Helper()
		form := url.Values{"token": {cred}}
		req := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "198.51.100.9:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}

	post("wrong-credential-value")
	logs := sink.String()
	if !strings.Contains(logs, "admin login failed") {
		t.Fatal("missing admin login failure WARN")
	}
	for _, want := range []string{"remote=198.51.100.9", "attempts=1", "reason=invalid_token"} {
		if !strings.Contains(logs, want) {
			t.Errorf("login WARN missing %q", want)
		}
	}
	if strings.Contains(logs, "wrong-credential-value") || strings.Contains(logs, "secret-admin-token") {
		t.Fatal("login failure log leaked the credential")
	}

	post("another-wrong-value")
	if !strings.Contains(sink.String(), "attempts=2") {
		t.Errorf("second failure did not log attempts=2: %s", sink.String())
	}
	if strings.Contains(sink.String(), "another-wrong-value") {
		t.Fatal("second login failure log leaked the credential")
	}
}

// TestConfigSaveLogsChangedKeys verifies T15: a config save logs the sorted
// changed effective key NAMES (never values — including secret values).
func TestConfigSaveLogsChangedKeys(t *testing.T) {
	testutil.UnsetConfigEnv(t)
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", []byte("SAFE_MODE=true\nAUTH_TOKENS=tok-a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mock := testutil.NewMock()
	defer mock.Close()
	// The helper cfg must mirror the initial .env effective state
	// (SAFE_MODE=true, one token) so the save diff is old-vs-new effective.
	srv, sink := newLoggingServer(t, mock, func(c *config.Config) {
		c.SafeMode = true
		c.AuthTokens = []string{"tok-a"}
	})
	h := srv.Handler()

	// Loopback remote + Host: the open-mode adminSensitive gate requires it.
	req := httptest.NewRequest(http.MethodPost, "/admin/config",
		strings.NewReader("SAFE_MODE=false\nAUTH_TOKENS=tok-a\nADMIN_TOKEN=new-secret-xyz\nMAX_MESSAGES_PER_DAY=10\n"))
	req.Header.Set("Content-Type", "text/plain")
	req.RemoteAddr = "127.0.0.1:1234"
	req.Host = "127.0.0.1:3457"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("config save status = %d: %s", rec.Code, rec.Body.String())
	}

	logs := sink.String()
	if !strings.Contains(logs, "dashboard config saved and reloaded") {
		t.Fatal("missing config save INFO")
	}
	if !strings.Contains(logs, "changed_keys=") {
		t.Fatalf("save INFO missing changed_keys: %s", logs)
	}
	for _, key := range []string{"ADMIN_TOKEN", "MAX_MESSAGES_PER_DAY", "SAFE_MODE"} {
		if !strings.Contains(logs, key) {
			t.Errorf("changed_keys missing %q: %s", key, logs)
		}
	}
	// AUTH_TOKENS kept the same count → not changed.
	if strings.Contains(logs, "AUTH_TOKENS") {
		t.Errorf("AUTH_TOKENS listed as changed despite the same count: %s", logs)
	}
	// Values — including the new ADMIN_TOKEN secret — must never appear.
	for _, leaked := range []string{"new-secret-xyz", "SAFE_MODE=false", "MAX_MESSAGES_PER_DAY=10"} {
		if strings.Contains(logs, leaked) {
			t.Errorf("config save log leaked %q: %s", leaked, logs)
		}
	}
}

// TestEmbeddingsUnsupportedWarn verifies T16: the unsupported_endpoint 400
// logs a WARN with path, remote, and status.
func TestEmbeddingsUnsupportedWarn(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	srv, sink := newLoggingServer(t, mock, nil)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"x"}`))
	req.RemoteAddr = "198.51.100.11:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("embeddings status = %d, want 400", rec.Code)
	}
	logs := sink.String()
	for _, want := range []string{"unsupported endpoint requested", "path=/v1/embeddings", "remote=198.51.100.11", "status=400"} {
		if !strings.Contains(logs, want) {
			t.Errorf("embeddings WARN missing %q", want)
		}
	}
}

// TestModelsEmptyRegistryWarn verifies T16: /v1/models with an empty
// registry logs a WARN (model_count 0) when requested — not at startup.
func TestModelsEmptyRegistryWarn(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	srv, sink := newLoggingServer(t, mock, nil)
	// Replace the fallback-populated registry with an empty one.
	srv.reg = registry.New(srv.cfg.Load(), nil)
	h := srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("models status = %d, want 200", rec.Code)
	}
	logs := sink.String()
	for _, want := range []string{"model list requested with empty registry", "model_count=0"} {
		if !strings.Contains(logs, want) {
			t.Errorf("empty-registry WARN missing %q", want)
		}
	}
}
