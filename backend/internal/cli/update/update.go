// Package update implements the -update mode: check the GitHub releases API
// for the latest release, verify its checksums.txt, download the platform
// asset, and swap the running binary (deferred on Windows via a helper .bat).
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
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// maxUpdateDownloadBytes caps a single -update download body (release asset
// or checksums.txt) so an oversized CDN response cannot OOM the updater (S7;
// mirrors the registry's 2MiB source cap with a generous release-asset limit).
const maxUpdateDownloadBytes = 64 << 20

// maxUpdateArchiveEntryBytes caps a single archive member read so a
// decompression-amplified release archive (zip bomb / gzip bomb) cannot
// OOM the updater: the archive bytes themselves are capped at
// maxUpdateDownloadBytes, but a gzip/zip member can decompress to far more.
const maxUpdateArchiveEntryBytes = maxUpdateDownloadBytes

// defaultReleasesURL is the GitHub API endpoint checked for the latest
// release. FREEBUFF_UPDATE_API_URL overrides it so tests (and self-hosted
// mirrors) can point -update at a fake release server.
const defaultReleasesURL = "https://api.github.com/repos/trefeon/freebuff-proxy/releases/latest"

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type githubRelease struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

// githubReleasesURL returns the release-check endpoint: the
// FREEBUFF_UPDATE_API_URL override when set, else the GitHub API default.
// The default behavior is unchanged; the override exists so -update can be
// exercised end-to-end against a fake release server.
func githubReleasesURL() string {
	if u := strings.TrimSpace(os.Getenv("FREEBUFF_UPDATE_API_URL")); u != "" {
		return u
	}
	return defaultReleasesURL
}

// isUpToDate reports whether the running version already matches the latest
// release tag. "dev" builds (no ldflags injection) are never up to date,
// and the tag may carry a "v" prefix the version string omits. Comparison
// is exact equality — there is no semver ordering.
func isUpToDate(currentVersion, latestTag string) bool {
	return currentVersion != "dev" && (currentVersion == latestTag || "v"+currentVersion == latestTag)
}

// platformAssetSuffix returns the release-asset filename suffix for the
// given platform, e.g. "linux_amd64.tar.gz" or "windows_amd64.zip".
func platformAssetSuffix(goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("%s_%s%s", goos, goarch, ext)
}

// matchReleaseAssets picks the platform asset (name + download URL) and the
// checksums.txt URL from the release's asset list. ok is false when no
// asset matches the platform suffix. A missing checksums.txt is NOT an
// error here — requireChecksums decides that separately (fail closed, S5).
func matchReleaseAssets(assets []releaseAsset, goos, goarch string) (assetName, assetURL, checksumURL string, ok bool) {
	suffix := platformAssetSuffix(goos, goarch)
	for _, a := range assets {
		if strings.HasSuffix(a.Name, suffix) {
			assetName, assetURL = a.Name, a.BrowserDownloadURL
		}
		if a.Name == "checksums.txt" {
			checksumURL = a.BrowserDownloadURL
		}
	}
	return assetName, assetURL, checksumURL, assetURL != ""
}

// requireChecksums fails closed when the release carries no checksums.txt
// asset (S5): the update must never proceed unverified (see
// .github/SECURITY.md). A release missing the checksum manifest is refused
// instead of silently skipping verification.
func requireChecksums(checksumURL string) error {
	if checksumURL == "" {
		return errors.New("release has no checksums.txt asset; refusing to install unverified binary")
	}
	return nil
}

