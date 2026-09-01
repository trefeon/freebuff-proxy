// session_poll.go — session liveness polling, split from session.go (CI line
// cap): the periodic compact poll (Poll), persisted-session resume
// (pollPersisted), the upstream status → typed-error/reason mappings
// (statusError, tableReason) shared by the poll and refresh paths, and the
// admission probe-cache TTL setter (SetAdmissionProbeTTL).
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"freebuff-proxy/backend/internal/upstream"
)

// SetAdmissionProbeTTL configures the admission probe cache TTL (issue #60):
// session poll GETs within d of the last successful session response are
// skipped. d <= 0 disables. Wired by the pool from SESSION_PROBE_CACHE_TTL;
// safe to call at runtime.
func (m *Manager) SetAdmissionProbeTTL(d time.Duration) {
	m.mu.Lock()
	m.probeTTL = d
	m.mu.Unlock()
}

// tableReason maps an upstream session status to the terminal-event reason
// vocabulary. Used by the poll/refresh drop paths so the logged reason
// is always one of the table values; the raw upstream status rides in the
// log's status field.
func tableReason(status string) string {
	if status == "superseded" {
		return reasonSuperseded
	}
	return reasonEnded
}

// statusError maps an upstream session status to the typed error callers
// use for recovery (token cooldown, region surfacing). st supplies the
// fields carried by the error; non-error statuses return nil. Shared by
// refresh and Poll so both map the same way.
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
	case "rate_limited", "spend_limited":
		retryAfter := upstream.CooldownFromMillis(float64(st.RetryAfterMs))
		if retryAfter <= 0 {
			retryAfter = time.Minute
		}
		return &upstream.RateLimitError{
			Status:      status,
			Model:       st.Model,
			RetryAfter:  retryAfter,
			ResetAt:     st.ResetAt,
			Limit:       st.Limit,
			RecentCount: st.RecentCount,
			Body:        st.Message,
		}
	case "ip_capped":
		// Distinct error: ip_capped is admission-only (too many distinct
		// users on the egress IP) and NOT tied to a quota reset, so the
		// cooldown is bounded to retryAfterMs only — never the
		// Pacific-midnight lock (reference/freebuff freebuff-session.ts).
		retryAfter := upstream.CooldownFromMillis(float64(st.RetryAfterMs))
		if retryAfter <= 0 {
			retryAfter = time.Minute
		}
		return &upstream.IpCappedError{
			ActiveUsersForIP: st.ActiveUsersForIP,
			Limit:            st.Limit,
			RetryAfter:       retryAfter,
			Body:             st.Message,
		}
	case "session_model_mismatch", "limited_ip":
		// The egress IP cannot serve the requested model. The session row is
		// fine (bound to its admitted model) — not session-invalid, so it
		// must never be invalidated/refreshed (re-admitting burns a daily
		// session slot). Non-limited messages keep today's exact error text.
		if strings.Contains(strings.ToLower(st.Message), "limited") {
			return &upstream.LimitedIpError{
				RetryAfter: upstream.CooldownFromMillis(float64(st.RetryAfterMs)),
				Body:       st.Message,
			}
		}
		return fmt.Errorf("session: unknown upstream status %q", status)
	}
	return nil
}

