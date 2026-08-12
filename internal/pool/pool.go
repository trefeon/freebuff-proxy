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
//     only when every token is queued does the pool surface the waiting-room
//     error (503 + Retry-After upstream).
//   - run-invalid / session-invalid recoveries are NOT handled here: the
//     caller (server) retries once via a fresh Acquire after invalidating.
//   - anything else → next token; all failed → combined error.
package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/runs"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/upstream"
)

// maintainInterval is how often the background job rotates aged runs and
// advances queued sessions (PRD §3: 60s maintain ticker).
const maintainInterval = time.Minute

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

// Lease is one acquired right to send a chat request through a specific
// token. The caller must call Pool.LeaseRelease when the request completes
// or fails (it decrements the run's inflight counter).
type Lease struct {
	Token             int // index into config.AuthTokens (-1 for bridge leases)
	AgentID           string
	Run               *runs.Run
	SessionInstanceID string       // "" when the session is disabled
	TierAccess        string       // upstream accessTier, "" when unknown
	TierCountry       string       // upstream countryCode, "" when unknown
	Bridge            *bridgeEntry // nil for pooled (fixed-token) leases
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
}

// Pool balances requests across the configured tokens.
type Pool struct {
	cfg  *config.Config
	reg  *registry.Registry
	toks []*tokenEntry

	rr     atomic.Uint64 // round-robin start index
	logger *slog.Logger

	once   sync.Once
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Usage tracking for MAX_MESSAGES_PER_DAY: one timestamp per successful
	// upstream chat, per token. Guarded by usageMu.
	usageMu      sync.Mutex
	msgsPerToken [][]time.Time

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
}

type tokenEntry struct {
	session *session.Manager
	runs    *runs.RunManager
	client  *upstream.Client
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

	p := &Pool{cfg: cfg, reg: reg, logger: slog.Default(), bridge: make(map[string]*bridgeEntry)}
	p.msgsPerToken = make([][]time.Time, len(cfg.AuthTokens))
	for i := range cfg.AuthTokens {
		p.toks = append(p.toks, &tokenEntry{
			session: sessions[i],
			runs:    runs.NewRunManager(clients[i], sessions[i], cfg.RotationInterval),
			client:  clients[i],
		})
	}
	return p, nil
}

