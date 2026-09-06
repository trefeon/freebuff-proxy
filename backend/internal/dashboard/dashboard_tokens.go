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
	UnmeteredModels   []unmeteredRow `json:"unmetered_models,omitempty"`
	TokenRotation     string         `json:"token_rotation,omitempty"`
	RateLimitFailover bool           `json:"rate_limit_failover"`
	MaturityEnabled   bool           `json:"maturity_enabled"`
}

// tokenSessionQuota is the per-token session + quota block, identical on the
// full snapshot and the ?view=live hot poll. Both tokenDetail and
// tokenLiveDetail embed it, so the live projection can never drift from the
// full shape: adding a session/quota field here reaches both views, and
// sessionQuotaFor synthesizes it once for both builders.
type tokenSessionQuota struct {
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

type tokenDetail struct {
	tokenCard
	tokenSessionQuota
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
	mode := cfg.EffectiveMode()
	td := tokensData{
		BridgeTokens:      d.pool.BridgeCount(),
		TokenCount:        d.pool.TokenCount(),
		Mode:              mode,
		InBridge:          mode == "bridge",
		TokenRotation:     cfg.TokenRotation,
		RateLimitFailover: cfg.RateLimitFailover,
		MaturityEnabled:   cfg.MaturityEnabled,
	}
	// client cards. Pure bridge hides the (empty) pooled table; pure pooled
	// has no bridge cards.
	td.ShowBridge = td.Mode == "bridge" || td.Mode == "hybrid"
	for _, t := range d.pool.Snapshot() {
		td.Tokens = append(td.Tokens, tokenDetail{
			tokenCard:         cardFromSnapshot(t),
			tokenSessionQuota: d.sessionQuotaFor(t, true),
		})
	}
	td.HasTokens = len(td.Tokens) > 0
	td.UnmeteredModels = unmeteredModels(d.reg)
	// Bridge token cards (#187): live snapshots of bridge-mode entries.
	td.BridgeTokenCards = d.bridgeCards(td.ShowBridge)
	return td
}

// sessionQuotaFor synthesizes the session + quota block for one pool
// snapshot. Single-sourced: the full tokensData builder and the ?view=live
// projection both call it, so quota-row math (usage bars, promo rows,
// entitlement labels) can never disagree between views. Only the full view
// samples history (sample=true): the hot poll reuses the synthesis without
// touching the store.
func (d *Dashboard) sessionQuotaFor(t pool.TokenSnapshot, sample bool) tokenSessionQuota {
	sq := tokenSessionQuota{
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
		if sample {
			ent, _ := json.Marshal(q.Entitlement)
			d.sampleQuota(t.Token, model, q.Limit, q.RecentCount, q.ResetAt, string(ent))
		}
		sq.Quota = append(sq.Quota, row)
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
			sq.Quota = append(sq.Quota, glmRow)
			if sample {
				d.sampleQuota(t.Token, modelcat.Glm52ModelID, gp.DailySessions, 0, resetAt, "")
			}
		}
	}
	sort.Slice(sq.Quota, func(i, j int) bool { return sq.Quota[i].Model < sq.Quota[j].Model })
	sq.HasQuota = len(sq.Quota) > 0
	return sq
}

// tokenLiveDetail is the hot-poll subset of tokenDetail (issue #322): the
// live card plus the shared session/quota block. Account-stable card fields
// (email, account_id, daily_limit, standing_*, referral_*) ride the
// once-per-mount full fetch; the SPA merges them back by index.
type tokenLiveDetail struct {
	tokenLiveCard
	tokenSessionQuota
}

// tokensLiveData is the hot-poll subset of tokensData: live numbers only.
type tokensLiveData struct {
	BridgeTokens      int               `json:"bridge_tokens"`
	BridgeTokenCards  []bridgeTokenCard `json:"bridge_token_cards,omitempty"`
	TokenCount        int               `json:"token_count"`
	Tokens            []tokenLiveDetail `json:"tokens"`
	HasTokens         bool              `json:"has_tokens"`
	TokenRotation     string            `json:"token_rotation,omitempty"`
	RateLimitFailover bool              `json:"rate_limit_failover"`
	MaturityEnabled   bool              `json:"maturity_enabled"`
}

// tokensLiveData builds the 10s hot-poll payload directly from pool
// snapshots: the live card plus the shared session/quota synthesis. It never
// builds the full snapshot, so account-stable fields cannot leak into the
// hot poll by construction — there is no strip list to keep in sync.
func (d *Dashboard) tokensLiveData() tokensLiveData {
	cfg := d.cfg()
	mode := cfg.EffectiveMode()
	live := tokensLiveData{
		BridgeTokens:      d.pool.BridgeCount(),
		TokenCount:        d.pool.TokenCount(),
		TokenRotation:     cfg.TokenRotation,
		RateLimitFailover: cfg.RateLimitFailover,
		MaturityEnabled:   cfg.MaturityEnabled,
	}
	showBridge := mode == "bridge" || mode == "hybrid"
	live.BridgeTokenCards = d.bridgeCards(showBridge)
	for _, t := range d.pool.Snapshot() {
		live.Tokens = append(live.Tokens, tokenLiveDetail{
			tokenLiveCard:     liveCardFromSnapshot(t),
			tokenSessionQuota: d.sessionQuotaFor(t, false),
		})
	}
	live.HasTokens = len(live.Tokens) > 0
	return live
}

// bridgeCards snapshots bridge-mode entries for the client cards. Pure pooled
// mode has none; both the full and live tokens builders share it.
func (d *Dashboard) bridgeCards(show bool) []bridgeTokenCard {
	if !show {
		return nil
	}
	var out []bridgeTokenCard
	for _, snap := range d.pool.BridgeSnapshot() {
		out = append(out, bridgeCardFromSnapshot(snap))
	}
	return out
}