// pollPersisted attempts to resume a persisted session before a fresh create
// (restart reuse). requestedModel is the model the caller is about to create
// for: a persisted slot bound to a different model is dropped instead of
// adopted, so a model-mismatch refresh falls through to a create for the
// requested model.
//
// It returns a non-nil SessionState when the persisted slot is still active
// upstream and model-compatible (adopted), and (nil, nil) otherwise — either
// there is no persisted slot, it is expired, it is model-incompatible, or it
// is dead upstream (in which case it is removed from the store). Transport
// errors are returned so the caller surfaces them like a failed create
// instead of burning a fresh daily session slot on a merely-flaky upstream.
func (m *Manager) pollPersisted(ctx context.Context, requestedModel string) (*upstream.SessionState, error) {
	if m.store == nil || m.key == "" {
		return nil, nil
	}
	cs := m.store.Load(m.key)
	if cs == nil || cs.status != "active" || cs.instanceID == "" {
		return nil, nil
	}
	// Only resume a slot that is still genuinely active (with the 5s safety
	// margin). An expired-but-in-grace slot is draining; resume it is not
	// worth the risk of admitting new work onto a dying session.
	if cs.expiresAt.IsZero() || !time.Now().Before(cs.expiresAt.Add(-expiryMargin)) {
		m.store.Remove(m.key, cs.instanceID)
		return nil, nil
	}
	// Model gate (pre-flight): a persisted slot known to be bound to a
	// different model must never be re-adopted for another model — that
	// would pin the old model's session forever on every refresh.
	if cs.model != "" && requestedModel != "" && cs.model != requestedModel {
		m.store.Remove(m.key, cs.instanceID)
		return nil, nil
	}

	st, err := m.client.GetSession(ctx, cs.instanceID)
	if err != nil {
		// 428 waiting_room_required is session-ENDING (endsTheSession:true
		// — the seat is gone, same as the live Poll/refresh drop paths
		// #116/#140): drop the dead persisted row so the next iteration
		// re-admits fresh instead of re-polling it forever.
		if errors.Is(err, upstream.ErrWaitingRoomRequired) {
			m.store.Remove(m.key, cs.instanceID)
		}
		// Transport error: surface it instead of swallowing and falling
		// through to a fresh create. The caller retries (single-flight /
		// TRANSIENT_RETRIES); a create here would burn a session slot.
		return nil, err
	}
	switch st.Status {
	case "active":
		// Model gate (post-flight): the upstream may have bound the resumed
		// slot to a different model than requested. Adopt only when the
		// resumed model is compatible; otherwise drop the slot and fall
		// through to a create for the requested model.
		if st.Model != "" && requestedModel != "" && st.Model != requestedModel {
			m.store.Remove(m.key, st.InstanceID)
			return nil, nil
		}
		slog.Debug("session resumed from store", "instance_id", st.InstanceID, "expires_at", st.ExpiresAt.Format(time.RFC3339))
		return st, nil
	case "ended", "superseded", "none", "banned":
		m.store.Remove(m.key, st.InstanceID)
		return nil, nil
	case "country_blocked", "rate_limited", "ip_capped", "spend_limited":
		// Terminal admission refusals: the persisted slot is dead upstream;
		// drop it so a restart re-admits from scratch instead of re-polling.
		m.store.Remove(m.key, st.InstanceID)
		return nil, nil
	default:
		// queued or an unknown status: not resumable as an active slot, but
		// non-terminal — keep the entry for a later restart.
		return nil, nil
	}
}

