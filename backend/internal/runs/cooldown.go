package runs

// Token cooldown management: the cooldown durations and the classifiers
// that put a token into a cooldown window while remembering the triggering
// upstream error (rate limit, ip_capped, ban, country block) so Acquires
// keep surfacing the exact 429/403 + Retry-After instead of re-hitting
// upstream during the window.

import (
	cryptoRand "crypto/rand"
	"encoding/binary"
	"time"

	"freebuff-proxy/backend/internal/upstream"
)

// DefaultCooldown is the token cooldown applied on upstream auth rejection
// (PRD Â§5.3: "401 triggers 30-min token cooldown").
const DefaultCooldown = 30 * time.Minute

// countryBlockCooldown is the token cooldown applied when upstream reports a
// region block (country_blocked): long enough to stop the request hammer
// from re-hitting the blocked admission, short enough to re-probe after the
// client switches egress/VPN.
const countryBlockCooldown = 15 * time.Minute

// cooldownCeiling is the farthest future any cooldown deadline may extend
// (7 days, mirroring upstream.MaxCooldown). Applied defensively when
// converting upstream-controlled retry durations to deadlines: without it a
// huge RetryAfter (or a far-future ResetAt) would park the token in a
// cooldown for years.
const cooldownCeiling = 7 * 24 * time.Hour

// cappedAfter returns now.Add(d) clamped to at most now+cooldownCeiling.
func cappedAfter(now time.Time, d time.Duration) time.Time {
	if d > cooldownCeiling {
		d = cooldownCeiling
	}
	return now.Add(d)
}

// cappedDeadline clamps a future deadline to at most now+cooldownCeiling.
func cappedDeadline(t time.Time) time.Time {
	if ceiling := time.Now().Add(cooldownCeiling); t.After(ceiling) {
		return ceiling
	}
	return t
}

// maxIpCappedReAdmitsPerDay caps how many times one token may re-admit
// (and be refused ip_capped again) per Pacific day before it is locked
// until the next Pacific midnight. The CLI treats ip_capped as
// terminal-until-reset — it never loops an automatic re-admission — so the
// proxy mirrors that with this bounded budget instead of pacing an endless
// POST loop (issue #118). Test-shrinkable like the pool's unfit TTL
// (backend/internal/pool/unfit.go modelUnfitTTL).
var maxIpCappedReAdmitsPerDay = 3

// ipCappedCooldownJitter is the Â±fraction of retryAfterMs applied to the
// ip_capped re-admission window so concurrent tokens do not re-admit in
// lockstep (mirrors the CLI's 30sÂ±20% poll jitter; reference/freebuff
// cli/src/hooks/use-freebuff-session.ts).
const ipCappedCooldownJitter = 0.2

// Cooldown puts the token in a cooldown window of duration d (e.g.
// DefaultCooldown after an auth rejection). Durations <= 0 are ignored.
func (m *RunManager) Cooldown(d time.Duration) {
	if d <= 0 {
		return
	}
	m.mu.Lock()
	m.cooldownUntil = time.Now().Add(d)
	m.rateLimit = nil
	m.ban = nil
	m.banPermanent = false
	m.countryBlock = nil
	m.ipCapped = nil
	// The ban/country windows die with their remembered errors: leaving the
	// deadlines set would surface a stale future BannedUntil (healthz risk
	// gating via Snapshot) with no ban attached. Mirror ClearCooldowns.
	m.banUntil = time.Time{}
	m.countryUntil = time.Time{}
	m.ipCappedUntil = time.Time{}
	m.ipCappedReAdmits = 0
	m.ipCappedDayReset = time.Time{}
	m.mu.Unlock()
}

// ClearCooldowns removes any cooldown, rate-limit lock, and ban window so
// the token is immediately acquirable again (dashboard unlock action).
func (m *RunManager) ClearCooldowns() {
	m.mu.Lock()
	m.cooldownUntil = time.Time{}
	m.rateLimit = nil
	m.ban = nil
	m.banPermanent = false
	m.banUntil = time.Time{}
	m.countryBlock = nil
	m.countryUntil = time.Time{}
	m.ipCapped = nil
	m.ipCappedUntil = time.Time{}
	m.ipCappedReAdmits = 0
	m.ipCappedDayReset = time.Time{}
	m.mu.Unlock()
}

