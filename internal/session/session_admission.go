// session_admission.go — upstream session admission, split from session.go
// (CI line cap): the create/adopt handshake (adoptOrCreate, adoptOwner,
// CLIOwner/CLIAdoption), the create/poll refresh loop, pre-emptive re-admit
// + re-admit storm detection (asyncReAdmit, recordReAdmitTrigger,
// recordInvalidation), the admission-time config setters, and upstream
// teardown (EndSession, Shutdown).
package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"freebuff-proxy/internal/upstream"
)

// SetReAdmitLead configures the pre-emptive re-admit lead (issue #99): when
// the cached active session has less than d left, EnsureSessionForModel
// triggers an async re-admit and rides the old session. d <= 0 disables.
// Wired by the pool from SESSION_RE_ADMIT_LEAD; safe to call at runtime.
func (m *Manager) SetReAdmitLead(d time.Duration) {
	m.mu.Lock()
	m.reAdmitLead = d
	m.mu.Unlock()
}

// CLIOwner mirrors the official CLI's freebuff-instance-owner.json (issue
// #97, reference proxy-freebuff server.js readCliInstanceOwner): the CLI
// rewrites this file whenever its active session changes (restart,
// rotation, new conversation).
type CLIOwner struct {
	InstanceID string `json:"instanceId"`
	PID        int    `json:"pid"`
}

// CLIAdoption is the issue #97 opt-in wiring: with ADOPT_CLI_SESSION the
// proxy behaves like the official CLI for a single account — it adopts the
// CLI's ACTIVE session instance and never creates a competing one while the
// CLI process is alive. Enabled is false by default; OwnerFile is the
// freebuff-instance-owner.json path; Initial is the startup snapshot (the
// file is re-read before every refresh). testAlive overrides the PID
// liveness check in tests (nil = platform check).
type CLIAdoption struct {
	Enabled   bool
	OwnerFile string
	Initial   CLIOwner
	testAlive func(int) bool
}

// SetCLIAdoption configures (or clears, with a zero value) the CLI-session
// adoption mode (issue #97, ADOPT_CLI_SESSION). Wired by main.go before the
// pool starts serving.
func (m *Manager) SetCLIAdoption(a CLIAdoption) {
	m.mu.Lock()
	if a.Enabled {
		a.testAlive = processAlive
		m.adopt = &a
	} else {
		m.adopt = nil
	}
	m.mu.Unlock()
}

// SetScarceModels configures the scarce-model set (issue #155): models whose
// active sessions are kept on Shutdown instead of being DELETE'd upstream.
// Wired by the pool from SCARCE_SESSION_MODELS; safe to call at runtime.
// A nil or empty slice clears the set.
func (m *Manager) SetScarceModels(models []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(models) == 0 {
		m.scarce = nil
		return
	}
	m.scarce = make(map[string]bool, len(models))
	for _, mod := range models {
		if mod != "" {
			m.scarce[mod] = true
		}
	}
	if len(m.scarce) == 0 {
		m.scarce = nil
	}
}

// IsScarce reports whether model is in the scarce set (issue #155).
func (m *Manager) IsScarce(model string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.scarce[model]
}

// adoptOwner re-reads the CLI owner file fresh (issue #97(c)): the CLI
// rewrites freebuff-instance-owner.json when its session changes, so a startup snapshot alone would go stale after a CLI restart.
func (m *Manager) adoptOwner() (CLIOwner, bool) {
	m.mu.Lock()
	adopt := m.adopt
	m.mu.Unlock()
	if adopt == nil || !adopt.Enabled {
		return CLIOwner{}, false
	}
	data, err := os.ReadFile(adopt.OwnerFile)
	if err != nil {
		return CLIOwner{}, false
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	var owner CLIOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return CLIOwner{}, false
	}
	if owner.InstanceID == "" && owner.PID == 0 {
		// Empty owner record: fall back to the startup snapshot only for the
		// instance id (the pid is required for liveness).
		owner = adopt.Initial
	}
	return owner, true
}

