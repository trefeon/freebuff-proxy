package dashboard

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"freebuff-proxy/backend/internal/config"
	"freebuff-proxy/backend/internal/modelcat"
	"freebuff-proxy/backend/internal/pool"
	"freebuff-proxy/backend/internal/registry"
	"freebuff-proxy/backend/internal/upstream"
)

// --- overview ---

type overviewData struct {
	BaseURL              string            `json:"base_url"`
	Mode                 string            `json:"mode"`
	InBridge             bool              `json:"in_bridge"`
	ShowBridge           bool              `json:"show_bridge"`
	BridgeTokens         int               `json:"bridge_tokens"`
	BridgeTokenCards     []bridgeTokenCard `json:"bridge_token_cards,omitempty"`
	Models               []string          `json:"models"`
	ModelCount           int               `json:"model_count"`
	Uptime               string            `json:"uptime"`
	SafeMode             bool              `json:"safe_mode"`
	MaxMessagesPerDay    int               `json:"max_messages_per_day"`
	TransientRetries     int64             `json:"transient_retries"`
	FingerprintRotations int64             `json:"fingerprint_rotations"`
	Tokens               []tokenCard       `json:"tokens"`
	HasTokens            bool              `json:"has_tokens"`
	IsDefaultAdminToken  bool              `json:"is_default_admin_token"`
	RequireLogin         bool              `json:"require_login"`
	// UpstreamSync summarises the latest .github/workflows/upstream-drift
	// run (compiled into the binary). Users on an out-of-date build see
	// HasDrift=true + DriftedFiles and know to update.
	UpstreamSync *upstreamSync `json:"upstream_sync,omitempty"`
}

// upstreamSync is the dashboard-friendly view of the embedded
// backend/internal/dashboard/data/upstream_drift.json. Computed once at request
// time; cheap.
type upstreamSync struct {
	UpstreamSHA  string         `json:"upstream_sha"`            // short SHA, "(not yet reported)" before first CI run
	CheckedAt    string         `json:"checked_at"`              // RFC3339
	HasDrift     bool           `json:"has_drift"`               // any non-SAME file
	HasRegistry  bool           `json:"has_registry_drift"`      // 6 pinned files
	HasWire      bool           `json:"has_wire_drift"`          // wire files MISSING_UPSTREAM
	DriftedFiles []upstreamFile `json:"drifted_files,omitempty"` // the actual changes
	ReleasesURL  string         `json:"releases_url"`            // where to update
}

