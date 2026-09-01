package convert

import (
	"regexp"
	"strings"

	"freebuff-proxy/backend/internal/modelcat"
)

// ExtractReasoningEffort extracts the requested thinking/reasoning effort from
// OpenAI reasoning_effort, Codex/Anthropic reasoning.effort, thinking flags,
// or model name suffixes (e.g. "model(high)" or "model:max").
func ExtractReasoningEffort(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	if v, ok := payload["reasoning_effort"].(string); ok && v != "" {
		return strings.ToLower(strings.TrimSpace(v))
	}
	if rObj, ok := payload["reasoning"].(map[string]any); ok {
		if v, ok := rObj["effort"].(string); ok && v != "" {
			return strings.ToLower(strings.TrimSpace(v))
		}
	}
	if tObj, ok := payload["thinking"].(map[string]any); ok {
		if budget, ok := numInt64(tObj["budget_tokens"]); ok && budget > 0 {
			switch {
			case budget <= 512:
				return "minimal"
			case budget <= 1024:
				return "low"
			case budget <= 8192:
				return "medium"
			case budget <= 24576:
				return "high"
			default:
				return "xhigh"
			}
		}
		if v, ok := tObj["type"].(string); ok {
			return strings.ToLower(strings.TrimSpace(v))
		}
	}
	if m, ok := payload["model"].(string); ok && m != "" {
		m = strings.TrimSpace(m)
		if strings.HasSuffix(m, ")") {
			if idx := strings.LastIndex(m, "("); idx > 0 {
				tag := strings.ToLower(strings.TrimSpace(m[idx+1 : len(m)-1]))
				switch tag {
				case "max", "high", "medium", "low", "minimal", "xhigh", "ultra":
					return tag
				}
			}
		} else if idx := strings.LastIndex(m, ":"); idx > 0 {
			tag := strings.ToLower(strings.TrimSpace(m[idx+1:]))
			switch tag {
			case "max", "high", "medium", "low", "minimal", "xhigh", "ultra":
				return tag
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Reasoning effort normalization (issue #65 clamp + #111 plain wire field).
// Ported from freebuff/common/src/constants/reasoning-effort.ts
// (clampReasoningEffort). The DeepSeek thinking translation is server-side
// (codebuff-fork/web/src/llm-api/deepseek-request-body.ts), so the proxy
// never emits a thinking block — plain reasoning_effort goes on the wire.
// ---------------------------------------------------------------------------

// reasoningLadder is the one reasoning-effort vocabulary, strictly ascending.
// Its ORDER is load-bearing: clamping does index arithmetic on it.
var reasoningLadder = [...]string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

// defaultReasoningEffort is used when a request asks for an effort that is
// not on the ladder (mirrors DEFAULT_REASONING_EFFORT = 'high').
const defaultReasoningEffort = "high"

// Per-model effort ladders live in modelcat.Catalog (single source of truth,
// mirroring the upstream freebuff-models.ts rows): deepseek-v4-flash/pro,
// stealth/ox-alpha and z-ai/glm-5.3-flash run ['low','high','max'] (medium is
// not a distinct level and rewrites to high — see normalizeReasoning; the GLM
// row laddered 2026-08-30 with upstream defaultEffort 'max'); gpt-5.6-luna and
// claude-fable-5 run EFFORTS_THROUGH_MAX low..max; mimo-v2.5 and minimax-m3
// expose only {'high'} (no depth ladder upstream); z-ai/glm-5.2 and
// kimi-k3-eco accept but ignore reasoning_effort, so no clamp.
//
// Clamping mirrors upstream's resolveFreebuffReasoningEffort
// (reference/freebuff/common/src/constants/freebuff-models.ts): clamp-DOWN,
// keyed on the actually-served model, medium→high on DeepSeek V4 and Ox
// Alpha. For rows with no ladder upstream returns null and passes the client
// value through untouched (MIMO/MiniMax treat any rung as thinking-on;
// GLM/Kimi ignore it) — clamping those rows to "high" is the
// alias-equivalent normalization.
//
// Models absent from the catalog get the full ladder (no clamping). The
// provisioned -max variants are NOT listed: they are blocked at the
// ServedModels gate (issue #153) before conversion ever runs.

// ReasoningLookupFn looks up cached reasoning content by tool ID or by content + toolCalls JSON.
type ReasoningLookupFn func(toolID string, content, toolCallsJSON string) (reasoning, signature string, ok bool)

// effortsForModel returns the allowed effort rungs for a model: the catalog
// ladder, else the full ladder for unlisted models.
func effortsForModel(model string) []string {
	if allowed := modelcat.Efforts(model); allowed != nil {
		return allowed
	}
	return reasoningLadder[:]
}

// reasoningRank returns the position of effort on the ladder, or -1 when the
// effort is not a rung.
func reasoningRank(effort string) int {
	for i, rung := range reasoningLadder {
		if rung == effort {
			return i
		}
	}
	return -1
}

// clampReasoningEffort clamps requested DOWN to the nearest allowed rung no
// higher than requested (official clampReasoningEffort semantics): when every
// allowed rung is above what was asked for, the lowest allowed rung wins;
// when requested is absent or unrecognized, fallback wins. It never rejects
// and never changes the model.
func clampReasoningEffort(requested string, allowed []string, fallback string) string {
	if len(allowed) == 0 {
		return fallback
	}
	wanted := reasoningRank(requested)
	if wanted < 0 {
		return fallback
	}
	best := -1
	for _, candidate := range allowed {
		rank := reasoningRank(candidate)
		if rank < 0 || rank > wanted {
			continue
		}
		if rank > best {
			best = rank
		}
	}
	if best >= 0 {
		return reasoningLadder[best]
	}
	// Everything on offer is above what was asked for: give the least of them.
	lowest := allowed[0]
	for _, candidate := range allowed[1:] {
		if reasoningRank(candidate) < reasoningRank(lowest) {
			lowest = candidate
		}
	}
	return lowest
}

// isDeepSeekModel reports whether the route is one of the DeepSeek V4 models.
// DeepSeek routes accept prompt-cache hints (#84). Tolerates both the
// registry's full ids and bare model ids. The provisioned -max variants are
// blocked at the ServedModels gate (issue #153) and never reach conversion.
func isDeepSeekModel(model string) bool {
	m := strings.ToLower(model)
	return strings.HasSuffix(m, "deepseek-v4-flash") ||
		strings.HasSuffix(m, "deepseek-v4-pro")
}

// isMediumlessLadderModel reports whether the model's upstream effort ladder
// omits "medium" (DeepSeek V4, Ox Alpha, GLM 5.3 Flash): all rewrite a
// requested "medium" to "high" (#112) instead of the generic down-clamp,
// which would pick "low". The ladder facts live in modelcat.
func isMediumlessLadderModel(model string) bool {
	return modelcat.IsMediumlessLadderModel(model)
}

// isStrictReasoningModel reports whether the model requires an explicit reasoning_content
// field on assistant messages with tool calls (e.g. MiMo, DeepSeek-V4, Kimi).
// The matching rules live in modelcat.
func isStrictReasoningModel(model string) bool {
	return modelcat.IsStrictReasoningModel(model)
}

var (
	leakedThinkTagsRe   = regexp.MustCompile(`(?is)<think>(.*?)</think>|<thinking>(.*?)</thinking>|<reasoning>(.*?)</reasoning>|<antml:thinking>(.*?)</antml:thinking>`)
	unclosedThinkTagsRe = regexp.MustCompile(`(?is)<(?:think|thinking|reasoning|antml:thinking)>(.*)$`)
)

// extractLeakedThinkTags parses leaked reasoning tags (<think>, <thinking>,
// <reasoning>, <antml:thinking>) from text content. It returns the concatenated
// reasoning string and the cleaned content with tags stripped.
func extractLeakedThinkTags(content string) (string, string) {
	matches := leakedThinkTagsRe.FindAllStringSubmatch(content, -1)
	var reasoningParts []string
	for _, match := range matches {
		for _, part := range match[1:] {
			if part != "" {
				reasoningParts = append(reasoningParts, part)
				break
			}
		}
	}
	cleaned := leakedThinkTagsRe.ReplaceAllString(content, "")
	// If an unclosed think tag exists at the end of the text (e.g. generation
	// cutoff at max_tokens), extract the partial thinking and strip the tag.
	if unclosed := unclosedThinkTagsRe.FindStringSubmatch(cleaned); len(unclosed) > 1 {
		if partial := strings.TrimSpace(unclosed[1]); partial != "" {
			reasoningParts = append(reasoningParts, partial)
		}
		cleaned = unclosedThinkTagsRe.ReplaceAllString(cleaned, "")
	}
	if len(reasoningParts) == 0 {
		return "", content
	}
	return strings.Join(reasoningParts, "\n"), strings.TrimSpace(cleaned)
}

// normalizeReasoning rewrites the outbound reasoning_effort for the target
// model:
//
//   - reasoning_effort is always sent PLAIN, clamped to the model's allowed
//     rungs (down-nearest, never rejected; unknown efforts fall back to
//     "high"). The DeepSeek thinking translation is server-side per issue
//     #111 — the proxy never emits a thinking block.
//   - "medium" on mediumless ladders rewrites to "high" — a deliberate
//     proxy translation policy, NOT an upstream rule: upstream's authority
//     (resolveFreebuffReasoningEffort) special-cases medium→high ONLY for
//     DeepSeek, and clamps everything else DOWN (GLM medium → low = zero
//     thinking chars, measured). Real CLI clients can never send medium on
//     those ladders (refused at /reasoning), so this rewrite only ever
//     fires for stale/non-CLI preferences, where mapping to high preserves
//     the user's evident intent (high > low for deliberation) instead of
//     silently downgrading to a rung that emits no thinking at all.
//   - The normalized effort is mirrored into codebuff_metadata
//     freebuff_reasoning_effort on the upstream chat body — the field the
//     server's effort authority actually reads.
//   - reasoning.enabled === false or thinking.type "disabled" suppresses the
//     effort entirely (no reasoning_effort, no thinking field).
func normalizeReasoning(payload, out map[string]any) {
	model, _ := out["model"].(string)
	eff := ""
	if v, ok := out["reasoning_effort"].(string); ok && v != "" {
		eff = strings.ToLower(strings.TrimSpace(v))
	}
	if eff == "" {
		eff = ExtractReasoningEffort(payload)
	}

	disabled := false
	if rObj, ok := payload["reasoning"].(map[string]any); ok {
		if en, ok := rObj["enabled"].(bool); ok && !en {
			disabled = true
		}
	}
	// A client that explicitly disables thinking (Anthropic-style
	// thinking:{type:"disabled"}) must win even when reasoning_effort is
	// also present: re-enabling thinking the client turned off is a silent
	// behavioral override.
	if tObj, ok := payload["thinking"].(map[string]any); ok {
		if tt, ok := tObj["type"].(string); ok && strings.EqualFold(strings.TrimSpace(tt), "disabled") {
			disabled = true
		}
	}

	switch {
	case disabled:
		delete(out, "reasoning_effort")
	case eff == "" || eff == "none" || eff == "disabled":
		// No effort requested: leave the body untouched.
	default:
		var clamped string
		if isMediumlessLadderModel(model) && eff == "medium" {
			// CLI resolveFreebuffReasoningEffort: medium is intentionally
			// absent from these ladders (DeepSeek V4, Ox Alpha) and rewrites
			// to high — never down to low (the generic clamp would say low).
			clamped = "high"
		} else {
			clamped = clampReasoningEffort(eff, effortsForModel(model), defaultReasoningEffort)
		}
		out["reasoning_effort"] = clamped
	}
}

// ---------------------------------------------------------------------------
// Reasoning-in-content injection (issue #44).
//
// Some legacy clients never render a reasoning channel; when
// REASONING_IN_CONTENT is set (default off), reasoning_content is folded into
// delta.content as "<tag>...</tag>" text so those clients still see the
// reasoning. The reasoning channel is preserved alongside, and
// reasoning_details is never folded (it is replayed verbatim).
// ---------------------------------------------------------------------------

// reasoningInContentTag is the default think-tag label when the env var is a
// bare boolean.
const reasoningInContentTag = "think"

// foldReasoningIntoContent appends the <tag>reasoning</tag> text to the
// delta's content when folding is enabled. Reasoning precedes text (the
// model layer enqueues reasoning deltas before text deltas); non-string
// content is left untouched.
func foldReasoningIntoContent(delta map[string]any, reasoning string, tag string) {
	if tag == "" || reasoning == "" {
		return
	}
	switch c := delta["content"].(type) {
	case string:
		delta["content"] = "<" + tag + ">" + reasoning + "</" + tag + ">" + c
	case nil:
		delta["content"] = "<" + tag + ">" + reasoning + "</" + tag + ">"
	}
}
