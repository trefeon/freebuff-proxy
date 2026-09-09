package server

import (
	"context"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"strings"
	"time"

	"freebuff-proxy/backend/internal/dashboard"
)

// registerAdminRoutes mounts every dashboard.AdminRoutes row on the mux.
// Each row's Auth level selects the wrapping middleware stack it has always
// carried (see dashboard.AdminRoute for the level semantics); POST and
// DELETE rows are additionally wired through the CSRF gate. A row whose
// Path has no handler mapping panics — the table and the mapper ship as
// one commit.
func (s *Server) registerAdminRoutes(mux *http.ServeMux) {
	for _, r := range dashboard.AdminRoutes {
		h := s.adminHandler(r)
		// POST, PUT, and DELETE rows mutate state through the session cookie,
		// so all three carry the CSRF gate (GET rows are reads; the gate
		// itself skips GET/HEAD anyway).
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodDelete {
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
	case "GET /admin/api/settings":
		return http.HandlerFunc(s.admin.handleSettingsGet)
	case "POST /admin/api/settings":
		return http.HandlerFunc(s.admin.handleSettingsPost)
	case "DELETE /admin/api/settings/{key}":
		return http.HandlerFunc(s.admin.handleSettingsDelete)
	case "GET /admin/api/pages/{id}":
		return http.HandlerFunc(s.admin.handlePageStateGet)
	case "PUT /admin/api/pages/{id}":
		return http.HandlerFunc(s.admin.handlePageStatePut)
	case "GET /admin/api/logs":
		return s.dash.APIHandler("logs")
	case "GET /admin/api/quota/history":
		return s.dash.APIHandler("quota/history")
	case "GET /admin/api/maturity/history":
		return s.dash.APIHandler("maturity/history")
	case "GET /admin/api/logs/history":
		return s.dash.APIHandler("logs/history")
	case "GET /admin/api/metrics":
		return s.dash.APIHandler("metrics")
	case "GET /admin/api/version":
		return http.HandlerFunc(s.dash.APIVersion)
	case "GET /admin/api/auth/status":
		return http.HandlerFunc(s.admin.handleAdminAuthStatus)
	case "GET /admin/api/notices":
		return s.dash.APIHandler("notices")
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
	case "POST /admin/tokens/{id}/maturity":
		return http.HandlerFunc(s.admin.handleTokenMaturity)
	case "POST /admin/tokens/{id}/maturity/touch":
		return http.HandlerFunc(s.admin.handleTokenMaturityTouch)
	case "POST /admin/tokens/{id}/maturity/warn-reset":
		return http.HandlerFunc(s.admin.handleTokenMaturityWarnReset)
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
	case "POST /admin/api/require-login":
		return http.HandlerFunc(s.admin.handleAdminRequireLogin)
	case "POST /admin/smoke":
		return http.HandlerFunc(s.admin.handleSmoke)
	case "POST /admin/restart":
		return http.HandlerFunc(s.admin.handleAdminRestart)
	case "GET /admin/assets/":
		return noDirListing(http.StripPrefix("/admin/assets/", http.FileServerFS(mustSubFS(dashboard.DistFS(), "assets"))))
	default:
		panic("server: no admin handler for " + r.Method + " " + r.Path)
	}
}

// handleRoot answers GET /: 302 to /admin when the dashboard is enabled, 404
// otherwise. The exact-root pattern leaves every other path on the ServeMux
// default (/v1/*, /healthz, /metrics, /admin/* contracts untouched).
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Load().DashboardEnabled {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusFound)
}

// Handler returns the route table wrapped in an access-log middleware. Method
// mismatches and unknown paths get the ServeMux's automatic 405/404.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerOpenAIRoutes(mux)
	s.registerAnthropicRoutes(mux)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /{$}", s.handleRoot)
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
		// T17: LOG_ACCESS=false disables access lines entirely. Silent
		// paths (probes + dashboard GET polls) never log; quiet endpoints
		// (OPTIONS preflights) and UNKNOWN paths (404s) are rate-limited
		// to one access line per path per accessQuietWindow, and the
		// quiet-class budget caps the total quiet lines per window — a
		// client minting distinct paths cannot grow the access log without
		// bound (per-path gating alone would pass one line per unique
		// path). req_id/client_request_id survive in both cases.
		if !cfg.LogAccess {
			return
		}
		if silentAccessPath(r.Method, r.URL.Path) {
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
