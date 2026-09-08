package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// statusWriter captures the response status for access logging. It forwards
// Flusher/Hijacker/Pusher so streaming and similar protocols keep working
// through the access-log wrapper.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijack not supported")
}

func (w *statusWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

// silentAccessPath reports whether path never emits an access line: the
// liveness/metrics probes (health is proven by the container status and by
// the absence of error lines, not by a per-minute GET) and the dashboard's
// own GET polls under /admin/api/* (the open SPA polls tokens/quota/logs
// every few seconds; those lines drown the auth and session events the log
// viewer is actually read for). Mutations (POSTs, including login/logout),
// chat traffic, and 404s still log through the normal/quiet paths below.
func silentAccessPath(method, path string) bool {
	return path == "/healthz" || path == "/metrics" ||
		(method == http.MethodGet && strings.HasPrefix(path, "/admin/api/"))
}

// quietAccessPath reports whether path is a fire-and-forget endpoint whose
// access lines are rate-limited (T17): CORS OPTIONS preflights. Unknown
// paths (which 404) are gated the same way by the access wrapper — an
// arbitrary-path client must not flood the log.
func quietAccessPath(method, path string) bool {
	return method == http.MethodOptions
}

// accessGates are the per-Server access-log and rate-limit dedupe gates
// (issue #252). They were process-globals shared by every Server instance;
// now each Server owns one so instances never interfere, and the mutable
// knobs no longer serialize the test package.
type accessGates struct {
	// window is the quiet-endpoint gate window: at most one access line per
	// path per window (T17). A field so tests can shrink it per-Server.
	window time.Duration
	// logGate is the quiet-path access gate: map[path]lastLog plus a mutex
	// (T17). The map is CAPPED: OPTIONS preflights and unknown paths arrive
	// for arbitrary distinct paths, so unbounded membership would leak
	// memory; accessLogDue evicts the oldest entry past the cap.
	logGate struct {
		mu       sync.Mutex
		lastSeen map[string]time.Time
	}
	// quietBudget caps quiet-class access lines per window: per-path gating
	// alone lets a flood of DISTINCT paths emit one line each, so a global
	// budget bounds the total quiet lines per window.
	quietBudget struct {
		mu          sync.Mutex
		windowStart time.Time
		lines       int
	}
}

// maxAccessGateEntries bounds accessGates.logGate.lastSeen. The gate's quiet
// candidates are paths, and a client can mint unlimited distinct paths
// (OPTIONS preflights, unknown-path 404s), so the map must never grow past
// this cap: when full the OLDEST entry is evicted before a new one is
// recorded.
const maxAccessGateEntries = 512

// maxQuietAccessLines is the budget of quiet-class access lines per window.
// Distinct quiet paths beyond it are silent; the window rolls the budget
// over.
const maxQuietAccessLines = 60

// newAccessGates returns a gate set with the production defaults.
func newAccessGates() *accessGates {
	g := &accessGates{}
	g.window = 60 * time.Second
	g.logGate.lastSeen = make(map[string]time.Time)
	return g
}

// accessLogDue reports whether an access line may fire for path now,
// recording the current attempt. The first request for a path and any
// request at least window after the last line fire; requests inside the
// window are suppressed. The map stays bounded: past maxAccessGateEntries
// the oldest entry is evicted.
func (g *accessGates) accessLogDue(path string, now time.Time) bool {
	g.logGate.mu.Lock()
	defer g.logGate.mu.Unlock()
	if g.logGate.lastSeen == nil {
		g.logGate.lastSeen = make(map[string]time.Time)
	}
	last, ok := g.logGate.lastSeen[path]
	if !ok || now.Sub(last) >= g.window {
		g.logGate.lastSeen[path] = now
		// Bound the gate: arbitrary distinct paths must not grow the map
		// without limit.
		if len(g.logGate.lastSeen) > maxAccessGateEntries {
			oldestPath, oldest := "", time.Time{}
			for p, t := range g.logGate.lastSeen {
				if oldestPath == "" || t.Before(oldest) {
					oldestPath, oldest = p, t
				}
			}
			delete(g.logGate.lastSeen, oldestPath)
		}
		return true
	}
	return false
}

// accessQuietBudgetDue reports whether the quiet-class line budget remains
// and charges one line. The window rolls on first use after expiry.
func (g *accessGates) accessQuietBudgetDue(now time.Time) bool {
	g.quietBudget.mu.Lock()
	defer g.quietBudget.mu.Unlock()
	if g.quietBudget.windowStart.IsZero() || now.Sub(g.quietBudget.windowStart) >= g.window {
		g.quietBudget.windowStart = now
		g.quietBudget.lines = 0
	}
	if g.quietBudget.lines >= maxQuietAccessLines {
		return false
	}
	g.quietBudget.lines++
	return true
}

// reset clears the quiet-path access gate and budget (test hook).
func (g *accessGates) reset() {
	g.logGate.mu.Lock()
	clear(g.logGate.lastSeen)
	g.logGate.mu.Unlock()
	g.quietBudget.mu.Lock()
	g.quietBudget.windowStart = time.Time{}
	g.quietBudget.lines = 0
	g.quietBudget.mu.Unlock()
}

// remoteHost returns the client host without the port.
func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- Correlation IDs ---

// reqIDKey carries the per-request correlation id (req_id) through the
// request context. The key type is unexported so only this package can
// read/write it; the upstream client threads the same id a second way (via
// ChatOptions.RequestID) for its do()/retry log lines.
type reqIDKey struct{}

// reqIDFrom returns the request's correlation id, or "" when the request
// did not pass through the access wrapper (direct handler calls in tests).
func reqIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(reqIDKey{}).(string)
	return id
}

// originalBodyKey carries the client's raw request body (issue #140):
// handlers normalize+rename tools into a separate buffer, and chatCore needs
// the ORIGINAL names to build the response-side restore map.
type originalBodyKey struct{}

func withOriginalBody(ctx context.Context, body []byte) context.Context {
	return context.WithValue(ctx, originalBodyKey{}, body)
}

func originalBodyFromContext(ctx context.Context) []byte {
	b, _ := ctx.Value(originalBodyKey{}).([]byte)
	return b
}

// newReqID mints a UUIDv4 correlation id from crypto/rand (RFC 4122 §4.4:
// 122 random bits, version 4, variant 1). A rand failure is unrecoverable
// in practice; fall back to a time-seeded hex id rather than failing the
// request.
func newReqID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// clientRequestID sanitizes the client's X-Request-Id header for logging:
// trimmed, printable ASCII only (0x20-0x7e), max 64 runes. Returns "" when
// the header is absent or fails the checks — the field is then omitted from
// log lines (the proxy never trusts a client-supplied id as its correlation
// key, D1).
func clientRequestID(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get("X-Request-Id"))
	if v == "" || utf8.RuneCountInString(v) > 64 {
		return ""
	}
	for _, b := range []byte(v) {
		if b < 0x20 || b > 0x7e {
			return ""
		}
	}
	return v
}
