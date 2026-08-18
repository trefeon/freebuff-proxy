package registry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
)

// fileSource builds a file:// URL for a local fixture path (no network).
func fileSource(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return "file:///" + filepath.ToSlash(abs)
}

// expectedFallback is the model→agent map the JS fallback table (lines
// 14-41) must produce: first-seen assignment in entry order, so the five
// base2-free models belong to base2-free (not their dedicated one-model
// agents), and the gemini helper models belong to file-picker / file-picker-max
// (which precede file-lister / researcher-* / basher).
var expectedFallback = map[string]string{
	"deepseek/deepseek-v4-pro":     "base2-free",
	"deepseek/deepseek-v4-flash":   "base2-free",
	"minimax/minimax-m3":           "base2-free",
	"openai/gpt-5.6-luna":          "base2-free",
	"mimo/mimo-v2.5":               "base2-free",
	"z-ai/glm-5.2":                 "base2-free-glm",
	"anthropic/claude-fable-5":     "base2-free-fable",
	"google/gemini-2.5-flash-lite": "file-picker",
	"google/gemini-3.1-flash-lite": "file-picker-max",
	"google/gemini-3.5-flash-lite": "file-picker-max",
}

func TestFallbackMap(t *testing.T) {
	r := New(nil, nil)
	if r.ModelCount() != 0 {
		t.Fatalf("fresh registry ModelCount = %d, want 0", r.ModelCount())
	}
	if _, err := r.AgentForModel("deepseek/deepseek-v4-flash"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("fresh registry AgentForModel err = %v, want ErrModelNotFound", err)
	}

	r.LoadFallback()

	if got := r.ModelCount(); got != len(expectedFallback) {
		t.Errorf("ModelCount = %d, want %d", got, len(expectedFallback))
	}
	for model, wantAgent := range expectedFallback {
		agent, err := r.AgentForModel(model)
		if err != nil {
			t.Errorf("AgentForModel(%q): %v", model, err)
			continue
		}
		if agent != wantAgent {
			t.Errorf("AgentForModel(%q) = %q, want %q", model, agent, wantAgent)
		}
	}

	models := r.Models()
	if !sort.StringsAreSorted(models) {
		t.Error("Models() not sorted")
	}
	if len(models) != len(expectedFallback) {
		t.Errorf("Models() length = %d, want %d", len(models), len(expectedFallback))
	}
	for model := range expectedFallback {
		if !contains(models, model) {
			t.Errorf("Models() missing %q", model)
		}
	}
	// Models must be a copy: mutating it must not corrupt the registry.
	models[0] = "mutated/model"
	if got := r.Models()[0]; strings.HasPrefix(got, "mutated") {
		t.Error("Models() returned a shared slice")
	}

	if _, err := r.AgentForModel("does/not-exist"); !errors.Is(err, ErrModelNotFound) {
		t.Errorf("unknown model err = %v, want ErrModelNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Issue #121 — fallback must not advertise retired agents.
// ---------------------------------------------------------------------------

// liveFreeModeAgents is a snapshot of the FREE_MODE_AGENT_MODELS allowlist
// (reference/freebuff/common/src/constants/free-agents.ts) for every agent
// the fallback table lists. The vendored reference clone is gitignored and
// only present in the main checkout, so the snapshot lives in the test
// instead of being parsed at run time; keep it in sync with the reference
// when free-agents.ts changes.
var liveFreeModeAgents = map[string][]string{
	"base2-free": {
		"deepseek/deepseek-v4-pro",
		"deepseek/deepseek-v4-flash",
		"minimax/minimax-m3",
		"openai/gpt-5.6-luna",
		"mimo/mimo-v2.5",
	},
	"base2-free-deepseek":       {"deepseek/deepseek-v4-pro"},
	"base2-free-deepseek-flash": {"deepseek/deepseek-v4-flash"},
	"base2-free-mimo":           {"mimo/mimo-v2.5"},
	"base2-free-minimax-m3":     {"minimax/minimax-m3"},
	"base2-free-luna":           {"openai/gpt-5.6-luna"},
	"base2-free-glm":            {"z-ai/glm-5.2"},
	"base2-free-fable":          {"anthropic/claude-fable-5"},
	"file-picker":               {"google/gemini-2.5-flash-lite"},
	"file-picker-max":           {"google/gemini-3.1-flash-lite", "google/gemini-3.5-flash-lite"},
	"file-lister":               {"google/gemini-3.1-flash-lite", "google/gemini-3.5-flash-lite"},
	"researcher-web":            {"google/gemini-3.1-flash-lite", "google/gemini-3.5-flash-lite"},
	"researcher-docs":           {"google/gemini-3.1-flash-lite", "google/gemini-3.5-flash-lite"},
	"basher":                    {"google/gemini-3.1-flash-lite", "google/gemini-3.5-flash-lite"},
	"code-reviewer-lite":        {"deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-flash", "mimo/mimo-v2.5"},
}

// retiredFallbackAgents are the five fallback rows pruned for issue #121:
// all were removed from FREE_MODE_AGENT_MODELS on 2026-08-04/08-07
// (reference/freebuff/common/src/constants/free-agents.ts) and now 403
// free_mode_invalid_agent_model upstream.
var retiredFallbackAgents = []string{
	"base2-free-laguna-s-2-1",
	"base2-free-laguna-s-2-1-openrouter",
	"base2-free-ling-3-flash",
	"base2-free-greg-2-ultra",
	"base2-free-greg-2-super",
}

// TestFallbackAgentsLiveOnly pins issue #121: every fallback agent and model
// must exist in the live FREE_MODE_AGENT_MODELS allowlist, the five retired
// rows must stay pruned, and the alive rows the JS fallback table lists must
// still be present. In fallback state (boot before first refresh, or refresh
// failures) /v1/models is built from this table, so a dead row would advertise
// a model that cannot run.
func TestFallbackAgentsLiveOnly(t *testing.T) {
	fallback := make(map[string][]string, len(fallbackAgents))
	for _, entry := range fallbackAgents {
		fallback[entry.agent] = entry.models
	}

	// Every fallback entry must exist in the live allowlist with its models
	// allowed for that agent.
	for agent, models := range fallback {
		allowed, ok := liveFreeModeAgents[agent]
		if !ok {
			t.Errorf("fallback agent %q is absent from FREE_MODE_AGENT_MODELS (reference/freebuff/common/src/constants/free-agents.ts)", agent)
			continue
		}
		for _, model := range models {
			if !contains(allowed, model) {
				t.Errorf("fallback agent %q model %q is not in its FREE_MODE_AGENT_MODELS allowlist", agent, model)
			}
		}
	}

	// The five retired rows must stay pruned.
	for _, agent := range retiredFallbackAgents {
		if _, ok := fallback[agent]; ok {
			t.Errorf("retired fallback agent %q is still advertised (issue #121)", agent)
		}
	}

	// The alive rows the JS fallback table lists must still be present.
	for agent := range liveFreeModeAgents {
		if _, ok := fallback[agent]; !ok {
			t.Errorf("live fallback agent %q missing from fallbackAgents", agent)
		}
	}

	// User-visible acceptance: in fallback state the dead model ids must not
	// resolve to an agent (a request would otherwise send x-freebuff-model
	// upstream and 403 free_mode_invalid_agent_model).
	r := New(nil, nil)
	r.LoadFallback()
	for _, model := range []string{
		"poolside/laguna-s-2.1",
		"openrouter/poolside/laguna-s-2.1",
		"inclusionai/ling-3.0-flash:free",
		"crof/greg-2-ultra",
		"crof/greg-2-super",
	} {
		if _, err := r.AgentForModel(model); !errors.Is(err, ErrModelNotFound) {
			t.Errorf("AgentForModel(%q) after LoadFallback = %v, want ErrModelNotFound (issue #121)", model, err)
		}
	}
}

func TestResolver(t *testing.T) {
	text := `
export const A = 'a/x';
export const B = A
export const C = B as const
export const AGENTS = {
  one: 'o/1',
  two: 'o/2'
}
export const D = AGENTS.one
export const E = true;
export const F = false;
export const N = null;
export const Z = 42;
export const G = 'escape\'d/value';
export const S = new Set<string>(['s/1', A, 's/2'])
const LOCAL_SET = new Set(['l/1']);
export const LITERAL_LEAF = 'chain/leaf';
export const H1 = LITERAL_LEAF
export const H2 = H1
export const H3 = H2
export const H4 = H3
export const H5 = H4
export const H6 = H5
export const H7 = H6
export const H8 = H7
export const H9 = H8
export const H10 = H9
`
	res := buildConstantResolver([]string{text})

	// Plain table run: resolve each name. A "" want means unresolvable.
	names := map[string]string{
		"A": "a/x", "B": "a/x", "C": "a/x", "D": "o/1", "G": "escape\\'d/value",
		"E": "", "F": "", "N": "", "Z": "",
		// Alias chains: ≤8 hops resolve, 9+ hit the cap.
		"H1": "chain/leaf", "H8": "chain/leaf", "H9": "", "H10": "",
	}
	for name, want := range names {
		if got := res.resolve(name, 0); got != want {
			t.Errorf("resolve(%q) = %q, want %q", name, got, want)
		}
	}

	// Set constants keep raw members; the caller resolves identifiers.
	if got, want := res.sets["S"], []string{"s/1", "A", "s/2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("sets[S] = %v, want %v", got, want)
	}
	// Non-exported Set constants match too (JS `(?:export\s+)?const`).
	if got := res.sets["LOCAL_SET"]; !reflect.DeepEqual(got, []string{"l/1"}) {
		t.Errorf("sets[LOCAL_SET] = %v, want [l/1]", got)
	}
}

// expectFixture is the full model→agent map the synthetic fixture must
// produce: root entries win, unresolvable entries are dropped.
var expectFixture = map[string]string{
	"alpha/model-one":    "root-alpha",
	"alpha/model-extra":  "free-agent-alpha",
	"set/literal-one":    "free-agent-beta",
	"gamma/unused-model": "free-agent-beta",
	"beta/model-two":     "free-agent-gamma",
	"object/model-one":   "free-agent-delta",
}

func TestRefreshFromFixture(t *testing.T) {
	r := New(nil, nil)
	r.LoadFallback()
	r.SetSources([]string{fileSource(t, filepath.Join("testdata", "registry-fixture.ts"))})

	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if got := r.ModelCount(); got != len(expectFixture) {
		t.Errorf("ModelCount = %d, want %d", got, len(expectFixture))
	}
	for model, want := range expectFixture {
		agent, err := r.AgentForModel(model)
		if err != nil {
			t.Errorf("AgentForModel(%q): %v", model, err)
			continue
		}
		if agent != want {
			t.Errorf("AgentForModel(%q) = %q, want %q", model, agent, want)
		}
	}

	// Root-wins: alpha/model-one is listed for free-agent-alpha too.
	// Unresolvable root keys must leave no trace.
	for _, model := range []string{"chain/leaf", "unknown/model"} {
		if _, err := r.AgentForModel(model); !errors.Is(err, ErrModelNotFound) {
			t.Errorf("AgentForModel(%q) err = %v, want ErrModelNotFound", model, err)
		}
	}

	wantModels := []string{
		"alpha/model-extra", "alpha/model-one", "beta/model-two",
		"gamma/unused-model", "object/model-one", "set/literal-one",
	}
	models := r.Models()
	if !reflect.DeepEqual(models, wantModels) {
		t.Errorf("Models() = %v, want %v", models, wantModels)
	}
	wantAgents := []string{"free-agent-alpha", "free-agent-beta", "free-agent-gamma", "free-agent-delta"}
	if got := r.AgentIDs(); !reflect.DeepEqual(got, wantAgents) {
		t.Errorf("AgentIDs() = %v, want %v", got, wantAgents)
	}
}

func TestRefreshFailureKeepsState(t *testing.T) {
	r := New(nil, nil)
	r.SetSources([]string{fileSource(t, filepath.Join("testdata", "registry-fixture.ts"))})
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("initial Refresh: %v", err)
	}
	before := r.Models()
	want := expectFixture["beta/model-two"]

	// Point at a missing file: every fetch failing must fail the refresh.
	r.SetSources([]string{fileSource(t, filepath.Join("testdata", "does-not-exist.ts"))})
	if err := r.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh against missing source succeeded, want error")
	}

	if got := r.Models(); !reflect.DeepEqual(got, before) {
		t.Errorf("Models after failed refresh = %v, want unchanged %v", got, before)
	}
	if agent, err := r.AgentForModel("beta/model-two"); err != nil || agent != want {
		t.Errorf("AgentForModel after failed refresh = (%q, %v), want (%q, nil)", agent, err, want)
	}
}

func TestRefreshEmptySource(t *testing.T) {
	r := New(nil, nil)
	empty := filepath.Join(t.TempDir(), "empty.ts")
	if err := os.WriteFile(empty, []byte("// nothing here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.SetSources([]string{fileSource(t, empty)})
	err := r.Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no free agents") {
		t.Fatalf("Refresh on empty source err = %v, want 'no free agents' error", err)
	}
}

// TestRefreshOverLimitSourceKeepsState verifies fetchText caps source reads
// at maxFetchBytes: an over-limit source fails the refresh and the previous
// registry state is kept.
func TestRefreshOverLimitSourceKeepsState(t *testing.T) {
	r := New(nil, nil)
	r.SetSources([]string{fileSource(t, filepath.Join("testdata", "registry-fixture.ts"))})
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("initial Refresh: %v", err)
	}
	before := r.Models()
	want := expectFixture["beta/model-two"]

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", maxFetchBytes+1))
	}))
	defer srv.Close()

	r.SetSources([]string{srv.URL})
	err := r.Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Refresh against over-limit source err = %v, want size-exceeded error", err)
	}

	if got := r.Models(); !reflect.DeepEqual(got, before) {
		t.Errorf("Models after failed refresh = %v, want unchanged %v", got, before)
	}
	if agent, err := r.AgentForModel("beta/model-two"); err != nil || agent != want {
		t.Errorf("AgentForModel after failed refresh = (%q, %v), want (%q, nil)", agent, err, want)
	}
}

