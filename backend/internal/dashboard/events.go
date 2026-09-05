package dashboard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// eventStreamHub pushes tokensData snapshots to connected admin SPA clients
// over Server-Sent Events. Clients are registered by HandleEvents; a lazily
// started diff loop (1s) compares a field-hash of the current snapshot
// against the last broadcast and emits only on change, so a connected
// dashboard sees token status / session lifecycle / cooldown transitions
// within a second instead of on the next 10s poll.
//
// Backpressure: each client channel buffers 4 snapshots; a client that does
// not drain in time is dropped (single-operator UI — a stalled tab must not
// pin the loop). The loop stops itself when the last client disconnects.
type eventStreamHub struct {
	mu       sync.Mutex
	clients  map[chan eventMsg]struct{}
	lastHash string
	stop     chan struct{}
	stopped  bool
}

type eventMsg struct {
	event string
	data  string
}

const (
	eventHeartbeatEvery = 15 * time.Second
	eventDiffInterval   = 1 * time.Second
	eventClientBuffer   = 4
	eventMaxClients     = 8
)

func newEventStreamHub() *eventStreamHub {
	return &eventStreamHub{clients: make(map[chan eventMsg]struct{})}
}

// tokenStateHash fingerprints the operator-visible, liveness-sensitive state:
// per-token status, instance, cooldown, risk, active runs and a minute-bucket
// of the session countdown (so the client's own per-second tick is not
// spammed), plus mode, token count, and per-model quota recent counts.
func (d *Dashboard) tokenStateHash(td tokensData) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mode=%s;count=%d;rot=%s;failover=%v;mat_en=%v;",
		td.Mode, td.TokenCount, td.TokenRotation, td.RateLimitFailover, td.MaturityEnabled)
	for i := range td.Tokens {
		t := &td.Tokens[i]
		fmt.Fprintf(&b, "[%d]%s=%s;cd=%s;risk=%s;runs=%d;rem=%d;sess=%s;rpm=%d/%d;rpd=%d/%d;lock=%v;ban=%s:%s;",
			t.Index, t.SessionStatus, t.SessionInstance, t.CooldownUntil,
			t.RiskLevel, t.ActiveRuns,
			t.SessionRemainingSeconds/60, t.SessionModel,
			t.RequestsPerMinute, t.RequestsPerMinuteLimit,
			t.RequestsPerDay, t.RequestsPerDayLimit,
			t.Locked, t.BanType, t.BannedUntil)
		if t.Maturity != nil {
			fmt.Fprintf(&b, "m:%v/%d/%s/%s;", t.Maturity.Enabled, t.Maturity.Target, t.Maturity.Mode, t.Maturity.Badge)
		}
		for _, q := range t.Quota {
			fmt.Fprintf(&b, "q:%s=%s/%s;", q.Model, q.Recent, q.Limit)
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

// broadcast sends msg to every registered client, evicting any whose buffer
// is full (slow/stalled tab). Called with h.mu held by the diff loop.
func (h *eventStreamHub) broadcastLocked(msg eventMsg) {
	for ch := range h.clients {
		select {
		case ch <- msg:
		default:
			delete(h.clients, ch)
			close(ch)
		}
	}
}

// loop diffs snapshots every second while at least one client is connected.
// It runs on its own goroutine (started by subscribe) and stops itself when
// the last client leaves.
func (h *eventStreamHub) loop(d *Dashboard) {
	heartbeat := time.NewTicker(eventHeartbeatEvery)
	defer heartbeat.Stop()
	diff := time.NewTicker(eventDiffInterval)
	defer diff.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-heartbeat.C:
			h.mu.Lock()
			h.broadcastLocked(eventMsg{event: "ping", data: ""})
			h.mu.Unlock()
		case <-diff.C:
			td := d.tokensData()
			hash := d.tokenStateHash(td)
			h.mu.Lock()
			if hash != h.lastHash {
				h.lastHash = hash
				payload, err := json.Marshal(td)
				if err == nil {
					h.broadcastLocked(eventMsg{event: "tokens", data: string(payload)})
				}
			}
			h.mu.Unlock()
		}
	}
}

// subscribe registers a client and returns its message channel plus a
// cancel func. The first snapshot is delivered synchronously so the client
// renders instantly without waiting for the diff tick; the diff loop starts
// lazily on the first subscriber and stops when the last unsubscribes.
func (h *eventStreamHub) subscribe(d *Dashboard) (<-chan eventMsg, func(), string) {
	ch := make(chan eventMsg, eventClientBuffer)
	td := d.tokensData()
	payload, _ := json.Marshal(td)
	hash := d.tokenStateHash(td)

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) >= eventMaxClients {
		// Evict the oldest client (map iteration order is random; any
		// eviction is fine — the evicted tab falls back to polling).
		for old := range h.clients {
			delete(h.clients, old)
			break
		}
	}
	h.clients[ch] = struct{}{}
	if len(h.clients) == 1 {
		h.stop = make(chan struct{})
		h.stopped = false
		go h.loop(d)
	}
	h.lastHash = hash
	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.clients[ch]; ok {
			delete(h.clients, ch)
			close(ch)
			if len(h.clients) == 0 && !h.stopped {
				close(h.stop)
				h.stopped = true
			}
		}
	}
	return ch, cancel, string(payload)
}

// HandleEvents serves GET /admin/api/events as a text/event-stream. The
// session cookie gate is applied by the route table (AuthDashboard); the
// stream carries the same data the JSON APIs expose — nothing new — but
// pushes it the moment it changes instead of on the next poll.
func (d *Dashboard) HandleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	ch, cancel, first := d.events.subscribe(d)
	defer cancel()

	send := func(msg eventMsg) bool {
		if msg.event == "ping" {
			_, _ = fmt.Fprint(w, ": ping\n\n")
		} else {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", msg.event, msg.data)
		}
		flusher.Flush()
		return true
	}

	// First snapshot: full tokensData so the SPA paints immediately.
	_, _ = fmt.Fprintf(w, "event: tokens\ndata: %s\n\n", first)
	flusher.Flush()

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(eventHeartbeatEvery * 2))
	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			_ = rc.SetWriteDeadline(time.Now().Add(eventHeartbeatEvery * 2))
			send(msg)
		}
	}
}
