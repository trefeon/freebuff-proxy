# Package Contract: `backend/internal/notify`

Task-local contract for agents modifying this package. Load before editing any file here. The rest of the repo's mental model is NOT required.

## Purpose

Leaf package: best-effort outbound webhook notifications (pool events, ban/quota signals) via a throttled HTTP sender. Fire-and-forget by design.

## Public API (stable surface)

- `Sender`, `New(url, client)`, `Event`; `RedactURL(raw)`.

## Allowed dependencies

None. Zero internal imports — enforced by `archtest`.

## Forbidden dependencies

Everything internal. Notifications must not depend on `pool` (the subject), `config` (URL/client are caller-supplied), or anything that could deadlock a notifier called from inside pool locks.

## Critical invariants

- Best-effort always: send failure logs a warning and returns — never an error to the caller (`TestSendFailureLogsWarn`, `TestSendDisabledNoOps`). A dead webhook must not fail requests.
- Throttled (`TestSendThrottle`) — event storms (ban waves, mass cooldowns) must not become a self-DDoS.
- Redirects rejected (`TestSendRejectsRedirect`); URLs redacted on failure paths (`TestRedactURL`, `TestSendFailureRedactsURL`) — webhook URLs carry secrets.

## Tests that protect it

`TestSendPostsPayload`, `TestSendThrottle`, `TestSendDisabledNoOps`, `TestSendFailureLogsWarn`, `TestRedactURL`, `TestSendRejectsRedirect`, `TestSendFailureRedactsURL`.

## Safe modification patterns

- New event type: extend `Event`, keep payload small and redacted; add a `SendPostsPayload`-style case.
- Never make send synchronous-blocking for callers; never follow redirects; never log the raw URL.
