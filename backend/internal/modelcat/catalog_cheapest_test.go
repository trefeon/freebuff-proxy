package modelcat

import "testing"

// CheapestFreeIn intersects the cheapest-unmetered resolution with a
// registry allowlist: catalog order decides, membership filters.
func TestCheapestFreeIn(t *testing.T) {
	full := ServedIDs()
	if got := CheapestFreeIn(full, nil, false); got != "upstage/solar-pro4" {
		t.Errorf("CheapestFreeIn(full) = %q, want upstage/solar-pro4 (first static unmetered served row)", got)
	}
	// Without the cheapest row, the next unmetered served row wins — never
	// an unlisted id and never the alphabetically-first gated row.
	rest := []string{"anthropic/claude-fable-5", "mimo/mimo-v2.5", "openai/gpt-5.6-luna"}
	if got := CheapestFreeIn(rest, nil, false); got != "mimo/mimo-v2.5" {
		t.Errorf("CheapestFreeIn(restricted) = %q, want mimo/mimo-v2.5", got)
	}
	// Live-priced cheapest loses to the next price-0 row in the set.
	if got := CheapestFreeIn(full, map[string]float64{"upstage/solar-pro4": 5}, false); got != "z-ai/glm-5.3-flash" {
		t.Errorf("CheapestFreeIn(priced solar) = %q, want z-ai/glm-5.3-flash", got)
	}
	// Empty registry means no candidate, never an invented id.
	if got := CheapestFreeIn(nil, nil, false); got != "" {
		t.Errorf("CheapestFreeIn(nil) = %q, want empty", got)
	}
}
