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
	"freebuff-proxy/internal/notify"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/server"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/telemetry"
	"freebuff-proxy/internal/updatecheck"
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
	testToken := flag.Bool("test-token", false, "probe the first configured token with a zero-cost GET probe (no session consumed) and exit 0/1")
	installService := flag.Bool("install-service", false, "register the current binary as a background service and start it (Task Scheduler / systemd --user / launchd)")
	uninstallService := flag.Bool("uninstall-service", false, "stop and unregister the background service")
	serviceStatus := flag.Bool("service-status", false, "check whether the background service is registered and running (exit 0 registered, 1 not)")
	autoYes := flag.Bool("yes", false, "auto-confirm prompts during setup")
	refreshToken := flag.Int("refresh-token", -1, "re-authenticate token #N in .env via the headless GitHub login flow and exit (interactive: start → print login URL → poll; with -yes and GITHUB_USER/GITHUB_PASSWORD/GITHUB_TOTP set: protocol login)")
	flag.Parse()

	if w := modeFlagsExclusiveWarning(*showDoctor, *showUpdate, *showSetup, *testToken, *installService, *uninstallService, *serviceStatus); w != "" {
		fmt.Fprintln(os.Stderr, w)
	}

	if *showVersion {
		fmt.Println("freebuff-proxy", version)
		os.Exit(0)
	}
	if *testToken {
		runTokenTest(*configPath)
	}
	if *refreshToken >= 0 {
		runTokenRefresh(*configPath, *refreshToken, *autoYes)
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
	if *installService {
		runServiceInstall()
	}
	if *uninstallService {
		runServiceUninstall()
	}
	if *serviceStatus {
		runServiceStatus()
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freebuff-proxy: invalid config:", err)
		holdForExitIfConsole()
		os.Exit(1)
	}

	// Effective log level: LOG_LEVEL config wins, else -v → debug, else info.
	level := resolveLogLevel(cfg.LogLevel, *verbose)
	logger := telemetry.New(level, cfg.LogFile)
	// The dashboard log viewer reads from an in-memory ring that mirrors
	// every record the process logger emits (no log file or docker needed).
	logringHandler := logring.NewHandler(logger.Handler(), 500)
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
	logger.Info("config loaded", "env_file", envFile, "config_file", *configPath)
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

	// Issue #48: best-effort webhook alerts (WEBHOOK_URL) for pool
	// exhaustion / token bans — fire-and-forget, throttled, never blocking.
	if cfg.WebhookURL != "" {
		p.SetNotifier(notify.New(cfg.WebhookURL, nil))
		logger.Info("webhook alerts enabled", "url", cfg.WebhookURL)
	}

	// Issue #97: ADOPT_CLI_SESSION — seed every session manager with the
	// CLI-session adoption mode (owner file re-read per refresh; never
	// create a competing session while the CLI is alive).
	if cfg.AdoptCLISession {
		ownerFile, err := cliOwnerFilePath()
		if err != nil {
			logger.Error("ADOPT_CLI_SESSION: cannot resolve freebuff-instance-owner.json", "err", err)
			holdForExitIfConsole()
			os.Exit(1)
		}
		for _, sess := range sessions {
			sess.SetCLIAdoption(session.CLIAdoption{Enabled: true, OwnerFile: ownerFile})
		}
		logger.Info("ADOPT_CLI_SESSION: adopting the official CLI session (single-session friendly)", "owner_file", ownerFile)
	}

	// Prewarm + the 60s maintain loop run until ctx is canceled (shutdown).
	p.Start(ctx)

	// Egress probing: report the country/IP the direct outbound path appears
	// to come from (ban-avoidance diagnostics; the upstream hard-blocks
	// proxy/VPN egress, so the direct route is the only outbound path).
	// The official CLI never talks to cloudflare.com, so by default only ONE
	// probe runs at startup (fills the cache + risk-engine geo feed) — the
	// periodic loop is opt-in via EGRESS_PROBE_ENABLED (issue #123). When
	// enabled, the loop jitters its interval and routes through the same
	// utls stealth dialer as API traffic so no Go-default TLS fingerprint
	// ever reaches the Cloudflare edge.
	egressCache := egress.NewCache()
	if paths := egressPaths(cfg); len(paths) > 0 {
		if cfg.EgressProbeEnabled {
			go egress.RunLoop(ctx, logger, egressCache, paths, egress.ProbeTimeout, egress.DefaultTTL)
			logger.Info("egress probes started (periodic, jittered)", "paths", len(paths))
		} else {
			// One-shot startup probe: fills the region readout and the risk
			// engine's geo feed without the 10-minute clockwork signature.
			go egress.ProbeOnce(ctx, logger, egressCache, paths, egress.ProbeTimeout)
			logger.Info("egress probe scheduled once at startup", "paths", len(paths))
		}
	}

	// Issue #62: the dashboard login wizard drives the same headless OAuth
	// flow as the CLI against the proxy's own transport/stealth wiring; the
	// token it yields is added to the pool + .env (nil disables the wizard).
	loginClient, _ := upstream.NewForAuth(&cfg)
	serverOpts := []server.Option{server.WithLoginClient(loginClient)}
	// Issue #50b: release update indicator — the dashboard badge compares
	// the running version against the latest GitHub release (6h cache).
	serverOpts = append(serverOpts, server.WithVersion(version, updatecheck.New(updatecheck.DefaultRepo, nil)))

	srv := server.New(&cfg, p, reg, logger, logringHandler, *configPath, serverOpts...)
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
	if cfg.UserID != "" {
		logger.Info("acting user id set — x-freebuff-acting-user-id will be sent on chat calls", "user_id", cfg.UserID)
	}
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

// modeFlagsExclusiveWarning returns the warning printed when 2+ of the
// mutually-exclusive mode flags (-doctor/-update/-setup/-test-token/
// -install-service/-uninstall-service/-service-status) are set; "" when at
// most one is set (only the first flag then runs).
func modeFlagsExclusiveWarning(doctor, update, setup, testToken, installService, uninstallService, serviceStatus bool) string {
	n := 0
	for _, set := range []bool{doctor, update, setup, testToken, installService, uninstallService, serviceStatus} {
		if set {
			n++
		}
	}
	if n <= 1 {
		return ""
	}
	return "freebuff-proxy: warning: -doctor, -update, -setup, -test-token, -install-service, -uninstall-service and -service-status are mutually exclusive; only the first will run"
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

// egressPaths returns the outbound probe paths: the direct connection only
// (proxy routes were removed — the upstream hard-blocks proxy/VPN egress,
// so any proxied path is pure ban risk). When TLS_FINGERPRINT is
// configured, the path also carries the same utls stealth dialer used for
// API traffic, so even a probe request never presents Go's default TLS
// fingerprint to the Cloudflare edge (issue #123). The risk engine's geo
// feed and the doctor's "Egress region" row read results back from the
// shared cache.
func egressPaths(cfg config.Config) []egress.Path {
	p := egress.Path{Key: "direct", Dialer: egress.DirectDialer(egress.ProbeTimeout)}
	if dialTLS := egress.StealthDialer(cfg.TLSFingerprint); dialTLS != nil {
		p.DialTLS = dialTLS
	}
	return []egress.Path{p}
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
