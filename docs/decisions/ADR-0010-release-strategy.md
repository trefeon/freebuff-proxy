# ADR-0010 — Release strategy and versioning

Status: Accepted

Context:
Public single-binary project with installers, a dashboard embed, and
user-mandated versioning discipline. Releases must be reproducible, gated on
the same checks CI runs, and never produced from a dirty or stale state.

Decision:
- **Versioning** (user-mandated): bump the MINOR only for major changes;
  bump the PATCH/stage version for minor changes and features. Version comes
  from git tags (`vX.Y.Z`); GoReleaser stamps `main.version` via ldflags.
- **Gates**: `release.yml` re-runs the hermetic suite on tag push (gofmt,
  build, race tests, `go mod verify`, golangci-lint) plus the frontend job
  (check/lint/format/build + committed-dist freshness) before GoReleaser runs.
  A tag push cannot release past a failure.
- **Artifacts**: GoReleaser v2, `CGO_ENABLED=0`, 3 OS × 2 arch, one archive
  format per OS (tar.gz / zip), `checksums.txt`, LICENSE + `.env.example` +
  start/gen scripts bundled. SLSA build provenance attached via
  `actions/attest` (per-asset, no SBOM).
- **Docker**: `docker compose up -d --build`; image builds from the repo,
  unprivileged user, healthcheck on `/healthz`.
- **Smoke test**: release artifacts are exercised locally (binary boots,
  `/healthz`, basic API + dashboard availability) before tagging; CI does not
  need live upstream credentials (E2E uses the mock upstream).

Reasoning:
The tag-push path bypasses branch CI, so release.yml must re-gate
everything CI would have caught. Hermetic tests (AUTH_TOKENS/ADMIN_TOKEN
unset) keep releases independent of the operator's local env. The dist
freshness check exists because the dashboard is embedded: a stale bundle in a
release is invisible until users load `/admin`.

Alternatives considered:
- Nightly/dev releases: rejected; versioning rule + installers assume stable tags.
- SBOM generation: deferred; documented as out of scope in `.goreleaser.yml`.

Consequences:
- Every commit must keep `frontend` buildable and dist fresh (or the release gate fails).
- Version bumps are deliberate commits/tags, never accidental.
- Release flow: feature branch → PR (4 required CI checks) → squash merge
  to main → tag vX.Y.Z → push tag (release.yml gates + goreleaser +
  attest) → `gh release view` to verify assets. Direct pushes to main are
  rejected by branch protection (not by convention).

Invariants:
- A release never ships with failing tests, format violations, or stale generated assets.
- `main.version` reflects the tag (dev builds default to `dev` → debug logging).
- Hermetic test env is the release gate, not the developer's local env.

Affected packages:
`.goreleaser.yml`, `.github/workflows/release.yml`, `backend/cmd/freebuff-proxy` (version), `scripts/`, Taskfile.

Related tests:
`release.yml` gates; E2E suite in `backend/cmd/freebuff-proxy`; CI frontend job.