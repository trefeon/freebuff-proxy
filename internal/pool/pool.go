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
	cryptoRand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/notify"
	"freebuff-proxy/internal/phasetiming"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/runs"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/upstream"
)

// maintainInterval is how often the background job rotates aged runs and
// advances queued sessions (PRD §3: 60s maintain ticker). Session-liveness
// polls run on their own jittered schedule (see sessionPoll* below), not on
// this coarse grid.
const maintainInterval = time.Minute

// Session-liveness poll cadence (gap #2; reference/freebuff sdk
// polling-backoff.ts): while active the CLI polls the compact session every
// 30s ±20% (24–36s), capped to remaining+1s near expiry so the poll lands
// just after expires_at; on failure it backs off 20s → 300s (×2 per
// consecutive failure), never scheduling a retry before the server's
// Retry-After floor.
const (
	// sessionPollCheckInterval is the maintain loop's fine-grained wake-up
	// grid for due session polls; rotation/queued-advance stay on
	// maintainInterval.
	sessionPollCheckInterval = 2 * time.Second
	// sessionPollBaseInterval is the CLI's active poll cadence (30s).
	sessionPollBaseInterval = 30 * time.Second
	// sessionPollBackoffBase is the first failure backoff (20s); each
	// consecutive failure doubles it up to sessionPollBackoffMax (300s).
	sessionPollBackoffBase = 20 * time.Second
	sessionPollBackoffMax  = 300 * time.Second
)

// usageWindow is the rolling window for the per-token daily message cap
// (MAX_MESSAGES_PER_DAY): a token may send at most N successful chat
// requests per 24h of usage history.
const usageWindow = 24 * time.Hour

// shutdownTimeout bounds each token's Shutdown during Pool.Shutdown when the
// caller's context carries no earlier deadline.
const shutdownTimeout = 10 * time.Second

// maxBridgeEntries caps the in-memory bridge cache: one entry (upstream
// client + session manager + run manager) per distinct client token. LRU
// eviction makes room when the cap is exceeded.
const maxBridgeEntries = 32

// bridgeIdleEvict is how long a bridge entry may sit unused before the
// maintain loop FINISHes its runs and drops it from the cache.
const bridgeIdleEvict = 2 * time.Hour

// retiredDrainGrace is how long a retired token may sit without a lease
// before maintainTick drops it from the retired map. RemoveLastToken drains
// the entry at removal, so a parked entry's runs are already finished; the
// grace exists to cover an Acquire that loaded the pre-removal snapshot and
// is still mid-admission when the park happens (see RemoveLastToken).
const retiredDrainGrace = 2 * time.Minute

// Lease is one acquired right to send a chat request through a specific
// token. The caller must call Pool.LeaseRelease when the request completes
// or fails (it decrements the run's inflight counter).
type Lease struct {
	Token             int    // index into config.AuthTokens (-1 for bridge leases)
	Model             string // the model this lease's session/run is bound to (authoritative for opts.Model; may differ from the requested model after #100 fallback)
	AgentID           string
	Run               *runs.Run
	SessionInstanceID string       // "" when the session is disabled
	TierAccess        string       // upstream accessTier, "" when unknown
	TierCountry       string       // upstream countryCode, "" when unknown
	Bridge            *bridgeEntry // nil for pooled (fixed-token) leases
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
	// tier is the entry's last known upstream accessTier ("full"/"limited",
	// "" when unknown), cached from the admission response and refreshed by
	// successful session-liveness polls so a compact-poll-learned tier
	// survives into later leases even when the admission omits it. Guarded
	// by Pool.bridgeMu like usage.
	tier string
	// nextPollAt / pollFailures carry the session-liveness poll schedule
	// (gap #2), touched only by the maintain goroutine (bridgeSessionPollTick).
	nextPollAt   time.Time
	pollFailures int
}

