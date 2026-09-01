package cli

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/telemetry"
)

// TestHoldForExitIfConsolePipedStderrNoHang guards the console hold: with
// piped stderr (containers, log files, Task Scheduler, CI) holdForExitIfConsole
// must return immediately — a hang here would freeze every non-interactive
// startup error path.
func TestHoldForExitIfConsolePipedStderrNoHang(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()

	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	done := make(chan struct{})
	go func() {
		holdForExitIfConsole()
		close(done)
	}()
	select {
	case <-done:
		// Returned without waiting for input: an anonymous pipe is not a
		// character device, so the hold must be a no-op.
	case <-time.After(2 * time.Second):
		t.Fatal("holdForExitIfConsole blocked on piped stderr")
	}
}

// TestShutdownSignals guards the graceful-drain notify set: os.Interrupt and
// SIGTERM must always be registered. Go has no syscall.SIGBREAK constant on
// any platform — on Windows the runtime delivers both Ctrl+C and Ctrl+Break
// as os.Interrupt (runtime/os_windows.go ctrlHandler) — so the Ctrl+Break
// drain behavior itself is pinned by TestCtrlBreakDrainsGracefully
// (cli_windows_test.go).
func TestShutdownSignals(t *testing.T) {
	got := shutdownSignals()
	has := func(want os.Signal) bool {
		for _, s := range got {
			if s == want {
				return true
			}
		}
		return false
	}
	if !has(os.Interrupt) {
		t.Error("shutdownSignals missing os.Interrupt (covers Ctrl+C and Ctrl+Break on Windows)")
	}
	if !has(syscall.SIGTERM) {
		t.Error("shutdownSignals missing syscall.SIGTERM")
	}
}

// TestModeFlagsExclusiveWarning pins the mutually-exclusive-mode warning:
// 2+ of -doctor/-update/-setup/-test-token/-install-service/
// -uninstall-service/-service-status prints the warning (only the first
// flag then runs), at most one set prints nothing.
func TestModeFlagsExclusiveWarning(t *testing.T) {
	cases := []struct {
		name                                            string
		doctor, update, setup, testToken                bool
		installService, uninstallService, serviceStatus bool
		want                                            string
	}{
		{"none", false, false, false, false, false, false, false, ""},
		{"single", false, false, true, false, false, false, false, ""},
		{"single service", false, false, false, false, true, false, false, ""},
		{"two", true, false, true, false, false, false, false, "mutually exclusive"},
		{"three", true, true, false, true, false, false, false, "mutually exclusive"},
		{"service pair", false, false, false, false, true, true, false, "mutually exclusive"},
		{"all seven", true, true, true, true, true, true, true, "mutually exclusive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ModeFlagsExclusiveWarning(tc.doctor, tc.update, tc.setup, tc.testToken, tc.installService, tc.uninstallService, tc.serviceStatus)
			if tc.want == "" {
				if got != "" {
					t.Errorf("ModeFlagsExclusiveWarning = %q, want empty", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) || !strings.HasPrefix(got, "freebuff-proxy: warning:") {
				t.Errorf("ModeFlagsExclusiveWarning = %q, want warning containing %q", got, tc.want)
			}
		})
	}
}

// TestResolveLogLevel pins the effective log-level precedence: LOG_LEVEL
// config wins when set and parseable (even over -v), -v → debug, else
// info, and an unparseable LOG_LEVEL silently falls back to info.
func TestResolveLogLevel(t *testing.T) {
	cases := []struct {
		name     string
		logLevel string
		verbose  bool
		want     slog.Level
	}{
		{"empty not verbose", "", false, slog.LevelInfo},
		{"empty verbose", "", true, slog.LevelDebug},
		{"config wins", "warn", false, slog.LevelWarn},
		{"config beats verbose", "error", true, slog.LevelError},
		{"config case-insensitive", "DEBUG", false, slog.LevelDebug},
		{"trace level", "trace", false, telemetry.LevelTrace},
		{"trace case-insensitive", "TRACE", true, telemetry.LevelTrace},
		{"unparseable falls back to info", "bogus", true, slog.LevelInfo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveLogLevel(tc.logLevel, tc.verbose); got != tc.want {
				t.Errorf("resolveLogLevel(%q, %v) = %v, want %v", tc.logLevel, tc.verbose, got, tc.want)
			}
		})
	}
}

// TestLogLevelDisplay pins the startup-summary level rendering: trace shows
// as TRACE (not slog's "DEBUG-4"), every other level keeps slog's name.
func TestLogLevelDisplay(t *testing.T) {
	cases := []struct {
		level slog.Level
		want  string
	}{
		{telemetry.LevelTrace, "TRACE"},
		{slog.LevelDebug, "DEBUG"},
		{slog.LevelInfo, "INFO"},
		{slog.LevelWarn, "WARN"},
		{slog.LevelError, "ERROR"},
	}
	for _, tc := range cases {
		if got := logLevelDisplay(tc.level); got != tc.want {
			t.Errorf("logLevelDisplay(%v) = %q, want %q", tc.level, got, tc.want)
		}
	}
}

