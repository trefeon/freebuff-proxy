// Package notify fires best-effort webhook alerts for operational events
// (issue #48): a JSON POST to a configured WEBHOOK_URL when the token pool
// is exhausted or a token is classified banned. Delivery is fire-and-forget
// with its own timeout — the request path never blocks on the webhook — and
// each event type is throttled to at most one POST per 5 minutes so a
// repeated 429 storm alerts once, not once per request.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// defaultTimeout bounds one webhook POST (dial + write + body drain). The
// alert must never hold a request path, so the send runs on a background
// goroutine with this deadline even when the caller's context dies.
const defaultTimeout = 5 * time.Second

// throttleWindow is the dedupe window: at most one POST per event type per
// window (issue #48: "at most one per event type per 5m").
const throttleWindow = 5 * time.Minute

// Sender POSTs webhook alerts to a single URL. Zero value is a no-op sender
// (URL empty → Send drops silently), so callers can hold a *Sender without
// nil checks.
type Sender struct {
	url    string
	client *http.Client
	logger *slog.Logger // send-failure WARN sink (nil = slog.Default())

	mu        sync.Mutex
	lastSent  map[string]time.Time // event type → last accepted POST time
	startedAt time.Time
}

// New builds a sender for url ("" disables: Send becomes a no-op). client
// is used for the POSTs; nil uses a client with defaultTimeout.
func New(url string, client *http.Client) *Sender {
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	return &Sender{url: url, client: client, logger: slog.Default(), lastSent: make(map[string]time.Time), startedAt: time.Now()}
}

// SetLogger replaces the sender's log sink (nil restores slog.Default).
// Used by tests and by hosts that want the send-failure WARN on a custom
// logger.
func (s *Sender) SetLogger(l *slog.Logger) {
	if l == nil {
		l = slog.Default()
	}
	s.logger = l
}

// Event is one webhook payload (issue #48). Fields mirror the alert
// contract: event names the type, token_index the affected pool token
// (1-based; 0 for bridge), model the requested model when known, message a
// human-readable summary, timestamp the event time in RFC3339.
type Event struct {
	Event      string `json:"event"`       // "pool_exhausted" | "token_banned" | "agent_model_mismatch_escalation"
	TokenIndex int    `json:"token_index"` // 1-based pooled token index; 0 = bridge/unset
	Model      string `json:"model"`       // requested model ("" when unknown)
	Message    string `json:"message"`
	Timestamp  string `json:"timestamp"` // RFC3339 UTC
}

// Send fires a best-effort webhook POST for the event, throttled per event
// type (at most one per throttleWindow). It never blocks: the POST runs on
// a background goroutine with its own timeout. A nil receiver or an empty
// configured URL is a no-op. Delivery failures are logged as a WARN (T18) —
// the alert itself stays best-effort by design (issue #48:
// "fire-and-forget goroutine with its own timeout").
func (s *Sender) Send(event Event) {
	if s == nil || s.url == "" {
		return
	}
	if !s.throttle(event.Event) {
		return
	}
	if event.Timestamp == "" {
		event.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	go s.post(event)
}

// throttle reports whether a POST for eventType may fire now (first in the
// window, or the last POST was more than throttleWindow ago).
func (s *Sender) throttle(eventType string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if last, ok := s.lastSent[eventType]; ok && now.Sub(last) < throttleWindow {
		return false
	}
	s.lastSent[eventType] = now
	return true
}

// post performs one webhook POST with the sender's client timeout. The
// payload is the JSON event; the response is drained and closed, and a
// non-2xx status is treated as a failed delivery. Delivery failures are
// logged as a WARN with the err and the target URL (T18) — the alert
// itself stays best-effort and never blocks the request path.
func (s *Sender) post(event Event) {
	payload, err := json.Marshal(event)
	if err != nil {
		s.logger.Warn("webhook send failed", "err", err, "target", s.url)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(payload))
	if err != nil {
		s.logger.Warn("webhook send failed", "err", err, "target", s.url)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "freebuff-proxy-webhook/1.0")
	resp, err := s.client.Do(req)
	if err != nil {
		s.logger.Warn("webhook send failed", "err", err, "target", s.url)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = drain(resp)
		s.logger.Warn("webhook send failed", "err", fmt.Errorf("webhook returned status %d", resp.StatusCode), "target", s.url)
		return
	}
	_, _ = drain(resp)
}

// drain consumes and discards a bounded prefix of the response body so the
// connection can be reused; oversized bodies are cut short.
func drain(resp *http.Response) (int64, error) {
	buf := make([]byte, 4096)
	var n int64
	for n < 64<<10 {
		got, err := resp.Body.Read(buf)
		n += int64(got)
		if err != nil {
			return n, err
		}
	}
	return n, fmt.Errorf("webhook body exceeds 64KiB")
}
