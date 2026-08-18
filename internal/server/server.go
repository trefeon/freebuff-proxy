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
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/convert"
	"freebuff-proxy/internal/dashboard"
	"freebuff-proxy/internal/logring"
	"freebuff-proxy/internal/phasetiming"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/runs"
	"freebuff-proxy/internal/session"
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

	// dash is the embedded admin UI (htmx + vendored assets).
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
}

// loginFlow is one in-flight headless login (issue #62).
type loginFlow struct {
	ID         string // short flow id shown to the client (fingerprint prefix)
	Code       *upstream.CLILoginCode
	Started    time.Time
	Done       bool
	Completing bool // one status poll is mid-completion (guards double-add)
	Token      string
	Error      string
	Index      int // pooled token index after AddToken (0 when bridge)
}

// loginFlowTTL drops stale flows (never completed; browser closed).
const loginFlowTTL = 10 * time.Minute

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
	s := &Server{pool: p, reg: reg, logger: logger, started: time.Now(), configPath: configPath, loginFlows: make(map[string]*loginFlow)}
	s.cfg.Store(cfg)
	// The token estimator shares one o200k_base codec process-wide, so
	// count_tokens requests never rebuild the vocabulary.
	est, err := tokenestimate.New()
	if err != nil {
		logger.Error("token estimator unavailable; /v1/messages/count_tokens will fail", "err", err)
	}
	s.tokenEstimator = est
	for _, opt := range opts {
		opt(s)
	}
	dashOpts := []dashboard.Option{}
	if s.version != "" {
		dashOpts = append(dashOpts, dashboard.WithVersion(s.version, s.updates))
	}
	s.dash = dashboard.New(func() *config.Config { return s.cfg.Load() }, p, reg, logger, logs, dashOpts...)
	s.adminAuth = newAdminAuth()
	return s
}

// Handler returns the route table wrapped in an access-log middleware. Method
// mismatches and unknown paths get the ServeMux's automatic 405/404.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.requireAuth(s.handleChat))
	mux.HandleFunc("POST /v1/responses", s.requireAuth(s.handleResponses))
	mux.HandleFunc("POST /v1/messages", s.requireAuth(s.handleMessages))
	mux.HandleFunc("POST /v1/messages/count_tokens", s.requireAuth(s.handleMessagesCountTokens))
	mux.HandleFunc("POST /v1/embeddings", s.requireAuth(s.handleEmbeddings))
	mux.HandleFunc("GET /v1/models", s.requireAuth(s.handleModels))
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("POST /admin/reload", s.requireAdminToken(s.requireAuth(s.adminCSRF(http.HandlerFunc(s.handleReload)))))
	// Admin dashboard: cookie-authenticated browser UI. Assets are static
	// and public — the login page (served without a cookie) references them,
	// so they must NOT sit behind dashboardAuth. Overview/tokens/metrics are
	// read-only status and stay open when ADMIN_TOKEN is unset (legacy).
	// Config (read + write) and logs expose secrets and are gated further:
	// with ADMIN_TOKEN unset they require a loopback client.
	mux.HandleFunc("GET /admin/login", s.handleAdminLogin)
	// POST /admin/login consumes the per-IP login-attempt budget, so it must
	// carry the same CSRF gate as the other mutating admin routes: without it
	// a malicious page could fire cross-origin POSTs with wrong tokens and
	// lock the victim out of the dashboard (5 fails → 1-minute lockout,
	// repeatable). GET stays unwrapped — it just renders the login page.
	mux.HandleFunc("POST /admin/login", s.adminCSRF(http.HandlerFunc(s.handleAdminLogin)))
	mux.Handle("GET /admin", s.dashboardAuth(s.dash.Page("overview")))
	mux.Handle("GET /admin/tokens", s.dashboardAuth(s.dash.Page("tokens")))
	mux.Handle("GET /admin/models", s.dashboardAuth(s.dash.Page("models")))
	mux.Handle("GET /admin/traces", s.dashboardAuth(s.dash.Page("traces")))
	mux.Handle("GET /admin/setup", s.dashboardAuth(s.dash.Page("setup")))
	mux.Handle("GET /admin/playground", s.dashboardAuth(s.dash.Page("playground")))
	mux.Handle("POST /admin/playground/chat", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handlePlaygroundChat)))))
	mux.Handle("POST /admin/login/start", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleLoginStart)))))
	mux.Handle("GET /admin/login/status", s.dashboardAuth(s.adminSensitive(http.HandlerFunc(s.handleLoginStatus))))
	mux.Handle("GET /admin/config", s.dashboardAuth(s.adminSensitive(s.dash.Page("config"))))
	mux.Handle("GET /admin/logs", s.dashboardAuth(s.adminSensitive(s.dash.Page("logs"))))
	mux.Handle("GET /admin/metrics", s.dashboardAuth(s.dash.Page("metrics")))
	mux.Handle("POST /admin/config", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleConfigSave)))))
	mux.Handle("POST /admin/tokens/{id}/unlock", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleTokenUnlock)))))
	mux.Handle("POST /admin/tokens/{id}/finish", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleTokenFinish)))))
	mux.Handle("POST /admin/tokens/{id}/test", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleTokenTest)))))
	mux.Handle("POST /admin/tokens/test-all", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleTokenTestAll)))))
	mux.Handle("POST /admin/tokens/add", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleTokenAdd)))))
	mux.Handle("POST /admin/tokens/remove", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleTokenRemove)))))
	mux.Handle("POST /admin/mode", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleModeSwitch)))))
	mux.Handle("POST /admin/diag", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleDiag)))))
	mux.Handle("POST /admin/smoke", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleSmoke)))))
	// noDirListing must wrap StripPrefix, not the other way around: after
	// the strip the path is "" and a trailing-slash directory request would
	// slip past the guard into FileServerFS, which renders a listing.
	mux.Handle("GET /admin/assets/", noDirListing(http.StripPrefix("/admin/assets/", http.FileServerFS(mustSubFS(dashboard.AssetsFS(), "assets")))))
	// CORS middleware wraps the whole route table: it answers OPTIONS
	// preflights on the /v1/* API surface with 204 and stamps the allow
	// headers on every /v1/* response. Admin routes are intentionally left
	// untouched (cookie-authenticated dashboard; SameSite=Strict already
	// blocks cross-site reads, and an allow-origin would add nothing there).
	cors := s.corsMiddleware(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		cors.ServeHTTP(sw, r)
		s.logger.Info("access",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"ms", time.Since(start).Milliseconds(),
			"remote", remoteHost(r),
		)
	})
}

// corsOrigin returns the configured Access-Control-Allow-Origin, treating an
// empty value as the "*" default (an empty .env line must not disable CORS).
func (s *Server) corsOrigin() string {
	origin := strings.TrimSpace(s.cfg.Load().CORSAllowedOrigin)
	if origin == "" {
		return "*"
	}
	return origin
}

// corsMiddleware answers CORS preflights on the /v1/* API surface and stamps
// the allow headers on /v1/* responses. An OPTIONS request for any /v1/*
// path is answered with 204 before the route table sees it (so unknown
// /v1/* subpaths still get a clean preflight, matching the reference
// proxy-freebuff OPTIONS → 204). Admin routes pass through untouched.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			h := w.Header()
			origin := s.corsOrigin()
			h.Set("Access-Control-Allow-Origin", origin)
			// When the origin is pinned (not "*"), vary on Origin so caches
			// never serve the pinned header to a different requester.
			if origin != "*" {
				h.Add("Vary", "Origin")
			}
			h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, anthropic-version")
			h.Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// statusWriter captures the response status for access logging. It forwards
// Flusher/Hijacker/Pusher so streaming and similar protocols keep working
// through the access-log wrapper.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijack not supported")
}

func (w *statusWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

// mustSubFS returns the named subtree of an embed.FS. The directory is
// embedded at compile time, so a missing subtree is an invariant violation,
// not a runtime condition.
func mustSubFS(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic("dashboard: embedded subtree missing: " + err.Error())
	}
	return sub
}

// noDirListing rejects directory requests so FileServerFS never renders an
// index listing of the embedded assets.
func noDirListing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// remoteHost returns the client host without the port.
func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- auth ---

// requireAuth wraps a handler with client-auth enforcement. When no API keys
// are configured the handler passes through untouched; /healthz is always
// exempt (the caller wires it without requireAuth). Bridge mode (no
// AUTH_TOKENS) also passes through: the Authorization header IS the upstream
// token there, and API_KEYS is meaningless. Hybrid mode passes a Bearer
// token through (bridge relay: the client's own FreeBuff credential), but
// token-less requests fall back to the pool and must still pass the
// API_KEYS gate — an x-api-key is the API_KEYS scheme, never a bridge token.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfg.Load()
		if len(cfg.APIKeys) == 0 || cfg.BridgeMode() {
			next(w, r)
			return
		}
		if cfg.HybridMode && bearerToken(r) != "" {
			next(w, r)
			return
		}
		if !s.authorized(r) {
			s.writeJSONError(w, http.StatusUnauthorized,
				"Invalid API key", "invalid_request_error", "invalid_api_key", 0)
			return
		}
		next(w, r)
	}
}

// extractBearerToken extracts the token from an Authorization header if it has
// a case-insensitive "Bearer " prefix (per RFC 7235 / RFC 6750). Returns the
// trimmed token and true if the prefix matches, or ("", false) otherwise.
func extractBearerToken(authHeader string) (string, bool) {
	authHeader = strings.TrimSpace(authHeader)
	if len(authHeader) >= 7 && strings.EqualFold(authHeader[:7], "bearer ") {
		return strings.TrimSpace(authHeader[7:]), true
	}
	return "", false
}

// authorized reports whether the request carries a configured API key,
// either as "Authorization: Bearer <key>" or "x-api-key: <key>". Comparison
// is constant-time against every configured key.
func (s *Server) authorized(r *http.Request) bool {
	cfg := s.cfg.Load()
	provided := ""
	if tok, ok := extractBearerToken(r.Header.Get("Authorization")); ok {
		provided = tok
	} else if h := r.Header.Get("x-api-key"); h != "" {
		provided = h
	}
	if provided == "" {
		return false
	}
	for _, key := range cfg.APIKeys {
		if subtle.ConstantTimeCompare([]byte(provided), []byte(key)) == 1 {
			return true
		}
	}
	return false
}

// requireAdminToken guards POST /admin/reload when ADMIN_TOKEN is set: the
// request must present it as "Authorization: Bearer <token>" (constant-time
// compare). When ADMIN_TOKEN is unset the handler passes through untouched —
// the legacy API_KEYS gate still applies via requireAuth, and main.go logs a
// startup warning for the open (default) case.
func (s *Server) requireAdminToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfg.Load()
		if cfg.AdminToken == "" {
			next(w, r)
			return
		}
		provided := ""
		if tok, ok := extractBearerToken(r.Header.Get("Authorization")); ok {
			provided = tok
		}
		if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(cfg.AdminToken)) != 1 {
			s.writeJSONError(w, http.StatusUnauthorized,
				"Invalid admin token", "invalid_request_error", "invalid_admin_token", 0)
			return
		}
		next(w, r)
	}
}

// --- dashboard auth ---

// adminCookieName is the HttpOnly session cookie set after a successful
// ADMIN_TOKEN login. The value is stateless: unix expiry + HMAC-SHA256 over
// the expiry, keyed by a per-process random secret. No server-side session
// store; restart invalidates all sessions, which is the safe default for an
// admin UI.
const (
	adminCookieName = "fb_admin"
	adminCookieTTL  = 24 * time.Hour
)

// adminAuth issues and validates dashboard session cookies and rate-limits
// login attempts per remote IP.
type adminAuth struct {
	key   [32]byte
	mu    sync.Mutex
	fails map[string]failEntry
}

// failEntry tracks consecutive failed logins from one IP.
type failEntry struct {
	count int
	until time.Time
}

func newAdminAuth() *adminAuth {
	a := &adminAuth{fails: make(map[string]failEntry)}
	_, _ = rand.Read(a.key[:])
	return a
}

// cookieValue builds "expiry.hmac" for the given expiry.
func (a *adminAuth) cookieValue(expiry time.Time) string {
	mac := hmac.New(sha256.New, a.key[:])
	_, _ = mac.Write([]byte(strconv.FormatInt(expiry.Unix(), 10)))
	return strconv.FormatInt(expiry.Unix(), 10) + "." + hex.EncodeToString(mac.Sum(nil))
}

// valid reports whether the cookie value carries a not-yet-expired HMAC
// signature. Constant-time comparison via hmac.Equal.
func (a *adminAuth) valid(value string) bool {
	dot := strings.IndexByte(value, '.')
	if dot < 0 {
		return false
	}
	exp, err := strconv.ParseInt(value[:dot], 10, 64)
	if err != nil || time.Now().Unix() >= exp {
		return false
	}
	mac := hmac.New(sha256.New, a.key[:])
	_, _ = mac.Write([]byte(value[:dot]))
	got, err := hex.DecodeString(value[dot+1:])
	if err != nil || !hmac.Equal(got, mac.Sum(nil)) {
		return false
	}
	return true
}

// maxLoginFails caps consecutive failed logins from one IP before lockout;
// loginFailsCap bounds the fails map so distinct IP scans cannot grow it
// without bound (expired entries are dropped on access).
const (
	maxLoginFails = 5
	loginLockout  = time.Minute
	loginFailsCap = 1024
)

