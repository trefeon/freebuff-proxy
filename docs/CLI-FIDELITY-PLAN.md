# CLI-Fidelity Fill-Gaps & Optimize Plan — freebuff-proxy

**Goal:** What CLI serves, proxy serves. CLI-only reverse of `reference/freebuff @ 887786e`. Remove browser impersonation, keep wire parity.

## 0. Baseline: Already CLI-faithful (7f6f15b + 5687206)

* `TLS_FINGERPRINT=""` plain `crypto/tls` default (`config_env.go:488` no auto), `Bun/1.3.14` for session / `ai-sdk/openai-compatible/1.0.0/codebuff` for chat (`upstream/client.go:83,122`), `SanitizeHeaders` only, no `ApplyProfileHeaders` (`upstream/client_chat.go:91`).
* `REQUEST_JITTER 2s → 200ms` (`config_env.go:493`), `ch 64` (`server/stream_shared.go:215`), `Flush per Write` + `keepalive 15s` + byte-preserving fast path (`openai_chunk_pipeline.go`).
* Hermetic `env -u AUTH_TOKENS -u ADMIN_TOKEN go test ./backend/...` `ok`, `sync-upstream --check` `exit 0` (`registry` + `wire` SAMEs), cloud VPS benchmark `no stealth` logs, `~80-111 tok/s` streaming, `header 2.5-10.6s` upstream-dominated.

## 1. Gap Inventory (CLI vs Proxy)

### P0 — Wire fidelity (must fill, watchdog priority)

1. **Fingerprint `enhanced-` vs plain** — CLI `cli/src/utils/fingerprint.ts:87-128` full JSON (`fingerprintVersion 2.0`, `system{manufacturer,model,serial,uuid}`, `cpu{brand,cores,physicalCores}`, `os{platform,distro,arch,hostname}`, `runtime{nodeVersion,shell,cpuCount}`, `network{macAddresses sorted, interfaceCount}`, `machineId MachineGuid`) → `sha256 base64url enhanced-` (`process-cache 143-151`, fallback `codebuff-cli-` `131-133`). Proxy `auth_fingerprint.go` narrower (`hostname,MACs,CPU`) — must mirror exact CLI JSON per CLI-only (SYSTEM.md notes narrower is opaque but watchdog says `enhanced-` gap is P0).
2. **Instance-owner pid lock** — `cli/src/utils/freebuff-instance-owner.ts:12-54` `freebuff-instance-owner.json {instanceId,pid}` + `isFreebuffInstanceOwnedByDeadLocalProcess` via `process.kill(pid,0)` liveness (single local CLI guard). Proxy pooled never needs single-user lock; bridge per-token must respect `USER+SERVER-MINTED instanceId` rotation, not pid file — document divergence, no `kill` in proxy.
3. **Compact-session header** — `common/constants/freebuff-models.ts:2354-2370` `x-freebuff-compact-session:1` on `GET /api/v1/freebuff/session` poll omits `rateLimitsByModel` quota (CLI `sdk` compact). Proxy must send `compact=1` on poll; verify `upstream/client.go:newRequest` adds it when `compact`.
4. **Heartbeat omission** — `x-freebuff-heartbeat 45s` Desktop-only (`freebuff-models.ts:2354`), CLI TUI (`hooks/use-freebuff-session.ts:16-45`) never sends (`Desktop-only`). Proxy `pool` must never send `heartbeat` — already correct, gate verification.
5. **Trace.jsonl intentional divergence** — CLI `cli/src/utils/trace-writer.ts:9-80` + `common/types/contracts/trace.ts:11 RecordStep` → `trace.jsonl` one JSON line per step (`run-agent-step.ts:422-640`). Proxy is transport only (not executor `agents/base2`, `packages/agent-runtime`) — must NOT replicate; document as intentional gap.
6. **Auth wire** — `POST /api/auth/cli/code {fingerprintId} → {loginUrl,fingerprintHash,expiresAt epoch ms}` + `GET /status?fingerprintId&fingerprintHash&expiresAt` until `401` vs `200`. Proxy `gen-freebuff-token.sh` random per-run vs wizard stable — keep both.
7. **Chat envelope + model/agent map** — `sdk/impl/llm.ts:getProviderOptions` `codebuff_metadata {run_id, client_id 13-char base36, freebuff_instance_id}` + `data_collection:deny` + `stream:true` + `stop:"cb_easp"`; `common/constants/freebuff-models.ts` + `free-agents.ts` `FREEBUFF_ROOT_AGENT_ID_BY_MODEL` via `getFreebuffCliAgentIdForModel`. Proxy `injectEnvelope` + `registry/modelcat` pinned at `887786e` via `sync-upstream` — verify exact, `NewClientID` shape `substring(2,15)` pad `0`.

### P1 — Session/run lifecycle

7. **Instance rotation** — row keyed by `USER+SERVER-MINTED instanceId` (`freebuff-session.ts` `active`), rotated every admission POST, not fingerprint/token. Proxy `session` manager already rotates but verify `holdsLiveFreebuffSlot`.
8. **Agent-runs** — `sdk/impl/database.ts` `POST /api/v1/agent-runs START/FINISH` with `Authorization Bearer` + `x-codebuff-api-key` duplicate (`codebuff-web-api.ts:70-71`). Proxy `runs` manager must `START` before first chat per `instanceId` and `FINISH` honest (anti-ban contract), not coalesced across models.
9. **Rate limits** — `common/types/freebuff-session.ts` `rateLimitsByModel Record<string,FreebuffSessionRateLimit>` `recentCount` includes active reservation `1.0` → `0.1` rounding after settle (TUI wire facts 2026-08-31). Proxy `pool/quota` already wholesale-assigned, not max.

