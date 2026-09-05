// Token-health validation mode (-validate-tokens): a read-only, per-token
// account report for operators. For each token it performs exactly two
// non-mutating upstream calls — GET /api/v1/me?fields=id,email,discord_id
// and the CLI's own token probe GET /api/v1/freebuff/session (no instance
// id) — and classifies the result (OK / rate/spend limited / country
// blocked / banned / invalid / unknown), the email-domain risk from the
// upstream referral-abuse classifier, and a shared-mailbox heuristic.
//
// The anti-ban contract is absolute here: this code NEVER POSTs
// (no session admission), NEVER DELETEs, NEVER writes AUTH_TOKENS, and
// never revives or rotates anything. It only reports, so an operator can
// find dead or risky tokens before the pool burns requests on them.
//
// Email-domain lists mirror @codebuff/common/src/util/disposable-email.ts
// (reference/freebuff common/src/util/disposable-email.ts): exact-domain or
// any-subdomain match, case-insensitive. Lists are deliberately curated —
// lookalikes/substrings that are not exact entries (gmisel.com,
// mailinator.com.evil.com) are never flagged, exactly like upstream.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"freebuff-proxy/backend/internal/config"
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

// tokenHealthProbeTimeout bounds BOTH probes for one token. Smaller than
// the session-call timeout on purpose: a validation run over N tokens must
// not take unbounded wall time on a hanging upstream.
const tokenHealthProbeTimeout = 20 * time.Second

// CheckTokenHealth probes one token with exactly the non-mutating calls and
// returns its report row. Error is only for unreachable preconditions
// (request construction); probe failures classify into TokenUnknown rows,
// never an error — the caller always gets a row per token.
func CheckTokenHealth(ctx context.Context, c *Client) (TokenHealth, error) {
	row := TokenHealth{Index: c.tokenIndex, Risk: EmailRiskClean}

	if IsDummyToken(c.token) {
		// Package convention: dummy/mock tokens are never probed against the
		// network (see ProbeAccount/GetSession); report them as OK so pooled
		// mock fixtures keep working.
		row.State = TokenOK
		row.Hint = "mock token (no upstream probes executed)"
		return row, nil
	}

	acct, meStatus, meErr := c.probeMe(ctx)

	// Terminal on /api/v1/me: 401 = token rejected, 403 = auth/banned class.
	// These are unambiguous; no further probing needed for 401. A 403 is
	// refined by the session probe so a country_blocked account is reported
	// as COUNTRY_BLOCKED instead of BANNED (both exit 1 — the account is not
	// usable from this egress either way).
	if meErr == nil {
		switch meStatus {
		case http.StatusUnauthorized: // 401
			row.State = TokenInvalid
			row.Hint = "token rejected by /api/v1/me (401)"
			return row, nil
		case http.StatusForbidden: // 403
			row.State = TokenBanned
			sessState, sessHint := c.probeSession(ctx)
			if sessState == TokenCountryBlocked {
				row.State = TokenCountryBlocked
			}
			// The session probe only contributes when it found a *worse or
			// more specific* verdict; an OK verdict there adds nothing to
			// the terminal me classification.
			if sessState == TokenCountryBlocked || sessState == TokenBanned ||
				sessState == TokenInvalid || sessState == TokenUnknown {
				row.Hint = joinHint(fmt.Sprintf("account not usable (HTTP %d from /api/v1/me)", meStatus), sessHint)
			} else {
				row.Hint = fmt.Sprintf("account not usable (HTTP %d from /api/v1/me)", meStatus)
			}
			return row, nil
		}
	}
	// Non-terminal me outcome (200, or transport failure / 5xx / parse
	// error): the session probe is the authoritative classification; the
	// account-level check only supplies the mailbox.
	if meErr == nil && acct != nil {
		row.Email = maskEmail(acct.Email)
		row.Risk = classifyEmailDomain(acct.Email)
	}

	state, hint := c.probeSession(ctx)
	row.State = state
	row.Hint = hint
	switch {
	case meErr != nil:
		row.Hint = joinHint("/api/v1/me probe failed: "+meErr.Error(), row.Hint)
	case meStatus != http.StatusOK && meStatus != 0:
		row.Hint = joinHint(fmt.Sprintf("/api/v1/me returned HTTP %d; email unknown", meStatus), row.Hint)
	}
	if acct != nil && acct.Email == "" {
		row.Hint = joinHint(row.Hint, "me returned no email — mailbox risk not classified")
	}
	if r := riskHint(row.Risk); r != "" {
		row.Hint = joinHint(row.Hint, r)
	}
	return row, nil
}