// TestIgnoredExeAdjacentEnv pins the exe-adjacent .env warning branch: a
// .env next to the executable is flagged ONLY when the working directory
// differs from the executable's directory — the usual reason config "seems
// to vanish" under a launcher. Same directory, a missing exe-adjacent
// .env, or empty inputs must not warn.
func TestIgnoredExeAdjacentEnv(t *testing.T) {
	dir := t.TempDir()
	exeDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(exeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(exeDir, "freebuff-proxy.exe")
	envPath := filepath.Join(exeDir, ".env")
	if err := os.WriteFile(envPath, []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("same working directory", func(t *testing.T) {
		if got := ignoredExeAdjacentEnv(exeDir, exe); got != "" {
			t.Errorf("same dir returned %q, want empty", got)
		}
	})
	t.Run("different cwd with exe-adjacent env", func(t *testing.T) {
		work := filepath.Join(dir, "work")
		if err := os.MkdirAll(work, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := ignoredExeAdjacentEnv(work, exe); got != envPath {
			t.Errorf("cross-dir returned %q, want %q", got, envPath)
		}
	})
	t.Run("no env next to exe", func(t *testing.T) {
		cleanDir := filepath.Join(dir, "clean-bin")
		if err := os.MkdirAll(cleanDir, 0o755); err != nil {
			t.Fatal(err)
		}
		work := filepath.Join(dir, "work2")
		if err := os.MkdirAll(work, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := ignoredExeAdjacentEnv(work, filepath.Join(cleanDir, "other.exe")); got != "" {
			t.Errorf("missing exe-adjacent env returned %q, want empty", got)
		}
	})
	t.Run("empty inputs", func(t *testing.T) {
		if got := ignoredExeAdjacentEnv("", exe); got != "" {
			t.Errorf("empty cwd returned %q, want empty", got)
		}
		if got := ignoredExeAdjacentEnv(exeDir, ""); got != "" {
			t.Errorf("empty exe path returned %q, want empty", got)
		}
	})
}

// TestAdminTokenCleartextWarning pins the transport-security startup warning:
// ADMIN_TOKEN set + non-loopback LISTEN_ADDR must warn (the binary has no TLS
// support, so the admin login POST and session cookie travel in cleartext);
// loopback listen or no ADMIN_TOKEN must not.
func TestAdminTokenCleartextWarning(t *testing.T) {
	cases := []struct {
		name       string
		adminToken string
		listenAddr string
		wantWarn   bool
	}{
		{"no token, loopback", "", "127.0.0.1:3457", false},
		{"no token, all interfaces", "", ":3457", false},
		{"token, loopback v4", "secret", "127.0.0.1:3457", false},
		{"token, loopback v6", "secret", "[::1]:3457", false},
		{"token, localhost", "secret", "localhost:3457", false},
		{"token, all interfaces", "secret", ":3457", true},
		{"token, wildcard v4", "secret", "0.0.0.0:3457", true},
		{"token, non-loopback IP", "secret", "192.168.1.50:3457", true},
		{"token, hostname", "secret", "proxy.example.com:3457", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := adminTokenCleartextWarning(tc.adminToken, tc.listenAddr)
			if tc.wantWarn && got == "" {
				t.Errorf("adminTokenCleartextWarning(%q, %q) = \"\", want a warning", tc.adminToken, tc.listenAddr)
			}
			if !tc.wantWarn && got != "" {
				t.Errorf("adminTokenCleartextWarning(%q, %q) = %q, want empty", tc.adminToken, tc.listenAddr, got)
			}
			if tc.wantWarn && !strings.Contains(got, "loopback") {
				t.Errorf("warning %q does not advise a loopback bind", got)
			}
		})
	}
}

// TestPrintPortInUseHintText pins the actionable port-conflict message: the
// prominent header, the "already in use" diagnosis, the platform-specific
// find-and-stop suggestion, and the restart hint must all be present. The
// process-owner line is conditional (depends on live process detection) and
// is not asserted. stderr is piped, so the console hold is a no-op.
func TestPrintPortInUseHintText(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()

	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	done := make(chan struct{})
	go func() {
		printPortInUseHint(":3457", errors.New("bind: address already in use"))
		close(done)
	}()
	// Generous budget: on Windows the port-owner detection spawns PowerShell
	// (~1-3s cold start) when netstat finds no listener for the port, and the
	// test must still complete. The piped-stderr no-op itself is instant — a
	// real regression (blocking on Enter) would hang past any budget.
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("printPortInUseHint blocked on piped stderr")
	}
	_ = w.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	out := string(data)

	for _, want := range []string{
		"freebuff-proxy: cannot listen on :3457",
		"Port 3457 is already in use by another process.",
		"To close the other app, find and stop it:",
		"Then start freebuff-proxy again.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("hint missing %q; got:\n%s", want, out)
		}
	}
	if runtime.GOOS == "windows" {
		if !strings.Contains(out, "netstat -ano | findstr :3457") || !strings.Contains(out, "taskkill /PID <pid> /F") {
			t.Errorf("hint missing Windows instructions; got:\n%s", out)
		}
	} else {
		if !strings.Contains(out, "lsof -i :3457") || !strings.Contains(out, "kill <pid>") {
			t.Errorf("hint missing unix instructions; got:\n%s", out)
		}
	}
}
