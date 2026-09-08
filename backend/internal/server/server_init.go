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
	"log/slog"
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
	"freebuff-proxy/backend/internal/store"
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
	// hist is the dashboard history store (nil = live-only views). Threaded
	// into the dashboard at construction; closed via Close on shutdown.
	hist *store.Store
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

// WithHistory threads the dashboard history store into the embedded admin
// UI (ADR-0016). A nil store keeps every history view on live data.
func WithHistory(st *store.Store) Option {
	return func(s *Server) {
		s.hist = st
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
		if s.hist != nil {
			dashOpts = append(dashOpts, dashboard.WithHistory(s.hist))
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
		settings:       s.hist,
		adminAuth:      s.adminAuth,
		loginFlows:     s.loginFlows,
		authClientFunc: func() *upstream.Client { return s.authClient },
		rateLimiter:    s.rateLimiter,
		handleChat:     s.handleChat,
	}
	s.reasoningCache = reasoningcache.New(10000, 2*time.Hour)
	return s
}

// Close flushes and releases server-owned resources: the dashboard history
// consumer and store. Safe to call on a server built without WithHistory.
func (s *Server) Close() error {
	if s.dash != nil {
		return s.dash.Close()
	}
	return nil
}