// TestResolveObjectPropertyRegexMetachar verifies an object alias target
// whose property name contains regex metacharacters is skipped instead of
// panicking inside regexp.MustCompile (P3).
func TestResolveObjectPropertyRegexMetachar(t *testing.T) {
	res := &constantResolver{
		literals: map[string]string{},
		aliases:  map[string]string{"WEIRD": "AGENTS.foo[bar"},
		objects:  map[string]string{"AGENTS": "export const AGENTS = { good: 'ok/1' }"},
	}
	if got := res.resolve("WEIRD", 0); got != "" {
		t.Errorf("resolve(object property with metachar) = %q, want \"\" (skipped)", got)
	}
}

// TestConcurrentAccess hammers readers while refresh/fallback swap state;
// run with -race.
func TestConcurrentAccess(t *testing.T) {
	r := New(nil, nil)
	r.LoadFallback()
	src := []string{fileSource(t, filepath.Join("testdata", "registry-fixture.ts"))}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				r.Models()
				r.ModelCount()
				r.AgentIDs()
				_, _ = r.AgentForModel("deepseek/deepseek-v4-flash")
				_, _ = r.AgentForModel("beta/model-two")
				_, _ = r.AgentForModel("nope/nope")
			}
		}()
	}
	for i := 0; i < 25; i++ {
		r.SetSources(src)
		if err := r.Refresh(ctx); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		r.LoadFallback()
	}
	wg.Wait()

	// Final state must be one of the two valid states, fully consistent.
	models := r.Models()
	switch len(models) {
	case len(expectFixture), len(expectedFallback):
		if !sort.StringsAreSorted(models) {
			t.Error("final Models() not sorted")
		}
	default:
		t.Errorf("unexpected final model count %d", len(models))
	}
}
func TestModelAliases(t *testing.T) {
	cfg := &config.Config{
		ModelAliases: map[string]string{
			"gpt-4o": "deepseek/deepseek-v4-flash",
			"glm":    "z-ai/glm-5.2",
		},
	}
	r := New(cfg, nil)
	r.LoadFallback()

	if got := r.ResolveModel("gpt-4o"); got != "deepseek/deepseek-v4-flash" {
		t.Errorf("ResolveModel(gpt-4o) = %q, want deepseek/deepseek-v4-flash", got)
	}
	if got := r.ResolveModel("unknown"); got != "unknown" {
		t.Errorf("ResolveModel(unknown) = %q, want unknown", got)
	}

	agent, err := r.AgentForModel("gpt-4o")
	if err != nil {
		t.Fatalf("AgentForModel(gpt-4o) error: %v", err)
	}
	if agent != "base2-free" {
		t.Errorf("AgentForModel(gpt-4o) = %q, want base2-free", agent)
	}
}

