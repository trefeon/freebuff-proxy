// route_smart.go — step-1 smart pool routing: per-token live-turn slot
// semaphore with a FIFO waiter queue plus the unified scorer with a smooth
// weighted pick, behind the ROUTING_SMART master switch.
//
// Shape (approved step-1):
//   - TOKEN_MAX_CONCURRENT (default 2, floor 1) is a hard wall per account
//     on live turns: a lease is granted only while the token holds fewer
//     live turns than the cap; LeaseRelease/LeaseAbandon returns the slot
//     and wakes the FIFO head.
//   - Per-token FIFO wait queue: waiters park with the caller ctx plus the
//     QUEUE_WAIT (default 30s) deadline and the QUEUE_DEPTH (default 16)
//     cap. Overflow and timeout return the typed queue-exhausted signal
//     below, which the failover loop maps to the existing 429 rate-limit
//     shape — never a new client error code.
//   - Unified scorer over eligible tokens with the approved weights:
//     drain-only affinity +1000, hot session +200, warm model +50, 429
//     backoff -200, transient error -50 decaying, consecutive-turn
//     anti-clump -25, idle-longest tiebreak. Disqualification mirrors the
//     legacy skip gates (locked/banned/cooldown/cap-hit). A token with no
//     free live-turn slot is NOT hard-disqualified: it sorts behind free
//     tokens but stays waitable, so a single-token pool queues instead of
//     429ing (the racing-acquire contract: the third acquire waits, a
//     third live turn never exists).
//   - TOKEN_ROTATION strategies keep today's order in step 1: the scorer
//     re-ranks only the drain strategy (affinity is drain-only); any other
//     strategy returns the legacy acquireOrder output untouched. Strategy
//     reduction lands in step 2.
//   - ROUTING_SMART off == the legacy path: the order is unmodified and
//     the loop's slot/tracking hooks are skipped, so observable behavior
//     is byte-identical.
//
// Slot/queue/scorer state is in-memory only and resets to zero on restart
// (same discipline as the probe scheduler's transient kick/inflight flags):
// it rides no pool_state rows and invents no SQL.
package pool

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/upstream"
)

// Approved step-1 scorer weights.
const (
	// routeWeightAffinityDrain pins drain stickiness: the token that last
	// served the model keeps serving it (exhaust one account before
	// rotating). Drain-only; other strategies keep today's order.
	routeWeightAffinityDrain = 1000
	// routeWeightHotSession reuses a live session for the model instead
	// of admitting a new one (mirrors the drain matching-hot tier).
	routeWeightHotSession = 200
	// routeWeightWarmModel prefers the token that last served the model.
	routeWeightWarmModel = 50
	// routeWeightBackoff429 de-prefers a token carrying a remembered 429
	// that is still eligible (a different model's quota exemption kept it
	// in rotation). Cooling tokens are disqualified outright.
	routeWeightBackoff429 = -200
	// routeWeightTransient is the transport-transient penalty at full
	// strength (inside routeTransientFullWindow); it steps down inside
	// routeTransientHalfWindow and expires after it.
	routeWeightTransient = -50
	// routeWeightAntiClump spreads immediate repeats: the token that
	// served the previous turn (any model) yields slightly.
	routeWeightAntiClump = -25
)

// routeTransientFullWindow is how long a transport-transient failure counts
// at full strength; routeTransientHalfWindow bounds the stepped decay
// (half strength inside, expired outside).
const (
	routeTransientFullWindow = time.Minute
	routeTransientHalfWindow = 5 * time.Minute
)

// routeQueueHint is the Retry-After hint carried by queue-exhausted 429s: a
// minimal local backoff (failover to a free token is the first resort, so
// the hint only paces single-token pools).
const routeQueueHint = time.Second

// routeQueueExhaustedError is the typed queue-exhausted signal: a token's
// live-turn slots were full and the waiter either found a full queue
// (reason "full") or ran out of QUEUE_WAIT (reason "timeout"). The failover
// loop maps it to the existing 429 rate-limit shape; it never reaches a
// client verbatim.
type routeQueueExhaustedError struct {
	Reason string // "full" or "timeout"
	Token  int    // 1-based display index
	Cap    int    // live-turn cap in force
	Live   int    // live turns observed
	Wait   time.Duration
}

