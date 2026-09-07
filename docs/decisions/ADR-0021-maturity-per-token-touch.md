# ADR-0021 — Per-token maturity touch model + folded cards + Probe-all removal

Date: 2026-09-07. Status: Accepted. Branch: `refactor/db-persistent-dashboard`.

Live user audit of the Maturity UX on top of the persistence branch. Three
changes, all behavior the operator can see.

## 1. Per-token touch-model override with global fallback

`MATURITY_TOUCH_MODEL` stays the global default (keycatalog row kept), but
each token may override it from its Maturity card. `Pool.SetMaturity` gains
a `touch_model` argument (empty = fallback); the daily tick and the manual
touch resolve per-token-first via `maturityEffectiveModel`, and the snapshot
carries `touch_model,omitempty`.

Validation is shape-only at save (must be a `provider/model` id); served /
unmetered semantics stay in the fire path, which fails closed
(`skip:touch-model`, plus the existing `skip:touch-priced` meter guard).
The spend MODE select stays: `premium-short` admission still needs it (it
admits the shared premium-pool head regardless of the model pick), so the
Touch box holds a wide model select plus a compact mode select.

Candidates come from the existing `GET /admin/api/models` rows (same usable
filter as Quota Tracker: live agent binding, served, never the referral
grant) — no new endpoint. Server order is already cheapest-first, so the
page partitions unmetered-capable rows ahead of the premium pool without
re-sorting. Each option is labeled with its server-reported cost class
(`price_label` / `quota` / `pool`, premium named as `premium pool`). The
registry carries no price data, so prices are never invented.

## 2. Cards default folded

Maturity cards render folded: header (title, badges, pips, streak line) plus
an expand chevron. Controls and the restart-surviving history timeline mount
only when expanded — folded cards skip their history fetch. Expanded ids
persist in `pages_state` under the `maturity` scope via the shared pageState
helper, server-wins (snapshot restores, toggles merge back, out-of-range ids
clamp without re-persisting). Target Period and Touch boxes share the row
(`sm:col-span-6` each, equal height, `min-w-0` guards) so the number input
never overflows on mobile.

## 3. Probe-all removal rationale

The Tokens page header Probe-all button (and its `TokenTable`
`onProbeAll` / `probeAllPending` props, `handleProbeAll`, and the Tokens
e2e spec) is removed. Scheduled probing now keeps quotas fresh, so a manual
fan-out adds nothing but upstream GETs; per-token probe buttons stay for
targeted refreshes. `probeAllQuotas` in `stores/tokens.js` is kept while
Quota Tracker still consumes it — it goes only when its last caller does
(no dead code either way). DevTools' independent "Probe All Tokens" action
is out of scope and untouched.