// TokenSnapshot is one token's healthz view.
type TokenSnapshot struct {
	Token                int
	CooldownUntil        time.Time
	SessionStatus        string
	SessionInstanceID    string
	SessionQueuePosition int
	SessionQueueDepth    int
	ActiveRuns           int
	Requests             int
	Messages24h          int    // successful chats in the last 24h (MAX_MESSAGES_PER_DAY usage)
	DailyLimit           int    // configured MAX_MESSAGES_PER_DAY (0 = unlimited)
	UsagePct             int    // percentage of daily limit used (0 when unlimited)
	RiskLevel            string // "low", "moderate", "high", "critical" account safety indicator (#6)
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
	// TierAccess / CountryCode / CountryBlockReason are the token's last
	// known upstream session tier and region-block state. CountryBlockReason
	// is non-empty when the account (or its egress region) is blocked;
	// surfaced by /v1/models availability annotation and healthz.
	TierAccess         string
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

	// unfit is the per-(egress, model) unfit registry (issue #74 P2): models
	// refused upstream with limited_ip on this egress are marked unfit for
	// modelUnfitTTL so new requests are refused fast (409 model_ip_limited)
	// and re-admission does not burn a daily session slot. The server guards
	// NEW requests against it; Acquire deliberately does NOT consult it (the
	// chat recovery loop re-acquires through the plain acquire closure and
	// must reach a different token in mixed-tier pools). Guarded by unfitMu.
	unfitMu sync.Mutex
	unfit   map[unfitKey]unfitEntry

	// store persists session state across restarts (SESSION_PERSIST); nil
	// disables. Injected by the caller (main) via SetSessionStore so there
	// is exactly one store shared by pooled and bridge entries.
	store *session.Store

	// notify fires best-effort webhook alerts (issue #48): pool_exhausted
	// when every token is rate-limited, token_banned when a ban is
	// classified. nil disables. Wired by main from WEBHOOK_URL.
	notify *notify.Sender

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

	p := &Pool{reg: reg, logger: slog.Default(), bridge: make(map[string]*bridgeEntry), unfit: make(map[unfitKey]unfitEntry)}
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
	}
	p.bridgeMu.Lock()
	for _, entry := range p.bridge {
		entry.session.SetReAdmitLead(cfg.SessionReAdmitLead)
		entry.session.SetAdmissionProbeTTL(cfg.SessionProbeCacheTTL)
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

// RemoveLastToken removes the highest-index fixed token (dashboard action).
// Only the last index can be removed safely: removing a middle token would
// shift indices under in-flight leases. Refuses while the token has active
// runs. The removed token's runs are FINISHed and its admitted session
// ended (they used to leak upstream, contrast RemoveAllTokens); a lease
// that slips through the busy-check/swap race is released through the
// retired map and drained once it releases.
func (p *Pool) RemoveLastToken() error {
	toks := p.toks.Load()
	if len(*toks) == 0 {
		return errors.New("pool: no tokens to remove")
	}
	last := (*toks)[len(*toks)-1]
	if last.runs.InflightCount() > 0 {
		return errors.New("pool: token has in-flight requests; wait for them to finish")
	}
	next := append([]*tokenEntry{}, (*toks)[:len(*toks)-1]...)
	p.toks.Store(&next)
	p.usageMu.Lock()
	p.msgsPerToken = p.msgsPerToken[:len(p.msgsPerToken)-1]
	p.usageMu.Unlock()
	p.spendMu.Lock()
	p.spendPerToken = p.spendPerToken[:len(p.spendPerToken)-1]
	p.spendMu.Unlock()

	// The busy check above and the swap are TOCTOU: an Acquire that loaded
	// the pre-removal snapshot can lease the removed token in between. Park
	// the entry so that lease is still released (LeaseRelease bounds-checks
	// the new snapshot and would otherwise no-op, leaking the run's
	// inflight), then drain now when no lease slipped — finishing the
	// removed token's run and ending its admitted session. A slipped lease
	// keeps the entry parked; LeaseRelease drains it once the last lease
	// releases.
	slip := last.runs.InflightCount() > 0
	p.retiredMu.Lock()
	if p.retired == nil {
		p.retired = make(map[*tokenEntry]time.Time)
	}
	p.retired[last] = time.Now()
	p.retiredMu.Unlock()
	if !slip {
		p.drainRemovedToken(last)
	}
	return nil
}

// drainRemovedToken finishes the removed token's runs and ends its admitted
// session (mirrors RemoveAllTokens' run finish plus the session end that
// removal previously skipped), bounded by the per-token shutdown timeout so
// a hung upstream cannot block the dashboard action.
func (p *Pool) drainRemovedToken(entry *tokenEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	entry.runs.FinishAllRuns(ctx)
	_ = entry.session.EndSession(ctx)
}

// RemoveAllTokens finishes every fixed token's runs and empties the pool
// (bridge-mode switch). In-flight leases on removed tokens no-op on release
// (bounds-checked index access). Config must be updated separately.
func (p *Pool) RemoveAllTokens(ctx context.Context) {
	toks := p.toks.Load()
	for _, t := range *toks {
		t.runs.FinishAllRuns(ctx)
	}
	empty := make([]*tokenEntry, 0)
	p.toks.Store(&empty)
	p.usageMu.Lock()
	p.msgsPerToken = nil
	p.usageMu.Unlock()
	p.spendMu.Lock()
	p.spendPerToken = nil
	p.spendMu.Unlock()
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
	p.notify = n
}

// Acquire resolves the model's agent, picks a start token round-robin, and
// fails over linearly until a token yields both a run and a session. Returns
// a lease on success. Registry misses (unknown model) are returned as-is.
func (p *Pool) Acquire(ctx context.Context, model string) (*Lease, error) {
	toks := p.toks.Load()
	cfg := p.cfg.Load()
	if len(*toks) == 0 {
		return nil, errors.New("pool: no auth tokens configured")
	}
	agentID, err := p.reg.AgentForModel(model)
	if err != nil {
		return nil, err
	}

	start := int(p.rr.Add(1)-1) % len(*toks)
	// Hot-session-first selection: tokens that already hold a live session
	// are tried before any fresh account, so a request reuses the live slot
	// instead of admitting a new session (never create where one already
	// exists — the lowest fingerprint/quota-burn path). When at least one
	// token is hot, the pass iterates only over hot tokens; only when every
	// hot token fails does it fall back to the remaining eligible tokens
	// from the round-robin start (cold path), exactly like the historical
	// linear failover. When no token is hot the order is unchanged.
	order, quotaLimited := p.acquireOrder(toks, start, model)
	var errs []string
	var waiting []*session.WaitingRoomError
	var rateLimited []*upstream.RateLimitError
	var ipCapped []*upstream.IpCappedError
	var banned []*upstream.BanError
	var countryBlocked []*upstream.CountryBlockedError
	var modelLimited []*upstream.LimitedIpError
	var dailyLimited []*upstream.RateLimitError

	for _, idx := range order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Defensive bounds check: acquireOrder builds its order against the
		// SAME snapshot loaded above, but a removal racing this call must
		// never index past the slice it computed the order from. Skip
		// indices that are no longer present instead of panicking.
		if idx < 0 || idx >= len(*toks) {
			continue
		}
		tok := (*toks)[idx]
		name := fmt.Sprintf("token-%d", idx+1)

		if until := tok.runs.CooldownUntil(); time.Now().Before(until) {
			errs = append(errs, fmt.Sprintf("%s: cooling down until %s", name, until.Format(time.RFC3339)))
			p.logger.Debug("pool: token skipped (cooldown)", "token", idx+1, "until", until.Format(time.RFC3339))
			if be := tok.runs.BanError(); be != nil {
				banned = append(banned, be)
			}
			if cbe := tok.runs.CountryBlockedError(); cbe != nil {
				countryBlocked = append(countryBlocked, cbe)
			}
			if rle := tok.runs.RateLimitError(); rle != nil {
				rateLimited = append(rateLimited, rle)
			}
			if ice := tok.runs.IpCappedError(); ice != nil {
				ipCapped = append(ipCapped, ice)
			}
			continue
		}

		// Daily rolling cap: a token that already sent its
		// MAX_MESSAGES_PER_DAY successful chats in the last 24h is skipped
		// like a cooldown; when every token is capped, the pool surfaces a
		// 429 with the earliest window reset.
		if cfg.MaxMessagesPerDay > 0 && p.usageCount(idx) >= cfg.MaxMessagesPerDay {
			dailyLimited = append(dailyLimited, p.dailyLimitError(idx))
			errs = append(errs, fmt.Sprintf("%s: daily message limit (%d) reached", name, cfg.MaxMessagesPerDay))
			p.logger.Debug("pool: token skipped (daily message limit)", "token", idx+1, "limit", cfg.MaxMessagesPerDay)
			continue
		}

		// Issue #85: session-quota-capped token for the requested model.
		// The hot path excludes these in acquireOrder (their rate-limit
		// reasons ride back in quotaLimited); the no-hot round-robin path
		// reaches them here and records the reason the same way.
		if _, _, capped := quotaRemaining(tok, model); capped {
			rateLimited = append(rateLimited, quotaLimitError(tok, model))
			errs = append(errs, fmt.Sprintf("%s: session quota exhausted for model", name))
			p.logger.Debug("pool: token skipped (session quota exhausted)", "token", idx+1, "model", model)
			continue
		}

		// Session-create admission gate (issue #86): concurrent session
		// creates are bounded globally and per model; when the gate is at
		// capacity the acquire waits (the caller's deadline surfaces as
		// 503). The permit is held only for the admission call, never
		// across the upstream chat.
		permit, err := p.gate.acquire(ctx, model)
		if err != nil {
			// Context expired while waiting for a create slot: the caller's
			// deadline surfaces as 503 (wait-or-503). The pass is aborted —
			// the ctx is done, so trying further tokens would only repeat
			// the same wait.
			return nil, err
		}
		sessionStart := time.Now()
		// Issue #94(b): WAITING_ROOM_CHAIN gate — when the upstream last
		// refused this token with 428 waiting_room_required, fire the
		// reference pre-session ad-chain + streak flow (best-effort, bounded
		// by the client's own chain timeout) before the next session create
		// so the admission does not bounce off the same 428 again.
		if cfg.WaitingRoomChain && tok.client.ConsumeWaitingRoomChain() {
			p.logger.Debug("pool: firing waiting-room pre-session chain", "token", idx+1)
			tok.client.FireWaitingRoomChain(ctx)
		}
		instanceID, err := tok.session.EnsureSessionForModel(ctx, model)
		permit.Release()
		phasetiming.FromContext(ctx).Since(phasetiming.SessionRefreshMS, sessionStart)
		if err != nil {
			if errors.Is(err, upstream.ErrAuthRejected) {
				tok.runs.Cooldown(runs.DefaultCooldown)
				p.logger.Debug("pool: token cooling down", "token", idx+1, "duration", runs.DefaultCooldown.String())
			}
			var wr *session.WaitingRoomError
			if errors.As(err, &wr) {
				waiting = append(waiting, wr)
			}
			if rle := asRateLimit(err); rle != nil {
				tok.runs.CooldownRateLimit(rle)
				rateLimited = append(rateLimited, rle)
				// Issue #122: the fresh-admission spend ceiling is the
				// upstream's primary spend gate, so an admission-path
				// spend_limited counts on the ledger too (same counter as
				// the chat-path refusal in CooldownTokenRateLimit).
				if rle.Status == "spend_limited" {
					p.spendMu.Lock()
					p.recordSpendLimited(idx)
					p.spendMu.Unlock()
				}
			}
			if ice := asIpCapped(err); ice != nil {
				tok.runs.CooldownIpCapped(ice)
				ipCapped = append(ipCapped, ice)
			}
			if be := asBan(err); be != nil {
				tok.runs.CooldownBan(be)
				p.notifyBan(idx+1, model)
				banned = append(banned, be)
			}
			if cbe := asCountryBlocked(err); cbe != nil {
				tok.runs.CooldownCountryBlocked(cbe)
				countryBlocked = append(countryBlocked, cbe)
			}
			if lie := asLimitedIp(err); lie != nil {
				// Issue #74 P2: the egress IP cannot serve this model
				// (limited_ip). The session row is fine — it stays bound to
				// its admitted model — so nothing is invalidated or cooled
				// per-token: the (egress, model) pair is marked unfit so
				// new requests are refused fast instead of re-admitting and
				// burning a daily session slot on every token. The lie is
				// pool-owned here (fresh from the admission error), so
				// stamping Model makes the surfaced refusal self-describing;
				// the registry stores its own copy.
				lie.Model = model
				p.MarkModelUnfit(model, lie)
				modelLimited = append(modelLimited, lie)
				errs = append(errs, fmt.Sprintf("%s: %v", name, err))
				continue
			}
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			continue
		}

		// Re-validate the token is still current: a concurrent
		// RemoveLastToken may have swapped the snapshot while the session
		// admission above was in flight. Leasing a removed token would
		// strand its run's inflight — LeaseRelease always releases through
		// the lease's own entry, but the run would belong to a drained,
		// retiring manager — so skip instead (the removal path drains the
		// retired entry once it observes the slip).
		if cur := p.toks.Load(); idx < 0 || idx >= len(*cur) || (*cur)[idx] != tok {
			continue
		}
		ss := tok.session.Snapshot()
		effectiveModel := model
		effectiveAgentID := agentID
		if ss.Model != "" && ss.Model != model {
			effectiveModel = ss.Model
			if p.reg != nil {
				if resolvedAgent, aerr := p.reg.AgentForModel(effectiveModel); aerr == nil {
					effectiveAgentID = resolvedAgent
				}
			}
		}

		// Issue #90a: pre-create the run at session admission (best-effort)
		// so the first chat on a freshly-admitted session does not pay the
		// START latency. When a run already exists this is a cheap no-op;
		// when the START fails here the Acquire below retries and surfaces
		// the real error through the normal failover path.
		_ = tok.runs.Precreate(ctx, effectiveAgentID)
		runStart := time.Now()
		run, err := tok.runs.Acquire(ctx, effectiveAgentID)
		phasetiming.FromContext(ctx).Since(phasetiming.RunAcquireMS, runStart)
		if err != nil {
			if errors.Is(err, upstream.ErrAuthRejected) {
				tok.runs.Cooldown(runs.DefaultCooldown)
				p.logger.Debug("pool: token cooling down", "token", idx+1, "duration", runs.DefaultCooldown.String())
			}
			if rle := asRateLimit(err); rle != nil {
				tok.runs.CooldownRateLimit(rle)
				rateLimited = append(rateLimited, rle)
				// Issue #122: count run-start spend_limited refusals on the
				// ledger (same counter as the chat-path refusal).
				if rle.Status == "spend_limited" {
					p.spendMu.Lock()
					p.recordSpendLimited(idx)
					p.spendMu.Unlock()
				}
			}
			if ice := asIpCapped(err); ice != nil {
				tok.runs.CooldownIpCapped(ice)
				ipCapped = append(ipCapped, ice)
			}
			if be := asBan(err); be != nil {
				tok.runs.CooldownBan(be)
				p.notifyBan(idx+1, model)
				banned = append(banned, be)
			}
			if cbe := asCountryBlocked(err); cbe != nil {
				tok.runs.CooldownCountryBlocked(cbe)
				countryBlocked = append(countryBlocked, cbe)
			}
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		p.logger.Debug("pool: lease acquired", "token", idx+1, "model", effectiveModel, "agent", effectiveAgentID, "instance_id", instanceID,
			"tier", ss.TierAccess, "country", ss.TierCountry)
		// Track the activity and end any idle-maintenance pause: the next
		// maintain tick resumes rotation/refresh work.
		p.lastActiveMu.Lock()
		p.lastActive = time.Now()
		p.idleFinished = false
		p.lastActiveMu.Unlock()
		return &Lease{Token: idx, Model: effectiveModel, AgentID: effectiveAgentID, Run: run, SessionInstanceID: instanceID,
			TierAccess: ss.TierAccess, TierCountry: ss.TierCountry, entry: tok, AcquiredAt: time.Now()}, nil
	}

	// Failover precedence (PRD §6 error matrix): when buckets are mixed the
	// highest-precedence non-empty bucket wins — ban > country-blocked >
	// model-IP-limited > rate-limit > ip-capped > waiting-room > daily cap.
	// Each bucket contributes its best error (first ban, longest rate
	// window, first ip_capped, lowest queue position, earliest daily
	// reset). Only when every bucket is empty — all tokens failed with
	// errors outside the matrix — is the generic error surfaced.
	// Issue #85: quota-capped tokens were excluded in acquireOrder (never
	// attempted); their rate-limit reasons land here so a fully-capped pool
	// surfaces a real 429 with the earliest window reset instead of a
	// generic combined error.
	rateLimited = append(rateLimited, quotaLimited...)
	if len(banned) > 0 {
		return nil, banned[0]
	}
	if len(countryBlocked) > 0 {
		return nil, countryBlocked[0]
	}
	if len(modelLimited) > 0 {
		// Egress-model gate dominates per-token windows (issue #74 P2):
		// every token shares the one egress, so no token can serve the
		// model within the unfit window — retrying any token is pointless.
		// Surface the refusal (409 model_ip_limited) and let the server's
		// new-request guard fast-refuse until the window lapses.
		return nil, modelLimited[0]
	}
	if len(rateLimited) > 0 {
		// Pool exhausted (issue #48): every token failed and the highest-
		// precedence bucket is rate-limit — no ban/country is present, so
		// this is the "all tokens are at their quota/window limit" state the
		// operator wants to be alerted about. Fire-and-forget webhook
		// (throttled per event type); the 429 still surfaces as usual.
		if p.notify != nil {
			p.notify.Send(notify.Event{Event: "pool_exhausted", TokenIndex: 0, Model: model,
				Message: "all tokens are rate-limited; the pool cannot serve the request"})
		}
		return nil, bestRateLimit(rateLimited)
	}
	if len(ipCapped) > 0 {
		return nil, ipCapped[0]
	}
	if len(waiting) > 0 {
		wr := bestWaitingRoom(waiting)
		p.logger.Debug("pool: waiting room surfaced", "position", wr.Position, "queue_depth", wr.QueueDepth, "retry_after", wr.RetryAfter.String())
		return nil, wr
	}
	if len(dailyLimited) > 0 {
		return nil, bestDailyLimit(dailyLimited)
	}
	return nil, fmt.Errorf("unable to acquire run from any token: %s", strings.Join(errs, "; "))
}

// acquireOrder computes the token iteration order for one Acquire pass
// (hot-session-first selection, see Acquire). toks is the caller's snapshot
// (loaded once in Acquire) — the order is built against the same snapshot
// the failover loop indexes, so an AddToken racing the call can never make
// the loop index past its own snapshot. start is the round-robin start
// index; model is the requested upstream model.
//
// Issue #85: within the hot set, tokens whose last admission reported a
// known positive remaining session quota for the requested model rank above
// unknown-quota tokens, ordered by smallest remaining first (drain the
// account closest to its limit; preserve fuller quotas —
// reference/freebuff-reverse .../scheduler.go:472-496 tier ordering). Tokens
// whose quota is exhausted for the model (RecentCount >= Limit with a future
// ResetAt) are excluded from this pass entirely; their rate-limit reasons
// are returned so the caller surfaces a real 429 when every token is capped.
func (p *Pool) acquireOrder(toks *[]*tokenEntry, start int, model string) ([]int, []*upstream.RateLimitError) {
	cfg := p.cfg.Load()
	// eligible mirrors the per-token checks the failover loop applies:
	// not cooling down, under the daily message cap, and not quota-capped
	// for the requested model (issue #85). It never records the exclusion
	// reasons — the caller does that in one place.
	eligible := func(idx int) bool {
		tok := (*toks)[idx]
		// Quota-capped tokens are excluded from BOTH the hot set and the
		// cold fallback: their rate-limit reasons ride back in quotaLimited,
		// so the pool surfaces a real 429 when every token is capped.
		if _, _, capped := quotaRemaining(tok, model); capped {
			return false
		}
		return true
	}

	// Cooldown and daily-message-capped tokens stay ELIGIBLE for the cold
	// fallback (they are simply not hot): the failover loop visits them and
	// records their remembered ban/country-block/rate-limit/ip-capped/daily
	// reasons, matching the historical linear-failover behavior. Excluding
	// them here would drop those reasons from the error matrix when a hot
	// token fails (review finding).
	eligibleForHot := func(idx int) bool {
		tok := (*toks)[idx]
		if time.Now().Before(tok.runs.CooldownUntil()) {
			return false
		}
		if cfg.MaxMessagesPerDay > 0 && p.usageCount(idx) >= cfg.MaxMessagesPerDay {
			return false
		}
		return eligible(idx)
	}

	var hot []int
	for offset := 0; offset < len(*toks); offset++ {
		idx := (start + offset) % len(*toks)
		if !eligibleForHot(idx) || !tokenHasLiveSession((*toks)[idx]) {
			continue
		}
		hot = append(hot, idx)
	}
	if len(hot) == 0 {
		// No hot tokens: plain round-robin over every token, exactly like
		// the historical behavior. Capped/cooldown tokens stay in the order
		// — the failover loop re-checks and records their reasons.
		order := make([]int, len(*toks))
		for i := range order {
			order[i] = (start + i) % len(*toks)
		}
		return order, nil
	}

	// Quota-aware secondary sort (issue #85), stable over the round-robin
	// base order: session-model match first (existing tiebreak), then known
	// positive remaining quota before unknown, then smallest remaining
	// first.
	sort.SliceStable(hot, func(i, j int) bool {
		a, b := hot[i], hot[j]
		aMatch := (*toks)[a].session.Snapshot().Model == model
		bMatch := (*toks)[b].session.Snapshot().Model == model
		if aMatch != bMatch {
			return aMatch
		}
		aKnown, aRem, _ := quotaRemaining((*toks)[a], model)
		bKnown, bRem, _ := quotaRemaining((*toks)[b], model)
		if aKnown != bKnown {
			return aKnown
		}
		if aKnown {
			return aRem < bRem
		}
		return false
	})

	// Cold fallback: the remaining eligible tokens from the round-robin
	// start, excluding the hot tokens already attempted this pass (each
	// token is attempted at most once, as in the historical failover).
	attempted := make(map[int]struct{}, len(hot))
	for _, idx := range hot {
		attempted[idx] = struct{}{}
	}
	order := hot
	for offset := 0; offset < len(*toks); offset++ {
		idx := (start + offset) % len(*toks)
		if _, ok := attempted[idx]; ok || !eligible(idx) {
			continue
		}
		order = append(order, idx)
	}

	// The capped tokens excluded above are never visited by the failover
	// loop, so their rate-limit reasons must ride back with the order: when
	// every token is capped the pool surfaces a real 429 with the earliest
	// window reset instead of a generic combined error.
	inOrder := make(map[int]struct{}, len(order))
	for _, idx := range order {
		inOrder[idx] = struct{}{}
	}
	var quotaLimited []*upstream.RateLimitError
	for idx := range *toks {
		if _, ok := inOrder[idx]; ok {
			continue
		}
		if _, _, capped := quotaRemaining((*toks)[idx], model); capped {
			quotaLimited = append(quotaLimited, quotaLimitError((*toks)[idx], model))
		}
	}
	return order, quotaLimited
}

// quotaRemaining reports the token's session-quota state for model from the
// last admission (issue #85): known reports whether the quota is known with
// a positive remaining allowance; remaining is the positive delta; capped
// reports RecentCount >= Limit with a future ResetAt (the token must be
// skipped this pass — it cannot serve the model right now). Quotas with a
// past/absent ResetAt are treated as fresh (the window rolled) and never
// capped.
func quotaRemaining(tok *tokenEntry, model string) (known bool, remaining float64, capped bool) {
	q, ok := tok.session.Snapshot().QuotaByModel[model]
	if !ok || q.Limit <= 0 {
		return false, 0, false
	}
	resetFuture := !q.ResetAt.IsZero() && q.ResetAt.After(time.Now())
	if resetFuture && q.RecentCount >= q.Limit {
		return false, 0, true
	}
	if q.RecentCount < q.Limit {
		return true, q.Limit - q.RecentCount, false
	}
	// RecentCount >= Limit but the window already rolled: unknown until the
	// next admission reports a fresh count.
	return false, 0, false
}

// quotaLimitError builds the 429 surfaced when token is excluded for the
// model's exhausted session quota (issue #85): RetryAfter is the time until
// the window reset, mirroring the upstream RateLimitError contract.
func quotaLimitError(tok *tokenEntry, model string) *upstream.RateLimitError {
	q := tok.session.Snapshot().QuotaByModel[model]
	retryAfter := time.Duration(0)
	if !q.ResetAt.IsZero() && q.ResetAt.After(time.Now()) {
		retryAfter = time.Until(q.ResetAt)
	}
	return &upstream.RateLimitError{
		Status:      "rate_limited",
		RetryAfter:  retryAfter,
		Limit:       q.Limit,
		RecentCount: q.RecentCount,
		ResetAt:     q.ResetAt,
		Body:        "session quota exhausted for model",
	}
}

// tokenHasLiveSession reports whether token's cached session is active and
// unexpired — i.e. a request can reuse it without admitting a new session.
// Snapshot reads local state only (no upstream calls), safe to call per
// token per acquire.
func tokenHasLiveSession(tok *tokenEntry) bool {
	snap := tok.session.Snapshot()
	return snap.Status == "active" && !snap.ExpiresAt.IsZero() && snap.ExpiresAt.After(time.Now())
}

// AcquireBridge acquires a lease for one client-supplied token in bridge
// mode (no AUTH_TOKENS configured). The entry — upstream client, session
// manager, and run manager — is created lazily on first use and cached for
// reuse across that client's later requests (least quota burn). There is no
// multi-token failover: a single token either yields a lease or its error
// is returned as-is. Registry misses pass through.
func (p *Pool) AcquireBridge(ctx context.Context, clientToken, model string) (*Lease, error) {
	clientToken = strings.TrimSpace(clientToken)
	cfg := p.cfg.Load()
	if clientToken == "" {
		return nil, errors.New("bridge: empty client token")
	}
	agentID, err := p.reg.AgentForModel(model)
	if err != nil {
		return nil, err
	}

	entry, err := p.bridgeEntryFor(clientToken)
	if err != nil {
		return nil, err
	}

	// Cooldown: skip the entry during its window; surface the remembered
	// ban/country-block/rate-limit error so the client keeps getting 403/429
	// instead of a generic failure (mirrors the fixed-token cooldown-skip
	// branch). The remembered errors are mutually exclusive in the run
	// manager; checked in pool precedence order.
	if until := entry.runs.CooldownUntil(); time.Now().Before(until) {
		if be := entry.runs.BanError(); be != nil {
			return nil, be
		}
		if cbe := entry.runs.CountryBlockedError(); cbe != nil {
			return nil, cbe
		}
		if rle := entry.runs.RateLimitError(); rle != nil {
			return nil, rle
		}
		if ice := entry.runs.IpCappedError(); ice != nil {
			return nil, ice
		}
		return nil, fmt.Errorf("bridge: token cooling down until %s", until.Format(time.RFC3339))
	}

	// Daily rolling cap, per client token (mirrors the fixed-token path).
	if cfg.MaxMessagesPerDay > 0 && p.bridgeUsageCount(entry) >= cfg.MaxMessagesPerDay {
		p.logger.Debug("pool: bridge entry daily message limit", "limit", cfg.MaxMessagesPerDay)
		return nil, p.bridgeDailyLimitError(entry)
	}

	// Session-create admission gate (issue #86), mirroring the fixed-token
	// path: concurrent session creates are bounded globally and per model.
	permit, err := p.gate.acquire(ctx, model)
	if err != nil {
		return nil, err
	}
	sessionStart := time.Now()
	instanceID, err := entry.session.EnsureSessionForModel(ctx, model)
	permit.Release()
	phasetiming.FromContext(ctx).Since(phasetiming.SessionRefreshMS, sessionStart)
	if err != nil {
		if errors.Is(err, upstream.ErrAuthRejected) {
			entry.runs.Cooldown(runs.DefaultCooldown)
			p.logger.Debug("pool: bridge entry cooling down", "duration", runs.DefaultCooldown.String())
		}
		if rle := asRateLimit(err); rle != nil {
			entry.runs.CooldownRateLimit(rle)
			// Issue #122: count admission-path spend_limited refusals on
			// the bridge entry's ledger (same counter as the chat-path
			// refusal in CooldownBridgeRateLimit).
			if rle.Status == "spend_limited" {
				p.bridgeMu.Lock()
				p.bridgeRecordSpendLimited(entry)
				p.bridgeMu.Unlock()
			}
		}
		if ice := asIpCapped(err); ice != nil {
			entry.runs.CooldownIpCapped(ice)
		}
		if be := asBan(err); be != nil {
			entry.runs.CooldownBan(be)
		}
		if cbe := asCountryBlocked(err); cbe != nil {
			entry.runs.CooldownCountryBlocked(cbe)
		}
		return nil, err
	}
	ss := entry.session.Snapshot()
	effectiveModel := model
	effectiveAgentID := agentID
	if ss.Model != "" && ss.Model != model {
		effectiveModel = ss.Model
		if p.reg != nil {
			if resolvedAgent, aerr := p.reg.AgentForModel(effectiveModel); aerr == nil {
				effectiveAgentID = resolvedAgent
			}
		}
	}
	// Cache the admission's accessTier on the entry (PREFER_MAX_MODELS
	// limited-tier gating stores the per-token tier here). The lease
	// carries the fresh snapshot tier; a later lease falls back to this
	// cache when the admission omits it.
	tier := ss.TierAccess
	if tier == "" {
		p.bridgeMu.Lock()
		tier = entry.tier
		p.bridgeMu.Unlock()
	}
	if ss.TierAccess != "" {
		p.bridgeMu.Lock()
		entry.tier = ss.TierAccess
		p.bridgeMu.Unlock()
	}

	// Issue #90a: pre-create the run at session admission (best-effort).
	_ = entry.runs.Precreate(ctx, effectiveAgentID)
	runStart := time.Now()
	run, err := entry.runs.Acquire(ctx, effectiveAgentID)
	phasetiming.FromContext(ctx).Since(phasetiming.RunAcquireMS, runStart)
	if err != nil {
		if errors.Is(err, upstream.ErrAuthRejected) {
			entry.runs.Cooldown(runs.DefaultCooldown)
			p.logger.Debug("pool: bridge entry cooling down", "duration", runs.DefaultCooldown.String())
		}
		if rle := asRateLimit(err); rle != nil {
			entry.runs.CooldownRateLimit(rle)
			// Issue #122: count run-start spend_limited refusals on the
			// bridge entry's ledger (same counter as the chat-path refusal).
			if rle.Status == "spend_limited" {
				p.bridgeMu.Lock()
				p.bridgeRecordSpendLimited(entry)
				p.bridgeMu.Unlock()
			}
		}
		if ice := asIpCapped(err); ice != nil {
			entry.runs.CooldownIpCapped(ice)
		}
		if be := asBan(err); be != nil {
			entry.runs.CooldownBan(be)
		}
		if cbe := asCountryBlocked(err); cbe != nil {
			entry.runs.CooldownCountryBlocked(cbe)
		}
		return nil, err
	}

	p.logger.Debug("pool: bridge lease acquired", "model", effectiveModel, "agent", effectiveAgentID, "instance_id", instanceID,
		"tier", tier, "country", ss.TierCountry)
	// Track the activity and end any idle-maintenance pause, mirroring
	// Acquire: without this, IDLE_ROTATION_TIMEOUT was dead config in
	// bridge mode — lastActive stayed zero forever, so the pool never
	// idle-paused and bridge entries were maintained, polled, and
	// queued-advanced every pass indefinitely.
	p.lastActiveMu.Lock()
	p.lastActive = time.Now()
	p.idleFinished = false
	p.lastActiveMu.Unlock()
	return &Lease{Token: -1, Model: effectiveModel, AgentID: effectiveAgentID, Run: run, SessionInstanceID: instanceID,
		TierAccess: tier, TierCountry: ss.TierCountry, Bridge: entry, AcquiredAt: time.Now()}, nil
}

// LeaseRelease decrements the leased run's inflight counter. Call when the
// request completes or fails. Safe on nil leases.
func (p *Pool) LeaseRelease(lease *Lease) {
	if lease == nil || lease.Run == nil {
		return
	}
	if lease.Bridge != nil {
		lease.Bridge.runs.Release(lease.Run)
		return
	}
	if lease.entry == nil {
		return // synthetic lease without a backing entry
	}
	lease.entry.runs.Release(lease.Run)
	// A lease on a removed token (RemoveLastToken swapped the snapshot out
	// from under a concurrent Acquire) releases through its own entry — the
	// bounds-checked index path would no-op and leak the run's inflight, or
	// mis-target a reused index. RemoveLastToken parked the entry undrained
	// when it observed the slip; drain it once its last lease has released.
	p.retiredMu.Lock()
	_, parked := p.retired[lease.entry]
	p.retiredMu.Unlock()
	if parked && lease.entry.runs.InflightCount() == 0 {
		p.drainRemovedToken(lease.entry)
	}
}

// LeaseAbandon releases a lease whose downstream client context was
// cancelled mid-chat (issue #53, CLI DELETE-on-exit parity): when this was
// the LAST in-flight request on the run, the run is dropped from the active
// set and FINISHed through the bounded queue so upstream does not keep an
// abandoned agent run alive until rotation. Concurrent requests on the same
// run keep it alive. The server calls this instead of LeaseRelease when it
// observes a client disconnect.
func (p *Pool) LeaseAbandon(lease *Lease) {
	if lease == nil || lease.Run == nil {
		return
	}
	if lease.Bridge != nil {
		lease.Bridge.runs.ReleaseAbandoned(lease.Run)
		return
	}
	if lease.entry != nil {
		lease.entry.runs.ReleaseAbandoned(lease.Run)
		return
	}
	toks := p.toks.Load()
	if lease.Token < 0 || lease.Token >= len(*toks) {
		return
	}
	(*toks)[lease.Token].runs.ReleaseAbandoned(lease.Run)
}

// RecordRunStep records a completed chat step on the lease's run (issue
// #114): steps are accumulated in memory and sent WITH FINISH — recording
// is local-only and never an upstream call (the CLI has no /steps
// endpoint). The server fires it after a successful chat with the response
// message id ("" when the stream never carried one).
func (p *Pool) RecordRunStep(lease *Lease, messageID string) {
	if lease == nil || lease.Run == nil {
		return
	}
	if lease.Bridge != nil {
		lease.Bridge.runs.RecordStep(lease.Run, messageID)
		return
	}
	if lease.entry != nil {
		lease.entry.runs.RecordStep(lease.Run, messageID)
		return
	}
	toks := p.toks.Load()
	if lease.Token < 0 || lease.Token >= len(*toks) {
		return
	}
	(*toks)[lease.Token].runs.RecordStep(lease.Run, messageID)
}

// MarkRunFailed marks the lease's run as failed for its eventual FINISH
// (issue #114): the server calls it when a chat dies on a terminal upstream
// error so the run does not FINISH as completed (a gateway with zero failed
// runs looks synthetic). The run stays active; only its terminal status is
// recorded. Nil-safe (an acquire failure leaves no lease).
func (p *Pool) MarkRunFailed(lease *Lease) {
	if lease == nil || lease.Run == nil {
		return
	}
	if lease.Bridge != nil {
		lease.Bridge.runs.MarkFailed(lease.Run)
		return
	}
	if lease.entry != nil {
		lease.entry.runs.MarkFailed(lease.Run)
		return
	}
	toks := p.toks.Load()
	if lease.Token < 0 || lease.Token >= len(*toks) {
		return
	}
	(*toks)[lease.Token].runs.MarkFailed(lease.Run)
}

// RecordSpend adds tokens to the lease's backing token spend ledger (issue
// #87): the server reports the usage block of a completed chat. Non-positive
// deltas are ignored. Production caller: chatCore feeds the relay's observed
// usage total once per successful chat completion (#122). The daily $15/$5/
// $0.50 ceilings are server-enforced and cohort-dependent, so this
// token-count ledger is a heuristic proxy, not exact USD accounting — see
// spend.go's package comment.
func (p *Pool) RecordSpend(lease *Lease, tokens int64) {
	if lease == nil || tokens <= 0 {
		return
	}
	if lease.Bridge != nil {
		p.bridgeRecordSpend(lease.Bridge, tokens)
		return
	}
	if lease.entry != nil {
		p.recordSpendEntry(lease.entry, tokens)
		return
	}
	toks := p.toks.Load()
	if lease.Token < 0 || lease.Token >= len(*toks) {
		return
	}
	p.recordSpend(lease.Token, tokens)
}

// InvalidateSession drops the cached free session of token so the next
// Acquire re-creates it (session-invalid recovery). The invalidation is
// guarded to the given instance id (issue #132): after a pre-emptive
// re-admit replaced the cache, a chat that rode the old superseded instance
// failing must not invalidate the fresh one. Out-of-range tokens are
// ignored.
func (p *Pool) InvalidateSession(token int, instanceID string) {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return
	}
	(*toks)[token].session.InvalidateInstance(instanceID)
}

