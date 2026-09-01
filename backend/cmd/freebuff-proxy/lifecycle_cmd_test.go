package main

import (
	"flag"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/cli/setup"
	"freebuff-proxy/backend/internal/egress"
	"freebuff-proxy/backend/internal/testutil"
)

// lifecycleSemver is the semver shape an ldflags-injected release version
// must satisfy when printed by -version.
var lifecycleSemver = regexp.MustCompile(`^v?\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$`)

// TestLifecycleSetupDoctorVersion walks the operator-facing CLI lifecycle
// entry points hermetically: -version, -setup, -doctor. Each path
// os.Exit()s (or is reached via main()), so the test re-executes the test
// binary in a helper subprocess with a controlled environment (config env
// vars unset, HOME/USERPROFILE pointed at a temp dir, no external network
// needed).
//
// The assertions document the actual, observable CLI behavior:
//
//	-version prints "freebuff-proxy <semver>" — an ldflags-injected version
//	round-trips through the real flag branch (a non-injected build prints
//	"dev").
//	-setup configures detected client tools (Continue / opencode / aider);
//	it does NOT write the proxy .env (env creation is the installer /
//	gen-token / dashboard's job, documented in docs/user-lifecycle.md).
//	-doctor loads the config, reports bridge mode + local availability, and
//	fails only on the offline upstream reachability check — a fully green
//	doctor needs a reachable upstream, which a hermetic test cannot fake on
//	Windows (Go ignores SSL_CERT_FILE for the system pool).
func TestLifecycleSetupDoctorVersion(t *testing.T) {
	switch os.Getenv("GO_WANT_LIFECYCLE_HELPER") {
	case "version":
		// Simulate goreleaser `-ldflags -X main.version=1.4.0`: an injected
		// version must pass through the real -version flag branch untouched.
		version = "1.4.0"
		flag.CommandLine = flag.NewFlagSet("freebuff-proxy", flag.ExitOnError)
		os.Args = []string{"freebuff-proxy", "-version"}
		main()
		return

	case "setup":
		home := os.Getenv("GO_WANT_LIFECYCLE_HOME")
		testutil.UnsetConfigEnv(t)
		// Isolate from the real user profile: point every home-resolution
		// path at the temp dir, and hide any real aider on PATH so client
		// detection is deterministic ("Configured 0 client tool(s)").
		_ = os.Setenv("HOME", home)
		_ = os.Setenv("USERPROFILE", home)
		_ = os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
		_ = os.Setenv("PATH", home)
		setup.Run(true)
		return

	case "doctor":
		testutil.UnsetConfigEnv(t)
		// Point the egress probe at a local trace server so the region line
		// is a deterministic success (no external network).
		probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("ip=203.0.113.7\nloc=US\n"))
		}))
		egress.ProbeURL = probe.URL
		_ = os.Setenv("AUTO_DISCOVER_TOKEN", "false")
		// LISTEN_ADDR needs a real (1-65535) port: grab a free one so the
		// doctor's port-availability check passes, then release it.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		freePort := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
		_ = os.Setenv("LISTEN_ADDR", net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort)))
		// UPSTREAM_BASE_URL on a closed loopback port: DNS resolves, TLS
		// refused → deterministic offline reachability failure.
		_ = os.Setenv("UPSTREAM_BASE_URL", "https://127.0.0.1:1/v1")
		flag.CommandLine = flag.NewFlagSet("freebuff-proxy", flag.ExitOnError)
		os.Args = []string{"freebuff-proxy", "-doctor"}
		main()
		return
	}

	t.Run("version", func(t *testing.T) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestLifecycleSetupDoctorVersion$")
		cmd.Env = append(os.Environ(), "GO_WANT_LIFECYCLE_HELPER=version")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("-version helper exited with error: %v\n%s", err, out)
		}
		got := strings.TrimSpace(string(out))
		if want := "freebuff-proxy 1.4.0"; got != want {
			t.Fatalf("-version output = %q, want %q", got, want)
		}
		fields := strings.Fields(got)
		if len(fields) != 2 {
			t.Fatalf("-version output %q: expected exactly 2 fields", got)
		}
		if !lifecycleSemver.MatchString(fields[1]) {
			t.Errorf("injected version %q is not semver-shaped", fields[1])
		}
	})

	t.Run("setup", func(t *testing.T) {
		home := t.TempDir()
		cmd := exec.Command(os.Args[0], "-test.run=^TestLifecycleSetupDoctorVersion$")
		cmd.Env = append(os.Environ(),
			"GO_WANT_LIFECYCLE_HELPER=setup",
			"GO_WANT_LIFECYCLE_HOME="+home,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("-setup helper exited with error: %v\n%s", err, out)
		}
		s := string(out)
		for _, want := range []string{
			"freebuff-proxy interactive client setup",
			"Setup complete! Configured 0 client tool(s).",
			"Base URL: http://localhost:3457/v1",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("-setup output missing %q:\n%s", want, s)
			}
		}
		// -setup configures client tools; it must NOT create the proxy .env.
		var envFound bool
		_ = filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && filepath.Base(p) == ".env" {
				envFound = true
			}
			return nil
		})
		if envFound {
			t.Error("-setup unexpectedly wrote a .env; .env creation is the installer / gen-token / dashboard's job")
		}
	})

	t.Run("doctor", func(t *testing.T) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestLifecycleSetupDoctorVersion$")
		cmd.Env = append(os.Environ(), "GO_WANT_LIFECYCLE_HELPER=doctor")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("-doctor exited 0, want non-zero (offline upstream reachability fails)\n%s", out)
		}
		s := string(out)
		for _, want := range []string{
			"freebuff-proxy doctor diagnostic tool",
			"[ok] Configuration loaded & validated successfully",
			"AUTH_TOKENS is empty (bridge mode active)",
			"[ok] Listen address 127.0.0.1:", // port is a dynamically-freed ephemeral
			"[ok] DNS lookup for 127.0.0.1 resolved",
			"[FAIL] TLS connection to 127.0.0.1:1 failed",
			"[ok] Model registry offline fallback loaded",
			"[ok] Egress region: US (203.0.113.7)",
			"Summary:",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("-doctor output missing %q:\n%s", want, s)
			}
		}
	})
}
