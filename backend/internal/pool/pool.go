// Package pool is the multi-token front door: it picks the token that will
// serve a model request, then leases a run from that token's RunManager and
// an instance from its session manager. Port of freebuff2api-quorinex
// run_manager.go (Acquire half) with the upstream/session/runs split of this
// project.
//
// Failover semantics (PRD §6 error matrix):
//   - 401 (ErrAuthRejected) from a token's run START → 30-min cooldown for
//     that token, try the next.
//   - session waiting room → remember the best position, try the next token;
//     when every token fails, the pool surfaces the highest-precedence
//     non-empty error bucket (ban > country-blocked > model-IP-limited >
//     rate-limit > waiting-room > daily cap) instead of a generic 502 — a
//     queued token surfaces 503 + Retry-After as soon as no higher bucket
//     is populated.
//   - run-invalid / session-invalid recoveries are NOT handled here: the
//     caller (server) retries once via a fresh Acquire after invalidating.
//   - anything else → next token; all failed → combined error (only when no
//     error-bucket matched any token).
package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/notify"
	"freebuff-proxy/backend/internal/registry"
	"freebuff-proxy/backend/internal/runs"
	"freebuff-proxy/backend/internal/session"
	"freebuff-proxy/backend/internal/upstream"
)

// usageWindow is the rolling window for the per-token daily message cap
// (MAX_MESSAGES_PER_DAY): a token may send at most N successful chat
// requests per 24h of usage history.
const usageWindow = 24 * time.Hour

// shutdownTimeout bounds each token's Shutdown during Pool.Shutdown when the
// caller's context carries no earlier deadline.
const shutdownTimeout = 10 * time.Second

// Lease is one acquired right to send a chat request through a specific
// token. The caller must call Pool.LeaseRelease when the request completes
// or fails (it decrements the run's inflight counter).
type Lease struct {
	Token             int    // index into config.AuthTokens (-1 for bridge leases)
	Model             string // the model this lease's session/run is bound to (authoritative for opts.Model; may differ from the requested model after #100 fallback)
	AgentID           string
	Run               *runs.Run
	SessionInstanceID string       // "" when the session is disabled
	Bridge            *bridgeEntry // nil for pooled (fixed-token) leases
	// FallbackReason names WHY this lease serves a different model than the
	// requested one (issue #164): "quota_exhausted" when the QUOTA_FALLBACK_MODELS
	// path re-routed the request after every quota-positive token was
	// exhausted; "queue_timeout" when the server's FALLBACK_AFTER_MS
	// waiting-room path re-routed. Empty when the lease serves the requested
	// model directly. Surfaced to clients as the X-FreeBuff-Fallback
	// response header.
	FallbackReason string
	// entry is the fixed-token entry backing this lease. Set by Acquire so
	// LeaseRelease always releases through the right run manager: after a
	// concurrent RemoveLastToken, the Token index may be out of range (or
	// reused by a later AddToken), and a bounds-checked release would leak
	// the run's inflight or hit an unrelated manager.
	entry *tokenEntry
	// AcquiredAt is when this lease was handed out (per acquire attempt,
	// not per run — a chat retry re-acquires and gets a fresh timestamp).
	// The chat success path uses it to clear unfit marks that PREDATE this
	// admission (a retry's fresh acquire proves the mark stale, while an
	// older in-flight chat's success must not erase a mark that landed
	// after its admission).
	AcquiredAt time.Time
}