// ClearQueuedCaches drops every token's cached QUEUED session (issue #100):
// the queue-time model fallback calls this before re-acquiring with the
// fallback model, so the fallback acquire creates a fresh session instead of
// re-surfacing the same waiting room. Returns how many queued caches were
// cleared. Other states (active/disabled) are untouched.
func (p *Pool) ClearQueuedCaches() int {
	toks := p.toks.Load()
	cleared := 0
	for _, tok := range *toks {
		if tok.session.ClearQueued() {
			cleared++
		}
	}
	return cleared
}

// InvalidateRun drops the current run of token for agentID so the next
// Acquire starts a fresh one (run-invalid recovery). Out-of-range tokens are
// ignored.
func (p *Pool) InvalidateRun(token int, agentID string) {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return
	}
	(*toks)[token].runs.Invalidate(agentID)
}

// UnlockToken clears any cooldown/rate-limit/ban lock on token so Acquire
// can use it again (dashboard unlock action).
func (p *Pool) UnlockToken(token int) error {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return fmt.Errorf("pool: token %d out of range", token)
	}
	(*toks)[token].runs.ClearCooldowns()
	return nil
}

// FinishTokenRuns finishes all active runs of token (dashboard action).
func (p *Pool) FinishTokenRuns(ctx context.Context, token int) error {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return fmt.Errorf("pool: token %d out of range", token)
	}
	(*toks)[token].runs.FinishAllRuns(ctx)
	return nil
}

