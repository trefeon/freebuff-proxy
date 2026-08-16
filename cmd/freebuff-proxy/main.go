// Command freebuff-proxy is the FreeBuff proxy bridge entrypoint.
//
// Slice 1: config loading, the model registry (fallback at boot + background
// refresh at REGISTRY_REFRESH), and a graceful SIGINT/SIGTERM shutdown.
// Slice 4: the OpenAI-compatible HTTP surface (/v1/chat/completions,
// /v1/models, /healthz) over the multi-token pool, with graceful drain on
// shutdown (server stops accepting first, then runs/sessions finish).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	// Embed the IANA tzdata so NextPacificMidnight keeps exact DST math on
	// minimal images (alpine:3.20 has no /usr/share/zoneinfo) and Windows
	// hosts without the timezone registry entries. Without this, Pacific
	// resets fall back to a month-based approximation.
	_ "time/tzdata"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/egress"
	"freebuff-proxy/internal/logring"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/server"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/telemetry"
	"freebuff-proxy/internal/upstream"
)

// version is injected at build time by GoReleaser (-ldflags -X main.version=...).
// When building without GoReleaser it stays "dev".
var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to an optional JSON config file (keys mirror env names)")
	verbose := flag.Bool("v", false, "verbose (debug) logging")
	showVersion := flag.Bool("version", false, "print version and exit")
	showDoctor := flag.Bool("doctor", false, "run environment and configuration diagnostics")
	showUpdate := flag.Bool("update", false, "check for and download the latest release update")
	showSetup := flag.Bool("setup", false, "run interactive client configuration helper")
	testToken := flag.Bool("test-token", false, "probe the first configured token with a real session handshake and exit 0/1")
	autoYes := flag.Bool("yes", false, "auto-confirm prompts during setup")
	flag.Parse()

	modeFlags := 0
	for _, set := range []bool{*showDoctor, *showUpdate, *showSetup, *testToken} {
		if set {
			modeFlags++
		}
	}
	if modeFlags > 1 {
		fmt.Fprintln(os.Stderr, "freebuff-proxy: warning: -doctor, -update, -setup and -test-token are mutually exclusive; only the first will run")
	}

	if *showVersion {
		fmt.Println("freebuff-proxy", version)
		os.Exit(0)
	}
	if *testToken {
		runTokenTest(*configPath)
	}
	if *showDoctor {
		runDoctor(*configPath)
	}
	if *showUpdate {
		runUpdate()
	}
	if *showSetup {
		runSetup(*autoYes)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freebuff-proxy: invalid config:", err)
		holdForExitIfConsole()
		os.Exit(1)
	}

	// Effective log level: LOG_LEVEL config wins, else -v → debug, else info.
	level, _ := telemetry.ParseLevel(cfg.LogLevel)
	if cfg.LogLevel == "" {
		if *verbose {
			level = slog.LevelDebug
		} else {
			level = slog.LevelInfo
		}
	}
	logger := telemetry.New(level, cfg.LogFile)
	// The dashboard log viewer reads from an in-memory ring that mirrors
	// every record the process logger emits (no log file or docker needed).
	logringHandler := logring.NewHandler(logger.Handler(), 500)
	logger = slog.New(logringHandler)
	// The pool/upstream/session/runs log through slog.Default(); route it
	// through our logger so the configured level and log file cover them too.
	slog.SetDefault(logger)

	// The proxy reads ./.env from the working directory, which on Windows
	// launchers (Task Scheduler, shortcuts, services) is often not the
	// executable's directory. Log the absolute path used, and warn when a
	// .env sitting next to the executable is silently ignored — that is the
	// usual reason config "seems to vanish" under a non-interactive launcher.
	envFile, _ := filepath.Abs(".env")
	logger.Info("config loaded", "env_file", envFile, "config_file", *configPath)
	if cwd, err := os.Getwd(); err == nil {
		exe, exeErr := os.Executable()
		if exeErr == nil {
			exeDir := filepath.Dir(exe)
			if filepath.Clean(cwd) != exeDir {
				if _, statErr := os.Stat(filepath.Join(exeDir, ".env")); statErr == nil {
					logger.Warn("found .env next to the executable, but .env is read from the working directory — that file is NOT applied",
						"cwd", cwd, "exe_dir", exeDir, "env_file", envFile)
				}
			}
		}
	}

	// Load the hardcoded fallback immediately so the registry is usable
	// offline; the first background refresh replaces it on success.
	reg := registry.New(&cfg, &http.Client{Timeout: 30 * time.Second})
	reg.LoadFallback()

	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer stop()

	go refreshLoop(ctx, logger, reg, cfg.RegistryRefresh)

	// One upstream client and session manager per token, bound into the pool
	// together with a per-token run manager. When SESSION_PERSIST is enabled
	// one shared store backs every session manager (fixed, runtime-added, and
	// bridge entries), so a restart resumes unexpired sessions.
	var store *session.Store
	if cfg.SessionPersist {
		store = session.NewStore(cfg.SessionStateFile)
		logger.Info("session state persistence enabled", "file", cfg.SessionStateFile)
	}
	clients := make([]*upstream.Client, 0, len(cfg.AuthTokens))
	sessions := make([]*session.Manager, 0, len(cfg.AuthTokens))
	for i, token := range cfg.AuthTokens {
		client, err := upstream.NewWithIndex(token, i, &cfg)
		if err != nil {
			logger.Error("failed to build upstream client", "err", err)
			holdForExitIfConsole()
			os.Exit(1)
		}
		clients = append(clients, client)
		sessions = append(sessions, session.NewManagerWithStore(client, store))
	}
	if cfg.DiscoveredSource != "" {
		logger.Info("auto-discovered FreeBuff token from CLI login", "email", cfg.DiscoveredEmail, "file", cfg.DiscoveredSource)
	}
	p, err := pool.New(&cfg, clients, sessions, reg)
	if err != nil {
		logger.Error("failed to build pool", "err", err)
		holdForExitIfConsole()
		os.Exit(1)
	}
	p.SetSessionStore(store)

	// Prewarm + the 60s maintain loop run until ctx is canceled (shutdown).
	p.Start(ctx)

	// Egress probing: report the country/IP each outbound path appears to
	// come from (ban-avoidance diagnostics). The direct path is always
	// probed; SOCKS5_PROXIES entries are probed through their own dialer.
	// Results are cached and refreshed every 10 minutes; failures are
	// logged and cached with Err set (fail-open).
	egressCache := egress.NewCache()
	if paths := egressPaths(&cfg, logger); len(paths) > 0 {
		go egress.RunLoop(ctx, logger, egressCache, paths, egress.ProbeTimeout, egress.DefaultTTL)
		logger.Info("egress probes started", "paths", len(paths))
	}

	srv := server.New(&cfg, p, reg, logger, logringHandler, *configPath)
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       60 * time.Second,
		// IdleTimeout closes keep-alive connections that have been idle for
		// two minutes, bounding goroutines parked on dead clients.
		IdleTimeout: 120 * time.Second,
		// WriteTimeout is deliberately unset (0): /v1/chat/completions
		// streams SSE responses that can legitimately outlive any fixed
		// write budget.
	}

	// Startup summary -- token values are never logged, only counts.
	logger.Info("freebuff-proxy starting",
		"version", version,
		"listen_addr", cfg.ListenAddr,
		"upstream", cfg.UpstreamBaseURL,
		"auth_tokens", len(cfg.AuthTokens),
		"bridge_mode", len(cfg.AuthTokens) == 0,
		"api_keys", len(cfg.APIKeys),
		"cost_mode", cfg.CostMode,
		"rotation_interval", cfg.RotationInterval.String(),
		"registry_refresh", cfg.RegistryRefresh.String(),
		"registry_agents", len(reg.AgentIDs()),
		"registry_models", reg.ModelCount(),
		"log_level", level.String(),
		"verbose", *verbose,
	)
	// /admin/reload and the admin dashboard are open in default deployments
	// (no API_KEYS, or bridge mode): warn loudly so operators can decide
	// whether to set ADMIN_TOKEN.
	if cfg.AdminToken == "" && (len(cfg.APIKeys) == 0 || cfg.BridgeMode()) {
		logger.Warn("/admin/reload and the /admin dashboard are unauthenticated — any client that can reach the proxy can reload configuration and view its state. Set ADMIN_TOKEN to require a bearer token")
	}
	logger.Info("listening", "addr", cfg.ListenAddr)

	// Human-readable startup banner for interactive terminals. Suppressed
	// when stderr is piped (containers, log files, systemd) -- detected by
	// checking if the output is a character device (terminal).
	if stderrIsCharDevice() {
		mode := fmt.Sprintf("pooled (%d tokens)", len(cfg.AuthTokens))
		if cfg.BridgeMode() {
			mode = "bridge (clients send their own token)"
		}
		fmt.Fprintf(os.Stderr, "\n"+
			"  freebuff-proxy %s is running!\n"+
			"\n"+
			"  API endpoint:  http://%s/v1\n"+
			"  Health check:  http://%s/healthz\n"+
			"  Models:        http://%s/v1/models\n"+
			"  Mode:          %s\n"+
			"\n"+
			"  Quick test:\n"+
			"    curl http://%s/healthz\n"+
			"\n"+
			"  Press Ctrl+C to stop.\n\n",
			version, cfg.ListenAddr, cfg.ListenAddr, cfg.ListenAddr, mode, cfg.ListenAddr,
		)
	}

	// Serve until the server fails or a shutdown signal arrives.
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.ListenAndServe()
	}()

	// A bind failure (port already in use) is the most common startup
	// error and the one that looks like "cannot open" when the EXE is
	// double-clicked: print a prominent hint naming the offender before
	// draining. Any server failure exits non-zero so scripts/health checks
	// can tell the process did not come up.
	exitCode := 0
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			if isPortInUse(err) {
				printPortInUseHint(cfg.ListenAddr, err)
			} else {
				logger.Error("http server failed", "err", err)
			}
			exitCode = 1
			stop() // cancel ctx: stop the pool jobs, then drain
		}
	case <-ctx.Done():
	}

	// Graceful drain: stop accepting new requests first, then finish
	// runs/sessions. HTTP gets a 10s force deadline; the pool then gets its
	// OWN fresh budget — a slow-draining SSE stream can consume the whole
	// HTTP budget, and the pool drain must not be starved by it.
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http server shutdown incomplete", "err", err)
	}
	poolCtx, poolCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer poolCancel()
	p.Shutdown(poolCtx)
	logger.Info("shutdown complete")
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