### P2 — Not needed in proxy (intentionally diverge)

* TUI: OpenTUI `alternate-screen` (`cli/src/index.tsx:404`), `chat-input-bar`/`suggestion-engine`/`ask-user-bridge`, `freebuff-model-selector`, `landing-screen`, `command-registry` slash filtering — **keep out of proxy**.
* Skill/agent registry, `handleSteps` vs LLM `stream-xml-parser` tool loop (`agents/base2`, `packages/agent-runtime`) — proxy is transport, not executor.
* PostHog/analytics (`analytics.ts:148-152` `anonymousId` in `~/.config/manicode`, `APP_LAUNCHED`), `.codebuffignore` — drop intentionally.
* `read-files` `.env` block + `isSensitiveEnvFilePath` — proxy config loader is separate, not needed to replicate inside sandbox.

### Proxy weight to remove / opt-in (watchdog dead-code)

* Browser TLS default — **already removed** (`7f6f15b`).
* `REQUEST_JITTER=2s` → `200ms` (`5687206`), SG `0` explicit — CLI has `0`, `200ms` is SafeMode anti-ban compromise (watchdog notes jitter gap); `0` remains CLI-exact opt-in.
* Unbuffered `ch` — **already buffered** `64` (`5687206`).
* Keep `stealth/profiles.go` + `stealth/tls.go` as opt-in (`TLS_FINGERPRINT=chrome126/etc`), not default; dashboard/metrics kept (admin, not dead code).

## 2. Fill-Gaps Roadmap

**Phase A — P0 wire exactness (1-2 days, hermetic + live)**
- A1 `auth_fingerprint.go` → build full enhanced JSON matching `fingerprint.ts:87-128` (sorted MACs, `fingerprintVersion 2.0`, `MachineGuid` fallback). Keep output `enhanced-` 43-char.
- A2 `upstream/injectEnvelope` audit vs `sdk/impl/llm.ts:getProviderOptions` + `model-provider.ts` — add `cache_debug_correlation` if missing, verify `stop:cb_easp` exact.
- A3 `upstream/client.go:newRequest` header audit — ensure `GET /api/v1/freebuff/session` sends `x-freebuff-compact-session=1` when `compact`, never `heartbeat`, `takeover` only on superseded.
- A4 `client_id` shape — add conformance test `generateClientId` vs `JS substring(2,15)` (13-char base36, pad `0`).

**Phase B — P1 lifecycle (1 day)**
- B1 Session `pool/pool_lifecycle.go` `nextDelayMs` exact `POLL_INTERVAL_ACTIVE_MS 30s` vs `remaining+1s` near expiry, no heartbeat.
- B2 `runs` `START` once per `instanceId` (not per model), `FINISH` honest on `pool.Shutdown 30s` (already `stop_grace_period 30s`).

**Phase C — Remove / optimize (0.5 day)**
- C1 Delete any `TLS_FINGERPRINT=auto` hardcode left in scripts/docs (done except `scripts/sync-upstream.sh` pin comments — keep).
- C2 Keep `ch 64` + `200ms jitter` as CLI-faithful; no further buffering that would delay `Flush`.

## 3. Optimization Final Pass (done, keep)

* Channel `64` decouples `bufio.Scanner 64KB→16MiB` from `W+Flush` (~70-120 deltas/s) without adding latency.
* `200ms` jitter + `SanitizeChunkOpts` `sync.Pool` + single `Marshal` if mutated = `<1ms` per chunk (`header→first delta 0.248-0.421s` is upstream think, not proxy).
* `X-Accel-Buffering: no` + `keepalive 15s` + `": connecting"` grace.

## 4. Verification (gated)

* **Gate 1 — `bash scripts/sync-upstream.sh --check` then `--test-all`** (`exit 0`, `All pinned files match`, `check-upstream OK`, `registry fallback parity`) before each change (AGENTS.md). `--test-all` also runs `registry` tests; stale `reference` yields wrong wire.
* **Gate 2 — Hermetic** `env -u AUTH_TOKENS -u ADMIN_TOKEN go test ./backend/...` `ok` (`config` expects `TLS ""` + `env_example` empty, `TLSFingerprint` empty CLI-faithful).
* **Gate 3 — Live Cloud VPS** `cloud-vps:3457` `curl -N POST /v1/chat/completions stream:true` timestamp per `data:` → `header 2.5-10s` + `~80-111 tok/s` (`tokens≈chars/4`), `docker logs | grep stealth` `0` lines (plain), `GET /v1/models` 6 available vs 19 registry expected (tier, not TLS).
* **Dead-code audit:** Browser `stealth` profiles kept opt-in (`TLS_FINGERPRINT=chrome126/etc`) not default; dashboard/metrics kept (admin needed, not dead); `200ms jitter` vs CLI `0` is SafeMode compromise — `REQUEST_JITTER=0` remains CLI-exact opt-in (`SG` already `0`).