// ProbeNewToken validates a NOT-yet-added token against upstream with a
// zero-cost GET session probe (no session claim, no model needed). It builds
// the probe client from the pool's own config, so the base URL matches what
// AddToken would use (tests inject a mock URL here). Returns the session
// state on success, ErrNoActiveSession when the token is valid but idle,
// or the classified auth/network error (ErrBanned / ErrCountryBlocked /
// ErrAuthRejected / ErrRateLimited) otherwise.
func (p *Pool) ProbeNewToken(ctx context.Context, token string) (*upstream.SessionState, error) {
	if token == "" {
		return nil, errors.New("pool: empty token")
	}
	cfg := *p.cfg.Load()
	// Match the base URL of an existing pooled client when one exists: the
	// pool's fixed-token clients were built by the caller with the effective
	// upstream URL (tests inject a mock URL), while p.cfg.UpstreamBaseURL
	// may still hold the production default. A probe built from the wrong
	// URL would validate against a different host than the one the token
	// will actually use — silently false results.
	if toks := p.toks.Load(); len(*toks) > 0 {
		if base := (*toks)[0].client.BaseURL(); base != "" {
			cfg.UpstreamBaseURL = base
		}
	}
	client, err := upstream.New(token, &cfg)
	if err != nil {
		return nil, fmt.Errorf("pool: probe token: %w", err)
	}
	return client.ProbeAccount(ctx)
}

// ProbeToken validates token against upstream with a zero-cost GET session
// probe (dashboard test action): no session is created or claimed. Returns
// the live session state (including RateLimitsByModel quota) on success, or
// ErrNoActiveSession when the token has no active session (still a valid
// token), or the classified auth/network error otherwise.
func (p *Pool) ProbeToken(ctx context.Context, token int) (*upstream.SessionState, error) {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return nil, fmt.Errorf("pool: token %d out of range", token)
	}
	return (*toks)[token].client.ProbeAccount(ctx)
}

