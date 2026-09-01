package config

// Env-file access helpers shared by the config loader and the dashboard
// admin surface (issue #234). Every read and write of the proxy .env file
// goes through ResolveEnvFile/EnvFileForWrite so the cwd-wins resolution
// rule has exactly one implementation, and ApplyEnvUpdates is the single
// read-modify-write line-edit contract.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EnvFileForWrite returns the path the proxy should write its .env file to:
// the resolved existing file (ResolveEnvFile), or the first candidate
// (./.env) when none exists — cwd stays authoritative per the resolution
// rule in EnvFileCandidates.
func EnvFileForWrite() string {
	if p := ResolveEnvFile(); p != "" {
		return p
	}
	candidates := EnvFileCandidates()
	return candidates[0]
}

// EnvFileInfo resolves and reads the proxy .env file in one call.
// exists is false when no candidate file is present.
func EnvFileInfo() (path string, content []byte, exists bool, err error) {
	path = ResolveEnvFile()
	if path == "" {
		return "", nil, false, nil
	}
	content, err = os.ReadFile(path)
	if err != nil {
		return path, nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	return path, content, true, nil
}

// EnvUpdate is one KEY=VALUE line edit to apply to a .env document.
type EnvUpdate struct {
	Key   string
	Value string
}

// ApplyEnvUpdates applies KEY=VALUE line edits to a .env document without
// touching the file. Rules (shared with the dashboard's JS editor):
//   - a key line is replaced in place, comments and blank lines preserved;
//   - a missing key is appended at the end (after the existing trailing
//     newline, if any);
//   - CRLF documents stay CRLF; LF documents stay LF;
//   - a newline inside a value is rejected (a .env value is one line).
func ApplyEnvUpdates(content []byte, updates []EnvUpdate) ([]byte, error) {
	crlf := bytes.Contains(content, []byte("\r"))
	lines := make([]string, 0, len(content)/8)
	for _, l := range strings.Split(string(content), "\n") {
		lines = append(lines, strings.TrimSuffix(l, "\r"))
	}
	// A file ending with a newline has a trailing "" split element that is
	// an artifact of that newline, not a real blank line; drop it so
	// appended keys do not land after a spurious blank line.
	trailingNL := len(content) > 0 && content[len(content)-1] == '\n'
	if trailingNL {
		if n := len(lines); n > 0 && lines[n-1] == "" {
			lines = lines[:n-1]
		}
	}
	for _, u := range updates {
		// A raw newline would inject a second .env line and a CR would
		// shred the file's line endings; reject before writing.
		if strings.ContainsAny(u.Value, "\r\n") {
			return nil, fmt.Errorf("%s value must not contain newlines (a .env value is one line)", u.Key)
		}
		line := u.Key + "=" + u.Value
		replaced := false
		for i, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), u.Key+"=") {
				lines[i] = line
				replaced = true
				break
			}
		}
		if !replaced {
			if n := len(lines); n == 1 && lines[0] == "" {
				// Empty (or missing) file: the new line is the whole file.
				lines[0] = line
			} else {
				lines = append(lines, line)
			}
		}
	}
	eol := "\n"
	if crlf {
		eol = "\r\n"
	}
	out := []byte(strings.Join(lines, eol))
	if trailingNL {
		out = append(out, eol...)
	}
	return out, nil
}

// WriteEnvFile atomically writes the proxy .env file to EnvFileForWrite
// (temp + rename, 0600 temp).
func WriteEnvFile(content []byte) error {
	return WriteFileAtomic(EnvFileForWrite(), content)
}

// WriteFileAtomic writes data to path via a temp file in the same
// directory and a rename, 0600 temp file, with the rename retried a few
// times. Rename-over-existing fails transiently on Windows when the target
// has a briefly-open handle (antivirus scanning the file we just wrote):
// without retries one transient lock turns into a rejected dashboard save
// or a lost token add (rollback after a failed .env write).

// osRename is the rename seam used by WriteFileAtomic so tests can inject
// rename failures (Windows rename-over-existing fails transiently on
// virus-scanned files; the seam reproduces it deterministically).
var osRename = os.Rename

func WriteFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	// A directory at the target can never be replaced by a rename (every
	// platform rejects it) and must not be moved aside: the .bak dance
	// below would then "succeed" by renaming the directory away.
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		_ = os.Remove(tmpName)
		return fmt.Errorf("cannot replace %s: target is a directory", path)
	}
	// Rename-over-existing fails transiently on Windows when the target
	// has a briefly-open handle (antivirus scanning the file we just
	// wrote). Without retries, a transient lock turns into a rejected
	// dashboard save or a lost token add (rollback after a failed .env
	// write). The fallback renames the EXISTING target to path+".bak"
	// first — never removes it — so a failure between the two renames
	// cannot lose the old content: worst case the data sits in .bak and is
	// restored on the next attempt (or recovered by hand).
	const renameAttempts = 5
	var lastErr error
	for i := range renameAttempts {
		if err := osRename(tmpName, path); err != nil {
			lastErr = err
		} else {
			// Success; drop a stale .bak left by an interrupted run.
			_ = os.Remove(path + ".bak")
			return nil
		}
		if _, statErr := os.Stat(path); statErr == nil {
			// The target exists but the rename-over failed: move it aside
			// first (the target itself may be locked, so this too can
			// fail — retried next round), then retry the temp rename.
			if err := osRename(path, path+".bak"); err != nil {
				lastErr = err
			} else {
				if err := osRename(tmpName, path); err != nil {
					lastErr = err
				} else {
					_ = os.Remove(path + ".bak")
					return nil
				}
			}
		}
		// After any failure the original content may now sit in .bak while
		// the target is absent; restore it before the next attempt so the
		// target never stays missing on a retryable error.
		if _, statErr := os.Stat(path); statErr != nil {
			if _, bakErr := os.Stat(path + ".bak"); bakErr == nil {
				if err := osRename(path+".bak", path); err != nil {
					lastErr = errors.Join(lastErr, fmt.Errorf("restore %s from %s: %w", path, path+".bak", err))
				}
			}
		}
		time.Sleep(time.Duration(i+1) * 20 * time.Millisecond)
	}
	// The temp file may itself be transiently locked (antivirus scan of a
	// file we just wrote); retry its removal so a failed write cannot leak
	// a .env.tmp* file into the directory.
	for range 3 {
		if os.Remove(tmpName) == nil {
			return lastErr
		}
		time.Sleep(20 * time.Millisecond)
	}
	return lastErr
}
