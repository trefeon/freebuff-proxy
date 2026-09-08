package registry

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"freebuff-proxy/backend/internal/config"
)

// RawBase is the upstream source of the Codebuff TS constant files.
const RawBase = "https://raw.githubusercontent.com/CodebuffAI/freebuff/main/common/src/constants/"

// JsDelivrBase mirrors RawBase through the jsDelivr CDN. Tried after the raw
// source fails: raw.githubusercontent is throttled or blocked in some CI and
// regions (mirrors freebuff2api-workers' DYNAMIC_MODELS_*_SOURCES pattern,
// where every source carries a raw + jsDelivr pair).
const JsDelivrBase = "https://cdn.jsdelivr.net/gh/CodebuffAI/freebuff@main/common/src/constants/"

// mirrorFor returns the jsDelivr mirror for a raw source URL, or "" when the
// URL is not a raw source (SetSources overrides are used as-is and never
// mirrored).
func mirrorFor(url string) string {
	if strings.HasPrefix(url, RawBase) {
		return JsDelivrBase + strings.TrimPrefix(url, RawBase)
	}
	return ""
}

// sourceFiles are fetched in order; the first file is the one the agent
// blocks are parsed from (free-agents.ts), matching the JS.
var sourceFiles = []string{
	"free-agents.ts",
	"freebuff-model-ids.ts",
	"freebuff-models.ts",
	"gemini.ts",
	"model-config.ts",
	"freebuff-model-entitlements.ts",
}

// fetchTimeout mirrors the JS fetch timeout of 30000ms.
const fetchTimeout = 30 * time.Second

// maxFetchBytes caps a single registry source read (2 MiB). A source larger
// than this fails the fetch, which keeps the previous registry state.
const maxFetchBytes = 2 << 20

// Pinned upstream snapshot (registry/testdata/upstream/): the offline
// fallback tables are DERIVED from this embed at init through the exact
// parsers a live Refresh runs, so an upstream model-list change is one
// snapshot update — the hand-maintained Go tables that used to need a
// parallel edit are gone (issue #273). retiredRootOverrides stays an
// explicit small override map inside parse.go (it remarks a model the
// parser CAN read onto a root the parser cannot evaluate).
//
//go:embed testdata/upstream
var pinnedUpstreamFS embed.FS

// pinnedFallbackAgents / pinnedFallbackRootByModel are built once at init
// from the embedded snapshot through the same parsers a live Refresh runs.
var (
	pinnedFallbackAgents, pinnedFallbackRootByModel = buildPinnedFallback()
)

// buildPinnedFallback reads the six pinned snapshot files and runs the
// live parsers in the same order Refresh does. A corrupted embed yields an
// empty fallback rather than a panic: LoadFallback then simply serves no
// mappings, and the registry parity test fails loudly on any drift.
func buildPinnedFallback() ([]agentModels, map[string]string) {
	names := []string{
		"testdata/upstream/free-agents.ts",
		"testdata/upstream/freebuff-model-ids.ts",
		"testdata/upstream/freebuff-models.ts",
		"testdata/upstream/gemini.ts",
		"testdata/upstream/model-config.ts",
		"testdata/upstream/freebuff-model-entitlements.ts",
	}
	texts := make([]string, 0, len(names))
	for _, name := range names {
		b, err := pinnedUpstreamFS.ReadFile(name)
		if err != nil {
			return nil, nil
		}
		texts = append(texts, string(b))
	}
	resolver := buildConstantResolver(texts)
	return parseAgentModels(strings.Join(texts, "\n"), resolver), parseRootAgentMap(strings.Join(texts, "\n"), resolver)
}

// ErrModelNotFound is returned by AgentForModel for models absent from the
// registry.
var ErrModelNotFound = errors.New("model not found in registry")

// Registry is a concurrency-safe model→agent mapping with periodic refresh.
type Registry struct {
	mu     sync.RWMutex
	cfg    atomic.Pointer[config.Config] // swapped atomically on reload (SetConfig)
	client *http.Client                  // fetch client; redirects followed, fetchTimeout applied
	logger *slog.Logger                  // success-refresh INFO sink (nil = slog.Default())

	sources       []string // override of the default 6 source URLs (tests)
	lastAttempted []string // URLs tried during the most recent Refresh, in order
	modelToAgent  map[string]string
	allModels     []string // sorted
	agentModels   []agentModels
}

// New returns a Registry that fetches from the default Codebuff sources.
// client is used for all fetches; when nil, a client with the 30s fetch
// timeout is used. cfg is stored as the initial config the registry reads
// (currently only MODEL_ALIASES resolution); SetConfig replaces it at
// runtime after a dashboard save or /admin/reload.
func New(cfg *config.Config, client *http.Client) *Registry {
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}
	r := &Registry{client: client, logger: slog.Default()}
	if cfg != nil {
		r.cfg.Store(cfg)
	}
	return r
}

// SetLogger replaces the registry's log sink (nil restores slog.Default).
// Used by tests and by hosts that want the refresh INFO on a custom logger.
func (r *Registry) SetLogger(l *slog.Logger) {
	if l == nil {
		l = slog.Default()
	}
	r.logger = l
}

// SetConfig atomically replaces the config the registry reads, so alias
// resolution (ResolveModel) reflects a dashboard .env save or /admin/reload
// without a restart. A nil cfg clears the stored config.
func (r *Registry) SetConfig(cfg *config.Config) {
	r.cfg.Store(cfg)
}

// Models returns the sorted model list as a copy.
func (r *Registry) Models() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.allModels)
}

// ModelCount returns the number of models currently mapped.
func (r *Registry) ModelCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.allModels)
}

// AgentIDs returns the agent ids currently mapped (needed by the run manager
// for prewarming), in source entry order.
func (r *Registry) AgentIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.agentModels))
	for _, entry := range r.agentModels {
		ids = append(ids, entry.agent)
	}
	return ids
}

func (r *Registry) sourceURLs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.sources) > 0 {
		return slices.Clone(r.sources)
	}
	urls := make([]string, len(sourceFiles))
	for i, f := range sourceFiles {
		urls[i] = RawBase + f
	}
	return urls
}

// fetchText GETs url with the Accept/UA headers of the JS port. Redirects are
// followed by the client (equivalent to the JS manual re-request). file://
// URLs are read from disk — a test hook so the parser can run offline.
func fetchText(ctx context.Context, client *http.Client, src string) (string, error) {
	if u, err := url.Parse(src); err == nil && u.Scheme == "file" {
		path := u.Path
		// file:///C:/x on Windows parses to /C:/x; strip the leading slash.
		if runtime.GOOS == "windows" && len(path) >= 3 && path[0] == '/' && path[2] == ':' {
			path = path[1:]
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/plain")
	req.Header.Set("User-Agent", "freebuff-proxy/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxFetchBytes {
		return "", fmt.Errorf("source exceeds %d bytes", maxFetchBytes)
	}
	return string(b), nil
}
