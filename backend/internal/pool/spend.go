package pool

// Local per-token spend ledger (issue #87): an in-memory rolling 24h token
// spend window plus Pacific day/week/month buckets with rollover (issue
// #122: all period boundaries are America/Los_Angeles wall-clock — 00:00
// Pacific, Monday 00:00 Pacific, 1st 00:00 Pacific — resolving to
// 07:00/08:00 UTC by DST, matching the CLI's getZonedDayBounds /
// getZonedWeekBounds, reference/freebuff/common/src/util/zoned-time.ts:78-92,
// and the upstream wire periods pacific_day / pacific_week), mirroring the
// reference account quota bookkeeping (reference/freebuff-reverse
// internal/accounts/record.go QuotaUsed/QuotaPeriodStart and
// internal/quota/quota.go BucketStart/NeedsRollover). Updated from chat
// usage (pool.RecordSpend, fed by the server's parsed usage blocks) and
// surfaced next to Messages24h in the healthz token snapshot.
//
// The account's $15/$5/$0.50 daily spend ceilings (reference/freebuff
// freebuff-spend-ceilings.ts) are SERVER-enforced at Pacific midnight, and
// the proxy cannot know which cohort (full/limited/restricted) a token's
// account sits in — so this ledger is a token-count heuristic, not an exact
// USD accounting. Per-token granularity is the right level: one proxy token
// is one upstream account, so the per-token ledger tracks the same
// per-account spend the server counts.

import (
	"sync"
	"time"
)

// pacificLoc returns the cached America/Los_Angeles location, or nil when
// the IANA tzdata database is unavailable. time.LoadLocation consults
// ZONEINFO, the system zoneinfo, $GOROOT/lib/time/zoneinfo.zip, and the
// time/tzdata import in that order (go/src/time/zoneinfo.go LoadLocation);
// a stripped single-binary deployment with none of those gets nil and the
// callers fall back to the exact US-DST-rule boundaries below (pacificDate
// / pacificDayStart / bucketStartFallback), never a fixed UTC hour.
var pacificLoc = sync.OnceValue(func() *time.Location {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		return nil
	}
	return loc
})

// spendEntry is one recorded spend amount at a point in time (rolling 24h
// window).
type spendEntry struct {
	at     time.Time
	tokens int64
}

// spendLedger is one token's spend state. Guarded by Pool.spendMu.
type spendLedger struct {
	// rolling is the 24h window: amounts with timestamps, pruned on access.
	rolling []spendEntry
	// Pacific day/week/month buckets with their period start (unix); roll
	// over per BucketStart/NeedsRollover semantics (issue #122: boundaries
	// are America/Los_Angeles wall-clock, DST-correct).
	dayUsed    int64
	dayStart   int64
	weekUsed   int64
	weekStart  int64
	monthUsed  int64
	monthStart int64
	// spendLimited counts upstream spend_limited refusals observed for this
	// token since process start (issue #122): the server-side $ ceiling is
	// authoritative (the proxy cannot know the account's restricted cohort),
	// so this is an event counter, not a gate.
	spendLimited int
}

func newSpendLedger() *spendLedger { return &spendLedger{} }

// add records tokens spent now, rolling the period buckets when their
// windows closed (NeedsRollover). Caller holds Pool.spendMu.
func (l *spendLedger) add(tokens int64, now time.Time) {
	if l == nil || tokens <= 0 {
		return
	}
	// Rolling 24h window: prune entries outside the window, append this one.
	cutoff := now.Add(-24 * time.Hour)
	first := 0
	for first < len(l.rolling) && l.rolling[first].at.Before(cutoff) {
		first++
	}
	l.rolling = append(l.rolling[first:], spendEntry{at: now, tokens: tokens})

	// Period buckets with rollover.
	l.dayUsed, l.dayStart = rollBucket(l.dayUsed, l.dayStart, "day", now, tokens)
	l.weekUsed, l.weekStart = rollBucket(l.weekUsed, l.weekStart, "week", now, tokens)
	l.monthUsed, l.monthStart = rollBucket(l.monthUsed, l.monthStart, "month", now, tokens)
}

// rollBucket adds tokens to one period bucket, resetting it first when the
// window rolled over (start == 0 or PeriodEnd passed).
func rollBucket(used, start int64, period string, now time.Time, tokens int64) (int64, int64) {
	if needsRollover(start, period, now) {
		start = bucketStart(now, period)
		used = 0
	}
	return used + tokens, start
}

