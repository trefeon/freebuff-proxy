package server

// Review-fix regression tests for the Anthropic streaming relay
// (devdocs/review-2026-08-31.md, P2-7 + served-model P3):
//   1. P2-7 — multi-tool turns must keep the strictly sequential block
//      lifecycle: a fragment for a different upstream index closes the
//      currently open tool_use block before the next one opens (previously
//      both stops were deferred to finalize, leaving two open blocks).
//   2. P2-7 corollary — a straggler fragment for an already-closed upstream
//      index still reopens its block at a fresh index with the accumulated
//      prefix replayed (issue #171 machinery, pinned).
//   3. Served model — the upstream chunk echo must not overwrite the pinned
//      served model mid-stream; lease-less relays still trust the echo.
//
// The tests drive relayAnthropicStream directly with scripted SSE readers —
// no network/timing flakiness (same style as relay_protocol_fixes_test.go).

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/reasoningcache"
)

// TestAnthropicReviewFixSequentialToolBlocks pins P2-7: two tool calls
// (upstream index 0 then index 1) must emit strictly sequential
// start/delta/stop per block — the second content_block_start may only fire
// after the first block's content_block_stop (previously the second block
// opened while the first was still open, with both stops deferred to
// finalize).
func TestAnthropicReviewFixSequentialToolBlocks(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()

	ss := strings.Join([]string{
		testutilSSE(`{"id":"cmpl-rfa","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]},"finish_reason":null}]}`),
		testutilSSE(`{"id":"cmpl-rfa","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"Read","arguments":"{\"path\":\"main.go\"}"}}]},"finish_reason":null}]}`),
		testutilSSE(`{"id":"cmpl-rfa","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
	}, "")

	s.relayAnthropicStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now(), "m", 0)
	events := collectSSEFrames(t, rec.Body.String())

	open := map[int]bool{}
	var toolStarts []int
	var toolStartData []map[string]any
	var jsonDeltas []struct {
		idx     int
		partial string
	}
	stopCount := 0
	stopReason := ""
	for _, ev := range events {
		idx := -1
		if f, ok := ev.data["index"].(float64); ok {
			idx = int(f)
		}
		switch ev.data["type"] {
		case "content_block_start":
			if len(open) > 0 {
				t.Errorf("content_block_start %d while blocks %v still open (two open blocks violate the sequential lifecycle)", idx, open)
			}
			if cb, ok := ev.data["content_block"].(map[string]any); ok && cb["type"] == "tool_use" {
				if len(toolStarts) == 1 && stopCount == 0 {
					t.Errorf("second tool_use content_block_start %d fired before the first block's content_block_stop", idx)
				}
				toolStarts = append(toolStarts, idx)
				toolStartData = append(toolStartData, cb)
			}
			open[idx] = true
		case "content_block_delta":
			d, _ := ev.data["delta"].(map[string]any)
			if d != nil && d["type"] == "input_json_delta" {
				if !open[idx] {
					t.Fatalf("input_json_delta against closed/unstarted block index %d", idx)
				}
				pj, _ := d["partial_json"].(string)
				jsonDeltas = append(jsonDeltas, struct {
					idx     int
					partial string
				}{idx, pj})
			}
		case "content_block_stop":
			if !open[idx] {
				t.Fatalf("content_block_stop for unopen block index %d", idx)
			}
			stopCount++
			delete(open, idx)
		case "message_delta":
			if delta, ok := ev.data["delta"].(map[string]any); ok {
				stopReason, _ = delta["stop_reason"].(string)
			}
		}
	}
	if len(open) > 0 {
		t.Errorf("blocks %v left open at message end", open)
	}
	if len(toolStarts) != 2 {
		t.Fatalf("tool_use content_block_start count = %d, want 2; starts %v", len(toolStarts), toolStarts)
	}
	if stopCount != 2 {
		t.Errorf("content_block_stop count = %d, want 2", stopCount)
	}
	// Each tool_use block carries its own upstream identity and arguments.
	if name, _ := toolStartData[0]["name"].(string); name != "Bash" {
		t.Errorf("first block name = %q, want Bash", name)
	}
	if id, _ := toolStartData[0]["id"].(string); id != "call_a" {
		t.Errorf("first block id = %q, want call_a", id)
	}
	if name, _ := toolStartData[1]["name"].(string); name != "Read" {
		t.Errorf("second block name = %q, want Read", name)
	}
	if id, _ := toolStartData[1]["id"].(string); id != "call_b" {
		t.Errorf("second block id = %q, want call_b", id)
	}
	if len(jsonDeltas) != 2 {
		t.Fatalf("input_json_delta count = %d, want 2; %+v", len(jsonDeltas), jsonDeltas)
	}
	if jsonDeltas[0].idx != toolStarts[0] || jsonDeltas[0].partial != `{"cmd":"ls"}` {
		t.Errorf("first block args delta = %+v, want index %d with its own args", jsonDeltas[0], toolStarts[0])
	}
	if jsonDeltas[1].idx != toolStarts[1] || jsonDeltas[1].partial != `{"path":"main.go"}` {
		t.Errorf("second block args delta = %+v, want index %d with its own args", jsonDeltas[1], toolStarts[1])
	}
	if stopReason != "tool_use" {
		t.Errorf("message_delta stop_reason = %q, want tool_use", stopReason)
	}
}

// TestAnthropicReviewFixStragglerReopensWithPrefixReplay pins the issue #171
// reopen machinery under the P2-7 fix: a straggler fragment for an
// already-closed upstream index (closed by the fragment of the NEXT index)
// must reopen the block at a FRESH index, replay the accumulated argument
// prefix, and take the straggler fragment — never a delta against the closed
// index, and never two open blocks.
func TestAnthropicReviewFixStragglerReopensWithPrefixReplay(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()

	ss := strings.Join([]string{
		// Tool A (upstream index 0) opens and streams a partial argument.
		testutilSSE(`{"id":"cmpl-rfb","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\""}}]},"finish_reason":null}]}`),
		// Tool B (upstream index 1) closes A and opens its own block.
		testutilSSE(`{"id":"cmpl-rfb","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"Read","arguments":"{\"path\":\"x\"}"}}]},"finish_reason":null}]}`),
		// Straggler for the closed upstream index 0.
		testutilSSE(`{"id":"cmpl-rfb","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ls\"}"}}]},"finish_reason":null}]}`),
		testutilSSE(`{"id":"cmpl-rfb","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
	}, "")

	s.relayAnthropicStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now(), "m", 0)
	events := collectSSEFrames(t, rec.Body.String())

	open := map[int]bool{}
	var toolStarts []int
	var toolStartData []map[string]any
	var jsonDeltas []struct {
		idx     int
		partial string
	}
	stopReason := ""
	for _, ev := range events {
		idx := -1
		if f, ok := ev.data["index"].(float64); ok {
			idx = int(f)
		}
		switch ev.data["type"] {
		case "content_block_start":
			if len(open) > 0 {
				t.Errorf("content_block_start %d while blocks %v still open (two open blocks violate the sequential lifecycle)", idx, open)
			}
			if cb, ok := ev.data["content_block"].(map[string]any); ok && cb["type"] == "tool_use" {
				toolStarts = append(toolStarts, idx)
				toolStartData = append(toolStartData, cb)
			}
			open[idx] = true
		case "content_block_delta":
			d, _ := ev.data["delta"].(map[string]any)
			if d != nil && d["type"] == "input_json_delta" {
				if !open[idx] {
					t.Fatalf("input_json_delta against closed/unstarted block index %d", idx)
				}
				pj, _ := d["partial_json"].(string)
				jsonDeltas = append(jsonDeltas, struct {
					idx     int
					partial string
				}{idx, pj})
			}
		case "content_block_stop":
			if !open[idx] {
				t.Fatalf("content_block_stop for unopen block index %d", idx)
			}
			delete(open, idx)
		case "message_delta":
			if delta, ok := ev.data["delta"].(map[string]any); ok {
				stopReason, _ = delta["stop_reason"].(string)
			}
		}
	}
	if len(open) > 0 {
		t.Errorf("blocks %v left open at message end", open)
	}
	if len(toolStarts) != 3 {
		t.Fatalf("tool_use content_block_start count = %d, want 3 (A, B, reopened A); starts %v", len(toolStarts), toolStarts)
	}
	first, reopened := toolStarts[0], toolStarts[2]
	if reopened <= first {
		t.Errorf("reopened tool block index %d must be a fresh index above the first %d", reopened, first)
	}
	if len(jsonDeltas) != 4 {
		t.Fatalf("input_json_delta count = %d, want 4; %+v", len(jsonDeltas), jsonDeltas)
	}
	if jsonDeltas[0].idx != first || jsonDeltas[0].partial != `{"cmd":"` {
		t.Errorf("first block args delta = %+v, want index %d with the opening args", jsonDeltas[0], first)
	}
	if jsonDeltas[1].idx != toolStarts[1] || jsonDeltas[1].partial != `{"path":"x"}` {
		t.Errorf("second block args delta = %+v, want index %d with its own args", jsonDeltas[1], toolStarts[1])
	}
	// The reopened block replays the accumulated prefix, then the straggler.
	if jsonDeltas[2].idx != reopened || jsonDeltas[2].partial != `{"cmd":"` {
		t.Errorf("reopened block missing accumulated-prefix replay: %+v, want index %d with the full prefix", jsonDeltas[2], reopened)
	}
	if jsonDeltas[3].idx != reopened || jsonDeltas[3].partial != `ls"}` {
		t.Errorf("straggler args delta = %+v, want index %d with the closing args", jsonDeltas[3], reopened)
	}
	// The reopened block carries the accumulated id/name.
	if name, _ := toolStartData[2]["name"].(string); name != "Bash" {
		t.Errorf("reopened block name = %q, want Bash", name)
	}
	if id, _ := toolStartData[2]["id"].(string); id != "call_a" {
		t.Errorf("reopened block id = %q, want call_a", id)
	}
	if stopReason != "tool_use" {
		t.Errorf("message_delta stop_reason = %q, want tool_use", stopReason)
	}
}