// stderrIsCharDevice reports whether stderr is a character device (an
// interactive console). Piped or redirected stderr (containers, log files,
// services, Task Scheduler) is not, so interactive-only behavior is skipped.
func stderrIsCharDevice() bool {
	fi, err := os.Stderr.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// shutdownSignals are the OS signals that trigger graceful drain. On Windows
// the Go runtime delivers BOTH Ctrl+C and Ctrl+Break as os.Interrupt (see
// runtime/os_windows.go ctrlHandler: CTRL_C_EVENT and CTRL_BREAK_EVENT map
// to SIGINT), so registering os.Interrupt already makes Ctrl+Break drain
// instead of killing the process instantly. There is no separate
// syscall.SIGBREAK constant in Go; TestCtrlBreakDrainsGracefully pins the
// behavior end to end on Windows.
func shutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}

// holdForExitIfConsole prints "Press Enter to exit." and waits for input when
// stderr is an interactive console, so a double-clicked EXE does not flash
// its window shut before the error above it is readable. No-op when stderr is
// piped, so scripts and containers never hang on shutdown.
func holdForExitIfConsole() {
	if !stderrIsCharDevice() {
		return
	}
	fmt.Fprintln(os.Stderr, "Press Enter to exit.")
	_, _ = fmt.Scanln()
}

