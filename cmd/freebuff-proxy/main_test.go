package main

import (
	"flag"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/egress"
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

// TestWindowsUpdateScriptASCIIOnlyWithRetry guards the .bat helper template:
// cmd reads batch files with the console codepage, so the whole script must
// be ASCII (paths enter only as %~dp0 + ASCII basenames), and the swap must
// retry the move so a brief AV/Defender lock does not fail the update.
func TestWindowsUpdateScriptASCIIOnlyWithRetry(t *testing.T) {
	// Non-ASCII path (CJK user directory): embedding it verbatim would be
	// mangled by cmd depending on the console codepage.
	exe := `C:\Users\张三\freebuff-proxy.exe`
	tmp := `C:\Users\张三\freebuff-proxy.exe.tmp-123`
	script := windowsUpdateScript(exe, tmp, 4242)

	for _, want := range []string{
		`set "TARGET_PID=4242"`,
		"%~dp0",
		`:retry`,
		`set /a tries=0`,
		`move /y "%TEMP_FILE%" "%EXE_FILE%"`,
		"timeout /t 2 /nobreak",
		`echo OK>`,
		"endlocal",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("helper script missing %q; script:\n%s", want, script)
		}
	}
	// The whole .bat must be ASCII: no raw non-ASCII path bytes.
	for _, r := range script {
		if r > 127 {
			t.Errorf("helper script contains non-ASCII rune %q; script:\n%s", r, script)
		}
	}
	if strings.Contains(script, "张三") {
		t.Error("helper script embeds the raw non-ASCII path")
	}
	// Basenames must enter the script ASCII-only (winBase splits on both
	// separators, so this holds on any build host — filepath.Base would not
	// split backslashes on Linux).
	if !strings.Contains(script, winBase(exe)) || !strings.Contains(script, winBase(tmp)) {
		t.Error("helper script does not carry the ASCII file basenames")
	}
	// Sprintf escaping must collapse %% -> % (batch variable references).
	if strings.Contains(script, "%%") {
		t.Errorf("helper script contains unescaped %% (Sprintf escaping not applied); script:\n%s", script)
	}
}

// TestShutdownSignals guards the graceful-drain notify set: os.Interrupt and
// SIGTERM must always be registered. Go has no syscall.SIGBREAK constant on
// any platform — on Windows the runtime delivers both Ctrl+C and Ctrl+Break
// as os.Interrupt (runtime/os_windows.go ctrlHandler) — so the Ctrl+Break
// drain behavior itself is pinned by TestCtrlBreakDrainsGracefully
// (main_windows_test.go).
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

// TestEgressCacheGetSet guards the per-egress result cache: Set stores the
// latest result, Get returns it, keys stay independent, and missing keys
// report absent.
func TestEgressCacheGetSet(t *testing.T) {
	c := egress.NewCacheWithTTL(time.Minute)
	if _, ok := c.Get("direct"); ok {
		t.Fatal("empty cache returned a result for direct")
	}
	want := egress.Result{IP: "1.2.3.4", Country: "US"}
	c.Set("direct", want)
	got, ok := c.Get("direct")
	if !ok {
		t.Fatal("cached direct result not found")
	}
	if got != want {
		t.Errorf("Get = %+v, want %+v", got, want)
	}
	// Overwrite replaces the previous result.
	latest := egress.Result{IP: "5.6.7.8", Country: "DE"}
	c.Set("direct", latest)
	if got, _ := c.Get("direct"); got != latest {
		t.Errorf("Get after overwrite = %+v, want %+v", got, latest)
	}
	// Keys are independent.
	if _, ok := c.Get("proxy-0"); ok {
		t.Error("proxy-0 returned a result that was never set")
	}
}