// bucketStart is the start of the period containing now in Pacific
// wall-clock (America/Los_Angeles): day = 00:00 Pacific, week = Monday
// 00:00 Pacific, month = 1st 00:00 Pacific. All resolve to 07:00 UTC during
// PDT and 08:00 UTC during PST, mirroring the CLI's getZonedDayBounds /
// getZonedWeekBounds (reference/freebuff/common/src/util/zoned-time.ts:78-92,
// weekStartsOn = Monday) and the upstream wire periods pacific_day /
// pacific_week with resetTimeZone America/Los_Angeles. Month has no CLI
// equivalent and uses the Pacific calendar month for consistency. time.Date
// interprets the wall clock in the location and resolves DST transitions via
// the zone rules (go/src/time/time.go Date), so no fixed UTC offset is used;
// without tzdata the exact US-DST-rule fallback (pacificDate +
// bucketStartFallback) derives the same boundaries.
func bucketStart(now time.Time, period string) int64 {
	if loc := pacificLoc(); loc != nil {
		n := now.In(loc)
		switch period {
		case "week":
			days := (int(n.Weekday()) + 6) % 7 // Monday=0 … Sunday=6
			return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc).
				AddDate(0, 0, -days).UTC().Unix()
		case "month":
			return time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc).UTC().Unix()
		}
	}
	return bucketStartFallback(now, period)
}

// bucketStartFallback derives bucketStart without the IANA tzdata database,
// using the exact US DST rule (the same rule pacificDayStart documents):
// PDT = UTC-7 from the second Sunday of March 09:00Z to the first Sunday of
// November 08:00Z, PST = UTC-8 otherwise. The day boundary is
// pacificDayStart; week/month subtract to Monday / the 1st from now's
// Pacific wall-clock date and resolve that date's midnight with the offset
// in effect at 00:00 local (the transition Sundays keep the OLD offset at
// their own midnight). This extends the day bucket's exact-rule spirit to
// week/month instead of the reference's month-range approximation, so a
// stripped single-binary deployment still gets DST-correct boundaries
// year-round.
func bucketStartFallback(now time.Time, period string) int64 {
	y, m, d := pacificDate(now)
	switch period {
	case "week":
		date := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		daysBack := (int(date.Weekday()) + 6) % 7 // Monday=0 … Sunday=6
		mon := date.AddDate(0, 0, -daysBack)
		my, mm, md := mon.Date()
		return time.Date(my, mm, md, 0, 0, 0, 0, time.UTC).
			Add(pacificOffsetAtLocalMidnight(my, mm, md)).Unix()
	case "month":
		return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC).
			Add(pacificOffsetAtLocalMidnight(y, m, 1)).Unix()
	default: // day
		return pacificDayStart(now).Unix()
	}
}

// pacificDate returns the Pacific wall-clock calendar date of the instant t,
// derived from the exact US DST rule (no tzdata needed): the offset in
// effect at t shifts t to Pacific local time, whose date is the Pacific
// calendar date (reference getZonedDayBounds uses the same local-date
// derivation).
func pacificDate(t time.Time) (int, time.Month, int) {
	local := t.UTC().Add(-pacificOffsetAt(t))
	return local.Date()
}

// needsRollover reports whether a bucket whose window started at start has
// closed by now. The window ends at the next Pacific-wall-clock period
// boundary: midnight+1 day, Monday+7 days, 1st+1 month — calendar arithmetic
// via AddDate, which shifts the UTC instant across DST transitions exactly
// like the CLI's getZonedDayBounds / getZonedWeekBounds
// (reference/freebuff/common/src/util/zoned-time.ts:78-92; AddDate
// normalizes like Date, go/src/time/time.go). Without tzdata the boundaries
// are derived from the exact US DST rule in Pacific wall-clock (the same
// derivation as bucketStartFallback): a fixed-UTC AddDate window would be
// 1h off across transition weeks (the spring-forward week is 169h, the
// fall-back week 167h).
func needsRollover(start int64, period string, now time.Time) bool {
	if start == 0 {
		return true
	}
	loc := pacificLoc()
	end := time.Unix(start, 0).UTC()
	if loc != nil {
		end = end.In(loc)
	}
	switch period {
	case "week":
		if loc == nil {
			end = fallbackNextBoundary(end, 0, 7)
		} else {
			end = end.AddDate(0, 0, 7)
		}
	case "month":
		if loc == nil {
			end = fallbackNextBoundary(end, 1, 0)
		} else {
			end = end.AddDate(0, 1, 0)
		}
	default:
		if loc == nil {
			// tzdata-less: the day window ends at the NEXT Pacific
			// midnight, which is 23/24/25 hours after the start on DST
			// transition days — a fixed 24h window would roll the bucket
			// at the wrong instant (#122).
			end = nextPacificMidnight(end)
		} else {
			end = end.AddDate(0, 0, 1)
		}
	}
	return !now.Before(end)
}

