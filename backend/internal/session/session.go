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
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"freebuff-proxy/backend/internal/upstream"
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

	// Terminal-event reasons: the standardized session/invalidation
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

	// ReasonSuperseded is the terminal-event reason for a session another
	// instance took over (session_superseded, endsTheSession:true); exported
	// so the chat recovery path feeds the re-admit storm detector.
	ReasonSuperseded = reasonSuperseded

	// Re-admit storm detector: more than stormThreshold terminal
	// session events within stormWindow is a session re-admit storm — each
	// invalidation is followed by a fresh billable admission, so the burst
	// is surfaced once (one Info summary) and re-armed only after a full
	// quiet window passes.
	stormWindow    = 60 * time.Second
	stormThreshold = 3
)

// graceEndFromState prefers the server-defined grace end carried by the
// upstream response and falls back to the fixed expiresAt+graceWindow
// formula. Both admission refresh and poll use it so a server-defined
// grace window is never replaced by the proxy deadline (issue #240).
func graceEndFromState(expiresAt, wireGraceEnd time.Time) time.Time {
	if !wireGraceEnd.IsZero() {
		return wireGraceEnd
	}
	if expiresAt.IsZero() {
		return time.Time{}
	}
	return expiresAt.Add(graceWindow)
}

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

// snapshotState is the Manager's dashboard-resilience / observability state
// (issue #267): the saved fields that keep the dashboard quota table between
// quota-carrying responses, and the rolling recorders that feed the re-admit
// storm log. It is owned by the Manager and guarded by mu, but separated
// from the core cachedState + single-flight lifecycle so every new dashboard
// field has a single home.
type snapshotState struct {
	savedQuota map[string]upstream.ModelQuota
	// savedQuotaStale marks quota restored from the on-disk entry after a
	// restart (no live admission yet this process); savedQuotaAt is when
	// that entry was last polled. Cleared by the first live quota commit.
	savedQuotaStale bool
	savedQuotaAt    time.Time
	// savedQuotaSrcAt records, per model, the source time (Unix millis) of
	// the savedQuota entry: poll time for disk-restored rows, write time
	// for live probe/admission commits, probe TS for ADR-0024 boot-seed
	// rows. The seed path (SeedQuota) only overwrites a model when its row
	// is newer, so a stale seed never downgrades fresher live data.
	// Rebuilt wholesale alongside savedQuota; seed upserts stamp one key.
	savedQuotaSrcAt    map[string]int64
	savedRemainingMs   int64
	savedReferral      *upstream.SessionReferral
	savedGlmPromo      string
	savedAccessTier    string
	savedFreebucks     *upstream.FreebucksInfo
	invalidationEvents []invalidationEvent
	reAdmitTriggers    []time.Time
	lastStormAt        time.Time
}

// invalidationEvent is one terminal session event in the re-admit storm
// window: when the cached session was dropped and why.
type invalidationEvent struct {
	at     time.Time
	reason string
}

type cachedState struct {
	status             string
	instanceID         string
	model              string
	accessTier         string
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
	// remainingMs is the server-authoritative ms left in the active session
	// (wire remainingMs); 0 when absent.
	remainingMs int64
	// referral is the upstream referral block (FreebuffReferralInfo); nil
	// until an admission/poll that carried it.
	referral *upstream.SessionReferral
	// freebucks is the upstream Freebucks allowance block (issue #232); nil
	// until an admission/poll that carried it.
	freebucks     *upstream.FreebucksInfo
	upgradeHint   *upstream.SessionUpgradeHint
	serverMessage string
}

