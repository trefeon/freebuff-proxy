// maturity.go — nightly streak-maintenance automation (one global run in
// the 60 minutes before the Pacific-midnight reset, replacing the old
// per-token all-day slots).
//
// Enrolled (maturity-enabled) tokens stay leasable at all times:
// enrollment never locks an account out of serving rotation. Each enrolled
// token gets one daily low-cost touch inside the pre-reset window unless
// client traffic already used the account today. When the cached streak
// reaches the global target automation disables itself (no lock
// transitions anywhere — an operator-manually-locked token is skipped by
// the run and stays locked).
//
// Safety posture: global kill-switch (MATURITY_ENABLED, default on with
// dry-run probes), dry-run default (probe-only, zero
// session slots claimed), unmetered touch models only (never burns premium
// quota — the fire path fails closed on priced rows), per-token slots
// staggered with jitter inside the 60m window, restart-safe idempotency
// (touchDay/slotDay plus the upstream todayUsed flag), and a 429
// abort+backoff that pauses the walk instead of hammering. The run rides
// the 60s maintainTick pass — no new goroutine — and never touches
// quarantined, banned, cooling, country-blocked, or locked accounts.
// Window math is America/Los_Angeles wall-clock (never a fixed offset), so
// the run tracks Pacific midnight across DST.
package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"freebuff-proxy/backend/internal/modelcat"
	"freebuff-proxy/backend/internal/upstream"
)

// Maturity touch modes.
const (
	// MaturityModeUnmetered admits the configured unmetered model
	// (MATURITY_TOUCH_MODEL): reservation on an unpriced row, free,
	// plus a live session real traffic can reuse.
	MaturityModeUnmetered = "unmetered"
	// MaturityModePremiumShort admits one short session on a premium row
	// instead. It is billed in Freebucks and stays opt-in per token.
	MaturityModePremiumShort = "premium-short"
)

const (
	// maturityThrottle is the restart-safe minimum gap between two touches
	// on one token: a restart re-rolls the day's slot, and touchDay plus
	// the upstream todayUsed flag bound the worst case to one extra cheap
	// touch.
	maturityThrottle = 6 * time.Hour
	// maturityStreakFresh bounds streak-cache age for touch decisions: the
	// number moves daily, and a touch must never fire blind off stale data.
	maturityStreakFresh = time.Hour
	// maturityRunWindow is the fixed nightly maintenance window: the 60
	// minutes before the Pacific-midnight reset. One collapsed pre-reset
	// window for every account (not per-token all-day slots), so touches
	// land right before upstream rolls the daily streak.
	maturityRunWindow = 60 * time.Minute
	// maturity429Backoff pauses the nightly walk after a rate-limited
	// touch: the walk aborts and no further touch fires until this long
	// after the 429, instead of hammering a throttled upstream.
	maturity429Backoff = 15 * time.Minute
)

// MaturitySnapshot is the dashboard-ready per-token maturity view. Nil on
// TokenSnapshot until maturity is first enabled for the token, so tokens
// that never opt in carry no new payload.
type MaturitySnapshot struct {
	Enabled bool   `json:"enabled"`
	Target  int    `json:"target"`
	Mode    string `json:"mode"`
	// TouchModel is the per-token touch-model override ("" = automatic:
	// the cheapest served unmetered row, falling back to the global
	// MATURITY_TOUCH_MODEL when no unmetered served row exists).
	// Omitted on the wire when unset so never-enrolled tokens keep
	// their existing payload shape.
	TouchModel string    `json:"touch_model,omitempty"`
	Badge      string    `json:"badge"`
	Slot       time.Time `json:"slot,omitempty"`
	// SlotDay is the account-timezone calendar day the Slot belongs to
	// ("2006-01-02"): the dashboard derives the next-touch countdown
	// and the done/pending day-strip from Slot/SlotDay/LastTouch
	// without a new scheduler.
	SlotDay   string    `json:"slot_day,omitempty"`
	LastTouch time.Time `json:"last_touch,omitempty"`
	// TouchDay is the account-timezone calendar day of the last touch
	// ("2006-01-02"): TouchDay == SlotDay means touched today.
	TouchDay     string `json:"touch_day,omitempty"`
	LastAction   string `json:"last_action,omitempty"`
	LastResult   string `json:"last_result,omitempty"`
	LastAdvanced string `json:"last_advanced,omitempty"`
	// EffectiveTouchModel is the model the next touch will actually
	// admit (manual override, premium-short pool head, auto pick, or
	// explicit global fallback, in that precedence).
	EffectiveTouchModel string `json:"effective_touch_model,omitempty"`
	// AutoTouchModel is the automatic pick (cheapest served unmetered
	// row) with AutoTouchReason naming why ("auto:unmetered" or
	// "fallback:no-unmetered-served"). Shown by the UI next to the
	// manual dropdown so the Auto default is inspectable.
	AutoTouchModel  string `json:"auto_touch_model,omitempty"`
	AutoTouchReason string `json:"auto_touch_reason,omitempty"`
}

