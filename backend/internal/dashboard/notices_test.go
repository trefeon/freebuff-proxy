package dashboard

import (
	"testing"
)

func TestNoticesData(t *testing.T) {
	d := &Dashboard{}
	resp := d.noticesData()

	if resp.Count == 0 || len(resp.Notices) == 0 {
		t.Fatalf("noticesData() returned empty notices list")
	}

	foundTierChange := false
	for _, n := range resp.Notices {
		if n.ID == "upstream-tier-change" {
			foundTierChange = true
			if n.Type != "announcement" {
				t.Errorf("TierChange notice Type = %q, want 'announcement'", n.Type)
			}
			if n.Tone != "accent" {
				t.Errorf("TierChange notice Tone = %q, want 'accent'", n.Tone)
			}
		}
	}
	if !foundTierChange {
		t.Errorf("TierChange notice not found in noticesData()")
	}
}