// CooldownUntil returns the cooldown deadline (zero when not cooling down).
func (m *RunManager) CooldownUntil() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cooldownUntil
}

// MaintenanceEligible reports whether this manager should receive background
// maintenance work: the cooldown window has passed AND no live ban is
// active. It is the single gate shared by runs.Maintain and every pool
// maintain/poll caller (issue #266), replacing the previously copy-pasted
// time.Now().Before(CooldownUntil()) || BanError() != nil predicate — the
// pool pre-gates and runs.Maintain's own internal check had divergent
// semantics (runs.Maintain carried no ban check, so a hard-banned token with
// a zero cooldown deadline passed it and was only saved by the pool gate).
func (m *RunManager) MaintenanceEligible() bool {
	if time.Now().Before(m.CooldownUntil()) {
		return false
	}
	return m.BanError() == nil
}

// CooldownRateLimit applies a rate-limit cooldown and remembers the error
// so subsequent Acquires surface 429 + Retry-After instead of a generic
// 502. Errors with RetryAfter <= 0 are ignored.
func (m *RunManager) CooldownRateLimit(rle *upstream.RateLimitError) {
	if rle == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if rle.RetryAfter > 0 {
		m.cooldownUntil = cappedAfter(time.Now(), rle.RetryAfter)
	} else if !rle.ResetAt.IsZero() && rle.ResetAt.After(time.Now()) {
		m.cooldownUntil = cappedDeadline(rle.ResetAt)
	} else {
		m.cooldownUntil = upstream.NextPacificMidnight()
	}
	m.rateLimit = rle
	m.ban = nil
	m.banUntil = time.Time{}
	m.banPermanent = false
	m.countryBlock = nil
	m.countryUntil = time.Time{}
	m.ipCapped = nil
	m.ipCappedUntil = time.Time{}
}

// CooldownIpCapped applies an ip_capped cooldown bounded to the body's
// retryAfterMs ONLY — never the Pacific-midnight quota lock (ip_capped is
// admission-only, not tied to a quota reset). The window honors the FULL
// retryAfterMs plus the CLI's Â±20% poll jitter (#118). The CLI treats
// ip_capped as terminal-until-reset — it never loops an automatic
// re-admission — so the token's re-admission budget is capped at
// maxIpCappedReAdmitsPerDay per Pacific day: once exhausted the token stays
// locked (remembered 429 ip_capped + Retry-After reflecting the remaining
// window) until the next Pacific midnight. Remembered so Acquires keep
// surfacing 429 ip_capped + Retry-After during the window instead of
// re-hitting upstream (mirrors CooldownRateLimit). Errors with
// RetryAfter <= 0 are ignored.
func (m *RunManager) CooldownIpCapped(ice *upstream.IpCappedError) {
	if ice == nil || ice.RetryAfter <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	reset := upstream.NextPacificMidnight()
	if m.ipCappedDayReset.IsZero() || !m.ipCappedDayReset.Equal(reset) {
		// New Pacific day (or first refusal): fresh re-admission budget.
		m.ipCappedReAdmits = 0
		m.ipCappedDayReset = reset
	}
	m.ipCappedReAdmits++
	m.rateLimit = nil
	m.ban = nil
	m.banUntil = time.Time{}
	m.banPermanent = false
	m.countryBlock = nil
	m.countryUntil = time.Time{}
	if m.ipCappedReAdmits >= maxIpCappedReAdmitsPerDay {
		// Budget exhausted: terminal until the next Pacific reset. Surface
		// the REMAINING window as Retry-After so downstream 429s are honest
		// about the lock instead of promising a re-admit that will not
		// happen today.
		terminal := *ice
		terminal.RetryAfter = time.Until(reset)
		m.ipCapped = &terminal
		m.ipCappedUntil = reset
		m.cooldownUntil = reset
		return
	}
	m.ipCapped = ice
	m.ipCappedUntil = cappedAfter(now, ice.RetryAfter+ipCappedJitter(ice.RetryAfter))
	m.cooldownUntil = m.ipCappedUntil
}

