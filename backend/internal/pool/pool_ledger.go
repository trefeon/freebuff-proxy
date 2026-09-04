package pool

import "time"

// AccountLedger is one token entry's usage + spend state. It is embedded in
// both tokenEntry and bridgeEntry so the two modes share one ownership model
// (issue #263): a pooled entry's ledger is guarded by the pool's tokenRoster
// mutex, a bridge entry's ledger by Pool.bridgeMu. The methods assume the
// caller holds the owning subsystem's single lock.
type AccountLedger struct {
	usage []time.Time // rolling 24h successful-chat timestamps (MAX_MESSAGES_PER_DAY)
	spend *spendLedger
	// requests is the rolling 60s window of ADMITTED chat requests
	// (MAX_REQUESTS_PER_MINUTE): appended at lease grant, before the
	// upstream call, so upstream-visible bursts — including retries that
	// later fail — are throttled at the exact rate upstream observes.
	requests []time.Time
	// reqDayStart / reqDayCount track successful chat requests in the
	// current Pacific day (MAX_REQUESTS_PER_DAY): the bucket rolls at
	// Pacific midnight — the same instant upstream resets its daily quota
	// windows — so locked tokens unlock in sync with the official reset.
	reqDayStart int64
	reqDayCount int64
}

// newAccountLedger returns a fresh ledger with an empty usage window and a
// zero spend ledger.
func newAccountLedger() *AccountLedger {
	return &AccountLedger{spend: newSpendLedger()}
}

// recordChat appends one successful upstream chat at now and prunes the
// usage history outside the 24h window.
func (l *AccountLedger) recordChat(now time.Time) {
	cutoff := now.Add(-usageWindow)
	history := l.usage
	first := 0
	for first < len(history) && history[first].Before(cutoff) {
		first++
	}
	l.usage = append(history[first:], now)
}

// usageCount returns how many successful chats fall within the last
// usageWindow as of now, pruning expired timestamps.
func (l *AccountLedger) usageCount(now time.Time) int {
	cutoff := now.Add(-usageWindow)
	history := l.usage
	first := 0
	for first < len(history) && history[first].Before(cutoff) {
		first++
	}
	l.usage = history[first:]
	return len(l.usage)
}

// usageResetIn is how long until the oldest usage timestamp ages out of the
// window as of now (0 when no usage is recorded or the reset is due).
func (l *AccountLedger) usageResetIn(now time.Time) time.Duration {
	if len(l.usage) == 0 {
		return 0
	}
	reset := l.usage[0].Add(usageWindow).Sub(now)
	if reset < 0 {
		return 0
	}
	return reset
}

// recordSpend adds tokens to the ledger's spend bucket as of now.
func (l *AccountLedger) recordSpend(tokens int64, now time.Time) {
	l.spend.add(tokens, now)
}

// spendSnapshot snapshots the ledger's spend view as of now.
func (l *AccountLedger) spendSnapshot() spendView {
	return ledgerView(l.spend)
}

// recordSpendLimited marks one upstream spend_limited refusal.
func (l *AccountLedger) recordSpendLimited() {
	l.spend.spendLimited++
}

// recordRequest appends one admitted chat request at now and prunes the
// RPM window outside rpmWindow. Called when a lease is granted — success or
// failure upstream is irrelevant, the request was sent.
func (l *AccountLedger) recordRequest(now time.Time) {
	cutoff := now.Add(-rpmWindow)
	history := l.requests
	first := 0
	for first < len(history) && history[first].Before(cutoff) {
		first++
	}
	l.requests = append(history[first:], now)
}

// rpmCount returns how many admitted requests fall within the last rpmWindow
// as of now, pruning expired timestamps.
func (l *AccountLedger) rpmCount(now time.Time) int {
	cutoff := now.Add(-rpmWindow)
	history := l.requests
	first := 0
	for first < len(history) && history[first].Before(cutoff) {
		first++
	}
	l.requests = history[first:]
	return len(l.requests)
}

// rpmResetIn is how long until the oldest admitted request ages out of the
// RPM window as of now (0 when the window is empty or already expired).
func (l *AccountLedger) rpmResetIn(now time.Time) time.Duration {
	if len(l.requests) == 0 {
		return 0
	}
	reset := l.requests[0].Add(rpmWindow).Sub(now)
	if reset < 0 {
		return 0
	}
	return reset
}

// recordDayRequest counts one successful chat request in the current
// Pacific day, rolling the bucket at Pacific midnight (bucketStart
// semantics, DST-correct — the upstream official daily reset).
func (l *AccountLedger) recordDayRequest(now time.Time) {
	if start := bucketStart(now, "day"); start != l.reqDayStart {
		l.reqDayStart = start
		l.reqDayCount = 0
	}
	l.reqDayCount++
}

// dayRequestCount returns the successful-request count for the current
// Pacific day, rolling the bucket if the day turned over.
func (l *AccountLedger) dayRequestCount(now time.Time) int {
	if start := bucketStart(now, "day"); start != l.reqDayStart {
		l.reqDayStart = start
		l.reqDayCount = 0
	}
	return int(l.reqDayCount)
}

// dayRequestResetIn is how long until the next Pacific midnight (the
// official daily quota reset instant) as of now.
func (l *AccountLedger) dayRequestResetIn(now time.Time) time.Duration {
	dayStart := time.Unix(bucketStart(now, "day"), 0)
	return time.Until(nextPacificMidnight(dayStart))
}