func (e *routeQueueExhaustedError) Error() string {
	if e.Reason == "timeout" {
		return fmt.Sprintf("pool: token-%d live-turn queue wait (%s) elapsed with %d live turns (cap %d)", e.Token, e.Wait, e.Live, e.Cap)
	}
	return fmt.Sprintf("pool: token-%d live-turn queue full (%d live turns, cap %d)", e.Token, e.Live, e.Cap)
}

// routeSlotParams resolves the live slot cap, queue depth and wait bound
// for one acquire. A nil config yields the documented defaults; the loader
// floors TOKEN_MAX_CONCURRENT at 1 and rejects negative QUEUE_DEPTH, so the
// defensive branches below only fire for hand-built configs that bypass
// Load (unit tests).
func routeSlotParams(cfg *config.Config) (cap, depth int, wait time.Duration) {
	cap, depth, wait = 2, 16, 30*time.Second
	if cfg == nil {
		return cap, depth, wait
	}
	if cfg.TokenMaxConcurrent >= 1 {
		cap = cfg.TokenMaxConcurrent
	} else {
		cap = 1
	}
	if cfg.QueueDepth >= 0 {
		depth = cfg.QueueDepth
	}
	if cfg.QueueWait > 0 {
		wait = cfg.QueueWait
	}
	return cap, depth, wait
}

// routeSlotWaiter is one parked FIFO waiter. ch is closed exactly once on
// grant (under Pool.routeMu); granted is set in the same critical section
// so a concurrent timeout/ctx-expiry either takes the grant or dequeues,
// never both and never neither.
type routeSlotWaiter struct {
	ch      chan struct{}
	granted bool
	element *list.Element
}

// routeSlotState is one token's live-turn counter plus its FIFO waiter
// queue (arrival order = grant order).
type routeSlotState struct {
	live    int
	waiters *list.List // of *routeSlotWaiter, front = head
}

// routeSlotPermit is one held live-turn slot. Release returns it: with a
// non-empty queue the slot transfers directly to the FIFO head (the live
// count is unchanged — a third live turn never exists); otherwise the live
// count decrements. Nil-safe (legacy/off-path and synthetic leases carry
// no permit) and idempotent.
type routeSlotPermit struct {
	pool     *Pool
	entry    *tokenEntry
	released atomic.Bool
}

// Release returns the live-turn slot, waking the FIFO head when waiters
// park. Nil-safe and idempotent.
func (s *routeSlotPermit) Release() {
	if s == nil || s.pool == nil || !s.released.CompareAndSwap(false, true) {
		return
	}
	p := s.pool
	p.routeMu.Lock()
	defer p.routeMu.Unlock()
	st, ok := p.routeSlots[s.entry]
	if !ok || st == nil {
		return
	}
	if front := st.waiters.Front(); front != nil {
		w := front.Value.(*routeSlotWaiter)
		st.waiters.Remove(front)
		w.granted = true
		close(w.ch)
		return
	}
	if st.live > 0 {
		st.live--
	}
	if st.live == 0 {
		delete(p.routeSlots, s.entry)
	}
}

// routeSlotStateLocked returns the entry's slot state, creating it. Caller
// holds p.routeMu.
func (p *Pool) routeSlotStateLocked(entry *tokenEntry) *routeSlotState {
	if p.routeSlots == nil {
		p.routeSlots = make(map[*tokenEntry]*routeSlotState)
	}
	st, ok := p.routeSlots[entry]
	if !ok || st == nil {
		st = &routeSlotState{waiters: list.New()}
		p.routeSlots[entry] = st
	}
	if st.waiters == nil {
		st.waiters = list.New()
	}
	return st
}

