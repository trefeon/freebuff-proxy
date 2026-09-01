package refreshtoken

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

// TestUpdateEnvKeysAt pins the atomic .env key rewriter (issue #66):
// existing KEY= lines are replaced in place, absent keys are appended,
// comments and unrelated lines survive, the file lands 0600, and a missing
// file errors.
func TestUpdateEnvKeysAt(t *testing.T) {
	dir := t.TempDir()
	// Drain before TempDir's own RemoveAll: Windows AV locks can leave a
	// stray .env.tmp* behind that would fail the cleanup (see poll.go).
	testutil.DrainStrayTempFiles(t, dir)
	envPath := filepath.Join(dir, ".env")
	original := "# comment stays\nAUTH_TOKENS=tok-old-1,tok-old-2\n# another comment\nLISTEN_ADDR=127.0.0.1:3457\n"
	if err := os.WriteFile(envPath, []byte(original), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, err := updateEnvKeysAt(envPath, []envUpdate{
		{Key: "AUTH_TOKENS", Value: "tok-new-1,tok-new-2"},
		{Key: "SAFE_MODE", Value: "true"},
	})
	if err != nil {
		t.Fatalf("updateEnvKeysAt: %v", err)
	}

	got := string(out)
	for _, want := range []string{
		"# comment stays",
		"AUTH_TOKENS=tok-new-1,tok-new-2",
		"# another comment",
		"LISTEN_ADDR=127.0.0.1:3457",
		"SAFE_MODE=true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "tok-old") {
		t.Errorf("old token value still present:\n%s", got)
	}

	if runtime.GOOS != "windows" {
		fi, err := os.Stat(envPath)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %v, want 0600", fi.Mode().Perm())
		}
	}
}

// TestUpdateEnvKeysAtMissingFile asserts a missing .env is an error (the
// caller must have a token source).
func TestUpdateEnvKeysAtMissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := updateEnvKeysAt(filepath.Join(dir, "no-such.env"), []envUpdate{{Key: "AUTH_TOKENS", Value: "x"}})
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
