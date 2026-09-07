# ADR-0020 — Review-driven persistence corrections (amends ADR-0016/0019)

Date: 2026-09-07. Status: Accepted. Branch: `refactor/db-persistent-dashboard`.

Two read-only reviewer passes over the overlay + pages_state slices found
30 grounded gaps; this record covers the ones that change semantics. All
other findings were bug fixes inside already-approved behavior.

## 1. Five keys become RestartOnly

`LOG_LEVEL`, `LOG_FORMAT`, `LOG_FILE`, `LOG_RING_SIZE`, `LISTEN_ADDR`.
`applyReloadedConfig` fans out to cfgStore/registry/pool/rateLimiter only:
the logger is built once at boot and the socket never re-binds. POST on
these keys now returns `setting_restart_only`; the UI appends "Applies
after restart." Claiming live-apply was a false success report, and for
`LISTEN_ADDR` a security-relevant one (loopback intent not effected).

## 2. AUTO_DISCOVER_TOKEN blocked from the overlay

Env-only by loader design (`os.Getenv` in LoadOpts, effective view
hardcodes true). Storing false changed nothing while GET showed
`source=db`. POST now 400s; the key joins `SettingsBlockedKeys`.

## 3. Pages allowlist 9 → 12 ids

`setup`, `metrics`, `traces`, `playground` mount `recordPageVisit` but
PUT 404d. They are real pages; the backend now allows all 12 NAV_ITEMS
ids plus the `shell` chrome key.

## 4. Carry continues past empty candidates; multi-era reported

An existing-but-history-empty first candidate no longer blocks later
files. When a second candidate still holds rows after a carry, boot
WARN-logs the skipped file with per-table counts (staged trio copy,
never imported — rowid collisions forbid cross-file merge). Session
import logs hash collisions (later file wins); `.bak` archives are
re-consulted when `sessions_persist` is empty and the live JSON is gone.

## 5. Staging copies the WAL trio

Direct ATTACH first (live rw files carry with WAL intact); on failure a
staged temp copy of main+`-wal`+`-shm` (read-only mounts fail ATTACH
with error 14). Sources are never modified.

## 6. GET honesty + PUT shape

`GET /admin/api/settings` gains additive `degraded:bool` (nil store);
pages PUT rejects non-object data (400) and maps envelope overflow to
413 like over-cap data. `DbOverrideSave.svelte` introduces no new visual
primitive (ghost Button, 11px row status, `--fp-*` tokens, `role=status`
— all DESIGN.md-pinned), so no DESIGN.md change was needed.
