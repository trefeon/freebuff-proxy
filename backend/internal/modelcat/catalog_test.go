package modelcat

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The parity test below re-reads the pinned upstream snapshot
// (backend/internal/registry/testdata/upstream/*.ts, copied from
// reference/freebuff/common/src/constants on every sync) and asserts the
// catalog table matches it constant-for-constant. An upstream sync that
// changes a catalog fact WITHOUT updating this table fails here — that is
// the drift tripwire the whole package exists to provide.

const testdataDir = "../registry/testdata/upstream"

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testdataDir, name))
	if err != nil {
		t.Fatalf("read %s: %v (run scripts/sync-upstream.sh --test-all first)", name, err)
	}
	return string(b)
}

// modelIDs parses freebuff-model-ids.ts AND freebuff-models.ts into
// FREEBUFF_*_MODEL_ID → wire id (upstream splits the constants across both
// files).
func modelIDs(t *testing.T) map[string]string {
	t.Helper()
	re := regexp.MustCompile(`export const (\w+) =\s*'([^']+)'`)
	out := map[string]string{}
	for _, name := range []string{"freebuff-model-ids.ts", "freebuff-models.ts"} {
		src := readTestdata(t, name)
		for _, m := range re.FindAllStringSubmatch(src, -1) {
			out[m[1]] = m[2]
		}
	}
	return out
}

// pinnedRowIDs maps SUPPORTED_FREEBUFF_MODELS row names to wire model IDs.
// Updated with every upstream sync. The parse-based resolution above catches
// most; rows with indirect IDs (e.g. MIMO_V25_MODEL -> mimoModels.mimoV25 in
// model-config.ts) are hardcoded here.
var pinnedRowIDs = map[string]string{
	"OX_ALPHA_MODEL":          "stealth/ox-alpha",
	"DEEPSEEK_V4_PRO_MODEL":   "deepseek/deepseek-v4-pro",
	"MINIMAX_M3_MODEL":        "minimax/minimax-m3",
	"GPT_5_6_LUNA_MODEL":      "openai/gpt-5.6-luna",
	"GLM_V52_MODEL":           "z-ai/glm-5.2",
	"GLM_V53_FLASH_MODEL":     "z-ai/glm-5.3-flash",
	"DEEPSEEK_V4_FLASH_MODEL": "deepseek/deepseek-v4-flash",
	"MIMO_V25_MODEL":          "mimo/mimo-v2.5",
	"FABLE_5_MODEL":           "anthropic/claude-fable-5",
	"SOLAR_PRO_4_MODEL":       "upstage/solar-pro4",
	"GEMINI_38_FLASH_MODEL":   "google/gemini-3.8-flash",
}

// idAliases mirrors ID constants upstream moved out of literal form (the
// test's literal parser cannot follow them; the registry parser resolves
// them via member access). Update on sync when the literal set moves.
var idAliases = map[string]string{
	// FREEBUFF_SOLAR_PRO_4_MODEL_ID is now
	// FREEBUFF_SOLAR_PRO_4_ENTITLEMENT.modelId (entitlements file).
	"FREEBUFF_SOLAR_PRO_4_MODEL_ID": "upstage/solar-pro4",
}

// resolveModelRef resolves a FREEBUFF_*_MODEL_ID constant name OR a model
// row name (like OX_ALPHA_MODEL) to its wire id, consulting the parsed TS
// constants first and falling back to the hardcoded row table.
func resolveModelRef(t *testing.T, ids map[string]string, ref string) string {
	t.Helper()
	if v, ok := ids[ref]; ok {
		return v
	}
	if v, ok := idAliases[ref]; ok {
		return v
	}
	if strings.HasSuffix(ref, "_MODEL") && !strings.HasPrefix(ref, "FREEBUFF_") {
		if v, ok := pinnedRowIDs[ref]; ok {
			return v
		}
		if v, ok := ids["FREEBUFF_"+strings.TrimSuffix(ref, "_MODEL")+"_MODEL_ID"]; ok {
			return v
		}
	}
	t.Fatalf("unresolved model id reference %q", ref)
	return ""
}