// maturityState is the mutable per-token automation state, guarded by
// tokenEntry.maturityMu. Zero value = disabled.
type maturityState struct {
	enabled bool
	target  int
	mode    string
	// touchModel overrides the global MATURITY_TOUCH_MODEL for this
	// token only. Empty means "use the global fallback".
	touchModel    string
	slot          time.Time
	slotDay       string
	lastTouch     time.Time
	lastAction    string
	lastResult    string
	lastAdvanced  string
	lastStreak    int
	streakAtTouch int
	touchDay      string
}

// maturityPersisted is the JSON-stable mirror of maturityState for the
// maturity_json blob (DB column, not wire: field names stay snake_case and
// additive — old rows must still unmarshal after new counters land).
type maturityPersisted struct {
	Enabled       bool      `json:"enabled"`
	Target        int       `json:"target"`
	Mode          string    `json:"mode"`
	TouchModel    string    `json:"touch_model,omitempty"`
	Slot          time.Time `json:"slot,omitempty"`
	SlotDay       string    `json:"slot_day,omitempty"`
	LastTouch     time.Time `json:"last_touch,omitempty"`
	LastAction    string    `json:"last_action,omitempty"`
	LastResult    string    `json:"last_result,omitempty"`
	LastAdvanced  string    `json:"last_advanced,omitempty"`
	LastStreak    int       `json:"last_streak,omitempty"`
	StreakAtTouch int       `json:"streak_at_touch,omitempty"`
	TouchDay      string    `json:"touch_day,omitempty"`
}

func (m maturityState) marshalMaturity() (string, error) {
	raw, err := json.Marshal(maturityPersisted{
		Enabled:       m.enabled,
		Target:        m.target,
		Mode:          m.mode,
		TouchModel:    m.touchModel,
		Slot:          m.slot,
		SlotDay:       m.slotDay,
		LastTouch:     m.lastTouch,
		LastAction:    m.lastAction,
		LastResult:    m.lastResult,
		LastAdvanced:  m.lastAdvanced,
		LastStreak:    m.lastStreak,
		StreakAtTouch: m.streakAtTouch,
		TouchDay:      m.touchDay,
	})
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func unmarshalMaturity(raw string) (maturityState, error) {
	var stored maturityPersisted
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return maturityState{}, err
	}
	return maturityState{
		enabled:       stored.Enabled,
		target:        stored.Target,
		mode:          stored.Mode,
		touchModel:    stored.TouchModel,
		slot:          stored.Slot,
		slotDay:       stored.SlotDay,
		lastTouch:     stored.LastTouch,
		lastAction:    stored.LastAction,
		lastResult:    stored.LastResult,
		lastAdvanced:  stored.LastAdvanced,
		lastStreak:    stored.LastStreak,
		streakAtTouch: stored.StreakAtTouch,
		touchDay:      stored.TouchDay,
	}, nil
}

