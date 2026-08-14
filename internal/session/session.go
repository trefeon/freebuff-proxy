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
	maxOuterIterations = 5
	// defaultFallbackModel is the guaranteed-available model used when a
	// requested model is temporarily unavailable upstream.
	defaultFallbackModel = "deepseek/deepseek-v4-flash"
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

	mu         sync.Mutex
	state      *cachedState
	refreshCh  chan struct{} // closed by the in-flight refresher when done
	refreshing bool
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
}

// NewManager builds a session manager for the given upstream client.
func NewManager(client *upstream.Client) *Manager {
	return &Manager{client: client}
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
				if time.Now().Before(s.expiresAt.Add(-expiryMargin)) {
					instance := s.instanceID
					m.mu.Unlock()
					slog.Debug("session reused", "instance_id", instance, "model", s.model, "expires_at", s.expiresAt.Format(time.RFC3339))
					return instance, nil
				}
				// Freshness exceeded — fall through to refresh.
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
		singleFlight := m.refreshing
		var refreshCh chan struct{}
		if singleFlight {
			refreshCh = m.refreshCh
		} else {
			m.refreshing = true
			refreshCh = make(chan struct{})
			m.refreshCh = refreshCh
		}
		m.mu.Unlock()

		if singleFlight {
			select {
			case <-refreshCh:
				continue // loop re-evaluates cached state
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}

		// We are the refresher. Run the loop outside the lock.
		err := m.refresh(ctx, model)
		m.mu.Lock()
		m.refreshing = false
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
			st, err = m.client.CreateSessionForModel(ctx, targetModel)
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
			m.state = &cachedState{
				status:             "active",
				instanceID:         st.InstanceID,
				model:              model,
				expiresAt:          st.ExpiresAt,
				gracePeriodEndsAt:  st.ExpiresAt.Add(graceWindow),
				accessTier:         st.AccessTier,
				countryCode:        st.CountryCode,
				countryBlockReason: st.CountryBlockReason,
			}
			m.mu.Unlock()
			slog.Debug("session created", "status", "active", "instance_id", st.InstanceID,
				"model", model, "expires_at", st.ExpiresAt.Format(time.RFC3339))
			return nil
		case "disabled":
			m.mu.Lock()
			m.state = &cachedState{status: "disabled"}
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
			m.state = &cachedState{
				status:     "queued",
				instanceID: st.InstanceID,
				model:      model,
				position:   st.Position,
				queueDepth: st.QueueDepth,
				pollAt:     pollAt,
			}
			m.mu.Unlock()
			slog.Debug("session queued", "instance_id", st.InstanceID, "model", model,
				"position", st.Position, "queue_depth", st.QueueDepth, "poll_at", pollAt.Format(time.RFC3339))
			return nil
		case "ended", "superseded", "none":
			m.mu.Lock()
			m.state = nil
			m.mu.Unlock()
			slog.Debug("session recreated", "reason", status, "instance_id", st.InstanceID)
		case "banned":
			return fmt.Errorf("session: account banned upstream")
		case "country_blocked":
			return fmt.Errorf("session: country blocked upstream")
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
		case "model_locked":
			// Previous session is locked to a different model.
			// Release the old slot and retry with the desired model.
			m.mu.Lock()
			oldInstance := ""
			if m.state != nil {
				oldInstance = m.state.instanceID
			}
			m.state = nil
			m.mu.Unlock()
			_ = m.client.EndSession(ctx, oldInstance)
			slog.Debug("session released on model lock, retrying", "current", st.CurrentModel, "target", targetModel)
		case "model_unavailable":
			// Requested model is not available; fall back to default model.
			slog.Warn("session: model unavailable upstream, falling back to default", "requested", targetModel, "fallback", defaultFallbackModel)
			targetModel = defaultFallbackModel
			m.mu.Lock()
			m.state = nil
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
	Status             string
	InstanceID         string
	Model              string
	QueuePosition      int
	QueueDepth         int
	TierAccess         string
	TierCountry        string
	CountryBlockReason string
	ExpiresAt          time.Time
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
	return SessionSnapshot{
		Status:             m.state.status,
		InstanceID:         m.state.instanceID,
		Model:              m.state.model,
		QueuePosition:      m.state.position,
		QueueDepth:         m.state.queueDepth,
		TierAccess:         m.state.accessTier,
		TierCountry:        m.state.countryCode,
		CountryBlockReason: m.state.countryBlockReason,
		ExpiresAt:          m.state.expiresAt,
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
	m.state = nil
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
	m.state = nil
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
	if st.Status == "ended" || st.Status == "superseded" || st.Status == "none" {
		m.mu.Lock()
		m.state = nil
		m.mu.Unlock()
		slog.Debug("session ended during heartbeat", "reason", st.Status, "instance_id", instanceID)
	}
	return nil
}
