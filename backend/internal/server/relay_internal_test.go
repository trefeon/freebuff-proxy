package server

// Package-internal relay tests (#83/#59): the SSE grace-flush comment,
// periodic keepalive frames during an upstream pause, and the mid-stream
// death error frame. These drive relayStream directly with scripted readers
// so no network/timing flakiness is involved.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testRelayServer() *Server {
	return &Server{logger: slog.Default()}
}

// TestRelayStreamGraceFlush pins the ": connecting" comment: it must be
// written and flushed before any relayed chunk, so a client-side timeout
// can never fire during a long upstream admission pause.
func TestRelayStreamGraceFlush(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()
	r := strings.NewReader(
		testutilSSE(`{"id":"chatcmpl-g","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`),
	)
	s.relayStream(context.Background(), rec, r, &relayStats{}, time.Now())
	body := rec.Body.String()
	if !strings.Contains(body, ": connecting\n\n") {
		t.Errorf("body missing ': connecting' grace comment: %q", truncateStr(body, 300))
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Error("body missing [DONE] terminator")
	}
}

// TestRelayStreamKeepalive verifies a comment frame is emitted every
// keepaliveInterval of relay silence (upstream reasoning pause), and that
// normal chunks suppress it.
func TestRelayStreamKeepalive(t *testing.T) {
	old := keepaliveInterval
	keepaliveInterval = 20 * time.Millisecond
	t.Cleanup(func() { keepaliveInterval = old })

	s := testRelayServer()
	rec := httptest.NewRecorder()
	pr, pw := io.Pipe()
	defer func() { _ = pr.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.relayStream(context.Background(), rec, pr, &relayStats{}, time.Now())
	}()

	// One chunk, then silence.
	_, _ = pw.Write([]byte(testutilSSE(`{"id":"chatcmpl-k","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`)))
	time.Sleep(150 * time.Millisecond)
	_ = pw.Close()
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, ": keepalive\n\n") {
		t.Errorf("body missing keepalive comment frames: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, "hi") {
		t.Error("body missing the relayed chunk")
	}
}

