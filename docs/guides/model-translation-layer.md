# Model Translation Layer (full research)

Complete map of how the 15-model `/v1/models` catalog (the "Available Models" list
9router imports) translates to the upstream FreeBuff wire protocol. Every claim is
cited to `reference/freebuff` (vendored CLI 0.0.150) or `internal/` (proxy source).

Companion to [freebuff-cli-internals.md](freebuff-cli-internals.md) §7 (catalog,
reasoning, agent ids) and [9router-integration.md](9router-integration.md) (wiring).
This guide is the per-model reference; the internals doc is the wire-protocol
reference.

---

## 1. What the dashboard list actually is

The pasted 9router "Available Models" list is the proxy's `GET /v1/models` catalog
(15 rows — matches `healthz {"models":15}`), imported by 9router's
`modelsFetcher` at `reference/9router/open-sse/providers/registry/freebuff.js:33`.
9router prefixes each id with the provider name → clients call
`freebuff/<model-id>`.

Two tokens in the paste are NOT catalog models:

| Token | Verdict | Evidence |
|---|---|---|
| `gpt-4o` | Manually-added row in 9router (stock OpenAI alias). The proxy maps it to a real catalog id — it never appears in `/v1/models` (`modelListed` is strict, aliases one-way). | `internal/config/config.go:340` (`defaultModelAliases`), `internal/server/server.go:3281-3290` |
| `smart_toy` | Material Symbols **icon name** in the vendored 9router dashboard: the neutral "untested" glyph (`testStatus === "ok" ? "check_circle" : "error" ? "cancel" : "smart_toy"`) rendered next to every model row, and the default `icon` placeholder for provider registry entries. Zero occurrences anywhere else (grep-verified, incl. `reference/freebuff`). Not a model, not a wire id. | `reference/9router/src/app/(dashboard)/dashboard/providers/components/ModelsCard.js:18-19`, `ModelRow.js:25`, `CompatibleModelsSection.js:26`, `PassthroughModelsSection.js:27`, e.g. `reference/9router/open-sse/providers/registry/anthropic.js:7` |

So: 15 real catalog ids + 1 alias (`gpt-4o`) + 1 UI artifact (`smart_toy`).

---

## 2. Upstream catalog truth (source of record)

The catalog is **not** in `packages/llm-providers` — it lives in
`reference/freebuff/common/src/constants/freebuff-models.ts` (2223 lines; id
constants in `freebuff-model-ids.ts`, paid/BYOK ids in `model-config.ts`).

Membership layers (each id below is `FREEBUFF_*_MODEL_ID`):

| Set | Members | Meaning |
|---|---|---|
| `SUPPORTED_FREEBUFF_MODELS` (7) | pro, flash, luna, m3, mimo, muse, glm | Admission-wide catalog |
| `FREEBUFF_MODELS` (CLI picker, 5) | pro, flash, luna, m3, mimo | What the CLI can select (`freebuff-model-selector.tsx`) |
| `FREEBUFF_WEB_MODELS` (7) | + muse, glm | Web picker |
| `FREEBUFF_WEB_ALL_MODELS` (8) | + kimi (god-only) | Web incl. god-only |
| `FREEBUFF_PROVISIONED_MODELS` (3) | pro-max, flash-max, luna-max | Per-account grants; **absent from every picker and quota list** |
| Fable | — | Not in any picker; limited-offer trial |
| Gemini `*-flash-lite` | — | **Not catalog members at all** — `GEMINI_HELPER_MODELS` subagent ids (`gemini.ts:4-15`, `free-agents.ts:403-406`) |

Quota pools (separate from picker sets): `PREMIUM_MODEL_IDS` (m3/pro/luna, 4/day
Pacific), `GLM_V52_MODEL_IDS` (referral pool, exactly-1h sessions),
`LIMITED_OFFER_MODEL_IDS` (fable, 1/day global wave), `STANDARD_MODEL_IDS`
(flash+mimo, unlimited), `LIMITED_FREEBUFF_MODEL_IDS` (mimo — sole limited-tier
model), `DESKTOP_PREMIUM_BUCKET`.

