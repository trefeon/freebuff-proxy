package dashboard

import (
	"encoding/json"
	"freebuff-proxy/backend/internal/modelcat"
	"freebuff-proxy/backend/internal/pool"
	"sort"
	"time"
)

// --- tokens ---

type tokensData struct {
	Mode             string            `json:"mode"`
	InBridge         bool              `json:"in_bridge"`
	ShowBridge       bool              `json:"show_bridge"`
	BridgeTokens     int               `json:"bridge_tokens"`
	BridgeTokenCards []bridgeTokenCard `json:"bridge_token_cards,omitempty"`
	TokenCount       int               `json:"token_count"`
	Tokens           []tokenDetail     `json:"tokens"`
	HasTokens        bool              `json:"has_tokens"`
	// UnmeteredModels is the modelcat-derived unlimited-session rows
	// (issue #342); the SPA falls back to its static list when absent.
	UnmeteredModels []unmeteredRow `json:"unmetered_models,omitempty"`
}

type tokenDetail struct {
	tokenCard
	SessionInstance         string     `json:"session_instance"`
	SessionModel            string     `json:"session_model"`
	SessionRemainingSeconds int64      `json:"session_remaining_seconds"`
	SessionExpiresAt        string     `json:"session_expires_at,omitempty"`
	Quota                   []quotaRow `json:"quota"`
	HasQuota                bool       `json:"has_quota"`
	// QuotaStale labels quota restored from the on-disk session entry
	// after a restart; QuotaSavedAt is when it was last polled.
	QuotaStale   bool                       `json:"quota_stale,omitempty"`
	QuotaSavedAt string                     `json:"quota_saved_at,omitempty"`
	PremiumQuota *pool.PremiumQuotaSnapshot `json:"premium_quota,omitempty"`
}

type quotaRow struct {
	Model          string  `json:"model"`
	Pool           string  `json:"pool,omitempty"`
	PoolLabel      string  `json:"pool_label,omitempty"`
	Limit          string  `json:"limit"`
	Recent         string  `json:"recent"`
	Remaining      float64 `json:"remaining"`
	Period         string  `json:"period"`
	ResetAt        string  `json:"reset_at"`
	ResetAtUTC     string  `json:"reset_at_utc"`
	ResetsIn       string  `json:"resets_in"`
	Entitled       string  `json:"entitled"`
	HasEntitlement bool    `json:"has_entitlement"`
	UsagePct       int     `json:"usage_pct"`
	NearLimit      bool    `json:"near_limit"`
	HasBar         bool    `json:"has_bar"`
}