// Acquire resolves the model's agent, picks a start token round-robin, and
// fails over linearly until a token yields both a run and a session. Returns
// a lease on success. Registry misses (unknown model) are returned as-is.
func (p *Pool) Acquire(ctx context.Context, model string) (*Lease, error) {
	if len(p.toks) == 0 {
		return nil, errors.New("pool: no auth tokens configured")
	}
	agentID, err := p.reg.AgentForModel(model)
	if err != nil {
		return nil, err
	}

	start := int(p.rr.Add(1)-1) % len(p.toks)
	var errs []string
	var waiting []*session.WaitingRoomError
	var rateLimited []*upstream.RateLimitError
	var banned []*upstream.BanError
	var dailyLimited []*upstream.RateLimitError

	for offset := 0; offset < len(p.toks); offset++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		idx := (start + offset) % len(p.toks)
		tok := p.toks[idx]
		name := fmt.Sprintf("token-%d", idx+1)

		if until := tok.runs.CooldownUntil(); time.Now().Before(until) {
			errs = append(errs, fmt.Sprintf("%s: cooling down until %s", name, until.Format(time.RFC3339)))
			p.logger.Debug("pool: token skipped (cooldown)", "token", idx+1, "until", until.Format(time.RFC3339))
			if rle := tok.runs.RateLimitError(); rle != nil {
				rateLimited = append(rateLimited, rle)
			}
			if be := tok.runs.BanError(); be != nil {
				banned = append(banned, be)
			}
			continue
		}

		// Daily rolling cap: a token that already sent its
		// MAX_MESSAGES_PER_DAY successful chats in the last 24h is skipped
		// like a cooldown; when every token is capped, the pool surfaces a
		// 429 with the earliest window reset.
		if p.cfg.MaxMessagesPerDay > 0 && p.usageCount(idx) >= p.cfg.MaxMessagesPerDay {
			dailyLimited = append(dailyLimited, p.dailyLimitError(idx))
			errs = append(errs, fmt.Sprintf("%s: daily message limit (%d) reached", name, p.cfg.MaxMessagesPerDay))
			p.logger.Debug("pool: token skipped (daily message limit)", "token", idx+1, "limit", p.cfg.MaxMessagesPerDay)
			continue
		}

		run, err := tok.runs.Acquire(ctx, agentID)
		if err != nil {
			if errors.Is(err, upstream.ErrAuthRejected) {
				tok.runs.Cooldown(runs.DefaultCooldown)
				p.logger.Debug("pool: token cooling down", "token", idx+1, "duration", runs.DefaultCooldown.String())
			}
			if rle := asRateLimit(err); rle != nil {
				tok.runs.CooldownRateLimit(rle)
				rateLimited = append(rateLimited, rle)
			}
			if be := asBan(err); be != nil {
				tok.runs.CooldownBan(be)
				banned = append(banned, be)
			}
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			continue
		}

		instanceID, err := tok.session.EnsureSession(ctx)
		if err != nil {
			// Release the run lease we just acquired — otherwise the
			// inflight counter never returns to zero and the run can
			// never be FINISHed on rotation (draining-list leak).
			tok.runs.Release(run)
			var wr *session.WaitingRoomError
			if errors.As(err, &wr) {
				waiting = append(waiting, wr)
			}
			if rle := asRateLimit(err); rle != nil {
				tok.runs.CooldownRateLimit(rle)
				rateLimited = append(rateLimited, rle)
			}
			if be := asBan(err); be != nil {
				tok.runs.CooldownBan(be)
				banned = append(banned, be)
			}
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			continue
		}

		ss := tok.session.Snapshot()
		p.logger.Debug("pool: lease acquired", "token", idx+1, "model", model, "agent", agentID, "instance_id", instanceID,
			"tier", ss.TierAccess, "country", ss.TierCountry)
		// Track the activity and end any idle-maintenance pause: the next
		// maintain tick resumes rotation/refresh work.
		p.lastActiveMu.Lock()
		p.lastActive = time.Now()
		p.idleFinished = false
		p.lastActiveMu.Unlock()
		return &Lease{Token: idx, AgentID: agentID, Run: run, SessionInstanceID: instanceID,
			TierAccess: ss.TierAccess, TierCountry: ss.TierCountry}, nil
	}

	if len(waiting) == len(p.toks) && len(waiting) > 0 {
		wr := bestWaitingRoom(waiting)
		p.logger.Debug("pool: waiting room surfaced", "position", wr.Position, "queue_depth", wr.QueueDepth, "retry_after", wr.RetryAfter.String())
		return nil, wr
	}
	if len(rateLimited) == len(p.toks) && len(rateLimited) > 0 {
		return nil, bestRateLimit(rateLimited)
	}
	if len(banned) == len(p.toks) && len(banned) > 0 {
		return nil, banned[0]
	}
	if len(dailyLimited) == len(p.toks) && len(dailyLimited) > 0 {
		return nil, bestDailyLimit(dailyLimited)
	}
	return nil, fmt.Errorf("unable to acquire run from any token: %s", strings.Join(errs, "; "))
}

// AcquireBridge acquires a lease for one client-supplied token in bridge
// mode (no AUTH_TOKENS configured). The entry — upstream client, session
// manager, and run manager — is created lazily on first use and cached for
// reuse across that client's later requests (least quota burn). There is no
// multi-token failover: a single token either yields a lease or its error
// is returned as-is. Registry misses pass through.
func (p *Pool) AcquireBridge(ctx context.Context, clientToken, model string) (*Lease, error) {
	clientToken = strings.TrimSpace(clientToken)
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
	// rate-limit/ban error so the client keeps getting 429/403 instead of a
	// generic failure (mirrors the fixed-token cooldown-skip branch).
	if until := entry.runs.CooldownUntil(); time.Now().Before(until) {
		if rle := entry.runs.RateLimitError(); rle != nil {
			return nil, rle
		}
		if be := entry.runs.BanError(); be != nil {
			return nil, be
		}
		return nil, fmt.Errorf("bridge: token cooling down until %s", until.Format(time.RFC3339))
	}

	// Daily rolling cap, per client token (mirrors the fixed-token path).
	if p.cfg.MaxMessagesPerDay > 0 && p.bridgeUsageCount(entry) >= p.cfg.MaxMessagesPerDay {
		p.logger.Debug("pool: bridge entry daily message limit", "limit", p.cfg.MaxMessagesPerDay)
		return nil, p.bridgeDailyLimitError(entry)
	}

	run, err := entry.runs.Acquire(ctx, agentID)
	if err != nil {
		if errors.Is(err, upstream.ErrAuthRejected) {
			entry.runs.Cooldown(runs.DefaultCooldown)
			p.logger.Debug("pool: bridge entry cooling down", "duration", runs.DefaultCooldown.String())
		}
		if rle := asRateLimit(err); rle != nil {
			entry.runs.CooldownRateLimit(rle)
		}
		if be := asBan(err); be != nil {
			entry.runs.CooldownBan(be)
		}
		return nil, err
	}

	instanceID, err := entry.session.EnsureSession(ctx)
	if err != nil {
		// Release the run lease we just acquired — otherwise the inflight
		// counter never returns to zero and the run can never be FINISHed
		// on rotation (draining-list leak).
		entry.runs.Release(run)
		if rle := asRateLimit(err); rle != nil {
			entry.runs.CooldownRateLimit(rle)
		}
		if be := asBan(err); be != nil {
			entry.runs.CooldownBan(be)
		}
		return nil, err
	}

	ss := entry.session.Snapshot()
	p.logger.Debug("pool: bridge lease acquired", "model", model, "agent", agentID, "instance_id", instanceID,
		"tier", ss.TierAccess, "country", ss.TierCountry)
	return &Lease{Token: -1, AgentID: agentID, Run: run, SessionInstanceID: instanceID,
		TierAccess: ss.TierAccess, TierCountry: ss.TierCountry, Bridge: entry}, nil
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
	if lease.Token < 0 || lease.Token >= len(p.toks) {
		return
	}
	p.toks[lease.Token].runs.Release(lease.Run)
}

