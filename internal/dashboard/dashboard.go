// Package dashboard serves the embedded admin web UI: a single-binary,
// modern Svelte 5 + Tailwind control panel for the proxy (health, config, tokens, logs,
// metrics). Static assets and templates are vendored and embedded via go:embed —
// no runtime CDN, no runtime Node.js dependency, and zero external network calls.
package dashboard

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/logring"
	"freebuff-proxy/internal/phasetiming"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/updatecheck"
)

//go:embed all:dist
var files embed.FS

// AssetsFS exposes the vendored static assets for the server to mount under /admin/assets/.
func AssetsFS() embed.FS {
	return files
}

// DistFS returns the embedded dist filesystem for SPA serving.
func DistFS() fs.FS {
	if sub, err := fs.Sub(files, "dist"); err == nil {
		return sub
	}
	return files
}

// Dashboard renders the admin UI over the live pool, registry, and config.
type Dashboard struct {
	cfg     func() *config.Config // returns the current (hot-reloadable) config
	pool    *pool.Pool
	reg     *registry.Registry
	logger  *slog.Logger
	logs    *logring.Handler // dashboard log viewer source (nil = disabled)
	started time.Time

	// version is the running release tag ("" / "dev" for dev builds) and
	// updates is the release-update indicator (issue #50b); the layout
	// shows a badge when a newer GitHub release exists. Both may be left
	// unset (no badge).
	version string
	updates *updatecheck.Checker

	// metricHist is the rolling counter history sampled by the metrics page
	// (UI-poll-driven, not a background goroutine). Per-instance so multiple
	// dashboards never share one window.
	metricsMu  sync.Mutex
	metricHist []metricSample
}

// Option configures optional Dashboard features (version + update checker).
type Option func(*Dashboard)

// WithVersion wires the running release tag and the update checker for the
// header badge (issue #50b). Nil checker disables the badge.
func WithVersion(version string, updates *updatecheck.Checker) Option {
	return func(d *Dashboard) {
		d.version = version
		d.updates = updates
	}
}

