package dashboard

import (
	"testing"
	"time"

	"freebuff-proxy/backend/internal/pool"
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
		Maturity: &pool.MaturitySnapshot{Enabled: true, Target: 7, Mode: "unmetered", Badge: "Cold", Warn: true, NoAdvanceDays: 3},
	})
	if live.Maturity == nil || !live.Maturity.Warn || live.Maturity.NoAdvanceDays != 3 || live.Maturity.Badge != "Cold" {
		t.Errorf("live maturity card = %+v, want warn/3/Cold", live.Maturity)
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
