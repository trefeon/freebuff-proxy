// about the FreeBuff free catalog. One row per model in upstream
// SUPPORTED_FREEBUFF_MODELS (reference/freebuff/common/src/constants/
// freebuff-models.ts, pinned snapshot f25d405, vendor 0.0.167).
//
// Every package that needs a per-model fact — registry (served/paused gate,
// withdrawn-model copy), convert (effort ladders), pool (premium pool and
// per-model caps), upstream (mock session quotas), server (default model,
// /v1/models rows) — reads it from here instead of maintaining its own
// table. An upstream sync is one table edit below plus a rerun of the
// catalog parity test (catalog_test.go), which re-reads the pinned snapshot
// and fails on drift. Agent-root maps (FREE_MODE_AGENT_MODELS,
// FREEBUFF_ROOT_AGENT_ID_BY_MODEL) are wire-level agent assignments, not
// model facts, and stay in backend/internal/registry under their own parity test.
package modelcat

import (
	"slices"
	"strings"
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
	// Cap is the FREEBUFF_PER_MODEL_SESSION_CAPS daily ceiling (0 = none).
	Cap int
	// CapPool is the upstream pool id for Cap ("" when uncapped).
	CapPool string
	// ContextWindow mirrors FREEBUFF_MODEL_CONTEXT_WINDOWS in tokens; 0
	// means upstream falls back to DefaultContextWindow.
	ContextWindow int
	// Efforts is the upstream reasoning-effort ladder (nil = route accepts
	// and ignores reasoning_effort, so conversion uses the default ladder).
	Efforts []string
}

