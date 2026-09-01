// Package service implements -install-service / -uninstall-service /
// -service-status: register the current binary as a background service so it
// starts automatically. Every builder is a pure function (unit/plist/task args
// + status parsers) so the wiring is unit-testable on any platform; the exec
// wrappers only run on the host platform when the flag is actually used.
//
// Issue #49: desktop users start the proxy by double-clicking start-proxy.*,
// which leaves a console window open and dies on window close or reboot.
// These flags register the current binary as a background service so it
// starts automatically:
//
//   - Windows: a per-user Task Scheduler task (schtasks /sc onlogon) — no
//     admin rights needed, unlike sc.exe create. The task wraps the binary
//     in `cmd /c cd /d <dir> && <bin>` so ./.env resolves next to the
//     executable, matching start-proxy.cmd.
//   - Linux: a systemd --user unit in ~/.config/systemd/user, started with
//     systemctl --user enable --now (no sudo; the unit sets WorkingDirectory
//     to the executable's directory).
//   - macOS: a launchd LaunchAgent in ~/Library/LaunchAgents with the
//     WorkingDirectory key set, loaded with launchctl load -w.
package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const serviceTaskName = "freebuff-proxy"

// serviceWorkDir returns the directory the registered service runs from: the
// executable's directory, so ./.env resolves next to the binary (mirrors
// start-proxy.sh/.cmd). Falls back to the current working directory when the
// executable path is unavailable.
func serviceWorkDir() string {
	if exe, err := os.Executable(); err == nil {
		if dir := filepath.Dir(exe); dir != "" {
			return dir
		}
	}
	cwd, _ := os.Getwd()
	return cwd
}

// serviceBinPath returns the absolute path of the running binary, or "" when
// it cannot be determined (caller reports the error).
func serviceBinPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return "", err
	}
	return abs, nil
}

// --- Windows (Task Scheduler) ----------------------------------------------

// windowsTaskCreateArgs builds the schtasks /create invocation that
// registers a per-user on-logon task running the proxy from its own
// directory. The /tr value wraps the binary in cmd /c cd so ./.env is found
// next to the executable; Go's Windows argv quoting turns the value into
// schtasks' expected \"...\" form. /f overwrites an existing task; /rl
// limited keeps the task non-elevated.
func windowsTaskCreateArgs(bin, dir string) []string {
	wrap := fmt.Sprintf("cmd.exe /c cd /d \"%s\" && \"%s\"", dir, bin)
	return []string{
		"/create", "/tn", serviceTaskName,
		"/tr", wrap,
		"/sc", "onlogon",
		"/rl", "limited",
		"/f",
	}
}

// windowsTaskDeleteArgs builds the schtasks /delete invocation.
func windowsTaskDeleteArgs() []string {
	return []string{"/delete", "/tn", serviceTaskName, "/f"}
}

// windowsTaskQueryArgs builds the schtasks /query invocation that reports
// registration and current status as machine-readable LIST output.
func windowsTaskQueryArgs() []string {
	return []string{"/query", "/tn", serviceTaskName, "/v", "/fo", "LIST"}
}

// parseWindowsTaskStatus parses schtasks /query /v /fo LIST output into
// (registered, active). The output contains one "Status:" line per task
// ("Running", "Ready", "Disabled", ...); absence of the task yields neither.
func parseWindowsTaskStatus(out string) (registered, active bool) {
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Status:") {
			continue
		}
		registered = true
		status := strings.TrimSpace(strings.TrimPrefix(line, "Status:"))
		active = strings.EqualFold(status, "Running")
	}
	return registered, active
}

// --- Linux (systemd --user) -------------------------------------------------