Dead/retired ids (never send, farming signals): `crof/glm-5.2` (deleted
2026-08-04), `hy3/*` (removed everywhere 2026-08-07), Ling 3.0 Flash, Laguna,
Greg, `moonshotai/kimi-k2.6` / `kimi-k2.7-code` (removed from pickers 2026-08-04,
still defined in `model-config.ts:86-90`), `fireworks/deepseek-v4-flash`.

Resolution rules (`freebuff-models.ts`):
- Exact wire id **or** `-YYYYMMDD`-suffixed variant accepted
  (`freebuffModelIdMatches`, 1782-1796).
- Plain unknown id → resolved server-side to
  `FALLBACK_FREEBUFF_MODEL_ID = deepseek/deepseek-v4-flash` (1452-1453, 2215-2223).
- The proxy mirrors this: unknown-but-allowed ids route to Flash on session
  fallback (`internal/session/session.go:45` `DefaultFallbackModel`).

---

## 3. Per-model translation table (all 15 catalog ids + deltas)

Columns: upstream family/provider route → tier & pool → picker membership →
effort ladder (default) → context → agent root (proxy + upstream) → proxy
coverage (effort-table / fallback row) → special handling.

| # | Wire id | Family / route | Tier & pool | Picker | Effort ladder (default) | Ctx | Agent root | Proxy: effort table | Proxy: fallback row | Notes |
|---|---|---|---|---|---|---|---|---|---|---|
| 1 | `deepseek/deepseek-v4-flash` | DeepSeek V4 (CrofAI→official→RunInfra→Infron cascade, `provider-routes.ts:129-135`) | free, STANDARD unlimited; **full tier only** since 2026-08-18 | CLI | `[low,high,max]` (high) | 1M | `base2-free-deepseek-flash` | ✅ | target of 5 rows | Session `DefaultFallbackModel`; upstream `FALLBACK` id; probe/smoke default; gpt-4o-class alias `deepseek-chat` target |
| 2 | `deepseek/deepseek-v4-pro` | DeepSeek V4 | premium, `PREMIUM_MODEL_IDS` (4/day Pacific) | CLI | `[low,high,max]` (high) | 1M | `base2-free-deepseek` | ✅ | → flash | Upstream `DEFAULT` id; gpt-4o alias target; muse fallback target |
| 3 | `deepseek/deepseek-v4-flash-max` | DeepSeek V4 | provisioned (`-max`) | none | `[low,high,max]` | extended | `base2-free-deepseek-flash-max` | ✅ | — | fixed (G2) |
| 4 | `deepseek/deepseek-v4-pro-max` | DeepSeek V4 | provisioned | none | `[low,high,max]` | extended | `base2-free-deepseek-pro-max` | ✅ | — | fixed (G2) |
| 5 | `mimo/mimo-v2.5` | MiMo (Xiaomi direct / OpenRouter lane) | free, **sole limited-tier model** (`LIMITED_FREEBUFF_MODEL_IDS`) | CLI | `[high]` only (adaptive thinking, no levels) | — | `base2-free-mimo` | ✅ | — | 0.0.150 binary registry **default**; limited-tier coercion target; `LimitedTierModels` proxy map |
| 6 | `mimo/mimo-v2.5-pro` | MiMo | 0.0.150 registry delta (binary bundle only, `model-config.ts:77-79`) | none | `[high]` | — | ❌ unrouted | ❌ | — | Removed from free mode 2026-08-04 (guard test `freebuff-models.test.ts:81-84`); paid/BYOK only (`model-config.ts:77`). Proxy must NOT route it — 400 model_not_found is correct. Effort entry removed (G4). |
| 7 | `anthropic/claude-fable-5` | Anthropic (limited-offer) | offer, 1/day global wave (`LIMITED_OFFER_MODEL_IDS`, `SESSION_LIMIT=1`), `dataUse: training` | none | `[low,medium,high,xhigh,max]` (high) | — | `base2-free-fable` | ✅ | → flash | Capacity-gated: reachable only while server advertises the offer; `claude-3-5-sonnet` alias target |
| 8 | `crof/kimi-k3-eco` | CrofAI native (Q2_K quant, $1/$4 per M) | god-only web (`WEB_ALL`) | none | CrofAI ignores `reasoning_effort` | — | `base2-free-kimi-k3-eco` | ❌ | — | `-eco` is load-bearing in the wire id (distinct build from `kimi-k3`) |
| 9 | `google/gemini-2.5-flash-lite` | Gemini helper | **helper subagent** (`file-picker`) | none | full ladder (proxy) | — | `file-picker` | ❌ | — | Not a free-catalog coding row; listed by proxy deliberately |
| 10 | `google/gemini-3.1-flash-lite` | Gemini helper | helper subagent (`file-picker-max`) | none | full ladder | — | `file-picker-max` | ❌ | — | legacy `-preview` id exists (`gemini.ts:8`) |
| 11 | `google/gemini-3.5-flash-lite` | Gemini helper | helper subagent (`file-picker-max`) | none | full ladder | — | `file-picker-max` | ❌ | — | current Gemini helper id |
| 12 | `meta/muse-spark-1.2-contributor` | Meta (contributor tier, $0.10/$0.002/$0.20) | premium, web-only; Convex queue 60 RPM/team; 10s → `deepseek-v4-pro` | none (web) | `[minimal,low,medium,high,xhigh]` (**xhigh**) | 1M | `base2-free-muse-spark` | ✅ | → deepseek-v4-pro (G3) | `reasoning_effort:'none'` = hard 400; `MUSE_SPARK_FALLBACK_MODEL_ID` (`freebuff-models.ts:387,405-408`) |
| 13 | `minimax/minimax-m3` | MiniMax | premium, `PREMIUM_MODEL_IDS` | CLI | none (adaptive) | 524k | `base2-free-minimax-m3` | ❌ (fine — upstream omits) | → flash | no effort levels |
| 14 | `openai/gpt-5.6-luna` | OpenAI via OpenRouter, **provider pinned `openai`**, `max_price` 0.5/3.0 | premium, `PREMIUM_MODEL_IDS` | CLI | `[low,medium,high,xhigh,max]` (high) | 1M (deliberate under-entry) | `base2-free-luna` | ✅ | → flash | OpenRouter slug — falls through to default OpenRouter route |
| 15 | `openai/gpt-5.6-luna-max` | OpenAI via OpenRouter | provisioned (`-max`) | none | `[low..max]` | extended | `base2-free-luna-max` | ✅ | — | provisioned per-account |
| 16 | `z-ai/glm-5.2` | CrofAI (Fireworks-era wire prefix; native `glm-5.2`) | referral-gated (`GLM_V52_MODEL_IDS`), exactly-1h sessions; `glmPromo` free-tier promo | web | CrofAI ignores `reasoning_effort` | — | `base2-free-glm` | ❌ | → flash | `crof/glm-5.2` is **dead** (farming signal — never send); 0.0.150 `glmPromo` field |

