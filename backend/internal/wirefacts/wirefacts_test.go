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

const (
	testUpstream = "78a7ab4ed6754a694a7699091e03f5f9bfee45d9"
	testWireDir  = "testdata/wire"
	testRegDir   = "../registry/testdata/upstream"
	testGenFile  = "wirefacts_gen.go"
)

// TestGeneratedUpToDate regenerates the facts file in memory and requires it
// to equal the checked-in wirefacts_gen.go byte for byte. Any SHA-manifest
// mismatch (edited snapshots.json, re-pinned wire file, new registry pin
// content) fails here until the generator is re-run via go generate.
func TestGeneratedUpToDate(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := Run(testUpstream, testWireDir, testRegDir, &buf); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want, err := os.ReadFile(testGenFile)
	if err != nil {
		t.Fatalf("read %s: %v (run go generate ./backend/internal/wirefacts/)", testGenFile, err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("%s is stale: regenerate with go generate ./backend/internal/wirefacts/ and commit the result", testGenFile)
	}
}

// TestGeneratedPinsManifest cross-checks the generated tables against the
// manifest on disk: every wire entry must resolve to a real snapshot file
// with a matching hash, and every registry entry to a real mirror file.
func TestGeneratedPinsManifest(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(testWireDir, "snapshots.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if m.UpstreamSHA != UpstreamSHA {
		t.Fatalf("manifest upstream_sha %q != generated UpstreamSHA %q", m.UpstreamSHA, UpstreamSHA)
	}
	if len(WireFiles) != len(m.Files) {
		t.Fatalf("WireFiles has %d entries, manifest has %d", len(WireFiles), len(m.Files))
	}
	for i, f := range m.Files {
		if WireFiles[i] != f {
			t.Fatalf("WireFiles[%d] = %+v, manifest has %+v", i, WireFiles[i], f)
		}
		src, err := os.ReadFile(filepath.Join(testWireDir, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatalf("read snapshot %s: %v", f.Path, err)
		}
		sum := sha256.Sum256(src)
		if got := hex.EncodeToString(sum[:]); got != f.SHA256 {
			t.Fatalf("snapshot %s hash %s != manifest %s", f.Path, got, f.SHA256)
		}
	}
	if len(RegistryPins) != len(RegistryFiles) {
		t.Fatalf("RegistryPins has %d entries, want %d", len(RegistryPins), len(RegistryFiles))
	}
	for i, name := range RegistryFiles {
		if RegistryPins[i].Path != name {
			t.Fatalf("RegistryPins[%d].Path = %q, want %q", i, RegistryPins[i].Path, name)
		}
		src, err := os.ReadFile(filepath.Join(testRegDir, name))
		if err != nil {
			t.Fatalf("read registry pin %s: %v", name, err)
		}
		sum := sha256.Sum256(src)
		if got := hex.EncodeToString(sum[:]); got != RegistryPins[i].SHA256 {
			t.Fatalf("registry pin %s hash %s != generated %s", name, got, RegistryPins[i].SHA256)
		}
	}
}

// TestUnknownConstructFailsExplicit proves the fail-explicit gate: a snapshot
// with a construct outside the known set fails the run with file:line, the
// construct, and the commit — and emits nothing. (The non-zero process exit
// itself comes from log.Fatalf in cmd/wiregen, which writes the output file
// only after a successful Run.)
func TestUnknownConstructFailsExplicit(t *testing.T) {
	t.Parallel()
	bad, err := os.ReadFile("testdata/unknown-construct.ts")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	wireDir := t.TempDir()
	regDir := t.TempDir()
	sum := sha256.Sum256(bad)
	m := manifest{
		UpstreamSHA:   "0000000000000000000000000000000000000001",
		VendorVersion: "0.0.0-test",
		Files:         []WireFile{{Path: "unknown-construct.ts", SHA256: hex.EncodeToString(sum[:])}},
	}
	raw, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(wireDir, "snapshots.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wireDir, "unknown-construct.ts"), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range RegistryFiles {
		if err := os.WriteFile(filepath.Join(regDir, name), []byte("// test pin\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	err = Run(m.UpstreamSHA, wireDir, regDir, &out)
	if err == nil {
		t.Fatal("Run succeeded on unknown construct, want explicit failure")
		return
	}
	msg := err.Error()
	if !strings.Contains(msg, "unknown-construct.ts:5") {
		t.Fatalf("error %q lacks file:line unknown-construct.ts:5", msg)
	}
	if !strings.Contains(msg, "namespace") {
		t.Fatalf("error %q lacks the offending construct", msg)
	}
	if !strings.Contains(msg, m.UpstreamSHA) {
		t.Fatalf("error %q lacks the upstream commit", msg)
	}
	if out.Len() != 0 {
		t.Fatalf("failed run emitted %d bytes, want nothing", out.Len())
	}
}

// TestUpstreamFlagMismatch proves an -upstream value that disagrees with the
// manifest fails before any snapshot is even read.
func TestUpstreamFlagMismatch(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := Run("ffffffffffffffffffffffffffffffffffffffff", testWireDir, testRegDir, &out)
	if err == nil || !strings.Contains(err.Error(), "does not match manifest") {
		t.Fatalf("Run with wrong SHA = %v, want manifest-mismatch failure", err)
		return
	}
	if out.Len() != 0 {
		t.Fatalf("failed run emitted %d bytes, want nothing", out.Len())
	}
}