// ValidateTokens builds one client per token (same constructor as the pool)
// and runs CheckTokenHealth for each, bounded by tokenHealthProbeTimeout per
// token. A construction error (bad config) aborts the run — probe failures
// never do.
func ValidateTokens(ctx context.Context, cfg *config.Config, tokens []string) ([]TokenHealth, error) {
	rows := make([]TokenHealth, 0, len(tokens))
	for i, tok := range tokens {
		c, err := NewWithIndex(tok, i, cfg)
		if err != nil {
			return nil, fmt.Errorf("token #%d: %w", i+1, err)
		}
		tokCtx, cancel := context.WithTimeout(ctx, tokenHealthProbeTimeout)
		row, err := CheckTokenHealth(tokCtx, c)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("token #%d: %w", i+1, err)
		}
		rows = append(rows, row)
	}
	return rows, nil
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
			rows[i].Hint = joinHint(rows[i].Hint, "SHARED mailbox: same mailbox on "+fmt.Sprintf("%d token(s)", counts[keys[i]])+" (upstream caps >=3 accounts per mailbox; server-side count unknown)")
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

// probeMe performs GET /api/v1/me?fields=id,email,discord_id, the CLI's own
// account-state check (Bearer; codebuff-api.ts me()/request()). The URL
// shape mirrors the CLI exactly: fields joined by ',' and encoded by
// URLSearchParams (codebuff-api.test.ts pins fields=id%2Cemail).
func (c *Client) probeMe(ctx context.Context) (*meAccount, int, error) {
	q := url.Values{}
	q.Set("fields", "id,email,discord_id")
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v1/me?"+q.Encode(), nil)
	if err != nil {
		return nil, 0, err
	}
	resp, cancel, classErr := c.do(req, c.sessionCallTimeout)
	if classErr != nil && resp == nil {
		return nil, 0, classErr
	}
	defer releaseCancel(cancel)
	defer func() { _ = resp.Body.Close() }()
	body := drainBody(resp.Body)
	if classErr != nil || resp.StatusCode != http.StatusOK {
		// A non-200 response is the path's VERDICT, not a probe failure:
		// 401/403 carry the terminal classification and 5xx is a "me
		// unavailable" signal — the caller switches on the status code.
		// Only transport/parse problems are errors.
		return nil, resp.StatusCode, nil
	}
	var acct meAccount
	if err := json.Unmarshal([]byte(body), &acct); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse: %w", err)
	}
	return &acct, resp.StatusCode, nil
}

// meAccount is the parsed /api/v1/me response (UserDetails T = id/email/
// discord_id; discord_id is nullable per the CLI type).
type meAccount struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	DiscordID string `json:"discord_id"`
}

// FetchAccountInfo queries GET /api/v1/me to retrieve the account email and id.
func (c *Client) FetchAccountInfo(ctx context.Context) (email, id string, err error) {
	if IsDummyToken(c.token) {
		return "", "", nil
	}
	acct, status, err := c.probeMe(ctx)
	if err != nil {
		return "", "", err
	}
	if status != http.StatusOK || acct == nil {
		return "", "", fmt.Errorf("upstream returned status %d", status)
	}
	return acct.Email, acct.ID, nil
}