// egressPaths returns the probe paths for the configured outbound routes:
// index 0 is always the direct connection; each SOCKS5_PROXIES entry is
// probed through its own SOCKS5 dialer. Unparseable proxy addresses are
// skipped with a warning (fail-open); the direct probe always survives.
func egressPaths(cfg *config.Config, logger *slog.Logger) []egress.Path {
	paths := []egress.Path{{Key: "direct", Dialer: egress.DirectDialer(egress.ProbeTimeout)}}
	for i, raw := range cfg.SOCKS5Proxies {
		dialer, err := egress.Socks5Dialer(raw)
		if err != nil {
			logger.Warn("egress probe: skipping invalid SOCKS5 proxy", "index", i, "err", err)
			continue
		}
		paths = append(paths, egress.Path{Key: fmt.Sprintf("proxy-%d", i), Dialer: dialer})
	}
	return paths
}

// refreshLoop refreshes the registry immediately, then every interval.
// Refresh failures keep the previous state (the fallback at boot); the next
// tick retries.
func refreshLoop(ctx context.Context, logger *slog.Logger, reg *registry.Registry, interval time.Duration) {
	logRegistryRefresh(ctx, logger, reg)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logRegistryRefresh(ctx, logger, reg)
		}
	}
}

func logRegistryRefresh(ctx context.Context, logger *slog.Logger, reg *registry.Registry) {
	if err := reg.Refresh(ctx); err != nil {
		logger.Warn("registry refresh failed; keeping previous state", "err", err)
		return
	}
	logger.Info("registry refreshed", "agents", len(reg.AgentIDs()), "models", reg.ModelCount())
}
