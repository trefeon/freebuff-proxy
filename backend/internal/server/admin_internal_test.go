package server

import (
	"os"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/config"
)

func testIP(n int) string {
	// Distinct, fresh per-IP hosts for the global-budget tests.
	return "10." + string(rune('0'+n/1000%10)) + "." + string(rune('0'+n/100%10)) + "." + string(rune('0'+n%100/10)) + string(rune('0'+n%10))
}

// TestAdminAuthGlobalBudget pins the process-wide failed-login budget
// : failures from DISTINCT source IPs never trip the per-IP lockout,
// but crossing loginGlobalFailMax inside one window locks every source, and
// each subsequent breach escalates the lockout by doubling up to the cap.
func TestAdminAuthGlobalBudget(t *testing.T) {
	a := newAdminAuth()
	for i := range loginGlobalFailMax {
		a.recordFail(testIP(i))
	}
	if a.allow(testIP(9999)) {
		t.Fatal("allow() = true after the process-wide budget was crossed, want global lockout")
	}
	if a.globalLevel != 1 {
		t.Errorf("globalLevel = %d, want 1 after first breach", a.globalLevel)
	}
	if until := time.Until(a.globalUntil); until < loginGlobalLockout-time.Second || until > loginGlobalLockout+time.Second {
		t.Errorf("first global lockout = %v, want ~%v", until, loginGlobalLockout)
	}

	// While the global lockout is active the budget is not re-armed.
	for i := range loginGlobalFailMax {
		a.recordFail(testIP(10000 + i))
	}
	if a.globalLevel != 1 {
		t.Errorf("globalLevel = %d during active lockout, want still 1", a.globalLevel)
	}

	// After expiry, a fresh breach doubles the lockout.
	a.globalUntil = time.Now().Add(-time.Second)
	a.globalWindow = time.Now().Add(-2 * loginGlobalWindow)
	a.globalFails = 0
	for i := range loginGlobalFailMax {
		a.recordFail(testIP(20000 + i))
	}
	if a.globalLevel != 2 {
		t.Errorf("globalLevel = %d after second breach, want 2", a.globalLevel)
	}
	if until := time.Until(a.globalUntil); until < 2*loginGlobalLockout-time.Second || until > 2*loginGlobalLockout+time.Second {
		t.Errorf("second global lockout = %v, want ~%v (doubled)", until, 2*loginGlobalLockout)
	}

	// The lockout duration is capped at loginGlobalLockoutMax.
	a.globalUntil = time.Now().Add(-time.Second)
	a.globalWindow = time.Now().Add(-2 * loginGlobalWindow)
	a.globalFails = 0
	for i := range 10 {
		a.globalUntil = time.Now().Add(-time.Second)
		a.globalWindow = time.Now().Add(-2 * loginGlobalWindow)
		a.globalFails = 0
		for range loginGlobalFailMax {
			a.recordFail(testIP(30000 + i))
		}
	}
	if until := time.Until(a.globalUntil); until > loginGlobalLockoutMax {
		t.Errorf("global lockout = %v exceeds the cap %v", until, loginGlobalLockoutMax)
	}
}

// TestAdminAuthLoginSlotBound pins the concurrent-login semaphore:
// the bounded slots reject overflow and hand the slot back on release.
func TestAdminAuthLoginSlotBound(t *testing.T) {
	a := newAdminAuth()
	for range loginConcurrencyMax {
		if !a.tryLogin() {
			t.Fatal("tryLogin = false below the concurrency bound")
		}
	}
	if a.tryLogin() {
		t.Fatal("tryLogin = true above the concurrency bound")
	}
	a.releaseLogin()
	if !a.tryLogin() {
		t.Fatal("tryLogin = false after one release")
	}
	// Drain every slot, releasing exactly once per acquire.
	for range loginConcurrencyMax {
		a.releaseLogin()
	}
	if len(a.loginSlots) != 0 {
		t.Errorf("%d slots held after balanced acquire/release, want 0", len(a.loginSlots))
	}
}

// TestUpdateEnvKeysRejectsNewline pins the .env writer guard: updateEnvKeys writes raw
// Key=Value lines, so a value carrying a CR/LF would inject a second .env
// line or shred CRLF endings; it must be rejected before any write.
func TestUpdateEnvKeysRejectsNewline(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", []byte("SAFE_MODE=true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"a\nb", "a\rb", "a\r\nb"} {
		if _, err := updateEnvKeys([]config.EnvUpdate{{Key: "AUTH_TOKENS", Value: bad}}); err == nil {
			t.Errorf("updateEnvKeys(%q) = nil error, want rejection", bad)
		}
	}
	got, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "SAFE_MODE=true\n" {
		t.Errorf(".env mutated by rejected update: %q", got)
	}
}

// TestUpdateAuthTokensEnvRejectsComma pins the comma-joined list guard: AUTH_TOKENS is a
// comma-joined list, so an interior comma in one token would split it on
// the next reload; the whole update must be rejected.
func TestUpdateAuthTokensEnvRejectsComma(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := updateAuthTokensEnv([]string{"cb-ok", "cb,bad"}); err == nil {
		t.Fatal("updateAuthTokensEnv with comma-bearing token = nil error, want rejection")
	}
	if _, err := updateAuthTokensEnv([]string{"cb-ok", "cb\nbad"}); err == nil {
		t.Fatal("updateAuthTokensEnv with newline-bearing token = nil error, want rejection")
	}
	if _, err := updateAuthTokensEnv([]string{"cb-ok", "cb-two"}); err != nil {
		t.Fatalf("updateAuthTokensEnv clean list = %v, want nil", err)
	}
}

// TestNewAdminAuthKeyRandom verifies the boot-time key generation never
// leaves a zero key: the constructor panics on RNG failure, so a
// live adminAuth must always carry a non-zero key.
func TestNewAdminAuthKeyRandom(t *testing.T) {
	a := newAdminAuth()
	var zero [32]byte
	if a.key == zero {
		t.Fatal("newAdminAuth key is all zeroes")
	}
}
