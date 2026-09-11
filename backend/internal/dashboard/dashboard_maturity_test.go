package dashboard

import (
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/registry"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/testutil"
	"freebuff-proxy/backend/internal/upstream"
)

// Maturity cards render from the pool snapshot and stay out of the payload
// until a token first opts in.
func TestMaturityCardFromSnapshot(t *testing.T) {
	if got := maturityCardFromSnapshot(nil); got != nil {
		t.Errorf("maturityCardFromSnapshot(nil) = %+v, want nil", got)
	}
	slot := time.Date(2026, 9, 5, 7, 30, 0, 0, time.UTC)
	touch := slot.Add(2 * time.Hour)
	card := cardFromSnapshot(pool.TokenSnapshot{
		Streak: 3,
		Maturity: &pool.MaturitySnapshot{
			Enabled: true, Target: 7, Mode: "unmetered", Badge: "Warming",
			Slot: slot, LastTouch: touch,
			LastAction: "probe", LastResult: "ok", LastAdvanced: "yes",
		},
	})
	if card.Maturity == nil {
		t.Fatal("card.Maturity = nil, want rendered card")
		return
	}
	m := card.Maturity
	if !m.Enabled || m.Target != 7 || m.Mode != "unmetered" || m.Badge != "Warming" {
		t.Errorf("maturity card identity = %+v, want enabled/7/unmetered/Warming", m)
	}
	if m.Slot != slot.Format(time.RFC3339) || m.LastTouch != touch.Format(time.RFC3339) {
		t.Errorf("maturity card times = %q/%q, want RFC3339 slot/touch", m.Slot, m.LastTouch)
	}
	if m.LastAction != "probe" || m.LastResult != "ok" || m.LastAdvanced != "yes" {
		t.Errorf("maturity card touch = %+v, want probe/ok/yes", m)
	}
	live := liveCardFromSnapshot(pool.TokenSnapshot{
		Maturity: &pool.MaturitySnapshot{Enabled: true, Target: 7, Mode: "unmetered", Badge: "Cold"},
	})
	if live.Maturity == nil || !live.Maturity.Enabled || live.Maturity.Target != 7 || live.Maturity.Badge != "Cold" {
		t.Errorf("live maturity card = %+v, want enabled/7/Cold", live.Maturity)
	}
	// Never opted in: no maturity key on either card.
	bare := cardFromSnapshot(pool.TokenSnapshot{})
	if bare.Maturity != nil {
		t.Errorf("bare card.Maturity = %+v, want nil", bare.Maturity)
	}
	if liveBare := liveCardFromSnapshot(pool.TokenSnapshot{}); liveBare.Maturity != nil {
		t.Errorf("bare live.Maturity = %+v, want nil", liveBare.Maturity)
	}
}

// TouchModel rides the card: the per-token override set on the pool snapshot
// must survive the dashboard mapper on both the full and the live card, so
// the Maturity page select shows the saved value after a refresh.
func TestMaturityCardTouchModelRoundTrip(t *testing.T) {
	snap := pool.TokenSnapshot{
		Maturity: &pool.MaturitySnapshot{
			Enabled: true, Target: 7, Mode: "unmetered",
			TouchModel: "mimo/mimo-v2.5", Badge: "Warming",
		},
	}
	card := cardFromSnapshot(snap)
	if card.Maturity == nil {
		t.Fatal("card.Maturity = nil, want rendered card")
		return
	}
	if card.Maturity.TouchModel != "mimo/mimo-v2.5" {
		t.Errorf("card touch_model = %q, want mimo/mimo-v2.5", card.Maturity.TouchModel)
	}
	live := liveCardFromSnapshot(snap)
	if live.Maturity == nil {
		t.Fatal("live.Maturity = nil, want rendered card")
		return
	}
	if live.Maturity.TouchModel != "mimo/mimo-v2.5" {
		t.Errorf("live touch_model = %q, want mimo/mimo-v2.5", live.Maturity.TouchModel)
	}
	// Fallback (empty override) stays empty: the page renders Global default.
	bare := cardFromSnapshot(pool.TokenSnapshot{
		Maturity: &pool.MaturitySnapshot{Enabled: true, Target: 7, Mode: "unmetered"},
	})
	if bare.Maturity == nil || bare.Maturity.TouchModel != "" {
		t.Errorf("fallback touch_model = %+v, want empty", bare.Maturity)
	}
}

