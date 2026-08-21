// Package pool is the multi-token front door: it picks the token that will
// serve a model request, then leases a run from that token's RunManager and
// an instance from its session manager. Port of freebuff2api-quorinex
// run_manager.go (Acquire half) with the upstream/session/runs split of this
// project.
//
// Failover semantics (PRD §6 error matrix):
//   - 401 (ErrAuthRejected) from a token's run START → 30-min cooldown for
//     that token, try the next.
//   - session waiting room → remember the best position, try the next token;
//     when every token fails, the pool surfaces the highest-precedence
//     non-empty error bucket (ban > country-blocked > model-IP-limited >
//     rate-limit > waiting-room > daily cap) instead of a generic 502 — a
//     queued token surfaces 503 + Retry-After as soon as no higher bucket
//     is populated.
//   - run-invalid / session-invalid recoveries are NOT handled here: the
//     caller (server) retries once via a fresh Acquire after invalidating.
//   - anything else → next token; all failed → combined error (only when no
//     error-bucket matched any token).
package pool

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"freebuff-proxy/internal/notify"
	"freebuff-proxy/internal/phasetiming"
	"freebuff-proxy/internal/runs"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/upstream"
)

// Acquire resolves the model's agent, picks a start token round-robin, and
// fails over linearly until a token yields both a run and a session. Returns
// a lease on success. Registry misses (unknown model) are returned as-is.
func (p *Pool) Acquire(ctx context.Context, model string) (*Lease, error) {
	toks := p.toks.Load()
	cfg := p.cfg.Load()
	if len(*toks) == 0 {
		return nil, errors.New("pool: no auth tokens configured")
	}
	agentID, err := p.reg.AgentForModel(model)
	if err != nil {
		return nil, err
	}

	start := int(p.rr.Add(1)-1) % len(*toks)
	// Hot-session-first selection: tokens that already hold a live session
	// are tried before any fresh account, so a request reuses the live slot
	// instead of admitting a new session (never create where one already
	// exists — the lowest fingerprint/quota-burn path). When at least one
	// token is hot, the pass iterates only over hot tokens; only when every
	// hot token fails does it fall back to the remaining eligible tokens
	// from the round-robin start (cold path), exactly like the historical
	// linear failover. When no token is hot the order is unchanged.
	order, quotaLimited := p.acquireOrder(toks, start, model)

	// Issue #191: when an admission for this model is already in-flight on a
	// different token, override the cold ordering so concurrent requests land
	// on the same token and park on the existing single-flight refreshCh
	// instead of creating a competing session.
	p.admissionsMu.Lock()
	admittingToken, isAdmitting := p.admissions[model]
	if !isAdmitting {
		// No leader yet — register this goroutine as the admission leader
		// for this model so any concurrent cold request will follow us.
		p.admissions[model] = 0 // placeholder; will be updated in the token loop
	} else if admittingToken >= 0 {
		// An existing admission is in progress on admittingToken — pin
		// the cold ordering to that token so we park on its single-flight.
		reordered := make([]int, 0, len(order))
		reordered = append(reordered, admittingToken)
		for _, idx := range order {
			if idx != admittingToken {
				reordered = append(reordered, idx)
			}
		}
		order = reordered
	}
	p.admissionsMu.Unlock()
	var errs []string
	var waiting []*session.WaitingRoomError
	var rateLimited []*upstream.RateLimitError
	var ipCapped []*upstream.IpCappedError
	var banned []*upstream.BanError
	var countryBlocked []*upstream.CountryBlockedError
	var modelLimited []*upstream.LimitedIpError
	var dailyLimited []*upstream.RateLimitError
	var scarceUntil []time.Time
	scarceSet := scarceModelSet(cfg.ScarceSessionModels)
	for _, idx := range order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Defensive bounds check: acquireOrder builds its order against the
		// SAME snapshot loaded above, but a removal racing this call must
		// never index past the slice it computed the order from. Skip
		// indices that are no longer present instead of panicking.
		if idx < 0 || idx >= len(*toks) {
			continue
		}
		tok := (*toks)[idx]
		// Administratively locked tokens are never eligible for leasing.
		if tok.locked.Load() {
			continue
		}
		name := fmt.Sprintf("token-%d", idx+1)

		if until := tok.runs.CooldownUntil(); time.Now().Before(until) {
			// Issue #155: if the cooldown was caused by a specific model's quota exhaustion,
			// and we are requesting a different model (e.g. fallback to mimo-v2.5),
			// do not block this token from serving the requested model.
			if rle := tok.runs.RateLimitError(); rle != nil && rle.Model != "" && rle.Model != model && isQuotaExhaustedError(rle) {
				// Token is only quota-capped for rle.Model, but can still serve `model`.
			} else {
				errs = append(errs, fmt.Sprintf("%s: cooling down until %s", name, until.Format(time.RFC3339)))
				p.logger.Debug("pool: token skipped (cooldown)", "token", idx+1, "until", until.Format(time.RFC3339))
				if be := tok.runs.BanError(); be != nil {
					dup := false
					for _, existing := range banned {
						if existing.Error() == be.Error() {
							dup = true
							break
						}
					}
					if !dup {
						banned = append(banned, be)
					}
				}
				if cbe := tok.runs.CountryBlockedError(); cbe != nil {
					dup := false
					for _, existing := range countryBlocked {
						if existing.Error() == cbe.Error() {
							dup = true
							break
						}
					}
					if !dup {
						countryBlocked = append(countryBlocked, cbe)
					}
				}
				if rle := tok.runs.RateLimitError(); rle != nil {
					dup := false
					for _, existing := range rateLimited {
						if existing.Error() == rle.Error() {
							dup = true
							break
						}
					}
					if !dup {
						rateLimited = append(rateLimited, rle)
					}
				}
				if ice := tok.runs.IpCappedError(); ice != nil {
					dup := false
					for _, existing := range ipCapped {
						if existing.Error() == ice.Error() {
							dup = true
							break
						}
					}
					if !dup {
						ipCapped = append(ipCapped, ice)
					}
				}
				continue
			}
		}

		// Daily rolling cap: a token that already sent its
		// MAX_MESSAGES_PER_DAY successful chats in the last 24h is skipped
		// like a cooldown; when every token is capped, the pool surfaces a
		// 429 with the earliest window reset.
		if cfg.MaxMessagesPerDay > 0 && p.usageCount(idx) >= cfg.MaxMessagesPerDay {
			dailyLimited = append(dailyLimited, p.dailyLimitError(idx))
			errs = append(errs, fmt.Sprintf("%s: daily message limit (%d) reached", name, cfg.MaxMessagesPerDay))
			p.logger.Debug("pool: token skipped (daily message limit)", "token", idx+1, "limit", cfg.MaxMessagesPerDay)
			continue
		}
		// Issue #155: scarce-model session protection. If the token holds an
		// active scarce session (pro/luna) with > 1 minute remaining, do not
		// switch away from it to serve a different model — that would burn
		// the irreplaceable 1-session/day allocation.
		if scarceHeld(tok.session.Snapshot(), model, scarceSet) {
			exp := tok.session.Snapshot().ExpiresAt
			scarceUntil = append(scarceUntil, exp)
			errs = append(errs, fmt.Sprintf("%s: scarce session (%s) in use until %s", name, tok.session.Snapshot().Model, exp.Format(time.RFC3339)))
			p.logger.Debug("pool: token skipped (scarce session in use)", "token", idx+1, "model", tok.session.Snapshot().Model, "until", exp.Format(time.RFC3339))
			continue
		}

		// Issue #85: session-quota-capped token for the requested model.
		// The hot path excludes these in acquireOrder (their rate-limit
		// reasons ride back in quotaLimited); the no-hot round-robin path
		// reaches them here and records the reason the same way.
		if _, _, capped := quotaRemaining(tok, model); capped {
			rateLimited = append(rateLimited, quotaLimitError(tok, model))
			errs = append(errs, fmt.Sprintf("%s: session quota exhausted for model", name))
			p.logger.Debug("pool: token skipped (session quota exhausted)", "token", idx+1, "model", model)
			continue
		}

		// Session-create admission gate (issue #86): concurrent session
		// creates are bounded globally and per model; when the gate is at
		// capacity the acquire waits (the caller's deadline surfaces as
		// 503). The permit is held only for the admission call, never
		// across the upstream chat.
		p.admissionsMu.Lock()
		if p.admissions == nil {
			p.admissions = make(map[string]int)
		}
		// Update the pre-registered leader slot with the actual token index.
		p.admissions[model] = idx
		p.admissionsMu.Unlock()

		permit, err := p.gate.acquire(ctx, model)
		if err != nil {
			p.admissionsMu.Lock()
			if p.admissions != nil && p.admissions[model] == idx {
				delete(p.admissions, model)
			}
			p.admissionsMu.Unlock()
			return nil, err
		}
		sessionStart := time.Now()
		// Issue #94(b): WAITING_ROOM_CHAIN gate — when the upstream last
		// refused this token with 428 waiting_room_required, fire the
		// reference pre-session ad-chain + streak flow (best-effort, bounded
		// by the client's own chain timeout) before the next session create
		// so the admission does not bounce off the same 428 again.
		if cfg.WaitingRoomChain && tok.client.ConsumeWaitingRoomChain() {
			p.logger.Debug("pool: firing waiting-room pre-session chain", "token", idx+1)
			tok.client.FireWaitingRoomChain(ctx)
		}
		instanceID, err := tok.session.EnsureSessionForModel(ctx, model)
		permit.Release()
		p.admissionsMu.Lock()
		if p.admissions != nil && p.admissions[model] == idx {
			delete(p.admissions, model)
		}
		p.admissionsMu.Unlock()
		phasetiming.FromContext(ctx).Since(phasetiming.SessionRefreshMS, sessionStart)
		if err != nil {
			if errors.Is(err, upstream.ErrAuthRejected) {
				tok.runs.Cooldown(runs.DefaultCooldown)
				p.logger.Debug("pool: token cooling down", "token", idx+1, "duration", runs.DefaultCooldown.String())
			}
			var wr *session.WaitingRoomError
			if errors.As(err, &wr) {
				waiting = append(waiting, wr)
			}
			if rle := asRateLimit(err); rle != nil {
				// Issue #178: tag the refusal with the requested model when
				// the upstream body omits it, so the remembered cooldown can
				// be isolated per model — a quota cap on one model (glm-5.2,
				// gpt-5.6-luna) must not block the same token's other models.
				if rle.Model == "" {
					rle.Model = model
				}
				tok.runs.CooldownRateLimit(rle)
				dup := false
				for _, existing := range rateLimited {
					if existing.Error() == rle.Error() {
						dup = true
						break
					}
				}
				if !dup {
					rateLimited = append(rateLimited, rle)
				}
				// Issue #122: the fresh-admission spend ceiling is the
				// upstream's primary spend gate, so an admission-path
				// spend_limited counts on the ledger too (same counter as
				// the chat-path refusal in CooldownTokenRateLimit).
				if rle.Status == "spend_limited" {
					p.spendMu.Lock()
					p.recordSpendLimited(idx)
					p.spendMu.Unlock()
				}
			}
			if ice := asIpCapped(err); ice != nil {
				tok.runs.CooldownIpCapped(ice)
				dup := false
				for _, existing := range ipCapped {
					if existing.Error() == ice.Error() {
						dup = true
						break
					}
				}
				if !dup {
					ipCapped = append(ipCapped, ice)
				}
			}
			if be := asBan(err); be != nil {
				tok.runs.CooldownBan(be)
				p.notifyBan(idx+1, model)
				dup := false
				for _, existing := range banned {
					if existing.Error() == be.Error() {
						dup = true
						break
					}
				}
				if !dup {
					banned = append(banned, be)
				}
			}
			if cbe := asCountryBlocked(err); cbe != nil {
				tok.runs.CooldownCountryBlocked(cbe)
				dup := false
				for _, existing := range countryBlocked {
					if existing.Error() == cbe.Error() {
						dup = true
						break
					}
				}
				if !dup {
					countryBlocked = append(countryBlocked, cbe)
				}
			}
			if lie := asLimitedIp(err); lie != nil {
				// Issue #74 P2: the egress IP cannot serve this model
				// (limited_ip). The session row is fine — it stays bound to
				// its admitted model — so nothing is invalidated or cooled
				// per-token: the (egress, model) pair is marked unfit so
				// new requests are refused fast instead of re-admitting and
				// burning a daily session slot on every token. The lie is
				// pool-owned here (fresh from the admission error), so
				// stamping Model makes the surfaced refusal self-describing;
				// the registry stores its own copy.
				lie.Model = model
				p.MarkModelUnfit(model, lie)
				modelLimited = append(modelLimited, lie)
				errs = append(errs, fmt.Sprintf("%s: %v", name, err))
				continue
			}
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		tok.runs.ClearCooldowns()

		// Re-validate the token is still current: a concurrent
		// RemoveLastToken may have swapped the snapshot while the session
		// admission above was in flight. Leasing a removed token would
		// strand its run's inflight — LeaseRelease always releases through
		// the lease's own entry, but the run would belong to a drained,
		// retiring manager — so skip instead (the removal path drains the
		// retired entry once it observes the slip).
		if cur := p.toks.Load(); idx < 0 || idx >= len(*cur) || (*cur)[idx] != tok {
			continue
		}
		ss := tok.session.Snapshot()
		effectiveModel := model
		effectiveAgentID := agentID
		if ss.Model != "" && ss.Model != model {
			effectiveModel = ss.Model
			if p.reg != nil {
				if resolvedAgent, aerr := p.reg.AgentForModel(effectiveModel); aerr == nil {
					effectiveAgentID = resolvedAgent
				}
			}
		}

		// Issue #90a: pre-create the run at session admission (best-effort)
		// so the first chat on a freshly-admitted session does not pay the
		// START latency. When a run already exists this is a cheap no-op;
		// when the START fails here the Acquire below retries and surfaces
		// the real error through the normal failover path.
		_ = tok.runs.Precreate(ctx, effectiveAgentID)
		runStart := time.Now()
		run, err := tok.runs.Acquire(ctx, effectiveAgentID)
		phasetiming.FromContext(ctx).Since(phasetiming.RunAcquireMS, runStart)
		if err != nil {
			if errors.Is(err, upstream.ErrAuthRejected) {
				tok.runs.Cooldown(runs.DefaultCooldown)
				p.logger.Debug("pool: token cooling down", "token", idx+1, "duration", runs.DefaultCooldown.String())
			}
			if rle := asRateLimit(err); rle != nil {
				// Issue #178: tag the refusal with the requested model when
				// the upstream body omits it, so the remembered cooldown can
				// be isolated per model — a quota cap on one model (glm-5.2,
				// gpt-5.6-luna) must not block the same token's other models.
				if rle.Model == "" {
					rle.Model = model
				}
				tok.runs.CooldownRateLimit(rle)
				dup := false
				for _, existing := range rateLimited {
					if existing.Error() == rle.Error() {
						dup = true
						break
					}
				}
				if !dup {
					rateLimited = append(rateLimited, rle)
				}
				// Issue #122: count run-start spend_limited refusals on the
				// ledger (same counter as the chat-path refusal).
				if rle.Status == "spend_limited" {
					p.spendMu.Lock()
					p.recordSpendLimited(idx)
					p.spendMu.Unlock()
				}
			}
			if ice := asIpCapped(err); ice != nil {
				tok.runs.CooldownIpCapped(ice)
				dup := false
				for _, existing := range ipCapped {
					if existing.Error() == ice.Error() {
						dup = true
						break
					}
				}
				if !dup {
					ipCapped = append(ipCapped, ice)
				}
			}
			if be := asBan(err); be != nil {
				tok.runs.CooldownBan(be)
				p.notifyBan(idx+1, model)
				dup := false
				for _, existing := range banned {
					if existing.Error() == be.Error() {
						dup = true
						break
					}
				}
				if !dup {
					banned = append(banned, be)
				}
			}
			if cbe := asCountryBlocked(err); cbe != nil {
				tok.runs.CooldownCountryBlocked(cbe)
				dup := false
				for _, existing := range countryBlocked {
					if existing.Error() == cbe.Error() {
						dup = true
						break
					}
				}
				if !dup {
					countryBlocked = append(countryBlocked, cbe)
				}
			}
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		p.logger.Debug("pool: lease acquired", "token", idx+1, "model", effectiveModel, "agent", effectiveAgentID, "instance_id", instanceID,
			"country", ss.CountryCode)
		// Track the activity and end any idle-maintenance pause: the next
		// maintain tick resumes rotation/refresh work.
		p.lastActiveMu.Lock()
		p.lastActive = time.Now()
		p.idleFinished = false
		p.lastActiveMu.Unlock()
		p.lastTokenMu.Lock()
		if p.lastTokenByModel == nil {
			p.lastTokenByModel = make(map[string]int)
		}
		p.lastTokenByModel[effectiveModel] = idx
		p.lastTokenMu.Unlock()
		return &Lease{Token: idx, Model: effectiveModel, AgentID: effectiveAgentID, Run: run, SessionInstanceID: instanceID,
			entry: tok, AcquiredAt: time.Now()}, nil
	}

	// Failover precedence (PRD §6 error matrix): when buckets are mixed the
	// highest-precedence non-empty bucket wins — ban > country-blocked >
	// model-IP-limited > rate-limit > ip-capped > waiting-room > daily cap.
	// Each bucket contributes its best error (first ban, longest rate
	// window, first ip_capped, lowest queue position, earliest daily
	// reset). Only when every bucket is empty — all tokens failed with
	// errors outside the matrix — is the generic error surfaced.
	// Issue #85: quota-capped tokens were excluded in acquireOrder (never
	// attempted); their rate-limit reasons land here so a fully-capped pool
	// surfaces a real 429 with the earliest window reset instead of a
	// generic combined error.
	rateLimited = append(rateLimited, quotaLimited...)
	if len(banned) > 0 {
		return nil, banned[0]
	}
	if len(countryBlocked) > 0 {
		return nil, countryBlocked[0]
	}
	if len(modelLimited) > 0 {
		return nil, modelLimited[0]
	}
	if len(rateLimited) > 0 {
		// Issue #155: quota-exhaustion fallback — when every rate-limited error
		// is a session quota exhaustion for the requested model, fall back to
		// the unlimited model (mimo-v2.5) if configured.
		allQuotaCapped := true
		for _, rle := range rateLimited {
			if !isQuotaExhaustedError(rle) {
				allQuotaCapped = false
				break
			}
		}
		if allQuotaCapped {
			if fb := cfg.QuotaFallbackModels[model]; fb != "" && fb != model {
				p.logger.Info("pool: quota exhausted, falling back to unlimited model", "requested", model, "fallback", fb)
				// Issue #164: the fallback lease reports why it serves a
				// different model so the server surfaces the switch to the
				// client (X-FreeBuff-Fallback: quota_exhausted). By the time
				// this branch is reached every token with positive quota for
				// `model` has already been tried and failed (see acquireOrder:
				// quota-capped tokens are excluded from the order, all others
				// are visited by the failover loop before the fallback fires).
				fbLease, fbErr := p.Acquire(ctx, fb)
				if fbLease != nil {
					fbLease.FallbackReason = "quota_exhausted"
				}
				return fbLease, fbErr
			}
		}

		// Pool exhausted (issue #48): every token failed and the highest-
		// precedence bucket is rate-limit — no ban/country is present, so
		// this is the "all tokens are at their quota/window limit" state the
		// operator wants to be alerted about. Fire-and-forget webhook
		// (throttled per event type); the 429 still surfaces as usual.
		p.notifyMu.Lock()
		n := p.notify
		p.notifyMu.Unlock()
		if n != nil {
			n.Send(notify.Event{Event: "pool_exhausted", TokenIndex: 0, Model: model,
				Message: "all tokens are rate-limited; the pool cannot serve the request"})
		}
		return nil, bestRateLimit(rateLimited)
	}
	if len(ipCapped) > 0 {
		return nil, ipCapped[0]
	}
	if len(scarceUntil) > 0 {
		earliest := scarceUntil[0]
		for _, t := range scarceUntil[1:] {
			if t.Before(earliest) {
				earliest = t
			}
		}
		return nil, &ScarceSessionError{Model: model, ExpiresAt: earliest}
	}
	if len(waiting) > 0 {
		wr := bestWaitingRoom(waiting)
		p.logger.Debug("pool: waiting room surfaced", "position", wr.Position, "queue_depth", wr.QueueDepth, "retry_after", wr.RetryAfter.String())
		return nil, wr
	}
	if len(dailyLimited) > 0 {
		return nil, bestDailyLimit(dailyLimited)
	}
	return nil, fmt.Errorf("unable to acquire run from any token: %s", strings.Join(errs, "; "))
}

// acquireOrder computes the token iteration order for one Acquire pass
// (hot-session-first selection, see Acquire). toks is the caller's snapshot
// (loaded once in Acquire) — the order is built against the same snapshot
// the failover loop indexes, so an AddToken racing the call can never make
// the loop index past its own snapshot. start is the round-robin start
// index; model is the requested upstream model.
//
// Issue #85: within the hot set, tokens whose last admission reported a
// known positive remaining session quota for the requested model rank above
// unknown-quota tokens, ordered by smallest remaining first (drain the
// account closest to its limit; preserve fuller quotas —
// reference/freebuff-reverse .../scheduler.go:472-496). Tokens
// whose quota is exhausted for the model (RecentCount >= Limit with a future
// ResetAt) are excluded from this pass entirely; their rate-limit reasons
// are returned so the caller surfaces a real 429 when every token is capped.
//
// Issue #164 (fallback ordering): the order returned here exhausts EVERY
// token with positive quota for the requested model before the caller's
// QUOTA_FALLBACK_MODELS fallback can fire. Non-capped tokens (matching hot,
// cold, mismatched hot) are all in the order and the failover loop in
// Acquire visits each of them in turn — a token only fails the pass with a
// quota-exhausted error after it was actually attempted. Quota-capped
// tokens are excluded (they have nothing left to serve), and when every
// token is capped or cooling down the order degrades to full round-robin so
// the loop still records every reason. The fallback branch in Acquire only
// runs after that loop completed without a lease and every rate-limited
// error it recorded is a quota exhaustion.
func (p *Pool) acquireOrder(toks *[]*tokenEntry, start int, model string) ([]int, []*upstream.RateLimitError) {
	// eligible mirrors the per-token checks the failover loop applies:
	// not cooling down, under the daily message cap, and not quota-capped
	// for the requested model (issue #85). It never records the exclusion
	// reasons — the caller does that in one place.
	eligible := func(idx int) bool {
		tok := (*toks)[idx]
		// Administratively locked tokens are never eligible for leasing.
		if tok.locked.Load() {
			return false
		}
		// Quota-capped tokens are excluded from BOTH the hot set and the
		// cold fallback: their rate-limit reasons ride back in quotaLimited,
		// so the pool surfaces a real 429 when every token is capped.
		if _, _, capped := quotaRemaining(tok, model); capped {
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
	// 2. Known positive remaining quota: smallest remaining quota first (drain account closest to limit first).
	// 3. Equal/unknown quota: prefer last-used token for this model (session stickiness for multi-turn chats).
	// 4. Stable token index preference (lower index first) to avoid round-robin ping-pong across accounts.
	sort.SliceStable(matchingHot, func(i, j int) bool {
		a, b := matchingHot[i], matchingHot[j]
		tokA, tokB := (*toks)[a], (*toks)[b]
		snapA, snapB := tokA.session.Snapshot(), tokB.session.Snapshot()

		isAdmA := snapA.Refreshing || (isAdmitting && a == admittingToken)
		isAdmB := snapB.Refreshing || (isAdmitting && b == admittingToken)
		if isAdmA != isAdmB {
			return isAdmA
		}

		aKnown, aRem, _ := quotaRemaining(tokA, model)
		bKnown, bRem, _ := quotaRemaining(tokB, model)
		if aKnown != bKnown {
			return aKnown
		}
		if aKnown && aRem != bRem {
			return aRem < bRem
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

	order := make([]int, 0, len(*toks))
	order = append(order, matchingHot...)
	order = append(order, coldTokens...)
	order = append(order, mismatchedHot...)

	if len(order) == 0 {
		// All tokens are cooling down or capped: fallback to round-robin
		// so the failover loop visits them and records their errors.
		order = make([]int, len(*toks))
		for i := range order {
			order[i] = (start + i) % len(*toks)
		}
		return order, nil
	}
	// The capped tokens excluded above are never visited by the failover
	// loop, so their rate-limit reasons must ride back with the order: when
	// every token is capped the pool surfaces a real 429 with the earliest
	// window reset instead of a generic combined error.
	inOrder := make(map[int]struct{}, len(order))
	for _, idx := range order {
		inOrder[idx] = struct{}{}
	}
	var quotaLimited []*upstream.RateLimitError
	for idx := range *toks {
		if _, ok := inOrder[idx]; ok {
			continue
		}
		if _, _, capped := quotaRemaining((*toks)[idx], model); capped {
			quotaLimited = append(quotaLimited, quotaLimitError((*toks)[idx], model))
		}
	}
	return order, quotaLimited
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