// TokenSnapshot is one token's healthz view.
type TokenSnapshot struct {
	Token                   int
	Email                   string `json:"email,omitempty"`
	AccountID               string `json:"account_id,omitempty"`
	CooldownUntil           time.Time
	SessionStatus           string
	SessionInstanceID       string
	SessionQueuePosition    int
	SessionQueueDepth       int
	SessionModel            string
	SessionRemainingSeconds int64
	// SessionExpiresAt is the server-authored absolute session expiry
	// (wire expiresAt). Monotonic across compact polls, unlike the wire
	// remainingMs which only rides full admissions; the dashboard countdown
	// derives from it so a poll cycle can never re-anchor the timer.
	SessionExpiresAt time.Time `json:"session_expires_at,omitempty"`
	ActiveRuns       int
	Requests         int
	Messages24h      int    // successful chats in the last 24h (MAX_MESSAGES_PER_DAY usage)
	DailyLimit       int    // configured MAX_MESSAGES_PER_DAY (0 = unlimited)
	UsagePct         int    // percentage of daily limit used (0 when unlimited)
	RiskLevel        string // "low", "moderate", "high", "critical" account safety indicator (#6)
	// Spend24h / SpendDay / SpendWeek / SpendMonth are the local per-token
	// spend ledger (issue #87/#122): tokens spent in the rolling 24h window
	// and the current Pacific day/week/month buckets (with rollover —
	// boundaries are America/Los_Angeles wall-clock, DST-correct). Fed by
	// pool.RecordSpend from chat usage blocks; surfaced next to Messages24h.
	Spend24h        int64
	SpendDay        int64
	SpendWeek       int64
	SpendMonth      int64
	SpendDayStart   time.Time
	SpendWeekStart  time.Time
	SpendMonthStart time.Time
	// SpendLimit is the configured MAX_SPEND_PER_DAY ADVISORY ceiling in
	// ledger units (0 = unlimited). Never enforced: the upstream $ ceilings
	// ($15 full / $5 limited / $0.50 restricted, server-enforced, issue
	// #122) are the real gate and the proxy cannot know the account's
	// restricted cohort. SpendPct is the Pacific-day bucket's percentage of
	// SpendLimit (0 when unlimited). SpendLimited counts upstream
	// spend_limited refusals observed for this token since process start.
	SpendLimit   int64
	SpendPct     int
	SpendLimited int
	// CountryCode / CountryBlockReason are the token's last known upstream
	// region-block state. CountryBlockReason is non-empty when the account
	// (or its egress region) is blocked; surfaced by /v1/models availability
	// annotation and healthz.
	CountryCode        string
	CountryBlockReason string
	// AccessTier is the upstream access tier from the last session admission
	// ("full", "limited", "free"); "" until reported.
	AccessTier string `json:"access_tier,omitempty"`
	// SessionActiveUsersForIP is the last known distinct-user count on the
	// token's egress IP (upstream activeUsersForIp); zero when the session
	// response did not carry it.
	SessionActiveUsersForIP int
	// QuotaByModel is the live per-model session quota from the last
	// admission (key = model id); empty until the session reports it.
	QuotaByModel map[string]session.QuotaSnapshot
	Entitlement  map[string]float64
	// GlmPromo is the raw upstream glmPromo block ({dailySessions, endsAt})
	// from the token's last admission (issue #178); "" when absent. The
	// dashboard synthesizes the z-ai/glm-5.2 promo quota row from it.
	GlmPromo string
	// QuotaStale marks quota restored from the on-disk session entry after
	// a restart (no live admission yet this process); QuotaSavedAt is when
	// that entry was last polled. The dashboard labels it last-seen.
	QuotaStale   bool
	QuotaSavedAt time.Time
	// Standing is the upstream account standing block (issue #96); nil until
	// the session reports it.
	Standing *upstream.SessionStanding
	// Referral is the upstream referral block (FreebuffReferralInfo); nil until
	// the session reports it.
	Referral *upstream.SessionReferral
	// Freebucks is the upstream Freebucks allowance block (issue #232); nil until
	// the session reports it.
	Freebucks *upstream.FreebucksInfo `json:"freebucks,omitempty"`
	// FreeWindows is the upstream free-tier pool windows block
	// (day/week/month; issue #319). Display-only; nil until reported.
	FreeWindows *upstream.FreeWindowsInfo `json:"free_windows,omitempty"`
	// Subscription is the upstream subscription usage block (issue #319);
	// rollout-audience only; nil until reported.
	Subscription *upstream.SubscriptionInfo `json:"subscription,omitempty"`
	// UpgradeHint is the upstream upgradeHint block ({url, message})
	// broadcast by the session server; nil when absent.
	UpgradeHint *upstream.SessionUpgradeHint `json:"upgrade_hint,omitempty"`
	// ServerMessage is any live broadcast or error message sent by the
	// session server; "" when absent.
	ServerMessage string `json:"server_message,omitempty"`
	// TransientRetries / FingerprintRotations are this token's upstream
	// client counters (TRANSIENT_RETRIES): retried transport failures and
	// pinned TLS fingerprint swaps. Surfaced per-token in /metrics.
	TransientRetries     int64
	FingerprintRotations int64
	// RateLimitEvents is this token's upstream rate-limit classification
	// ledger, keyed by upstream body code (rate_limited, ip_capped,
	// spend_limited, insufficient_quota, limit_burst_rate,
	// free_mode_rate_limited, ...). Surfaced per-token in /metrics.
	RateLimitEvents map[string]int64
	// ModelLocked tallies model-lock session releases keyed by from → to
	// model pair (issue #160): each model_locked admission releases the
	// old slot and re-admits with the requested model. Surfaced per-token
	// in /metrics as freebuff_proxy_model_locked_total.
	ModelLocked map[string]map[string]int64
	// Locked is set when the token has been administratively locked by the
	// operator; Acquire never selects a locked token.
	Locked bool `json:"locked"`
	// Quarantined is set when the token's account hit a terminal state
	// (banned / country_blocked / 401 invalid) and the pool has permanently
	// stopped leasing it (anti-ban contract). QuarantineReason names the
	// state ("banned", "country_blocked", "invalid"). Both are surfaced so
	// the operator sees exactly which fixed token is dead and why.
	Quarantined      bool   `json:"quarantined,omitempty"`
	QuarantineReason string `json:"quarantine_reason,omitempty"`
	// AllowedModels is the slot's MODEL_LOCKS allowlist (issue #325); nil
	// when unlocked. AllowlistSkips counts Acquire-time skips for models
	// outside it.
	AllowedModels  []string `json:"allowed_models,omitempty"`
	AllowlistSkips int64    `json:"allowlist_skips,omitempty"`
	// PremiumQuota is the 5/day premium-pool quota (pacific_day) derived from
	// the live QuotaByModel entry for the premium models. Nil when no premium
	// quota has been reported.
	PremiumQuota *PremiumQuotaSnapshot `json:"premium_quota,omitempty"`
	// BanType / BannedUntil surface the token's active upstream ban
	// (issues #198/#199): BanType is "temporary" when the ban carries a
	// resumes_at deadline (auto-lifts at BannedUntil) and "hard" when it
	// does not (never self-heals; operator must appeal upstream). Both are
	// zero values when no ban window is active.
	BanType     string    `json:"ban_type,omitempty"`
	BannedUntil time.Time `json:"banned_until,omitempty"`
}

