// Package server exposes the OpenAI-compatible HTTP surface of the
// freebuff-proxy bridge: POST /v1/chat/completions (stream + non-stream),
// GET /v1/models, and GET /healthz. Stdlib only.
//
// Responsibilities (PRD §6 error matrix):
//   - optional client auth (Bearer / x-api-key exact match, constant-time)
//   - request sanitization via internal/convert before the upstream call
//   - retry-once recovery for session-invalid / run-invalid chat errors
//   - 30-min token cooldown on upstream auth rejection
//   - error mapping to the OpenAI error shape, 503 + Retry-After for the
//     waiting room, 502 when every token is exhausted
//   - SSE relay (sanitized chunks + [DONE]) and non-streaming accumulation
//   - client-disconnect propagation to the upstream (request context)
package server

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/convert"
	"freebuff-proxy/internal/dashboard"
	"freebuff-proxy/internal/logring"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/ratelimit"
	"freebuff-proxy/internal/reasoningcache"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/tokenestimate"
	"freebuff-proxy/internal/updatecheck"
	"freebuff-proxy/internal/upstream"
)

const (
	// maxRequestBody caps the inbound chat-completions body (32MB).
	maxRequestBody = 32 << 20
	// maxStreamLine caps one upstream SSE line the scanner will buffer.
	maxStreamLine = 16 << 20
)

// Server is the HTTP handler holder: routes are built by Handler(). cfg is an
// atomic pointer because /admin/reload swaps it while requests are in flight;
// every read site must Load() it once per request and use the local.
type Server struct {
	cfg     atomic.Pointer[config.Config]
	pool    *pool.Pool
	reg     *registry.Registry
	logger  *slog.Logger
	started time.Time

	// logs is the optional dashboard log viewer ring (nil = disabled); its
	// Counts feed freebuff_proxy_log_events_total on /metrics.
	logs *logring.Handler

	// dash is the embedded admin UI (Svelte SPA + vendored assets).
	dash *dashboard.Dashboard
	// adminAuth guards the dashboard: a stateless HMAC-signed session cookie
	// issued against ADMIN_TOKEN, plus a per-IP login rate limiter.
	adminAuth *adminAuth
	// adminSaveMu serializes .env saves (config editor) so a rejected save
	// cannot clobber a newer accepted one.
	adminSaveMu sync.Mutex
	// configPath is the -config JSON path ("" when none); reloads re-apply it
	// so JSON overrides survive dashboard saves and /admin/reload.
	configPath string

	// version is the running release tag (""/dev for dev builds); the
	// dashboard badge compares it against the latest GitHub release (#50b).
	// updates is the cached latest-release checker (nil = no badge).
	version string
	updates *updatecheck.Checker

	// authClient drives the headless OAuth login wizard (issue #62): a
	// token-less upstream client whose transport/stealth wiring matches the
	// pooled clients. nil disables the wizard endpoints with 503.
	authClient *upstream.Client
	// tokenEstimator counts tokens locally for /v1/messages/count_tokens
	// (nil only if the embedded codec failed to initialize at startup).
	tokenEstimator *tokenestimate.Estimator
	// loginFlows is the in-flight login-flow registry keyed by flow id
	// (fingerprint): start POSTs /api/auth/cli/code, status polls it until
	// the authToken lands (then AddToken + persist).
	loginMu    sync.Mutex
	loginFlows map[string]*loginFlow
	// reasoningCache caches reasoning content and signatures for tool calls across turns.
	reasoningCache *reasoningcache.Cache
	// rateLimiter caps client request rates per source IP (issue #137).
	rateLimiter *ratelimit.Limiter
	// rateLimitRejections tracks total client requests rejected by local rate limiter.
	rateLimitRejections atomic.Int64
}

// WithVersion wires the running release tag + update checker for the
// dashboard badge (issue #50b). A nil checker disables the badge.
func WithVersion(version string, updates *updatecheck.Checker) Option {
	return func(s *Server) {
		s.version = version
		s.updates = updates
	}
}

// WithLoginClient wires the token-less upstream client that drives the
// headless OAuth login wizard (issue #62). A nil client disables the
// wizard endpoints with 503.
func WithLoginClient(c *upstream.Client) Option {
	return func(s *Server) {
		s.authClient = c
	}
}

// Option configures optional server features (release-version badge).
type Option func(*Server)

