# ADR-0014 — RPM admission counted atomically at lease grant

Status: Accepted

Context: `MAX_REQUESTS_PER_MINUTE` was checked in `Acquire` but recorded later in `Pool.Chat` (after the upstream call). A concurrent burst (agent spawn batches of 6-8) all passed the cap before any record landed — e.g. a 5/min cap admitted 8 (observed via operator report 2026-09-06).

Decision: admission is checked AND recorded atomically at lease-grant time — `tryAdmitRequest` under the roster lock (pooled), `bridgeTryAdmitRequest` under `bridgeMu` (bridge). The `Acquire`-loop pre-filter stays as a cheap skip; the grant-time admit is authoritative. On a full window the freshly-leased run is released and the token counted as rate-limited so the loop tries the next one. Admission is always recorded (even with cap 0 = unlimited) so snapshot counters stay meaningful; `Pool.Chat` keeps only the success-side records (daily message cap, Pacific-day request cap).

Reasoning: check-then-act across a network call is never a rate limit — it is a race. The reservation must live in the same critical section as the check, at the moment the right to send is granted (the lease), not at the moment the bytes flow (the chat).

Alternatives considered: a semaphore per token (duplicates the ledger, loses the rolling-window shape); recording at `Acquire` entry before admission (counts attempts that never lease — over-counts on failover retries).

Consequences: a lease with no follow-up chat still consumed its admission slot (documented, tested). Failover on a full window costs one released run (cheap, local). The pre-filter + grant double-check is intentional defense in depth, not redundancy to "simplify" away.

Invariants: RPM = admitted requests, rolling 60s, per entry, atomic with grant; RPD = successful chats, Pacific day; 0 = unlimited (counting continues).

Affected packages: `internal/pool` (roster.go, quota.go, acquire.go, bridge.go, pool.go).

Related tests: `TestAcquireRPMAdmitBurstIsAtomic`, `TestBridgeRPMAdmitBurstIsAtomic`, `TestAcquireCountsAdmissionAtGrant`, `TestAcquirePerMinuteRequestCap`, `TestBridgeRequestCaps`.