// routeSlotAcquire takes one live-turn slot for entry, parking FIFO when
// full. The fast path (free slot) grants immediately; otherwise the caller
// queues behind earlier waiters until the head is granted, the caller ctx
// expires, or wait elapses. It returns the permit, whether the caller
// parked, and either a *routeQueueExhaustedError (full queue or wait
// elapsed — the caller maps it to the existing 429 shape) or ctx.Err()
// (the caller's own deadline, matching today's gate behavior).
func (p *Pool) routeSlotAcquire(ctx context.Context, entry *tokenEntry, displayIdx int, cap, depth int, wait time.Duration) (*routeSlotPermit, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cap < 1 {
		cap = 1
	}
	p.routeMu.Lock()
	st := p.routeSlotStateLocked(entry)
	if st.live < cap {
		st.live++
		p.routeMu.Unlock()
		return &routeSlotPermit{pool: p, entry: entry}, false, nil
	}
	if depth <= 0 || st.waiters.Len() >= depth {
		qerr := &routeQueueExhaustedError{Reason: "full", Token: displayIdx, Cap: cap, Live: st.live}
		p.routeMu.Unlock()
		return nil, false, qerr
	}
	w := &routeSlotWaiter{ch: make(chan struct{})}
	w.element = st.waiters.PushBack(w)
	live := st.live
	p.routeMu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-w.ch:
		return &routeSlotPermit{pool: p, entry: entry}, true, nil
	case <-ctx.Done():
		p.routeMu.Lock()
		if w.granted {
			p.routeMu.Unlock()
			return &routeSlotPermit{pool: p, entry: entry}, true, nil
		}
		if w.element != nil {
			st.waiters.Remove(w.element)
			w.element = nil
		}
		p.routeMu.Unlock()
		return nil, true, ctx.Err()
	case <-timer.C:
		p.routeMu.Lock()
		if w.granted {
			p.routeMu.Unlock()
			return &routeSlotPermit{pool: p, entry: entry}, true, nil
		}
		if w.element != nil {
			st.waiters.Remove(w.element)
			w.element = nil
		}
		p.routeMu.Unlock()
		return nil, true, &routeQueueExhaustedError{Reason: "timeout", Token: displayIdx, Cap: cap, Live: live, Wait: wait}
	}
}

// routeSlotLive reports the entry's current live-turn count (tests and the
// scorer's free-slot partition).
func (p *Pool) routeSlotLive(entry *tokenEntry) int {
	p.routeMu.Lock()
	defer p.routeMu.Unlock()
	if st, ok := p.routeSlots[entry]; ok && st != nil {
		return st.live
	}
	return 0
}

// routeSlotQueued reports the entry's parked waiter count (tests).
func (p *Pool) routeSlotQueued(entry *tokenEntry) int {
	p.routeMu.Lock()
	defer p.routeMu.Unlock()
	if st, ok := p.routeSlots[entry]; ok && st != nil && st.waiters != nil {
		return st.waiters.Len()
	}
	return 0
}

// unclassified reports whether an admission failure carried none of the
// typed refusal signals (no auth/rate-limit/ip-cap/ban/country/model-limit
// marker): the shape of a transport-transient failure worth remembering
// for the decaying scorer penalty.
func (c *classifiedError) unclassified() bool {
	return c != nil && !c.authRejected && c.rateLimited == nil && c.ipCapped == nil &&
		c.banned == nil && c.countryBlocked == nil && c.limitedIp == nil && !c.spendLimited
}

// routeNoteTransient records one transport-transient failure for entry. The
// scorer penalty steps down with age (full inside
// routeTransientFullWindow, half inside routeTransientHalfWindow).
func (p *Pool) routeNoteTransient(entry *tokenEntry) {
	if entry == nil {
		return
	}
	entry.routeTransientAt.Store(time.Now().UnixNano())
	entry.routeTransientCount.Add(1)
}

// routeTransientPenalty returns the decaying transient penalty for entry:
// full strength inside routeTransientFullWindow, half strength inside
// routeTransientHalfWindow, zero afterwards (or when nothing was recorded).
func routeTransientPenalty(entry *tokenEntry, now time.Time) int {
	if entry == nil || entry.routeTransientCount.Load() == 0 {
		return 0
	}
	at := entry.routeTransientAt.Load()
	if at == 0 {
		return 0
	}
	switch age := now.Sub(time.Unix(0, at)); {
	case age < 0:
		return routeWeightTransient
	case age < routeTransientFullWindow:
		return routeWeightTransient
	case age < routeTransientHalfWindow:
		return routeWeightTransient / 2
	default:
		return 0
	}
}

// routeNoteGranted records a smart-path lease grant for the idle-longest
// tiebreak and the consecutive-turn anti-clump signal.
func (p *Pool) routeNoteGranted(entry *tokenEntry) {
	if entry == nil {
		return
	}
	entry.routeLastLease.Store(time.Now().UnixNano())
	p.routeMu.Lock()
	p.routePrev = entry
	p.routeMu.Unlock()
}

// routeDrainAffinity reports whether the scorer's +1000 affinity applies:
// drain strategy only (empty rotation normalizes to drain, mirroring
// acquireOrder's default).
func routeDrainAffinity(cfg *config.Config) bool {
	if cfg == nil || cfg.TokenRotation == "" {
		return true
	}
	return cfg.TokenRotation == "drain"
}