func (a *adminAuth) setCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    a.cookieValue(time.Now().Add(adminCookieTTL)),
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
		MaxAge:   int(adminCookieTTL.Seconds()),
	})
}

// allow reports whether ip may attempt a login right now. Entries track the
// running failure count until a lockout is set (until non-zero); an expired
// lockout is dropped so the map does not grow without bound.
func (a *adminAuth) allow(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.fails[ip]
	if !ok {
		return true
	}
	if !e.until.IsZero() {
		if time.Now().Before(e.until) {
			return false
		}
		delete(a.fails, ip)
	}
	return true
}

// recordFail counts a failed login, locking ip out after maxLoginFails. The
// map is capped: when a new IP arrives at the cap, expired entries are swept
// first, then the oldest remaining lockout is dropped (a brute-force scan
// rotating fresh IPs cannot grow the map without bound).
func (a *adminAuth) recordFail(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.fails[ip]
	e.count++
	if e.count >= maxLoginFails {
		e.until = time.Now().Add(loginLockout)
		e.count = 0
	}
	if _, exists := a.fails[ip]; !exists && len(a.fails) >= loginFailsCap {
		now := time.Now()
		for k, v := range a.fails {
			if now.After(v.until) {
				delete(a.fails, k)
			}
		}
		if len(a.fails) >= loginFailsCap {
			// No expired entries to reclaim — drop one lockout (map
			// iteration order is fine; the bound is what matters).
			for k := range a.fails {
				delete(a.fails, k)
				break
			}
		}
	}
	a.fails[ip] = e
}

func (a *adminAuth) clearFails(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.fails, ip)
}

// dashboardAuth guards the browser UI. With ADMIN_TOKEN unset the dashboard
// is open (legacy behavior, matching /admin/reload; main.go warns at startup).
// Otherwise the request must carry a valid fb_admin cookie; missing/invalid
// sessions are redirected to the login page. htmx polls get 401 + HX-Redirect
// so the login page replaces the swapped region instead of a bare fragment.
func (s *Server) dashboardAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfg.Load()
		if cfg.AdminToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie(adminCookieName); err == nil && s.adminAuth.valid(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/admin/login")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/admin/login", http.StatusFound)
	})
}

// adminSensitive gates the secret-bearing admin routes (config read/write,
// logs) in the default-open mode: when ADMIN_TOKEN is unset, only loopback
// clients may access them, so a remotely reachable proxy cannot leak or let
// anyone rewrite the .env. With ADMIN_TOKEN set the cookie gate already ran
// (this middleware is wrapped inside dashboardAuth).
func (s *Server) adminSensitive(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfg.Load()
		if cfg.AdminToken == "" && !isLoopback(r) {
			s.dash.RenderRestricted(w, r, "This page is only available to loopback clients while ADMIN_TOKEN is unset.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopback reports whether the request came from a loopback address.
func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// adminCSRF rejects cross-origin mutating admin requests. Browsers send
// Origin (and/or Sec-Fetch-Site) on every POST; a malicious site's form
// would carry an Origin that does not match the proxy's own host. Requests
// with NEITHER header (curl, API clients, legacy tests) pass through, so the
// admin API stays scriptable while a victim's browser cannot drive the
// dashboard cross-site. Origin is compared case-insensitively per RFC 6454
// host matching; Sec-Fetch-Site must be same-origin or none (direct
// navigation). Wired inside dashboardAuth → adminSensitive so the cookie
// and loopback gates still run first.
func (s *Server) adminCSRF(next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || !strings.EqualFold(u.Host, r.Host) {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					w.WriteHeader(http.StatusForbidden)
					s.dash.RenderConfigResult(w, r, false, "Cross-origin request rejected.")
					return
				}
			}
			if sfs := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); sfs != "" {
				if sfs != "same-origin" && sfs != "none" {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					w.WriteHeader(http.StatusForbidden)
					s.dash.RenderConfigResult(w, r, false, "Cross-origin request rejected.")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	}
}

// handleAdminLogin renders the login page and processes the token form:
// constant-time ADMIN_TOKEN comparison, per-IP rate limiting, and a signed
// session cookie on success. With ADMIN_TOKEN unset it redirects straight to
// the dashboard.
func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Load()
	if cfg.AdminToken == "" {
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}
	ip := remoteHost(r)
	if r.Method == http.MethodPost {
		if !s.adminAuth.allow(ip) {
			s.dash.RenderLogin(w, r, "Too many failed attempts — try again in a minute.")
			return
		}
		token := r.FormValue("token")
		if subtle.ConstantTimeCompare([]byte(token), []byte(cfg.AdminToken)) == 1 {
			s.adminAuth.clearFails(ip)
			// Secure only when the login arrived over an actual TLS
			// connection (direct HTTPS or a TLS-terminating reverse proxy
			// setting X-Forwarded-Proto). A Secure cookie over plain HTTP is
			// rejected by browsers, silently breaking remote login.
			s.adminAuth.setCookie(w, r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"))
			http.Redirect(w, r, "/admin", http.StatusFound)
			return
		}
		s.adminAuth.recordFail(ip)
		s.dash.RenderLogin(w, r, "Invalid admin token.")
		return
	}
	s.dash.RenderLogin(w, r, "")
}

// maxEnvSize caps the .env editor payload (64KB is generous for a config file).
const maxEnvSize = 64 << 10

// tokenActionID parses the {id} path value into a 0-based token index.
func tokenActionID(r *http.Request) (int, error) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 0 {
		return 0, errors.New("invalid token id")
	}
	return id, nil
}

// handleTokenUnlock clears a token's cooldown/rate-limit/ban lock. Gated as
// sensitive: unlocking a banned token lets upstream traffic resume, so it is
// loopback-only in open mode.
func (s *Server) handleTokenUnlock(w http.ResponseWriter, r *http.Request) {
	id, err := tokenActionID(r)
	if err == nil {
		err = s.pool.UnlockToken(id)
	}
	if err != nil {
		s.dash.RenderConfigResult(w, r, false, "Unlock failed: "+err.Error())
		return
	}
	s.logger.Info("dashboard token unlocked", "token", id)
	s.dash.RenderConfigResult(w, r, true, "Token "+strconv.Itoa(id)+" unlocked — no cooldown or ban window remains.")
}

// handleTokenFinish finishes all active runs of a token.
func (s *Server) handleTokenFinish(w http.ResponseWriter, r *http.Request) {
	id, err := tokenActionID(r)
	if err == nil {
		err = s.pool.FinishTokenRuns(r.Context(), id)
	}
	if err != nil {
		s.dash.RenderConfigResult(w, r, false, "Finish failed: "+err.Error())
		return
	}
	s.logger.Info("dashboard token runs finished", "token", id)
	s.dash.RenderConfigResult(w, r, true, "Token "+strconv.Itoa(id)+" runs finished.")
}

// probeModel returns the safest model to default a smoke test to: the
// fallback default (deepseek-v4-flash — the model every account gets, incl.
// limited tier) when it is in the catalog, else the first catalog model.
// Alphabetical models[0] would otherwise pick anthropic/claude-fable-5, a
// capacity-gated offer model that makes smoke tests fail on most accounts.
func probeModel(reg *registry.Registry) string {
	models := reg.Models()
	if len(models) == 0 {
		return ""
	}
	for _, id := range models {
		if id == session.DefaultFallbackModel {
			return id
		}
	}
	return models[0]
}

// quotaSummary renders the live per-model session quota from a probe's
// RateLimitsByModel map (models sorted for determinism), plus the account
// tier and glmPromo when the response carried them (the
// x-freebuff-include-unused-rate-limits probe header asks upstream to
// include the unused limits); "" when the upstream response carried no quota
// data (compact responses omit it).
func quotaSummary(st *upstream.SessionState) string {
	if st == nil || (len(st.RateLimitsByModel) == 0 && st.AccessTier == "" && st.GlmPromo == "") {
		return ""
	}
	var parts []string
	if st.AccessTier != "" {
		parts = append(parts, "tier "+st.AccessTier)
	}
	models := make([]string, 0, len(st.RateLimitsByModel))
	for id := range st.RateLimitsByModel {
		models = append(models, id)
	}
	sort.Strings(models)
	for _, id := range models {
		q := st.RateLimitsByModel[id]
		entry := fmt.Sprintf("%s %s/%s", id, strconv.FormatFloat(q.Limit, 'f', -1, 64), strconv.FormatFloat(q.RecentCount, 'f', -1, 64))
		if q.Period != "" {
			entry += " " + q.Period
		}
		if !q.ResetAt.IsZero() {
			entry += fmt.Sprintf(", resets %s", q.ResetAt.Format(time.RFC3339))
		}
		parts = append(parts, entry)
	}
	if st.GlmPromo != "" {
		parts = append(parts, "glmPromo "+st.GlmPromo)
	}
	return "quota: " + strings.Join(parts, "; ")
}

// handleTokenTest probes a token with a zero-cost upstream GET probe (no
// session claim, no model needed) and renders the result plus the live
// per-model quota when the upstream response carries it.
func (s *Server) handleTokenTest(w http.ResponseWriter, r *http.Request) {
	id, err := tokenActionID(r)
	var state *upstream.SessionState
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		state, err = s.pool.ProbeToken(ctx, id)
	}
	if err != nil {
		if errors.Is(err, upstream.ErrNoActiveSession) {
			s.logger.Info("dashboard token probe ok (no active session)", "token", id)
			s.dash.RenderConfigResult(w, r, true, "Token "+strconv.Itoa(id)+" OK — zero-cost probe succeeded (no active session).")
			return
		}
		s.logger.Warn("dashboard token probe failed", "token", id, "err", err)
		s.dash.RenderConfigResult(w, r, false, "Token "+strconv.Itoa(id)+" test failed: "+err.Error())
		return
	}
	msg := "Token " + strconv.Itoa(id) + " OK — zero-cost probe succeeded"
	if q := quotaSummary(state); q != "" {
		msg += " (" + q + ")"
	}
	msg += "."
	s.logger.Info("dashboard token probe ok", "token", id)
	s.dash.RenderConfigResult(w, r, true, msg)
}

// handleTokenTestAll probes every pooled token (dashboard "Test all"). Each
// probe is a zero-cost upstream GET (no session claim, no model needed);
// per-token results are rendered as a fragment.
func (s *Server) handleTokenTestAll(w http.ResponseWriter, r *http.Request) {
	count := 0
	for _, snap := range s.pool.PoolSnapshot().Tokens {
		i := snap.Token
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		state, err := s.pool.ProbeToken(ctx, i)
		cancel()
		ok := err == nil || errors.Is(err, upstream.ErrNoActiveSession)
		msg := "ok"
		switch {
		case errors.Is(err, upstream.ErrNoActiveSession):
			msg = "ok (no active session)"
		case err != nil:
			msg = err.Error()
		default:
			if q := quotaSummary(state); q != "" {
				msg = "ok (" + q + ")"
			}
		}
		s.dash.RenderTestResult(w, r, i, ok, msg, "")
		count++
	}
	if count == 0 {
		s.dash.RenderConfigResult(w, r, false, "No tokens to test (bridge mode has no fixed AUTH_TOKENS).")
	}
}

// handleEmbeddings answers POST /v1/embeddings with a structured
// unsupported-endpoint error: the proxy serves chat completions only, and
// the error body points clients at /v1/chat/completions and the live model
// list so a picker/fallback client can self-correct. 400 with the
// documented "unsupported_endpoint" code (distinct from the mux's bare 404,
// which gives an embeddings client no actionable signal).
func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	s.writeJSONError(w, http.StatusBadRequest,
		"this proxy serves chat completions only; embeddings are not supported. Use POST /v1/chat/completions with one of: "+strings.Join(s.reg.Models(), ", "),
		"unsupported_endpoint", "unsupported_endpoint", 0)
}

// smokeRequest is the dashboard smoke-test payload (a real chat through the
// exact client path clients use).
type smokeRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Token  string `json:"token"` // bridge mode: client token to relay upstream
}

// maxSmokeBytes bounds the upstream body read for the smoke preview.
const maxSmokeBytes = 32 << 10

