import { fetchAPI } from "./api/client.js";
import { adminApi } from "./api/paths.js";

// Static fallback for when the admin API is unreachable (e.g. `npm run dev`
// before the gateway is up). Bare ids only, no live prices here; the live
// /admin/api/models payload (with per-model Freebucks/hr) supersedes them
// whenever present. Offline-dev fallback only: every id below must exist in
// the served registry (e2e/fixtures/models.json mirrors it).
export const fallbackModelOptions = [
  {
    id: "openai/gpt-5.6-luna",
    label: "openai/gpt-5.6-luna",
    tag: "premium",
  },
  {
    id: "meta/muse-spark-1.2-contributor",
    label: "meta/muse-spark-1.2-contributor",
    tag: "premium",
  },
  {
    id: "upstage/solar-pro4",
    label: "upstage/solar-pro4",
    tag: "free",
  },
  {
    id: "mimo/mimo-v2.5",
    label: "mimo/mimo-v2.5",
    tag: "free",
  },
  {
    id: "z-ai/glm-5.3-flash",
    label: "z-ai/glm-5.3-flash",
    tag: "free",
  },
  {
    id: "z-ai/glm-5.2",
    label: "z-ai/glm-5.2",
    tag: "referral",
  },
  {
    id: "deepseek/deepseek-v4-flash",
    label: "deepseek/deepseek-v4-flash",
    tag: "free",
  },
];

// cheapestFreeOption returns the default spawn/playground pick for an
// option list: the first tag=free row (live prices already drive tags),
// else the first non-premium row, else the first row. Never a pinned id —
// the default tracks the cheapest free row wherever the list comes from.
export function cheapestFreeOption(rows) {
  const list = Array.isArray(rows) ? rows : [];
  const free = list.find((m) => m?.tag === "free");
  if (free?.id) return free.id;
  const unmetered = list.find((m) => m?.tag !== "premium");
  if (unmetered?.id) return unmetered.id;
  return list[0]?.id ?? "";
}

// tag derives from the server-side Freebucks price label so chips track the
// meter, not legacy session pools: referral grant / 0 Freebucks/hr / priced
// Freebucks/hr / "" when the server sent no price.
function tagFor(m) {
  const label = m.price_label ?? m.priceLabel ?? "";
  if (/referral/i.test(label)) return "referral";
  if (!label) return "";
  if (/^0\b/.test(label)) return "free";
  return "premium";
}

// fetchModelOptions returns {id, label, tag} rows from /admin/api/models
// (registry → modelcat → upstream-parity-pinned), falling back to the
// static list on any error. Results are memoized per page load: multiple
// consumers (TokenCard renders one per pool token) share one fetch, and
// callers re-assign $state rows from the promise.
let cached = null;
export async function fetchModelOptions() {
  if (cached) return cached;
  try {
    const data = await fetchAPI(adminApi.models);
    const rows = Array.isArray(data?.models) ? data.models : [];
    if (rows.length === 0) return fallbackModelOptions;
    cached = rows.map((m) => ({
      id: m.id,
      label:
        m.price_label && /Freebucks\/hr/.test(m.price_label)
          ? `${m.id} (${m.price_label})`
          : m.id,
      tag: tagFor(m),
    }));
    return cached;
  } catch {
    return fallbackModelOptions;
  }
}
