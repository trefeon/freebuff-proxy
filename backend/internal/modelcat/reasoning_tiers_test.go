package modelcat

import "testing"

// Tests for the effort-ladder helpers added by the 2026-08-31 review fix
// (P3: model literals must live only in modelcat). The tables pin the exact
// matching semantics convert historically applied: suffix-tolerant matching
// on the row short name for ladder facts, Contains matching for strict
// reasoning — including the loose edges (loose suffix hits, legacy kimi id).

func TestIsMediumlessLadderModel(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{"exact ox-alpha", "stealth/ox-alpha", true},
		{"exact deepseek pro", "deepseek/deepseek-v4-pro", true},
		{"dated suffix NOT tolerated (strict HasSuffix, historical)",
			"stealth/ox-alpha-20260812", false},
		{"dated suffix deepseek NOT tolerated (strict HasSuffix)",
			"deepseek/deepseek-v4-flash-20260901", false},
		{"exact deepseek flash", "deepseek/deepseek-v4-flash", true},
		{"exact glm-5.3-flash", "z-ai/glm-5.3-flash", true},
		{"bare short id", "ox-alpha", true},
		{"bare deepseek short id", "deepseek-v4-flash", true},
		{"case-insensitive", "STEALTH/OX-ALPHA", true},
		{"loose suffix hit (historical behavior)", "notreal/ox-alpha", true},
		{"luna ladder has medium", "openai/gpt-5.6-luna", false},
		{"fable ladder has medium", "anthropic/claude-fable-5", false},
		{"mimo single-rung ladder", "mimo/mimo-v2.5", false},
		{"minimax single-rung ladder", "minimax/minimax-m3", false},
		{"glm-5.2 ignores effort", "z-ai/glm-5.2", false},
		{"solar ignores effort", "upstage/solar-pro4", false},
		{"unknown model", "acme/widget-9000", false},
		{"empty id", "", false},
		{"prefix but not suffix", "ox-alpha-suffix", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsMediumlessLadderModel(tt.id); got != tt.want {
				t.Errorf("IsMediumlessLadderModel(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

func TestIsStrictReasoningModel(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{"exact mimo", "mimo/mimo-v2.5", true},
		{"bare mimo", "mimo", true},
		{"deepseek flash", "deepseek/deepseek-v4-flash", true},
		{"deepseek pro", "deepseek/deepseek-v4-pro", true},
		{"bare deepseek", "deepseek-v4-pro", true},
		{"suffixed deepseek variant", "deepseek/deepseek-v4-flash-20260901", true},
		{"legacy kimi id without catalog row", "crof/kimi-k3-eco", true},
		{"bare kimi", "kimi", true},
		{"case-insensitive", "MIMO/MIMO-V2.5", true},
		{"luna", "openai/gpt-5.6-luna", false},
		{"glm-5.3-flash", "z-ai/glm-5.3-flash", false},
		{"minimax", "minimax/minimax-m3", false},
		{"glm-5.2", "z-ai/glm-5.2", false},
		{"unknown", "acme/widget-9000", false},
		{"empty id", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsStrictReasoningModel(tt.id); got != tt.want {
				t.Errorf("IsStrictReasoningModel(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}