// Catalog is the full free-catalog table, in upstream SUPPORTED_FREEBUFF_MODELS
// order. Re-verify every row against the pinned snapshot on sync; the parity
// test enforces it.
var Catalog = []ModelInfo{
	{ID: "stealth/ox-alpha", DisplayName: "Ox Alpha",
		PausedReplacement: "z-ai/glm-5.3-flash", ContextWindow: 1_000_000,
		Efforts: []string{"low", "high", "max"}},
	{ID: "deepseek/deepseek-v4-pro", DisplayName: "DeepSeek V4 Pro",
		PausedReplacement: "z-ai/glm-5.3-flash", ContextWindow: 1_048_576,
		Efforts: []string{"low", "high", "max"}},
	{ID: "minimax/minimax-m3", DisplayName: "MiniMax M3",
		PausedReplacement: "z-ai/glm-5.3-flash", ContextWindow: 524_288,
		Efforts: []string{"high"}},
	{ID: "openai/gpt-5.6-luna", DisplayName: "GPT-5.6 Luna",
		Served: true, Premium: true, ContextWindow: 1_000_000,
		Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
	{ID: "upstage/solar-pro4", DisplayName: "Solar Pro 4",
		Served: true, ContextWindow: 500_000},
	// google/gemini-3.8-flash returned 2026-09-04 behind the Pro paywall,
	// Web-only (upstream FREEBUFF_PRO_ONLY_CATALOG_MODEL_IDS): removed from
	// FREEBUFF_PAUSED_FREE_MODEL_IDS, but NOT back in FREEBUFF_MODELS, so no
	// CLI/Desktop surface may serve it. The proxy has no Pro surface, so the
	// row stays recognized-but-never-served: no PausedReplacement (upstream
	// no longer pauses it), no Served. No context entry upstream; the $0.50
	// per-session spend ceiling is upstream-enforced, so unmodeled.
	{ID: "google/gemini-3.8-flash", DisplayName: "Gemini 3.8 Flash",
		Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
	// meta/muse-spark-1.3-contributor joined every surface 2026-09-04
	// (upstream SUPPORTED_FREEBUFF_MODELS + FREEBUFF_MODELS, last on
	// purpose — the one row that may answer on a different model when its
	// shared ceiling is full). Premium shared-pool row; rate-limited with a
	// 15s queue window, then answers on DeepSeek V4 Flash. Contributor
	// terms: Meta trains on prompts and completions (dataUse 'training'),
	// and upstream keeps no traces for it. Full ladder, defaultEffort
	// xhigh. No context entry upstream (only 1.2 has one), so it falls
	// back to DefaultContextWindow.
	{ID: "meta/muse-spark-1.3-contributor", DisplayName: "Muse Spark 1.3",
		Served: true, Premium: true,
		Efforts: []string{"minimal", "low", "medium", "high", "xhigh"}},
	{ID: "z-ai/glm-5.2", DisplayName: "GLM 5.2", PausedReplacement: "z-ai/glm-5.3-flash"},
	{ID: "z-ai/glm-5.3-flash", DisplayName: "GLM 5.3 Flash",
		Served: true, ContextWindow: 1_000_000,
		Efforts: []string{"low", "high", "max"}},
	{ID: "deepseek/deepseek-v4-flash", DisplayName: "DeepSeek V4 Flash",
		Served: true, ContextWindow: 1_048_576,
		Efforts: []string{"low", "high", "max"}},
	{ID: "mimo/mimo-v2.5", DisplayName: "MiMo 2.5",
		Served: true, Efforts: []string{"high"}},
	// anthropic/claude-fable-5 stays in the catalog (upstream SUPPORTED list,
	// parity test) but is NOT served: it is a paid-API model metered by its
	// own global offer pool that free accounts cannot reach, so advertising
	// it would surface admissions that always fail.
	{ID: "anthropic/claude-fable-5", DisplayName: "Claude Fable 5",
		Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
}

// DefaultModelID mirrors upstream DEFAULT_FREEBUFF_MODEL_ID, pinned to
// FREEBUFF_MODELS[0] (GLM 5.3 Flash leads the picker again as of 2026-09-05,
// retaking the lead from DeepSeek V4 Flash 09-02→09-05; see upstream
// freebuff-models.ts's note on the move). It is what the upstream CLI
// resolves a blank model pick to, and what paused-model refusals recommend.
const DefaultModelID = "z-ai/glm-5.3-flash"

// FallbackModelID mirrors upstream FALLBACK_FREEBUFF_MODEL_ID: the model
// guaranteed available on EVERY tier that unavailable picks are coerced to.
// mimo is region-universal — premium-pool models (luna) only
// resolve on full-tier accounts, so any proxy-side "no model" default must
// be mimo, never the premium picker lead.
const FallbackModelID = "mimo/mimo-v2.5"

// LimitedModelID mirrors upstream LIMITED_FREEBUFF_MODEL_ID: the model
// guaranteed available on the limited tier.
const LimitedModelID = "mimo/mimo-v2.5"

// IsLimitedTierAllowed reports whether the model is available on the limited tier
// without requiring special referral grants.
func IsLimitedTierAllowed(id string) bool {
	return id == LimitedModelID
}

// Glm52ModelID is the referral-reward model, metered by its own promo pool
// rather than the shared premium pool.
const Glm52ModelID = "z-ai/glm-5.2"

// Glm53ModelID is an UNMETERED standard row since 2026-08-28 (left the shared
// daily premium pool: it bills $0.000249/msg via the Merge lane, cheaper than
// any other served row, so it joins MiMo and DeepSeek V4 Flash with no
// ceiling) AND the proxy default since 2026-08-30 (vendor moved the model to
// the picker lead — always available, unmetered, cheapest row). Laddered
// 2026-08-30 as ['low','high','max'] (mediumless; upstream defaultEffort
// 'max'). The premium flag must always agree with the upstream
// FREEBUFF_PREMIUM_MODEL_IDS list (isFreebuffPremiumModelId and the
// FREEBUFF_STANDARD_MODEL_IDS derivation disagree if not).
const Glm53ModelID = "z-ai/glm-5.3-flash"

// SolarPro4ModelID mirrors FREEBUFF_SOLAR_PRO_4_MODEL_ID: the Upstage row,
// UNMETERED since 2026-09-04 (entitlement fullAccess.premium=false;
// "unmetered at full access" per the availability copy). Previously shared
// premium pool; the per-model count cap closed 2026-09-01 (upstream
// 051fd4d9) and pool metering followed it out.
const SolarPro4ModelID = "upstage/solar-pro4"

// PremiumSessionLimit mirrors upstream FREEBUFF_PREMIUM_SESSION_LIMIT: the
// LEVEL-0 premium floor per Pacific day (4). The runtime default
// entitlement is 5/day: under FREEBUFF_TRUST_LEVELS=observe (the default)
// the flat base applies, and under FREEBUFF_LEVEL_SESSIONS=off the
// pre-Levels base (5) is selected. 4 only applies with trust levels
// enforced at the floor; the ladder raises it back (5 at level 2+, 7
// ceiling).
const PremiumSessionLimit = 4

// GLMSessionLength mirrors upstream FREEBUFF_GLM_V52_SESSION_LENGTH_MS: GLM
// sessions are exactly one hour of wall-clock time, regardless of the global
// free-session length.
const GLMSessionLength = time.Hour

// DefaultContextWindow mirrors upstream FREEBUFF_DEFAULT_CONTEXT_WINDOW:
// assumed for any model absent from FREEBUFF_MODEL_CONTEXT_WINDOWS.
const DefaultContextWindow = 128*1024 + 1024 // 131_072

func byID(id string) *ModelInfo {
	for i := range Catalog {
		if Catalog[i].ID == id {
			return &Catalog[i]
		}
	}
	return nil
}

// DisplayName returns the upstream catalog display name for id, or id when
// the model is unknown (mirrors freebuffWithdrawnModelMessage's fallback).
func DisplayName(id string) string {
	if m := byID(id); m != nil {
		return m.DisplayName
	}
	return id
}

// IsServed reports whether id passes the ServedModels gate.
func IsServed(id string) bool {
	if m := byID(id); m != nil {
		return m.Served
	}
	return false
}

// IsPaused reports whether id is upstream-recognized but withdrawn
// (FREEBUFF_PAUSED_FREE_MODEL_IDS): refused at admission, never served.
func IsPaused(id string) bool {
	if m := byID(id); m != nil {
		return m.PausedReplacement != ""
	}
	return false
}

// PausedReplacement returns the model the withdrawn-model refusal copy
// recommends for id ("" when id is not paused).
func PausedReplacement(id string) string {
	if m := byID(id); m != nil {
		return m.PausedReplacement
	}
	return ""
}

// WithdrawnModelMessage mirrors upstream freebuffWithdrawnModelMessage
// (freebuff-models.ts:1685-1697): names the model asked for and what to use
// instead — the client that sends this id is a released binary whose picker
// still lists it, so "unavailable" alone leaves the user staring at a row
// that looks fine and does not work.
func WithdrawnModelMessage(id string) string {
	replacement := PausedReplacement(id)
	if replacement == "" {
		return DisplayName(id) + " is no longer available in Freebuff."
	}
	return DisplayName(id) + " is no longer available in Freebuff. We recommend using " + DisplayName(replacement) + " instead."
}

// IsPremium reports whether id is in the shared daily premium pool
// (FREEBUFF_PREMIUM_MODEL_IDS)
func IsPremium(id string) bool {
	if m := byID(id); m != nil {
		return m.Premium || m.Cap > 0
	}
	return false
}

// SharedPremiumModels returns the ids metered by the shared daily premium
// pool: Luna + Muse Spark 1.3 since 2026-09-04 (solar left the pool when its
// entitlement went unmetered; gemini is Pro-paywalled).
// GLM 5.3 Flash is unmetered.
func SharedPremiumModels() []string {
	var out []string
	for i := range Catalog {
		if Catalog[i].Premium {
			out = append(out, Catalog[i].ID)
		}
	}
	return out
}

// PerModelCap returns the FREEBUFF_PER_MODEL_SESSION_CAPS ceiling and pool id
// for a model (0, "" when the model has no cap).
func PerModelCap(id string) (limit int, pool string) {
	if m := byID(id); m != nil && m.Cap > 0 {
		return m.Cap, m.CapPool
	}
	return 0, ""
}

// ContextWindow returns the model's context window in tokens, or
// DefaultContextWindow when the model has no observed entry.
func ContextWindow(id string) int {
	if m := byID(id); m != nil && m.ContextWindow > 0 {
		return m.ContextWindow
	}
	return DefaultContextWindow
}

// Efforts returns the model's reasoning-effort ladder; nil when the route
// accepts and ignores reasoning_effort (callers use the default ladder).
func Efforts(id string) []string {
	if m := byID(id); m != nil {
		return m.Efforts
	}
	return nil
}

// shortName returns the model short name (the segment after the org
// prefix) of a wire id: "stealth/ox-alpha" → "ox-alpha".
func shortName(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}

// IsMediumlessLadderModel reports whether the model's catalog effort ladder
// offers multiple rungs but omits "medium" (DeepSeek V4, Ox Alpha, GLM 5.3
// Flash). Consumers use this to decide the medium-rewrite policy (#112):
// DeepSeek routes map medium→high because upstream's own authority does;
// the other mediumless ladders map medium→high as a deliberate proxy
// translation for stale preferences (upstream would clamp DOWN to low —
// zero thinking on GLM — but CLI clients can never send medium there, so
// no on-wire request ever contradicts upstream). Single-rung ladders
// (["high"])
// are excluded — their medium requests down-clamp to the single rung
// anyway. Matching is suffix-tolerant on the row's short name, mirroring the
// matching convert historically applied: stealth/ox-alpha-20260812 matches
// stealth/ox-alpha, bare short ids match, and the comparison is
// case-insensitive.
func IsMediumlessLadderModel(id string) bool {
	m := strings.ToLower(id)
	for i := range Catalog {
		efforts := Catalog[i].Efforts
		if len(efforts) < 2 || slices.Contains(efforts, "medium") {
			continue
		}
		if strings.HasSuffix(m, shortName(Catalog[i].ID)) {
			return true
		}
	}
	return false
}

// strictReasoningFragments lists the id fragments of models that require an
// explicit reasoning_content field on assistant messages carrying tool calls
// (MiMo, DeepSeek V4, Kimi). This is a wire-behavior fact, not derivable
// from the Efforts ladder (mimo-v2.5 and minimax-m3 share the {"high"}
// ladder but only MiMo is strict), and "kimi" covers a legacy id
// (crof/kimi-k3-eco) with no catalog row. Contains matching (not suffix) is
// intentional: it tolerates org prefixes, bare ids and dated variants in
// one rule, mirroring the matching convert historically applied.
var strictReasoningFragments = []string{"mimo", "deepseek-v4", "kimi"}

// IsStrictReasoningModel reports whether the model requires an explicit
// reasoning_content field on assistant messages with tool calls (e.g. MiMo,
// DeepSeek-V4, Kimi).
func IsStrictReasoningModel(id string) bool {
	m := strings.ToLower(id)
	for _, frag := range strictReasoningFragments {
		if strings.Contains(m, frag) {
			return true
		}
	}
	return false
}

// PausedMap builds the paused-model map (id → replacement id) from the
// catalog, mirroring upstream FREEBUFF_PAUSED_FREE_MODEL_IDS.
func PausedMap() map[string]string {
	out := make(map[string]string, len(Catalog))
	for i := range Catalog {
		if Catalog[i].PausedReplacement != "" {
			out[Catalog[i].ID] = Catalog[i].PausedReplacement
		}
	}
	return out
}

// ServedMap builds the ServedModels gate map (id → true) from the catalog.
func ServedMap() map[string]bool {
	out := make(map[string]bool, len(Catalog))
	for i := range Catalog {
		if Catalog[i].Served {
			out[Catalog[i].ID] = true
		}
	}
	return out
}

// ServedIDs returns the served model ids in catalog order.
func ServedIDs() []string {
	var out []string
	for i := range Catalog {
		if Catalog[i].Served {
			out = append(out, Catalog[i].ID)
		}
	}
	return out
}

// ServedHelpText formats the served model list for error messages.
func ServedHelpText() string {
	ids := ServedIDs()
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ", "
		}
		out += id
	}
	return out
}
