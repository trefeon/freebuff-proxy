// Package cli implements the freebuff-proxy serve mode (the default when no
// subcommand flag is set): config loading, log construction, registry,
// pool/session wiring, the HTTP server, and graceful drain on shutdown. It
// also holds the shared helpers every mode uses (signal set, console-hold,
// log-level resolution, exe-adjacent .env warning, admin cleartext warning).
//
// The per-mode logic lives in sibling subpackages (setup, update, service,
// doctor, port, refreshtoken, validate); main.go is a thin flag parser that
// dispatches to whichever mode is selected and maps the returned exit code to
// os.Exit.
package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	// Embed the IANA tzdata so NextPacificMidnight keeps exact DST math on
	// minimal images (alpine:3.20 has no /usr/share/zoneinfo) and Windows
	// hosts without the timezone registry entries. Without this, Pacific
	// resets fall back to a month-based approximation.
	_ "time/tzdata"

	"freebuff-proxy/backend/internal/cli/port"
	"freebuff-proxy/backend/internal/clicreds"
	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/logring"
	"freebuff-proxy/backend/internal/notify"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/registry"
	"freebuff-proxy/backend/internal/server"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/telemetry"
	"freebuff-proxy/backend/internal/updatecheck"
	"freebuff-proxy/backend/internal/upstream"
)

