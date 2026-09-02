// Package server exposes the OpenAI-compatible HTTP surface of the
// freebuff-proxy bridge: POST /v1/chat/completions (stream + non-stream),
// GET /v1/models, and GET /healthz. Stdlib only.
//
// Responsibilities (PRD §6 error matrix):
//   - optional client auth (Bearer / x-api-key exact match, constant-time)
//   - request sanitization via backend/internal/convert before the upstream call
//   - retry-once recovery for session-invalid / run-invalid chat errors
//   - 30-min token cooldown on upstream auth rejection
//   - error mapping to the OpenAI error shape, 503 + Retry-After for the
//     waiting room, 502 when every token is exhausted
//   - SSE relay (sanitized chunks + [DONE]) and non-streaming accumulation
//   - client-disconnect propagation to the upstream (request context)
package server

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/convert"
	"freebuff-proxy/backend/internal/dashboard"
	"freebuff-proxy/backend/internal/logring"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/ratelimit"
	"freebuff-proxy/backend/internal/reasoningcache"
	"freebuff-proxy/backend/internal/registry"
	"freebuff-proxy/backend/internal/tokenestimate"
	"freebuff-proxy/backend/internal/updatecheck"
	"freebuff-proxy/backend/internal/upstream"
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
	loginFlows map[string]*loginFlow
	// reasoningCache caches reasoning content and signatures for tool calls across turns.
	reasoningCache *reasoningcache.Cache
	// rateLimiter caps client request rates per source IP (issue #137).
	rateLimiter *ratelimit.Limiter
	// rateLimitRejections tracks total client requests rejected by the
	// local rate limiter.
	rateLimitRejections atomic.Int64

	// admin owns the /admin surface (issue #250): the admin handlers are
	// methods on *adminHandlers, not *Server, so the API surface and the
	// admin surface do not share one mutable god struct.
	admin *adminHandlers

	// gates are the per-Server access-log quiescence gates (issue #252):
	// previously process-global, now owned per instance so two Servers do
	// not share one access gate.
	gates *accessGates
	// rateLimitDedupe gates identical (token, code, window) `request failed`
	// logs (D6): the first + every 50th occurrence fire; the counter always
	// increments so a silent burst stays countable, and the client response
	// is always written. Per-Server like the access gates (issue #252).
	rateLimitDedupe struct {
		mu sync.Mutex
		m  map[string]int64
	}
}

// convertOptions builds the per-request convert options from the live
// config (issue #277/#251): feature knobs resolved once (never per chunk)
// plus the per-Server reasoning lookup threaded through the call chain
// instead of a process-global hook.
func (s *Server) convertOptions() convert.Options {
	opts := convert.Options{
		MaxSchemaNodes:          convert.DefaultMaxSchemaNodes,
		CompressKeepLast:        convert.DefaultCompressKeepLast,
		CompressMaxContentBytes: convert.DefaultCompressMaxContentBytes,
		ReasoningLookup: func(toolID, content, toolCallsJSON string) (string, string, bool) {
			if s.reasoningCache == nil {
				return "", "", false
			}
			return s.reasoningCache.Get(toolID, content, toolCallsJSON)
		},
	}
	if cfg := s.cfg.Load(); cfg != nil {
		opts.CompressPrompt = cfg.CompressPrompt
		opts.CacheControlInjection = cfg.CacheControlInjection
		opts.ReasoningInContent = cfg.ReasoningInContent
	}
	return opts
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
	s := &Server{pool: p, reg: reg, logger: logger, started: time.Now(), configPath: configPath, loginFlows: make(map[string]*loginFlow), logs: logs, gates: newAccessGates()}
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
	s.admin = &adminHandlers{
		dash:           s.dash,
		logfunc:        func() *slog.Logger { return s.logger },
		pool:           p,
		reg:            reg,
		cfgLoad:        s.cfg.Load,
		cfgStore:       s.cfg.Store,
		configPath:     configPath,
		adminAuth:      s.adminAuth,
		loginFlows:     s.loginFlows,
		authClientFunc: func() *upstream.Client { return s.authClient },
		rateLimiter:    s.rateLimiter,
		handleChat:     s.handleChat,
	}
	s.reasoningCache = reasoningcache.New(10000, 2*time.Hour)
	return s
}

