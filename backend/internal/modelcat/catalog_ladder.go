package modelcat

import (
	"slices"
	"strings"
)

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