// Run drives the -update mode for the running version and exits. version is
// the ldflags-injected version string ("dev" when not injected).
func Run(version string) {
	// A deferred Windows swap (see update_swap.go) records its outcome in
	// <exe>.update.result only AFTER this process exits — the swap helper
	// waits for the parent, so the parent cannot wait on it. Surface any
	// stale marker before doing anything else: a FAILED swap from the
	// previous run must not be silently ignored.
	if execPath, err := os.Executable(); err == nil {
		reportUpdateResultMarker(execPath)
	}

	fmt.Println("freebuff-proxy self-updater")
	fmt.Println("===========================")
	fmt.Printf("Current version: %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubReleasesURL(), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: build request: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("User-Agent", "freebuff-proxy/"+version)
	req.Header.Set("Accept", "application/vnd.github+json")

	// No per-request timeout: each request is bounded by its own context
	// rather than a fixed timeout that would abort a slow-but-healthy
	// download. The release metadata and checksums.txt share the 30s
	// context above; the asset download gets a separate, longer budget
	// (below) so a slow link can finish the 64MB file without dead-lining
	// mid-download.
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: check latest release: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "ERROR: GitHub API returned status %d\n", resp.StatusCode)
		os.Exit(1)
	}

	var rel githubRelease
	// FREEBUFF_UPDATE_API_URL can point at an arbitrary server: bound the
	// release-JSON decode body (S7 invariant) so a misbehaving endpoint
	// cannot stream an unbounded payload into memory. A truncated or
	// malformed body surfaces as a decode error and aborts the update.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: decode release json: %v\n", err)
		os.Exit(1)
	}

	if rel.TagName == "" {
		fmt.Fprintln(os.Stderr, "ERROR: release has no tag_name")
		os.Exit(1)
	}

	fmt.Printf("Latest release: %s\n", rel.TagName)
	if isUpToDate(version, rel.TagName) {
		fmt.Println("Already up to date!")
		os.Exit(0)
	}

	// Match asset for platform.
	assetName, assetURL, checksumURL, ok := matchReleaseAssets(rel.Assets, runtime.GOOS, runtime.GOARCH)
	if !ok {
		fmt.Fprintf(os.Stderr, "ERROR: no release asset found matching platform suffix %q\n", platformAssetSuffix(runtime.GOOS, runtime.GOARCH))
		os.Exit(1)
	}
	// S5: fail closed — a release without a checksums.txt asset is refused,
	// never installed unverified (previously verification was silently
	// skipped, contradicting the SECURITY.md "never proceed unverified").
	if err := requireChecksums(checksumURL); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Downloading %s ...\n", assetURL)
	// Independent deadline for the asset download: the 30s context above
	// stays with the release metadata fetch and the checksums verify, while
	// the up-to-64MB asset gets its own 5-minute window derived from
	// context.Background() so an exhausted metadata budget cannot kill the
	// download that the metadata already approved mid-transfer. All other
	// safety properties are unchanged: the body is still size-capped (S7),
	// the checksum is bound to the filename before any install, and the
	// temp-file swap runs on the same volume as the executable.
	assetCtx, assetCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer assetCancel()
	assetBytes, err := downloadURL(assetCtx, client, assetURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: download asset: %v\n", err)
		os.Exit(1)
	}

	if err := verifyChecksum(ctx, client, checksumURL, assetName, assetBytes); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Checksum verified successfully [ok]")

	// Extract binary
	binaryName := "freebuff-proxy"
	if runtime.GOOS == "windows" {
		binaryName = "freebuff-proxy.exe"
	}
	binaryBytes, err := extractBinaryFromArchive(assetURL, assetBytes, binaryName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	execPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: find current executable path: %v\n", err)
		os.Exit(1)
	}

	// Write the new binary to a temp file in the SAME directory as the
	// executable so the final swap is an atomic rename on the same volume.
	tmp, err := os.CreateTemp(filepath.Dir(execPath), filepath.Base(execPath)+".tmp-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: create temp file for updated binary: %v\n", err)
		os.Exit(1)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(binaryBytes); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		fmt.Fprintf(os.Stderr, "ERROR: write updated binary: %v\n", err)
		os.Exit(1)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		fmt.Fprintf(os.Stderr, "ERROR: close temp file: %v\n", err)
		os.Exit(1)
	}
	if err := os.Chmod(tmpPath, 0755); err != nil {
		_ = os.Remove(tmpPath)
		fmt.Fprintf(os.Stderr, "ERROR: set permissions on updated binary: %v\n", err)
		os.Exit(1)
	}

	deferredMsg, err := replaceExecutable(execPath, tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		fmt.Fprintf(os.Stderr, "ERROR: install updated binary: %v\n", err)
		os.Exit(1)
	}
	if deferredMsg != "" {
		// The swap is deferred: the helper runs after this process exits and
		// records the outcome in the result marker. Do not print a SUCCESS
		// line until that marker reads OK.
		fmt.Println(deferredMsg)
		os.Exit(0)
	}

	fmt.Printf("\nSUCCESS: freebuff-proxy updated to %s!\n", rel.TagName)
	fmt.Println("Please restart freebuff-proxy to run the new version.")
	os.Exit(0)
}

