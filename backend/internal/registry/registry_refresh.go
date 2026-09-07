package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"
)

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
	start := time.Now()
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
	agents, models := len(agentModels), len(allModels)
	r.mu.Unlock()
	// T18: the success path was silent (the failure path logs in main.go) —
	// surface the refresh outcome with agents/models counts and duration.
	r.logger.Info("registry refreshed", "agents", agents, "models", models, "ms", time.Since(start).Milliseconds())
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
// the previous state — initially the fallback — is retained). The root map is
// applied exactly like a live refresh, so fallback routing matches live
// routing model-for-model.
func (r *Registry) LoadFallback() {
	modelToAgent, allModels := buildModelMapping(pinnedFallbackAgents, pinnedFallbackRootByModel)
	agents := make([]agentModels, len(pinnedFallbackAgents))
	for i, entry := range pinnedFallbackAgents {
		agents[i] = agentModels{agent: entry.agent, models: slices.Clone(entry.models)}
	}
	r.mu.Lock()
	r.agentModels = agents
	r.modelToAgent = modelToAgent
	r.allModels = allModels
	r.mu.Unlock()
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
