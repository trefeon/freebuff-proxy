import { fetchAPI } from "../api/client.js";
import { adminApi } from "../api/paths.js";

// Central history fetchers (ADR-0016): one place builds the query strings
// for the three history rows, and every shape degrades to {enabled:false}
// when the gateway runs live-only. Pages never hardcode history paths.

// Maturity events for one token, newest first (cap for the ledger strip).
export async function fetchMaturityHistory(tokenIdx, limit = 8) {
  const res = await fetchAPI(
    `${adminApi.maturityHistory}?token=${tokenIdx}&limit=${limit}`,
  );
  if (!res?.enabled) return { enabled: false, events: [] };
  return { enabled: true, events: res.events ?? [] };
}

// Quota samples for one token+model, oldest first (sparkline order).
export async function fetchQuotaHistory(tokenIdx, model, limit = 60) {
  const res = await fetchAPI(
    `${adminApi.quotaHistory}?token=${tokenIdx}&model=${encodeURIComponent(model)}&limit=${limit}`,
  );
  if (!res?.enabled) return { enabled: false, snapshots: [] };
  return { enabled: true, snapshots: res.snapshots ?? [] };
}