// systemdUserUnitPath is where the per-user unit is written.
func systemdUserUnitPath() string {
	configDir, err := os.UserConfigDir()
	if err != nil || configDir == "" {
		configDir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(configDir, "systemd", "user", serviceTaskName+".service")
}

// systemdUserUnit renders the systemd --user unit: WorkingDirectory set to
// the executable's directory so ./.env resolves, Restart=on-failure so the
// proxy comes back after crashes, WantedBy=default.target for enable --now.
// This is the per-user variant; scripts/freebuff-proxy.service is the system
// unit (dedicated user, /var/lib/freebuff-proxy) and is intentionally
// different (see TestCommittedServiceUnitsMatchBuilders).
func systemdUserUnit(bin, dir string) string {
	return fmt.Sprintf(`[Unit]
Description=FreeBuff Proxy Bridge
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=%s
ExecStart=%s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, dir, bin)
}

// --- macOS (launchd LaunchAgent) -------------------------------------------

// launchdPlistPath is where the per-user LaunchAgent is written.
func launchdPlistPath() string {
	return filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", "com."+serviceTaskName+".plist")
}

// launchdPlist renders the LaunchAgent plist: RunAtLoad + KeepAlive for
// autostart and crash respawn, WorkingDirectory so ./.env resolves, and
// stdout/stderr captured to /tmp logs (mirrors scripts/com.freebuff-proxy.plist).
func launchdPlist(bin, dir string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
    </array>
    <key>WorkingDirectory</key>
    <string>%s</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/%s.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/%s.err</string>
</dict>
</plist>
`, serviceTaskName, bin, dir, serviceTaskName, serviceTaskName)
}

// --- status parsers ---------------------------------------------------------

// parseSystemdActive parses `systemctl --user is-active` output: "active"
// means running, "inactive"/"failed"/"activating" are not.
func parseSystemdActive(out string) bool {
	return strings.TrimSpace(out) == "active"
}

// parseLaunchctlList parses `launchctl list` output for a loaded label: each
// row is "PID\tStatus\tLabel"; a loaded agent has a non-empty PID column.
func parseLaunchctlList(out, label string) bool {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[2] == label && fields[0] != "-" {
			return true
		}
	}
	return false
}

// --- exec helpers -----------------------------------------------------------

// runCmd runs cmd and returns its output. Exit status is not an error here:
// callers interpret nonzero exits (e.g. schtasks query on a missing task).
func runCmd(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// writeFile0600 writes data to path, creating parent dirs, with owner-only
// permissions (the unit contains no secrets, but the pattern matches .env).
func writeFile0600(path, data string) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return os.WriteFile(path, []byte(data), 0o600)
}

// --- flag entrypoints -------------------------------------------------------

// Install registers the current binary as a background service and starts
// it. Prints a human result and exits 0/1.
func Install() {
	bin, err := serviceBinPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "freebuff-proxy: -install-service: cannot locate executable: %v\n", err)
		os.Exit(1)
	}
	dir := serviceWorkDir()

	switch runtime.GOOS {
	case "windows":
		if out, err := runCmd("schtasks", windowsTaskCreateArgs(bin, dir)...); err != nil {
			fmt.Fprintf(os.Stderr, "freebuff-proxy: -install-service: schtasks failed: %v\n%s\n", err, out)
			os.Exit(1)
		}
		fmt.Printf("Installed: Task Scheduler task %q starts %s at logon (auto-start on boot/restart).\n", serviceTaskName, bin)
	case "linux":
		unitPath := systemdUserUnitPath()
		if err := writeFile0600(unitPath, systemdUserUnit(bin, dir)); err != nil {
			fmt.Fprintf(os.Stderr, "freebuff-proxy: -install-service: write unit %s: %v\n", unitPath, err)
			os.Exit(1)
		}
		if out, err := runCmd("systemctl", "--user", "daemon-reload"); err != nil {
			fmt.Fprintf(os.Stderr, "freebuff-proxy: -install-service: systemctl daemon-reload: %v\n%s\n", err, out)
			os.Exit(1)
		}
		if out, err := runCmd("systemctl", "--user", "enable", "--now", serviceTaskName); err != nil {
			fmt.Fprintf(os.Stderr, "freebuff-proxy: -install-service: systemctl enable --now: %v\n%s\n", err, out)
			os.Exit(1)
		}
		fmt.Printf("Installed: systemd user unit %s started %s (enable --now).\n", unitPath, bin)
	case "darwin":
		plistPath := launchdPlistPath()
		if err := writeFile0600(plistPath, launchdPlist(bin, dir)); err != nil {
			fmt.Fprintf(os.Stderr, "freebuff-proxy: -install-service: write plist %s: %v\n", plistPath, err)
			os.Exit(1)
		}
		if out, err := runCmd("launchctl", "load", "-w", plistPath); err != nil {
			fmt.Fprintf(os.Stderr, "freebuff-proxy: -install-service: launchctl load: %v\n%s\n", err, out)
			os.Exit(1)
		}
		fmt.Printf("Installed: LaunchAgent %s loaded %s (RunAtLoad + KeepAlive).\n", plistPath, bin)
	default:
		fmt.Fprintf(os.Stderr, "freebuff-proxy: -install-service: unsupported platform %q (use the scripts/ start-proxy.* launchers)\n", runtime.GOOS)
		os.Exit(1)
	}
	os.Exit(0)
}