// SetMaturity enrolls or disenrolls token in the nightly streak-maintenance
// run. Enrollment never locks: the account stays leasable in serving
// rotation while automation touches it. Disabling stops the touches and
// never touches the lock either (only the operator locks/unlocks).
// target <= 0 falls back to the global MATURITY_TARGET_DAYS default (the
// dashboard enrolls with target 0: per-account targets are gone, one global
// 7-day target covers every account); mode "" means unmetered.
// mode premium-short spends from the account's metered pool and stays opt-in
// per token.
// touchModel is the per-token touch-model override; "" (or "auto") selects
// the automatic default (cheapest served unmetered row, falling back to the
// global MATURITY_TOUCH_MODEL when no unmetered served row exists). Any
// other non-empty value must be a provider/model id (shape only —
// served/unmetered semantics stay in the fire path, which fails closed on
// misconfigured models). The override is stored on disable too, so
// re-enabling restores it.
func (p *Pool) SetMaturity(token int, enabled bool, target int, mode string, touchModel string) error {
	toks := p.roster.Load()
	if toks == nil || token < 0 || token >= len(*toks) {
		return fmt.Errorf("pool: token %d out of range", token)
	}
	if mode == "" {
		mode = MaturityModeUnmetered
	}
	if mode != MaturityModeUnmetered && mode != MaturityModePremiumShort {
		return fmt.Errorf("pool: unknown maturity mode %q (want %q or %q)", mode, MaturityModeUnmetered, MaturityModePremiumShort)
	}
	touchModel = strings.TrimSpace(touchModel)
	if modelcat.IsAutoTouchSentinel(touchModel) {
		touchModel = ""
	}
	if touchModel != "" && !strings.Contains(touchModel, "/") {
		return fmt.Errorf("pool: maturity touch model %q must be a provider/model id (e.g. upstage/solar-pro4)", touchModel)
	}
	if target < 0 || target > 28 {
		return fmt.Errorf("pool: maturity target %d out of range (want 0..28, 0 = global MATURITY_TARGET_DAYS default)", target)
	}
	if enabled && target <= 0 {
		target = p.maturityDefaultTarget()
	}
	tok := (*toks)[token]
	tok.maturityMu.Lock()
	tok.maturity.enabled = enabled
	tok.maturity.touchModel = touchModel
	if enabled {
		tok.maturity.target = target
		tok.maturity.mode = mode
		if tok.maturity.slot.IsZero() {
			now := time.Now()
			tok.maturity.slot, tok.maturity.slotDay = rollMaturitySlotInWindow(pacificDayKey(now), now)
		}
	}
	tok.maturityMu.Unlock()
	p.saveMaturity(token, tok)
	if enabled {
		detail := fmt.Sprintf("enabled target=%d mode=%s", target, mode)
		if touchModel != "" {
			detail += " touch=" + touchModel
		}
		p.emitMaturity(token, "config", detail)
	} else {
		p.emitMaturity(token, "config", "disabled")
	}
	return nil
}

// maturityAutoFor resolves the automatic touch-model pick for one token
// from its live Freebucks meter: the cheapest IsServedModel row with
// price 0 or a quota exemption. Honeypot, god-only, eval, paused, and
// priced rows can never win (the IsServed gate plus the premium and live
// price gates inside modelcat, never a naive price sort). The served set
// only changes on registry sync (wiregen), so the pick is stable across
// touches; per-touch meter movement stays enforced by the fail-closed
// fire path, not by re-sorting here.
func maturityAutoFor(tok *tokenEntry) (string, string) {
	var prices map[string]float64
	exempt := false
	if tok != nil && tok.sessionMgr() != nil {
		if snap := tok.sessionMgr().Snapshot(); snap.Freebucks != nil {
			prices = snap.Freebucks.Prices
			exempt = snap.Freebucks.QuotaExempt
		}
	}
	return modelcat.AutoUnmeteredTouchModel(prices, exempt)
}

// maturityResolveEffective resolves the model the next touch will
// actually admit, plus the auto pick and its reason for display.
// Precedence: manual per-token override, premium-short pool head,
// auto pick when the global is the auto sentinel ("auto"/""), else the
// explicit global fallback. Empty effective means fail closed
// (skip:touch-model) — never an invented model.
func (p *Pool) maturityResolveEffective(st maturityState, tok *tokenEntry, global string) (effective, auto, reason string) {
	auto, reason = maturityAutoFor(tok)
	if st.touchModel != "" {
		return st.touchModel, auto, reason
	}
	if st.mode == MaturityModePremiumShort {
		if premium := modelcat.SharedPremiumModels(); len(premium) > 0 {
			return premium[0], auto, reason
		}
		return "", auto, reason
	}
	if modelcat.IsAutoTouchSentinel(global) {
		return auto, auto, reason
	}
	return global, auto, reason
}

// maturityDefaultTarget resolves the fallback streak target: the configured
// MATURITY_TARGET_DAYS default, 7 when unset (direct Config construction in
// tests bypasses Load).
func (p *Pool) maturityDefaultTarget() int {
	if cfg := p.cfg.Load(); cfg != nil && cfg.MaturityTargetDays > 0 {
		return cfg.MaturityTargetDays
	}
	return 7
}

func (p *Pool) maturityCopy(tok *tokenEntry) maturityState {
	tok.maturityMu.Lock()
	defer tok.maturityMu.Unlock()
	return tok.maturity
}

// maturityTick runs one maturity pass over the fixed tokens. It is called
// from maintainTick (both the active and the idle-stretch paths): idle
// accounts are exactly the ones whose streaks need keeping.
func (p *Pool) maturityTick(ctx context.Context) {
	p.maturityTickAt(ctx, time.Now())
}

