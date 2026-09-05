# ADR-0009 — Reasoning effort mapping and multi-turn cache

Status: Accepted

Context:
Clients send `reasoning_effort` (OpenAI) or `reasoning.effort` (Anthropic) as
low/medium/high/max, plus `minimal`. Upstream exposes per-model reasoning
engines with their own tiers and budgets. Mapping must be model-aware (a
`high` on one model means something else on another), and the proxy additionally
restores prior reasoning across turns so the upstream agent does not re-derive
it.

Decision:
- Effort extraction lives in `convert/effort.go`; the ladders (client effort →
  upstream tier, thinking-budget scaling, `minimal` → no-thinking) are owned by
  `modelcat` — a single source of truth, never a local copy in convert.
- Think-tag extraction (`<thinking>…</thinking>`) and reasoning-delta handling
  are part of the streaming translation.
- `internal/reasoningcache` stores per-conversation reasoning state
  (canonical-keyed, tool-id binding) so a multi-turn tool loop restores prior
  reasoning instead of paying for re-derivation; threaded into convert via a
  `ReasoningLookupFn` (issue #279), never a global hook.
- `convert` is allowed exactly two extra imports for this path
  (`reasoningcache`, `tokenestimate`), pre-approved and pinned in the archtest
  matrix; `tokenestimate` also serves `/v1/messages/count_tokens` and streaming
  input-token estimation (o200k_base BPE).

Reasoning:
Reasoning behavior is the most model-specific part of the translation layer.
Centralizing ladders in modelcat means a new model or an upstream tier change
is one fact edit plus parity tests, not a whitelist edit in convert. The cache
is bounded, keyed canonically (not by client message ids, which are untrusted),
and never holds secrets.

Alternatives considered:
- Pass-through of client effort verbatim: rejected; breaks on models without
  that tier and misleads upstream budgeting.
- Process-global reasoning hook: rejected; testability and cross-request
  isolation (the current design threads the lookup per server instance).

Consequences:
- modelcat parity tests pin ladders against the upstream snapshot.
- Cache correctness (canonical key stability, tool-id binding, eviction) has
  its own test files; a broken cache degrades quality but must never leak data.

Invariants:
- Ladders live in modelcat; convert only consults them.
- Reasoning cache keys never depend on client-controlled ids.
- `minimal`/`low` never silently upgrade to `high` for a model that lacks it.

Affected packages:
`internal/modelcat`, `internal/convert` (effort.go, reasoning_restore path), `internal/reasoningcache`, `internal/tokenestimate`.

Related tests:
`effort_modelcat_test.go`, `reasoning_tiers_test.go`, `reasoning_restore_test.go`, `canonical_key_test.go`, `toolid_binding_test.go`, `cache_test.go`, `estimator_test.go`.