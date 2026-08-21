// Package session implements the FreeBuff free-session lifecycle for a
// single token: create, poll, cache, invalidate, and end — with a
// single-flight refresh so concurrent callers share one upstream request.
//
// Semantics ported from proxy-freebuff lib/sessions.js and
// freebuff2api-quorinex free_session.go:
//   - active: ready until expiresAt-5s
//   - disabled: no instance id needed; proceed without one
//   - queued: waiting room; callers get WaitingRoomError until pollAt
//   - ended/superseded/none: transparently re-created
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"freebuff-proxy/internal/upstream"
)

const (
	// expiryMargin is subtracted from expiresAt before a session is
	// considered ready (mirrors the references' 5s safety margin).
	expiryMargin = 5 * time.Second
	// graceWindow is the 30-minute drain window (FREEBUFF_SESSION_GRACE_MS)
	// after expiresAt where in-flight agent chat completions still succeed.
	graceWindow = 30 * time.Minute
	// maxRefreshIterations bounds the create/poll status loop.
	maxRefreshIterations = 5
	// maxOuterIterations bounds EnsureSession's refresh attempts per call so
	// a pathological upstream (always-expired or never-advancing queue)
	// cannot spin forever.
	maxOuterIterations = 10
	// DefaultFallbackModel is the guaranteed-available model used when a
	// requested model is temporarily unavailable upstream, and the default
	// probe target for token tests / smoke: every account can use it, unlike
	// an alphabetical-first catalog pick.
	DefaultFallbackModel = "deepseek/deepseek-v4-flash"
	// asyncReAdmitTimeout bounds the background pre-emptive re-admit
	// (issue #99) so a hung upstream never leaks a goroutine.
	asyncReAdmitTimeout = time.Minute

	// Terminal-event reasons (T9): the standardized session/invalidation
	// cause vocabulary shared by every terminal session log line. The
	// poll/refresh drop paths map upstream statuses through tableReason;
	// InvalidateWithReason accepts these so callers can name the cause.
	reasonEnded      = "ended"
	reasonSuperseded = "superseded"
	reasonShutdown   = "shutdown"
	reasonModelLock  = "model_lock"
	reasonExpired    = "expired"
	reason409        = "409"
	reasonPoll       = "poll"
	reasonStore      = "store"

	// ReasonSuperseded is the T9 terminal-event reason for a session another
	// instance took over (session_superseded, endsTheSession:true); exported
	// so the chat recovery path feeds the re-admit storm detector (T10).
	ReasonSuperseded = reasonSuperseded

	// Re-admit storm detector (T10): more than stormThreshold terminal
	// session events within stormWindow is a session re-admit storm — each
	// invalidation is followed by a fresh admission that burns a daily
	// session slot, so the burst is surfaced once (one Info summary) and
	// re-armed only after a full quiet window passes.
	stormWindow    = 60 * time.Second
	stormThreshold = 3
)

// WaitingRoomError is returned when the session is queued and pollAt has not
// passed. Callers should surface it as 503 with Retry-After.
type WaitingRoomError struct {
	Position   int
	QueueDepth int
	RetryAfter time.Duration
}

func (e *WaitingRoomError) Error() string {
	return fmt.Sprintf("waiting room: position %d of %d, retry after %s", e.Position, e.QueueDepth, e.RetryAfter)
}

