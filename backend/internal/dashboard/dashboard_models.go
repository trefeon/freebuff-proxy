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
	ID          string   `json:"id"`
	DisplayName string   `json:"display_name,omitempty"`
	Tagline     string   `json:"tagline,omitempty"`
	Notice      string   `json:"notice,omitempty"`
	Badges      []string `json:"badges,omitempty"`
	Price       float64  `json:"price"`
	PriceLabel  string   `json:"price_label,omitempty"`
	Pool        string   `json:"pool,omitempty"`
	Agent       string   `json:"agent"`
	Quota       string   `json:"quota"`
	Served      bool     `json:"served"`
	Efforts     []string `json:"efforts,omitempty"`
}

type modelMeta struct {
	DisplayName  string
	Tagline      string
	Badges       []string
	Notice       string
	DefaultPrice float64
}

// defaultModelMeta pins the official Freebuff CLI TUI display information
// (reference/freebuff/common/src/constants/freebuff-models.ts & freebucks.ts):
// display names, taglines, badges, notices, and default Freebucks/hr prices.
var defaultModelMeta = map[string]modelMeta{
	"upstage/solar-pro4": {
		DisplayName:  "Solar Pro 4",
		Tagline:      "Fast & Direct",
		Notice:       "Labor Day weekend (through Sep 7 PT)",
		DefaultPrice: 0,
	},
	"z-ai/glm-5.3-flash": {
		DisplayName:  "GLM 5.3 Flash",
		Tagline:      "Deep reasoning",
		Badges:       []string{"Reasoning: max*", "Images", "NEW"},
		DefaultPrice: 5,
	},
	"mimo/mimo-v2.5": {
		DisplayName:  "MiMo 2.5",
		Tagline:      "Balanced",
		Badges:       []string{"Images"},
		DefaultPrice: 10,
	},
	"deepseek/deepseek-v4-flash": {
		DisplayName:  "DeepSeek V4 Flash 07/31",
		Tagline:      "Smart & Fast",
		Badges:       []string{"Reasoning: high", "NEW"},
		Notice:       "May use data for AI training",
		DefaultPrice: 15,
	},
	"meta/muse-spark-1.3-contributor": {
		DisplayName:  "Muse Spark 1.3",
		Tagline:      "Queues, then falls back",
		Badges:       []string{"Reasoning: xhigh", "NEW"},
		Notice:       "May use data for AI training",
		DefaultPrice: 15,
	},
	"openai/gpt-5.6-luna": {
		DisplayName:  "GPT-5.6 Luna",
		Tagline:      "Strong all-around",
		Badges:       []string{"Reasoning: high", "Images"},
		DefaultPrice: 20,
	},
	"z-ai/glm-5.2": {
		DisplayName:  "GLM 5.2",
		Tagline:      "Referral reward",
		Badges:       []string{"Referral only"},
		Notice:       "Unlocked via referral code",
		DefaultPrice: 0,
	},
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

type unmeteredRow struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// unmeteredModels derives the unlimited-session rows from modelcat (issue
// #342): served models outside the shared premium pool (IsPremium covers
// both the Premium flag and per-model Cap pools, so capped promo rows stay
// metered). Deliberately NOT derived from quota rows — compact session
// polls omit quota fields, which would falsely mark every model unmetered
// between full admissions. Sorted for stable payloads.
func unmeteredModels(reg *registry.Registry) []unmeteredRow {
	out := make([]unmeteredRow, 0, 8)
	for _, id := range servedModels(reg) {
		if modelcat.IsPremium(id) {
			continue
		}
		out = append(out, unmeteredRow{ID: id, Name: modelcat.DisplayName(id)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
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

// firstFreebucksPrices returns the first token snapshot's Freebucks price
// map (issue #350 price sort). Prices are parse-time effective (the
// announced schedule is already applied), so the sort reads the same
// numbers the pool meter gates on. nil when no token reports prices.
func (d *Dashboard) firstFreebucksPrices() map[string]float64 {
	if d.pool == nil {
		return nil
	}
	for _, t := range d.pool.Snapshot() {
		if t.Freebucks != nil && len(t.Freebucks.Prices) > 0 {
			return t.Freebucks.Prices
		}
	}
	return nil
}

// firstFreebucksPriceNotices returns the first token snapshot's Freebucks
// price-notices map (live promo taglines). nil when absent.
func (d *Dashboard) firstFreebucksPriceNotices() map[string]string {
	if d.pool == nil {
		return nil
	}
	for _, t := range d.pool.Snapshot() {
		if t.Freebucks != nil && len(t.Freebucks.PriceNotices) > 0 {
			return t.Freebucks.PriceNotices
		}
	}
	return nil
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
	// must never be presented as servable. One exception: the referral
	// row (GLM 5.2) is listed with Served=false so users discover the
	// grant path; the quota label ("referral +1/day") carries the terms.
	livePrices := d.firstFreebucksPrices()
	liveNotices := d.firstFreebucksPriceNotices()
	effectivePrices := make(map[string]float64)
	for _, id := range d.reg.Models() {
		served := modelcat.IsServed(id)
		if !served && id != modelcat.Glm52ModelID {
			continue
		}
		row := modelRow{
			ID:      id,
			Served:  served,
			Efforts: modelcat.Efforts(id),
		}
		if meta, ok := defaultModelMeta[id]; ok {
			row.DisplayName = meta.DisplayName
			row.Tagline = meta.Tagline
			row.Badges = meta.Badges
			row.Notice = meta.Notice
			row.Price = meta.DefaultPrice
		} else {
			row.DisplayName = modelcat.DisplayName(id)
		}
		if p, ok := livePrices[id]; ok {
			row.Price = p
		}
		if n, ok := liveNotices[id]; ok && n != "" {
			row.Notice = n
		}
		if id == modelcat.Glm52ModelID {
			row.PriceLabel = "Referral grant"
			row.Pool = "—"
		} else if row.Price == 0 {
			row.PriceLabel = "0 Freebucks/hr"
			row.Pool = "unlimited"
			effectivePrices[id] = 0
		} else {
			row.PriceLabel = fmt.Sprintf("%s Freebucks/hr", formatSessionUnits(row.Price))
			if modelcat.IsPremium(id) {
				row.Pool = "premium"
			} else {
				row.Pool = "unlimited"
			}
			effectivePrices[id] = row.Price
		}
		if agent, err := d.reg.AgentForModel(id); err == nil {
			row.Agent = agent
		}
		row.Quota = d.quotaFor(id)
		md.Models = append(md.Models, row)
	}
	// Metered price order (issue #350 — mirrors sortModelsByPrice in
	// cli/src/utils/freebucks.ts): rows sort cheapest-first so the menu's
	// subject (cost) leads; ties break on display name, unpriced/referral
	// rows sort last.
	sortModelRowsByPrice(md.Models, effectivePrices)
	md.Count = len(md.Models)
	cfg := d.cfg()
	for alias, real := range cfg.ModelAliases {
		md.Aliases = append(md.Aliases, aliasRow{Alias: alias, Real: real})
	}
	sort.Slice(md.Aliases, func(i, j int) bool { return md.Aliases[i].Alias < md.Aliases[j].Alias })
	md.HasAliases = len(md.Aliases) > 0
	return md
}

// sortModelRowsByPrice orders catalog rows cheapest-first by the metered
// price map (pure form of the modelsData sort, kept separate for testing).
func sortModelRowsByPrice(rows []modelRow, prices map[string]float64) {
	sort.SliceStable(rows, func(i, j int) bool {
		pi, iok := prices[rows[i].ID]
		pj, jok := prices[rows[j].ID]
		if iok != jok {
			return iok
		}
		if iok && pi != pj {
			return pi < pj
		}
		return modelcat.DisplayName(rows[i].ID) < modelcat.DisplayName(rows[j].ID)
	})
}
