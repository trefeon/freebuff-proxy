# Package Contract: `backend/internal/reasoningcache`

Task-local contract for agents modifying this package. Load before editing any file here.

## Purpose

Multi-turn reasoning cache: stores assistant reasoning keyed canonically so a
tool loop restores prior reasoning instead of re-deriving it. Owns: TTL +
prune, canonical key derivation (`canonical_key`), identity/tool-id binding
(`toolid_binding`). Threaded into convert via a `ReasoningLookupFn`, never a
global hook.

## Allowed dependencies

None (stdlib only; archtest leaf). Accessed from convert only through the
pre-approved edge (issue #279).

## Forbidden dependencies

Everything internal. The cache must not know about requests, tokens, or the wire.

## Critical invariants

- Keys are canonical and stable: never depend on client-controlled message
  ids (untrusted). Identity binding prevents cross-conversation restore.
- TTL + prune bounds the cache; a broken cache degrades answer quality but
  must never leak data between conversations.
- The lookup is per-server (threaded through `convertOptions`), not a
  process-global; concurrent servers must not interfere.

## Tests that protect it

`canonical_key_test.go`, `toolid_binding_test.go`, `cache_test.go`
(TTL/prune/concurrency), `reasoning_restore_test.go` (convert side).

## Safe modification patterns

- New binding dimension: extend the canonical key + binding tests together;
  a key change is a cache-miss event, never a collision event.