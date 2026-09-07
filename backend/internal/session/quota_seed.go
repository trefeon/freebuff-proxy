package session

import (
	"time"

	"freebuff-proxy/backend/internal/upstream"
)

// SeedQuota installs one persisted quota row (ADR-0024 boot seed) as
// last-known quota. It reports whether the row was applied.
//
// Rules: a live session owns the view — when this process already holds
// cached state, the seed is a no-op (live data is always fresher than a
// persisted row). Otherwise the row wins only when its probe time is newer
// than the current source for the model (per-model Unix-millis compare
// against savedQuotaSrcAt: poll time for disk-restored rows, write time for
// live probe commits). Applied rows are marked stale with their probe time,
// so the dashboard renders last-known values and the pool scheduler learns
// reset_at from the seed. Re-pushing the same rows is a no-op (idempotent);
// a zero probe time never applies.
func (m *Manager) SeedQuota(q upstream.ModelQuota, probedAt time.Time) bool {
	if q.Model == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != nil {
		return false
	}
	probed := probedAt.UnixMilli()
	if cur := m.snap.savedQuotaSrcAt[q.Model]; probed <= cur {
		return false
	}
	if m.snap.savedQuota == nil {
		m.snap.savedQuota = make(map[string]upstream.ModelQuota, 1)
	}
	m.snap.savedQuota[q.Model] = q
	if m.snap.savedQuotaSrcAt == nil {
		m.snap.savedQuotaSrcAt = make(map[string]int64, 1)
	}
	m.snap.savedQuotaSrcAt[q.Model] = probed
	m.snap.savedQuotaStale = true
	if probedAt.After(m.snap.savedQuotaAt) {
		m.snap.savedQuotaAt = probedAt
	}
	return true
}

// stampQuotaSourceLocked records at as the source time of every model
// currently in savedQuota (wholesale writers: live probe commits, admission
// restores, disk restores). Caller must hold m.mu.
func (m *Manager) stampQuotaSourceLocked(at time.Time) {
	if len(m.snap.savedQuota) == 0 {
		m.snap.savedQuotaSrcAt = nil
		return
	}
	ms := at.UnixMilli()
	src := make(map[string]int64, len(m.snap.savedQuota))
	for model := range m.snap.savedQuota {
		src[model] = ms
	}
	m.snap.savedQuotaSrcAt = src
}