// handleSmoke sends one real chat request through the pool (Acquire + Chat,
// the same path clients use) and reports status, latency, and a content
// preview. Bridge mode requires a client token in the payload.
func (s *Server) handleSmoke(w http.ResponseWriter, r *http.Request) {
	var req smokeRequest
	// The dashboard form posts urlencoded model=&prompt=&token=; read those
	// first and only fall back to JSON for programmatic clients (mirrors
	// handleTokenAdd).
	var err error
	req.Model = strings.TrimSpace(r.FormValue("model"))
	req.Prompt = strings.TrimSpace(r.FormValue("prompt"))
	req.Token = strings.TrimSpace(r.FormValue("token"))
	if req.Model == "" && req.Prompt == "" && req.Token == "" {
		var body []byte
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			s.dash.RenderConfigResult(w, r, false, "Failed to read request: "+err.Error())
			return
		}
		if err = json.Unmarshal(body, &req); err != nil {
			s.dash.RenderConfigResult(w, r, false, "Invalid request JSON: "+err.Error())
			return
		}
	}
	req.Model = strings.TrimSpace(req.Model)
	req.Prompt = strings.TrimSpace(req.Prompt)
	req.Token = strings.TrimSpace(req.Token)
	if req.Model == "" {
		req.Model = probeModel(s.reg)
		if req.Model == "" {
			s.dash.RenderConfigResult(w, r, false, "No models in the registry to test.")
			return
		}
	}
	if req.Prompt == "" {
		req.Prompt = "ping"
	}
	if len(req.Prompt) > 200 {
		s.dash.RenderConfigResult(w, r, false, "Prompt too long (max 200 chars).")
		return
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	ctx, phases := phasetiming.WithContext(ctx)

	cfg := s.cfg.Load()
	chatBody := []byte(`{"model":` + strconv.Quote(req.Model) + `,"messages":[{"role":"user","content":` + strconv.Quote(req.Prompt) + `}],"stream":false}`)
	chatOpts := upstream.ChatOptions{Model: req.Model}

	var lease *pool.Lease
	var up io.ReadCloser
	acquireStart := time.Now()
	if cfg.BridgeMode() {
		if req.Token == "" {
			s.dash.RenderConfigResult(w, r, false, "Bridge mode: include a client token in the smoke request.")
			return
		}
		lease, err = s.pool.AcquireBridge(ctx, req.Token, req.Model)
	} else {
		lease, err = s.pool.Acquire(ctx, req.Model)
	}
	phases.Since(phasetiming.AcquireMS, acquireStart)
	if err == nil {
		up, err = s.pool.Chat(ctx, lease, chatOpts, chatBody)
	}
	if err != nil {
		if lease != nil {
			s.pool.LeaseRelease(lease)
		}
		phases.Since(phasetiming.TotalMS, start)
		s.logger.Warn("dashboard smoke test failed", "model", req.Model, "err", err)
		s.dash.RenderConfigResult(w, r, false, "Smoke test failed: "+err.Error())
		return
	}
	defer s.pool.LeaseRelease(lease)
	defer func() { _ = up.Close() }()

	// Read a bounded prefix of the SSE stream for the preview.
	chatStart := time.Now()
	preview, readErr := readBounded(up, maxSmokeBytes)
	phases.Since(phasetiming.UpstreamTTFBMS, chatStart)
	phases.Since(phasetiming.TotalMS, start)
	ms := time.Since(start).Milliseconds()
	if readErr != nil {
		s.dash.RenderConfigResult(w, r, false, "Smoke test: upstream accepted but stream read failed: "+readErr.Error())
		return
	}
	s.dash.RenderSmokeResult(w, r, req.Model, tokenLabel(lease), ms, preview, dashboard.PhaseList(phases.All()))
}

// handlePlaygroundChat is the dashboard playground's streaming chat
// endpoint (issue #45): it routes a {model, prompt} through the exact same
// /v1/chat/completions pipeline (acquire → upstream → SSE relay) without an
// API key — dashboard auth + CSRF already ran. The page streams the SSE.
func (s *Server) handlePlaygroundChat(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "failed to read request: "+err.Error(), "invalid_request_error", "invalid_json", 0)
		return
	}
	var req struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "request must be a JSON object", "invalid_request_error", "invalid_json", 0)
		return
	}
	req.Model = strings.TrimSpace(req.Model)
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Model == "" {
		if m := probeModel(s.reg); m != "" {
			req.Model = m
		} else {
			s.writeJSONError(w, http.StatusBadRequest, "no model specified and no models in the registry", "invalid_request_error", "model_not_found", 0)
			return
		}
	}
	if req.Prompt == "" {
		req.Prompt = "ping"
	}
	// Build a chat-completions request and run it through the real handler
	// (streaming forced, exactly like /v1/chat/completions).
	chatBody := []byte(`{"model":` + strconv.Quote(req.Model) +
		`,"messages":[{"role":"user","content":` + strconv.Quote(req.Prompt) + `}],"stream":true}`)
	playReq := r.Clone(r.Context())
	playReq.Body = io.NopCloser(bytes.NewReader(chatBody))
	playReq.ContentLength = int64(len(chatBody))
	s.handleChat(w, playReq)
}

// handleLoginStart begins the headless OAuth login wizard (issue #62):
// POST /admin/login/start → the server requests a fresh /api/auth/cli/code
// from upstream and returns {flow_id, login_url, expires_at} for the page
// to hand to the user; the page then polls GET /admin/login/status.
func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if s.authClient == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "login wizard disabled (no upstream auth client)", "server_error", "login_unavailable", 0)
		return
	}
	s.pruneLoginFlows()
	code, err := s.authClient.StartCLILogin(r.Context())
	if err != nil {
		s.logger.Warn("login wizard: start failed", "err", err)
		s.writeJSONError(w, http.StatusBadGateway, "failed to start browser login: "+err.Error(), "server_error", "login_start_failed", 0)
		return
	}
	flowID := shortFlowID(code.FingerprintID)
	flow := &loginFlow{ID: flowID, Code: code, Started: time.Now()}
	s.loginMu.Lock()
	s.loginFlows[code.FingerprintID] = flow
	s.loginMu.Unlock()
	s.logger.Info("login wizard: started", "flow", flowID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"flow_id":     flowID,
		"fingerprint": code.FingerprintID, // full id: the status poll key
		"login_url":   code.LoginURL,
		"expires_at":  code.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// handleLoginStatus polls an in-flight login (issue #62): the server polls
// upstream /api/auth/cli/status; when the authToken appears the token is
// added to the live pool AND persisted to .env (survives restart), like the
// dashboard "Add token" action.
func (s *Server) handleLoginStatus(w http.ResponseWriter, r *http.Request) {
	s.pruneLoginFlows()
	fp := strings.TrimSpace(r.URL.Query().Get("fingerprint"))
	if fp == "" {
		s.writeJSONError(w, http.StatusBadRequest, "missing fingerprint query param", "invalid_request_error", "bad_request", 0)
		return
	}
	s.loginMu.Lock()
	flow := s.loginFlows[fp]
	s.loginMu.Unlock()
	if flow == nil {
		s.writeJSONError(w, http.StatusNotFound, "login flow not found or expired — start a new one", "invalid_request_error", "login_flow_missing", 0)
		return
	}
	// Read the completion state under the lock: concurrent status polls
	// (second tab, htmx retry) must not both proceed to addTokenPersist —
	// the completing flag is set before the network poll so exactly one
	// goroutine owns the add.
	s.loginMu.Lock()
	done := flow.Done
	completing := flow.Completing
	flow.Completing = true
	s.loginMu.Unlock()
	if done {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "token_index": flow.Index, "token": flow.Token})
		return
	}
	if completing {
		// Another poll is mid-completion; report pending so the client
		// re-polls instead of double-adding.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "pending"})
		return
	}
	status, err := s.authClient.PollCLILogin(r.Context(), flow.Code)
	if err != nil {
		// Transient poll failure: keep the flow alive, report pending. A
		// later poll may retry completion.
		s.loginMu.Lock()
		flow.Completing = false
		s.loginMu.Unlock()
		s.logger.Debug("login wizard: poll failed", "flow", flow.ID, "err", err)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "pending"})
		return
	}
	if !status.Done {
		s.loginMu.Lock()
		flow.Completing = false
		s.loginMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "pending"})
		return
	}
	// Completed: add to the pool + persist to .env (mirrors handleTokenAdd).
	// All completion fields are written under the lock so a concurrent poll
	// observing Done reads a consistent record.
	flow.Done = true
	flow.Token = status.AuthToken
	s.loginMu.Lock()
	s.loginFlows[fp] = flow
	s.loginMu.Unlock()
	index, addErr := s.addTokenPersist(r.Context(), status.AuthToken)
	if addErr != nil {
		flow.Error = addErr.Error()
		s.loginMu.Lock()
		flow.Completing = false
		s.loginMu.Unlock()
		s.logger.Warn("login wizard: token persist failed", "flow", flow.ID, "err", addErr)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": addErr.Error()})
		return
	}
	flow.Index = index
	s.logger.Info("login wizard: completed", "flow", flow.ID, "token_index", index, "user", status.User.Name)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "token_index": index, "user": status.User.Name})
}

// addTokenPersist adds token to the live pool and persists the new
// AUTH_TOKENS list to .env (mirrors handleTokenAdd's mutation + persistence
// sequence, without the dashboard fragment render).
func (s *Server) addTokenPersist(ctx context.Context, token string) (int, error) {
	cfg := s.cfg.Load()
	existing := cfg.AuthTokens
	if len(existing) > 0 {
		idx, err := s.pool.AddToken(token)
		if err != nil {
			return 0, fmt.Errorf("add token to pool: %w", err)
		}
		// Persist the runtime list (pool may have bridge additions too, but
		// AUTH_TOKENS is the fixed set — append only when not already there).
		tokens := append([]string(nil), existing...)
		seen := false
		for _, t := range tokens {
			if t == token {
				seen = true
				break
			}
		}
		if !seen {
			tokens = append(tokens, token)
		}
		if err := s.syncTokensAfterMutation(tokens); err != nil {
			return 0, err
		}
		return idx, nil
	}
	// Bridge mode (no fixed tokens): the first wizard token switches to
	// pooled mode, exactly like handleTokenAdd.
	idx, err := s.pool.AddToken(token)
	if err != nil {
		return 0, fmt.Errorf("add token to pool: %w", err)
	}
	if err := s.syncTokensAfterMutation([]string{token}); err != nil {
		return 0, err
	}
	return idx, nil
}

// shortFlowID renders a compact flow id for the UI/logs.
func shortFlowID(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	return fp
}

// pruneLoginFlows drops flows older than loginFlowTTL.
func (s *Server) pruneLoginFlows() {
	cutoff := time.Now().Add(-loginFlowTTL)
	s.loginMu.Lock()
	for fp, flow := range s.loginFlows {
		if flow.Started.Before(cutoff) {
			delete(s.loginFlows, fp)
		}
	}
	s.loginMu.Unlock()
}

// readBounded reads up to n bytes from r, tolerating an EOF mid-prefix.
func readBounded(r io.Reader, n int) ([]byte, error) {
	buf := make([]byte, n)
	got, err := io.ReadFull(r, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return buf[:got], nil
}

// envUpdate is one KEY=VALUE replacement for updateEnvKeys.
type envUpdate struct {
	Key   string
	Value string
}

// updateEnvKeys rewrites the given KEY=VALUE lines in .env (appending each
// missing key), preserving every other line. The existing EOL style is
// preserved — CRLF files stay CRLF — so a Windows-edited .env is never
// rewritten with mixed line endings. Updates apply in order; later updates
// to an already-replaced key win (callers keep keys distinct).
func updateEnvKeys(updates []envUpdate) ([]byte, error) {
	content, err := os.ReadFile(".env")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	crlf := bytes.Contains(content, []byte("\r"))
	lines := make([]string, 0, len(content)/8)
	for _, l := range strings.Split(string(content), "\n") {
		lines = append(lines, strings.TrimSuffix(l, "\r"))
	}
	// A file ending with a newline has a trailing "" split element that is
	// an artifact of that newline, not a real blank line; drop it so
	// appended keys do not land after a spurious blank line.
	trailingNL := len(content) > 0 && content[len(content)-1] == '\n'
	if trailingNL {
		if n := len(lines); n > 0 && lines[n-1] == "" {
			lines = lines[:n-1]
		}
	}
	for _, u := range updates {
		line := u.Key + "=" + u.Value
		replaced := false
		for i, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), u.Key+"=") {
				lines[i] = line
				replaced = true
				break
			}
		}
		if !replaced {
			if n := len(lines); n == 1 && lines[0] == "" {
				// Empty (or missing) file: the new line is the whole file.
				lines[0] = line
			} else {
				lines = append(lines, line)
			}
		}
	}
	eol := "\n"
	if crlf {
		eol = "\r\n"
	}
	out := []byte(strings.Join(lines, eol))
	if trailingNL {
		out = append(out, eol...)
	}
	if err := writeFileAtomic(".env", out); err != nil {
		return nil, err
	}
	return out, nil
}

// updateAuthTokensEnv rewrites the AUTH_TOKENS= line in .env (appending it
// when absent), preserving every other line. Returns the new content. The
// existing EOL style is preserved — CRLF files stay CRLF — so a
// Windows-edited .env is never rewritten with mixed line endings.
func updateAuthTokensEnv(tokens []string) ([]byte, error) {
	return updateEnvKeys([]envUpdate{{Key: "AUTH_TOKENS", Value: strings.Join(tokens, ",")}})
}

// syncTokensAfterMutation updates .env + reloads config after a pool token
// mutation, so the change survives a restart and cfg reflects the new list.
func (s *Server) syncTokensAfterMutation(tokens []string) error {
	if _, err := updateAuthTokensEnv(tokens); err != nil {
		return fmt.Errorf("persist AUTH_TOKENS: %w", err)
	}
	newCfg, err := config.Load(s.configPath)
	if err != nil {
		return fmt.Errorf("reload config: %w", err)
	}
	s.cfg.Store(&newCfg)
	s.reg.SetConfig(&newCfg)
	s.pool.SetConfig(&newCfg)
	return nil
}