// TestSetConfigUpdatesAliases verifies a runtime config swap (dashboard
// save / /admin/reload) is reflected by alias resolution without a restart:
// the registry must read MODEL_ALIASES through the atomic pointer, not the
// startup pointer.
func TestSetConfigUpdatesAliases(t *testing.T) {
	cfg := &config.Config{
		ModelAliases: map[string]string{"gpt-4o": "deepseek/deepseek-v4-flash"},
	}
	r := New(cfg, nil)
	r.LoadFallback()

	if got := r.ResolveModel("gpt-4o"); got != "deepseek/deepseek-v4-flash" {
		t.Fatalf("ResolveModel(gpt-4o) before SetConfig = %q, want deepseek/deepseek-v4-flash", got)
	}

	r.SetConfig(&config.Config{
		ModelAliases: map[string]string{"gpt-4o": "z-ai/glm-5.2", "glm": "z-ai/glm-5.2"},
	})
	if got := r.ResolveModel("gpt-4o"); got != "z-ai/glm-5.2" {
		t.Errorf("ResolveModel(gpt-4o) after SetConfig = %q, want z-ai/glm-5.2 (reload must apply)", got)
	}
	if got := r.ResolveModel("glm"); got != "z-ai/glm-5.2" {
		t.Errorf("ResolveModel(glm) after SetConfig = %q, want z-ai/glm-5.2 (new alias must apply)", got)
	}

	// Clearing the config must fall back to identity resolution.
	r.SetConfig(nil)
	if got := r.ResolveModel("gpt-4o"); got != "gpt-4o" {
		t.Errorf("ResolveModel(gpt-4o) after SetConfig(nil) = %q, want unchanged", got)
	}
}

