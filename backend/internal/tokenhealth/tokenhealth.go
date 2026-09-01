// Package tokenhealth holds the -validate-tokens report vocabulary and the
// pure helpers that the upstream client's health probes use: the account-state
// and email-domain-risk classifications, the email-domain lists mirroring
// reference/freebuff common/src/util/disposable-email.ts, and the report
// formatting. The client-dependent probing (CheckTokenHealth/ValidateTokens/
// FetchAccountInfo) stays in the upstream package, which imports this one for
// the pure logic and keeps the exported symbols it needs.
package tokenhealth

import (
	"fmt"
	"strings"
)

// TokenHealthState is one row's upstream account state. BANNED and INVALID
// are terminal (never recoverable); COUNTRY_BLOCKED is terminal for this
// account+egress; RATE_LIMITED/SPEND_LIMITED are caps or transient and only
// need cooldown; UNKNOWN means the probes could not reach a verdict.
type TokenHealthState string

const (
	TokenOK             TokenHealthState = "OK"
	TokenRateLimited    TokenHealthState = "RATE_LIMITED"
	TokenSpendLimited   TokenHealthState = "SPEND_LIMITED"
	TokenCountryBlocked TokenHealthState = "COUNTRY_BLOCKED"
	TokenBanned         TokenHealthState = "BANNED"
	TokenInvalid        TokenHealthState = "INVALID"
	TokenUnknown        TokenHealthState = "UNKNOWN"
)

// EmailRiskKind is the email-domain classification, mirroring upstream's
// FlaggedEmailDomainKind (disposable-email.ts classifyEmailDomain).
type EmailRiskKind string

const (
	EmailRiskClean      EmailRiskKind = "clean"
	EmailRiskMainstream EmailRiskKind = "mainstream"
	EmailRiskRelay      EmailRiskKind = "relay"
	EmailRiskDisposable EmailRiskKind = "disposable"
)

// TokenHealth is one row of the -validate-tokens report. Email is always
// masked (domain kept, local part masked); the raw address never leaves the
// probe.
type TokenHealth struct {
	Index  int
	State  TokenHealthState
	Risk   EmailRiskKind
	Shared bool
	Email  string // masked; "" when /api/v1/me returned no email
	Hint   string
}

// FlagSharedMailboxes marks Shared=true on every row whose mailbox is held
// by more than one token in the set. Mailbox identity is the lowercased
// domain plus the lowercased local part with any "+tag" alias suffix
// stripped (gmail-style tags share the same physical mailbox). The
// upstream referral risk caps at >=3 accounts per mailbox; the server-side
// count is unknowable from here, so a repeat is only a warning, never a
// verdict. A bare local-part match across different domains is NOT a shared
// mailbox (bob@a.com / bob@b.com) and is deliberately not flagged.
func FlagSharedMailboxes(rows []TokenHealth) {
	counts := map[string]int{}
	keys := make([]string, len(rows))
	for i := range rows {
		k, ok := mailboxKey(rows[i].Email)
		keys[i] = k
		if ok {
			counts[k]++
		}
	}
	for i := range rows {
		if keys[i] != "" && counts[keys[i]] > 1 {
			rows[i].Shared = true
			rows[i].Hint = JoinHint(rows[i].Hint, "SHARED mailbox: same mailbox on "+fmt.Sprintf("%d token(s)", counts[keys[i]])+" (upstream caps >=3 accounts per mailbox; server-side count unknown)")
		}
	}
}

// FormatHealthReport renders the -validate-tokens table: one row per token
// (index, state, email-domain risk, shared flag, masked email, hint).
func FormatHealthReport(rows []TokenHealth) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%3s  %-17s  %-10s  %-7s  %-30s  %s\n", "#", "STATE", "RISK", "SHARED", "EMAIL", "HINT")
	rule := strings.Repeat("-", 78)
	b.WriteString(rule)
	b.WriteByte('\n')
	for _, r := range rows {
		shared := "no"
		if r.Shared {
			shared = "yes"
		}
		fmt.Fprintf(&b, "%3d  %-17s  %-10s  %-7s  %-30s  %s\n",
			r.Index+1, string(r.State), string(r.Risk), shared, r.Email, r.Hint)
	}
	return b.String()
}