// handleTokenAdd adds a token to the live pool and persists it (dashboard
// "Add token"). Rolls the pool back if persistence fails.
func (s *Server) handleTokenAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	req.Token = strings.TrimSpace(r.FormValue("token"))
	if req.Token == "" {
		// JSON fallback for programmatic clients.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<10))
		if err != nil {
			s.dash.RenderConfigResult(w, r, false, "Failed to read request: "+err.Error())
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			s.dash.RenderConfigResult(w, r, false, "Invalid request: "+err.Error())
			return
		}
	}
	req.Token = strings.TrimSpace(req.Token)
	if req.Token == "" || strings.HasPrefix(strings.ToLower(req.Token), "bearer ") {
		s.dash.RenderConfigResult(w, r, false, "Invalid token (must not start with 'Bearer ').")
		return
	}

	// adminSaveMu serializes the pool mutation + persist + reload with the
	// other .env writers (config editor, token remove, mode switch) so a
	// concurrent save cannot interleave and lose a token from .env.
	s.adminSaveMu.Lock()
	defer s.adminSaveMu.Unlock()

	cfg := s.cfg.Load()
	// Divergence guard (mirrors handleTokenRemove): a config-editor
	// AUTH_TOKENS edit or /admin/reload can diverge cfg.AuthTokens from the
	// live pool. Adding to a stale list would persist cfg.AuthTokens+new to
	// .env while the pool holds its own list, leaving pool/.env/cfg
	// permanently divergent — and the next remove is rejected by the same
	// guard, stranding the operator until restart.
	if len(cfg.AuthTokens) != s.pool.TokenCount() {
		s.dash.RenderConfigResult(w, r, false, "AUTH_TOKENS in .env differs from the live pool — reconcile in the Config editor or restart.")
		return
	}
	idx, err := s.pool.AddToken(req.Token)
	if err != nil {
		s.dash.RenderConfigResult(w, r, false, err.Error())
		return
	}
	tokens := append(append([]string{}, cfg.AuthTokens...), req.Token)
	if err := s.syncTokensAfterMutation(tokens); err != nil {
		_ = s.pool.RemoveLastToken()
		s.logger.Warn("dashboard token add rolled back", "err", err)
		s.dash.RenderConfigResult(w, r, false, err.Error())
		return
	}
	s.logger.Info("dashboard token added", "index", idx)
	s.dash.RenderConfigResult(w, r, true, "Token added at index "+strconv.Itoa(idx)+" and persisted to .env.")
}

// handleTokenRemove removes the last pooled token (dashboard "Remove last").
func (s *Server) handleTokenRemove(w http.ResponseWriter, r *http.Request) {
	// adminSaveMu serializes the pool mutation + persist + reload with the
	// other .env writers, exactly like handleTokenAdd.
	s.adminSaveMu.Lock()
	defer s.adminSaveMu.Unlock()

	cfg := s.cfg.Load()
	// A config-editor AUTH_TOKENS edit or /admin/reload can diverge
	// cfg.AuthTokens from the live pool; removing "the last token" from a
	// stale list would persist the wrong .env and leave pool/.env/cfg
	// permanently inconsistent.
	if len(cfg.AuthTokens) != s.pool.TokenCount() {
		s.dash.RenderConfigResult(w, r, false, "AUTH_TOKENS in .env differs from the live pool — reconcile in the Config editor or restart.")
		return
	}
	removed := ""
	if len(cfg.AuthTokens) > 0 {
		removed = cfg.AuthTokens[len(cfg.AuthTokens)-1]
	}
	if err := s.pool.RemoveLastToken(); err != nil {
		s.dash.RenderConfigResult(w, r, false, err.Error())
		return
	}
	tokens := cfg.AuthTokens
	if len(tokens) > 0 {
		tokens = tokens[:len(tokens)-1]
	}
	if err := s.syncTokensAfterMutation(tokens); err != nil {
		// Roll the pool back so a failed persist does not leave the token
		// removed from the pool but still listed in .env/cfg (mirrors
		// handleTokenAdd's rollback).
		if removed != "" {
			if _, addErr := s.pool.AddToken(removed); addErr != nil {
				s.logger.Warn("dashboard token remove rollback re-add failed", "err", addErr)
			}
		}
		s.logger.Warn("dashboard token remove rolled back", "err", err)
		s.dash.RenderConfigResult(w, r, false, err.Error())
		return
	}
	s.logger.Info("dashboard token removed")
	s.dash.RenderConfigResult(w, r, true, "Last token removed and persisted to .env.")
}

// handleModeSwitch flips between bridge, pooled, and hybrid mode at runtime
// (dashboard mode control). Pooled→bridge removes all tokens; bridge→pooled
// requires at least one token to add (use the Add-token form first). Hybrid
// keeps the pooled tokens and additionally relays client-supplied tokens;
// switching to it persists HYBRID_MODE=true in .env.
//
// Order matters: the .env is persisted and the config reloaded BEFORE the
// live pool is drained, and the reload result is verified to actually be in
// the requested mode. Otherwise a failed persist (or a higher-precedence
// AUTH_TOKENS/HYBRID_MODE source such as a -config JSON file or real
// environment variable) would empty the pool while cfg still claims pooled —
// leaving the proxy broken and the dashboard pill showing the old mode.
func (s *Server) handleModeSwitch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
	}
	req.Mode = r.FormValue("mode")
	if req.Mode == "" {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
		if err != nil {
			s.dash.RenderConfigResult(w, r, false, "Failed to read request: "+err.Error())
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			s.dash.RenderConfigResult(w, r, false, "Invalid request: "+err.Error())
			return
		}
	}
	cfg := s.cfg.Load()
	switch strings.ToLower(strings.TrimSpace(req.Mode)) {
	case "bridge":
		if cfg.BridgeMode() && !cfg.HybridMode {
			s.dash.RenderConfigResult(w, r, false, "Already in bridge mode.")
			return
		}
		// adminSaveMu serializes the persist → verify → rollback sequence
		// with the other .env writers (config editor, token add/remove) so a
		// concurrent save cannot interleave between the write and the reload.
		// The live-pool drain stays outside the lock, after the reload is
		// verified (persist → verify → drain).
		s.adminSaveMu.Lock()
		defer s.adminSaveMu.Unlock()
		// Persist AUTH_TOKENS= (explicit empty) + HYBRID_MODE=false and
		// reload, verifying the effective config actually lands in bridge
		// mode before touching the live pool. Roll the .env back on failure.
		old, oldErr := os.ReadFile(".env")
		if _, err := updateEnvKeys([]envUpdate{{Key: "AUTH_TOKENS", Value: ""}, {Key: "HYBRID_MODE", Value: "false"}}); err != nil {
			s.dash.RenderConfigResult(w, r, false, "Failed to persist .env: "+err.Error())
			return
		}
		newCfg, err := config.Load(s.configPath)
		if err != nil {
			restoreEnvFile(old, oldErr)
			s.dash.RenderConfigResult(w, r, false, "Reload rejected: "+err.Error())
			return
		}
		if !newCfg.BridgeMode() {
			// A higher-precedence source (e.g. AUTH_TOKENS in a -config JSON
			// file or the real environment) still supplies tokens — .env alone
			// cannot clear it, so the switch cannot succeed.
			restoreEnvFile(old, oldErr)
			s.dash.RenderConfigResult(w, r, false, "Could not switch to bridge mode: AUTH_TOKENS is still set by a -config JSON file or the environment, which overrides .env. Clear it there, or run without -config, then retry.")
			return
		}
		s.cfg.Store(&newCfg)
		s.reg.SetConfig(&newCfg)
		s.pool.SetConfig(&newCfg)
		s.pool.RemoveAllTokens(r.Context())
		s.logger.Info("dashboard switched to bridge mode")
		s.dash.RenderConfigResult(w, r, true, "Switched to bridge mode — AUTH_TOKENS cleared; clients now send their own token.")
	case "pooled":
		if !cfg.BridgeMode() && !cfg.HybridMode {
			s.dash.RenderConfigResult(w, r, false, "Already in pooled mode.")
			return
		}
		if cfg.BridgeMode() {
			s.dash.RenderConfigResult(w, r, false, "Pooled mode needs tokens — add one via the Add-token form first.")
			return
		}
		// Hybrid → pooled: keep the tokens, just clear HYBRID_MODE.
		s.adminSaveMu.Lock()
		defer s.adminSaveMu.Unlock()
		old, oldErr := os.ReadFile(".env")
		if _, err := updateEnvKeys([]envUpdate{{Key: "HYBRID_MODE", Value: "false"}}); err != nil {
			s.dash.RenderConfigResult(w, r, false, "Failed to persist .env: "+err.Error())
			return
		}
		newCfg, err := config.Load(s.configPath)
		if err != nil {
			restoreEnvFile(old, oldErr)
			s.dash.RenderConfigResult(w, r, false, "Reload rejected: "+err.Error())
			return
		}
		if newCfg.HybridMode {
			restoreEnvFile(old, oldErr)
			s.dash.RenderConfigResult(w, r, false, "Could not switch to pooled mode: HYBRID_MODE is still true via a -config JSON file or the environment, which overrides .env. Clear it there, then retry.")
			return
		}
		s.cfg.Store(&newCfg)
		s.reg.SetConfig(&newCfg)
		s.pool.SetConfig(&newCfg)
		s.logger.Info("dashboard switched to pooled mode", "auth_tokens", len(newCfg.AuthTokens))
		s.dash.RenderConfigResult(w, r, true, "Switched to pooled mode — HYBRID_MODE cleared; all requests now use the pool.")
	case "hybrid":
		if cfg.HybridMode {
			s.dash.RenderConfigResult(w, r, false, "Already in hybrid mode.")
			return
		}
		// Hybrid → pooled: keep the tokens, just clear HYBRID_MODE.
		s.adminSaveMu.Lock()
		defer s.adminSaveMu.Unlock()
		old, oldErr := os.ReadFile(".env")
		if _, err := updateEnvKeys([]envUpdate{{Key: "HYBRID_MODE", Value: "true"}}); err != nil {
			s.dash.RenderConfigResult(w, r, false, "Failed to persist .env: "+err.Error())
			return
		}
		newCfg, err := config.Load(s.configPath)
		if err != nil {
			restoreEnvFile(old, oldErr)
			s.dash.RenderConfigResult(w, r, false, "Reload rejected: "+err.Error())
			return
		}
		if !newCfg.HybridMode {
			restoreEnvFile(old, oldErr)
			s.dash.RenderConfigResult(w, r, false, "Could not switch to hybrid mode: HYBRID_MODE is still false via a -config JSON file or the environment, which overrides .env. Set it there, then retry.")
			return
		}
		s.cfg.Store(&newCfg)
		s.reg.SetConfig(&newCfg)
		s.pool.SetConfig(&newCfg)
		msg := "Switched to hybrid mode — clients with a token relay it; token-less requests use the pool."
		if len(newCfg.AuthTokens) == 0 {
			msg += " Warning: no AUTH_TOKENS — token-less requests will fail (502) until a token is added."
			s.logger.Warn("hybrid mode enabled without AUTH_TOKENS: token-less requests will 502 until a token is added")
		} else {
			s.logger.Info("dashboard switched to hybrid mode", "auth_tokens", len(newCfg.AuthTokens))
		}
		s.dash.RenderConfigResult(w, r, true, msg)
	default:
		s.dash.RenderConfigResult(w, r, false, "Mode must be 'bridge', 'pooled', or 'hybrid'.")
	}
}

// restoreEnvFile writes old content back to .env, or removes the file when it
// did not exist before. Best-effort rollback for failed mode switches. When
// the previous .env existed but was unreadable (oldErr not os.ErrNotExist),
// nothing is done: removing it would destroy the operator's file, and the old
// bytes needed for a restore were never read.
func restoreEnvFile(old []byte, oldErr error) {
	switch {
	case oldErr == nil:
		_ = writeFileAtomic(".env", old)
	case errors.Is(oldErr, os.ErrNotExist):
		_ = os.Remove(".env")
	}
}

// dialTarget returns the host:port to dial for an upstream base host,
// defaulting to 443 only when the host carries no explicit port — an
// UpstreamBaseURL like "https://host:8443" must not become "host:8443:443".
func dialTarget(host string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(host, "443")
}