// Pool balances requests across the configured tokens.
type Pool struct {
	// cfg is the pool's effective configuration. It is an atomic pointer so
	// the dashboard can swap in a reloaded config at runtime (SetConfig);
	// every reader Load()s once per call instead of caching the pointer.
	cfg atomic.Pointer[config.Config]
	reg *registry.Registry
	// roster owns the fixed-token entry list plus its per-entry ledger and
	// the mismatch escalation map behind a single mutex (issues #262/#263).
	// Lock-free readers use roster.Load(); all mutations and ledger ops go
	// through its methods.
	roster tokenRoster

	// retired maps token entries removed by RemoveLastToken to the time they
	// were parked. The busy check and the toks swap are TOCTOU: an Acquire
	// that loaded the pre-removal snapshot can lease the removed token in
	// between, and its LeaseRelease would no-op on the post-removal bounds
	// check (or mis-target a reused index). Leases carry their entry, so
	// release always lands on the right manager; LeaseRelease drains a
	// parked entry once its last lease releases. maintainTick prunes
	// entries that never saw a slipped lease (already drained at removal).
	retiredMu sync.Mutex
	retired   map[*tokenEntry]time.Time

	rr     atomic.Uint64 // round-robin start index
	logger *slog.Logger

	// requestsServed counts successful upstream chat calls across BOTH
	// pooled and bridge leases (bridge entries are ephemeral and excluded
	// from the per-token counters, so this is the mode-independent total).
	requestsServed atomic.Uint64

	once   sync.Once
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// draining is set at the START of Shutdown: request-path admissions are
	// refused from then on, so no session POST or run START can land after
	// the shutdown drain has released the upstream sessions (post-drain
	// re-admission gate). Never cleared — Shutdown is terminal.
	draining atomic.Bool

	// createGate bounds concurrent session admissions (issue #86): per-model
	// and global in-flight create counters with wait-or-503, wired from
	// SESSION_CREATE_MAX_PARALLEL_GLOBAL/PER_MODEL.
	gate *createGate

	// Idle rotation (IDLE_ROTATION_TIMEOUT): last successful Acquire and
	// whether the maintain loop already FINISHed all runs for the current
	// idle stretch. Guarded by lastActiveMu.
	lastActiveMu sync.Mutex
	lastActive   time.Time
	idleFinished bool
	// sessionsEnded mirrors idleFinished for the opt-in SESSION_IDLE_END
	// sweep: whether upstream sessions were already released for the
	// current idle stretch. Guarded by lastActiveMu.
	sessionsEnded bool

	// Bridge mode (no AUTH_TOKENS): lazily-created per-client-token entries.
	// bridgeOrder keeps the LRU order, oldest first. Guarded by bridgeMu.
	bridgeMu    sync.RWMutex
	bridge      map[string]*bridgeEntry
	bridgeOrder []string

	// bridgeCreateGate bounds concurrent bridge client creation:
	// upstream.New involves network calls; limiting concurrency to 4
	// prevents thundering-herd creation when many new client tokens
	// arrive simultaneously.
	bridgeCreateGate chan struct{}

	// bridgeDailyUsage tracks the total number of successful chats across
	// ALL bridge entries for the BRIDGE_DAILY_LIMIT global cap.
	// Guarded by bridgeMu.
	bridgeDailyUsage int

	// bridgeSurvivors preserves the 24h-window usage of evicted bridge
	// entries so an eviction does not reset an active client's contribution
	// to the global BRIDGE_DAILY_LIMIT between maintain recomputes
	// (review 2026-08-31 P3). Bounded; survivors expire after one usage
	// window. Guarded by bridgeMu. Type and helpers live in
	// bridge_cache.go.
	bridgeSurvivors []bridgeSurvivor

	// unfit is the per-(egress, model) unfit registry (issue #74): models
	// refused upstream with limited_ip on this egress are marked unfit for
	// modelUnfitTTL so new requests are refused fast (409 model_ip_limited)
	// and re-admission does not burn a daily session slot. The server guards
	// NEW requests against it; Acquire deliberately does NOT consult it (the
	// chat recovery loop re-acquires through the plain acquire closure and
	// must reach a different token in mixed pools). Guarded by unfitMu.
	unfitMu sync.Mutex
	unfit   map[unfitKey]unfitEntry

	// lastTokenByModel tracks the token index last successfully acquired for
	// each model (model stickiness / multi-turn session preservation).
	// Guarded by lastTokenMu.
	lastTokenMu      sync.Mutex
	lastTokenByModel map[string]int

	// admissions tracks in-flight session admissions per model across the pool
	// (issue #191: prevents concurrent requests from creating duplicate sessions
	// on different tokens for the same model). Guarded by admissionsMu.
	admissionsMu sync.Mutex
	admissions   map[string]int

	// modelAdmissionGate serializes cold-path Acquire per model: the leader
	// creates a gate on registration; concurrent followers block on it
	// Guarded by modelAdmissionGateMu; entries are deleted when the channel
	// is closed.
	modelAdmissionGateMu sync.Mutex
	modelAdmissionGate   map[string]*admissionGate
	// testGatePark, when non-nil (tests only), runs while modelAdmissionGateMu is held
	// at the moment a follower is about to park on the leader's gate. Lets tests
	// deterministically count parked waiters before the leader releases.
	testGatePark func()
	// store persists session state across restarts (SESSION_PERSIST); nil
	// disables. Injected by the caller (main) via SetSessionStore so there
	// is exactly one store shared by pooled and bridge entries.
	store *session.Store

	// notify fires best-effort webhook alerts (issue #48): pool_exhausted
	// when every token is rate-limited, token_banned when a ban is
	// classified, agent_model_mismatch_escalation when one token takes 3+
	// free_mode_invalid_agent_model refusals in 60s (issue #140). nil
	// disables. Wired by main from WEBHOOK_URL.
	notify   *notify.Sender
	notifyMu sync.Mutex // guards notify reads/writes (data race)

	// storeSessionPersist and storeStateFile record the persistence config
	// the store was created with (captured by SetSessionStore), so SetConfig
	// can detect a reload that changes the persistence semantics — the live
	// store cannot be swapped at runtime, so such a change only takes
	// effect on the next restart.
	storeSessionPersist bool
	storeStateFile      string

	// randMu and randGen support stochastic rotation ("random" TokenRotation mode)
	randMu  sync.Mutex
	randGen *rand.Rand
}

