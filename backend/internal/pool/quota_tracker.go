package pool

import (
	"sort"
	"time"

	"freebuff-proxy/backend/internal/modelcat"
	"freebuff-proxy/backend/internal/session"
)

// PremiumQuotaSnapshot is the per-token premium quota view (5/day pacific_day
// shared pool) derived from session.QuotaSnapshot. Mirrors the CLI's TUI
// "N of M used · resets in …" for the PREMIUM header — fractional session
// units (0.1 = 6 min, 1.0 = 60 min) exactly as the server counts them, not
// truncated ints. See reference/freebuff/common/src/types/freebuff-session.ts
// FreebuffSessionRateLimit.recentCount (float) and the clock in
// cli/src/components/freebuff-model-selector.tsx:83.
type PremiumQuotaSnapshot struct {
	Limit       float64   `json:"limit"`
	Used        float64   `json:"used"`
	Remaining   float64   `json:"remaining"`
	Period      string    `json:"period"`
	ResetAt     time.Time `json:"reset_at"`
	PercentUsed int       `json:"percent_used"`
	Entitled    bool      `json:"entitled"`
	Capped      bool      `json:"capped"`
}

// premiumPoolModels is the shared daily premium pool (upstream
// FREEBUFF_PREMIUM_MODEL_IDS). Derived from modelcat so the set follows
// upstream on sync.
var premiumPoolModels = modelcat.SharedPremiumModels()

// isPremiumModel reports whether model is part of the shared premium pool.
// Kept for acquire-path gating and tests.
func isPremiumModel(model string) bool {
	return modelcat.IsPremium(model)
}

func buildPremiumSnapshot(q session.QuotaSnapshot) *PremiumQuotaSnapshot {
	return buildPremiumSnapshotAt(q, time.Now())
}

func buildPremiumSnapshotAt(q session.QuotaSnapshot, now time.Time) *PremiumQuotaSnapshot {
	limit := q.Limit
	used := q.RecentCount
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	percent := 0
	if limit > 0 {
		percent = int(used * 100 / limit)
		if percent > 100 {
			percent = 100
		}
		if percent < 0 {
			percent = 0
		}
	}
	capped := !q.ResetAt.IsZero() && q.ResetAt.After(now) && q.RecentCount >= q.Limit
	entitled := len(q.Entitlement) > 0 || q.Limit > 0
	return &PremiumQuotaSnapshot{Limit: limit, Used: used, Remaining: remaining, Period: q.Period, ResetAt: q.ResetAt, PercentUsed: percent, Entitled: entitled, Capped: capped}
}

func premiumSnapshotFromQuotaMap(m map[string]session.QuotaSnapshot) *PremiumQuotaSnapshot {
	return premiumSnapshotFromQuotaMapAt(m, time.Now())
}

func premiumSnapshotFromQuotaMapAt(m map[string]session.QuotaSnapshot, now time.Time) *PremiumQuotaSnapshot {
	if len(m) == 0 {
		return nil
	}
	var premium *PremiumQuotaSnapshot
	if q, ok := m[premiumPoolModels[0]]; ok {
		premium = buildPremiumSnapshotAt(q, now)
	} else {
		var present []string
		for _, model := range premiumPoolModels {
			if _, ok := m[model]; ok {
				present = append(present, model)
			}
		}
		if len(present) > 0 {
			sort.Strings(present)
			premium = buildPremiumSnapshotAt(m[present[0]], now)
		}
	}
	return premium
}

func (p *Pool) PremiumQuotaForToken(idx int) *PremiumQuotaSnapshot {
	toks := p.roster.Load()
	if toks == nil || idx < 0 || idx >= len(*toks) {
		return nil
	}
	tok := (*toks)[idx]
	if tok == nil || tok.session == nil {
		return nil
	}
	return premiumSnapshotFromQuotaMap(tok.session.Snapshot().QuotaByModel)
}

// PremiumSnapshotForToken is an alias kept for backward compat.
func (p *Pool) PremiumSnapshotForToken(idx int) *PremiumQuotaSnapshot {
	return p.PremiumQuotaForToken(idx)
}

func (p *Pool) PremiumQuotaForBridge(key string) *PremiumQuotaSnapshot {
	p.bridgeMu.RLock()
	entry, ok := p.bridge[key]
	if !ok {
		entry, ok = p.bridge[tokenKey(key)]
	}
	p.bridgeMu.RUnlock()
	if !ok || entry == nil || entry.session == nil {
		return nil
	}
	return premiumSnapshotFromQuotaMap(entry.session.Snapshot().QuotaByModel)
}

func (p *Pool) PremiumSnapshotForBridge(key string) *PremiumQuotaSnapshot {
	return p.PremiumQuotaForBridge(key)
}
