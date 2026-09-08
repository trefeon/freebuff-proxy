// maturity.go — streak-maturity automation (docs/maturity-plan.md PR2,
// preserve-only v1).
//
// A token with maturity enabled is kept out of serving rotation by the
// administrative lock (SetMaturity locks on enable) while a daily low-cost
// touch keeps its streak alive. When the cached streak reaches the token's
// target the lock auto-releases and automation disables itself.
//
// Safety posture (plan §4): global kill-switch (MATURITY_ENABLED, default
// off), dry-run default (probe-only, zero session slots claimed), unmetered
// touch models only (never burns premium quota), jittered per-token daily
// slots in the account's own timezone, restart-safe 6h throttle, and an
// effectiveness loop that stops firing after 3 consecutive non-advancing
// days with a warning badge. The scheduler rides the 60s maintainTick pass —
// no new goroutine — and never touches quarantined, banned, cooling, or
// country-blocked accounts.
package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"freebuff-proxy/backend/internal/modelcat"
	"freebuff-proxy/backend/internal/upstream"
)

// Maturity touch modes.
const (
	// MaturityModeUnmetered admits the configured unmetered model
	// (MATURITY_TOUCH_MODEL): reservation against an infinite limit, free,
	// plus a live session real traffic can reuse.
	MaturityModeUnmetered = "unmetered"
	// MaturityModePremiumShort admits one short session on a premium row
	// instead. It spends from the account's metered pool and stays
	// opt-in per token.
	MaturityModePremiumShort = "premium-short"
)

const (
	// maturityThrottle is the restart-safe minimum gap between two touches
	// on one token: a restart re-rolls the day's slot, and todayUsed plus
	// this throttle bound the worst case to one extra cheap touch.
	maturityThrottle = 6 * time.Hour
	// maturityStreakFresh bounds streak-cache age for touch decisions: the
	// number moves daily, and a touch must never fire blind off stale data.
	maturityStreakFresh = time.Hour
	// maturityNoAdvanceLimit stops firing after this many consecutive
	// touches with no observed streak advance (anti-blind-running loop).
	maturityNoAdvanceLimit = 3
	// maturityRelockDays re-locks a released token after this many
	// consecutive post-release days with the streak below its release
	// target (one bad day is noise; two is a lapsed streak).
	maturityRelockDays = 2
)

// MaturitySnapshot is the dashboard-ready per-token maturity view. Nil on
// TokenSnapshot until maturity is first enabled for the token, so tokens
// that never opt in carry no new payload.
type MaturitySnapshot struct {
	Enabled bool   `json:"enabled"`
	Target  int    `json:"target"`
	Mode    string `json:"mode"`
	// TouchModel is the per-token touch-model override ("" = the global
	// MATURITY_TOUCH_MODEL fallback). Omitted on the wire when unset so
	// never-enrolled tokens keep their existing payload shape.
	TouchModel    string    `json:"touch_model,omitempty"`
	Badge         string    `json:"badge"`
	Slot          time.Time `json:"slot,omitempty"`
	LastTouch     time.Time `json:"last_touch,omitempty"`
	LastAction    string    `json:"last_action,omitempty"`
	LastResult    string    `json:"last_result,omitempty"`
	LastAdvanced  string    `json:"last_advanced,omitempty"`
	Warn          bool      `json:"warn,omitempty"`
	NoAdvanceDays int       `json:"no_advance_days,omitempty"`
}

// maturityState is the mutable per-token automation state, guarded by
// tokenEntry.maturityMu. Zero value = disabled.
type maturityState struct {
	enabled bool
	target  int
	mode    string
	// touchModel overrides the global MATURITY_TOUCH_MODEL for this
	// token only. Empty means "use the global fallback".
	touchModel       string
	slot             time.Time
	slotDay          string
	lastTouch        time.Time
	lastAction       string
	lastResult       string
	lastAdvanced     string
	lastStreak       int
	streakAtTouch    int
	touchDay         string
	noAdvanceDays    int
	lastNoAdvanceDay string
	warn             bool
	// releasedTarget is the streak target at the last auto-release (0 =
	// never released). A released token whose streak later drops below
	// this target re-locks after belowTargetDays consecutive days.
	releasedTarget int
	// belowTargetDays counts consecutive post-release days with the
	// streak below releasedTarget (once per calendar day, like
	// noAdvanceDays); lastBelowDay is the last counted day.
	belowTargetDays int
	lastBelowDay    string
}