// admissionGate is the per-model leader election gate: the leader creates
// the gate, followers block on gate.ch, and the chosen token is
// communicated via gate.token/hasToken (guarded by modelAdmissionGateMu).
type admissionGate struct {
	ch       chan struct{}
	token    int
	hasToken bool
}
type tokenEntry struct {
	session   *session.Manager
	runs      *runs.RunManager
	client    *upstream.Client
	ledger    *AccountLedger // usage + spend state, guarded by the pool roster mutex
	email     atomic.Pointer[string]
	accountID atomic.Pointer[string]
	// accountFetch guards the background account-info backfill so at most
	// one FetchAccountInfo runs per entry at a time (issue #269).
	accountFetch atomic.Bool
	// config reload that replaces the account at this slot REBUILDS the
	// entry (see Pool.SetConfig): the old entry is retired and drained
	// (runs FINISHed, session ended) and a fresh one is constructed for
	// the new token, so a quarantine never leaks onto a different account
	// and the old (dead) account is never re-admitted.
	token string

	// Session-liveness poll schedule: nextPollAt is when the next
	// compact poll is due (zero = due on the next sessionPollTick pass);
	// pollFailures counts consecutive poll failures for the 20s→300s backoff.
	// Touched only by the maintain goroutine (sessionPollTick), so no lock is
	// needed — AddToken entries are appended to a fresh slice the poll loop
	// has not loaded yet.
	nextPollAt   time.Time
	pollFailures int

	// drained ensures FinishAllRuns + EndSession runs at most once for a
	// retired entry. drainRemovedToken is called from both LeaseRelease
	// (when the last inflight releases) and pruneRetired; the sync.Once
	// prevents the race between these two callers from double-draining.
	drained sync.Once

	// locked is set by LockToken/UnlockLockToken to administratively
	// exclude a token from Acquire without clearing its cooldown state.
	locked atomic.Bool
	// allowlistSkips counts Acquire-time model-allowlist skips for this slot
	// (MODEL_LOCKS, issue #325): requests for models the slot is not locked
	// to. Surfaced per-token in snapshots, cards, and metrics.
	allowlistSkips atomic.Int64

	// quarantine, when non-nil, marks this fixed pooled token permanently
	// ineligible for leasing: its account reached a terminal state (banned,
	// country_blocked, or 401 invalid) that the pool must never revive —
	// no re-admission attempts, no automatic unban. Stored on the entry (not
	// an index-keyed slice) so a concurrent RemoveLastToken index-reuse can
	// never quarantine the wrong account. Set exactly once per terminal
	// refusal (CompareAndSwap) and cleared either by UnlockToken or by the
	// entry rebuild that an AUTH_TOKENS slot change triggers (SetConfig —
	// the whole entry is replaced, so the marker dies with it).
	quarantine atomic.Pointer[quarantineState]
}

func (e *tokenEntry) Email() string {
	if p := e.email.Load(); p != nil {
		return *p
	}
	return ""
}