type upstreamFile struct {
	Group     string `json:"group"` // "registry" | "wire"
	File      string `json:"file"`
	PinnedSHA string `json:"pinned_sha"`
	VendorSHA string `json:"vendor_sha"`
	Status    string `json:"status"` // DRIFT | MISSING_UPSTREAM | SAME
}
type tokenCard struct {
	Index         int    `json:"index"`
	Email         string `json:"email,omitempty"`
	AccountID     string `json:"account_id,omitempty"`
	SessionStatus string `json:"session_status"`
	AccessTier    string `json:"access_tier,omitempty"`
	QueuePosition int    `json:"queue_position"`
	QueueDepth    int    `json:"queue_depth"`
	ActiveRuns    int    `json:"active_runs"`
	Requests      int    `json:"requests"`
	Messages24h   int    `json:"messages_24h"`
	DailyLimit    int    `json:"daily_limit"`
	UsagePct      int    `json:"usage_pct"`
	// Per-token request limits (issue: RPD/RPM): live counters + configured
	// caps (0 = unlimited) + time until the next Pacific midnight (the
	// official daily reset) in seconds, so the dashboard can show usage and
	// lock state per account.
	RequestsPerMinute      int     `json:"requests_per_minute"`
	RequestsPerDay         int     `json:"requests_per_day"`
	RequestsPerMinuteLimit int     `json:"requests_per_minute_limit"`
	RequestsPerDayLimit    int     `json:"requests_per_day_limit"`
	RequestsPerDayResetIn  int     `json:"requests_per_day_reset_in"` // seconds
	RiskLevel              string  `json:"risk_level"`
	CooldownActive         bool    `json:"cooldown_active"`
	CooldownUntil          string  `json:"cooldown_until"`
	Locked                 bool    `json:"locked"`
	BanType                string  `json:"ban_type,omitempty"`
	BannedUntil            string  `json:"banned_until,omitempty"`
	TransientRetries       int64   `json:"transient_retries"`
	AllowlistSkips         int64   `json:"allowlist_skips,omitempty"`
	HasStanding            bool    `json:"has_standing"`
	StandingLevel          string  `json:"standing_level"`
	StandingLabel          string  `json:"standing_label"`
	StandingScore          float64 `json:"standing_score"`
	StandingNextLevel      string  `json:"standing_next_level"`
	StandingNextLevelAt    string  `json:"standing_next_level_at"`
	// Standing cap + earn-back hints (issue #140, FreebuffStandingInfo):
	// cappedBy/cappedReason name the trust cap holding the level, blurb is
	// upstream's human explanation, nextSteps the suggested actions.
	StandingCappedBy     string             `json:"standing_capped_by,omitempty"`
	StandingCappedReason string             `json:"standing_capped_reason,omitempty"`
	StandingBlurb        string             `json:"standing_blurb,omitempty"`
	StandingNextSteps    []standingStepCard `json:"standing_next_steps,omitempty"`
	// Referral (FreebuffReferralInfo): invite program state for this token.
	HasReferral            bool   `json:"has_referral"`
	ReferralCode           string `json:"referral_code,omitempty"`
	ReferralQualifiedCount int    `json:"referral_qualified_count"`
	ReferralSessionsLeft   int    `json:"referral_sessions_left"`
	ReferralGithubLinked   bool   `json:"referral_github_linked"`
	ReferralResetAt        string `json:"referral_reset_at,omitempty"`
	// Freebucks (issue #232): balance + daily/weekly/monthly windows +
	// bindingWindow + prices. Nil when the session has not reported it.
	Freebucks *freebucksCard `json:"freebucks,omitempty"`
	// FreeWindows (issue #319): free-tier session-pool day/week/month
	// windows. Display-only; nil when the session has not reported it.
	FreeWindows *freeWindowsCard `json:"free_windows,omitempty"`
	// Subscription (issue #319): subscriber usage rings + provider spend,
	// rollout-audience only; nil otherwise.
	Subscription *subscriptionCard `json:"subscription,omitempty"`
	// AllowedModels is the slot's MODEL_LOCKS allowlist (issue #325); nil
	// when unlocked. Config-static: rides the full fetch, cached by the SPA.
	AllowedModels   []string `json:"allowed_models,omitempty"`
	Streak          int      `json:"streak,omitempty"`
	TodayUsed       bool     `json:"today_used,omitempty"`
	LastUsage       string   `json:"last_usage,omitempty"`
	StreakUpdatedAt string   `json:"streak_updated_at,omitempty"`
	// Maturity is the streak-maturity automation view (nil until maturity
	// is first enabled for the token).
	Maturity *maturityCard `json:"maturity,omitempty"`
}

// maturityCard is the dashboard view of pool.MaturitySnapshot: automation
// toggle + streak target + touch mode + badge + today's slot + last touch
// (time, action, result, advance) + non-advance warning. Nil when the token
// never opted in.
type maturityCard struct {
	Enabled       bool   `json:"enabled"`
	Target        int    `json:"target"`
	Mode          string `json:"mode"`
	TouchModel    string `json:"touch_model,omitempty"`
	Badge         string `json:"badge,omitempty"`
	Slot          string `json:"slot,omitempty"`
	LastTouch     string `json:"last_touch,omitempty"`
	LastAction    string `json:"last_action,omitempty"`
	LastResult    string `json:"last_result,omitempty"`
	LastAdvanced  string `json:"last_advanced,omitempty"`
	Warn          bool   `json:"warn,omitempty"`
	NoAdvanceDays int    `json:"no_advance_days,omitempty"`
}

