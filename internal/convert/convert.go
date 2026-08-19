// Package convert implements pure OpenAI request/response normalization for
// the freebuff-proxy bridge.
//
// It performs no I/O and depends on no other internal package. Everything
// here is a pure function over JSON: request sanitization (parameter
// whitelist, developer→system role rewrite, tool-schema normalization), SSE
// frame encoding, per-chunk stream sanitization, and the non-streaming
// response accumulator. Envelope injection (codebuff_metadata, forced
// stream, x-freebuff headers) is deliberately out of scope — that lives in
// internal/upstream.
package convert

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxSchemaDepth caps recursive JSON-schema normalization. Ported from
// proxy-freebuff's normalizeSchemaMap, which resolves with depth 12.
const maxSchemaDepth = 12

// maxSchemaNodes is the total-node budget for one request's schema
// normalization: beyond it, remaining structure is returned unchanged. A
// pathological schema would otherwise be re-copied at every ancestor up to
// maxSchemaDepth (12x memory amplification). Tests may shrink it.
var maxSchemaNodes = 100_000

// capHint returns a+b for use as a make() size hint, or 0 when the sum would
// overflow int. An unguarded len(a)+len(b) hint can wrap negative and panic
// the runtime (makeslice: "cap out of range" / "len out of range"; makemap
// clamps negative hints but a wrapped positive is still wrong). On overflow
// we drop the hint entirely and let the container grow dynamically - the
// hint is pure preallocation optimization, never correctness. CodeQL:
// "size computation for allocation may overflow" (go/allocation-size-overflow,
// convert.go:432,754,905,1006).
func capHint(a, b int) int {
	if a > math.MaxInt-b {
		return 0
	}
	return a + b
}

// upstreamKeys is the whitelist of chat-completions body keys forwarded to
// the upstream, plus messages/model which are always kept. Ported from
// freebuff-api-kiprana's _UPSTREAM_CHAT_KEYS. Note "stream" is NOT
// whitelisted: the upstream layer forces stream:true itself.
var upstreamKeys = map[string]bool{
	"frequency_penalty":     true,
	"logit_bias":            true,
	"logprobs":              true,
	"max_completion_tokens": true,
	"max_tokens":            true,
	"metadata":              true,
	"modalities":            true,
	"parallel_tool_calls":   true,
	"presence_penalty":      true,
	"reasoning_effort":      true,
	"response_format":       true,
	"seed":                  true,
	"service_tier":          true,
	"stop":                  true,
	"store":                 true,
	"stream_options":        true,
	"temperature":           true,
	"tool_choice":           true,
	"tools":                 true,
	"top_logprobs":          true,
	"top_p":                 true,
	"user":                  true,
}

var fallbackCounter atomic.Uint64

// randHex returns n random bytes hex-encoded (16 bytes → the 32 hex chars of
// a uuid4 hex, 12 bytes → 24 hex chars). Falls back to time+counter if
// crypto/rand fails, which practically never happens.
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	return fmt.Sprintf("%x%x", time.Now().UnixNano(), fallbackCounter.Add(1))
}

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

// modelReasoningEfforts is the per-model effort allowance, mirroring
// freebuff-models.ts (08/13 catalog): deepseek-v4-flash AND deepseek-v4-pro
// expose [low, high, max] (Pro gained low on 08/13; medium intentionally
// absent — rewritten to high, see normalizeReasoning), gpt-5.6-luna
// EFFORTS_THROUGH_MAX low..max (xhigh included), muse-spark
// EFFORTS_THROUGH_XHIGH minimal..xhigh. The provisioned -max variants
// (deepseek-v4-flash-max, deepseek-v4-pro-max, gpt-5.6-luna-max) mirror
// their base model's ladder — same lanes, higher ceiling tier. Models
// absent from the table get the full ladder (no clamping): kimi/glm are
// deliberately absent (CrofAI ignores reasoning_effort) and gemini rows
// are absent (helper models, no upstream restriction). The table is
// refreshable at runtime via SetModelEffortLookup.
var modelReasoningEfforts = map[string][]string{
	"deepseek/deepseek-v4-flash":      {"low", "high", "max"},
	"deepseek/deepseek-v4-pro":        {"low", "high", "max"},
	"deepseek/deepseek-v4-flash-max":  {"low", "high", "max"},
	"deepseek/deepseek-v4-pro-max":    {"low", "high", "max"},
	"mimo/mimo-v2.5":                  {"high"},
	"mimo/mimo-v2.5-pro":              {"high"},
	"anthropic/claude-fable-5":        {"low", "medium", "high", "xhigh", "max"},
	"openai/gpt-5.6-luna":             {"low", "medium", "high", "xhigh", "max"},
	"openai/gpt-5.6-luna-max":         {"low", "medium", "high", "xhigh", "max"},
	"meta/muse-spark-1.2-contributor": {"minimal", "low", "medium", "high", "xhigh"},
}

// ReasoningLookupFn looks up cached reasoning content by tool ID or by content + toolCalls JSON.
type ReasoningLookupFn func(toolID string, content, toolCallsJSON string) (reasoning, signature string, ok bool)

var globalReasoningLookup atomic.Pointer[ReasoningLookupFn]

// SetReasoningLookup installs a global reasoning lookup hook used to restore
// missing reasoning_content on assistant messages in multi-turn requests.
// Passing nil clears the hook.
func SetReasoningLookup(fn ReasoningLookupFn) {
	if fn == nil {
		globalReasoningLookup.Store(nil)
		return
	}
	globalReasoningLookup.Store(&fn)
}

// modelEffortLookup, when set, overrides modelReasoningEfforts for the model
// in question. Main wires this to registry data when the registry exposes
// per-model effort arrays; the hardcoded table is the fallback.
var modelEffortLookup atomic.Pointer[func(string) []string]

// SetModelEffortLookup installs a per-model effort lookup used by the
// reasoning clamp. The function receives a model id and returns the allowed
// effort rungs (ascending ladder order), or nil to fall back to the
// hardcoded table. Passing nil removes the override.
func SetModelEffortLookup(fn func(string) []string) {
	if fn == nil {
		modelEffortLookup.Store(nil)
		return
	}
	modelEffortLookup.Store(&fn)
}

// effortsForModel returns the allowed effort rungs for a model: the override
// lookup when it answers, else the hardcoded table (with the full ladder for
// unlisted models).
func effortsForModel(model string) []string {
	if p := modelEffortLookup.Load(); p != nil {
		if allowed := (*p)(model); allowed != nil {
			return allowed
		}
	}
	if allowed, ok := modelReasoningEfforts[model]; ok {
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
// DeepSeek routes accept prompt-cache hints (#84) and rewrite requested
// "medium" to "high" (#112). Tolerates both the registry's full ids and bare
// model ids, and the provisioned `-max` variants (which upstream routes to the
// same DeepSeek lanes).
func isDeepSeekModel(model string) bool {
	m := strings.ToLower(model)
	return strings.HasSuffix(m, "deepseek-v4-flash") ||
		strings.HasSuffix(m, "deepseek-v4-pro") ||
		strings.HasSuffix(m, "deepseek-v4-flash-max") ||
		strings.HasSuffix(m, "deepseek-v4-pro-max")
}

// isStrictReasoningModel reports whether the model requires an explicit reasoning_content
// field on assistant messages with tool calls (e.g. MiMo, DeepSeek-V4, Kimi).
func isStrictReasoningModel(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "mimo") || strings.Contains(m, "deepseek-v4") || strings.Contains(m, "kimi")
}

var leakedThinkTagsRe = regexp.MustCompile(`(?is)<think>(.*?)</think>|<thinking>(.*?)</thinking>|<reasoning>(.*?)</reasoning>|<antml:thinking>(.*?)</antml:thinking>`)

// extractLeakedThinkTags parses leaked reasoning tags (<think>, <thinking>,
// <reasoning>, <antml:thinking>) from text content. It returns the concatenated
// reasoning string and the cleaned content with tags stripped.
func extractLeakedThinkTags(content string) (string, string) {
	matches := leakedThinkTagsRe.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return "", content
	}
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
	return strings.Join(reasoningParts, "\n"), strings.TrimSpace(cleaned)
}