// adoptOrCreate is the issue #97 session-creation path: when CLI adoption
// is enabled it adopts the CLI's active session (or refuses to create a
// competing one); otherwise it creates a fresh session exactly as before.
func (m *Manager) adoptOrCreate(ctx context.Context, requestedModel string) (*upstream.SessionState, error) {
	m.mu.Lock()
	adopt := m.adopt
	m.mu.Unlock()
	if adopt == nil || !adopt.Enabled {
		return m.createSessionForModel(ctx, requestedModel)
	}

	owner, ok := m.adoptOwner()
	if !ok {
		// Owner file missing/unreadable (issue #97(c)): the CLI's session
		// state is unknown, so a create could supersede and log out the CLI.
		// Refuse loudly rather than compete.
		slog.Warn("ADOPT_CLI_SESSION: freebuff-instance-owner.json missing — refusing to create a competing session",
			"file", adopt.OwnerFile)
		return nil, fmt.Errorf("ADOPT_CLI_SESSION: freebuff-instance-owner.json missing (%s) — refusing to create a competing session (start the CLI once or disable ADOPT_CLI_SESSION)", adopt.OwnerFile)
	}
	if owner.PID <= 0 || !adopt.testAlive(owner.PID) {
		// The CLI process is not running: the proxy may create (and own) a
		// session for the account, exactly like the reference fallback.
		return m.createSessionForModel(ctx, requestedModel)
	}
	if owner.InstanceID == "" {
		return nil, fmt.Errorf("ADOPT_CLI_SESSION: the FreeBuff CLI is running but no session instance was recorded — refusing to create a competing session (stop the CLI or retry)")
	}
	// CLI alive: adopt ITS session — poll it, never POST a competing one
	// (a create supersedes the CLI's session and logs it out).
	st, err := m.client.GetSession(ctx, owner.InstanceID)
	if err != nil {
		return nil, fmt.Errorf("ADOPT_CLI_SESSION: CLI session %s could not be verified (%v) — refusing to create a competing session (stop the CLI or retry)", shortInstance(owner.InstanceID), err)
	}
	status := strings.TrimSpace(st.Status)
	switch status {
	case "active":
		if requestedModel != "" && st.Model != "" && st.Model != requestedModel {
			return nil, fmt.Errorf("ADOPT_CLI_SESSION: the CLI session is for model %s but %s was requested — refusing to create a competing session (use %s or stop the CLI)", st.Model, requestedModel, st.Model)
		}
		slog.Info("adopted existing CLI freebuff session", "instance_id", shortInstance(st.InstanceID), "model", st.Model)
		return st, nil
	case "queued":
		// Adopt the queue position: pollAt mirrors the create path.
		if st.PollAt.IsZero() {
			wait := time.Duration(st.EstimatedWaitMs) * time.Millisecond
			if wait < time.Second {
				wait = time.Second
			}
			if wait > 5*time.Second {
				wait = 5 * time.Second
			}
			st.PollAt = time.Now().Add(wait)
		}
		slog.Info("adopted queued CLI freebuff session", "instance_id", shortInstance(st.InstanceID), "position", st.Position)
		return st, nil
	case "disabled":
		slog.Info("adopted disabled CLI freebuff session")
		return st, nil
	default:
		return nil, fmt.Errorf("ADOPT_CLI_SESSION: CLI session %s is not adoptable (status %q) — refusing to create a competing session (restart the CLI or stop it)", shortInstance(owner.InstanceID), status)
	}
}

// createSessionForModel POSTs a fresh session for model and tags any
// rate-limit refusal with the requested model (issue #178): upstream 429
// quota bodies can omit the model field, and the pool needs it to isolate
// the cooldown per model — a quota cap on z-ai/glm-5.2 must not block
// deepseek/deepseek-v4-flash on the same token.
func (m *Manager) createSessionForModel(ctx context.Context, model string) (*upstream.SessionState, error) {
	if model == "z-ai/glm-5.2" {
		// Pre-admission guard (issue #183): z-ai/glm-5.2 is referral-only.
		// If the token has never probed/admitted (or has no known GLM entitlement),
		// probe first with a zero-cost GET /api/v1/freebuff/session to check
		// whether the account holds referral quota or an active promo.
		// If unentitled, fail fast before sending POST /api/sessions/create
		// with x-freebuff-model: z-ai/glm-5.2 (which upstream punishes with 403 account_banned).
		if !m.HasGlmEntitlement() {
			if m.client != nil {
				if probeState, err := m.client.ProbeAccount(ctx); err == nil && probeState != nil {
					m.mu.Lock()
					if probeState.GlmPromo != "" {
						m.savedGlmPromo = probeState.GlmPromo
						if m.state != nil {
							m.state.glmPromo = probeState.GlmPromo
						}
					}
					if len(probeState.RateLimitsByModel) > 0 {
						m.savedQuota = probeState.RateLimitsByModel
						if m.state != nil {
							m.state.quotaByModel = probeState.RateLimitsByModel
						}
					}
					m.mu.Unlock()
				}
			}
			if !m.HasGlmEntitlement() {
				return nil, &upstream.RateLimitError{
					Status: "rate_limited",
					Model:  model,
					Body:   "token has no referral quota for " + model,
				}
			}
		}
	}
	st, err := m.client.CreateSessionForModel(ctx, model)
	if err != nil {
		var rle *upstream.RateLimitError
		if errors.As(err, &rle) && rle.Model == "" {
			rle.Model = model
		}
	}
	return st, err
}

