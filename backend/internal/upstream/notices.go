package upstream

import (
	"time"
)

// Upstream static notices and announcement strings from CodebuffAI/freebuff.
// Kept in sync with common/src/util/freebuff-model-availability.ts and
// common/src/constants/freebuff-spend-ceilings.ts.

const (
	// TierChangeNotice is FREEBUFF_TIER_CHANGE_NOTICE from upstream
	// (common/src/util/freebuff-model-availability.ts:26-27).
	TierChangeNotice = "Solar Pro 4 is now unmetered at full access and available with limited access. GPT-5.6 Luna still uses your shared premium allowance, charging partial time rounded up to a tenth. —❤️ Freebuff Team"

	// CapacityNotice is FREEBUFF_CAPACITY_NOTICE
	// (common/src/constants/freebuff-spend-ceilings.ts:185-186).
	CapacityNotice = "Capacity is now limited per account — sustained automated abuse forced us to cap how much any one account can use."

	// RestrictedNotice is FREEBUFF_RESTRICTED_NOTICE
	// (common/src/constants/freebuff-spend-ceilings.ts:202-203).
	RestrictedNotice = "This account has reduced capacity: it was flagged for VPN or proxy usage, a restricted location, or an email domain commonly used by bot farms. If you are on a VPN, connecting directly restores normal limits."

	// BudgetNotice is FREEBUFF_BUDGET_NOTICE
	// (common/src/constants/freebuff-spend-ceilings.ts:260-261).
	BudgetNotice = "You have used all of today’s free usage on this account."

	// FreebucksCeilingNotice is FREEBUFF_FREEBUCKS_CEILING_NOTICE
	// (common/src/constants/freebuff-spend-ceilings.ts:244-245).
	FreebucksCeilingNotice = "You have reached today’s usage limit on this account — your Freebucks cover the session price, and a hard daily ceiling catches only the heaviest days."
)

// DeepSeekPeakHoursWindow holds the live evaluation of DeepSeek pricing windows
// (common/src/constants/freebuff-peak-hours.ts: DEEPSEEK_EXPENSIVE_WINDOW_UTC [0, 10]).
type DeepSeekPeakHoursWindow struct {
	IsPeak         bool      `json:"is_peak"`
	CurrentUTC     string    `json:"current_utc"`
	WindowStartUTC string    `json:"window_start_utc"`
	WindowEndUTC   string    `json:"window_end_utc"`
	NextWindowAt   time.Time `json:"next_window_at"`
	NextWindowIn   string    `json:"next_window_in"`
}

// EvaluateDeepSeekPeak determines whether the given time falls within
// DeepSeek's weekday peak pricing window (00:00 UTC - 10:00 UTC, Mon-Fri).
func EvaluateDeepSeekPeak(now time.Time) DeepSeekPeakHoursWindow {
	utc := now.UTC()
	weekday := utc.Weekday()
	hour := utc.Hour()

	isWeekday := weekday >= time.Monday && weekday <= time.Friday
	isPeakHour := hour >= 0 && hour < 10
	isPeak := isWeekday && isPeakHour

	var next time.Time
	if isPeak {
		// Window ends today at 10:00 UTC
		next = time.Date(utc.Year(), utc.Month(), utc.Day(), 10, 0, 0, 0, time.UTC)
	} else {
		// Next window starts tomorrow at 00:00 UTC, or Monday if weekend
		nextDay := utc.Add(24 * time.Hour)
		next = time.Date(nextDay.Year(), nextDay.Month(), nextDay.Day(), 0, 0, 0, 0, time.UTC)
		for next.Weekday() == time.Saturday || next.Weekday() == time.Sunday {
			next = next.Add(24 * time.Hour)
		}
	}

	remaining := next.Sub(utc).Round(time.Minute)
	remStr := remaining.String()

	return DeepSeekPeakHoursWindow{
		IsPeak:         isPeak,
		CurrentUTC:     utc.Format("15:04 UTC"),
		WindowStartUTC: "00:00 UTC",
		WindowEndUTC:   "10:00 UTC",
		NextWindowAt:   next,
		NextWindowIn:   remStr,
	}
}
