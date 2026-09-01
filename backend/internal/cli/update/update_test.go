package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestInstallUnixAtomicSwap(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, "freebuff-proxy")
	newPath := filepath.Join(dir, "freebuff-proxy.new")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("new-binary"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := installUnix(execPath, newPath); err != nil {
		t.Fatalf("installUnix: %v", err)
	}

	got, err := os.ReadFile(execPath)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if string(got) != "new-binary" {
		t.Errorf("installed content = %q, want %q", got, "new-binary")
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Errorf("temp file %q should have been consumed by the swap", newPath)
	}
	if _, err := os.Stat(execPath + ".old"); !os.IsNotExist(err) {
		t.Errorf(".old backup %q should have been removed", execPath+".old")
	}
}

func TestInstallUnixFailsWhenOldBinaryGone(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, "freebuff-proxy")
	newPath := filepath.Join(dir, "freebuff-proxy.new")
	if err := os.WriteFile(newPath, []byte("new-binary"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := installUnix(execPath, newPath); err == nil {
		t.Fatal("expected error when current binary is missing")
	}
	// The temp file must survive a failed swap.
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("temp file should survive a failed swap: %v", err)
	}
}

func TestWindowsUpdateScript(t *testing.T) {
	exe := `C:\tools\freebuff proxy\freebuff-proxy.exe`
	tmp := `C:\tools\freebuff proxy\freebuff-proxy.exe.tmp-123`
	script := windowsUpdateScript(exe, tmp, 4242)

	for _, want := range []string{
		`set "TARGET_PID=4242"`,
		`tasklist /FI "PID eq %TARGET_PID%"`,
		`findstr "%TARGET_PID%"`,
		`set "TEMP_FILE=%~dp0`,
		`set "EXE_FILE=%~dp0`,
		`move /y "%TEMP_FILE%" "%EXE_FILE%"`,
		`echo OK>`,
		"timeout /t 1 /nobreak",
		"endlocal",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("helper script missing %q; script:\n%s", want, script)
		}
	}
	// Sprintf escaping must collapse %% -> % (batch variable references).
	if strings.Contains(script, "%%") {
		t.Errorf("helper script contains unescaped %% (Sprintf escaping not applied); script:\n%s", script)
	}
}

// TestWindowsUpdateScriptRunsAndSwaps is the end-to-end Windows check: the
// generated .bat must resolve %~dp0 paths, swap the temp binary over the
// executable, and write an OK result marker. (The helper does not self-delete
// — that races cmd's file reads — so the marker is the source of truth.)
func TestWindowsUpdateScriptRunsAndSwaps(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only deferred swap")
	}
	dir := t.TempDir()
	execPath := filepath.Join(dir, "freebuff-proxy.exe")
	tmpPath := filepath.Join(dir, "freebuff-proxy.exe.tmp-123")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmpPath, []byte("new-binary"), 0755); err != nil {
		t.Fatal(err)
	}

	// A pid that is already gone: the helper's waitloop exits immediately
	// instead of waiting for a real process.
	dead := exec.Command("cmd", "/c", "exit")
	if err := dead.Start(); err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	deadPid := dead.Process.Pid
	_ = dead.Wait()

	batPath := execPath + ".update.bat"
	if err := os.WriteFile(batPath, []byte(windowsUpdateScript(execPath, tmpPath, deadPid)), 0755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("cmd", "/c", batPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper script failed: %v\n%s", err, out)
	}

	got, err := os.ReadFile(execPath)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if string(got) != "new-binary" {
		t.Errorf("installed content = %q, want %q", got, "new-binary")
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("temp file should be consumed by the swap, stat err = %v", err)
	}
	marker, err := os.ReadFile(execPath + ".update.result")
	if err != nil {
		t.Fatalf("read result marker: %v", err)
	}
	if !strings.HasPrefix(string(marker), "OK") {
		t.Errorf("result marker = %q, want OK prefix", marker)
	}
}

