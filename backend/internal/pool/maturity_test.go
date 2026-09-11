package pool

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/modelcat"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// laDay renders the account-timezone calendar day for slot pinning (tests
// avoid the crypto-random roll by pinning slots explicitly).
func laDay(t time.Time) string {
	return t.In(maturityLocation("America/Los_Angeles")).Format("2006-01-02")
}

// newMaturityPool wires one mock token with maturity automation on.
// dryRun selects the touch ladder rung; the touch model is the unmetered
// flash row.
func newMaturityPool(t *testing.T, mock *testutil.MockUpstream, dryRun bool) *Pool {
	t.Helper()
	return newTestPoolCfg(t, func(cfg *config.Config) {
		cfg.MaturityEnabled = true
		cfg.MaturityDryRun = dryRun
		cfg.MaturityTouchModel = modelB
		cfg.MaturityTargetDays = 7
	}, mock)
}

// setMaturitySlot pins the token's slot to a fixed instant (tests avoid the
// crypto-random roll).
func setMaturitySlot(p *Pool, token int, slot time.Time, day string) {
	toks := p.roster.Load()
	e := (*toks)[token]
	e.maturityMu.Lock()
	defer e.maturityMu.Unlock()
	e.maturity.slot = slot
	e.maturity.slotDay = day
}

// maturityResult reads the token's last automation result.
func maturityResult(p *Pool, token int) (action, result string) {
	toks := p.roster.Load()
	e := (*toks)[token]
	e.maturityMu.Lock()
	defer e.maturityMu.Unlock()
	return e.maturity.lastAction, e.maturity.lastResult
}

// windowNow returns a clock inside tonight's maintenance window for firing
// tests: the real clock when already in-window with room to spare, else
// 30m before the next Pacific midnight. Test offsets (+1m/+10m ticks) stay
// inside the window either way.
func windowNow() time.Time {
	now := time.Now()
	if maturityInWindow(now) {
		if _, end := maturityWindowFor(now); now.Before(end.Add(-5 * time.Minute)) {
			return now
		}
	}
	_, end := maturityWindowFor(now)
	return end.Add(-30 * time.Minute)
}

// seedStreak caches a fresh streak reading as of now (what backfillLoop
// maintains in prod), so window-gated firing tests don't trip the
// streak-stale guard when the window clock runs ahead of wall time.
func seedStreak(p *Pool, token int, streak int, todayUsed bool, now time.Time) {
	toks := p.roster.Load()
	(*toks)[token].SetStreak(&upstream.StreakInfo{
		Streak:    streak,
		TodayUsed: todayUsed,
		TimeZone:  "America/Los_Angeles",
		UpdatedAt: now,
	})
}

func streakBody(streak int, todayUsed bool) map[string]any {
	return map[string]any{
		"streak":    streak,
		"todayUsed": todayUsed,
		"timeZone":  "America/Los_Angeles",
	}
}

// A dry-run touch probes (zero-cost) and never admits a session.
func TestMaturityDryRunProbeOnly(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newMaturityPool(t, mock, true)
	now := windowNow()
	seedStreak(p, 0, 2, false, now)
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	setMaturitySlot(p, 0, now.Add(-time.Hour), laDay(now))

	p.maturityTickAt(context.Background(), now)

	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Errorf("SessionProbes = %d, want 1 (dry-run probe)", got)
	}
	if got := mock.SessionCreatesSnapshot(); got != 0 {
		t.Errorf("SessionCreates = %d, want 0 (dry-run never admits)", got)
	}
	action, result := maturityResult(p, 0)
	if action != "probe" || result != "ok" {
		t.Errorf("last touch = %q/%q, want probe/ok", action, result)
	}
}