// fallbackNextBoundary resolves the tzdata-less Pacific-wall-clock boundary
// months/days after the instant start: derive start's Pacific calendar date,
// step the calendar, and resolve that date's midnight with the offset in
// effect at 00:00 local (the transition Sundays keep the OLD offset at their
// own midnight). start is always a period start (Monday / 1st midnight), so
// the AddDate steps land on the next period's first day.
func fallbackNextBoundary(start time.Time, months, days int) time.Time {
	u := start.UTC()
	local := u.Add(-pacificOffsetAt(u))
	y, m, d := local.AddDate(0, months, days).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Add(pacificOffsetAtLocalMidnight(y, m, d))
}

// pacificDayStart is the Pacific-midnight (America/Los_Angeles) start of the
// day containing now. Prefers the system tz database; when unavailable it
// derives the boundary from the US DST rule (PDT = UTC-7 from the second
// Sunday of March 09:00Z to the first Sunday of November 08:00Z; PST = UTC-8
// otherwise) — never a fixed UTC hour, so the 07:00Z/08:00Z midnight is
// DST-aware (#122, reference/freebuff zoned-time.ts getZonedDayBounds).
func pacificDayStart(now time.Time) time.Time {
	if loc := pacificLoc(); loc != nil {
		y, m, d := now.In(loc).Date()
		return time.Date(y, m, d, 0, 0, 0, 0, loc)
	}
	// No system tzdata: compute the Pacific calendar date of now via the
	// offset in effect at this instant, then that date's midnight via the
	// offset in effect at 00:00 local (the transition Sundays keep the OLD
	// offset at their own midnight — the spring-forward day starts in PST
	// and the fall-back day starts in PDT).
	u := now.UTC()
	local := u.Add(-pacificOffsetAt(u))
	y, m, d := local.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Add(pacificOffsetAtLocalMidnight(y, m, d))
}

// nextPacificMidnight returns the first Pacific midnight strictly after the
// Pacific-midnight instant start (23/24/25 hours later — the DST day
// length). Wall-clock calendar math, so the offset is re-resolved for the
// new date instead of naively adding 24h.
func nextPacificMidnight(start time.Time) time.Time {
	if loc := pacificLoc(); loc != nil {
		return start.In(loc).AddDate(0, 0, 1)
	}
	// tzdata-less: derive start's Pacific wall-clock date, add one day, and
	// resolve that date's midnight with the offset in effect at 00:00 local.
	u := start.UTC()
	local := u.Add(-pacificOffsetAt(u))
	y, m, d := local.AddDate(0, 0, 1).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Add(pacificOffsetAtLocalMidnight(y, m, d))
}

// pacificOffsetAt returns the Pacific UTC offset in effect at the instant t:
// PDT (UTC-7) from the second Sunday of March 10:00Z (02:00 PST) to the
// first Sunday of November 09:00Z (02:00 PDT), PST (UTC-8) otherwise. The
// transition instants are the wall-clock 02:00 local times: spring 02:00
// PST = 10:00Z, fall 02:00 PDT = 09:00Z.
func pacificOffsetAt(t time.Time) time.Duration {
	u := t.UTC()
	spring := nthSunday(u.Year(), time.March, 2).Add(10 * time.Hour)
	fall := nthSunday(u.Year(), time.November, 1).Add(9 * time.Hour)
	if !u.Before(spring) && u.Before(fall) {
		return 7 * time.Hour
	}
	return 8 * time.Hour
}

// pacificOffsetAtLocalMidnight returns the Pacific UTC offset in effect at
// 00:00 local on the calendar date y-m-d. The transition Sundays keep the
// OLD offset at their own midnight (both transitions happen at 02:00 local),
// so the date is compared strictly inside the DST span.
func pacificOffsetAtLocalMidnight(y int, m time.Month, d int) time.Duration {
	spring := nthSunday(y, time.March, 2)
	fall := nthSunday(y, time.November, 1)
	date := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	if date.After(spring) && date.Before(fall.AddDate(0, 0, 1)) {
		return 7 * time.Hour
	}
	return 8 * time.Hour
}