// probeSession reuses the Client's zero-cost token probe (GET
// /api/v1/freebuff/session with no x-freebuff-instance-id header — claims
// no session slot, consumes none of the daily allowance) and maps the
// shared sentinel matrix onto the report vocabulary:
//
//	404 / ended / none / active  -> OK      (idle or live, no problem)
//	403 banned/account_suspended -> BANNED
//	403 country_blocked          -> COUNTRY_BLOCKED
//	401                          -> INVALID
//	429 spend_limited            -> SPEND_LIMITED (soft, cooldown only)
//	429 rate_limited/ip_capped   -> RATE_LIMITED (soft, cooldown only)
//	402 no credits               -> SPEND_LIMITED (soft)
//	5xx / 428 / unknown          -> UNKNOWN
func (c *Client) probeSession(ctx context.Context) (TokenHealthState, string) {
	state, err := c.ProbeAccount(ctx)
	if err == nil {
		return sessionStateFromStatus(state.Status)
	}
	switch {
	case errors.Is(err, ErrNoActiveSession):
		return TokenOK, "no active session (idle)"
	case errors.Is(err, ErrBanned):
		var ban *BanError
		_ = errors.As(err, &ban)
		hint := "banned upstream (body: " + truncate(err.Error(), 120) + ")"
		if ban != nil && !ban.ResumesAt.IsZero() {
			hint = "banned upstream (resumes at " + ban.ResumesAt.Format(time.RFC3339) + ")"
		}
		return TokenBanned, hint
	case errors.Is(err, ErrCountryBlocked):
		var cb *CountryBlockedError
		_ = errors.As(err, &cb)
		hint := "country_blocked upstream (free mode not available from this egress; terminal)"
		if cb != nil && cb.CountryCode != "" {
			hint = "country_blocked upstream (country " + cb.CountryCode + "; terminal for this account+egress)"
			if cb.CountryBlockReason != "" {
				hint += ", reason: " + cb.CountryBlockReason
			}
		}
		return TokenCountryBlocked, hint
	case errors.Is(err, ErrAuthRejected):
		return TokenInvalid, "token rejected upstream (HTTP 401)"
	case errors.Is(err, ErrRateLimited):
		var rle *RateLimitError
		_ = errors.As(err, &rle)
		if rle != nil && (rle.Status == "spend_limited" || strings.Contains(strings.ToLower(rle.Body), "spend_limited")) {
			return TokenSpendLimited, "spend ceiling reached upstream (reset " + rateLimitResetHint(rle) + "; soft — cooldown, not death)"
		}
		return TokenRateLimited, "rate limited upstream" + rateLimitResetHint(rle) + " (soft — cooldown, not death)"
	case errors.Is(err, ErrIpCapped):
		var ipc *IpCappedError
		_ = errors.As(err, &ipc)
		hint := "ip_capped: too many distinct users on this egress IP (admission-only, soft)"
		if ipc != nil && ipc.ActiveUsersForIP > 0 {
			hint = fmt.Sprintf("ip_capped: %d of %v distinct users on this egress IP (admission-only, soft)", ipc.ActiveUsersForIP, ipc.Limit)
		}
		return TokenRateLimited, hint
	case errors.Is(err, ErrCredits):
		return TokenSpendLimited, "no credits upstream (HTTP 402; soft — top-up or wait)"
	case errors.Is(err, ErrCapacityDeferred):
		return TokenOK, "free capacity deferred (transient queue, same-session retry)"
	case errors.Is(err, ErrSessionSuperseded):
		return TokenOK, "session held by another instance (superseded; not a token problem)"
	case errors.Is(err, ErrSessionLimitReached):
		return TokenOK, "account at concurrent-session limit (409; not a token problem)"
	case errors.Is(err, ErrModelIPLimited):
		return TokenRateLimited, "model limited on this egress IP (session fine, soft)"
	case errors.Is(err, ErrTurnSpendLimited):
		return TokenRateLimited, "turn spend breaker tripped upstream (runaway turn killed; soft — start a fresh turn, not a token problem)"
	case errors.Is(err, ErrWaitingRoom), errors.Is(err, ErrWaitingRoomRequired):
		return TokenUnknown, "upstream waiting room (transient; retry later)"
	case errors.Is(err, ErrFreeModeCLIRequired):
		return TokenUnknown, "free-mode CLI envelope rejected (403 free_mode_cli_required)"
	case errors.Is(err, ErrSessionInvalid):
		return TokenUnknown, "session invalid upstream (stale row; refresh on demand)"
	default:
		var ue *UpstreamError
		if errors.As(err, &ue) {
			if ue.Retryable {
				return TokenUnknown, "upstream transiently unavailable (HTTP " + fmt.Sprint(ue.Status) + ", retry later)"
			}
			return TokenUnknown, "upstream HTTP " + fmt.Sprint(ue.Status) + ": " + truncate(ue.Body, 120)
		}
		return TokenUnknown, "probe failed: " + truncate(err.Error(), 150)
	}
}

// sessionStateFromStatus maps a 200 session status on the probe path (the
// ProbeAccount error conversions already handled banned/country_blocked/
// ended) onto the report vocabulary.
func sessionStateFromStatus(status string) (TokenHealthState, string) {
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

// rateLimitResetHint renders the reset signal from a rate-limit error, or
// "" when the body carried none.
func rateLimitResetHint(rle *RateLimitError) string {
	if rle == nil {
		return ""
	}
	switch {
	case !rle.ResetAt.IsZero():
		return " (reset at " + rle.ResetAt.Format(time.RFC3339) + ")"
	case rle.RetryAfter > 0:
		return " (retry after " + rle.RetryAfter.String() + ")"
	}
	return ""
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

// classifyEmailDomain ports upstream's classifyEmailDomain: exact-domain or
// any-subdomain match (a.b.mailinator.com → mailinator.com), case-insensitive.
// Clean for unparseable email (upstream returns null there).
func classifyEmailDomain(email string) EmailRiskKind {
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

// maskEmail keeps the domain and masks the local part (first rune, "***",
// last rune) so the report never prints a full mailbox address.
func maskEmail(email string) string {
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

// riskHint renders the operator-facing meaning of a non-clean mailbox risk
// (upstream freebuff-spend-ceilings.ts: restricted-risk cohorts meet a
// $0.50/day spend ceiling and a separate 2x hard cap; mainstream privacy
// mailboxes are classified but never priced).
func riskHint(k EmailRiskKind) string {
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

// joinHint joins non-empty hint fragments with "; ".
func joinHint(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "; ")
}