// Enrollment never locks: the account stays leasable in serving rotation
// while automation touches it. A live touch admits the unmetered touch
// model through the token's own session manager.
func TestMaturityEnrollStaysLeasable(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newMaturityPool(t, mock, false)
	now := windowNow()
	seedStreak(p, 0, 2, false, now)
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	if p.Snapshot()[0].Locked {
		t.Fatal("SetMaturity(true) locked the token out of serving")
	}
	// Acquire eligibility ignores maturity state: the enrolled token is
	// in the lease order.
	toks := p.roster.Load()
	order, _ := p.acquireOrder(toks, 0, modelB)
	found := false
	for _, idx := range order {
		if idx == 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("acquire order = %v, want token 0 eligible right after enroll", order)
	}
	setMaturitySlot(p, 0, now.Add(-time.Hour), laDay(now))

	p.maturityTickAt(context.Background(), now)

	if got := mock.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("SessionCreates = %d, want 1 (live unmetered admission)", got)
	}
	action, result := maturityResult(p, 0)
	if action != "admit" || result != "ok" {
		t.Errorf("last touch = %q/%q, want admit/ok", action, result)
	}
	// Still leasable after the touch: the run never locks.
	if p.Snapshot()[0].Locked {
		t.Error("live touch locked the token out of serving")
	}
}

// An operator-manually-locked token stays out of the nightly run: no touch
// fires, the ledger records skip:locked, and the lock survives the pass.
func TestMaturitySkipsLockedStaysLocked(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newMaturityPool(t, mock, false)
	now := windowNow()
	seedStreak(p, 0, 2, false, now)
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	toks := p.roster.Load()
	(*toks)[0].locked.Store(true) // operator lock, not enrollment
	setMaturitySlot(p, 0, now.Add(-time.Hour), laDay(now))

	p.maturityTickAt(context.Background(), now)

	if got := mock.SessionCreatesSnapshot(); got != 0 {
		t.Errorf("SessionCreates = %d, want 0 (locked account)", got)
	}
	if got := mock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("SessionProbes = %d, want 0 (locked account)", got)
	}
	if _, result := maturityResult(p, 0); result != "skip:locked" {
		t.Errorf("result = %q, want skip:locked", result)
	}
	snap := p.Snapshot()[0]
	if !snap.Locked {
		t.Error("run unlocked the operator-locked token")
	}
	if snap.Maturity == nil || !snap.Maturity.Enabled {
		t.Errorf("maturity snapshot = %+v, want still enabled (skip, not disable)", snap.Maturity)
	}
}

// Active days cost zero extra traffic: the upstream todayUsed flag skips
// the touch (restart-safe idempotency, upstream half).
func TestMaturitySkipsTodayUsed(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newMaturityPool(t, mock, false)
	now := windowNow()
	seedStreak(p, 0, 3, true, now)
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	setMaturitySlot(p, 0, now.Add(-time.Hour), laDay(now))

	p.maturityTickAt(context.Background(), now)

	if got := mock.SessionCreatesSnapshot(); got != 0 {
		t.Errorf("SessionCreates = %d, want 0 (today already used)", got)
	}
	if got := mock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("SessionProbes = %d, want 0 (today already used)", got)
	}
	if _, result := maturityResult(p, 0); result != "skip:today-used" {
		t.Errorf("result = %q, want skip:today-used", result)
	}
}

// Future slots wait: no touch before the jittered slot.
func TestMaturitySkipsFutureSlot(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newMaturityPool(t, mock, true)
	now := windowNow()
	seedStreak(p, 0, 2, false, now)
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	setMaturitySlot(p, 0, now.Add(10*time.Minute), laDay(now))

	p.maturityTickAt(context.Background(), now)

	if got := mock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("SessionProbes = %d, want 0 (slot in the future)", got)
	}
	if _, result := maturityResult(p, 0); result != "skip:slot" {
		t.Errorf("result = %q, want skip:slot", result)
	}
}

