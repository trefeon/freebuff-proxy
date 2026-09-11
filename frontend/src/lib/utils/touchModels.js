/**
 * Shared touch-model option helpers (Streak Maintenance global select +
 * Settings → Advanced → MATURITY_TOUCH_MODEL global select). Single source
 * so both dropdowns stay identical; priced labels come straight from
 * /admin/api/models rows (price_label/quota/pool) — never invented.
 */

/**
 * Served touch candidates, cheapest-Freebucks-cost first: rows the gateway
 * can admit (live agent binding, served, never the referral grant). Server
 * order already sorts cheapest-first, so priced rows stay ahead without
 * re-sorting; the premium pool trails.
 */
export function touchCandidates(modelRows) {
  const rows = (modelRows ?? []).filter(
    (m) => m?.agent && m?.served !== false && m?.pool !== "referral",
  );
  return [
    ...rows.filter((m) => m.pool !== "premium"),
    ...rows.filter((m) => m.pool === "premium"),
  ];
}

/** Server-reported cost class for one candidate row (never invented). */
export function touchCostClass(m) {
  if (!m) return "";
  return m.price_label || m.quota || "";
}

export function touchLabel(m) {
  const cls = touchCostClass(m);
  return cls ? `${m.id} (${cls})` : m.id;
}

/**
 * Fail-open options: live candidates when the catalog loaded, else the
 * current value alone so the select never empties.
 */
export function touchOptions(modelRows, currentId = "") {
  const cands = touchCandidates(modelRows);
  if (cands.length > 0) return cands;
  if (currentId) {
    return [{ id: currentId, price_label: "", quota: "", pool: "unlimited" }];
  }
  return [];
}

/**
 * Automatic touch-model pick (mirrors modelcat.AutoUnmeteredTouchModel):
 * the first served, non-premium, non-referral row whose live price is 0.
 * A quota-exempt token re-admits priced rows (server-authorized), so exempt
 * tokens accept the first served non-premium row. Honeypot, god-only, eval,
 * paused, and priced rows can never win — never a naive price sort.
 * Returns "" when no unmetered served row exists (caller falls back to the
 * configured global, or fails closed when that is itself auto).
 */
export function autoTouchPick(modelRows, exempt = false) {
  for (const m of modelRows ?? []) {
    if (!m?.agent || m?.served === false || m?.pool === "referral") continue;
    if (m?.pool === "premium") continue;
    const price = typeof m?.price === "number" ? m.price : 0;
    if (!exempt && price > 0) continue;
    return m.id;
  }
  // Fallback order when prices are absent (old servers / fixtures without a
  // price key): first served non-premium row in server order.
  for (const m of modelRows ?? []) {
    if (!m?.agent || m?.served === false || m?.pool === "referral") continue;
    if (m?.pool === "premium") continue;
    if (m?.price === undefined) return m.id;
  }
  return "";
}

/** Reason string for the Auto pick (inspectable default, never invented). */
export function autoTouchReason(modelRows, exempt = false) {
  return autoTouchPick(modelRows, exempt)
    ? "auto:unmetered"
    : "fallback:no-unmetered-served";
}

/** Cost class for one model id from the live rows ("" when unknown). */
export function touchCostFor(modelId, modelRows) {
  const row = (modelRows ?? []).find((m) => m?.id === modelId);
  return touchCostClass(row);
}

/** Numeric live price for one model id (NaN when the rows carry none). */
export function touchPriceFor(modelId, modelRows) {
  const row = (modelRows ?? []).find((m) => m?.id === modelId);
  return typeof row?.price === "number" ? row.price : NaN;
}
