# Package Contract: `backend/internal/telemetry`

Task-local contract for agents modifying this package. Load before editing any file here.

## Purpose

Observability plumbing: Prometheus `/metrics` series (uptime, model count,
per-token messages/requests/runs/cooldown, per-model quota gauges
`freebuff_proxy_quota_recent`/`_limit`), structured logging helpers, header
and secret redaction for logs, and Prometheus label escaping. Consumers:
server (`/metrics`), pool (quota gauges), upstream (classification lines),
dashboard (log stream via `logring` on top).
## Public API (stable surface)

- Logging: `NewLogger(verbose, logFile)`, `New(level, logFile, format)`,
  `ParseLevel`, `FormatAttrValue`, `FormatAttrPair`, `FlattenAttrs`.
- Redaction: `RedactHeaders`, `RedactSecrets`.
- Metrics: `RecordModelUnavailableSkip` (+ quota/series recorders used by
  pool/server).

## Allowed dependencies

`config` (archtest matrix). Nothing else internal.

## Forbidden dependencies

`server`, `pool`, `upstream`, `dashboard`. Telemetry observes; it must never
call back into what it measures (no import cycles, no behavior coupling).

## Critical invariants

- Label escaping is mandatory on every dynamic label (token ids, model ids);
  unescaped labels corrupt the `/metrics` feed for all clients.
- Redaction helpers cover every credential header (`Authorization`,
  `x-codebuff-api-key`, `x-api-key`, `anthropic-api-key`); new credential
  headers must extend the redaction list in the same commit.
- Metrics updates are cheap and non-blocking on the request path (no locks
  held across I/O, no network in metric recording).

## Tests that protect it

`telemetry_test.go` (series, redaction, escaping), `model_unavailable.go`
coverage via availability tests.

## Safe modification patterns

- New metric: stable name + help text, label set reviewed for cardinality
  (per-model is bounded by the catalog; per-request labels are forbidden),
  extend the `/metrics` section of `docs/api/API-ENDPOINTS.md`.