// TestWindowsUpdateScriptDefersSwapUntilParentExits pins the deferred-swap
// contract: the helper launched detached via `cmd /c start /b` must outlive
// the launcher, wait for the updating process (pid) to exit, and only then
// swap the binary and write the OK marker.
// Known Windows-local flake: can fail once under Temp/AV timing (a slow
// Defender scan of the temp binary can push the swap past the 15s deadline).
// It is green on re-run and unrelated to the code — CI gates on the ubuntu
// run, so treat a lone Windows hiccup here as a re-run, not a defect.
func TestWindowsUpdateScriptDefersSwapUntilParentExits(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only deferred swap")
	}
	dir := t.TempDir()
	execPath := filepath.Join(dir, "freebuff-proxy.exe")
	tmpPath := filepath.Join(dir, "freebuff-proxy.exe.tmp-123")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmpPath, []byte("new-binary"), 0755); err != nil {
		t.Fatal(err)
	}

	// The "updating parent": a helper process that stays alive ~2s. The swap
	// must NOT happen until it exits.
	parent := exec.Command(os.Args[0], "-test.run=TestWindowsUpdateSleepHelperProcess")
	parent.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	if err := parent.Start(); err != nil {
		t.Fatalf("cannot start updating-parent helper: %v", err)
	}
	parentPid := parent.Process.Pid
	parentDone := make(chan struct{})
	go func() {
		_ = parent.Wait()
		close(parentDone)
	}()
	t.Cleanup(func() {
		_ = parent.Process.Kill()
		<-parentDone
	})

	batPath := execPath + ".update.bat"
	if err := os.WriteFile(batPath, []byte(windowsUpdateScript(execPath, tmpPath, parentPid)), 0755); err != nil {
		t.Fatal(err)
	}

	// Launch exactly like installWindows: detached via cmd /c start /b.
	// Note: use Start+Wait on the launcher, NOT CombinedOutput — the /b
	// helper inherits the launcher's stdio, so CombinedOutput would block
	// until the helper itself finishes, masking the deferred timing.
	start := time.Now()
	launcher := exec.Command("cmd", "/c", "start", "/b", "", batPath)
	if err := launcher.Start(); err != nil {
		t.Fatalf("launch helper script: %v", err)
	}
	if err := launcher.Wait(); err != nil {
		t.Fatalf("launch helper script: %v", err)
	}

	// The swap must be deferred: shortly after launch the parent is still
	// alive, so the old binary must still be in place.
	time.Sleep(500 * time.Millisecond)
	if got, _ := os.ReadFile(execPath); string(got) != "old-binary" {
		t.Fatalf("swap happened before the updating parent exited (content %q)", got)
	}

	// Once the parent exits (~2s), the helper must complete the swap.
	deadline := time.Now().Add(15 * time.Second)
	for {
		got, err := os.ReadFile(execPath)
		if err == nil && string(got) == "new-binary" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("swap did not happen after the updating parent exited")
		}
		time.Sleep(100 * time.Millisecond)
	}
	marker, err := os.ReadFile(execPath + ".update.result")
	if err != nil {
		t.Fatalf("read result marker: %v", err)
	}
	if !strings.HasPrefix(string(marker), "OK") {
		t.Errorf("result marker = %q, want OK prefix", marker)
	}
	t.Logf("deferred swap completed %v after launch", time.Since(start).Round(100*time.Millisecond))
}

// TestWindowsUpdateSleepHelperProcess is the re-exec helper for
// TestWindowsUpdateScriptDefersSwapUntilParentExits: it simply stays alive
// ~2s so the update helper has something to wait for.
func TestWindowsUpdateSleepHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	time.Sleep(2 * time.Second)
}

// TestVerifyChecksumFetchFailureAborts guards the supply-chain guarantee
// (see .github/SECURITY.md): a checksums.txt fetch failure must abort the
// update, not silently proceed unverified.
func TestVerifyChecksumFetchFailureAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	err := verifyChecksum(context.Background(), &http.Client{Timeout: 5 * time.Second}, srv.URL+"/checksums.txt", "freebuff-proxy_linux_amd64.tar.gz", []byte("asset-bytes"))
	if err == nil {
		t.Fatal("verifyChecksum succeeded, want error when checksums.txt fetch fails")
	}
	if !strings.Contains(err.Error(), "checksums.txt") {
		t.Errorf("verifyChecksum error = %v, want mention of checksums.txt", err)
	}
}

func TestVerifyChecksumMatchAndMismatch(t *testing.T) {
	assetBytes := []byte("asset-bytes")
	sum := sha256.Sum256(assetBytes)
	checksums := hex.EncodeToString(sum[:]) + "  freebuff-proxy_linux_amd64.tar.gz\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(checksums))
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	url := srv.URL + "/checksums.txt"
	assetFilename := "freebuff-proxy_linux_amd64.tar.gz"

	if err := verifyChecksum(context.Background(), client, url, assetFilename, assetBytes); err != nil {
		t.Fatalf("verifyChecksum(matching) = %v, want nil", err)
	}
	if err := verifyChecksum(context.Background(), client, url, assetFilename, []byte("other-bytes")); err == nil {
		t.Fatal("verifyChecksum(mismatch) = nil, want checksum mismatch error")
	}
}