// normalizeReasoning rewrites the outbound reasoning_effort for the target
// model:
//
//   - reasoning_effort is always sent PLAIN, clamped to the model's allowed
//     rungs (down-nearest, never rejected; unknown efforts fall back to
//     "high"). The DeepSeek thinking translation is server-side per issue
//     #111 — the proxy never emits a thinking block.
//   - "medium" on a DeepSeek route rewrites to "high" first (CLI
//     resolveFreebuffReasoningEffort: medium → high on Flash/Pro, #112).
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
	// also present (review P2): re-enabling thinking the client turned off
	// is a silent behavioral override.
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
		if isDeepSeekModel(model) && eff == "medium" {
			// CLI resolveFreebuffReasoningEffort: medium is intentionally
			// absent from the DeepSeek ladders and rewrites to high — never
			// down to low (the generic clamp would say low).
			clamped = "high"
		} else {
			clamped = clampReasoningEffort(eff, effortsForModel(model), defaultReasoningEffort)
		}
		out["reasoning_effort"] = clamped
	}
}

// NormalizeRequest sanitizes a client OpenAI chat-completions request body:
//
//   - keeps ONLY the whitelisted upstream keys (plus messages and model);
//     null-valued whitelisted keys are dropped (they are meaningless upstream,
//     matching kiprana's "value is not None" filter)
//   - converts message role "developer" to "system"
//   - normalizes tool JSON schemas (bare $ref/$defs resolution, nullable
//     anyOf/oneOf simplification, type/enum/const cleanup, depth cap 12)
//   - extracts and normalizes reasoning effort from alternate structures
//
// modelOverride, when non-empty, replaces the client's model in the
// forwarded body (used for alias resolution). The returned bytes are compact
// JSON. Errors only occur on invalid JSON or a non-object body.
func NormalizeRequest(body []byte, modelOverride string) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if payload == nil {
		return nil, fmt.Errorf("convert: request body must be a JSON object")
	}
	out := make(map[string]any, len(payload))
	for key, value := range payload {
		if key == "model" || key == "messages" {
			out[key] = value
			continue
		}
		if upstreamKeys[key] && value != nil {
			out[key] = value
		}
	}
	if modelOverride != "" {
		out["model"] = modelOverride
	}
	normalizeReasoning(payload, out)
	model, _ := out["model"].(string)
	normalizeMessages(out, model)
	// Optional prompt compression (#58): drops middle non-tool turns and caps
	// long content, env-gated (COMPRESS_PROMPT=true), never touching tool
	// calls/results or the current message.
	if compressionEnabled() {
		if msgs, ok := out["messages"].([]any); ok {
			out["messages"], _ = compressMessages(msgs)
		}
	}
	// DeepSeek prompt-cache hints (#84): cache_control ephemeral on the stable
	// context prefix (messages at indices 2-3), env-gated default on.
	if model, _ := out["model"].(string); deepseekCacheControlEnabled() && isDeepSeekModel(model) {
		if msgs, ok := out["messages"].([]any); ok {
			InjectCacheControl(msgs)
		}
	}
	normalizeToolSchemas(out)
	return json.Marshal(out)
}

// normalizeMessages rewrites message role "developer" to "system" in place,
// extracts leaked think tags from assistant messages, restores missing reasoning_content
// via globalReasoningLookup, and ensures content: null on assistant tool calls.
func normalizeMessages(payload map[string]any, model string) {
	msgs, _ := payload["messages"].([]any)
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "developer" {
			msg["role"] = "system"
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}

		// Check for non-empty tool_calls
		var hasToolCalls bool
		var tcSlice []any
		if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 {
			hasToolCalls = true
			tcSlice = tcs
		} else if tcs, ok := msg["tool_calls"].([]map[string]any); ok && len(tcs) > 0 {
			hasToolCalls = true
			for _, tc := range tcs {
				tcSlice = append(tcSlice, tc)
			}
		}

		if hasToolCalls {
			// If content is "" (empty string) or nil, set msg["content"] = nil (explicit JSON null)
			cVal, hasContent := msg["content"]
			if !hasContent || cVal == nil || cVal == "" {
				msg["content"] = nil
			}

			// If reasoning_content is missing or ""
			rc, _ := msg["reasoning_content"].(string)
			if rc == "" {
				// 1. If content has string, check extractLeakedThinkTags
				if cStr, ok := cVal.(string); ok && cStr != "" {
					reasoning, cleaned := extractLeakedThinkTags(cStr)
					if reasoning != "" {
						msg["reasoning_content"] = reasoning
						rc = reasoning
						if cleaned == "" {
							msg["content"] = nil
						} else {
							msg["content"] = cleaned
						}
					}
				}

				// 2. If still missing, check globalReasoningLookup if set
				if rc == "" {
					if fnPtr := globalReasoningLookup.Load(); fnPtr != nil && *fnPtr != nil {
						fn := *fnPtr
						// Look up by each tool call id
						for _, item := range tcSlice {
							if tcMap, ok := item.(map[string]any); ok {
								if id, _ := tcMap["id"].(string); id != "" {
									if r, _, ok := fn(id, "", ""); ok && r != "" {
										rc = r
										msg["reasoning_content"] = r
										break
									}
								}
							}
						}
						// If still not found, look up by content + toolCalls JSON
						if rc == "" {
							cStr := ""
							if s, ok := msg["content"].(string); ok {
								cStr = s
							}
							var tcJSON string
							if b, err := json.Marshal(msg["tool_calls"]); err == nil {
								tcJSON = string(b)
							}
							if r, _, ok := fn("", cStr, tcJSON); ok && r != "" {
								rc = r
								msg["reasoning_content"] = r
							}
						}
					}
				}

				// 3. If still missing and isStrictReasoningModel(model), set msg["reasoning_content"] = ""
				if rc == "" && isStrictReasoningModel(model) {
					msg["reasoning_content"] = ""
				}
			}
		} else {
			// No tool calls: still extract leaked think tags if reasoning_content is missing
			rc, _ := msg["reasoning_content"].(string)
			if rc == "" {
				if cStr, ok := msg["content"].(string); ok && cStr != "" {
					reasoning, cleaned := extractLeakedThinkTags(cStr)
					if reasoning != "" {
						msg["reasoning_content"] = reasoning
						msg["content"] = cleaned
					}
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Optional prompt & context compression (issue #58).
//
// Env-gated (COMPRESS_PROMPT=true, default off) and deliberately
// conservative: only plain user/assistant content turns strictly inside the
// middle of the conversation are dropped, tool calls/results and the last
// message are never touched, and long content is capped with an explicit
// summary marker. Tests may shrink the budget vars.
// ---------------------------------------------------------------------------

const (
	// compressMarkerPrefix/Suffix form the summary marker inserted where the
	// truncation begins: "[truncated by freebuff-proxy compression; N earlier
	// messages omitted]".
	compressMarkerPrefix = "[truncated by freebuff-proxy compression; "
	compressMarkerSuffix = " earlier messages omitted]"
	// compressContentMarker is appended to a kept message whose content was
	// capped.
	compressContentMarker = "[truncated by freebuff-proxy compression]"
)

var (
	// compressKeepLast is the number of trailing messages that are always
	// kept (the current turn and recent context).
	compressKeepLast = 10
	// compressMaxContentBytes caps string content on kept user/assistant
	// turns (never the last message, never tool results).
	compressMaxContentBytes = 8 << 10
)

// compressionEnabled reports whether prompt compression is on
// (COMPRESS_PROMPT=true).
func compressionEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("COMPRESS_PROMPT")))
	switch v {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// compressMessages compresses a message list in place: middle user/assistant
// content turns beyond the trailing budget are dropped and summarized by ONE
// marker message; long content on kept user/assistant turns is capped. Tool
// results, assistant tool_calls, system messages and the last message are
// never dropped or truncated. Returns the (possibly new) slice and the
// number of messages omitted.
func compressMessages(messages []any) ([]any, int) {
	capLongContents(messages)
	n := len(messages)
	if n <= compressKeepLast {
		return messages, 0
	}
	keepStart := n - compressKeepLast // first index of the trailing window

	// Pass 1: count droppable middle turns and where the marker goes.
	dropped := 0
	markerIdx := -1
	for i := 0; i < keepStart; i++ {
		m, ok := messages[i].(map[string]any)
		if !ok {
			continue // non-map entry: cannot classify, keep it
		}
		if mustKeepMessage(m) {
			continue // system prompt, tool results, assistant tool_calls
		}
		dropped++
		if markerIdx < 0 {
			markerIdx = i
		}
	}
	if dropped == 0 {
		return messages, 0
	}

	// Pass 2: rebuild, replacing the dropped span with one marker message.
	out := make([]any, 0, capHint(n-dropped, 1))
	for i := 0; i < n; i++ {
		if i < keepStart {
			m, ok := messages[i].(map[string]any)
			if ok && !mustKeepMessage(m) {
				if i == markerIdx {
					out = append(out, map[string]any{
						"role":    "system",
						"content": fmt.Sprintf("%s%d%s", compressMarkerPrefix, dropped, compressMarkerSuffix),
					})
				}
				continue
			}
		}
		out = append(out, messages[i])
	}
	return out, dropped
}

func roleOf(m map[string]any) string {
	role, _ := m["role"].(string)
	return role
}

// mustKeepMessage reports whether a message must survive compression: tool
// results, assistant tool_calls and non-user/assistant roles (system,
// developer, function) are never dropped — dropping them would break the
// tool-call schema or lose instructions.
func mustKeepMessage(m map[string]any) bool {
	switch roleOf(m) {
	case "user":
		return false
	case "assistant":
		if _, has := m["tool_calls"]; has {
			return true
		}
		return false
	default:
		return true // system, developer, tool, function, unknown
	}
}

// capLongContents truncates string content longer than compressMaxContentBytes
// on kept user/assistant turns, appending the summary marker. The last
// message and tool messages are never touched.
func capLongContents(messages []any) {
	if len(messages) == 0 {
		return
	}
	for i := 0; i < len(messages)-1; i++ { // never the last (current) message
		m, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		switch roleOf(m) {
		case "user", "assistant":
		default:
			continue
		}
		if _, has := m["tool_calls"]; has {
			continue
		}
		content, ok := m["content"].(string)
		if !ok || len(content) <= compressMaxContentBytes {
			continue
		}
		m["content"] = truncateRunes(content, compressMaxContentBytes) + "…" + compressContentMarker
	}
}

// truncateRunes cuts s to at most maxBytes bytes on a rune boundary.
func truncateRunes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := 0
	for cut = range s {
		if cut >= maxBytes {
			break
		}
	}
	return s[:cut]
}

// ---------------------------------------------------------------------------
// DeepSeek prompt-cache cache_control injection (issue #84).
// Ported from freebuff-reverse/internal/channels/freebuff/model.go
// injectCacheControl.
// ---------------------------------------------------------------------------

// deepseekCacheControlEnabled reports whether cache_control injection is on.
// Default ON; set CACHE_CONTROL_INJECTION=false (or 0/off/no/disabled) to
// disable, preserving SAFE_MODE behavior.
func deepseekCacheControlEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("CACHE_CONTROL_INJECTION")))
	switch v {
	case "0", "false", "off", "no", "disabled":
		return false
	}
	return true
}