Sources: `freebuff-models.ts` (rows), `free-agents.ts` (roots),
`provider-routes.ts` (routes/pricing), `gemini.ts` (helpers),
`model-config.ts` (paid/deltas), proxy `internal/registry/registry_test.go:39-56`
(15-row `expectedFallback`), `internal/config/config.go:353-361`
(`defaultFallbackModels`), `internal/convert/convert.go:172-180`
(`modelReasoningEfforts`).

---

## 4. The translation pipeline (per request, 7 stages)

```
client model ─► 1.ResolveModel ─► 2.modelAllowed ─► 3.AgentForModel ─► 4.effort clamp
                (suffix→alias→-max)  (MODELS_ALLOW)   (registry root)   (convert)
                                                                    │
                wire: 6.body model = lease.Model ─► 5.Acquire (admission coercion)
                      + x-freebuff-model on session POST only (#106)
                                                                    │
                errors: 7.fallback (queue-wait only) / 409 / 429 / model_not_found
```

1. **ResolveModel** — `internal/registry/registry.go:358-420`
   - Strip effort/max suffix: `x(max)`, `x:max`, `(high)`… parsed at 363-381
     (`max` sets the upgrade flag; other rungs are dropped).
   - Alias lookup in `cfg.ModelAliases` — one hop, never recursed
     (`registry_test.go:613-627`). Defaults (`config.go:338-342`):
     `gpt-4o → deepseek/deepseek-v4-pro`, `deepseek-chat → deepseek/deepseek-v4-flash`,
     `claude-3-5-sonnet → anthropic/claude-fable-5`.
   - `-max` upgrade when suffix `max` or `PREFER_MAX_MODELS`: `MaxVariantOf`
     (`registry.go:81-88`: pro/flash/luna → `-max`) or `legacyMaxVariants`
     (96-100: `gpt-4o → pro-max`, `deepseek-reasoner → pro-max`, `deepseek-chat → flash-max`),
     gated by `maxUpgradeAllowed` (434-450: variant must be routed; limited tier
     restricted to `LimitedTierModels`; `ProvisionedModels` set gate — issue #140).
2. **Allowlist** — `server.go:3257-3274`: empty `MODELS_ALLOW` = allow all; else
   exact id or (PREFER_MAX_MODELS) the `-max` of an allowlisted base. Violation →
   `404 model_not_found`.
3. **Routing** — `AgentForModel` (`registry.go:477-484`): model → `base2-free-<model>`
   root (live fetch of the pinned TS sources, else `fallbackAgents` 122-157 +
   `fallbackRootByModel` 167-176). Unknown → `400 model_not_found`
   (`writeError` 3876-3878). Parity pinned by `TestFallbackParityWithPinnedUpstream`
   (`registry_test.go:118`).
4. **Effort clamp** — `convert.normalizeReasoning` (`convert.go:317-357`): effort
   from `reasoning_effort` field, else the model suffix; DeepSeek `medium → high`
   (341-346); else clamp DOWN to the model's rungs (`modelReasoningEfforts`
   `convert.go:172-180` — 9 entries; absent → full ladder `reasoningLadder`
   156-157);
   `reasoning.enabled=false` / thinking disabled suppresses entirely. Strict
   reasoning models (`mimo`, `deepseek-v4`, `kimi` — `isStrictReasoningModel`
   278-281) get `reasoning_content:""` on tool-call assistant messages
   (507-510).
