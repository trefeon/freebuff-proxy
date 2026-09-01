// Shared wire/response helpers for the session, rate-limit, and chat
// response paths: JSON field extraction (getNumber/getTime), error-detail
// formatting (retryDetail/containsAny/unixFrom), and body draining and
// truncation (drainBody/truncate/truncateRunes). The debug dump writer lives
// in dump.go.
package upstream

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

func getNumber(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			switch n := v.(type) {
			case float64:
				return n, true
			case int:
				return float64(n), true
			case int64:
				return float64(n), true
			case string:
				if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
					return f, true
				}
			}
		}
	}
	return 0, false
}

func getTime(m map[string]any, keys ...string) (time.Time, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			// Trim whitespace from string timestamps (the original getTime
			// tolerated leading/trailing spaces; parseFlexTime does not), then
			// delegate the whole RFC3339/Nano + numeric-seconds/milliseconds
			// interpretation to parseFlexTime so both callers share ONE
			// semantics (unixFrom treats >= 1e11 as milliseconds).
			if s, ok := v.(string); ok {
				v = strings.TrimSpace(s)
			}
			if t, err := parseFlexTime(v); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

func retryDetail(retryAfter time.Duration) string {
	if retryAfter > 0 {
		return fmt.Sprintf(" (Retry-After %s)", retryAfter)
	}
	return ""
}

func containsAny(lower string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(lower, n) {
			return true
		}
	}
	return false
}

func unixFrom(secs int64) time.Time {
	// Heuristic: milliseconds if 10^12 or larger, else seconds.
	if secs >= 100_000_000_000 {
		return time.Unix(0, secs*int64(time.Millisecond))
	}
	return time.Unix(secs, 0)
}

func drainBody(r io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(r, maxDumpRead))
	return string(data)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// truncateRunes truncates s to at most max runes without an ellipsis. The
// CLI's FINISH errorMessage cap is 5000 chars (truncateString in
// reference/freebuff/sdk/src/impl/database.ts), applied on the whole
// payload — a full Go stack trace must not blow the cap.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
