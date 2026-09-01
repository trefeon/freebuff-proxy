package server

import (
	"bytes"
	"encoding/json"
)

// rewriteChatChunkModel returns clean unchanged unless the chunk's model
// field differs from the served model, in which case it re-marshals the
// chunk with the served model. A chunk carrying no model field is left
// untouched (the upstream OpenAI-compatible surface always includes it;
// injecting into arbitrary frames would re-marshal every chunk). served ""

// trackToolCallIndexes records per-stream tool-call state on every chunk
// BEFORE StripEndTurnToolCalls deletes the end_turn entries (tracking after
// the strip would never see them). end_turn indexes feed the downstream
// continuation-fragment drop — later argument fragments carry the same index
// but an empty name, so they are only recognizable by index — and any real
// named call flips seenRealToolCalls so the terminal finish_reason rewrite
// never downgrades a genuine tool-call turn to "stop". The cheap substring

// dropEndTurnContinuations removes delta.tool_calls fragments whose index
// belongs to an already-stripped end_turn call. Upstream models emit a
// call's arguments in later fragments that carry only the index and an
// empty name — never the "end_turn" string — so the drop must run on every
// tool-bearing chunk, not just chunks containing the name (a nameless
// fragment would otherwise leak as a native tool_calls entry). The chunk is
// re-marshaled only when a fragment was actually dropped.
func dropEndTurnContinuations(clean []byte, endTurnCallIndexes map[int]bool) []byte {
	if len(endTurnCallIndexes) == 0 || !bytes.Contains(clean, []byte(`"tool_calls"`)) {
		return clean
	}
	var chunk map[string]any
	if json.Unmarshal(clean, &chunk) != nil {
		return clean
	}
	dropped, _ := dropEndTurnContinuationsInChunk(chunk, endTurnCallIndexes)
	if !dropped {
		return clean
	}
	if b, err := json.Marshal(chunk); err == nil {
		return b
	}
	return clean
}

// dropEndTurnContinuationsInChunk is the map-level core shared by the
// bytes-based streaming drop above and the Responses relay's chunk loop:
// it removes delta.tool_calls fragments whose index belongs to an
// already-stripped end_turn call from an unmarshalled chat chunk map.
// dropped reports whether any fragment was removed; emptied reports
// whether a choice's tool_calls list was emptied by the drop (its delta
// key is deleted).
func dropEndTurnContinuationsInChunk(chunk map[string]any, endTurnCallIndexes map[int]bool) (dropped, emptied bool) {
	if len(endTurnCallIndexes) == 0 {
		return false, false
	}
	choices, _ := chunk["choices"].([]any)
	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		if choice == nil {
			continue
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		tcs, _ := delta["tool_calls"].([]any)
		if len(tcs) == 0 {
			continue
		}
		filtered := make([]any, 0, len(tcs))
		for _, tc := range tcs {
			tcMap, _ := tc.(map[string]any)
			if tcMap == nil {
				filtered = append(filtered, tc)
				continue
			}
			idx := 0
			if i, ok := tcMap["index"].(float64); ok {
				idx = int(i)
			}
			if endTurnCallIndexes[idx] {
				dropped = true
				continue // drop continuation fragment
			}
			filtered = append(filtered, tc)
		}
		if dropped {
			if len(filtered) == 0 {
				delete(delta, "tool_calls")
				emptied = true
			} else {
				delta["tool_calls"] = filtered
			}
		}
	}
	return dropped, emptied
}

// ensureChatChunkRole injects "role":"assistant" into the FIRST relayed
// chunk's delta (choice 0) when the upstream omitted it — the OpenAI spec
// carries the role in the first chunk of a chat completion stream and some
// clients assemble message.role from it. Idempotent per stream via
// roleSent; chunks that already carry role (or any later chunk) pass

// bumpXMLCallIndex raises the per-stream synthetic tool-call index floor
// past the largest native tool-call index in tcs (the chunk's existing
// delta.tool_calls entries) so extracted XML fragments can never collide
// with upstream indexes. The floor persists across chunks via the pointer,
// so a floor established in one chunk applies to all later synthetics.
func bumpXMLCallIndex(tcs []any, xmlCallIndex *int) {
	for _, raw := range tcs {
		tc, _ := raw.(map[string]any)
		if tc == nil {
			continue
		}
		if i, ok := tc["index"].(float64); ok && int(i) >= *xmlCallIndex {
			*xmlCallIndex = int(i) + 1
		}
	}
}