// InjectCacheControl adds {"type":"ephemeral"} cache_control to every content
// block of messages at indices 2 and 3 (the stable context prefix) when the
// block does not already carry one. Messages whose content is not a block
// array (e.g. plain strings) are skipped untouched. Exportable so the CLI
// envelope builder can apply the same hints after rewriting messages.
func InjectCacheControl(messages []any) {
	for i := 2; i < len(messages) && i < 4; i++ {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		content, ok := msg["content"]
		if !ok {
			continue
		}
		blocks, ok := content.([]any)
		if !ok {
			continue
		}
		for _, raw := range blocks {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if _, exists := block["cache_control"]; !exists {
				block["cache_control"] = map[string]any{"type": "ephemeral"}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Tool-schema normalization (ported from proxy-freebuff/lib/convert.js,
// normalizeToolSchemas / normalizeSchemaMap, lines ~40-154).
// ---------------------------------------------------------------------------

// normalizeToolSchemas normalizes fn.parameters for every function tool in
// the payload, in place. Each tool's parameters are normalized through the
// schema cache (#67): the raw schema JSON hash + starting node budget key a
// bounded, mutex-guarded LRU, so repeated tool-call loops re-send identical
// context without re-running normalization.
func normalizeToolSchemas(payload map[string]any) {
	tools, _ := payload["tools"].([]any)
	if len(tools) == 0 {
		return
	}
	// One node budget per request, shared across tools.
	budget := maxSchemaNodes
	hasEndTurn := false
	for _, t := range tools {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		if name, ok := fn["name"].(string); ok && name == "end_turn" {
			hasEndTurn = true
		}
		params, ok := fn["parameters"].(map[string]any)
		if !ok {
			continue
		}
		fn["parameters"] = normalizeToolSchemaCached(params, &budget)
	}
	// Inject end_turn tool definition to pass Codebuff's foreign_toolset validation
	if !hasEndTurn {
		payload["tools"] = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "end_turn",
				"description": "Signal the end of the current task.",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
		})
	}
}

// ---------------------------------------------------------------------------
// Tool-schema cache (issue #67/#95).
// ---------------------------------------------------------------------------

// schemaCacheKey identifies one cached normalization: the SHA-256 of the raw
// schema JSON plus the node budget it was computed under. normalizeSchemaMap
// is a pure function of (raw schema, starting budget, maxSchemaDepth), so the
// pair pins the output exactly.
type schemaCacheKey struct {
	hash   [sha256.Size]byte
	budget int
}

// schemaCacheMax bounds the LRU. Schemas are usually small; 256 entries
// covers a typical long tool-call loop many times over.
const schemaCacheMax = 256

// schemaCache is a small mutex-guarded LRU of normalized tool schemas.
type schemaCacheType struct {
	mu      sync.Mutex
	entries map[schemaCacheKey]map[string]any
	order   []schemaCacheKey // most-recently-used last
}

var schemaCache = schemaCacheType{entries: make(map[schemaCacheKey]map[string]any)}

// schemaCacheHits/Misses count cache outcomes (tests + diagnostics).
var (
	schemaCacheHits   atomic.Uint64
	schemaCacheMisses atomic.Uint64
)

// resetSchemaCache clears the cache and its counters (tests).
func resetSchemaCache() {
	schemaCache.mu.Lock()
	schemaCache.entries = make(map[schemaCacheKey]map[string]any)
	schemaCache.order = schemaCache.order[:0]
	schemaCache.mu.Unlock()
	schemaCacheHits.Store(0)
	schemaCacheMisses.Store(0)
}

// schemaCacheStats returns the hit/miss counters.
func schemaCacheStats() (hits, misses uint64) {
	return schemaCacheHits.Load(), schemaCacheMisses.Load()
}

// normalizeToolSchemaCached returns the normalized tool schema for params,
// consulting the cache first. Cache hits return a deep clone (callers must
// never mutate cached values); misses normalize, store the result and return
// it directly. The shared per-request budget is decremented only on misses.
func normalizeToolSchemaCached(params map[string]any, budget *int) map[string]any {
	raw, err := json.Marshal(params)
	if err != nil {
		// Values not produced by encoding/json cannot be hashed reliably:
		// normalize uncached.
		return normalizeSchemaMap(params, extractDefinitions(params), params, nil, maxSchemaDepth, budget)
	}
	key := schemaCacheKey{hash: sha256.Sum256(raw), budget: *budget}
	if cached, ok := schemaCacheGet(key); ok {
		schemaCacheHits.Add(1)
		return cloneValue(cached, maxSchemaDepth).(map[string]any)
	}
	schemaCacheMisses.Add(1)
	normalized := normalizeSchemaMap(params, extractDefinitions(params), params, nil, maxSchemaDepth, budget)
	schemaCachePut(key, normalized)
	return normalized
}

// schemaCacheGet returns the cached normalized schema for key, promoting it
// to most-recently-used.
func schemaCacheGet(key schemaCacheKey) (map[string]any, bool) {
	schemaCache.mu.Lock()
	defer schemaCache.mu.Unlock()
	v, ok := schemaCache.entries[key]
	if !ok {
		return nil, false
	}
	for i, k := range schemaCache.order {
		if k == key {
			schemaCache.order = append(schemaCache.order[:i], schemaCache.order[i+1:]...)
			break
		}
	}
	schemaCache.order = append(schemaCache.order, key)
	return v, true
}

// schemaCachePut stores normalized under key, evicting the least-recently-used
// entry when the cache is full.
func schemaCachePut(key schemaCacheKey, normalized map[string]any) {
	schemaCache.mu.Lock()
	defer schemaCache.mu.Unlock()
	if _, ok := schemaCache.entries[key]; ok {
		schemaCache.entries[key] = normalized
		return
	}
	if len(schemaCache.entries) >= schemaCacheMax {
		oldest := schemaCache.order[0]
		delete(schemaCache.entries, oldest)
		schemaCache.order = schemaCache.order[1:]
	}
	schemaCache.entries[key] = normalized
	schemaCache.order = append(schemaCache.order, key)
}

// extractDefinitions merges a schema node's "definitions" and "$defs" maps
// ($defs wins on name collision, matching JS Object.assign order). Returns
// nil when neither exists.
func extractDefinitions(node map[string]any) map[string]any {
	var merged map[string]any
	if defs, ok := node["definitions"].(map[string]any); ok {
		merged = make(map[string]any, len(defs))
		for k, v := range defs {
			merged[k] = v
		}
	}
	if defs, ok := node["$defs"].(map[string]any); ok {
		if merged == nil {
			merged = make(map[string]any, len(defs))
		}
		for k, v := range defs {
			merged[k] = v
		}
	}
	return merged
}

// mergeDefinitions combines a parent definition table with one extracted
// from a nested node; the local table wins on collision.
func mergeDefinitions(parent, local map[string]any) map[string]any {
	if parent == nil {
		return local
	}
	if local == nil {
		return parent
	}
	merged := make(map[string]any, capHint(len(parent), len(local)))
	for k, v := range parent {
		merged[k] = v
	}
	for k, v := range local {
		merged[k] = v
	}
	return merged
}

// normalizeSchemaMap normalizes one JSON-schema node: resolves $ref nodes
// against the definition table AND the schema root (issue #95 — JSON-pointer
// lookup with sibling merging and a cycle guard), recurses into values
// (depth-capped and node-budgeted), drops definitions/$defs/nullable,
// simplifies nullable anyOf/oneOf, and cleans up type/enum/const fields.
//
// root is the tool's raw parameters schema, used to resolve "#/..." JSON
// pointers (inlineLocalSchemaRefs semantics); refStack tracks the refs on the
// current descent path so a re-entered ref resolves to {} instead of looping.
// The returned map is always freshly allocated except at the depth cap or
// budget exhaustion, where the node is returned as-is.
func normalizeSchemaMap(node map[string]any, defs map[string]any, root map[string]any, refStack map[string]bool, maxDepth int, budget *int) map[string]any {
	if maxDepth <= 0 || *budget <= 0 {
		return node // cap: leave the remaining structure untouched
	}
	*budget--
	defs = mergeDefinitions(defs, extractDefinitions(node))

	if ref, _ := node["$ref"].(string); ref != "" && strings.HasPrefix(ref, "#/") {
		if refStack[ref] {
			return map[string]any{} // cycle guard: re-entered ref resolves to {}
		}
		nextStack := make(map[string]bool, len(refStack)+1)
		for k, v := range refStack {
			nextStack[k] = v
		}
		nextStack[ref] = true

		// 1. Bare refs against the merged definition table (existing
		//    behavior; also handles #/definitions/<name> and #/$defs/<name>).
		if replaced, ok := tryResolveRef(node, defs, maxDepth); ok {
			if resolved, isMap := replaced.(map[string]any); isMap {
				return normalizeSchemaMap(resolved, defs, root, nextStack, maxDepth-1, budget)
			}
			return node
		}

		// 2. JSON-pointer resolution against the schema root, merging $ref
		//    siblings over the resolved target (inlineLocalSchemaRefs).
		if target, ok := lookupJsonPointer(root, ref); ok {
			resolved := normalizeSchemaValue(target, defs, root, nextStack, maxDepth-1, budget)
			rm, isMap := resolved.(map[string]any)
			if !isMap {
				return node
			}
			if siblings := withoutRef(node); len(siblings) > 0 {
				merged := mergeMaps(rm, siblings) // siblings win, JS {...resolved, ...siblings}
				return normalizeSchemaMap(merged, defs, root, refStack, maxDepth-1, budget)
			}
			return rm
		}

		// 3. Unresolvable: keep bare refs as-is (existing behavior); visit
		//    the remaining siblings when the ref carries any.
		if siblings := withoutRef(node); len(siblings) > 0 {
			return normalizeSchemaMap(siblings, defs, root, refStack, maxDepth-1, budget)
		}
		return node
	}

	normalized := make(map[string]any, len(node))
	for key, value := range node {
		normalized[key] = normalizeSchemaValue(value, defs, root, refStack, maxDepth-1, budget)
	}
	delete(normalized, "definitions")
	delete(normalized, "$defs")
	delete(normalized, "nullable")
	normalized = simplifyNullableCombinator(normalized, "anyOf")
	normalized = simplifyNullableCombinator(normalized, "oneOf")
	normalizeTypeField(normalized)
	normalizeEnumField(normalized)
	normalizeConstField(normalized)
	return normalized
}

// normalizeSchemaValue recurses into arrays and objects; scalars pass
// through untouched.
func normalizeSchemaValue(value any, defs map[string]any, root map[string]any, refStack map[string]bool, maxDepth int, budget *int) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = normalizeSchemaValue(e, defs, root, refStack, maxDepth, budget)
		}
		return out
	case map[string]any:
		return normalizeSchemaMap(v, defs, root, refStack, maxDepth, budget)
	default:
		return value
	}
}

