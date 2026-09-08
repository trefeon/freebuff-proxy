package pool

import (
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/upstream"
	"math/rand/v2"
	"sort"
	"time"
)

// acquireOrder computes the token iteration order for one Acquire pass
// (hot-session-first selection, see Acquire). toks is the caller's snapshot
// (loaded once in Acquire) — the order is built against the same snapshot
// the failover loop indexes, so an AddToken racing the call can never make
// the loop index past its own snapshot. start is the round-robin start
// index; model is the requested upstream model.
//
// ADR-0027: the pool no longer mirrors session-count quotas. Cached
// QuotaByModel counts (limits, recent counts, resets) are envelope data
// only — every token is attempted and upstream refusals surface as honest
// errors. Within the hot set, tokens with the smallest Freebucks balance
// for the requested model rank first (drain the thinnest allowance).
// See the sort below.
//
// Issue #164 (fallback ordering): the order returned here exhausts EVERY
// eligible token for the requested model before the caller's
// QUOTA_FALLBACK_MODELS fallback can fire. Non-capped tokens (matching hot,
// cold, mismatched hot) are all in the order and the failover loop in
// Acquire visits each of them in turn — a token only fails the pass with a
// quota-exhausted error after it was actually attempted. Freebucks-capped
// tokens are excluded (their allowance cannot cover the price), and when
// every token is capped or cooling down the order degrades to full
// round-robin so the loop still records every reason. The fallback branch
// in Acquire only runs after that loop completed without a lease and every
// rate-limited error it recorded is a quota exhaustion.
func (p *Pool) acquireOrder(toks *[]*tokenEntry, start int, model string) ([]int, []*upstream.RateLimitError) {
	// eligible mirrors the per-token checks the failover loop applies:
	// not cooling down, under the daily message cap, and not
	// Freebucks-capped for the requested model (ADR-0027: session-count
	// caps are gone). It never records the exclusion reasons — the caller
	// does that in one place.
	eligible := func(idx int) bool {
		tok := (*toks)[idx]
		// Administratively locked tokens are never eligible for leasing.
		if tok.locked.Load() {
			return false
		}
		// Model-allowlist routing (MODEL_LOCKS, issue #325): slots locked
		// to other models are skipped for this request (as if unavailable),
		// never demoted or punished. Unlocked slots serve anything.
		if lockedOutByModel(p.cfg.Load(), p.reg, idx, model) {
			tok.allowlistSkips.Add(1)
			return false
		}
		if capped, _ := freebucksCapped(tok, model); capped {
			return false
		}
		return true
	}

	p.lastTokenMu.Lock()
	lastUsedToken, hasLastUsed := p.lastTokenByModel[model]
	p.lastTokenMu.Unlock()

	p.admissionsMu.Lock()
	admittingToken, isAdmitting := p.admissions[model]
	p.admissionsMu.Unlock()

	// Issue #155 & #191: Model Stickiness and Session Preservation:
	// 1. Matching Hot: tokens with active/usable session (or in-flight admission) for the model.
	// 2. Cold Tokens: tokens with no active session (fresh session, avoids switch penalty).
	// 3. Mismatched Hot: tokens with active session for a DIFFERENT model.
	var matchingHot []int
	var coldTokens []int
	var mismatchedHot []int

	for offset := 0; offset < len(*toks); offset++ {
		idx := (start + offset) % len(*toks)
		if !eligible(idx) {
			continue
		}
		tok := (*toks)[idx]
		snap := tok.session.Snapshot()
		isAdm := isAdmitting && idx == admittingToken
		hasLive := snap.Usable() || snap.Refreshing || isAdm

		if hasLive {
			if snap.MatchesModel(model) || isAdm {
				matchingHot = append(matchingHot, idx)
			} else {
				mismatchedHot = append(mismatchedHot, idx)
			}
		} else {
			coldTokens = append(coldTokens, idx)
		}
	}

	// Sort matchingHot:
	// 1. In-flight refreshing/admitting tokens rank first so concurrent requests park on single-flight refreshCh.
	sort.SliceStable(matchingHot, func(i, j int) bool {
		a, b := matchingHot[i], matchingHot[j]
		tokA, tokB := (*toks)[a], (*toks)[b]
		snapA, snapB := tokA.session.Snapshot(), tokB.session.Snapshot()

		isAdmA := snapA.Refreshing || (isAdmitting && a == admittingToken)
		isAdmB := snapB.Refreshing || (isAdmitting && b == admittingToken)
		if isAdmA != isAdmB {
			return isAdmA
		}

		// Freebucks ordering: drain smallest balance first (preserve
		// fuller Freebucks allowances). Only applies when both tokens
		// price the model.
		if snapA.Freebucks != nil && snapB.Freebucks != nil {
			if _, okA := snapA.Freebucks.Prices[model]; okA {
				if _, okB := snapB.Freebucks.Prices[model]; okB {
					if snapA.Freebucks.Balance != snapB.Freebucks.Balance {
						return snapA.Freebucks.Balance < snapB.Freebucks.Balance
					}
				}
			}
		}

		if hasLastUsed {
			if a == lastUsedToken && b != lastUsedToken {
				return true
			}
			if b == lastUsedToken && a != lastUsedToken {
				return false
			}
		}

		return a < b
	})
	rot := "drain"
	if c := p.cfg.Load(); c != nil && c.TokenRotation != "" {
		rot = c.TokenRotation
	}

	var order []int
	switch rot {
	case "round_robin":
		// Round-robin mode: visit all eligible tokens sequentially starting from 'start'
		for offset := 0; offset < len(*toks); offset++ {
			idx := (start + offset) % len(*toks)
			if eligible(idx) {
				order = append(order, idx)
			}
		}

	case "least_used":
		// Least-used mode: rank tokens with the LARGEST remaining quota first (preserve balance)
		order = leastUsedOrder(toks, model, eligible)

	case "random":
		// Random mode: stochastic shuffle among eligible tokens
		var eligibleTokens []int
		for idx := range *toks {
			if eligible(idx) {
				eligibleTokens = append(eligibleTokens, idx)
			}
		}
		if len(eligibleTokens) > 1 {
			p.randMu.Lock()
			if p.randGen == nil {
				p.randGen = rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 1))
			}
			p.randGen.Shuffle(len(eligibleTokens), func(i, j int) {
				eligibleTokens[i], eligibleTokens[j] = eligibleTokens[j], eligibleTokens[i]
			})
			p.randMu.Unlock()
		}
		order = eligibleTokens

	default: // "drain" (default)
		order = make([]int, 0, len(*toks))
		order = append(order, matchingHot...)
		order = append(order, coldTokens...)
		order = append(order, mismatchedHot...)
	}
	// Burst balance (ADR-0023, opt-in): while this model's sliding-window
	// admission count exceeds its threshold, THAT model's order switches to
	// least_used among healthy tokens, capped at BURST_MAX_TOKENS distinct
	// accounts. Disabled or below-threshold: order untouched (the default
	// path is byte-identical). The availability sort below still applies,
	// so demoted over-cap tokens are reached when the spread set fails.
	if cfg := p.cfg.Load(); cfg != nil && cfg.BurstBalanceEnabled {
		if plan := p.burstPlanForModel(cfg, model, time.Now()); plan.active {
			order = plan.apply(leastUsedOrder(toks, model, eligible))
		}
	}

	// Smart availability (rate-limit handling): demote tokens that are
	// cooling down or banned behind tokens that can serve right now, and
	// order the unavailable ones by earliest unblock so the failover loop
	// reaches an available account first and, when every token is limited,
	// the surfaced 429 carries the shortest retry. The per-model quota
	// exemption mirrors the failover loop (acquire.go): a cooldown caused
	// by a DIFFERENT model's quota cap still leaves this token available
	// for `model`.
	sort.SliceStable(order, func(i, j int) bool {
		ai := tokenAvailable((*toks)[order[i]], model)
		aj := tokenAvailable((*toks)[order[j]], model)
		if ai != aj {
			return ai
		}
		if !ai {
			ci := (*toks)[order[i]].runs.CooldownUntil()
			cj := (*toks)[order[j]].runs.CooldownUntil()
			if !ci.Equal(cj) {
				return ci.Before(cj)
			}
		}
		return false
	})

	if len(order) == 0 {
		// All tokens are cooling down or capped: fallback to round-robin
		// so the failover loop visits them and records their errors.
		order = make([]int, len(*toks))
		for i := range order {
			order[i] = (start + i) % len(*toks)
		}
		return order, nil
	}
	// The Freebucks-capped tokens excluded above are never visited by the
	// failover loop, so their rate-limit reasons must ride back with the
	// order: when every token is capped the pool surfaces a real 429 with
	// the earliest window reset instead of a generic combined error.
	inOrder := make(map[int]struct{}, len(order))
	for _, idx := range order {
		inOrder[idx] = struct{}{}
	}
	var quotaLimited []*upstream.RateLimitError
	for idx := range *toks {
		if _, ok := inOrder[idx]; ok {
			continue
		}
		if capped, _ := freebucksCapped((*toks)[idx], model); capped {
			quotaLimited = append(quotaLimited, freebucksLimitError((*toks)[idx], model))
		}
	}
	return order, quotaLimited
}

