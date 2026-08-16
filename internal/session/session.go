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
	// probe target for token tests / smoke: every account (including
	// limited tier) can use it, unlike an alphabetical-first catalog pick.
	DefaultFallbackModel = "deepseek/deepseek-v4-flash"
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
	accessTier         string
	countryCode        string
	countryBlockReason string
	// quotaByModel is the live per-model session quota from the last
	// admission/poll that carried rateLimitsByModel (key = model id);
	// nil until such a response is seen.
	quotaByModel map[string]upstream.ModelQuota
}

// NewManager builds a session manager for the given upstream client.
func NewManager(client *upstream.Client) *Manager {
	return NewManagerWithStore(client, nil)
}

// NewManagerWithStore builds a session manager that also persists its cached
// state through store (nil disables persistence).
func NewManagerWithStore(client *upstream.Client, store *Store) *Manager {
	m := &Manager{client: client, store: store}
	if client != nil {
		m.key = client.TokenKey()
	}
	return m
}

// commit replaces the cached state and mirrors it into the store (when
// configured). Caller must hold m.mu.
func (m *Manager) commit(cs *cachedState) {
	m.state = cs
	if m.store != nil && m.key != "" {
		m.store.Save(m.key, cs)
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
			case "active":
				if (model == "" || s.model == "" || s.model == model) && time.Now().Before(s.expiresAt.Add(-expiryMargin)) {
					instance := s.instanceID
					m.mu.Unlock()
					slog.Debug("session reused", "instance_id", instance, "model", s.model, "expires_at", s.expiresAt.Format(time.RFC3339))
					return instance, nil
				}
				// Freshness exceeded or model mismatch — fall through to refresh.
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

		err := m.refresh(ctx, model)
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

// statusError maps an upstream session status to the typed error callers
// use for recovery (token cooldown, region surfacing). st supplies the
// fields carried by the error; non-error statuses return nil. Shared by
// refresh and Heartbeat so both map the same way.
func statusError(status string, st *upstream.SessionState) error {
	switch status {
	case "banned":
		return &upstream.BanError{ResumesAt: st.ResumesAt, Body: st.Message}
	case "country_blocked":
		return &upstream.CountryBlockedError{
			CountryCode:        st.CountryCode,
			CountryBlockReason: st.CountryBlockReason,
			IpPrivacySignals:   st.IpPrivacySignals,
		}
	case "rate_limited", "ip_capped", "spend_limited":
		retryAfter := time.Duration(st.RetryAfterMs) * time.Millisecond
		if retryAfter <= 0 {
			retryAfter = time.Minute
		}
		return &upstream.RateLimitError{
			Status:      status,
			RetryAfter:  retryAfter,
			ResetAt:     st.ResetAt,
			Limit:       st.Limit,
			RecentCount: st.RecentCount,
			Body:        st.Message,
		}
	}
	return nil
}

// refresh runs the create/poll status loop, updating cached state, until the
// session is active, disabled, or the iteration budget is exhausted.
func (m *Manager) refresh(ctx context.Context, requestedModel string) error {
	targetModel := requestedModel
	for i := 0; i < maxRefreshIterations; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		m.mu.Lock()
		cached := m.state
		m.mu.Unlock()

		var (
			st  *upstream.SessionState
			err error
		)
		if cached != nil && cached.status == "queued" && cached.instanceID != "" {
			st, err = m.client.GetSession(ctx, cached.instanceID)
		} else {
			// Resume a persisted session (restart reuse) before creating a
			// new one: a persisted active slot that is still alive upstream
			// is adopted instead of burning a fresh session quota.
			st = m.pollPersisted(ctx)
			if st == nil {
				st, err = m.client.CreateSessionForModel(ctx, targetModel)
			}
		}
		if err != nil {
			return err
		}

		status := st.Status
		switch status {
		case "active":
			model := st.Model
			if model == "" {
				model = targetModel
			}
			m.mu.Lock()
			m.commit(&cachedState{
				status:             "active",
				instanceID:         st.InstanceID,
				model:              model,
				expiresAt:          st.ExpiresAt,
				gracePeriodEndsAt:  st.ExpiresAt.Add(graceWindow),
				accessTier:         st.AccessTier,
				countryCode:        st.CountryCode,
				countryBlockReason: st.CountryBlockReason,
				quotaByModel:       st.RateLimitsByModel,
			})
			m.mu.Unlock()
			slog.Debug("session created", "status", "active", "instance_id", st.InstanceID,
				"model", model, "expires_at", st.ExpiresAt.Format(time.RFC3339))
			return nil
		case "disabled":
			m.mu.Lock()
			m.commit(&cachedState{status: "disabled"})
			m.mu.Unlock()
			slog.Debug("session created", "status", "disabled", "instance_id", "")
			return nil
		case "queued":
			pollAt := st.PollAt
			if pollAt.IsZero() {
				wait := time.Duration(st.EstimatedWaitMs) * time.Millisecond
				if wait < time.Second {
					wait = time.Second
				}
				if wait > 5*time.Second {
					wait = 5 * time.Second
				}
				pollAt = time.Now().Add(wait)
			}
			model := st.Model
			if model == "" {
				model = targetModel
			}
			m.mu.Lock()
			m.commit(&cachedState{
				status:     "queued",
				instanceID: st.InstanceID,
				model:      model,
				position:   st.Position,
				queueDepth: st.QueueDepth,
				pollAt:     pollAt,
			})
			m.mu.Unlock()
			slog.Debug("session queued", "instance_id", st.InstanceID, "model", model,
				"position", st.Position, "queue_depth", st.QueueDepth, "poll_at", pollAt.Format(time.RFC3339))
			return nil
		case "ended", "superseded", "none":
			m.mu.Lock()
			m.commit(nil)
			m.mu.Unlock()
			slog.Debug("session recreated", "reason", status, "instance_id", st.InstanceID)
		case "banned", "country_blocked", "rate_limited", "ip_capped", "spend_limited":
			return statusError(status, st)
		case "model_locked":
			// Previous session is locked to a different model.
			// Release the old slot and retry with the desired model.
			m.mu.Lock()
			oldInstance := ""
			if m.state != nil {
				oldInstance = m.state.instanceID
			}
			m.commit(nil)
			m.mu.Unlock()
			_ = m.client.EndSession(ctx, oldInstance)
			slog.Debug("session released on model lock, retrying", "current", st.CurrentModel, "target", targetModel)
		case "model_unavailable":
			// Requested model is not available; fall back to default model.
			slog.Warn("session: model unavailable upstream, falling back to default", "requested", targetModel, "fallback", DefaultFallbackModel)
			targetModel = DefaultFallbackModel
			m.mu.Lock()
			m.commit(nil)
			m.mu.Unlock()
		default:
			return fmt.Errorf("session: unknown upstream status %q", status)
		}
	}
	return errors.New("session: refresh iteration budget exhausted")
}

// SessionSnapshot is a lock-free best-effort view of the cached session
// state, for healthz-style reporting (pool.TokenSnapshot).
type SessionSnapshot struct {
	Status        string
	InstanceID    string
	Model         string
	QueuePosition int
	QueueDepth    int
	TierAccess    string
	// CountryCode is the admitted session's country ("" when absent).
	CountryCode        string
	TierCountry        string
	CountryBlockReason string
	ExpiresAt          time.Time
	// GracePeriodEndsAt is when the 30-minute drain window after ExpiresAt
	// closes (previously computed but never surfaced).
	GracePeriodEndsAt time.Time
	// QuotaByModel carries the live per-model session quotas (key = model id).
	// Entitlement is a top-level per-token view; it stays empty because the
	// upstream wire nests entitlement inside each rate-limit entry.
	QuotaByModel map[string]QuotaSnapshot
	Entitlement  map[string]float64
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
// internal/pool snapshotting; no upstream calls are made.
func (m *Manager) Snapshot() SessionSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil {
		return SessionSnapshot{}
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
	return SessionSnapshot{
		Status:             m.state.status,
		InstanceID:         m.state.instanceID,
		Model:              m.state.model,
		QueuePosition:      m.state.position,
		QueueDepth:         m.state.queueDepth,
		TierAccess:         m.state.accessTier,
		CountryCode:        m.state.countryCode,
		TierCountry:        m.state.countryCode,
		CountryBlockReason: m.state.countryBlockReason,
		ExpiresAt:          m.state.expiresAt,
		GracePeriodEndsAt:  m.state.gracePeriodEndsAt,
		QuotaByModel:       quota,
	}
}

// Invalidate drops the cached session so the next EnsureSession re-creates
// it. Used when a chat request reports a session-level error.
func (m *Manager) Invalidate() {
	m.mu.Lock()
	instanceID := ""
	if m.state != nil {
		instanceID = m.state.instanceID
	}
	m.commit(nil)
	m.mu.Unlock()
	slog.Debug("session invalidated", "instance_id", instanceID)
}

// EndSession deletes the upstream session (if any) and clears the cache.
func (m *Manager) EndSession(ctx context.Context) error {
	m.mu.Lock()
	instanceID := ""
	if s := m.state; s != nil {
		instanceID = s.instanceID
	}
	m.commit(nil)
	m.mu.Unlock()

	if instanceID == "" {
		return nil
	}
	slog.Debug("session ended", "instance_id", instanceID)
	if err := m.client.EndSession(ctx, instanceID); err != nil && !errors.Is(err, upstream.ErrSessionInvalid) {
		return err
	}
	return nil
}

// Shutdown handles session teardown at process shutdown. When persistence is
// disabled it behaves exactly like EndSession (DELETE upstream). When
// persistence is enabled it leaves the upstream session alive — so a later
// restart can resume it — and ensures the current state is flushed to the
// store. Runs are FINISHed separately by the run manager.
func (m *Manager) Shutdown(ctx context.Context) error {
	if m.store == nil {
		return m.EndSession(ctx)
	}
	m.mu.Lock()
	if m.state != nil && m.state.status == "active" && m.state.instanceID != "" {
		m.store.Save(m.key, m.state)
	}
	m.mu.Unlock()
	slog.Debug("session kept for restart", "instance_id", func() string {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.state != nil {
			return m.state.instanceID
		}
		return ""
	}())
	return nil
}

// pollPersisted attempts to resume a persisted session before a fresh create
// (restart reuse). It returns a non-nil SessionState when the persisted slot
// is still active upstream (adopted), and nil otherwise — either there is no
// persisted slot, it is expired, or it is dead upstream (in which case it is
// removed from the store). Transport errors are returned so the caller
// surfaces them like a failed create.
func (m *Manager) pollPersisted(ctx context.Context) *upstream.SessionState {
	if m.store == nil || m.key == "" {
		return nil
	}
	cs := m.store.Load(m.key)
	if cs == nil || cs.status != "active" || cs.instanceID == "" {
		return nil
	}
	// Only resume a slot that is still genuinely active (with the 5s safety
	// margin). An expired-but-in-grace slot is draining; resume it is not
	// worth the risk of admitting new work onto a dying session.
	if cs.expiresAt.IsZero() || !time.Now().Before(cs.expiresAt.Add(-expiryMargin)) {
		m.store.Remove(m.key)
		return nil
	}

	st, err := m.client.GetSession(ctx, cs.instanceID)
	if err != nil {
		slog.Debug("session resume poll failed", "instance_id", cs.instanceID, "err", err)
		return nil
	}
	switch st.Status {
	case "active":
		slog.Debug("session resumed from store", "instance_id", st.InstanceID, "expires_at", st.ExpiresAt.Format(time.RFC3339))
		return st
	case "ended", "superseded", "none", "banned":
		m.store.Remove(m.key)
		return nil
	default:
		// queued / country_blocked / etc. — not resumable as an active slot.
		return nil
	}
}

// Heartbeat sends a periodic compact GET with x-freebuff-heartbeat: 1 for active sessions,
// keeping last_seen_at fresh on the server so the session concurrency slot is maintained.
func (m *Manager) Heartbeat(ctx context.Context) error {
	m.mu.Lock()
	if m.state == nil || m.state.status != "active" || m.state.instanceID == "" {
		m.mu.Unlock()
		return nil
	}
	instanceID := m.state.instanceID
	m.mu.Unlock()

	st, err := m.client.GetSessionWithOpts(ctx, instanceID, true, true)
	if err != nil {
		return err
	}
	if serr := statusError(st.Status, st); serr != nil {
		// A banned session is dead until the account unban: drop the cached
		// admission (cooldown) so the token re-admits only after the pool's
		// ban window, instead of heartbeat-ing a stale slot.
		if st.Status == "banned" {
			m.mu.Lock()
			if m.state != nil && m.state.instanceID == instanceID {
				m.commit(nil)
				slog.Debug("session dropped during heartbeat", "reason", st.Status, "instance_id", instanceID)
			}
			m.mu.Unlock()
		}
		return serr
	}
	if st.Status == "ended" || st.Status == "superseded" || st.Status == "none" {
		m.mu.Lock()
		if m.state != nil && m.state.instanceID == instanceID {
			m.commit(nil)
			slog.Debug("session ended during heartbeat", "reason", st.Status, "instance_id", instanceID)
		}
		m.mu.Unlock()
	}
	return nil
}