// maturityTickAt is maturityTick with the clock injected (fake-clock tests).
// The nightly walk aborts on the first rate-limited touch (429 backoff):
// remaining tokens keep last night's ledger until the backoff lifts.
func (p *Pool) maturityTickAt(ctx context.Context, now time.Time) {
	cfg := p.cfg.Load()
	if cfg == nil || !cfg.MaturityEnabled {
		return
	}
	if p.maturityBackedOff(now) {
		return
	}
	toks := p.roster.Load()
	if toks == nil {
		return
	}
	for i, tok := range *toks {
		if p.maturityTickOne(ctx, cfg.MaturityDryRun, cfg.MaturityTouchModel, i, tok, now) {
			return
		}
	}
}

// maturityTickOne evaluates and possibly fires one token's nightly touch.
// It reports whether the touch was rate-limited (429): the nightly walk
// aborts on the first 429 and backs off instead of hammering.
//
// Bookkeeping (streak refresh, advance accounting, auto-disable) runs on
// every pass, day and night — all local, zero upstream cost. The last-run
// ledger (skips and touches) is only written inside the nightly window, so
// the dashboard shows last night's outcome instead of all-day skip spam.
func (p *Pool) maturityTickOne(ctx context.Context, dryRun bool, touchModel string, idx int, tok *tokenEntry, now time.Time) (rateLimited bool) {
	// Persist-on-exit: every mutation below (skips, touches, disable)
	// lands in the maturity_json blob on the way out. Never-enrolled
	// tokens no-op inside saveMaturity.
	defer p.saveMaturity(idx, tok)
	st := p.maturityCopy(tok)
	if !st.enabled {
		return false
	}
	label := tokenEntryLabel(tok)
	inWindow := maturityInWindow(now)

	// Operator lock beats automation: a manually locked token stays out of
	// both serving rotation (acquire_order.go) and the nightly run, and
	// the run never locks or unlocks — the lock survives the pass.
	if tok.locked.Load() {
		if inWindow {
			p.maturityRecord(tok, "", "skip:locked", "")
		}
		return false
	}

	// Health gates: quarantined, banned, cooling, or country-blocked
	// accounts are never touched — automation must not poke an account
	// upstream already flagged.
	p.clearLiftedQuarantine(tok)
	if q := tok.quarantine.Load(); q != nil {
		if inWindow {
			p.maturityRecord(tok, "", "skip:quarantined", "")
		}
		return false
	}
	rs := tok.runs.Snapshot()
	if rs.BanError != nil && (rs.BannedUntil.IsZero() || now.Before(rs.BannedUntil)) {
		if inWindow {
			p.maturityRecord(tok, "", "skip:banned", "")
		}
		return false
	}
	if !rs.CooldownUntil.IsZero() && now.Before(rs.CooldownUntil) {
		if inWindow {
			p.maturityRecord(tok, "", "skip:cooling", "")
		}
		return false
	}
	if tok.runs.CountryBlockedError() != nil {
		if inWindow {
			p.maturityRecord(tok, "", "skip:country-blocked", "")
		}
		return false
	}

	// Fresh streak or skip: decisions below need truth no older than an
	// hour. backfillLoop usually keeps it fresh; refresh synchronously on
	// the rare stale pass, bounded so one slow account cannot stall the
	// maintain tick for long.
	cached := tok.Streak()
	if cached == nil || now.Sub(cached.UpdatedAt) > maturityStreakFresh {
		var err error
		cached, err = p.maturityRefreshStreak(ctx, tok)
		if err != nil || cached == nil {
			if inWindow {
				p.maturityRecord(tok, "", "skip:streak-stale", "stale")
			}
			return false
		}
	}

	target := st.target
	if target <= 0 {
		target = p.maturityDefaultTarget()
	}

	// Advance accounting: a touch that moved the streak records it for the
	// ledger (last_advanced + the advance history event).
	p.maturityAccountAdvance(idx, tok, cached)

	// Auto-disable: target reached on a healthy account. This is a local
	// state change (no upstream cost) so it runs in dry-run mode too.
	// There is no lock to release — enrollment never locks, and a
	// manually locked token never reaches this branch (locked gate above).
	if cached.Streak >= target {
		tok.maturityMu.Lock()
		tok.maturity.enabled = false
		tok.maturity.lastResult = "released:mature"
		tok.maturity.lastAdvanced = "yes"
		tok.maturity.lastStreak = cached.Streak
		tok.maturityMu.Unlock()
		p.emitMaturity(idx, "release", fmt.Sprintf("streak=%d target=%d", cached.Streak, target))
		p.logger.Info("pool: maturity target reached, automation disabled",
			"token", idx+1, "token_label", label, "streak", cached.Streak, "target", target)
		return false
	}

	// Outside the nightly window: bookkeeping above stays fresh, but the
	// last-run ledger is untouched until tonight.
	if !inWindow {
		return false
	}

	// Client-traffic activity skip: one successful client chat since the
	// last Pacific reset means the account is already alive today — no
	// touch needed. The signal is the local Pacific-day ledger, fed ONLY
	// by the Chat success path: internal probes and warming touches never
	// record here, so automation can never self-skip every account.
	if p.dayRequestCount(idx) > 0 {
		p.maturityRecord(tok, "", "skip:client-active", "")
		return false
	}

	// Restart-safe idempotency, local half: this token already fired today
	// (touchDay/slotDay survive restarts in the maturity_json blob), so a
	// reboot inside the window must not double-touch.
	loc := maturityLocation("America/Los_Angeles")
	today := now.In(loc).Format("2006-01-02")
	tok.maturityMu.Lock()
	touchedToday := tok.maturity.touchDay == today && !tok.maturity.lastTouch.IsZero()
	tok.maturityMu.Unlock()
	if touchedToday {
		p.maturityRecord(tok, "", "skip:today-used", "")
		return false
	}

	// Restart-safe idempotency, upstream half: any activity today (client
	// traffic or an earlier firing) marks the day used, making the day
	// indistinguishable from human use. Zero extra traffic.
	if cached.TodayUsed {
		tok.maturityMu.Lock()
		tok.maturity.lastStreak = cached.Streak
		tok.maturity.lastAdvanced = "yes"
		if tok.maturity.lastResult == "" || strings.HasPrefix(tok.maturity.lastResult, "skip:") {
			tok.maturity.lastResult = "skip:today-used"
		}
		tok.maturityMu.Unlock()
		return false
	}

	// Nightly slot inside the pre-reset window, staggered with jitter and
	// re-rolled every Pacific day (restart-safe via touchDay/todayUsed +
	// the 6h throttle: a re-roll can only cause one extra cheap touch).
	tok.maturityMu.Lock()
	if tok.maturity.slot.IsZero() || tok.maturity.slotDay != today {
		tok.maturity.slot, tok.maturity.slotDay = rollMaturitySlotInWindow(today, now)
	}
	slot := tok.maturity.slot
	lastTouch := tok.maturity.lastTouch
	tok.maturityMu.Unlock()
	if now.Before(slot) {
		p.maturityRecord(tok, "", "skip:slot", "")
		return false
	}
	if !lastTouch.IsZero() && now.Sub(lastTouch) < maturityThrottle {
		p.maturityRecord(tok, "", "skip:throttle", "")
		return false
	}

	// Precedence: per-token override, premium-short pool head, auto pick
	// when the global is the auto sentinel, else the explicit global
	// fallback. Empty resolves fail closed in the fire path.
	effective, _, _ := p.maturityResolveEffective(st, tok, touchModel)
	if p.maturityFire(ctx, dryRun, effective, idx, tok, label, cached, today, now) {
		p.maturityNoteRateLimit(now)
		return true
	}
	return false
}