// Poll runs the periodic session-liveness poll: a compact GET with NO
// heartbeat header — the CLI never beats (x-freebuff-heartbeat is
// Desktop-only, reference/freebuff freebuff-models.ts:1212-1215); liveness
// comes from the recurring compact GET itself. It refreshes the
// cached state the way the CLI's 30s compact poll does: statusError
// mappings, drop-on-ban, invalidate on superseded/none, and — within the
// 30-minute grace drain — an ended response that still carries the instance
// id is kept as a usable "ended" row instead of being invalidated.
func (m *Manager) Poll(ctx context.Context) error {
	m.mu.Lock()
	if m.state == nil || (m.state.status != "active" && m.state.status != "ended") || m.state.instanceID == "" {
		m.mu.Unlock()
		return nil
	}
	// Issue #60: admission probe caching — within the probe TTL of the last
	// successful session response the cached state is authoritative, so the
	// poll GET is redundant; skip it (less upstream traffic, fewer chances
	// to trip the one-client-at-a-time gate).
	if m.probeTTL > 0 && !m.lastAdmitted.IsZero() && time.Since(m.lastAdmitted) < m.probeTTL {
		m.mu.Unlock()
		return nil
	}
	instanceID := m.state.instanceID
	m.mu.Unlock()

	start := time.Now()
	st, err := m.client.GetSessionWithOpts(ctx, instanceID, true)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		// #116: 428 waiting_room_required is session-ENDING
		// (endsTheSession:true per FREEBUFF_GATE_CODES — the seat is gone;
		// reference/freebuff freebuff-session.ts). Drop the cached admission
		// so the next EnsureSession re-admits fresh (the pool's
		// WAITING_ROOM_CHAIN fires before the create). Any other poll error
		// is left for the pool's failure backoff.
		if errors.Is(err, upstream.ErrWaitingRoomRequired) {
			dropped := false
			m.mu.Lock()
			if m.state != nil && m.state.instanceID == instanceID {
				m.commit(nil)
				dropped = true
			}
			m.mu.Unlock()
			if dropped {
				m.recordInvalidation(reasonPoll)
				slog.Warn("session dropped during poll", "reason", reasonPoll, "status", "waiting_room_required", "instance_id", instanceID)
			}
		}
		return err
	}
	m.mu.Lock()
	// A successful GET confirms the cached state: refresh the probe window.
	m.lastAdmitted = time.Now()
	m.mu.Unlock()
	if serr := statusError(st.Status, st); serr != nil {
		// A banned session is dead until the account unban: drop the cached
		// admission (cooldown) so the token re-admits only after the pool's
		// ban window, instead of polling a stale slot.
		if st.Status == "banned" {
			dropped := false
			m.mu.Lock()
			if m.state != nil && m.state.instanceID == instanceID {
				m.commit(nil)
				dropped = true
			}
			m.mu.Unlock()
			if dropped {
				m.recordInvalidation(reasonPoll)
				slog.Warn("session dropped during poll", "reason", reasonPoll, "status", st.Status, "instance_id", instanceID)
			}
		}
		return serr
	}
	if st.Status == "superseded" || st.Status == "none" {
		dropped := false
		m.mu.Lock()
		if m.state != nil && m.state.instanceID == instanceID {
			m.commit(nil)
			dropped = true
		}
		m.mu.Unlock()
		if dropped {
			m.recordInvalidation(tableReason(st.Status))
			slog.Warn("session ended during poll", "reason", tableReason(st.Status), "status", st.Status, "instance_id", instanceID)
		}
		return nil
	}
	if st.Status == "ended" {
		// Ended WITH the instance id still present: the row is in the 30-min
		// grace drain and stays usable. Refresh the cached state
		// as ended-with-instance so the fast path keeps serving it until
		// grace closes; the pool keeps polling. The grace end comes from the
		// response when present, else expiresAt + graceWindow.
		graceEnd := graceEndFromState(st.ExpiresAt, st.GracePeriodEndsAt)
		if st.InstanceID != "" && !graceEnd.IsZero() && time.Now().Before(graceEnd) {
			m.mu.Lock()
			if m.state != nil && m.state.instanceID == instanceID {
				m.commit(&cachedState{
					status:            "ended",
					instanceID:        st.InstanceID,
					model:             m.state.model,
					expiresAt:         st.ExpiresAt,
					gracePeriodEndsAt: graceEnd,
				})
				slog.Debug("session in grace drain during poll", "instance_id", instanceID, "grace_ends_at", graceEnd.Format(time.RFC3339))
			}
			m.mu.Unlock()
			return nil
		}
		// The row is gone (no instance id) or past grace: drop it so the
		// next EnsureSession re-creates a fresh session.
		dropped := false
		m.mu.Lock()
		if m.state != nil && m.state.instanceID == instanceID {
			m.commit(nil)
			dropped = true
		}
		m.mu.Unlock()
		if dropped {
			m.recordInvalidation(tableReason(st.Status))
			slog.Warn("session ended during poll", "reason", tableReason(st.Status), "status", st.Status, "instance_id", instanceID)
		}
		return nil
	}
	// Heartbeat liveness confirmed: the compact poll returned a usable
	// status (active). instance/ms/status standardize the heartbeat poll
	// line so ops can see each liveness beat and its latency.
	slog.Debug("session: heartbeat poll", "instance_id", shortInstance(instanceID), "ms", ms, "status", st.Status)
	return nil
}
