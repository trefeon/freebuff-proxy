# Architectural Boundaries

This file is the committed source of truth for the backend package matrix.
The machine-readable guard is `backend/internal/archtest/arch_test.go`
(allowlist per package; unknown edges fail the build). Extending the matrix
is an architecture decision: update the test, cite why, and keep the older
per-package guards (`config/layer_imports_test.go`,
`convert/layer_reviewfix_test.go`) consistent.

## Layer rules

The layer table lives in exactly one place: `DEPENDENCIES.md` (Internal
package matrix) with `backend/internal/archtest/arch_test.go` as the
machine-readable guard. This file records *why* each boundary exists, not
the edge list — do not duplicate the matrix here.

Direction of knowledge: leaves know facts, convert transforms JSON, upstream
talks to the wire, session/runs keep state, pool orchestrates tokens, server
mounts HTTP. `cmd/*` are thin entrypoints (main.go is a flag parser; the serve
mode lives in `internal/cli`).

## Boundary rules (why each exists)

1. **convert is transport-independent.** It must never import server/config/pool/
   upstream; it receives everything it needs via `Options`. Reason: translation
   is the highest-drift surface (ADR-0005); keeping it pure makes it unit-testable
   without servers and keeps protocol quirks in one place.
2. **upstream never imports session.** The wire client must not know about
   state management; session builds on top of it. A reverse edge is a cycle
   waiting to happen.
3. **session/runs never import pool.** Pool consumes them; upstream of the
   dependency graph only.
4. **pool never imports server/dashboard/convert.** The pool is a library the
   server drives; the admin surface is mounted by server. Pool errors are
   typed (`upstream_error`), never HTTP-shaped.
5. **dashboard never imports server.** Server mounts dashboard; the dashboard
   package exposes data APIs (`AdminRoutes` rows with auth levels) that server
   registers. Callbacks upward are forbidden.
6. **modelcat imports nothing internal.** Per-model facts (served/paused/
   premium/caps/efforts) are the single source of truth; consumers derive.
   Wire-dependent facts live in upstream, mapping in registry.
7. **config imports nothing internal.** It is the bottom layer: shape-only
   validation; semantic checks (served/unmetered) live where modelcat is
   visible (pool fire path + admin endpoint).
8. **leaves never import anything internal.** They own facts, they do not know
   about the rest of the system.
9. **testutil is test-only.** Production packages must never import it; the
   matrix enforces this by listing testutil only in test-visible edges (the
   archtest scan covers non-test files).
10. **cmd/* stay thin.** Entrypoints dispatch; logic lives in internal
    packages (cli subpackages per mode).

## Config boundary

`internal/config` is a bottom-layer package: it rejects all internal imports.
Consequences (learned the hard way):

- New env knobs get shape-only `Validate` checks there (e.g. provider/model
  slash check); semantic checks live in the pool fire path + admin endpoint.
- Catalog keys must be byte-ascending within group (`TestCatalogOrdered`).
- `dotenvKeys` must gain every new key; the e2e fixture
  (`frontend/e2e/fixtures/config-meta.json`) regenerates with `FP_REGEN_FIXTURE=1`.
- Direct-struct `Config` literals in tests bypass `Load` defaults, so
  `Validate` must accept zero-values (0 target = unset; pool normalizes).

## Admin boundary

- Dashboard business logic lives in `internal/dashboard`; the SPA consumes
  admin APIs only (never embeds proxy logic).
- Sensitive admin routes are gated by auth level + loopback when the factory
  `ADMIN_TOKEN` is active; see docs/security/SECURITY-MODEL.md.
- `/admin/api/*` is the SPA data surface; `/admin/*` non-API paths serve the
  embedded SPA; `/admin/reload` is the bearer-gated config reload.

## Violation report format

A matrix failure means the change crosses a documented boundary. Fix by
re-routing the dependency (preferred) or, only when the architecture
deliberately changes, by extending the allowlist with a reason and updating
this file and the affected CONTRACT.md. The archtest error message names the
offending package and import; keep that readable.