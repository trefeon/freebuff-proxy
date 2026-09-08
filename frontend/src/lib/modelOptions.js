import { fetchAPI } from "./api/client.js";
import { adminApi } from "./api/paths.js";

// Static fallback for when the admin API is unreachable (e.g. `npm run dev`
// before the gateway is up). Freebucks terms only — no live prices here, so
// rows read "(metered)" until the live /admin/api/models payload (with
// per-model Freebucks/hr) supersedes them whenever present.
export const fallbackModelOptions = [
  {
    id: "openai/gpt-5.6-luna",
    label: "openai/gpt-5.6-luna (metered)",
    tag: "metered",
  },
  {
    id: "meta/muse-spark-1.2-contributor",
    label: "meta/muse-spark-1.2-contributor (metered)",
    tag: "metered",
  },
  {
    id: "upstage/solar-pro4",
    label: "upstage/solar-pro4 (metered)",
    tag: "metered",
  },
  {
    id: "mimo/mimo-v2.5",
    label: "mimo/mimo-v2.5 (metered)",
    tag: "metered",
  },
  {
    id: "z-ai/glm-5.3-flash",
    label: "z-ai/glm-5.3-flash (metered)",
    tag: "metered",
  },
  {
    id: "deepseek/deepseek-v4-flash",
    label: "deepseek/deepseek-v4-flash (metered)",
    tag: "metered",
  },
  {
    id: "z-ai/glm-5.2",
    label: "z-ai/glm-5.2 (referral grant)",
    tag: "referral",
  },
];

// tag derives from the server-side Freebucks price label so chips track the
// meter, not legacy session pools: referral grant / free (0 Freebucks/hr) /
// metered (priced) / "" when the server sent no price.
function tagFor(m) {
  const label = m.price_label ?? m.priceLabel ?? "";
  if (/referral/i.test(label)) return "referral";
  if (!label) return "";
  if (/^0\b/.test(label)) return "free";
  return "metered";
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
      label: m.price_label ? `${m.id} (${m.price_label})` : m.id,
      tag: tagFor(m),
    }));
    return cached;
  } catch {
    return fallbackModelOptions;
  }
}
