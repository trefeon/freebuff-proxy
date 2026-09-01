//go:build windows

package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestCtrlBreakDrainsGracefully pins the behavior behind the audit item
// "register syscall.SIGBREAK so Ctrl+Break drains gracefully": Go has no
// syscall.SIGBREAK constant on any platform, but the runtime delivers
// CTRL_BREAK_EVENT as os.Interrupt (runtime/os_windows.go ctrlHandler).
// This test proves end to end that a process registering exactly what Serve
// registers (shutdownSignals) survives Ctrl+Break and drains via ctx.Done
// instead of being terminated.
func TestCtrlBreakDrainsGracefully(t *testing.T) {
	helper := exec.Command(os.Args[0], "-test.run=TestCtrlBreakHelperProcess")
	helper.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	// A new process group so the CTRL_BREAK_EVENT targets only the helper
	// (its pid is its group id), never this test or the surrounding console.
	helper.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}

	out, err := helper.CombinedOutput()
	if strings.Contains(string(out), "NO_CONSOLE") {
		t.Skip("no console available to generate Ctrl+Break")
	}
	if err != nil {
		t.Fatalf("helper exited with error: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "DRAINED") {
		t.Errorf("Ctrl+Break did not drain gracefully; output:\n%s", out)
	}
}

// TestCtrlBreakHelperProcess is the re-exec helper for
// TestCtrlBreakDrainsGracefully (os/exec test pattern): it registers the
// same notify set as Serve, sends CTRL_BREAK_EVENT to its own process group,
// and reports whether the context drained.
func TestCtrlBreakHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer stop()

	drained := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(drained)
	}()

	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(os.Getpid())); err != nil {
		fmt.Println("NO_CONSOLE")
		os.Exit(2)
	}

	select {
	case <-drained:
		fmt.Println("DRAINED")
	case <-time.After(5 * time.Second):
		fmt.Println("NOT_DRAINED")
		os.Exit(3)
	}
}