// CooldownToken puts token in a cooldown window of duration d (auth-reject
// recovery, e.g. runs.DefaultCooldown). Out-of-range tokens are ignored.
func (p *Pool) CooldownToken(token int, d time.Duration) {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return
	}
	(*toks)[token].runs.Cooldown(d)
}

// CooldownTokenRateLimit applies a rate-limit cooldown to token
// (remembered so Acquire surfaces 429 + Retry-After during the window).
// Out-of-range tokens are ignored. When the refusal is spend_limited
// (issue #122), the event is also counted on the token's spend ledger —
// the $ ceiling is server-enforced, so the ledger only records the event.
func (p *Pool) CooldownTokenRateLimit(token int, rle *upstream.RateLimitError) {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) || rle == nil {
		return
	}
	(*toks)[token].runs.CooldownRateLimit(rle)
	if rle.Status == "spend_limited" {
		p.spendMu.Lock()
		p.recordSpendLimited(token)
		p.spendMu.Unlock()
	}
}

// CooldownTokenIpCapped applies an ip_capped cooldown to token via
// runs.CooldownIpCapped: each hit backs off the error's RetryAfter + ±20%
// jitter, with a per-token daily re-admission cap (#118 — the 3rd hit in a
// rolling window locks until the Pacific-midnight reset and surfaces
// 429 ip_capped; upstream itself is admission-only, not a quota reset).
// Out-of-range tokens are ignored.
func (p *Pool) CooldownTokenIpCapped(token int, ice *upstream.IpCappedError) {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) || ice == nil {
		return
	}
	(*toks)[token].runs.CooldownIpCapped(ice)
}

// CooldownTokenBan applies a ban cooldown to token (remembered so
// Acquire surfaces 403 banned + resumes-at during the window) and fires the
// token_banned webhook alert (issue #48, throttled per event type).
func (p *Pool) CooldownTokenBan(token int, be *upstream.BanError) {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) || be == nil {
		return
	}
	(*toks)[token].runs.CooldownBan(be)
	p.notifyBan(token+1, "")
}

// CooldownTokenCountryBlocked applies a country-block cooldown to token
// (remembered so Acquire surfaces the region-block error during the ~15m
// window instead of re-hitting upstream).
func (p *Pool) CooldownTokenCountryBlocked(token int, cbe *upstream.CountryBlockedError) {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) || cbe == nil {
		return
	}
	(*toks)[token].runs.CooldownCountryBlocked(cbe)
}

// InvalidateBridgeSession drops the cached free session of the bridge
// entry so the next AcquireBridge re-creates it (session-invalid recovery).
// Guarded to the lease's instance id (issue #132) — see InvalidateSession.
func (p *Pool) InvalidateBridgeSession(lease *Lease) {
	if lease == nil || lease.Bridge == nil {
		return
	}
	lease.Bridge.session.InvalidateInstance(lease.SessionInstanceID)
}

// InvalidateBridgeRun drops the current run of the bridge entry for agentID
// so the next AcquireBridge starts a fresh one (run-invalid recovery).
func (p *Pool) InvalidateBridgeRun(lease *Lease, agentID string) {
	if lease == nil || lease.Bridge == nil {
		return
	}
	lease.Bridge.runs.Invalidate(agentID)
}

// CooldownBridge puts the bridge entry's token in a cooldown window of
// duration d (auth-reject recovery, e.g. runs.DefaultCooldown).
func (p *Pool) CooldownBridge(lease *Lease, d time.Duration) {
	if lease == nil || lease.Bridge == nil {
		return
	}
	lease.Bridge.runs.Cooldown(d)
}

// CooldownBridgeRateLimit applies a rate-limit cooldown to the bridge entry
// (remembered so AcquireBridge surfaces 429 + Retry-After). When the refusal
// is spend_limited (issue #122), the event is also counted on the entry's
// spend ledger — the $ ceiling is server-enforced, so the ledger only
// records the event.
func (p *Pool) CooldownBridgeRateLimit(lease *Lease, rle *upstream.RateLimitError) {
	if lease == nil || lease.Bridge == nil || rle == nil {
		return
	}
	lease.Bridge.runs.CooldownRateLimit(rle)
	if rle.Status == "spend_limited" {
		p.bridgeMu.Lock()
		p.bridgeRecordSpendLimited(lease.Bridge)
		p.bridgeMu.Unlock()
	}
}

// CooldownBridgeIpCapped applies an ip_capped cooldown to the bridge entry
// via runs.CooldownIpCapped (full RetryAfter + jitter, per-token daily cap
// until Pacific midnight — #118).
func (p *Pool) CooldownBridgeIpCapped(lease *Lease, ice *upstream.IpCappedError) {
	if lease == nil || lease.Bridge == nil || ice == nil {
		return
	}
	lease.Bridge.runs.CooldownIpCapped(ice)
}

// CooldownBridgeBan applies a ban cooldown to the bridge entry (remembered
// so AcquireBridge surfaces 403 banned + resumes-at during the window) and
// fires the token_banned webhook alert (issue #48, throttled).
func (p *Pool) CooldownBridgeBan(lease *Lease, be *upstream.BanError) {
	if lease == nil || lease.Bridge == nil || be == nil {
		return
	}
	lease.Bridge.runs.CooldownBan(be)
	p.notifyBan(0, "")
}

// notifyBan fires the token_banned webhook (issue #48). tokenIndex is the
// 1-based pooled token index (0 = bridge). model is the requested model
// when the caller knows it ("" otherwise). Throttled by the sender.
func (p *Pool) notifyBan(tokenIndex int, model string) {
	if p.notify == nil {
		return
	}
	p.notify.Send(notify.Event{Event: "token_banned", TokenIndex: tokenIndex, Model: model,
		Message: "a FreeBuff token was classified banned upstream (403)"})
}

// CooldownBridgeCountryBlocked applies a country-block cooldown to the
// bridge entry (remembered so AcquireBridge surfaces the region-block error
// during the ~15m window instead of re-hitting upstream).
func (p *Pool) CooldownBridgeCountryBlocked(lease *Lease, cbe *upstream.CountryBlockedError) {
	if lease == nil || lease.Bridge == nil || cbe == nil {
		return
	}
	lease.Bridge.runs.CooldownCountryBlocked(cbe)
}

// BridgeCount returns the number of cached bridge entries (healthz).
func (p *Pool) BridgeCount() int {
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	return len(p.bridge)
}

// Chat sends a chat-completion request through the leased token's upstream
// client, returning the raw SSE body reader on 2xx. The caller must release
// the lease (LeaseRelease) once the request completes or fails, and close
// the returned body.
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

// Start launches the background jobs: a best-effort prewarm of every
// registry agent across every token (so the first request does not pay the
// START latency) and the 60s maintain loop (rotate aged runs + advance
// queued sessions). Both stop when ctx is canceled; Pool.Shutdown cancels.
func (p *Pool) Start(ctx context.Context) {
	p.once.Do(func() {
		agentIDs := p.reg.AgentIDs()
		runCtx, cancel := context.WithCancel(ctx)
		p.cancel = cancel
		p.wg.Add(2)
		go p.prewarm(runCtx, agentIDs)
		go p.maintainLoop(runCtx)
	})
}

// Shutdown stops the background jobs and drains every token: FINISH all
// runs, end the sessions, bounded by a 10s force deadline per token. Cached
// bridge entries (bridge mode) are drained best-effort the same way after
// the fixed tokens: FINISH all runs and end each entry's session so no
// upstream activity is left behind.
func (p *Pool) Shutdown(ctx context.Context) {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()

	var errs []string
	toks := p.toks.Load()
	for i, tok := range *toks {
		tokCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
		tok.runs.Shutdown(tokCtx)
		cancel()
		// With run persistence the runs are intentionally kept alive for
		// restart-resume — not a drain failure (review P3).
		if !tok.runs.KeptForPersistence() {
			if snap := tok.runs.Snapshot(); snap.ActiveRuns > 0 {
				errs = append(errs, fmt.Sprintf("token-%d: %d runs left after shutdown", i+1, snap.ActiveRuns))
			}
		}
	}

	// Drain the cached bridge entries best-effort. The maintain loop is
	// already stopped (wg.Wait above), so the entry list is stable.
	p.bridgeMu.Lock()
	entries := make([]*bridgeEntry, 0, len(p.bridge))
	for _, entry := range p.bridge {
		entries = append(entries, entry)
	}
	p.bridgeMu.Unlock()
	for _, entry := range entries {
		entryCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
		entry.runs.FinishAllRuns(entryCtx)
		if snap := entry.runs.Snapshot(); snap.ActiveRuns > 0 {
			errs = append(errs, fmt.Sprintf("bridge %s: %d runs left after shutdown", bridgeTokenLabel(entry), snap.ActiveRuns))
		}
		if err := entry.session.Shutdown(entryCtx); err != nil {
			errs = append(errs, fmt.Sprintf("bridge %s: shutdown session: %v", bridgeTokenLabel(entry), err))
		}
		cancel()
	}

	if len(errs) > 0 {
		slog.Warn("pool: shutdown incomplete", "errors", strings.Join(errs, "; "))
	}
}

// Snapshot returns the per-token healthz view.
func (p *Pool) Snapshot() []TokenSnapshot {
	toks := p.toks.Load()
	out := make([]TokenSnapshot, 0, len(*toks))
	dailyLimit := p.cfg.Load().MaxMessagesPerDay
	spendLimit := p.cfg.Load().MaxSpendPerDay
	for i, tok := range *toks {
		rs := tok.runs.Snapshot()
		ss := tok.session.Snapshot()
		msgs := p.usageCount(i)

		// Region/tier view: the session snapshot carries the last admitted
		// tier/country; an active country-block cooldown overrides it with
		// the remembered block (the session never admitted after a block,
		// so its snapshot would be empty for the blocked country).
		countryCode, countryReason := ss.CountryCode, ss.CountryBlockReason
		if cbe := tok.runs.CountryBlockedError(); cbe != nil {
			if cbe.CountryCode != "" {
				countryCode = cbe.CountryCode
			}
			if cbe.CountryBlockReason != "" {
				countryReason = cbe.CountryBlockReason
			}
		}

		usagePct := 0
		if dailyLimit > 0 {
			usagePct = (msgs * 100) / dailyLimit
			if usagePct > 100 {
				usagePct = 100
			}
		}

		riskLevel := "low"
		switch {
		// Ban is checked first: CooldownBan fills the shared cooldown
		// deadline, so the cooldown case below would otherwise shadow a
		// banned token as "high". The ban risk is gated on the ban window
		// still being active (BannedUntil) so an expired ban does not stay
		// sticky "critical" forever.
		case rs.BanError != nil && time.Now().Before(rs.BannedUntil):
			riskLevel = "critical"
		case !rs.CooldownUntil.IsZero() && time.Now().Before(rs.CooldownUntil):
			riskLevel = "high"
		case dailyLimit > 0 && usagePct >= 90:
			riskLevel = "critical"
		case dailyLimit > 0 && usagePct >= 70:
			riskLevel = "high"
		case msgs > 120:
			riskLevel = "high"
		case (dailyLimit > 0 && usagePct >= 30) || msgs >= 50:
			riskLevel = "moderate"
		}

		spend := p.spendSnapshot(i)

		// Advisory spend ceiling (issue #122): the Pacific-day bucket vs
		// MAX_SPEND_PER_DAY, capped at 100% like UsagePct. Informational only —
		// the upstream $ ceilings are server-enforced.
		spendPct := 0
		if spendLimit > 0 {
			spendPct = int((spend.Day * 100) / spendLimit)
			if spendPct > 100 {
				spendPct = 100
			}
		}

		out = append(out, TokenSnapshot{
			Token:                   i,
			CooldownUntil:           rs.CooldownUntil,
			ActiveRuns:              rs.ActiveRuns,
			Requests:                rs.Requests,
			Messages24h:             msgs,
			DailyLimit:              dailyLimit,
			UsagePct:                usagePct,
			RiskLevel:               riskLevel,
			SessionStatus:           ss.Status,
			SessionInstanceID:       ss.InstanceID,
			SessionQueuePosition:    ss.QueuePosition,
			SessionQueueDepth:       ss.QueueDepth,
			TierAccess:              ss.TierAccess,
			CountryCode:             countryCode,
			CountryBlockReason:      countryReason,
			SessionActiveUsersForIP: ss.ActiveUsersForIP,
			QuotaByModel:            ss.QuotaByModel,
			Entitlement:             ss.Entitlement,
			Standing:                ss.Standing,
			TransientRetries:        tok.client.TransientRetries(),
			FingerprintRotations:    tok.client.FingerprintRotations(),
			RateLimitEvents:         tok.client.RateLimitEvents(),
			Spend24h:                spend.Rolling24h,
			SpendDay:                spend.Day,
			SpendWeek:               spend.Week,
			SpendMonth:              spend.Month,
			SpendDayStart:           spend.DayStart,
			SpendWeekStart:          spend.WeekStart,
			SpendMonthStart:         spend.MonthStart,
			SpendLimit:              spendLimit,
			SpendPct:                spendPct,
			SpendLimited:            spend.SpendLimited,
		})
	}
	return out
}

