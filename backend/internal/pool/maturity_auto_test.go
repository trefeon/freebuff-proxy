package pool

import (
	"context"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/modelcat"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// Auto resolution never serves honeypot, god-only, eval, paused, premium,
// or live-priced rows: only a served unmetered row with price 0 (or a
// quota exemption) can win, in catalog order.
func TestAutoUnmeteredGate(t *testing.T) {
	if got, _ := modelcat.AutoUnmeteredTouchModel(nil, false); got != "upstage/solar-pro4" {
		t.Errorf("auto(nil) = %q, want upstage/solar-pro4 (first static unmetered served row)", got)
	}
	for _, honeypot := range []string{
		"crof/kimi-k3-eco", "openai/gpt-5.6-luna-es",
		"minimax/minimax-m3", "deepseek/deepseek-v4-pro", "stealth/ox-alpha",
		"z-ai/glm-5.2",
	} {
		if modelcat.IsServed(honeypot) {
			t.Errorf("IsServed(%q) = true, want false (honeypot/god-only/paused/referral never served)", honeypot)
		}
	}
	for _, premium := range modelcat.SharedPremiumModels() {
		if got, _ := modelcat.AutoUnmeteredTouchModel(map[string]float64{premium: 0}, false); got == premium {
			t.Errorf("auto picked premium %q at price 0, want never (premium pool is metered)", premium)
		}
	}
	// Live-priced solar loses to the next price-0 unmetered row.
	if got, _ := modelcat.AutoUnmeteredTouchModel(map[string]float64{"upstage/solar-pro4": 5}, false); got == "upstage/solar-pro4" {
		t.Errorf("auto picked priced solar-pro4, want next unmetered row")
	}
	// Exemption re-admits a priced row (server-authorized, not a bargain hunt).
	if got, _ := modelcat.AutoUnmeteredTouchModel(map[string]float64{"upstage/solar-pro4": 5}, true); got != "upstage/solar-pro4" {
		t.Errorf("auto(exempt) = %q, want upstage/solar-pro4", got)
	}
	// Every static unmetered row priced with no exemption: no candidate.
	allPriced := map[string]float64{}
	for _, id := range modelcat.ServedIDs() {
		if !modelcat.IsPremium(id) {
			allPriced[id] = 5
		}
	}
	if got, reason := modelcat.AutoUnmeteredTouchModel(allPriced, false); got != "" || reason != "fallback:no-unmetered-served" {
		t.Errorf("auto(all priced) = %q/%q, want \"\"/fallback:no-unmetered-served", got, reason)
	}
	if !modelcat.IsAutoTouchSentinel("") || !modelcat.IsAutoTouchSentinel("auto") || !modelcat.IsAutoTouchSentinel(" Auto ") {
		t.Error("IsAutoTouchSentinel missed \"\"/auto/whitespace variant")
	}
	if modelcat.IsAutoTouchSentinel("deepseek/deepseek-v4-flash") {
		t.Error("IsAutoTouchSentinel(explicit id) = true, want false")
	}
}

// New default: a token with no per-token override and the global auto
// sentinel touches the cheapest served unmetered row (solar), and the
// snapshot carries the slot days plus the resolved effective/auto models.
func TestMaturityAutoDefaultResolves(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.StreakBody = streakBody(2, false)
	p := newMaturityPool(t, mock, false)
	p.cfg.Load().MaturityTouchModel = "auto"
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	action, result, err := p.MaturityTouchNow(context.Background(), 0)
	if err != nil {
		t.Fatalf("auto touch: %v", err)
	}
	if action != "admit" || result != "ok" {
		t.Fatalf("auto touch = %q/%q, want admit/ok", action, result)
	}
	snap := p.Snapshot()[0].Maturity
	if snap == nil {
		t.Fatal("snapshot maturity = nil, want resolved view")
	}
	if snap.AutoTouchModel != "upstage/solar-pro4" || snap.AutoTouchReason != "auto:unmetered" {
		t.Errorf("auto = %q/%q, want upstage/solar-pro4/auto:unmetered", snap.AutoTouchModel, snap.AutoTouchReason)
	}
	if snap.EffectiveTouchModel != "upstage/solar-pro4" {
		t.Errorf("effective = %q, want upstage/solar-pro4", snap.EffectiveTouchModel)
	}
	if snap.SlotDay == "" {
		t.Error("slot_day empty, want account-day for the next-touch countdown")
	}
	if snap.TouchDay == "" || snap.TouchDay != snap.SlotDay {
		t.Errorf("touch_day = %q slot_day = %q, want equal after a same-day touch", snap.TouchDay, snap.SlotDay)
	}
}

// Auto skips a live-priced head and admits the next price-0 unmetered row;
// the snapshot still names the winning pick.
func TestMaturityAutoSkipsPricedHead(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.StreakBody = streakBody(2, false)
	p := newMaturityPool(t, mock, false)
	p.cfg.Load().MaturityTouchModel = "auto"
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	toks := p.roster.Load()
	(*toks)[0].sessionMgr().UpdateQuotaFromProbe(&upstream.SessionState{
		Freebucks: &upstream.FreebucksInfo{
			Balance: 10,
			Prices:  map[string]float64{"upstage/solar-pro4": 5},
		},
	})
	action, result, err := p.MaturityTouchNow(context.Background(), 0)
	if err != nil {
		t.Fatalf("auto repriced touch: %v", err)
	}
	if action != "admit" || result != "ok" {
		t.Fatalf("auto repriced touch = %q/%q, want admit/ok on the next unmetered row", action, result)
	}
	snap := p.Snapshot()[0].Maturity
	if snap == nil || snap.EffectiveTouchModel == "" || snap.EffectiveTouchModel == "upstage/solar-pro4" {
		t.Errorf("effective after reprice = %+v, want a non-solar unmetered pick", snap)
	}
	if got := mock.SessionCreatesSnapshot(); got != 1 {
		t.Errorf("SessionCreates = %d, want 1 (no priced admission)", got)
	}
}

// No unmetered served row and the global itself is auto: fail closed, never
// an invented model. An explicit global still wins over auto (existing
// explicit-config contract, incl. the premium fail-closed path).
func TestMaturityAutoFallbackClosed(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newMaturityPool(t, mock, false)
	p.cfg.Load().MaturityTouchModel = "auto"
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	allPriced := map[string]float64{}
	for _, id := range modelcat.ServedIDs() {
		if !modelcat.IsPremium(id) {
			allPriced[id] = 5
		}
	}
	toks := p.roster.Load()
	(*toks)[0].sessionMgr().UpdateQuotaFromProbe(&upstream.SessionState{
		Freebucks: &upstream.FreebucksInfo{Balance: 10, Prices: allPriced},
	})
	now := windowNow()
	seedStreak(p, 0, 2, false, now)
	setMaturitySlot(p, 0, now.Add(-time.Hour), laDay(now))
	p.maturityTickAt(context.Background(), now)
	if _, result := maturityResult(p, 0); result != "skip:touch-model" {
		t.Errorf("auto-exhausted tick = %q, want skip:touch-model (fail closed)", result)
	}
	if got := mock.SessionCreatesSnapshot() + mock.SessionProbesSnapshot(); got != 0 {
		t.Errorf("upstream calls = %d, want 0 (guarded before admission)", got)
	}
	snap := p.Snapshot()[0].Maturity
	if snap == nil || snap.EffectiveTouchModel != "" || snap.AutoTouchReason != "fallback:no-unmetered-served" {
		t.Errorf("exhausted snapshot = %+v, want empty effective + fallback reason", snap)
	}
}

// Explicit per-token and global models keep their precedence over auto;
// "auto" as a per-token value normalizes to the automatic default.
func TestMaturityAutoPrecedence(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.StreakBody = streakBody(2, false)
	p := newMaturityPool(t, mock, false)
	if err := p.SetMaturity(0, true, 7, "", "auto"); err != nil {
		t.Fatalf("auto override save: %v", err)
	}
	if got := p.Snapshot()[0].Maturity.TouchModel; got != "" {
		t.Errorf("snapshot touch_model = %q, want empty (auto normalizes to default)", got)
	}
	// Explicit global still wins when set (backward-compatible contract).
	p.cfg.Load().MaturityTouchModel = modelB
	if err := p.SetMaturity(0, true, 7, "", ""); err != nil {
		t.Fatal(err)
	}
	snap := p.Snapshot()[0].Maturity
	if snap == nil || snap.EffectiveTouchModel != modelB {
		t.Errorf("effective with explicit global = %+v, want %s", snap, modelB)
	}
	if snap.AutoTouchModel == "" {
		t.Error("auto pick empty alongside explicit global, want display pick retained")
	}
}