// lookupJsonPointer resolves a "#/..." JSON pointer against the schema root,
// decoding ~1 → "/" and ~0 → "~" per segment (JS decodeJsonPointerSegment in
// openai-compatible-prepare-tools.ts). Array indices resolve numerically.
func lookupJsonPointer(root map[string]any, pointer string) (any, bool) {
	if !strings.HasPrefix(pointer, "#/") {
		return nil, false
	}
	var current any = root
	for _, segment := range strings.Split(pointer[2:], "/") {
		segment = strings.ReplaceAll(segment, "~1", "/")
		segment = strings.ReplaceAll(segment, "~0", "~")
		switch c := current.(type) {
		case map[string]any:
			v, ok := c[segment]
			if !ok {
				return nil, false
			}
			current = v
		case []any:
			idx, err := strconv.Atoi(segment)
			if err != nil || idx < 0 || idx >= len(c) {
				return nil, false
			}
			current = c[idx]
		default:
			return nil, false
		}
	}
	return current, true
}

// withoutRef returns a copy of node with the "$ref" key removed, or nil when
// node has no $ref key.
func withoutRef(node map[string]any) map[string]any {
	if _, ok := node["$ref"]; !ok {
		return nil
	}
	out := make(map[string]any, len(node)-1)
	for k, v := range node {
		if k != "$ref" {
			out[k] = v
		}
	}
	return out
}