// New builds the server over the configured pool and registry. A nil logger
// falls back to slog.Default(). The started timestamp pins /v1/models
// "created" and /healthz uptime. logs is the optional dashboard log viewer
// ring (nil disables the /admin/logs page data). configPath is the -config
// JSON path the process was started with ("" = none), used by reloads so a
// dashboard save or /admin/reload re-applies the JSON overrides. opts
// configure optional features (release-version badge, login wizard client).
func New(cfg *config.Config, p *pool.Pool, reg *registry.Registry, logger *slog.Logger, logs *logring.Handler, configPath string, opts ...Option) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{pool: p, reg: reg, logger: logger, started: time.Now(), configPath: configPath, loginFlows: make(map[string]*loginFlow), logs: logs}
	s.cfg.Store(cfg)
	s.rateLimiter = ratelimit.New(cfg.RateLimitPerIP, cfg.RateLimitBurst, 10000)
	// The token estimator shares one o200k_base codec process-wide, so
	// count_tokens requests never rebuild the vocabulary.
	est, err := tokenestimate.New()
	if err != nil {
		logger.Warn("token estimator unavailable; /v1/messages/count_tokens will fail", "err", err)
	}
	s.tokenEstimator = est
	for _, opt := range opts {
		opt(s)
	}
	if cfg.DashboardEnabled {
		dashOpts := []dashboard.Option{}
		if s.version != "" {
			dashOpts = append(dashOpts, dashboard.WithVersion(s.version, s.updates))
		}
		s.dash = dashboard.New(func() *config.Config { return s.cfg.Load() }, p, reg, logger, logs, dashOpts...)
	}
	s.adminAuth = newAdminAuth()
	s.reasoningCache = reasoningcache.New(10000, 2*time.Hour)
	convert.SetReasoningLookup(func(toolID string, content, toolCallsJSON string) (string, string, bool) {
		return s.reasoningCache.Get(toolID, content, toolCallsJSON)
	})
	return s
}

