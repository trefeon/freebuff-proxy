package dashboard_test

import (
	"encoding/json"
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

// --- frontend endpoint parity ---

var (
	exportRe  = regexp.MustCompile(`export const (\w+) = \{([\s\S]*?)\};`)
	entryRe   = regexp.MustCompile(`(\w+)\s*:\s*(?:\([^)]*\)\s*=>\s*)?(?:'([^']*)'|"([^"]*)"|\x60([^\x60]*)\x60)`)
	callRe    = regexp.MustCompile(`\b(fetch|fetchAPI|postForm|postAPI|triggerAction|triggerTokenAction)\s*\(\s*([^,()]+(?:\([^()]*\)[^,()]*)?)`)
	literalRe = regexp.MustCompile(`(['"\x60])(/admin[^'"\x60]*)(['"\x60])`)
	memberRe  = regexp.MustCompile(`^(\w+)\.(\w+)(?:\([^)]*\))?$`)
	interpRe  = regexp.MustCompile(`\$\{([^}]*)\}`)
	methodRe  = regexp.MustCompile(`method\s*:\s*['"](POST|GET)['"]`)
)

const frontendSrc = "../../../frontend/src"

// stripComments removes // and /* */ comments so doc examples and prose in
// the source tree never look like live endpoint calls.
func stripComments(src string) string {
	var out strings.Builder
	inBlock := false
	for _, line := range strings.Split(src, "\n") {
		if inBlock {
			if idx := strings.Index(line, "*/"); idx >= 0 {
				inBlock = false
				line = line[idx+2:]
			} else {
				continue
			}
		}
		if idx := strings.Index(line, "/*"); idx >= 0 {
			inBlock = true
			out.WriteString(line[:idx])
			out.WriteString("\n")
			continue
		}
		if idx := strings.Index(line, "//"); idx >= 0 {
			before := line[:idx]
			if !strings.ContainsAny(before, "'\"`") {
				line = before
			}
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	return out.String()
}

// readPathsMap parses frontend/src/lib/api/paths.js into export → member →
// value. The value is the raw literal: a plain path or a template pattern
// carrying ${...} wildcards (tokenActions style). Missing file = era 0
// (no map yet); missing file is fine.
func readPathsMap(t *testing.T) map[string]map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(frontendSrc, "lib", "api", "paths.js"))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]map[string]string{}
		}
		t.Fatalf("read paths.js: %v", err)
	}
	out := map[string]map[string]string{}
	for _, m := range exportRe.FindAllStringSubmatch(string(data), -1) {
		for _, e := range entryRe.FindAllStringSubmatch(m[2], -1) {
			val := e[2]
			if val == "" {
				val = e[3]
			}
			if val == "" {
				val = e[4]
			}
			if out[m[1]] == nil {
				out[m[1]] = map[string]string{}
			}
			out[m[1]][e[1]] = val
		}
	}
	return out
}

// normalizeWildcards replaces every ${...} interpolation with a wildcard
// marker and strips query strings/fragments, so template patterns compare
// structurally.
func normalizeWildcards(p string) string {
	p = interpRe.ReplaceAllString(p, "{x}")
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	return p
}

// patternSegments splits a (normalized) path into segments; a segment that
// is a Go mux wildcard ({id}/{key}) or an interpolation marker is "{}".
func patternSegments(p string) []string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if strings.Contains(s, "{") || strings.Contains(s, "}") {
			segs[i] = "{}"
		}
	}
	return segs
}

// matchRow reports whether a route row covers the literal pattern: same
// segment count, and each position is either the row's wildcard, the
// literal's wildcard, or identical text.
func matchRow(route dashboard.AdminRoute, lit []string) bool {
	row := patternSegments(route.Path)
	if len(row) != len(lit) {
		return false
	}
	for i := range row {
		if row[i] == "{}" || lit[i] == "{}" {
			continue
		}
		if row[i] != lit[i] {
			return false
		}
	}
	return true
}

// endpoint is one required (method, path-pattern) pair from the SPA. An
// empty method means the call context did not pin one (endpoint map
// entries, pathname comparisons): the path must still be registered.
type endpoint struct {
	method string
	path   string
	where  string
}

// resolveArg turns a call argument into a path pattern. It accepts raw
// string literals, template literals with ${...} (paths.js references are
// substituted, anything else becomes a wildcard), and paths.js member
// expressions including tokenActions-style functions.
func resolveArg(arg string, maps map[string]map[string]string) (string, bool) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", false
	}
	if arg[0] == '\'' || arg[0] == '"' {
		if len(arg) >= 2 && arg[len(arg)-1] == arg[0] {
			return arg[1 : len(arg)-1], true
		}
		return "", false
	}
	if arg[0] == '`' {
		end := strings.Index(arg[1:], "`")
		if end < 0 {
			return "", false
		}
		tpl := arg[1 : end+1]
		// Substitute resolvable paths.js references inside the template;
		// everything else (idx placeholders, runtime locals) is a wildcard.
		return interpRe.ReplaceAllStringFunc(tpl, func(full string) string {
			inner := strings.TrimSpace(full[2 : len(full)-1])
			if m := memberRe.FindStringSubmatch(inner); m != nil {
				if val, ok := maps[m[1]][m[2]]; ok && !strings.Contains(val, "${") {
					return val
				}
			}
			return "{x}"
		}), true
	}
	m := memberRe.FindStringSubmatch(arg)
	if m == nil {
		return "", false
	}
	val, ok := maps[m[1]][m[2]]
	if !ok {
		return "", false
	}
	return val, true
}