// TestRelayStreamKeepaliveJunkUpstream pins the #161 root cause: upstream
// comment/junk lines (gateway keepalives, blank frames) that arrive more
// often than keepaliveInterval must NOT reset the client keepalive timer.
// Pre-fix, each dropped line advanced "relayed", so the
// time.Since(relayed) >= keepaliveInterval check never passed and NO
// liveness frame was ever written — clients with stall detectors (e.g.
// Next.js openai-compatible-chat) saw multi-minute silence. The client
// must still receive keepalive frames within ~2x keepaliveInterval, the
// junk must never be relayed, and [DONE] must stay intact.
func TestRelayStreamKeepaliveJunkUpstream(t *testing.T) {
	old := keepaliveInterval
	keepaliveInterval = 20 * time.Millisecond
	t.Cleanup(func() { keepaliveInterval = old })

	s := testRelayServer()
	rec := httptest.NewRecorder()
	pr, pw := io.Pipe()
	defer func() { _ = pr.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.relayStream(context.Background(), rec, pr, &relayStats{}, time.Now())
	}()

	// One real chunk, then silence on data.
	_, _ = pw.Write([]byte(testutilSSE(`{"id":"chatcmpl-j","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`)))

	// Dribble upstream comment frames every 10ms (half keepaliveInterval).
	// Pre-fix this starved the client of liveness frames indefinitely.
	stopJunk := make(chan struct{})
	go func() {
		junk := time.NewTicker(10 * time.Millisecond)
		defer junk.Stop()
		for {
			select {
			case <-stopJunk:
				return
			case <-junk.C:
				_, _ = io.WriteString(pw, ": ping\n\n")
			}
		}
	}()

	time.Sleep(150 * time.Millisecond)
	close(stopJunk)
	_ = pw.Close()
	<-done

	body := rec.Body.String()
	if got := strings.Count(body, ": keepalive\n\n"); got < 2 {
		t.Errorf("keepalive frames = %d, want >= 2 (junk dribble must not starve the client): %q", got, truncateStr(body, 400))
	}
	if strings.Contains(body, ": ping\n") {
		t.Errorf("upstream comment junk leaked to the client: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, "hi") {
		t.Error("body missing the relayed chunk")
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Error("body missing [DONE] terminator")
	}
}

// errAfterLineReader yields one SSE line then a transport-style error.
type errAfterLineReader struct {
	once bool
}

func (r *errAfterLineReader) Read(p []byte) (int, error) {
	if !r.once {
		r.once = true
		line := testutilSSE(`{"id":"chatcmpl-x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`)
		return copy(p, line), nil
	}
	return 0, errors.New("connection reset by peer")
}

// TestRelayStreamMidStreamError pins the late-failure contract: when the
// upstream dies mid-stream, the relay mirrors an in-band error frame
// (type upstream_error + the provider/transport message) and then [DONE] —
// never a bare hang and never an HTTP status (headers already flushed).
func TestRelayStreamMidStreamError(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()
	s.relayStream(context.Background(), rec, &errAfterLineReader{}, &relayStats{}, time.Now())
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"upstream_error"`) {
		t.Errorf("body missing upstream_error frame: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, "upstream stream interrupted: connection reset by peer") {
		t.Errorf("body missing provider message: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, "partial") {
		t.Error("body missing the relayed partial chunk before the error")
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Error("body missing [DONE] after the error frame")
	}
}

// TestStreamErrorAttrs pins the mid-stream death correlation fields: req_id
// rides along when the relay context carries one (chatCore stamps it) and is
// omitted for lease-less direct relay calls, so the WARN is never noise.
func TestStreamErrorAttrs(t *testing.T) {
	stats := &relayStats{chunks: 3, bytes: 128}
	toMap := func(attrs []any) map[string]any {
		m := make(map[string]any, len(attrs)/2)
		for i := 0; i+1 < len(attrs); i += 2 {
			m[fmt.Sprint(attrs[i])] = attrs[i+1]
		}
		return m
	}
	withID := toMap(streamErrorAttrs(
		context.WithValue(context.Background(), reqIDKey{}, "req-test-1"),
		time.Now(), stats, errors.New("boom")))
	if withID["req_id"] != "req-test-1" {
		t.Errorf("req_id = %v, want req-test-1", withID["req_id"])
	}
	if withID["chunks"] != 3 || withID["bytes"] != 128 {
		t.Errorf("progress = (%v, %v), want (3, 128)", withID["chunks"], withID["bytes"])
	}
	if _, ok := withID["elapsed_ms"]; !ok {
		t.Error("missing elapsed_ms")
	}
	withoutID := toMap(streamErrorAttrs(context.Background(), time.Now(), stats, errors.New("boom")))
	if v, ok := withoutID["req_id"]; ok {
		t.Errorf("req_id = %v, want absent without a request context", v)
	}
}

// TestRelayStreamErrorLogsReqID pins the anomaly-analysis contract: a
// mid-stream death after WriteHeader(200) logs req_id + relay progress, so a
// dashboard "ERROR 200" group resolves back to its request.
func TestRelayStreamErrorLogsReqID(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))}
	rec := httptest.NewRecorder()
	ctx := context.WithValue(context.Background(), reqIDKey{}, "req-test-2")
	s.relayStream(ctx, rec, &errAfterLineReader{}, &relayStats{}, time.Now())
	out := buf.String()
	for _, want := range []string{"upstream stream error", "req_id=req-test-2", "chunks=", "bytes=", "elapsed_ms="} {
		if !strings.Contains(out, want) {
			t.Errorf("stream error log missing %q: %q", want, out)
		}
	}
}

// testutilSSE renders one data frame (local copy of the SSEEvent helper so
// the internal test package stays dependency-light).
func testutilSSE(data string) string { return "data: " + data + "\n\n" }

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
