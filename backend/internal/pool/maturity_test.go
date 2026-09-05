package pool

import (
	"context"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
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
	mock.StreakBody = streakBody(2, false)
	p := newMaturityPool(t, mock, true)
	now := time.Now()
	if err := p.SetMaturity(0, true, 7, ""); err != nil {
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

// A live touch admits the unmetered touch model through the token's own
// session manager — even though the warming token is locked out of serving.
func TestMaturityLiveAdmitsWhileLocked(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.StreakBody = streakBody(2, false)
	p := newMaturityPool(t, mock, false)
	now := time.Now()
	if err := p.SetMaturity(0, true, 7, ""); err != nil {
		t.Fatal(err)
	}
	if !p.Snapshot()[0].Locked {
		t.Fatal("SetMaturity(true) did not lock the token out of serving")
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
	// The lock still holds: warming accounts never serve client traffic.
	if !p.Snapshot()[0].Locked {
		t.Error("live touch unlocked the token before its target")
	}
}

// Active days cost zero extra traffic: todayUsed skips the touch.
func TestMaturitySkipsTodayUsed(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.StreakBody = streakBody(3, true)
	p := newMaturityPool(t, mock, false)
	now := time.Now()
	if err := p.SetMaturity(0, true, 7, ""); err != nil {
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
	mock.StreakBody = streakBody(2, false)
	p := newMaturityPool(t, mock, true)
	now := time.Now()
	if err := p.SetMaturity(0, true, 7, ""); err != nil {
		t.Fatal(err)
	}
	setMaturitySlot(p, 0, now.Add(2*time.Hour), laDay(now))

	p.maturityTickAt(context.Background(), now)

	if got := mock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("SessionProbes = %d, want 0 (slot in the future)", got)
	}
	if _, result := maturityResult(p, 0); result != "skip:slot" {
		t.Errorf("result = %q, want skip:slot", result)
	}
}

// The 6h throttle makes a restart (which re-rolls the slot) idempotent: a
// second pass right after a firing touches nothing.
func TestMaturityThrottleIdempotent(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.StreakBody = streakBody(2, false)
	p := newMaturityPool(t, mock, true)
	now := time.Now()
	if err := p.SetMaturity(0, true, 7, ""); err != nil {
		t.Fatal(err)
	}
	setMaturitySlot(p, 0, now.Add(-time.Hour), laDay(now))

	p.maturityTickAt(context.Background(), now)
	// Restart re-rolls the slot to "now" — the throttle must still hold.
	setMaturitySlot(p, 0, now, laDay(now))
	p.maturityTickAt(context.Background(), now.Add(time.Minute))

	if got := mock.SessionProbesSnapshot(); got != 1 {
		t.Errorf("SessionProbes = %d, want 1 (throttle blocks the re-fire)", got)
	}
	if _, result := maturityResult(p, 0); result != "skip:throttle" {
		t.Errorf("result = %q, want skip:throttle", result)
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
	if err := p.SetMaturity(0, true, 7, ""); err != nil {
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

// Target reached on a healthy account auto-releases the lock and stops.
func TestMaturityAutoReleaseAtTarget(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.StreakBody = streakBody(7, false)
	p := newMaturityPool(t, mock, true)
	now := time.Now()
	if err := p.SetMaturity(0, true, 7, ""); err != nil {
		t.Fatal(err)
	}
	if !p.Snapshot()[0].Locked {
		t.Fatal("warming token must start locked")
	}

	p.maturityTickAt(context.Background(), now)

	snap := p.Snapshot()[0]
	if snap.Locked {
		t.Error("target reached but the lock still holds")
	}
	if snap.Maturity == nil || snap.Maturity.Enabled {
		t.Fatalf("maturity snapshot = %+v, want disabled after release", snap.Maturity)
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
	mock.StreakBody = streakBody(2, false)
	p := newMaturityPool(t, mock, true)
	now := time.Now()
	if err := p.SetMaturity(0, true, 7, ""); err != nil {
		t.Fatal(err)
	}
	setMaturitySlot(p, 0, now.Add(-time.Hour), laDay(now))
	p.CooldownToken(0, time.Hour)

	p.maturityTickAt(context.Background(), now)

	if got := mock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("SessionProbes = %d, want 0 (cooling account)", got)
	}
	if _, result := maturityResult(p, 0); result != "skip:cooling" {
		t.Errorf("result = %q, want skip:cooling", result)
	}
}

// Three consecutive non-advancing days raise the warning and stop firing.
func TestMaturityNoAdvanceWarnStops(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.StreakBody = streakBody(2, false)
	p := newMaturityPool(t, mock, true)
	now := time.Now()
	if err := p.SetMaturity(0, true, 7, ""); err != nil {
		t.Fatal(err)
	}
	day := now
	setMaturitySlot(p, 0, day.Add(-time.Hour), laDay(day))
	p.maturityTickAt(context.Background(), day) // day 1: probe fires

	for i := 1; i <= 3; i++ {
		day = day.Add(24 * time.Hour)
		// Streak stuck at 2 while touches fire daily.
		setMaturitySlot(p, 0, day.Add(-time.Hour), day.In(maturityLocation("America/Los_Angeles")).Format("2006-01-02"))
		p.maturityTickAt(context.Background(), day)
	}

	snap := p.Snapshot()[0]
	if snap.Maturity == nil || !snap.Maturity.Warn {
		t.Fatalf("maturity snapshot = %+v, want warn after 3 flat days", snap.Maturity)
	}
	if snap.Maturity.NoAdvanceDays != 3 {
		t.Errorf("NoAdvanceDays = %d, want 3", snap.Maturity.NoAdvanceDays)
	}
	probes := mock.SessionProbesSnapshot()
	// One more day: warned tokens never fire again.
	day = day.Add(24 * time.Hour)
	setMaturitySlot(p, 0, day.Add(-time.Hour), day.In(maturityLocation("America/Los_Angeles")).Format("2006-01-02"))
	p.maturityTickAt(context.Background(), day)
	if got := mock.SessionProbesSnapshot(); got != probes {
		t.Errorf("SessionProbes grew %d → %d after warn, want no more firing", probes, got)
	}
}

// SetMaturity rejects bad targets, unknown modes, gated premium mode, and
// out-of-range tokens.
func TestSetMaturityValidation(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newMaturityPool(t, mock, true)
	if err := p.SetMaturity(0, true, 29, ""); err == nil {
		t.Error("target 29 accepted, want range error")
	}
	if err := p.SetMaturity(0, true, 7, "turbo"); err == nil {
		t.Error("mode turbo accepted, want unknown-mode error")
	}
	if err := p.SetMaturity(0, true, 7, MaturityModePremiumShort); err == nil {
		t.Error("premium-short accepted without MATURITY_ALLOW_PREMIUM, want gate error")
	}
	if err := p.SetMaturity(9, true, 7, ""); err == nil {
		t.Error("token 9 accepted, want out-of-range error")
	}
	p.cfg.Load().MaturityAllowPremium = true
	if err := p.SetMaturity(0, true, 7, MaturityModePremiumShort); err != nil {
		t.Errorf("premium-short with allow flag: %v", err)
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
	if err := p.SetMaturity(0, true, 7, ""); err != nil {
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
	if err := p.SetMaturity(0, true, 7, ""); err != nil {
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