// PoolSnapshot is the pool-wide metrics view: aggregate transient-retry
// counters summed across every fixed token's client, plus the per-token rows
// (same shape as Snapshot). Bridge-mode entries are not counted in the
// per-token rows (they are per-client-token ephemeral slots), but live
// bridge clients' retry/rotation counters are summed in, and RequestsServed
// is mode-independent (every successful upstream chat).
type PoolSnapshot struct {
	TransientRetries     int64
	FingerprintRotations int64
	RequestsServed       uint64
	Tokens               []TokenSnapshot
}

// PoolSnapshot returns the pool-wide snapshot with aggregate counters.
func (p *Pool) PoolSnapshot() PoolSnapshot {
	ps := PoolSnapshot{Tokens: p.Snapshot(), RequestsServed: p.requestsServed.Load()}
	toks := p.toks.Load()
	for _, tok := range *toks {
		ps.TransientRetries += tok.client.TransientRetries()
		ps.FingerprintRotations += tok.client.FingerprintRotations()
	}
	// Live bridge entries: their counters survive while the entry is cached
	// (LRU eviction drops old ones — the view is "recent bridge activity").
	p.bridgeMu.Lock()
	for _, be := range p.bridge {
		ps.TransientRetries += be.client.TransientRetries()
		ps.FingerprintRotations += be.client.FingerprintRotations()
	}
	p.bridgeMu.Unlock()
	return ps
}

// --- internals ---

// recordChat appends one successful upstream chat for token and prunes the
// token's usage history outside the 24h window.
func (p *Pool) recordChat(token int) {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return
	}
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	// Authoritative bound under the lock: p.msgsPerToken is the index space
	// AddToken/RemoveLastToken/RemoveAllTokens keep consistent, so a
	// removal that raced the snapshot check above (or a lease issued from a
	// snapshot that already went stale) is caught here instead of indexing
	// past the usage slice.
	if token < 0 || token >= len(p.msgsPerToken) {
		return
	}
	cutoff := time.Now().Add(-usageWindow)
	history := p.msgsPerToken[token]
	first := 0
	for first < len(history) && history[first].Before(cutoff) {
		first++
	}
	p.msgsPerToken[token] = append(history[first:], time.Now())
}

// recordChatEntry appends one successful upstream chat for the lease's
// backing entry and prunes its usage history outside the 24h window. The
// entry is located by pointer in the CURRENT token list so the usage lands
// on the right token: after a concurrent RemoveLastToken+AddToken, the
// lease's Token index may point at a different token (or be out of range),
// and charging by index would mis-record. An entry that is no longer in the
// pool (removed while the request was in flight) skips the recording.
func (p *Pool) recordChatEntry(entry *tokenEntry) {
	if entry == nil {
		return
	}
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	for idx, tok := range *p.toks.Load() {
		if tok != entry {
			continue
		}
		// Authoritative bound under the lock: the msgsPerToken slice is
		// rebuilt under usageMu by AddToken/RemoveLastToken/RemoveAllTokens,
		// and a removal racing this snapshot can leave the entry present in
		// toks but absent from the usage slice — never index past it.
		if idx < 0 || idx >= len(p.msgsPerToken) {
			return
		}
		cutoff := time.Now().Add(-usageWindow)
		history := p.msgsPerToken[idx]
		first := 0
		for first < len(history) && history[first].Before(cutoff) {
			first++
		}
		p.msgsPerToken[idx] = append(history[first:], time.Now())
		return
	}
	// Entry removed from the pool: skip recording rather than charge a
	// reused index.
}

// usageWindow, pruning expired timestamps.
func (p *Pool) usageCount(token int) int {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return 0
	}
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	// Authoritative bound under the lock (see recordChat): the usage slice
	// may have been truncated/nil'd by a removal racing the snapshot check.
	if token < 0 || token >= len(p.msgsPerToken) {
		return 0
	}
	cutoff := time.Now().Add(-usageWindow)
	history := p.msgsPerToken[token]
	first := 0
	for first < len(history) && history[first].Before(cutoff) {
		first++
	}
	p.msgsPerToken[token] = history[first:]
	return len(p.msgsPerToken[token])
}

// dailyLimitError builds the 429 surfaced when token is capped by
// MAX_MESSAGES_PER_DAY: RetryAfter is the time until the token's oldest
// recorded chat ages out of the 24h window (the earliest moment a slot
// frees).
func (p *Pool) dailyLimitError(token int) *upstream.RateLimitError {
	return &upstream.RateLimitError{
		RetryAfter:  p.usageResetIn(token),
		Limit:       float64(p.cfg.Load().MaxMessagesPerDay),
		RecentCount: float64(p.usageCount(token)),
		Body:        "daily message limit reached",
	}
}

// usageResetIn is how long until token's oldest usage timestamp ages out of
// the window (0 when the token has no recorded usage or the reset is due).
func (p *Pool) usageResetIn(token int) time.Duration {
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	// Bounds check under the lock (see recordChat): the usage slice may
	// have been truncated/nil'd by a removal racing this call — usageResetIn
	// previously had no guard at all and indexed past the end.
	if token < 0 || token >= len(p.msgsPerToken) {
		return 0
	}
	history := p.msgsPerToken[token]
	if len(history) == 0 {
		return 0
	}
	reset := time.Until(history[0].Add(usageWindow))
	if reset < 0 {
		return 0
	}
	return reset
}

// --- bridge mode internals ---

// bridgeEntryFor returns the cached bridge entry for clientToken, creating
// it on first use (upstream client + session manager + run manager) and
// recording the use for LRU order. A token that cannot build an upstream
// client yields an error and is never cached.
func (p *Pool) bridgeEntryFor(clientToken string) (*bridgeEntry, error) {
	p.bridgeMu.Lock()

	if entry, ok := p.bridge[clientToken]; ok {
		entry.lastUsed = time.Now()
		p.bridgeTouch(clientToken)
		p.bridgeMu.Unlock()
		return entry, nil
	}

	client, err := upstream.New(clientToken, p.cfg.Load())
	if err != nil {
		p.bridgeMu.Unlock()
		return nil, fmt.Errorf("bridge: %w", err)
	}
	entry := &bridgeEntry{token: clientToken, client: client, spend: newSpendLedger()}
	cfg := p.cfg.Load()
	entry.session = session.NewManagerWithStore(client, p.store)
	entry.session.SetReAdmitLead(cfg.SessionReAdmitLead)
	entry.session.SetAdmissionProbeTTL(cfg.SessionProbeCacheTTL)
	entry.runs = runs.NewRunManagerOpts(client, entry.session, runOptions(cfg))
	entry.lastUsed = time.Now()

	p.bridge[clientToken] = entry
	p.bridgeOrder = append(p.bridgeOrder, clientToken)
	// Drop the LRU victims under the lock, then FINISH their runs after
	// releasing it: FinishAllRuns is a sequential upstream call bounded by
	// the session-call timeout, so running it under bridgeMu would stall
	// every other bridge operation (AcquireBridge, bridgeRecordChat,
	// BridgeCount, bridgeMaintain) for the full eviction duration.
	victims := p.bridgeEvictLocked(entry)
	p.bridgeMu.Unlock()

	for _, victim := range victims {
		victim.runs.FinishAllRuns(context.Background())
	}
	return entry, nil
}

// bridgeTouch moves clientToken to the newest end of the LRU order.
func (p *Pool) bridgeTouch(clientToken string) {
	for i, tok := range p.bridgeOrder {
		if tok == clientToken {
			if i < len(p.bridgeOrder)-1 {
				copy(p.bridgeOrder[i:], p.bridgeOrder[i+1:])
				p.bridgeOrder[len(p.bridgeOrder)-1] = clientToken
			}
			return
		}
	}
	p.bridgeOrder = append(p.bridgeOrder, clientToken)
}

// bridgeEvictLocked evicts the oldest bridge entries while the cache is
// over maxBridgeEntries (LRU): the victims are removed from the cache and
// LRU order and returned so the caller can FINISH their runs best-effort
// (bounded by the client's session-call timeout) AFTER releasing bridgeMu —
// the upstream FINISH calls must not run under the lock, or a full cache
// would stall every other bridge operation for the whole eviction. keep is
// the entry that was just created by the caller; it is excluded from the
// victim scan (like busy entries) because bridgeEntryFor hands it back for
// immediate use — evicting it here would leave its run and admitted session
// outside the cache, where neither bridgeMaintain nor Pool.Shutdown would
// ever sweep them. Caller holds bridgeMu.
func (p *Pool) bridgeEvictLocked(keep *bridgeEntry) []*bridgeEntry {
	var victims []*bridgeEntry
	for len(p.bridgeOrder) > maxBridgeEntries {
		// Scan from the LRU end for an entry WITHOUT outstanding leases:
		// FINISHing the run of an entry that still serves a request would
		// kill the in-flight chat. Busy entries are left in the cache for
		// the idle sweep (bridgeMaintain) once their leases drain; when
		// every entry is busy, nothing is evicted this pass.
		evicted := false
		for i := 0; i < len(p.bridgeOrder); {
			oldest := p.bridgeOrder[i]
			entry, ok := p.bridge[oldest]
			if !ok {
				// Stale LRU token (cache entry dropped elsewhere): trim it
				// and keep scanning.
				p.bridgeOrder = removeBridgeOrder(p.bridgeOrder, oldest)
				continue
			}
			// The just-created entry is never its own eviction victim: the
			// caller will admit a session and START a run on it, and an
			// entry outside the cache is invisible to bridgeMaintain and
			// Pool.Shutdown — a leaked upstream run + admitted session
			// burning a daily slot per new client under saturation. Skip it
			// like a busy entry; the cache may briefly sit one over the cap
			// until an older entry's lease drains.
			if entry == keep {
				i++
				continue
			}
			if entry.runs.InflightCount() > 0 {
				i++
				continue
			}
			victims = append(victims, entry)
			delete(p.bridge, oldest)
			p.bridgeOrder = removeBridgeOrder(p.bridgeOrder, oldest)
			p.logger.Debug("pool: bridge entry evicted (cache full)", "bridge_entries", len(p.bridge))
			evicted = true
			break
		}
		if !evicted {
			break
		}
	}
	return victims
}