// routeScore computes one candidate's smart-routing score for model.
// eligible=false disqualifies the token (locked, quarantined/banned,
// cooling, or cap-hit — mirroring the legacy failover skip gates); reason
// names the disqualifier for logs. A token with no free live-turn slot
// stays eligible (it sorts behind free tokens in routeSmartRank but remains
// waitable, so a single-token pool queues instead of 429ing).
func (p *Pool) routeScore(cfg *config.Config, toks *[]*tokenEntry, idx int, model string, now time.Time) (score int, eligible bool, reason string) {
	if toks == nil || idx < 0 || idx >= len(*toks) {
		return 0, false, "out of range"
	}
	tok := (*toks)[idx]
	if tok.locked.Load() {
		return 0, false, "locked"
	}
	if q := tok.quarantine.Load(); q != nil && !p.clearLiftedQuarantine(tok) {
		return 0, false, "quarantined"
	}
	if until := tok.runs.CooldownUntil(); now.Before(until) || tok.runs.BanError() != nil {
		// Per-model quota exemption (mirrors the failover loop): a
		// cooldown caused by a DIFFERENT model's quota exhaustion still
		// leaves the token eligible for this request.
		if rle := tok.runs.RateLimitError(); rle != nil && rle.Model != "" && rle.Model != model && isQuotaExhaustedError(rle) {
			score += routeWeightBackoff429
		} else {
			return 0, false, "cooldown"
		}
	} else if rle := tok.runs.RateLimitError(); rle != nil {
		score += routeWeightBackoff429
	}
	if lockedOutByModel(cfg, p.reg, idx, model) {
		return 0, false, "model allowlist"
	}
	if cfg != nil {
		if cfg.MaxMessagesPerDay > 0 && p.usageCount(idx) >= cfg.MaxMessagesPerDay {
			return 0, false, "daily message cap"
		}
		if cfg.MaxRequestsPerMinute > 0 && p.rpmCount(idx) >= cfg.MaxRequestsPerMinute {
			return 0, false, "per-minute request cap"
		}
		if cfg.MaxRequestsPerDay > 0 && p.dayRequestCount(idx) >= cfg.MaxRequestsPerDay {
			return 0, false, "daily request cap"
		}
	}
	p.lastTokenMu.Lock()
	lastUsed, hasLastUsed := p.lastTokenByModel[model]
	p.lastTokenMu.Unlock()
	p.admissionsMu.Lock()
	admittingToken, isAdmitting := p.admissions[model]
	p.admissionsMu.Unlock()

	// Affinity (+1000, drain-only) and warm (+50) reinforce HOT reuse
	// only: they pin the drain "exhaust this account's session before
	// rotating" stickiness (mirroring the matching-hot tier's last-used
	// preference). A cold token never earns them, so cold rotation falls
	// through to anti-clump + smooth + base position — exactly today's
	// round-robin start order (a global last-used bonus would otherwise
	// re-pin the previous token after every session invalidation).
	snap := tok.session.Snapshot()
	isAdm := isAdmitting && idx == admittingToken
	hotForModel := (snap.Usable() || snap.Refreshing || isAdm) && (snap.MatchesModel(model) || isAdm)
	if hotForModel {
		score += routeWeightHotSession
		if hasLastUsed && idx == lastUsed {
			if routeDrainAffinity(cfg) {
				score += routeWeightAffinityDrain
			}
			score += routeWeightWarmModel
		}
	}
	score += routeTransientPenalty(tok, now)
	p.routeMu.Lock()
	prev := p.routePrev
	p.routeMu.Unlock()
	if prev != nil && prev == tok {
		score += routeWeightAntiClump
	}
	return score, true, ""
}

// routeCand is one scored smart-routing candidate.
type routeCand struct {
	idx     int
	entry   *tokenEntry
	score   int
	weight  int64
	current int64
	idle    int64
	basePos int
	free    bool
}