// leastUsedOrder ranks eligible token indexes with the LARGEST Freebucks
// balance first (preserve balance; ADR-0027: session-count remaining is
// envelope data, never ranking input). Shared by the "least_used" strategy
// and the burst-balance override (ADR-0023) so the two rankings cannot
// drift.
func leastUsedOrder(toks *[]*tokenEntry, model string, eligible func(int) bool) []int {
	var eligibleTokens []int
	for idx := range *toks {
		if eligible(idx) {
			eligibleTokens = append(eligibleTokens, idx)
		}
	}
	sort.SliceStable(eligibleTokens, func(i, j int) bool {
		a, b := eligibleTokens[i], eligibleTokens[j]
		aBal, aOK := freebucksBalance((*toks)[a], model)
		bBal, bOK := freebucksBalance((*toks)[b], model)
		if aOK != bOK {
			return aOK
		}
		if aOK && aBal != bBal {
			return aBal > bBal // largest Freebucks balance first
		}
		return a < b
	})
	return eligibleTokens
}

// freebucksBalance reports the token's Freebucks spendable balance for
// model: false when the snapshot carries no Freebucks block or the model
// has no price (unpriced rows never rank by balance).
func freebucksBalance(tok *tokenEntry, model string) (float64, bool) {
	snap := tok.session.Snapshot()
	fb := snap.Freebucks
	if fb == nil {
		return 0, false
	}
	if _, ok := fb.Prices[model]; !ok {
		return 0, false
	}
	return fb.Balance, true
}

