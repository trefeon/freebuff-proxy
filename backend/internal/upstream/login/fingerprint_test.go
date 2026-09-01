package login

import (
	"strings"
	"testing"
)

// TestGenerateFingerprintIDStable pins the process-wide fingerprint cache: two
// calls in the same process return the same id (the CLI mirrors this —
// fingerprint.ts memoizes once per process), and the id carries the
// "enhanced-" prefix with a 43-char base64url suffix.
func TestGenerateFingerprintIDStable(t *testing.T) {
	a := GenerateFingerprintID()
	b := GenerateFingerprintID()
	if a != b {
		t.Errorf("GenerateFingerprintID() not stable across calls: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "enhanced-") {
		t.Errorf("id %q missing enhanced- prefix", a)
	}
	suffix := strings.TrimPrefix(a, "enhanced-")
	if len(suffix) != 43 {
		t.Errorf("suffix length = %d, want 43 (base64url 32 bytes)", len(suffix))
	}
}

// TestFingerprintIDVariesByHost verifies the derivation is a function of
// the machine identity: identical inputs reproduce byte-for-byte, distinct
// hosts (hostname or MACs) produce distinct ids.
func TestFingerprintIDVariesByHost(t *testing.T) {
	a := fingerprintIDFrom("host-alpha", []string{"aa:bb:cc:dd:ee:01"}, 4, 8)
	if a != fingerprintIDFrom("host-alpha", []string{"aa:bb:cc:dd:ee:01"}, 4, 8) {
		t.Fatal("same machine inputs must reproduce the same id")
	}
	b := fingerprintIDFrom("host-beta", []string{"aa:bb:cc:dd:ee:02"}, 4, 8)
	if a == b {
		t.Fatal("distinct hosts produced the same fingerprint id")
	}
	c := fingerprintIDFrom("host-alpha", []string{"aa:bb:cc:dd:ee:09"}, 4, 8)
	if a == c {
		t.Fatal("distinct MAC sets produced the same fingerprint id")
	}
}

// TestGenerateIsolatedFingerprintIDFresh pins that the isolated mint draws a
// fresh random id per call (multi-account flows must not correlate by a
// shared hardware identifier), while still carrying the "enhanced-" prefix.
func TestGenerateIsolatedFingerprintIDFresh(t *testing.T) {
	a := GenerateIsolatedFingerprintID()
	b := GenerateIsolatedFingerprintID()
	if a == b {
		t.Errorf("GenerateIsolatedFingerprintID() gave the same id twice: %q", a)
	}
	if !strings.HasPrefix(a, "enhanced-") || !strings.HasPrefix(b, "enhanced-") {
		t.Errorf("isolated ids missing enhanced- prefix: %q %q", a, b)
	}
}