// reportUpdateResultMarker consumes the result marker left by a deferred
// Windows swap from a previous -update run: it prints the recorded outcome
// ("OK" or "FAILED: ...", see update_swap.go) and deletes the marker so it is
// reported exactly once. No-op when no marker exists. Returns the marker
// contents ("" when none) for tests.
func reportUpdateResultMarker(execPath string) string {
	marker := updateResultMarker(execPath)
	data, err := os.ReadFile(marker)
	if err != nil {
		return "" // no stale marker: no deferred swap, or already reported
	}
	result := strings.TrimSpace(string(data))
	if result != "" {
		fmt.Printf("Previous deferred update result: %s\n", result)
	}
	_ = os.Remove(marker)
	return result
}

func downloadURL(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// S7: read at most maxUpdateDownloadBytes+1 so an oversized response is
	// rejected instead of exhausting memory (previously unbounded ReadAll).
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUpdateDownloadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxUpdateDownloadBytes {
		return nil, fmt.Errorf("download exceeds %d-byte safety cap", maxUpdateDownloadBytes)
	}
	return body, nil
}

// verifyChecksum downloads checksums.txt and confirms it lists the sha256 of
// assetBytes BOUND TO assetFilename — the release asset being installed (S6).
// A bare hash match for a different filename must not pass: the checksum
// line only counts when both the hash and the filename match. The update
// must never proceed unverified (see .github/SECURITY.md), so a checksums.txt
// fetch failure aborts with an error instead of being silently skipped.
func verifyChecksum(ctx context.Context, client *http.Client, checksumURL, assetFilename string, assetBytes []byte) error {
	checksumBytes, err := downloadURL(ctx, client, checksumURL)
	if err != nil {
		return fmt.Errorf("download checksums.txt: %w", err)
	}
	computed := sha256.Sum256(assetBytes)
	computedHex := hex.EncodeToString(computed[:])
	// GoReleaser writes one "<sha256>  <filename>" pair per line (two-space
	// separator); a "*" prefix (sha256sum style) is tolerated too. Match
	// both fields: the hash of the downloaded bytes AND the asset filename.
	for _, line := range strings.Split(string(checksumBytes), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		hash, name := fields[0], strings.TrimPrefix(fields[1], "*")
		if strings.EqualFold(hash, computedHex) && name == assetFilename {
			return nil
		}
	}
	return fmt.Errorf("checksum mismatch! Calculated: %s for %s", computedHex, assetFilename)
}

// readArchiveEntry reads one release-archive member through a cap: entries
// larger than maxUpdateArchiveEntryBytes are rejected with "release archive
// entry too large" instead of being loaded unboundedly — and never
// installed truncated. kind ("zip" or "tar") only labels error messages.
func readArchiveEntry(r io.Reader, kind string) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxUpdateArchiveEntryBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s entry: %w", kind, err)
	}
	if len(b) > maxUpdateArchiveEntryBytes {
		return nil, fmt.Errorf("release archive entry too large (exceeds %d-byte cap)", maxUpdateArchiveEntryBytes)
	}
	return b, nil
}

// extractBinaryFromArchive returns the bytes of binaryName found anywhere in
// the release archive (a goreleaser zip or tar.gz; the binary may be nested
// under a versioned directory). An unreadable archive or an absent binary is
// an error — Run exits rather than installing garbage.
func extractBinaryFromArchive(assetURL string, assetBytes []byte, binaryName string) ([]byte, error) {
	if strings.HasSuffix(assetURL, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(assetBytes), int64(len(assetBytes)))
		if err != nil {
			return nil, fmt.Errorf("read zip: %w", err)
		}
		for _, f := range zr.File {
			if filepath.Base(f.Name) != binaryName {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("open zip file: %w", err)
			}
			b, readErr := readArchiveEntry(rc, "zip")
			_ = rc.Close()
			if readErr != nil {
				return nil, readErr
			}
			return b, nil
		}
		return nil, fmt.Errorf("binary %q not found in downloaded release archive", binaryName)
	}

	gzr, err := gzip.NewReader(bytes.NewReader(assetBytes))
	if err != nil {
		return nil, fmt.Errorf("read gzip: %w", err)
	}
	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar archive: %w", err)
		}
		if filepath.Base(hdr.Name) != binaryName {
			continue
		}
		b, readErr := readArchiveEntry(tr, "tar")
		if readErr != nil {
			return nil, readErr
		}
		return b, nil
	}
	return nil, fmt.Errorf("binary %q not found in downloaded release archive", binaryName)
}
