# Threat Model (lightweight)

Realistic threats only. Each row: attack surface, existing mitigation, gap,
recommended action (with pointer to TECH-DEBT where open).

## Remote client abuse

- Surface: `/v1/*` from the network.
- Mitigation: `API_KEYS` gate (when set), per-IP buckets (`ratelimit`),
  CW request validation/budgets, bounded access-log gates.
- Gap: pooled mode with empty `API_KEYS` is silently open (SECURITY-MODEL
  gap 3). Action: set `API_KEYS` or loopback-bind; startup warning added.

## Admin endpoint abuse

- Surface: `/admin/*` from the network.
- Mitigation: four independent controls (auth/authorization/CSRF/rate
  limit), loopback gates under factory default, atomic config saves with
  rollback.
- Gap: factory-default remote bootstrap (SECURITY-MODEL gap 1, HIGH).
  Action: rotate `ADMIN_TOKEN` immediately on every deployment; P1
  remediation options recorded.

## Credential leakage (logs/dumps/errors/metrics/repos)

- Surface: every place a token could be echoed.
- Mitigation: central redaction (`telemetry`, `upstream/do()`), secret-free
  repo (audit-verified), redacted fixtures, gitignored artifact classes.
- Gap: redaction coupled to `cb_`/`Bearer` shapes; `.dockerignore` lag
  (both recorded). Action: extend redaction with new token formats;
  `.dockerignore` mirrored in this program.

## CSRF against the admin

- Surface: browser-driven state-changing POSTs.
- Mitigation: double-submit cookie + header, Origin/Sec-Fetch-Site checks,
  SameSite=Strict session cookie.
- Gap: cookie-less requests get origin checks only (adequate: no session
  cookie means no authenticated action). No action.

## Malformed / hostile requests

- Surface: request bodies, tool schemas, SSE from upstream.
- Mitigation: parameter whitelist, schema depth/node budgets, `$ref` cycle
  guard, body size caps, absent-tolerant parsers, bounded XML hold.
- Gap: no fuzz targets (TECH-DEBT P2). Action: add fuzz seeds for the XML
  extractor, schema normalization, SSE sanitization.

## Resource exhaustion

- Surface: admission storms, drain queues, connection churn, bridge cache,
  log gates.
- Mitigation: global + per-model admission caps, bounded drain/finish
  queues with TTL/force-drop, LRU bridge cache with idle eviction, bounded
  access-log tables, webhook throttling.
- Gap: none known; shutdown/persistence paths are lifecycle-tested.

## Rate-limit bypass / quota gaming

- Surface: clients cycling identities or models to dodge local locks.
- Mitigation: per-token (not per-client) quota locks, hybrid credential
  routing by exact `API_KEYS` match, `MODELS_ALLOW` allowlist, RPM/RPD
  ledgers.
- Gap: none known.

## Upstream manipulation (malicious or drifting upstream)

- Surface: upstream responses, registry pins, wire shapes.
- Mitigation: classify-once with redaction, absent-tolerant parsing, pinned
  constants + parity tests + 6h drift CI with classified ports.
- Gap: drift queue needs human review (by design). No action beyond keeping
  the sync workflow healthy.

## Supply chain

- Surface: Go modules, npm packages, CI actions, release artifacts, Docker
  base image.
- Mitigation: pinned modules (go.sum), locked npm (package-lock.json),
  SHA-pinned actions, Dependabot weekly groups, `go mod verify` in CI,
  dependency-review on PRs, CodeQL weekly, SLSA attestations on release
  artifacts, `golang:1.26.6` Docker base with binary-only final stage.
- Gap: no SBOM (documented out of scope); npm minor/patch auto-PRs still
  need test gates (they have them: CI frontend job on every PR).

## CI / release compromise

- Surface: workflow permissions, third-party actions, tag pushes, artifact
  publishing.
- Mitigation: least-privilege per-workflow permissions, SHA pins, release
  re-gates the full hermetic suite on tag push, SLSA provenance attached
  post-release, no auto-merge on drift PRs.
- Gap: release.yml test/frontend jobs inherit workflow-level write
  permissions (minor; scoped in this program). No `pull_request_target`
  usage, no secret exfiltration paths found.

## Log leakage to operators' machines

- Surface: `LOG_FILE`, `dump/`, log ring, `/admin/logs`.
- Mitigation: same central redaction; `dump/` gitignored; ring bounded
  (50–5000); trace level documented as wire-visible.
- Gap: `LOG_LEVEL=trace` intentionally shows wire bodies (redacted but
  verbose); operator awareness, documented.