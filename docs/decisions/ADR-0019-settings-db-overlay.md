# ADR-0019 — Settings DB overlay (env > db > file > default)

Status: Accepted

Context: operators edit knobs in the dashboard Settings page, which today
rewrites the whole `.env` file. File rewrites are coarse (one textarea, no
per-key validation feedback), they fight container-managed environments
(where the process env or an orchestrator owns the file), and every restart
story depends on that single file surviving. The v2 store
(`data/freebuff-proxy.db`, ADR-0016) already persists control state
(settings/pages/sessions/tokens); per-key operator settings belong there,
layered over the file without replacing it.

Decision: a DB settings overlay with fixed precedence and secrets excluded.

- Precedence, lowest to highest: built-in defaults < JSON `-config` <
  `.env` file < DB overlay < explicit process environment. Rationale:
  12-factor env stays king (compose secrets keep working), while the DB
  overlay beats the file so UI edits stick across restarts without
  rewriting `.env`.
- Secrets NEVER enter the DB: `AUTH_TOKENS`, `ADMIN_TOKEN`, `API_KEYS`,
  `WEBHOOK_URL`, plus `UPSTREAM_BASE_URL` and `DB_PATH`. POST rejects them;
  they stay env/.env-only. Load filters them defensively, so even a
  tampered row cannot flip pooling, auth, or the upstream endpoint.
- Overlay rows live in the generic `settings` table under `config:<KEY>`
  namespacing (raw strings, verbatim). `config.OverlayRowKey` /
  `config.OverlayFromRows` own the mapping; the store stays opaque.
- `POST /admin/api/settings {key, value}` validates the key (known,
  writable catalog key) and the value (kind-shaped, then a full trial Load
  through existing config validation) BEFORE persisting; then it reloads
  through the same `applyReloadedConfig` machinery as `/admin/reload`
  (reuse, never a fork). Live keys hot-apply; `RestartOnly` catalog keys
  persist but report `restart_only` — they take effect on restart, exactly
  like the `.env` editor.
- `DELETE /admin/api/settings/:key` drops the overlay row; the effective
  value falls back to file/env. `GET /admin/api/settings` returns the
  effective view with a `source` tier per key (`env|db|file|default`).
- `/admin/config` (full `.env` save) works unchanged; its success message
  now names DB-overridden keys, because the overlay still wins until
  deleted.
- The Settings page hydrates sources fetch-only (no localStorage) and
  shows a "DB override" badge + per-row Reset (DELETE) affordance.

Alternatives considered: rewriting `.env` per key from the UI (rejected:
same file-fight, plus concurrent-save tearing the document); allowing
secrets in the DB for "one store for everything" (rejected: a world-
readable-by-mistake sqlite with tokens/passwords is worse than two stores
with clear ownership); a separate overlay table (rejected: the generic
settings table with a prefix is enough, no schema bump).

Consequences: boot opens the store BEFORE the first Load so the overlay
applies from process start (serve mode). Every admin reload path funnels
through `adminHandlers.loadConfig` (overlay-aware); bare `config.Load` on
the admin surface is a regression by definition. A missing/unreadable DB
degrades to file/env/default with mutations 503 — never a failed boot.

Invariants: no new Go deps; `CGO_ENABLED=0` holds; config imports nothing
internal (the overlay arrives as a plain map); retention never touches
control tables; redaction rules apply to the settings view like the config
view.

Affected packages: `internal/config` (overlay contract + load tier +
sources), `internal/store` (`DeleteSetting`/`ListSettings`),
`internal/server` (`admin_settings.go`, overlay-aware reloads, DELETE verb
+ CSRF), `internal/dashboard` (manifest rows), `internal/cli` (boot
overlay), `frontend` (badge/reset wiring, rebuilt `dist/`).

Related tests: `TestSettingsOverlay*`/`TestOverlayCoversCatalog`/
`TestValidateSettingValue`/`TestSettingSources*` (config),
`TestSettingsDeleteAndList` (store), `TestSettingsOverlayCycle`/
`TestSettingsPostRejects`/`TestSettingsDeleteCSRF`/
`TestSettingsWithoutStore` (server API), `TestAdminManifestFrontendParity`
+ `TestAdminRoutesAllRegister` (parity).