// InvalidateSession drops the cached free session of token so the next
// Acquire re-creates it (session-invalid recovery). Out-of-range tokens are
// ignored.
func (p *Pool) InvalidateSession(token int) {
	if token < 0 || token >= len(p.toks) {
		return
	}
	p.toks[token].session.Invalidate()
}

// InvalidateRun drops the current run of token for agentID so the next
// Acquire starts a fresh one (run-invalid recovery). Out-of-range tokens are
// ignored.
func (p *Pool) InvalidateRun(token int, agentID string) {
	if token < 0 || token >= len(p.toks) {
		return
	}
	p.toks[token].runs.Invalidate(agentID)
}

// CooldownToken puts token in a cooldown window of duration d (auth-reject
// recovery, e.g. runs.DefaultCooldown). Out-of-range tokens are ignored.
func (p *Pool) CooldownToken(token int, d time.Duration) {
	if token < 0 || token >= len(p.toks) {
		return
	}
	p.toks[token].runs.Cooldown(d)
}

// CooldownTokenRateLimit applies a rate-limit cooldown to token
// (remembered so Acquire surfaces 429 + Retry-After during the window).
// Out-of-range tokens are ignored.
func (p *Pool) CooldownTokenRateLimit(token int, rle *upstream.RateLimitError) {
	if token < 0 || token >= len(p.toks) || rle == nil {
		return
	}
	p.toks[token].runs.CooldownRateLimit(rle)
}

// CooldownTokenBan applies a ban cooldown to token (remembered so
// Acquire surfaces 403 banned + resumes-at during the window).
func (p *Pool) CooldownTokenBan(token int, be *upstream.BanError) {
	if token < 0 || token >= len(p.toks) || be == nil {
		return
	}
	p.toks[token].runs.CooldownBan(be)
}

