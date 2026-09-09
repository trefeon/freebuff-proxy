// Catalog emission for cmd/wiregen (Wave D slice 2).
//
// EmitCatalog derives backend/internal/modelcat/catalog_gen.go from the pinned
// registry snapshots in registryDir (backend/internal/registry/testdata/
// upstream/*.ts). Those snapshots are read-only inputs: this file never
// writes them, and any construct it does not understand fails the run
// explicitly (file plus the upstream commit) instead of emitting a
// half-read table.
//
// What flows from the snapshots: SUPPORTED order and row ids, Served
// (FREEBUFF_MODELS membership), Paused (FREEBUFF_PAUSED_FREE_MODEL_IDS),
// Premium (served rows mirror the resolved row flag; paused rows never are),
// context windows, effort ladders, display names,
// taglines, training notices, multimodal/isNew badges, and the default,
// fallback, session-length and context-default scalars.
//
// What stays curated in pinned tables below (with the snapshot precondition
// each pin asserts): the display-trim set (withdrawn/never-served rows drop
// picker copy), the solar promo strings (freebuff-solar-promo.ts is not a
// mirrored snapshot), the GLM 5.2 referral copy, the "max*" badge star, the
// 1.3 contributor window inheritance, and LimitedModelID (upstream split the
// limited hero from the coercion target on 2026-09-07; the proxy admission
// gate still allows only the unlimited fallback row, so the pin follows
// FALLBACK, not upstream LIMITED, until deliberately revisited).
package wirefacts

