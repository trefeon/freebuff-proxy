// Package registry maintains the live model→agent map for FreeBuff free
// sessions, parsed from the Codebuff TS constant sources and refreshed on a
// timer. Port of reference/proxy-freebuff/lib/registry.js.
//
// Refresh fetches the 5 TS files in parallel; any failure keeps the previous
// mapping (the hardcoded fallback at boot). FREEBUFF_ROOT_AGENT_ID_BY_MODEL
// wins over first-seen FREE_MODE_AGENT_MODELS assignment, exactly like the
// JS.
package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"freebuff-proxy/internal/config"
)

// RawBase is the upstream source of the Codebuff TS constant files.
const RawBase = "https://raw.githubusercontent.com/CodebuffAI/codebuff/main/common/src/constants/"

// JsDelivrBase mirrors RawBase through the jsDelivr CDN. Tried after the raw
// source fails: raw.githubusercontent is throttled or blocked in some CI and
// regions (mirrors freebuff2api-workers' DYNAMIC_MODELS_*_SOURCES pattern,
// where every source carries a raw + jsDelivr pair).
const JsDelivrBase = "https://cdn.jsdelivr.net/gh/CodebuffAI/codebuff@main/common/src/constants/"

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
}

// fetchTimeout mirrors the JS fetch timeout of 30000ms.
const fetchTimeout = 30 * time.Second

// maxFetchBytes caps a single registry source read (2 MiB). A source larger
// than this fails the fetch, which keeps the previous registry state.
const maxFetchBytes = 2 << 20

// LimitedTierModels is the model set available to 'limited' access-tier
// accounts (egress region demotion, privacy-signal demotion). Mirrors the
// upstream LIMITED_FREEBUFF_MODEL_IDS constant
// (freebuff/common/src/constants/freebuff-models.ts): deepseek-v4-flash +
// mimo-v2.5. Used to annotate /v1/models availability per token tier.
var LimitedTierModels = map[string]bool{
	"deepseek/deepseek-v4-flash": true,
	"mimo/mimo-v2.5":             true,
}

// fallbackAgents is the hardcoded model→agent fallback used when the sources
// are unreachable. Ported from registry.js (lines 14-41), entry order
// preserved: first-seen assignment decides which agent owns models that
// appear in several entries (e.g. the gemini helpers all list the same two
// models; file-picker-max comes first).
//
// Five rows that referenced retired agents (base2-free-laguna-s-2-1,
// base2-free-laguna-s-2-1-openrouter, base2-free-ling-3-flash,
// base2-free-greg-2-ultra, base2-free-greg-2-super) were pruned: they were
// removed from FREE_MODE_AGENT_MODELS on 2026-08-04/08-07
// (reference/freebuff/common/src/constants/free-agents.ts) and 403
// free_mode_invalid_agent_model upstream, so a fallback advertising them
// would list models that cannot run in boot / refresh-failure state
// (issue #121).
var fallbackAgents = []agentModels{
	{agent: "base2-free", models: []string{
		"deepseek/deepseek-v4-pro",
		"deepseek/deepseek-v4-flash",
		"minimax/minimax-m3",
		"openai/gpt-5.6-luna",
		"mimo/mimo-v2.5",
	}},
	{agent: "base2-free-minimax-m3", models: []string{"minimax/minimax-m3"}},
	{agent: "base2-free-luna", models: []string{"openai/gpt-5.6-luna"}},
	{agent: "base2-free-deepseek", models: []string{"deepseek/deepseek-v4-pro"}},
	{agent: "base2-free-deepseek-flash", models: []string{"deepseek/deepseek-v4-flash"}},
	{agent: "base2-free-mimo", models: []string{"mimo/mimo-v2.5"}},
	{agent: "base2-free-glm", models: []string{"z-ai/glm-5.2"}},
	{agent: "base2-free-fable", models: []string{"anthropic/claude-fable-5"}},
	{agent: "file-picker", models: []string{"google/gemini-2.5-flash-lite"}},
	{agent: "file-picker-max", models: []string{"google/gemini-3.1-flash-lite", "google/gemini-3.5-flash-lite"}},
	{agent: "file-lister", models: []string{"google/gemini-3.1-flash-lite", "google/gemini-3.5-flash-lite"}},
	{agent: "researcher-web", models: []string{"google/gemini-3.1-flash-lite", "google/gemini-3.5-flash-lite"}},
	{agent: "researcher-docs", models: []string{"google/gemini-3.1-flash-lite", "google/gemini-3.5-flash-lite"}},
	{agent: "basher", models: []string{"google/gemini-3.1-flash-lite", "google/gemini-3.5-flash-lite"}},
	{agent: "code-reviewer-lite", models: []string{"deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-flash", "mimo/mimo-v2.5"}},
}

// ErrModelNotFound is returned by AgentForModel for models absent from the
// registry.
var ErrModelNotFound = errors.New("model not found in registry")

// Registry is a concurrency-safe model→agent mapping with periodic refresh.
type Registry struct {
	mu     sync.RWMutex
	cfg    atomic.Pointer[config.Config] // swapped atomically on reload (SetConfig)
	client *http.Client                  // fetch client; redirects followed, fetchTimeout applied

	sources       []string // override of the default 5 source URLs (tests)
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
	r := &Registry{client: client}
	if cfg != nil {
		r.cfg.Store(cfg)
	}
	return r
}

// SetConfig atomically replaces the config the registry reads, so alias
// resolution (ResolveModel) reflects a dashboard .env save or /admin/reload
// without a restart. A nil cfg clears the stored config.
func (r *Registry) SetConfig(cfg *config.Config) {
	r.cfg.Store(cfg)
}