// routeSmartRank re-ranks the legacy base order for the smart path. Only
// the drain strategy is re-ranked (affinity is drain-only); every other
// strategy returns base untouched so round_robin/least_used/random behave
// exactly as today. Disqualified tokens (locked/banned/cooldown/cap-hit)
// are dropped — when none survive, base is returned so the legacy loop
// still visits every token and records the honest error buckets. Tokens
// with no free live-turn slot sort behind free ones but stay waitable. The
// head is a smooth weighted pick (accumulators ride the entries, so the
// smoothing survives dashboard reorders); the tail follows score, then
// idle-longest, then base position (under no pressure every score ties and
// the output equals the drain base order).
func (p *Pool) routeSmartRank(cfg *config.Config, toks *[]*tokenEntry, base []int, model string) []int {
	if !routeDrainAffinity(cfg) || len(base) == 0 {
		return base
	}
	now := time.Now()
	capN, _, _ := routeSlotParams(cfg)
	cands := make([]routeCand, 0, len(base))
	minScore := 0
	for pos, idx := range base {
		if toks == nil || idx < 0 || idx >= len(*toks) {
			continue
		}
		score, eligible, _ := p.routeScore(cfg, toks, idx, model, now)
		if !eligible {
			continue
		}
		if len(cands) == 0 || score < minScore {
			minScore = score
		}
		cands = append(cands, routeCand{
			idx:     idx,
			entry:   (*toks)[idx],
			score:   score,
			idle:    (*toks)[idx].routeLastLease.Load(),
			basePos: pos,
			free:    p.routeSlotLive((*toks)[idx]) < capN,
		})
	}
	if len(cands) == 0 {
		// Every token disqualified: degrade to the legacy order so the
		// loop still visits each token and records the honest error
		// buckets (ban > rate-limit > waiting > daily cap).
		return base
	}
	// Free-slot partition: when at least one candidate has a free slot,
	// the smooth pick runs over the free set; full tokens still follow in
	// the tail so a failover can wait on them. The free set is a fresh
	// slice (filtering cands in place would alias its backing array).
	pool := cands
	hasFree := false
	for _, c := range cands {
		if c.free {
			hasFree = true
			break
		}
	}
	if hasFree {
		free := make([]routeCand, 0, len(cands))
		for _, c := range cands {
			if c.free {
				free = append(free, c)
			}
		}
		pool = free
	}
	var total int64
	for i := range pool {
		pool[i].weight = int64(pool[i].score-minScore) + 1
		total += pool[i].weight
	}
	winner := 0
	for i := range pool {
		pool[i].current = pool[i].entry.routeSmooth.Add(pool[i].weight)
		if i == 0 {
			continue
		}
		a, b := pool[i], pool[winner]
		if a.current > b.current ||
			(a.current == b.current && (a.idle < b.idle ||
				(a.idle == b.idle && a.basePos < b.basePos))) {
			winner = i
		}
	}
	pool[winner].entry.routeSmooth.Add(-total)
	head := pool[winner]
	rest := make([]routeCand, 0, len(cands)-1)
	for _, c := range cands {
		if c.idx == head.idx && c.entry == head.entry {
			continue
		}
		rest = append(rest, c)
	}
	// Tail: free first, then score, then idle-longest, then base position.
	for i := 1; i < len(rest); i++ {
		for j := i; j > 0; j-- {
			a, b := rest[j], rest[j-1]
			swap := false
			switch {
			case a.free != b.free:
				swap = a.free
			case a.score != b.score:
				swap = a.score > b.score
			case a.idle != b.idle:
				swap = a.idle < b.idle
			default:
				swap = a.basePos < b.basePos
			}
			if !swap {
				break
			}
			rest[j], rest[j-1] = rest[j-1], rest[j]
		}
	}
	order := make([]int, 0, len(cands))
	order = append(order, head.idx)
	for _, c := range rest {
		order = append(order, c.idx)
	}
	return order
}

// routeQueueRateLimit maps a queue-exhausted signal to the existing 429
// rate-limit shape (failover bucket): same code the pool surfaces for its
// per-minute cap, with the local queue hint as Retry-After.
func routeQueueRateLimit(qerr *routeQueueExhaustedError, model string, cap int, live int) *upstream.RateLimitError {
	body := ""
	if qerr != nil {
		body = qerr.Error()
	}
	return &upstream.RateLimitError{
		Status:      "rate_limited",
		Model:       model,
		RetryAfter:  routeQueueHint,
		Limit:       float64(cap),
		RecentCount: float64(live),
		Body:        body,
	}
}

// routeIsQueueExhausted reports whether err is the typed queue-exhausted
// signal (overflow or wait timeout), as opposed to the caller's own ctx
// expiry.
func routeIsQueueExhausted(err error) bool {
	var qerr *routeQueueExhaustedError
	return errors.As(err, &qerr)
}
