// Package archtest pins the backend package dependency matrix.
//
// This is the "is the architecture still legal?" gate. Every internal package
// declares the exact set of internal packages its non-test files may import;
// stdlib is always allowed, everything else is forbidden until the matrix is
// deliberately extended. The matrix below was derived from the actual import
// graph on 2026-09-06 (verified via `go list`) and matches the committed
// layer rules in docs/architecture/BOUNDARIES.md (bottom: leaves, top: server).
//
// Why allowlists and not "forbidden edges": a permissive rule set lets a new
// package or a new edge slip through as "not yet forbidden". An allowlist
// fails loudly on ANY unknown edge or package, forcing a conscious decision.
//
// Rule summary:
//   - Leaves (config, modelcat, clicreds, egress, notify, phasetiming,
//     ratelimit, reasoningcache, stealth, tokenestimate, tokenhealth,
//     updatecheck): zero internal imports. They own facts; they do not know
//     about the rest of the system.
//   - convert: pure JSON transformation. Only modelcat (+ pre-approved
//     reasoningcache/tokenestimate for the reasoning-restore and token-count
//     paths, issue #279).
//   - Middle layers (registry, telemetry, logring, testutil, upstream,
//     session, runs, pool, dashboard): exactly the edges below. Never upward
//     into server.
//   - server: top of the internal stack, imports everything.
//   - cmd/*: entrypoints, imports everything.
//
// A dependency that is not in this file is not allowed. Extending the matrix
// is an architecture decision: update this table, cite why, and keep the
// existing per-package guards (config/layer_imports_test.go,
// convert/layer_reviewfix_test.go) consistent.

package archtest

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const modulePrefix = "freebuff-proxy/backend/"

// allowed maps an internal package path (relative to backend/) to the set of
// internal packages it may import. nil or missing value means "none".
// Keys must match the actual package dirs; unknown dirs fail the test.
var allowed = map[string][]string{
	// ---- bottom layer: own facts, know nothing ----
	"internal/clicreds":       {},
	"internal/config":         {}, // bottom layer; see config/layer_imports_test.go
	"internal/modelcat":       {}, // owns per-model truth, stdlib only
	"internal/egress":         {},
	"internal/notify":         {},
	"internal/phasetiming":    {},
	"internal/ratelimit":      {},
	"internal/reasoningcache": {},
	"internal/stealth":        {},
	"internal/tokenestimate":  {}, // o200k_base BPE, stdlib only
	"internal/tokenhealth":    {},
	"internal/updatecheck":    {},

	// ---- layer 1: small dependents of config/leaves ----
	"internal/telemetry": {"internal/config"},
	"internal/logring":   {"internal/telemetry"},
	"internal/registry":  {"internal/config", "internal/modelcat"},
	"internal/testutil":  {"internal/config"},

	// ---- layer 2: protocol/wire ----
	"internal/convert": { // pure JSON transforms; see convert/layer_reviewfix_test.go
		"internal/modelcat",
		"internal/reasoningcache", // pre-approved, issue #279
		"internal/tokenestimate",  // pre-approved, issue #279
	},
	"internal/upstream": {
		"internal/config",
		"internal/stealth",
		"internal/telemetry",
		"internal/upstream/login",
	},
	"internal/upstream/login": {},
	"internal/upstream/testmock": { // mock codebuff server for tests
		"internal/modelcat",
		"internal/upstream",
	},

	// ---- layer 3: stateful middle ----
	"internal/session": {"internal/telemetry", "internal/upstream"},
	"internal/runs":    {"internal/session", "internal/upstream"},

	// ---- layer 4: orchestration ----
	"internal/pool": {
		"internal/config",
		"internal/modelcat",
		"internal/notify",
		"internal/phasetiming",
		"internal/registry",
		"internal/runs",
		"internal/session",
		"internal/upstream",
	},
	"internal/dashboard": { // embedded admin API; must never import server
		"internal/config",
		"internal/logring",
		"internal/modelcat",
		"internal/phasetiming",
		"internal/pool",
		"internal/registry",
		"internal/updatecheck",
		"internal/upstream",
	},

	// ---- layer 5+: top of the stack ----
	"internal/server": { // orchestration point; imports everything below
		"internal/config",
		"internal/convert",
		"internal/dashboard",
		"internal/logring",
		"internal/modelcat",
		"internal/phasetiming",
		"internal/pool",
		"internal/ratelimit",
		"internal/reasoningcache",
		"internal/registry",
		"internal/runs",
		"internal/session",
		"internal/telemetry",
		"internal/tokenestimate",
		"internal/updatecheck",
		"internal/upstream",
	},
	"internal/cli": {
		"internal/cli/port",
		"internal/clicreds",
		"internal/config",
		"internal/logring",
		"internal/notify",
		"internal/pool",
		"internal/registry",
		"internal/server",
		"internal/session",
		"internal/telemetry",
		"internal/updatecheck",
		"internal/upstream",
	},
	"internal/cli/port":    {},
	"internal/cli/setup":   {},
	"internal/cli/update":  {},
	"internal/cli/service": {},
	"internal/cli/doctor": { // -doctor diagnostics
		"internal/config",
		"internal/egress",
		"internal/registry",
		"internal/upstream",
	},
	"internal/cli/refreshtoken": {"internal/config", "internal/upstream"},
	"internal/cli/validate":     {"internal/config", "internal/upstream"},
}

