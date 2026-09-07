package convert

// FromUpstreamChunk restores client tool names in one upstream SSE chunk or
// completion object, IN PLACE: walks choices[].delta.tool_calls[] and
// choices[].message.tool_calls[], rewriting function.name. Returns the chunk
// unchanged (same semantics as callers' other in-place mutators). raw is the
// already-marshaled JSON; callers re-marshal only when this returns true.
func (m ToolMapper) FromUpstreamChunk(chunk map[string]any) bool {
	changed := false
	restore := func(fn map[string]any) {
		if fn == nil {
			return
		}
		name, _ := fn["name"].(string)
		if name == "" {
			return
		}
		if orig, ok := m.upstreamToClient[name]; ok {
			fn["name"] = orig
			changed = true
		}
	}
	choices, ok := chunk["choices"].([]any)
	if !ok {
		return false
	}
	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if delta, ok := choice["delta"].(map[string]any); ok {
			if tcs, ok := delta["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					tcMap, _ := tc.(map[string]any)
					if tcMap == nil {
						continue
					}
					fn, _ := tcMap["function"].(map[string]any)
					restore(fn)
				}
			}
		}
		if msg, ok := choice["message"].(map[string]any); ok {
			if tcs, ok := msg["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					tcMap, _ := tc.(map[string]any)
					if tcMap == nil {
						continue
					}
					fn, _ := tcMap["function"].(map[string]any)
					restore(fn)
				}
			}
		}
	}
	return changed
}

// RestoreName maps one upstream tool name back to the client's original
// (identity when unmapped). Used by the Anthropic stream state machine,
// which tracks names outside the chunk-JSON shapes above.
func (m ToolMapper) RestoreName(name string) string {
	if orig, ok := m.upstreamToClient[name]; ok {
		return orig
	}
	return name
}

// Len reports how many names the mapper restores. Zero = identity mapper;
// relays use it to skip their restore pass entirely.
func (m ToolMapper) Len() int { return len(m.upstreamToClient) }