// shortInstance renders a session instance id's first 8 chars for logs.
func shortInstance(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

// asyncReAdmit runs a pre-emptive refresh in the background (issue #99): the
// triggering request rides the old session while the new admission proceeds;
// concurrent requests park on the single-flight refreshCh and get the new
// instance once it lands (or ride the old session when the refresh fails and
// it is still usable). Bounded by asyncReAdmitTimeout so a hung upstream
// never leaks a goroutine.
func (m *Manager) asyncReAdmit(model string) {
	ctx, cancel := context.WithTimeout(context.Background(), asyncReAdmitTimeout)
	defer cancel()
	err := m.refresh(ctx, model, true)
	m.mu.Lock()
	m.refreshing = false
	if err != nil {
		m.refreshErr = err
	}
	close(m.refreshCh)
	m.refreshCh = nil
	m.mu.Unlock()
	if err != nil {
		slog.Debug("session: pre-emptive re-admit failed", "err", err)
		return
	}
	slog.Debug("session: pre-emptive re-admit done")
}

// recordReAdmitTrigger remembers a pre-emptive re-admit trigger (issue #99)
// for the re-admit storm summary's burned_slots count (T10): a trigger whose
// session is later invalidated burned a daily session slot. Caller must NOT
// hold m.mu.
func (m *Manager) recordReAdmitTrigger() {
	m.mu.Lock()
	now := m.now()
	cutoff := now.Add(-stormWindow)
	m.reAdmitTriggers = append(m.reAdmitTriggers, now)
	triggers := m.reAdmitTriggers[:0]
	for _, t := range m.reAdmitTriggers {
		if t.After(cutoff) {
			triggers = append(triggers, t)
		}
	}
	m.reAdmitTriggers = triggers
	m.mu.Unlock()
}

// recordInvalidation appends a terminal session event to the rolling
// re-admit storm window (T10) and, when more than stormThreshold
// invalidations land within stormWindow and the suppression window has
// passed, emits ONE Info summary (count, duration_ms, superseded,
// burned_slots). Caller must NOT hold m.mu; the summary is logged outside
// the lock.
func (m *Manager) recordInvalidation(reason string) {
	m.mu.Lock()
	now := m.now()
	m.invalidationEvents = append(m.invalidationEvents, invalidationEvent{at: now, reason: reason})
	cutoff := now.Add(-stormWindow)
	events := m.invalidationEvents[:0]
	for _, ev := range m.invalidationEvents {
		if ev.at.After(cutoff) {
			events = append(events, ev)
		}
	}
	m.invalidationEvents = events
	triggers := m.reAdmitTriggers[:0]
	for _, t := range m.reAdmitTriggers {
		if t.After(cutoff) {
			triggers = append(triggers, t)
		}
	}
	m.reAdmitTriggers = triggers

	// Storm only when strictly more than the threshold invalidations sit in
	// the window, and only once per suppression window (60s of quiet re-arms
	// the detector).
	if len(m.invalidationEvents) <= stormThreshold || (!m.lastStormAt.IsZero() && now.Sub(m.lastStormAt) < stormWindow) {
		m.mu.Unlock()
		return
	}
	m.lastStormAt = now
	count := len(m.invalidationEvents)
	duration := m.invalidationEvents[len(m.invalidationEvents)-1].at.Sub(m.invalidationEvents[0].at).Milliseconds()
	superseded := 0
	for _, ev := range m.invalidationEvents {
		if ev.reason == reasonSuperseded {
			superseded++
		}
	}
	// burned_slots: pre-emptive re-admit triggers within the same window —
	// each one whose session the storm then invalidated burned a daily slot.
	// The trigger list is pruned to the window above, so its length is the
	// count.
	burned := len(m.reAdmitTriggers)
	m.mu.Unlock()

	slog.Info("session re-admit storm",
		"count", count,
		"duration_ms", duration,
		"superseded", superseded,
		"burned_slots", burned)
}

// refresh runs the create/poll status loop, updating cached state, until the
// session is active, disabled, or the iteration budget is exhausted.
// preemptive marks an issue #99 async re-admit: a create refusal while the
// old instance is still authoritative must NOT invalidate the cached session
// (the caller is riding it) — return instead of committing nil and looping.
func (m *Manager) refresh(ctx context.Context, requestedModel string, preemptive bool) error {
	targetModel := requestedModel
	// Issue #158: a model cached unavailable skips the 409 admission
	// roundtrip entirely (see modelUnavailableShortCircuit).
	if m.modelUnavailableShortCircuit(&targetModel) {
		return nil
	}
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
		} else if cached == nil {
			// Fresh manager (first call or restart): resume a persisted
			// session before creating a new one. A persisted active slot
			// that is still alive upstream (and model-compatible) is adopted
			// instead of burning a fresh session quota.
			st, err = m.pollPersisted(ctx, targetModel)
			if st == nil && err == nil {
				st, err = m.adoptOrCreate(ctx, targetModel)
			}
		} else {
			// Live refresh (expired cache or model mismatch): never consult
			// the store. Re-adopting a persisted slot here would pin the
			// previous model's session on every refresh; always create for
			// the requested model (baseline behavior).
			st, err = m.adoptOrCreate(ctx, targetModel)
		}
		if err != nil {
			// #140 P2: a 428 waiting_room_required on the queued row's
			// refresh GET is session-ENDING (endsTheSession:true — the seat
			// is gone, same as Poll's #116 handling). Drop the dead queued
			// row (instance-guarded) so the next EnsureSession re-admits
			// fresh, then return the error for the pool's failover.
			if errors.Is(err, upstream.ErrWaitingRoomRequired) {
				dropped := false
				m.mu.Lock()
				if m.state != nil && m.state.instanceID == cached.instanceID {
					m.commit(nil)
					dropped = true
				}
				m.mu.Unlock()
				if dropped {
					m.recordInvalidation(reasonPoll)
					slog.Warn("session dropped on queued refresh", "reason", reasonPoll, "status", "waiting_room_required", "instance_id", cached.instanceID)
				}
			}
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
				countryCode:        st.CountryCode,
				countryBlockReason: st.CountryBlockReason,
				activeUsersForIP:   st.ActiveUsersForIP,
				ipPrivacySignals:   st.IpPrivacySignals,
				limit:              st.Limit,
				quotaByModel:       st.RateLimitsByModel,
				glmPromo:           st.GlmPromo,
				standing:           st.Standing,
			})
			// Issue #60: the successful admission refreshes the probe cache
			// window — subsequent session poll GETs within the TTL are
			// skipped.
			m.lastAdmitted = time.Now()
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
			if preemptive {
				// Issue #163: a pre-emptive re-admit that lands in the
				// waiting room must not displace the ridden session —
				// inside the grace drain the old row stays authoritative
				// until grace closes, and committing the queue would
				// surface waiting-room latency on the next request. Keep
				// the cached session; the once-per-expiry guard stops a
				// retry storm.
				slog.Debug("session: pre-emptive re-admit queued, riding old session")
				return nil
			}
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
				glmPromo:   st.GlmPromo,
			})
			m.mu.Unlock()
			slog.Debug("session queued", "instance_id", st.InstanceID, "model", model,
				"position", st.Position, "queue_depth", st.QueueDepth, "poll_at", pollAt.Format(time.RFC3339))
			return nil
		case "ended", "superseded", "none":
			if preemptive {
				// Issue #132: the upstream refused a fresh admission while
				// the old instance is still authoritative (the re-admit
				// overlap). Keep the cached session — the triggering
				// request is riding it until expiry — and stop; the
				// once-per-expiry guard prevents a retry storm.
				return errors.New("session: pre-emptive re-admit refused (old session still active)")
			}
			m.mu.Lock()
			m.commit(nil)
			m.mu.Unlock()
			m.recordInvalidation(tableReason(status))
			slog.Debug("session recreated", "reason", tableReason(status), "status", status, "instance_id", st.InstanceID)
		case "banned", "country_blocked", "rate_limited", "ip_capped", "spend_limited", "session_model_mismatch", "limited_ip":
			return statusError(status, st)
		case "model_locked":
			// Previous session is locked to a different model.
			// Release the old slot and retry with the desired model.
			m.mu.Lock()
			m.commit(nil)
			m.mu.Unlock()
			m.recordInvalidation(reasonModelLock)
			m.recordModelLock(st.CurrentModel, targetModel)
			_ = m.client.EndSession(ctx)
			slog.Debug("session released on model lock, retrying", "reason", reasonModelLock, "current", st.CurrentModel, "target", targetModel)
		case "model_unavailable":
			// Requested model is not available; fall back to default model.
			// Issue #158: cache the refusal (with its availability window,
			// when the body carries one) so subsequent admissions for this
			// model short-circuit to the fallback without the 409
			// roundtrip. A live refusal is now rare (once per TTL per
			// model); the frequent skip path logs at DEBUG in refresh.
			m.recordModelUnavailable(targetModel, st.UnavailableWindow, st.AvailableHours)
			slog.Warn("session: model unavailable upstream, falling back to default", "requested", targetModel, "fallback", DefaultFallbackModel, "available_hours", st.AvailableHours)
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
	slog.Debug("session ended", "instance_id", instanceID, "reason", reasonEnded)
	// A superseded DELETE is the same "slot already gone" case as
	// session-invalid (#119): swallow both so teardown never errors on a
	// slot another instance took over.
	if err := m.client.EndSession(ctx); err != nil && !errors.Is(err, upstream.ErrSessionInvalid) && !errors.Is(err, upstream.ErrSessionSuperseded) {
		return err
	}
	return nil
}

