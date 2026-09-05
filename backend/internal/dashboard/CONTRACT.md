# Package Contract: `backend/internal/dashboard`

Task-local contract for agents modifying this package. Load before editing any file here.

## Purpose

Embedded admin surface: the data APIs the SPA consumes (`/admin/api/*`),
server-rendered page fragments/logs/config/token views
(`dashboard_*.go`, `events.go`), quota/sparkline computation, the
`AdminRoute` auth-level table consumed by `server.registerAdminRoutes`, and
the embed wiring (`assets_embed.go`/`assets_stub.go` → `dist/`). Business
logic for admin lives here; presentation lives in `frontend/`.

## Allowed dependencies

`config`, `logring`, `modelcat`, `phasetiming`, `pool`, `registry`,
`updatecheck`, `upstream` (archtest matrix).

## Forbidden dependencies

`server` (mounted by server; never calls back), `convert`, `session`,
`runs`, `telemetry`, `stealth`, `ratelimit`, `tokenestimate`,
`reasoningcache`. The dashboard consumes pool views and upstream facts; it
never drives relays.

## Critical invariants

- Every `AdminRoutes` row maps to a server handler; unmapped rows panic at
  wiring (table and mapper ship as one commit).
- Route auth levels select the middleware stacks they have always carried;
  sensitive rows keep the loopback gate while the factory `ADMIN_TOKEN` is
  active (INV-SEC-002). New sensitive routes must pick the correct level.
- Secrets render redacted (set/unset + counts) in effective-config and token
  views; `.env` writes go through the atomic save path (adminSaveMu +
  temp+rename, mode 0600), never direct writes.
- Metric histograms live on the `Dashboard` struct, never package globals
  (concurrent-server safety); `cardFromSnapshot` dedupes tokens/overview.
- Assets are served WITHOUT dashboard auth (the login page must render);
  only the assets subtree is exposed (`fs.Sub`), no directory listings.
- `admin_manifest.json` + `data/` describe navigation/catalog surfaces the
  SPA builds against; keep them consistent with `frontend/src` routes.

## Tests that protect it

`dashboard_live_test.go`, `dashboard_internal_test.go`,
`dashboard_maturity_test.go`, `events_test.go`, token/model/config/log page
tests; server-side `dashboard_test.go` + all `admin_*_test.go`.

## Safe modification patterns

- New admin API: add `AdminRoute` row + server handler mapping + auth level
  in one commit; extend the SPA call site + a dashboard test.
- New settings card: add the key to `keycatalog.go` first (dashboard forms
  build deterministically from it); the e2e fixture regenerates with
  `FP_REGEN_FIXTURE=1`.
- After editing display-struct literals: `git diff | grep '^-[^-]'` —
  dropped fields compile as zero values and silently hide cards.