// Manager owns the cached session state for one token.
type Manager struct {
	client *upstream.Client

	// store, when non-nil, persists the cached session state across process
	// restarts (SESSION_PERSIST). key is the stable store key derived from
	// the client token (upstream.Client.TokenKey).
	store *Store
	key   string

	mu         sync.Mutex
	state      *cachedState
	refreshCh  chan struct{} // closed by the in-flight refresher when done
	refreshing bool
	// refreshErr retains the last refresh's error under mu so waiters parked
	// on that refresh surface it (after one state re-check) instead of each
	// becoming the next refresher and re-running the failing upstream create.
	// Cleared when a new refresh starts, so a later caller retries normally.
	refreshErr error

	// reAdmitLead (issue #99, SESSION_RE_ADMIT_LEAD default 60s): when the
	// cached active session has less than this much time left, EnsureSession
	// triggers a pre-emptive async re-admit (single-flight through the
	// existing refreshing machinery) and rides the old session; the next
	// request gets the new instance. 0 disables.
	reAdmitLead time.Duration
	// reAdmitExpiry (issue #132) is the expiresAt of the session the last
	// pre-emptive re-admit was triggered for. A failed re-admit must not be
	// re-triggered on every subsequent request in the lead window (each
	// trigger is an upstream session create, and the upstream refuses fresh
	// instances while the old is still authoritative — a 30-create storm
	// was observed). The guard resets naturally when a new session (with a
	// new expiresAt) lands. Guarded by mu.
	reAdmitExpiry time.Time
	// probeTTL (issue #60, SESSION_PROBE_CACHE_TTL default 15s) + lastAdmitted:
	// the last successful upstream session response is reused to skip a
	// redundant poll GET within the TTL.
	probeTTL     time.Duration
	lastAdmitted time.Time
	// unavailableTTL + modelUnavailable cache model_unavailable refusals per
	// model (issue #158); entry.until = min(next window opening, now+TTL).
	unavailableTTL   time.Duration
	modelUnavailable map[string]modelUnavailableEntry
	// savedQuota preserves the most recent non-nil quota map across
	// invalidation/re-admission cycles (issue #146).  When commit(nil)
	// drops the cached state, the quota map is stashed here; a later
	// commit(non-nil) without fresh quota restores it so the dashboard
	// quota table stays visible between quota-carrying responses.
	savedQuota map[string]upstream.ModelQuota
	// savedGlmPromo preserves the last glmPromo block across
	// invalidation/re-admission cycles, mirroring savedQuota (issue #178):
	// the GLM promo quota row must stay visible while the session is
	// between quota-carrying responses.
	savedGlmPromo string

	// adopt is the issue #97 CLI-session adoption mode (ADOPT_CLI_SESSION):
	// nil (default) = create sessions normally. When set, the manager adopts
	// the CLI's active instance and refuses to create a competing session
	// while the CLI process is alive.
	adopt *CLIAdoption

	// scarce tracks which models are scarce (issue #155): a scarce model
	// session is kept on Shutdown instead of DELETEing upstream so the slot
	// survives a restart via pollPersisted. Guarded by mu.
	scarce map[string]bool

	// now returns the current time; injectable in tests to drive the
	// re-admit storm detector deterministically. Defaults to time.Now.
	now func() time.Time

	// invalidationEvents is the rolling stormWindow of terminal session
	// events (timestamps + reason) feeding the re-admit storm detector
	// (T10); reAdmitTriggers records pre-emptive re-admit trigger times so
	// a storm summary can report how many daily slots the burst burned;
	// lastStormAt suppresses repeat summaries until a quiet window passes.
	invalidationEvents []invalidationEvent
	reAdmitTriggers    []time.Time
	lastStormAt        time.Time

	// modelLocked tallies model-lock release events keyed by from → to
	// model pair (issue #160): every model_locked admission releases the
	// old slot and re-admits with the requested model, so the pair counts
	// the model-switch cost. Guarded by modelLockedMu (refresh holds no
	// other lock while recording).
	modelLockedMu sync.Mutex
	modelLocked   map[string]map[string]int64
}

// invalidationEvent is one terminal session event in the re-admit storm
// window (T10): when the cached session was dropped and why.
type invalidationEvent struct {
	at     time.Time
	reason string
}

type cachedState struct {
	status             string
	instanceID         string
	model              string
	expiresAt          time.Time
	gracePeriodEndsAt  time.Time
	position           int
	queueDepth         int
	pollAt             time.Time
	countryCode        string
	countryBlockReason string
	// ipPrivacySignals / activeUsersForIP / limit are surfaced for the
	// passive ban-risk view (#64): the upstream's own egress classification
	// and the ip_capped admission pressure. Kept out of the persisted store
	// (ephemeral diagnostics; zero after a restart until the next refresh).
	ipPrivacySignals []string
	activeUsersForIP int
	limit            float64
	// quotaByModel is the live per-model session quota from the last
	// admission/poll that carried rateLimitsByModel (key = model id);
	// nil until such a response is seen.
	quotaByModel map[string]upstream.ModelQuota
	// glmPromo carries the raw upstream glmPromo block ({dailySessions,
	// endsAt}) from the last admission/poll that included it (issue #178);
	// "" until such a response is seen.
	glmPromo string
	// standing is the upstream account standing block (issue #96); nil until
	// an admission/poll that carried it.
	standing *upstream.SessionStanding
}