// Shutdown handles session teardown at process shutdown. Per the CLI
// (gap #13), exit ALWAYS releases the upstream session slot (DELETE),
// whether or not persistence is enabled — the CLI DELETEs on exit and a
// later restart re-admits fresh. When persistence is enabled the cached
// state is flushed to the store FIRST (so a crash mid-shutdown does not
// lose the entry) and the entry survives the DELETE: a restart resumes via
// pollPersisted, which re-adopts the slot when the DELETE did not take
// effect upstream, or drops the dead entry and re-POSTs fresh when it did.
// Issue #155: when persistence is enabled and the cached session is active
// on a scarce model with remaining time >0, Shutdown SKIPS the upstream
// DELETE and keeps the store entry for restart resume via pollPersisted.
// Runs are FINISHed separately by the run manager.
func (m *Manager) Shutdown(ctx context.Context) error {
	if m.store == nil {
		// No persistence: exactly the normal EndSession path (DELETE
		// upstream + drop the cache).
		return m.EndSession(ctx)
	}
	m.mu.Lock()
	instanceID := ""
	scarceKeep := false
	if m.state != nil && m.state.instanceID != "" {
		instanceID = m.state.instanceID
		// Issue #155: keep scarce active sessions with remaining lifetime.
		if m.state.status == "active" && m.scarce[m.state.model] && !m.state.expiresAt.IsZero() && time.Until(m.state.expiresAt) > 0 {
			scarceKeep = true
		}
		m.store.Save(m.key, m.state)
		// Surface a failed flush: without the persisted entry a restart
		// cannot resume the slot, so a write/rename failure must not be
		// silent. Re-read the FILE through a fresh Store — the in-memory
		// map is updated before the flush attempt and cannot verify disk.
		if persisted := NewStore(m.store.path).Load(m.key); persisted == nil || persisted.instanceID != instanceID {
			slog.Warn("session: shutdown persist failed", "instance_id", shortInstance(instanceID))
		}
	}
	m.mu.Unlock()

	if instanceID == "" {
		return nil
	}
	if scarceKeep {
		slog.Debug("session kept on shutdown (scarce)", "instance_id", shortInstance(instanceID))
		return nil
	}
	// Release the upstream slot directly (not EndSession): EndSession's CAS
	// commit(nil) would remove the store entry we just flushed, and the
	// DELETE is keyed on the user, not the instance (reference/freebuff
	// session wire: DELETE = Bearer only, #120 — EndSession never sends the
	// instance header). The cached state is kept in-memory so the store
	// entry stays; the process is exiting.
	slog.Debug("session ended on shutdown", "instance_id", shortInstance(instanceID), "reason", reasonShutdown)
	if err := m.client.EndSession(ctx); err != nil && !errors.Is(err, upstream.ErrSessionInvalid) && !errors.Is(err, upstream.ErrSessionSuperseded) {
		return err
	}
	return nil
}
