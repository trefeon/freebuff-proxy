package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWindowsTaskCreateArgs pins the schtasks /create invocation: per-user
// on-logon task, non-elevated, the /tr wrapper cds into the executable's
// directory so ./.env resolves (matching start-proxy.cmd), /f overwrite.
func TestWindowsTaskCreateArgs(t *testing.T) {
	args := windowsTaskCreateArgs(`C:\tools\freebuff-proxy.exe`, `C:\tools`)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"/create",
		"/tn freebuff-proxy",
		"/sc onlogon",
		"/rl limited",
		"/f",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("windowsTaskCreateArgs missing %q in %q", want, joined)
		}
	}
	if !strings.Contains(joined, `cmd.exe /c cd /d "C:\tools" && "C:\tools\freebuff-proxy.exe"`) {
		t.Errorf("windowsTaskCreateArgs /tr wrapper wrong: %q", joined)
	}
}

// TestWindowsTaskStatusParsing pins the schtasks /v /fo LIST parser: a
// Running task registers+activates, a Ready task registers without
// activating, and output without the task registers nothing.
func TestWindowsTaskStatusParsing(t *testing.T) {
	cases := []struct {
		name           string
		out            string
		wantRegistered bool
		wantActive     bool
	}{
		{"running", "TaskName:   freebuff-proxy\nStatus:     Running\nTask To Run: cmd.exe ...\n", true, true},
		{"ready", "TaskName:   freebuff-proxy\nStatus:     Ready\n", true, false},
		{"disabled", "Status: Disabled", true, false},
		{"missing", "ERROR: The system cannot find the file specified.", false, false},
		{"empty", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registered, active := parseWindowsTaskStatus(tc.out)
			if registered != tc.wantRegistered || active != tc.wantActive {
				t.Errorf("parseWindowsTaskStatus(%q) = (%v,%v), want (%v,%v)",
					tc.out, registered, active, tc.wantRegistered, tc.wantActive)
			}
		})
	}
}

// TestSystemdUserUnit pins the unit content: WorkingDirectory and ExecStart
// point at the executable's directory and binary, and the unit is restartable
// and enable-able (Restart + WantedBy present).
func TestSystemdUserUnit(t *testing.T) {
	unit := systemdUserUnit("/opt/freebuff-proxy/freebuff-proxy", "/opt/freebuff-proxy")
	for _, want := range []string{
		"WorkingDirectory=/opt/freebuff-proxy",
		"ExecStart=/opt/freebuff-proxy/freebuff-proxy",
		"Restart=on-failure",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("systemdUserUnit missing %q in:\n%s", want, unit)
		}
	}
}

// TestSystemdActiveParsing pins the systemctl is-active parser: only the
// literal "active" state counts as running.
func TestSystemdActiveParsing(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"active", true},
		{"inactive", false},
		{"failed", false},
		{"activating", false},
		{"active\n", true},
	}
	for _, tc := range cases {
		if got := parseSystemdActive(tc.out); got != tc.want {
			t.Errorf("parseSystemdActive(%q) = %v, want %v", tc.out, got, tc.want)
		}
	}
}

// TestLaunchdPlist pins the LaunchAgent content: label, ProgramArguments
// pointing at the binary, WorkingDirectory so ./.env resolves, and
// RunAtLoad+KeepAlive for autostart/respawn.
func TestLaunchdPlist(t *testing.T) {
	plist := launchdPlist("/usr/local/bin/freebuff-proxy", "/usr/local/bin")
	for _, want := range []string{
		"com.freebuff-proxy",
		"/usr/local/bin/freebuff-proxy",
		"WorkingDirectory",
		"/usr/local/bin",
		"RunAtLoad",
		"KeepAlive",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("launchdPlist missing %q in:\n%s", want, plist)
		}
	}
}