// NewManager builds a session manager for the given upstream client.
func NewManager(client *upstream.Client) *Manager {
	return NewManagerWithStore(client, nil)
}

// NewManagerWithStore builds a session manager that also persists its cached
// state through store (nil disables persistence).
func NewManagerWithStore(client *upstream.Client, store *Store) *Manager {
	m := &Manager{client: client, store: store, now: time.Now}
	if client != nil {
		m.key = client.TokenKey()
	}
	return m
}

// sessionUsable reports whether the cached state can serve a chat right now:
// an active session until expiresAt-5s (the reference safety margin), or any
// state that still holds a live instance id within the 30-minute grace drain
// after expiry (gap #13, FREEBUFF_SESSION_GRACE_MS: within grace the row
// stays alive and chat passes — reference/freebuff freebuff-session.ts). The
// instance-id test guards the grace extension: an ended row whose instance id
// is gone cannot be ridden, and an expired active cache is only reusable
// while its slot survives upstream.
func sessionUsable(s *cachedState) bool {
	if s == nil || s.instanceID == "" {
		return false
	}
	if s.status == "active" && time.Now().Before(s.expiresAt.Add(-expiryMargin)) {
		return true
	}
	graceEnd := graceEndsAt(s)
	return !graceEnd.IsZero() && time.Now().Before(graceEnd)
}

// commit replaces the cached state and mirrors it into the store (when
// configured). Caller must hold m.mu.
//
// A nil cs removes the store entry conditionally on the instance id being
// dropped: the entry is only deleted while it still belongs to the session
// being invalidated, so a stale commit cannot clobber a persisted slot that
// was replaced concurrently (e.g. a restart re-adopting a different one).
func (m *Manager) commit(cs *cachedState) {
	oldInstance := ""
	if m.state != nil {
		oldInstance = m.state.instanceID
		// Stash the quota map before dropping state so it survives
		// invalidation (commit(nil)) and later re-admission (issue #146).
		if m.state.quotaByModel != nil {
			m.savedQuota = m.state.quotaByModel
		}
		// Stash the glmPromo block the same way (issue #178): it survives
		// invalidation so the GLM promo row stays on the dashboard between
		// quota-carrying responses.
		if m.state.glmPromo != "" {
			m.savedGlmPromo = m.state.glmPromo
		}
	}
	// Restore the previously-seen quota map when the new state omits
	// rateLimitsByModel (the upstream intermittently drops the field on
	// re-admission or compact polls — issue #146).  This keeps the
	// dashboard quota table visible between quota-carrying responses.
	if cs != nil && cs.quotaByModel == nil && m.savedQuota != nil {
		cs.quotaByModel = m.savedQuota
	}
	// Restore the previously-seen glmPromo block when the new state omits
	// it (issue #178), mirroring the quota-map restore above.
	if cs != nil && cs.glmPromo == "" && m.savedGlmPromo != "" {
		cs.glmPromo = m.savedGlmPromo
	}
	m.state = cs
	if m.store != nil && m.key != "" {
		if cs == nil {
			m.store.Remove(m.key, oldInstance)
		} else {
			m.store.Save(m.key, cs)
		}
	}
}

// EnsureSession returns the session instance id for the default model, or ""
// when the upstream session is disabled.
func (m *Manager) EnsureSession(ctx context.Context) (string, error) {
	return m.EnsureSessionForModel(ctx, "")
}