// TestRefreshNon200KeepsState verifies fetchText surfaces a non-200 HTTP
// status as an error and Refresh keeps the previous registry state (R1: only
// file://-missing and over-limit paths were previously covered).
func TestRefreshNon200KeepsState(t *testing.T) {
	r := New(nil, nil)
	r.SetSources([]string{fileSource(t, filepath.Join("testdata", "registry-fixture.ts"))})
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("initial Refresh: %v", err)
	}
	before := r.Models()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	r.SetSources([]string{srv.URL})
	err := r.Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("Refresh against 500 source err = %v, want 'status 500' error", err)
	}
	if got := r.Models(); !reflect.DeepEqual(got, before) {
		t.Errorf("Models after failed refresh = %v, want unchanged %v", got, before)
	}
}

// TestRefreshCanceledCtxKeepsState verifies a canceled context fails the
// fetch and Refresh keeps the previous registry state (R2). The HTTP fetch
// path is used (the file:// test hook does not consult ctx).
func TestRefreshCanceledCtxKeepsState(t *testing.T) {
	r := New(nil, nil)
	r.SetSources([]string{fileSource(t, filepath.Join("testdata", "registry-fixture.ts"))})
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("initial Refresh: %v", err)
	}
	before := r.Models()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.SetSources([]string{srv.URL})
	if err := r.Refresh(ctx); err == nil {
		t.Fatal("Refresh with canceled ctx succeeded, want error")
	}
	if got := r.Models(); !reflect.DeepEqual(got, before) {
		t.Errorf("Models after canceled Refresh = %v, want unchanged %v", got, before)
	}
}

