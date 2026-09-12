package registry

// Port of reference/gateways/proxy-freebuff/lib/registry.js parsing (the authoritative
// spec): a text-based extractor for the Codebuff TS constants files. It is
// deliberately regex-shaped like the JS original rather than a real TS parser,
// so the two stay behavior-identical — including quirks (computed keys without
// quotes, unquoted object keys, and `false`/`null`/number constants are all
// skipped exactly as in the JS).

import (
	"regexp"
	"sort"
	"strings"
)

// Regexes ported 1:1 from registry.js. (?s) = JS `s` flag, (?m) = JS `m`
// flag; RE2 lacks lookahead, so the alias regex's negative lookahead
// (?!['{0-9nOf]) is reproduced as a post-filter in matchAlias.
var (
	reLiteral   = regexp.MustCompile(`(?s)export const\s+([A-Za-z_][A-Za-z0-9_]*)[^=]*?=\s*'((?:[^'\\]|\\.)*)'`)
	reAlias     = regexp.MustCompile(`(?m)export const\s+([A-Za-z_][A-Za-z0-9_]*)[^=]*?=\s*([A-Za-z_][A-Za-z0-9_.]*)\s*(?:as\s+const)?\s*$`)
	reObject    = regexp.MustCompile(`(?s)export const\s+([A-Za-z_][A-Za-z0-9_]*)[^=]*?=\s*\{([\s\S]*?)\n\}`)
	reSetConst  = regexp.MustCompile(`(?s)(?:export\s+)?const\s+([A-Za-z_][A-Za-z0-9_]*)[^=]*?=\s*new\s+Set(?:<[^>]*>)?\(\[([\s\S]*?)\]\)`)
	reSetMember = regexp.MustCompile(`'([^']+)'|([A-Za-z_][A-Za-z0-9_.]*)`)

	reRootBlock = regexp.MustCompile(`(?s)export const FREEBUFF_ROOT_AGENT_ID_BY_MODEL[^=]*?=\s*\{([\s\S]*?)\n\}`)
	reRootEntry = regexp.MustCompile(`\[([A-Za-z_][A-Za-z0-9_]*)\]\s*:\s*'([^']+)'`)

	reAgentBlock = regexp.MustCompile(`(?s)export const FREE_MODE_AGENT_MODELS[^=]*?=\s*\{([\s\S]*?)\n\}`)
	reAgentEntry = regexp.MustCompile(`'([^']+)'\s*:\s*(?:new\s+Set(?:<[^>]*>)?\(\[([\s\S]*?)\]\)|([A-Za-z_][A-Za-z0-9_]*))`)
)

// maxAliasDepth mirrors JS `if (depth > 8) return null` — alias chains longer
// than 8 hops do not resolve.
const maxAliasDepth = 8

// constantResolver holds every constant extracted from the joined TS sources.
type constantResolver struct {
	literals map[string]string
	aliases  map[string]string
	objects  map[string]string
	sets     map[string][]string
}

// buildConstantResolver extracts literal/alias/object/Set constants from the
// joined source texts, exactly like buildConstantResolver in registry.js.
// Multiple files are joined because constants (e.g. model ids) are defined
// across freebuff-models.ts / gemini.ts and referenced by free-agents.ts.
func buildConstantResolver(texts []string) *constantResolver {
	res := &constantResolver{
		literals: make(map[string]string),
		aliases:  make(map[string]string),
		objects:  make(map[string]string),
		sets:     make(map[string][]string),
	}
	allText := strings.Join(texts, "\n")

	for _, m := range reLiteral.FindAllStringSubmatch(allText, -1) {
		res.literals[m[1]] = m[2]
	}
	for _, m := range matchAlias(allText) {
		res.aliases[m[1]] = m[2]
	}
	for _, m := range reObject.FindAllStringSubmatch(allText, -1) {
		res.objects[m[1]] = m[2]
	}
	for _, m := range reSetConst.FindAllStringSubmatch(allText, -1) {
		res.sets[m[1]] = resolveSetMembers(m[2])
	}
	return res
}

