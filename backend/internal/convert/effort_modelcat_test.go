package convert

import (
	"testing"

	"freebuff-proxy/backend/internal/modelcat"
)

// The convert-side effort helpers must delegate to modelcat rather than
// re-encode ladder facts (2026-08-31 review, P3 model-literal cleanup): the
// wrappers and the modelcat helpers have to agree on every input class —
// exact ids, suffixed variants, bare ids, and unknown ids.

func TestEffortHelpersMatchModelcat(t *testing.T) {
	models := []string{
		"deepseek/deepseek-v4-flash",
		"deepseek/deepseek-v4-pro",
		"stealth/ox-alpha",
		"stealth/ox-alpha-20260812",
		"z-ai/glm-5.3-flash",
		"openai/gpt-5.6-luna",
		"mimo/mimo-v2.5",
		"minimax/minimax-m3",
		"z-ai/glm-5.2",
		"crof/kimi-k3-eco",
		"ox-alpha",
		"deepseek-v4-flash",
		"acme/widget-9000",
		"",
	}
	for _, model := range models {
		if got, want := isMediumlessLadderModel(model), modelcat.IsMediumlessLadderModel(model); got != want {
			t.Errorf("isMediumlessLadderModel(%q) = %v, modelcat says %v", model, got, want)
		}
		if got, want := isStrictReasoningModel(model), modelcat.IsStrictReasoningModel(model); got != want {
			t.Errorf("isStrictReasoningModel(%q) = %v, modelcat says %v", model, got, want)
		}
	}
}
