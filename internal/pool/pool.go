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
	"sync"
	"sync/atomic"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/notify"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/runs"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/upstream"
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

// bridgeEntry is one lazily-created client-token slot in bridge mode: the
// upstream client, session manager, and run manager for a single client-
// supplied token, created on first use and reused across that client's
// later requests. lastUsed and usage are guarded by Pool.bridgeMu.
type bridgeEntry struct {
	token    string
	client   *upstream.Client
	session  *session.Manager
	runs     *runs.RunManager
	lastUsed time.Time
	usage    []time.Time // rolling 24h successful-chat timestamps (MAX_MESSAGES_PER_DAY)
	// spend is the per-client-token spend ledger (issue #87); guarded by
	// Pool.bridgeMu like usage.
	spend *spendLedger
	// nextPollAt / pollFailures carry the session-liveness poll schedule
	// (gap #2), touched only by the maintain goroutine (bridgeSessionPollTick).
	nextPollAt   time.Time
	pollFailures int
	// locked is an administrative lock that prevents AcquireBridge from
	// leasing runs to this entry (#187). Set/cleared by LockBridgeEntry/
	// UnlockBridgeEntry; in-flight leases are unaffected.
	locked atomic.Bool

	// admissionGate serializes session creation per entry: the first
	// request creates the session; concurrent requests block on the
	// channel until it completes or fails. sync.Once ensures the session
	// is created exactly once per entry lifecycle.
	admissionGate chan struct{}
	admissionOnce sync.Once
	admissionErr  error // result of leader's session creation

	// lastModel tracks the last model successfully served by this entry
	// for fast-path session reuse (model stickiness).
	lastModel string
}

// TokenSnapshot is one token's healthz view.
type TokenSnapshot struct {
	Token                   int
	CooldownUntil           time.Time
	SessionStatus           string
	SessionInstanceID       string
	SessionQueuePosition    int
	SessionQueueDepth       int
	SessionModel            string
	SessionRemainingSeconds int64
	ActiveRuns              int
	Requests                int
	Messages24h             int    // successful chats in the last 24h (MAX_MESSAGES_PER_DAY usage)
	DailyLimit              int    // configured MAX_MESSAGES_PER_DAY (0 = unlimited)
	UsagePct                int    // percentage of daily limit used (0 when unlimited)
	RiskLevel               string // "low", "moderate", "high", "critical" account safety indicator (#6)
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
	// SessionActiveUsersForIP is the last known distinct-user count on the
	// token's egress IP (upstream activeUsersForIp); zero when the session
	// response did not carry it.
	SessionActiveUsersForIP int
	// QuotaByModel is the live per-model session quota from the last
	// admission (key = model id); empty until the session reports it.
	// Entitlement is a top-level per-token view (empty: the upstream wire
	// nests entitlement inside each rate-limit entry).
	QuotaByModel map[string]session.QuotaSnapshot
	Entitlement  map[string]float64
	// GlmPromo is the raw upstream glmPromo block ({dailySessions, endsAt})
	// from the token's last admission (issue #178); "" when absent. The
	// dashboard synthesizes the z-ai/glm-5.2 promo quota row from it.
	GlmPromo string
	// Standing is the upstream account standing block (issue #96); nil until
	// the session reports it.
	Standing *upstream.SessionStanding
	// TransientRetries / FingerprintRotations are this token's upstream
	// client counters (TRANSIENT_RETRIES): retried transport failures and
	// pinned TLS fingerprint swaps. Surfaced per-token in /metrics.
	TransientRetries     int64
	FingerprintRotations int64
	// RateLimitEvents is this token's upstream rate-limit classification
	// ledger (T7), keyed by upstream body code (rate_limited, ip_capped,
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
	// toks is the fixed-token list. It is an atomic pointer so the dashboard
	// can add/remove tokens at runtime (AddToken/RemoveLastToken/
	// RemoveAllTokens rebuild the slice); every reader Load()s once per call
	// and bounds-checks indices, since the slice can shrink mid-flight.
	toks atomic.Pointer[[]*tokenEntry]

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

	// Usage tracking for MAX_MESSAGES_PER_DAY: one timestamp per successful
	// upstream chat, per token. Guarded by usageMu.
	usageMu      sync.Mutex
	msgsPerToken [][]time.Time

	// createGate bounds concurrent session admissions (issue #86): per-model
	// and global in-flight create counters with wait-or-503, wired from
	// SESSION_CREATE_MAX_PARALLEL_GLOBAL/PER_MODEL.
	gate *createGate

	// Spend ledger (issue #87): per-token token spend, rolling 24h window
	// plus day/week/month buckets with rollover. Guarded by spendMu;
	// spendPerToken stays index-aligned with msgsPerToken under usageMu's
	// publish order (AddToken/RemoveLastToken update both slices together).
	spendMu       sync.Mutex
	spendPerToken []*spendLedger

	// Idle rotation (IDLE_ROTATION_TIMEOUT): last successful Acquire and
	// whether the maintain loop already FINISHed all runs for the current
	// idle stretch. Guarded by lastActiveMu.
	lastActiveMu sync.Mutex
	lastActive   time.Time
	idleFinished bool

	// Bridge mode (no AUTH_TOKENS): lazily-created per-client-token entries.
	// bridgeOrder keeps the LRU order, oldest first. Guarded by bridgeMu.
	bridgeMu    sync.Mutex
	bridge      map[string]*bridgeEntry
	bridgeOrder []string

	// bridgeCreateGate bounds concurrent bridge client creation (B1):
	// upstream.New involves network calls; limiting concurrency to 4
	// prevents thundering-herd creation when many new client tokens
	// arrive simultaneously.
	bridgeCreateGate chan struct{}

	// bridgeDailyUsage tracks the total number of successful chats across
	// ALL bridge entries for the BRIDGE_DAILY_LIMIT global cap (B5).
	// Guarded by bridgeMu.
	bridgeDailyUsage int

	// unfit is the per-(egress, model) unfit registry (issue #74 P2): models
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
	// creates a channel on registration; concurrent followers block on it
	// until the leader either picks a token (and they follow) or fails.
	// Guarded by modelAdmissionGateMu; entries are deleted when the channel
	// is closed.
	modelAdmissionGateMu sync.Mutex
	modelAdmissionGate   map[string]chan struct{}

	// store persists session state across restarts (SESSION_PERSIST); nil
	// disables. Injected by the caller (main) via SetSessionStore so there
	// is exactly one store shared by pooled and bridge entries.
	store *session.Store

	// notify fires best-effort webhook alerts (issue #48): pool_exhausted
	// when every token is rate-limited, token_banned when a ban is
	// classified. nil disables. Wired by main from WEBHOOK_URL.
	notify   *notify.Sender
	notifyMu sync.Mutex // guards notify reads/writes (P2-1 data race)

	// storeSessionPersist and storeStateFile record the persistence config
	// the store was created with (captured by SetSessionStore), so SetConfig
	// can detect a reload that changes the persistence semantics — the live
	// store cannot be swapped at runtime, so such a change only takes
	// effect on the next restart.
	storeSessionPersist bool
	storeStateFile      string
}