func normalize(p string) string {
	p = filepath.ToSlash(p)
	if strings.HasPrefix(p, modulePrefix) {
		return strings.TrimPrefix(p, modulePrefix)
	}
	return p
}

// collect walks dir recursively and returns non-test .go files.
func collect(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".go") && !strings.HasSuffix(d.Name(), "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

// TestBackendDependencyMatrix scans every non-test Go file under backend/
// (internal + cmd) and fails on any internal import not in the allowed matrix.
func TestBackendDependencyMatrix(t *testing.T) {
	// Resolve backend root from this file's own path, never from the
	// working directory: `go test` runs with cwd set to the package dir,
	// but a compiled test binary inherits its invoker's cwd — a relative
	// "../.." from the wrong cwd escapes the repo (observed live: walk
	// reached D:\$RECYCLE.BIN and died on access denied).
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	files, err := collect(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no Go files found under backend/ — wrong working directory?")
	}

	fset := token.NewFileSet()
	seen := map[string]bool{}
	for _, file := range files {
		rel, err := filepath.Rel(root, file)
		if err != nil {
			t.Fatal(err)
		}
		rel = filepath.ToSlash(rel)
		// Only enforce on internal/ and cmd/ trees.
		if !strings.HasPrefix(rel, "internal/") && !strings.HasPrefix(rel, "cmd/") {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		// cmd/* are thin entrypoints; they may import everything (server/cli
		// already forbid upward edges internally).
		if strings.HasPrefix(dir, "cmd/") {
			continue
		}

		f, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		seen[dir] = true
		if dir == "internal/archtest" {
			continue // self
		}
		for _, imp := range f.Imports {
			path := normalize(strings.Trim(imp.Path.Value, `"`))
			if !strings.HasPrefix(path, "internal/") && !strings.HasPrefix(path, "cmd/") {
				continue
			}
			allowedSet := allowed[dir]
			ok := false
			for _, a := range allowedSet {
				if a == path {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("%s imports %s — not in the allowed dependency matrix (backend/internal/archtest/arch_test.go). Extend the matrix deliberately or the architecture guard stays red.", dir, path)
			}
		}
	}

	// Every package in the matrix must actually exist — a stale entry means
	// the matrix rotted and a new package may have been added unguarded.
	for pkg := range allowed {
		if _, ok := seen[pkg]; !ok {
			// Packages with zero internal imports still appear via seen (any
			// non-test file registers its dir). A missing dir = renamed or
			// deleted package; update the matrix.
			t.Errorf("matrix entry %q has no non-test Go files — package renamed or removed; update arch_test.go", pkg)
		}
	}
}

// TestKnownForbiddenEdges pins the advisor-flagged edges explicitly so a
// regression carries a readable reason instead of a bare matrix error.
func TestKnownForbiddenEdges(t *testing.T) {
	cases := []struct {
		from, to string
		why      string
	}{
		{"internal/convert", "internal/server", "convert is a pure JSON transformation layer"},
		{"internal/pool", "internal/dashboard", "pool must not know about the admin surface"},
		{"internal/modelcat", "internal/upstream", "modelcat owns model facts and must not depend on the wire"},
		{"internal/registry", "internal/server", "registry resolves models; it is not an orchestration point"},
		{"internal/upstream", "internal/session", "upstream is the wire client; session manages state on top of it"},
		{"internal/session", "internal/pool", "pool consumes sessions; reverse would be a cycle"},
		{"internal/dashboard", "internal/server", "server mounts dashboard; dashboard must not call back"},
	}
	for _, tc := range cases {
		for _, a := range allowed[tc.from] {
			if a == tc.to {
				t.Errorf("forbidden edge %s -> %s: %s", tc.from, tc.to, tc.why)
			}
		}
	}
}