// handleDiag runs the dashboard diagnostics: config state, upstream
// reachability (DNS + TLS), registry health, and per-token validity probes —
// the same checks -doctor performs, rendered as a fragment. The probes are
// zero-cost upstream GETs (no session claim, no model needed), so they always
// run for pooled and hybrid modes.
func (s *Server) handleDiag(w http.ResponseWriter, r *http.Request) {
	checks := []dashboard.DiagCheck{}

	cfg := s.cfg.Load()
	switch cfg.EffectiveMode() {
	case "bridge":
		checks = append(checks, dashboard.DiagCheck{OK: true, Message: "Configuration: bridge mode (clients relay their own token)"})
	case "hybrid":
		checks = append(checks, dashboard.DiagCheck{OK: true, Message: fmt.Sprintf("Configuration: hybrid mode, %d pooled token(s) (client tokens relayed; token-less requests use the pool)", len(cfg.AuthTokens))})
	default:
		checks = append(checks, dashboard.DiagCheck{OK: true, Message: fmt.Sprintf("Configuration: pooled mode, %d token(s)", len(cfg.AuthTokens))})
	}

	// Upstream reachability: DNS + TLS to the configured base host. The DNS
	// lookup uses the bare host, not u.Host verbatim: "host:8443" would be
	// treated as a literal DNS name and NXDOMAIN, a false red row (the -doctor
	// tool strips the port the same way). The display and dial target keep the
	// port so the TCP row still connects to the real endpoint.
	targetHost := "www.codebuff.com"
	dnsHost := targetHost
	if u, err := url.Parse(cfg.UpstreamBaseURL); err == nil && u.Host != "" {
		targetHost = u.Host
		if h := u.Hostname(); h != "" {
			dnsHost = h
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if _, err := net.DefaultResolver.LookupHost(ctx, dnsHost); err != nil {
		checks = append(checks, dashboard.DiagCheck{Message: "DNS lookup failed for " + dnsHost + ": " + err.Error()})
	} else {
		checks = append(checks, dashboard.DiagCheck{OK: true, Message: "DNS resolves " + dnsHost})
	}
	hostForDial := dialTarget(targetHost)
	if conn, err := net.DialTimeout("tcp", hostForDial, 5*time.Second); err != nil {
		checks = append(checks, dashboard.DiagCheck{Message: "TCP connect to " + hostForDial + " failed: " + err.Error()})
	} else {
		_ = conn.Close()
		checks = append(checks, dashboard.DiagCheck{OK: true, Message: "TCP reachable " + hostForDial})
	}

	checks = append(checks, dashboard.DiagCheck{OK: true, Message: fmt.Sprintf("Model registry: %d models", s.reg.ModelCount())})

	// Per-token validity probes (pooled and hybrid-with-tokens modes). Each
	// probe is a zero-cost upstream GET /api/v1/freebuff/session (no session
	// claim, no model needed), so they always run; a token with no active
	// session still counts as valid.
	if !cfg.BridgeMode() {
		for _, snap := range s.pool.PoolSnapshot().Tokens {
			idx := snap.Token
			probeCtx, probeCancel := context.WithTimeout(r.Context(), 8*time.Second)
			state, err := s.pool.ProbeToken(probeCtx, idx)
			probeCancel()
			switch {
			case errors.Is(err, upstream.ErrNoActiveSession):
				checks = append(checks, dashboard.DiagCheck{OK: true, Message: fmt.Sprintf("Token #%d validity probe succeeded (no active session)", idx+1)})
			case err != nil:
				checks = append(checks, dashboard.DiagCheck{Message: fmt.Sprintf("Token #%d validity probe failed: %v", idx+1, err)})
			default:
				msg := fmt.Sprintf("Token #%d validity probe succeeded", idx+1)
				if q := quotaSummary(state); q != "" {
					msg += " (" + q + ")"
				}
				checks = append(checks, dashboard.DiagCheck{OK: true, Message: msg})
			}
		}
	} else {
		checks = append(checks, dashboard.DiagCheck{Warn: true, Message: "No pooled tokens to probe (the smoke test uses a client token)."})
	}

	s.dash.RenderDiag(w, r, checks)
}

// handleConfigSave persists the submitted .env text and hot-reloads the
// config. The flow: write the file atomically (temp + rename) → full
// config.Load("") — the same pipeline used at startup, so every semantic
// validation (durations, URLs, fingerprints, Validate) runs — and swap the
// atomic pointer. Any failure restores the previous .env content. adminSaveMu
// serializes concurrent saves so a rejected save can never clobber a newer
// accepted one.
func (s *Server) handleConfigSave(w http.ResponseWriter, r *http.Request) {
	const envPath = ".env"
	r.Body = http.MaxBytesReader(w, r.Body, maxEnvSize)

	// The dashboard textarea posts application/x-www-form-urlencoded
	// (name="content"); a raw urlencoded body written verbatim as .env would
	// become "content=KEY=VALUE..." and destroy the file. Programmatic
	// clients (text/plain) post the raw .env text and keep the raw path.
	var content []byte
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		if err := r.ParseForm(); err != nil {
			s.dash.RenderConfigResult(w, r, false, "Failed to read request form.")
			return
		}
		content = []byte(r.FormValue("content"))
	} else {
		var err error
		content, err = io.ReadAll(r.Body)
		if err != nil {
			s.dash.RenderConfigResult(w, r, false, "Failed to read request body.")
			return
		}
	}

	// Guard: an empty payload (urlencoded POST without content=, or an empty
	// text/plain body) must never write an empty .env. config.Load succeeds
	// on an empty file with built-in defaults, so the write would silently
	// wipe the operator's AUTH_TOKENS/ADMIN_TOKEN/API_KEYS/SAFE_MODE while
	// reporting a green "Saved and reloaded". Reject it and leave the file
	// untouched.
	if len(bytes.TrimSpace(content)) == 0 {
		s.dash.RenderConfigResult(w, r, false, "Configuration rejected: empty .env content — nothing to save.")
		return
	}

	s.adminSaveMu.Lock()
	defer s.adminSaveMu.Unlock()

	old, oldErr := os.ReadFile(envPath)
	if err := writeFileAtomic(envPath, content); err != nil {
		s.dash.RenderConfigResult(w, r, false, "Failed to write .env: "+err.Error())
		return
	}
	newCfg, err := config.Load(s.configPath)
	if err != nil {
		switch {
		case oldErr == nil:
			_ = writeFileAtomic(envPath, old)
		case errors.Is(oldErr, os.ErrNotExist):
			// The .env did not exist before the save: remove the rejected
			// write so the state matches.
			_ = os.Remove(envPath)
		default:
			// The previous .env existed but was unreadable (permissions, ACL):
			// deleting it would destroy the operator's file. Leave the newly
			// written content and warn — a restore is impossible without the
			// old bytes.
			s.logger.Warn("dashboard config save rejected; previous .env unreadable, not restored", "readErr", oldErr, "err", err)
		}
		s.logger.Warn("dashboard config save rejected", "err", err)
		s.dash.RenderConfigResult(w, r, false, "Configuration rejected: "+err.Error())
		return
	}
	s.cfg.Store(&newCfg)
	s.reg.SetConfig(&newCfg)
	s.pool.SetConfig(&newCfg)
	s.logger.Info("dashboard config saved and reloaded",
		"auth_tokens", len(newCfg.AuthTokens), "safe_mode", newCfg.SafeMode)
	s.dash.RenderConfigResult(w, r, true, "Saved and reloaded — effective configuration updated.")
}

// writeFileAtomic writes data to path via a temp file + rename: readers never
// observe a truncated file, and a crash mid-write leaves the previous content
// intact. os.Rename replaces an existing target atomically on every supported
// platform (Windows uses MoveFileEx with MOVEFILE_REPLACE_EXISTING); only
// filesystems without atomic replace support need the remove-then-rename
// fallback (a tiny non-atomic window, acceptable for an admin action).
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		if _, statErr := os.Stat(path); statErr == nil {
			// The target exists but rename-over-existing failed: fall back
			// to removing it first, then renaming.
			_ = os.Remove(path)
			if err := os.Rename(tmpName, path); err == nil {
				return nil
			} else {
				_ = os.Remove(tmpName)
				return err
			}
		}
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// clientToken returns the request's bearer token (Authorization: Bearer or
// x-api-key), trimmed. Empty when the request carries none. In bridge mode
// this token IS the client's FreeBuff token relayed upstream.
func clientToken(r *http.Request) string {
	provided := ""
	if tok, ok := extractBearerToken(r.Header.Get("Authorization")); ok {
		provided = tok
	} else if h := r.Header.Get("x-api-key"); h != "" {
		provided = h
	}
	return strings.TrimSpace(provided)
}

// bearerToken returns only the Authorization: Bearer token. In hybrid mode
// this is the discriminator between bridge traffic (client relays its own
// FreeBuff token) and pooled traffic (no bearer; x-api-key is the API_KEYS
// scheme and must never be relayed upstream as a FreeBuff credential).
func bearerToken(r *http.Request) string {
	if tok, ok := extractBearerToken(r.Header.Get("Authorization")); ok {
		return tok
	}
	return ""
}

// --- chat ---

// --- chat ---

// relayFunc relays the upstream SSE reader to the client in the endpoint's
// wire format (chat.completion chunks, Responses events, or Anthropic
// events). Implementations set their own headers, flush, and write terminal
// frames. chatStart is when the upstream chat call returned; the first
// relayed chunk records the upstream TTFB phase.
type relayFunc func(ctx context.Context, w http.ResponseWriter, up io.Reader, stats *relayStats, chatStart time.Time)

// handleChat is the OpenAI chat-completions entry point: sanitize the
// request, then route through chatCore with the chat wire format.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			s.writeJSONError(w, http.StatusRequestEntityTooLarge,
				"request body exceeds the 32MB limit", "invalid_request_error", "content_too_large", 0)
		} else {
			s.writeJSONError(w, http.StatusBadRequest,
				"failed to read request body: "+err.Error(), "invalid_request_error", "invalid_json", 0)
		}
		return
	}

	// The raw map decides the response mode (stream) before sanitization.
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		s.writeJSONError(w, http.StatusBadRequest,
			"request body must be a valid JSON object", "invalid_request_error", "invalid_json", 0)
		return
	}
	rawModel, _ := raw["model"].(string)
	if rawModel == "" {
		s.writeJSONError(w, http.StatusBadRequest,
			"missing required field \"model\"; available: "+strings.Join(s.reg.Models(), ", "),
			"invalid_request_error", "model_not_found", 0)
		return
	}
	model := s.reg.ResolveModel(rawModel)
	stream := false
	if v, ok := raw["stream"].(bool); ok {
		stream = v
	}
	normalized, err := convert.NormalizeRequest(body, model)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest,
			"request body must be a valid JSON object: "+err.Error(), "invalid_request_error", "invalid_json", 0)
		return
	}
	var relay relayFunc
	if stream {
		relay = s.relayStream
	} else {
		relay = s.relayJSON
	}
	s.chatCore(w, r, model, stream, normalized, convert.ExtractReasoningEffort(raw), "chat", relay)
}