// registerAdminRoutes mounts every dashboard.AdminRoutes row on the mux.
// Each row's Auth level selects the wrapping middleware stack it has always
// carried (see dashboard.AdminRoute for the level semantics); POST rows are
// additionally wired through the CSRF gate. A row whose Path has no handler
// mapping panics — the table and the mapper ship as one commit.
func (s *Server) registerAdminRoutes(mux *http.ServeMux) {
	for _, r := range dashboard.AdminRoutes {
		h := s.adminHandler(r)
		if r.Method == http.MethodPost {
			h = s.admin.adminCSRF(h)
		}
		switch r.Auth {
		case dashboard.AuthNone:
			// No auth wrapper: login page, logout, static assets.
		case dashboard.AuthDashboard:
			h = s.admin.dashboardAuth(h)
		case dashboard.AuthSensitive:
			h = s.admin.adminSensitive(h)
			h = s.admin.dashboardAuth(h)
		case dashboard.AuthAdminToken:
			h = s.admin.adminSensitive(h)
			h = s.requireAdminToken(h)
		default:
			panic("server: unknown admin auth level " + r.Auth)
		}
		mux.Handle(r.Method+" "+r.Path, h)
	}
}

// adminHandler resolves one AdminRoutes row to its handler implementation,
// before auth wrapping. Unmapped paths panic: a table row without an
// implementation is a build invariant violation.
func (s *Server) adminHandler(r dashboard.AdminRoute) http.Handler {
	switch r.Method + " " + r.Path {
	case "POST /admin/reload":
		return http.HandlerFunc(s.admin.handleReload)
	case "GET /admin/login":
		return http.HandlerFunc(s.admin.handleAdminLogin)
	case "POST /admin/login":
		return http.HandlerFunc(s.admin.handleAdminLogin)
	case "GET /admin/logout":
		return http.HandlerFunc(s.admin.handleAdminLogout)
	case "POST /admin/logout":
		return http.HandlerFunc(s.admin.handleAdminLogout)
	case "GET /admin/api/overview":
		return s.dash.APIHandler("overview")
	case "GET /admin/api/tokens":
		return s.dash.APIHandler("tokens")
	case "GET /admin/api/events":
		return http.HandlerFunc(s.dash.HandleEvents)
	case "GET /admin/api/models":
		return s.dash.APIHandler("models")
	case "GET /admin/api/traces":
		return s.dash.APIHandler("traces")
	case "GET /admin/api/setup":
		return s.dash.APIHandler("setup")
	case "GET /admin/api/config":
		return s.dash.APIHandler("config")
	case "GET /admin/api/config/meta":
		return http.HandlerFunc(s.dash.APIConfigMeta)
	case "GET /admin/api/logs":
		return s.dash.APIHandler("logs")
	case "GET /admin/api/metrics":
		return s.dash.APIHandler("metrics")
	case "GET /admin/api/version":
		return http.HandlerFunc(s.dash.APIVersion)
	case "GET /admin/api/upstream-drift":
		return s.dash.APIHandler("upstream")
	case "GET /admin/api/auth/status":
		return http.HandlerFunc(s.admin.handleAdminAuthStatus)
	case "GET /admin", "GET /admin/", "GET /admin/tokens", "GET /admin/models", "GET /admin/traces",
		"GET /admin/setup", "GET /admin/playground", "GET /admin/config", "GET /admin/logs", "GET /admin/metrics":
		// SPA shell routes: the gateway serves the Svelte app directly.
		return http.HandlerFunc(s.dash.ServeSPA)
	case "POST /admin/playground/chat":
		return http.HandlerFunc(s.admin.handlePlaygroundChat)
	case "POST /admin/login/start":
		return http.HandlerFunc(s.admin.handleLoginStart)
	case "GET /admin/login/status":
		return http.HandlerFunc(s.admin.handleLoginStatus)
	case "POST /admin/config":
		return http.HandlerFunc(s.admin.handleConfigSave)
	case "POST /admin/tokens/{id}/unlock":
		return http.HandlerFunc(s.admin.handleTokenUnlock)
	case "POST /admin/tokens/{id}/lock":
		return http.HandlerFunc(s.admin.handleTokenLock)
	case "POST /admin/tokens/{id}/unlock-lock":
		return http.HandlerFunc(s.admin.handleTokenUnlockLock)
	case "POST /admin/bridge-tokens/{key}/lock":
		return http.HandlerFunc(s.admin.handleBridgeTokenLock)
	case "POST /admin/bridge-tokens/{key}/unlock":
		return http.HandlerFunc(s.admin.handleBridgeTokenUnlock)
	case "POST /admin/tokens/{id}/finish":
		return http.HandlerFunc(s.admin.handleTokenFinish)
	case "POST /admin/tokens/{id}/drop-session":
		return http.HandlerFunc(s.admin.handleTokenDropSession)
	case "POST /admin/tokens/{id}/test":
		return http.HandlerFunc(s.admin.handleTokenTest)
	case "POST /admin/tokens/{id}/session":
		return http.HandlerFunc(s.admin.handleTokenSpawnSession)
	case "POST /admin/tokens/test-all":
		return http.HandlerFunc(s.admin.handleTokenTestAll)
	case "POST /admin/tokens/add":
		return http.HandlerFunc(s.admin.handleTokenAdd)
	case "POST /admin/tokens/remove":
		return http.HandlerFunc(s.admin.handleTokenRemove)
	case "POST /admin/tokens/swap":
		return http.HandlerFunc(s.admin.handleTokenSwap)
	case "POST /admin/mode":
		return http.HandlerFunc(s.admin.handleModeSwitch)
	case "POST /admin/diag":
		return http.HandlerFunc(s.admin.handleDiag)
	case "POST /admin/api/change-password":
		return http.HandlerFunc(s.admin.handleAdminChangePassword)
	case "POST /admin/smoke":
		return http.HandlerFunc(s.admin.handleSmoke)
	case "GET /admin/assets/":
		return noDirListing(http.StripPrefix("/admin/assets/", http.FileServerFS(mustSubFS(dashboard.DistFS(), "assets"))))
	default:
		panic("server: no admin handler for " + r.Method + " " + r.Path)
	}
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
		s.registerAdminRoutes(mux)
	}
	// CORS middleware wraps the whole route table: it answers OPTIONS
	// preflights on the /v1/* API surface with 204 and stamps the allow
	// headers on every /v1/* response. Admin routes are intentionally left
	// untouched (cookie-authenticated dashboard; SameSite=Strict already
	// blocks cross-site reads, and an allow-origin would add nothing there).
	cors := s.corsMiddleware(mux)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// D1: mint the request's correlation id exactly once here, then
		// carry it in the request context so every downstream log line
		// (chat routing/done/trace, request failed, upstream do/retry)
		// shares it. Handlers reached without this wrapper (direct calls
		// in tests) mint a fallback id in chatCore.
		reqID := newReqID()
		r = r.WithContext(context.WithValue(r.Context(), reqIDKey{}, reqID))
		// Client-side per-IP rate limiting at the OUTERMOST wrapper (issue
		// #137): when RATE_LIMIT_PER_IP is enabled every /v1/* route is
		// covered — chat completions, Responses, Anthropic messages,
		// count_tokens, /v1/models — not just the completion core, so a
		// client cannot burn upstream work through an unthrottled surface.
		// Exempt (documented): /admin/* (the dashboard owns its own
		// throttles), /healthz and /metrics (liveness/monitoring must never
		// be rate-limited away), CORS OPTIONS preflights (they must answer
		// so browsers learn the policy), and non-/v1/ paths (404s are
		// logged only, they cost no upstream work).
		cfg := s.cfg.Load()
		if cfg.RateLimitPerIP > 0 && strings.HasPrefix(r.URL.Path, "/v1/") && r.Method != http.MethodOptions {
			if allowed, retryAfter := s.rateLimiter.Allow(r.RemoteAddr); !allowed {
				retrySec := int(math.Ceil(retryAfter.Seconds()))
				if retrySec < 1 {
					retrySec = 1
				}
				s.logger.Warn("rate limit exceeded",
					"remote", remoteHost(r),
					"req_id", reqID,
					"retry_after_sec", retrySec,
				)
				s.rateLimitRejections.Add(1)
				// One envelope dispatch (issue #253): the wire decides the
				// body shape; the Retry-After ceiling is computed inside.
				s.writeClientError(w, r, http.StatusTooManyRequests,
					fmt.Sprintf("client rate limit exceeded (Retry-After: %ds)", retrySec),
					"rate_limit_exceeded", retryAfter)
				return
			}
		}
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
		// endpoints (/healthz, /metrics, OPTIONS preflights) and UNKNOWN
		// paths (404s) are rate-limited to one access line per path per
		// accessQuietWindow, and the quiet-class budget caps the total
		// quiet lines per window — a client minting distinct paths cannot
		// grow the access log without bound (per-path gating alone would
		// pass one line per unique path). req_id/client_request_id survive
		// in both cases.
		if !cfg.LogAccess {
			return
		}
		if quiet := quietAccessPath(r.Method, r.URL.Path) || sw.status == http.StatusNotFound; quiet {
			if !s.gates.accessLogDue(r.URL.Path, start) || !s.gates.accessQuietBudgetDue(start) {
				return
			}
		}
		s.logger.Info("access", attrs...)
	})
	// gzip wraps the ENTIRE route table (admin + APIs). Streaming
	// responses are exempted inside by Content-Type gate; HEAD and
	// no-body statuses are skipped.
	return gzipMiddleware(h)
}
