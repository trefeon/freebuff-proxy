# Dependency Map

Two kinds of dependency matter here: the **internal package matrix** (Go import
edges inside `backend/`, enforced by `backend/internal/archtest/arch_test.go`)
and the **external dependency set** (modules, npm packages, CI actions). The
internal matrix is the load-bearing one; extend it only deliberately.

## Internal package matrix

Source of truth: `backend/internal/archtest/arch_test.go`. The test scans every
non-test Go file under `backend/internal/` and fails on any internal import not
listed in `allowed`. `cmd/` entrypoints are exempt (they may import everything;
`internal/server` and `internal/cli` already bound the stack below them).

### Layers

| Layer | Packages | May import (internal) | May NOT import |
|---|---|---|---|
| **Leaves** (own facts, know nothing) | `config`, `modelcat`, `clicreds`, `egress`, `notify`, `phasetiming`, `ratelimit`, `reasoningcache`, `stealth`, `tokenestimate`, `tokenhealth`, `updatecheck`, `upstream/login` | nothing | any internal package |
| **L1 small dependents** | `telemetry` | `config` | leaves other than `config`, everything above |
| | `logring` | `telemetry` | |
| | `registry` | `config`, `modelcat` | |
| | `testutil` | `config` | |
| **L2 protocol/wire** | `convert` | `modelcat`, `reasoningcache` (issue #279), `tokenestimate` (issue #279) | `config`, `pool`, `upstream`, `server`, `dashboard`, `registry`, `session`, `runs`, `stealth`, `telemetry`, `logring` |
| | `upstream` | `config`, `stealth`, `telemetry`, `upstream/login` | `session` (state lives above the wire), `pool`, `server` |
| | `upstream/testmock` (test-only) | `modelcat`, `upstream` | |
| **L3 stateful middle** | `session` | `telemetry`, `upstream` | `pool`, `server` |
| | `runs` | `session`, `upstream` | `pool`, `server` |
| **L4 orchestration** | `pool` | `config`, `modelcat`, `notify`, `phasetiming`, `registry`, `runs`, `session`, `upstream` | `server`, `convert`, `dashboard`, `stealth`, `ratelimit`, `logring`, `tokenestimate`, `reasoningcache` |
| | `dashboard` | `config`, `logring`, `modelcat`, `phasetiming`, `pool`, `registry`, `updatecheck`, `upstream` | `server` (mounted by server; must never call back) |
| **L5 top** | `server` | everything below (full list in arch_test.go) | — |
| | `cli` + `cli/{port,setup,update,service,doctor,refreshtoken,validate}` | bounded subsets, see arch_test.go | upward edges |

### Pinned forbidden edges (with reasons)

| Edge | Why |
|---|---|
| `convert` → `server` | convert is a pure JSON transformation layer; transport lives in server |
| `pool` → `dashboard` | pool must not know about the admin surface |
| `modelcat` → `upstream` | modelcat owns model facts; must not depend on the wire |
| `registry` → `server` | registry resolves models; it is not an orchestration point |
| `upstream` → `session` | upstream is the wire client; session manages state on top of it |
| `session` → `pool` | pool consumes sessions; the reverse would be a cycle |
| `dashboard` → `server` | server mounts dashboard; dashboard must not call back |
| any leaf → anything | leaves own facts and know nothing |

Per-package guards that predate archtest and must stay consistent: `config/layer_imports_test.go`, `convert/layer_reviewfix_test.go`.

## External dependencies

### Go modules (go.mod, all pinned by go.sum)

| Module | Where used | Why it exists | Swap risk |
|---|---|---|---|
| `github.com/refraction-networking/utls` | `internal/stealth/tls.go` | Browser TLS fingerprint emulation (JA3) | High; upstream API churns |
| `github.com/tiktoken-go/tokenizer` | `internal/tokenestimate` | Local `o200k_base` BPE token estimation | Medium; o200k parity matters |
| `golang.org/x/net` | HTTP/2 negotiation, proxy code | Standard extended net stack | Low |
| `golang.org/x/sys` | Windows service integration, platform bits | Standard syscall surface | Low |
| (indirect) `brotli`, `dlclark/regexp2`, `klauspost/compress`, `xyproto/randomstring`, `x/crypto`, `x/text` | transitive | — | — |

No test-only external Go modules. `CGO_ENABLED=0` for release builds.

### Frontend (frontend/package.json)

| Package | Role |
|---|---|
| `svelte` 5 | SPA framework (runes, no stores boilerplate) |
| `tailwindcss` 4 + `@tailwindcss/vite` | styling, token-driven |
| `vite` 6 | build + dev server |
| `@fontsource/ibm-plex-{sans,mono}` | self-hosted type (no CDN) |
| `@lucide/svelte` | icon set only |
| `@playwright/test` | e2e suite |
| `svelte-check`, `eslint`, `prettier` | check/lint/format gates |

### CI / release actions (all SHA-pinned in workflows)

`actions/checkout` v7, `actions/setup-go` v7, `actions/setup-node` v7, `golangci/golangci-lint-action` v9, `goreleaser/goreleaser-action` v7, `actions/attest` v4, `github/codeql-action` v4, `actions/dependency-review-action` v5. Dependabot updates them weekly with grouped minor/patch PRs.

### Docker

`Dockerfile` builds from `golang:1.26.6` (multi-stage, unprivileged runtime user), `docker-compose.yml` pins healthcheck on `/healthz`. `CGO_ENABLED=0`.

## Version pins that must move together

- `go.mod` Go version ↔ `.github/workflows/*` `go-version-file: go.mod` ↔ Docker base image tag.
- Frontend `package.json` version ↔ embedded dashboard build; the committed `backend/internal/dashboard/dist` must equal a fresh `npm run build` (CI enforces via `git diff --exit-code`).
- Registry pinned upstream constants (`backend/internal/registry/testdata/upstream/`) ↔ upstream vendor SHA, handled by `scripts/sync-upstream.sh` (see docs/upstream/UPSTREAM-COMPATIBILITY.md).
- `CLI_VERSION` config default ↔ informational dashboard display only; deliberately no wire impact.