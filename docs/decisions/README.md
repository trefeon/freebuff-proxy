# Architecture Decision Records

ADRs explain **why** the system is shaped this way. Contracts (CONTRACT.md) and
boundaries (docs/architecture/) explain **how**. If a future change makes an ADR
obsolete, mark it `Superseded by ADR-NNNN` and write the new one; never silently
rewrite history.

## Format

```markdown
# ADR-NNNN — Title

Status: Accepted | Superseded | Deprecated

Context:
Decision:
Reasoning:
Alternatives considered:
Consequences:
Invariants:
Affected packages:
Related tests:
```

## Index

| ADR | Title | Status |
|---|---|---|
| [ADR-0001](ADR-0001-single-binary-dashboard.md) | Single binary with embedded dashboard | Accepted |
| [ADR-0002](ADR-0002-token-operating-modes.md) | Pooled, bridge, and hybrid token modes | Accepted |
| [ADR-0003](ADR-0003-token-lifecycle.md) | Token lifecycle: cooldowns, quota locks, quarantine | Accepted |
| [ADR-0004](ADR-0004-session-run-lifecycle.md) | Session and run lifecycle | Accepted |
| [ADR-0005](ADR-0005-protocol-translation.md) | Protocol translation across three API surfaces | Accepted |
| [ADR-0006](ADR-0006-upstream-reference-sync.md) | Upstream reference synchronization | Accepted |
| [ADR-0007](ADR-0007-config-precedence.md) | Configuration precedence and hot reload | Accepted |
| [ADR-0008](ADR-0008-admin-security.md) | Admin authentication and authorization | Accepted |
| [ADR-0009](ADR-0009-reasoning-effort-cache.md) | Reasoning effort mapping and multi-turn cache | Accepted |
| [ADR-0010](ADR-0010-release-strategy.md) | Release strategy and versioning | Accepted |
| [ADR-0011](ADR-0011-model-catalog.md) | Model catalog, registry, and fallback resolution | Accepted |
| [ADR-0012](ADR-0012-anti-ban-contract.md) | Anti-ban contract with upstream | Accepted |
| [ADR-0013](ADR-0013-request-timeout-ttfb.md) | REQUEST_TIMEOUT guards TTFB only | Accepted |
| [ADR-0014](ADR-0014-rpm-admission-atomic.md) | RPM admission counted atomically at lease grant | Accepted |
| [ADR-0015](ADR-0015-tool-calls-plural-dialect.md) | `<tool_calls>` plural dialect extracts like singular | Accepted |