// TestRefreshPartialMultiSourceFailureKeepsState verifies a multi-source
// refresh with ONE failing source fails the whole refresh and keeps the
// previous state (R3: only single-source failures were previously covered).
func TestRefreshPartialMultiSourceFailureKeepsState(t *testing.T) {
	r := New(nil, nil)
	r.SetSources([]string{fileSource(t, filepath.Join("testdata", "registry-fixture.ts"))})
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("initial Refresh: %v", err)
	}
	before := r.Models()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	r.SetSources([]string{
		fileSource(t, filepath.Join("testdata", "registry-fixture.ts")),
		srv.URL,
	})
	if err := r.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh with one 404 source succeeded, want error")
	}
	if got := r.Models(); !reflect.DeepEqual(got, before) {
		t.Errorf("Models after partially-failed Refresh = %v, want unchanged %v", got, before)
	}
}

// TestSetSourcesNilRestoresDefaults verifies SetSources(nil) (or an empty
// slice) restores the default 5 Codebuff source URLs (R4).
func TestSetSourcesNilRestoresDefaults(t *testing.T) {
	r := New(nil, nil)
	r.SetSources([]string{"file:///custom"})
	if got := r.sourceURLs(); !reflect.DeepEqual(got, []string{"file:///custom"}) {
		t.Fatalf("sourceURLs after SetSources = %v, want [file:///custom]", got)
	}

	r.SetSources(nil)
	want := make([]string, len(sourceFiles))
	for i, f := range sourceFiles {
		want[i] = RawBase + f
	}
	if got := r.sourceURLs(); !reflect.DeepEqual(got, want) {
		t.Errorf("sourceURLs after SetSources(nil) = %v, want defaults %v", got, want)
	}
}