// Next-touch visibility rides the card: slot/touch days plus the resolved
// effective/auto models must survive the mapper on both the full and the
// live card, so the Warming tab renders its countdown, day-strip, and Auto
// pick without a new scheduler. Old snapshots without the keys still map
// (omitempty keeps the payload shape).
func TestMaturityCardNextTouchRoundTrip(t *testing.T) {
	slot := time.Date(2026, 9, 5, 7, 30, 0, 0, time.UTC)
	touch := slot.Add(2 * time.Hour)
	snap := pool.TokenSnapshot{
		Maturity: &pool.MaturitySnapshot{
			Enabled: true, Target: 7, Mode: "unmetered", Badge: "Warming",
			Slot: slot, SlotDay: "2026-09-05", LastTouch: touch, TouchDay: "2026-09-05",
			LastAction: "probe", LastResult: "ok",
			EffectiveTouchModel: "upstage/solar-pro4",
			AutoTouchModel:      "upstage/solar-pro4",
			AutoTouchReason:     "auto:unmetered",
		},
	}
	for name, card := range map[string]*maturityCard{
		"full": cardFromSnapshot(snap).Maturity,
		"live": liveCardFromSnapshot(snap).Maturity,
	} {
		if card == nil {
			t.Fatalf("%s card.Maturity = nil, want rendered card", name)
		}
		if card.SlotDay != "2026-09-05" || card.TouchDay != "2026-09-05" {
			t.Errorf("%s slot/touch day = %q/%q, want 2026-09-05/2026-09-05", name, card.SlotDay, card.TouchDay)
		}
		if card.EffectiveTouchModel != "upstage/solar-pro4" {
			t.Errorf("%s effective = %q, want upstage/solar-pro4", name, card.EffectiveTouchModel)
		}
		if card.AutoTouchModel != "upstage/solar-pro4" || card.AutoTouchReason != "auto:unmetered" {
			t.Errorf("%s auto = %q/%q, want upstage/solar-pro4/auto:unmetered", name, card.AutoTouchModel, card.AutoTouchReason)
		}
	}
}

// The tokens payload carries the nightly-maintenance globals: the dry-run
// flag for the badge plus tonight's window (RFC3339 absolute instants) for
// the next-run countdown — a fixed 60m ending at Pacific midnight.
func TestTokensDataMaturityWindow(t *testing.T) {
	cfg := &config.Config{
		AuthTokens:         []string{"tok-window-0"},
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
		MaturityEnabled:    true,
		MaturityDryRun:     true,
	}
	mock := testutil.NewMock()
	t.Cleanup(mock.Close)
	clientCfg := *cfg
	clientCfg.UpstreamBaseURL = mock.URL()
	client, err := upstream.New(cfg.AuthTokens[0], &clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, []*session.Manager{session.NewManager(client)}, reg)
	if err != nil {
		t.Fatal(err)
	}
	d := New(func() *config.Config { return cfg }, p, reg, nil, nil)
	td := d.tokensData()
	if !td.MaturityEnabled {
		t.Error("maturity_enabled = false, want true")
	}
	if !td.MaturityDryRun {
		t.Error("maturity_dry_run = false, want true (badge source)")
	}
	start, err := time.Parse(time.RFC3339, td.MaturityWindowStart)
	if err != nil {
		t.Fatalf("maturity_window_start = %q, want RFC3339: %v", td.MaturityWindowStart, err)
	}
	end, err := time.Parse(time.RFC3339, td.MaturityWindowEnd)
	if err != nil {
		t.Fatalf("maturity_window_end = %q, want RFC3339: %v", td.MaturityWindowEnd, err)
	}
	if end.Sub(start) != time.Hour {
		t.Errorf("window length = %v, want 60m (fixed pre-reset window)", end.Sub(start))
	}
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	if pe := end.In(la); pe.Hour() != 0 || pe.Minute() != 0 {
		t.Errorf("window end = %v Pacific, want midnight", pe.Format("15:04"))
	}
}
