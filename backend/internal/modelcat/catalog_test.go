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
}

// resolveModelRef resolves a FREEBUFF_*_MODEL_ID constant name OR a model
// row name (like OX_ALPHA_MODEL) to its wire id, consulting the parsed TS
// constants first and falling back to the hardcoded row table.
func resolveModelRef(t *testing.T, ids map[string]string, ref string) string {
	t.Helper()
	if v, ok := ids[ref]; ok {
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

	// Premium set.
	premiumRefs := parseList(t, modelsSrc, "FREEBUFF_PREMIUM_MODEL_IDS")
	premium := map[string]bool{}
	for _, ref := range premiumRefs {
		premium[resolveModelRef(t, ids, ref)] = true
	}
	for _, m := range Catalog {
		if m.Premium != premium[m.ID] {
			t.Errorf("catalog premium[%q] = %v, upstream FREEBUFF_PREMIUM_MODEL_IDS = %v", m.ID, m.Premium, premium[m.ID])
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
// the served set, the shared premium pool (Luna + Solar Pro 4; GLM 5.3
// Flash is unmetered), the paused map (all three withdrawn rows recommend
// the default model), per-model caps (none at this pin), and per-model
// effort ladders. This pins what the doc comments CLAIM so a stale claim
// (e.g. "GLM 5.3 Flash is premium") fails here before an operator reads it.
func TestCatalogFactsPinned(t *testing.T) {
	// Served set, catalog order.
	wantServed := []string{
		"openai/gpt-5.6-luna",
		"upstage/solar-pro4",
		"z-ai/glm-5.2",
		"z-ai/glm-5.3-flash",
		"deepseek/deepseek-v4-flash",
		"mimo/mimo-v2.5",
	}
	if got := ServedIDs(); !slices.Equal(got, wantServed) {
		t.Errorf("ServedIDs() = %v, want %v", got, wantServed)
	}

	// Shared premium pool = Luna + Solar Pro 4 (both metered by the shared
	// 5/day pool; solar additionally has a per-model 1/day cap
	// solar_pro4). GLM 5.3 Flash unmetered.
	wantPremium := []string{"openai/gpt-5.6-luna", "upstage/solar-pro4"}
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

	// Paused map: each withdrawn row recommends the default model.
	wantPaused := map[string]string{
		"stealth/ox-alpha":         DefaultModelID,
		"deepseek/deepseek-v4-pro": DefaultModelID,
		"minimax/minimax-m3":       DefaultModelID,
	}
	if got := PausedMap(); !maps.Equal(got, wantPaused) {
		t.Errorf("PausedMap() = %v, want %v", got, wantPaused)
	}

	// Per-model caps: Solar Pro 4 is the only capped row (freebuff-
	// spend-ceilings.ts experimental 1-session cap, freebuff-model-
	// availability.ts copy; pin a6be463).
	for _, id := range ServedIDs() {
		limit, pool := PerModelCap(id)
		if id == "upstage/solar-pro4" {
			if limit != 1 || pool != "solar_pro4" {
				t.Errorf("PerModelCap(%q) = (%d, %q), want (1, \"solar_pro4\")", id, limit, pool)
			}
			continue
		}
		if limit != 0 || pool != "" {
			t.Errorf("PerModelCap(%q) = (%d, %q), want (0, \"\")", id, limit, pool)
		}
	}

	// Effort ladders for served models (nil = the route ignores it).
	wantEfforts := map[string][]string{
		"openai/gpt-5.6-luna":        {"low", "medium", "high", "xhigh", "max"},
		"deepseek/deepseek-v4-flash": {"low", "high", "max"},
		"mimo/mimo-v2.5":             {"high"},
		"upstage/solar-pro4":         nil,
		"z-ai/glm-5.2":               nil,
		"z-ai/glm-5.3-flash":         {"low", "high", "max"},
	}
	for id, want := range wantEfforts {
		if got := Efforts(id); !slices.Equal(got, want) {
			t.Errorf("Efforts(%q) = %v, want %v", id, got, want)
		}
	}
}