// maturityRefreshStreak fetches one token's streak synchronously (bounded)
// and caches it. Only the stale path calls it; the hot path stays a pure
// cache read.
func (p *Pool) maturityRefreshStreak(ctx context.Context, tok *tokenEntry) (*upstream.StreakInfo, error) {
	fetch, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	st, err := tok.client.GetStreak(fetch)
	if err != nil || st == nil {
		if err == nil {
			err = fmt.Errorf("pool: empty streak response")
		}
		return nil, err
	}
	tok.SetStreak(st)
	return st, nil
}

// maturityAccountAdvance records streak movement against the fresh reading
// for the run ledger: a touch that moved the streak past its at-touch
// value marks last_advanced and emits the advance history event.
func (p *Pool) maturityAccountAdvance(idx int, tok *tokenEntry, cached *upstream.StreakInfo) {
	tok.maturityMu.Lock()
	m := &tok.maturity
	advanced := false
	m.lastStreak = cached.Streak
	if m.streakAtTouch > 0 && cached.Streak > m.streakAtTouch {
		m.lastAdvanced = "yes"
		m.streakAtTouch = 0
		advanced = true
	}
	tok.maturityMu.Unlock()
	// History emits happen outside the token mutex: the sink must never run
	// under pool locks.
	if advanced {
		p.emitMaturity(idx, "advance", fmt.Sprintf("streak=%d", cached.Streak))
	}
}