// bridgeRecordChat appends one successful upstream chat for the bridge
// entry and prunes its usage history outside the 24h window.
func (p *Pool) bridgeRecordChat(entry *bridgeEntry) {
	if entry == nil {
		return
	}
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	cutoff := time.Now().Add(-usageWindow)
	history := entry.usage
	first := 0
	for first < len(history) && history[first].Before(cutoff) {
		first++
	}
	entry.usage = append(history[first:], time.Now())
}

// bridgeUsageCount returns how many successful chats the bridge entry sent
// within the last usageWindow, pruning expired timestamps.
func (p *Pool) bridgeUsageCount(entry *bridgeEntry) int {
	if entry == nil {
		return 0
	}
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	cutoff := time.Now().Add(-usageWindow)
	history := entry.usage
	first := 0
	for first < len(history) && history[first].Before(cutoff) {
		first++
	}
	entry.usage = history[first:]
	return len(entry.usage)
}

// bridgeUsageResetIn is how long until the bridge entry's oldest usage
// timestamp ages out of the window (0 when no usage is recorded or the
// reset is due).
func (p *Pool) bridgeUsageResetIn(entry *bridgeEntry) time.Duration {
	if entry == nil {
		return 0
	}
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	history := entry.usage
	if len(history) == 0 {
		return 0
	}
	reset := time.Until(history[0].Add(usageWindow))
	if reset < 0 {
		return 0
	}
	return reset
}

// bridgeDailyLimitError builds the 429 surfaced when the bridge entry is
// capped by MAX_MESSAGES_PER_DAY (mirrors dailyLimitError for fixed
// tokens): RetryAfter is the time until the entry's oldest recorded chat
// ages out of the 24h window.
func (p *Pool) bridgeDailyLimitError(entry *bridgeEntry) *upstream.RateLimitError {
	return &upstream.RateLimitError{
		RetryAfter:  p.bridgeUsageResetIn(entry),
		Limit:       float64(p.cfg.Load().MaxMessagesPerDay),
		RecentCount: float64(p.bridgeUsageCount(entry)),
		Body:        "daily message limit reached",
	}
}

// bridgeLen returns the number of cached bridge entries (test accessor).
func (p *Pool) bridgeLen() int {
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	return len(p.bridge)
}

// bridgeToken returns the cached entry for clientToken (test accessor).
func (p *Pool) bridgeToken(clientToken string) *bridgeEntry {
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	return p.bridge[clientToken]
}

// bridgeTokenLabel returns a short non-reversible label for a bridge
// entry's client token, safe for logs: the sha256 of the token, hex,
// truncated to 8 chars. The raw client token must never reach logs (logring
// retains them for /admin/logs), so shutdown and diagnostics use the label,
// not the token.
func bridgeTokenLabel(entry *bridgeEntry) string {
	if entry == nil || entry.client == nil {
		return "bridge"
	}
	return "token-" + entry.client.TokenKey()[:8]
}

// bestDailyLimit picks the daily-cap error whose window frees first: the
// client retries when the first token has a free slot again.
func bestDailyLimit(entries []*upstream.RateLimitError) *upstream.RateLimitError {
	best := entries[0]
	for _, e := range entries[1:] {
		if e.RetryAfter < best.RetryAfter {
			best = e
		}
	}
	return best
}

// idleFor is how long the pool has gone without a successful Acquire (0
// when no request ever arrived, so a freshly prewarmed pool is not treated
// as idle).
func (p *Pool) idleFor() time.Duration {
	p.lastActiveMu.Lock()
	defer p.lastActiveMu.Unlock()
	if p.lastActive.IsZero() {
		return 0
	}
	return time.Since(p.lastActive)
}

// setIdleFinishedOnce marks the idle FINISH as done and reports whether
// this call performed it (false when it was already done). The next
// Acquire success resets the flag.
func (p *Pool) setIdleFinishedOnce() bool {
	p.lastActiveMu.Lock()
	defer p.lastActiveMu.Unlock()
	if p.idleFinished {
		return false
	}
	p.idleFinished = true
	return true
}

// prewarm starts a run for every agent on every token, best-effort, bounded
// by the request timeout.
func (p *Pool) prewarm(ctx context.Context, agentIDs []string) {
	defer p.wg.Done()
	toks := p.toks.Load()
	for i, tok := range *toks {
		preCtx, cancel := context.WithTimeout(ctx, p.cfg.Load().RequestTimeout)
		tok.runs.Prewarm(preCtx, agentIDs)
		cancel()
		p.logger.Debug("pool: prewarm done", "token", i+1, "agents", len(agentIDs))
	}
}

// maintainLoop ticks every maintainInterval: per token, rotate aged runs and
// advance queued sessions. Session-liveness polls run on their own finer
// jittered schedule (sessionPollTick fires when a token's nextPollAt is
// due; see the sessionPoll* constants). When IDLE_ROTATION_TIMEOUT is set,
// the pool pauses this activity after it has been idle past the timeout:
// one pass FINISHes all runs (so no rotation/session-refresh activity
// continues upstream) and every further pass is skipped until the next
// request — Acquire re-creates runs on demand.
func (p *Pool) maintainLoop(ctx context.Context) {
	defer p.wg.Done()
	ticker := time.NewTicker(maintainInterval)
	defer ticker.Stop()
	// The poll grid is finer than maintainInterval so the per-token jittered
	// ~30s liveness polls (gap #2) are not quantized onto the 60s rotation
	// grid — a due poll fires on the first grid point at/after nextPollAt.
	pollTicker := time.NewTicker(sessionPollCheckInterval)
	defer pollTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.maintainTick(ctx)
		case <-pollTicker.C:
			p.sessionPollTick(ctx)
		}
	}
}

// maintainTick runs one maintenance pass: the idle handling (see
// maintainLoop), then the per-token rotate/refresh work. Split out of
// maintainLoop so tests can drive a pass without waiting for the
// minute-long ticker.
func (p *Pool) maintainTick(ctx context.Context) {
	toks := p.toks.Load()
	cfg := p.cfg.Load()
	// Drop retired tokens that never saw a slipped lease (their runs were
	// drained at removal). Entries still carrying a lease stay until
	// LeaseRelease drains them; the park grace covers an Acquire that loaded
	// the pre-removal snapshot (see RemoveLastToken).
	p.pruneRetired()
	if cfg.IdleRotationTimeout > 0 && p.idleFor() > cfg.IdleRotationTimeout {
		// Past the idle threshold. If this is the first idle pass, FINISH
		// every run so the token's rotation/refresh activity stops
		// upstream; sessions are left untouched. Later passes skip the
		// per-token work entirely while the pool stays idle.
		if !p.setIdleFinishedOnce() {
			// Later idle passes still sweep idle bridge entries: without
			// this, entries idle past bridgeIdleEvict are never evicted
			// while the pool stays idle and their sessions stay admitted
			// upstream until expiry.
			p.bridgeMaintain(ctx, true)
			return
		}
		for _, tok := range *toks {
			// Skip tokens with outstanding leases: FINISHing this run
			// would kill an in-flight chat; leave it for rotation once the
			// lease drains (same rule as the bridge idle sweep).
			if tok.runs.InflightCount() > 0 {
				continue
			}
			// Thread the maintain ctx: Pool.Shutdown cancels it first, so a
			// mid-drain FINISH must abort on cancel instead of blocking
			// shutdown for the full upstream call timeout.
			tok.runs.FinishAllRuns(ctx)
		}
		p.bridgeMaintain(ctx, true)
		return
	}
	for i, tok := range *toks {
		// Cooldown: skip all per-token maintain work (rotate, draining
		// FINISH, queued-session advance). Upstream calls during a cooldown
		// look like abuse; the skip is silent — the cooldown itself is
		// already surfaced elsewhere (Acquire logs the skip).
		if time.Now().Before(tok.runs.CooldownUntil()) {
			continue
		}
		mCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
		tok.runs.Maintain(mCtx)
		// Advance queued sessions (GET poll). Skipped while a chat is in
		// flight: the upstream allows one client per account at a time, and
		// a poll GET that lands mid-chat can kick the active session (428
		// waiting_room). Mirror the reference session manager's in-flight
		// gate (reference/freebuff-proxy-hengxin session-manager.js:37-49,
		// 259-260). Active-session liveness polls are NOT part of this pass
		// — they run on the jittered sessionPollTick schedule (gap #2).
		if tok.runs.InflightCount() == 0 {
			snap := tok.session.Snapshot()
			if snap.Status == "queued" {
				if _, err := tok.session.EnsureSession(mCtx); err != nil {
					p.logger.Debug("pool: maintain session not ready", "token", i+1, "err", err)
				} else {
					// Issue #90a: the queue advanced to active — pre-create
					// the run for the session's model agent so the first
					// request on this session does not pay the START latency.
					after := tok.session.Snapshot()
					if agentID, err := p.reg.AgentForModel(after.Model); err == nil && agentID != "" {
						_ = tok.runs.Precreate(mCtx, agentID)
					}
				}
			}
		}
		cancel()
	}
	// Bridge sweep: drop entries idle past bridgeIdleEvict (runs FINISHed
	// best-effort), maintain the rest like the fixed tokens above.
	p.bridgeMaintain(ctx, false)
}