// SessionStateFromStatus maps a 200 session status on the probe path (the
// ProbeAccount error conversions already handled banned/country_blocked/
// ended) onto the report vocabulary.
func SessionStateFromStatus(status string) (TokenHealthState, string) {
	switch status {
	case "none", "active", "ended", "queued":
		return TokenOK, "session " + status
	case "rate_limited":
		return TokenRateLimited, "session rate limited upstream (soft — cooldown, not death)"
	case "spend_limited":
		return TokenSpendLimited, "session spend ceiling reached (soft — cooldown, not death)"
	case "ip_capped":
		return TokenRateLimited, "session ip_capped (admission-only, soft)"
	case "banned":
		return TokenBanned, "banned upstream (terminal)"
	case "country_blocked":
		return TokenCountryBlocked, "country_blocked upstream (terminal for this account+egress)"
	case "superseded":
		return TokenOK, "session held by another instance (superseded; not a token problem)"
	case "premium_slot_taken":
		return TokenOK, "premium slot already taken (not a token problem)"
	default:
		return TokenOK, "session status " + status + " (account reachable)"
	}
}

// --- email-domain classification (mirrors disposable-email.ts) ---

// disposableDomains mirrors DISPOSABLE_EMAIL_DOMAINS in
// reference/freebuff common/src/util/disposable-email.ts:31-219 — the
// one-time inbox providers plus the farm-observed rings, exactly as curated
// upstream (no lookalike that the upstream list deliberately excludes).
var disposableDomains = newDomainSet(
	// Classic one-time inbox providers.
	"10minutemail.com",
	"dispostable.com",
	"dropmail.me",
	"emailondeck.com",
	"fakeinbox.com",
	"getnada.com",
	"grr.la",
	"guerrillamail.com",
	"guerrillamail.net",
	"maildrop.cc",
	"mailinator.com",
	"mailnesia.com",
	"mail.tm",
	"minuteinbox.com",
	"mintemail.com",
	"mohmal.com",
	"sharklasers.com",
	"temp-mail.org",
	"tempinbox.com",
	"tempmail.com",
	"tempmail.dev",
	"throwawaymail.com",
	"trashmail.com",
	"yopmail.com",
	// Observed in Freebuff referral farms, 2026-07.
	"aifotoeditor.com",
	"animateany.com",
	"animatimg.com",
	"biscoito.email",
	"oldtranslator.com",
	// Observed 2026-08-01: 48-account free-mode compute ring.
	"l0veyou.com",
	"pumpkinai.space",
	"pumpkinai.it.com",
	// Compiled 2026-08-03 from prod (>=20 accounts, >=75% banned).
	"azahram.com",
	"barcondi.my.id",
	"bukitsakura.com",
	"cilisung.com",
	"cindohub.com",
	"duccky.com",
	"fomolu.com",
	"gamlo.my.id",
	"gamontok.com",
	"gehil.my.id",
	"geusil.com",
	"geusil.my.id",
	"ggmul.com",
	"ghyuil.my.id",
	"gkmaill.com",
	"gmaiko.com",
	"gmbel.com",
	"gmiliu.my.id",
	"gmisol.my.id",
	"gmito.my.id",
	"gmole.xyz",
	"gmosel.com",
	"gsuel.my.id",
	"gumel.store",
	"guzeil.com",
	"gwemol.my.id",
	"hayate.us",
	"jokowi.store",
	"jujusa.my.id",
	"mikontol.online",
	"monetsssky1.com",
	"satukataku.com",
	"simosel.site",
	"wdrvk.dpdns.org",
	"wdrvks.eu.org",
	"xabree.com",
	// Added 2026-08-03 on behavioural evidence.
	"proxyvpn.cn",
	"impact.qd.je",
	"fincy.qd.je",
	// Compiled 2026-08-15 from 14d of prod logs (SG farm).
	"dhisy.com",
	"dewaa.id",
	"sendang.space",
	"yotube.id",
	"gusil.my.id",
	// Added 2026-08-24 on honeypot probing.
	"duojumbo.online",
	"gmaoiil.com",
	"itesun.com",
	"duojumbo.com",
	"geusil.com",
)

