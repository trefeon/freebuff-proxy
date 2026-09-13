package pool

import "time"

// AccountLedger is one token entry's usage + spend state. It is embedded in
// both tokenEntry and bridgeEntry so the two modes share one ownership model
// (issue #263): a pooled entry's ledger is guarded by the pool's tokenRoster
// mutex, a bridge entry's ledger by Pool.bridgeMu. The methods assume the
// caller holds the owning subsystem's single lock.
type AccountLedger struct {
	usage []time.Time // rolling 24h successful-chat timestamps (messages_24h display)
	spend *spendLedger
	// reqDayStart / reqDayCount track successful chat requests in the
	// current Pacific day: the bucket rolls at Pacific midnight — the same
	// instant upstream resets its daily quota windows. Fed by recordChat
	// paths; read by the dashboard's per-day display and the maturity
	// client-active skip. Upstream quota/429 is the enforcement.
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