// Restart-safe idempotency: a reboot inside the window re-rolls the slot,
// but the persisted touchDay bounds the worst case — the second pass
// records skip:today-used instead of double-touching.
func TestMaturityRestartIdempotent(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newMaturityPool(t, mock, true)
	now := windowNow()
	seedStreak(p, 0, 2, false, now)
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	setMaturitySlot(p, 0, now.Add(-time.Hour), laDay(now))

	p.maturityTickAt(context.Background(), now)
	// Restart re-rolls the slot to "now" — touchDay must still hold.
	setMaturitySlot(p, 0, now, laDay(now))
	p.maturityTickAt(context.Background(), now.Add(time.Minute))

	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Errorf("SessionProbes = %d, want 1 (touchDay blocks the re-fire)", got)
	}
	if _, result := maturityResult(p, 0); result != "skip:today-used" {
		t.Errorf("result = %q, want skip:today-used", result)
	}
}

// The global kill-switch beats every per-token toggle.
func TestMaturityGlobalKillSwitch(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.StreakBody = streakBody(2, false)
	p := newMaturityPool(t, mock, true)
	now := time.Now()
	p.cfg.Load().MaturityEnabled = false
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	setMaturitySlot(p, 0, now.Add(-time.Hour), laDay(now))

	p.maturityTickAt(context.Background(), now)

	if got := mock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("SessionProbes = %d, want 0 (global kill-switch off)", got)
	}
	if got := mock.StreakHitsSnapshot(); got != 0 {
		t.Errorf("StreakHits = %d, want 0 (disabled pass reads nothing)", got)
	}
}

// Target reached on a healthy account disables automation and stops.
func TestMaturityAutoReleaseAtTarget(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.StreakBody = streakBody(7, false)
	p := newMaturityPool(t, mock, true)
	now := time.Now()
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	if p.Snapshot()[0].Locked {
		t.Fatal("enrolled token must stay leasable")
	}

	p.maturityTickAt(context.Background(), now)

	snap := p.Snapshot()[0]
	if snap.Locked {
		t.Error("target reached but the token is locked")
	}
	if snap.Maturity == nil || snap.Maturity.Enabled {
		t.Fatalf("maturity snapshot = %+v, want disabled after release", snap.Maturity)
		return
	}
	if snap.Maturity.Badge != "Mature" {
		t.Errorf("badge = %q, want Mature", snap.Maturity.Badge)
	}
	// Release is local state: no touch fires on the release pass.
	if got := mock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("SessionProbes = %d, want 0 (release needs no touch)", got)
	}
}

// A cooling account is never touched.
func TestMaturitySkipsCooling(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newMaturityPool(t, mock, true)
	now := windowNow()
	seedStreak(p, 0, 2, false, now)
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	setMaturitySlot(p, 0, now.Add(-time.Hour), laDay(now))
	p.CooldownToken(0, time.Until(now)+time.Hour)

	p.maturityTickAt(context.Background(), now)

	if got := mock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("SessionProbes = %d, want 0 (cooling account)", got)
	}
	if _, result := maturityResult(p, 0); result != "skip:cooling" {
		t.Errorf("result = %q, want skip:cooling", result)
	}
}

// SetMaturity rejects bad targets, unknown modes, and out-of-range
// tokens. premium-short is opt-in per token with no global gate: spend
// is bounded by the account's metered pool.
func TestSetMaturityValidation(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newMaturityPool(t, mock, true)
	if err := p.SetMaturity(0, true, 29, "", ""); err == nil {
		t.Error("target 29 accepted, want range error")
	}
	if err := p.SetMaturity(0, true, -1, "", ""); err == nil {
		t.Error("target -1 accepted, want range error")
	}
	// Target 0 is the global default (the dashboard enrolls with target 0:
	// per-account targets are gone).
	if err := p.SetMaturity(0, true, 0, "", ""); err != nil {
		t.Errorf("target 0 rejected, want global-default acceptance: %v", err)
	}
	if got := p.Snapshot()[0].Maturity.Target; got != 7 {
		t.Errorf("target-0 snapshot target = %d, want 7 (global default)", got)
	}
	if err := p.SetMaturity(0, true, 7, "turbo", ""); err == nil {
		t.Error("mode turbo accepted, want unknown-mode error")
	}
	if err := p.SetMaturity(9, true, 7, "", ""); err == nil {
		t.Error("token 9 accepted, want out-of-range error")
	}
	if err := p.SetMaturity(0, true, 7, MaturityModePremiumShort, ""); err != nil {
		t.Errorf("premium-short rejected without a global gate: %v", err)
	}
}