// chatCore is the shared acquire→relay core for every completion-style
// endpoint (chat completions, Responses, Anthropic messages): acquire a
// token lease (bridge/hybrid routing included), call upstream with
// retry-once recovery, then relay the forced stream to the client through
// relay. kind names the endpoint in request/done log lines.
func (s *Server) chatCore(w http.ResponseWriter, r *http.Request, model string, stream bool, normalized []byte, reasoningEffort, kind string, relay relayFunc) {
	ctx, phases := phasetiming.WithContext(r.Context())
	start := time.Now()

	agentID, _ := s.reg.AgentForModel(model)
	reqAttrs := []any{
		"model", model,
		"agent", agentID,
		"stream", stream,
		"remote", remoteHost(r),
	}
	if reasoningEffort != "" {
		reqAttrs = append(reqAttrs, "reasoning_effort", reasoningEffort)
	}
	s.logger.Info(kind+" request", reqAttrs...)
	// Bridge routing: pure bridge (no AUTH_TOKENS, not hybrid) always relays
	// the client's Authorization header as the upstream token; hybrid mode
	// relays when a token is present and falls back to the pool otherwise.
	// No token in pure bridge → 401 before touching the pool.
	var up io.ReadCloser
	var lease *pool.Lease
	cfg := s.cfg.Load()
	fallbackUsed := false
	// In hybrid, only an Authorization: Bearer token selects the bridge
	// path — an x-api-key is the API_KEYS scheme for pooled clients and
	// must never be relayed upstream as a FreeBuff credential.
	tok := bearerToken(r)
	bridge := false
	switch {
	case cfg.BridgeMode() && !cfg.HybridMode:
		// Pure bridge: the client token is the only upstream credential.
		bridge = true
		tok = clientToken(r)
	case cfg.HybridMode:
		// Hybrid: a Bearer token is relayed like bridge; token-less
		// requests fall back to the pool.
		bridge = tok != ""
	}
	// Issue #74 P2: refuse new requests fast when (egress, model) is marked
	// unfit — the direct egress cannot serve this model for ~5 min. The
	// pooled path only: bridge clients relay their own token (the client's
	// own account may serve the model on this egress and their session
	// slots are theirs to spend), so the registry never gates them.
	// MarkModelUnfit always stores a LimitedIpError, so lie is non-nil in
	// practice; the bare sentinel keeps the refusal deterministic if it
	// ever is nil.
	if !bridge {
		if until, lie := s.pool.ModelUnfit(model); !until.IsZero() && time.Now().Before(until) {
			if lie != nil {
				lie.RetryAfter = time.Until(until)
			}
			phases.Since(phasetiming.TotalMS, start)
			s.logger.Info(kind+" request refused", "model", model, "reason", "model_limited_on_egress", "until", until.Format(time.RFC3339))
			var refuseErr = upstream.ErrModelIPLimited
			if lie != nil {
				refuseErr = lie
			}
			s.traceChat(nil, model, time.Since(start).Milliseconds(), "error", "model_ip_limited", phases.All())
			s.writeError(w, r, refuseErr)
			return
		}
	}
	// Acquire is timed per call; on the retry-once path the last acquire
	// wins (that is the lease-producing one, matching the pool's
	// per-attempt session/run phases).
	acquireTimed := func(acquire func(context.Context, string) (*pool.Lease, error)) func(context.Context, string) (*pool.Lease, error) {
		return func(ctx context.Context, model string) (*pool.Lease, error) {
			acquireStart := time.Now()
			l, err := acquire(ctx, model)
			phases.Since(phasetiming.AcquireMS, acquireStart)
			return l, err
		}
	}
	var err error
	if bridge {
		if tok == "" {
			s.writeJSONError(w, http.StatusUnauthorized,
				"bridge mode: send your FreeBuff token as Authorization: Bearer <token> (no AUTH_TOKENS configured on the proxy)",
				"invalid_request_error", "missing_bearer_token", 0)
			return
		}
		up, lease, err = s.chatAttempt(ctx, model, normalized,
			acquireTimed(func(ctx context.Context, model string) (*pool.Lease, error) {
				return s.pool.AcquireBridge(ctx, tok, model)
			}),
			s.pool.Chat,
			s.pool.InvalidateBridgeSession,
			s.pool.InvalidateBridgeRun,
			func(l *pool.Lease) { s.pool.CooldownBridge(l, runs.DefaultCooldown) },
			s.pool.CooldownBridgeBan,
			s.pool.CooldownBridgeRateLimit,
			s.pool.CooldownBridgeIpCapped,
			s.pool.CooldownBridgeCountryBlocked,
		)
	} else {
		// Issue #100: bounded queue-time model fallback. When the request's
		// model has a configured fallback (FALLBACK_MODEL) and the pool
		// surfaces a waiting-room/queue delay of at least FALLBACK_AFTER_MS,
		// re-route the SAME token to the fallback model instead of handing
		// the client a 503 the client would have to wait out. Conservative:
		// only when a fallback is configured; the switch is surfaced to the
		// client via the X-FreeBuff-Fallback-Model response header and in
		// the routing log line.
		acquire := acquireTimed(func(ctx context.Context, model string) (*pool.Lease, error) { return s.pool.Acquire(ctx, model) })
		fallbackModel := cfg.FallbackModels[model]
		if cfg.FallbackAfter > 0 && fallbackModel != "" && fallbackModel != model {
			wrapped := acquire
			acquire = func(ctx context.Context, m string) (*pool.Lease, error) {
				l, err := wrapped(ctx, m)
				if err == nil || errors.Is(err, registry.ErrModelNotFound) {
					return l, err
				}
				var wr *session.WaitingRoomError
				if errors.As(err, &wr) && wr.RetryAfter >= cfg.FallbackAfter {
					s.logger.Info("model fallback: waiting room exceeds FALLBACK_AFTER_MS; switching model",
						"model", m, "fallback", fallbackModel, "retry_after", wr.RetryAfter.String())
					// Drop the queued session caches so the fallback-model
					// acquire can CREATE a fresh session instead of
					// re-surfacing the same waiting room (issue #100).
					if cleared := s.pool.ClearQueuedCaches(); cleared > 0 {
						s.logger.Debug("model fallback: cleared queued session caches", "cleared", cleared)
					}
					l2, err2 := wrapped(ctx, fallbackModel)
					if err2 == nil {
						fallbackUsed = true
					}
					return l2, err2
				}
				return l, err
			}
		}
		up, lease, err = s.chatAttempt(ctx, model, normalized,
			acquire,
			s.pool.Chat,
			func(l *pool.Lease) { s.pool.InvalidateSession(l.Token) },
			func(l *pool.Lease, agentID string) { s.pool.InvalidateRun(l.Token, agentID) },
			func(l *pool.Lease) { s.pool.CooldownToken(l.Token, runs.DefaultCooldown) },
			func(l *pool.Lease, be *upstream.BanError) { s.pool.CooldownTokenBan(l.Token, be) },
			func(l *pool.Lease, rle *upstream.RateLimitError) { s.pool.CooldownTokenRateLimit(l.Token, rle) },
			func(l *pool.Lease, ice *upstream.IpCappedError) { s.pool.CooldownTokenIpCapped(l.Token, ice) },
			func(l *pool.Lease, cbe *upstream.CountryBlockedError) {
				s.pool.CooldownTokenCountryBlocked(l.Token, cbe)
			},
		)
	}
	if err != nil {
		phases.Since(phasetiming.TotalMS, start)
		s.traceChat(lease, model, time.Since(start).Milliseconds(), "error", chatErrClass(err), phases.All())
		// Issue #114: a chat that died on a terminal upstream error must
		// not leave its run FINISHing as completed — report it honestly
		// (nil-safe: an acquire failure leaves no lease).
		s.pool.MarkRunFailed(lease)
		s.writeError(w, r, err)
		return
	}
	defer func() { _ = up.Close() }()
	// Issue #53: when the downstream client disconnects mid-stream, abandon
	// the lease instead of a plain release — the run is FINISHed through the
	// bounded queue (last-in-flight only) so upstream does not keep an
	// abandoned agent run alive until the 6h rotation. A normal completion
	// releases the lease as before.
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if r.Context().Err() != nil {
			s.pool.LeaseAbandon(lease)
			return
		}
		s.pool.LeaseRelease(lease)
	}
	defer release()

	routingAttrs := []any{
		"token", tokenLabel(lease),
		"model", model,
		"agent", lease.AgentID,
		"instance_id", lease.SessionInstanceID,
		"tier", lease.TierAccess,
		"country", lease.TierCountry,
	}
	if reasoningEffort != "" {
		routingAttrs = append(routingAttrs, "reasoning_effort", reasoningEffort)
	}
	if fallbackUsed {
		// Surface the transparent model switch to the client (issue #100):
		// the streamed response itself is indistinguishable, so the header
		// is the notice.
		w.Header().Set("X-FreeBuff-Fallback-Model", cfg.FallbackModels[model])
		routingAttrs = append(routingAttrs, "fallback", cfg.FallbackModels[model])
	}
	s.logger.Info(kind+" routing", routingAttrs...)

	chatStart := time.Now()
	stats := &relayStats{}
	relay(ctx, w, up, stats, chatStart)
	// Issue #114: record the completed chat as a run step — steps are
	// batched in memory and sent WITH FINISH (the CLI has no /steps
	// endpoint). The response message id is not extracted from the stream;
	// the CLI step schema allows a null messageId.
	s.pool.RecordRunStep(lease, "")
	phases.Since(phasetiming.TotalMS, start)
	ms := time.Since(start).Milliseconds()
	s.logger.Info(kind+" done", chatDoneAttrs(model, lease.AgentID, stream, ms, stats.chunks, stats.bytes, reasoningEffort)...)
	s.traceChat(lease, model, ms, "ok", "", phases.All())
}

// traceChat records a structured "chat trace" entry for the dashboard
// traces page (the page filters the shared log ring by msg == "chat trace").
// phases carries the per-request latency phases (#89); the map is ordered
// deterministically for stable log output.
func (s *Server) traceChat(lease *pool.Lease, model string, ms int64, status, errClass string, phases map[string]int64) {
	attrs := []any{"model", model, "status", status, "ms", ms}
	if lease != nil {
		attrs = append(attrs, "token", tokenLabel(lease), "agent", lease.AgentID)
	}
	if errClass != "" {
		attrs = append(attrs, "error", errClass)
	}
	for _, name := range []string{
		phasetiming.AcquireMS,
		phasetiming.SessionRefreshMS,
		phasetiming.RunAcquireMS,
		phasetiming.UpstreamTTFBMS,
		phasetiming.TotalMS,
	} {
		if v, ok := phases[name]; ok {
			attrs = append(attrs, name, v)
		}
	}
	s.logger.Info("chat trace", attrs...)
}

// chatErrClass buckets an upstream error into the trace error column.
func chatErrClass(err error) string {
	switch err.(type) {
	case *upstream.RateLimitError:
		return "rate_limited"
	case *upstream.BanError:
		return "banned"
	case *upstream.IpCappedError:
		return "ip_capped"
	case *upstream.LimitedIpError:
		return "model_ip_limited"
	case *upstream.SessionLimitError:
		return "session_limit_reached"
	case *upstream.WaitingRoomError, *session.WaitingRoomError:
		return "waiting_room"
	case *upstream.UpstreamError:
		return "upstream"
	default:
		return "error"
	}
}

// chatDoneAttrs builds the structured log attributes for a completed chat,
// including reasoning effort when the client requested it.
func chatDoneAttrs(model, agent string, stream bool, ms int64, chunks, bytes int, reasoningEffort string) []any {
	attrs := []any{
		"model", model,
		"agent", agent,
		"stream", stream,
		"ms", ms,
		"bytes", bytes,
	}
	if stream {
		attrs = append(attrs, "chunks", chunks)
	}
	if reasoningEffort != "" {
		attrs = append(attrs, "reasoning_effort", reasoningEffort)
	}
	return attrs
}

// chatAttempt runs the retry-once recovery loop for one chat request: chat
// through the leased token; on session-invalid / run-invalid the lease is
// released, the cached session/run invalidated, and a fresh lease acquired
// once; on auth-reject / ban / rate-limit / ip-capped the token is cooled
// down (ip_capped bounded to its retryAfterMs — never the Pacific-midnight
// lock) and the error returned for writeError. The acquire/chat/invalidate/
// cooldown hooks are closures so the pooled (fixed-token) and bridge paths
// share the exact same recovery semantics. On success the returned body
// reader and final lease belong to the caller: close the body and release
// the lease via Pool.LeaseRelease when done.
func (s *Server) chatAttempt(
	ctx context.Context,
	model string,
	normalized []byte,
	acquire func(context.Context, string) (*pool.Lease, error),
	chat func(context.Context, *pool.Lease, upstream.ChatOptions, []byte) (io.ReadCloser, error),
	invalidateSession func(*pool.Lease),
	invalidateRun func(*pool.Lease, string),
	cooldownAuth func(*pool.Lease),
	cooldownBan func(*pool.Lease, *upstream.BanError),
	cooldownRate func(*pool.Lease, *upstream.RateLimitError),
	cooldownIpCapped func(*pool.Lease, *upstream.IpCappedError),
	cooldownCountry func(*pool.Lease, *upstream.CountryBlockedError),
) (io.ReadCloser, *pool.Lease, error) {
	lease, err := acquire(ctx, model)
	if err != nil {
		return nil, nil, err
	}

	// The lease is the authoritative source for the model its session/run
	// are bound to: after a #100 fallback the acquire returned a lease for
	// the FALLBACK model while the caller still holds the requested model.
	// opts.Model, the body model and x-freebuff-model must all agree with
	// the lease (review P2 — previously the request went upstream labeled
	// with the requested model against the fallback session/run).
	effectiveModel := lease.Model
	if effectiveModel == "" {
		effectiveModel = model
	}
	if effectiveModel != model {
		if renormalized, nerr := convert.NormalizeRequest(normalized, effectiveModel); nerr == nil {
			normalized = renormalized
		}
	}

	opts := upstream.ChatOptions{
		Model:             effectiveModel,
		RunID:             lease.Run.RunID,
		SessionInstanceID: lease.SessionInstanceID,
		TraceSessionID:    lease.Run.TraceSessionID,
		// Issue #113: stamp the run's 1-based per-chat step counter so
		// codebuff_metadata["llm_step_number"] matches the CLI (each chat
		// call is one agent step; run-agent-step.ts increments per step).
		// Incremented once per chatAttempt — the retry-once loop below
		// retries the SAME step.
		StepNumber: int(lease.Run.NextStepNumber()),
	}

	released := false
	release := func() {
		if !released {
			released = true
			s.pool.LeaseRelease(lease)
		}
	}
	defer release()

	var up io.ReadCloser
	attempts := 0
	for {
		up, err = chat(ctx, lease, opts, normalized)
		if err == nil {
			// Issue #74 P2: a successful chat is egress-level proof the
			// model is servable again — drop any (egress, model) unfit mark.
			s.pool.ClearModelUnfit(effectiveModel)
			released = true // Disarm deferred release: ownership transferred to caller
			return up, lease, nil
		}
		attempts++
		switch {
		case errors.Is(err, upstream.ErrModelIPLimited):
			// Issue #74 P2: the egress IP is limited for the requested
			// model. Mark (egress, model) unfit for ~5 min so new requests
			// refuse fast instead of re-admitting against a known-limited
			// gate (each admission burns a daily session slot). Retry once
			// through a fresh acquire — a different token (full-tier
			// account) may still serve the model. The session is bound to
			// its admitted model and is NOT invalidated.
			var lie *upstream.LimitedIpError
			if errors.As(err, &lie) {
				s.pool.MarkModelUnfit(effectiveModel, lie)
			} else {
				s.pool.MarkModelUnfit(effectiveModel, nil)
			}
			release()
			if attempts > 1 {
				return nil, nil, err
			}
		case errors.Is(err, upstream.ErrSessionInvalid):
			release()
			invalidateSession(lease)
			if attempts > 1 {
				return nil, nil, err
			}
		case errors.Is(err, upstream.ErrRunInvalid):
			release()
			invalidateRun(lease, lease.AgentID)
			if attempts > 1 {
				return nil, nil, err
			}
		case errors.Is(err, upstream.ErrAuthRejected):
			cooldownAuth(lease)
			release()
			return nil, nil, err
		case errors.Is(err, upstream.ErrBanned):
			var be *upstream.BanError
			if errors.As(err, &be) {
				cooldownBan(lease, be)
			}
			release()
			return nil, nil, err
		case errors.Is(err, upstream.ErrRateLimited):
			var rle *upstream.RateLimitError
			if errors.As(err, &rle) {
				cooldownRate(lease, rle)
			}
			release()
			return nil, nil, err
		case errors.Is(err, upstream.ErrIpCapped):
			// ip_capped is admission-only (too many distinct users on the
			// egress IP), NOT a quota reset: cool the token only until the
			// body's retryAfterMs — never the Pacific-midnight lock — and
			// never invalidate the session (existing sessions keep running).
			var ice *upstream.IpCappedError
			if errors.As(err, &ice) {
				cooldownIpCapped(lease, ice)
			}
			release()
			return nil, nil, err
		case errors.Is(err, upstream.ErrCountryBlocked):
			// A chat-path country block cools the token like the admission
			// path does: without it the cached session stays "active" and
			// every request re-hits upstream run-start inside the window.
			var cbe *upstream.CountryBlockedError
			if errors.As(err, &cbe) {
				cooldownCountry(lease, cbe)
			}
			release()
			return nil, nil, err
		default:
			release()
			// Retryable UpstreamErrors (e.g. deployment_outside_hours) are
			// temporarily unavailable, not transient: a blind retry burns a
			// fresh lease against the same wall. Surface them for writeError
			// (503 upstream_retryable) instead.
			var ue *upstream.UpstreamError
			if errors.As(err, &ue) && ue.Retryable {
				return nil, nil, err
			}
			if attempts > 1 {
				return nil, nil, err
			}
			s.logger.Debug("transient chat error, retrying once", "err", err)
		}
		lease, err = acquire(ctx, effectiveModel)
		if err != nil {
			return nil, nil, err
		}
		released = false
		// A fresh lease may bind a different model (fallback path): refresh
		// the effective model + body so opts.Model, the body and the
		// lease's session/run stay consistent.
		effectiveModel = lease.Model
		if effectiveModel == "" {
			effectiveModel = model
		}
		if effectiveModel != model {
			if renormalized, nerr := convert.NormalizeRequest(normalized, effectiveModel); nerr == nil {
				normalized = renormalized
			}
		}
		opts.Model = effectiveModel
		if lease.Run.RunID != opts.RunID {
			// The retry landed on a FRESH run (run-invalid path): the new
			// run's step counter starts at 1 — stamp its number so
			// llm_step_number stays per-run like the CLI.
			opts.StepNumber = int(lease.Run.NextStepNumber())
		}
		opts.RunID = lease.Run.RunID
		opts.SessionInstanceID = lease.SessionInstanceID
	}
}