// sessionUsable reports whether the cached state can serve a chat right now:
// an active session until expiresAt-5s (the reference safety margin), or any
// state that still holds a live instance id within the 30-minute grace drain
// after expiry (FREEBUFF_SESSION_GRACE_MS: within grace the row
// stays alive and chat passes — upstream/freebuff freebuff-session.ts). The
// instance-id test guards the grace extension: an ended row whose instance id
// is gone cannot be ridden, and an expired active cache is only reusable
// while the session survives upstream.
func sessionUsable(s *cachedState) bool {
	if s == nil || s.instanceID == "" {
		return false
	}
	graceEnd := graceEndsAt(s)
	return !graceEnd.IsZero() && time.Now().Before(graceEnd)
}

// commit replaces the cached state and mirrors it into the store (when
// configured). Caller must hold m.mu; m.mu is still held when commit
// returns. The store write itself runs with m.mu released (see
// persistSaveLocked): a slow flush must not amplify admission latency for
// concurrent EnsureSessionForModel/Snapshot callers.
//
// A nil cs removes the store entry conditionally on the instance id being
// dropped: the entry is only deleted while it still belongs to the session
// being invalidated, so a stale commit cannot clobber a persisted session
// that was replaced concurrently (e.g. a restart re-adopting a different one).
func (m *Manager) commit(cs *cachedState) {
	oldInstance := ""
	if m.state != nil {
		oldInstance = m.state.instanceID
		// Stash the quota map before dropping state so it survives
		// invalidation (commit(nil)) and later re-admission (issue #146).
		// Re-stamp: the stash is live data, newer than any persisted row.
		if m.state.quotaByModel != nil {
			m.snap.savedQuota = m.state.quotaByModel
			m.stampQuotaSourceLocked(m.now())
		}
		// Stash the glmPromo block the same way (issue #178): it survives
		// invalidation so the GLM promo row stays on the dashboard between
		// quota-carrying responses.
		if m.state.glmPromo != "" {
			m.snap.savedGlmPromo = m.state.glmPromo
		}
		// Stash the server-authoritative countdown and referral block the same
		// way, so the dashboard keeps them across quota-carrying cycles.
		if m.state.remainingMs > 0 {
			m.snap.savedRemainingMs = m.state.remainingMs
		}
		if m.state.referral != nil {
			m.snap.savedReferral = m.state.referral
		}
		if m.state.accessTier != "" {
			m.snap.savedAccessTier = m.state.accessTier
		}
		if m.state.freebucks != nil {
			m.snap.savedFreebucks = m.state.freebucks
		}
	}
	// Freshness for the stale-mark clear below: captured BEFORE the
	// restore, so a re-applied saved map does not pose as fresh quota.
	freshQuota := cs != nil && cs.quotaByModel != nil
	// Restore the previously-seen quota map when the new state omits
	// re-admission or compact polls — issue #146).  This keeps the
	// dashboard quota table visible between quota-carrying responses.
	if cs != nil && cs.quotaByModel == nil && m.snap.savedQuota != nil {
		cs.quotaByModel = m.snap.savedQuota
	}
	// Restore the previously-seen glmPromo block when the new state omits
	// it (issue #178), mirroring the quota-map restore above.
	if cs != nil && cs.glmPromo == "" && m.snap.savedGlmPromo != "" {
		cs.glmPromo = m.snap.savedGlmPromo
	}
	// Restore the countdown and referral the same way when the new state
	// omits them.
	if cs != nil && cs.accessTier == "" && m.snap.savedAccessTier != "" {
		cs.accessTier = m.snap.savedAccessTier
	}
	if cs != nil && cs.remainingMs == 0 && m.snap.savedRemainingMs > 0 {
		cs.remainingMs = m.snap.savedRemainingMs
	}
	if cs != nil && cs.referral == nil && m.snap.savedReferral != nil {
		cs.referral = m.snap.savedReferral
	}
	if cs != nil && cs.freebucks == nil && m.snap.savedFreebucks != nil {
		cs.freebucks = m.snap.savedFreebucks
	}
	m.state = cs
	// Fresh quota-carrying state clears the restart-restored stale mark;
	// the tracker is live again (freshQuota was captured before the
	// saved-map restore, so a re-applied map stays marked).
	if freshQuota {
		m.snap.savedQuotaStale = false
	}
	if m.store != nil && m.key != "" {
		if cs == nil {
			m.persistRemoveLocked(oldInstance)
		} else {
			// Snapshot before the lock release: Save reads the struct
			// fields, and concurrent refreshes mutate the live state in
			// place after the release (quota/glmPromo/remaining probes).
			// The shared maps are only ever read — nothing writes into
			// them after parse — so a shallow copy is a stable view.
			snap := *cs
			m.persistSaveLocked(&snap)
		}
	}
}