// mainstreamPrivacyDomains mirrors MAINSTREAM_PRIVACY_EMAIL_DOMAINS
// (disposable-email.ts:221-239): big-brand consumer privacy mailboxes —
// classified so the operator SEES them, but deliberately NOT priced by the
// upstream spend ceiling.
var mainstreamPrivacyDomains = newDomainSet(
	"proton.me",
	"protonmail.ch",
	"protonmail.com",
	"pm.me",
	"privaterelay.appleid.com",
	"duck.com",
	"mozmail.com",
	"tuta.com",
	"tuta.io",
	"tutamail.com",
	"tutanota.com",
)

// privacyRelayDomains mirrors PRIVACY_RELAY_EMAIL_DOMAINS
// (disposable-email.ts:241-248): alias/relay services whose product IS
// mailbox multiplication — classified and still priced upstream.
var privacyRelayDomains = newDomainSet(
	"passmail.net",
	"aleeas.com",
	"anonaddy.me",
	"simplelogin.com",
	"simplelogin.io",
)

// domainSet is an immutable lowercase-exact domain set.
type domainSet map[string]struct{}

func newDomainSet(domains ...string) domainSet {
	s := make(domainSet, len(domains))
	for _, d := range domains {
		s[d] = struct{}{}
	}
	return s
}

// ClassifyEmailDomain ports upstream's classifyEmailDomain: exact-domain or
// any-subdomain match (a.b.mailinator.com → mailinator.com), case-insensitive.
// Clean for unparseable email (upstream returns null there).
func ClassifyEmailDomain(email string) EmailRiskKind {
	domain := emailDomainOf(email)
	if domain == "" {
		return EmailRiskClean
	}
	switch {
	case domainInSet(domain, disposableDomains):
		return EmailRiskDisposable
	case domainInSet(domain, privacyRelayDomains):
		return EmailRiskRelay
	case domainInSet(domain, mainstreamPrivacyDomains):
		return EmailRiskMainstream
	}
	return EmailRiskClean
}

// emailDomainOf returns the lowercased, trimmed domain after the last '@'.
func emailDomainOf(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(email[at+1:]))
}

// domainInSet matches the domain exactly or any of its parent suffixes
// (upstream matchesSet). A lookalike like mailinator.com.evil.com never
// matches: only full suffix labels are tested.
func domainInSet(domain string, set domainSet) bool {
	if _, ok := set[domain]; ok {
		return true
	}
	rest := domain
	for {
		i := strings.Index(rest, ".")
		if i < 0 {
			return false
		}
		rest = rest[i+1:]
		if _, ok := set[rest]; ok {
			return true
		}
	}
}

// MaskEmail keeps the domain and masks the local part (first rune, "***",
// last rune) so the report never prints a full mailbox address.
func MaskEmail(email string) string {
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return ""
	}
	local := email[:at]
	domain := strings.ToLower(email[at+1:])
	runes := []rune(local)
	switch len(runes) {
	case 0:
		return "*@" + domain
	case 1:
		return "*@" + domain
	case 2:
		return string(runes[0]) + "*@" + domain
	default:
		return string(runes[0]) + "***" + string(runes[len(runes)-1]) + "@" + domain
	}
}

// mailboxKey returns the shared-mailbox comparison key for an email: the
// lowercased domain plus the lowercased local part with any "+tag" alias
// suffix stripped. ok=false for unparseable addresses.
func mailboxKey(email string) (string, bool) {
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return "", false
	}
	local := strings.ToLower(strings.TrimSpace(email[:at]))
	domain := strings.ToLower(strings.TrimSpace(email[at+1:]))
	if i := strings.Index(local, "+"); i >= 0 {
		local = local[:i]
	}
	return domain + "|" + local, true
}

// RiskHint renders the operator-facing meaning of a non-clean mailbox risk
// (upstream freebuff-spend-ceilings.ts: restricted-risk cohorts meet a
// $0.50/day spend ceiling and a separate 2x hard cap; mainstream privacy
// mailboxes are classified but never priced).
func RiskHint(k EmailRiskKind) string {
	switch k {
	case EmailRiskDisposable:
		return "disposable mailbox (RISK-HIGH: upstream prices a $0.50/day restricted ceiling + 2x hard cap)"
	case EmailRiskRelay:
		return "privacy-relay mailbox (RISK-MED: aliases stay priced upstream)"
	case EmailRiskMainstream:
		return "mainstream-privacy mailbox (RISK-LOW: classified but NOT priced upstream)"
	}
	return ""
}

// JoinHint joins non-empty hint fragments with "; ".
func JoinHint(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "; ")
}
