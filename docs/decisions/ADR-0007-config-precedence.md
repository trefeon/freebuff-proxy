# ADR-0007 — Configuration precedence and hot reload

Status: Accepted

Context:
Operators configure the proxy via environment, `.env` files, and an optional
JSON file, and the admin dashboard edits `.env` at runtime. Precedence must be
deterministic, secrets must stay out of logs and the repo, and reloads must not
corrupt the file or half-apply a bad config.

Decision:
- Precedence, lowest to highest: **built-in defaults < JSON `-config` < `.env` < environment**.
  `.env` is resolved automatically from the platform config directory
  (Linux `~/.config/freebuff-proxy/.env`, macOS `~/Library/Application Support/freebuff-proxy/.env`,
  Windows `%APPDATA%\freebuff-proxy\.env`); a `./.env` in the working directory
  still wins when present. Some keys are environment-only by design
  (`AUTO_DISCOVER_TOKEN`; the three request-translation knobs).
- Typed loader with validation in `internal/config` (`config.go`,
  `config_env.go`, `config_validate.go`); list values are comma-separated in
  env and arrays in JSON. `dotenvKeys` catalog (byte-ascending) defines the
  `.env` surface; `keycatalog.go` mirrors it.
- Hot reload: `POST /admin/reload` and dashboard config saves re-load from disk
  and atomically swap the config pointer (`atomic.Pointer`). Construction-fixed
  state (pool roster, listener) does not change on reload; the dashboard says so.
- Admin config save is serialized (`adminSaveMu`) and atomic
  (temp file + rename, mode 0600): no stale-restore clobber, no truncated `.env`.
  Validate-before-write, rollback on rejection.
- Secrets are redacted in the effective-config table (set/unset + counts), never
  printed in logs; `config.example.json` placeholders are deliberately rejected
  by validation.

Reasoning:
One deterministic chain beats ad-hoc "which file wins" logic; the atomic swap
makes reloads safe under concurrent traffic. Validation at load time catches
typos early; shape checks live in `config` (bottom layer), semantic checks
(served/unmetered) live where `modelcat` is visible.

Alternatives considered:
- Single source (env only): rejected; platform config dirs + JSON are documented UX.
- Live-editable full config object: rejected; pool is construction-fixed by design.

Consequences:
- New keys must be added to: `keycatalog.go` (byte-ascending), `dotenvKeys`,
  `.env.example`/`.env.full-example`, README config table, and the e2e config
  fixture (`FP_REGEN_FIXTURE=1` regenerates it).
- Direct-struct Config literals in tests bypass `Load` defaults; `Validate`
  must accept zero-values (0 target = unset, pool normalizes).
- `config` is a bottom layer: it must not import any internal package.

Invariants:
- Precedence chain never changes silently; reload never mutates construction-fixed state.
- `.env` writes are atomic and 0600; validation failure rolls back.
- No secret is ever logged or shown in full in the dashboard.

Affected packages:
`internal/config`, `internal/server` (admin_config, reload), `backend/cmd/freebuff-proxy` (flag wiring).

Related tests:
`config_test.go`, `config_env_test.go`, `config_validate_test.go`, `keycatalog_test.go`, `layer_imports_test.go`, `admin_password_test.go` (config save paths), dashboard `dashboard_test.go`.