// InvalidateBridgeSession drops the cached free session of the bridge
// entry so the next AcquireBridge re-creates it (session-invalid recovery).
func (p *Pool) InvalidateBridgeSession(lease *Lease) {
	if lease == nil || lease.Bridge == nil {
		return
	}
	lease.Bridge.session.Invalidate()
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
// (remembered so AcquireBridge surfaces 429 + Retry-After).
func (p *Pool) CooldownBridgeRateLimit(lease *Lease, rle *upstream.RateLimitError) {
	if lease == nil || lease.Bridge == nil || rle == nil {
		return
	}
	lease.Bridge.runs.CooldownRateLimit(rle)
}

// CooldownBridgeBan applies a ban cooldown to the bridge entry (remembered
// so AcquireBridge surfaces 403 banned + resumes-at during the window).
func (p *Pool) CooldownBridgeBan(lease *Lease, be *upstream.BanError) {
	if lease == nil || lease.Bridge == nil || be == nil {
		return
	}
	lease.Bridge.runs.CooldownBan(be)
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
		}
		return rc, err
	}
	if lease.Token < 0 || lease.Token >= len(p.toks) {
		return nil, errors.New("pool: chat: invalid lease token")
	}
	rc, err := p.toks[lease.Token].client.ChatCompletions(ctx, opts, body)
	if err == nil {
		// Only chats that actually went upstream count against the daily
		// cap; errors are not recorded.
		p.recordChat(lease.Token)
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
// runs, end the sessions, bounded by a 10s force deadline per token.
func (p *Pool) Shutdown(ctx context.Context) {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()

	var errs []string
	for i, tok := range p.toks {
		tokCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
		tok.runs.Shutdown(tokCtx)
		cancel()
		if snap := tok.runs.Snapshot(); snap.ActiveRuns > 0 {
			errs = append(errs, fmt.Sprintf("token-%d: %d runs left after shutdown", i+1, snap.ActiveRuns))
		}
	}
	if len(errs) > 0 {
		slog.Warn("pool: shutdown incomplete", "errors", strings.Join(errs, "; "))
	}
}

// Snapshot returns the per-token healthz view.
func (p *Pool) Snapshot() []TokenSnapshot {
	out := make([]TokenSnapshot, 0, len(p.toks))
	dailyLimit := p.cfg.MaxMessagesPerDay
	for i, tok := range p.toks {
		rs := tok.runs.Snapshot()
		ss := tok.session.Snapshot()
		msgs := p.usageCount(i)

		usagePct := 0
		if dailyLimit > 0 {
			usagePct = (msgs * 100) / dailyLimit
			if usagePct > 100 {
				usagePct = 100
			}
		}

		riskLevel := "low"
		switch {
		case !rs.CooldownUntil.IsZero() && time.Now().Before(rs.CooldownUntil):
			riskLevel = "high"
		case rs.BanError != nil:
			riskLevel = "critical"
		case dailyLimit > 0 && usagePct >= 90:
			riskLevel = "critical"
		case dailyLimit > 0 && usagePct >= 70:
			riskLevel = "high"
		case msgs > 120:
			riskLevel = "high"
		case (dailyLimit > 0 && usagePct >= 30) || msgs >= 50:
			riskLevel = "moderate"
		}

		out = append(out, TokenSnapshot{
			Token:                i,
			CooldownUntil:        rs.CooldownUntil,
			ActiveRuns:           rs.ActiveRuns,
			Requests:             rs.Requests,
			Messages24h:          msgs,
			DailyLimit:           dailyLimit,
			UsagePct:             usagePct,
			RiskLevel:            riskLevel,
			SessionStatus:        ss.Status,
			SessionInstanceID:    ss.InstanceID,
			SessionQueuePosition: ss.QueuePosition,
			SessionQueueDepth:    ss.QueueDepth,
		})
	}
	return out
}

// --- internals ---

// recordChat appends one successful upstream chat for token and prunes the
// token's usage history outside the 24h window.
func (p *Pool) recordChat(token int) {
	if token < 0 || token >= len(p.toks) {
		return
	}
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	cutoff := time.Now().Add(-usageWindow)
	history := p.msgsPerToken[token]
	first := 0
	for first < len(history) && history[first].Before(cutoff) {
		first++
	}
	p.msgsPerToken[token] = append(history[first:], time.Now())
}

// usageCount returns how many successful chats token sent within the last
// usageWindow, pruning expired timestamps.
func (p *Pool) usageCount(token int) int {
	if token < 0 || token >= len(p.toks) {
		return 0
	}
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
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
		Limit:       float64(p.cfg.MaxMessagesPerDay),
		RecentCount: float64(p.usageCount(token)),
		Body:        "daily message limit reached",
	}
}

// usageResetIn is how long until token's oldest usage timestamp ages out of
// the window (0 when the token has no recorded usage or the reset is due).
func (p *Pool) usageResetIn(token int) time.Duration {
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
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
	defer p.bridgeMu.Unlock()

	if entry, ok := p.bridge[clientToken]; ok {
		entry.lastUsed = time.Now()
		p.bridgeTouch(clientToken)
		return entry, nil
	}

	client, err := upstream.New(clientToken, p.cfg)
	if err != nil {
		return nil, fmt.Errorf("bridge: %w", err)
	}
	entry := &bridgeEntry{token: clientToken, client: client}
	entry.session = session.NewManager(client)
	entry.runs = runs.NewRunManager(client, entry.session, p.cfg.RotationInterval)
	entry.lastUsed = time.Now()

	p.bridge[clientToken] = entry
	p.bridgeOrder = append(p.bridgeOrder, clientToken)
	p.bridgeEvictLocked()
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
// over maxBridgeEntries (LRU): the evicted entry's runs are FINISHed
// best-effort (bounded by the client's session-call timeout), then the
// entry is dropped. Caller holds bridgeMu.
func (p *Pool) bridgeEvictLocked() {
	for len(p.bridgeOrder) > maxBridgeEntries {
		oldest := p.bridgeOrder[0]
		if entry, ok := p.bridge[oldest]; ok {
			entry.runs.FinishAllRuns(context.Background())
		}
		delete(p.bridge, oldest)
		p.bridgeOrder = p.bridgeOrder[1:]
		p.logger.Debug("pool: bridge entry evicted (cache full)", "bridge_entries", len(p.bridge))
	}
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
		Limit:       float64(p.cfg.MaxMessagesPerDay),
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
	for i, tok := range p.toks {
		preCtx, cancel := context.WithTimeout(ctx, p.cfg.RequestTimeout)
		tok.runs.Prewarm(preCtx, agentIDs)
		cancel()
		p.logger.Debug("pool: prewarm done", "token", i+1, "agents", len(agentIDs))
	}
}

// maintainLoop ticks every maintainInterval: per token, rotate aged runs and
// refresh the session (advances queued sessions past pollAt). When
// IDLE_ROTATION_TIMEOUT is set, the pool pauses this activity after it has
// been idle past the timeout: one pass FINISHes all runs (so no
// rotation/session-refresh activity continues upstream) and every further
// pass is skipped until the next request — Acquire re-creates runs on
// demand.
func (p *Pool) maintainLoop(ctx context.Context) {
	defer p.wg.Done()
	ticker := time.NewTicker(maintainInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.maintainTick(ctx)
		}
	}
}