// freebucksWindowCard is one window of the Freebucks allowance (issue #232):
// limit/spent/remaining + reset_at (RFC3339 string; empty when zero) +
// percent_used (spent/limit*100, 0 when limit==0). Mirrors
// upstream.FreebucksWindow but with string times and snake_case JSON for the
// dashboard API (daily/weekly/monthly).
type freebucksWindowCard struct {
	Limit       float64 `json:"limit"`
	Spent       float64 `json:"spent"`
	Remaining   float64 `json:"remaining"`
	ResetAt     string  `json:"reset_at,omitempty"`
	PercentUsed float64 `json:"percent_used"`
}

// freebucksCard is the dashboard view of upstream.FreebucksInfo (issue #232,
// shape issue #321): balance + the daily pool window + the never-expiring
// wallet + the USD spend ceiling + the plan id + per-model prices.
// Nil when the session has not reported Freebucks (nil-safe callers check).
// This is now the quota view (ADR-0027 dropped the premium_quota mirror).
type freebucksCard struct {
	Balance float64             `json:"balance"`
	Daily   freebucksWindowCard `json:"daily"`
	Wallet  freebucksWalletCard `json:"wallet"`
	Spend   freebucksSpendCard  `json:"spend"`
	// Monthly is the monthly dollar allowance (wire drift 2026-09-04,
	// issue #330). Nil when the server predates it — the SPA renders
	// nothing rather than a zero that would read as "spent".
	Monthly      *freebucksWindowCard `json:"monthly,omitempty"`
	PlanID       string               `json:"plan_id,omitempty"`
	Prices       map[string]float64   `json:"prices,omitempty"`
	PriceNotices map[string]string    `json:"price_notices,omitempty"`
	// QuotaExempt is the server-authorized quota exemption (wire drift
	// 2026-09-05, issue #350): new sessions stay usable at zero balance.
	QuotaExempt bool `json:"quota_exempt,omitempty"`
}

// freebucksWalletCard is the dashboard view of the never-expiring Freebucks
// wallet: spendable balance, monthly plan bonus (0 on free), and the ISO
// instant the next plan bonus lands (empty when absent).
type freebucksWalletCard struct {
	Balance      float64 `json:"balance"`
	MonthlyBonus float64 `json:"monthly_bonus"`
	NextBonusAt  string  `json:"next_bonus_at,omitempty"`
}

// freebucksSpendCard is the dashboard view of the Freebucks USD spend
// ceiling: the cap plus the ISO instant the day rolls. Upstream deprecated
// the wire field at abd1eed4a (no longer enforced or displayed): new
// servers omit spend and this renders the zero value; legacy servers still
// fill it. No frontend surface reads it.
type freebucksSpendCard struct {
	LimitUsd float64 `json:"limit_usd"`
	ResetAt  string  `json:"reset_at,omitempty"`
}

// freeWindowsCard is the dashboard view of upstream.FreeWindowsInfo
// (issue #319): free-tier session-pool day/week/month used/limit windows.
type freeWindowsCard struct {
	DayUsed      float64 `json:"day_used"`
	DayLimit     float64 `json:"day_limit"`
	WeekUsed     float64 `json:"week_used"`
	WeekLimit    float64 `json:"week_limit"`
	MonthUsed    float64 `json:"month_used"`
	MonthLimit   float64 `json:"month_limit"`
	DayResetAt   string  `json:"day_reset_at,omitempty"`
	MonthResetAt string  `json:"month_reset_at,omitempty"`
}

// subscriptionCard is the dashboard view of upstream.SubscriptionInfo
// (issue #319): subscriber day / five-day / month usage rings + provider
// spend USD. Rollout-audience only. The month_*_spend_usd pair is deprecated
// upstream (abd1eed4a omits it): zero reads as "not reported".
type subscriptionCard struct {
	DayUsed            float64  `json:"day_used"`
	DayLimit           float64  `json:"day_limit"`
	FiveDayUsed        float64  `json:"five_day_used"`
	FiveDayLimit       float64  `json:"five_day_limit"`
	MonthUsed          float64  `json:"month_used"`
	MonthLimit         float64  `json:"month_limit"`
	DayPremiumUsed     float64  `json:"day_premium_used"`
	DayPremiumLimit    float64  `json:"day_premium_limit"`
	DayResetAt         string   `json:"day_reset_at,omitempty"`
	PeriodEndsAt       string   `json:"period_ends_at,omitempty"`
	MonthSpendUsd      float64  `json:"month_spend_usd"`
	MonthSpendLimitUsd float64  `json:"month_spend_limit_usd"`
	FreeDayUsed        *float64 `json:"free_day_used,omitempty"`
	FreeDayLimit       *float64 `json:"free_day_limit,omitempty"`
}