// Uninstall stops and unregisters the service. Missing registration
// is not an error (idempotent uninstall); the exit code stays 0.
func Uninstall() {
	switch runtime.GOOS {
	case "windows":
		if out, err := runCmd("schtasks", windowsTaskDeleteArgs()...); err != nil {
			if strings.Contains(out, "does not exist") || strings.Contains(strings.ToLower(out), "not found") {
				fmt.Printf("Uninstalled: Task Scheduler task %q was not registered (nothing to do).\n", serviceTaskName)
				os.Exit(0)
			}
			fmt.Fprintf(os.Stderr, "freebuff-proxy: -uninstall-service: schtasks failed: %v\n%s\n", err, out)
			os.Exit(1)
		}
		fmt.Printf("Uninstalled: Task Scheduler task %q deleted.\n", serviceTaskName)
	case "linux":
		unitPath := systemdUserUnitPath()
		// disable --now stops and disables; a missing unit is not an error.
		if out, err := runCmd("systemctl", "--user", "disable", "--now", serviceTaskName); err != nil && !strings.Contains(out, "not loaded") && !strings.Contains(out, "not found") {
			fmt.Fprintf(os.Stderr, "freebuff-proxy: -uninstall-service: systemctl disable --now: %v\n%s\n", err, out)
			os.Exit(1)
		}
		_ = os.Remove(unitPath)
		_, _ = runCmd("systemctl", "--user", "daemon-reload")
		fmt.Printf("Uninstalled: systemd user unit %s removed.\n", unitPath)
	case "darwin":
		plistPath := launchdPlistPath()
		if _, err := runCmd("launchctl", "unload", "-w", plistPath); err != nil && !strings.Contains(strings.ToLower(err.Error()), "no such") {
			fmt.Fprintf(os.Stderr, "freebuff-proxy: -uninstall-service: launchctl unload: %v\n", err)
			os.Exit(1)
		}
		_ = os.Remove(plistPath)
		fmt.Printf("Uninstalled: LaunchAgent %s removed.\n", plistPath)
	default:
		fmt.Fprintf(os.Stderr, "freebuff-proxy: -uninstall-service: unsupported platform %q\n", runtime.GOOS)
		os.Exit(1)
	}
	os.Exit(0)
}

// Status reports whether the service is registered and active.
// Exits 0 when registered, 1 when not (scriptable check).
func Status() {
	registered, active := false, false
	switch runtime.GOOS {
	case "windows":
		out, _ := runCmd("schtasks", windowsTaskQueryArgs()...)
		registered, active = parseWindowsTaskStatus(out)
	case "linux":
		if _, err := os.Stat(systemdUserUnitPath()); err == nil {
			registered = true
		}
		if out, err := runCmd("systemctl", "--user", "is-active", serviceTaskName); err == nil {
			active = parseSystemdActive(out)
		}
	case "darwin":
		if _, err := os.Stat(launchdPlistPath()); err == nil {
			registered = true
		}
		if out, err := runCmd("launchctl", "list"); err == nil {
			active = parseLaunchctlList(out, "com."+serviceTaskName)
		}
	default:
		fmt.Fprintf(os.Stderr, "freebuff-proxy: -service-status: unsupported platform %q\n", runtime.GOOS)
		os.Exit(1)
	}

	switch {
	case registered && active:
		fmt.Printf("freebuff-proxy service: registered, running\n")
		os.Exit(0)
	case registered:
		fmt.Printf("freebuff-proxy service: registered, not running\n")
		os.Exit(0)
	default:
		fmt.Printf("freebuff-proxy service: not registered\n")
		os.Exit(1)
	}
}