// mergeMaps combines base with override; override wins on key collision
// (JS object spread semantics).
func mergeMaps(base, override map[string]any) map[string]any {
	out := make(map[string]any, capHint(len(base), len(override)))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}

// tryResolveRef resolves a node that is a BARE {"$ref": "..."} (no sibling
// keys) against the definition table. Returns (replacement, true) on
// success; the replacement is a deep clone, depth-capped so cyclic or deeply
// nested definitions cannot explode. Ported from JS tryResolveRef: only
// "#/definitions/<name>" and "#/$defs/<name>" pointers resolve, and only
// when the name exists in the table.
func tryResolveRef(node map[string]any, defs map[string]any, maxDepth int) (any, bool) {
	ref, _ := node["$ref"].(string)
	if ref == "" || len(node) != 1 || defs == nil {
		return nil, false
	}
	var name string
	switch {
	case strings.HasPrefix(ref, "#/definitions/"):
		name = ref[len("#/definitions/"):]
	case strings.HasPrefix(ref, "#/$defs/"):
		name = ref[len("#/$defs/"):]
	}
	if name == "" {
		return nil, false
	}
	resolved, ok := defs[name]
	if !ok {
		return nil, false
	}
	return cloneValue(resolved, maxDepth), true
}

// cloneValue deep-clones any JSON value, stopping below depth 0 (returning
// the value unchanged there) so a single $ref cannot balloon on deeply
// nested or cyclic definition subtrees. Values shared by the depth cap are
// only ever read downstream, never mutated.
func cloneValue(v any, maxDepth int) any {
	if maxDepth <= 0 {
		return v
	}
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = cloneValue(val, maxDepth-1)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = cloneValue(val, maxDepth-1)
		}
		return out
	default:
		return v
	}
}

// isNullSchema reports whether a sub-schema only admits null: type "null",
// const null, or enum [null]. Ported from JS isNullSchema.
func isNullSchema(schema map[string]any) bool {
	if t, _ := schema["type"].(string); t == "null" {
		return true
	}
	if c, ok := schema["const"]; ok && c == nil {
		return true
	}
	if enum, ok := schema["enum"].([]any); ok && len(enum) == 1 && enum[0] == nil {
		return true
	}
	return false
}

// simplifyNullableCombinator rewrites an anyOf/oneOf that contains null-only
// sub-schemas: null options are removed; with one option left the combinator
// is inlined (its option's keys override the schema's other keys); with zero
// left the key is dropped. Non-object entries (e.g. a bare null literal) are
// kept, matching the JS filter. Ported from JS simplifyNullableCombinator.
func simplifyNullableCombinator(schema map[string]any, key string) map[string]any {
	raw, ok := schema[key].([]any)
	if !ok {
		return schema
	}
	filtered := make([]any, 0, len(raw))
	for _, o := range raw {
		if m, isMap := o.(map[string]any); isMap && isNullSchema(m) {
			continue
		}
		filtered = append(filtered, o)
	}
	switch {
	case len(filtered) == 0:
		delete(schema, key)
	case len(filtered) == 1:
		if single, isMap := filtered[0].(map[string]any); isMap {
			merged := make(map[string]any, capHint(len(schema), len(single)))
			for k, v := range schema {
				if k != key {
					merged[k] = v
				}
			}
			for k, v := range single {
				merged[k] = v
			}
			return merged
		}
		schema[key] = filtered
	default:
		schema[key] = filtered
	}
	return schema
}

// normalizeTypeField reduces a string-array "type" to its first non-null
// entry, or drops the key when none remains. Non-string entries are dropped.
// Ported from JS normalizeTypeField.
func normalizeTypeField(schema map[string]any) {
	raw, ok := schema["type"].([]any)
	if !ok {
		return
	}
	var nonNull []string
	for _, t := range raw {
		if s, isStr := t.(string); isStr && strings.TrimSpace(s) != "" && s != "null" {
			nonNull = append(nonNull, s)
		}
	}
	if len(nonNull) == 0 {
		delete(schema, "type")
	} else {
		schema["type"] = nonNull[0]
	}
}

// normalizeEnumField removes null entries and duplicates from an "enum"
// array, dropping the key entirely when nothing remains. Dedupe keys combine
// Go type with JSON encoding, mirroring JS's `${typeof}:${JSON.stringify}`.
// Ported from JS normalizeEnumField.
func normalizeEnumField(schema map[string]any) {
	raw, ok := schema["enum"].([]any)
	if !ok {
		return
	}
	seen := make(map[string]bool, len(raw))
	filtered := make([]any, 0, len(raw))
	for _, entry := range raw {
		if entry == nil {
			continue
		}
		key := fmt.Sprintf("%T:%s", entry, jsonRepr(entry))
		if seen[key] {
			continue
		}
		seen[key] = true
		filtered = append(filtered, entry)
	}
	if len(filtered) == 0 {
		delete(schema, "enum")
	} else {
		schema["enum"] = filtered
	}
}

// normalizeConstField drops a const:null key. Ported from JS normalizeConstField.
func normalizeConstField(schema map[string]any) {
	if c, ok := schema["const"]; ok && c == nil {
		delete(schema, "const")
	}
}

// jsonRepr marshals a JSON-decoded value; it cannot fail on values that came
// from encoding/json, and the fallback keeps the dedupe key deterministic.
func jsonRepr(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
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

// reasoningInContentMode returns the think-tag label when reasoning folding is
// enabled, or "" when off. The env var REASONING_IN_CONTENT may be a boolean
// (true → "think") or an explicit tag word ("thinking" → "thinking").
func reasoningInContentMode() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("REASONING_IN_CONTENT")))
	switch v {
	case "", "0", "false", "off", "no", "disabled":
		return ""
	case "1", "true", "yes", "on":
		return reasoningInContentTag
	}
	return v
}

