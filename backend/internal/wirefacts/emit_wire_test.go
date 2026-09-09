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

// TestEmitWireUpToDate regenerates wirecodes_gen.go and notices_gen.go in
// memory and requires them to equal the committed files byte for byte.
func TestEmitWireUpToDate(t *testing.T) {
	t.Parallel()
	var wireBuf, noticesBuf bytes.Buffer
	if err := EmitWire(testUpstream, testWireDir, &wireBuf, &noticesBuf); err != nil {
		t.Fatalf("EmitWire: %v", err)
	}
	for _, tc := range []struct {
		path string
		got  []byte
	}{
		{"../upstream/wirecodes_gen.go", wireBuf.Bytes()},
		{"../upstream/notices_gen.go", noticesBuf.Bytes()},
	} {
		want, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("read %s: %v (regenerate via go run ./backend/cmd/wiregen -upstream %s)", tc.path, err, testUpstream)
		}
		if !bytes.Equal(tc.got, want) {
			t.Errorf("%s is stale: regenerate via go run ./backend/cmd/wiregen -upstream %s and commit the result", tc.path, testUpstream)
		}
	}
}

// wireTestDir stages copies of the four wire snapshots plus a manifest with
// matching hashes, then applies mutate to the staged tree.
func wireTestDir(t *testing.T, mutate func(dir string)) string {
	t.Helper()
	dir := t.TempDir()
	files := []string{wireSessionFile, wireCeilingsFile, wireAvailFile, wirePeakFile}
	type entry struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	}
	var manifest struct {
		UpstreamSHA   string  `json:"upstream_sha"`
		VendorVersion string  `json:"vendor_version"`
		Files         []entry `json:"files"`
	}
	manifest.UpstreamSHA, manifest.VendorVersion = testUpstream, "test"
	for _, f := range files {
		src, err := os.ReadFile(filepath.Join(testWireDir, filepath.FromSlash(f)))
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, src, 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(src)
		manifest.Files = append(manifest.Files, entry{f, hex.EncodeToString(sum[:])})
	}
	if mutate != nil {
		mutate(dir)
		// Re-hash after mutation so the failure under test is the content
		// check, not the manifest hash gate.
		for i, e := range manifest.Files {
			src, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(e.Path)))
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(src)
			manifest.Files[i].SHA256 = hex.EncodeToString(sum[:])
		}
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshots.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestEmitWireFailsExplicit proves each drift class fails with its marker
// and the commit, emitting nothing to either output.
func TestEmitWireFailsExplicit(t *testing.T) {
	t.Parallel()
	replace := func(rel, old, neu string) func(string) {
		return func(dir string) {
			p := filepath.Join(dir, filepath.FromSlash(rel))
			src, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			out := bytes.ReplaceAll(src, []byte(old), []byte(neu))
			if bytes.Equal(out, src) {
				t.Fatalf("fixture anchor %q not found in %s", old, rel)
			}
			if err := os.WriteFile(p, out, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(string)
		want   string
	}{
		{"unknown status", func(dir string) {
			p := filepath.Join(dir, filepath.FromSlash(wireSessionFile))
			src, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			src = append(src, []byte("\n  | {\n      status: 'zzz_unknown_code'\n    }\n")...)
			if err := os.WriteFile(p, src, 0o644); err != nil {
				t.Fatal(err)
			}
		}, "zzz_unknown_code"},
		{"notice drift", replace(wireCeilingsFile, "sustained automated abuse", "sustained automated misuse"), "FREEBUFF_CAPACITY_NOTICE"},
		{"peak drift", replace(wirePeakFile, "[6, 10]", "[6, 11]"), "DEEPSEEK_PEAK_HOUR_RANGES_UTC"},
	} {
		dir := wireTestDir(t, tc.mutate)
		var wireBuf, noticesBuf bytes.Buffer
		err := EmitWire(testUpstream, dir, &wireBuf, &noticesBuf)
		if err == nil {
			t.Fatalf("%s: EmitWire succeeded, want error containing %q", tc.name, tc.want)
			return
		}
		if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), testUpstream) {
			t.Fatalf("%s: error = %q, want %q plus the commit", tc.name, err, tc.want)
		}
		if wireBuf.Len() != 0 || noticesBuf.Len() != 0 {
			t.Fatalf("%s: wrote %d/%d bytes on error, want nothing", tc.name, wireBuf.Len(), noticesBuf.Len())
		}
	}
}