// maturityFire performs one touch: dry-run probes (zero-cost, never claims
// a slot); live mode admits the touch model through the token's own session
// manager — wire-identical to a user opening the CLI. It reports whether
// the touch was rate-limited (429): the nightly walk aborts on the first
// 429 and backs off. The IsServedModel honeypot rejection and the
// fail-closed priced-touch skips above stay untouched.
func (p *Pool) maturityFire(ctx context.Context, dryRun bool, touchModel string, idx int, tok *tokenEntry, label string, cached *upstream.StreakInfo, today string, now time.Time) (rateLimited bool) {
	st := p.maturityCopy(tok)
	model := touchModel
	if st.mode == MaturityModePremiumShort {
		premium := modelcat.SharedPremiumModels()
		if len(premium) == 0 {
			p.maturityRecord(tok, "admit", "skip:no-premium-model", "")
			p.emitMaturity(idx, "touch", "admit skip:no-premium-model")
			return
		}
		model = premium[0]
	} else if reason := maturityGuardTouchModel(model); reason != "" {
		p.logger.Warn("pool: maturity touch misconfigured (not a served unmetered model), skipping",
			"token", idx+1, "token_label", label, "model", model)
		p.maturityRecord(tok, "admit", reason, "")
		p.emitMaturity(idx, "touch", fmt.Sprintf("admit %s model=%s", reason, model))
		return
	}
	// Meter-aware lane (issue #350 adaptation): the touch rides the
	// unmetered lane, never the meter. Dry-run probes claim no slot and
	// stay exempt from this check; a LIVE touch on a model this account
	// meters (price > 0, no server exemption) skips instead of spending —
	// maturity preserves streaks, it never buys sessions.
	if !dryRun {
		if snap := tok.sessionMgr().Snapshot(); snap.Freebucks != nil {
			if price, ok := snap.Freebucks.Prices[model]; ok && price > 0 && !snap.Freebucks.QuotaExempt {
				p.logger.Warn("pool: maturity touch model is metered on this account, skipping",
					"token", idx+1, "token_label", label, "model", model, "price", price)
				p.maturityRecord(tok, "admit", "skip:touch-priced", "")
				p.emitMaturity(idx, "touch", fmt.Sprintf("admit skip:touch-priced model=%s price=%v", model, price))
				return
			}
		}
	}

	fire, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	action := "admit"
	var err error
	if dryRun {
		action = "probe"
		_, err = tok.client.ProbeAccount(fire)
	} else {
		_, err = tok.session.EnsureSessionForModel(fire, model)
	}
	result := "ok"
	if err != nil {
		result = "error:" + firstLine(err.Error())
		rateLimited = errors.Is(err, upstream.ErrRateLimited)
	}
	tok.maturityMu.Lock()
	tok.maturity.lastTouch = now
	tok.maturity.lastAction = action
	tok.maturity.lastResult = result
	tok.maturity.touchDay = today
	if err == nil {
		tok.maturity.streakAtTouch = cached.Streak
	}
	tok.maturityMu.Unlock()
	p.emitMaturity(idx, "touch", fmt.Sprintf("%s %s model=%s streak=%d", action, result, model, cached.Streak))
	if err != nil {
		if rateLimited {
			p.logger.Warn("pool: maturity touch rate-limited, aborting nightly walk",
				"token", idx+1, "token_label", label, "action", action, "err", err)
		} else {
			p.logger.Warn("pool: maturity touch failed", "token", idx+1, "token_label", label, "action", action, "err", err)
		}
		return rateLimited
	}
	p.logger.Info("pool: maturity touch fired", "token", idx+1, "token_label", label,
		"action", action, "model", model, "streak", cached.Streak)
	return false
}

// maturityGuardTouchModel fails a misconfigured unmetered touch model
// closed: the model must be a served, non-premium catalog row, else the
// tick (and the manual touch, which funnels through the same fire path)
// records skip:touch-model before any upstream admission. Empty means go.
func maturityGuardTouchModel(model string) string {
	if !modelcat.IsServed(model) || modelcat.IsPremium(model) {
		return "skip:touch-model"
	}
	return ""
}

