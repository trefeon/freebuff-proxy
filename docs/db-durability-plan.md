# Database durability plan — embedded SQLite persistence

## Goal

Every operator-visible data the proxy tracks survives restart, container
recreate, binary replace (auto-update), and host reboot — with zero manual
steps. Data is only ever deleted when the operator explicitly asks for it
(dashboard wipe / API delete). No silent loss on crash, upgrade, or reset.

Today `SESSION_PERSIST=true` already resumes upstream sessions and active
agent runs across restarts, and the compose directory mount keeps the state
file and `.env` across container recreates. Everything else — per-token
spend/usage accounting, cooldown and ban memory, bridge-mode client cache,
reasoning restore hints — is in-memory only and resets to zero on every
restart. This plan closes that gap.

## Non-goals

- No new service, sidecar, or network dependency. Single binary and the
  existing compose setup stay as they are.
- No multi-node replication or clustering. This is single-node durability.
- No change to the upstream wire protocol or the anti-ban contract.

## Decision: embedded SQLite (pure Go), one file

Use an embedded SQLite database via a pure-Go driver (`database/sql`
compatible, no cgo) stored as a single file under the already-persisted
working directory (default `./data/freebuff.db`, overridable via `DB_PATH`).

Why:

- Keeps the single-binary story: cross-compiles for all release targets,
  no second container, no operator-run database server.
- The dashboard needs real queries (per-token x per-model quota, spend
  windows, audit views). A key-value store would force hand-rolled indexes.
- cgo-based drivers are rejected: they break Windows builds and the
  minimal container image.

Why not the alternatives:

- Postgres / Redis / any server DB: operational burden (second service,
  backups, credentials) for a single-node write rate of a few rows per
  chat. Rejected.
- Bare key-value files (one JSON per domain): what we have today, and the
  reason accounting resets. Concurrent writers + crash windows make this a
  data-loss generator. Rejected for anything beyond the legacy snapshot.
- `VACUUM INTO` (single-statement consistent snapshot) is the backup
  primitive — not raw file copies, which are unsafe while the DB is open
  in WAL mode.

## What gets stored

| Domain | Examples | Durability |
|---|---|---|
| Session resume | Upstream session instance id + expiry, per-model quota snapshots | Synchronous commit before ack |
| Agent runs | Active run id per token + agent | Synchronous commit before ack |
| Pool roster | Configured token slots and model locks | Synchronous commit before ack |
| Spend / usage ledgers | Rolling 24h + Pacific day/week/month buckets, request counters, spend-limited flags | Batched (5s / 500-row / shutdown flush) |
| Cooldowns / bans | Temporary + hard ban memory, cooldown-until | Batched |
| Bridge-mode cache | Per-client entries, LRU position, daily-limit survivors | Batched |
| Reasoning hints | Content/tool-call bindings (TTL, best-effort) | Memory first, persisted best-effort |
| Ephemeral (never stored) | Request logs ring, metrics samples, rate-limiter buckets, in-flight streams | Memory only |

Raw credentials are never written. All rows are keyed by non-reversible
token hashes, matching how the existing session snapshot already behaves.

## Durability tiers (why "perfect" does not mean "slow")

- Tier-0 (synchronous): session, run, roster, config version. Rare writes;
  committed inside the requesting transaction before the proxy acknowledges.
- Tier-1 (batched): every per-chat counter. A single writer goroutine
  coalesces them into one transaction every 5 seconds (or 500 rows, or at
  shutdown). A `kill -9` can lose at most a 5-second window; a graceful
  shutdown (the compose `stop_grace_period` window) loses nothing.
- Tier-2 (memory only): reasoning hints, logs, metrics, limiter buckets.
  Loss costs extra upstream work, never corruption.

## Storage, backup, restore

- Location: `DB_PATH`, default `./data/freebuff.db` inside the persisted
  working directory (the same directory mount that already keeps `.env`
  and the session snapshot). File `0600`, directory `0700`, created on
  first boot. A directory mount (not a single-file bind) is required so
  atomic commits work.
