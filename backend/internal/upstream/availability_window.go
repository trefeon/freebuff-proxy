package upstream

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AvailabilityWindow is the parsed daily availability window from a
// model_unavailable admission response's availableHours string (issue #158),
// e.g. "9am ET-5pm PT every day" or "08:00-20:00". Times are normalized to
// minutes since midnight in US Pacific — the reference zone FreeBuff
// sessions and quota windows are Pacific-based — so a skip decision needs no
// DST math at compare time. ET/EST/EDT are converted by subtracting the
// fixed 3-hour ET→PT offset (both observe US DST in lockstep).
type AvailabilityWindow struct {
	// StartMinute/EndMinute bound the daily window, minutes since midnight
	// Pacific. StartMinute == EndMinute means a degenerate/24-7 window:
	// callers must not skip on it.
	StartMinute int
	EndMinute   int
	// Raw is the original availableHours string, for logging.
	Raw string
}

// availableTimeRE matches "H[:MM] [am|pm] [zone]" - "H[:MM] [am|pm] [zone]"
// pairs in an availableHours string. Zones are best-effort 2-3 letter
// tokens (ET/PT/…); unrecognized tails are ignored (the regex is not
// anchored).
var availableTimeRE = regexp.MustCompile(`(?i)(\d{1,2})(?::(\d{2}))?\s*(am|pm)?\s*([a-z]{2,3})?\s*-\s*(\d{1,2})(?::(\d{2}))?\s*(am|pm)?\s*([a-z]{2,3})?`)

// ParseAvailabilityWindow parses an upstream availableHours string into a
// daily window. Supported shapes:
//
//	"08:00-20:00"            24-hour window (interpreted in Pacific)
//	"9am ET-5pm PT every day"  12-hour window with US timezone abbreviations
//
// ok is false when no start/end time pair can be extracted, so callers fall
// back to the plain cache TTL instead of deriving a skip bound.
func ParseAvailabilityWindow(s string) (AvailabilityWindow, bool) {
	m := availableTimeRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return AvailabilityWindow{}, false
	}
	start, ok1 := parseAvailableTime(m[1], m[2], m[3], m[4])
	end, ok2 := parseAvailableTime(m[5], m[6], m[7], m[8])
	if !ok1 || !ok2 {
		return AvailabilityWindow{}, false
	}
	return AvailabilityWindow{StartMinute: start, EndMinute: end, Raw: s}, true
}

// parseAvailableTime converts one time token to minutes since midnight
// Pacific. hour/minute may be 12-hour (with meridiem) or 24-hour; the zone
// token converts ET-family zones to Pacific minutes. Unknown/absent zones
// are interpreted directly as Pacific (best-effort — the caller's TTL cap
// bounds any misparse).
func parseAvailableTime(hour, minute, meridiem, zone string) (int, bool) {
	h, err := strconv.Atoi(hour)
	if err != nil || h < 0 || h > 23 {
		return 0, false
	}
	min := 0
	if minute != "" {
		m, err := strconv.Atoi(minute)
		if err != nil || m < 0 || m > 59 {
			return 0, false
		}
		min = m
	}
	switch strings.ToLower(meridiem) {
	case "am":
		if h == 12 {
			h = 0
		}
	case "pm":
		if h < 12 {
			h += 12
		}
	case "":
	default:
		return 0, false
	}
	total := h*60 + min
	switch strings.ToUpper(zone) {
	case "ET", "EST", "EDT":
		total -= 3 * 60
	case "PT", "PST", "PDT", "":
	default:
		// Unknown zone: keep as Pacific (best-effort).
	}
	total %= 1440
	if total < 0 {
		total += 1440
	}
	return total, true
}

// pacificLoc returns America/Los_Angeles, falling back to a fixed PDT zone
// when the tz database is unavailable (mirrors pool/spend.go's loc helper).
var pacificLoc = sync.OnceValue(func() *time.Location {
	if loc, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return loc
	}
	return time.FixedZone("PDT", -7*60*60)
})

// AvailableAt reports whether minute-of-day (Pacific) falls inside the
// window. A degenerate window (StartMinute == EndMinute) is always
// available: the caller must not skip on it.
func (w AvailabilityWindow) AvailableAt(now time.Time) bool {
	if w.StartMinute == w.EndMinute {
		return true
	}
	m := now.In(pacificLoc()).Hour()*60 + now.In(pacificLoc()).Minute()
	if w.StartMinute < w.EndMinute {
		return m >= w.StartMinute && m < w.EndMinute
	}
	// Overnight window (e.g. 22:00-06:00): open from start until midnight,
	// then from midnight until end.
	return m >= w.StartMinute || m < w.EndMinute
}

// NextStart returns the next wall-clock instant the window opens, strictly
// after now. For a degenerate window it returns now (no future opening to
// wait for).
func (w AvailabilityWindow) NextStart(now time.Time) time.Time {
	loc := pacificLoc()
	t := now.In(loc)
	m := t.Hour()*60 + t.Minute()
	if w.StartMinute == w.EndMinute {
		return now
	}
	start := time.Date(t.Year(), t.Month(), t.Day(), w.StartMinute/60, w.StartMinute%60, 0, 0, loc)
	if m < w.StartMinute {
		return start
	}
	// The window already opened today (and we are outside it — a refusal
	// was cached, so the upstream disagrees with our parse): the next
	// opening is tomorrow.
	return start.AddDate(0, 0, 1)
}