// TestReportUpdateResultMarkerReportsAndDeletes pins the deferred-swap marker
// contract: a stale <exe>.update.result left by a previous Windows swap is
// surfaced ("Previous deferred update result: ...") and deleted, so a FAILED
// swap is reported exactly once on the next -update invocation.
func TestReportUpdateResultMarkerReportsAndDeletes(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "freebuff-proxy.exe")
	marker := updateResultMarker(exe)

	// FAILED case: the previous deferred swap did not complete.
	failed := "FAILED: could not replace the running binary after 5 attempts.\nInstall manually: move \"new\" over \"old\".\n"
	if err := os.WriteFile(marker, []byte(failed), 0644); err != nil {
		t.Fatal(err)
	}
	if got := reportUpdateResultMarker(exe); got != strings.TrimSpace(failed) {
		t.Errorf("reportUpdateResultMarker(FAILED) = %q, want %q", got, strings.TrimSpace(failed))
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("FAILED marker was not deleted after reporting")
	}

	// OK case: the previous deferred swap succeeded.
	if err := os.WriteFile(marker, []byte("OK\r\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := reportUpdateResultMarker(exe); got != "OK" {
		t.Errorf("reportUpdateResultMarker(OK) = %q, want %q", got, "OK")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("OK marker was not deleted after reporting")
	}

	// No marker: no-op, returns "".
	if got := reportUpdateResultMarker(exe); got != "" {
		t.Errorf("reportUpdateResultMarker(no marker) = %q, want \"\"", got)
	}
}

// --- downloadURL (S7 regression + error paths) ---

func TestDownloadURLNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := downloadURL(context.Background(), &http.Client{Timeout: 5 * time.Second}, srv.URL)
	if err == nil {
		t.Fatal("downloadURL succeeded, want error on non-200")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("downloadURL error = %v, want HTTP 500 mentioned", err)
	}
}

func TestDownloadURLCtxCancel(t *testing.T) {
	// A handler that blocks until the request context is canceled.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := downloadURL(ctx, &http.Client{Timeout: 30 * time.Second}, srv.URL)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("downloadURL succeeded, want context-canceled error")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("downloadURL error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("downloadURL did not return after context cancel")
	}
}

// TestDownloadURLOversizedBody pins S7: a body larger than the safety cap
// must be rejected instead of being buffered into memory unboundedly.
func TestDownloadURLOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, maxUpdateDownloadBytes+1))
	}))
	defer srv.Close()

	_, err := downloadURL(context.Background(), &http.Client{Timeout: 30 * time.Second}, srv.URL)
	if err == nil {
		t.Fatal("downloadURL succeeded, want oversized-body error")
	}
	if !strings.Contains(err.Error(), "safety cap") {
		t.Errorf("downloadURL error = %v, want mention of the safety cap", err)
	}

	// A body exactly at the cap boundary still succeeds.
	srvOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, maxUpdateDownloadBytes))
	}))
	defer srvOK.Close()
	if _, err := downloadURL(context.Background(), &http.Client{Timeout: 30 * time.Second}, srvOK.URL); err != nil {
		t.Errorf("downloadURL(at cap) = %v, want nil", err)
	}
}

// --- archive extraction ---

