package server

// Conformance test for the streaming keepalive contract
// (devdocs/compatibility-roadmap.md Phase 3): while the upstream is silent
// the relay must keep writing liveness frames to the client so a harness
// per-chunk idle timeout never aborts the stream on a keepalive-only gap.
// Grounding: kilocode's per-chunk SSE idle timeout (chunkTimeout, when
// configured) aborts on sparse keep-alives (reference/agents/kilocode/
// WIRE-NOTES.md aisdk.ts:54-82), yet its SSE decoder tolerates comment
// keep-alive lines (reference/agents/kilocode/WIRE-NOTES.md shared.ts:261-267);
// codex aborts a stream idle past 5 minutes (reference/agents/codex/
// WIRE-NOTES.md). The test drives the live HTTP chat surface end-to-end with
// the mock upstream, with no network.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/testutil"
)

// keepalivePingsOf counts the "\n\n"-terminated ": keepalive" comment frames
// in a client SSE body. On the OpenAI-compatible wire the relay's liveness
// signal is an SSE comment frame (the Anthropic wire uses an "event: ping");
// comment frames are exactly the keep-alive token harnesses like kilocode
// tolerate as liveness (reference/agents/kilocode/WIRE-NOTES.md shared.ts).
func keepalivePingsOf(t *testing.T, body string) int {
	t.Helper()
	return strings.Count(body, ": keepalive\n\n")
}

// TestConformanceKeepaliveDuringUpstreamSilence pins the Phase 3 contract: a
// proxy keepalive must keep the client-visible stream alive while the
// upstream produces no bytes, and the stream must still complete with the
// terminal finish_reason once data resumes — never aborting mid-silence.
func TestConformanceKeepaliveDuringUpstreamSilence(t *testing.T) {
	old := keepaliveInterval
	// Shrink the cadence so the test is fast (pattern from
	// relay_internal_test.go TestRelayStreamKeepalive). The 15s production
	// cadence is a var (engine_sse.go) precisely so tests can do this.
	keepaliveInterval = 50 * time.Millisecond
	t.Cleanup(func() { keepaliveInterval = old })

	mock := testutil.NewMock()
	defer mock.Close()

	// The mock streams ONE text frame, then stays SILENT for ~10x
	// keepaliveInterval (no bytes, no pings), then emits the terminal
	// finish_reason and [DONE]. A harness chunkTimeout that aborts on sparse
	// keep-alives (kilocode chunkTimeout) would kill the turn during that gap
	// unless the relay's own keepalive frames reach the client first.
	const sleepFactor = 10
	silence := time.Duration(sleepFactor) * keepaliveInterval

	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		// One text frame, flushed so the relay sees it immediately (this also
		// proves the silent window starts AFTER it).
		_, _ = io.WriteString(w, testutilSSE(fmt.Sprintf(
			`{"id":"chatcmpl-k","object":"chat.completion.chunk","created":1,"model":%q,"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
			testModelA)))
		if flusher != nil {
			flusher.Flush()
		}

		// Silent stretch: upstream produces nothing; only the relay's
		// keepalive frames may reach the client.
		select {
		case <-time.After(silence):
		case <-r.Context().Done():
			return
		}

		// Terminal frame once data resumes.
		_, _ = io.WriteString(w, testutilSSE(fmt.Sprintf(
			`{"id":"chatcmpl-k","object":"chat.completion.chunk","created":1,"model":%q,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			testModelA)))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	srv := newServer(t, mock, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	reqBody := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"ping"}],"stream":true}`, testModelA)
	resp, data := doTestJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(reqBody), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, string(data))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	body := string(data)

	// (a) client-visible keepalive frames while the upstream is silent: the
	// surrogate need is >=2 liveness signals per client timeout window.
	pings := keepalivePingsOf(t, body)
	if pings < 2 {
		t.Errorf("keepalive frames = %d, want >= 2 (>=2 pings per timeout window): %q", pings, truncateStr(body, 400))
	}
	// At least one keepalive must land strictly inside the silent window:
	// after the text frame, before the terminal finish_reason.
	contentIdx := strings.Index(body, `"content":"hi"`)
	lastPing := strings.LastIndex(body, ": keepalive\n\n")
	finishIdx := strings.Index(body, `"finish_reason":"stop"`)
	if contentIdx < 0 || lastPing < 0 || finishIdx < 0 {
		t.Fatalf("frame markers missing: content=%d lastPing=%d finish=%d", contentIdx, lastPing, finishIdx)
	}
	if contentIdx >= lastPing || lastPing >= finishIdx {
		t.Errorf("keepalive must be emitted during the silent window (content before keepalive before finish): content=%d lastPing=%d finish=%d", contentIdx, lastPing, finishIdx)
	}

	// (b) the stream completes with the terminal finish_reason after the
	// upstream resumes, and (c) does not hang/abort: doTestJSON bounds the
	// request with a 10s client timeout (a hang would fail it here), and the
	// relay signals a clean EOF with [DONE].
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("stream must end with [DONE]: %q", truncateStr(body, 400))
	}
}
