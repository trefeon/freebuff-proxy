package config

// Tests for the shared env-file helpers (issue #234): the atomic writer
// with its Windows rename-retry/.bak-restore safety, and the line-edit
// contract of ApplyEnvUpdates.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/testutil"
)

func TestWriteFileAtomicRestoresBackupOnRenameFailure(t *testing.T) {
	dir := t.TempDir()
	// Drain before TempDir's own RemoveAll: Windows AV locks can leave a
	// stray .bak behind (the injected-failure path restores it), failing
	// the cleanup (see poll.go).
	testutil.DrainStrayTempFiles(t, dir)
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("OLD\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	realRename := osRename
	defer func() { osRename = realRename }()
	// Fail every rename whose SOURCE is the temp file WriteFileAtomic
	// mints (".env.tmp*"); the .bak aside/restore renames still run real.
	osRename = func(old, new string) error {
		if strings.Contains(filepath.Base(old), ".env.tmp") {
			return errors.New("injected rename failure")
		}
		return realRename(old, new)
	}

	if err := WriteFileAtomic(path, []byte("NEW\n")); err == nil {
		t.Fatal("WriteFileAtomic succeeded under injected rename failures, want error")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("target missing after failed write (data-loss window): %v", err)
	}
	if string(got) != "OLD\n" {
		t.Errorf("target content after failed write = %q, want %q", got, "OLD\n")
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Error(".bak left behind after the original was restored")
	}
	assertNoTmpFiles(t, dir, ".env")
}

func TestWriteFileAtomicPreservesBackupWhenRestoreFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("OLD\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	realRename := osRename
	defer func() { osRename = realRename }()
	osRename = func(old, new string) error {
		if strings.Contains(filepath.Base(old), ".env.tmp") || new == path {
			return errors.New("injected rename failure")
		}
		return realRename(old, new)
	}

	err := WriteFileAtomic(path, []byte("NEW\n"))
	if err == nil {
		t.Fatal("WriteFileAtomic succeeded under injected rename failures, want error")
	}
	if !strings.Contains(err.Error(), "restore") {
		t.Errorf("error = %v, want it to mention the .bak restore failure", err)
	}
	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf(".bak missing after total rename failure: %v", err)
	}
	if string(bak) != "OLD\n" {
		t.Errorf(".bak content = %q, want %q (data must survive in .bak)", bak, "OLD\n")
	}
	assertNoTmpFiles(t, dir, ".env")
}

func assertNoTmpFiles(t *testing.T, dir, base string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "."+base+".tmp*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, m := range matches {
		t.Errorf("stray temp file left behind: %s", m)
	}
}

func TestApplyEnvUpdatesContract(t *testing.T) {
	// Replace in place preserves comments and blank lines.
	out, err := ApplyEnvUpdates([]byte("# comment\nA=1\n\nB=2\n"), []EnvUpdate{{Key: "A", Value: "9"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "# comment\nA=9\n\nB=2\n" {
		t.Errorf("replace = %q", out)
	}
	// Missing key appends after the trailing newline.
	out, _ = ApplyEnvUpdates([]byte("A=1\n"), []EnvUpdate{{Key: "B", Value: "2"}})
	if string(out) != "A=1\nB=2\n" {
		t.Errorf("append = %q", out)
	}
	// Empty document becomes the sole line.
	out, _ = ApplyEnvUpdates(nil, []EnvUpdate{{Key: "A", Value: "1"}})
	if string(out) != "A=1" {
		t.Errorf("empty doc = %q", out)
	}
	// CRLF stays CRLF.
	out, _ = ApplyEnvUpdates([]byte("A=1\r\n"), []EnvUpdate{{Key: "B", Value: "2"}})
	if string(out) != "A=1\r\nB=2\r\n" {
		t.Errorf("crlf = %q", out)
	}
	// Newline inside a value is rejected.
	if _, err := ApplyEnvUpdates([]byte("A=1\n"), []EnvUpdate{{Key: "A", Value: "x\ny"}}); err == nil {
		t.Error("newline value accepted, want rejection")
	}
}

func TestWriteFileAtomicRefusesDirectoryTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(path, "keep.txt")
	if err := os.WriteFile(kept, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("NEW\n")); err == nil {
		t.Fatal("WriteFileAtomic over a non-empty directory succeeded, want error")
	}
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		t.Errorf("target dir missing or not a dir after failed write: %v", err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("target content lost after failed write: %v", err)
	}
	assertNoTmpFiles(t, dir, ".env")
}