// standingStepCard is one dashboard-ready earn-back action
// (FreebuffTrustNextStep).
type standingStepCard struct {
	ID     string  `json:"id"`
	Label  string  `json:"label"`
	Detail string  `json:"detail,omitempty"`
	Points float64 `json:"points"`
	Href   string  `json:"href,omitempty"`
}

// bridgeTokenCard is a dashboard-ready view of one bridge entry (#187).
type bridgeTokenCard struct {
	Key           string         `json:"key"`    // masked hash prefix
	Status        string         `json:"status"` // active|cooldown|locked
	Model         string         `json:"model"`
	ActiveRuns    int            `json:"active_runs"`
	Requests      int            `json:"requests"`
	Locked        bool           `json:"locked"`
	CooldownUntil string         `json:"cooldown_until"`
	SessionActive bool           `json:"session_active"`
	SpendDay      float64        `json:"spend_day"`
	SpendPct      int            `json:"spend_pct"`
	BanType       string         `json:"ban_type,omitempty"`
	BannedUntil   string         `json:"banned_until,omitempty"`
	Freebucks     *freebucksCard `json:"freebucks,omitempty"`
}

func bridgeCardFromSnapshot(snap pool.BridgeTokenSnapshot) bridgeTokenCard {
	status := "active"
	if snap.Locked {
		status = "locked"
	} else if snap.CooldownUntil.After(time.Now()) {
		status = "cooldown"
	}
	bannedUntil := ""
	if !snap.BannedUntil.IsZero() && snap.BanType == "temporary" {
		bannedUntil = snap.BannedUntil.Format(time.RFC3339)
	}
	return bridgeTokenCard{
		Key:           shortKey(snap.Key),
		Status:        status,
		Model:         snap.Model,
		ActiveRuns:    snap.ActiveRuns,
		Requests:      snap.Requests,
		Locked:        snap.Locked,
		CooldownUntil: shortTime(snap.CooldownUntil),
		SessionActive: snap.SessionActive,
		SpendDay:      snap.SpendDay,
		SpendPct:      snap.SpendPct,
		BanType:       snap.BanType,
		BannedUntil:   bannedUntil,
		Freebucks:     freebucksCardFromInfo(snap.Freebucks),
	}
}

type configData struct {
	EnvContent string     `json:"env_content"`
	HasEnvFile bool       `json:"has_env_file"`
	Effective  []configKV `json:"effective"`
}

type configKV struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Secret bool   `json:"secret"`
}

func (d *Dashboard) configData() configData {
	cfg := d.cfg()
	cd := configData{}
	if _, raw, exists, err := config.EnvFileInfo(); err == nil && exists {
		cd.HasEnvFile = true
		cd.EnvContent = string(raw)
	} else {
		cd.EnvContent = config.DefaultEnvTemplate()
	}
	// Effective values come from the config package's own catalog-driven
	// rendering (issue #288): one key map, no per-key switch here.
	for _, entry := range cfg.Data() {
		cd.Effective = append(cd.Effective, configKV{
			Key:    entry.Key,
			Value:  entry.Value,
			Secret: entry.Secret,
		})
	}
	return cd
}

// baseURLForRequest computes the dynamic API base URL (/v1) for dashboard views.
// It prioritizes the incoming request's Host / X-Forwarded headers so that operators
// accessing the dashboard via VPS IP, domain, VPN, or reverse proxy see the exact
// URL their AI coding clients should dial. Falls back to LISTEN_ADDR when r is nil.
func baseURLForRequest(cfg *config.Config, r *http.Request) string {
	scheme := "http"
	host := ""
	if r != nil {
		if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
			scheme = proto
		} else if r.TLS != nil {
			scheme = "https"
		}
		if fHost := r.Header.Get("X-Forwarded-Host"); fHost != "" {
			host = fHost
		} else if r.Host != "" {
			host = r.Host
		}
	}
	if host == "" {
		host = "127.0.0.1:3457"
		if cfg != nil && cfg.ListenAddr != "" {
			h, p, err := net.SplitHostPort(cfg.ListenAddr)
			if err == nil {
				if h == "" || h == "0.0.0.0" || h == "::" {
					h = "127.0.0.1"
				}
				host = net.JoinHostPort(h, p)
			} else {
				host = cfg.ListenAddr
			}
		}
	}
	return scheme + "://" + host + "/v1"
}

