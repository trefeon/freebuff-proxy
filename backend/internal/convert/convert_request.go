package convert

import (
	"encoding/json"
	"fmt"
)

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
// Whitelist policy (feature-translation parity): the whitelist is the
// documented drop contract — chat-completions params NOT listed here are
// intentionally not forwarded (e.g. prediction, verbosity, n, audio,
// web_search_options, moderation) because the upstream chat endpoint has no
// equivalent. Params whose absence would silently change observable client
// behavior are rejected with a clear 400 by the handlers BEFORE
// normalization (n != 1, audio, web_search_options, moderation on
// /v1/chat/completions; previous_response_id, conversation, background,
// built-in tools/tool_choice on /v1/responses — see openai.go and
// responses.go), so a request that arrives at this function carries either
// mapped-supported params or params the client can afford to lose.
//
// opts are resolved from DefaultOptions by this wrapper; callers holding a
// config.Config should use NormalizeRequestOpts to supply cfg-derived
// options (issue #277).
func NormalizeRequest(body []byte, modelOverride string) ([]byte, error) {
	return NormalizeRequestOpts(body, modelOverride, DefaultOptions())
}

// NormalizeRequestOpts is NormalizeRequest with an explicit Options: the
// compression, cache_control and reasoning-in-content modes are taken from
// opts instead of the process environment (issue #277). Callers that have a
// resolved config.Config construct Options once and pass them here.
func NormalizeRequestOpts(body []byte, modelOverride string, opts Options) ([]byte, error) {
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
	if out["tools"] == nil {
		if fns, ok := payload["functions"].([]any); ok && len(fns) > 0 {
			tools := make([]any, 0, len(fns))
			for _, fnRaw := range fns {
				if fn, ok := fnRaw.(map[string]any); ok {
					tools = append(tools, map[string]any{
						"type":     "function",
						"function": fn,
					})
				}
			}
			if len(tools) > 0 {
				out["tools"] = tools
			}
		}
	}
	if out["tool_choice"] == nil {
		if fc, ok := payload["function_call"]; ok && fc != nil {
			switch typed := fc.(type) {
			case string:
				out["tool_choice"] = typed
			case map[string]any:
				if name, ok := typed["name"].(string); ok && name != "" {
					out["tool_choice"] = map[string]any{
						"type":     "function",
						"function": map[string]any{"name": name},
					}
				}
			}
		}
	}
	if modelOverride != "" {
		out["model"] = modelOverride
	}
	normalizeReasoning(payload, out)
	model, _ := out["model"].(string)
	normalizeMessages(out, model, opts)
	// Optional prompt compression (#58): drops middle non-tool turns and caps
	// long content, gated by opts.CompressPrompt, never touching tool
	// calls/results or the current message.
	if opts.CompressPrompt {
		if msgs, ok := out["messages"].([]any); ok {
			out["messages"], _ = compressMessages(msgs, opts)
		}
	}
	// DeepSeek prompt-cache hints (#84): cache_control ephemeral on the stable
	// context prefix (messages at indices 2-3), gated by
	// opts.CacheControlInjection (default on).
	if model, _ := out["model"].(string); opts.CacheControlInjection && isDeepSeekModel(model) {
		if msgs, ok := out["messages"].([]any); ok {
			InjectCacheControl(msgs)
		}
	}
	normalizeToolSchemas(out, opts)
	return json.Marshal(out)
}

// NormalizeRequestMapped is NormalizeRequest plus the issue #140
// tool-name tolerance layer: client tool names with official equivalents are
// renamed to the signature names on the wire (so upstream's foreign_toolset
// check and the third_party_client trust cap never see them) and the
// returned ToolMapper restores the client's names on every response path.
// The parameter schemas are forwarded untouched — the model fills arguments
// per the schema it was shown, so only names need restoring.
func NormalizeRequestMapped(body []byte, modelOverride string) ([]byte, ToolMapper, error) {
	return NormalizeRequestMappedOpts(body, modelOverride, DefaultOptions())
}

// NormalizeRequestMappedOpts is NormalizeRequestMapped with an explicit
// Options (issue #277). See NormalizeRequestOpts.
func NormalizeRequestMappedOpts(body []byte, modelOverride string, opts Options) ([]byte, ToolMapper, error) {
	mapper := NewToolMapper(body)
	out, err := NormalizeRequestOpts(body, modelOverride, opts)
	if err != nil {
		return nil, ToolMapper{}, err
	}
	// Apply renames on top of the normalized body.
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		return out, ToolMapper{}, nil //nolint:NormalizeRequest already validated; unreachable in practice
	}
	mapper.ToUpstream(payload)
	mapper.RenameRequestToolChoice(payload)
	renamed, merr := json.Marshal(payload)
	if merr != nil {
		return out, ToolMapper{}, nil // fall back to unrenamed rather than fail the request
	}
	return renamed, mapper, nil
}

// normalizeMessages rewrites message role "developer" to "system" in place,
// extracts leaked think tags from assistant messages, restores missing reasoning_content
// via globalReasoningLookup, and ensures content: null on assistant tool calls.
func normalizeMessages(payload map[string]any, model string, opts Options) {
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

				// 2. If still missing, check the per-call reasoning lookup
				// (threaded through Options, issue #251)
				if rc == "" && opts.ReasoningLookup != nil {
					fn := opts.ReasoningLookup
					// Look up by each tool call id, bound to this message's
					// content and the canonical identity of its tool_calls so
					// a per-conversation sequential tool_call_id cannot
					// restore another conversation's reasoning — including
					// tool-only turns whose content is empty or null (the
					// cache canonicalizes the raw array internally, so both
					// wire shapes bind identically).
					cStr0 := ""
					if s, ok := cVal.(string); ok {
						cStr0 = s
					}
					rawTC, _ := json.Marshal(msg["tool_calls"])
					for _, item := range tcSlice {
						if tcMap, ok := item.(map[string]any); ok {
							if id, _ := tcMap["id"].(string); id != "" {
								if r, _, ok := fn(id, cStr0, string(rawTC)); ok && r != "" {
									rc = r
									msg["reasoning_content"] = r
									break
								}
							}
						}
					}
					// If still not found, look up by content + toolCalls JSON.
					if rc == "" {
						cStr := ""
						if s, ok := msg["content"].(string); ok {
							cStr = s
						}
						tcJSON := string(rawTC)
						if r, _, ok := fn("", cStr, tcJSON); ok && r != "" {
							rc = r
							msg["reasoning_content"] = r
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
