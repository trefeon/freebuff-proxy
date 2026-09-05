# ADR-0008 — Admin authentication and authorization

Status: Accepted

Context:
The binary ships an embedded admin dashboard that can edit `.env`, manage
tokens, view logs, and reload config. The dashboard is reachable wherever the
proxy listens; in the worst common case (cloud VPS, no TLS) the surface is
plain HTTP on a public port. Credential compromise of the admin surface is
worse than an API-key leak: it can rotate tokens and rewrite config.

Decision:
Four independent controls, each load-bearing (INV-SEC-002):
1. **Authentication**: login with `ADMIN_TOKEN` (constant-time compare),
   per-IP rate limit (5 fails → 1 min lockout), bounded concurrent login slots
   (backpressure, not a queue). Session cookie `fb_admin`: HttpOnly,
   SameSite=Strict, 24h, `Secure` dynamically when TLS/`X-Forwarded-Proto: https`
   (`ADMIN_FORCE_SECURE_COOKIES=true` forces it).
2. **Authorization**: route-level auth levels in `dashboard.AdminRoute`
   (open / login / sensitive). Sensitive routes (config editor, logs, token
   management, reload) additionally require a **loopback client** while
   `ADMIN_TOKEN` is the factory default `123456`; a startup warning and a
   dashboard banner prompt rotation. `/admin/reload` uses bearer
   `Authorization: Bearer <ADMIN_TOKEN>`.
3. **CSRF**: double-submit cookie (32 random bytes hex) echoed as
   `X-CSRF-Token` on state-changing POSTs; the CSRF cookie is deliberately not
   HttpOnly because the SPA must read it.
4. **Rate limiting**: login failure lockout per IP, capped failure table
   (1024), bounded login concurrency.

Reasoning:
Open-mode secrets disclosure was a real historical bug: with no `ADMIN_TOKEN`
the config/logs/token endpoints were reachable remotely. The loopback gate
closes that while keeping local-first ergonomics. Each control was added
because the others demonstrably did not cover its hole (CSRF exists because
cookie auth alone is CSRF-able; the cookie Secure flag exists because plain
HTTP would otherwise silently drop it or expose it).

Alternatives considered:
- Full TLS in the binary: rejected; out of scope, HTTPS is operator's
  responsibility (documented in README + docs/https.md).
- Time-based tokens / OAuth for admin: rejected; overkill for a solo operator.

Consequences:
- `/admin/api/change-password` works from anywhere (requires current password).
- Assets are served without dashboard auth so the login page renders; APIs are gated separately.
- Logout requires no valid cookie (must always work).
- Any new sensitive admin route must pick the correct auth level in
  `AdminRoutes`; wrong level = security regression.

Invariants:
- Sensitive routes stay loopback-gated while the factory default token is active.
- CSRF gate applies to every state-changing admin POST.
- Login failures never reveal whether the token was right (constant-time compare).

Affected packages:
`internal/server` (admin_auth.go, admin.go, admin_handlers.go, middleware.go), `internal/dashboard` (AdminRoutes).

Related tests:
`admin_auth` internal tests, `admin_csrf_test.go`, `admin_openmode_test.go`,
`admin_require_login_test.go`, `admin_password_test.go`,
`admin_password_remote_test.go`, `admin_routes_test.go`.

## Security note (2026-09-06 audit, HIGH)

Window: from first boot on the factory default until the operator rotates it,
anyone who can reach the port owns the instance — log in with the public
`123456`, take the session cookie, satisfy CSRF with the issued token, change
the password with the known current password. Every prerequisite is public or
self-issued; the first scanner to arrive wins. Operators must rotate before
exposing the port. Remediation options are P1 in
docs/maintenance/TECH-DEBT.md (TD-P1-1). This behavior is deliberate and
pinned by `admin_password_remote_test.go:19`; changing it is an owner decision.