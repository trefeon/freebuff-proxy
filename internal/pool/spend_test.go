package pool

// Issue #122: the spend ledger's day/week/month buckets roll at Pacific
// wall-clock boundaries (America/Los_Angeles), DST-correct — 07:00 UTC
// during PDT (summer) and 08:00 UTC during PST (winter) — mirroring the
// CLI's getZonedDayBounds / getZonedWeekBounds
// (reference/freebuff/common/src/util/zoned-time.ts:78-92, weekStartsOn =
// Monday) and the upstream wire periods pacific_day / pacific_week with
// resetTimeZone America/Los_Angeles. July dates exercise PDT, January
// dates exercise PST.

import (
	"context"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

func TestBucketStartPacificDay(t *testing.T) {
	// July 2026 = PDT (UTC-7): the Pacific day starts at 07:00 UTC.
	// 06:59 UTC is still the PREVIOUS Pacific day (23:59 PDT).
	pdt := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	if got := bucketStart(pdt, "day"); got != time.Date(2026, time.July, 15, 7, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("bucketStart(July noon, day) = %v, want July 15 07:00 UTC (PDT)", time.Unix(got, 0).UTC())
	}
	before := time.Date(2026, time.July, 15, 6, 59, 0, 0, time.UTC)
	if got := bucketStart(before, "day"); got != time.Date(2026, time.July, 14, 7, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("bucketStart(July 06:59 UTC, day) = %v, want July 14 07:00 UTC (PDT day not yet rolled)", time.Unix(got, 0).UTC())
	}

	// January 2026 = PST (UTC-8): the Pacific day starts at 08:00 UTC.
	pst := time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC)
	if got := bucketStart(pst, "day"); got != time.Date(2026, time.January, 15, 8, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("bucketStart(January noon, day) = %v, want January 15 08:00 UTC (PST)", time.Unix(got, 0).UTC())
	}
	beforePST := time.Date(2026, time.January, 15, 7, 59, 0, 0, time.UTC)
	if got := bucketStart(beforePST, "day"); got != time.Date(2026, time.January, 14, 8, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("bucketStart(January 07:59 UTC, day) = %v, want January 14 08:00 UTC (PST day not yet rolled)", time.Unix(got, 0).UTC())
	}
}

// TestSpendLedgerPacificDayRollover pins the day-bucket rollover across the
// Pacific-midnight boundary in both PDT (July) and PST (January): spend
// before the boundary lands in the previous Pacific day, spend at/after it
// starts a fresh bucket.
func TestSpendLedgerPacificDayRollover(t *testing.T) {
	for _, tc := range []struct {
		name        string
		midnightUTC time.Time // 00:00 Pacific on the roll day
		prevDayUTC  time.Time // 00:00 Pacific on the day before
	}{
		{
			name:        "PDT July",
			midnightUTC: time.Date(2026, time.July, 16, 7, 0, 0, 0, time.UTC),
			prevDayUTC:  time.Date(2026, time.July, 15, 7, 0, 0, 0, time.UTC),
		},
		{
			name:        "PST January",
			midnightUTC: time.Date(2026, time.January, 16, 8, 0, 0, 0, time.UTC),
			prevDayUTC:  time.Date(2026, time.January, 15, 8, 0, 0, 0, time.UTC),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := newSpendLedger()
			// One minute before the Pacific-midnight boundary: still the
			// previous Pacific day.
			before := tc.midnightUTC.Add(-time.Minute)
			ledger.add(100, before)
			v := ledgerView(ledger)
			if v.Day != 100 {
				t.Fatalf("day after first add = %d, want 100", v.Day)
			}
			if v.DayStart.Unix() != tc.prevDayUTC.Unix() {
				t.Errorf("DayStart before boundary = %v, want %v (previous Pacific day)", v.DayStart, tc.prevDayUTC)
			}

			// At the boundary the bucket rolls: the new day starts fresh.
			ledger.add(50, tc.midnightUTC)
			v = ledgerView(ledger)
			if v.Day != 50 {
				t.Errorf("day after rollover = %d, want 50 (bucket reset at Pacific midnight)", v.Day)
			}
			if v.DayStart.Unix() != tc.midnightUTC.Unix() {
				t.Errorf("DayStart after boundary = %v, want %v", v.DayStart, tc.midnightUTC)
			}
		})
	}
}

func TestBucketStartPacificWeek(t *testing.T) {
	// July 13 2026 is a Monday; in PDT (UTC-7) the Pacific week starts at
	// 07:00 UTC that day.
	thu := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC) // Thursday
	if got := bucketStart(thu, "week"); got != time.Date(2026, time.July, 13, 7, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("bucketStart(July Thursday, week) = %v, want Monday July 13 07:00 UTC (PDT)", time.Unix(got, 0).UTC())
	}
	// January 12 2026 is a Monday; in PST (UTC-8) the week starts at
	// 08:00 UTC.
	janThu := time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC)
	if got := bucketStart(janThu, "week"); got != time.Date(2026, time.January, 12, 8, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("bucketStart(January Thursday, week) = %v, want Monday January 12 08:00 UTC (PST)", time.Unix(got, 0).UTC())
	}
	// A Sunday is the LAST day of the Pacific week.
	sun := time.Date(2026, time.July, 19, 23, 0, 0, 0, time.UTC)
	if got := bucketStart(sun, "week"); got != time.Date(2026, time.July, 13, 7, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("bucketStart(July Sunday, week) = %v, want Monday July 13 07:00 UTC", time.Unix(got, 0).UTC())
	}
}

