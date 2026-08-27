# Rotation Lab — Backend + Frontend Plan

**Goal:** Sediakan lab studi 4 skema rotasi token agar user bebas pilih `gimana token mau dipakai` (aman vs eksperimen banned), untuk jawab pertanyaan: *“rolling per 1 session per 1 auth key lebih bagus daripada 1 auth key dihabisin 5/5 baru rolling?”*

**Default aman tetap `drain`** (hot-session-first, `tok0×5 → tok1×5 …`) sesuai `acquire.go:612-765` + `README:330` + `docs/9router-integration.md:57` (NEVER round-robin — farm detection).

## Backend

### 1) `internal/config/config.go` + `.env.example`
- Field baru `TokenRotation string` (`drain|round_robin|least_used|random`, default `drain`)
- Validasi enum, hot-reload via `atomic.Pointer` seperti knob lain
- `.env.example` + `README` env table: jelasin `drain` aman, `round_robin`/`least_used`/`random` *study only — naikkan risiko ban*
- `internal/config/config_wave6_test.go` tambah `TestTokenRotationDefaultIsDrain`

### 2) `internal/pool/acquire.go` (`acquireOrder`)
- Branch **hanya** di dalam `acquireOrder`, jaga `hot-session exclusion` + `quotaLimited` bucket tetap utuh (quota habis → 429 via `bestRateLimit`, bukan `BanError`)
- `drain` (default): sort by `remaining asc` (paling kecil sisa dulu, issue #85) — kuras 1 token sampai `recentCount==limit` baru pindah
- `round_robin`: `start = (start+1)%n` tiap `Acquire` sukses, ignore hot-session, murni putar
- `least_used`: sort by `remaining desc` (paling banyak sisa dulu) — halus dari round-robin
- `random`: `rand.New(rand.NewSource)` + test hook `pool.testRand` biar deterministik di test, pilih acak di antara `remaining>0`
- `internal/pool/pool.go` tambah `TokenRotation` di struct, `internal/pool/pool_test.go` cover branching

### 3) `internal/pool/rotation_study_test.go` (hermetik, `env -u AUTH_TOKENS`)
- Mock 5 token × `openai/gpt-5.6-luna` limit 5 (reset `America/Los_Angeles` Pacific midnight), pakai `testutil.MockUpstream` yang track `rateLimitsByModel["openai/gpt-5.6-luna"].recentCount`
- `TestLuna_5x5_DrainSequential` — 25 `Acquire("luna")` sukses, assert distribusi `tok0×5 tok1×5 … tok4×5`, ke-26 `ErrNoAvailableToken` + `Retry-After`, `banned_until == nil`
- `TestLuna_5x5_RoundRobin` — 25 `Acquire`, assert `tok0 tok1 tok2 tok3 tok4 tok0 …` rata, hitung `429`/`ban` counter
- Reuse `frontend/e2e/fixtures/tokens.json` 5-token shape (quota array per token) dan `overview.json` 5-trimmed

## Frontend

### 4) `frontend/src/lib/pages/Overview.svelte` — Client Integration card
- Toggle `TOKEN_ROTATION` di bawah `Gateway Base URL` (sejajar `Supported Wire Protocols`): segmented `Drain | Round Robin | Least Used | Random` (Svelte 5 `$state`, `bind:value`)
- `fetchData()` baca `GET /admin/api/config` → `env_content` parse `TOKEN_ROTATION`, `onchange` → `POST /admin/config` (existing Config Studio pattern: validate + `save.json()` + `fetchData()`), hot-reload tanpa restart (config `atomic.Pointer`)
- `frontend/e2e/fixtures/config.json` + `overview.json` tambah `TOKEN_ROTATION`, `dashboard.spec.ts` tambah e2e `Overview toggles TokenRotation` (klik `Round Robin` → intercept `POST /admin/config` → expect `200` + badge berubah)
- `frontend/src/lib/i18n.js` tambah key `Token Rotation`

## Docs & Rollout

- `README` + `docs/dashboard.md` + `DESIGN.md` update: jelasin 4 mode + risiko ban
- `.drift-report.json` tidak sentuh (DRIFT `freebuff-models.ts`/`freebuff-session.ts` di SHA 6e4f6d6 tetap, `sync --check` tetap jalan)
- Serial rollout: ship `drain` vs `round_robin` dulu + test, baru `least_used`/`random` setelah data study — biar diff reviewable

## Verification

- `env -u AUTH_TOKENS -u ADMIN_TOKEN go vet ./... && go test ./...` (18 paket, `rotation_study_test` 2 skenario)
- `npm --prefix frontend run build` → `internal/dashboard/dist` (hash baru)
- `npx --prefix frontend playwright test` 8/8 hermetik (4173 `page.route`, 5-token fixtures lockstep, `reuseExistingServer:true` tapi `hub restart serve-static` sebelum run kalau hash baru)
- Live `3457` (go-proxy `-tags dashboard` + `ADMIN_TOKEN=dev123`) + `5173` (vite HMR) + `4173` (serve-static) via `hub ps`

## Out of Scope
- Tidak ubah `dataFor('setup')/APIHandler('setup')` (Setup sudah dihapus, `#setup` blank sengaja)
- Tidak ubah `freebuff-models.ts` pin — bukan registry work