func utcAttr(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (d *Dashboard) tokensData() tokensData {
	cfg := d.cfg()
	td := tokensData{BridgeTokens: d.pool.BridgeCount(), TokenCount: d.pool.TokenCount(), Mode: cfg.EffectiveMode()}
	td.InBridge = td.Mode == "bridge"
	// Hybrid mode shows BOTH surfaces: the pooled table and the live bridge
	// client cards. Pure bridge hides the (empty) pooled table; pure pooled
	// has no bridge cards.
	td.ShowBridge = td.Mode == "bridge" || td.Mode == "hybrid"
	for _, t := range d.pool.Snapshot() {
		detail := tokenDetail{
			tokenCard:               cardFromSnapshot(t),
			SessionInstance:         t.SessionInstanceID,
			SessionModel:            t.SessionModel,
			SessionRemainingSeconds: t.SessionRemainingSeconds,
			SessionExpiresAt:        utcAttr(t.SessionExpiresAt),
			QuotaStale:              t.QuotaStale,
			QuotaSavedAt:            utcAttr(t.QuotaSavedAt),
			PremiumQuota:            t.PremiumQuota,
		}
		for model, q := range t.QuotaByModel {
			if !modelcat.IsServed(model) {
				// Only reverse-engineer and display models the official CLI
				// truly serves. Unserved web models in upstream's ledger
				// (kimi-k3-eco, muse-spark, luna-es) are ignored.
				continue
			}
			rem := float64(0)
			if q.Limit > 0 {
				rem = q.Limit - q.RecentCount
				if rem < 0 {
					rem = 0
				}
			}
			row := quotaRow{
				Model:      model,
				Pool:       q.Pool,
				PoolLabel:  q.PoolLabel,
				Limit:      formatQuota(q.Limit),
				Recent:     formatQuota(q.RecentCount),
				Remaining:  rem,
				Period:     q.Period,
				ResetAt:    shortTime(q.ResetAt),
				ResetAtUTC: utcAttr(q.ResetAt),
			}
			if q.Limit > 0 {
				row.UsagePct = int(q.RecentCount * 100 / q.Limit)
				if row.UsagePct > 100 {
					row.UsagePct = 100
				}
				row.NearLimit = row.UsagePct >= 80
				row.HasBar = true
			}
			if !q.ResetAt.IsZero() {
				if d := time.Until(q.ResetAt); d > 0 {
					row.ResetsIn = "in " + humanDuration(d)
				}
			}
			if len(q.Entitlement) > 0 {
				row.Entitled = formatEntitlement(q.Entitlement)
				row.HasEntitlement = true
			}
			detail.Quota = append(detail.Quota, row)
		}

		// Scarcity/promo isolation (issue #178): the upstream glmPromo block
		// ({dailySessions, endsAt}) grants a referral quota on limited models
		// like GLM/Luna/Pro. Synthesize a dashboard row for z-ai/glm-5.2 so
		// the promo is visible even though no per-model quota was admitted;
		// a real rateLimitsByModel entry for the model wins over the promo.
		if _, exists := t.QuotaByModel[modelcat.Glm52ModelID]; !exists && t.GlmPromo != "" {
			var gp struct {
				DailySessions float64 `json:"dailySessions"`
				EndsAt        string  `json:"endsAt"`
			}
			if err := json.Unmarshal([]byte(t.GlmPromo), &gp); err == nil && gp.DailySessions > 0 {
				var resetAt time.Time
				if ts, err := time.Parse(time.RFC3339, gp.EndsAt); err == nil {
					resetAt = ts
				}
				resetsIn := ""
				// Guard the promo countdown (issue #225): an expired promo has
				// EndsAt in the past, and humanDuration would round the
				// negative duration to a misleading "1m".
				if !resetAt.IsZero() {
					if d := time.Until(resetAt); d > 0 {
						resetsIn = "in " + humanDuration(d)
					}
				}
				glmRow := quotaRow{
					Model:          modelcat.Glm52ModelID,
					Limit:          formatQuota(gp.DailySessions),
					Recent:         "0",
					Remaining:      gp.DailySessions,
					Period:         "promo",
					ResetAt:        shortTime(resetAt),
					ResetAtUTC:     utcAttr(resetAt),
					ResetsIn:       resetsIn,
					Entitled:       "referral",
					HasEntitlement: true,
					UsagePct:       0,
					HasBar:         true,
				}
				detail.Quota = append(detail.Quota, glmRow)
			}
		}
		sort.Slice(detail.Quota, func(i, j int) bool { return detail.Quota[i].Model < detail.Quota[j].Model })
		detail.HasQuota = len(detail.Quota) > 0
		td.Tokens = append(td.Tokens, detail)
	}
	td.HasTokens = len(td.Tokens) > 0
	td.UnmeteredModels = unmeteredModels(d.reg)
	// Bridge token cards (#187): live snapshots of bridge-mode entries.
	if td.ShowBridge {
		for _, snap := range d.pool.BridgeSnapshot() {
			td.BridgeTokenCards = append(td.BridgeTokenCards, bridgeCardFromSnapshot(snap))
		}
	}
	return td
}

// tokenLiveDetail is the hot-poll subset of tokenDetail (issue #322): the
// live card plus session and quota rows. Account-stable card fields
// (email, account_id, daily_limit, standing_*, referral_*) ride the
// once-per-mount full fetch; the SPA merges them back by index.
type tokenLiveDetail struct {
	tokenLiveCard
	SessionInstance         string                     `json:"session_instance"`
	SessionModel            string                     `json:"session_model"`
	SessionRemainingSeconds int64                      `json:"session_remaining_seconds"`
	SessionExpiresAt        string                     `json:"session_expires_at,omitempty"`
	Quota                   []quotaRow                 `json:"quota"`
	HasQuota                bool                       `json:"has_quota"`
	QuotaStale              bool                       `json:"quota_stale,omitempty"`
	QuotaSavedAt            string                     `json:"quota_saved_at,omitempty"`
	PremiumQuota            *pool.PremiumQuotaSnapshot `json:"premium_quota,omitempty"`
}

// tokensLiveData is the hot-poll subset of tokensData: live numbers only.
type tokensLiveData struct {
	BridgeTokens     int               `json:"bridge_tokens"`
	BridgeTokenCards []bridgeTokenCard `json:"bridge_token_cards,omitempty"`
	TokenCount       int               `json:"token_count"`
	Tokens           []tokenLiveDetail `json:"tokens"`
	HasTokens        bool              `json:"has_tokens"`
}

// tokensLiveData builds the 10s hot-poll payload by stripping the
// account-stable card fields off the full snapshot. Quota-row synthesis
// stays single-sourced in tokensData; this only projects.
func (d *Dashboard) tokensLiveData() tokensLiveData {
	full := d.tokensData()
	live := tokensLiveData{
		BridgeTokens:     full.BridgeTokens,
		BridgeTokenCards: full.BridgeTokenCards,
		TokenCount:       full.TokenCount,
		HasTokens:        full.HasTokens,
	}
	for _, tok := range full.Tokens {
		c := tok.tokenCard
		live.Tokens = append(live.Tokens, tokenLiveDetail{
			tokenLiveCard: tokenLiveCard{
				Index:            c.Index,
				SessionStatus:    c.SessionStatus,
				QueuePosition:    c.QueuePosition,
				QueueDepth:       c.QueueDepth,
				ActiveRuns:       c.ActiveRuns,
				Requests:         c.Requests,
				Messages24h:      c.Messages24h,
				UsagePct:         c.UsagePct,
				RiskLevel:        c.RiskLevel,
				CooldownActive:   c.CooldownActive,
				CooldownUntil:    c.CooldownUntil,
				Locked:           c.Locked,
				BanType:          c.BanType,
				BannedUntil:      c.BannedUntil,
				TransientRetries: c.TransientRetries,
				Streak:           c.Streak,
				TodayUsed:        c.TodayUsed,
				LastUsage:        c.LastUsage,
				StreakUpdatedAt:  c.StreakUpdatedAt,
				Freebucks:        c.Freebucks,
				FreeWindows:      c.FreeWindows,
				Subscription:     c.Subscription,
			},
			SessionInstance:         tok.SessionInstance,
			SessionModel:            tok.SessionModel,
			SessionRemainingSeconds: tok.SessionRemainingSeconds,
			SessionExpiresAt:        tok.SessionExpiresAt,
			Quota:                   tok.Quota,
			HasQuota:                tok.HasQuota,
			QuotaStale:              tok.QuotaStale,
			QuotaSavedAt:            tok.QuotaSavedAt,
			PremiumQuota:            tok.PremiumQuota,
		})
	}
	return live
}