// persistSaveLocked persists snap outside the manager lock (review
// 2026-08-31 P3): Store.Save's temp write + rename is disk I/O. Caller
// must hold m.mu; it is released around the write and re-acquired before
// returning, so callers keep their lock discipline. persistMu — taken
// while m.mu is still held — keeps concurrent commits' writes ordered as
// committed even though the I/O itself runs unlocked.
func (m *Manager) persistSaveLocked(snap *cachedState) {
	m.persistMu.Lock()
	m.mu.Unlock()
	m.store.Save(m.key, snap)
	m.persistMu.Unlock()
	m.mu.Lock()
}

// persistRemoveLocked drops the old store entry outside the manager lock,
// mirroring persistSaveLocked (Remove flushes the file too). Caller must
// hold m.mu.
func (m *Manager) persistRemoveLocked(oldInstance string) {
	m.persistMu.Lock()
	m.mu.Unlock()
	m.store.Remove(m.key, oldInstance)
	m.persistMu.Unlock()
	m.mu.Lock()
}

// restorePersistedQuotaLocked seeds the saved quota from the on-disk entry
// when this process has never seen live quota (restart with lazy session
// resume). Caller must hold m.mu. Store.Load is in-memory after the first
// read, so the per-poll Snapshot cost is one map lookup until live data
// arrives and the live commit clears the stale mark.
func (m *Manager) restorePersistedQuotaLocked() {
	if m.store == nil || m.key == "" || len(m.snap.savedQuota) > 0 {
		return
	}
	cs := m.store.Load(m.key)
	if cs == nil {
		return
	}
	// quotaByModel/pollAt/glmPromo are unexported but reachable: same package.
	if len(cs.quotaByModel) == 0 {
		return
	}
	m.snap.savedQuota = cs.quotaByModel
	if cs.glmPromo != "" && m.snap.savedGlmPromo == "" {
		m.snap.savedGlmPromo = cs.glmPromo
	}
	m.snap.savedQuotaStale = true
	m.snap.savedQuotaAt = cs.pollAt
	if m.snap.savedQuotaAt.IsZero() {
		m.snap.savedQuotaAt = time.Now()
	}
	// Baseline for seed ts-compare: the restore is as old as its poll.
	m.stampQuotaSourceLocked(m.snap.savedQuotaAt)
}