// EnsureSessionForModel returns the session instance id bound to the requested
// model. If the session is currently active on a different model, it automatically
// switches models by releasing the previous slot.
func (m *Manager) EnsureSessionForModel(ctx context.Context, model string) (string, error) {
	for attempts := 0; attempts < maxOuterIterations; attempts++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		m.mu.Lock()
		s := m.state
		if s != nil && !m.refreshing {
			switch s.status {
			case "active", "ended":
				// Fast path: reuse the cached instance while it is usable —
				// an active session until expiresAt-5s, or (gap #13) a
				// session whose instance id survives the 30-minute grace
				// drain (FREEBUFF_SESSION_GRACE_MS: within grace the row
				// stays alive and chat passes).
				if (model == "" || s.model == "" || s.model == model) && sessionUsable(s) {
					instance := s.instanceID
					// Issue #99/#163: pre-emptive re-admit — pre-expiry
					// (within reAdmitLead of expiresAt-5s) or while the
					// session is ridden through its grace drain (#163: a
					// long stream crossing expiry must hand over to a
					// fresh session without paying a synchronous
					// admission at grace end). The refresh runs
					// preemptively on a background context so a refusal
					// or queue keeps the cache and rides on.
					if m.reAdmitLead > 0 && !m.reAdmitExpiry.Equal(s.expiresAt) &&
						reAdmitDue(s, m.reAdmitLead, time.Now()) {
						// Issue #132: one attempt per expiry window. The
						// upstream refuses a fresh create while the old
						// instance is still authoritative, so a failed
						// re-admit must ride the old session to expiry
						// instead of re-triggering on every request (each
						// trigger burns a session slot).
						window := "lead"
						if !time.Now().Before(s.expiresAt.Add(-expiryMargin)) {
							window = "grace"
						}
						m.reAdmitExpiry = s.expiresAt
						m.refreshing = true
						m.refreshErr = nil
						refreshCh := make(chan struct{})
						m.refreshCh = refreshCh
						m.mu.Unlock()
						go m.asyncReAdmit(model)
						m.recordReAdmitTrigger()
						slog.Debug("session: pre-emptive re-admit triggered", "instance_id", instance, "model", s.model, "window", window)
						return instance, nil
					}
					m.mu.Unlock()
					slog.Debug("session reused", "instance_id", instance, "model", s.model, "expires_at", s.expiresAt.Format(time.RFC3339))
					return instance, nil
				}
				// Usability exhausted (past grace) or model mismatch — fall
				// through to refresh.
			case "disabled":
				m.mu.Unlock()
				return "", nil
			case "queued":
				if now := time.Now(); now.Before(s.pollAt) {
					wa := WaitingRoomError{
						Position:   s.position,
						QueueDepth: s.queueDepth,
						RetryAfter: s.pollAt.Sub(now),
					}
					m.mu.Unlock()
					return "", &wa
				}
				// pollAt passed — fall through to refresh and advance.
			}
		}
		if m.refreshing {
			// Another caller is the refresher: park on its completion signal.
			refreshCh := m.refreshCh
			m.mu.Unlock()
			select {
			case <-refreshCh:
				// The refresh finished. If it failed, surface its retained
				// error to every waiter (after one state re-check) instead of
				// letting each waiter become the next refresher and re-run
				// the failing upstream create (N callers → N serial POSTs).
				m.mu.Lock()
				err := m.refreshErr
				m.mu.Unlock()
				if err != nil {
					if (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) && ctx.Err() == nil {
						// Leader goroutine was canceled or timed out, but this waiter's
						// context is still active. Loop back to become a candidate leader
						// and refresh rather than propagating the aborted leader's error.
						continue
					}
					m.mu.Lock()
					s = m.state
					m.mu.Unlock()
					// One state re-check: the failed refresh may still have
					// advanced the queue (e.g. to queued with a future
					// pollAt) — honor that before surfacing the error.
					if s != nil && s.status == "queued" && time.Now().Before(s.pollAt) {
						return "", &WaitingRoomError{
							Position:   s.position,
							QueueDepth: s.queueDepth,
							RetryAfter: time.Until(s.pollAt),
						}
					}
					// Issue #99: a failed pre-emptive re-admit leaves the old
					// session authoritative — ride it rather than erroring a
					// request that could still be served (through the grace
					// drain, gap #13).
					if s != nil && sessionUsable(s) {
						return s.instanceID, nil
					}
					return "", err
				}
				continue // refresh succeeded; loop re-evaluates cached state
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}

		// We are the refresher. Run the create/poll loop outside the lock and
		// clear any previously retained refresh error.
		m.refreshing = true
		m.refreshErr = nil
		refreshCh := make(chan struct{})
		m.refreshCh = refreshCh
		m.mu.Unlock()

		err := m.refresh(ctx, model, false)
		m.mu.Lock()
		m.refreshing = false
		if err != nil {
			m.refreshErr = err
		}
		close(m.refreshCh)
		m.refreshCh = nil
		m.mu.Unlock()
		if err != nil {
			return "", err
		}

		// Freshly refreshed: trust the new state.
		m.mu.Lock()
		s = m.state
		m.mu.Unlock()
		if s == nil {
			continue // ended/superseded cleared it; refresh again
		}
		switch s.status {
		case "active":
			return s.instanceID, nil
		case "disabled":
			return "", nil
		case "queued":
			if now := time.Now(); now.Before(s.pollAt) {
				return "", &WaitingRoomError{
					Position:   s.position,
					QueueDepth: s.queueDepth,
					RetryAfter: s.pollAt.Sub(now),
				}
			}
		}
	}
	return "", errors.New("session: not ready after repeated refreshes")
}