// TestModelAliasesOneHop verifies alias resolution is one-hop only: an alias
// whose value is itself an alias resolves to that alias, never recursed
// (R5, documented in ResolveModel).
func TestModelAliasesOneHop(t *testing.T) {
	cfg := &config.Config{
		ModelAliases: map[string]string{
			"alias-a": "alias-b",
			"alias-b": "deepseek/deepseek-v4-flash",
		},
	}
	r := New(cfg, nil)
	if got := r.ResolveModel("alias-a"); got != "alias-b" {
		t.Errorf("ResolveModel(alias-a) = %q, want alias-b (one hop only, no recursion)", got)
	}
	if got := r.ResolveModel("alias-b"); got != "deepseek/deepseek-v4-flash" {
		t.Errorf("ResolveModel(alias-b) = %q, want deepseek/deepseek-v4-flash", got)
	}
}

// TestAgentForModelAliasToUnmappedModel verifies an alias resolving to a
// model that is absent from the registry surfaces ErrModelNotFound (the
// alias is resolved first, then the lookup misses).
func TestAgentForModelAliasToUnmappedModel(t *testing.T) {
	cfg := &config.Config{
		ModelAliases: map[string]string{"gpt-4o": "not/in-the-registry"},
	}
	r := New(cfg, nil)
	r.LoadFallback()

	if _, err := r.AgentForModel("gpt-4o"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("AgentForModel(alias→unmapped) err = %v, want ErrModelNotFound", err)
	}
}

// TestRefreshNoModelsResolvedGuard pins the reachable behavior around the
// "no models resolved" guard in Refresh. A source whose agent entries only
// reference unresolvable models collapses to the "no free agents" error: the
// parser drops every zero-model entry, so the len(allModels)==0 guard at
// registry.go is defensive and currently unreachable through the parser
// (verified against parse.go: entries with no resolvable models are dropped,
// and every kept entry contributes at least one model).
func TestRefreshNoModelsResolvedGuard(t *testing.T) {
	r := New(nil, nil)
	src := filepath.Join(t.TempDir(), "no-models.ts")
	content := `
export const FREE_MODE_AGENT_MODELS = {
  'agent-one': new Set([MISSING_ONE, MISSING_TWO]),
  'agent-two': UNRESOLVABLE_SET
}
`
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	r.SetSources([]string{fileSource(t, src)})
	err := r.Refresh(context.Background())
	if err == nil {
		t.Fatal("Refresh with zero resolvable models succeeded, want error")
	}
	if !strings.Contains(err.Error(), "no free agents") {
		t.Errorf("err = %v, want 'no free agents' (the 'no models resolved' branch is unreachable via the parser)", err)
	}
}