import (
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// EmitCatalog renders the modelcat generated source for upstreamSHA from the
// registry mirror files in registryDir. Out receives nothing on error.
func EmitCatalog(upstreamSHA, registryDir string, out io.Writer) error {
	c, err := loadCatalogInputs(registryDir, upstreamSHA)
	if err != nil {
		return err
	}
	rows, err := buildCatalogRows(c)
	if err != nil {
		return err
	}
	src, err := format.Source(renderCatalog(upstreamSHA, c, rows))
	if err != nil {
		return fmt.Errorf("wiregen: format catalog source: %w", err)
	}
	_, err = out.Write(src)
	return err
}

// catalogInputs are the parsed snapshot facts every row and scalar derives from.
type catalogInputs struct {
	commit      string
	ids         map[string]string   // const/member refs -> wire id or bool word
	bools       map[string]bool     // UI flag consts
	strArrays   map[string][]string // effort ladders (SUPPORTED/MODELS/PAUSED kept separate)
	rowNames    []string            // SUPPORTED_FREEBUFF_MODELS order
	served      map[string]bool     // FREEBUFF_MODELS membership by row name
	pausedNames []string            // FREEBUFF_PAUSED_FREE_MODEL_IDS row refs
	fields      map[string]map[string]string
	ctx         map[string]int // wire id -> context window
	defaultID   string
	fallbackID  string
	glm52ID     string
	glm53ID     string
	solarID     string
	defaultCtx  int
}

func loadCatalogInputs(registryDir, commit string) (*catalogInputs, error) {
	read := func(name string) (string, error) {
		b, err := os.ReadFile(filepath.Join(registryDir, name))
		if err != nil {
			return "", fmt.Errorf("wiregen: read registry pin %s: %w", name, err)
		}
		return string(b), nil
	}
	models, err := read("freebuff-models.ts")
	if err != nil {
		return nil, err
	}
	idsSrc, err := read("freebuff-model-ids.ts")
	if err != nil {
		return nil, err
	}
	entSrc, err := read("freebuff-model-entitlements.ts")
	if err != nil {
		return nil, err
	}
	cfgSrc, err := read("model-config.ts")
	if err != nil {
		return nil, err
	}
	c := &catalogInputs{
		commit: commit,
		served: map[string]bool{},
		ctx:    map[string]int{},
	}
	c.ids = parseTSStrings(idsSrc + "\n" + models)
	c.bools = parseTSBools(models)
	c.strArrays = parseTSArrays(models)
	for k, v := range parseMimoMembers(cfgSrc) {
		c.ids["mimoModels."+k] = v
	}
	if m := regexp.MustCompile(`modelId:\s*'([^']+)'`).FindStringSubmatch(entSrc); m != nil {
		c.ids["FREEBUFF_SOLAR_PRO_4_ENTITLEMENT.modelId"] = "'" + m[1] + "'"
		c.ids["FREEBUFF_SOLAR_PRO_4_MODEL_ID"] = "'" + m[1] + "'"
	} else {
		return nil, fmt.Errorf("wiregen: freebuff-model-entitlements.ts: no modelId literal at upstream commit %s", commit)
	}
	if m := regexp.MustCompile(`fullAccess:\s*\{\s*premium:\s*(true|false)`).FindStringSubmatch(entSrc); m != nil {
		c.ids["FREEBUFF_SOLAR_PRO_4_ENTITLEMENT.fullAccess.premium"] = m[1]
	} else {
		return nil, fmt.Errorf("wiregen: freebuff-model-entitlements.ts: no fullAccess.premium flag at upstream commit %s", commit)
	}
	resolve := func(ref, what string) (string, error) {
		return resolveCatalogRef(c.ids, ref, what, commit)
	}
	if c.rowNames, err = parseNameList(models, "SUPPORTED_FREEBUFF_MODELS", commit); err != nil {
		return nil, err
	}
	modelsNames, err := parseModelsList(models, c.bools, commit)
	if err != nil {
		return nil, err
	}
	for _, n := range modelsNames {
		c.served[n] = true
	}
	if c.pausedNames, err = parseNameList(models, "FREEBUFF_PAUSED_FREE_MODEL_IDS", commit); err != nil {
		return nil, err
	}
	// Row objects for every SUPPORTED row; each row's id registers its name.
	c.fields = map[string]map[string]string{}
	for _, n := range c.rowNames {
		f, err := parseRowFields(models, n, commit)
		if err != nil {
			return nil, err
		}
		c.fields[n] = f
		idRef, ok := f["id"]
		if !ok {
			return nil, fmt.Errorf("wiregen: freebuff-models.ts: row %s has no id field at upstream commit %s", n, commit)
		}
		id, err := resolve(idRef, "row "+n+" id")
		if err != nil {
			return nil, err
		}
		c.ids[n] = "'" + id + "'"
	}
	// Context windows: [REF]: N entries.
	ctxBody, ok := balancedBody(models, "FREEBUFF_MODEL_CONTEXT_WINDOWS", commit)
	if !ok {
		return nil, fmt.Errorf("wiregen: freebuff-models.ts: no FREEBUFF_MODEL_CONTEXT_WINDOWS map at upstream commit %s", commit)
	}
	for _, m := range regexp.MustCompile(`\[(\w+)\]:\s*([\d_]+)`).FindAllStringSubmatch(ctxBody, -1) {
		id, err := resolve(m[1], "context window key")
		if err != nil {
			return nil, err
		}
		v, _ := strconv.Atoi(strings.ReplaceAll(m[2], "_", ""))
		c.ctx[id] = v
	}
	// Scalar ids.
	for _, s := range []struct {
		dst  *string
		name string
	}{
		{&c.defaultID, "DEFAULT_FREEBUFF_MODEL_ID"},
		{&c.fallbackID, "FALLBACK_FREEBUFF_MODEL_ID"},
		{&c.glm52ID, "FREEBUFF_GLM_V52_MODEL_ID"},
		{&c.glm53ID, "FREEBUFF_GLM_V53_FLASH_MODEL_ID"},
		{&c.solarID, "FREEBUFF_SOLAR_PRO_4_MODEL_ID"},
	} {
		ref := singleRef(models, s.name)
		if ref == "" {
			// Defined outside freebuff-models.ts (solar's id lives in the
			// entitlements mirror); resolve from the collected consts.
			var ok bool
			if ref, ok = c.ids[s.name]; !ok {
				return nil, fmt.Errorf("wiregen: no %s const at upstream commit %s", s.name, commit)
			}
		}
		v, err := resolve(ref, s.name)
		if err != nil {
			return nil, err
		}
		*s.dst = v
	}
	// Reward session length must stay exactly one hour; anything else is a
	// deliberate revisit, not a silent carry.
	if m := regexp.MustCompile(`export const FREEBUFF_REWARD_SESSION_LENGTH_MS = ([\d *]+)`).FindStringSubmatch(models); m != nil {
		var prod int64 = 1
		for _, p := range strings.Split(m[1], "*") {
			v, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("wiregen: freebuff-models.ts: bad REWARD_SESSION_LENGTH_MS %q at upstream commit %s", m[1], commit)
			}
			prod *= v
		}
		if prod != 3_600_000 {
			return nil, fmt.Errorf("wiregen: freebuff-models.ts: REWARD_SESSION_LENGTH_MS is %dms, want 3600000ms at upstream commit %s", prod, commit)
		}
	} else {
		return nil, fmt.Errorf("wiregen: freebuff-models.ts: no FREEBUFF_REWARD_SESSION_LENGTH_MS at upstream commit %s", commit)
	}
	if m := regexp.MustCompile(`export const FREEBUFF_DEFAULT_CONTEXT_WINDOW = ([\d_]+)`).FindStringSubmatch(models); m != nil {
		c.defaultCtx, _ = strconv.Atoi(strings.ReplaceAll(m[1], "_", ""))
	} else {
		return nil, fmt.Errorf("wiregen: freebuff-models.ts: no FREEBUFF_DEFAULT_CONTEXT_WINDOW at upstream commit %s", commit)
	}
	// Training-notice import guard: the pinned copy below must track the file.
	if !strings.Contains(models, "FREEBUFF_AI_TRAINING_NOTICE,") || !strings.Contains(models, "from './freebuff-data-use'") {
		return nil, fmt.Errorf("wiregen: freebuff-models.ts: FREEBUFF_AI_TRAINING_NOTICE import moved at upstream commit %s; re-check the pinned training notice", commit)
	}
	return c, nil
}