// TestAnthropicReviewFixModelEchoGuard pins the served-model fix: with a
// lease, the upstream chunk echo must not overwrite the pinned served model
// (observable via the reasoning-cache entry finalize records); a lease-less
// relay still trusts the echo as the only identity available.
func TestAnthropicReviewFixModelEchoGuard(t *testing.T) {
	cases := []struct {
		name        string
		servedModel string // relayStats.servedModel; "" = lease-less relay
		requested   string
		wantCached  string // model recorded on the finalize reasoning-cache entry
	}{
		{
			name:        "lease pins served model against upstream echo",
			servedModel: "gpt-5.6-luna",
			requested:   "gpt-5.6-luna",
			wantCached:  "gpt-5.6-luna",
		},
		{
			name:        "lease-less relay trusts the upstream echo",
			servedModel: "",
			requested:   "m",
			wantCached:  "upstream/echo",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testRelayServer()
			s.reasoningCache = reasoningcache.New(16, time.Hour)
			rec := httptest.NewRecorder()

			// Thinking + a tool call so finalize records a cache entry whose
			// Model exposes the stream state's final model value; the echo
			// arrives mid-stream, before finalize.
			ss := strings.Join([]string{
				testutilSSE(`{"id":"cmpl-rfc","model":"upstream/echo","choices":[{"index":0,"delta":{"reasoning_content":"hmm"},"finish_reason":null}]}`),
				testutilSSE(`{"id":"cmpl-rfc","model":"upstream/echo","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_m","type":"function","function":{"name":"Bash","arguments":"{}"}}]},"finish_reason":null}]}`),
				testutilSSE(`{"id":"cmpl-rfc","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
			}, "")

			s.relayAnthropicStream(context.Background(), rec, strings.NewReader(ss), &relayStats{servedModel: tc.servedModel}, time.Now(), tc.requested, 0)

			// message_start names the pinned served model when a lease
			// exists, else the requested model.
			wantStart := tc.requested
			if tc.servedModel != "" {
				wantStart = tc.servedModel
			}
			for _, ev := range collectSSEFrames(t, rec.Body.String()) {
				if ev.data["type"] != "message_start" {
					continue
				}
				msg, _ := ev.data["message"].(map[string]any)
				if got, _ := msg["model"].(string); got != wantStart {
					t.Errorf("message_start model = %q, want %q", got, wantStart)
				}
			}

			entry, ok := s.reasoningCache.GetEntryByToolID("call_m")
			if !ok {
				t.Fatalf("reasoning cache missing entry for call_m")
			}
			if entry.Model != tc.wantCached {
				t.Errorf("cached entry model = %q, want %q (upstream echo must not override the pinned served model)", entry.Model, tc.wantCached)
			}
		})
	}
}