// SetSources overrides the source URLs fetched by Refresh (mainly for tests,
// e.g. file:// URLs pointing at fixtures). An empty or nil slice restores the
// default Codebuff sources.
func (r *Registry) SetSources(urls []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(urls) == 0 {
		r.sources = nil
	} else {
		r.sources = slices.Clone(urls)
	}
}

// Refresh fetches the sources in parallel and atomically replaces the
// registry state. Each source file is tried against its raw URL first and its
// jsDelivr mirror second; on any fetch or parse failure the previous state is
// kept and the error returned. Every URL actually attempted is recorded for
// LastAttemptedSources (-doctor output).
func (r *Registry) Refresh(ctx context.Context) error {
	candidates := r.sourceCandidates()

	texts := make([]string, len(candidates))
	errs := make([]error, len(candidates))
	attempted := make([][]string, len(candidates))
	var wg sync.WaitGroup
	for i, urls := range candidates {
		wg.Add(1)
		go func(i int, urls []string) {
			defer wg.Done()
			texts[i], attempted[i], errs[i] = fetchSource(ctx, r.client, urls)
		}(i, urls)
	}
	wg.Wait()

	tried := make([]string, 0, len(candidates)*2)
	for _, a := range attempted {
		tried = append(tried, a...)
	}
	r.mu.Lock()
	r.lastAttempted = tried
	r.mu.Unlock()

	for i, err := range errs {
		if err != nil {
			return fmt.Errorf("registry refresh: fetch %s: %w", attempted[i][len(attempted[i])-1], err)
		}
	}

	resolver := buildConstantResolver(texts)
	agentsText := texts[0]
	rootAgentByModel := parseRootAgentMap(agentsText, resolver)
	agentModels := parseAgentModels(agentsText, resolver)
	if len(agentModels) == 0 && len(rootAgentByModel) == 0 {
		return errors.New("registry refresh: no free agents found in source")
	}
	modelToAgent, allModels := buildModelMapping(agentModels, rootAgentByModel)
	if len(allModels) == 0 {
		return errors.New("registry refresh: no models resolved from source")
	}

	r.mu.Lock()
	r.agentModels = agentModels
	r.modelToAgent = modelToAgent
	r.allModels = allModels
	r.mu.Unlock()
	return nil
}

// fetchSource tries each candidate URL in order until one succeeds, recording
// every attempted URL. The last error is returned when all fail. This is the
// raw-then-mirror fallback: the jsDelivr mirror is only attempted after the
// raw source fails.
func fetchSource(ctx context.Context, client *http.Client, urls []string) (string, []string, error) {
	attempted := make([]string, 0, len(urls))
	var lastErr error
	for _, u := range urls {
		attempted = append(attempted, u)
		text, err := fetchText(ctx, client, u)
		if err == nil {
			return text, attempted, nil
		}
		lastErr = err
	}
	return "", attempted, lastErr
}

// LastAttemptedSources returns the URLs tried during the most recent Refresh
// (primary raw source plus any jsDelivr mirrors attempted after a failure),
// in fetch order. Intended for -doctor output; empty before the first
// refresh and after LoadFallback.
func (r *Registry) LastAttemptedSources() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.lastAttempted)
}

// LoadFallback replaces the registry state with the hardcoded fallback map,
// giving an offline-first model list at boot (and after every failed refresh
// the previous state — initially the fallback — is retained).
func (r *Registry) LoadFallback() {
	modelToAgent, allModels := buildModelMapping(fallbackAgents, map[string]string{})
	agents := make([]agentModels, len(fallbackAgents))
	for i, entry := range fallbackAgents {
		agents[i] = agentModels{agent: entry.agent, models: slices.Clone(entry.models)}
	}
	r.mu.Lock()
	r.agentModels = agents
	r.modelToAgent = modelToAgent
	r.allModels = allModels
	r.mu.Unlock()
}

// ResolveModel resolves an alias (e.g. "gpt-4o") to its real model ID if mapped
// in cfg.ModelAliases, or returns model unchanged.
func (r *Registry) ResolveModel(model string) string {
	if r == nil {
		return model
	}
	cfg := r.cfg.Load()
	if cfg != nil && len(cfg.ModelAliases) > 0 {
		if realModel, ok := cfg.ModelAliases[model]; ok && realModel != "" {
			return realModel
		}
	}
	return model
}

// AgentForModel returns the agent id that serves model (after resolving aliases),
// or an ErrModelNotFound-wrapped error.
func (r *Registry) AgentForModel(model string) (string, error) {
	model = r.ResolveModel(model)
	r.mu.RLock()
	defer r.mu.RUnlock()
	agent, ok := r.modelToAgent[model]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrModelNotFound, model)
	}
	return agent, nil
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

// sourceCandidates returns the per-file URL lists tried by Refresh, in order:
// the primary URL first, then its jsDelivr mirror for the default sources.
// SetSources overrides are used as-is (one URL per entry — mirrors belong to
// the default raw sources).
func (r *Registry) sourceCandidates() [][]string {
	r.mu.RLock()
	custom := len(r.sources) > 0
	r.mu.RUnlock()
	primaries := r.sourceURLs()
	out := make([][]string, len(primaries))
	for i, u := range primaries {
		out[i] = []string{u}
		if !custom {
			if m := mirrorFor(u); m != "" {
				out[i] = append(out[i], m)
			}
		}
	}
	return out
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