// Pinned display policy: wire ids whose picker copy the proxy drops.
// Withdrawn rows with no surfacing copy (ox/pro/minimax) plus rows served on
// no proxy surface (gemini is Pro-paywalled, fable is a paid-API trial row).
// A future SUPPORTED row defaults to trimmed unless served; restoring a row
// to FREEBUFF_MODELS automatically restores its copy.
var catalogDisplayTrimmed = map[string]bool{
	"stealth/ox-alpha":         true,
	"deepseek/deepseek-v4-pro": true,
	"minimax/minimax-m3":       true,
	"google/gemini-3.8-flash":  true,
	"anthropic/claude-fable-5": true,
}

// Pinned copy the snapshots only reference indirectly (asserted at build).
const (
	pinnedTrainingNotice = "May use data for AI training"
	pinnedSolarTagline   = "Fast & Direct"
	pinnedSolarNotice    = "Labor Day weekend (through Sep 7 PT)"
	pinnedGlm52Tagline   = "Referral reward"
	pinnedGlm52Notice    = "Unlocked via referral code"
)

// effortsPinned preserves the proxy single-rung ["high"] ladder for rows
// whose upstream object exposes no effort parameter at all (Xiaomi exposes
// only disabled/high for MiMo; MiniMax M3 has no levels). Nil would mean
// "ignores reasoning_effort, use the default ladder" and would widen what
// convert and the codex surface admit, so the pin holds until upstream ships
// a real ladder for the row — which fails explicitly below.
var effortsPinned = map[string][]string{
	"mimo/mimo-v2.5":     {"high"},
	"minimax/minimax-m3": {"high"},
}

// catalogRow is one emitted Catalog entry.
type catalogRow struct {
	id, display, tagline, notice string
	badges                       []string
	served, premium              bool
	pausedReplacement            string
	ctx                          int
	efforts                      []string
	hasEfforts                   bool
}