// maturityPersisted is the JSON-stable mirror of maturityState for the
// maturity_json blob (DB column, not wire: field names stay snake_case and
// additive — old rows must still unmarshal after new counters land).
type maturityPersisted struct {
	Enabled          bool      `json:"enabled"`
	Target           int       `json:"target"`
	Mode             string    `json:"mode"`
	TouchModel       string    `json:"touch_model,omitempty"`
	Slot             time.Time `json:"slot,omitempty"`
	SlotDay          string    `json:"slot_day,omitempty"`
	LastTouch        time.Time `json:"last_touch,omitempty"`
	LastAction       string    `json:"last_action,omitempty"`
	LastResult       string    `json:"last_result,omitempty"`
	LastAdvanced     string    `json:"last_advanced,omitempty"`
	LastStreak       int       `json:"last_streak,omitempty"`
	StreakAtTouch    int       `json:"streak_at_touch,omitempty"`
	TouchDay         string    `json:"touch_day,omitempty"`
	NoAdvanceDays    int       `json:"no_advance_days,omitempty"`
	LastNoAdvanceDay string    `json:"last_no_advance_day,omitempty"`
	Warn             bool      `json:"warn,omitempty"`
	ReleasedTarget   int       `json:"released_target,omitempty"`
	BelowTargetDays  int       `json:"below_target_days,omitempty"`
	LastBelowDay     string    `json:"last_below_day,omitempty"`
}

