package port

import (
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
)

func TestIsPortInUse(t *testing.T) {
	// A bind failure surfaces as *net.OpError → *os.SyscallError → Errno,
	// exactly what errors.Is unwraps.
	real := &net.OpError{Op: "listen", Err: &os.SyscallError{Syscall: "bind", Err: syscall.EADDRINUSE}}
	if !IsPortInUse(real) {
		t.Error("IsPortInUse(EADDRINUSE chain) = false, want true")
	}
	if IsPortInUse(errors.New("boom")) {
		t.Error("IsPortInUse(random) = true, want false")
	}
	// Windows: WSAEADDRINUSE surfaces as a plain Errno whose string differs
	// from syscall.EADDRINUSE — matched by message fallback.
	if !IsPortInUse(errors.New("bind: Only one usage of each socket address (protocol/network address/port) is normally permitted.")) {
		t.Error("IsPortInUse(win bind msg) = false, want true")
	}
	if !IsPortInUse(errors.New("listen tcp 127.0.0.1:3457: bind: address already in use")) {
		t.Error("IsPortInUse(linux bind msg) = false, want true")
	}
	if IsPortInUse(nil) {
		t.Error("IsPortInUse(nil) = true, want false")
	}
}

func TestPortOf(t *testing.T) {
	cases := []struct{ addr, want string }{
		{":3457", "3457"},
		{"127.0.0.1:3457", "3457"},
		{"[::1]:8080", "8080"},
		{"", ""},
		{"no-port", ""},
	}
	for _, c := range cases {
		if got := PortOf(c.addr); got != c.want {
			t.Errorf("PortOf(%q) = %q, want %q", c.addr, got, c.want)
		}
	}
}

func TestWindowsPortPIDFromOutput(t *testing.T) {
	out := `  TCP    127.0.0.1:3456         0.0.0.0:0              LISTENING       111
  TCP    127.0.0.1:3457         0.0.0.0:0              LISTENING       44420
  TCP    127.0.0.1:3457         0.0.0.0:0              ESTABLISHED     9999
`
	if got := windowsPortPIDFromOutput(out, "3457"); got != "44420" {
		t.Errorf("windowsPortPIDFromOutput(3457) = %q, want 44420", got)
	}
	// Port with no listener → empty.
	if got := windowsPortPIDFromOutput(out, "9999"); got != "" {
		t.Errorf("windowsPortPIDFromOutput(9999) = %q, want empty", got)
	}
	if got := windowsPortPIDFromOutput("", "3457"); got != "" {
		t.Errorf("windowsPortPIDFromOutput(empty) = %q, want empty", got)
	}
}

// TestWindowsPortPIDFromPowerShellOutput pins the Get-NetTCPConnection
// fallback parser: the first numeric PID wins, non-numeric stray lines are
// skipped, and empty output yields "".
func TestWindowsPortPIDFromPowerShellOutput(t *testing.T) {
	// Multiple listening connections print one OwningProcess per line.
	multi := "44420\n111\n"
	if got := windowsPortPIDFromPowerShellOutput(multi); got != "44420" {
		t.Errorf("windowsPortPIDFromPowerShellOutput(multi) = %q, want 44420", got)
	}
	// A stray non-numeric line (e.g. a warning) before the PID is skipped.
	stray := "WARNING: something\n1234\n"
	if got := windowsPortPIDFromPowerShellOutput(stray); got != "1234" {
		t.Errorf("windowsPortPIDFromPowerShellOutput(stray) = %q, want 1234", got)
	}
	// No listener: empty output (SilentlyContinue) → empty.
	if got := windowsPortPIDFromPowerShellOutput(""); got != "" {
		t.Errorf("windowsPortPIDFromPowerShellOutput(empty) = %q, want empty", got)
	}
}

func TestTaskNameFromCSV(t *testing.T) {
	if got := taskNameFromCSV(`"freebuff-proxy-dash.exe","44420","Console","1","50,776 K"`); got != "freebuff-proxy-dash.exe" {
		t.Errorf("taskNameFromCSV = %q, want freebuff-proxy-dash.exe", got)
	}
	if got := taskNameFromCSV("no quotes"); got != "" {
		t.Errorf("taskNameFromCSV(no quotes) = %q, want empty", got)
	}
	if got := taskNameFromCSV(""); got != "" {
		t.Errorf("taskNameFromCSV(empty) = %q, want empty", got)
	}
}

// TestSSPortPID pins the ss -ltnp pid= parser: the first pid= value wins,
// values are cut at "," or ")", and a line whose pid= is empty is skipped.
func TestSSPortPID(t *testing.T) {
	out := `State  Recv-Q Send-Q Local Address:Port Peer Address:Port Process
LISTEN 0 128 0.0.0.0:3456 0.0.0.0:* users:(("other",pid=111,fd=5))
LISTEN 0 128 0.0.0.0:3457 0.0.0.0:* users:(("proc",pid=1234,fd=5))
LISTEN 0 128 [::]:8080 [::]:* users:(("foo",pid=99,fd=3))
`
	if got := ssPortPID(out); got != "111" {
		t.Errorf("ssPortPID = %q, want first pid 111", got)
	}
	if got := ssPortPID("no pid here\n"); got != "" {
		t.Errorf("ssPortPID(no pid) = %q, want empty", got)
	}
	if got := ssPortPID(""); got != "" {
		t.Errorf("ssPortPID(empty) = %q, want empty", got)
	}
	// pid= with nothing after the comma is skipped, the next line wins.
	two := `users:(("x",pid=,fd=1))
users:(("y",pid=42,fd=1))
`
	if got := ssPortPID(two); got != "42" {
		t.Errorf("ssPortPID(empty pid then 42) = %q, want 42", got)
	}
}

// TestBusyboxPortPID pins the busybox netstat -ltnp parser: only tcp
// LISTEN lines whose local address ends with ":port" match, and the
// "PID/program" last field yields the PID (tcp6 lines included).
func TestBusyboxPortPID(t *testing.T) {
	out := `Active Internet connections (only servers)
Proto Recv-Q Send-Q Local Address Foreign Address State PID/Program name
tcp  0  0 127.0.0.1:9999 0.0.0.0:* LISTEN 4321/x
tcp  0  0 0.0.0.0:3457 0.0.0.0:* LISTEN 1234/freebuff-proxy
tcp6 0  0 [::]:8080 [::]:* LISTEN 5678/other
tcp  0  0 0.0.0.0:3457 0.0.0.0:* ESTABLISHED 8888
`
	if got := busyboxPortPID(out, "3457"); got != "1234" {
		t.Errorf("busyboxPortPID(3457) = %q, want 1234", got)
	}
	if got := busyboxPortPID(out, "8080"); got != "5678" {
		t.Errorf("busyboxPortPID(8080) = %q, want 5678 (tcp6)", got)
	}
	if got := busyboxPortPID(out, "9999"); got != "4321" {
		t.Errorf("busyboxPortPID(9999) = %q, want 4321", got)
	}
	if got := busyboxPortPID(out, "3456"); got != "" {
		t.Errorf("busyboxPortPID(3456) = %q, want empty (not listening)", got)
	}
	if got := busyboxPortPID("", "3457"); got != "" {
		t.Errorf("busyboxPortPID(empty) = %q, want empty", got)
	}
}
