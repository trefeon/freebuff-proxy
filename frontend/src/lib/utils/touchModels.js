/**
 * Shared touch-model option helpers (Maturity per-token select + Settings →
 * Advanced → MATURITY_TOUCH_MODEL global select). Single source so both
 * dropdowns stay identical; priced labels come straight from
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
