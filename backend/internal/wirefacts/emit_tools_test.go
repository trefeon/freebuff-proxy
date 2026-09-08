package wirefacts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEmitToolsUpToDate regenerates the convert tool-names table in memory
// and requires it to equal the checked-in toolnames_gen.go byte for byte.
func TestEmitToolsUpToDate(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := EmitTools(testUpstream, testWireDir, testRegDir, &buf); err != nil {
		t.Fatalf("EmitTools: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("..", "convert", "toolnames_gen.go"))
	if err != nil {
		t.Fatalf("read toolnames_gen.go: %v (run the wiregen tools emitter and commit the result)", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("toolnames_gen.go is stale: regenerate via EmitTools and commit the result")
	}
}

// toolsFixtureStage copies the consumed snapshots into a temp wire dir with a
// matching manifest, applying content overrides first.
func toolsFixtureStage(t *testing.T, override map[string]func(string) string) (wireDir, regDir, sha string) {
	t.Helper()
	wireDir, regDir = t.TempDir(), t.TempDir()
	sha = "0000000000000000000000000000000000000002"
	var files []WireFile
	for _, rel := range []string{toolsConstantsPath, foreignSignalsPath, sessionTypesPath} {
		raw, err := os.ReadFile(filepath.Join(testWireDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if rw, ok := override[rel]; ok {
			raw = []byte(rw(string(raw)))
		}
		if err := os.MkdirAll(filepath.Join(wireDir, filepath.Dir(filepath.FromSlash(rel))), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wireDir, filepath.FromSlash(rel)), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		files = append(files, WireFile{Path: rel, SHA256: hex.EncodeToString(sum[:])})
	}
	manifestRaw, _ := json.Marshal(manifest{UpstreamSHA: sha, VendorVersion: "0.0.0-test", Files: files})
	if err := os.WriteFile(filepath.Join(wireDir, "snapshots.json"), manifestRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range RegistryFiles {
		if err := os.WriteFile(filepath.Join(regDir, name), []byte("// test pin\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return wireDir, regDir, sha
}

// TestEmitToolsFailsExplicit proves every drift path emits nothing and names
// file, field, and commit: an unknown session status, a removed envelope
// param, and a wrong -upstream SHA.
func TestEmitToolsFailsExplicit(t *testing.T) {
	t.Parallel()
	t.Run("unknown status", func(t *testing.T) {
		t.Parallel()
		wireDir, regDir, sha := toolsFixtureStage(t, map[string]func(string) string{
			sessionTypesPath: func(s string) string {
				return strings.Replace(s, "status: 'superseded'", "status: 'time_travel'", 1)
			},
		})
		var out bytes.Buffer
		err := EmitTools(sha, wireDir, regDir, &out)
		if err == nil {
			t.Fatal("EmitTools succeeded on unknown session status, want explicit failure")
		}
		for _, want := range []string{sessionTypesPath, "time_travel", sha} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q lacks %q", err.Error(), want)
			}
		}
		if out.Len() != 0 {
			t.Errorf("failed run emitted %d bytes, want nothing", out.Len())
		}
	})
	t.Run("missing const", func(t *testing.T) {
		t.Parallel()
		wireDir, regDir, sha := toolsFixtureStage(t, map[string]func(string) string{
			toolsConstantsPath: func(s string) string {
				return strings.Replace(s, "export const toolNameParam", "export const renamedParam", 1)
			},
		})
		var out bytes.Buffer
		err := EmitTools(sha, wireDir, regDir, &out)
		if err == nil {
			t.Fatal("EmitTools succeeded on missing toolNameParam, want explicit failure")
		}
		for _, want := range []string{toolsConstantsPath, "toolNameParam", sha} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q lacks %q", err.Error(), want)
			}
		}
		if out.Len() != 0 {
			t.Errorf("failed run emitted %d bytes, want nothing", out.Len())
		}
	})
	t.Run("upstream mismatch", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		err := EmitTools("ffffffffffffffffffffffffffffffffffffffff", testWireDir, testRegDir, &out)
		if err == nil || !strings.Contains(err.Error(), "does not match manifest") {
			t.Fatalf("EmitTools with wrong SHA = %v, want manifest-mismatch failure", err)
		}
	})
}