// maintainTick runs one maintenance pass: the idle handling (see
// maintainLoop), then the per-token rotate/refresh work. Split out of
// maintainLoop so tests can drive a pass without waiting for the
// minute-long ticker.
func (p *Pool) maintainTick(ctx context.Context) {
	if p.cfg.IdleRotationTimeout > 0 && p.idleFor() > p.cfg.IdleRotationTimeout {
		// Past the idle threshold. If this is the first idle pass, FINISH
		// every run so the token's rotation/refresh activity stops
		// upstream; sessions are left untouched. Later passes skip the
		// per-token work entirely while the pool stays idle.
		if !p.setIdleFinishedOnce() {
			return
		}
		for _, tok := range p.toks {
			tok.runs.FinishAllRuns(context.Background())
		}
		return
	}
	for i, tok := range p.toks {
		mCtx, cancel := context.WithTimeout(ctx, p.cfg.RequestTimeout)
		tok.runs.Maintain(mCtx)
		// Advance queued sessions only (GET poll — zero quota cost). Session
		// creation stays lazy on first request: a scheduled POST here would
		// burn one of the ~6 daily admissions every hour of uptime.
		if snap := tok.session.Snapshot(); snap.Status == "queued" {
			if _, err := tok.session.EnsureSession(mCtx); err != nil {
				p.logger.Debug("pool: maintain session not ready", "token", i+1, "err", err)
			}
		}
		cancel()
	}
	// Bridge sweep: drop entries idle past bridgeIdleEvict (runs FINISHed
	// best-effort), maintain the rest like the fixed tokens above.
	p.bridgeMaintain(ctx)
}

// bridgeMaintain sweeps the bridge cache: entries idle past bridgeIdleEvict
// are dropped (runs FINISHed best-effort); the rest get the per-token
// maintain work — rotate aged runs and advance queued sessions, bounded by
// the same RequestTimeout ctx as the fixed-token loop.
func (p *Pool) bridgeMaintain(ctx context.Context) {
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	now := time.Now()
	for token, entry := range p.bridge {
		if now.Sub(entry.lastUsed) > bridgeIdleEvict {
			entry.runs.FinishAllRuns(context.Background())
			delete(p.bridge, token)
			p.bridgeOrder = removeBridgeOrder(p.bridgeOrder, token)
			p.logger.Debug("pool: bridge entry evicted (idle)", "bridge_entries", len(p.bridge))
			continue
		}
		mCtx, cancel := context.WithTimeout(ctx, p.cfg.RequestTimeout)
		entry.runs.Maintain(mCtx)
		if snap := entry.session.Snapshot(); snap.Status == "queued" {
			if _, err := entry.session.EnsureSession(mCtx); err != nil {
				p.logger.Debug("pool: bridge maintain session not ready", "err", err)
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

// asBan extracts a BanError from err (nil when absent).
func asBan(err error) *upstream.BanError {
	var be *upstream.BanError
	if errors.As(err, &be) {
		return be
	}
	return nil
}

// bestRateLimit picks the rate-limit error with the longest retry
// window (the token that unblocks last bounds the wait).
func bestRateLimit(entries []*upstream.RateLimitError) *upstream.RateLimitError {
	best := entries[0]
	for _, e := range entries[1:] {
		if e.RetryAfter > best.RetryAfter {
			best = e
		}
	}
	return best
}