// sessionPollTick runs the per-token session-liveness polls on their own
// jittered schedule (see the sessionPoll* constants): an active (or
// in-grace ended) session is compact-polled every ~30s ±20% — capped to
// remaining+1s near expiry — with 20s→300s failure backoff honoring the
// server's Retry-After, mirroring the CLI's liveness fingerprint (gap #2;
// reference/freebuff sdk polling-backoff.ts). Rotation and queued-session
// advance stay on the coarse maintainInterval ticker (maintainTick). The
// poll is skipped while a chat is in flight (the upstream allows one client
// per account at a time; a poll landing mid-chat can kick the active
// session with 428) and while the token cools down, exactly like
// maintainTick.
func (p *Pool) sessionPollTick(ctx context.Context) {
	cfg := p.cfg.Load()
	if cfg.IdleRotationTimeout > 0 && p.idleFor() > cfg.IdleRotationTimeout {
		// Session polls pause with the fixed tokens while idle (the
		// maintain pass already FINISHed every run upstream).
		return
	}
	toks := p.toks.Load()
	for i, tok := range *toks {
		if time.Now().Before(tok.runs.CooldownUntil()) {
			// Cooldown: no session poll (same rule as maintainTick).
			continue
		}
		if tok.runs.InflightCount() > 0 {
			// Mid-chat in-flight gate (same rule as maintainTick): a poll
			// GET can kick the active session (428 waiting_room). Leave the
			// schedule due; the next pass polls once the lease drains.
			continue
		}
		now := time.Now()
		if !tok.nextPollAt.IsZero() && now.Before(tok.nextPollAt) {
			continue
		}
		mCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
		err := tok.session.Poll(mCtx)
		cancel()
		var delay time.Duration
		if err != nil {
			tok.pollFailures++
			delay = sessionPollBackoffDelay(tok.pollFailures, sessionPollRetryAfter(err))
			p.logger.Debug("pool: session poll failed", "token", i+1, "err", err, "retry_in", delay)
		} else {
			tok.pollFailures = 0
			delay = sessionPollSuccessDelay(tok.session.Snapshot())
		}
		tok.nextPollAt = time.Now().Add(delay)
	}
	p.bridgeSessionPollTick(ctx, cfg)
}

// bridgeSessionPollTick polls the bridge cache's active sessions on the same
// jittered schedule as the fixed tokens (gap #2). The sweep/eviction half
// stays in bridgeMaintain; only the per-entry session poll runs here so its
// timing is not quantized onto the 60s rotation grid.
func (p *Pool) bridgeSessionPollTick(ctx context.Context, cfg *config.Config) {
	p.bridgeMu.Lock()
	entries := make([]*bridgeEntry, 0, len(p.bridge))
	for _, entry := range p.bridge {
		entries = append(entries, entry)
	}
	p.bridgeMu.Unlock()

	for _, entry := range entries {
		if time.Now().Before(entry.runs.CooldownUntil()) {
			continue
		}
		if entry.runs.InflightCount() > 0 {
			continue
		}
		now := time.Now()
		if !entry.nextPollAt.IsZero() && now.Before(entry.nextPollAt) {
			continue
		}
		mCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
		err := entry.session.Poll(mCtx)
		cancel()
		var delay time.Duration
		if err != nil {
			entry.pollFailures++
			delay = sessionPollBackoffDelay(entry.pollFailures, sessionPollRetryAfter(err))
			p.logger.Debug("pool: bridge session poll failed", "err", err, "retry_in", delay)
		} else {
			entry.pollFailures = 0
			snap := entry.session.Snapshot()
			// A successful poll is a session probe: refresh the entry's
			// cached accessTier so a tier learned here (even a compact
			// response) survives into leases whose admissions omit it.
			if snap.TierAccess != "" {
				p.bridgeMu.Lock()
				entry.tier = snap.TierAccess
				p.bridgeMu.Unlock()
			}
			delay = sessionPollSuccessDelay(snap)
		}
		entry.nextPollAt = time.Now().Add(delay)
	}
}

// sessionPollSuccessDelay returns the delay before the next liveness poll
// after a SUCCESSFUL poll: ~30s ±20% jitter, capped so a poll near expiry
// lands ~1s after expires_at (the CLI observes the status flip then;
// reference/freebuff sdk polling-backoff.ts). Sessions already inside the
// grace drain poll at the plain jittered cadence.
func sessionPollSuccessDelay(snap session.SessionSnapshot) time.Duration {
	d := sessionPollJittered(sessionPollBaseInterval)
	if !snap.ExpiresAt.IsZero() {
		if rem := time.Until(snap.ExpiresAt); rem > 0 && rem+time.Second < d {
			d = rem + time.Second
		}
	}
	return d
}

// sessionPollBackoffDelay returns the delay after a FAILED poll: 20s ×2 per
// consecutive failure (cap 300s) with equal jitter over the lower half of
// the window, and never before the server's Retry-After floor (multiplied
// by 1 ± 0.2 jitter, capped 300s) — polling-backoff.ts semantics.
func sessionPollBackoffDelay(failures int, retryAfter time.Duration) time.Duration {
	if failures < 1 {
		failures = 1
	}
	d := sessionPollBackoffBase << min(failures-1, 5)
	if d > sessionPollBackoffMax {
		d = sessionPollBackoffMax
	}
	d = d/2 + time.Duration(sessionRand()%uint64(d/2))
	if retryAfter > 0 {
		ra := retryAfter - retryAfter/5 + time.Duration(sessionRand()%uint64(2*retryAfter/5))
		if ra > d {
			d = ra
		}
		if d > sessionPollBackoffMax {
			d = sessionPollBackoffMax
		}
	}
	return d
}

// sessionPollJittered applies the CLI's symmetric ±20% jitter around d.
func sessionPollJittered(d time.Duration) time.Duration {
	span := d / 5
	return d - span + time.Duration(sessionRand()%uint64(2*span+1))
}

// sessionRand draws one uint64 from crypto/rand (the pool's jitter source,
// matching the upstream client's pattern). A read failure is unrecoverable
// in practice; fall back to the clock rather than panicking in a background
// loop.
func sessionRand() uint64 {
	var b [8]byte
	if _, err := cryptoRand.Read(b[:]); err != nil {
		return uint64(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint64(b[:])
}

// sessionPollRetryAfter extracts the server's Retry-After floor from a
// failed session poll error (0 when the error carries none). The backoff
// never schedules a retry before this floor.
func sessionPollRetryAfter(err error) time.Duration {
	var ue *upstream.UpstreamError
	if errors.As(err, &ue) {
		return ue.RetryAfter
	}
	var rle *upstream.RateLimitError
	if errors.As(err, &rle) {
		return rle.RetryAfter
	}
	var wrr *upstream.WaitingRoomRequiredError
	if errors.As(err, &wrr) {
		return wrr.RetryAfter
	}
	var wr *session.WaitingRoomError
	if errors.As(err, &wr) {
		return wr.RetryAfter
	}
	return 0
}

// pruneRetired drops retired tokens that hold no leases and have been
// parked past the drain grace (their runs were already finished at removal
// or on the last release). Entries still carrying a lease stay until
// LeaseRelease drains them.
func (p *Pool) pruneRetired() {
	p.retiredMu.Lock()
	defer p.retiredMu.Unlock()
	for entry, swappedAt := range p.retired {
		if entry.runs.InflightCount() == 0 && time.Since(swappedAt) > retiredDrainGrace {
			delete(p.retired, entry)
		}
	}
}

// bridgeMaintain sweeps the bridge cache: entries idle past bridgeIdleEvict
// are dropped (runs FINISHed and the upstream session ended, best-effort);
// entries with in-flight leases are NEVER evicted — FINISHing their runs
// would kill the in-flight chat, so busy entries always get the per-token
// maintain work below and are only swept once their leases drain and they
// stay idle. The remaining entries get the per-token maintain work — rotate
// aged runs and advance queued sessions, bounded by the same RequestTimeout
// ctx as the fixed-token loop. On idle passes (idle=true) only the sweep
// runs: the per-entry queued-advance pauses with the fixed tokens, and the
// idle-sweep keeps bridge entries from staying admitted upstream past
// bridgeIdleEvict while the pool stays idle. Active-session liveness polls
// are NOT part of this pass — they run on the jittered
// bridgeSessionPollTick schedule (gap #2).
func (p *Pool) bridgeMaintain(ctx context.Context, idle bool) {
	cfg := p.cfg.Load()
	var toEvict []*bridgeEntry
	var toMaintain []*bridgeEntry

	p.bridgeMu.Lock()
	now := time.Now()
	for token, entry := range p.bridge {
		// Busy entry: leave it for the maintain pass (same rule as
		// bridgeEvictLocked's busy skip — the idle sweep only handles
		// entries once their leases drain).
		if entry.runs.InflightCount() > 0 {
			toMaintain = append(toMaintain, entry)
			continue
		}
		if now.Sub(entry.lastUsed) > bridgeIdleEvict {
			toEvict = append(toEvict, entry)
			delete(p.bridge, token)
			p.bridgeOrder = removeBridgeOrder(p.bridgeOrder, token)
			p.logger.Debug("pool: bridge entry evicted (idle)", "bridge_entries", len(p.bridge))
		} else {
			toMaintain = append(toMaintain, entry)
		}
	}
	p.bridgeMu.Unlock()

	for _, entry := range toEvict {
		// Mirror the shutdown drain: FINISH the runs AND end the entry's
		// upstream session, so a dropped idle entry does not leak its
		// session upstream. Bounded by the same RequestTimeout ctx as the
		// per-token maintain work so a hung upstream cannot stall the loop.
		eCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
		entry.runs.FinishAllRuns(eCtx)
		_ = entry.session.EndSession(eCtx)
		cancel()
	}
	for _, entry := range toMaintain {
		if idle {
			// Idle pass: the per-entry maintain work pauses with the fixed
			// tokens; only the idle-eviction sweep above runs.
			continue
		}
		// Same cooldown skip as the fixed-token loop: no queued-session
		// EnsureSession, no rotation while cooling down.
		if time.Now().Before(entry.runs.CooldownUntil()) {
			continue
		}
		mCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
		entry.runs.Maintain(mCtx)
		// Same in-flight gate as the fixed-token loop: skip the queued-
		// session GET while a chat is in flight so it cannot kick the active
		// session (reference/freebuff-proxy-hengxin session-manager.js:37-49,
		// 259-260). Active-session liveness polls run on the jittered
		// bridgeSessionPollTick schedule instead.
		if entry.runs.InflightCount() == 0 {
			snap := entry.session.Snapshot()
			if snap.Status == "queued" {
				if _, err := entry.session.EnsureSession(mCtx); err != nil {
					p.logger.Debug("pool: bridge maintain session not ready", "err", err)
				} else {
					// Issue #90a: pre-create the run for the session's model
					// agent so the first request on this session does not pay
					// the START latency (mirrors the fixed-token path).
					after := entry.session.Snapshot()
					if agentID, err := p.reg.AgentForModel(after.Model); err == nil && agentID != "" {
						_ = entry.runs.Precreate(mCtx, agentID)
					}
				}
			}
		}
		cancel()
	}
}

// removeBridgeOrder drops token from the LRU order slice.
func removeBridgeOrder(order []string, token string) []string {
	for i, tok := range order {
		if tok == token {
			return append(order[:i], order[i+1:]...)
		}
	}
	return order
}

// bestWaitingRoom picks the queue entry with the lowest position; ties break
// on the lowest queue depth (PRD §3: best-waiting-room-position selection).
func bestWaitingRoom(entries []*session.WaitingRoomError) *session.WaitingRoomError {
	best := entries[0]
	for _, candidate := range entries[1:] {
		if betterWait(candidate, best) {
			best = candidate
		}
	}
	return best
}

// betterWait reports whether a outranks b. Positions <= 0 mean "unknown" and
// rank below any known position (mirrors freebuff2api-quorinex).
func betterWait(a, b *session.WaitingRoomError) bool {
	if b == nil {
		return true
	}
	if a.Position <= 0 {
		return false
	}
	if b.Position <= 0 {
		return true
	}
	if a.Position != b.Position {
		return a.Position < b.Position
	}
	return a.QueueDepth < b.QueueDepth
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