func (e *tokenEntry) SetEmail(email string) {
	if email != "" {
		e.email.Store(&email)
	}
}

func (e *tokenEntry) AccountID() string {
	if p := e.accountID.Load(); p != nil {
		return *p
	}
	return ""
}

func (e *tokenEntry) SetAccountID(id string) {
	if id != "" {
		e.accountID.Store(&id)
	}
}

// asyncAccountInfoFetch backfills the account email/id for one pooled token
// entry whose email is still unknown. It never blocks the caller, and at most
// one fetch per entry is in flight at a time (issue #269). A failed fetch
// releases the guard so a later backfill pass retries.
func (p *Pool) asyncAccountInfoFetch(e *tokenEntry) {
	if e.email.Load() != nil || !e.accountFetch.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer e.accountFetch.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		email, id, err := e.client.FetchAccountInfo(ctx)
		if err == nil && email != "" {
			e.SetEmail(email)
			e.SetAccountID(id)
		}
	}()
}

// SetTokenAccountInfo stamps the account email and ID onto a pooled token entry.
func (p *Pool) SetTokenAccountInfo(index int, email, accountID string) {
	toks := p.roster.Load()
	if toks != nil && index >= 0 && index < len(*toks) {
		(*toks)[index].SetEmail(email)
		(*toks)[index].SetAccountID(accountID)
	}
}

// quarantineState is the terminal account state that permanently removes one
// fixed pooled token from rotation (anti-ban contract). reason is one of
// "banned", "country_blocked", or "invalid"; err carries the typed upstream
// error for failover-bucket aggregation (nil for a 401, which has no bucket);
// detail is the human-readable error string for logging/dashboards.
type quarantineState struct {
	reason string
	err    error
	detail string
	// liftAt is when a time-limited terminal state (a temporary upstream
	// ban with a future resumes_at) auto-lifts; zero means the state is
	// permanent (hard ban, country block, 401 invalid) and only an
	// operator action (UnlockToken) or an AUTH_TOKENS slot replacement
	// clears it.
	liftAt time.Time
}

// leaseTarget fields the lease dispatch methods (LeaseRelease, LeaseAbandon,
// RecordRunStep, MarkRunFailed, RecordSpend, Chat) need from a lease. It
// collapses the old 3-way Bridge/entry/index skeleton into one accessor
// (issue #265): production leases always carry entry (acquire.go) or Bridge
// (bridge.go), so the historical index-fallback path is dropped — an index
// could be reused by a concurrent RemoveLastToken+AddToken and mis-target a
// different entry. A nil target means the lease is synthetic (no backing
// entry or bridge); the dispatch methods no-op on it.
type leaseTarget struct {
	runs   *runs.RunManager
	client *upstream.Client
	entry  *tokenEntry
	bridge *bridgeEntry
}

// leaseTarget resolves the lease's backing run manager, upstream client and
// entry (pooled) or bridge entry. Returns nil for a synthetic lease.
func (l *Lease) leaseTarget() *leaseTarget {
	if l.entry != nil {
		return &leaseTarget{runs: l.entry.runs, client: l.entry.client, entry: l.entry}
	}
	if l.Bridge != nil {
		return &leaseTarget{runs: l.Bridge.runs, client: l.Bridge.client, bridge: l.Bridge}
	}
	return nil
}

// New builds the pool over the configured tokens. len(clients) and
// len(sessions) must both equal len(cfg.AuthTokens); each pair is bound to
// one token and one RunManager.
func New(cfg *config.Config, clients []*upstream.Client, sessions []*session.Manager, reg *registry.Registry) (*Pool, error) {
	if cfg == nil {
		return nil, errors.New("pool: nil config")
	}
	if reg == nil {
		return nil, errors.New("pool: nil registry")
	}
	if len(clients) != len(cfg.AuthTokens) {
		return nil, fmt.Errorf("pool: %d clients for %d tokens", len(clients), len(cfg.AuthTokens))
	}
	if len(sessions) != len(cfg.AuthTokens) {
		return nil, fmt.Errorf("pool: %d sessions for %d tokens", len(sessions), len(cfg.AuthTokens))
	}

	p := &Pool{reg: reg, logger: slog.Default(), bridge: make(map[string]*bridgeEntry), unfit: make(map[unfitKey]unfitEntry), bridgeCreateGate: make(chan struct{}, 4), lastTokenByModel: make(map[string]int), admissions: make(map[string]int), modelAdmissionGate: make(map[string]*admissionGate)}
	p.cfg.Store(cfg)
	p.gate = newCreateGate(cfg.SessionCreateMaxParallelGlobal, cfg.SessionCreateMaxParallelPerModel)
	toks := make([]*tokenEntry, 0, len(cfg.AuthTokens))
	for i := range cfg.AuthTokens {
		if sessions[i] == nil || clients[i] == nil {
			return nil, fmt.Errorf("pool: nil session/client at index %d", i)
		}
		sess := sessions[i]
		sess.SetReAdmitLead(cfg.SessionReAdmitLead)
		sess.SetAdmissionProbeTTL(cfg.SessionProbeCacheTTL)
		sess.SetModelUnavailableCacheTTL(cfg.ModelUnavailableCacheTTL)
		entry := &tokenEntry{
			session: sess,
			runs:    runs.NewRunManagerOpts(clients[i], sess, runOptions(cfg)),
			client:  clients[i],
			token:   cfg.AuthTokens[i],
			ledger:  newAccountLedger(),
		}
		toks = append(toks, entry)
	}
	p.roster = *newTokenRoster(toks)
	return p, nil
}