func (d *Dashboard) overviewData(r *http.Request) overviewData {
	cfg := d.cfg()
	ps := d.pool.PoolSnapshot()
	mode := cfg.EffectiveMode()
	od := overviewData{
		BaseURL:              baseURLForRequest(cfg, r),
		Mode:                 mode,
		InBridge:             mode == "bridge",
		ShowBridge:           mode == "bridge" || mode == "hybrid",
		Models:               servedModels(d.reg),
		ModelCount:           len(servedModels(d.reg)),
		Uptime:               humanDuration(time.Since(d.started)),
		SafeMode:             cfg.SafeMode,
		MaxMessagesPerDay:    cfg.MaxMessagesPerDay,
		TransientRetries:     ps.TransientRetries,
		FingerprintRotations: ps.FingerprintRotations,
		BridgeTokens:         d.pool.BridgeCount(),
		IsDefaultAdminToken:  cfg.IsDefaultAdminToken(),
		RequireLogin:         cfg.RequireLogin(),
	}
	for _, t := range ps.Tokens {
		od.Tokens = append(od.Tokens, cardFromSnapshot(t))
	}
	// Regression guard (#200): df7a16a dropped this line, leaving
	// has_tokens permanently false so pooled operators saw "No upstream
	// tokens configured" on Overview while the Tokens tab worked.
	od.HasTokens = len(od.Tokens) > 0
	// Bridge token cards (#187): live snapshots of bridge-mode entries.
	if od.ShowBridge {
		for _, snap := range d.pool.BridgeSnapshot() {
			od.BridgeTokenCards = append(od.BridgeTokenCards, bridgeCardFromSnapshot(snap))
		}
	}
	od.UpstreamSync = parseUpstreamSync(upstreamDriftJSON)
	return od
}

// overviewLiveData is the hot-poll subset of overviewData (issue #322):
// live numbers only. Restart/deploy-only fields (base_url, mode, models,
// safe_mode, max_messages_per_day, transient_retries, upstream_sync) and
// account-stable card fields ride the once-per-mount full fetch; the SPA
// merges them back over this shape.
type overviewLiveData struct {
	Uptime           string            `json:"uptime"`
	Tokens           []tokenLiveCard   `json:"tokens"`
	HasTokens        bool              `json:"has_tokens"`
	BridgeTokens     int               `json:"bridge_tokens"`
	BridgeTokenCards []bridgeTokenCard `json:"bridge_token_cards,omitempty"`
}

// overviewLiveData builds the 15s hot-poll payload: uptime, per-token live
// cards, and bridge relay state. Uptime is string-formatted like the full
// view; bridge cards are live snapshots, identical to the full shape.
func (d *Dashboard) overviewLiveData() overviewLiveData {
	ps := d.pool.PoolSnapshot()
	od := overviewLiveData{
		Uptime:       humanDuration(time.Since(d.started)),
		BridgeTokens: d.pool.BridgeCount(),
	}
	for _, t := range ps.Tokens {
		od.Tokens = append(od.Tokens, liveCardFromSnapshot(t))
	}
	od.HasTokens = len(od.Tokens) > 0
	if mode := d.cfg().EffectiveMode(); mode == "bridge" || mode == "hybrid" {
		for _, snap := range d.pool.BridgeSnapshot() {
			od.BridgeTokenCards = append(od.BridgeTokenCards, bridgeCardFromSnapshot(snap))
		}
	}
	return od
}

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
	BurstEnabled      bool           `json:"burst_balance_enabled"`
	ChatMaxMetered    int            `json:"chat_max_inflight_metered"`
	ChatMaxUnmetered  int            `json:"chat_max_inflight_unmetered"`
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
	QuotaStale   bool   `json:"quota_stale,omitempty"`
	QuotaSavedAt string `json:"quota_saved_at,omitempty"`
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
		BurstEnabled:      cfg.BurstBalanceEnabled,
		ChatMaxMetered:    cfg.ChatMaxInflightMetered,
		ChatMaxUnmetered:  cfg.ChatMaxInflightUnmetered,
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
	BurstEnabled      bool              `json:"burst_balance_enabled"`
	ChatMaxMetered    int               `json:"chat_max_inflight_metered"`
	ChatMaxUnmetered  int               `json:"chat_max_inflight_unmetered"`
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
		BurstEnabled:      cfg.BurstBalanceEnabled,
		ChatMaxMetered:    cfg.ChatMaxInflightMetered,
		ChatMaxUnmetered:  cfg.ChatMaxInflightUnmetered,
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

