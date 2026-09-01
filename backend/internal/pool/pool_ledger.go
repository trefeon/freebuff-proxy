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