- Connection: WAL mode, `busy_timeout=5000`, foreign keys on. Single
  writer; readers never block chats. Contention retries with backoff
  (3 attempts); a write that still cannot commit fails the admission
  safely rather than recording phantom state.
- Startup backup: before applying any schema migration, the proxy takes a
  consistent snapshot (`VACUUM INTO`) next to the DB. A failed migration
  refuses to serve and keeps the backup — the old binary restarts cleanly.
- Schema: versioned, forward-only migrations (`schema_version` table).
  Downgrades never auto-run.
- Corruption fallback: an unreadable DB is quarantined aside
  (`freebuff.db.corrupt.<timestamp>`) and the proxy rebuilds from the
  legacy JSON session snapshot when present, so a first crash reads as
  "resumed", never as "wiped".
- Restore: dashboard backup download, restore-from-upload, and explicit
  wipe. Restore closes, replaces, reopens (re-applying all pragmas, which
  are per-connection) and fsyncs the containing directory so the new file
  is durable before serving resumes. Wipe requires auth plus explicit
  confirmation and is the only path that deletes operator data.
- Import-once: when the DB is empty and a legacy JSON session snapshot
  exists, it is imported on first boot, then archived to `.bak`.

## Auto-update continuity

Boot order becomes: open DB (create + pragma) → snapshot backup →
migrate → load sessions/runs → prewarm agent runs → serve. The updater
(dynamic binary replace, compose recreate, host reboot) needs no new
steps: the DB file sits on the persisted mount, migrations are idempotent,
and a failed migration leaves the previous backup plus a non-serving
process instead of a half-migrated DB.

## Rollout slices

- Slice 1 (first diff): `store` package (open / migrate / backup /
  checkpoint) + session + agent-run cutover to SQLite, including the
  legacy-snapshot import and the corruption-fallback path. Proves
  end-to-end resume with one migration to test.
- Slice 2: spend/usage ledgers, cooldowns/bans, bridge cache via the
  batched writer + shutdown drain.
- Slice 3: reasoning-hint persistence (best-effort), dashboard
  backup/restore/wipe endpoints, operator docs (`README`, `.env.example`).

## Acceptance (slice 1)

- Restart resumes unexpired sessions and active runs without re-admission
  or re-START (existing persist tests, repointed at SQLite).
- `kill -9` mid-chat → restart resumes Tier-0 with zero loss.
- Corrupt-DB boot quarantines the bad file and resumes from the legacy
  snapshot instead of serving empty.
- Failed migration keeps a verified backup and refuses to serve.
- Restore re-applies pragmas and dir-fsyncs; post-restore restart is clean.
- Full hermetic suite stays green (`go test ./backend/...` with auth env
  unset; store tests use in-memory / temp-dir databases only).
- Binary-size and cold-boot regression noted in the slice-1 PR
  description (pure-Go SQLite cost).

## Risks

- Pure-Go SQLite adds binary size (~15MB) and compile time. Mitigated by
  isolating it behind one package so unrelated builds/tests are unaffected.
- Per-chat counters must never sit on the synchronous path. Mitigated by
  the Tier-1 single-writer design and the admission-fails-safe rule.
- Migration bugs are the classic wipe vector. Mitigated by snapshot-first,
  forward-only, refuse-to-serve-on-failure, and the corruption quarantine.

## Open questions for review

1. Default `DB_PATH`: `./data/freebuff.db` (new subdirectory) vs the
   working-directory root. Proposal picks the subdirectory to keep the
   mount root tidy; objection path is one line in config.
2. Reasoning-hint persistence in slice 3 vs never (accept cold-start
   re-thinking after restarts). Proposal: best-effort persist; cheap,
   bounded by TTL.
3. Retention window for startup backups (proposal: keep last 3, prune on
   boot).
