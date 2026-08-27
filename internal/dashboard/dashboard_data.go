package dashboard

import (
	"encoding/json"
	"fmt"
	"freebuff-proxy/internal/phasetiming"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// --- logs ---

type logsData struct {
	Enabled   bool       `json:"enabled"`
	Level     string     `json:"level"`
	Msg       string     `json:"msg"`
	HasFilter bool       `json:"has_filter"`
	Entries   []logEntry `json:"entries"`
}

type logEntry struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
	Fields  string `json:"fields"`
}

func (d *Dashboard) logsData(r *http.Request) logsData {
	ld := logsData{Enabled: d.logs != nil}
	if d.logs == nil {
		return ld
	}
	level := ""
	msg := ""
	if r != nil && r.URL != nil {
		level = strings.TrimSpace(r.URL.Query().Get("level"))
		msg = strings.TrimSpace(r.URL.Query().Get("msg"))
	}
	ld.Level = strings.ToLower(level)
	ld.Msg = msg
	ld.HasFilter = level != "" || msg != ""
	msgLower := strings.ToLower(msg)
	for _, e := range d.logs.Recent(200) {
		if level != "" && !strings.EqualFold(e.Level, level) {
			continue
		}
		if msg != "" && !strings.Contains(strings.ToLower(e.Message), msgLower) {
			continue
		}
		ld.Entries = append(ld.Entries, logEntry{
			Time:    e.Time,
			Level:   e.Level,
			Message: e.Message,
			Fields:  strings.Join(e.Fields, "  "),
		})
	}
	return ld
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
		Models:               d.reg.ModelCount(),
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
	md.RequestsSpark = sparklineSVG(requests, "var(--fp-amber)", "requests served over time")
	md.RetriesSpark = sparklineSVG(retries, "var(--fp-teal)", "transient retries over time")

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

// --- tokens ---

type tokensData struct {
	Mode             string            `json:"mode"`
	InBridge         bool              `json:"in_bridge"`
	BridgeTokens     int               `json:"bridge_tokens"`
	BridgeTokenCards []bridgeTokenCard `json:"bridge_token_cards,omitempty"`
	TokenCount       int               `json:"token_count"`
	Tokens           []tokenDetail     `json:"tokens"`
	HasTokens        bool              `json:"has_tokens"`
}

type tokenDetail struct {
	tokenCard
	SessionInstance         string     `json:"session_instance"`
	SessionModel            string     `json:"session_model"`
	SessionRemainingSeconds int64      `json:"session_remaining_seconds"`
	Quota                   []quotaRow `json:"quota"`
	HasQuota                bool       `json:"has_quota"`
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
	for _, t := range d.pool.Snapshot() {
		detail := tokenDetail{
			tokenCard:               cardFromSnapshot(t),
			SessionInstance:         shortID(t.SessionInstanceID),
			SessionModel:            t.SessionModel,
			SessionRemainingSeconds: t.SessionRemainingSeconds,
		}
		for model, q := range t.QuotaByModel {
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
		// ({dailySessions, endsAt}) grants a referral quota on scarce models
		// like GLM/Luna/Pro. Synthesize a dashboard row for z-ai/glm-5.2 so
		// the promo is visible even though no per-model quota was admitted;
		// a real rateLimitsByModel entry for the model wins over the promo.
		if _, exists := t.QuotaByModel["z-ai/glm-5.2"]; !exists && t.GlmPromo != "" {
			var gp struct {
				DailySessions float64 `json:"dailySessions"`
				EndsAt        string  `json:"endsAt"`
			}
			if err := json.Unmarshal([]byte(t.GlmPromo), &gp); err == nil && gp.DailySessions > 0 {
				var resetAt time.Time
				if ts, err := time.Parse(time.RFC3339, gp.EndsAt); err == nil {
					resetAt = ts
				}
				glmRow := quotaRow{
					Model:          "z-ai/glm-5.2",
					Limit:          formatQuota(gp.DailySessions),
					Recent:         "0",
					Remaining:      gp.DailySessions,
					Period:         "promo",
					ResetAt:        shortTime(resetAt),
					ResetAtUTC:     utcAttr(resetAt),
					ResetsIn:       humanDuration(time.Until(resetAt)),
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
	// Bridge token cards (#187): live snapshots of bridge-mode entries.
	if td.InBridge {
		for _, snap := range d.pool.BridgeSnapshot() {
			td.BridgeTokenCards = append(td.BridgeTokenCards, bridgeCardFromSnapshot(snap))
		}
	}
	return td
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
	ID    string `json:"id"`
	Agent string `json:"agent"`
}

// servedModels returns the registry ids that pass the strict ServedModels
// gate (issue #189 set): the vendor catalog also carries god-only/eval rows
// (luna-es) that must never appear as servable in dashboard/setup views.
func servedModels(reg *registry.Registry) []string {
	out := make([]string, 0, 8)
	for _, id := range reg.Models() {
		if registry.IsServedModel(id) {
			out = append(out, id)
		}
	}
	return out
}

type aliasRow struct {
	Alias string `json:"alias"`
	Real  string `json:"real"`
}

func (d *Dashboard) modelsData() modelsData {
	md := modelsData{Count: d.reg.ModelCount(), Agents: len(d.reg.AgentIDs())}
	// Served gate: the dashboard shows the models this proxy actually
	// serves (issue #189 strict set), not the raw upstream registry — the
	// vendor catalog now carries god-only/eval rows (e.g. luna-es) that
	// must never be presented as servable.
	for _, id := range d.reg.Models() {
		if !registry.IsServedModel(id) {
			continue
		}
		row := modelRow{ID: id}
		if agent, err := d.reg.AgentForModel(id); err == nil {
			row.Agent = agent
		}
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

// --- traces ---

type tracesData struct {
	Enabled bool         `json:"enabled"`
	Traces  []traceEntry `json:"traces"`
}

type traceEntry struct {
	Time   string    `json:"time"`
	Token  string    `json:"token"`
	Model  string    `json:"model"`
	Status string    `json:"status"`
	Ms     string    `json:"ms"`
	Error  string    `json:"error"`
	Phases []PhaseKV `json:"phases,omitempty"`
}

func (d *Dashboard) tracesData() tracesData {
	td := tracesData{Enabled: d.logs != nil}
	if d.logs == nil {
		return td
	}
	phaseNames := map[string]bool{
		phasetiming.AcquireMS:        true,
		phasetiming.SessionRefreshMS: true,
		phasetiming.RunAcquireMS:     true,
		phasetiming.UpstreamTTFBMS:   true,
		phasetiming.TotalMS:          true,
	}
	for _, e := range d.logs.Recent(200) {
		if e.Message != "chat trace" {
			continue
		}
		entry := traceEntry{Time: e.Time, Status: "ok"}
		var phaseMap map[string]int64
		for _, f := range e.Fields {
			key, value, ok := strings.Cut(f, "=")
			if !ok {
				continue
			}
			switch key {
			case "token":
				entry.Token = value
			case "model":
				entry.Model = value
			case "status":
				entry.Status = value
			case "ms":
				entry.Ms = value + "ms"
			case "error":
				entry.Error = value
			default:
				if phaseNames[key] {
					if phaseMap == nil {
						phaseMap = make(map[string]int64, 5)
					}
					if v, err := strconv.ParseInt(value, 10, 64); err == nil {
						phaseMap[key] = v
					}
				}
			}
		}
		if phaseMap != nil {
			entry.Phases = PhaseList(phaseMap)
		}
		if entry.Token == "" {
			entry.Token = "—"
		}
		td.Traces = append(td.Traces, entry)
	}
	return td
}

// --- client setup ---

type setupData struct {
	BaseURL      string   `json:"base_url"`
	KeyHint      string   `json:"key_hint"`
	Model        string   `json:"model"`
	Models       []string `json:"models"`
	Mode         string   `json:"mode"`
	Bridge       bool     `json:"bridge"`
	BridgeTokens int      `json:"bridge_tokens"`
	TokenCount   int      `json:"token_count"`
	HasTokens    bool     `json:"has_tokens"`
}

func (d *Dashboard) setupData() setupData {
	cfg := d.cfg()
	host := "localhost"
	if h, _, err := net.SplitHostPort(cfg.ListenAddr); err == nil && h != "" && h != "0.0.0.0" && h != "::" {
		host = h
	}
	mode := cfg.EffectiveMode()
	sd := setupData{
		BaseURL:      "http://" + host + "/v1",
		Mode:         mode,
		Bridge:       mode == "bridge",
		BridgeTokens: d.pool.BridgeCount(),
		TokenCount:   d.pool.TokenCount(),
		Models:       servedModels(d.reg),
	}
	sd.HasTokens = sd.TokenCount > 0
	if len(sd.Models) > 0 {
		sd.Model = pickDefaultModel(sd.Models)
	}
	switch mode {
	case "bridge":
		sd.KeyHint = "your FreeBuff token (bridge mode: the client's Authorization header IS the upstream token)"
	default:
		sd.KeyHint = "sk-any (pooled mode; the proxy picks from AUTH_TOKENS)"
	}
	return sd
}

func cardFromSnapshot(t pool.TokenSnapshot) tokenCard {
	card := tokenCard{
		Index:            t.Token,
		SessionStatus:    t.SessionStatus,
		QueuePosition:    t.SessionQueuePosition,
		QueueDepth:       t.SessionQueueDepth,
		ActiveRuns:       t.ActiveRuns,
		Requests:         t.Requests,
		Messages24h:      t.Messages24h,
		DailyLimit:       t.DailyLimit,
		UsagePct:         t.UsagePct,
		RiskLevel:        t.RiskLevel,
		TransientRetries: t.TransientRetries,
		Locked:           t.Locked,
	}
	if !t.CooldownUntil.IsZero() && time.Now().Before(t.CooldownUntil) {
		card.CooldownActive = true
		card.CooldownUntil = t.CooldownUntil.Format(time.RFC3339)
	}
	if t.BanType != "" {
		card.BanType = t.BanType
		if !t.BannedUntil.IsZero() {
			card.BannedUntil = t.BannedUntil.Format(time.RFC3339)
		}
	}
	if t.Standing != nil {
		card.HasStanding = true
		card.StandingLevel = t.Standing.Level
		card.StandingLabel = t.Standing.Label
		card.StandingScore = t.Standing.Score
		card.StandingNextLevel = t.Standing.NextLevel
		if !t.Standing.NextLevelAt.IsZero() {
			card.StandingNextLevelAt = t.Standing.NextLevelAt.Format(time.RFC3339)
		}
		card.StandingCappedBy = t.Standing.CappedBy
		card.StandingCappedReason = t.Standing.CappedReason
		card.StandingBlurb = t.Standing.Blurb
		for _, s := range t.Standing.NextSteps {
			card.StandingNextSteps = append(card.StandingNextSteps, standingStepCard{
				ID: s.ID, Label: s.Label, Detail: s.Detail, Points: s.Points, Href: s.Href,
			})
		}
	}
	if t.Referral != nil {
		card.HasReferral = true
		card.ReferralCode = t.Referral.Code
		card.ReferralQualifiedCount = t.Referral.QualifiedCount
		card.ReferralSessionsLeft = t.Referral.WeeklySessionsRemaining
		card.ReferralGithubLinked = t.Referral.GithubLinked
		if !t.Referral.ResetAt.IsZero() {
			card.ReferralResetAt = t.Referral.ResetAt.Format(time.RFC3339)
		}
	}
	return card
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

// shortKey returns the first 8 chars of a bridge key hash for display (#187).
func shortKey(key string) string {
	if len(key) > 8 {
		return key[:8] + "…"
	}
	return key
}

func formatQuota(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func formatEntitlement(e map[string]float64) string {
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+formatQuota(e[k]))
	}
	return strings.Join(parts, ", ")
}

func shortTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("15:04 Jan 2")
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	if d < time.Minute {
		d = time.Minute
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

// --- overview ---

type overviewData struct {
	BaseURL              string            `json:"base_url"`
	Mode                 string            `json:"mode"`
	InBridge             bool              `json:"in_bridge"`
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
	// UpstreamSync summarises the latest .github/workflows/upstream-drift
	// run (compiled into the binary). Users on an out-of-date build see
	// HasDrift=true + DriftedFiles and know to update.
	UpstreamSync *upstreamSync `json:"upstream_sync,omitempty"`
}

// upstreamSync is the dashboard-friendly view of the embedded
// internal/dashboard/data/upstream_drift.json. Computed once at request
// time; cheap.
type upstreamSync struct {
	UpstreamSHA  string         `json:"upstream_sha"`            // short SHA, "(not yet reported)" before first CI run
	CheckedAt    string         `json:"checked_at"`              // RFC3339
	HasDrift     bool           `json:"has_drift"`               // any non-SAME file
	HasRegistry  bool           `json:"has_registry_drift"`      // 5 pinned files
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
	Index               int     `json:"index"`
	SessionStatus       string  `json:"session_status"`
	QueuePosition       int     `json:"queue_position"`
	QueueDepth          int     `json:"queue_depth"`
	ActiveRuns          int     `json:"active_runs"`
	Requests            int     `json:"requests"`
	Messages24h         int     `json:"messages_24h"`
	DailyLimit          int     `json:"daily_limit"`
	UsagePct            int     `json:"usage_pct"`
	RiskLevel           string  `json:"risk_level"`
	CooldownActive      bool    `json:"cooldown_active"`
	CooldownUntil       string  `json:"cooldown_until"`
	Locked              bool    `json:"locked"`
	BanType             string  `json:"ban_type,omitempty"`
	BannedUntil         string  `json:"banned_until,omitempty"`
	TransientRetries    int64   `json:"transient_retries"`
	HasStanding         bool    `json:"has_standing"`
	StandingLevel       string  `json:"standing_level"`
	StandingLabel       string  `json:"standing_label"`
	StandingScore       float64 `json:"standing_score"`
	StandingNextLevel   string  `json:"standing_next_level"`
	StandingNextLevelAt string  `json:"standing_next_level_at"`
	// Standing cap + earn-back hints (issue #140 P3d, FreebuffStandingInfo):
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
	Key           string  `json:"key"`    // masked hash prefix
	Status        string  `json:"status"` // active|cooldown|locked
	Model         string  `json:"model"`
	ActiveRuns    int     `json:"active_runs"`
	Requests      int     `json:"requests"`
	Locked        bool    `json:"locked"`
	CooldownUntil string  `json:"cooldown_until"`
	SessionActive bool    `json:"session_active"`
	SpendDay      float64 `json:"spend_day"`
	SpendPct      int     `json:"spend_pct"`
	BanType       string  `json:"ban_type,omitempty"`
	BannedUntil   string  `json:"banned_until,omitempty"`
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
	if raw, err := os.ReadFile(".env"); err == nil {
		cd.HasEnvFile = true
		cd.EnvContent = string(raw)
	} else {
		cd.EnvContent = defaultEnvTemplate
	}
	cd.Effective = []configKV{
		{Key: "LISTEN_ADDR", Value: cfg.ListenAddr},
		{Key: "UPSTREAM_BASE_URL", Value: cfg.UpstreamBaseURL},
		{Key: "AUTH_TOKENS", Value: fmt.Sprintf("%d token(s)", len(cfg.AuthTokens)), Secret: true},
		{Key: "API_KEYS", Value: fmt.Sprintf("%d key(s)", len(cfg.APIKeys)), Secret: true},
		{Key: "ADMIN_TOKEN", Value: boolWord(cfg.AdminToken != ""), Secret: true},
		{Key: "ROTATION_INTERVAL", Value: cfg.RotationInterval.String()},
		{Key: "REQUEST_TIMEOUT", Value: cfg.RequestTimeout.String()},
		{Key: "SESSION_CALL_TIMEOUT", Value: cfg.SessionCallTimeout.String()},
		{Key: "COST_MODE", Value: cfg.CostMode},
		{Key: "TLS_FINGERPRINT", Value: cfg.TLSFingerprint},
		{Key: "REGISTRY_REFRESH", Value: cfg.RegistryRefresh.String()},
		{Key: "DEBUG_DUMP", Value: strconv.FormatBool(cfg.DebugDump)},
		{Key: "LOG_FILE", Value: cfg.LogFile},
		{Key: "LOG_LEVEL", Value: cfg.LogLevel},
		{Key: "MAX_MESSAGES_PER_DAY", Value: strconv.Itoa(cfg.MaxMessagesPerDay)},
		{Key: "IDLE_ROTATION_TIMEOUT", Value: cfg.IdleRotationTimeout.String()},
		{Key: "SAFE_MODE", Value: strconv.FormatBool(cfg.SafeMode)},
		{Key: "REQUEST_JITTER", Value: cfg.RequestJitter.String()},
		{Key: "CLI_VERSION", Value: cfg.CLIVersion},
		{Key: "MODEL_ALIASES", Value: fmt.Sprintf("%d alias(es)", len(cfg.ModelAliases)), Secret: true},
		{Key: "MODELS_ALLOW", Value: strings.Join(cfg.ModelsAllow, ",")},
		{Key: "TRANSIENT_RETRIES", Value: strconv.Itoa(cfg.TransientRetries)},
	}
	return cd
}

func boolWord(v bool) string {
	if v {
		return "set"
	}
	return "unset"
}

const defaultEnvTemplate = `# freebuff-proxy configuration (.env)
# Keys mirror the environment variables; leave commented to keep the default.
# See the README and docs/guides for the full reference.

#LISTEN_ADDR=127.0.0.1:3457
#UPSTREAM_BASE_URL=https://www.codebuff.com
#AUTH_TOKENS=token1,token2
#API_KEYS=sk-local-...
#ADMIN_TOKEN=change-me
#ROTATION_INTERVAL=6h
#REQUEST_TIMEOUT=15m
#SESSION_CALL_TIMEOUT=30s
#COST_MODE=free
#TLS_FINGERPRINT=chrome120
#REGISTRY_REFRESH=6h
#DEBUG_DUMP=false
#LOG_FILE=
#LOG_LEVEL=info
#MAX_MESSAGES_PER_DAY=0
#IDLE_ROTATION_TIMEOUT=0
#SAFE_MODE=true
#REQUEST_JITTER=0s
#CLI_VERSION=0.10.7
#MODEL_ALIASES=
#MODELS_ALLOW=
#TRANSIENT_RETRIES=1
`

func (d *Dashboard) overviewData() overviewData {
	cfg := d.cfg()
	ps := d.pool.PoolSnapshot()
	mode := cfg.EffectiveMode()
	host := "localhost"
	if h, _, err := net.SplitHostPort(cfg.ListenAddr); err == nil && h != "" && h != "0.0.0.0" && h != "::" {
		host = h
	}
	od := overviewData{
		BaseURL:              "http://" + host + "/v1",
		Mode:                 mode,
		InBridge:             mode == "bridge",
		Models:               servedModels(d.reg),
		ModelCount:           len(servedModels(d.reg)),
		SafeMode:             cfg.SafeMode,
		MaxMessagesPerDay:    cfg.MaxMessagesPerDay,
		TransientRetries:     ps.TransientRetries,
		FingerprintRotations: ps.FingerprintRotations,
		BridgeTokens:         d.pool.BridgeCount(),
		IsDefaultAdminToken:  cfg.IsDefaultAdminToken(),
	}
	for _, t := range ps.Tokens {
		od.Tokens = append(od.Tokens, cardFromSnapshot(t))
	}
	// Regression guard (#200): df7a16a dropped this line, leaving
	// has_tokens permanently false so pooled operators saw "No upstream
	// tokens configured" on Overview while the Tokens tab worked.
	od.HasTokens = len(od.Tokens) > 0
	// Bridge token cards (#187): live snapshots of bridge-mode entries.
	if od.InBridge {
		for _, snap := range d.pool.BridgeSnapshot() {
			od.BridgeTokenCards = append(od.BridgeTokenCards, bridgeCardFromSnapshot(snap))
		}
	}
	od.UpstreamSync = parseUpstreamSync(upstreamDriftJSON)
	return od
}