// Snapshot returns a best-effort view of the cached session state. All
// fields may be zero when no session has been created yet. Added for
func (m *Manager) Snapshot() SessionSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil {
		// Restart with lazy session resume: no admission yet this process,
		// so seed the last-seen quota from the on-disk entry (quota tracker
		// stays populated instead of empty until the next request re-polls).
		m.restorePersistedQuotaLocked()
		var quota map[string]QuotaSnapshot
		if len(m.snap.savedQuota) > 0 {
			quota = make(map[string]QuotaSnapshot, len(m.snap.savedQuota))
			for modelID, q := range m.snap.savedQuota {
				quota[modelID] = QuotaSnapshot{
					Model:       q.Model,
					Limit:       q.Limit,
					RecentCount: q.RecentCount,
					ResetAt:     q.ResetAt,
					Period:      q.Period,
					Pool:        q.Pool,
					PoolLabel:   q.PoolLabel,
					Entitlement: q.Entitlement,
				}
			}
		}
		return SessionSnapshot{
			Refreshing:   m.refreshing,
			QuotaByModel: quota,
			QuotaStale:   m.snap.savedQuotaStale && len(quota) > 0,
			QuotaSavedAt: m.snap.savedQuotaAt,
			GlmPromo:     m.snap.savedGlmPromo,
			RemainingMs:  m.snap.savedRemainingMs,
			Referral:     m.snap.savedReferral,
			AccessTier:   m.snap.savedAccessTier,
			Freebucks:    m.snap.savedFreebucks,
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
			Pool:        q.Pool,
			PoolLabel:   q.PoolLabel,
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
		AccessTier:         m.state.accessTier,
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
		// A quota-less compact commit re-applies the saved map (issue
		// #146): when that map is restart-restored, it stays marked until
		// genuinely fresh quota lands.
		QuotaStale:    m.snap.savedQuotaStale && len(quota) > 0,
		QuotaSavedAt:  m.snap.savedQuotaAt,
		GlmPromo:      m.state.glmPromo,
		Standing:      m.state.standing,
		RemainingMs:   m.state.remainingMs,
		Referral:      m.state.referral,
		Freebucks:     m.state.freebucks,
		UpgradeHint:   m.state.upgradeHint,
		ServerMessage: m.state.serverMessage,
	}
}

// HasGlmEntitlement reports whether the manager currently holds verified
// referral entitlement for z-ai/glm-5.2 from active or saved quota/promo (issue #183).
func (m *Manager) HasGlmEntitlement() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hasGlmEntitlementLocked()
}

func (m *Manager) hasGlmEntitlementLocked() bool {
	quota := m.snap.savedQuota
	promo := m.snap.savedGlmPromo
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
		m.snap.savedGlmPromo = st.GlmPromo
		if m.state != nil {
			m.state.glmPromo = st.GlmPromo
		}
	}
	if len(st.RateLimitsByModel) > 0 {
		m.snap.savedQuota = st.RateLimitsByModel
		// Live probe contact clears the restart-restored stale mark and
		// re-stamps the source times, so a later boot seed compares
		// against this write (never downgrades it with an older row).
		m.snap.savedQuotaStale = false
		m.stampQuotaSourceLocked(m.now())
		if m.state != nil {
			m.state.quotaByModel = st.RateLimitsByModel
		}
	}
	if st.RemainingMs > 0 {
		m.snap.savedRemainingMs = st.RemainingMs
		if m.state != nil {
			m.state.remainingMs = st.RemainingMs
		}
	}
	if st.Referral != nil {
		m.snap.savedReferral = st.Referral
		if m.state != nil {
			m.state.referral = st.Referral
		}
	}
	if st.AccessTier != "" {
		m.snap.savedAccessTier = st.AccessTier
		if m.state != nil {
			m.state.accessTier = st.AccessTier
		}
	}
	if st.Freebucks != nil {
		m.snap.savedFreebucks = st.Freebucks
		if m.state != nil {
			m.state.freebucks = st.Freebucks
		}
	}
}

// Invalidate drops the cached session. Used when a chat request reports a session-level error. The
// invalidation is recorded with the canonical 409 reason (the session-invalid
// chat family); callers that can name a more specific cause use
// InvalidateWithReason.
func (m *Manager) Invalidate() {
	m.InvalidateWithReason(reason409, 0)
}

// InvalidateWithReason drops the cached session, recording WHY and
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
// (#159): additionally records WHY and the triggering HTTP status.
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

// SetSessionStateWithTierForTest sets cached session state with an access tier for tests.
func (m *Manager) SetSessionStateWithTierForTest(status, instanceID, model, accessTier string, expiresAt, graceEndsAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commit(&cachedState{
		status:            status,
		instanceID:        instanceID,
		model:             model,
		accessTier:        accessTier,
		expiresAt:         expiresAt,
		gracePeriodEndsAt: graceEndsAt,
	})
}