// ipCappedJitter returns a one-sided jitter of up to
// ipCappedCooldownJitter (20%) of base, crypto/rand-seeded so concurrent
// tokens never re-admit in lockstep (mirrors the CLI's 30sÂ±20% poll
// jitter; reference/freebuff cli/src/hooks/use-freebuff-session.ts).
func ipCappedJitter(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	var b [8]byte
	_, _ = cryptoRand.Read(b[:])
	u := binary.BigEndian.Uint64(b[:])
	extra := int64(u % uint64(float64(base)*ipCappedCooldownJitter))
	return time.Duration(extra)
}

// IpCappedError returns the remembered ip_capped error while its short
// cooldown window is active, nil otherwise.
func (m *RunManager) IpCappedError() *upstream.IpCappedError {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Now().Before(m.ipCappedUntil) && m.ipCapped != nil {
		return m.ipCapped
	}
	return nil
}

// RateLimitError returns the remembered rate-limit error while its
// cooldown is still active, nil otherwise.
func (m *RunManager) RateLimitError() *upstream.RateLimitError {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Now().Before(m.cooldownUntil) && m.rateLimit != nil {
		return m.rateLimit
	}
	return nil
}

// CooldownBan applies a ban cooldown and remembers the error so Acquires
// keep surfacing 403 banned + resumes-at until the unban time.
func (m *RunManager) CooldownBan(be *upstream.BanError) {
	if be == nil {
		return
	}
	m.mu.Lock()
	m.ban = be
	if be.ResumesAt.IsZero() {
		// Hard ban (no resumes_at): the account is dead upstream — trust
		// caps (past_enforcement) make it permanent, and a timed retry only
		// generates repeated 403 contacts against a banned account. Keep
		// the remembered ban live indefinitely (banUntil zero = permanent:
		// BanError() returns it, banView renders hard/zero, Acquire skips
		// via the BanError guard) until the operator clears it (dashboard
		// unlock / AUTH_TOKENS change).
		m.banUntil = time.Time{}
		m.banPermanent = true
	} else if !be.ResumesAt.After(time.Now()) {
		// resumes_at present but past: an expired temporary ban — already
		// lifted upstream, so keep no ban memory at all. Retiring it would
		// wrongly kill a merely-expired temporary ban; a stale window would
		// only delay the next (correct) admission.
		m.ban = nil
		m.banUntil = time.Time{}
		m.banPermanent = false
	} else {
		m.banUntil = be.ResumesAt
		m.banPermanent = false
	}
	// The ban also fills the shared cooldown deadline so Acquire skips the
	// token entirely during the window (the remembered error is surfaced by
	// the cooldown-skip branch instead of re-hitting upstream).
	m.cooldownUntil = m.banUntil
	m.rateLimit = nil // a ban supersedes any rate-limit cooldown
	m.countryBlock = nil
	m.ipCapped = nil
	m.mu.Unlock()
}

// BanError returns the remembered ban error while the ban window is
// active, nil otherwise. A permanent (hard) ban is always live.
func (m *RunManager) BanError() *upstream.BanError {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ban != nil && (m.banPermanent || time.Now().Before(m.banUntil)) {
		return m.ban
	}
	return nil
}

// CooldownCountryBlocked applies a country-block cooldown and remembers the
// error so Acquires keep surfacing the region-block instead of re-hitting
// upstream during the window (mirrors CooldownRateLimit/CooldownBan).
func (m *RunManager) CooldownCountryBlocked(cbe *upstream.CountryBlockedError) {
	if cbe == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// A ban outranks a country block (pool precedence ban > country): keep
	// the ban window and its remembered error instead of downgrading to the
	// shorter country cooldown.
	if m.ban != nil && (m.banPermanent || time.Now().Before(m.banUntil)) {
		return
	}
	m.countryBlock = cbe
	m.countryUntil = time.Now().Add(countryBlockCooldown)
	// The block also fills the shared cooldown deadline so Acquire skips
	// the token entirely during the window (the remembered error is
	// surfaced by the cooldown-skip branch instead of re-hitting upstream).
	m.cooldownUntil = m.countryUntil
	m.rateLimit = nil
	m.ban = nil
	m.banPermanent = false
	m.banUntil = time.Time{}
	m.ipCapped = nil
}

// CountryBlockedError returns the remembered country-block error while its
// cooldown window is active, nil otherwise.
func (m *RunManager) CountryBlockedError() *upstream.CountryBlockedError {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Now().Before(m.countryUntil) && m.countryBlock != nil {
		return m.countryBlock
	}
	return nil
}
