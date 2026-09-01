package server

// OpenAI chat streaming translation: relayStream forwards sanitized upstream
// SSE lines with keepalive, XML tool-call extraction, end_turn stripping,
// served-model rewriting and reasoning capture; relayJSON drains the
// non-streaming path into one chat.completion object. The chunk-rewrite
// helpers shared with the Responses relay live in openai_stream_rewrite.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"freebuff-proxy/backend/internal/convert"
	"freebuff-proxy/backend/internal/phasetiming"
)

// relayStream forwards sanitized upstream SSE lines to the client with
// per-chunk flushing, a ": connecting" grace-flush comment, a keepalive
// comment every keepaliveInterval of relay silence, a [DONE] terminator,
// and an error chunk (then DONE) when the upstream stream dies while the
// client context is still live.
func (s *Server) relayStream(ctx context.Context, w http.ResponseWriter, r io.Reader, stats *relayStats, chatStart time.Time) {
	flusher, keepalive, lines, lastWrite, ok := newStreamRelay(ctx, w, r)
	if !ok {
		s.logger.Warn("response writer does not support flushing")
		return
	}
	defer keepalive.Stop()

	// Issue #164: the stream's model identity is the proxy's served model
	// (lease.Model, fallbacks included) — synthetic XML-flush chunks and the
	// reasoning cache key on it. The upstream chunk echo only fills the
	// field when no lease drove the relay (direct unit-test calls).
	//
	// All per-chunk rewrite state lives in the chunk pipeline (issue #249):
	// one unmarshal, a chain of map-level rewrites, one marshal, with the
	// byte-preserving fast path for untouched chunks.
	rw := newChunkRewriter(stats)

	// emitXMLFlush releases anything the XML extractor still holds at
	// stream end (Flush) as one synthetic chunk through the normal frame
	// path: completed calls inside a never-closed block still become native
	// fragments, and dangling tags are scrubbed from the released text.
	emitXMLFlush := func() {
		ft, frags := drainXMLToolCalls(rw.xmlExtractor, &rw.xmlCallIndex)
		if ft == "" && len(frags) == 0 {
			return
		}
		delta := make(map[string]any, 2)
		if !rw.roleSent {
			// A flush chunk may be the stream's first frame (upstream sent
			// no relayable chunk before EOF): it must open with the role.
			delta["role"] = "assistant"
			rw.roleSent = true
		}
		if ft != "" {
			delta["content"] = ft
			rw.contentParts = append(rw.contentParts, ft)
		}
		if len(frags) > 0 {
			delta["tool_calls"] = frags
			rw.xmlCallsSeen = true
		}
		streamID := rw.xmlStreamID
		if streamID == "" {
			streamID = "chatcmpl-flush"
		}
		chunk := map[string]any{
			"id":      streamID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   rw.streamModel,
			"choices": []any{map[string]any{"index": 0, "delta": delta}},
		}
		// Restore client tool names (#140): the synthetic flush chunk carries
		// official signature names extracted from XML; the client dispatches
		// on its own declared names, so the flush path must run the same
		// restore the normal chunk path does.
		if stats.toolMap.Len() > 0 {
			stats.toolMap.FromUpstreamChunk(chunk)
		}
		writeFrame := func(chunk map[string]any) bool {
			b, err := json.Marshal(chunk)
			if err != nil {
				return false
			}
			frame := convert.EncodeSSE(b)
			if _, err := w.Write(frame); err != nil {
				s.logger.Debug("stream write failed", "err", err)
				return false
			}
			stats.chunks++
			stats.bytes += len(frame)
			*lastWrite = time.Now()
			flusher.Flush()
			return true
		}
		if !writeFrame(chunk) {
			return
		}
		// Tool calls flushed from an unclosed XML block arrive AFTER the
		// terminal chunk, so the in-loop stop->tool_calls flip (keyed on
		// xmlCallsSeen at terminal time) never fired for them: the client
		// already saw finish_reason "stop" and would end a turn that
		// carried fully-delivered extracted calls. Append a synthetic
		// empty-delta terminal chunk reading "tool_calls" so the stream
		// ends consistent with the relayed calls — only when the extracted
		// calls are the sole delivery (no native tool-call fragments,
		// mirroring relayJSON's guard) and the upstream reason was "stop"
		// ("length" stays honest).
		if rw.xmlCallsSeen && rw.lastFinishReason == "stop" && !rw.seenRealToolCalls {
			writeFrame(map[string]any{
				"id":      streamID,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   rw.streamModel,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
			})
		}
	}

	first := true
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			maybeKeepalive(w, flusher, lastWrite, ": keepalive\n\n")
		case lc := <-lines:
			if lc.err != nil {
				if ctx.Err() == nil {
					emitXMLFlush()
					s.logger.Warn("upstream stream error", "err", lc.err)
					_, _ = w.Write(convert.ErrorChunk("upstream stream interrupted: "+lc.err.Error(), "upstream_stream_error"))
					_, _ = w.Write(convert.DONE)
					flusher.Flush()
				}
				s.ingestStreamReasoning(rw.streamModel, rw.reasoningParts, rw.contentParts, rw.toolIDs, rw.streamToolCalls)
				return
			}
			if lc.done {
				// Clean end of stream (EOF is not a scanner error).
				emitXMLFlush()
				_, _ = w.Write(convert.DONE)
				flusher.Flush()
				s.ingestStreamReasoning(rw.streamModel, rw.reasoningParts, rw.contentParts, rw.toolIDs, rw.streamToolCalls)
				return
			}
			clean, drop := convert.SanitizeChunkOpts(lc.line, s.convertOptions())
			if drop {
				// Non-chunk lines (upstream comments, junk frames) are never
				// relayed and must NOT advance the keepalive timer: the
				// client sees only real frames, so a steady dribble of
				// upstream comments would starve it of liveness signals
				// indefinitely (#161).
				continue
			}
			// The whole per-chunk rewrite gauntlet (end_turn track/strip,
			// continuation drop, XML feed, tool-name restore, both finish
			// flips, model/id/reasoning capture, usage capture, model
			// stamp, role ensure) runs as one parse-mutate-marshal
			// pipeline (issue #249).
			clean = rw.rewrite(clean)
			if first {
				first = false
				phasetiming.FromContext(ctx).Since(phasetiming.UpstreamTTFBMS, chatStart)
			}
			frame := convert.EncodeSSE(clean)
			if _, err := w.Write(frame); err != nil {
				s.logger.Debug("stream write failed", "err", err)
				return
			}
			stats.chunks++
			stats.bytes += len(frame)
			*lastWrite = time.Now()
			flusher.Flush()
		}
	}
}

