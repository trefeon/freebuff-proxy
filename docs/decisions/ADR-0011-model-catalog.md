# ADR-0011 — Model catalog, registry, and fallback resolution

Status: Accepted

Context:
Clients request models as `provider/model` (often through aliases or harness
names); upstream runs agents, not model ids; availability changes by tier,
region, time window, and upstream pauses. One place must own model facts, and
the resolution path (alias → model → agent → fallback) must be deterministic
and drift-detectable.

Decision:
- `internal/modelcat` owns per-model facts: served/paused/premium, caps,
  context windows, reasoning tiers. Single source of truth; every consumer
  derives. `catalog.go` is the snapshot; `catalog_test.go` pins it against the
  upstream reference SHA.
- `internal/registry` owns resolution: model→agent (`pinnedFallbackAgents`,
  per-model roots), alias resolution (`ResolveModel`), plus the offline pinned
  catalog (six upstream constant files in `testdata/upstream/`) and live
  refresh (`REGISTRY_REFRESH`, failure keeps previous state).
- Fallback chain: per-model fallback (e.g. queue-wait `FALLBACK_MODEL`) →
  quota fallback (`QUOTA_FALLBACK_MODELS`) → local degradation (luna holds its
  scarce session instead of hammering quota 429s). Unavailability signals:
  `model_unavailable` refusals are remembered per model
  (`MODEL_UNAVAILABLE_CACHE_TTL`), `/v1/models` marks them
  (`available:false, status:"region_limited"`), `MODELS_HIDE_UNAVAILABLE`
  prunes them for picker clients, `MODELS_ALLOW` enforces an operator allowlist.
- Sync with upstream pins is mechanical (ADR-0006): when pins change, update
  `pinnedFallbackAgents`/`pinnedFallbackRootByModel` (+ `retiredRootOverrides` in `parse.go`) to match and re-run drift analysis.

Reasoning:
Facts (what upstream has) vs mapping (how we resolve) vs policy (what we
prune/allow) are three different concerns with three different update triggers
(upstream sync, code mapping, operator config). Conflating them was how stale
models ("minimax-m3") and region bugs happened; the split makes each legible
and independently testable.

Alternatives considered:
- Live-only resolution (no pinned catalog): rejected; cold start and tests
  need a deterministic fallback.
- Registry owning facts too: rejected; modelcat's stdlib-only leaf status is
  what keeps facts free of wire imports.

Consequences:
- A model rename upstream is: sync pins → update registry maps → parity test
  → modelcat facts; never a rename in only one place.
- `MODELS_ALLOW` rejection is `404 model_not_found` after alias resolution;
  this order must stay stable for router clients.
- Catalog keys must be byte-ascending within group (`TestCatalogOrdered`).

Invariants:
- modelcat facts never import the wire; registry never owns policy defaults
  that belong in config (MODELS_ALLOW is a config knob, not a registry constant).
- Pinned snapshot == recorded upstream SHA, or the parity test fails.

Affected packages:
`internal/modelcat`, `internal/registry`, `internal/server` (models.go),
`internal/pool` (unfit/mark-model paths).

Related tests:
`catalog_test.go` (parity), `reasoning_tiers_test.go`, `registry_test.go`,
`retired_root_test.go`, `unfit_test.go`, models endpoint tests in `server`.