// Package port hosts the pure, testable port-conflict diagnostics used by
// the serve path's bind-failure hint: which process holds LISTEN_ADDR's port
// and what the operator should run next.
package port

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// IsPortInUse reports whether err is a bind failure because the address is
// already taken. EADDRINUSE is portable: net.Listen wraps it in
// *net.OpError → *os.SyscallError → syscall.Errno, and errors.Is unwraps.
// On Windows the bind error is WSAEADDRINUSE, which this toolchain's
// syscall.EADDRINUSE constant does not equal, so the OS message is matched
// as a fallback (same substring style as upstream's isTransient).
func IsPortInUse(err error) bool {
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "address already in use") ||
		strings.Contains(msg, "only one usage of each socket address")
}

// PortOf extracts the numeric port from a ListenAddr (":3457",
// "127.0.0.1:3457", "[::1]:3457"). Empty when unparseable.
func PortOf(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return port
}

// PortOwner returns a human label for the process listening on port, e.g.
// "PID 44420  freebuff-proxy-dash.exe". Best-effort: empty when detection
// fails or the tool is missing.
func PortOwner(port string) string {
	var pid string
	switch runtime.GOOS {
	case "windows":
		pid = windowsPortPID(port)
	case "linux", "darwin":
		pid = unixPortPID(port)
	}
	if pid == "" {
		return ""
	}
	label := "PID " + pid
	if name := processName(pid); name != "" {
		label += "  " + name
	}
	return label
}

// windowsPortPID finds the LISTENING PID for port from netstat -ano output.
// When netstat finds nothing, falls back to PowerShell's Get-NetTCPConnection
// (-State Listen is locale-independent, unlike netstat's localized state
// strings — e.g. German "ABHÖREN" — so the fallback works on non-English
// Windows where the netstat parse misses).
func windowsPortPID(port string) string {
	out, err := exec.Command("netstat", "-ano").Output()
	if err != nil {
		return ""
	}
	if pid := windowsPortPIDFromOutput(string(out), port); pid != "" {
		return pid
	}
	return windowsPortPIDFromPowerShell(port)
}

// windowsPortPIDPowerShellTimeout bounds the PowerShell fallback query: the
// diagnostic is best-effort, and a hung PowerShell must not stall the
// bind-failure hint indefinitely.
const windowsPortPIDPowerShellTimeout = 4 * time.Second

// windowsPortPIDFromPowerShell queries the LISTENING PID for port via
// PowerShell: "Get-NetTCPConnection -State Listen -LocalPort <port>". The
// -State Listen keyword is English on every locale, so it works where the
// netstat parse fails. The command forces UTF-8 console output (powershell
// 5.1 otherwise writes UTF-16LE to pipes, which would NUL-mangle the PID)
// and is bounded by windowsPortPIDPowerShellTimeout. Returns the first
// numeric PID, or "" when none is listening or the query fails.
func windowsPortPIDFromPowerShell(port string) string {
	ctx, cancel := context.WithTimeout(context.Background(), windowsPortPIDPowerShellTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command",
		"[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; (Get-NetTCPConnection -State Listen -LocalPort "+port+" -ErrorAction SilentlyContinue).OwningProcess").Output()
	if err != nil {
		return ""
	}
	return windowsPortPIDFromPowerShellOutput(string(out))
}

// windowsPortPIDFromPowerShellOutput extracts the first numeric PID from
// Get-NetTCPConnection output — one OwningProcess per line when multiple
// connections listen. Pure so the parsing is testable without PowerShell.
func windowsPortPIDFromPowerShellOutput(out string) string {
	for _, line := range strings.Split(out, "\n") {
		pid := strings.TrimSpace(line)
		if pid == "" {
			continue
		}
		// A PID is pure digits; skip any stray non-numeric output.
		numeric := true
		for _, r := range pid {
			if r < '0' || r > '9' {
				numeric = false
				break
			}
		}
		if numeric {
			return pid
		}
	}
	return ""
}

// windowsPortPIDFromOutput parses netstat -ano text: the local address field
// ends with ":PORT" and the state field is LISTENING; the last field is the
// PID. Pure so the parsing is testable.
func windowsPortPIDFromOutput(out, port string) string {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		if strings.EqualFold(f[0], "TCP") && strings.HasSuffix(f[1], ":"+port) && strings.Contains(strings.ToUpper(f[3]), "LISTEN") {
			return f[len(f)-1]
		}
	}
	return ""
}

// unixPortPID returns the first LISTENING PID for port. Tool availability
// varies by distro: lsof (most full installs), ss (iproute2), or netstat
// (busybox in minimal alpine containers) — try them in order.
func unixPortPID(port string) string {
	if pid := execFirstOutput("lsof", "-ti", ":"+port, "-sTCP:LISTEN"); pid != "" {
		return pid
	}
	if out := execFirstOutput("ss", "-ltnp", "sport = :"+port); out != "" {
		return ssPortPID(out)
	}
	return busyboxPortPID(execFirstOutput("netstat", "-ltnp"), port)
}

// ssPortPID extracts the first "pid=N" from ss -ltnp output, e.g.
// "LISTEN 0 128 0.0.0.0:3457 0.0.0.0:* users:(("proc",pid=1234,fd=5))".
// Pure so the parsing is testable without the ss binary.
func ssPortPID(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, "pid="); i >= 0 {
			rest := line[i+len("pid="):]
			if j := strings.IndexAny(rest, ",)"); j >= 0 {
				rest = rest[:j]
			}
			if rest != "" {
				return rest
			}
		}
	}
	return ""
}

// busyboxPortPID extracts the LISTENING pid for port from busybox netstat
// -ltnp output:
//
//	tcp  0  0 0.0.0.0:3457 0.0.0.0:*  LISTEN  1234/program
//
// State is field 5; the last field is "PID/program" — the name is stripped.
// Pure so the parsing is testable without the netstat binary.
func busyboxPortPID(out, port string) string {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 7 && strings.HasPrefix(f[0], "tcp") && strings.Contains(f[5], "LISTEN") && strings.HasSuffix(f[3], ":"+port) {
			if pid, _, ok := strings.Cut(f[len(f)-1], "/"); ok && pid != "" {
				return pid
			}
		}
	}
	return ""
}

// execFirstOutput runs a command and returns its stdout, or "" on any error
// (missing binary, non-zero exit).
func execFirstOutput(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// processName resolves a PID to a process name: tasklist CSV on Windows,
// ps on unix. Empty when unavailable.
func processName(pid string) string {
	if runtime.GOOS == "windows" {
		out, err := exec.Command("tasklist", "/FI", "PID eq "+pid, "/FO", "CSV", "/NH").Output()
		if err != nil {
			return ""
		}
		return taskNameFromCSV(string(out))
	}
	out, err := exec.Command("ps", "-p", pid, "-o", "comm=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// taskNameFromCSV extracts the quoted image name from a tasklist CSV line
// like "freebuff-proxy-dash.exe","44420","Console","1","50,776 K".
func taskNameFromCSV(line string) string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, `"`) {
		return ""
	}
	if i := strings.IndexByte(line[1:], '"'); i >= 0 {
		return line[1 : 1+i]
	}
	return ""
}
