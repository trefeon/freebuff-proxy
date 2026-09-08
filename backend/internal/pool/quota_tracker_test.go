package pool

import (
	"testing"

	"freebuff-proxy/backend/internal/modelcat"
)

// TestIsPremiumModel pins the shared premium-pool identity (ADR-0027 keeps
// the identity — maturity premium-short still keys off it; only the 5/day
// count mirror is gone): luna is premium, glm-5.3-flash left the pool on
// 2026-08-28 (unmetered), mimo and glm-5.2 never were.
func TestIsPremiumModel(t *testing.T) {
	if !modelcat.IsPremium("openai/gpt-5.6-luna") {
		t.Error("expected premium for luna")
	}
	if modelcat.IsPremium("z-ai/glm-5.3-flash") {
		t.Error("glm-5.3-flash left the premium pool on 2026-08-28 (unmetered)")
	}
	if modelcat.IsPremium("mimo/mimo-v2.5") {
		t.Error("unexpected premium for mimo")
	}
	if modelcat.IsPremium("z-ai/glm-5.2") {
		t.Error("glm-5.2 not premium pool")
	}
}