// Serve runs the default serve mode: load config, construct the logger,
// build the registry and pool bound to one upstream client + session manager
// per token, start the background registry refresh and pool, serve HTTP, and
// drain gracefully on a shutdown signal. It returns the process exit code
// (0 normal, 1 server failure); the caller maps it to os.Exit.
func Serve(configPath string, verbose bool, version string) int {
	cfg, err := config.LoadOpts(configPath, config.LoadOptions{DiscoverCLIToken: clicreds.DiscoverToken})
	if err != nil {
		fmt.Fprintln(os.Stderr, "freebuff-proxy: invalid config:", err)
		holdForExitIfConsole()
		return 1
	}

	// Effective log level: LOG_LEVEL config wins, else -v → debug, else info.
	level := resolveLogLevel(cfg.LogLevel, verbose)
	logger := telemetry.New(level, cfg.LogFile, cfg.LogFormat)
	// The dashboard log viewer reads from an in-memory ring that mirrors
	// every record the process logger emits (no log file or docker needed).
	logringHandler := logring.NewHandler(logger.Handler(), cfg.LogRingSize)
	logger = slog.New(logringHandler)
	// The pool/upstream/session/runs log through slog.Default(); route it
	// through our logger so the configured level and log file cover them too.
	slog.SetDefault(logger)

	// The proxy reads the resolved .env (issue #39): ./.env in the working
	// directory wins; otherwise the platform config dir is tried
	// ($XDG_CONFIG_HOME / %APPDATA% / ~/Library/Application Support, under
	// freebuff-proxy/). Log the absolute path used, and warn when a .env
	// sitting next to the executable is silently ignored — that is the
	// usual reason config "seems to vanish" under a non-interactive
	// launcher (Task Scheduler, shortcuts, services).
	envFile := cfg.EnvFile
	if envFile != "" {
		if abs, err := filepath.Abs(envFile); err == nil {
			envFile = abs
		}
	}
	logger.Info("config loaded", "env_file", envFile, "config_file", configPath)
	if cfg.EnvFile == "" {
		if cwd, err := os.Getwd(); err == nil {
			exe, exeErr := os.Executable()
			if exeErr == nil {
				if p := ignoredExeAdjacentEnv(cwd, exe); p != "" {
					logger.Warn("found .env next to the executable, but no .env candidate exists in the config search path — that file is NOT applied",
						"cwd", cwd, "exe_dir", filepath.Dir(exe))
				}
			}
		}
	} else if cwd, err := os.Getwd(); err == nil {
		exe, exeErr := os.Executable()
		if exeErr == nil {
			if p := ignoredExeAdjacentEnv(cwd, exe); p != "" {
				logger.Warn("found .env next to the executable, but the config search resolved a different .env — that file is NOT applied",
					"cwd", cwd, "exe_dir", filepath.Dir(exe), "env_file", envFile)
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
		// Log the absolute state-file path: a relative SESSION_STATE_FILE is
		// resolved against the working directory, which is where the file
		// actually appears on disk.
		stateFile := cfg.SessionStateFile
		if abs, err := filepath.Abs(stateFile); err == nil {
			stateFile = abs
		}
		store = session.NewStore(stateFile)
		logger.Info("session state persistence enabled", "file", stateFile)

		// Same cwd-vs-exe trap as .env: on Windows launchers (Task
		// Scheduler, shortcuts, services) the working directory is often not
		// the executable's directory, so warn when a state file next to the
		// executable is silently ignored for the same reason.
		if !filepath.IsAbs(cfg.SessionStateFile) {
			if cwd, err := os.Getwd(); err == nil {
				exe, exeErr := os.Executable()
				if exeErr == nil {
					exeDir := filepath.Dir(exe)
					if filepath.Clean(cwd) != exeDir {
						if _, statErr := os.Stat(filepath.Join(exeDir, cfg.SessionStateFile)); statErr == nil {
							logger.Warn("found session state file next to the executable, but SESSION_STATE_FILE is read from the working directory — that file is NOT used",
								"cwd", cwd, "exe_dir", exeDir, "state_file", stateFile)
						}
					}
				}
			}
		}
	}
	clients := make([]*upstream.Client, 0, len(cfg.AuthTokens))
	sessions := make([]*session.Manager, 0, len(cfg.AuthTokens))
	for i, token := range cfg.AuthTokens {
		client, err := upstream.NewWithIndex(token, i, &cfg)
		if err != nil {
			logger.Error("failed to build upstream client", "err", err)
			holdForExitIfConsole()
			return 1
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
		return 1
	}
	p.SetSessionStore(store)

	// Issue #48: best-effort webhook alerts (WEBHOOK_URL) for pool
	// exhaustion / token bans — fire-and-forget, throttled, never blocking.
	if cfg.WebhookURL != "" {
		p.SetNotifier(notify.New(cfg.WebhookURL, nil))
		logger.Info("webhook alerts enabled", "url", notify.RedactURL(cfg.WebhookURL))
	}

	// Issue #97: ADOPT_CLI_SESSION — seed every session manager with the
	// CLI-session adoption mode (owner file re-read per refresh; never
	// create a competing session while the CLI is alive).
	if cfg.AdoptCLISession {
		ownerFile, err := cliOwnerFilePath()
		if err != nil {
			logger.Error("ADOPT_CLI_SESSION: cannot resolve freebuff-instance-owner.json", "err", err)
			holdForExitIfConsole()
			return 1
		}
		for _, sess := range sessions {
			sess.SetCLIAdoption(session.CLIAdoption{Enabled: true, OwnerFile: ownerFile})
		}
		logger.Info("ADOPT_CLI_SESSION: adopting the official CLI session (single-session friendly)", "owner_file", ownerFile)
	}

	// Prewarm + the 60s maintain loop run until ctx is canceled (shutdown).
	p.Start(ctx)

	// Egress probing is deliberately NOT wired into startup (#123): the
	// official CLI never talks to cloudflare.com (the probe target), and
	// the background loop's risk-engine feed has no consumer (Score()
	// reads only upstream privacy signals + ip-cap ratios, never the
	// probe's IP/country). The probe still runs on demand — `-doctor`
	// re-probes with its own cache — so operators keep the "Egress region"
	// readout without an extra recurring request the CLI would never make.

	// Issue #62: the dashboard login wizard drives the same headless OAuth
	// flow as the CLI against the proxy's own transport/stealth wiring; the
	// token it yields is added to the pool + .env (nil disables the wizard).
	loginClient, err := upstream.NewForAuth(&cfg)
	if err != nil {
		slog.Warn("dashboard login client unavailable (login wizard disabled)", "err", err)
	}
	serverOpts := []server.Option{server.WithLoginClient(loginClient)}
	// Issue #50b: release update indicator — the dashboard badge compares
	// the running version against the latest GitHub release (6h cache).
	serverOpts = append(serverOpts, server.WithVersion(version, updatecheck.New(updatecheck.DefaultRepo, nil)))

	srv := server.New(&cfg, p, reg, logger, logringHandler, configPath, serverOpts...)
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
		"log_level", logLevelDisplay(level),
		"verbose", verbose,
		"dashboard_enabled", cfg.DashboardEnabled,
	)
	if cfg.ActingUserID != "" {
		// #126: the header is only safe with the token's OWN account id (the
		// CLI derives it from /api/v1/me; the server honors it only for the
		// FreeBuff Web service account) — any other value impersonates a
		// foreign user and can flag the account.
		logger.Info("acting user id set — x-freebuff-acting-user-id will be sent on chat calls (only safe with the token's own account id; any other value impersonates another user)", "acting_user_id", cfg.ActingUserID)
	}
	// Warn loudly if the dashboard is running with the factory default password ("123456").
	if cfg.DashboardEnabled && cfg.IsDefaultAdminToken() {
		logger.Warn("ADMIN_TOKEN is using default password ('123456') — change it immediately in dashboard settings or .env to secure this instance")
	}
	if w := adminTokenCleartextWarning(cfg.AdminToken, cfg.ListenAddr); w != "" {
		logger.Warn(w)
	}
	logger.Info("listening", "addr", cfg.ListenAddr)

	// Human-readable startup banner for interactive terminals. Suppressed
	// when stderr is piped (containers, log files, systemd) -- detected by
	// checking if the output is a character device (terminal).
	if stderrIsCharDevice() {
		mode := fmt.Sprintf("pooled (%d tokens)", len(cfg.AuthTokens))
		switch {
		case cfg.BridgeMode():
			mode = "bridge (clients send their own token)"
		case cfg.HybridBridgeMode():
			mode = fmt.Sprintf("hybrid (%d pooled tokens + bridge relay)", len(cfg.AuthTokens))
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
			if port.IsPortInUse(err) {
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
	return exitCode
}

// stderrIsCharDevice reports whether stderr is a character device (an
// interactive console). Piped or redirected stderr (containers, log files,
// services, Task Scheduler) is not, so interactive-only behavior is skipped.
func stderrIsCharDevice() bool {
	fi, err := os.Stderr.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// adminTokenCleartextWarning returns the startup warning for an ADMIN_TOKEN
// deployment served over plain HTTP: the proxy binary has no TLS support
// (http.Server.ListenAndServe only), so when LISTEN_ADDR binds a
// non-loopback interface the admin login POST and the fb_admin session
// cookie travel in cleartext across the network. Empty when there is
// nothing to warn about (no ADMIN_TOKEN, or loopback-only listen).
func adminTokenCleartextWarning(adminToken, listenAddr string) string {
	if adminToken == "" || listenIsLoopback(listenAddr) {
		return ""
	}
	return "ADMIN_TOKEN is set but LISTEN_ADDR binds a non-loopback interface and the proxy does not serve TLS — the admin login POST and session cookie travel in cleartext. Bind LISTEN_ADDR to a loopback address (e.g. 127.0.0.1:3457) or terminate TLS in front of the proxy"
}

// listenIsLoopback reports whether a LISTEN_ADDR binds only loopback
// interfaces: a loopback IP (127.0.0.0/8 or ::1, optional port) or the name
// "localhost". An empty host (":3457") binds every interface — not loopback.
func listenIsLoopback(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

// ModeFlagsExclusiveWarning returns the warning printed when 2+ of the
// mutually-exclusive mode flags (-doctor/-update/-setup/-test-token/
// -validate-tokens/-install-service/-uninstall-service/-service-status)
// are set; "" when at most one is set (only the first flag then runs).
func ModeFlagsExclusiveWarning(flags ...bool) string {
	n := 0
	for _, set := range flags {
		if set {
			n++
		}
	}
	if n <= 1 {
		return ""
	}
	return "freebuff-proxy: warning: -doctor, -update, -setup, -test-token, -validate-tokens, -install-service, -uninstall-service and -service-status are mutually exclusive; only the first will run"
}

// resolveLogLevel applies the effective log-level precedence: a set
// LOG_LEVEL config wins, -v → debug, else info. An unparseable LOG_LEVEL
// silently falls back to info (ParseLevel returns level 0, which is Info).
func resolveLogLevel(cfgLogLevel string, verbose bool) slog.Level {
	if cfgLogLevel != "" {
		if lv, ok := telemetry.ParseLevel(cfgLogLevel); ok {
			return lv
		}
		return slog.LevelInfo
	}
	if verbose {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// logLevelDisplay renders the configured level for the startup summary.
// LevelTrace prints as TRACE instead of slog's "DEBUG-4" (the level sits
// below DEBUG, so slog's String() appends the negative offset).
func logLevelDisplay(level slog.Level) string {
	if level == telemetry.LevelTrace {
		return "TRACE"
	}
	return level.String()
}

// ignoredExeAdjacentEnv returns the path of a .env that sits next to the
// executable while the process reads ./.env from the working directory —
// the usual reason config "seems to vanish" under a non-interactive
// launcher (Task Scheduler, shortcuts, services). Empty when the working
// directory IS the executable's directory, or no .env exists next to it.
func ignoredExeAdjacentEnv(cwd, exePath string) string {
	if cwd == "" || exePath == "" {
		return ""
	}
	exeDir := filepath.Dir(exePath)
	if filepath.Clean(cwd) == exeDir {
		return ""
	}
	p := filepath.Join(exeDir, ".env")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
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
	// Success is logged inside Registry.Refresh (agents/models/ms); only the
	// failure path lives here so refresh failures stay visible at the caller.
	if err := reg.Refresh(ctx); err != nil {
		logger.Warn("registry refresh failed; keeping previous state", "err", err)
	}
}

// cliOwnerFilePath returns the platform freebuff-instance-owner.json path
// (issue #97) — the manicode config dir, matching the credentials file the
// auto-discoverer reads.
func cliOwnerFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("cannot resolve home directory")
	}
	return filepath.Join(home, ".config", "manicode", "freebuff-instance-owner.json"), nil
}

// printPortInUseHint writes the actionable port-conflict message to stderr.
// On an interactive console (stderr is a char device — the double-clicked
// EXE case) it holds the window open until Enter so the message is readable.
func printPortInUseHint(addr string, err error) {
	p := port.PortOf(addr)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "freebuff-proxy: cannot listen on", addr)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  Port "+p+" is already in use by another process.")
	if owner := port.PortOwner(p); owner != "" {
		fmt.Fprintln(os.Stderr, "  The process using it:  "+owner)
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  To close the other app, find and stop it:")
	if isWindows() {
		fmt.Fprintln(os.Stderr, "    netstat -ano | findstr :"+p+"   (note the PID of the LISTENING line)")
		fmt.Fprintln(os.Stderr, "    taskkill /PID <pid> /F")
	} else {
		fmt.Fprintln(os.Stderr, "    lsof -i :"+p+"   (note the PID)")
		fmt.Fprintln(os.Stderr, "    kill <pid>")
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  Then start freebuff-proxy again.")
	fmt.Fprintln(os.Stderr)
	// Shared with the other fatal startup errors: hold the window open on an
	// interactive console, no-op when stderr is piped.
	holdForExitIfConsole()
}

// isWindows reports whether the process runs on Windows.
func isWindows() bool { return runtime.GOOS == "windows" }
