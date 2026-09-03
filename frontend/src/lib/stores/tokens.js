import { writable } from "svelte/store";
import { fetchAPI, SessionExpiredError } from "../api/client.js";
import { adminApi } from "../api/paths.js";
import { useEventStream } from "../utils/events.js";
import { isSessionDead } from "./session.js";

// Shared tokens snapshot (issue #292). Tokens.svelte, QuotaTracker.svelte and
// DevTools.svelte previously each wired their own /admin/api/tokens poll and
// /admin/api/events SSE subscription with the same try/catch scaffold. This
// module-level singleton owns ONE poll + ONE SSE subscription (visibility-aware,
// mirroring polling.js) and every page renders from the same value, so a
// mutation on one page is reflected immediately on the others.

const INTERVAL_MS = 10000;

/** @type {import('svelte/store').Writable<any>} */
export const tokensData = writable(null);

/** @type {import('svelte/store').Writable<string>} */
export const tokensError = writable("");

let started = false;
let busy = false;
let timer = null;
let unsubEvents = null;
let consumers = 0;

// Issue #322: account-stable fields (email/account_id, daily_limit,
// standing_*, referral_*) ride a once-per-mount full fetch; the 10s hot poll
// hits ?view=live and merges over the cached static snapshot. A full refresh
// every ~5min (plus every mutation and every full-shape SSE push) picks up
// mid-session changes (trust updates, referral consumption, pool edits).
const LIVE_QS = "?view=live";
const FULL_EVERY_POLLS = 30;
const FULL_EVERY_MS = 5 * 60 * 1000;
const STATIC_TOP_KEYS = ["mode", "in_bridge", "show_bridge"];
const STATIC_TOKEN_KEYS = [
  "email",
  "account_id",
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
  "referral_sessions_left",
  "referral_github_linked",
  "referral_reset_at",
];
let staticTop = null;
let staticTokensByIndex = {};
let polls = 0;
let lastFullAt = 0;

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
  lastFullAt = Date.now();
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
  polls = 0;
  return data;
}

async function poll() {
  if (busy || isSessionDead()) return;
  busy = true;
  try {
    polls += 1;
    let data;
    if (
      staticTop === null ||
      polls % FULL_EVERY_POLLS === 0 ||
      Date.now() - lastFullAt > FULL_EVERY_MS
    ) {
      data = await fetchFull();
    } else {
      data = mergeLive(await fetchAPI(adminApi.tokens + LIVE_QS));
    }
    tokensData.set(data);
    tokensError.set("");
  } catch (e) {
    // A SessionExpiredError already surfaced the banner via the API client;
    // do not show a page-level error for that. Everything else surfaces the
    // message so the page can leave its loading state instead of spinning.
    if (!(e instanceof SessionExpiredError)) {
      tokensError.set(e.message || "Failed to fetch tokens");
      console.warn("tokens store: poll failed", e);
    }
  } finally {
    busy = false;
  }
}

function startInterval() {
  clearInterval(timer);
  timer = setInterval(poll, INTERVAL_MS);
}

function stopInterval() {
  clearInterval(timer);
  timer = null;
}

function handleVisibility() {
  if (document.hidden) {
    stopInterval();
  } else {
    startInterval();
    poll();
  }
}

function startStore() {
  if (started) return;
  started = true;
  poll();
  startInterval();
  document.addEventListener("visibilitychange", handleVisibility);
  unsubEvents = useEventStream({
    onTokens: (data) => {
      // SSE pushes the full tokensData shape: refresh the static cache too.
      if (data && typeof data === "object") rememberStatic(data);
      tokensData.set(data);
      tokensError.set("");
    },
  });
}

function stopStore() {
  if (!started) return;
  started = false;
  stopInterval();
  document.removeEventListener("visibilitychange", handleVisibility);
  unsubEvents?.();
  unsubEvents = null;
}

/**
 * Reference-counted activation: a page calls this in onMount, keeps holding
 * the store alive until it unmounts. First consumer starts the poll + SSE;
 * the last release stops them (the cached value stays for the next page).
 * @returns {() => void} Release function for onDestroy.
 */
export function ensureTokensStore() {
  consumers += 1;
  startStore();
  return function release() {
    consumers -= 1;
    if (consumers <= 0) {
      consumers = 0;
      stopStore();
    }
  };
}

/**
 * Force an immediate refetch, used by page mutations (add/remove/lock/
 * rotation) so the shared value updates without waiting for the next tick.
 * @returns {Promise<void>}
 */
export function refreshTokens() {
  if (!started) startStore();
  // Mutations can change pool membership and account state: drop the static
  // cache so the next poll takes the full shape.
  staticTop = null;
  return poll();
}
