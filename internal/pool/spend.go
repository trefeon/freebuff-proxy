package pool

// Local per-token spend ledger (issue #87): an in-memory rolling 24h token
// spend window plus Pacific day/week/month buckets with rollover (issue
// #122: all period boundaries are America/Los_Angeles wall-clock — 00:00
// Pacific, Monday 00:00 Pacific, 1st 00:00 Pacific — resolving to
// 07:00/08:00 UTC by DST, matching the CLI's getZonedDayBounds /
// getZonedWeekBounds, reference/freebuff/common/src/util/zoned-time.ts:78-92,
// and the upstream wire periods pacific_day / pacific_week). Updated from
// chat usage (pool.RecordSpend, fed by the server's parsed usage blocks) and
// surfaced next to Messages24h in the healthz token snapshot.

import (
	"sync"
	"time"
)

// pacificLoc returns the cached America/Los_Angeles location, or nil when
// the IANA tzdata database is unavailable. time.LoadLocation consults
// ZONEINFO, the system zoneinfo, $GOROOT/lib/time/zoneinfo.zip, and the
// time/tzdata import in that order (go/src/time/zoneinfo.go LoadLocation);
// a stripped single-binary deployment with none of those gets nil and the
// callers fall back to bucketStartFallback, mirroring
// upstream.pacificMidnightFallback.
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
// the zone rules (go/src/time/time.go Date), so no fixed UTC offset is used.
func bucketStart(now time.Time, period string) int64 {
	loc := pacificLoc()
	if loc == nil {
		return bucketStartFallback(now, period)
	}
	n := now.In(loc)
	switch period {
	case "week":
		days := (int(n.Weekday()) + 6) % 7 // Monday=0 … Sunday=6
		return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc).
			AddDate(0, 0, -days).UTC().Unix()
	case "month":
		return time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc).UTC().Unix()
	default: // day
		return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc).UTC().Unix()
	}
}

// bucketStartFallback approximates bucketStart without the IANA tzdata
// database: America/Los_Angeles is UTC-7 during PDT (roughly March-
// November) and UTC-8 during PST (roughly November-March). The month range
// is the documented approximation; the exact DST transition dates require
// tzdata (mirrors upstream.pacificMidnightFallback). The "start after now"
// guard keeps the result ≤ now, as bucketStart guarantees.
func bucketStartFallback(now time.Time, period string) int64 {
	u := now.UTC()
	hour := 7 // PDT
	if u.Month() < time.March || u.Month() > time.November {
		hour = 8 // PST: December, January, February
	}
	switch period {
	case "week":
		weekday := int(u.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		start := time.Date(u.Year(), u.Month(), u.Day(), hour, 0, 0, 0, time.UTC).
			AddDate(0, 0, -(weekday - 1))
		if start.After(u) {
			start = start.AddDate(0, 0, -7)
		}
		return start.Unix()
	case "month":
		start := time.Date(u.Year(), u.Month(), 1, hour, 0, 0, 0, time.UTC)
		if start.After(u) {
			start = start.AddDate(0, -1, 0)
		}
		return start.Unix()
	default: // day
		start := time.Date(u.Year(), u.Month(), u.Day(), hour, 0, 0, 0, time.UTC)
		if start.After(u) {
			start = start.AddDate(0, 0, -1)
		}
		return start.Unix()
	}
}

// needsRollover reports whether a bucket whose window started at start has
// closed by now. The window ends at the next Pacific-wall-clock period
// boundary: midnight+1 day, Monday+7 days, 1st+1 month — calendar arithmetic
// via AddDate, which shifts the UTC instant across DST transitions exactly
// like the CLI's getZonedDayBounds / getZonedWeekBounds
// (reference/freebuff/common/src/util/zoned-time.ts:78-92; AddDate
// normalizes like Date, go/src/time/time.go). Without tzdata the fallback
// buckets are fixed-hour (07:00/08:00 UTC), so 24h windows are exact there.
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
		end = end.AddDate(0, 0, 7)
	case "month":
		end = end.AddDate(0, 1, 0)
	default:
		end = end.AddDate(0, 0, 1)
	}
	return !now.Before(end)
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

// recordSpend adds tokens to token's ledger (fixed-token index path).
func (p *Pool) recordSpend(token int, tokens int64) {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return
	}
	p.spendMu.Lock()
	defer p.spendMu.Unlock()
	if token < 0 || token >= len(p.spendPerToken) {
		return
	}
	p.spendPerToken[token].add(tokens, time.Now())
}

// recordSpendEntry adds tokens to the lease's backing entry's ledger by
// pointer (mirrors recordChatEntry: after a concurrent RemoveLastToken+
// AddToken, the lease's Token index may target a different token).
func (p *Pool) recordSpendEntry(entry *tokenEntry, tokens int64) {
	if entry == nil {
		return
	}
	p.spendMu.Lock()
	defer p.spendMu.Unlock()
	for idx, tok := range *p.toks.Load() {
		if tok != entry {
			continue
		}
		if idx < 0 || idx >= len(p.spendPerToken) {
			return
		}
		p.spendPerToken[idx].add(tokens, time.Now())
		return
	}
}

// bridgeRecordSpend adds tokens to a bridge entry's ledger.
func (p *Pool) bridgeRecordSpend(entry *bridgeEntry, tokens int64) {
	if entry == nil {
		return
	}
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	entry.spend.add(tokens, time.Now())
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
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return spendView{}
	}
	p.spendMu.Lock()
	defer p.spendMu.Unlock()
	if token < 0 || token >= len(p.spendPerToken) {
		return spendView{}
	}
	return ledgerView(p.spendPerToken[token])
}

// bridgeSpendSnapshot returns the bridge entry's ledger view.
func (p *Pool) bridgeSpendSnapshot(entry *bridgeEntry) spendView {
	if entry == nil {
		return spendView{}
	}
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	return ledgerView(entry.spend)
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

// recordSpendLimited marks one upstream spend_limited refusal on the
// fixed-token ledger (issue #122). Caller holds Pool.spendMu.
func (p *Pool) recordSpendLimited(token int) {
	if token < 0 || token >= len(p.spendPerToken) {
		return
	}
	p.spendPerToken[token].spendLimited++
}

// bridgeRecordSpendLimited marks one upstream spend_limited refusal on a
// bridge entry's ledger (issue #122). Caller holds Pool.bridgeMu.
func (p *Pool) bridgeRecordSpendLimited(entry *bridgeEntry) {
	if entry == nil {
		return
	}
	entry.spend.spendLimited++
}

func unixToTime(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}
