# ADR-0012 — Anti-ban contract with upstream

Status: Accepted

Context:
Using third-party tokens through this proxy conflicts with upstream terms;
abuse detection can suspend or ban accounts. The proxy cannot eliminate that
risk, but it can avoid the common, detectable automation shapes. These choices
are deliberate egress behavior, not incidental headers, and they must not be
"cleaned up" as redundant.

Decision (the contract):
- **Session POST** carries `x-freebuff-model` + `x-freebuff-instance-id`;
  **chat POST carries NO model header**. These are the upstream-expected shapes.
- **Pinned user agents**: `ai-sdk/openai-compatible/1.0.0/codebuff` on chat,
  `Freebuff-CLI/1.0.0` on ads, `Bun/1.3.14` on session/auth. `CLI_VERSION`
  is informational only.
- **TLS stealth**: `TLS_FINGERPRINT` selects uTLS profiles
  (chrome/firefox/safari/edge); SAFE_MODE default is CLI-faithful baseline +
  proxy-header sanitization (25 headers) + 0-200ms jitter + 30m idle rotation.
- **HTTP/2 upstream** (`HTTP2_UPSTREAM`) so ALPN matches real browsers.
- **Honest FINISH** on every run termination (ADR-0004).
- **Zero spam**: local quota locks answer without upstream traffic; admits are
  single-flighted and capped (global + per-model); `SESSION_PROBE_CACHE_TTL`
  reuses poll state.
- **Drain, not rotate**: hot-session-first pooling; one key runs until it is
  rate-limited instead of cycling healthy keys (looks like account farming).
- **Acting user id**: only the token's own id is sent
  (`x-freebuff-acting-user-id`); impersonation values are rejected at validation.

Reasoning:
Upstream detection is documented in the open-source FreeBuff client (per-IP
scoring, trust levels, sticky caps, farm sweeps). Each contract line maps to a
known detector: TLS JA3, proxy headers, UA mismatch, robotic cadence, session
churn, token rotation patterns. This is why the values are pinned rather than
"modernized": changing an egress string changes what abuse detection sees.

Alternatives considered:
- No stealth (plain Go egress): the historical baseline; judged too noisy for
  datacenter egress, keeps `SAFE_MODE` presets as the default.
- Self-update of fingerprints to newest Chrome always: rejected; matches the
  CLI behavior, not the browser latest.

Consequences:
- Upstream client changes require header/UA/fingerprint review, not just logic review.
- `SAFE_MODE=false` is a support escape hatch, not a recommendation; document
  the risk delta when suggesting it.
- Benchmarks and tests must run against the mock upstream or off-peak staging,
  never as admission storms on the live box during active sessions (a past
  50-request bench matrix superseded a live 245-TURN session).

Invariants:
- Chat carries no model header; `CLI_VERSION` never leaks into the wire.
- Quota-locked tokens generate zero upstream traffic.
- FINISH stays honest on rotation, drain, idle timeout, shutdown.

Affected packages:
`internal/upstream`, `internal/stealth`, `internal/pool`, `internal/session`, `internal/runs`, `internal/config` (knobs).

Related tests:
`stealth_test.go`, `client_chat_test.go`, `client_retry_test.go`,
wire tests, quarantine/hardban/pool quota tests.