func (m maturityState) marshalMaturity() (string, error) {
	raw, err := json.Marshal(maturityPersisted{
		Enabled:          m.enabled,
		Target:           m.target,
		Mode:             m.mode,
		TouchModel:       m.touchModel,
		Slot:             m.slot,
		SlotDay:          m.slotDay,
		LastTouch:        m.lastTouch,
		LastAction:       m.lastAction,
		LastResult:       m.lastResult,
		LastAdvanced:     m.lastAdvanced,
		LastStreak:       m.lastStreak,
		StreakAtTouch:    m.streakAtTouch,
		TouchDay:         m.touchDay,
		NoAdvanceDays:    m.noAdvanceDays,
		LastNoAdvanceDay: m.lastNoAdvanceDay,
		Warn:             m.warn,
		ReleasedTarget:   m.releasedTarget,
		BelowTargetDays:  m.belowTargetDays,
		LastBelowDay:     m.lastBelowDay,
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
		enabled:          stored.Enabled,
		target:           stored.Target,
		mode:             stored.Mode,
		touchModel:       stored.TouchModel,
		slot:             stored.Slot,
		slotDay:          stored.SlotDay,
		lastTouch:        stored.LastTouch,
		lastAction:       stored.LastAction,
		lastResult:       stored.LastResult,
		lastAdvanced:     stored.LastAdvanced,
		lastStreak:       stored.LastStreak,
		streakAtTouch:    stored.StreakAtTouch,
		touchDay:         stored.TouchDay,
		noAdvanceDays:    stored.NoAdvanceDays,
		lastNoAdvanceDay: stored.LastNoAdvanceDay,
		warn:             stored.Warn,
		releasedTarget:   stored.ReleasedTarget,
		belowTargetDays:  stored.BelowTargetDays,
		lastBelowDay:     stored.LastBelowDay,
	}, nil
}

// SetMaturity enables or disables streak-maturity automation for token.
// Enabling also applies the administrative lock so the warming account
// leaves serving rotation until its streak reaches target and auto-releases;
// disabling never unlocks (the operator decides when a token serves again).
// target <= 0 falls back to the configured MATURITY_TARGET_DAYS default;
// mode "" means unmetered. mode premium-short spends from the account's
// metered pool and stays opt-in per token.
// touchModel is the per-token touch-model override; "" keeps the global
// MATURITY_TOUCH_MODEL fallback. A non-empty value must be a provider/model
// id (shape only — served/unmetered semantics stay in the fire path, which
// fails closed on misconfigured models). The override is stored on disable
// too, so re-enabling restores it.
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
	if touchModel != "" && !strings.Contains(touchModel, "/") {
		return fmt.Errorf("pool: maturity touch model %q must be a provider/model id (e.g. deepseek/deepseek-v4-flash)", touchModel)
	}
	if enabled && target <= 0 {
		target = p.maturityDefaultTarget()
	}
	if target < 0 || target > 28 {
		return fmt.Errorf("pool: maturity target %d out of range (want 1..28)", target)
	}
	tok := (*toks)[token]
	tok.maturityMu.Lock()
	tok.maturity.enabled = enabled
	tok.maturity.touchModel = touchModel
	if enabled {
		tok.maturity.target = target
		tok.maturity.mode = mode
		tok.maturity.warn = false
		tok.maturity.noAdvanceDays = 0
		tok.maturity.lastNoAdvanceDay = ""
		tok.maturity.releasedTarget = 0
		tok.maturity.belowTargetDays = 0
		tok.maturity.lastBelowDay = ""
		// Warming accounts leave rotation immediately; the streak target
		// auto-releases the lock later.
		tok.locked.Store(true)
		if tok.maturity.slot.IsZero() {
			var tz string
			if cached := tok.Streak(); cached != nil {
				tz = cached.TimeZone
			}
			tok.maturity.slot, tok.maturity.slotDay = seedMaturitySlot(tok.maturity.slotDay, time.Now(), tz)
		}
	} else {
		// Disabling drops the release watch too: a manually disabled
		// token must never re-lock behind the operator's back.
		tok.maturity.releasedTarget = 0
		tok.maturity.belowTargetDays = 0
		tok.maturity.lastBelowDay = ""
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

// maturityEffectiveModel resolves the touch model for one token: the
// per-token override when set, else the global MATURITY_TOUCH_MODEL
// fallback. An empty result means "no configured model", which the fire
// path fails closed on (skip:touch-model).
func maturityEffectiveModel(st maturityState, global string) string {
	if st.touchModel != "" {
		return st.touchModel
	}
	return global
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
func (p *Pool) maturityTickAt(ctx context.Context, now time.Time) {
	cfg := p.cfg.Load()
	if cfg == nil || !cfg.MaturityEnabled {
		return
	}
	toks := p.roster.Load()
	if toks == nil {
		return
	}
	for i, tok := range *toks {
		p.maturityTickOne(ctx, cfg.MaturityDryRun, cfg.MaturityTouchModel, i, tok, now)
	}
}

// maturityTickOne evaluates and possibly fires one token's daily touch.
func (p *Pool) maturityTickOne(ctx context.Context, dryRun bool, touchModel string, idx int, tok *tokenEntry, now time.Time) {
	// Persist-on-exit: every mutation below (skips, touches, release,
	// warning, relock watch) lands in the maturity_json blob on the way
	// out. Never-enrolled tokens no-op inside saveMaturity.
	defer p.saveMaturity(idx, tok)
	st := p.maturityCopy(tok)
	if !st.enabled {
		// Released-token watch: a Mature account whose streak later
		// drops re-locks after maturityRelockDays consecutive below days.
		p.maturityRelockWatch(ctx, idx, tok, now)
		return
	}
	if st.warn {
		return
	}
	label := tokenEntryLabel(tok)

	// Health gates: quarantined, banned, cooling, or country-blocked
	// accounts are never touched — automation must not poke an account
	// upstream already flagged.
	p.clearLiftedQuarantine(tok)
	if q := tok.quarantine.Load(); q != nil {
		p.maturityRecord(tok, "", "skip:quarantined", "")
		return
	}
	rs := tok.runs.Snapshot()
	if rs.BanError != nil && (rs.BannedUntil.IsZero() || now.Before(rs.BannedUntil)) {
		p.maturityRecord(tok, "", "skip:banned", "")
		return
	}
	if !rs.CooldownUntil.IsZero() && now.Before(rs.CooldownUntil) {
		p.maturityRecord(tok, "", "skip:cooling", "")
		return
	}
	if tok.runs.CountryBlockedError() != nil {
		p.maturityRecord(tok, "", "skip:country-blocked", "")
		return
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
			p.maturityRecord(tok, "", "skip:streak-stale", "stale")
			return
		}
	}

	target := st.target
	if target <= 0 {
		target = p.maturityDefaultTarget()
	}

	// Effectiveness accounting: a touch that moved the streak resets the
	// no-advance counter; a touch older than 20h with no movement counts
	// one non-advancing day (once per calendar day).
	p.maturityAccountAdvance(idx, tok, cached, now)

	// Auto-release: target reached on a healthy account. This is a local
	// state change (no upstream cost) so it runs in dry-run mode too.
	if cached.Streak >= target {
		tok.locked.Store(false)
		tok.maturityMu.Lock()
		tok.maturity.enabled = false
		tok.maturity.lastResult = "released:mature"
		tok.maturity.lastAdvanced = "yes"
		tok.maturity.lastStreak = cached.Streak
		tok.maturity.releasedTarget = target
		tok.maturity.belowTargetDays = 0
		tok.maturity.lastBelowDay = ""
		tok.maturityMu.Unlock()
		p.emitMaturity(idx, "release", fmt.Sprintf("streak=%d target=%d", cached.Streak, target))
		p.logger.Info("pool: maturity target reached, token auto-released",
			"token", idx+1, "token_label", label, "streak", cached.Streak, "target", target)
		return
	}

	// Active days cost zero extra traffic: real usage (or an earlier
	// firing) sets todayUsed, making the day indistinguishable from
	// human use.
	if cached.TodayUsed {
		tok.maturityMu.Lock()
		tok.maturity.lastStreak = cached.Streak
		tok.maturity.lastAdvanced = "yes"
		if tok.maturity.lastResult == "" || strings.HasPrefix(tok.maturity.lastResult, "skip:") {
			tok.maturity.lastResult = "skip:today-used"
		}
		tok.maturityMu.Unlock()
		return
	}

	// Daily slot in the account's own timezone, re-rolled every day and at
	// boot (restart-safe via todayUsed + the 6h throttle).
	loc := maturityLocation(cached.TimeZone)
	today := now.In(loc).Format("2006-01-02")
	tok.maturityMu.Lock()
	if tok.maturity.slot.IsZero() || tok.maturity.slotDay != today {
		tok.maturity.slot, tok.maturity.slotDay = rollMaturitySlotFor(today, now, loc)
	}
	slot := tok.maturity.slot
	lastTouch := tok.maturity.lastTouch
	tok.maturityMu.Unlock()
	if now.Before(slot) {
		p.maturityRecord(tok, "", "skip:slot", "")
		return
	}
	if !lastTouch.IsZero() && now.Sub(lastTouch) < maturityThrottle {
		p.maturityRecord(tok, "", "skip:throttle", "")
		return
	}

	// Per-token override wins; empty falls back to the global
	// MATURITY_TOUCH_MODEL passed in from the tick.
	p.maturityFire(ctx, dryRun, maturityEffectiveModel(st, touchModel), idx, tok, label, cached, today, now)
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

// maturityAccountAdvance maintains the anti-blind-running counters against
// the fresh streak reading.
func (p *Pool) maturityAccountAdvance(idx int, tok *tokenEntry, cached *upstream.StreakInfo, now time.Time) {
	tok.maturityMu.Lock()
	advanced := false
	warned := false
	noAdvanceDays := 0
	m := &tok.maturity
	if m.streakAtTouch > 0 && cached.Streak > m.streakAtTouch {
		m.noAdvanceDays = 0
		m.lastAdvanced = "yes"
		m.streakAtTouch = 0
		m.lastStreak = cached.Streak
		advanced = true
	} else {
		m.lastStreak = cached.Streak
		if m.streakAtTouch > 0 && !m.lastTouch.IsZero() && now.Sub(m.lastTouch) > 20*time.Hour && cached.Streak <= m.streakAtTouch {
			day := now.UTC().Format("2006-01-02")
			if m.lastNoAdvanceDay != day {
				m.lastNoAdvanceDay = day
				m.noAdvanceDays++
				m.lastAdvanced = "no"
				if m.noAdvanceDays >= maturityNoAdvanceLimit && !m.warn {
					m.warn = true
					warned = true
				}
				noAdvanceDays = m.noAdvanceDays
			}
		}
	}
	tok.maturityMu.Unlock()
	// History emits happen outside the token mutex: the sink must never run
	// under pool locks.
	if advanced {
		p.emitMaturity(idx, "advance", fmt.Sprintf("streak=%d", cached.Streak))
	}
	if warned {
		p.emitMaturity(idx, "warn", fmt.Sprintf("no_advance_days=%d", noAdvanceDays))
	}
}

// maturityRelockWatch tracks released tokens: a Mature account whose streak
// later drops below its release target re-locks for warming after
// maturityRelockDays consecutive below days (the counter lives in the
// persisted blob, so restarts never reset the episode). Recovery at or above
// target resets the counter. Flagged accounts (quarantine/ban/cooling/
// country-block) are left alone, and a manual disable clears the watch in
// SetMaturity, so this never re-locks behind the operator.
func (p *Pool) maturityRelockWatch(ctx context.Context, idx int, tok *tokenEntry, now time.Time) {
	tok.maturityMu.Lock()
	target := tok.maturity.releasedTarget
	tok.maturityMu.Unlock()
	if target <= 0 {
		return
	}
	label := tokenEntryLabel(tok)
	p.clearLiftedQuarantine(tok)
	if q := tok.quarantine.Load(); q != nil {
		return
	}
	rs := tok.runs.Snapshot()
	if rs.BanError != nil && (rs.BannedUntil.IsZero() || now.Before(rs.BannedUntil)) {
		return
	}
	if !rs.CooldownUntil.IsZero() && now.Before(rs.CooldownUntil) {
		return
	}
	if tok.runs.CountryBlockedError() != nil {
		return
	}
	cached := tok.Streak()
	if cached == nil || now.Sub(cached.UpdatedAt) > maturityStreakFresh {
		var err error
		cached, err = p.maturityRefreshStreak(ctx, tok)
		if err != nil || cached == nil {
			return
		}
	}
	tok.maturityMu.Lock()
	m := &tok.maturity
	relock := false
	if cached.Streak >= m.releasedTarget && m.releasedTarget > 0 {
		m.belowTargetDays = 0
		m.lastBelowDay = ""
		m.lastStreak = cached.Streak
	} else {
		m.lastStreak = cached.Streak
		day := now.UTC().Format("2006-01-02")
		if m.lastBelowDay != day {
			m.lastBelowDay = day
			m.belowTargetDays++
			if m.belowTargetDays >= maturityRelockDays {
				m.enabled = true
				m.belowTargetDays = 0
				m.lastBelowDay = ""
				relock = true
			}
		}
	}
	tok.maturityMu.Unlock()
	if !relock {
		return
	}
	tok.locked.Store(true)
	p.emitMaturity(idx, "relock", fmt.Sprintf("streak=%d target=%d", cached.Streak, target))
	p.logger.Info("pool: maturity streak lapsed, token re-locked for warming",
		"token", idx+1, "token_label", label, "streak", cached.Streak, "target", target)
}

// maturityFire performs one touch: dry-run probes (zero-cost, never claims
// a slot); live mode admits the touch model through the token's own session
// manager — wire-identical to a user opening the CLI.
func (p *Pool) maturityFire(ctx context.Context, dryRun bool, touchModel string, idx int, tok *tokenEntry, label string, cached *upstream.StreakInfo, today string, now time.Time) {
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
		p.logger.Warn("pool: maturity touch failed", "token", idx+1, "token_label", label, "action", action, "err", err)
		return
	}
	p.logger.Info("pool: maturity touch fired", "token", idx+1, "token_label", label,
		"action", action, "model", model, "streak", cached.Streak)
	if tok.maturityWarned() {
		p.logger.Warn("pool: maturity touch is not advancing the streak — escalate or disable",
			"token", idx+1, "token_label", label, "no_advance_days", tok.maturityNoAdvance())
	}
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

// MaturityTouchNow fires one manual maturity touch outside the daily slot
// (dashboard validation lever for the §3 ladder experiment). Slot wait and
// 6h throttle are bypassed; health gates, streak freshness, and todayUsed
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
	if st.warn {
		return "", "", fmt.Errorf("pool: maturity stopped for token %d (touch is not advancing the streak — escalate or disable)", token)
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
	loc := maturityLocation(cached.TimeZone)
	p.maturityFire(ctx, cfg.MaturityDryRun, maturityEffectiveModel(st, cfg.MaturityTouchModel), token, tok, tokenEntryLabel(tok), cached, now.In(loc).Format("2006-01-02"), now)
	p.saveMaturity(token, tok)
	fin := p.maturityCopy(tok)
	return fin.lastAction, fin.lastResult, nil
}

// maturityWarned reports the warning flag (log call sites must not hold the
// mutex while logging).
func (e *tokenEntry) maturityWarned() bool {
	e.maturityMu.Lock()
	defer e.maturityMu.Unlock()
	return e.maturity.warn
}

// maturityNoAdvance returns the non-advancing day count.
func (e *tokenEntry) maturityNoAdvance() int {
	e.maturityMu.Lock()
	defer e.maturityMu.Unlock()
	return e.maturity.noAdvanceDays
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
func (p *Pool) maturitySnapshot(tok *tokenEntry, streak int) *MaturitySnapshot {
	tok.maturityMu.Lock()
	defer tok.maturityMu.Unlock()
	m := tok.maturity
	if !m.enabled && m.lastAction == "" && m.lastResult == "" {
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
	return &MaturitySnapshot{
		Enabled:       m.enabled,
		Target:        target,
		Mode:          mode,
		TouchModel:    m.touchModel,
		Badge:         badge,
		Slot:          m.slot,
		LastTouch:     m.lastTouch,
		LastAction:    m.lastAction,
		LastResult:    m.lastResult,
		LastAdvanced:  m.lastAdvanced,
		Warn:          m.warn,
		NoAdvanceDays: m.noAdvanceDays,
	}
}

// seedMaturitySlot draws the enable-time slot in the account's own timezone
// (midnight plus uniform [0, 24h)): the cached streak's IANA zone when known,
// else the shared maturityLocation fallback chain. Seeding in UTC would pin
// the wrong calendar day for accounts east/west of UTC.
func seedMaturitySlot(staleDay string, now time.Time, tz string) (time.Time, string) {
	return rollMaturitySlotFor(staleDay, now, maturityLocation(tz))
}

func rollMaturitySlotFor(staleDay string, now time.Time, loc *time.Location) (time.Time, string) {
	today := now.In(loc).Format("2006-01-02")
	y, m, d := now.In(loc).Date()
	jitter := time.Duration(sessionRand() % uint64(24*time.Hour))
	return time.Date(y, m, d, 0, 0, 0, 0, loc).Add(jitter), today
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