// releaseArchiveBytes builds a release archive in memory: zip when ext ==
// ".zip", else tar.gz. entryName is the archive member path (may be nested).
func releaseArchiveBytes(t *testing.T, ext, entryName string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if ext == ".zip" {
		zw := zip.NewWriter(&buf)
		fw, err := zw.Create(entryName)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(content); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		hdr := &tar.Header{Name: entryName, Mode: 0755, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func TestExtractBinaryFromArchive(t *testing.T) {
	want := []byte("binary-bytes-v9.9.9")
	const binaryName = "freebuff-proxy"
	tests := []struct {
		name      string
		ext       string
		entryName string
		wantBytes []byte
		wantErr   string // substring; "" = success
	}{
		{
			name:      "zip top-level match",
			ext:       ".zip",
			entryName: binaryName,
			wantBytes: want,
		},
		{
			name:      "tar.gz top-level match",
			ext:       ".tar.gz",
			entryName: binaryName,
			wantBytes: want,
		},
		{
			// Goreleaser archives nest the binary under a versioned dir:
			// freebuff-proxy_9.9.9_linux_amd64/freebuff-proxy
			name:      "goreleaser nested layout",
			ext:       ".tar.gz",
			entryName: "freebuff-proxy_9.9.9_linux_amd64/" + binaryName,
			wantBytes: want,
		},
		{
			name:      "zip missing binary",
			ext:       ".zip",
			entryName: "README.md",
			wantErr:   "not found in downloaded release archive",
		},
		{
			name:      "tar.gz missing binary",
			ext:       ".tar.gz",
			entryName: "README.md",
			wantErr:   "not found in downloaded release archive",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			archive := releaseArchiveBytes(t, tt.ext, tt.entryName, want)
			got, err := extractBinaryFromArchive("https://example.com/release"+tt.ext, archive, binaryName)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("extractBinaryFromArchive succeeded, want error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("extractBinaryFromArchive error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("extractBinaryFromArchive: %v", err)
			}
			if !bytes.Equal(got, tt.wantBytes) {
				t.Errorf("extracted bytes = %q, want %q", got, tt.wantBytes)
			}
		})
	}
}

// TestExtractBinaryFromArchiveOversizedEntry pins the decompression-
// amplification guard: an archive member larger than the entry cap must be
// rejected with "release archive entry too large" instead of being loaded
// unboundedly (or installed truncated). Exercised through the real
// production path for both archive formats with a cap+1 entry of zeros
// (highly compressible, so the archives stay small).
func TestExtractBinaryFromArchiveOversizedEntry(t *testing.T) {
	const binaryName = "freebuff-proxy"
	for _, ext := range []string{".zip", ".tar.gz"} {
		t.Run(ext, func(t *testing.T) {
			archive := releaseArchiveBytes(t, ext, binaryName, make([]byte, maxUpdateArchiveEntryBytes+1))
			_, err := extractBinaryFromArchive("https://example.com/release"+ext, archive, binaryName)
			if err == nil {
				t.Fatal("extractBinaryFromArchive succeeded, want entry-too-large error")
			}
			if !strings.Contains(err.Error(), "release archive entry too large") {
				t.Errorf("extractBinaryFromArchive error = %v, want 'release archive entry too large'", err)
			}
		})
	}
}

// --- release JSON decode + platform asset matching ---

func TestReleaseJSONDecodeAndAssetMatch(t *testing.T) {
	// The exact GitHub API wire shape for a release with several assets.
	releaseJSON := `{
		"tag_name": "v9.9.9",
		"assets": [
			{"name": "freebuff-proxy_9.9.9_linux_amd64.tar.gz", "browser_download_url": "https://cdn.example/fp_linux_amd64.tar.gz"},
			{"name": "freebuff-proxy_9.9.9_linux_arm64.tar.gz", "browser_download_url": "https://cdn.example/fp_linux_arm64.tar.gz"},
			{"name": "freebuff-proxy_9.9.9_windows_amd64.zip", "browser_download_url": "https://cdn.example/fp_windows_amd64.zip"},
			{"name": "checksums.txt", "browser_download_url": "https://cdn.example/checksums.txt"}
		]
	}`
	var rel githubRelease
	if err := json.Unmarshal([]byte(releaseJSON), &rel); err != nil {
		t.Fatalf("decode release json: %v", err)
	}
	if rel.TagName != "v9.9.9" {
		t.Errorf("TagName = %q, want v9.9.9", rel.TagName)
	}

	// linux/amd64 → tar.gz asset + checksums.
	assetName, assetURL, checksumURL, ok := matchReleaseAssets(rel.Assets, "linux", "amd64")
	if !ok {
		t.Fatal("matchReleaseAssets(linux/amd64) ok = false, want true")
	}
	if assetName != "freebuff-proxy_9.9.9_linux_amd64.tar.gz" {
		t.Errorf("assetName = %q, want the linux amd64 tar.gz", assetName)
	}
	if !strings.HasSuffix(assetURL, ".tar.gz") {
		t.Errorf("assetURL = %q, want .tar.gz suffix on linux", assetURL)
	}
	if !strings.HasSuffix(checksumURL, "checksums.txt") {
		t.Errorf("checksumURL = %q, want checksums.txt", checksumURL)
	}

	// windows/amd64 → zip.
	assetName, assetURL, _, ok = matchReleaseAssets(rel.Assets, "windows", "amd64")
	if !ok {
		t.Fatal("matchReleaseAssets(windows/amd64) ok = false, want true")
	}
	if assetName != "freebuff-proxy_9.9.9_windows_amd64.zip" {
		t.Errorf("assetName = %q, want the windows amd64 zip", assetName)
	}
	if !strings.HasSuffix(assetURL, ".zip") {
		t.Errorf("assetURL = %q, want .zip suffix on windows", assetURL)
	}

	// An unsupported platform has no asset → ok=false.
	if _, _, _, ok := matchReleaseAssets(rel.Assets, "darwin", "arm64"); ok {
		t.Error("matchReleaseAssets(darwin/arm64) ok = true, want false")
	}

	// platformAssetSuffix: windows → zip, others → tar.gz.
	if got := platformAssetSuffix("windows", "amd64"); got != "windows_amd64.zip" {
		t.Errorf("platformAssetSuffix(windows) = %q, want windows_amd64.zip", got)
	}
	if got := platformAssetSuffix("linux", "amd64"); got != "linux_amd64.tar.gz" {
		t.Errorf("platformAssetSuffix(linux) = %q, want linux_amd64.tar.gz", got)
	}
}

// --- up-to-date vs dev version branch ---

func TestIsUpToDate(t *testing.T) {
	tests := []struct {
		version, tag string
		want         bool
	}{
		{"dev", "v9.9.9", false},    // dev builds are never up to date
		{"dev", "dev", false},       // ... even against a "dev" tag
		{"9.9.9", "v9.9.9", true},   // tag carries the v prefix
		{"9.9.9", "9.9.9", true},    // exact match without prefix
		{"9.9.8", "v9.9.9", false},  // older version
		{"10.0.0", "v9.9.9", false}, // newer-but-different is not a match (no semver)
		{"9.9.9", "v9.9.10", false}, // prefix mismatch must not match
	}
	for _, tt := range tests {
		if got := isUpToDate(tt.version, tt.tag); got != tt.want {
			t.Errorf("isUpToDate(%q, %q) = %v, want %v", tt.version, tt.tag, got, tt.want)
		}
	}
}

// --- S5: checksums.txt-absent must fail closed ---

func TestRequireChecksumsFailsClosed(t *testing.T) {
	if err := requireChecksums(""); err == nil {
		t.Fatal("requireChecksums(\"\") = nil, want error for a release without checksums.txt")
	} else if !strings.Contains(err.Error(), "refusing to install unverified") {
		t.Errorf("requireChecksums(\"\") error = %v, want 'refusing to install unverified'", err)
	}
	if err := requireChecksums("https://cdn.example/checksums.txt"); err != nil {
		t.Errorf("requireChecksums(valid URL) = %v, want nil", err)
	}
}

func TestMatchReleaseAssetsMissingChecksums(t *testing.T) {
	assets := []releaseAsset{
		{Name: "freebuff-proxy_9.9.9_linux_amd64.tar.gz", BrowserDownloadURL: "https://cdn.example/a.tar.gz"},
	}
	_, _, checksumURL, ok := matchReleaseAssets(assets, "linux", "amd64")
	if !ok {
		t.Fatal("platform asset must match even without checksums.txt")
	}
	if checksumURL != "" {
		t.Errorf("checksumURL = %q, want empty when the release has no checksums.txt", checksumURL)
	}
}

// --- S6: checksum must be bound to its asset filename ---

func TestVerifyChecksumFilenameBinding(t *testing.T) {
	assetBytes := []byte("asset-bytes")
	sum := sha256.Sum256(assetBytes)
	goodHash := hex.EncodeToString(sum[:])

	tests := []struct {
		name          string
		checksums     string
		assetFilename string
		wantErr       bool
	}{
		{
			name:          "hash and filename match",
			checksums:     goodHash + "  freebuff-proxy_linux_amd64.tar.gz\n",
			assetFilename: "freebuff-proxy_linux_amd64.tar.gz",
		},
		{
			name:          "goreleaser two-space format",
			checksums:     goodHash + "  freebuff-proxy_9.9.9_linux_amd64.tar.gz\n",
			assetFilename: "freebuff-proxy_9.9.9_linux_amd64.tar.gz",
		},
		{
			name:          "sha256sum star prefix tolerated",
			checksums:     goodHash + " *freebuff-proxy_linux_amd64.tar.gz\n",
			assetFilename: "freebuff-proxy_linux_amd64.tar.gz",
		},
		{
			// S6 regression: the SAME hash bound to a DIFFERENT file must
			// not vouch for this asset.
			name:          "same hash different filename rejected",
			checksums:     goodHash + "  freebuff-proxy_linux_arm64.tar.gz\n",
			assetFilename: "freebuff-proxy_linux_amd64.tar.gz",
			wantErr:       true,
		},
		{
			name:          "hash only in a different-filename line still fails",
			checksums:     "0000000000000000000000000000000000000000000000000000000000000000  freebuff-proxy_linux_amd64.tar.gz\n" + goodHash + "  other-file.bin\n",
			assetFilename: "freebuff-proxy_linux_amd64.tar.gz",
			wantErr:       true,
		},
		{
			name:          "wrong hash with right filename",
			checksums:     "0000000000000000000000000000000000000000000000000000000000000000  freebuff-proxy_linux_amd64.tar.gz\n",
			assetFilename: "freebuff-proxy_linux_amd64.tar.gz",
			wantErr:       true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tt.checksums))
			}))
			defer srv.Close()

			err := verifyChecksum(context.Background(), &http.Client{Timeout: 5 * time.Second}, srv.URL+"/checksums.txt", tt.assetFilename, assetBytes)
			if tt.wantErr && err == nil {
				t.Fatal("verifyChecksum succeeded, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("verifyChecksum = %v, want nil", err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "checksum mismatch") {
				t.Errorf("verifyChecksum error = %v, want 'checksum mismatch'", err)
			}
		})
	}
}

