package server

// Anthropic-half of the streaming XML tool-call extraction (issue #151):
// feed/flushAnthropicXMLToolCalls drive convert.XMLToolCallExtractor from
// relayAnthropicStream chunks and emit synthetic Anthropic tool_use
// content-block events. The OpenAI half lives in streamxml_openai.go
// (streamChatContentToToolCalls). See convert.XMLToolCallExtractor for the
// incremental parser contract (Feed/Flush per stream).

import (
	"time"

	"freebuff-proxy/backend/internal/convert"
)

// feedAnthropicXMLToolCalls feeds one upstream content delta through the
// stream's XML tool-call extractor and rewrites the delta in place: withheld
// block text is removed from content (the key is dropped when empty) and any
// completed calls are appended as native tool-call fragments with per-stream
// sequential indexes so they cannot collide with upstream indexes. Existing
// native tool_calls fragments are left untouched. The delta is only rewritten
// when the extractor actually withheld or consumed text (shared core, issue
// #245).
func feedAnthropicXMLToolCalls(xmlExtractor *convert.XMLToolCallExtractor, chunk map[string]any, xmlCallIndex *int) {
	feedXMLToolCalls(xmlExtractor, chunk, xmlCallIndex)
}

// flushAnthropicXMLToolCalls releases any still-open XML candidate block at
// stream end through the same accumulation path (trailing text and/or native
// tool-call fragments continuing the stream's sequential indexes) so text
// and tool_use blocks emit normally before finalize. No-op when nothing was
// buffered.
func (s *Server) flushAnthropicXMLToolCalls(send func(map[string]any), st *anthropicStreamState, xmlExtractor *convert.XMLToolCallExtractor, xmlCallIndex *int) {
	ft, frags := drainXMLToolCalls(xmlExtractor, xmlCallIndex)
	if ft == "" && len(frags) == 0 {
		return
	}
	delta := make(map[string]any)
	if ft != "" {
		delta["content"] = ft
	}
	if len(frags) > 0 {
		delta["tool_calls"] = frags
	}
	s.accumulateAnthropicChunk(send, st, map[string]any{
		"id":      "chatcmpl-flush",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   st.model,
		"choices": []any{map[string]any{"delta": delta}},
	})
}