// A live touch on a model the account meters (price > 0, no exemption)
// skips instead of spending — maturity rides the unmetered lane (meter
// adaptation, issue #350). Dry-run probes stay exempt from the check.
func TestMaturityLiveSkipsPricedTouch(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.StreakBody = streakBody(2, false)
	p := newMaturityPool(t, mock, false)
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	// First touch admits (snapshot has no freebucks yet).
	if _, _, err := p.MaturityTouchNow(context.Background(), 0); err != nil {
		t.Fatalf("first touch: %v", err)
	}
	if got := mock.SessionCreatesSnapshot(); got != 1 {
		t.Fatalf("SessionCreates = %d, want 1", got)
	}
	// The account starts metering the touch model: inject the wire block
	// (as a compact poll would deliver it).
	toks := p.roster.Load()
	(*toks)[0].sessionMgr().UpdateQuotaFromProbe(&upstream.SessionState{
		Freebucks: &upstream.FreebucksInfo{
			Balance: 1,
			Prices:  map[string]float64{modelB: 5},
		},
	})
	// Second touch sees the metered price and skips with zero new admissions.
	action, result, _ := p.MaturityTouchNow(context.Background(), 0)
	if result != "skip:touch-priced" {
		t.Errorf("second touch = %q/%q, want skip:touch-priced", action, result)
	}
	if got := mock.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("SessionCreates = %d, want still 1 (no priced admission)", got)
	}
}

// An exempt account still admits the touch at zero balance (canStart).
func TestMaturityLiveAdmitsWhenExempt(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.StreakBody = streakBody(2, false)
	p := newMaturityPool(t, mock, false)
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	toks := p.roster.Load()
	e := (*toks)[0]
	fb := &upstream.FreebucksInfo{Balance: 0, QuotaExempt: true, Prices: map[string]float64{modelB: 5}}
	e.sessionMgr().UpdateQuotaFromProbe(&upstream.SessionState{Freebucks: fb})
	action, result, err := p.MaturityTouchNow(context.Background(), 0)
	if err != nil {
		t.Fatalf("exempt touch: %v", err)
	}
	if action != "admit" || result != "ok" {
		t.Errorf("exempt touch = %q/%q, want admit/ok", action, result)
	}
}

