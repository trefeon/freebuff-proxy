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

// printGroupedHelp renders --help dashboard-first: serve flags up top, then
// the headless and bootstrap flags as advanced. Flag names, defaults, and
// exit codes are unchanged; only the grouping and the dashboard pointers are
// new (issue #359, docs plus help strings only).
func printGroupedHelp() {
	fmt.Fprintln(os.Stderr, "Usage: freebuff-proxy [flags]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Daily management lives in the dashboard (/admin). The flags below stay fully")
	fmt.Fprintln(os.Stderr, "working as the headless and bootstrap path; nothing was removed or renamed.")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Serve (default, no flag runs the proxy):")
	printHelpFlag("config")
	printHelpFlag("v")
	printHelpFlag("version")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Advanced (headless-only; dashboard twin noted per flag):")
	printHelpFlag("doctor")
	printHelpFlag("test-token")
	printHelpFlag("validate-tokens")
	printHelpFlag("refresh-token")
	printHelpFlag("setup")
	printHelpFlag("yes")
	printHelpFlag("update")
	printHelpFlag("install-service")
	printHelpFlag("uninstall-service")
	printHelpFlag("service-status")
}

// printHelpFlag prints one registered flag in the same shape as the standard
// flag package (placeholder plus usage line). The usage strings carry the
// dashboard twin, so the grouping here only orders them.
func printHelpFlag(name string) {
	f := flag.Lookup(name)
	if f == nil {
		return
	}
	placeholder, usage := flag.UnquoteUsage(f)
	if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
		placeholder = ""
	}
	if f.Name == "refresh-token" {
		usage += fmt.Sprintf(" (default %s)", f.DefValue)
	}
	if placeholder != "" {
		fmt.Fprintf(os.Stderr, "  -%s %s\n    \t%s\n", f.Name, placeholder, usage)
		return
	}
	fmt.Fprintf(os.Stderr, "  -%s\n    \t%s\n", f.Name, usage)
}

func main() {
	configPath := flag.String("config", "", "path to an optional JSON config file (keys mirror env names). Dashboard: Settings page plus raw .env editor; apply with POST /admin/reload")
	verbose := flag.Bool("v", false, "verbose (debug) logging. Dashboard: Logs viewer plus the Settings log level")
	showVersion := flag.Bool("version", false, "print version and exit. Dashboard: Overview status line shows the same version")
	showDoctor := flag.Bool("doctor", false, "run environment and configuration diagnostics. Dashboard: POST /admin/diag covers the same checks on a running server")
	showUpdate := flag.Bool("update", false, "check for and download the latest release update. Dashboard: Overview update badge shows the release link plus restart; the dashboard never swaps the binary")
	showSetup := flag.Bool("setup", false, "run interactive client configuration helper (writes client files). Dashboard: Setup page shows copy blocks only, no file writes")
	testToken := flag.Bool("test-token", false, "probe the first configured token with a zero-cost GET probe (no session consumed) and exit 0/1. Dashboard: Tokens page per-token Test plus POST /admin/tokens/test-all on a running server")
	validateTokens := &tokenListFlag{}
	flag.Var(validateTokens, "validate-tokens", "validate every configured token with non-mutating upstream probes, print a health report, and exit 0 (healthy) / 1 (banned, invalid, or disposable mailbox) / 2 (config error); a comma-separated list overrides AUTH_TOKENS (-validate-tokens=tok1,tok2). Dashboard: POST /admin/tokens/test-all on a running server")
	installService := flag.Bool("install-service", false, "register the current binary as a background service and start it (Task Scheduler / systemd --user / launchd). No dashboard twin: a browser tab cannot register OS services")
	uninstallService := flag.Bool("uninstall-service", false, "stop and unregister the background service. No dashboard twin: a browser tab cannot remove OS services")
	serviceStatus := flag.Bool("service-status", false, "check whether the background service is registered and running (exit 0 registered, 1 not). No dashboard twin: headless-only check for scripts")
	autoYes := flag.Bool("yes", false, "auto-confirm prompts during setup. Headless-only modifier for -setup and -refresh-token")
	refreshToken := flag.Int("refresh-token", -1, "re-authenticate token #N in .env via the headless login flow and exit (interactive: start, print login URL, poll; with -yes and GITHUB_USER/GITHUB_PASSWORD/GITHUB_TOTP set: protocol login). Dashboard: Tokens page login wizard adds a pool token; it does not re-auth slot N in place")
	flag.Usage = printGroupedHelp
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
