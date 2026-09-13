package pool

import (
	"testing"
	"time"
)

// TestRequestLedgerDayBucketRollsAtPacificMidnight pins the RPD bucket
// boundary: the day bucket rolls at Pacific midnight
// (America/Los_Angeles), NOT UTC midnight — the same instant upstream resets
// its daily quota windows. July exercises PDT (07:00Z), January PST
// (08:00Z), mirroring TestSpendDayBucketRollsAtPacificMidnight.
func TestRequestLedgerDayBucketRollsAtPacificMidnight(t *testing.T) {
	// PDT July: 06:59Z is still the PREVIOUS Pacific day.
	before := time.Date(2026, 7, 16, 6, 59, 0, 0, time.UTC)
	at := time.Date(2026, 7, 16, 7, 0, 0, 0, time.UTC) // 00:00 PDT 07-16
	l := newAccountLedger()
	l.recordDayRequest(before)
	if got := l.dayRequestCount(before); got != 1 {
		t.Errorf("PDT count before midnight = %d, want 1", got)
	}
	l.recordDayRequest(at)
	if got := l.dayRequestCount(at); got != 1 {
		t.Errorf("PDT count at midnight = %d, want 1 (bucket rolled, then +1)", got)
	}
	l.recordDayRequest(at.Add(time.Minute))
	if got := l.dayRequestCount(at.Add(time.Minute)); got != 2 {
		t.Errorf("PDT count after +1m = %d, want 2", got)
	}

	// PST January: the boundary is 08:00Z.
	winter := time.Date(2026, 1, 16, 8, 0, 0, 0, time.UTC) // 00:00 PST 01-16
	lw := newAccountLedger()
	lw.recordDayRequest(winter.Add(-time.Minute))
	if got := lw.dayRequestCount(winter.Add(-time.Minute)); got != 1 {
		t.Errorf("PST count before midnight = %d, want 1", got)
	}
	lw.recordDayRequest(winter)
	if got := lw.dayRequestCount(winter); got != 1 {
		t.Errorf("PST count at midnight = %d, want 1 (rolled)", got)
	}

}

// TestAcquireCountsAdmissionAtGrant pins that an admitted request is
// counted the moment the lease is granted — before any chat. A lease with
// no follow-up chat still consumed its admission slot.

// concurrent AcquireBridge calls for one client token against cap=1 must
// admit exactly one.