// matchAlias is reAlias plus the JS negative lookahead (?!['{0-9nOf]) applied
// to the target's first character: string/object/number constants and the
// literals null/false (and any identifier starting with n/O/f) are not
// aliases.
func matchAlias(text string) [][]string {
	var out [][]string
	for _, m := range reAlias.FindAllStringSubmatch(text, -1) {
		target := m[2]
		if target == "" || strings.ContainsRune("'{0123456789nOf", rune(target[0])) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// resolveSetMembers extracts the members of `new Set([...])` as raw strings:
// quoted members verbatim, identifiers by name (resolved by the caller).
func resolveSetMembers(inner string) []string {
	var models []string
	for _, m := range reSetMember.FindAllStringSubmatch(inner, -1) {
		if m[1] != "" {
			models = append(models, m[1])
		} else {
			models = append(models, m[2])
		}
	}
	return models
}

// resolve resolves a constant name to a model string: a literal wins
// immediately, otherwise alias chains are followed (depth-capped); an alias
// target containing a dot (`agents.primary`) is looked up as a property of an
// extracted object constant.
func (r *constantResolver) resolve(name string, depth int) string {
	if depth > maxAliasDepth {
		return ""
	}
	if value, ok := r.literals[name]; ok {
		return value
	}
	target, ok := r.aliases[name]
	if !ok {
		return ""
	}
	if strings.Contains(target, ".") {
		obj, prop, _ := strings.Cut(target, ".")
		objText, ok := r.objects[obj]
		if !ok {
			return ""
		}
		// Port of `new RegExp(\b${prop}\s*:\s*'([^']+)')` — first match wins.
		// The property name is derived from upstream TS alias targets and may
		// contain regex metacharacters: compile defensively and skip the
		// lookup instead of panicking.
		re, err := regexp.Compile(`\b` + prop + `\s*:\s*'([^']+)'`)
		if err != nil {
			return ""
		}
		m := re.FindStringSubmatch(objText)
		if m == nil {
			return ""
		}
		return m[1]
	}
	return r.resolve(target, depth+1)
}

// parseRootAgentMap extracts FREEBUFF_ROOT_AGENT_ID_BY_MODEL. Keys are model
// constants (`[MODEL_ID]: 'agent'`); each must resolve or the entry is
// dropped. This map WINS over the agent-models map.
func parseRootAgentMap(text string, resolver *constantResolver) map[string]string {
	modelToAgent := make(map[string]string)
	block := reRootBlock.FindStringSubmatch(text)
	if len(block) < 2 {
		return modelToAgent
	}
	for _, m := range reRootEntry.FindAllStringSubmatch(block[1], -1) {
		if model := resolver.resolve(m[1], 0); model != "" {
			modelToAgent[model] = m[2]
		}
	}
	return modelToAgent
}

// agentModels is one agent→models entry. Agents are kept as an ordered slice
// (not a map) because first-seen assignment in buildModelMapping depends on
// the entry order, which in the JS is the source object's insertion order.
type agentModels struct {
	agent  string
	models []string
}

// parseAgentModels extracts FREE_MODE_AGENT_MODELS: quoted agent keys mapped
// to either an inline `new Set([...])` (quoted members verbatim, identifier
// members only when resolvable) or a named Set constant (members resolved with
// fallback to the raw name). Entries that resolve to nothing are dropped. In
// the JS an object literal reuses the key slot, so a duplicated agent keeps
// the LAST entry's value at its FIRST position.
func parseAgentModels(text string, resolver *constantResolver) []agentModels {
	var agents []agentModels
	idx := make(map[string]int)
	block := reAgentBlock.FindStringSubmatch(text)
	if len(block) < 2 {
		return agents
	}
	inner := block[1]
	for _, m := range reAgentEntry.FindAllStringSubmatch(inner, -1) {
		agentID := m[1]
		models := orderedSet{}
		if m[2] != "" { // inline new Set([...])
			for _, member := range reSetMember.FindAllStringSubmatch(m[2], -1) {
				if member[1] != "" {
					models.add(member[1])
				} else if resolved := resolver.resolve(member[2], 0); resolved != "" {
					models.add(resolved)
				}
			}
		} else { // named Set constant
			for _, member := range resolver.sets[m[3]] {
				if resolved := resolver.resolve(member, 0); resolved != "" {
					models.add(resolved)
				} else {
					models.add(member)
				}
			}
		}
		if models.len() == 0 {
			continue
		}
		if i, ok := idx[agentID]; ok {
			agents[i].models = models.slice()
			continue
		}
		idx[agentID] = len(agents)
		agents = append(agents, agentModels{agent: agentID, models: models.slice()})
	}
	return agents
}

// retiredRootOverrides pins a model whose parsed root upstream retired
// server-side. FREEBUFF_ROOT_AGENT_ID_BY_MODEL — the only root map this text
// parser can read — still names base2-free-luna for Luna, and base2-free-luna
// is still in the upstream agent allowlist, but the backend now rejects a Luna
// turn on it with free_mode_legacy_luna_agent ("Retired Luna agent — start a
// new conversation"). That code classifies as ErrSessionInvalid, so the
// recovery path re-admits onto the same dead root until the retry budget is
// gone and the client sees 502.
//
// Upstream's own Web and CLI clients send base3-free-luna
// (FREEBUFF_{WEB,CLI}_BASE3_AGENT_ID_BY_MODEL). Those rows, and their
// allowlist entries, are derived through an Object.fromEntries spread the
// parser cannot evaluate, so a live refresh never sees them.
//
// ponytail: a one-model override, not a base3 map parser — one model is
// retired, not the catalog. Revisit if the upstream
// FREEBUFF_BASE3_HARNESS_DISABLED kill switch routes base2 back on, or if a
// second model starts returning a retired-agent code.
var retiredRootOverrides = map[string]string{
	"openai/gpt-5.6-luna": "base3-free-luna",
}

// buildModelMapping merges the root map (wins) with first-seen agent→models
// assignment, in entry order, and returns the model→agent map plus sorted
// model list.
func buildModelMapping(agentModels []agentModels, rootAgentByModel map[string]string) (map[string]string, []string) {
	modelToAgent := make(map[string]string, len(agentModels)+len(rootAgentByModel))
	for model, agent := range rootAgentByModel {
		modelToAgent[model] = agent
	}
	for _, entry := range agentModels {
		for _, model := range entry.models {
			if _, ok := modelToAgent[model]; !ok {
				modelToAgent[model] = entry.agent
			}
		}
	}
	// Applied after both sources so the live-refresh and offline-fallback
	// paths (both call this function) agree. Only remaps a model the sources
	// already serve — an override never invents a catalog row.
	for model, agent := range retiredRootOverrides {
		if _, ok := modelToAgent[model]; ok {
			modelToAgent[model] = agent
		}
	}
	allModels := make([]string, 0, len(modelToAgent))
	for model := range modelToAgent {
		allModels = append(allModels, model)
	}
	sort.Strings(allModels)
	return modelToAgent, allModels
}

// orderedSet is a deduping string set preserving insertion order — the Go
// equivalent of the JS `new Set()` used while collecting agent models.
type orderedSet struct {
	order []string
	seen  map[string]struct{}
}

func (s *orderedSet) add(v string) {
	if s.seen == nil {
		s.seen = make(map[string]struct{})
	}
	if _, ok := s.seen[v]; ok {
		return
	}
	s.seen[v] = struct{}{}
	s.order = append(s.order, v)
}

func (s *orderedSet) slice() []string { return s.order }

func (s *orderedSet) len() int { return len(s.order) }
