import { fetchAPI } from "./api/client.js";
import { adminApi } from "./api/paths.js";

// Static fallback for when the admin API is unreachable (e.g. `npm run dev`
// before the gateway is up). Kept in sync with the catalog by the CI embed
// gate — the live /admin/api/models payload supersedes it whenever present.
export const fallbackModelOptions = [
  {
    id: "openai/gpt-5.6-luna",
    label: "openai/gpt-5.6-luna (5 premium quota)",
    tag: "premium",
  },
  {
    id: "meta/muse-spark-1.3-contributor",
    label: "meta/muse-spark-1.3-contributor (5 premium quota)",
    tag: "premium",
  },
  {
    id: "upstage/solar-pro4",
    label: "upstage/solar-pro4 (unlimited session)",
    tag: "unmetered",
  },
  {
    id: "mimo/mimo-v2.5",
    label: "mimo/mimo-v2.5 (unlimited session)",
    tag: "unmetered",
  },
  {
    id: "z-ai/glm-5.3-flash",
    label: "z-ai/glm-5.3-flash (unlimited session)",
    tag: "unmetered",
  },
  {
    id: "deepseek/deepseek-v4-flash",
    label: "deepseek/deepseek-v4-flash (unlimited session)",
    tag: "unmetered",
  },
  {
    id: "z-ai/glm-5.2",
    label: "z-ai/glm-5.2 (referral promo)",
    tag: "referral",
  },
];

// tag derives from the server-side quota label so the chips track the
// catalog automatically (quotaFor: 5 premium quota / referral / unlimited session).
function tagFor(quota) {
  if (!quota) return "";
  if (quota.includes("premium")) return "premium";
  if (quota.includes("referral")) return "referral";
  if (quota.includes("unmetered") || quota.includes("unlimited"))
    return "unmetered";
  return "";
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
      label: m.quota ? `${m.id} (${m.quota})` : m.id,
      tag: tagFor(m.quota),
    }));
    return cached;
  } catch {
    return fallbackModelOptions;
  }
}