// SessionSnapshot is a lock-free best-effort view of the cached session
// state, for healthz-style reporting (pool.TokenSnapshot).
type SessionSnapshot struct {
	Status        string
	InstanceID    string
	Model         string
	QueuePosition int
	QueueDepth    int
	// Refreshing reports whether a session admission or pre-emptive re-admit
	// is currently in flight for this manager.
	Refreshing         bool
	CountryCode        string
	CountryBlockReason string
	// ActiveUsersForIP is the last known distinct-user count on the token's
	// egress IP (upstream activeUsersForIp); 0 when absent.
	ActiveUsersForIP int
	// IPPrivacySignals is the upstream's own egress-IP classification
	// (e.g. vpn/proxy/tor/hosting); Limit is the ip_capped ceiling. Both
	// feed the passive ban-risk view (#64); empty/0 when absent.
	IPPrivacySignals []string
	Limit            float64
	ExpiresAt        time.Time
	// GracePeriodEndsAt is when the 30-minute drain window after ExpiresAt
	// closes (previously computed but never surfaced).
	GracePeriodEndsAt time.Time
	// QuotaByModel carries the live per-model session quotas (key = model id).
	// Entitlement is a top-level per-token view; it stays empty because the
	// upstream wire nests entitlement inside each rate-limit entry.
	QuotaByModel map[string]QuotaSnapshot
	Entitlement  map[string]float64
	// GlmPromo carries the raw upstream glmPromo block ({dailySessions,
	// endsAt}) from the last admission/poll (issue #178); "" when absent.
	// Kept as a string so callers render the shape without the upstream
	// adding fields.
	GlmPromo string
	// Standing is the upstream account standing block (issue #96); nil until
	// an admission/poll that carried it.
	Standing *upstream.SessionStanding
}

// QuotaSnapshot is one model's live session quota for healthz/metrics
// reporting (pool.TokenSnapshot). Mirrors upstream.ModelQuota.
type QuotaSnapshot struct {
	Model       string
	Limit       float64
	RecentCount float64
	ResetAt     time.Time
	Period      string
	Entitlement map[string]float64
}

