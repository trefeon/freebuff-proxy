package upstream

import (
	"testing"
	"time"
)

func TestEvaluateDeepSeekPeak(t *testing.T) {
	// Peak window: Wednesday at 05:00 UTC (isWeekday && hour in [0, 10))
	wedPeak := time.Date(2026, 9, 2, 5, 0, 0, 0, time.UTC)
	res := EvaluateDeepSeekPeak(wedPeak)
	if !res.IsPeak {
		t.Errorf("EvaluateDeepSeekPeak(Wed 05:00 UTC).IsPeak = false, want true")
	}
	if res.NextWindowAt.Hour() != 10 {
		t.Errorf("res.NextWindowAt.Hour() = %d, want 10", res.NextWindowAt.Hour())
	}

	// Off-peak weekday: Wednesday at 15:00 UTC
	wedOff := time.Date(2026, 9, 2, 15, 0, 0, 0, time.UTC)
	resOff := EvaluateDeepSeekPeak(wedOff)
	if resOff.IsPeak {
		t.Errorf("EvaluateDeepSeekPeak(Wed 15:00 UTC).IsPeak = true, want false")
	}
	if resOff.NextWindowAt.Weekday() != time.Thursday || resOff.NextWindowAt.Hour() != 0 {
		t.Errorf("resOff.NextWindowAt = %v, want Thursday 00:00 UTC", resOff.NextWindowAt)
	}

	// Weekend: Saturday at 05:00 UTC
	sat := time.Date(2026, 9, 5, 5, 0, 0, 0, time.UTC)
	resSat := EvaluateDeepSeekPeak(sat)
	if resSat.IsPeak {
		t.Errorf("EvaluateDeepSeekPeak(Sat 05:00 UTC).IsPeak = true, want false (weekend is off-peak)")
	}
	if resSat.NextWindowAt.Weekday() != time.Monday || resSat.NextWindowAt.Hour() != 0 {
		t.Errorf("resSat.NextWindowAt = %v, want Monday 00:00 UTC", resSat.NextWindowAt)
	}
}

func TestNoticeConstants(t *testing.T) {
	// Exact copy parity with upstream
	// common/src/constants/freebuff-spend-ceilings.ts (vendor 13105816):
	// a silent reword here desyncs refusal UX from the official CLI.
	want := map[string]string{
		"CapacityNotice":         "Capacity is now limited per account — sustained automated abuse forced us to cap how much any one account can use.",
		"RestrictedNotice":       "This account has reduced capacity: it was flagged for VPN or proxy usage, a restricted location, or an email domain commonly used by bot farms. If you are on a VPN, connecting directly restores normal limits.",
		"BudgetNotice":           "You have used all of today’s free usage on this account.",
		"FreebucksCeilingNotice": "This account hit today’s hard usage cap. Freebucks pay for sessions, but the compute a day can draw is capped at three times what its Freebucks are worth, to protect the service from runaway usage.",
	}
	got := map[string]string{
		"CapacityNotice":         CapacityNotice,
		"RestrictedNotice":       RestrictedNotice,
		"BudgetNotice":           BudgetNotice,
		"FreebucksCeilingNotice": FreebucksCeilingNotice,
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s = %q, want upstream copy %q", name, got[name], w)
		}
	}
	if TierChangeNotice == "" {
		t.Errorf("TierChangeNotice is empty")
	}
}