5. **Admission coercion** — `pool.Acquire` (`pool.go:755-832`): the upstream
   session snapshot is authoritative. If it admitted a different model (limited
   tier coerces everything to `mimo/mimo-v2.5`, server-side — internals §11),
   `lease.Model = ss.Model` and the agent re-resolves for the admitted model
   (771-776; bridge 1153-1158). `ProvisionedModels` learned from `QuotaByModel`
   (465-473) gate future `-max` upgrades.
6. **Wire emission** — `chatAttempt` (`server.go:2659+`): `effectiveModel =
   lease.Model` (2685), body renormalized (2693-2696) so body == lease ==
   `codebuff_metadata` stays consistent with the session row. Chat POST carries
   **no** `x-freebuff-model` header (client.go:876-880, #106); only the session
   POST sends it (`CreateSessionForModel` client.go:949-957).
7. **Fallback / errors** — `FALLBACK_MODEL` fires **only** on waiting-room queue
   waits ≥ `FALLBACK_AFTER_MS` (server.go:2326-2353), surfaced via
   `X-FreeBuff-Fallback-Model` (2422). 429 `rate_limited` / `ip_capped` /
   `spend_limited` never fall back (chatAttempt 2849-2866; `writeError` 3759-3816).
   Session `model_unavailable` → coerce to `DefaultFallbackModel = flash`
   (session.go:865-874). Chat 409 `session_model_mismatch` → retry-once
   (`ErrSessionInvalid`, 2790-2800), or `LimitedIpError` (never invalidates) when
   the body carries `limited` (client.go:2249-2257). Unfit `(egress,model)`
   registry marks on `limited_ip` and refuses fast pre-acquire (server.go:2271-2294,
   `pool/unfit.go`), cleared on later success.

`/v1/models` shape (`server.go:3299-3330`): `{object:"list", data:[{id, object,
"model", created, owned_by:"freebuff", available, status,
current_access_tier}]}`; `status ∈ unknown|quota_exhausted|locked|available|
region_limited` (3349-3387). `MODELS_ALLOW` prunes via strict `modelListed`
(no `-max` expansion — the catalog surface stays exactly the allowlist);
`MODELS_HIDE_UNAVAILABLE` prunes unavailable rows. Aliases are never listed.

---

## 5. Reasoning effort — shared rules

- One ordered ladder `[minimal, low, medium, high, xhigh, max, ultra]`
  (`reasoning-effort.ts:20-28`); `clampReasoningEffort` clamps **DOWN**
  (46-75). Everything a client sends is a request, never a command.
- Server defaults per model: Pro/Flash/Luna high, Muse xhigh; GLM/Kimi/MiMo/M3
  omit (CrofAI ignores the field).
- DeepSeek `medium` is intentionally absent and rewritten to `high`
  (`resolveFreebuffReasoningEffort`, freebuff-models.ts:1946-1952) — proxy
  mirrors at convert.go:341-346.
- Proxy table (`modelReasoningEfforts`, 9 rows, `convert.go:172-180`):
  deepseek flash/pro + `-max` variants `[low,high,max]`, mimo `[high]`,
  fable/luna/luna-max `[low..max]`, muse `[minimal..xhigh]`.

---

## 6. Admission, gates, recovery

- **Agent binding**: LITE roots `base2-free-<model>` for all 9 free-session
  models (`free-agents.ts:350-360`); base3 twins per selectable model (CLI map 7
  incl. Fable; Web map 8 incl. Kimi+Muse). `-max` roots, muse, fable, and both
  cloud-planners are roots with **no base3 twin and no picker row**. Publisher
  must be `codebuff` (`isFreebuffRootAgent`, 619-622); wrong agent+model → 403
  `free_mode_invalid_agent_model`.
- **Capacity gates**: Muse 60 RPM/team shared Convex queue; 10s →
  `deepseek-v4-pro` (upstream fallback, not proxy). Fable 1/day global wave,
  `dataUse: training`, not in picker. GLM referral-gated, exactly-1h sessions,
  `glmPromo` free-tier promo. Kimi god-only web.
- **model_locked** (CLI `use-freebuff-session.ts:475-530`): explicit pick →
  DELETE then re-POST with the requested model; background rejoin → revert to
  `currentModel`, no DELETE. Proxy mirrors: refresh `model_locked` →
  `EndSession` + retry (session.go:858-864).
- **Session coercion**: 409 `session_model_mismatch` on chat = retry-once; with
  `limited` marker = `LimitedIpError` (session stays valid, model unfit marked).

---

## 7. Gaps & recommendations (status ledger)

**G1 — RESOLVED.** Effort table covers 9 of 15 (`modelReasoningEfforts`,
`convert.go:172-180`): the deepseek 4 bases + their `-max` variants, mimo,
fable, luna/luna-max, and muse. The other 6 — kimi-k3-eco and glm-5.2
deliberately (CrofAI ignores `reasoning_effort`), minimax-m3 (no effort levels;
upstream omits), and the 3 gemini rows (helper models, no upstream restriction)
— pass the full ladder unclamped, which is correct (9 covered + 6 absent = 15).
`SetModelEffortLookup` remains available as optional registry-driven hardening.

**G2 — RESOLVED.** `isDeepSeekModel` (`convert.go:272-279`) now checks all four
suffixes, so the `-max` DeepSeek rows get the `medium→high` rewrite and
prompt-cache hints (#84) exactly like their bases.

**G3 — RESOLVED.** `defaultFallbackModels` (`config.go:351-359`) now has the muse
row → `deepseek-v4-pro` per `MUSE_SPARK_FALLBACK_MODEL_ID`. (The
`FALLBACK_MODEL` path still only fires on waiting-room queue waits — never 429
quota exhaustion — by design.)

**G4 — RESOLVED.** `mimo/mimo-v2.5-pro` is a paid-only removal (2026-08-04), not
a gap; the proxy's effort entry was removed and 400 `model_not_found` is the
correct routing (documented in §3 row 6).

**G5 — RESOLVED.** The pinned snapshot re-syncs the limited tier to mimo-only
(`CLOUD_PLANNER_LIMITED_MODEL_ID = LIMITED_FREEBUFF_MODEL_ID` in
`testdata/upstream/free-agents.ts`, `freebuff-models.ts:1371-1376`) and the Go
fallback row `base2-free-cloud-planner-limited` now mirrors it
(`registry.go:143`, `mimo/mimo-v2.5`).

**G6 — documented non-fix.** gemini rows are helper models.
`google/gemini-*-flash-lite` belong to `GEMINI_HELPER_MODELS` subagents
(file-listing/research), not the free coding catalog. Listing them in
`/v1/models` is a deliberate proxy choice (so codebuff clients' file-picker
subagents route through the proxy) — but 9router users should not default to
them. Optionally gate them behind `MODELS_ALLOW`.

**G7 — documented non-fix.** parser skips unquoted keys.
`basher` (unquoted TS key in the upstream mirror) is skipped by `parse.go` on
both sides. Cosmetic today (no model id depends on it) but a future unquoted
key carrying a model would silently drop. This is upstream-identical port
parity (`parse.go:10-13` deliberately mirrors `registry.js` quirks) — do not
'fix'.

---

## 8. Reference index

| Fact | Location |
|---|---|
| Catalog rows, sets, pools, ladders, fallback/default ids | `reference/freebuff/common/src/constants/freebuff-models.ts` |
| Id constants | `reference/freebuff/common/src/constants/freebuff-model-ids.ts` |
| Paid/BYOK ids, 0.0.150 deltas | `reference/freebuff/common/src/constants/model-config.ts` |
| Effort ladder + clamp | `reference/freebuff/common/src/constants/reasoning-effort.ts` |
| Provider routes/pricing | `reference/freebuff/common/src/constants/provider-routes.ts` |
| Agent maps, roots, helpers | `reference/freebuff/common/src/constants/free-agents.ts` |
| Gemini helper ids | `reference/freebuff/common/src/constants/gemini.ts` |
| CLI model_locked / unavailable | `reference/freebuff/cli/src/hooks/use-freebuff-session.ts:475-543` |
| CLI picker + glmPromo | `reference/freebuff/cli/src/components/freebuff-model-selector.tsx` |
| Proxy pipeline | `internal/registry/registry.go:358-484`, `internal/server/server.go:3257-3387`, `internal/convert/convert.go:156-357` |
| Proxy defaults | `internal/config/config.go:338-361` |
| Proxy catalog parity | `internal/registry/registry_test.go:39-56,118` |
| 9router import + smart_toy icon | `reference/9router/open-sse/providers/registry/freebuff.js:33-34` (validateUrl + modelsFetcher → proxy `/v1/models`), `reference/9router/src/app/(dashboard)/dashboard/providers/components/ModelsCard.js:18-19` + `ModelRow.js:25` |
