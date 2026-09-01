package update

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// replaceExecutable installs the freshly downloaded binary (tempPath, in the
// same directory as execPath) over the currently running executable.
// On Unix the swap is atomic (rename); on Windows the running executable
// cannot be replaced while in use, so a detached helper script performs the
// swap after this process exits. The returned message describes deferred
// behavior when applicable.
func replaceExecutable(execPath, tempPath string) (string, error) {
	if runtime.GOOS == "windows" {
		return installWindows(execPath, tempPath)
	}
	if err := installUnix(execPath, tempPath); err != nil {
		return "", err
	}
	return "", nil
}

// installUnix atomically replaces execPath with tempPath via rename: the old
// binary is moved aside to execPath.old, the new one is renamed into place,
// and the old file is removed.
func installUnix(execPath, tempPath string) error {
	oldPath := execPath + ".old"
	_ = os.Remove(oldPath)
	if err := os.Rename(execPath, oldPath); err != nil {
		return fmt.Errorf("rename current binary aside: %w", err)
	}
	if err := os.Rename(tempPath, execPath); err != nil {
		_ = os.Rename(oldPath, execPath) // rollback
		return fmt.Errorf("install updated binary: %w", err)
	}
	_ = os.Remove(oldPath)
	return nil
}

// updateResultMarker is the file the Windows helper writes after the deferred
// swap: "OK" once the new binary replaced the old, or "FAILED: ..." plus
// manual-install instructions. The parent must not claim the update succeeded
// until this marker says OK.
func updateResultMarker(execPath string) string {
	return execPath + ".update.result"
}

// installWindows writes a small .bat helper next to the executable that waits
// for the current process (pid) to exit, then moves the downloaded temp file
// into place (retrying while AV/Defender briefly holds the lock) and writes a
// result marker. The helper is launched detached via `cmd /c start /b`, so it
// survives this process exiting without flashing a console window. /b (not
// plain start) is required: without it, start blocks trying to create a new
// console window in non-interactive contexts (Task Scheduler, services).
func installWindows(execPath, tempPath string) (string, error) {
	batPath := execPath + ".update.bat"
	script := windowsUpdateScript(execPath, tempPath, os.Getpid())
	if err := os.WriteFile(batPath, []byte(script), 0755); err != nil {
		return "", fmt.Errorf("write update helper script: %w", err)
	}

	cmd := exec.Command("cmd", "/c", "start", "/b", "", batPath)
	if err := cmd.Start(); err != nil {
		_ = os.Remove(batPath)
		return "", fmt.Errorf("launch update helper script: %w", err)
	}

	marker := updateResultMarker(execPath)
	return fmt.Sprintf("The new binary will be installed automatically after this process (PID %d) exits.\n"+
		"Result marker: %s (OK once the swap finished; FAILED means it did not).\n"+
		"If it fails, finish manually: move %s over %s.",
		os.Getpid(), marker, tempPath, execPath), nil
}

// windowsUpdateScript returns the content of the .bat helper used to install
// the updated binary on Windows. It polls tasklist until the updating process
// (pid) is gone, then moves tempPath over execPath, retrying while
// AV/Defender briefly locks the running exe, and writes a result marker.
// Only ASCII values are interpolated: the pid and the file basenames. The
// directory comes from %~dp0 (the helper's own location), which cmd resolves
// from the real path — correct even for non-ASCII directories and with no
// console/codepage dependency. This keeps the .bat pure ASCII: cmd reads
// batch files with the console codepage, which would mangle non-ASCII paths
// embedded verbatim. The helper does not delete itself (self-deletion races
// cmd's incremental file reads); the marker is the source of truth and the
// inert .bat is overwritten on the next run.
func windowsUpdateScript(execPath, tempPath string, pid int) string {
	markerBase := winBase(updateResultMarker(execPath))
	script := fmt.Sprintf(`@echo off
setlocal
set "TARGET_PID=%d"
set "TEMP_FILE=%%~dp0%s"
set "EXE_FILE=%%~dp0%s"

:waitloop
tasklist /FI "PID eq %%TARGET_PID%%" 2>nul | findstr "%%TARGET_PID%%" >nul
if errorlevel 1 goto install
timeout /t 1 /nobreak >nul
goto waitloop

:install
set /a tries=0
:retry
move /y "%%TEMP_FILE%%" "%%EXE_FILE%%" >nul 2>&1
if not errorlevel 1 goto installed
set /a tries+=1
if %%tries%% geq 5 goto failed
timeout /t 2 /nobreak >nul
goto retry

:installed
echo OK> "%%~dp0%s"
goto cleanup

:failed
echo FAILED: could not replace the running binary after 5 attempts.> "%%~dp0%s"
echo Install manually: move "%%TEMP_FILE%%" over "%%EXE_FILE%%".>> "%%~dp0%s"
goto cleanup

:cleanup
endlocal
`, pid,
		winBase(tempPath), winBase(execPath),
		markerBase, markerBase, markerBase)
	// Batch files must use CRLF line endings: cmd misparses LF-only files
	// (goto labels and multi-line constructs break with "cannot be found").
	return strings.ReplaceAll(script, "\n", "\r\n")
}

// winBase returns the final path element split on BOTH Windows and Unix
// separators. The .bat template interpolates basenames (paths enter only as
// %~dp0 + basename), and on a non-Windows build host filepath.Base would not
// split backslashes — leaking a full non-ASCII Windows path into the batch
// file, which cmd would mangle (console codepage) and the ASCII guard test
// would reject.
func winBase(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