// Snapshot returns a best-effort view of the cached session state. All
// fields may be zero when no session has been created yet. Added for
func (m *Manager) Snapshot() SessionSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil {
		var quota map[string]QuotaSnapshot
		if len(m.savedQuota) > 0 {
			quota = make(map[string]QuotaSnapshot, len(m.savedQuota))
			for modelID, q := range m.savedQuota {
				quota[modelID] = QuotaSnapshot{
					Model:       q.Model,
					Limit:       q.Limit,
					RecentCount: q.RecentCount,
					ResetAt:     q.ResetAt,
					Period:      q.Period,
					Entitlement: q.Entitlement,
				}
			}
		}
		return SessionSnapshot{
			Refreshing:   m.refreshing,
			QuotaByModel: quota,
			GlmPromo:     m.savedGlmPromo,
		}
	}
	quota := make(map[string]QuotaSnapshot, len(m.state.quotaByModel))
	for modelID, q := range m.state.quotaByModel {
		quota[modelID] = QuotaSnapshot{
			Model:       q.Model,
			Limit:       q.Limit,
			RecentCount: q.RecentCount,
			ResetAt:     q.ResetAt,
			Period:      q.Period,
			Entitlement: q.Entitlement,
		}
	}
	var sigs []string
	if len(m.state.ipPrivacySignals) > 0 {
		sigs = make([]string, len(m.state.ipPrivacySignals))
		copy(sigs, m.state.ipPrivacySignals)
	}
	return SessionSnapshot{
		Status:             m.state.status,
		InstanceID:         m.state.instanceID,
		Model:              m.state.model,
		QueuePosition:      m.state.position,
		QueueDepth:         m.state.queueDepth,
		Refreshing:         m.refreshing,
		CountryCode:        m.state.countryCode,
		CountryBlockReason: m.state.countryBlockReason,
		ActiveUsersForIP:   m.state.activeUsersForIP,
		IPPrivacySignals:   sigs,
		Limit:              m.state.limit,
		ExpiresAt:          m.state.expiresAt,
		GracePeriodEndsAt:  m.state.gracePeriodEndsAt,
		QuotaByModel:       quota,
		GlmPromo:           m.state.glmPromo,
		Standing:           m.state.standing,
	}
}

// Usable reports whether the session can serve a chat right now:
// an active session until expiresAt-5s (the reference safety margin), or
// any session that holds an instance id within its grace drain window.
func (s SessionSnapshot) Usable() bool {
	if s.InstanceID == "" {
		return false
	}
	if s.Status == "active" && !s.ExpiresAt.IsZero() && time.Now().Before(s.ExpiresAt.Add(-expiryMargin)) {
		return true
	}
	return !s.GracePeriodEndsAt.IsZero() && time.Now().Before(s.GracePeriodEndsAt)
}

// MatchesModel reports whether the session matches model (empty model matches any).
func (s SessionSnapshot) MatchesModel(model string) bool {
	return model == "" || s.Model == "" || s.Model == model
}

// HasGlmEntitlement reports whether the session snapshot holds an active
// referral quota or valid unexpired promo for z-ai/glm-5.2 (issue #183).
func (s SessionSnapshot) HasGlmEntitlement() bool {
	if q, ok := s.QuotaByModel["z-ai/glm-5.2"]; ok && q.Limit > 0 {
		if q.RecentCount < q.Limit {
			return true
		}
		if !q.ResetAt.IsZero() && q.ResetAt.Before(time.Now()) {
			return true
		}
	}
	if s.GlmPromo != "" {
		var gp struct {
			DailySessions float64 `json:"dailySessions"`
			EndsAt        string  `json:"endsAt"`
		}
		if err := json.Unmarshal([]byte(s.GlmPromo), &gp); err == nil && gp.DailySessions > 0 {
			if gp.EndsAt == "" {
				return true
			}
			if ends, err := time.Parse(time.RFC3339, gp.EndsAt); err == nil {
				if ends.After(time.Now()) {
					return true
				}
			} else {
				return true
			}
		}
	}
	if s.Entitlement != nil {
		if s.Entitlement["glm"] > 0 || s.Entitlement["z-ai/glm-5.2"] > 0 {
			return true
		}
	}
	return false
}

// HasGlmEntitlement reports whether the manager currently holds verified
// referral entitlement for z-ai/glm-5.2 from active or saved quota/promo (issue #183).
func (m *Manager) HasGlmEntitlement() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hasGlmEntitlementLocked()
}

func (m *Manager) hasGlmEntitlementLocked() bool {
	quota := m.savedQuota
	promo := m.savedGlmPromo
	if m.state != nil {
		if m.state.quotaByModel != nil {
			quota = m.state.quotaByModel
		}
		if m.state.glmPromo != "" {
			promo = m.state.glmPromo
		}
	}
	if q, ok := quota["z-ai/glm-5.2"]; ok && q.Limit > 0 {
		if q.RecentCount < q.Limit {
			return true
		}
		if !q.ResetAt.IsZero() && q.ResetAt.Before(time.Now()) {
			return true
		}
	}
	if promo != "" {
		var gp struct {
			DailySessions float64 `json:"dailySessions"`
			EndsAt        string  `json:"endsAt"`
		}
		if err := json.Unmarshal([]byte(promo), &gp); err == nil && gp.DailySessions > 0 {
			if gp.EndsAt == "" {
				return true
			}
			if ends, err := time.Parse(time.RFC3339, gp.EndsAt); err == nil {
				if ends.After(time.Now()) {
					return true
				}
			} else {
				return true
			}
		}
	}
	return false
}