// --- winBase (update_swap.go) ---

func TestWinBase(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{`C:\tools\freebuff-proxy.exe`, "freebuff-proxy.exe"},
		{`C:\tools\freebuff proxy\freebuff-proxy.exe.tmp-123`, "freebuff-proxy.exe.tmp-123"},
		{"freebuff-proxy.exe", "freebuff-proxy.exe"},
		{"tools/freebuff-proxy", "freebuff-proxy"},
		{`C:\Users\张三\freebuff-proxy.exe`, "freebuff-proxy.exe"},
		{"/usr/local/bin/freebuff-proxy", "freebuff-proxy"},
	}
	for _, tt := range tests {
		if got := winBase(tt.in); got != tt.want {
			t.Errorf("winBase(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// --- installUnix rollback path ---

// TestInstallUnixRollback pins the rollback in installUnix: when the second
// rename (temp → executable) fails, the old binary must be restored in
// place and the .old staging file cleaned up, so a failed swap never leaves
// the executable missing.
func TestInstallUnixRollback(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, "freebuff-proxy")
	// The temp file does NOT exist: the first rename (exec → .old) succeeds,
	// the second (missing temp → exec) fails, triggering the rollback.
	missingTemp := filepath.Join(dir, "freebuff-proxy.tmp-999")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := installUnix(execPath, missingTemp); err == nil {
		t.Fatal("installUnix succeeded, want error when the temp binary is missing")
	}

	got, err := os.ReadFile(execPath)
	if err != nil {
		t.Fatalf("read restored binary: %v", err)
	}
	if string(got) != "old-binary" {
		t.Errorf("executable after rollback = %q, want old content restored", got)
	}
	if _, err := os.Stat(execPath + ".old"); !os.IsNotExist(err) {
		t.Errorf(".old staging file should be cleaned up by the rollback, stat err = %v", err)
	}
}

// TestGithubReleasesURLOverride pins the FREEBUFF_UPDATE_API_URL injection:
// the override must win, the default must be unchanged when unset.
func TestGithubReleasesURLOverride(t *testing.T) {
	if got := githubReleasesURL(); got != defaultReleasesURL {
		t.Errorf("githubReleasesURL() default = %q, want %q", got, defaultReleasesURL)
	}
	t.Setenv("FREEBUFF_UPDATE_API_URL", "http://127.0.0.1:9999/releases/latest")
	if got := githubReleasesURL(); got != "http://127.0.0.1:9999/releases/latest" {
		t.Errorf("githubReleasesURL() with override = %q, want the override", got)
	}
	t.Setenv("FREEBUFF_UPDATE_API_URL", "  ")
	if got := githubReleasesURL(); got != defaultReleasesURL {
		t.Errorf("githubReleasesURL() with blank override = %q, want default", got)
	}
}
