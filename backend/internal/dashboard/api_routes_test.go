package dashboard_test

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"freebuff-proxy/backend/internal/dashboard"
)

// --- AdminRoutes table shape ---

// TestAdminRoutesTableShape pins the table's own invariants: only GET/POST
// rows, known auth levels, and no duplicate (method, path) pairs (a
// duplicate would panic the mux at registration).
func TestAdminRoutesTableShape(t *testing.T) {
	type key struct{ method, path string }
	seen := make(map[key]bool)
	for _, r := range dashboard.AdminRoutes {
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			t.Errorf("%s %s: method %q not GET/POST", r.Method, r.Path, r.Method)
			continue
		}
		switch r.Auth {
		case dashboard.AuthNone, dashboard.AuthDashboard, dashboard.AuthSensitive, dashboard.AuthAdminToken:
		default:
			t.Errorf("%s %s: unknown auth level %q", r.Method, r.Path, r.Auth)
		}
		k := key{r.Method, r.Path}
		if seen[k] {
			t.Errorf("duplicate admin route %s %s", r.Method, r.Path)
		}
		seen[k] = true
	}
}

// --- frontend endpoint parity (issue #285) ---

var (
	exportRe = regexp.MustCompile(`export const (\w+) = \{([\s\S]*?)\};`)
	entryRe  = regexp.MustCompile(`(\w+)\s*:\s*(?:\([^)]*\)\s*=>\s*\x60([^\x60]*)\x60|'([^']*)'|"([^"]*)"|\x60([^\x60]*)\x60)`)
)

// readPathsMap parses frontend/src/lib/api/paths.js into export → member →
// value (a plain path or a template pattern carrying ${...} wildcards).
// Missing file = no map at all (every manifest JS row then fails loudly).
func readPathsMap(t *testing.T) map[string]map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("../../../frontend/src", "lib", "api", "paths.js"))
	if err != nil {
		t.Fatalf("read paths.js: %v", err)
	}
	out := map[string]map[string]string{}
	for _, m := range exportRe.FindAllStringSubmatch(string(data), -1) {
		for _, e := range entryRe.FindAllStringSubmatch(m[2], -1) {
			val := ""
			for i := 2; i <= 5; i++ {
				if e[i] != "" {
					val = e[i]
					break
				}
			}
			if out[m[1]] == nil {
				out[m[1]] = map[string]string{}
			}
			out[m[1]][e[1]] = val
		}
	}
	return out
}

// TestAdminManifestFrontendParity checks ONE machine-readable contract: the
// embedded admin_manifest.json. Every row with a JS entry must appear in
// frontend/src/lib/api/paths.js under the same export map and key with the
// same path (template rows use the ${...} wildcard shape); a missing key or
// a mismatched path fails loudly — no silent resolveArg continues, no
// wrapper-name heuristics.
func TestAdminManifestFrontendParity(t *testing.T) {
	routes, err := dashboard.ParseAdminManifestEntries()
	if err != nil {
		t.Fatalf("parse admin manifest: %v", err)
	}
	parsed := readPathsMap(t)
	prefix := "../../../frontend/src"
	for _, route := range routes {
		if route.JSExport == "" {
			continue
		}
		var wildcardPath string
		if strings.Contains(route.Path, "{") {
			// Template row: the client composes `/admin/tokens/${idx}/...`.
			wildcardPath = strings.ReplaceAll(strings.ReplaceAll(route.Path, "{id}", "${idx}"), "{key}", "${key}")
		} else {
			wildcardPath = route.Path
		}
		m, ok := parsed[route.JSExport]
		if !ok {
			t.Errorf("manifest row %s %s: paths.js has no %s map", route.Method, route.Path, route.JSExport)
			continue
		}
		got, ok := m[route.JSKey]
		if !ok {
			t.Errorf("manifest row %s %s: paths.js.%s missing key %q", route.Method, route.Path, route.JSExport, route.JSKey)
			continue
		}
		if got != wildcardPath {
			t.Errorf("manifest row %s %s: paths.js.%s.%s = %q, want %q", route.Method, route.Path, route.JSExport, route.JSKey, got, wildcardPath)
		}
	}
	_ = prefix
}