// TestEgressCacheTTL guards the freshness window: an entry must not be
// returned after its TTL elapses, and a re-Set refreshes the timestamp.
func TestEgressCacheTTL(t *testing.T) {
	c := egress.NewCacheWithTTL(50 * time.Millisecond)
	c.Set("direct", egress.Result{IP: "1.2.3.4", Country: "US"})
	if _, ok := c.Get("direct"); !ok {
		t.Fatal("fresh entry not returned")
	}
	time.Sleep(80 * time.Millisecond)
	if _, ok := c.Get("direct"); ok {
		t.Error("expired entry still returned")
	}
	c.Set("direct", egress.Result{IP: "1.2.3.4", Country: "US"})
	if _, ok := c.Get("direct"); !ok {
		t.Error("re-Set entry not returned")
	}
}

// TestEgressPaths pins the probe-path construction (the cmd package's
// highest-value pure function): the direct connection is the only outbound
// route (proxy routes were removed — the upstream hard-blocks proxy egress),
// and the utls stealth dialer rides along when TLS_FINGERPRINT is
// configured so the probe never presents Go's default TLS fingerprint to
// the Cloudflare edge (issue #123).
func TestEgressPaths(t *testing.T) {
	paths := egressPaths(config.Config{})
	if len(paths) != 1 {
		t.Fatalf("paths = %d, want 1 (direct only)", len(paths))
	}
	if paths[0].Key != "direct" {
		t.Errorf("paths[0].Key = %q, want direct", paths[0].Key)
	}
	if paths[0].Dialer == nil {
		t.Error("paths[0].Dialer is nil, want the direct dialer")
	}
	if paths[0].DialTLS != nil {
		t.Error("paths[0].DialTLS set without a TLS_FINGERPRINT, want nil (plain Go TLS)")
	}

	// With a fingerprint, the path carries the utls stealth dialer.
	paths = egressPaths(config.Config{TLSFingerprint: "chrome126"})
	if paths[0].DialTLS == nil {
		t.Error("paths[0].DialTLS nil with TLS_FINGERPRINT=chrome126, want the stealth dialer")
	}

	// auto is a valid profile name too (SafeMode's default).
	if paths = egressPaths(config.Config{TLSFingerprint: "auto"}); paths[0].DialTLS == nil {
		t.Error("paths[0].DialTLS nil with TLS_FINGERPRINT=auto, want the stealth dialer")
	}

	// An unknown fingerprint is not a panic: it falls back to plain TLS.
	paths = egressPaths(config.Config{TLSFingerprint: "nope"})
	if paths[0].DialTLS != nil {
		t.Error("paths[0].DialTLS set for unknown fingerprint, want nil fallback")
	}
}

// TestVersionFlagPrintsVersion re-executes the test binary with -version
// (main() os.Exit's, so it cannot run in-process) and pins the output:
// "freebuff-proxy <version>" on stdout, exit 0.
func TestVersionFlagPrintsVersion(t *testing.T) {
	if os.Getenv("GO_WANT_VERSION_HELPER") == "1" {
		// Re-executed: the test framework already consumed -test.* flags on
		// the global flag set, so swap in a fresh set before running main.
		flag.CommandLine = flag.NewFlagSet("freebuff-proxy", flag.ExitOnError)
		os.Args = []string{"freebuff-proxy", "-version"}
		main()
		return // unreachable: main os.Exit(0)s
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestVersionFlagPrintsVersion$")
	cmd.Env = append(os.Environ(), "GO_WANT_VERSION_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper exited with error: %v\n%s", err, out)
	}
	if want := "freebuff-proxy " + version; !strings.Contains(string(out), want) {
		t.Errorf("output %q missing %q", out, want)
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
			got := modeFlagsExclusiveWarning(tc.doctor, tc.update, tc.setup, tc.testToken, tc.installService, tc.uninstallService, tc.serviceStatus)
			if tc.want == "" {
				if got != "" {
					t.Errorf("modeFlagsExclusiveWarning = %q, want empty", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) || !strings.HasPrefix(got, "freebuff-proxy: warning:") {
				t.Errorf("modeFlagsExclusiveWarning = %q, want warning containing %q", got, tc.want)
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