// MaturityTouchNow fires one manual maturity touch outside the nightly
// window (the dashboard manual override). Slot wait, window gate, and 6h
// throttle are bypassed; health gates, streak freshness, and todayUsed
// still apply. The token must have maturity enabled. It returns the action
// (probe/admit) and result for the dashboard confirmation line.
func (p *Pool) MaturityTouchNow(ctx context.Context, token int) (string, string, error) {
	toks := p.roster.Load()
	if toks == nil || token < 0 || token >= len(*toks) {
		return "", "", fmt.Errorf("pool: token %d out of range", token)
	}
	cfg := p.cfg.Load()
	if cfg == nil || !cfg.MaturityEnabled {
		return "", "", fmt.Errorf("pool: maturity automation is disabled (MATURITY_ENABLED=0)")
	}
	tok := (*toks)[token]
	now := time.Now()
	st := p.maturityCopy(tok)
	if !st.enabled {
		return "", "", fmt.Errorf("pool: maturity is not enabled for token %d", token)
	}
	p.clearLiftedQuarantine(tok)
	if q := tok.quarantine.Load(); q != nil {
		return "", "skip:quarantined", fmt.Errorf("pool: token %d is quarantined (%s)", token, q.reason)
	}
	rs := tok.runs.Snapshot()
	if rs.BanError != nil && (rs.BannedUntil.IsZero() || now.Before(rs.BannedUntil)) {
		return "", "skip:banned", fmt.Errorf("pool: token %d is banned", token)
	}
	if !rs.CooldownUntil.IsZero() && now.Before(rs.CooldownUntil) {
		return "", "skip:cooling", fmt.Errorf("pool: token %d is cooling down", token)
	}
	if tok.runs.CountryBlockedError() != nil {
		return "", "skip:country-blocked", fmt.Errorf("pool: token %d is country-blocked", token)
	}
	cached := tok.Streak()
	if cached == nil || now.Sub(cached.UpdatedAt) > maturityStreakFresh {
		var err error
		cached, err = p.maturityRefreshStreak(ctx, tok)
		if err != nil || cached == nil {
			p.maturityRecord(tok, "", "skip:streak-stale", "stale")
			return "", "skip:streak-stale", fmt.Errorf("pool: token %d streak unavailable", token)
		}
	}
	if cached.TodayUsed {
		p.maturityRecord(tok, "", "skip:today-used", "yes")
		return "", "skip:today-used", fmt.Errorf("pool: token %d already used today", token)
	}
	today := pacificDayKey(now)
	effective, _, _ := p.maturityResolveEffective(st, tok, cfg.MaturityTouchModel)
	p.maturityFire(ctx, cfg.MaturityDryRun, effective, token, tok, tokenEntryLabel(tok), cached, today, now)
	p.saveMaturity(token, tok)
	fin := p.maturityCopy(tok)
	return fin.lastAction, fin.lastResult, nil
}

// maturityRecord stores a skip/result marker without touching touch times.
func (p *Pool) maturityRecord(tok *tokenEntry, action, result, advanced string) {
	tok.maturityMu.Lock()
	defer tok.maturityMu.Unlock()
	if !tok.maturity.enabled {
		return
	}
	if action != "" {
		tok.maturity.lastAction = action
	}
	tok.maturity.lastResult = result
	if advanced != "" {
		tok.maturity.lastAdvanced = advanced
	}
}

// maturitySnapshot builds the dashboard view for one entry (nil until first
// enabled). Badge: Mature when the streak reached target, Warming while an
// enabled token still climbs, Cold when an enabled token sits at zero.
// SlotDay/TouchDay (one Pacific calendar shared by the whole pool) plus the
// resolved effective/auto models let the dashboard render the streak chip,
// the last-run ledger, and the Auto pick with its reason without a new
// scheduler.
func (p *Pool) maturitySnapshot(tok *tokenEntry, streak int) *MaturitySnapshot {
	tok.maturityMu.Lock()
	m := tok.maturity
	tok.maturityMu.Unlock()
	// A drafted touch-model override counts as state: operators pre-configure
	// it while disabled, and the card must echo it back after refresh.
	if !m.enabled && m.lastAction == "" && m.lastResult == "" && m.touchModel == "" {
		return nil
	}
	target := m.target
	if target <= 0 {
		target = p.maturityDefaultTarget()
	}
	badge := ""
	switch {
	case streak >= target && target > 0:
		badge = "Mature"
	case m.enabled && streak > 0:
		badge = "Warming"
	case m.enabled:
		badge = "Cold"
	}
	mode := m.mode
	if mode == "" {
		mode = MaturityModeUnmetered
	}
	global := ""
	if cfg := p.cfg.Load(); cfg != nil {
		global = cfg.MaturityTouchModel
	}
	effective, auto, reason := p.maturityResolveEffective(maturityState{enabled: m.enabled, target: m.target, mode: mode, touchModel: m.touchModel}, tok, global)
	return &MaturitySnapshot{
		Enabled:             m.enabled,
		Target:              target,
		Mode:                mode,
		TouchModel:          m.touchModel,
		Badge:               badge,
		Slot:                m.slot,
		SlotDay:             m.slotDay,
		LastTouch:           m.lastTouch,
		TouchDay:            m.touchDay,
		LastAction:          m.lastAction,
		LastResult:          m.lastResult,
		LastAdvanced:        m.lastAdvanced,
		EffectiveTouchModel: effective,
		AutoTouchModel:      auto,
		AutoTouchReason:     reason,
	}
}