// runOptions maps config knobs to the run manager's Options (issues
// #90/#55): the bounded finish queue and draining-list bounds.
func runOptions(cfg *config.Config) runs.Options {
	return runs.Options{
		RotationInterval:    cfg.RotationInterval,
		FinishQueueSize:     cfg.RunFinishQueueSize,
		InlineFinishTimeout: cfg.RunFinishInlineTimeout,
		DrainQueueCap:       cfg.RunsDrainQueueCap,
		DrainTTL:            cfg.RunsDrainTTL,
	}
}

// SetConfig swaps in a reloaded configuration. The pool reads config
// through an atomic pointer, so a config change takes effect on the next
// Acquire/maintain pass without rebuilding the pool, except that an AUTH_TOKENS slot change rebuilds that entry (see below).
func (p *Pool) SetConfig(cfg *config.Config) {
	p.cfg.Store(cfg)

	// Runtime-adjustable knobs: the create gate caps (#86) and the session
	// re-admit lead / probe cache TTL (#99/#60) follow config reloads.
	if p.gate != nil {
		p.gate.setLimits(cfg.SessionCreateMaxParallelGlobal, cfg.SessionCreateMaxParallelPerModel)
	}
	toks := p.roster.Load()
	for _, tok := range *toks {
		tok.session.SetReAdmitLead(cfg.SessionReAdmitLead)
		tok.session.SetAdmissionProbeTTL(cfg.SessionProbeCacheTTL)
		tok.session.SetModelUnavailableCacheTTL(cfg.ModelUnavailableCacheTTL)
	}
	p.bridgeMu.Lock()
	for _, entry := range p.bridge {
		entry.session.SetReAdmitLead(cfg.SessionReAdmitLead)
		entry.session.SetAdmissionProbeTTL(cfg.SessionProbeCacheTTL)
		entry.session.SetModelUnavailableCacheTTL(cfg.ModelUnavailableCacheTTL)
	}
	p.bridgeMu.Unlock()

	// AUTH_TOKENS slot reconciliation. A quarantine is bound to the exact
	// account string an entry was built from; when a reload replaces the
	// account at a slot (operator edited AUTH_TOKENS in the Config editor
	// or .env), the old entry's terminal state no longer describes the
	// account now configured there — and keeping the entry would keep
	// leasing the OLD account while the replacement token sat idle until a
	// restart. The slot is therefore REBUILT end-to-end: the old entry is
	// retired and drained (runs FINISHed, admitted session ended — exactly
	// like RemoveLastToken) and a fresh entry is constructed for the new
	// token through the same path AddToken uses. The dead account is never
	// re-admitted, and the quarantine never leaks onto the replacement (a
	// rebuilt entry carries none). Tokens appended to AUTH_TOKENS get an
	// entry the same way. REMOVAL is not handled here: the membership path
	// (RemoveLastToken/RemoveAllTokens) owns it, so a reload with fewer
	// tokens leaves the surplus entries in place until that path or a
	// restart drops them.
	base := *p.roster.Load()
	rebuilt := make([]*tokenEntry, 0, len(cfg.AuthTokens))
	type slotChange struct {
		idx int
		old *tokenEntry
	}
	var changes []slotChange
	changed := false

	// Map available base entries so swapped/reordered tokens reuse their
	// existing client/session/run-manager triple without tear-down or rebuild.
	available := make(map[string]*tokenEntry, len(base))
	for _, e := range base {
		if e != nil && e.token != "" {
			available[e.token] = e
		}
	}

	for i := range cfg.AuthTokens {
		tokVal := cfg.AuthTokens[i]
		if i < len(base) && base[i].token == tokVal {
			rebuilt = append(rebuilt, base[i])
			delete(available, tokVal)
			continue
		}
		if existing, ok := available[tokVal]; ok {
			rebuilt = append(rebuilt, existing)
			delete(available, tokVal)
			continue
		}
		entry, err := p.buildTokenEntry(i, tokVal)
		if err != nil {
			// Keep serving the previous entry for this slot; log the
			// failure so the operator sees the reload did not fully apply.
			p.logger.Warn("pool: AUTH_TOKENS reload: slot kept on the previous entry (entry build failed)",
				"token", i+1, "err", err)
			if i < len(base) {
				rebuilt = append(rebuilt, base[i])
			}
			continue
		}
		logged := false
		if i < len(base) {
			if prev := base[i].quarantine.Load(); prev != nil {
				p.logger.Info("pool: AUTH_TOKENS slot rebuilt (quarantined account replaced)",
					"token", i+1,
					"from_token", tokenEntryLabel(base[i]),
					"to_token", tokenEntryLabel(entry),
					"state", prev.reason)
				logged = true
			}
		}
		if !logged && i < len(base) {
			p.logger.Info("pool: AUTH_TOKENS slot rebuilt (config reload)",
				"token", i+1,
				"from_token", tokenEntryLabel(base[i]),
				"to_token", tokenEntryLabel(entry))
		}
		rebuilt = append(rebuilt, entry)
		changed = true
		if i < len(base) {
			changes = append(changes, slotChange{idx: i, old: base[i]})
		}
	}
	if changed {
		// Replace the roster wholesale: rebuilt slots carry a fresh entry
		// (built by buildTokenEntry with a new ledger), so a changed slot's
		// usage/spend history resets automatically — no index-aligned slice
		// publish order to maintain.
		p.roster.replaceAll(rebuilt)

		// Retire and drain the replaced entries. A replaced entry with
		// in-flight leases is parked like RemoveLastToken does: the swap
		// happens now, the drain waits for the last lease so the in-flight
		// chat is never killed (the entry's sync.Once guards the race with
		// LeaseRelease's drain).
		p.retiredMu.Lock()
		if p.retired == nil {
			p.retired = make(map[*tokenEntry]time.Time)
		}
		for _, c := range changes {
			p.retired[c.old] = time.Now()
		}
		p.retiredMu.Unlock()
		for _, c := range changes {
			if c.old.runs.InflightCount() == 0 {
				p.drainRemovedToken(c.old)
			}
		}
	}

	// Session persistence is decided at startup: the store is built from the
	// boot config and injected once via SetSessionStore, so a reload cannot
	// move the live store. Warn when the reloaded config changes the
	// persistence semantics — otherwise operators get a silent no-op (the
	// dashboard save / admin reload funnels through here).
	persistChanged := cfg.SessionPersist != p.storeSessionPersist ||
		(cfg.SessionPersist && cfg.SessionStateFile != p.storeStateFile)
	if persistChanged {
		p.logger.Warn("SESSION_PERSIST/SESSION_STATE_FILE changed via reload; takes effect on the next restart",
			"old_session_persist", p.storeSessionPersist,
			"new_session_persist", cfg.SessionPersist,
			"old_state_file", p.storeStateFile,
			"new_state_file", cfg.SessionStateFile)
	}
}