// quotaFor returns the price label for a model row, Freebucks-based like the
// CLI picker (cli/src/utils/freebucks.ts): the wire prices map is the only
// source of cost. Priced rows render "<n> Freebucks/hr", referral GLM 5.2
// keeps "referral +1/day", all other rows return "" so tables render the
// existing em-dash fallback and pickers render bare ids. Session-count caps
// are retired upstream (see ADR-0027), so no label renders used/limit counts
// or the word session.
func (d *Dashboard) quotaFor(id string) string {
	if id == modelcat.Glm52ModelID {
		return "referral +1/day"
	}
	if d.pool != nil {
		if p, ok := d.firstFreebucksPrices()[id]; ok {
			return freebucksPriceLabel(p)
		}
	}
	return ""
}

// freebucksPriceLabel renders one wire price as "<n> Freebucks/hr" (0 reads
// bare, fractionals to one decimal). Shared by the models table price and
// quota columns so both stay Freebucks-based.
func freebucksPriceLabel(p float64) string {
	if p == 0 {
		return "0 Freebucks/hr"
	}
	return fmt.Sprintf("%s Freebucks/hr", formatSessionUnits(p))
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

// formatSessionUnits mirrors the CLI's unit display
// (format-session-units.ts): integers render bare, fractionals to one
// decimal. Shared name with the CLI file; here it formats Freebucks/hr
// prices, never session counts.
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
		row.DisplayName = modelcat.DisplayName(id)
		row.Tagline = modelcat.Tagline(id)
		row.Badges = modelcat.Badges(id)
		row.Notice = modelcat.Notice(id)
		if n, ok := liveNotices[id]; ok && n != "" {
			row.Notice = n
		}
		if p, ok := livePrices[id]; ok {
			row.Price = p
			effectivePrices[id] = p
			row.PriceLabel = freebucksPriceLabel(p)
		}
		if id == modelcat.Glm52ModelID {
			row.PriceLabel = "Referral grant"
			row.Pool = "referral"
		} else if modelcat.IsPremium(id) {
			row.Pool = "premium"
		} else {
			row.Pool = "unlimited"
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

// --- metrics ---

type metricSample struct {
	Requests int64
	Retries  int64
	Rotation int64
}

const maxMetricSamples = 120

type metricTrend struct {
	Direction  string  `json:"direction"` // "up", "down", "flat"
	Percentage float64 `json:"percentage"`
}

type perTokenMetrics struct {
	Token                int    `json:"token"`
	Requests24h          int    `json:"requests_24h"`
	TransientRetries     int64  `json:"transient_retries"`
	FingerprintRotations int64  `json:"fingerprint_rotations"`
	SpendDay             int64  `json:"spend_day"`
	RiskLevel            string `json:"risk_level"`
}

type metricsData struct {
	TransientRetries     int64             `json:"transient_retries"`
	FingerprintRotations int64             `json:"fingerprint_rotations"`
	RequestsTotal        int64             `json:"requests_total"`
	Models               int               `json:"models"`
	SampleCount          int               `json:"sample_count"`
	RequestsSpark        string            `json:"requests_spark"`
	RetriesSpark         string            `json:"retries_spark"`
	RequestsTrend        metricTrend       `json:"requests_trend"`
	RetriesTrend         metricTrend       `json:"retries_trend"`
	PerTokens            []perTokenMetrics `json:"per_tokens"`
}

func (d *Dashboard) metricsData() metricsData {
	ps := d.pool.PoolSnapshot()
	md := metricsData{
		TransientRetries:     ps.TransientRetries,
		FingerprintRotations: ps.FingerprintRotations,
		RequestsTotal:        int64(ps.RequestsServed),
		// The served gate keeps /admin/metrics consistent with /v1/models,
		// /healthz and /admin/overview (the raw registry carries god-only /
		// eval rows such as luna-es that are not servable).
		Models: len(servedModels(d.reg)),
	}
	d.metricsMu.Lock()
	d.metricHist = append(d.metricHist, metricSample{Requests: md.RequestsTotal, Retries: ps.TransientRetries, Rotation: ps.FingerprintRotations})
	if len(d.metricHist) > maxMetricSamples {
		d.metricHist = d.metricHist[len(d.metricHist)-maxMetricSamples:]
	}
	hist := make([]metricSample, len(d.metricHist))
	copy(hist, d.metricHist)
	d.metricsMu.Unlock()
	md.SampleCount = len(hist)

	requests := make([]float64, len(hist))
	retries := make([]float64, len(hist))
	for i, s := range hist {
		requests[i] = float64(s.Requests)
		retries[i] = float64(s.Retries)
	}
	// NOTE: sparklineSVG inlines color into the SVG stroke *attribute*, where
	// CSS var() cannot resolve (and --fp-amber/--fp-teal are not even defined
	// in app.css) — so concrete theme hexes are required or polylines render
	// invisible. Keep in sync with frontend/src/app.css (--fp-accent/--fp-info).
	md.RequestsSpark = sparklineSVG(requests, "#e3a857", "requests served over time")
	md.RetriesSpark = sparklineSVG(retries, "#7dd3fc", "transient retries over time")

	// Trend: compare last 10 samples vs previous 10.
	md.RequestsTrend = computeTrend(hist, true)
	md.RetriesTrend = computeTrend(hist, false)

	// Per-token breakdown from pool snapshot.
	for _, tok := range ps.Tokens {
		md.PerTokens = append(md.PerTokens, perTokenMetrics{
			Token:                tok.Token,
			Requests24h:          tok.Messages24h,
			TransientRetries:     tok.TransientRetries,
			FingerprintRotations: tok.FingerprintRotations,
			SpendDay:             tok.SpendDay,
			RiskLevel:            tok.RiskLevel,
		})
	}
	return md
}

// computeTrend compares the sum of the last 10 samples to the previous 10.
// useRequests selects the Requests column (true) or Retries (false).
func computeTrend(hist []metricSample, useRequests bool) metricTrend {
	const window = 10
	n := len(hist)
	if n < 2*window {
		return metricTrend{Direction: "flat"}
	}
	recent := hist[n-window:]
	previous := hist[n-2*window : n-window]

	var recentSum, previousSum int64
	for i := range window {
		if useRequests {
			recentSum += recent[i].Requests
			previousSum += previous[i].Requests
		} else {
			recentSum += recent[i].Retries
			previousSum += previous[i].Retries
		}
	}

	if previousSum == 0 {
		if recentSum == 0 {
			return metricTrend{Direction: "flat"}
		}
		return metricTrend{Direction: "up", Percentage: 100}
	}
	pct := float64(recentSum-previousSum) / float64(previousSum) * 100
	if pct > 5 {
		return metricTrend{Direction: "up", Percentage: pct}
	} else if pct < -5 {
		return metricTrend{Direction: "down", Percentage: pct}
	}
	return metricTrend{Direction: "flat", Percentage: pct}
}

func sparklineSVG(values []float64, color, label string) string {
	const w, h = 260, 44
	if len(values) < 2 {
		return `<svg viewBox="0 0 ` + strconv.Itoa(w) + ` ` + strconv.Itoa(h) + `" role="img" aria-label="` + label + `"><polyline points="0,` + strconv.Itoa(h-2) + ` ` + strconv.Itoa(w) + `,` + strconv.Itoa(h-2) + `" fill="none" stroke="` + color + `" stroke-width="1.5"/></svg>`
	}
	min, max := values[0], values[0]
	for _, v := range values[1:] {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	span := max - min
	if span == 0 {
		span = 1
	}
	var sb strings.Builder
	sb.WriteString(`<svg viewBox="0 0 ` + strconv.Itoa(w) + ` ` + strconv.Itoa(h) + `" role="img" aria-label="` + label + `" preserveAspectRatio="none"><polyline points="`)
	for i, v := range values {
		x := float64(i) * float64(w) / float64(len(values)-1)
		y := float64(h-2) - (v-min)/span*float64(h-4)
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(strconv.FormatFloat(x, 'f', 1, 64) + "," + strconv.FormatFloat(y, 'f', 1, 64))
	}
	sb.WriteString(`" fill="none" stroke="` + color + `" stroke-width="1.5"/></svg>`)
	return sb.String()
}

// NoticeItem represents one surfaced upstream announcement, peak window,
// or live broadcast.
type NoticeItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"` // "announcement" | "peak_hours" | "upgrade_hint" | "server_message"
	Title     string `json:"title"`
	Message   string `json:"message"`
	URL       string `json:"url,omitempty"`
	Badge     string `json:"badge,omitempty"`
	Tone      string `json:"tone"` // "info" | "warning" | "accent"
	TokenIdx  *int   `json:"token_index,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
}

// NoticesResponse is the payload returned by GET /admin/api/notices.
type NoticesResponse struct {
	Notices   []NoticeItem                     `json:"notices"`
	PeakHours upstream.DeepSeekPeakHoursWindow `json:"peak_hours"`
	Count     int                              `json:"count"`
}

// noticesData aggregates upstream static announcements, live DeepSeek peak
// hours, and dynamic per-token server messages/hints for the dashboard.
func (d *Dashboard) noticesData() NoticesResponse {
	var list []NoticeItem

	// 1. Official Upstream Tier & Model Announcement
	list = append(list, NoticeItem{
		ID:      "upstream-tier-change",
		Type:    "announcement",
		Title:   "Official Upstream Announcement",
		Message: upstream.TierChangeNotice,
		Badge:   "Freebuff Team",
		Tone:    "accent",
	})

	// 2. Live DeepSeek Peak Hours Evaluation
	peak := upstream.EvaluateDeepSeekPeak(time.Now())
	if peak.IsPeak {
		list = append(list, NoticeItem{
			ID:      "deepseek-peak-active",
			Type:    "peak_hours",
			Title:   "DeepSeek Peak Hours Active",
			Message: fmt.Sprintf("DeepSeek models are currently in peak pricing window (%s - %s). Normal pricing resumes in %s.", peak.WindowStartUTC, peak.WindowEndUTC, peak.NextWindowIn),
			Badge:   "Peak Window",
			Tone:    "warning",
		})
	}

	// 3. Dynamic Session Broadcasts & Upgrade Hints from Pool Tokens
	if d.pool != nil {
		snaps := d.pool.Snapshot()
		for i, tok := range snaps {
			idx := i
			if tok.UpgradeHint != nil && (tok.UpgradeHint.Message != "" || tok.UpgradeHint.URL != "") {
				list = append(list, NoticeItem{
					ID:       fmt.Sprintf("upgrade-hint-tok-%d", tok.Token),
					Type:     "upgrade_hint",
					Title:    fmt.Sprintf("Account #%d Broadcast", tok.Token+1),
					Message:  tok.UpgradeHint.Message,
					URL:      tok.UpgradeHint.URL,
					Badge:    "Upstream Promo",
					Tone:     "info",
					TokenIdx: &idx,
				})
			}
			if tok.ServerMessage != "" {
				list = append(list, NoticeItem{
					ID:       fmt.Sprintf("server-msg-tok-%d", tok.Token),
					Type:     "server_message",
					Title:    fmt.Sprintf("Account #%d System Message", tok.Token+1),
					Message:  tok.ServerMessage,
					Badge:    "Server Notice",
					Tone:     "warning",
					TokenIdx: &idx,
				})
			}
		}
	}

	return NoticesResponse{
		Notices:   list,
		PeakHours: peak,
		Count:     len(list),
	}
}