// maturityWindowFor returns the nightly maintenance window for now: end is
// the upcoming Pacific midnight, start is maturityRunWindow earlier. One
// collapsed pre-reset window for every account, so touches land right
// before upstream rolls the daily streak.
//
// DST-safe: the boundary is America/Los_Angeles wall-clock midnight via
// time.Date calendar math (never a fixed UTC offset — July midnight is
// 07:00Z, January is 08:00Z). Pacific midnight never falls in a DST gap
// (LA transitions at 02:00), and AddDate re-resolves the offset for the
// new date, so 23h/25h DST days still end at the right instant.
func maturityWindowFor(now time.Time) (start, end time.Time) {
	loc := maturityLocation("America/Los_Angeles")
	y, m, d := now.In(loc).Date()
	end = time.Date(y, m, d, 0, 0, 0, 0, loc).AddDate(0, 0, 1)
	return end.Add(-maturityRunWindow), end
}

// maturityInWindow reports whether now falls inside tonight's maintenance
// window ([start, end): touches fire at/after start, never at/after the
// reset).
func maturityInWindow(now time.Time) bool {
	start, end := maturityWindowFor(now)
	return !now.Before(start) && now.Before(end)
}

// MaturityWindow exposes tonight's maintenance window for the dashboard
// countdown (absolute instants: the SPA only formats them).
func (p *Pool) MaturityWindow() (start, end time.Time) {
	return maturityWindowFor(time.Now())
}

// pacificDayKey renders the Pacific calendar day containing now
// ("2006-01-02"): slotDay/touchDay live in this day so the whole pool
// shares one pre-reset calendar.
func pacificDayKey(now time.Time) string {
	return now.In(maturityLocation("America/Los_Angeles")).Format("2006-01-02")
}

// rollMaturitySlotInWindow draws one token's staggered slot uniformly
// inside tonight's window (window start plus [0, 60m)): every enrolled
// token fires once per night at a different minute, and a restart re-roll
// can only cause one extra cheap touch (touchDay/todayUsed + the 6h
// throttle stay the idempotency bound).
func rollMaturitySlotInWindow(today string, now time.Time) (time.Time, string) {
	start, end := maturityWindowFor(now)
	if span := end.Sub(start); span > 0 {
		return start.Add(time.Duration(sessionRand() % uint64(span))), today
	}
	return start, today
}

// maturityBackedOff reports whether the nightly walk is paused after a 429.
func (p *Pool) maturityBackedOff(now time.Time) bool {
	p.maturityBackoffMu.Lock()
	defer p.maturityBackoffMu.Unlock()
	return !p.maturityBackoffUntil.IsZero() && now.Before(p.maturityBackoffUntil)
}

// maturityNoteRateLimit pauses the nightly walk after a rate-limited touch
// (429 abort+backoff): the walk stops for maturity429Backoff instead of
// hammering a throttled upstream. In-memory only — a restart clears it,
// and touchDay/todayUsed still prevent double-touches.
func (p *Pool) maturityNoteRateLimit(now time.Time) {
	p.maturityBackoffMu.Lock()
	p.maturityBackoffUntil = now.Add(maturity429Backoff)
	p.maturityBackoffMu.Unlock()
	p.logger.Warn("pool: maturity nightly walk backed off after 429",
		"backoff", maturity429Backoff.String())
}

// maturityLocation resolves the account timezone for slot math: the streak
// response's IANA name, Pacific fallback (upstream rolls its daily windows
// at Pacific midnight).
func maturityLocation(tz string) *time.Location {
	if tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		return time.UTC
	}
	return loc
}

// firstLine truncates an error for the compact last_result field.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 120 {
		return s[:120]
	}
	return s
}