func buildCatalogRows(c *catalogInputs) ([]catalogRow, error) {
	paused := map[string]bool{}
	for _, n := range c.pausedNames {
		id, err := resolveCatalogRef(c.ids, n, "paused id", c.commit)
		if err != nil {
			return nil, err
		}
		paused[id] = true
	}
	rows := make([]catalogRow, 0, len(c.rowNames))
	for _, n := range c.rowNames {
		f := c.fields[n]
		id := tsMustUnquote(c.ids[n])
		r := catalogRow{id: id}
		d, ok := tsUnquote(f["displayName"])
		if !ok {
			return nil, fmt.Errorf("wiregen: freebuff-models.ts: row %s has no displayName literal at upstream commit %s", n, c.commit)
		}
		r.display = d
		r.served = c.served[n]
		if paused[id] {
			r.pausedReplacement = c.defaultID
		}
		// Premium: served rows mirror the resolved row flag, paused rows never.
		if r.served {
			switch v := f["premium"]; v {
			case "true":
				r.premium = true
			case "false":
			default:
				b, err := resolveCatalogRef(c.ids, v, "row "+n+" premium", c.commit)
				if err != nil {
					return nil, err
				}
				if b != "true" && b != "false" {
					return nil, fmt.Errorf("wiregen: freebuff-models.ts: row %s premium=%q is not a bool at upstream commit %s", n, v, c.commit)
				}
				r.premium = b == "true"
			}
		}
		if v, ok := c.ctx[id]; ok {
			r.ctx = v
		} else if id == tsMustUnquote(c.ids["MUSE_SPARK_13_CONTRIBUTOR_MODEL"]) {
			// Frozen row fact: 1.3 keeps 1.2's window (Meta publishes the same
			// value for every Muse Spark variant; upstream keys this id).
			v, ok := c.ctx[tsMustUnquote(c.ids["MUSE_SPARK_12_CONTRIBUTOR_MODEL"])]
			if !ok {
				return nil, fmt.Errorf("wiregen: freebuff-models.ts: muse 1.2 context entry missing at upstream commit %s", c.commit)
			}
			r.ctx = v
		}
		if ref, ok := f["efforts"]; ok {
			if _, pinned := effortsPinned[id]; pinned {
				return nil, fmt.Errorf("wiregen: freebuff-models.ts: row %s gained an upstream efforts ladder; reconcile the pinned compat ladder at upstream commit %s", n, c.commit)
			}
			l, ok := c.strArrays[ref]
			if !ok {
				return nil, fmt.Errorf("wiregen: freebuff-models.ts: row %s efforts=%q is not a known ladder at upstream commit %s", n, ref, c.commit)
			}
			r.efforts, r.hasEfforts = append([]string(nil), l...), true
		} else if pin, ok := effortsPinned[id]; ok {
			r.efforts, r.hasEfforts = append([]string(nil), pin...), true
		}
		if catalogDisplayTrimmed[id] && !r.served {
			rows = append(rows, r)
			continue
		}
		// Picker copy for served rows plus the kept paused rows (1.3 keeps its
		// frozen facts for the installed binaries it still answers; 5.2 keeps
		// the referral banner copy).
		switch t := f["tagline"]; {
		case isTSQuoted(t):
			r.tagline, _ = tsUnquote(t)
		case t == "SOLAR_REGULAR_OFFER.tagline":
			r.tagline = pinnedSolarTagline
		default:
			return nil, fmt.Errorf("wiregen: freebuff-models.ts: row %s tagline=%q is not a string at upstream commit %s", n, t, c.commit)
		}
		if id == c.glm52ID {
			if f["tagline"] != "'Unlock by referring friends'" {
				return nil, fmt.Errorf("wiregen: freebuff-models.ts: GLM 5.2 tagline moved at upstream commit %s; re-check the pinned referral copy", c.commit)
			}
			r.tagline = pinnedGlm52Tagline
			r.notice = pinnedGlm52Notice
			r.badges = []string{"Referral only"}
		} else {
			switch w, ok := f["warning"]; {
			case ok && w == "FREEBUFF_AI_TRAINING_NOTICE":
				r.notice = pinnedTrainingNotice
			case ok && isTSQuoted(w):
				r.notice, _ = tsUnquote(w)
			case ok:
				return nil, fmt.Errorf("wiregen: freebuff-models.ts: row %s warning=%q is not a known notice at upstream commit %s", n, w, c.commit)
			case id == c.solarID:
				// No warning field upstream; the promo notice rides the offer
				// object (freebuff-solar-promo.ts, not a mirrored snapshot).
				if f["tagline"] != "SOLAR_REGULAR_OFFER.tagline" {
					return nil, fmt.Errorf("wiregen: freebuff-models.ts: solar offer reference moved at upstream commit %s; re-check the pinned promo copy", c.commit)
				}
				r.notice = pinnedSolarNotice
			}
			if v, ok := f["reasoningEffort"]; ok {
				var name string
				if isTSQuoted(v) {
					name, _ = tsUnquote(v)
				} else {
					var err error
					if name, err = resolveCatalogRef(c.ids, v, "row "+n+" reasoningEffort", c.commit); err != nil {
						return nil, err
					}
				}
				if name == "" {
					return nil, fmt.Errorf("wiregen: freebuff-models.ts: row %s reasoningEffort=%q is not a string at upstream commit %s", n, v, c.commit)
				}
				badge := "Reasoning: " + name
				if badge == "Reasoning: max" {
					// The star marks the loop-risk rung (see the row comment
					// upstream); proxy copy keeps it distinct from plain max.
					badge = "Reasoning: max*"
				}
				r.badges = append(r.badges, badge)
			}
			if v := f["multimodal"]; v == "true" {
				r.badges = append(r.badges, "Images")
			} else if v != "false" {
				return nil, fmt.Errorf("wiregen: freebuff-models.ts: row %s multimodal=%q is not a bool at upstream commit %s", n, v, c.commit)
			}
			if v, ok := f["isNew"]; ok {
				if v != "true" {
					return nil, fmt.Errorf("wiregen: freebuff-models.ts: row %s isNew=%q at upstream commit %s", n, v, c.commit)
				}
				r.badges = append(r.badges, "NEW")
			}
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// resolveCatalogRef resolves a quoted literal, a const chain (A -> B), or a
// member access (mimoModels.mimoV25, ENT.modelId, ENT.fullAccess.premium) to
// its value. Anything else fails explicitly.
func resolveCatalogRef(ids map[string]string, ref, what, commit string) (string, error) {
	ref = strings.TrimSpace(ref)
	for i := 0; i < 100; i++ {
		if s, ok := tsUnquote(ref); ok {
			return s, nil
		}
		if ref == "true" || ref == "false" {
			return ref, nil
		}
		v, ok := ids[ref]
		if !ok {
			return "", fmt.Errorf("wiregen: %s: unresolvable reference %q at upstream commit %s", what, ref, commit)
		}
		v = strings.TrimSpace(v)
		if v == ref {
			break
		}
		ref = v
	}
	return "", fmt.Errorf("wiregen: %s: reference cycle at upstream commit %s", what, commit)
}

func isTSQuoted(s string) bool {
	_, ok := tsUnquote(s)
	return ok
}

func tsUnquote(s string) (string, bool) {
	if len(s) >= 2 && (s[0] == '\'' && s[len(s)-1] == '\'' || s[0] == '"' && s[len(s)-1] == '"') {
		return s[1 : len(s)-1], true
	}
	return "", false
}

func tsMustUnquote(s string) string {
	v, _ := tsUnquote(s)
	return v
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := range s {
		switch c := s[i]; {
		case c == '_', c == '$':
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z':
		case '0' <= c && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// parseTSStrings collects `export const NAME = 'lit'` (both quote styles)
// plus `export const NAME[: T] = REF` member/const right-hand sides.
func parseTSStrings(src string) map[string]string {
	out := map[string]string{}
	for _, m := range regexp.MustCompile(`export const (\w+) =\s*('[^']*'|"[^"]*")`).FindAllStringSubmatch(src, -1) {
		out[m[1]] = m[2]
	}
	for _, m := range regexp.MustCompile(`export const (\w+)(?::\s*\w+)?\s*=\s*\n?\s*([A-Za-z_][\w.]*)`).FindAllStringSubmatch(src, -1) {
		if _, ok := out[m[1]]; !ok {
			out[m[1]] = m[2]
		}
	}
	return out
}

// parseTSBools collects `export const NAME = true|false`.
func parseTSBools(src string) map[string]bool {
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`export const (\w+) = (true|false)`).FindAllStringSubmatch(src, -1) {
		out[m[1]] = m[2] == "true"
	}
	return out
}

// parseTSArrays collects `const NAME = ['a', ...] as const` (export or local,
// single- or multi-line, both quote styles).
func parseTSArrays(src string) map[string][]string {
	out := map[string][]string{}
	for _, m := range regexp.MustCompile(`(?:export )?const (\w+) = \[([\s\S]*?)\] as const`).FindAllStringSubmatch(src, -1) {
		var items []string
		for _, it := range regexp.MustCompile(`'([^']*)'|"([^"]*)"`).FindAllStringSubmatch(m[2], -1) {
			if it[1] != "" {
				items = append(items, it[1])
			} else {
				items = append(items, it[2])
			}
		}
		out[m[1]] = items
	}
	return out
}

// parseMimoMembers reads the mimoModels = { k: 'v' } block for member access.
func parseMimoMembers(src string) map[string]string {
	out := map[string]string{}
	m := regexp.MustCompile(`export const mimoModels = \{([\s\S]*?)\} as const`).FindStringSubmatch(src)
	if m == nil {
		return out
	}
	for _, e := range regexp.MustCompile(`(\w+):\s*'([^']+)'`).FindAllStringSubmatch(m[1], -1) {
		out[e[1]] = "'" + e[2] + "'"
	}
	return out
}

// balancedBody returns the inside of the first `NAME ... = { ... }` brace run
// (strings and // comments excluded from balance). ok=false when absent.
func balancedBody(src, name, commit string) (string, bool) {
	_ = commit
	re := regexp.MustCompile(`(?:export |const )` + name + `[^\n]*?=\s*\{`)
	loc := re.FindStringIndex(src)
	if loc == nil {
		return "", false
	}
	depth := 0
	var q byte
	for i := loc[1] - 1; i < len(src); i++ {
		ch := src[i]
		if q != 0 {
			if ch == q {
				q = 0
			}
			continue
		}
		switch {
		case ch == '\'' || ch == '"' || ch == '`':
			q = ch
		case ch == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case ch == '{':
			depth++
		case ch == '}':
			depth--
			if depth == 0 {
				return src[loc[1]:i], true
			}
		}
	}
	return "", false
}

// parseNameList reads `export const NAME = [ IDENT, ... ]`; any spread or
// non-identifier item fails explicitly.
func parseNameList(src, name, commit string) ([]string, error) {
	// Bracket scan: NAME lists use [ ], balanced here quote-aware.
	re := regexp.MustCompile(`export const ` + name + `[^\n]*?=\s*\[`)
	loc := re.FindStringIndex(src)
	if loc == nil {
		return nil, fmt.Errorf("wiregen: freebuff-models.ts: no %s list at upstream commit %s", name, commit)
	}
	depth := 0
	end := -1
	var q byte
	for i := loc[1] - 1; i < len(src); i++ {
		ch := src[i]
		if q != 0 {
			if ch == q {
				q = 0
			}
			continue
		}
		switch {
		case ch == '\'' || ch == '"' || ch == '`':
			q = ch
		case ch == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case ch == '[':
			depth++
		case ch == ']':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return nil, fmt.Errorf("wiregen: freebuff-models.ts: %s brackets unbalanced at upstream commit %s", name, commit)
	}
	var out []string
	for i, line := range strings.Split(src[loc[1]:end], "\n") {
		t := strings.TrimSpace(stripTSComment(line))
		t = strings.TrimSpace(strings.TrimSuffix(t, ","))
		if t == "" {
			continue
		}
		if !isIdent(t) {
			return nil, fmt.Errorf("wiregen: freebuff-models.ts: %s:%d: unexpected item %q at upstream commit %s (want a plain row name)", name, i+1, t, commit)
		}
		out = append(out, t)
	}
	return out, nil
}

// stripTSComment cuts a // comment outside of quotes.
func stripTSComment(line string) string {
	var q byte
	for i := 0; i < len(line); i++ {
		switch {
		case q != 0 && line[i] == q:
			q = 0
		case q == 0 && (line[i] == '\'' || line[i] == '"'):
			q = line[i]
		case q == 0 && line[i] == '/' && i+1 < len(line) && line[i+1] == '/':
			return line[:i]
		}
	}
	return line
}

// parseModelsList reads FREEBUFF_MODELS, evaluating `...(FLAG ? [ROW] : [])`
// spreads against the parsed bool consts.
func parseModelsList(src string, bools map[string]bool, commit string) ([]string, error) {
	re := regexp.MustCompile(`export const FREEBUFF_MODELS[^\n]*?=\s*\[`)
	loc := re.FindStringIndex(src)
	if loc == nil {
		return nil, fmt.Errorf("wiregen: freebuff-models.ts: no FREEBUFF_MODELS list at upstream commit %s", commit)
	}
	depth := 0
	end := -1
	var q byte
	i := loc[1] - 1
	for ; i < len(src); i++ {
		ch := src[i]
		if q != 0 {
			if ch == q {
				q = 0
			}
			continue
		}
		switch {
		case ch == '\'' || ch == '"' || ch == '`':
			q = ch
		case ch == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case ch == '[':
			depth++
		case ch == ']':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return nil, fmt.Errorf("wiregen: freebuff-models.ts: FREEBUFF_MODELS brackets unbalanced at upstream commit %s", commit)
	}
	spread := regexp.MustCompile(`^\.\.\.\((\w+) \? \[(\w+)\] : \[\]\)$`)
	var out []string
	for ln, line := range strings.Split(src[loc[1]:end], "\n") {
		t := strings.TrimSpace(stripTSComment(line))
		t = strings.TrimSpace(strings.TrimSuffix(t, ","))
		if t == "" {
			continue
		}
		if m := spread.FindStringSubmatch(t); m != nil {
			v, ok := bools[m[1]]
			if !ok {
				return nil, fmt.Errorf("wiregen: freebuff-models.ts: FREEBUFF_MODELS:%d: unknown flag %q at upstream commit %s", ln+1, m[1], commit)
			}
			if v {
				out = append(out, m[2])
			}
			continue
		}
		if !isIdent(t) {
			return nil, fmt.Errorf("wiregen: freebuff-models.ts: FREEBUFF_MODELS:%d: unexpected item %q at upstream commit %s", ln+1, t, commit)
		}
		out = append(out, t)
	}
	return out, nil
}

// rowFieldOK names every field a SUPPORTED row object may carry. Handled
// fields are parsed above; the rest are presence-validated only, so a
// brand-new field fails here instead of slipping past silently.
var rowFieldOK = map[string]bool{
	"id": true, "displayName": true, "tagline": true, "availability": true,
	"unavailableFallback": true, "warning": true, "dataUse": true,
	"premium": true, "multimodal": true, "reasoningEffort": true,
	"efforts": true, "defaultEffort": true, "experimental": true,
	"taglineTooltip": true, "isNew": true,
}

// parseRowFields extracts the top-level `key: value` fields of row NAME.
func parseRowFields(src, name, commit string) (map[string]string, error) {
	re := regexp.MustCompile(`(?:export |const )` + name + ` = \{`)
	loc := re.FindStringIndex(src)
	if loc == nil {
		return nil, fmt.Errorf("wiregen: freebuff-models.ts: no row object %s at upstream commit %s", name, commit)
	}
	depth := 0
	end := -1
	var q byte
	i := loc[1] - 1
	for ; i < len(src); i++ {
		ch := src[i]
		if q != 0 {
			if ch == q {
				q = 0
			}
			continue
		}
		switch {
		case ch == '\'' || ch == '"' || ch == '`':
			q = ch
		case ch == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case ch == '{':
			depth++
		case ch == '}':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return nil, fmt.Errorf("wiregen: freebuff-models.ts: row %s braces unbalanced at upstream commit %s", name, commit)
	}
	out := map[string]string{}
	fre := regexp.MustCompile(`^([A-Za-z]+):\s*(.+?)\s*,?\s*$`)
	for _, ln := range strings.Split(src[loc[1]:end], "\n") {
		t := strings.TrimSpace(stripTSComment(ln))
		if t == "" {
			continue
		}
		m := fre.FindStringSubmatch(t)
		if m == nil {
			return nil, fmt.Errorf("wiregen: freebuff-models.ts: row %s: cannot parse field line %q at upstream commit %s", name, t, commit)
		}
		if !rowFieldOK[m[1]] {
			return nil, fmt.Errorf("wiregen: freebuff-models.ts: row %s: unknown field %q at upstream commit %s (teach the emitter, then re-run)", name, m[1], commit)
		}
		out[m[1]] = m[2]
	}
	return out, nil
}

// singleRef returns the raw right-hand side of `export const NAME = ...`
// (possibly on the next line), or "" when absent.
func singleRef(src, name string) string {
	m := regexp.MustCompile(`export const ` + name + `[^\n=]*=\s*\n?\s*([^\s,;]+)`).FindStringSubmatch(src)
	if m == nil {
		return ""
	}
	return m[1]
}

func renderCatalog(commit string, c *catalogInputs, rows []catalogRow) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, `// Code generated by cmd/wiregen -upstream %s; DO NOT EDIT.
// Source: backend/internal/registry/testdata/upstream/freebuff-models.ts
// (plus freebuff-model-ids.ts, freebuff-model-entitlements.ts, model-config.ts)
// at upstream commit %s.
package modelcat

import (
	"time"
)

// ModelInfo describes one catalog model and every proxy fact about it.
type ModelInfo struct {
	// ID is the wire model id (provider/model).
	ID string
	// DisplayName is the upstream catalog display name (used in the
	// withdrawn-model refusal copy, mirroring freebuffWithdrawnModelMessage).
	DisplayName string
	// Served gates /v1/models and the chat handlers: the ids this gateway
	// serves or advertises. Paused models are never Served.
	Served bool
	// PausedReplacement is non-empty exactly when upstream
	// FREEBUFF_PAUSED_FREE_MODEL_IDS lists the model: recognized but
	// admission-refused. It names the model the refusal copy recommends.
	PausedReplacement string
	// Premium marks FREEBUFF_PREMIUM_MODEL_IDS membership (the shared daily
	// premium pool). Fable 5 is premium-flagged upstream but metered by its
	// own global pool (FREEBUFF_LIMITED_OFFER_MODEL_IDS), not the shared
	// pool, so it is NOT marked Premium here.
	Premium bool
	// ContextWindow mirrors FREEBUFF_MODEL_CONTEXT_WINDOWS in tokens; 0
	// means upstream falls back to DefaultContextWindow.
	ContextWindow int
	// Efforts is the upstream reasoning-effort ladder (nil = route accepts
	// and ignores reasoning_effort, so conversion uses the default ladder).
	Efforts []string
	// Tagline is the upstream catalog description (used in picker & catalog).
	Tagline string
	// Notice is the upstream warning or special offer (e.g. AI training, promo).
	Notice string
	// Badges are the capability/freshness chips (e.g. Reasoning: max*, Images, NEW).
	Badges []string
}

// Catalog is the full free-catalog table, in upstream SUPPORTED_FREEBUFF_MODELS
// order.
var Catalog = []ModelInfo{
`, commit, commit)
	for _, r := range rows {
		fmt.Fprintf(&b, "\t{ID: %q, DisplayName: %q", r.id, r.display)
		if r.tagline != "" {
			fmt.Fprintf(&b, ",\n\t\tTagline: %q", r.tagline)
		}
		if len(r.badges) > 0 {
			fmt.Fprintf(&b, ",\n\t\tBadges: []string{%s}", quotedList(r.badges))
		}
		if r.notice != "" {
			fmt.Fprintf(&b, ",\n\t\tNotice: %q", r.notice)
		}
		if r.served {
			b.WriteString(",\n\t\tServed: true")
		}
		if r.premium {
			b.WriteString(",\n\t\tPremium: true")
		}
		if r.ctx != 0 {
			fmt.Fprintf(&b, ",\n\t\tContextWindow: %d", r.ctx)
		}
		if r.hasEfforts {
			fmt.Fprintf(&b, ",\n\t\tEfforts: []string{%s}", quotedList(r.efforts))
		}
		if r.pausedReplacement != "" {
			fmt.Fprintf(&b, ",\n\t\tPausedReplacement: %q", r.pausedReplacement)
		}
		b.WriteString("},\n")
	}
	b.WriteString("}\n")
	fmt.Fprintf(&b, `
// DefaultModelID mirrors upstream DEFAULT_FREEBUFF_MODEL_ID
// (FREEBUFF_MODELS[0]): what the upstream CLI resolves a blank model pick
// to, and what paused-model refusals recommend.
const DefaultModelID = %q

// FallbackModelID mirrors upstream FALLBACK_FREEBUFF_MODEL_ID: the model
// guaranteed available on EVERY tier that unavailable picks are coerced to.
const FallbackModelID = %q

// LimitedModelID is the proxy limited-tier admission row: the unlimited
// fallback row, pinned to FALLBACK (not to upstream LIMITED_FREEBUFF_MODEL_ID,
// which upstream split from the picker hero on 2026-09-07 into a coercion
// target of its own). The server gate admits only this row without quota, so
// following upstream here would widen limited-tier admission; revisit
// deliberately, never on sync.
const LimitedModelID = %q

// DeepSeekV4FlashModelID mirrors upstream FREEBUFF_DEEPSEEK_V4_FLASH_MODEL_ID.
const DeepSeekV4FlashModelID = "deepseek/deepseek-v4-flash"

// LimitedTierModelIDs mirrors upstream LIMITED_FREEBUFF_MODEL_IDS: the four models
// available to limited-access tier accounts (GLM 5.3 Flash, DeepSeek V4 Flash,
// MiMo 2.5, Solar Pro 4).
var LimitedTierModelIDs = []string{
	Glm53ModelID,
	DeepSeekV4FlashModelID,
	LimitedModelID,
	SolarPro4ModelID,
}

// IsLimitedTierAllowed reports whether the model is available on the limited tier
// without requiring special referral grants (matches upstream LIMITED_FREEBUFF_MODEL_IDS).
func IsLimitedTierAllowed(id string) bool {
	switch id {
	case Glm53ModelID, DeepSeekV4FlashModelID, LimitedModelID, SolarPro4ModelID:
		return true
	default:
		return false
	}
}

// Glm52ModelID is the referral-reward model, metered by its own promo pool
// rather than the shared premium pool.
const Glm52ModelID = %q

// Glm53ModelID is the unmetered standard row and the proxy default.
const Glm53ModelID = %q

// SolarPro4ModelID mirrors FREEBUFF_SOLAR_PRO_4_MODEL_ID: the Upstage row,
// unmetered at full access (entitlement fullAccess.premium=false).
const SolarPro4ModelID = %q

// GLMSessionLength mirrors upstream FREEBUFF_REWARD_SESSION_LENGTH_MS (the
// earned-reward session pool GLM 5.2 admits from; the older
// FREEBUFF_GLM_V52_SESSION_LENGTH_MS name is gone upstream): GLM sessions
// are exactly one hour of wall-clock time.
const GLMSessionLength = time.Hour

// DefaultContextWindow mirrors upstream FREEBUFF_DEFAULT_CONTEXT_WINDOW:
// assumed for any model absent from FREEBUFF_MODEL_CONTEXT_WINDOWS.
const DefaultContextWindow = %d
`, c.defaultID, c.fallbackID, c.fallbackID, c.glm52ID, c.glm53ID, c.solarID, c.defaultCtx)
	return []byte(b.String())
}

func quotedList(items []string) string {
	qs := make([]string, len(items))
	for i, s := range items {
		qs[i] = strconv.Quote(s)
	}
	return strings.Join(qs, ", ")
}