// methodFor derives the HTTP method of a call: the POST helpers are POST;
// fetch/fetchAPI default to GET unless the options object sets method.
func methodFor(wrapper, rest string) string {
	switch wrapper {
	case "postAPI", "postForm", "triggerAction", "triggerTokenAction":
		return http.MethodPost
	}
	window := rest
	if len(window) > 200 {
		window = window[:200]
	}
	if m := methodRe.FindStringSubmatch(window); m != nil {
		return m[1]
	}
	return http.MethodGet
}

// TestAdminRoutesParity scans the SPA for admin endpoints — raw literals
// and the paths.js endpoint map plus its call sites — and asserts every
// one is registered in AdminRoutes, with a matching method wherever the
// call context pins one.
func TestAdminRoutesParity(t *testing.T) {
	maps := readPathsMap(t)
	var endpoints []endpoint
	seen := map[string]bool{}

	// Walk 1: call-site endpoints (wrapper + argument resolution).
	err := filepath.WalkDir(frontendSrc, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".js" && ext != ".svelte" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		src := stripComments(string(data))
		for _, m := range callRe.FindAllStringSubmatchIndex(src, -1) {
			wrapper := src[m[2]:m[3]]
			argText := src[m[4]:m[5]]
			lit, ok := resolveArg(argText, maps)
			if !ok || !strings.HasPrefix(lit, "/admin") {
				continue
			}
			norm := normalizeWildcards(lit)
			method := methodFor(wrapper, src[m[5]:])
			key := method + " " + norm
			if !seen[key] {
				seen[key] = true
				endpoints = append(endpoints, endpoint{method: method, path: norm, where: path})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk frontend: %v", err)
	}

	// Walk 2: every admin path literal anywhere in the source (pathname
	// comparisons, era-0 raw fetches, the paths.js map values themselves).
	err = filepath.WalkDir(frontendSrc, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".js" && ext != ".svelte" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		src := stripComments(string(data))
		for _, m := range literalRe.FindAllStringSubmatch(src, -1) {
			if m[1] != m[3] {
				continue
			}
			norm := normalizeWildcards(m[2])
			if norm == "{x}" {
				continue
			}
			key := " " + norm
			if !seen[key] {
				seen[key] = true
				endpoints = append(endpoints, endpoint{path: norm, where: path})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk frontend literals: %v", err)
	}

	for _, e := range endpoints {
		lit := patternSegments(e.path)
		match := false
		for _, r := range dashboard.AdminRoutes {
			if e.method != "" && r.Method != e.method {
				continue
			}
			if matchRow(r, lit) {
				match = true
				break
			}
		}
		if !match {
			t.Errorf("%s: endpoint %s (method %q) is not registered in AdminRoutes", e.where, e.path, e.method)
		}
	}

	// Sentinels: endpoints the SPA actually calls, with the method the call
	// context pins. These prove the scanner exercises call sites (method
	// derivation) rather than only the literal sweep — especially
	// /admin/login/status, whose map group implies POST while the call and
	// the registered route are GET.
	for _, want := range []endpoint{
		{method: http.MethodGet, path: "/admin/api/models"},
		{method: http.MethodGet, path: "/admin/api/version"},
		{method: http.MethodPost, path: "/admin/tokens/add"},
		{method: http.MethodPost, path: "/admin/config"},
		{method: http.MethodGet, path: "/admin/login/status"},
		{method: http.MethodPost, path: "/admin/tokens/{x}/unlock"},
	} {
		found := false
		for _, e := range endpoints {
			if e.method == want.method && e.path == want.path {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("SPA endpoint %s %s not found by scanner (call site missing or method derivation broken)", want.method, want.path)
		}
	}
}

// --- models payload quota ---

// TestModelsPageQuota pins the /admin/api/models quota column fallback
// contract (no live pool quota in this fixture): premium pool rows carry
// the "5 premium quota" label, GLM 5.2 the referral label, and every other
// served row is "unlimited session". With live quota data the premium column
// instead shows "<limit> premium quota" from the wire snapshot (quotaFor
// prefers livePremiumQuotaLabel). Contract: glm-5.3-flash == "unlimited
// session" when no live data exists.
func TestModelsPageQuota(t *testing.T) {
	ts := newDashboardForPages(t, false, "models")
	resp, err := http.Get(ts.URL + "/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var data map[string]any
	if err := json.Unmarshal(mustReadAll(t, resp), &data); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	rows, _ := data["models"].([]any)
	if len(rows) == 0 {
		t.Fatal("models array should not be empty")
	}
	quotaBy := map[string]string{}
	for _, m := range rows {
		row, _ := m.(map[string]any)
		id, _ := row["id"].(string)
		quota, _ := row["quota"].(string)
		quotaBy[id] = quota
	}
	want := map[string]string{
		"z-ai/glm-5.3-flash":         "unlimited session",
		"deepseek/deepseek-v4-flash": "unlimited session",
		"mimo/mimo-v2.5":             "unlimited session",
		"openai/gpt-5.6-luna":        "5 premium quota",
		"upstage/solar-pro4":         "5 premium quota",
		"z-ai/glm-5.2":               "referral +1/day",
	}
	for id, wantQuota := range want {
		got, ok := quotaBy[id]
		if !ok {
			t.Errorf("models payload missing %q", id)
			continue
		}
		if got != wantQuota {
			t.Errorf("quota for %s = %q, want %q", id, got, wantQuota)
		}
	}
}