// tokenAvailable reports whether tok can serve model right now, for ordering
// (not gating): a locked, cooling, or banned token is demoted behind
// available ones. The per-model quota exemption mirrors the failover loop's
// cooldown skip in leaseFromOrder: a cooldown caused by a DIFFERENT model's
// quota exhaustion (quota errors carry the model) leaves the token available
// for this request.
func tokenAvailable(tok *tokenEntry, model string) bool {
	if tok.locked.Load() {
		return false
	}
	until := tok.runs.CooldownUntil()
	if !until.IsZero() && time.Now().Before(until) {
		if rle := tok.runs.RateLimitError(); rle != nil && rle.Model != "" && rle.Model != model && isQuotaExhaustedError(rle) {
			return true
		}
		return false
	}
	return tok.runs.BanError() == nil
}

// bestWaitingRoom picks the queue entry with the lowest position; ties break
// on the lowest queue depth (PRD §3: best-waiting-room-position selection).
func bestWaitingRoom(entries []*session.WaitingRoomError) *session.WaitingRoomError {
	best := entries[0]
	for _, candidate := range entries[1:] {
		if betterWait(candidate, best) {
			best = candidate
		}
	}
	return best
}

// betterWait reports whether a outranks b. Positions <= 0 mean "unknown" and
// rank below any known position (mirrors freebuff2api-quorinex).
func betterWait(a, b *session.WaitingRoomError) bool {
	if b == nil {
		return true
	}
	if a.Position <= 0 {
		return false
	}
	if b.Position <= 0 {
		return true
	}
	if a.Position != b.Position {
		return a.Position < b.Position
	}
	return a.QueueDepth < b.QueueDepth
}
