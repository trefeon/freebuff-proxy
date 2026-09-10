// quota_bootseed.go — quota view boot seeding.
//
// On restart the live quota view would render empty until the next probe:
// this file seeds it from the persisted quota_snapshots (the CLI
// orchestrates: it queries the store and calls this setter; the pool never
// imports the store). Seeded rows carry their probe timestamp, show as
// last-known immediately, and are idempotent (re-pushing changes nothing,
// fresher live data is never downgraded — the session manager compares
// source times).
//
// QUOTA_AUTO_PROBE=false changes nothing here: showing persisted data is
// never probing. The first smart-probe round after Start (see
// quota_smartprobe.go) then refreshes every token live.
package pool

import (
	"time"

	"freebuff-proxy/backend/internal/upstream"
)

// QuotaSeedRow is one latest-per-(token,model) persisted quota sample for
// the boot seed. Token is the pool token index; ProbedAt is the row's probe
// timestamp (TS column); Entitlement is the decoded entitlements object
// (nil when the row carries none).
type QuotaSeedRow struct {
	Token       int
	Model       string
	Limit       float64
	Recent      float64
	ResetAt     time.Time
	ProbedAt    time.Time
	Entitlement map[string]float64
}

// SeedQuotaSnapshot pushes persisted quota rows into the live quota view.
// Rows for unknown token indexes or empty models are dropped.
//
// Call once at boot before Start; it touches no locks beyond the session
// managers' own.
func (p *Pool) SeedQuotaSnapshot(rows []QuotaSeedRow) {
	toks := p.roster.Load()
	if toks == nil {
		return
	}
	applied := 0
	for _, r := range rows {
		if r.Token < 0 || r.Token >= len(*toks) || r.Model == "" {
			continue
		}
		q := upstream.ModelQuota{
			Model:       r.Model,
			Limit:       r.Limit,
			RecentCount: r.Recent,
			ResetAt:     r.ResetAt,
			Entitlement: r.Entitlement,
		}
		if (*toks)[r.Token].session.SeedQuota(q, r.ProbedAt) {
			applied++
		}
	}
	if applied > 0 {
		p.logger.Debug("pool: quota boot seed applied", "rows", applied)
	}
}