type tokenEntry struct {
	session *session.Manager
	runs    *runs.RunManager
	client  *upstream.Client

	// Session-liveness poll schedule (gap #2): nextPollAt is when the next
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

	p := &Pool{reg: reg, logger: slog.Default(), bridge: make(map[string]*bridgeEntry), unfit: make(map[unfitKey]unfitEntry), bridgeCreateGate: make(chan struct{}, 4), lastTokenByModel: make(map[string]int), admissions: make(map[string]int), modelAdmissionGate: make(map[string]chan struct{})}
	p.cfg.Store(cfg)
	p.msgsPerToken = make([][]time.Time, len(cfg.AuthTokens))
	p.spendPerToken = make([]*spendLedger, len(cfg.AuthTokens))
	for i := range p.spendPerToken {
		p.spendPerToken[i] = newSpendLedger()
	}
	p.gate = newCreateGate(cfg.SessionCreateMaxParallelGlobal, cfg.SessionCreateMaxParallelPerModel)
	toks := make([]*tokenEntry, 0, len(cfg.AuthTokens))
	for i := range cfg.AuthTokens {
		sess := sessions[i]
		sess.SetReAdmitLead(cfg.SessionReAdmitLead)
		sess.SetAdmissionProbeTTL(cfg.SessionProbeCacheTTL)
		sess.SetModelUnavailableCacheTTL(cfg.ModelUnavailableCacheTTL)
		sess.SetScarceModels(cfg.ScarceSessionModels)
		toks = append(toks, &tokenEntry{
			session: sess,
			runs:    runs.NewRunManagerOpts(clients[i], sess, runOptions(cfg)),
			client:  clients[i],
		})
	}
	p.toks.Store(&toks)
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
// Acquire/maintain pass without rebuilding the pool.
func (p *Pool) SetConfig(cfg *config.Config) {
	p.cfg.Store(cfg)

	// Runtime-adjustable knobs: the create gate caps (#86) and the session
	// re-admit lead / probe cache TTL (#99/#60) follow config reloads.
	if p.gate != nil {
		p.gate.setLimits(cfg.SessionCreateMaxParallelGlobal, cfg.SessionCreateMaxParallelPerModel)
	}
	toks := p.toks.Load()
	for _, tok := range *toks {
		tok.session.SetReAdmitLead(cfg.SessionReAdmitLead)
		tok.session.SetAdmissionProbeTTL(cfg.SessionProbeCacheTTL)
		tok.session.SetModelUnavailableCacheTTL(cfg.ModelUnavailableCacheTTL)
		tok.session.SetScarceModels(cfg.ScarceSessionModels)
	}
	p.bridgeMu.Lock()
	for _, entry := range p.bridge {
		entry.session.SetReAdmitLead(cfg.SessionReAdmitLead)
		entry.session.SetAdmissionProbeTTL(cfg.SessionProbeCacheTTL)
		entry.session.SetModelUnavailableCacheTTL(cfg.ModelUnavailableCacheTTL)
		entry.session.SetScarceModels(cfg.ScarceSessionModels)
	}
	p.bridgeMu.Unlock()

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

// AddToken adds a token to the pool at runtime (dashboard action): builds
// the client/session/run-manager triple and appends it, returning the new
// token index. The config must be updated separately (AUTH_TOKENS + reload)
// so the change survives a restart.
func (p *Pool) AddToken(token string) (int, error) {
	toks := p.toks.Load()
	idx := len(*toks)
	client, err := upstream.NewWithIndex(token, idx, p.cfg.Load())
	if err != nil {
		return 0, fmt.Errorf("pool: add token: %w", err)
	}
	sess := session.NewManagerWithStore(client, p.store)
	cfg := p.cfg.Load()
	sess.SetReAdmitLead(cfg.SessionReAdmitLead)
	sess.SetAdmissionProbeTTL(cfg.SessionProbeCacheTTL)
	sess.SetScarceModels(cfg.ScarceSessionModels)
	entry := &tokenEntry{
		session: sess,
		runs:    runs.NewRunManagerOpts(client, sess, runOptions(cfg)),
		client:  client,
	}
	next := make([]*tokenEntry, 0, len(*toks)+1)
	next = append(next, *toks...)
	next = append(next, entry)
	// Publish the usage slice BEFORE the token snapshot: a concurrent
	// reader that observes the new snapshot (via p.toks) must always find
	// a matching entry in p.msgsPerToken, so recordChat/usageCount for the
	// new index can never index past the usage slice. The two fields are
	// otherwise independent (toks is an atomic pointer, msgsPerToken is
	// usageMu-guarded); only this publish order matters. The spend ledger
	// slice rides along so Snapshot() stays index-aligned too.
	p.usageMu.Lock()
	p.msgsPerToken = append(p.msgsPerToken, nil)
	p.usageMu.Unlock()
	p.spendMu.Lock()
	p.spendPerToken = append(p.spendPerToken, newSpendLedger())
	p.spendMu.Unlock()
	p.toks.Store(&next)
	return idx, nil
}

// TokenCount returns the current fixed-token count.
func (p *Pool) TokenCount() int {
	return len(*p.toks.Load())
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
	toks := p.toks.Load()
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
	if lease.Bridge != nil {
		rc, err := lease.Bridge.client.ChatCompletions(ctx, opts, body)
		if err == nil {
			// Only chats that actually went upstream count against the
			// daily cap; errors are not recorded.
			p.bridgeRecordChat(lease.Bridge)
			p.requestsServed.Add(1)
		}
		return rc, err
	}
	// Fixed-token leases dispatch through their backing entry — the
	// authoritative owner pinned by Acquire. A concurrent RemoveLastToken+
	// AddToken can leave the lease's Token index out of range (chat would
	// fail with "invalid lease token") or reused by a DIFFERENT token (chat
	// would go through the wrong account's client and charge the wrong
	// usage/error path); the entry is a stable pointer immune to both.
	if lease.entry != nil {
		rc, err := lease.entry.client.ChatCompletions(ctx, opts, body)
		if err == nil {
			p.recordChatEntry(lease.entry)
			p.requestsServed.Add(1)
		}
		return rc, err
	}
	// Synthetic leases without an entry keep the historical index path.
	toks := p.toks.Load()
	if lease.Token < 0 || lease.Token >= len(*toks) {
		return nil, errors.New("pool: chat: invalid lease token")
	}
	rc, err := (*toks)[lease.Token].client.ChatCompletions(ctx, opts, body)
	if err == nil {
		p.recordChat(lease.Token)
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