func contains(values []string, needle string) bool {
	for _, v := range values {
		if v == needle {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Issue #93 — jsDelivr mirrors + fallback ordering.
// ---------------------------------------------------------------------------

// TestMirrorForURLConstruction pins the jsDelivr mirror URL construction:
// every default raw source has a matching mirror, and non-raw URLs (SetSources
// overrides) are never mirrored.
func TestMirrorForURLConstruction(t *testing.T) {
	for _, f := range sourceFiles {
		raw := RawBase + f
		want := JsDelivrBase + f
		if got := mirrorFor(raw); got != want {
			t.Errorf("mirrorFor(%q) = %q, want %q", raw, got, want)
		}
	}
	if got := mirrorFor(JsDelivrBase + "free-agents.ts"); got != "" {
		t.Errorf("mirrorFor(jsDelivr URL) = %q, want \"\" (no double mirror)", got)
	}
	if got := mirrorFor("file:///x.ts"); got != "" {
		t.Errorf("mirrorFor(override) = %q, want \"\"", got)
	}
}

// TestSourceCandidatesMirrorOrdering pins the per-file URL lists Refresh
// tries: default sources are [raw, jsDelivr] pairs (mirror after primary),
// while SetSources overrides are single-URL entries used as-is.
func TestSourceCandidatesMirrorOrdering(t *testing.T) {
	r := New(nil, nil)
	cands := r.sourceCandidates()
	if len(cands) != len(sourceFiles) {
		t.Fatalf("candidates = %d, want %d source files", len(cands), len(sourceFiles))
	}
	for i, f := range sourceFiles {
		want := []string{RawBase + f, JsDelivrBase + f}
		if !reflect.DeepEqual(cands[i], want) {
			t.Errorf("candidates[%d] = %v, want %v", i, cands[i], want)
		}
	}

	r.SetSources([]string{"file:///custom", "http://example/x.ts"})
	cands = r.sourceCandidates()
	want := [][]string{{"file:///custom"}, {"http://example/x.ts"}}
	if !reflect.DeepEqual(cands, want) {
		t.Errorf("override candidates = %v, want %v", cands, want)
	}
}

// TestFetchSourceFallbackOrdering verifies fetchSource tries candidate URLs
// in order and stops at the first success: a failing primary falls through to
// the mirror (the attempted list records both), while a successful primary
// never triggers the fallback.
func TestFetchSourceFallbackOrdering(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/raw":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "/mirror":
			_, _ = io.WriteString(w, "export const MIRROR_CONTENT = 'm/mirror'")
		case "/ok":
			_, _ = io.WriteString(w, "export const OK_CONTENT = 'm/ok'")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}

	// Primary fails, mirror succeeds: mirror content wins, both URLs recorded.
	text, attempted, err := fetchSource(ctx, client, []string{srv.URL + "/raw", srv.URL + "/mirror"})
	if err != nil {
		t.Fatalf("fetchSource(raw→mirror): %v", err)
	}
	if !strings.Contains(text, "MIRROR_CONTENT") {
		t.Errorf("text = %q, want mirror content", text)
	}
	wantAttempted := []string{srv.URL + "/raw", srv.URL + "/mirror"}
	if !reflect.DeepEqual(attempted, wantAttempted) {
		t.Errorf("attempted = %v, want %v", attempted, wantAttempted)
	}

	// All candidates fail: error returned, every URL recorded.
	_, attempted, err = fetchSource(ctx, client, []string{srv.URL + "/raw", srv.URL + "/missing"})
	if err == nil {
		t.Fatal("fetchSource with all-failing candidates succeeded, want error")
	}
	if len(attempted) != 2 {
		t.Errorf("attempted on total failure = %v, want both URLs", attempted)
	}

	// Primary succeeds: the mirror is never attempted.
	text, attempted, err = fetchSource(ctx, client, []string{srv.URL + "/ok", srv.URL + "/mirror"})
	if err != nil {
		t.Fatalf("fetchSource(ok→mirror): %v", err)
	}
	if !strings.Contains(text, "OK_CONTENT") {
		t.Errorf("text = %q, want primary content", text)
	}
	if !reflect.DeepEqual(attempted, []string{srv.URL + "/ok"}) {
		t.Errorf("attempted = %v, want only the primary URL (no fallback)", attempted)
	}
}

// TestLastAttemptedSources verifies the -doctor helper: every URL tried in
// the most recent Refresh is recorded, including after failures.
func TestLastAttemptedSources(t *testing.T) {
	r := New(nil, nil)
	if got := r.LastAttemptedSources(); got != nil {
		t.Fatalf("fresh registry LastAttemptedSources = %v, want nil", got)
	}

	missing := fileSource(t, filepath.Join("testdata", "does-not-exist.ts"))
	r.SetSources([]string{missing})
	if err := r.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh against missing source succeeded, want error")
	}
	if got := r.LastAttemptedSources(); !reflect.DeepEqual(got, []string{missing}) {
		t.Errorf("LastAttemptedSources after failed refresh = %v, want [%s]", got, missing)
	}

	// A successful refresh records the sources it actually tried.
	fixture := fileSource(t, filepath.Join("testdata", "registry-fixture.ts"))
	r.SetSources([]string{fixture})
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := r.LastAttemptedSources(); !reflect.DeepEqual(got, []string{fixture}) {
		t.Errorf("LastAttemptedSources after successful refresh = %v, want [%s]", got, fixture)
	}
}
