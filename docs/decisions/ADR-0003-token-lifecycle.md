# ADR-0003 — Token lifecycle: cooldowns, quota locks, quarantine

Status: Accepted

Context:
Upstream answers with classified errors (401 auth, 429 quota, 428 waiting room,
403 banned/country_blocked, `ip_capped`, `model_unavailable`). Hammering a
rejected token wastes quota signals and feeds abuse detection. The proxy must
remember each token's state locally and route around it, cheaply.

Decision:
- Classification lives in `internal/upstream/classify.go`; error classes map to
  token state transitions in `internal/pool` (`cooldown.go`, `quota.go`,
  `unfit.go`, `hardban` handling).
- 401 auth rejection → 30-minute cooldown, try next token.
- 429 quota → parse the reset timestamp, lock the token locally until reset;
  local answers are `<1ms` with `429` + `Retry-After`, no upstream traffic.
- Terminal classes (`banned`, `country_blocked`) → permanent quarantine; the
  token leaves rotation. `ip_capped` and `waiting_room` are transient, not token faults.
- `turn_spend_limited` (429, code `turn_spend_limited`) is TERMINAL for the
  turn: no Retry-After, no token cooldown, no cross-token failover spin. Only a
  fresh turn can succeed; retrying a poisoned turn re-trips the breaker.
- Failover error-bucket precedence: ban > country-blocked > model-IP-limited >
  rate-limit > waiting-room > daily cap; combined error only when no bucket matches.
- Local ledgers: RPM (admitted requests, rolling 60s), RPD (successful chats,
  Pacific day, rolls at midnight), `MAX_MESSAGES_PER_DAY` (rolling 24h). Spend
  ledger records events only; the $ ceiling is server-enforced (future work).

Reasoning:
Tokens are scarce and upstream-side rejection is expensive in trust terms.
Local locks convert noisy upstream 429 storms into silent local rejects, which
is the "zero-spam guarantee" users rely on. Classifying errors once, in
`upstream`, keeps the rest of the system free of string-matching.

Alternatives considered:
- Blind retry on every 429: rejected; proven to re-trip spend breakers for minutes.
- Global lock when any token fails: rejected; loses the failover property.

Consequences:
- Cooldown/quarantine state is in-memory; restarts clear it (sessions may persist, states do not).
- Token state transitions are the highest-churn logic in the repo; every new error class needs a classify.go mapping plus pool transition tests.

Invariants:
- Ban/country-blocked must never be treated as transient cooldowns.
- `turn_spend_limited` must never carry Retry-After or trigger token cooldown.
- Ledger counters live in the pool ledger, never in the static cache; bridge ledgers update under `bridgeMu`.

Affected packages:
`internal/upstream` (classify.go, errors.go, wirecodes.go), `internal/pool` (cooldown.go, quota.go, unfit.go, spend.go), `internal/server` (errors.go, engine_attempt.go).

Related tests:
`pool_cooldown_test.go`, `pool_quota_test.go`, `pool_quota_window_test.go`, `pool_request_test.go`, `quarantine_test.go`, `hardban_test.go`, `spend_test.go`, `TestClassifyTurnSpendLimited`, `TestChatAttemptTurnSpendTerminal`.