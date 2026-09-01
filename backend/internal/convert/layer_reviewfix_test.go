package convert

// Regression for the 2026-08-31 review P3 (convert package purity): convert
// claims to be pure JSON functions with no I/O and no internal dependencies
// beyond modelcat. The invariant was enforced by prose alone, and prose
// rotted (the package read os.Getenv directly). This guard mirrors config's
// TestConfigDoesNotImportTelemetry at the parser/importer level.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allowedInternal is the exact set of internal packages convert production
// files may import: stdlib always allowed, plus these. modelcat is the
// single current dependency (effort.go); reasoningcache and tokenestimate
// are pre-approved for the reasoning-restore and token-count paths so a
// future legitimate import does not trip the guard (issue #279).
var allowedInternal = map[string]bool{
	"freebuff-proxy/backend/internal/modelcat":       true,
	"freebuff-proxy/backend/internal/reasoningcache": true,
	"freebuff-proxy/backend/internal/tokenestimate":  true,
}

// TestConvertDoesNotImportPackageBoundary scans the package's non-test
// sources for internal imports: convert must not depend on any internal
// package beyond allowedInternal (it is a pure JSON transformation layer).
func TestConvertDoesNotImportPackageBoundary(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(path, "freebuff-proxy/backend/internal/") && !allowedInternal[path] {
				t.Errorf("%s imports %s; convert must not import internal packages beyond modelcat/reasoningcache/tokenestimate", name, path)
			}
		}
	}
}