// foldReasoningIntoContent appends the <tag>reasoning</tag> text to the
// delta's content when folding is enabled. Reasoning precedes text (the
// model layer enqueues reasoning deltas before text deltas); non-string
// content is left untouched.
func foldReasoningIntoContent(delta map[string]any, reasoning string) {
	tag := reasoningInContentMode()
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

// ---------------------------------------------------------------------------
// SSE framing (ported from freebuff-api-kiprana sse.py encode_sse).
// ---------------------------------------------------------------------------

// EncodeSSE renders one SSE frame: "data: " + data (callers pass compact
// JSON from json.Marshal) + "\n\n".
func EncodeSSE(data []byte) []byte {
	out := append([]byte("data: "), data...)
	out = append(out, '\n', '\n')
	return out
}

// DONE is the "[DONE]" SSE frame that terminates a stream.
var DONE = []byte("data: [DONE]\n\n")

// ErrorChunk renders an SSE error frame with the OpenAI error shape:
// data: {"error":{"message":m,"type":"upstream_error","code":c}}\n\n
// The code field is omitted when empty. The caller appends DONE after it.
func ErrorChunk(message, code string) []byte {
	err := map[string]any{"message": message, "type": "upstream_error"}
	if code != "" {
		err["code"] = code
	}
	b, _ := json.Marshal(map[string]any{"error": err})
	return EncodeSSE(b)
}

// parseSSEData extracts the payload from one SSE data line. It accepts both
// "data: {...}" and plain "{...}"; blank lines, comment lines (": ..."), and
// other SSE fields (event:/id:/retry:) are skipped. Returns (payload, true)
// when a payload is present.
func parseSSEData(line []byte) ([]byte, bool) {
	s := bytes.TrimSpace(line)
	if len(s) == 0 || s[0] == ':' {
		return nil, false // blank or comment line
	}
	if bytes.HasPrefix(s, []byte("data:")) {
		s = bytes.TrimSpace(s[len("data:"):])
		if len(s) == 0 {
			return nil, false
		}
		return s, true
	}
	if s[0] == '{' {
		return s, true // plain JSON (chunks are objects)
	}
	return nil, false // other SSE fields (event:, id:, retry:)
}

// chunkMapPool reuses the top-level decode map across SanitizeChunk calls.
// The pool hands out exclusive ownership and maps are cleared on reuse, so
// concurrent streams are safe.
var chunkMapPool = sync.Pool{
	New: func() any { return make(map[string]any, 8) },
}

// cleanMapPool reuses the sanitized output map across SanitizeChunk calls.
var cleanMapPool = sync.Pool{
	New: func() any { return make(map[string]any, 5) },
}

// SanitizeChunk cleans ONE upstream SSE data line (with or without the
// "data: " prefix) for relay to the client:
//
//   - chunks with no choices AND no usage are dropped (returns drop=true)
//   - id/object/created/model are ensured (default id "chatcmpl-"+hex,
//     object "chat.completion.chunk", created now, model "")
//   - reasoning_content stays in delta as its own key and is never merged
//     into content (unless REASONING_IN_CONTENT is enabled, issue #44);
//     an explicit null content is removed
//   - system_fingerprint/logprobs/usage/finish_reason pass through
//
// Output is compact JSON. Malformed or non-JSON lines are dropped.
//
// Chunks that already satisfy every invariant are returned WITHOUT
// re-encoding: the returned slice then aliases line (a zero-allocation fast
// path), so callers must copy it if they retain it past the current
// processing step.
//
// Ported from freebuff-api-kiprana sanitize_stream_chunk.
func SanitizeChunk(line []byte) ([]byte, bool) {
	data, ok := parseSSEData(line)
	if !ok {
		return nil, true
	}
	chunk := chunkMapPool.Get().(map[string]any)
	clear(chunk)
	if err := json.Unmarshal(data, &chunk); err != nil {
		chunkMapPool.Put(chunk)
		return nil, true
	}
	// Fast path: the chunk is already in sanitized shape, so re-encoding it
	// would be a no-op. Emit the raw payload (a subslice of line) untouched.
	// The reasoning-in-content fold changes output, so it disables the path.
	if reasoningInContentMode() == "" && !needsSanitize(chunk) {
		chunkMapPool.Put(chunk)
		return data, false
	}
	clean := cleanMapPool.Get().(map[string]any)
	result := sanitizeChunk(chunk, clean)
	chunkMapPool.Put(chunk)
	if result == nil {
		clear(clean)
		cleanMapPool.Put(clean)
		return nil, true
	}
	out, err := json.Marshal(result)
	clear(result)
	cleanMapPool.Put(result)
	if err != nil {
		return nil, true
	}
	return out, false
}

// sanitizeChunk implements the per-chunk cleanup into the pooled clean map;
// returns nil to drop the chunk. clean is cleared by the caller on drop.
func sanitizeChunk(chunk map[string]any, clean map[string]any) map[string]any {
	if errVal, hasErr := chunk["error"]; hasErr && errVal != nil {
		clear(clean)
		if id, ok := chunk["id"].(string); ok && id != "" {
			clean["id"] = id
		}
		if obj, ok := chunk["object"].(string); ok && obj != "" {
			clean["object"] = obj
		}
		if created, ok := numInt64(chunk["created"]); ok && created > 0 {
			clean["created"] = created
		}
		if model, ok := chunk["model"].(string); ok && model != "" {
			clean["model"] = model
		}
		if errMap, ok := errVal.(map[string]any); ok {
			cleanErr := make(map[string]any, len(errMap))
			for k, v := range errMap {
				cleanErr[k] = v
			}
			if _, ok := cleanErr["message"]; !ok {
				cleanErr["message"] = "upstream error"
			}
			if _, ok := cleanErr["type"]; !ok {
				cleanErr["type"] = "upstream_error"
			}
			clean["error"] = cleanErr
		} else if errStr, ok := errVal.(string); ok {
			clean["error"] = map[string]any{
				"message": errStr,
				"type":    "upstream_error",
			}
		} else {
			clean["error"] = map[string]any{
				"message": fmt.Sprintf("%v", errVal),
				"type":    "upstream_error",
			}
		}
		return clean
	}

	clear(clean)
	if id, ok := chunk["id"].(string); ok && id != "" {
		clean["id"] = id
	} else {
		clean["id"] = "chatcmpl-" + randHex(16)
	}
	if obj, ok := chunk["object"].(string); ok && obj != "" {
		clean["object"] = obj
	} else {
		clean["object"] = "chat.completion.chunk"
	}
	if created, ok := numInt64(chunk["created"]); ok && created > 0 {
		clean["created"] = created
	} else {
		clean["created"] = time.Now().Unix()
	}
	if model, ok := chunk["model"].(string); ok {
		clean["model"] = model
	} else {
		clean["model"] = ""
	}
	clean["choices"] = []any{}
	if fp, ok := chunk["system_fingerprint"].(string); ok && fp != "" {
		clean["system_fingerprint"] = fp
	}
	if usage, ok := chunk["usage"]; ok && usage != nil {
		clean["usage"] = usage
	}
	for _, c := range choicesOf(chunk) {
		item := make(map[string]any, 4)
		index := 0
		if i, ok := numInt64(c["index"]); ok {
			index = int(i)
		}
		item["index"] = index
		delta := make(map[string]any)
		if d, ok := c["delta"].(map[string]any); ok {
			for k, v := range d {
				delta[k] = v
			}
		}
		var reasoningStr string
		if rc, ok := delta["reasoning_content"]; ok {
			delete(delta, "reasoning_content")
			if s, isStr := rc.(string); isStr {
				reasoningStr = s
			}
		}
		if r, ok := delta["reasoning"]; ok {
			delete(delta, "reasoning")
			if s, isStr := r.(string); isStr && reasoningStr == "" {
				reasoningStr = s
			}
		}
		if reasoningStr != "" {
			delta["reasoning_content"] = reasoningStr
			delta["reasoning"] = reasoningStr
			// Issue #44: fold reasoning into content as <think> text for
			// clients that don't render a reasoning channel. reasoning_details
			// is never folded.
			foldReasoningIntoContent(delta, reasoningStr)
		}
		if v, ok := delta["content"]; ok && v == nil {
			delete(delta, "content")
		}
		item["delta"] = delta
		fr, hasFR := c["finish_reason"]
		if !hasFR {
			fr = nil
		}
		item["finish_reason"] = fr
		if lp, ok := c["logprobs"]; ok && lp != nil {
			item["logprobs"] = lp
		}
		clean["choices"] = append(clean["choices"].([]any), item)
	}
	if len(clean["choices"].([]any)) == 0 && clean["usage"] == nil {
		return nil
	}
	return clean
}

// needsSanitize reports whether sanitizeChunk would change the decoded chunk
// in any way. When it returns false the chunk is already in output shape and
// can be relayed verbatim (the SanitizeChunk zero-allocation fast path).
func needsSanitize(chunk map[string]any) bool {
	if _, hasErr := chunk["error"]; hasErr {
		return true // error chunks are rebuilt entirely
	}
	id, ok := chunk["id"].(string)
	if !ok || id == "" {
		return true // default id injected
	}
	if obj, _ := chunk["object"].(string); obj != "chat.completion.chunk" {
		return true
	}
	cv, ok := chunk["created"]
	if !ok || !isJSONInteger(cv) {
		return true
	}
	if c, _ := numInt64(cv); c <= 0 {
		return true // default created injected
	}
	if _, ok := chunk["model"].(string); !ok {
		return true
	}
	choices, ok := chunk["choices"].([]any)
	if !ok {
		return true
	}
	usage, hasUsage := chunk["usage"]
	if hasUsage && usage == nil {
		return true // null usage is dropped
	}
	if len(choices) == 0 && (!hasUsage || usage == nil) {
		return true // the chunk would be dropped outright
	}
	if fp, hasFP := chunk["system_fingerprint"]; hasFP {
		if s, ok := fp.(string); !ok || s == "" {
			return true
		}
	}
	for k := range chunk {
		switch k {
		case "id", "object", "created", "model", "choices", "system_fingerprint", "usage":
		default:
			return true // unknown top-level keys are dropped
		}
	}
	for _, raw := range choices {
		c, ok := raw.(map[string]any)
		if !ok {
			return true // non-object choices are dropped
		}
		idx, ok := c["index"]
		if !ok || !isJSONInteger(idx) {
			return true // default index 0 injected / fraction truncated
		}
		delta, ok := c["delta"].(map[string]any)
		if !ok {
			return true // empty delta injected
		}
		if content, has := delta["content"]; has && content == nil {
			return true // null content removed
		}
		hasRC := false
		if rc, has := delta["reasoning_content"]; has {
			if _, isStr := rc.(string); !isStr {
				return true // non-string reasoning_content dropped
			}
			hasRC = true
		}
		hasR := false
		if r, has := delta["reasoning"]; has {
			if _, isStr := r.(string); !isStr {
				return true // non-string reasoning dropped
			}
			hasR = true
		}
		if hasRC != hasR {
			return true // normalize to both reasoning_content and reasoning
		}
		if _, ok := c["finish_reason"]; !ok {
			return true // explicit null finish_reason injected
		}
		if lp, has := c["logprobs"]; has && lp == nil {
			return true // null logprobs dropped
		}
		for k := range c {
			switch k {
			case "index", "delta", "finish_reason", "logprobs":
			default:
				return true // unknown choice keys are dropped
			}
		}
	}
	return false
}

// isJSONInteger reports whether v is a JSON number whose sanitize conversion
// round-trips: integral AND representable as int64 (encoding/json decodes
// numbers as float64; sanitize truncates fractions via numInt64). A value
// like 1e20 is integral but saturates int64, so it must still take the
// sanitize path.
func isJSONInteger(v any) bool {
	switch n := v.(type) {
	case float64:
		return n == math.Trunc(n) && n == float64(int64(n))
	case int, int64:
		return true
	}
	return false
}

// numInt64 extracts an integer from a JSON-decoded number.
func numInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	}
	return 0, false
}