// Per-token touch-model override: bad shapes reject at save, the override
// rides the snapshot, and the fire path prefers it over the global
// MATURITY_TOUCH_MODEL fallback (empty = fallback). premium-short mode is
// unaffected: it always admits the shared premium pool head.
func TestSetMaturityTouchModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.StreakBody = streakBody(2, false)
	p := newMaturityPool(t, mock, false)
	premium := modelcat.SharedPremiumModels()
	if len(premium) == 0 {
		t.Fatal("no shared premium models, want at least one")
	}
	// Shape-only validation at save.
	if err := p.SetMaturity(0, true, 7, "", "not-a-model"); err == nil {
		t.Error("touch model without provider/ accepted, want shape error")
	}
	// Whitespace-only trims to the fallback.
	if err := p.SetMaturity(0, true, 7, "", "  "); err != nil {
		t.Fatalf("blank touch model: %v", err)
	}
	if got := p.Snapshot()[0].Maturity.TouchModel; got != "" {
		t.Errorf("snapshot touch_model = %q, want fallback empty", got)
	}
	// Override stored + visible on the snapshot.
	if err := p.SetMaturity(0, true, 7, "", modelB); err != nil {
		t.Fatalf("override save: %v", err)
	}
	if got := p.Snapshot()[0].Maturity.TouchModel; got != modelB {
		t.Errorf("snapshot touch_model = %q, want %q", got, modelB)
	}
	// Override wins over a hostile global: the global points at a served
	// premium model (the unmetered lane fails it closed) while the
	// per-token unmetered override still admits.
	p.cfg.Load().MaturityTouchModel = premium[0]
	action, result, err := p.MaturityTouchNow(context.Background(), 0)
	if err != nil {
		t.Fatalf("override touch: %v", err)
	}
	if action != "admit" || result != "ok" {
		t.Errorf("override touch = %q/%q, want admit/ok", action, result)
	}
	if got := mock.SessionCreatesSnapshot(); got != 1 {
		t.Fatalf("SessionCreates = %d, want 1", got)
	}
	// Reversed: a premium override fails closed even with a healthy global.
	p.cfg.Load().MaturityTouchModel = modelB
	if err := p.SetMaturity(0, true, 7, "", premium[0]); err != nil {
		t.Fatalf("premium override save: %v", err)
	}
	action, result, _ = p.MaturityTouchNow(context.Background(), 0)
	if result != "skip:touch-model" {
		t.Errorf("premium override touch = %q/%q, want skip:touch-model", action, result)
	}
	if got := mock.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("SessionCreates = %d, want still 1 (no premium admission)", got)
	}
	// Empty clears back to the fallback (and admits again off the global).
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatalf("override clear: %v", err)
	}
	if got := p.Snapshot()[0].Maturity.TouchModel; got != "" {
		t.Errorf("cleared touch_model = %q, want fallback empty", got)
	}
	action, result, err = p.MaturityTouchNow(context.Background(), 0)
	if err != nil {
		t.Fatalf("fallback touch: %v", err)
	}
	if action != "admit" || result != "ok" {
		t.Errorf("fallback touch = %q/%q, want admit/ok", action, result)
	}
	// Disabling with an override keeps it for the next enable.
	if err := p.SetMaturity(0, false, 0, "", modelB); err != nil {
		t.Fatalf("disable with override: %v", err)
	}
	if got := p.Snapshot()[0].Maturity.TouchModel; got != modelB {
		t.Errorf("disabled touch_model = %q, want kept %q", got, modelB)
	}
}

// The nightly window ends at Pacific midnight wall-clock, never a fixed
// UTC offset: July (PDT, UTC-7) ends at 07:00Z, January (PST, UTC-8) at
// 08:00Z. A fixed-offset implementation would pin one of them wrong.
func TestMaturityWindowFollowsPacific(t *testing.T) {
	// July noon Pacific: tonight's window ends at July midnight PDT.
	julyNoon := time.Date(2026, 7, 15, 12, 0, 0, 0, time.FixedZone("PDT", -7*3600))
	start, end := maturityWindowFor(julyNoon)
	if want := time.Date(2026, 7, 16, 7, 0, 0, 0, time.UTC); !end.Equal(want) {
		t.Errorf("july window end = %v, want %v (midnight PDT)", end, want)
	}
	if end.Sub(start) != maturityRunWindow {
		t.Errorf("july window length = %v, want %v", end.Sub(start), maturityRunWindow)
	}
	// January noon Pacific: tonight's window ends at January midnight PST.
	janNoon := time.Date(2026, 1, 15, 12, 0, 0, 0, time.FixedZone("PST", -8*3600))
	_, jend := maturityWindowFor(janNoon)
	if want := time.Date(2026, 1, 16, 8, 0, 0, 0, time.UTC); !jend.Equal(want) {
		t.Errorf("january window end = %v, want %v (midnight PST)", jend, want)
	}
}

