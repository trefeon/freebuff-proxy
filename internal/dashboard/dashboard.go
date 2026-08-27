// Package dashboard serves the embedded admin web UI: a single-binary,
// modern Svelte 5 + Tailwind control panel for the proxy (health, config, tokens, logs,
// metrics). Static assets and templates are vendored and embedded via go:embed —
// no runtime CDN, no runtime Node.js dependency, and zero external network calls.
package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/logring"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/updatecheck"
)

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

// pickDefaultModel selects mimo/mimo-v2.5 when present, or the first available model.
func pickDefaultModel(models []string) string {
	if len(models) == 0 {
		return ""
	}
	const preferred = "mimo/mimo-v2.5"
	for _, m := range models {
		if m == preferred {
			return preferred
		}
	}
	for _, m := range models {
		if strings.Contains(m, "mimo-v2.5") {
			return m
		}
	}
	return models[0]
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
	case "metrics":
		return d.metricsData()
	case "upstream":
		return d.upstreamData()
	default:
		return nil
	}
}

// upstreamData surfaces the embedded upstream-drift JSON the
// .github/workflows/upstream-drift.yml job refreshes after every check
// run. The user sees whether the running build is current with
// CodebuffAI/freebuff; the data is shipped at compile time, so a stale
// runtime that cannot reach GitHub still gets a real answer.
func (d *Dashboard) upstreamData() map[string]any {
	return map[string]any{
		"drift": json.RawMessage(upstreamDriftJSON),
	}
}

// upstreamReport is the on-disk JSON shape written by
// scripts/check-upstream.sh. The fields match the script's printf order so
// the two stay lock-step (a parse failure here is a deploy-time regression
// caught by `go build`).
type upstreamReport struct {
	Upstream    string            `json:"upstream"`
	UpstreamSHA string            `json:"upstream_sha"`
	CheckedAt   string            `json:"checked_at"`
	Files       []upstreamFileRaw `json:"files"`
}

type upstreamFileRaw struct {
	Group     string `json:"group"`
	File      string `json:"file"`
	PinnedSHA string `json:"pinned_sha"`
	VendorSHA string `json:"vendor_sha"`
	Status    string `json:"status"`
}

// isHexSHA reports whether s looks like a 40-char git SHA. Used to
// distinguish real upstream SHAs from placeholder strings
// ("(not yet reported)", "(parse error)") in the dashboard banner.
func isHexSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// parseUpstreamSync turns the embedded drift JSON into the dashboard view.
// Always returns a non-nil struct; never errors — the embed is a known
// shape, and a parse failure is a deploy-time regression caught by `go
// build`. The ReleasesURL is hard-coded here (not from the JSON) because
// the dashboard banner must always point at the current repo even if the
// drift JSON is stale.
func parseUpstreamSync(raw []byte) *upstreamSync {
	const releasesURL = "https://github.com/trefeon/freebuff-proxy/releases"
	sync := &upstreamSync{ReleasesURL: releasesURL}
	if len(raw) == 0 {
		return sync
	}
	// The embedded JSON has the file-level shape; the dashboard view is a
	// rolled-up summary. Re-decode into upstreamReport so the summary can
	// name the affected files.
	var rep upstreamReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		// Parse failure on a shipped artifact: return the empty summary
		// with the timestamp placeholder so the banner still renders.
		sync.UpstreamSHA = "(parse error)"
		return sync
	}
	sync.UpstreamSHA = rep.UpstreamSHA
	// A real upstream SHA is hex; the placeholder "(not yet reported)"
	// (or a "(parse error)" literal) is not. Only truncate hex SHAs so the
	// placeholder stays readable in the dashboard banner.
	if isHexSHA(sync.UpstreamSHA) && len(sync.UpstreamSHA) > 12 {
		sync.UpstreamSHA = sync.UpstreamSHA[:12]
	}
	sync.CheckedAt = rep.CheckedAt
	for _, f := range rep.Files {
		if f.Status == "SAME" {
			continue
		}
		sync.DriftedFiles = append(sync.DriftedFiles, upstreamFile(f))
		switch f.Group {
		case "registry":
			sync.HasRegistry = true
		case "wire":
			sync.HasWire = true
		}
	}
	sync.HasDrift = sync.HasRegistry || sync.HasWire
	return sync
}