// Handler returns the route table wrapped in an access-log middleware. Method
// mismatches and unknown paths get the ServeMux's automatic 405/404.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerOpenAIRoutes(mux)
	s.registerAnthropicRoutes(mux)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	if s.cfg.Load().DashboardEnabled {
		mux.HandleFunc("POST /admin/reload", s.requireAdminToken(s.adminSensitive(s.requireAuth(s.adminCSRF(http.HandlerFunc(s.handleReload))))))
		// Admin dashboard: cookie-authenticated browser UI. Assets are static
		// and public — the login page (served without a cookie) references them,
		// so they must NOT sit behind dashboardAuth. Overview/tokens/metrics are
		// read-only status and stay open when ADMIN_TOKEN is unset (legacy).
		// Config (read + write) and logs expose secrets and are gated further:
		// with ADMIN_TOKEN unset they require a loopback client.
		// GET /admin/login serves the SPA login page (client-side form, posts to
		// the JSON API below); with ADMIN_TOKEN unset it redirects straight to
		// the dashboard (handleAdminLogin's first branch). POST /admin/login is
		// the JSON token-check API.
		mux.HandleFunc("GET /admin/login", s.handleAdminLogin)
		// POST /admin/login consumes the per-IP login-attempt budget, so it must
		// carry the same CSRF gate as the other mutating admin routes: without it
		// a malicious page could fire cross-origin POSTs with wrong tokens and
		// lock the victim out of the dashboard (5 fails → 1-minute lockout,
		// repeatable).
		mux.HandleFunc("POST /admin/login", s.adminCSRF(http.HandlerFunc(s.handleAdminLogin)))
		// GET /admin/logout clears the session cookie and returns to the login
		// page; POST /admin/logout does the same but answers JSON {"ok":true}.
		// Logout deliberately runs WITHOUT a valid cookie (expired sessions must
		// still be logged out) and is NOT wrapped in adminSensitive — it exposes
		// nothing and must work for anyone capable of reaching /admin/login.
		mux.HandleFunc("GET /admin/logout", s.handleAdminLogout)
		mux.HandleFunc("POST /admin/logout", s.handleAdminLogout)
		// Admin dashboard API routes (JSON)
		mux.Handle("GET /admin/api/overview", s.dashboardAuth(s.dash.APIHandler("overview")))
		mux.Handle("GET /admin/api/tokens", s.dashboardAuth(s.dash.APIHandler("tokens")))
		mux.Handle("GET /admin/api/models", s.dashboardAuth(s.dash.APIHandler("models")))
		mux.Handle("GET /admin/api/traces", s.dashboardAuth(s.dash.APIHandler("traces")))
		mux.Handle("GET /admin/api/setup", s.dashboardAuth(s.dash.APIHandler("setup")))
		mux.Handle("GET /admin/api/config", s.dashboardAuth(s.adminSensitive(s.dash.APIHandler("config"))))
		mux.Handle("GET /admin/api/logs", s.dashboardAuth(s.adminSensitive(s.dash.APIHandler("logs"))))
		mux.Handle("GET /admin/api/metrics", s.dashboardAuth(s.dash.APIHandler("metrics")))
		mux.Handle("GET /admin/api/version", s.dashboardAuth(http.HandlerFunc(s.dash.APIVersion)))

		mux.Handle("GET /admin/api/auth/status", s.dashboardAuth(http.HandlerFunc(s.handleAdminAuthStatus)))
		// SPA: all admin/* GET routes serve the embedded Svelte SPA
		mux.Handle("GET /admin", s.dashboardAuth(http.HandlerFunc(s.dash.ServeSPA)))
		mux.Handle("GET /admin/", s.dashboardAuth(http.HandlerFunc(s.dash.ServeSPA)))
		mux.Handle("GET /admin/tokens", s.dashboardAuth(http.HandlerFunc(s.dash.ServeSPA)))
		mux.Handle("GET /admin/models", s.dashboardAuth(http.HandlerFunc(s.dash.ServeSPA)))
		mux.Handle("GET /admin/traces", s.dashboardAuth(http.HandlerFunc(s.dash.ServeSPA)))
		mux.Handle("GET /admin/setup", s.dashboardAuth(http.HandlerFunc(s.dash.ServeSPA)))
		mux.Handle("GET /admin/playground", s.dashboardAuth(http.HandlerFunc(s.dash.ServeSPA)))
		mux.Handle("GET /admin/config", s.dashboardAuth(s.adminSensitive(http.HandlerFunc(s.dash.ServeSPA))))
		mux.Handle("GET /admin/logs", s.dashboardAuth(s.adminSensitive(http.HandlerFunc(s.dash.ServeSPA))))
		mux.Handle("GET /admin/metrics", s.dashboardAuth(http.HandlerFunc(s.dash.ServeSPA)))
		mux.Handle("POST /admin/playground/chat", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handlePlaygroundChat)))))
		mux.Handle("POST /admin/login/start", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleLoginStart)))))
		mux.Handle("GET /admin/login/status", s.dashboardAuth(s.adminSensitive(http.HandlerFunc(s.handleLoginStatus))))
		mux.Handle("POST /admin/config", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleConfigSave)))))
		mux.Handle("POST /admin/tokens/{id}/unlock", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleTokenUnlock)))))
		mux.Handle("POST /admin/tokens/{id}/lock", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleTokenLock)))))
		mux.Handle("POST /admin/tokens/{id}/unlock-lock", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleTokenUnlockLock)))))
		mux.Handle("POST /admin/tokens/{id}/finish", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleTokenFinish)))))
		mux.Handle("POST /admin/tokens/{id}/test", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleTokenTest)))))
		mux.Handle("POST /admin/tokens/test-all", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleTokenTestAll)))))
		mux.Handle("POST /admin/tokens/add", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleTokenAdd)))))
		mux.Handle("POST /admin/tokens/remove", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleTokenRemove)))))
		mux.Handle("POST /admin/mode", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleModeSwitch)))))
		mux.Handle("POST /admin/diag", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleDiag)))))
		mux.Handle("POST /admin/api/change-password", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleAdminChangePassword)))))
		mux.Handle("POST /admin/smoke", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleSmoke)))))
		// Static assets: serve from embedded dist/assets
		mux.Handle("GET /admin/assets/", noDirListing(http.StripPrefix("/admin/assets/", http.FileServerFS(mustSubFS(dashboard.DistFS(), "assets")))))
	}
	// CORS middleware wraps the whole route table: it answers OPTIONS
	// preflights on the /v1/* API surface with 204 and stamps the allow
	// headers on every /v1/* response. Admin routes are intentionally left
	// untouched (cookie-authenticated dashboard; SameSite=Strict already
	// blocks cross-site reads, and an allow-origin would add nothing there).
	cors := s.corsMiddleware(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// D1: mint the request's correlation id exactly once here, then
		// carry it in the request context so every downstream log line
		// (chat routing/done/trace, request failed, upstream do/retry)
		// shares it. Handlers reached without this wrapper (direct calls
		// in tests) mint a fallback id in chatCore.
		reqID := newReqID()
		r = r.WithContext(context.WithValue(r.Context(), reqIDKey{}, reqID))
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		cors.ServeHTTP(sw, r)
		attrs := []any{
			"req_id", reqID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"ms", time.Since(start).Milliseconds(),
			"remote", remoteHost(r),
		}
		// The client's X-Request-Id is preserved as a separate
		// client_request_id field (never trusted as the correlation key).
		if crid := clientRequestID(r); crid != "" {
			attrs = append(attrs, "client_request_id", crid)
		}
		// T17: LOG_ACCESS=false disables access lines entirely. Quiet
		// endpoints (/healthz, /metrics, OPTIONS preflights) are
		// rate-limited to one access line per path per accessQuietWindow so
		// a poller or browser preflight does not flood the log; every other
		// path keeps one line per request. req_id/client_request_id survive
		// in both cases.
		if !s.cfg.Load().LogAccess {
			return
		}
		if quietAccessPath(r.Method, r.URL.Path) && !accessLogDue(r.URL.Path, start) {
			return
		}
		s.logger.Info("access", attrs...)
	})
}