// DST Sundays keep a full 60m window ending at Pacific midnight: the 23h
// spring-forward day and the 25h fall-back day both end at the right
// instant (LA transitions at 02:00, never at midnight).
func TestMaturityWindowDSTBounds(t *testing.T) {
	// Spring forward 2026-03-08 (clocks jump 02:00 → 03:00 PDT).
	springEve := time.Date(2026, 3, 7, 12, 0, 0, 0, time.FixedZone("PST", -8*3600))
	sstart, send := maturityWindowFor(springEve)
	if want := time.Date(2026, 3, 8, 8, 0, 0, 0, time.UTC); !send.Equal(want) {
		t.Errorf("spring-forward window end = %v, want %v (midnight PST→PDT day)", send, want)
	}
	if send.Sub(sstart) != maturityRunWindow {
		t.Errorf("spring-forward window length = %v, want %v", send.Sub(sstart), maturityRunWindow)
	}
	// Fall back 2026-11-01 (clocks repeat 01:00–02:00, back to PST).
	fallEve := time.Date(2026, 10, 31, 12, 0, 0, 0, time.FixedZone("PDT", -7*3600))
	fstart, fend := maturityWindowFor(fallEve)
	if want := time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC); !fend.Equal(want) {
		t.Errorf("fall-back window end = %v, want %v (midnight PDT→PST day)", fend, want)
	}
	if fend.Sub(fstart) != maturityRunWindow {
		t.Errorf("fall-back window length = %v, want %v", fend.Sub(fstart), maturityRunWindow)
	}
	// In-window membership brackets the 60m exactly.
	if !maturityInWindow(sstart) || !maturityInWindow(send.Add(-time.Second)) {
		t.Error("window edges not in-window (start inclusive, end-exclusive)")
	}
	if maturityInWindow(sstart.Add(-time.Second)) || maturityInWindow(send) {
		t.Error("outside instants report in-window")
	}
}

// Slots roll inside tonight's window: staggered across the 60m, never at
// or past the reset.
func TestMaturitySlotRollsInWindow(t *testing.T) {
	now := windowNow()
	start, end := maturityWindowFor(now)
	for range 25 {
		slot, day := rollMaturitySlotInWindow(pacificDayKey(now), now)
		if slot.Before(start) || !slot.Before(end) {
			t.Fatalf("slot = %v, want within [%v, %v)", slot, start, end)
		}
		if day != pacificDayKey(now) {
			t.Fatalf("slot day = %q, want %q", day, pacificDayKey(now))
		}
	}
}

// The daily tick fails a misconfigured touch model closed before any
// upstream admission: zero session creates and zero probes on both a served
// premium global and an unserved global (no per-token override set).
func TestMaturityTickGuardsTouchModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newMaturityPool(t, mock, false)
	now := windowNow()
	seedStreak(p, 0, 2, false, now)
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	premium := modelcat.SharedPremiumModels()
	if len(premium) == 0 {
		t.Fatal("no shared premium models, want at least one")
	}
	p.cfg.Load().MaturityTouchModel = premium[0]
	setMaturitySlot(p, 0, now.Add(-time.Hour), laDay(now))

	p.maturityTickAt(context.Background(), now)

	if _, result := maturityResult(p, 0); result != "skip:touch-model" {
		t.Errorf("premium-global tick result = %q, want skip:touch-model", result)
	}
	if got := mock.SessionCreatesSnapshot(); got != 0 {
		t.Errorf("SessionCreates = %d, want 0 (no premium admission)", got)
	}
	if got := mock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("SessionProbes = %d, want 0 (guarded before probe)", got)
	}
	// Unserved globals fail closed the same way (skips never arm the 6h
	// throttle, so the second tick evaluates fresh).
	p.cfg.Load().MaturityTouchModel = "nope/nothing"
	p.maturityTickAt(context.Background(), now)
	if _, result := maturityResult(p, 0); result != "skip:touch-model" {
		t.Errorf("unserved-global tick result = %q, want skip:touch-model", result)
	}
	if got := mock.SessionCreatesSnapshot(); got != 0 {
		t.Errorf("SessionCreates = %d, want still 0", got)
	}
}

// Outside the nightly window nothing fires and the last-run ledger is
// untouched: bookkeeping (release/advance) stays fresh, but skips never
// overwrite last night's outcome with all-day spam.
func TestMaturityOutsideWindowNoFire(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newMaturityPool(t, mock, true)
	start, _ := maturityWindowFor(time.Now())
	now := start.Add(-2 * time.Hour) // provably outside any window
	seedStreak(p, 0, 2, false, now)
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	setMaturitySlot(p, 0, now.Add(-time.Hour), pacificDayKey(now))

	p.maturityTickAt(context.Background(), now)

	if got := mock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("SessionProbes = %d, want 0 (outside the window)", got)
	}
	if got := mock.SessionCreatesSnapshot(); got != 0 {
		t.Errorf("SessionCreates = %d, want 0 (outside the window)", got)
	}
	if action, result := maturityResult(p, 0); action != "" || result != "" {
		t.Errorf("ledger = %q/%q, want untouched outside the window", action, result)
	}
}

