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

async function poll() {
  if (busy || isSessionDead()) return;
  busy = true;
  try {
    const data = await fetchAPI(adminApi.tokens);
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
  return poll();
}
