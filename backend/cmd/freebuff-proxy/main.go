// Command freebuff-proxy is the FreeBuff proxy bridge entrypoint. It parses
// the CLI flags, then dispatches to whichever mode is selected: the per-mode
// logic lives in backend/internal/cli subpackages (setup/update/service/
// doctor/port/refreshtoken/validate), and the default serve mode lives in the
// cli package. Each mode preserves its own exit code; main.go stays a thin
// flag parser + os.Exit mapper.
package main

import (
	"flag"
	"fmt"
	"os"

	"freebuff-proxy/backend/internal/cli"
	"freebuff-proxy/backend/internal/cli/doctor"
	"freebuff-proxy/backend/internal/cli/refreshtoken"
	"freebuff-proxy/backend/internal/cli/service"
	"freebuff-proxy/backend/internal/cli/setup"
	"freebuff-proxy/backend/internal/cli/update"
	"freebuff-proxy/backend/internal/cli/validate"
)

// version is injected at build time by GoReleaser (-ldflags -X main.version=...).
// When building without GoReleaser it stays "dev".
var version = "dev"

// tokenListFlag is a tri-state flag value for -validate-tokens: the flag
// package parses a bare "-validate-tokens" as Set("true") (validate the
// configured tokens), "-validate-tokens=tok1,tok2" as the override list,
// and "-validate-tokens=false" as the off switch (bool-flag semantics).
type tokenListFlag struct {
	set   bool
	value string
}

func (f *tokenListFlag) String() string { return f.value }

func (f *tokenListFlag) Set(s string) error {
	f.set = true
	f.value = s
	return nil
}

// IsBoolFlag lets -validate-tokens be passed with no value, like the other
// mode flags, while Set still accepts the comma-separated override.
func (f *tokenListFlag) IsBoolFlag() bool { return true }

func main() {
	configPath := flag.String("config", "", "path to an optional JSON config file (keys mirror env names)")
	verbose := flag.Bool("v", false, "verbose (debug) logging")
	showVersion := flag.Bool("version", false, "print version and exit")
	showDoctor := flag.Bool("doctor", false, "run environment and configuration diagnostics")
	showUpdate := flag.Bool("update", false, "check for and download the latest release update")
	showSetup := flag.Bool("setup", false, "run interactive client configuration helper")
	testToken := flag.Bool("test-token", false, "probe the first configured token with a zero-cost GET probe (no session consumed) and exit 0/1")
	validateTokens := &tokenListFlag{}
	flag.Var(validateTokens, "validate-tokens", "validate every configured token with non-mutating upstream probes, print a health report, and exit 0 (healthy) / 1 (banned, invalid, or disposable mailbox) / 2 (config error); a comma-separated list overrides AUTH_TOKENS (-validate-tokens=tok1,tok2)")
	installService := flag.Bool("install-service", false, "register the current binary as a background service and start it (Task Scheduler / systemd --user / launchd)")
	uninstallService := flag.Bool("uninstall-service", false, "stop and unregister the background service")
	serviceStatus := flag.Bool("service-status", false, "check whether the background service is registered and running (exit 0 registered, 1 not)")
	autoYes := flag.Bool("yes", false, "auto-confirm prompts during setup")
	refreshToken := flag.Int("refresh-token", -1, "re-authenticate token #N in .env via the headless GitHub login flow and exit (interactive: start → print login URL → poll; with -yes and GITHUB_USER/GITHUB_PASSWORD/GITHUB_TOTP set: protocol login)")
	flag.Parse()

	if w := cli.ModeFlagsExclusiveWarning(*showDoctor, *showUpdate, *showSetup, *testToken, *installService, *uninstallService, *serviceStatus, validateTokens.set); w != "" {
		fmt.Fprintln(os.Stderr, w)
	}

	if *showVersion {
		fmt.Println("freebuff-proxy", version)
		os.Exit(0)
	}
	if *testToken {
		doctor.RunTokenTest(*configPath)
	}
	if validateTokens.set && validateTokens.value != "false" {
		// Bare -validate-tokens (flag parses it as Set("true")) probes the
		// configured AUTH_TOKENS; -validate-tokens=tok1,tok2 probes the
		// override list. Both paths run Run and exit there.
		override := ""
		if v := validateTokens.value; v != "" && v != "true" {
			override = v
		}
		validate.Run(*configPath, override)
	}
	if *refreshToken >= 0 {
		refreshtoken.Run(*configPath, *refreshToken, *autoYes)
	}
	if *showDoctor {
		doctor.Run(*configPath)
	}
	if *showUpdate {
		update.Run(version)
	}
	if *showSetup {
		setup.Run(*autoYes)
	}
	if *installService {
		service.Install()
	}
	if *uninstallService {
		service.Uninstall()
	}
	if *serviceStatus {
		service.Status()
	}

	os.Exit(cli.Serve(*configPath, *verbose, version))
}
