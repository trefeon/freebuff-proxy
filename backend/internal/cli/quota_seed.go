package cli

import (
	"encoding/json"
	"log/slog"
	"time"

	"freebuff-proxy/backend/internal/pool"
	history "freebuff-proxy/backend/internal/store"
)

// quotaSeedStoreLimit bounds the boot-seed read: one row per (token, model)
// group, so a handful of tokens times the served catalog is far below this.
const quotaSeedStoreLimit = 2000

// quotaSeedStore is the store half of the quota boot-seed seam (ADR-0024):
// satisfied by *history.Store in Serve, by fakes in tests.
type quotaSeedStore interface {
	LatestQuotaSnapshots(limit int) ([]history.QuotaSnapshot, error)
}

// quotaSeedPool is the pool half of the seam: satisfied by *pool.Pool in
// Serve, by fakes in tests. The pool never imports the store — this package
// maps rows across the boundary.
type quotaSeedPool interface {
	SeedQuotaSnapshot(rows []pool.QuotaSeedRow)
}

// seedQuotaFromStore loads the latest persisted quota row per (token, model)
// and pushes it into the pool's live view (ADR-0024 boot seeding), so a
// restart shows last-known quotas instantly and the auto-probe scheduler
// learns reset_at from the seed. Warn-only: any failure (or nil store)
// keeps the boot green on the live-only path.
func seedQuotaFromStore(logger *slog.Logger, st quotaSeedStore, p quotaSeedPool, tokenCount int) {
	if st == nil || p == nil || tokenCount <= 0 {
		return
	}
	rows, err := st.LatestQuotaSnapshots(quotaSeedStoreLimit)
	if err != nil {
		logger.Warn("quota boot seed skipped; quota view fills on next probe", "err", err)
		return
	}
	seeds := make([]pool.QuotaSeedRow, 0, len(rows))
	for _, r := range rows {
		s, ok := quotaSeedRow(r, tokenCount)
		if !ok {
			continue
		}
		seeds = append(seeds, s)
	}
	if len(seeds) == 0 {
		return
	}
	p.SeedQuotaSnapshot(seeds)
	logger.Info("quota boot seed applied", "rows", len(seeds))
}

// quotaSeedRow maps one persisted row onto the pool seed shape. Rows for
// token indexes outside the configured pool (stale rows from a removed
// token) or with an empty model are dropped. A corrupt entitlements blob
// degrades to nil — limit/recent/reset still seed the view.
func quotaSeedRow(q history.QuotaSnapshot, tokenCount int) (pool.QuotaSeedRow, bool) {
	if q.TokenIdx < 0 || q.TokenIdx >= tokenCount || q.Model == "" {
		return pool.QuotaSeedRow{}, false
	}
	var s pool.QuotaSeedRow
	s.Token = q.TokenIdx
	s.Model = q.Model
	s.Limit = q.Limit
	s.Recent = q.Recent
	if q.ResetAt > 0 {
		s.ResetAt = time.UnixMilli(q.ResetAt)
	}
	if q.TS > 0 {
		s.ProbedAt = time.UnixMilli(q.TS)
	}
	if q.Entitlements != "" {
		var ent map[string]float64
		if err := json.Unmarshal([]byte(q.Entitlements), &ent); err == nil {
			s.Entitlement = ent
		}
	}
	return s, true
}
