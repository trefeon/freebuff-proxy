// Package logring is a bounded in-memory log buffer for the dashboard's log
// viewer. It wraps the process slog handler so every record is written to the
// normal sink (stderr/log file) AND retained for the UI — no log file or
// docker access needed.
package logring

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"freebuff-proxy/backend/internal/telemetry"
)

// Entry is one retained log record, pre-formatted for display.
type Entry struct {
	Time    string // RFC3339
	Level   string // slog level token ("INFO", "ERROR", ...)
	Message string
	Fields  []string // "key=value" pairs, flattened
}

// Ring is the bounded store shared by every handler clone.
type Ring struct {
	mu       sync.Mutex
	buf      []Entry
	next     int // next write position (ring)
	filled   int // entries written so far (grows to capacity)
	capacity int
	// counts tallies every handled record by "level|msg" (level lowercased)
	// so the /metrics endpoint can export log-event counters without a
	// second subscription. Ring entries are bounded; counts are not (a full
	// ring of distinct messages still counts every record).
	counts map[string]int64
}

// Handler forwards records to next while retaining the last capacity entries
// in the shared ring. WithAttrs/WithGroup return clones sharing the ring (the
// bound attrs are folded into the retained fields, mirroring telemetry's
// flat handler).
type Handler struct {
	ring   *Ring
	next   slog.Handler
	attrs  []slog.Attr
	groups []string
}

// NewHandler wraps next with a ring of the given capacity (records retained
// for the dashboard log viewer).
func NewHandler(next slog.Handler, capacity int) *Handler {
	if capacity < 1 {
		capacity = 1
	}
	return &Handler{ring: &Ring{buf: make([]Entry, capacity), capacity: capacity, counts: make(map[string]int64)}, next: next}
}

// Recent returns up to n entries, newest first.
func (h *Handler) Recent(n int) []Entry {
	return h.ring.recent(n)
}

// Counts returns a snapshot of the handled-record counters keyed
// "level|msg" (level lowercased). The snapshot is independent of the ring:
// mutating the returned map never affects later counts, and concurrent
// Handle calls are safe.
func (h *Handler) Counts() map[string]int64 {
	return h.ring.countsSnapshot()
}

func (r *Ring) countsSnapshot() map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int64, len(r.counts))
	for k, v := range r.counts {
		out[k] = v
	}
	return out
}

func (r *Ring) push(timeStr, level, message string, fields []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = Entry{Time: timeStr, Level: level, Message: message, Fields: fields}
	r.next = (r.next + 1) % r.capacity
	if r.filled < r.capacity {
		r.filled++
	}
	r.counts[countKey(level, message)]++
}

// countKey builds the "level|msg" metric key. slog's level tokens are a
// fixed set, so the common path lowercases without an extra allocation (the
// key itself is the only per-record allocation, required by the map lookup).
func countKey(level, message string) string {
	switch level {
	case "DEBUG":
		return "debug|" + message
	case "INFO":
		return "info|" + message
	case "WARN":
		return "warn|" + message
	case "ERROR":
		return "error|" + message
	}
	// Custom levels (e.g. telemetry.LevelTrace, which slog renders as
	// "DEBUG-4") fall through to a general lowercase.
	return strings.ToLower(level) + "|" + message
}

func (r *Ring) recent(n int) []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n < 0 {
		// S9: a negative count must return empty, not panic in make with a
		// negative capacity. No in-repo caller passes one, but this is a
		// public API (dashboard Recent(200)).
		n = 0
	}
	if n > r.filled {
		n = r.filled
	}
	out := make([]Entry, 0, n)
	// Walk backwards from the newest entry.
	for i := 0; i < n; i++ {
		idx := (r.next - 1 - i + r.capacity) % r.capacity
		out = append(out, r.buf[idx])
	}
	return out
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, rec slog.Record) error {
	prefix := strings.Join(h.groups, ".")
	fields := make([]string, 0, len(h.attrs)+4)
	for _, a := range h.attrs {
		fields = append(fields, telemetry.FlattenAttrs(prefix, []slog.Attr{a})...)
	}
	var flat []string
	rec.Attrs(func(a slog.Attr) bool {
		flat = append(flat, telemetry.FlattenAttrs(prefix, []slog.Attr{a})...)
		return true
	})
	fields = append(fields, flat...)
	h.ring.push(rec.Time.Format(time.RFC3339), rec.Level.String(), rec.Message, fields)
	return h.next.Handle(ctx, rec)
}

// WithAttrs clones the handler, folding the attrs into the retained fields
// and forwarding them to the wrapped sink (matching slog's contract).
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	c.next = h.next.WithAttrs(attrs)
	return &c
}

// WithGroup clones the handler, tracking the group for retained-field
// prefixes and forwarding it to the wrapped sink.
func (h *Handler) WithGroup(name string) slog.Handler {
	c := *h
	c.groups = append(append([]string{}, h.groups...), name)
	c.next = h.next.WithGroup(name)
	return &c
}