// tokenLabel renders the lease's token for logging: "bridge" for bridge
// leases, the 1-based fixed-token index otherwise.
func tokenLabel(lease *pool.Lease) string {
	if lease == nil || lease.Bridge != nil {
		return "bridge"
	}
	return fmt.Sprintf("%d", lease.Token+1)
}

// relayStats accumulates per-response relay counters for logging.
type relayStats struct {
	chunks int
	bytes  int
}

// keepaliveInterval is how long the relay may sit without relaying a data
// chunk before it emits an SSE comment frame to hold the connection open.
// Long upstream reasoning pauses produce no chunks, and proxies/clients may
// treat silence as a dead connection. A var (not const) so tests can shrink
// it.
var keepaliveInterval = 15 * time.Second

// lineChunk is one upstream SSE line or the terminal send. done is set only
// on the clean-EOF send (a real empty line also arrives as line==nil, so the
// terminal state must be explicit, not inferred from a nil slice).
type lineChunk struct {
	line []byte
	err  error
	done bool
}

// relayReadLoop drains r line by line onto ch, stopping when the stream
// ends or ctx is canceled. The final send carries done (clean EOF) or the
// terminal read error; on cancellation the goroutine exits without sending
// (the request context cancellation closes the upstream body read, so Scan
// returns promptly).
func relayReadLoop(ctx context.Context, r io.Reader, ch chan<- lineChunk) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxStreamLine)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		select {
		case ch <- lineChunk{line: line}:
		case <-ctx.Done():
			return
		}
	}
	var terminal lineChunk
	if err := scanner.Err(); err != nil {
		terminal = lineChunk{err: err}
	} else {
		terminal = lineChunk{done: true}
	}
	select {
	case ch <- terminal:
	case <-ctx.Done():
	}
}

// relayStream forwards sanitized upstream SSE lines to the client with
// per-chunk flushing, a ": connecting" grace-flush comment, a keepalive
// comment every keepaliveInterval of relay silence, a [DONE] terminator,
// and an error chunk (then DONE) when the upstream stream dies while the
// client context is still live.
func (s *Server) relayStream(ctx context.Context, w http.ResponseWriter, r io.Reader, stats *relayStats, chatStart time.Time) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.logger.Warn("response writer does not support flushing")
		return
	}
	w.WriteHeader(http.StatusOK)

	// The official CLI client treats a ": connecting" comment as the signal
	// that headers have flushed and the stream is live (grace flush): write
	// it before relaying anything so a client-side timeout can never fire
	// during a long upstream admission pause. Comment frames are ignored by
	// SSE parsers.
	_, _ = io.WriteString(w, ": connecting\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()

	lines := make(chan lineChunk)
	go relayReadLoop(ctx, r, lines)

	relayed := time.Now()
	first := true
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if time.Since(relayed) >= keepaliveInterval {
				_, _ = io.WriteString(w, ": keepalive\n\n")
				relayed = time.Now()
				flusher.Flush()
			}
		case lc := <-lines:
			if lc.err != nil {
				if ctx.Err() == nil {
					s.logger.Warn("upstream stream error", "err", lc.err)
					_, _ = w.Write(convert.ErrorChunk("upstream stream interrupted: "+lc.err.Error(), "upstream_stream_error"))
					_, _ = w.Write(convert.DONE)
					flusher.Flush()
				}
				return
			}
			if lc.done {
				// Clean end of stream (EOF is not a scanner error).
				_, _ = w.Write(convert.DONE)
				flusher.Flush()
				return
			}
			clean, drop := convert.SanitizeChunk(lc.line)
			if drop {
				// Non-chunk lines (upstream comments) still prove liveness.
				relayed = time.Now()
				continue
			}
			if first {
				first = false
				phasetiming.FromContext(ctx).Since(phasetiming.UpstreamTTFBMS, chatStart)
			}
			frame := convert.EncodeSSE(clean)
			if _, err := w.Write(frame); err != nil {
				s.logger.Debug("stream write failed", "err", err)
				return
			}
			stats.chunks++
			stats.bytes += len(frame)
			relayed = time.Now()
			flusher.Flush()
		}
	}
}

// relayJSON drains the upstream SSE stream through the accumulator and
// writes one chat.completion JSON response. On any decode or stream error
// nothing is written and a 502 is returned (the client asked for a single
// response; a partial one would be worse than none).
func (s *Server) relayJSON(ctx context.Context, w http.ResponseWriter, r io.Reader, stats *relayStats, chatStart time.Time) {
	acc := convert.NewAccumulator()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxStreamLine)
	first := true
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		if first {
			first = false
			phasetiming.FromContext(ctx).Since(phasetiming.UpstreamTTFBMS, chatStart)
		}
		if err := acc.Add(scanner.Bytes()); err != nil {
			s.writeJSONError(w, http.StatusBadGateway,
				"failed to decode upstream stream: "+err.Error(), "upstream_error", "upstream_unavailable", 0)
			return
		}
		stats.chunks++
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() == nil {
			s.writeJSONError(w, http.StatusBadGateway,
				"upstream stream error: "+err.Error(), "upstream_error", "upstream_unavailable", 0)
		}
		return
	}
	out := acc.Finish()
	stats.bytes = len(out)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// --- models / healthz ---

// handleModels serves the OpenAI model-list shape with the registry's
// current models; created is pinned to server start so every entry matches.
// Each row carries an advisory availability annotation derived from the pool
// token snapshots (available/status/current_access_tier) so clients can
// surface quota or lock signals without probing.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	created := s.started.Unix()
	snaps := s.pool.Snapshot()
	models := s.reg.Models()
	hideUnavailable := s.cfg.Load().ModelsHideUnavailable
	data := make([]map[string]any, 0, len(models))
	for _, id := range models {
		available, status, tier := modelAvailability(id, snaps)
		if hideUnavailable && !available {
			// MODELS_HIDE_UNAVAILABLE=true: prune region/tier/quota-locked
			// models so picker clients never auto-select one. Off by default
			// because a stale signal could hide a working model.
			continue
		}
		data = append(data, map[string]any{
			"id":                  id,
			"object":              "model",
			"created":             created,
			"owned_by":            "freebuff",
			"available":           available,
			"status":              status,
			"current_access_tier": tier,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// modelAvailability derives the advisory per-model annotation from the pool
// token snapshots. The snapshot does not carry the model of a live session,
// so the signal set is: quotaByModel presence (the session admitted this
// model), quota exhaustion (recent >= limit), session-level locks, and the
// access tier. A token demoted to the 'limited' tier (region/privacy
// demotion) can only use LimitedTierModels — every other model is marked
// unavailable with status "region_limited" (kept in the list, never hidden,
// so a stale tier can't strand a working model). available defaults to true
// when no signal exists, so a working model is never hidden.
func modelAvailability(id string, snaps []pool.TokenSnapshot) (available bool, status, tier string) {
	available = true
	status = "unknown"
	quotaHit := false
	quotaExhausted := false
	locked := false
	for _, snap := range snaps {
		if tier == "" {
			tier = snap.TierAccess
		}
		switch snap.SessionStatus {
		case "model_locked", "disabled":
			locked = true
		}
		if q, ok := snap.QuotaByModel[id]; ok {
			quotaHit = true
			if q.Limit > 0 && q.RecentCount >= q.Limit {
				quotaExhausted = true
			}
		}
	}
	switch {
	case quotaExhausted:
		status = "quota_exhausted"
	case locked:
		status = "locked"
	case quotaHit:
		status = "available"
	}
	if status == "unknown" && tier == "limited" && !registry.LimitedTierModels[id] {
		// Region/privacy demotion: the model is not on the limited tier's
		// allowlist and the session never admitted it. Keep it listed but
		// honest — clients that auto-pick on the available flag skip it,
		// and a stale tier can never hide a model the session admitted
		// (admission is ground truth, handled above).
		return false, "region_limited", tier
	}
	return available, status, tier
}

// handleHealthz reports uptime, model count, the per-token snapshot, the
// cached bridge entries (bridge mode), and the effective routing mode.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	snaps := s.pool.Snapshot()
	cfg := s.cfg.Load()
	w.Header().Set("Content-Type", "application/json")
	tokens := make([]map[string]any, 0, len(snaps))
	for _, snap := range snaps {
		tok := map[string]any{
			"Token":                snap.Token,
			"CooldownUntil":        snap.CooldownUntil,
			"SessionStatus":        snap.SessionStatus,
			"SessionInstanceID":    snap.SessionInstanceID,
			"SessionQueuePosition": snap.SessionQueuePosition,
			"SessionQueueDepth":    snap.SessionQueueDepth,
			"ActiveRuns":           snap.ActiveRuns,
			"Requests":             snap.Requests,
			"Messages24h":          snap.Messages24h,
			"DailyLimit":           snap.DailyLimit,
			"UsagePct":             snap.UsagePct,
			"RiskLevel":            snap.RiskLevel,
			"tier":                 snap.TierAccess,
			"country":              snap.CountryCode,
		}
		if len(snap.QuotaByModel) > 0 {
			quota := make(map[string]any, len(snap.QuotaByModel))
			for model, q := range snap.QuotaByModel {
				entry := map[string]any{
					"limit":        q.Limit,
					"recent_count": q.RecentCount,
					"period":       q.Period,
				}
				if !q.ResetAt.IsZero() {
					entry["reset_at"] = q.ResetAt
				}
				if len(q.Entitlement) > 0 {
					entry["entitlement"] = q.Entitlement
				}
				quota[model] = entry
			}
			tok["quota"] = quota
		}
		if len(snap.Entitlement) > 0 {
			tok["entitlement"] = snap.Entitlement
		}
		tokens = append(tokens, tok)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":         "ok",
		"mode":           cfg.EffectiveMode(),
		"uptime_seconds": time.Since(s.started).Seconds(),
		"models":         s.reg.ModelCount(),
		"tokens":         tokens,
		"bridge_tokens":  s.pool.BridgeCount(),
	})
}

// escapeLabelValue escapes a Prometheus label value per the text exposition
// format: backslash, double quote, and newline are escaped; everything else
// passes through unchanged.
func escapeLabelValue(v string) string {
	if !strings.ContainsAny(v, `\"\n`) {
		return v
	}
	var sb strings.Builder
	for _, r := range v {
		switch r {
		case '\\':
			sb.WriteString(`\\`)
		case '"':
			sb.WriteString(`\"`)
		case '\n':
			sb.WriteString(`\n`)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// handleMetrics exports Prometheus metrics (#24).
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var sb strings.Builder
	uptime := time.Since(s.started).Seconds()
	ps := s.pool.PoolSnapshot()
	snaps := ps.Tokens

	sb.WriteString("# HELP freebuff_proxy_uptime_seconds Process uptime in seconds\n")
	sb.WriteString("# TYPE freebuff_proxy_uptime_seconds gauge\n")
	fmt.Fprintf(&sb, "freebuff_proxy_uptime_seconds %.2f\n\n", uptime)

	sb.WriteString("# HELP freebuff_proxy_models_total Count of models available in registry\n")
	sb.WriteString("# TYPE freebuff_proxy_models_total gauge\n")
	fmt.Fprintf(&sb, "freebuff_proxy_models_total %d\n\n", s.reg.ModelCount())

	sb.WriteString("# HELP freebuff_proxy_tokens_total Count of configured tokens in pool\n")
	sb.WriteString("# TYPE freebuff_proxy_tokens_total gauge\n")
	fmt.Fprintf(&sb, "freebuff_proxy_tokens_total %d\n\n", len(snaps))

	sb.WriteString("# HELP freebuff_proxy_token_messages_24h Rolling 24h message count per token\n")
	sb.WriteString("# TYPE freebuff_proxy_token_messages_24h gauge\n")
	for _, snap := range snaps {
		fmt.Fprintf(&sb, "freebuff_proxy_token_messages_24h{token=\"%d\"} %d\n", snap.Token+1, snap.Messages24h)
	}
	sb.WriteString("\n")

	sb.WriteString("# HELP freebuff_proxy_token_requests_total Total requests served per token\n")
	sb.WriteString("# TYPE freebuff_proxy_token_requests_total counter\n")
	for _, snap := range snaps {
		fmt.Fprintf(&sb, "freebuff_proxy_token_requests_total{token=\"%d\"} %d\n", snap.Token+1, snap.Requests)
	}
	sb.WriteString("\n")

	sb.WriteString("# HELP freebuff_proxy_token_active_runs Active agent runs per token\n")
	sb.WriteString("# TYPE freebuff_proxy_token_active_runs gauge\n")
	for _, snap := range snaps {
		fmt.Fprintf(&sb, "freebuff_proxy_token_active_runs{token=\"%d\"} %d\n", snap.Token+1, snap.ActiveRuns)
	}
	sb.WriteString("\n")

	sb.WriteString("# HELP freebuff_proxy_token_cooldown_active Is token currently cooling down (1=yes, 0=no)\n")
	sb.WriteString("# TYPE freebuff_proxy_token_cooldown_active gauge\n")
	now := time.Now()
	for _, snap := range snaps {
		cd := 0
		if !snap.CooldownUntil.IsZero() && now.Before(snap.CooldownUntil) {
			cd = 1
		}
		fmt.Fprintf(&sb, "freebuff_proxy_token_cooldown_active{token=\"%d\"} %d\n", snap.Token+1, cd)
	}
	sb.WriteString("\n")

	sb.WriteString("# HELP freebuff_proxy_quota_recent Current usage toward the per-model quota window\n")
	sb.WriteString("# TYPE freebuff_proxy_quota_recent gauge\n")
	for _, snap := range snaps {
		for model, q := range snap.QuotaByModel {
			fmt.Fprintf(&sb, "freebuff_proxy_quota_recent{token=\"%d\",model=\"%s\",period=\"%s\"} %g\n",
				snap.Token+1, escapeLabelValue(model), escapeLabelValue(q.Period), q.RecentCount)
		}
	}
	sb.WriteString("\n")

	sb.WriteString("# HELP freebuff_proxy_quota_limit Per-model quota limit for the window\n")
	sb.WriteString("# TYPE freebuff_proxy_quota_limit gauge\n")
	for _, snap := range snaps {
		for model, q := range snap.QuotaByModel {
			fmt.Fprintf(&sb, "freebuff_proxy_quota_limit{token=\"%d\",model=\"%s\",period=\"%s\"} %g\n",
				snap.Token+1, escapeLabelValue(model), escapeLabelValue(q.Period), q.Limit)
		}
	}
	sb.WriteString("\n")

	sb.WriteString("# HELP freebuff_proxy_transient_retries_total Transient transport failures retried per token\n")
	sb.WriteString("# TYPE freebuff_proxy_transient_retries_total counter\n")
	for _, snap := range snaps {
		if snap.TransientRetries > 0 {
			fmt.Fprintf(&sb, "freebuff_proxy_transient_retries_total{token=\"%d\"} %d\n", snap.Token+1, snap.TransientRetries)
		}
	}
	sb.WriteString("\n")

	sb.WriteString("# HELP freebuff_proxy_fingerprint_rotations_total TLS fingerprint rotations per token\n")
	sb.WriteString("# TYPE freebuff_proxy_fingerprint_rotations_total counter\n")
	for _, snap := range snaps {
		if snap.FingerprintRotations > 0 {
			fmt.Fprintf(&sb, "freebuff_proxy_fingerprint_rotations_total{token=\"%d\"} %d\n", snap.Token+1, snap.FingerprintRotations)
		}
	}
	sb.WriteString("\n")

	_, _ = w.Write([]byte(sb.String()))
}

// handleReload handles POST /admin/reload for hot configuration reloads (#26).
func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	s.logger.Info("admin reload requested")
	newCfg, err := config.Load(s.configPath)
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to reload config: "+err.Error(), "internal_error", "reload_failed", 0)
		return
	}
	s.cfg.Store(&newCfg)
	s.reg.SetConfig(&newCfg)
	s.pool.SetConfig(&newCfg)
	s.logger.Info("config reloaded successfully", "auth_tokens", len(newCfg.AuthTokens), "safe_mode", newCfg.SafeMode)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":      "ok",
		"message":     "configuration reloaded",
		"auth_tokens": len(newCfg.AuthTokens),
		"safe_mode":   newCfg.SafeMode,
	})
}