// tokenEntryLabel returns a short non-reversible label for a fixed pooled
// token entry, safe for logs: the sha256 of the raw AUTH_TOKENS string, hex
// truncated to 8 chars (mirrors bridgeTokenLabel; the raw token must never
// reach logs — logring retains them for /admin/logs).
func tokenEntryLabel(e *tokenEntry) string {
	if e == nil || e.client == nil {
		return "token"
	}
	return "token-" + e.client.TokenKey()[:8]
}

// buildTokenEntry constructs the client/session/run-manager triple for one
// AUTH_TOKENS slot at runtime, wired like New (session knobs) and AddToken
// (session store). Also used by SetConfig's slot rebuilds.
func (p *Pool) buildTokenEntry(idx int, token string) (*tokenEntry, error) {
	cfg := p.cfg.Load()
	client, err := upstream.NewWithIndex(token, idx, cfg)
	if err != nil {
		return nil, err
	}
	sess := session.NewManagerWithStore(client, p.store)
	sess.SetReAdmitLead(cfg.SessionReAdmitLead)
	sess.SetAdmissionProbeTTL(cfg.SessionProbeCacheTTL)
	sess.SetModelUnavailableCacheTTL(cfg.ModelUnavailableCacheTTL)
	entry := &tokenEntry{
		session: sess,
		runs:    runs.NewRunManagerOpts(client, sess, runOptions(cfg)),
		client:  client,
		token:   token,
		ledger:  newAccountLedger(),
	}
	go p.asyncAccountInfoFetch(entry)
	return entry, nil
}

// isPooledToken reports whether raw matches one of the fixed AUTH_TOKENS
// entries (exact string compare against the pool's own in-memory copies).
func (p *Pool) isPooledToken(raw string) bool {
	if raw == "" {
		return false
	}
	for _, tok := range *p.roster.Load() {
		if tok.token == raw {
			return true
		}
	}
	return false
}

// AddToken adds a token to the pool at runtime (dashboard action): builds
// the client/session/run-manager triple and appends it, returning the new
// token index. The config must be updated separately (AUTH_TOKENS + reload)
// so the change survives a restart.
func (p *Pool) AddToken(token string) (int, error) {
	toks := p.roster.Load()
	idx := len(*toks)
	entry, err := p.buildTokenEntry(idx, token)
	if err != nil {
		return 0, fmt.Errorf("pool: add token: %w", err)
	}
	// Append through the roster: the entry carries its own ledger, so no
	// index-aligned usage/spend slice needs to be extended — the publish
	// order rule is satisfied by construction.
	idx = p.roster.add(entry)
	return idx, nil
}