// streamToolAcc accumulates one upstream tool call's identity across its
// streamed fragments: the first fragment carries the id and function name,
// later fragments append argument bytes.
type streamToolAcc struct {
	id   string
	name string
	args strings.Builder
}

func (s *Server) ingestStreamReasoning(model string, reasoningParts, contentParts, toolIDs []string, streamToolCalls map[int]*streamToolAcc) {
	if s.reasoningCache == nil || len(reasoningParts) == 0 {
		return
	}
	rc := strings.Join(reasoningParts, "")
	if rc == "" {
		return
	}
	cStr := strings.Join(contentParts, "")
	s.reasoningCache.PutCanonical(toolIDs, cStr, canonicalStreamToolKey(streamToolCalls), rc, "", model)
}

// canonicalStreamToolKey reduces the per-index accumulated tool calls to
// the canonical identity key, in upstream index order — the order the
// relayed fragments reconstruct the client's tool_calls array from. Only
// calls whose id actually arrived are included, matching the toolIDs list
// that is put alongside the key.
func canonicalStreamToolKey(calls map[int]*streamToolAcc) string {
	return buildCanonicalToolKey(calls, func(acc *streamToolAcc) (string, string, string) {
		if acc == nil {
			return "", "", ""
		}
		return acc.id, acc.name, acc.args.String()
	})
}

