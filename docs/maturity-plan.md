# Streak Display + Maturity Automation — Full Plan (no code yet)

Status: planning only. PR1 (display) and PR2 (automation) land separately.
Target: v1.8.9+ follow-ups after PoolReview triage.

## 0. Binding principles

- **Unknown ≠ 0.** A streak that fails to fetch renders as "unavailable",
  never as 0. No number or dot is ever drawn from stale data (same rule
  that fixed the quota-stale 429s).
- **Display first, automation later.** Display is read-only (zero risk).
  Automation carries ban-risk and ships only after the validation
  protocol (§3) passes on a sacrificial token.
- **Timezones follow the account**, never the VPS: the streak response
  carries `timeZone` + `lastUsageDate`; the scheduler uses those.

## 1. PR1 — Streak display + backend validation (safe, read-only)

### Backend (`upstream` + `pool` + `dashboard`)

- New client call `GET /api/v1/freebuff/streak` →
  `{streak, todayUsed, lastUsageDate, timeZone}`. Per pool token,
  cached 1h + refreshed on demand (the number moves daily; no
  per-minute polling).
- Shape validation: `streak` int ≥ 0, `todayUsed` bool, lenient date
  parse. Auth/network failure → `streak_unavailable`, not zeros.
- Dashboard payload per token: `streak`, `today_used`, `last_usage`,
  `streak_updated_at`, `streak_unavailable`.

### Frontend (Quota Tracker, per account)

- Streak row in CLI style: `18 day streak ●●●●●●●+` (7-dot grid,
  `+` past 7).
- Perk note (ported logic): below 7 days →
  `🎁 N more days to unlock +1 bonus session every day`; at 7+ →
  `🎁 Streak perk: …` (GLM variant only when `accessTier == 'full'`).
- Zero/unavailable streak → row hidden (a lapsed user needs a first
  day, not a countdown).

### Tests

- Streak endpoint in the `testutil` mock; client parse + shape-guard
  tests; dashboard API tests (including the unavailable path); e2e
  fixtures (dots + perk note + hidden state).

## 2. PR2 — Random smart ping per auth token

### Scheduler (per token, per day)

```
slot[token] = uniform_random(00:00–24:00, account timezone)
              // drawn at boot + every day rollover
every maintain tick (15 min):
  for automation-enabled tokens:
    streak = cached_streak(token)          // refresh if >1h old
    if streak.todayUsed:        skip       // real traffic (or an
                                           // earlier firing) touched it
    if now < slot[token]:       skip
    if last_attempt < 6h ago:   skip       // restart-safe throttle
    if cooling/banned/quarantine/locked: skip
    touch(token)
    record attempt_at
```

- **Idempotent without persisted schedule:** a restart re-rolls the
  slot for the remaining day; double-firing is prevented by
  `todayUsed` + the 6h throttle. Worst case is one extra cheap touch.
- **Independent per token** → slots spread naturally across the day
  (no midnight stampede).
- **Active days cost zero extra traffic:** real usage sets
  `todayUsed`, so those days are indistinguishable from human use.

### Touch ladder (cheapest first, escalate only on evidence)

1. **Dry-run mode** (global flag, ON for the first 24h): session
   probe only — validates scheduler mechanics (slots, skips, logs)
   with zero side effects. A probe almost certainly does NOT flip
   `todayUsed` (zero-cost by design).
2. **Default rung: admission on an unmetered model**
   (deepseek-flash / glm-flash) through the normal Acquire path —
   wire-identical to a user opening the CLI. Reservation 1.0 against
   an infinite limit = free, plus a live session real traffic can
   reuse. Never burns premium quota for farming.
3. Escalate (e.g. minimal chat) only if §3 proves rung 2
   insufficient. Never by default.

### Effectiveness loop (anti-blind-running)

The next day the scheduler compares `streak`:
- Advanced / `todayUsed` true → `streak_advanced: yes`.
- No advance 3 days running → per-token warning badge
  ("touch is not advancing the streak — escalate or disable") and
  firing stops automatically. Never spam upstream for zero result.

### State, config, UI

- Per-token toggle on the Tokens row (persisted with the existing
  per-token settings mechanism) + global env kill-switch.
  Default: OFF everywhere, explicit opt-in.
- Per-token columns: automation on/off · today's slot (account-local
  time) · last touch (time, action, result) · streak advanced
  yes/no/stale · maturity badge (`Mature` ≥ 7 + healthy standing and
  quota, `Warming` 0–7, `Cold` 0).
- Every firing writes one dashboard log line (action + result).

### Tests

- Fake-clock scheduler: slot spread, skip on todayUsed/cooling/
  banned, 6h throttle, restart idempotency (re-roll, no double-fire).
- Dry-run: zero admission POSTs (mock counter).
- E2E: toggle persist, status columns, no-advance warning.
- Hermetic: mock streak endpoint (`todayUsed` flip) + unmetered
  admission.

## 3. Validation protocol (required before PR2 goes live)

Ladder order, stop at the first rung that flips `todayUsed`:
1. Token-level session probe (expected: no),
2. Compact poll of a live session,
3. Fresh admission on an unmetered model,
4. Minimal chat (certain, but billed — last resort).

One sacrificial token; `todayUsed` flips same-day (no 7-day wait
needed to validate the mechanism). 7-day soak criteria for fleet:
streak increments daily, zero ban/cooldown signals, standing does
not drop.

## 4. Risk (do not skip)

A daily mechanical touch from a datacenter IP to farm streaks is a
bot pattern in the eyes of upstream abuse detection — this proxy's
entire philosophy is anti-ban / CLI fidelity, and a flag costs pool
accounts, not just streaks. Mitigations baked into the design:
default OFF with honest UI copy, jittered per-token daytime slots,
preserve-before-build (keep existing streaks alive first; building
from 0 for 7 straight days on many tokens is the most bot-like
shape and needs explicit approval), unmetered models only, audit
log, global kill-switch. Fleet rollout only after one token passes
the 7-day soak. Start with preserve-only.

## 5. Execution order

1. PR1 (display) → merge → deploy → numbers verified against the
   real TUI.
2. Usage-ladder experiment (§3) on one token.
3. PR2 (automation, preserve-only) → 7-day soak on one token →
   fleet decision.
