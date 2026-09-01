// Quota-ledger policy: per-code rate-limit event counting, window
// derivation, and the classification Debug line. The classification matrix
// itself (classifyError and the body parsers) lives in classify.go, and the
// sentinels + typed errors in errors.go.
package upstream

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// countRateLimitEvent increments the per-code rate-limit ledger. The
// map entry is created lazily so clients built without the constructor
// (tests, bridge entries) still record safely.
func (c *Client) countRateLimitEvent(code string) {
	c.rateLimitMu.Lock()
	ctr := c.rateLimitEvents[code]
	if ctr == nil {
		ctr = &atomic.Int64{}
		if c.rateLimitEvents == nil {
			c.rateLimitEvents = make(map[string]*atomic.Int64)
		}
		c.rateLimitEvents[code] = ctr
	}
	c.rateLimitMu.Unlock()
	ctr.Add(1)
}

// RateLimitEvents returns a copy of this client's per-code rate-limit
// classification counters (pool snapshot /metrics aggregation).
func (c *Client) RateLimitEvents() map[string]int64 {
	c.rateLimitMu.Lock()
	defer c.rateLimitMu.Unlock()
	out := make(map[string]int64, len(c.rateLimitEvents))
	for code, ctr := range c.rateLimitEvents {
		out[code] = ctr.Load()
	}
	return out
}

// classify maps an upstream error response through the shared matrix and
// records the 428 waiting_room_required flag (issue #94) so the pool can
// fire the gated pre-session chain before the next session create. All
// in-client error paths must use this wrapper; the free classifyError stays
// pure for tests.
func (c *Client) classify(status int, body string, hdr http.Header) error {
	err := classifyError(status, body, hdr)
	// classifyError returns a concrete typed error in the interface, never a
	// nil interface — so err is always non-nil; test the sentinel directly.
	if errors.Is(err, ErrWaitingRoomRequired) {
		c.waitingRoomRequired.Store(true)
	}
	// Rate-limit ledger: count every rate-limit-family classification by its
	// upstream body code and surface one Debug line carrying the FULL
	// (redacted) body, so the distinct refusal codes (free_mode_rate_limited,
	// insufficient_quota, limit_burst_rate, ip_capped, spend_limited,
	// rate_limited, ...) are distinguishable in logs before the #133
	// behavior fix lands.
	if code, window := rateLimitInfo(body, err); code != "" {
		c.countRateLimitEvent(code)
		logRateLimitClassified(status, body, code, window, err)
	}
	return err
}

// rateLimitInfo derives the ledger code and window for a rate-limit
// classification. The classification must be in the rate-limit error family
// (RateLimitError/IpCappedError/CapacityDeferredError) — 403 bans, 401 auth
// refusals, waiting rooms and other gates never count; code is empty then
// and nothing is logged.
func rateLimitInfo(body string, err error) (code, window string) {
	switch err.(type) {
	case *RateLimitError, *IpCappedError, *CapacityDeferredError:
	default:
		return "", ""
	}
	code = rateLimitCode(body, err)
	if code == "" {
		return "", ""
	}
	return code, rateLimitWindow(body, err)
}

// rateLimitCode extracts the upstream refusal code from the body's
// "error"/"type" field (free_mode_rate_limited, insufficient_quota,
// limit_burst_rate, ip_capped, spend_limited, rate_limited, ...), falling
// back to the classified error type when the body carries no code key.
func rateLimitCode(body string, err error) string {
	if code := bodyCode(body); code != "" {
		return code
	}
	switch e := err.(type) {
	case *CapacityDeferredError:
		return string(WireCodeFreeModeCapacityDeferred)
	case *IpCappedError:
		return string(WireCodeIpCapped)
	case *RateLimitError:
		if e.Status != "" {
			return e.Status // load_shedding | peak_hours
		}
		return string(WireCodeRateLimited)
	}
	return ""
}

// bodyCode reads the first non-empty "error":"X" or "type":"X" string from a
// JSON error body (the ledger's code source).
func bodyCode(body string) string {
	var raw struct {
		Error string `json:"error"`
		Type  string `json:"type"`
	}
	if json.Unmarshal([]byte(body), &raw) != nil {
		return ""
	}
	if raw.Error != "" {
		return raw.Error
	}
	return raw.Type
}

// rateLimitWindow maps a rate-limit classification to the shared window
// table: the body's own "1 minute"/"30 minutes" text when present, else
// "reset" when the error carries a reset timestamp, else "retry-after" when
// it carries a retry delay, else "none".
func rateLimitWindow(body string, err error) string {
	lower := strings.ToLower(body)
	if strings.Contains(lower, "1 minute") {
		return "1 minute"
	}
	if strings.Contains(lower, "30 minutes") {
		return "30 minutes"
	}
	switch e := err.(type) {
	case *RateLimitError:
		if !e.ResetAt.IsZero() {
			return "reset"
		}
		if e.RetryAfter > 0 {
			return "retry-after"
		}
	case *IpCappedError:
		if e.RetryAfter > 0 {
			return "retry-after"
		}
	case *CapacityDeferredError:
		if e.RetryAfter > 0 {
			return "retry-after"
		}
	}
	return "none"
}

// rateLimitFields extracts the retry-after delay and reset timestamp a
// rate-limit-family error carries, for the classification Debug line.
func rateLimitFields(err error) (time.Duration, time.Time) {
	switch e := err.(type) {
	case *RateLimitError:
		return e.RetryAfter, e.ResetAt
	case *IpCappedError:
		return e.RetryAfter, time.Time{}
	case *CapacityDeferredError:
		return e.RetryAfter, time.Time{}
	}
	return 0, time.Time{}
}

// logRateLimitClassified emits the rate-limit ledger Debug line. The body is logged
// in FULL (the 200-rune truncation applies to the HTTP error response only)
// and must already be redacted by the caller.
func logRateLimitClassified(status int, body, code, window string, err error) {
	attrs := []any{
		"status", status,
		"code", code,
		"window", window,
		"body", body,
	}
	if retryAfter, resetAt := rateLimitFields(err); retryAfter > 0 {
		attrs = append(attrs, "retry_after", int(retryAfter.Seconds()))
		if !resetAt.IsZero() {
			attrs = append(attrs, "reset_at", resetAt.UTC().Format(time.RFC3339))
		}
	}
	slog.Debug("upstream rate limit classified", attrs...)
}

// NextPacificMidnight returns the upcoming 00:00 Pacific Time in UTC
// (which is 07:00 UTC during PDT / 08:00 UTC during PST).
func NextPacificMidnight() time.Time {
	loc, err := time.LoadLocation("America/Los_Angeles")
	now := time.Now()
	if err != nil {
		return pacificMidnightFallback(now)
	}
	nowLoc := now.In(loc)
	nextDay := time.Date(nowLoc.Year(), nowLoc.Month(), nowLoc.Day()+1, 0, 0, 0, 0, loc)
	return nextDay.UTC()
}

// pacificMidnightFallback approximates the upcoming Pacific midnight without
// the IANA tzdata database: America/Los_Angeles is UTC-7 during PDT
// (roughly March-November) and UTC-8 during PST (roughly November-March).
// The month range is the documented approximation; the exact DST transition
// dates require tzdata.
func pacificMidnightFallback(now time.Time) time.Time {
	hour := 7 // PDT
	if m := now.UTC().Month(); m < time.March || m > time.November {
		hour = 8 // PST: December, January, February
	}
	t := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), hour, 0, 0, 0, time.UTC)
	if !t.After(now) {
		t = t.Add(24 * time.Hour)
	}
	return t
}
