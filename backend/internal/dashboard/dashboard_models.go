package dashboard

import (
	"fmt"
	"sort"
	"strconv"

	"freebuff-proxy/backend/internal/modelcat"
	"freebuff-proxy/backend/internal/registry"
)

// --- models ---

type modelsData struct {
	Models     []modelRow `json:"models"`
	Count      int        `json:"count"`
	Agents     int        `json:"agents"`
	Aliases    []aliasRow `json:"aliases"`
	HasAliases bool       `json:"has_aliases"`
}

type modelRow struct {
	ID    string `json:"id"`
	Agent string `json:"agent"`
	Quota string `json:"quota"`
}

// servedModels returns the registry ids that pass the strict ServedModels
// gate (issue #189 set): the vendor catalog also carries god-only/eval rows
// (luna-es) that must never appear as servable in dashboard/setup views.
func servedModels(reg *registry.Registry) []string {
	out := make([]string, 0, 8)
	for _, id := range reg.Models() {
		if modelcat.IsServed(id) {
			out = append(out, id)
		}
	}
	return out
}

type aliasRow struct {
	Alias string `json:"alias"`
	Real  string `json:"real"`
}

// quotaFor returns the daily session-quota label for a model row. For premium
// pool models (luna, solar-pro4) it prefers the LIVE wire snapshot's limit
// (rateLimitsByModel mirrored per token — server-computed, moves with trust/
// streak/referral bonuses) rendered as "<limit> premium quota", falling back
// to the static "5 premium quota" when no live data exists (5 = default
// entitlement: modelcat.PremiumSessionLimit is the floor 4, but the runtime
// default entitlement is 5/day — floor 4 only with trust levels enforced;
// see modelcat.PremiumSessionLimit comment). Referral GLM 5.2 keeps
// "referral +1/day", and all other served rows are "unlimited session".
// The old live copy "1 of 5 used" was per-single-token usage, which confused
// the catalog view (the table should show the model-level quota, not one
// token's used count); "unmetered" is now "unlimited session" per UX request.
func (d *Dashboard) quotaFor(id string) string {
	if modelcat.IsPremium(id) {
		if live := d.livePremiumQuotaLabel(id); live != "" {
			return live
		}
		return fmt.Sprintf("%s premium quota", formatSessionUnits(float64(modelcat.PremiumSessionLimit+1)))
	}
	if d.pool != nil {
		if live := d.liveQuotaLabel(id); live != "" {
			return live
		}
	}
	if id == modelcat.Glm52ModelID {
		return "referral +1/day"
	}
	return "unlimited session"
}

// livePremiumQuotaLabel renders "<limit> premium quota" from the first token
// quota snapshot carrying an entry for the premium model ("5 premium quota").
// "" when no token has live data for the model. Uses Limit only (not
// RecentCount) — the catalog view shows the model-level quota, not per-token
// usage; per-token usage remains in the Tokens → per-token quota table.
func (d *Dashboard) livePremiumQuotaLabel(id string) string {
	if d.pool == nil {
		return ""
	}
	for _, t := range d.pool.Snapshot() {
		if q, ok := t.QuotaByModel[id]; ok && q.Limit > 0 {
			return fmt.Sprintf("%s premium quota", formatSessionUnits(q.Limit))
		}
	}
	return ""
}

// liveQuotaLabel renders "used of limit" from the first token quota snapshot
// carrying an entry for the model ("1.6 of 5 used" — the CLI's fractional
// unit display). "" when no token has live data for the model. Kept for
// non-premium rows (e.g. referral GLM 5.2 promo after it gains live data).
func (d *Dashboard) liveQuotaLabel(id string) string {
	if d.pool == nil {
		return ""
	}
	for _, t := range d.pool.Snapshot() {
		if q, ok := t.QuotaByModel[id]; ok && q.Limit > 0 {
			return fmt.Sprintf("%s of %s used", formatSessionUnits(q.RecentCount), formatSessionUnits(q.Limit))
		}
	}
	return ""
}

// formatSessionUnits mirrors the CLI's unit display
// (format-session-units.ts): integers render bare, fractionals to one
// decimal — a long run can consume 1.3 sessions and billing floors at 0.1.
func formatSessionUnits(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

func (d *Dashboard) modelsData() modelsData {
	md := modelsData{Count: d.reg.ModelCount(), Agents: len(d.reg.AgentIDs())}
	// Served gate: the dashboard shows the models this proxy actually
	// serves (issue #189 strict set), not the raw upstream registry — the
	// vendor catalog now carries god-only/eval rows (e.g. luna-es) that
	// must never be presented as servable.
	for _, id := range d.reg.Models() {
		if !modelcat.IsServed(id) {
			continue
		}
		row := modelRow{ID: id}
		if agent, err := d.reg.AgentForModel(id); err == nil {
			row.Agent = agent
		}
		row.Quota = d.quotaFor(id)
		md.Models = append(md.Models, row)
	}
	md.Count = len(md.Models)
	cfg := d.cfg()
	for alias, real := range cfg.ModelAliases {
		md.Aliases = append(md.Aliases, aliasRow{Alias: alias, Real: real})
	}
	sort.Slice(md.Aliases, func(i, j int) bool { return md.Aliases[i].Alias < md.Aliases[j].Alias })
	md.HasAliases = len(md.Aliases) > 0
	return md
}
