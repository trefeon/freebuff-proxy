import { fetchAPI, csrfHeader } from "../api/client.js";
import { adminApi, adminActions } from "../api/paths.js";
import { useEventStream } from "../utils/events.js";
import { createQueryStore } from "./query.js";

// Shared tokens snapshot (issue #292). Tokens.svelte, QuotaTracker.svelte and
// DevTools.svelte previously each wired their own /admin/api/tokens poll and
// /admin/api/events SSE subscription with the same try/catch scaffold. This
// module-level singleton owns ONE poll + ONE SSE subscription and every page
// renders from the same value, so a mutation on one page is reflected
// immediately on the others.
//
// The poll loop itself lives in ./query.js (createQueryStore): refcounted,
// visibility-aware, overlap-guarded. This module only contributes the
// tokens-specific pieces — the static/live merge below and the SSE push —
// while keeping the historical exports stable for all consumers.

const INTERVAL_MS = 10000;

// Issue #322: account-stable fields (email/account_id, daily_limit,
// standing_*, referral_*) ride a once-per-mount full fetch; the 10s hot poll
// hits ?view=live and merges over the cached static snapshot. A full refresh
// every ~5min (plus every mutation and every full-shape SSE push) picks up
// mid-session changes (trust updates, referral consumption, pool edits).
const LIVE_QS = "?view=live";
const STATIC_TOP_KEYS = [
  "mode",
  "in_bridge",
  "show_bridge",
  "unmetered_models",
  "token_rotation",
  "rate_limit_failover",
  "maturity_enabled",
  "maturity_dry_run",
  "maturity_window_start",
  "maturity_window_end",
];
const STATIC_TOKEN_KEYS = [
  "email",
  "account_id",
  "access_tier",
  "daily_limit",
  "has_standing",
  "standing_level",
  "standing_label",
  "standing_score",
  "standing_next_level",
  "standing_next_level_at",
  "standing_capped_by",
  "standing_capped_reason",
  "standing_blurb",
  "standing_next_steps",
  "has_referral",
  "referral_code",
  "referral_qualified_count",
  "referral_github_linked",
  "referral_reset_at",
  "allowed_models",
  "streak",
  "today_used",
  "last_usage",
  "streak_updated_at",
];
let staticTop = null;
let staticTokensByIndex = {};

function pick(obj, keys) {
  const out = {};
  for (const k of keys) if (k in obj) out[k] = obj[k];
  return out;
}

function rememberStatic(full) {
  staticTop = pick(full, STATIC_TOP_KEYS);
  staticTokensByIndex = {};
  for (const t of full.tokens ?? []) {
    staticTokensByIndex[t.index ?? -1] = pick(t, STATIC_TOKEN_KEYS);
  }
}

function mergeLive(live) {
  // Old servers and hermetic mocks answer the live URL with the full shape:
  // refresh the static cache instead of rendering stale snapshots.
  if ("mode" in live && live.tokens?.some?.((t) => "email" in t)) {
    rememberStatic(live);
  }
  return {
    ...staticTop,
    ...live,
    tokens: (live.tokens ?? []).map((lt) => ({
      ...(staticTokensByIndex[lt.index ?? -1] ?? {}),
      ...lt,
    })),
  };
}

async function fetchFull() {
  const data = await fetchAPI(adminApi.tokens);
  rememberStatic(data);
  return data;
}

async function fetchLive() {
  return fetchAPI(adminApi.tokens + LIVE_QS);
}

const store = createQueryStore({
  intervalMs: INTERVAL_MS,
  fetchFull,
  fetchLive,
  merge: (_cached, live) => mergeLive(live),
  subscribe: (next) =>
    useEventStream({
      onTokens: (data) => {
        // SSE pushes the full tokensData shape: refresh the static cache too.
        if (data && typeof data === "object") rememberStatic(data);
        next(data);
      },
    }),
});

/** @type {import('svelte/store').Writable<any>} */
export const tokensData = store.data;

/** @type {import('svelte/store').Writable<string>} */
export const tokensError = store.error;

/**
 * Reference-counted activation: a page calls this in onMount, keeps holding
 * the store alive until it unmounts. First consumer starts the poll + SSE;
 * the last release stops them (the cached value stays for the next page).
 * @returns {() => void} Release function for onDestroy.
 */
export function ensureTokensStore() {
  return store.ensure();
}

/**
 * Force an immediate refetch, used by page mutations (add/remove/lock/
 * rotation) so the shared value updates without waiting for the next tick.
 * @returns {Promise<void>}
 */
export function refreshTokens() {
  // Mutations can change pool membership and account state: drop the static
  // cache so the next poll takes the full shape.
  staticTop = null;
  return store.refresh();
}

/**
 * Zero-cost quota refresh for every pooled token (the CLI-landing trick):
 * POSTs /admin/tokens/test-all, which probes each token with a read-only
 * upstream GET (no session claim, no slot spent) and writes the fresh
 * quota into the snapshots via UpdateQuotaFromProbe. The store refetch
 * below then renders the new numbers. The manual endpoint answers with
 * one JSON array; the body is drained as text and ignored either way —
 * only res.ok matters here. Per-token detail stays on the
 * Tokens page probe buttons.
 * With `{ auto: true }` (ADR-0025 visit probe) the request carries
 * `?auto=1`: the server probes only when its pool-scoped last-bulk-probe
 * timestamp is older than an hour, otherwise it returns the current view
 * untouched. Either way the body shape stays drain-as-text.
 * @param {{ auto?: boolean }} [opts]
 * @returns {Promise<void>}
 */
export function probeAllQuotas(opts = {}) {
  const url = opts.auto
    ? `${adminActions.tokenTestAll}?auto=1`
    : adminActions.tokenTestAll;
  return fetch(url, {
    method: "POST",
    headers: csrfHeader("POST"),
  })
    .then(async (res) => {
      const text = await res.text().catch(() => "");
      if (!res.ok) {
        // Admin endpoints emit one envelope ({ok,message[,code]}); surface
        // the human message (e.g. "No tokens to test…") instead of a bare
        // HTTP status. The manual test-all body shape (array vs objects)
        // is irrelevant here — only the failure message is parsed.
        let msg;
        try {
          const parsed = JSON.parse(text);
          msg = parsed?.message ?? "";
        } catch {
          msg = text;
        }
        throw new Error(msg || `HTTP ${res.status}`);
      }
    })
    .then(() => refreshTokens());
}