// Client traffic since the last Pacific reset skips the nightly touch: the
// account is already alive today. The signal is the local Pacific-day
// ledger, fed ONLY by the Chat success path — internal probes and warming
// touches never record here, so automation can never self-skip.
func TestMaturitySkipsClientActive(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newMaturityPool(t, mock, false)
	now := windowNow()
	seedStreak(p, 0, 2, false, now)
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	setMaturitySlot(p, 0, now.Add(-time.Hour), laDay(now))
	// One successful client chat today (real-now ledger: same Pacific day
	// as tonight's window by construction).
	toks := p.roster.Load()
	p.recordChatEntry((*toks)[0])

	p.maturityTickAt(context.Background(), now)

	if got := mock.SessionCreatesSnapshot(); got != 0 {
		t.Errorf("SessionCreates = %d, want 0 (client-active account)", got)
	}
	if got := mock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("SessionProbes = %d, want 0 (client-active account)", got)
	}
	if _, result := maturityResult(p, 0); result != "skip:client-active" {
		t.Errorf("result = %q, want skip:client-active", result)
	}
}

// A warming touch never counts as client activity: dry-run probes and live
// touches bypass the Chat ledger, so a touched account is still eligible
// tomorrow (only touchDay/todayUsed gate the same night).
func TestMaturityTouchNotClientActivity(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newMaturityPool(t, mock, false)
	now := windowNow()
	seedStreak(p, 0, 2, false, now)
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	setMaturitySlot(p, 0, now.Add(-time.Hour), laDay(now))

	p.maturityTickAt(context.Background(), now)

	if got := mock.SessionCreatesSnapshot(); got != 1 {
		t.Fatalf("SessionCreates = %d, want 1 (live touch fired)", got)
	}
	if got := p.dayRequestCount(0); got != 0 {
		t.Errorf("dayRequestCount = %d, want 0 (touches never feed the activity ledger)", got)
	}
}

// A rate-limited touch aborts the nightly walk and backs off: the next
// token stays untouched and no retry fires inside the backoff window.
func TestMaturityRateLimitAbortsWalk(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	var posts atomic.Int64
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"status":"rate_limited","limit":3,"recentCount":3,"period":"pacific_day","retryAfterMs":900000}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"active"}`)
	}
	p := newTestPoolCfg(t, func(cfg *config.Config) {
		cfg.MaturityEnabled = true
		cfg.MaturityDryRun = false
		cfg.MaturityTouchModel = modelB
		cfg.MaturityTargetDays = 7
	}, mock, mock)
	now := windowNow()
	for i := range 2 {
		seedStreak(p, i, 2, false, now)
		if err := p.SetMaturity(i, true, 7, "", ""); err != nil {
			t.Fatal(err)
		}
		setMaturitySlot(p, i, now.Add(-time.Hour), laDay(now))
	}

	p.maturityTickAt(context.Background(), now)

	if got := posts.Load(); got != 1 {
		t.Errorf("session POSTs = %d, want 1 (walk aborts on first 429)", got)
	}
	if _, result := maturityResult(p, 0); !strings.HasPrefix(result, "error:") {
		t.Errorf("token0 result = %q, want 429 error", result)
	}
	if action, result := maturityResult(p, 1); action != "" || result != "" {
		t.Errorf("token1 ledger = %q/%q, want untouched (walk aborted)", action, result)
	}
	// Inside the backoff the walk stays paused (token1 would otherwise be
	// due immediately: its slot is already past).
	p.maturityTickAt(context.Background(), now.Add(time.Minute))
	if got := posts.Load(); got != 1 {
		t.Errorf("session POSTs = %d, want still 1 (429 backoff holds)", got)
	}
}