// TestLaunchctlListParsing pins the launchctl list parser: the row with the
// label and a numeric PID column means loaded; "-" PID or absent label does
// not.
func TestLaunchctlListParsing(t *testing.T) {
	const label = "com.freebuff-proxy"
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{"loaded", "PID\tStatus\tLabel\n1234\t0\tcom.freebuff-proxy\n", true},
		{"loaded no header", "1234\t0\tcom.freebuff-proxy\n", true},
		{"unloaded dash", "-\t0\tcom.freebuff-proxy\n", false},
		{"absent", "PID\tStatus\tLabel\n1234\t0\tcom.other\n", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseLaunchctlList(tc.out, label); got != tc.want {
				t.Errorf("parseLaunchctlList(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

// repoRoot walks up from the package dir to the module root (go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test working directory")
		}
		dir = parent
	}
}

// TestCommittedServiceUnitsMatchBuilders pins the committed service unit
// samples (scripts/) to the builders in this package (issue #298). The
// committed units are versioned samples; this test asserts the field parity
// (cwd, binary path, log destinations) that MUST hold:
//
//   - launchdPlist's contract (WorkingDirectory, ProgramArguments, /tmp log
//     paths, label) must equal scripts/com.freebuff-proxy.plist for the
//     sample's binary path, so a manually installed LaunchAgent resolves
//     ./.env next to the binary instead of starting with cwd=/" (the config
//     "vanishes" trap).
//   - systemdUserUnit and scripts/freebuff-proxy.service are intentionally
//     DIFFERENT platform models and this test pins that divergence as
//     deliberate: the builder is the --user unit (exe-dir cwd, WantedBy=
//     default.target); the committed sample is the system unit (/var/lib/
//     freebuff-proxy, dedicated system user, WantedBy=multi-user.target).
func TestCommittedServiceUnitsMatchBuilders(t *testing.T) {
	root := repoRoot(t)

	// --- launchd: exact parity with the builder ---------------------------
	bin := "/usr/local/bin/freebuff-proxy"
	dir := "/usr/local/bin"
	plistPath := filepath.Join(root, "scripts", "com.freebuff-proxy.plist")
	want := launchdPlist(bin, dir)
	got, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("read %s: %v", plistPath, err)
	}
	if !strings.EqualFold(strings.TrimRight(string(got), "\r\n"), strings.TrimRight(want, "\r\n")) {
		t.Errorf("committed %s does not match launchdPlist(%q, %q).\nwant:\n%s\ngot:\n%s",
			plistPath, bin, dir, want, got)
	}
	for _, key := range []string{"<key>WorkingDirectory</key>", "<string>/usr/local/bin</string>", "<key>StandardOutPath</key>", "<key>StandardErrorPath</key>"} {
		if !strings.Contains(string(got), key) {
			t.Errorf("committed plist missing %s", key)
		}
	}

	// --- systemd: intentional divergence pinned ---------------------------
	sysUnit := readFile(t, filepath.Join(root, "scripts", "freebuff-proxy.service"))
	for _, want := range []string{
		"User=freebuff-proxy",
		"Group=freebuff-proxy",
		"WorkingDirectory=/var/lib/freebuff-proxy",
		"EnvironmentFile=/etc/freebuff-proxy/env",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(sysUnit, want) {
			t.Errorf("committed freebuff-proxy.service missing %q (system-unit marker)", want)
		}
	}

	userUnit := systemdUserUnit(bin, dir)
	for _, want := range []string{
		"WantedBy=default.target", // --user unit, not multi-user
		"WorkingDirectory=" + dir,
		"ExecStart=" + bin,
	} {
		if !strings.Contains(userUnit, want) {
			t.Errorf("systemdUserUnit missing %q (user-unit marker)", want)
		}
	}
	if strings.Contains(userUnit, "User=freebuff-proxy") {
		t.Errorf("systemdUserUnit must not set a dedicated system user (that is the system unit's role)")
	}
	// Both share the restart + description contract.
	for _, shared := range []string{"Restart=on-failure", "After=network-online.target", "Description=FreeBuff Proxy Bridge"} {
		if !strings.Contains(sysUnit, shared) {
			t.Errorf("committed system unit missing shared %q", shared)
		}
		if !strings.Contains(userUnit, shared) {
			t.Errorf("systemdUserUnit missing shared %q", shared)
		}
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}