// New builds the dashboard. cfg must return the current configuration — the
// server passes its atomic pointer loader so /admin/reload is reflected
// immediately. A nil logger falls back to slog.Default(). Template parse
// failures panic: the templates are embedded, so a parse error is a build
// invariant violation, not a runtime condition. logs is the optional log
// viewer ring (nil hides the /admin/logs page data).
func New(cfg func() *config.Config, p *pool.Pool, reg *registry.Registry, logger *slog.Logger, logs *logring.Handler, opts ...Option) *Dashboard {
	if logger == nil {
		logger = slog.Default()
	}
	d := &Dashboard{cfg: cfg, pool: p, reg: reg, logger: logger, started: time.Now(), logs: logs}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// releaseURL is where the update badge points (the releases page).
const releaseURL = "https://github.com/trefeon/freebuff-proxy/releases"

// pickDefaultModel selects deepseek/deepseek-v4-flash when present, or the first available model.
func pickDefaultModel(models []string) string {
	if len(models) == 0 {
		return ""
	}
	const preferred = "deepseek/deepseek-v4-flash"
	for _, m := range models {
		if m == preferred {
			return preferred
		}
	}
	for _, m := range models {
		if strings.Contains(m, "deepseek-v4-flash") {
			return m
		}
	}
	return models[0]
}

// ServeSPA serves the embedded single-page application and static assets from dist/.
func (d *Dashboard) ServeSPA(w http.ResponseWriter, r *http.Request) {
	dist, err := fs.Sub(files, "dist")
	if err != nil {
		http.Error(w, "SPA not available", http.StatusInternalServerError)
		return
	}

	reqPath := strings.TrimPrefix(r.URL.Path, "/admin")
	reqPath = strings.TrimPrefix(reqPath, "/")

	if reqPath != "" && !strings.Contains(reqPath, "..") {
		if f, err := dist.Open(reqPath); err == nil {
			_ = f.Close()
			http.FileServerFS(dist).ServeHTTP(w, r)
			return
		}
	}

	index, err := dist.Open("index.html")
	if err != nil {
		http.Error(w, "SPA index not available", http.StatusInternalServerError)
		return
	}
	defer func() { _ = index.Close() }()

	stat, err := index.Stat()
	if err != nil {
		http.Error(w, "SPA index not available", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", stat.ModTime(), index.(io.ReadSeeker))
}

// APIHandler returns a handler that writes the named view model as JSON.
func (d *Dashboard) APIHandler(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		data := d.dataFor(name, r)
		_ = json.NewEncoder(w).Encode(data)
	}
}

// APIVersion returns the running version and update check result as JSON.
func (d *Dashboard) APIVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"current_version": d.version,
		"has_update":      false,
		"latest_version":  "",
		"update_url":      releaseURL,
	}
	if d.version != "" && d.updates != nil && r.Context() != nil {
		if latest, err := d.updates.Latest(r.Context()); err == nil && latest != "" && updatecheck.UpdateAvailable(d.version, latest) {
			resp["has_update"] = true
			resp["latest_version"] = latest
		}
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// RenderLogin renders the login page with an optional error message.
func (d *Dashboard) RenderLogin(w http.ResponseWriter, r *http.Request, errMsg string) {
	w.Header().Set("Content-Type", "application/json")
	if errMsg != "" {
		w.WriteHeader(http.StatusUnauthorized)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": errMsg})
}

// RenderRestricted renders the access-denied page as JSON.
func (d *Dashboard) RenderRestricted(w http.ResponseWriter, r *http.Request, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": msg})
}

// dataFor resolves the page data for a named content template.
func (d *Dashboard) dataFor(name string, r *http.Request) any {
	switch name {
	case "overview":
		return d.overviewData()
	case "config":
		return d.configData()
	case "tokens":
		return d.tokensData()
	case "models":
		return d.modelsData()
	case "logs":
		return d.logsData(r)
	case "traces":
		return d.tracesData()
	case "setup":
		return d.setupData()
	case "playground":
		return d.playgroundData()
	case "metrics":
		return d.metricsData()
	default:
		return nil
	}
}

// --- playground ---

type playgroundData struct {
	Models    []string `json:"models"`
	Model     string   `json:"model"`
	HasModels bool     `json:"has_models"`
	Mode      string   `json:"mode"`
}

func (d *Dashboard) playgroundData() playgroundData {
	models := d.reg.Models()
	pd := playgroundData{Models: models, Mode: d.cfg().EffectiveMode()}
	pd.HasModels = len(models) > 0
	if pd.HasModels {
		pd.Model = pickDefaultModel(models)
	}
	return pd
}

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

type metricsData struct {
	TransientRetries     int64  `json:"transient_retries"`
	FingerprintRotations int64  `json:"fingerprint_rotations"`
	RequestsTotal        int64  `json:"requests_total"`
	Models               int    `json:"models"`
	SampleCount          int    `json:"sample_count"`
	RequestsSpark        string `json:"requests_spark"`
	RetriesSpark         string `json:"retries_spark"`
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
	return md
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
	Mode         string        `json:"mode"`
	InBridge     bool          `json:"in_bridge"`
	BridgeTokens int           `json:"bridge_tokens"`
	TokenCount   int           `json:"token_count"`
	Tokens       []tokenDetail `json:"tokens"`
	HasTokens    bool          `json:"has_tokens"`
}

type tokenDetail struct {
	tokenCard
	SessionInstance string     `json:"session_instance"`
	Quota           []quotaRow `json:"quota"`
	HasQuota        bool       `json:"has_quota"`
}

type quotaRow struct {
	Model          string `json:"model"`
	Limit          string `json:"limit"`
	Recent         string `json:"recent"`
	Period         string `json:"period"`
	ResetAt        string `json:"reset_at"`
	ResetAtUTC     string `json:"reset_at_utc"`
	ResetsIn       string `json:"resets_in"`
	Entitled       string `json:"entitled"`
	HasEntitlement bool   `json:"has_entitlement"`
	UsagePct       int    `json:"usage_pct"`
	NearLimit      bool   `json:"near_limit"`
	HasBar         bool   `json:"has_bar"`
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
			tokenCard:       cardFromSnapshot(t),
			SessionInstance: shortID(t.SessionInstanceID),
		}
		for model, q := range t.QuotaByModel {
			row := quotaRow{
				Model:      model,
				Limit:      formatQuota(q.Limit),
				Recent:     formatQuota(q.RecentCount),
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
		sort.Slice(detail.Quota, func(i, j int) bool { return detail.Quota[i].Model < detail.Quota[j].Model })
		detail.HasQuota = len(detail.Quota) > 0
		td.Tokens = append(td.Tokens, detail)
	}
	td.HasTokens = len(td.Tokens) > 0
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

type aliasRow struct {
	Alias string `json:"alias"`
	Real  string `json:"real"`
}

func (d *Dashboard) modelsData() modelsData {
	md := modelsData{Count: d.reg.ModelCount(), Agents: len(d.reg.AgentIDs())}
	for _, id := range d.reg.Models() {
		row := modelRow{ID: id}
		if agent, err := d.reg.AgentForModel(id); err == nil {
			row.Agent = agent
		}
		md.Models = append(md.Models, row)
	}
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
	Time   string `json:"time"`
	Token  string `json:"token"`
	Model  string `json:"model"`
	Status string `json:"status"`
	Ms     string `json:"ms"`
	Error  string `json:"error"`
}

func (d *Dashboard) tracesData() tracesData {
	td := tracesData{Enabled: d.logs != nil}
	if d.logs == nil {
		return td
	}
	for _, e := range d.logs.Recent(200) {
		if e.Message != "chat trace" {
			continue
		}
		entry := traceEntry{Time: e.Time, Status: "ok"}
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
			}
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
		Models:       d.reg.Models(),
	}
	sd.HasTokens = sd.TokenCount > 0
	if len(sd.Models) > 0 {
		sd.Model = pickDefaultModel(sd.Models)
	}
	switch mode {
	case "bridge":
		sd.KeyHint = "your FreeBuff token (bridge mode: the client's Authorization header IS the upstream token)"
	case "hybrid":
		sd.KeyHint = "your FreeBuff token — sent when present; otherwise the proxy picks from AUTH_TOKENS (hybrid mode)"
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
	}
	if !t.CooldownUntil.IsZero() && time.Now().Before(t.CooldownUntil) {
		card.CooldownActive = true
		card.CooldownUntil = t.CooldownUntil.Format(time.RFC3339)
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
	}
	return card
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
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

// RenderConfigResult renders the response after a config save or token action.
func (d *Dashboard) RenderConfigResult(w http.ResponseWriter, r *http.Request, ok bool, message string) {
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": ok, "message": message})
}

// RenderTestResult appends one per-token outcome.
func (d *Dashboard) RenderTestResult(w http.ResponseWriter, r *http.Request, token int, ok bool, message, instanceID string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token":       token,
		"ok":          ok,
		"message":     message,
		"instance_id": shortID(instanceID),
	})
}

// PhaseKV is one rendered latency phase.
type PhaseKV struct {
	Name string `json:"name"`
	Ms   int64  `json:"ms"`
}

// PhaseList orders a phase map for rendering.
func PhaseList(phases map[string]int64) []PhaseKV {
	order := []string{
		phasetiming.AcquireMS,
		phasetiming.SessionRefreshMS,
		phasetiming.RunAcquireMS,
		phasetiming.UpstreamTTFBMS,
		phasetiming.TotalMS,
	}
	out := make([]PhaseKV, 0, len(order))
	for _, name := range order {
		if v, ok := phases[name]; ok {
			out = append(out, PhaseKV{Name: name, Ms: v})
		}
	}
	return out
}

// RenderSmokeResult renders the smoke-test outcome.
func (d *Dashboard) RenderSmokeResult(w http.ResponseWriter, r *http.Request, model, token string, ms int64, preview []byte, phases []PhaseKV) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"model":   model,
		"token":   token,
		"ms":      ms,
		"preview": string(preview),
		"phases":  phases,
	})
}

