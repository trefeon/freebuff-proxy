// Mode helpers shared by the serve path and the main flag dispatcher:
// signal set, console-hold, log-level resolution, exe-adjacent .env warning,
// admin cleartext warning, and the per-mode exclusivity warning.
//
// The per-mode logic lives in sibling subpackages (setup, update, service,
// doctor, port, refreshtoken, validate); main.go is a thin flag parser that
// dispatches to whichever mode is selected and maps the returned exit code to
// os.Exit.
package cli

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
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
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/registry"
	history "freebuff-proxy/backend/internal/store"
	"freebuff-proxy/backend/internal/telemetry"
)

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

// openAPIWarning returns the startup warning for a pooled deployment whose
// /v1 surface is reachable without client credentials: AUTH_TOKENS set, no
// API_KEYS, non-loopback LISTEN_ADDR. Empty when there is nothing to warn
// about (bridge mode, API_KEYS set, or loopback-only listen).
func openAPIWarning(authTokens, apiKeys []string, listenAddr string) string {
	if len(authTokens) == 0 || len(apiKeys) > 0 || listenIsLoopback(listenAddr) {
		return ""
	}
	return "AUTH_TOKENS is set but API_KEYS is empty and LISTEN_ADDR binds a non-loopback interface — the /v1 API and the token pool are open to the network. Set API_KEYS or bind LISTEN_ADDR to a loopback address (e.g. 127.0.0.1:3457)"
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
// LOG_LEVEL config wins, -v → debug, a dev build (version "dev", i.e. no
// ldflags version stamp) defaults to debug so anomalies are analyzable
// without flags, else info. An unparseable LOG_LEVEL silently falls back
// to info (ParseLevel returns level 0, which is Info).
func resolveLogLevel(cfgLogLevel string, verbose bool, version string) slog.Level {
	if cfgLogLevel != "" {
		if lv, ok := telemetry.ParseLevel(cfgLogLevel); ok {
			return lv
		}
		return slog.LevelInfo
	}
	if verbose {
		return slog.LevelDebug
	}
	if version == "dev" {
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

// poolHistorySink adapts pool maturity events to the history store. It runs
// on pool goroutines (maintain tick, admin handlers): single-row inserts,
// never calls back into the pool, and no-ops without a store.
type poolHistorySink struct {
	st *history.Store
}

func (s *poolHistorySink) RecordMaturity(e pool.MaturityHistoryEvent) {
	if s == nil || s.st == nil {
		return
	}
	if err := s.st.RecordMaturity(history.MaturityEvent{
		TS:       e.TS,
		TokenIdx: e.TokenIdx,
		Kind:     e.Kind,
		Detail:   e.Detail,
	}); err != nil {
		slog.Warn("history maturity record failed", "err", err)
	}
}