// TokenCount returns the current fixed-token count.
func (p *Pool) TokenCount() int {
	return len(*p.roster.Load())
}

// SetSessionStore injects the shared session-state store used by runtime
// token additions (AddToken) and bridge entries. Call before the pool
// starts serving requests; the fixed-token session managers are built by the
// caller and must use the same store instance. A nil store disables
// persistence. The persistence config is captured here so SetConfig can warn
// when a later reload tries to change it (that only takes effect on the next
// restart, when the caller builds a fresh store).
func (p *Pool) SetSessionStore(store *session.Store) {
	p.store = store
	// Issue #40: run persistence rides the same store. The fixed-token run
	// managers were built before the store existed (SetSessionStore runs
	// after New), so inject it here; runtime-added tokens pass it through
	// Options at construction.
	toks := p.roster.Load()
	for _, tok := range *toks {
		tok.runs.SetStore(store)
	}
	if store == nil {
		p.storeSessionPersist = false
		p.storeStateFile = ""
		return
	}
	cfg := p.cfg.Load()
	p.storeSessionPersist = cfg.SessionPersist
	p.storeStateFile = cfg.SessionStateFile
}

// SetNotifier wires the best-effort webhook sender (issue #48, WEBHOOK_URL);
// nil disables alerts. Safe to call at runtime (nil-friendly).
func (p *Pool) SetNotifier(n *notify.Sender) {
	p.notifyMu.Lock()
	defer p.notifyMu.Unlock()
	p.notify = n
}

// Chat sends a chat-completion request through the leased token's upstream
// client, returning the raw SSE body reader on 2xx. The caller must release
// the lease (LeaseRelease) once the request completes or fails, and close
// the returned body.
// multi-token failover: a single token either yields a lease or its error
// is returned as-is. Registry misses pass through.
func (p *Pool) Chat(ctx context.Context, lease *Lease, opts upstream.ChatOptions, body []byte) (io.ReadCloser, error) {
	if lease == nil {
		return nil, errors.New("pool: chat: invalid lease")
	}
	// Leases dispatch through their authoritative owner pinned by Acquire or
	// AcquireBridge (issue #265): entry for fixed-token leases, Bridge for
	// bridge leases. A concurrent RemoveLastToken+AddToken can leave a
	// lease's Token index out of range (chat would fail with "invalid lease
	// token") or reused by a DIFFERENT token (chat would go through the
	// wrong account's client and charge the wrong usage/error path); the
	// entry/bridge is a stable pointer immune to both. The historical
	// index-fallback path is dropped — production leases always carry one.
	t := lease.leaseTarget()
	if t == nil {
		return nil, errors.New("pool: chat: invalid lease")
	}
	rc, err := t.client.ChatCompletions(ctx, opts, body)
	if err == nil {
		if t.bridge != nil {
			// Only chats that actually went upstream count against the
			// daily cap; errors are not recorded.
			p.bridgeRecordChat(t.bridge)
		} else if t.entry != nil {
			p.recordChatEntry(t.entry)
		}
		p.requestsServed.Add(1)
	}
	return rc, err
}

// asRateLimit extracts a RateLimitError from err (nil when absent).
func asRateLimit(err error) *upstream.RateLimitError {
	var rle *upstream.RateLimitError
	if errors.As(err, &rle) {
		return rle
	}
	return nil
}

// asIpCapped extracts an IpCappedError from err (nil when absent).
func asIpCapped(err error) *upstream.IpCappedError {
	var ice *upstream.IpCappedError
	if errors.As(err, &ice) {
		return ice
	}
	return nil
}

// asBan extracts a BanError from err (nil when absent).
func asBan(err error) *upstream.BanError {
	var be *upstream.BanError
	if errors.As(err, &be) {
		return be
	}
	return nil
}

// asCountryBlocked extracts a CountryBlockedError from err (nil when
// absent).
func asCountryBlocked(err error) *upstream.CountryBlockedError {
	var cbe *upstream.CountryBlockedError
	if errors.As(err, &cbe) {
		return cbe
	}
	return nil
}

// asLimitedIp extracts a LimitedIpError from err (nil when absent).
func asLimitedIp(err error) *upstream.LimitedIpError {
	var lie *upstream.LimitedIpError
	if errors.As(err, &lie) {
		return lie
	}
	return nil
}

// bestRateLimit picks the rate-limit error with the shortest retry
// window (the token that unblocks earliest bounds the wait).
func bestRateLimit(entries []*upstream.RateLimitError) *upstream.RateLimitError {
	best := entries[0]
	for _, e := range entries[1:] {
		if e.RetryAfter < best.RetryAfter {
			best = e
		}
	}
	return best
}

// EnsureTokenSession admits/creates an upstream session for a specific model on a specific token (dashboard dev action).
func (p *Pool) EnsureTokenSession(ctx context.Context, token int, model string) (string, error) {
	toks := p.roster.Load()
	if token < 0 || token >= len(*toks) {
		return "", fmt.Errorf("pool: token %d out of range", token)
	}
	tok := (*toks)[token]
	return tok.session.EnsureSessionForModel(ctx, model)
}