// UpdateQuotaFromProbe records the quota map and glmPromo block from a zero-cost
// session probe into the manager's saved state (issue #183).
func (m *Manager) UpdateQuotaFromProbe(st *upstream.SessionState) {
	if st == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if st.GlmPromo != "" {
		m.savedGlmPromo = st.GlmPromo
		if m.state != nil {
			m.state.glmPromo = st.GlmPromo
		}
	}
	if len(st.RateLimitsByModel) > 0 {
		m.savedQuota = st.RateLimitsByModel
		if m.state != nil {
			m.state.quotaByModel = st.RateLimitsByModel
		}
	}
}

// Invalidate drops the cached session so the next EnsureSession re-creates
// it. Used when a chat request reports a session-level error. The
// invalidation is recorded with the canonical 409 reason (the session-invalid
// chat family); callers that can name a more specific cause use
// InvalidateWithReason.
func (m *Manager) Invalidate() {
	m.InvalidateWithReason(reason409, 0)
}

// InvalidateWithReason drops the cached session, recording WHY (T9/T10) and
// feeding the re-admit storm detector. reason is a terminal-event cause from
// the vocabulary (ended|superseded|shutdown|model_lock|expired|409|poll|
// store); status is the triggering HTTP status when known (e.g. 409 from the
// chat/poll error), 0 when unknown — a 0 status is omitted from the log.
func (m *Manager) InvalidateWithReason(reason string, status int) {
	m.mu.Lock()
	instanceID := ""
	if m.state != nil {
		instanceID = m.state.instanceID
	}
	m.commit(nil)
	m.mu.Unlock()
	m.recordInvalidation(reason)
	if status > 0 {
		slog.Debug("session invalidated", "instance_id", instanceID, "reason", reason, "status", status)
		return
	}
	slog.Debug("session invalidated", "instance_id", instanceID, "reason", reason)
}

// InvalidateInstance drops the cached session only when its instance id
// matches instanceID (issue #132): after a pre-emptive re-admit lands a new
// instance, a chat that was still riding the OLD (superseded) instance
// failing must not invalidate the NEW cached session — that would force the
// next request to re-create and restart the churn. A mismatch leaves the
// cache untouched.
func (m *Manager) InvalidateInstance(instanceID string) {
	m.InvalidateInstanceWithReason(instanceID, "instance_invalidated", 0)
}

// InvalidateInstanceWithReason is the reason-aware form of InvalidateInstance
// (#159): additionally records WHY (T9/T10) and the triggering HTTP status.
func (m *Manager) InvalidateInstanceWithReason(instanceID, reason string, status int) {
	if instanceID == "" {
		return
	}
	m.mu.Lock()
	if m.state == nil || m.state.instanceID != instanceID {
		m.mu.Unlock()
		return
	}
	m.commit(nil)
	m.mu.Unlock()
	m.recordInvalidation(reason)
	if status > 0 {
		slog.Debug("session invalidated", "instance_id", instanceID, "reason", reason, "status", status)
		return
	}
	slog.Debug("session invalidated", "instance_id", instanceID, "reason", reason)
}

// ClearQueued drops the cached session only when it is in the queued
// (waiting-room) state, and reports whether it did (issue #100: the
// queue-time model fallback clears queued caches so a fallback-model
// acquire can create a fresh session instead of re-surfacing the same
// waiting room). Active/disabled states are untouched.
func (m *Manager) ClearQueued() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != nil && m.state.status == "queued" {
		m.commit(nil)
		return true
	}
	return false
}

// SetSessionStateForTest sets cached session state for tests across packages.
func (m *Manager) SetSessionStateForTest(status, instanceID, model string, expiresAt, graceEndsAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commit(&cachedState{
		status:            status,
		instanceID:        instanceID,
		model:             model,
		expiresAt:         expiresAt,
		gracePeriodEndsAt: graceEndsAt,
	})
}
