package server

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"freebuff-proxy/backend/internal/config"
)

// cfgSnapshotKey carries the per-request *config.Config snapshot through
// the request context. requireAuth (the outermost /v1 auth wrapper) loads
// the config ONCE per request and stamps it here; chatCore and authorized
// then decide pooled-vs-bridge routing from that same snapshot, so a
// config swap (e.g. /admin/reload) landing between the middleware's
// pass-through and the handler's routing cannot split one request's
// decision across two different config views.
type cfgSnapshotKey struct{}

func withCfgSnapshot(ctx context.Context, cfg *config.Config) context.Context {
	return context.WithValue(ctx, cfgSnapshotKey{}, cfg)
}

// cfgSnapshotFrom returns the config snapshot stamped by requireAuth, or
// nil when the request never passed through it (direct handler calls in
// tests) — callers fall back to their own load in that case.
func cfgSnapshotFrom(ctx context.Context) *config.Config {
	cfg, _ := ctx.Value(cfgSnapshotKey{}).(*config.Config)
	return cfg
}

// requireAuth wraps a handler with client-auth enforcement. When no API keys
// are configured the handler passes through untouched; /healthz is always
// exempt (the caller wires it without requireAuth). Bridge mode (no
// AUTH_TOKENS) also passes through: the Authorization header IS the upstream
// token there, and API_KEYS is meaningless.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfg.Load()
		// Pin the snapshot this decision used: chatCore and authorized
		// below must route this same request from the same config view.
		r = r.WithContext(withCfgSnapshot(r.Context(), cfg))
		// Hybrid mode (AUTH_TOKENS + BRIDGE_ENABLED) passes through too:
		// the per-request decision — pooled vs bridge — happens in
		// chatCore, where a credential matching API_KEYS uses the pool and
		// any other credential is relayed as a bridge token.
		if len(cfg.APIKeys) == 0 || cfg.BridgeMode() || cfg.HybridBridgeMode() {
			next(w, r)
			return
		}
		if !s.authorized(cfg, r) {
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
// either as "Authorization: Bearer <key>", "x-api-key: <key>", or
// "anthropic-api-key: <key>". Comparison is constant-time against every
// configured key. cfg is the caller's config snapshot (see cfgSnapshotKey):
// the check must use the same view that made the surrounding routing
// decision, never a fresh load.
func (s *Server) authorized(cfg *config.Config, r *http.Request) bool {
	provided := ""
	if tok, ok := extractBearerToken(r.Header.Get("Authorization")); ok {
		provided = tok
	} else if h := strings.TrimSpace(r.Header.Get("x-api-key")); h != "" {
		provided = h
	} else if h := strings.TrimSpace(r.Header.Get("anthropic-api-key")); h != "" {
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
// the loopback gate (adminSensitive, wired between requireAdminToken and
// requireAuth) and the legacy API_KEYS gate then apply, and main.go logs a
// startup warning for the open (default) case.
func (s *Server) requireAdminToken(next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfg.Load()
		if cfg.AdminToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		provided := ""
		if tok, ok := extractBearerToken(r.Header.Get("Authorization")); ok {
			provided = tok
		}
		if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(cfg.AdminToken)) != 1 {
			s.writeJSONError(w, http.StatusUnauthorized,
				"Invalid password", "invalid_request_error", "invalid_admin_token", 0)
			return
		}
		next.ServeHTTP(w, r)
	}
}

// clientToken returns the request's bearer token (Authorization: Bearer,
// x-api-key, or anthropic-api-key), trimmed. Empty when the request carries
// none. In bridge mode this token IS the client's FreeBuff token relayed
// upstream.
func clientToken(r *http.Request) string {
	provided := ""
	if tok, ok := extractBearerToken(r.Header.Get("Authorization")); ok {
		provided = tok
	} else if h := r.Header.Get("x-api-key"); h != "" {
		provided = h
	} else if h := r.Header.Get("anthropic-api-key"); h != "" {
		provided = h
	}
	return strings.TrimSpace(provided)
}

// bearerToken returns only the Authorization: Bearer token (the
// Authorization header value without the "Bearer " prefix). Returns "" if
// no Bearer token is present.
func bearerToken(r *http.Request) string {
	if tok, ok := extractBearerToken(r.Header.Get("Authorization")); ok {
		return tok
	}
	return ""
}