// parseList parses `export const NAME = [ ...items... ] as const` items
// (single identifiers or strings on their own lines), handling nested
// brackets inside conditional spreads.
func parseList(t *testing.T, src, name string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?s)export const ` + regexp.QuoteMeta(name) + `[^=]*=\s*\[`)
	m := re.FindStringIndex(src)
	if m == nil {
		t.Fatalf("constant %s not found in pinned snapshot", name)
	}
	// Balance brackets to find the matching closing ]. Scan from the
	// opening bracket itself so depth 0 lands on the real closing one.
	start := m[1] - 1
	depth := 0
	body := ""
scan:
	for i := start; i < len(src); i++ {
		switch src[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				body = src[start+1 : i]
				break scan
			}
		}
	}
	if body == "" {
		t.Fatalf("constant %s has unbalanced brackets", name)
	}
	var items []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ","))
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		// conditional spread: ...(COND ? [REF] : [])
		if strings.HasPrefix(line, "...") {
			ref := strings.TrimSpace(strings.TrimPrefix(line, "..."))
			if i := strings.LastIndex(ref, "["); i >= 0 {
				ref = ref[i+1:]
			}
			if i := strings.Index(ref, "]"); i >= 0 {
				ref = ref[:i]
			}
			if ref != "" {
				items = append(items, strings.TrimSpace(ref))
			}
			continue
		}
		items = append(items, line)
	}
	return items
}

// rowPremiumFlag resolves the `premium:` field of the model row object
// `const NAME = { ... }` to its boolean value: literals directly, member
// chains (FREEBUFF_X_ENTITLEMENT.fullAccess.premium) through the
// entitlements source. found=false when the row has no premium field.
func rowPremiumFlag(modelsSrc, extraSrc, name string) (val, found bool) {
	re := regexp.MustCompile(`(?s)const ` + regexp.QuoteMeta(name) + `\s*=\s*\{`)
	m := re.FindStringIndex(modelsSrc)
	if m == nil {
		return false, false
	}
	depth := 0
	body := ""
	for i := m[1] - 1; i < len(modelsSrc); i++ {
		switch modelsSrc[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				body = modelsSrc[m[1]:i]
			}
		}
		if body != "" {
			break
		}
	}
	fm := regexp.MustCompile(`(?m)^\s*premium:\s*([^,\n]+),?\s*$`).FindStringSubmatch(body)
	if fm == nil {
		return false, false
	}
	expr := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(fm[1]), ","))
	switch expr {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	// Member chain into the entitlements source (e.g. solar's
	// FREEBUFF_SOLAR_PRO_4_ENTITLEMENT.fullAccess.premium).
	scope := extraSrc
	parts := strings.Split(expr, ".")
	for i, p := range parts {
		var anchor string
		if i == 0 {
			mm := regexp.MustCompile(`(?s)const ` + regexp.QuoteMeta(p) + `\s*=\s*\{`).FindString(scope)
			if mm == "" {
				return false, false
			}
			anchor = mm
		} else {
			mm := regexp.MustCompile(`(?s)\b` + regexp.QuoteMeta(p) + `\s*:\s*\{?`).FindString(scope)
			if mm == "" {
				return false, false
			}
			anchor = mm
		}
		scope = scope[strings.Index(scope, anchor)+len(anchor)-1:]
	}
	tail := strings.TrimSpace(scope)
	if strings.HasPrefix(tail, "{") {
		return false, false
	}
	if strings.HasPrefix(tail, "true") {
		return true, true
	}
	if strings.HasPrefix(tail, "false") {
		return false, true
	}
	return false, false
}
func TestCatalogParityWithPinnedUpstream(t *testing.T) {
	ids := modelIDs(t)
	modelsSrc := readTestdata(t, "freebuff-models.ts")

	// SUPPORTED_FREEBUFF_MODELS must exactly match the catalog rows (order
	// preserved).
	supported := parseList(t, modelsSrc, "SUPPORTED_FREEBUFF_MODELS")
	if len(supported) != len(Catalog) {
		t.Fatalf("catalog has %d rows, pinned SUPPORTED_FREEBUFF_MODELS has %d\ncatalog: %v\npinned: %v",
			len(Catalog), len(supported), catalogIDs(), supported)
	}
	for i, ref := range supported {
		want := resolveModelRef(t, ids, ref)
		if Catalog[i].ID != want {
			t.Errorf("catalog row %d = %q, want %q (SUPPORTED_FREEBUFF_MODELS order)", i, Catalog[i].ID, want)
		}
	}

	// Paused set.
	pausedRefs := parseList(t, modelsSrc, "FREEBUFF_PAUSED_FREE_MODEL_IDS")
	for _, ref := range pausedRefs {
		id := resolveModelRef(t, ids, ref)
		if !IsPaused(id) {
			t.Errorf("catalog not paused for %q, upstream FREEBUFF_PAUSED_FREE_MODEL_IDS lists it", id)
		}
	}
	for _, m := range Catalog {
		if m.PausedReplacement != "" {
			found := false
			for _, ref := range pausedRefs {
				if resolveModelRef(t, ids, ref) == m.ID {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("catalog pauses %q but upstream FREEBUFF_PAUSED_FREE_MODEL_IDS does not", m.ID)
			}
		}
	}
	// Every paused replacement must be the upstream default model (the
	// refusal copy names it).
	for _, m := range Catalog {
		if m.PausedReplacement != "" && m.PausedReplacement != DefaultModelID {
			t.Errorf("paused %q replacement = %q, want %q (upstream default)", m.ID, m.PausedReplacement, DefaultModelID)
		}
	}

	// Premium set. Upstream derives FREEBUFF_PREMIUM_MODEL_IDS from the row
	// flags, but our Premium means live shared-pool metering: served rows
	// mirror the resolved upstream flag (literals directly, entitlement
	// member chains through the entitlements source), while paused
	// (withdrawn) rows must NOT be premium — they cannot consume the pool,
	// whatever vestigial flag their frozen row object keeps.
	entSrc := readTestdata(t, "freebuff-model-entitlements.ts")
	premium := map[string]bool{}
	for _, ref := range supported {
		id := resolveModelRef(t, ids, ref)
		if v, ok := rowPremiumFlag(modelsSrc, entSrc, ref); ok && v && IsServed(id) {
			premium[id] = true
		}
	}
	for _, m := range Catalog {
		if m.Premium != premium[m.ID] {
			t.Errorf("catalog premium[%q] = %v, want %v (served upstream flag)", m.ID, m.Premium, premium[m.ID])
		}
	}

	// Default model: FREEBUFF_MODELS[0] must be DefaultModelID.
	defaultModels := parseList(t, modelsSrc, "FREEBUFF_MODELS")
	if len(defaultModels) == 0 {
		t.Fatal("FREEBUFF_MODELS empty in pinned snapshot")
	}
	if want := resolveModelRef(t, ids, defaultModels[0]); want != DefaultModelID {
		t.Errorf("DefaultModelID = %q, want %q (FREEBUFF_MODELS[0])", DefaultModelID, want)
	}
	// Fallback model: FALLBACK_FREEBUFF_MODEL_ID.
	fallbackRe := regexp.MustCompile(`export const FALLBACK_FREEBUFF_MODEL_ID = '([^']+)'`)
	if fm := fallbackRe.FindStringSubmatch(modelsSrc); fm != nil {
		if fm[1] != FallbackModelID {
			t.Errorf("FallbackModelID = %q, want %q (upstream FALLBACK_FREEBUFF_MODEL_ID)", FallbackModelID, fm[1])
		}
	}

	// Per-model caps.
	capsRe := regexp.MustCompile(`\[(\w+)\]:\s*\{[\s\S]*?limit:\s*(\d+),[\s\S]*?pool:\s*'([^']+)'`)
	seen := 0
	for _, m := range capsRe.FindAllStringSubmatch(modelsSrc, -1) {
		id := resolveModelRef(t, ids, m[1])
		wantLimit, _ := strconv.Atoi(m[2])
		gotLimit, gotPool := PerModelCap(id)
		if gotLimit != wantLimit || gotPool != m[3] {
			t.Errorf("cap[%q] = (%d, %q), want (%d, %q)", id, gotLimit, gotPool, wantLimit, m[3])
		}
		seen++
	}
	capCount := 0
	for _, c := range Catalog {
		if c.Cap > 0 {
			capCount++
		}
	}
	if seen != capCount {
		t.Errorf("capped rows = %d, upstream FREEBUFF_PER_MODEL_SESSION_CAPS entries = %d", capCount, seen)
	}

	// Context windows — extract entries inside FREEBUFF_MODEL_CONTEXT_WINDOWS
	// (balance braces to find the map body).
	ctxOpen := regexp.MustCompile(`FREEBUFF_MODEL_CONTEXT_WINDOWS[^=]*=\s*\{`)
	if cm := ctxOpen.FindStringIndex(modelsSrc); cm != nil {
		ctxStart := cm[1] - 1
		cdepth := 0
		var ctxBody string
	inner:
		for i := ctxStart; i < len(modelsSrc); i++ {
			switch modelsSrc[i] {
			case '{':
				cdepth++
			case '}':
				cdepth--
				if cdepth == 0 {
					ctxBody = modelsSrc[ctxStart+1 : i]
					break inner
				}
			}
		}
		ctxSeen := 0
		ctxRe := regexp.MustCompile(`\[(\w+)\]:\s*([\d_]+)`)
		for _, m := range ctxRe.FindAllStringSubmatch(ctxBody, -1) {
			id := resolveModelRef(t, ids, m[1])
			if byID(id) == nil {
				continue // not a catalog row (god-only/web models like luna-es, muse-spark)
			}
			want, _ := strconv.Atoi(strings.ReplaceAll(m[2], "_", ""))
			if got := ContextWindow(id); got != want {
				t.Errorf("context window[%q] = %d, want %d", id, got, want)
			}
			ctxSeen++
		}
		_ = ctxSeen
	}

	// Scalar constants.
	scalarChecks := []struct {
		name string
		got  int
	}{
		{"FREEBUFF_PREMIUM_SESSION_LIMIT", PremiumSessionLimit},
	}
	for _, sc := range scalarChecks {
		re := regexp.MustCompile(`export const ` + sc.name + ` = (\d+)`)
		if m := re.FindStringSubmatch(modelsSrc); m != nil {
			want, _ := strconv.Atoi(m[1])
			if sc.got != want {
				t.Errorf("%s = %d, want %d", sc.name, sc.got, want)
			}
		}
	}
	// GLM session length (60 * 60 * 1000).
	if m := regexp.MustCompile(`export const FREEBUFF_GLM_V52_SESSION_LENGTH_MS = ([\d *]+)`).FindStringSubmatch(modelsSrc); m != nil {
		want, err := strconv.Atoi(strings.ReplaceAll(m[1], " ", ""))
		if err == nil && int(GLMSessionLength.Milliseconds()) != want {
			t.Errorf("GLMSessionLength = %dms, want %dms", GLMSessionLength.Milliseconds(), want)
		}
	}
}

func catalogIDs() []string {
	out := make([]string, len(Catalog))
	for i := range Catalog {
		out[i] = Catalog[i].ID
	}
	return out
}

// TestCatalogFactsPinned asserts the documented catalog reality directly:
// the served set, the shared premium pool (Luna + Muse Spark 1.3 since
// 2026-09-04; GLM 5.3 Flash is unmetered), the paused map (all four
// withdrawn rows recommend the default model), per-model caps (none at
// this pin), and per-model effort ladders. This pins what the doc comments
// CLAIM so a stale claim (e.g. "GLM 5.3 Flash is premium") fails here
// before an operator reads it.
func TestCatalogFactsPinned(t *testing.T) {
	// Served set, catalog order.
	wantServed := []string{
		"openai/gpt-5.6-luna",
		"upstage/solar-pro4",
		"meta/muse-spark-1.3-contributor",
		"z-ai/glm-5.3-flash",
		"deepseek/deepseek-v4-flash",
		"mimo/mimo-v2.5",
	}
	if got := ServedIDs(); !slices.Equal(got, wantServed) {
		t.Errorf("ServedIDs() = %v, want %v", got, wantServed)
	}

	// Shared premium pool = Luna + Muse Spark 1.3 since 2026-09-04 (solar's
	// entitlement went unmetered; gemini is Pro-paywalled and cannot consume
	// the pool). GLM 5.3 Flash unmetered.
	wantPremium := []string{"openai/gpt-5.6-luna", "meta/muse-spark-1.3-contributor"}
	if got := SharedPremiumModels(); !slices.Equal(got, wantPremium) {
		t.Errorf("SharedPremiumModels() = %v, want %v", got, wantPremium)
	}
	if IsPremium("z-ai/glm-5.3-flash") {
		t.Error("glm-5.3-flash marked premium, want unmetered (not premium)")
	}
	for _, id := range wantPremium {
		if !IsPremium(id) {
			t.Errorf("IsPremium(%q) = false, want true (shared premium pool)", id)
		}
	}
	wantPaused := map[string]string{
		"stealth/ox-alpha":         DefaultModelID,
		"deepseek/deepseek-v4-pro": DefaultModelID,
		"minimax/minimax-m3":       DefaultModelID,
		"z-ai/glm-5.2":             DefaultModelID,
	}
	if got := PausedMap(); !maps.Equal(got, wantPaused) {
		t.Errorf("PausedMap() = %v, want %v", got, wantPaused)
	}

	// Per-model count caps: none at this pin. Upstream
	// FREEBUFF_PER_MODEL_SESSION_CAPS is EMPTY — solar's 1/day trial cap
	// closed 2026-09-01 (upstream 051fd4d9, its count cap came off; the
	// per-session $ spend ceiling stays upstream-side).
	for _, id := range ServedIDs() {
		limit, pool := PerModelCap(id)
		if limit != 0 || pool != "" {
			t.Errorf("PerModelCap(%q) = (%d, %q), want (0, \"\")", id, limit, pool)
		}
	}

	// Effort ladders for served models (nil = the route ignores it).
	wantEfforts := map[string][]string{
		"openai/gpt-5.6-luna":             {"low", "medium", "high", "xhigh", "max"},
		"meta/muse-spark-1.3-contributor": {"minimal", "low", "medium", "high", "xhigh"},
		"deepseek/deepseek-v4-flash":      {"low", "high", "max"},
		"mimo/mimo-v2.5":                  {"high"},
		"upstage/solar-pro4":              nil,
		"z-ai/glm-5.2":                    nil,
		"z-ai/glm-5.3-flash":              {"low", "high", "max"},
	}
	for id, want := range wantEfforts {
		if got := Efforts(id); !slices.Equal(got, want) {
			t.Errorf("Efforts(%q) = %v, want %v", id, got, want)
		}
	}
}
