package convert

import "testing"

// The upstream free-mode gate classifies a request as third-party when it
// offers tools but NONE of them is a signature tool — i.e. a name from
// toolNames that is not in GENERIC_TOOL_NAMES (reference/freebuff
// common/src/constants/foreign-client-signals.ts:33-44). The proxy's
// countermeasure is injecting end_turn (normalizeToolSchemas), which is a
// SIGNATURE tool: end_turn ∈ toolNames and ∉ GENERIC_TOOL_NAMES, so proxied
// traffic with otherwise-foreign toolsets classifies first-party.
//
// If upstream ever moves end_turn into the generic exclusion set, every
// proxied request silently downgrades to FREEBUFF_DOWNGRADE_MODEL_ID
// (inclusionai/ling-3.0-tiny:free) under the foreign_toolset signal. Recheck
// this pin on EVERY sync-upstream run.
func TestInjectedEndTurnIsSignatureTool(t *testing.T) {
	// GENERIC_TOOL_NAMES verbatim (foreign-client-signals.ts:33-44).
	generic := map[string]bool{
		"write_file":  true,
		"web_search":  true,
		"glob":        true,
		"skill":       true,
		"apply_patch": true,
	}
	if generic["end_turn"] {
		t.Fatalf("end_turn joined GENERIC_TOOL_NAMES upstream: the injected tool no longer marks traffic first-party; remap the injection to another signature tool immediately")
	}
}