// relayJSON drains the upstream SSE stream through the accumulator and
// writes one chat.completion JSON response. On any decode or stream error
// nothing is written and a 502 is returned (the client asked for a single
// response; a partial one would be worse than none).
func (s *Server) relayJSON(ctx context.Context, w http.ResponseWriter, r io.Reader, stats *relayStats, chatStart time.Time) {
	acc := convert.NewAccumulatorOpts(s.convertOptions())
	// Override the upstream model in the response with the served model
	// (which is the requested model, not necessarily what upstream returned).
	if stats.servedModel != "" {
		acc.SetRequestedModel(stats.servedModel)
	}
	// Track whether the upstream stream carried any native delta.tool_calls
	// fragment: when it did, the accumulator skips XML extraction, so the
	// delivered tool_calls are native and an upstream "stop" is deliberate.
	// The XML-extraction finish flip below must then stay off — a native
	// tool-call response keeps its upstream finish_reason (pinned by
	// TestChatNonStream). When NO native fragment was seen, delivered calls
	// can only be extracted ones, which pair with an upstream "stop".
	sawNativeToolCalls := false
	probe := func(line []byte) {
		if bytes.Contains(line, []byte(`"tool_calls":[{`)) {
			sawNativeToolCalls = true
		}
	}
	if err := drainUpstream(ctx, r, acc, stats, chatStart, probe); err != nil {
		if errors.Is(err, errDrainUpstreamDecode) {
			s.writeJSONError(w, http.StatusBadGateway,
				"failed to decode upstream stream: "+errDrainCause(err), "upstream_error", "upstream_unavailable", 0)
		} else {
			s.writeJSONError(w, http.StatusBadGateway,
				"upstream stream error: "+errDrainCause(err), "upstream_error", "upstream_unavailable", 0)
		}
		return
	}
	out := acc.Finish()
	// Strip Codebuff end_turn pseudo-tool-calls from the non-streaming
	// response, then align finish_reason with the tool_calls actually
	// delivered: a response whose calls were all end_turn (zero calls after
	// stripping) must read "stop", while XML-extracted calls carried in
	// message.tool_calls with an upstream "stop" (accumulator Finish) must
	// read "tool_calls" — parity with the streaming path's two flips.
	var comp map[string]any
	if bytes.Contains(out, []byte(`"tool_calls"`)) || bytes.Contains(out, []byte(`"finish_reason":"tool_calls"`)) {
		if json.Unmarshal(out, &comp) == nil {
			changed := false
			if bytes.Contains(out, []byte(`"end_turn"`)) {
				convert.StripEndTurnToolCalls(comp)
				changed = true
			}
			// Restore client tool names (#140) before the finish_reason
			// alignment below reads the delivered tool_calls.
			if stats.toolMap.FromUpstreamChunk(comp) {
				changed = true
			}
			if rawChoices, ok := comp["choices"].([]any); ok {
				for _, raw := range rawChoices {
					choice, _ := raw.(map[string]any)
					if choice == nil {
						continue
					}
					fr, _ := choice["finish_reason"].(string)
					msg, _ := choice["message"].(map[string]any)
					if msg == nil {
						continue
					}
					tcs, _ := msg["tool_calls"].([]any)
					switch {
					case len(tcs) == 0 && fr == "tool_calls":
						choice["finish_reason"] = "stop"
						changed = true
					case len(tcs) > 0 && fr == "stop" && !sawNativeToolCalls:
						choice["finish_reason"] = "tool_calls"
						changed = true
					}
				}
			}
			if changed {
				if b, err := json.Marshal(comp); err == nil {
					out = b
				}
			}
		}
	}
	// Issue #164: stamp the served model onto the non-streaming response
	// body so clients see the model that actually served the request
	// (fallbacks included). Runs before the reasoning-cache block below so
	// the cache also keys on the served model, matching the streaming path.
	if stats.servedModel != "" {
		var comp map[string]any
		if json.Unmarshal(out, &comp) == nil {
			if cur, _ := comp["model"].(string); cur != stats.servedModel {
				comp["model"] = stats.servedModel
				if b, err := json.Marshal(comp); err == nil {
					out = b
				}
			}
		}
	}
	stats.bytes = len(out)
	// Capture the accumulated usage total for the spend ledger (#122);
	// only adopt when the response actually carries a usage block.
	var usageObj struct {
		Usage any `json:"usage"`
	}
	if json.Unmarshal(out, &usageObj) == nil && usageObj.Usage != nil {
		stats.usageTokens = usageTotalTokens(usageObj.Usage)
	}
	if s.reasoningCache != nil {
		var comp map[string]any
		if json.Unmarshal(out, &comp) == nil {
			model, _ := comp["model"].(string)
			if choices, ok := comp["choices"].([]any); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]any); ok {
					if msg, ok := choice["message"].(map[string]any); ok {
						rc, _ := msg["reasoning_content"].(string)
						if rc == "" {
							rc, _ = msg["reasoning"].(string)
						}
						if rc != "" {
							var toolIDs []string
							var tcJSON string
							if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 {
								for _, raw := range tcs {
									if tc, ok := raw.(map[string]any); ok {
										if id, ok := tc["id"].(string); ok && id != "" {
											toolIDs = append(toolIDs, id)
										}
									}
								}
								if b, err := json.Marshal(tcs); err == nil {
									tcJSON = string(b)
								}
							}
							cStr, _ := msg["content"].(string)
							s.reasoningCache.Put(toolIDs, cStr, tcJSON, rc, "", model)
						}
					}
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// errDrainCause strips the drainUpstream sentinel wrapper so relays build
// their protocol envelope from the underlying accumulator/scanner error.
func errDrainCause(err error) string {
	return strings.TrimPrefix(err.Error(), errDrainUpstreamDecode.Error()+": ")
}
