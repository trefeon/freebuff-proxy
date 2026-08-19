# Version Stability & Ban Findings (2026-08-19 field report)

Operational findings from live deployment across the SG VPS (`172.188.64.104`) and
acerblue. **Bottom line: v0.11.2 in bridge mode is the proven-stable deployment;
newer versions (`v0.11.3+`) with `PREFER_MAX_MODELS=true` in pooled mode are
correlated with instant account bans.** This document captures the evidence, the
mechanism, and the safe deployment recipe.

---

## TL;DR

| Version | Mode | `PREFER_MAX_MODELS` | Outcome |
|---|---|---|---|
| **v0.11.2** | **bridge** | n/a (base-only clients) | ✅ **Stable, days uptime, no ban** |
| v0.11.3+ | pooled | `true` | ❌ **403 `free_mode_invalid_agent_model` storm → account demoted → banned** |
| v0.11.3+ | pooled | `false` | ✅ Untested at scale; should be safe (base-only) |
| v0.11.9-dev (unreleased) | pooled | `true` + provisioned-model gate | ✅ Fix landed but **not yet battle-tested** |

---

## The incident (why accounts get banned)

### Trigger chain

1. `PREFER_MAX_MODELS=true` upgrades every requested base model to its `-max`
   variant (`deepseek/deepseek-v4-flash` → `deepseek/deepseek-v4-flash-max`).
2. Upstream provisions `-max` roots **per-account** — most full-tier accounts
   have only **base** models provisioned (`rateLimitsByModel` lists
   `deepseek-v4-pro`, `deepseek-v4-flash`, `mimo-v2.5`, `minimax-m3`,
   `gpt-5.6-luna`, `kimi-k3-eco`, `muse-spark` — **no `-max` entries**).
3. Every upgraded request hits upstream with an agent the account cannot run →
   **403 `free_mode_invalid_agent_model`** on *every* chat call.
4. The 403 storm (rapid repeated refusals, short retry loop) is a classic
   abuse/failure signature → upstream demotes the account (`full` → `limited`)
   then **bans** it (`{"status":"banned"}`).

### Evidence (2026-08-19, VPS)

- Token A (`2e709d9b-…`, minted during the v0.11.3-era `-max` storm):
  probe → `{"status":"banned","accessTier":null}`. Dead upstream.
- Token B (`b48ef53a-…`, earlier): same — banned.
- Token C (`1ff0c735-…`, live CLI on VPS, account "Hermes Bleu"):
  probe → `{"status":"active","accessTier":"full"}`, `rateLimitsByModel` lists
  **base models only** (5/day each, `pacific_day`, entitlement base=5). This is
  the definitive signal: **full tier ≠ `-max` provisioned**.
- Token D (`b52a5647-…`, account "Veroke", minted via the dashboard login
  wizard on v0.11.2 bridge): probe → `{"status":"active","accessTier":"full"}`.
  Verified working: single `deepseek/deepseek-v4-flash` chat → 200, answered.

### Why v0.11.2 bridge is safe

- **Bridge mode** relays the client's own `Authorization: Bearer <token>`
  upstream per request. No pooled session, no model-lock, no `-max` upgrade
  applied by the proxy (the client asks for exactly the model it sends).
- v0.11.2 predates the `PREFER_MAX_MODELS` suffix/`-max` upgrade machinery
  (that landed in v0.11.3+), so the proxy never *invents* a `-max` variant the
  account can't run.
- Verified: days of uptime on acerblue (v0.11.2, bridge), healthy, no bans.

---

## The fix (landed, unreleased)

`2eb47ed` gates the `-max` upgrade on the account's **provisioned model set**
learned from the upstream session response (`rateLimitsByModel` keys):

- `Config.ProvisionedModels` — learned at probe/admission, never operator-set.
- `registry.maxUpgradeAllowed` — refuses `-max` variants absent from the
  provisioned set, even for `full` tier.
- Result: a base-only account keeps base models; the 403 storm cannot start.

**Status:** committed to `main`, full test suite green, **not yet released**.
This is the correct long-term fix; v0.11.2 bridge remains the safe stopgap.

---

## Safe deployment recipe (proven)

```bash
# On the target box (acerblue / VPS):
cd ~/freebuff-proxy
git checkout v0.11.2
DOCKER_BUILDKIT=0 docker build --network=host --build-arg VERSION=v0.11.2 -t freebuff-proxy:latest .
docker compose up -d
```

`.env` (bridge mode — `AUTH_TOKENS` EMPTY, clients carry their own token):

```
AUTH_TOKENS=
LISTEN_ADDR=:3457
UPSTREAM_BASE_URL=https://codebuff.com
COST_MODE=free
SAFE_MODE=true
ADMIN_TOKEN=<dashboard password>
MODELS_ALLOW=deepseek/deepseek-v4-flash,mimo/mimo-v2.5
API_KEYS=            # optional; bridge bypasses the gate by design
```

Client (9router on acerblue → VPS):

```yaml
providers:
  freebuff-vps:
    type: openai
    base_url: http://172.188.64.104/v1
    api_key: <client's own FreeBuff token>
models:
  deepseek-v4-flash:
    provider: freebuff-vps
    model: deepseek/deepseek-v4-flash
```

---

## Rules for staying unbanned

1. **Base models only.** Use `deepseek/deepseek-v4-flash` / `mimo/mimo-v2.5`
   — never `-max` variants on a base-only account.
2. **Never set `PREFER_MAX_MODELS=true` on a pooled deployment** until
   `2eb47ed` (or later) ships and is verified — the upgrade silently 403-storms.
3. **Bridge > pooled for multi-client fan-in** unless you control the token
   set: bridge gives each client its own credential (no shared-session
   correlation, no model-lock).
4. **Don't mint tokens from datacenter egress** (SG VPS). Issue #140 P0:
   residential egress (acerblue / home) is the safe minting path. Tokens minted
   on the VPS *can* be healthy (token C/D were), but the cohort risk is real.
5. **Probe before you trust**: `GET /api/v1/freebuff/session` with
   `Authorization: Bearer <token>` is zero-cost — check `status`/`accessTier`
   before pointing traffic at an account.

---

## Version diff notes

- **v0.11.2** (`ca206d4`): dashboard 1-click key generators. Last version
  before the `-max` upgrade machinery. **Stable in bridge.**
- **v0.11.3–v0.11.8**: `PREFER_MAX_MODELS` suffix parsing, tier-aware gating
  (limited-tier only — insufficient: full tier still upgraded to unprovisioned
  `-max`), `MODELS_ALLOW` allowlist, dashboard SPA + login page, API key copy.
  The `-max` gate only blocked `limited` tier, so **full-tier base-only
  accounts were still upgraded → 403 storm → ban**.
- **v0.11.9** (released): `SessionCallTimeout` tighter-bound fix. Still carries
  the pre-provisioned-gate `-max` behavior.
- **`main` `2eb47ed`** (unreleased): provisioned-model gate — the actual ban
  fix. Deploy this (or v0.11.2) only.