// --- error mapping ---

// openAIError is the OpenAI error body with an optional human-readable hint (#19).
type openAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Hint    string `json:"hint,omitempty"`
}

// writeJSONError writes an OpenAI-shaped error response. Retry-After is set
// (in ceil seconds) only when retryAfter > 0.
func (s *Server) writeJSONError(w http.ResponseWriter, status int, message, typ, code string, retryAfter time.Duration) {
	s.writeJSONErrorWithHint(w, status, message, typ, code, "", retryAfter)
}

func (s *Server) writeJSONErrorWithHint(w http.ResponseWriter, status int, message, typ, code, hint string, retryAfter time.Duration) {
	if hint == "" {
		hint = defaultHintForCode(code, message)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if retryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": openAIError{Message: message, Type: typ, Code: code, Hint: hint},
	})
}

func defaultHintForCode(code, message string) string {
	lowerMsg := strings.ToLower(message)
	switch {
	case code == "free_mode_cli_required" || strings.Contains(lowerMsg, "free_mode_cli_required"):
		return "Upstream free tier gate requires official CLI traffic envelope. See FAQ: https://github.com/trefeon/freebuff-proxy#faq"
	case code == "account_banned" || strings.Contains(lowerMsg, "banned"):
		return "Account suspended upstream. Token is dead; create a fresh account with an established GitHub login."
	case code == "country_blocked" || strings.Contains(lowerMsg, "country blocked") || strings.Contains(lowerMsg, "country_blocked"):
		return "Your egress IP is in an unsupported region. Route traffic through an allowed country (e.g. US/EU/ID/SG)."
	case code == "out_of_credits" || strings.Contains(lowerMsg, "out of credits"):
		return "Upstream free-tier credits exhausted. Check COST_MODE=free in .env — a typo routes requests as PAID and fresh free accounts get 402."
	case code == "upstream_timeout":
		return "The upstream request exceeded its deadline. Retry, or raise REQUEST_TIMEOUT/SESSION_CALL_TIMEOUT in .env."
	case code == "upstream_auth_rejected" || code == "invalid_api_key" || strings.Contains(lowerMsg, "invalid api key"):
		return "Token invalid or expired. Get a fresh token by running freebuff or scripts/get-freebuff-token.sh"
	case code == "rate_limited":
		return "Daily message cap or rate limit reached. Wait for quota reset or add another token."
	case code == "missing_bearer_token":
		return "Bridge mode active: pass your FreeBuff token in Authorization: Bearer <token>"
	case code == "model_not_found":
		return "Check available models via GET /v1/models"
	default:
		return ""
	}
}

// writeError maps any error from the pool/upstream to the PRD §6 matrix and
// logs it once. Canceled client contexts are logged at debug and dropped (no
// response written).
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		s.logger.Debug("request canceled by client", "err", err)
		return
	}
	if r != nil && r.Context().Err() != nil {
		s.logger.Debug("client context canceled; not writing error", "err", err)
		return
	}

	status := http.StatusBadGateway
	code := "upstream_unavailable"
	message := err.Error()
	var retryAfter time.Duration

	var wr *session.WaitingRoomError
	var uwr *upstream.WaitingRoomError
	var ue *upstream.UpstreamError
	var rle *upstream.RateLimitError
	var ice *upstream.IpCappedError
	var sle *upstream.SessionLimitError
	var lie *upstream.LimitedIpError
	var be *upstream.BanError
	var cbe *upstream.CountryBlockedError
	var ce *upstream.CreditsError
	var cde *upstream.CapacityDeferredError
	switch {
	case errors.As(err, &be):
		status, code = http.StatusForbidden, "account_banned"
		message, retryAfter = be.Error(), time.Until(be.ResumesAt)
		if retryAfter < 0 {
			retryAfter = 0
		}
	case errors.As(err, &rle):
		status, code = http.StatusTooManyRequests, "rate_limited"
		message, retryAfter = rle.Error(), rle.RetryAfter
		if !rle.ResetAt.IsZero() && rle.ResetAt.After(time.Now()) {
			retryAfter = time.Until(rle.ResetAt)
		}
		if retryAfter < 0 {
			retryAfter = 0
		}
	case errors.As(err, &ice):
		// ip_capped: admission-only (too many distinct users on the egress
		// IP) — 429, not the quota 429, with the body's retryAfterMs only.
		status, code = http.StatusTooManyRequests, "ip_capped"
		message, retryAfter = ice.Error(), ice.RetryAfter
		if retryAfter < 0 {
			retryAfter = 0
		}
	case errors.As(err, &sle):
		// session_limit_reached (409): the ACCOUNT is over its concurrent-tab
		// budget; this session's row is fine. Never session-invalid.
		status, code = http.StatusConflict, "session_limit_reached"
		message = sle.Body
		if message == "" {
			message = "session limit reached"
		}
	case errors.As(err, &lie):
		// Issue #74 P2: the egress IP cannot serve the requested model.
		// 409 (not a quota lock): a different egress or a full-tier token
		// may still serve the model. The body's retryAfterMs is surfaced
		// as Retry-After but does not set the unfit window.
		status, code = http.StatusConflict, "model_ip_limited"
		message, retryAfter = lie.Error(), lie.RetryAfter
		if retryAfter < 0 {
			retryAfter = 0
		}
	case errors.As(err, &wr):
		status, code = http.StatusServiceUnavailable, "waiting_room_queued"
		message, retryAfter = wr.Error(), wr.RetryAfter
	case errors.As(err, &uwr):
		status, code = http.StatusServiceUnavailable, "waiting_room_queued"
		message, retryAfter = uwr.Error(), uwr.RetryAfter
	case errors.As(err, &cde):
		// #105 (server half): the client's capacity-deferred retry budget
		// (TRANSIENT_RETRIES) is exhausted, so the free tier's transient
		// capacity queue is surfaced to downstream clients as 429 +
		// Retry-After — they must honor the window, not hammer a 502/503.
		// MUST precede the generic errors.As(err, &ue) branch: the error
		// unwraps to a Retryable UpstreamError, which would otherwise be
		// swallowed as 503 upstream_retryable.
		status, code = http.StatusTooManyRequests, "free_mode_capacity_deferred"
		message = cde.Body
		if message == "" {
			message = cde.Error()
		}
		retryAfter = cde.RetryAfter
		if retryAfter <= 0 {
			retryAfter = 10 * time.Second
		}
	case errors.As(err, &ue):
		if ue.Retryable {
			// deployment_outside_hours etc.: temporarily unavailable, worth
			// a later retry — 503 lets clients/9router back off instead of
			// treating it as a hard failure.
			status, code = http.StatusServiceUnavailable, "upstream_retryable"
		} else {
			status = ue.Status
			if status != http.StatusPaymentRequired && status != http.StatusConflict && status != http.StatusTooManyRequests {
				status = http.StatusBadGateway
			}
		}
		message = ue.Body
		if message == "" {
			message = "upstream error"
		}
		retryAfter = ue.RetryAfter
	case errors.Is(err, registry.ErrModelNotFound):
		status, code = http.StatusBadRequest, "model_not_found"
		message = err.Error() + "; available: " + strings.Join(s.reg.Models(), ", ")
	case errors.Is(err, upstream.ErrAuthRejected):
		status, code = http.StatusBadGateway, "upstream_auth_rejected"
		message = err.Error()
	case errors.Is(err, upstream.ErrWaitingRoom):
		status, code = http.StatusServiceUnavailable, "waiting_room_queued"
		message = err.Error()
	case errors.As(err, &cbe):
		status, code = http.StatusForbidden, "country_blocked"
		message = cbe.Error()
	case errors.Is(err, upstream.ErrFreeModeCLIRequired):
		status, code = http.StatusForbidden, "free_mode_cli_required"
		message = err.Error()
	case errors.As(err, &ce):
		// 402 "Out of credits": surfacing the upstream body verbatim keeps
		// the quota detail (limit/recent/reset) for the client.
		status, code = http.StatusPaymentRequired, "out_of_credits"
		message = ce.Body
		if message == "" {
			message = "out of credits"
		}
	case errors.Is(err, context.DeadlineExceeded):
		status, code = http.StatusGatewayTimeout, "upstream_timeout"
		message = "upstream request timed out: " + err.Error()
	}

	s.logger.Warn("request failed", "status", status, "code", code, "err", err)
	s.writeJSONError(w, status, message, "upstream_error", code, retryAfter)
}
