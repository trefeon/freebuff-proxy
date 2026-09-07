// session_manager.go - Manager plus single-flight admission entry, split from
// session.go (Wave C revamp): the Manager struct, constructors, and the
// EnsureSession/EnsureSessionForModel single-flight loop. Upstream admission
// (session_admission.go), polling (session_poll.go), state commits (commit in
// session.go), and persistence (store.go) stay where they are.
package session

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"freebuff-proxy/backend/internal/upstream"
)

// Manager owns the cached session state for one token.
type Manager struct {
	client *upstream.Client

	// store, when non-nil, persists the cached session state across process
	// restarts (SESSION_PERSIST). key is the stable store key derived from
	// the client token (upstream.Client.TokenKey).
	store *Store
	key   string

	mu sync.Mutex
	// persistMu serializes the store writes issued by commit/Shutdown
	// (review 2026-08-31 P3): those writes run with mu RELEASED — the
	// temp write + rename inside Store is disk I/O and must not sit under
	// the manager lock — so persistMu, always taken while mu is still
	// held, keeps them landing in the order the commits were made. Lock
	// hierarchy: mu → persistMu; persistMu is released before mu is
	// re-acquired, so no cycle is possible.
	persistMu  sync.Mutex
	state      *cachedState
	refreshCh  chan struct{} // closed by the in-flight refresher when done
	refreshing bool
	// refreshErr retains the last refresh's error under mu so waiters parked
	// on that refresh surface it (after one state re-check) instead of each
	// becoming the next refresher and re-running the failing upstream create.
	// Cleared when a new refresh starts, so a later caller retries normally.
	refreshErr error
	// testWaiterPark, when set (tests only), runs while mu is held at the
	// moment a follower parks on refreshCh — lets tests deterministically
	// count parked waiters before releasing a held leader request.
	testWaiterPark func()

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
	// snap holds the manager's dashboard-resilience / observability state
	// (issue #267): the saved fields that keep the dashboard quota table
	// between quota-carrying responses, and the rolling recorders that feed
	// the re-admit storm log. Guarded by mu like the rest of the manager.
	snap snapshotState

	// adopt is the issue #97 CLI-session adoption mode (ADOPT_CLI_SESSION):
	// nil (default) = create sessions normally. When set, the manager adopts
	// the CLI's active instance and refuses to create a competing session
	// while the CLI process is alive.
	adopt *CLIAdoption

	// now returns the current time; injectable in tests to drive the
	// re-admit storm detector deterministically. Defaults to time.Now.
	now func() time.Time

	// modelLocked tallies model-lock release events keyed by from → to
	// model pair (issue #160): every model_locked admission releases the
	// old slot and re-admits with the requested model, so the pair counts
	// the model-switch cost. Guarded by modelLockedMu (refresh holds no
	// other lock while recording).
	modelLockedMu sync.Mutex
	modelLocked   map[string]map[string]int64
}

// NewManager builds a session manager for the given upstream client.
func NewManager(client *upstream.Client) *Manager {
	if client == nil {
		panic("session: nil client")
	}
	return NewManagerWithStore(client, nil)
}

// NewManagerWithStore builds a session manager that also persists its cached
// state through store (nil disables persistence).
func NewManagerWithStore(client *upstream.Client, store *Store) *Manager {
	if client == nil {
		panic("session: nil client")
	}
	m := &Manager{client: client, store: store, now: time.Now}
	m.key = client.TokenKey()
	return m
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
				// an active session until expiresAt-5s, or a
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
				// through to refresh; refresh releases the old slot before
				// the new admission (see releaseHeldSlotForTarget).
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
			if m.testWaiterPark != nil {
				m.testWaiterPark()
			}
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
					// request that could still be served (through the grace drain).
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
