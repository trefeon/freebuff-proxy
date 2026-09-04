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
	if TierChangeNotice == "" {
		t.Errorf("TierChangeNotice is empty")
	}
	if CapacityNotice == "" {
		t.Errorf("CapacityNotice is empty")
	}
	if RestrictedNotice == "" {
		t.Errorf("RestrictedNotice is empty")
	}
	if BudgetNotice == "" {
		t.Errorf("BudgetNotice is empty")
	}
	if FreebucksCeilingNotice == "" {
		t.Errorf("FreebucksCeilingNotice is empty")
	}
}
