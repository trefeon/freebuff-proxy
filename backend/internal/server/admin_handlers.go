package server

// adminHandlers own the /admin surface (issue #250): every admin handler is
// a method on this struct instead of *Server, so the API/engine surface and
// the admin/dashboard surface no longer share one mutable god struct. The
// deps below are exactly what the admin handlers reach into; Server.New
// wires them once and server.adminHandler dispatches through s.admin.

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

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

var restartProcess = func() {
	os.Exit(0)
}

func (a *adminHandlers) handleAdminRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Validate configuration before restart to prevent exiting on broken settings
	if _, err := config.Load(a.configPath); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      false,
			"message": "Config validation failed — aborting restart: " + err.Error(),
		})
		return
	}

	a.logfunc().Info("admin restart initiated via dashboard", "remote", remoteHost(r))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"message": "Gateway process restart initiated.",
	})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	go func() {
		time.Sleep(200 * time.Millisecond)
		restartProcess()
	}()
}