// nthSunday returns the calendar date of the nth Sunday in month m of year
// y (US DST rule: second Sunday of March, first Sunday of November).
func nthSunday(y int, m time.Month, n int) time.Time {
	first := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	return first.AddDate(0, 0, (7-int(first.Weekday()))%7+(n-1)*7)
}

// rolling24h prunes the window and returns the total within the last 24h.
// Caller holds Pool.spendMu.
func (l *spendLedger) rolling24h(now time.Time) int64 {
	if l == nil {
		return 0
	}
	cutoff := now.Add(-24 * time.Hour)
	first := 0
	for first < len(l.rolling) && l.rolling[first].at.Before(cutoff) {
		first++
	}
	l.rolling = l.rolling[first:]
	var total int64
	for _, e := range l.rolling {
		total += e.tokens
	}
	return total
}

// --- pool wiring ---

// recordSpend adds tokens to the entry at token's ledger (fixed-token index
// path). The ledger travels with the entry, so the roster's single mutex
// guards it (issue #263).
func (p *Pool) recordSpend(token int, tokens int64) {
	p.roster.recordSpend(token, tokens)
	p.logSpendBuckets(tokens)
}

// logSpendBuckets emits one Debug line per period bucket a spend record
// updated: bucket names the ledger bucket, spend_delta the tokens
// added, period the wire-style period name. Debug level so the per-chat
// ledger noise only appears when operators opt in.
func (p *Pool) logSpendBuckets(tokens int64) {
	p.logger.Debug("pool: spend bucket updated", "bucket", "day", "spend_delta", tokens, "period", "pacific_day")
	p.logger.Debug("pool: spend bucket updated", "bucket", "week", "spend_delta", tokens, "period", "pacific_week")
	p.logger.Debug("pool: spend bucket updated", "bucket", "month", "spend_delta", tokens, "period", "pacific_month")
}

// recordSpendEntry adds tokens to the lease's backing entry's ledger by
// pointer (mirrors recordChatEntry: after a concurrent RemoveLastToken+
// AddToken, the lease's Token index may target a different token). The entry
// is the authoritative owner of its ledger, so no index lookup is needed.
func (p *Pool) recordSpendEntry(entry *tokenEntry, tokens int64) {
	p.roster.recordSpendEntry(entry, tokens)
	p.logSpendBuckets(tokens)
}

// bridgeRecordSpend adds tokens to a bridge entry's ledger.
func (p *Pool) bridgeRecordSpend(entry *bridgeEntry, tokens int64) {
	if entry == nil {
		return
	}
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	entry.ledger.recordSpend(tokens, time.Now())
	p.logSpendBuckets(tokens)
}

// spendView is one ledger's snapshot for healthz (issue #87).
type spendView struct {
	Rolling24h   int64
	Day          int64
	DayStart     time.Time
	Week         int64
	WeekStart    time.Time
	Month        int64
	MonthStart   time.Time
	SpendLimited int // upstream spend_limited refusals since process start (#122)
}

// spendSnapshot returns the fixed-token ledger view (index path).
func (p *Pool) spendSnapshot(token int) spendView {
	return p.roster.spendSnapshot(token)
}

// bridgeSpendSnapshot returns the bridge entry's ledger view.
func (p *Pool) bridgeSpendSnapshot(entry *bridgeEntry) spendView {
	if entry == nil {
		return spendView{}
	}
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	return entry.ledger.spendSnapshot()
}

// ledgerView snapshots a ledger under its guard.
func ledgerView(l *spendLedger) spendView {
	if l == nil {
		return spendView{}
	}
	now := time.Now()
	return spendView{
		Rolling24h:   l.rolling24h(now),
		Day:          l.dayUsed,
		DayStart:     unixToTime(l.dayStart),
		Week:         l.weekUsed,
		WeekStart:    unixToTime(l.weekStart),
		Month:        l.monthUsed,
		MonthStart:   unixToTime(l.monthStart),
		SpendLimited: l.spendLimited,
	}
}

// recordSpendLimited marks one upstream spend_limited refusal on the entry
// at token's ledger (issue #122). The roster's single mutex guards it.
func (p *Pool) recordSpendLimited(token int) {
	p.roster.recordSpendLimited(token)
}

// bridgeRecordSpendLimited marks one upstream spend_limited refusal on a
// bridge entry's ledger (issue #122). Caller holds Pool.bridgeMu.
func (p *Pool) bridgeRecordSpendLimited(entry *bridgeEntry) {
	if entry == nil {
		return
	}
	entry.ledger.recordSpendLimited()
}

func unixToTime(sec int64) time.Time { return time.Unix(sec, 0) }
