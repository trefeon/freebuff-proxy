package server

// adminHandlers own the /admin surface (issue #250): every admin handler is
// a method on this struct instead of *Server, so the API/engine surface and
// the admin/dashboard surface no longer share one mutable god struct. The
// deps below are exactly what the admin handlers reach into; Server.New
// wires them once and server.adminHandler dispatches through s.admin.

import (
	"log/slog"
	"net/http"
	"sync"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/dashboard"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/ratelimit"
	"freebuff-proxy/backend/internal/registry"
	"freebuff-proxy/backend/internal/upstream"
)

type adminHandlers struct {
	dash *dashboard.Dashboard
	// logfunc reads the Server's CURRENT logger: tests replace it after
	// New (logging_wave2_test.newLoggingServer), so the admin surface must
	// follow it.
	logfunc    func() *slog.Logger
	pool       *pool.Pool
	reg        *registry.Registry
	cfgLoad    func() *config.Config
	cfgStore   func(*config.Config)
	configPath string

	adminAuth   *adminAuth
	adminSaveMu sync.Mutex
	loginMu     sync.Mutex
	loginFlows  map[string]*loginFlow
	// authClientFunc reads the Server's current login-wizard client: options
	// may install it after construction.
	authClientFunc func() *upstream.Client
	rateLimiter    *ratelimit.Limiter

	// handleChat forwards the playground's synthetic chat request to the
	// normal chat pipeline (admin.go:176).
	handleChat func(w http.ResponseWriter, r *http.Request)
}
