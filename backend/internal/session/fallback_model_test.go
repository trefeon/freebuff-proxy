package session

import "testing"

// DefaultFallbackModelFor resolves cheapest-first: the static cheapest
// served unmetered row with no live meter, the next unmetered row when the
// cheapest is live-priced, the priced cheapest back when the account is
// quota-exempt, and the picker-lead default (never "") when everything is
// priced without an exemption.
func TestDefaultFallbackModelCheapestFirst(t *testing.T) {
	if got := DefaultFallbackModel(); got != "upstage/solar-pro4" {
		t.Errorf("DefaultFallbackModel() = %q, want upstage/solar-pro4 (first static unmetered served row)", got)
	}
	if got := DefaultFallbackModelFor(map[string]float64{"upstage/solar-pro4": 5}, false); got != "z-ai/glm-5.3-flash" {
		t.Errorf("DefaultFallbackModelFor(priced solar) = %q, want z-ai/glm-5.3-flash", got)
	}
	if got := DefaultFallbackModelFor(map[string]float64{"upstage/solar-pro4": 5}, true); got != "upstage/solar-pro4" {
		t.Errorf("DefaultFallbackModelFor(exempt) = %q, want upstage/solar-pro4", got)
	}
	allPriced := map[string]float64{
		"upstage/solar-pro4":         5,
		"z-ai/glm-5.3-flash":         2,
		"deepseek/deepseek-v4-flash": 3,
		"mimo/mimo-v2.5":             1,
	}
	if got := DefaultFallbackModelFor(allPriced, false); got != "z-ai/glm-5.3-flash" {
		t.Errorf("DefaultFallbackModelFor(all priced) = %q, want the picker-lead z-ai/glm-5.3-flash, never empty", got)
	}
}
