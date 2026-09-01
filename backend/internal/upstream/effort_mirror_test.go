package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// The upstream server's effort authority reads
// codebuff_metadata.freebuff_reasoning_effort (reference/freebuff
// freebuff-models.ts resolveFreebuffReasoningEffort; the CLI carries its
// /reasoning pick there per request — use-send-message.ts:602-608). The proxy
// mirrors the normalized top-level reasoning_effort into that field, and must
// stay ABSENT when the client requested no effort so upstream applies its own
// catalog default (silent-turn contract; for GLM unset is deeper than max).
func TestEnvelopeMirrorsFreebuffReasoningEffort(t *testing.T) {
	body := `{"model":"z-ai/glm-5.3-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`
	out, err := injectEnvelope([]byte(body), "free", ChatOptions{RunID: "r1", ClientID: "abc123def0123", StepNumber: 1})
	if err != nil {
		t.Fatalf("injectEnvelope: %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal(out, &sent); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	md, ok := sent["codebuff_metadata"].(map[string]any)
	if !ok {
		t.Fatalf("missing codebuff_metadata: %s", out)
	}
	if got, want := md["freebuff_reasoning_effort"], "high"; got != want {
		t.Errorf("freebuff_reasoning_effort = %v, want %q", got, want)
	}
}

func TestEnvelopeOmitsFreebuffReasoningEffortOnSilentTurn(t *testing.T) {
	// No reasoning_effort anywhere in the body: the metadata field must not
	// appear (unset = upstream default; for GLM unset is deeper than max).
	body := `{"model":"z-ai/glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`
	out, err := injectEnvelope([]byte(body), "free", ChatOptions{RunID: "r1", ClientID: "abc123def0123"})
	if err != nil {
		t.Fatalf("injectEnvelope: %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal(out, &sent); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	md := sent["codebuff_metadata"].(map[string]any)
	if _, present := md["freebuff_reasoning_effort"]; present {
		t.Errorf("freebuff_reasoning_effort present on a silent turn: %v", md)
	}
}

// A client that disabled reasoning arrives here with reasoning_effort
// already deleted by the convert layer — same shape as a silent turn.
func TestEnvelopeOmitsFreebuffReasoningEffortWhenDisabled(t *testing.T) {
	body := `{"model":"z-ai/glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`
	out, err := injectEnvelope([]byte(body), "free", ChatOptions{RunID: "r1", ClientID: "abc123def0123"})
	if err != nil {
		t.Fatalf("injectEnvelope: %v", err)
	}
	if strings.Contains(string(out), "freebuff_reasoning_effort") {
		t.Errorf("freebuff_reasoning_effort leaked into an effort-less body: %s", out)
	}
}