// choicesOf returns the object entries of a chunk's "choices" array.
func choicesOf(chunk map[string]any) []map[string]any {
	raw, _ := chunk["choices"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, c := range raw {
		if m, ok := c.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// toolCallsOf returns the object entries of a delta's "tool_calls" array.
func toolCallsOf(delta map[string]any) []map[string]any {
	raw, _ := delta["tool_calls"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, c := range raw {
		if m, ok := c.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Non-streaming accumulator (ported from freebuff-api-kiprana
// CompletionAccumulator).
// ---------------------------------------------------------------------------

// toolCall is one assembled tool call: id/type/function.name come from the
// first fragment, function.arguments is concatenated across fragments.
type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Accumulator assembles a non-streaming chat.completion response from
// upstream SSE lines. It is not safe for concurrent use (one stream, one
// accumulator).
type Accumulator struct {
	id                string
	created           int64
	model             string
	contentParts      []string
	reasoningParts    []string
	finishReason      string
	usage             any
	systemFingerprint string
	toolCalls         map[int]*toolCall
}

// NewAccumulator returns an accumulator with a fresh chatcmpl- id and
// created timestamp; model/id/created are refined by the first chunks seen.
func NewAccumulator() *Accumulator {
	return &Accumulator{
		id:        "chatcmpl-" + randHex(16),
		created:   time.Now().Unix(),
		toolCalls: make(map[int]*toolCall),
	}
}

// Add parses one SSE data line (with or without "data: " prefix; [DONE] and
// non-data lines are ignored) and accumulates its content into the response.
func (a *Accumulator) Add(line []byte) error {
	data, ok := parseSSEData(line)
	if !ok {
		return nil
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		return nil
	}
	var chunk map[string]any
	if err := json.Unmarshal(data, &chunk); err != nil {
		return fmt.Errorf("convert: invalid chunk JSON: %w", err)
	}
	if errVal, ok := chunk["error"]; ok && errVal != nil {
		if errMap, ok := errVal.(map[string]any); ok {
			if msg, ok := errMap["message"].(string); ok && msg != "" {
				return fmt.Errorf("upstream error: %s", msg)
			}
		} else if errStr, ok := errVal.(string); ok && errStr != "" {
			return fmt.Errorf("upstream error: %s", errStr)
		}
		return fmt.Errorf("upstream error: %v", errVal)
	}
	a.accumulate(chunk)
	return nil
}

func (a *Accumulator) accumulate(chunk map[string]any) {
	if id, ok := chunk["id"].(string); ok && id != "" {
		a.id = id
	}
	if created, ok := numInt64(chunk["created"]); ok && created > 0 {
		a.created = created
	}
	if model, ok := chunk["model"].(string); ok && model != "" {
		a.model = model
	}
	if usage, ok := chunk["usage"]; ok && usage != nil {
		a.usage = usage
	}
	if fp, ok := chunk["system_fingerprint"].(string); ok && fp != "" {
		a.systemFingerprint = fp
	}
	for _, c := range choicesOf(chunk) {
		delta, _ := c["delta"].(map[string]any)
		if content, ok := delta["content"].(string); ok {
			a.contentParts = append(a.contentParts, content)
		}
		if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
			a.reasoningParts = append(a.reasoningParts, rc)
		} else if r, ok := delta["reasoning"].(string); ok && r != "" {
			a.reasoningParts = append(a.reasoningParts, r)
		}
		for _, tc := range toolCallsOf(delta) {
			a.addToolCall(tc)
		}
		if fr, ok := c["finish_reason"].(string); ok && fr != "" {
			a.finishReason = fr
		}
	}
}

// addToolCall stitches one tool-call fragment by index: id/type/name are
// taken from the first fragment that provides them, arguments are appended
// across fragments.
func (a *Accumulator) addToolCall(tc map[string]any) {
	index := 0
	if i, ok := numInt64(tc["index"]); ok {
		index = int(i)
	}
	cur, ok := a.toolCalls[index]
	if !ok {
		cur = &toolCall{ID: "call_" + randHex(12), Type: "function"}
		a.toolCalls[index] = cur
	}
	if id, ok := tc["id"].(string); ok && id != "" {
		cur.ID = id
	}
	if typ, ok := tc["type"].(string); ok && typ != "" {
		cur.Type = typ
	}
	if fn, ok := tc["function"].(map[string]any); ok {
		if name, ok := fn["name"].(string); ok && name != "" {
			cur.Function.Name = name
		}
		if args, ok := fn["arguments"].(string); ok && args != "" {
			cur.Function.Arguments += args
		}
	}
}

// xmlToolCallRegex matches XML-based tool calls such as:
//
//	<tool_call>
//	<function=bash>
//	<parameter=command>...</parameter>
//	</function>
//	</tool_call>
//
// or <tool_call>{"name":"...","arguments":{...}}</tool_call>
var (
	xmlToolCallBlockRe = regexp.MustCompile(`(?s)<tool_call>(.*?)</tool_call>|<function_call>(.*?)</function_call>|<\|?tool[_\-]?call[_\-]?start\|?>(.*?)<\|?tool[_\-]?call[_\-]?end\|?>`)
	fencedToolCallRe   = regexp.MustCompile("(?s)```(?:json|tool_?call)?\\s*\\n?(\\{\\s*\"(?:name|function)\"\\s*:\\s*.*?\\})\\s*\\n?```")
	xmlFunctionHeadRe  = regexp.MustCompile(`(?i)<function[=\s]+["']?([^>"\s]+)["']?>`)
	xmlParamRe         = regexp.MustCompile(`(?s)<parameter[=\s]+["']?([^>"\s]+)["']?>(.*?)</parameter>|<param[=\s]+["']?([^>"\s]+)["']?>(.*?)</param>`)
	danglingToolTagsRe = regexp.MustCompile(`(?i)</?(?:tool_call|function_call|function|parameter|param|\|?tool[_\-]?call[_\-]?(?:start|end)\|?)(?:[=\s][^>]*)?>`)
)

// extractXMLToolCalls parses text-based tool calls (Hermes/Qwen/MiMo XML format)
// that were emitted into content instead of native OpenAI tool_calls fields.
// It returns the cleaned content string and the extracted tool calls.
func extractXMLToolCalls(content string) (string, []*toolCall) {
	matches := xmlToolCallBlockRe.FindAllStringSubmatchIndex(content, -1)
	fencedMatches := fencedToolCallRe.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 && len(fencedMatches) == 0 {
		return content, nil
	}

	var calls []*toolCall

	// 1. Check XML block matches (<tool_call>...</tool_call>)
	for _, loc := range matches {
		block := content[loc[0]:loc[1]]
		inner := xmlToolCallBlockRe.FindStringSubmatch(block)
		if len(inner) < 2 {
			continue
		}
		raw := inner[1]
		if raw == "" && len(inner) > 2 {
			raw = inner[2]
		}
		if raw == "" && len(inner) > 3 {
			raw = inner[3]
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}

		if tc := parseToolCallRaw(raw); tc != nil {
			calls = append(calls, tc)
		}
	}

	// 2. Check fenced code blocks (```json {"name": "..."} ```)
	if len(calls) == 0 {
		for _, loc := range fencedMatches {
			block := content[loc[0]:loc[1]]
			inner := fencedToolCallRe.FindStringSubmatch(block)
			if len(inner) >= 2 {
				raw := strings.TrimSpace(inner[1])
				if tc := parseToolCallRaw(raw); tc != nil {
					calls = append(calls, tc)
				}
			}
		}
	}

	if len(calls) == 0 {
		return content, nil
	}

	// Clean the tool_call blocks from content
	cleaned := xmlToolCallBlockRe.ReplaceAllString(content, "")
	cleaned = fencedToolCallRe.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned), calls
}

// parseToolCallRaw parses a single raw tool call string in either JSON or XML format.
func parseToolCallRaw(raw string) *toolCall {
	// Try direct JSON: {"name":"...", "arguments":{...}} or {"function":{...}}
	if strings.HasPrefix(raw, "{") && strings.HasSuffix(raw, "}") {
		var jObj map[string]any
		if err := json.Unmarshal([]byte(raw), &jObj); err == nil {
			name, _ := jObj["name"].(string)
			if name == "" {
				if fnObj, ok := jObj["function"].(map[string]any); ok {
					name, _ = fnObj["name"].(string)
				} else {
					name, _ = jObj["function"].(string)
				}
			}
			if name != "" {
				var argsStr string
				if argsObj, ok := jObj["arguments"].(map[string]any); ok {
					if b, err := json.Marshal(argsObj); err == nil {
						argsStr = string(b)
					}
				} else if aStr, ok := jObj["arguments"].(string); ok {
					argsStr = aStr
				} else {
					argsStr = "{}"
				}
				return &toolCall{
					ID:   "call_" + randHex(12),
					Type: "function",
					Function: toolFunction{
						Name:      name,
						Arguments: argsStr,
					},
				}
			}
		}
	}

	// Try XML format: <function=NAME><parameter=KEY>VAL</parameter></function>
	fnMatch := xmlFunctionHeadRe.FindStringSubmatch(raw)
	if len(fnMatch) >= 2 {
		fnName := strings.TrimSpace(fnMatch[1])
		paramMatches := xmlParamRe.FindAllStringSubmatch(raw, -1)
		argsMap := make(map[string]any)
		for _, pm := range paramMatches {
			pName := pm[1]
			pVal := pm[2]
			if pName == "" && len(pm) > 4 {
				pName = pm[3]
				pVal = pm[4]
			}
			pName = strings.TrimSpace(pName)
			pVal = strings.TrimSpace(pVal)
			argsMap[pName] = pVal
		}
		argsBytes, _ := json.Marshal(argsMap)
		return &toolCall{
			ID:   "call_" + randHex(12),
			Type: "function",
			Function: toolFunction{
				Name:      fnName,
				Arguments: string(argsBytes),
			},
		}
	}

	return nil
}

// Finish returns the assembled chat.completion response as compact JSON:
// content and reasoning_content are concatenated across chunks, tool_calls
// are stitched by index and sorted, finish_reason is the last non-empty one
// seen ("stop" when none), and usage is the last one seen (zeroed when none).
func (a *Accumulator) Finish() []byte {
	content := strings.Join(a.contentParts, "")
	// Issue #44: fold reasoning into message content for clients that don't
	// render the reasoning channel (same env toggle as the streaming path).
	if tag := reasoningInContentMode(); tag != "" {
		if rc := strings.Join(a.reasoningParts, ""); rc != "" {
			content = "<" + tag + ">" + rc + "</" + tag + ">" + content
		}
	}
	// If native toolCalls are empty, try extracting any inline XML tool calls
	// that were emitted into content (e.g. from Hermes/Qwen/MiMo models).
	if len(a.toolCalls) == 0 {
		var extracted []*toolCall
		content, extracted = extractXMLToolCalls(content)
		if len(extracted) == 0 && len(a.reasoningParts) > 0 {
			// Fallback: model might have emitted tool_call inside reasoning_content (smallcode finding / Tau2 fix)
			reasoningFull := strings.Join(a.reasoningParts, "")
			var cleanedReasoning string
			cleanedReasoning, extracted = extractXMLToolCalls(reasoningFull)
			if len(extracted) > 0 {
				a.reasoningParts = []string{cleanedReasoning}
			}
		}
		for idx, tc := range extracted {
			a.toolCalls[idx] = tc
		}
	}
	// Scrub dangling tool XML tags from content
	content = danglingToolTagsRe.ReplaceAllString(content, "")
	content = strings.TrimSpace(content)

	var msgContent any = content
	if len(a.toolCalls) > 0 && content == "" {
		msgContent = nil
	}
	msg := map[string]any{
		"role":    "assistant",
		"content": msgContent,
	}
	if len(a.toolCalls) > 0 {
		keys := make([]int, 0, len(a.toolCalls))
		for k := range a.toolCalls {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		calls := make([]any, 0, len(keys))
		for _, k := range keys {
			calls = append(calls, a.toolCalls[k])
		}
		msg["tool_calls"] = calls
	}
	if rc := strings.Join(a.reasoningParts, ""); rc != "" {
		msg["reasoning_content"] = rc
	}
	usage := a.usage
	if usage == nil {
		usage = map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	}
	finish := a.finishReason
	if finish == "" {
		if len(a.toolCalls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	resp := map[string]any{
		"id":      a.id,
		"object":  "chat.completion",
		"created": a.created,
		"model":   a.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
		"usage": usage,
	}
	if a.systemFingerprint != "" {
		resp["system_fingerprint"] = a.systemFingerprint
	}
	// Values came from encoding/json, so marshaling cannot fail.
	b, _ := json.Marshal(resp)
	return b
}