func TestBucketStartPacificMonth(t *testing.T) {
	// Month has no CLI equivalent; it uses the Pacific calendar month for
	// consistency with the day/week buckets (issue #122).
	mid := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	if got := bucketStart(mid, "month"); got != time.Date(2026, time.July, 1, 7, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("bucketStart(July, month) = %v, want July 1 07:00 UTC (PDT)", time.Unix(got, 0).UTC())
	}
	jan := time.Date(2026, time.January, 16, 12, 0, 0, 0, time.UTC)
	if got := bucketStart(jan, "month"); got != time.Date(2026, time.January, 1, 8, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("bucketStart(January, month) = %v, want January 1 08:00 UTC (PST)", time.Unix(got, 0).UTC())
	}
}

// TestBucketStartFallback pins the tzdata-less fallback (mirrors
// upstream.pacificMidnightFallback): Pacific is UTC-7 (07:00 UTC midnight)
// March-November and UTC-8 (08:00 UTC) otherwise, and bucketStart always
// returns a start <= now.
func TestBucketStartFallback(t *testing.T) {
	july := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	if got := bucketStartFallback(july, "day"); got != time.Date(2026, time.July, 15, 7, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("fallback July day = %v, want July 15 07:00 UTC", time.Unix(got, 0).UTC())
	}
	jan := time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC)
	if got := bucketStartFallback(jan, "day"); got != time.Date(2026, time.January, 15, 8, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("fallback January day = %v, want January 15 08:00 UTC", time.Unix(got, 0).UTC())
	}
	// Pre-midnight instant: 06:00 UTC July 15 is still Pacific July 14.
	pre := time.Date(2026, time.July, 15, 6, 0, 0, 0, time.UTC)
	if got := bucketStartFallback(pre, "day"); got != time.Date(2026, time.July, 14, 7, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("fallback pre-midnight day = %v, want July 14 07:00 UTC", time.Unix(got, 0).UTC())
	}
}

// TestRecordSpendLimited pins issue #122's event counter: an upstream
// spend_limited refusal is counted per token on the ledger, while a plain
// rate_limited refusal is not (the $ ceiling is server-enforced; the ledger
// only records the event).
func TestRecordSpendLimited(t *testing.T) {
	p := newTestPool(t, testutil.NewMock())
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.CooldownTokenRateLimit(lease.Token, &upstream.RateLimitError{Status: "spend_limited", RetryAfter: time.Minute})
	p.CooldownTokenRateLimit(lease.Token, &upstream.RateLimitError{Status: "rate_limited", RetryAfter: time.Minute})
	p.CooldownTokenRateLimit(lease.Token, &upstream.RateLimitError{Status: "spend_limited", RetryAfter: time.Minute})
	p.LeaseRelease(lease)

	snaps := p.Snapshot()
	if len(snaps) != 1 {
		t.Fatalf("tokens = %d, want 1", len(snaps))
	}
	if snaps[0].SpendLimited != 2 {
		t.Errorf("SpendLimited = %d, want 2 (only spend_limited refusals counted)", snaps[0].SpendLimited)
	}

	// Bridge path counts on the bridge entry's ledger.
	bp := newBridgePool(t, testutil.NewMock())
	blease, err := bp.AcquireBridge(context.Background(), "client-tok", modelA)
	if err != nil {
		t.Fatal(err)
	}
	bp.CooldownBridgeRateLimit(blease, &upstream.RateLimitError{Status: "spend_limited", RetryAfter: time.Minute})
	bp.CooldownBridgeRateLimit(blease, &upstream.RateLimitError{Status: "rate_limited", RetryAfter: time.Minute})
	bv := bp.bridgeSpendSnapshot(blease.Bridge)
	if bv.SpendLimited != 1 {
		t.Errorf("bridge SpendLimited = %d, want 1 (only spend_limited counted)", bv.SpendLimited)
	}
	bp.LeaseRelease(blease)
}

// TestSpendPct pins the advisory ceiling surface (issue #122): the
// Pacific-day bucket vs MAX_SPEND_PER_DAY, capped at 100%.
func TestSpendPct(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPoolCfg(t, func(c *config.Config) { c.MaxSpendPerDay = 1000 }, mock)
	lease, err := p.Acquire(context.Background(), modelA)
	if err != nil {
		t.Fatal(err)
	}
	p.RecordSpend(lease, 300)
	p.LeaseRelease(lease)

	snaps := p.Snapshot()
	if len(snaps) != 1 {
		t.Fatalf("tokens = %d, want 1", len(snaps))
	}
	if snaps[0].SpendLimit != 1000 {
		t.Errorf("SpendLimit = %d, want 1000 (MAX_SPEND_PER_DAY)", snaps[0].SpendLimit)
	}
	if snaps[0].SpendPct != 30 {
		t.Errorf("SpendPct = %d, want 30 (300 of 1000)", snaps[0].SpendPct)
	}
	if snaps[0].SpendDay != 300 {
		t.Errorf("SpendDay = %d, want 300", snaps[0].SpendDay)
	}
}
