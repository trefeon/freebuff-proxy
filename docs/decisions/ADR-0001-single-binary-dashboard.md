# ADR-0001 — Single binary with embedded dashboard

Status: Accepted

Context:
The gateway must run as a local service with zero runtime dependencies, and it
ships an operational web dashboard. Shipping the dashboard as a separate Node
process would double the install surface, break offline operation, and make
version skew between proxy and UI a permanent hazard.

Decision:
Compile the Svelte 5 SPA (`frontend/`) into `backend/internal/dashboard/dist`
and embed it in the binary via `go:embed`. The dashboard is served under
`/admin`, toggled at runtime by `DASHBOARD_ENABLED`, and requires no build tag.
Unmatched `/admin/*` deep links fall back to the SPA index.

Reasoning:
A single binary means install, update, service registration, and container
image stay trivial; the release artifacts are one file per OS/arch. The embed
happens at compile time, so CI must prove the committed dist is fresh, or a
stale bundle sails silently into every release.

Alternatives considered:
- Separate Node service: rejected; runtime dependency, version skew, packaging pain.
- Build tag (`dashboard` vs `nodashboard`): rejected; two binary variants double test/release matrices.
- Runtime fetch of the SPA from a CDN: rejected; offline requirement and supply-chain surface.

Consequences:
- `npm run build` output MUST be committed atomically with frontend changes (CI enforces freshness).
- `task build` builds frontend first, then the Go binary.
- Dev uses Vite on `127.0.0.1:5173` proxying `/admin/*` to the local gateway; prod uses the embedded bundle.
- Dashboard assets are served without dashboard auth (login page must render pre-auth), but sensitive admin APIs are separately gated.

Invariants:
- Committed `backend/internal/dashboard/dist` == fresh `npm run build` output.
- Dashboard business logic stays in `backend/internal/dashboard`; the SPA consumes admin APIs only.

Affected packages:
`frontend/`, `backend/internal/dashboard`, `backend/cmd/freebuff-proxy`.

Related tests:
CI `frontend` job (`ci.yml`): `npm run check/lint/format:check/build/test:e2e` + `git diff --exit-code -- backend/internal/dashboard/dist`.