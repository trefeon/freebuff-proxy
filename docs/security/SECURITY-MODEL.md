# Security Model

Factual trust-boundary documentation for freebuff-proxy. No overclaiming:
each section states what is trusted, what is not, and where the gaps are
(findings reference the 2026-09-06 audit; open items live in
docs/maintenance/TECH-DEBT.md).

## Trust boundaries

```text
untrusted                          boundary                    trusted
─────────                          ────────                    ───────
client network ──► /v1/* handler (auth? validation? redact?) ──► pool/upstream
browser ──► /admin/* (cookie+CSRF+loopback gates) ──► dashboard/admin handlers
upstream (codebuff.com) ──► wire client (classify once, redact) ──► internal state
operator shell ──► CLI flags/env/.env (0600) ──► config loader (validate)
GitHub Actions ──► workflows (least-priv, SHA-pinned) ──► release artifacts
```

## What is trusted

- `AUTH_TOKENS`/client bridge tokens as presented (correctness of account
  identity is upstream's verdict via 401/ban classification, not ours).
- The operator's `.env`/config files on disk (0600); the loader validates
  shape but cannot detect a malicious-but-valid file.
- GitHub `GITHUB_TOKEN` inside workflows (scoped per job; see finding: CI).
- The upstream TLS endpoint after fingerprint negotiation (no cert pinning;
  system roots apply).

## What is untrusted

- Every client request body/header (`X-Request-Id` sanitized; tool schemas
  normalized with depth/node budgets; bodies size-capped).
- Upstream response bodies: classified once in `upstream/do()`, redacted
  before any downstream read (logs, dumps, ledger, client messages).
- `X-Forwarded-Proto` (trusted unconditionally for the Secure cookie flag;
  see finding 3 — set `ADMIN_FORCE_SECURE_COOKIES=true` behind a real proxy).
- The `Host` header for loopback decisions is parsed defensively
  (`isLoopbackHost`); malformed input fails toward reject.

## Authentication vs authorization vs CSRF vs rate limiting

Four independent controls (INV-SEC-002); no substitution:

| Control | Mechanism | File |
|---|---|---|
| Admin auth | `ADMIN_TOKEN`, constant-time compare, HMAC-SHA256 24h cookie (`fb_admin`, HttpOnly, SameSite=Strict, adaptive Secure), per-IP 5-fail/1min lockout + global budget + slot cap | `server/admin_auth.go` |
| Authorization | Per-route auth levels in `dashboard/admin_manifest.json`; sensitive routes (config, logs, tokens, reload) loopback-gated while `ADMIN_TOKEN` is unset or the factory default | `server/admin.go`, `admin_manifest.json` |
| CSRF | Double-submit cookie + `X-CSRF-Token` header on state-changing admin POSTs; Origin + Sec-Fetch-Site checks | `server/admin_auth.go` |
| Rate limiting | Login lockout table (capped), per-IP client buckets (`ratelimit`), bounded access-log gates | `server/admin_auth.go`, `middleware.go` |

## Secrets inventory

| Secret | Where it lives | How it is protected |
|---|---|---|
| Upstream tokens (`AUTH_TOKENS`) | process env, memory, pool | never logged/dumped (redacted `cb_`/`Bearer`), never persisted raw (SHA-256 keys only), dashboard shows set/unset + counts |
| Bridge client tokens | request headers, bridge cache | same redaction; evicted after 72h idle |
| `ADMIN_TOKEN` | env/.env | constant-time compares; never logged; loopback gates under factory default |
| `API_KEYS` | env/.env | constant-time compares |
| `fb_admin`/`fb_csrf` cookies | browser | HttpOnly (session), SameSite=Strict, adaptive Secure |
| Session state file | disk (0600) when `SESSION_PERSIST` | SHA-256 token keys + metadata only; no raw tokens |
| Debug dumps (`dump/`) | disk when enabled | Authorization + `x-codebuff-api-key` redacted; `dump/` gitignored |

## Known gaps (from 2026-09-06 audit)

1. **HIGH — Factory-default bootstrap allows remote takeover.** With the
   public default `ADMIN_TOKEN=123456`, a remote client can log in at
   `/admin/login` (no loopback gate) and POST `/admin/api/change-password`
   with `current_password=123456` (the deliberate BOOTSTRAP EXEMPTION, pinned
   by `admin_password_remote_test.go:19`), owning config/tokens/logs in two
   requests. Operators MUST rotate the token immediately (banner + startup
   warning say so). Remediation options (loopback-gated bootstrap or
   boot-generated one-time secret) are recorded as P1 in TECH-DEBT; changing
   this is a documented-behavior decision, not a silent fix.
2. **LOW — `.dockerignore` lags `.gitignore`.** `API_Keys*`,
   `*.session-state.json`, `*.tmp`, `.drift-report.json`, agent scratch dirs
   ship in the Docker build context (`COPY . .`). They never enter the final
   image (binary-only stage). Fix: mirror secret-class entries (done in this
   program; verify `.dockerignore`).
3. **INFO — Anonymous `/v1` in pooled mode.** When `AUTH_TOKENS` is set but
   `API_KEYS` is empty, the whole `/v1` surface and the pool are open to
   anyone who can reach the port. Mitigation: startup warning added (this
   program); operators should set `API_KEYS` or bind loopback.
4. **INFO — Redaction shape-coupled to `cb_`/`Bearer` regexes.** Adequate for
   every credential in play; extend `telemetry.RedactSecrets` if token formats
   change.

## Redaction guarantees

- Central: `telemetry.RedactSecrets` (`cb_`/Bearer) + `RedactHeaders`
  (Authorization, x-api-key, x-codebuff-api-key, Cookie/Set-Cookie, all
  `x-freebuff-*`); `notify.RedactURL` + `redactTransportErr`.
- Upstream bodies redacted once in `do()` before classification/rewrap; all
  downstream readers (logs, dumps, ledger, client messages) see redacted text
  (pinned by `TestDoUpstreamResponseLogsAndPreservesBody`,
  `TestDumpRedactsTokenHeaders`).
- Access logs carry method/path/status/ms/remote/req_id only. Dashboard
  effective-config shows secrets as set/unset + counts.
- No real secrets in the repo (audit-verified 2026-09-06: all credential-like
  literals are test/fixture placeholders; see baseline).

## Public-repository exposure

`.gitignore` covers every suspicious artifact class (`.env*`, `config*.json`,
session-state files, `*.exe`, `*.tmp`, `API_Keys*`, `main/`, `reference/`,
`devdocs/`, agent scratch); verified tracked-clean 2026-09-06. Local-only
content must never be committed (see CONTRIBUTING/PR template checklist).