func (d *Dashboard) RenderDiag(w http.ResponseWriter, r *http.Request, checks []DiagCheck) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"checks": checks})
}

// --- overview ---

type overviewData struct {
	Mode                 string      `json:"mode"`
	InBridge             bool        `json:"in_bridge"`
	BridgeTokens         int         `json:"bridge_tokens"`
	Models               []string    `json:"models"`
	ModelCount           int         `json:"model_count"`
	Uptime               string      `json:"uptime"`
	SafeMode             bool        `json:"safe_mode"`
	MaxMessagesPerDay    int         `json:"max_messages_per_day"`
	TransientRetries     int64       `json:"transient_retries"`
	FingerprintRotations int64       `json:"fingerprint_rotations"`
	Tokens               []tokenCard `json:"tokens"`
	HasTokens            bool        `json:"has_tokens"`
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
	TransientRetries    int64   `json:"transient_retries"`
	HasStanding         bool    `json:"has_standing"`
	StandingLevel       string  `json:"standing_level"`
	StandingLabel       string  `json:"standing_label"`
	StandingScore       float64 `json:"standing_score"`
	StandingNextLevel   string  `json:"standing_next_level"`
	StandingNextLevelAt string  `json:"standing_next_level_at"`
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

type DiagCheck struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Warn    bool   `json:"warn"`
	Message string `json:"message"`
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
		{Key: "API_KEYS", Value: strings.Join(cfg.APIKeys, ","), Secret: true},
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
		{Key: "HYBRID_MODE", Value: strconv.FormatBool(cfg.HybridMode)},
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
#HYBRID_MODE=false
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
	od := overviewData{
		Mode:                 mode,
		InBridge:             mode == "bridge",
		Models:               d.reg.Models(),
		ModelCount:           d.reg.ModelCount(),
		Uptime:               time.Since(d.started).Round(time.Second).String(),
		SafeMode:             cfg.SafeMode,
		MaxMessagesPerDay:    cfg.MaxMessagesPerDay,
		TransientRetries:     ps.TransientRetries,
		FingerprintRotations: ps.FingerprintRotations,
		BridgeTokens:         d.pool.BridgeCount(),
	}
	for _, t := range ps.Tokens {
		od.Tokens = append(od.Tokens, cardFromSnapshot(t))
	}
	od.HasTokens = len(od.Tokens) > 0
	return od
}
