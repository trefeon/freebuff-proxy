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
	// MaturityModePremiumShort admits one short premium-pool session
	// instead. It burns daily premium quota and stays gated behind
	// MATURITY_ALLOW_PREMIUM plus an explicit per-token opt-in.
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
)

// MaturitySnapshot is the dashboard-ready per-token maturity view. Nil on
// TokenSnapshot until maturity is first enabled for the token, so tokens
// that never opt in carry no new payload.
type MaturitySnapshot struct {
	Enabled       bool      `json:"enabled"`
	Target        int       `json:"target"`
	Mode          string    `json:"mode"`
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
	enabled          bool
	target           int
	mode             string
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
}

// SetMaturity enables or disables streak-maturity automation for token.
// Enabling also applies the administrative lock so the warming account
// leaves serving rotation until its streak reaches target and auto-releases;
// disabling never unlocks (the operator decides when a token serves again).
// target <= 0 falls back to the configured MATURITY_TARGET_DAYS default;
// mode "" means unmetered. mode premium-short requires MATURITY_ALLOW_PREMIUM.
func (p *Pool) SetMaturity(token int, enabled bool, target int, mode string) error {
	toks := p.roster.Load()
	if toks == nil || token < 0 || token >= len(*toks) {
		return fmt.Errorf("pool: token %d out of range", token)
	}
	cfg := p.cfg.Load()
	if mode == "" {
		mode = MaturityModeUnmetered
	}
	if mode != MaturityModeUnmetered && mode != MaturityModePremiumShort {
		return fmt.Errorf("pool: unknown maturity mode %q (want %q or %q)", mode, MaturityModeUnmetered, MaturityModePremiumShort)
	}
	if mode == MaturityModePremiumShort && (cfg == nil || !cfg.MaturityAllowPremium) {
		return fmt.Errorf("pool: maturity mode %q requires MATURITY_ALLOW_PREMIUM=1", MaturityModePremiumShort)
	}
	if enabled && target <= 0 {
		target = p.maturityDefaultTarget()
	}
	if target < 0 || target > 28 {
		return fmt.Errorf("pool: maturity target %d out of range (want 1..28)", target)
	}
	tok := (*toks)[token]
	tok.maturityMu.Lock()
	defer tok.maturityMu.Unlock()
	tok.maturity.enabled = enabled
	if enabled {
		tok.maturity.target = target
		tok.maturity.mode = mode
		tok.maturity.warn = false
		tok.maturity.noAdvanceDays = 0
		tok.maturity.lastNoAdvanceDay = ""
		// Warming accounts leave rotation immediately; the streak target
		// auto-releases the lock later.
		tok.locked.Store(true)
		if tok.maturity.slot.IsZero() {
			tok.maturity.slot, tok.maturity.slotDay = p.rollMaturitySlot(tok.maturity.slotDay, time.Now())
		}
	}
	return nil
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
	st := p.maturityCopy(tok)
	if !st.enabled || st.warn {
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
	p.maturityAccountAdvance(tok, cached, now)

	// Auto-release: target reached on a healthy account. This is a local
	// state change (no upstream cost) so it runs in dry-run mode too.
	if cached.Streak >= target {
		tok.locked.Store(false)
		tok.maturityMu.Lock()
		tok.maturity.enabled = false
		tok.maturity.lastResult = "released:mature"
		tok.maturity.lastAdvanced = "yes"
		tok.maturity.lastStreak = cached.Streak
		tok.maturityMu.Unlock()
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

	p.maturityFire(ctx, dryRun, touchModel, idx, tok, label, cached, today, now)
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
func (p *Pool) maturityAccountAdvance(tok *tokenEntry, cached *upstream.StreakInfo, now time.Time) {
	tok.maturityMu.Lock()
	defer tok.maturityMu.Unlock()
	m := &tok.maturity
	if m.streakAtTouch > 0 && cached.Streak > m.streakAtTouch {
		m.noAdvanceDays = 0
		m.lastAdvanced = "yes"
		m.streakAtTouch = 0
		m.lastStreak = cached.Streak
		return
	}
	m.lastStreak = cached.Streak
	if m.streakAtTouch > 0 && !m.lastTouch.IsZero() && now.Sub(m.lastTouch) > 20*time.Hour && cached.Streak <= m.streakAtTouch {
		day := now.UTC().Format("2006-01-02")
		if m.lastNoAdvanceDay != day {
			m.lastNoAdvanceDay = day
			m.noAdvanceDays++
			m.lastAdvanced = "no"
			if m.noAdvanceDays >= maturityNoAdvanceLimit {
				m.warn = true
			}
		}
	}
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
			return
		}
		model = premium[0]
	} else if !modelcat.IsServed(model) || modelcat.IsPremium(model) {
		p.logger.Warn("pool: maturity touch misconfigured (not a served unmetered model), skipping",
			"token", idx+1, "token_label", label, "model", model)
		p.maturityRecord(tok, "admit", "skip:touch-model", "")
		return
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
	p.maturityFire(ctx, cfg.MaturityDryRun, cfg.MaturityTouchModel, token, tok, tokenEntryLabel(tok), cached, now.In(loc).Format("2006-01-02"), now)
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

// rollMaturitySlot draws today's uniform slot for a token whose stored day
// is stale: midnight in the account timezone plus uniform [0, 24h).
func (p *Pool) rollMaturitySlot(staleDay string, now time.Time) (time.Time, string) {
	return rollMaturitySlotFor(staleDay, now, time.UTC)
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
