/**
 * Per-page dashboard state persisted in the gateway's pages_state table.
 *
 * The SPA stays fetch-only (no localStorage): every page loads its snapshot
 * on mount and saves it back debounced (~1s). All failures are warn-only —
 * a persistence outage must never break the page. Snapshots are opaque JSON
 * objects keyed by page id (see lib/nav.js ids plus the "shell" chrome key
 * holding { lastHash }); the server rejects unknown ids and caps data at
 * 64KB.
 *
 * Precedence rules:
 * - Load is server-wins. The freshly fetched snapshot REPLACES the local
 *   merge base; a stale tab must never resurrect cached values over newer
 *   server state (a second tab may have saved while this one sat in the
 *   background). Only unflushed debounced patches (≤1s old, still in
 *   `timers`) survive a load, and they do so by re-saving after it.
 * - shell.lastHash is last-writer-wins: every hashchange PUTs the newest
 *   hash and the server applies PUTs in arrival order. There is no
 *   compare-and-swap — two tabs racing simply resolve to whichever write
 *   lands last. Callers (App.svelte) must only ever write known page ids.
 * - Unload flushes through `pagehide` with a keepalive PUT so a late
 *   debounced save is not silently lost when the tab closes mid-debounce.
 * - An oversized snapshot (PUT 413 page_too_large) is evicted from the
 *   merge cache and surfaced via `pageStateNotice` so the UI can tell the
 *   operator the page state was discarded instead of retrying it forever.
 */

import { writable } from "svelte/store";
import { fetchAPI, putAPI, csrfHeader } from "../api/client.js";
import { adminApi } from "../api/paths.js";

/** Last successfully loaded snapshot per page (debounce merge base). */
const cache = new Map();

/** Pending debounced save timer per page. */
const timers = new Map();

/**
 * User-visible persistence hint ({ text, at } or null). Set when an
 * oversized snapshot is discarded (PUT 413); pages with filter-heavy state
 * (Logs) render it as a dismissible warning. Null means no warning.
 */
export const pageStateNotice = writable(null);

/** True once the unload-flush listener is registered (module singleton). */
let flushArmed = false;

/**
 * Best-effort unload flush: fire every pending debounced save as an
 * untracked keepalive PUT so closing the tab mid-debounce does not drop the
 * latest patch. Warn-only and synchronous — never throws, never awaits.
 */
function armUnloadFlush() {
  if (flushArmed || typeof window === "undefined") return;
  flushArmed = true;
  window.addEventListener("pagehide", () => {
    for (const [pageId, t] of timers) {
      clearTimeout(t);
      timers.delete(pageId);
      try {
        const body = JSON.stringify({ data: cache.get(pageId) ?? {} });
        fetch(adminApi.pageState(pageId), {
          method: "PUT",
          headers: {
            "Content-Type": "application/json",
            Accept: "application/json",
            "X-Requested-With": "fetch",
            ...csrfHeader("PUT"),
          },
          body,
          keepalive: true,
        }).catch(() => {});
      } catch {
        // Unload path: persistence is warn-only, never throw.
      }
    }
  });
}

/**
 * Load a page snapshot. Never throws: a failure warns and resolves to the
 * cached snapshot (or {} when nothing was ever loaded). The server payload
 * replaces the cache (server-wins); see the module doc for why the old
 * cache-over-server merge was dropped.
 * @param {string} pageId
 * @returns {Promise<Record<string, any>>}
 */
export async function loadPageState(pageId) {
  try {
    const res = await fetchAPI(adminApi.pageState(pageId));
    const data =
      res && typeof res.data === "object" && res.data !== null ? res.data : {};
    cache.set(pageId, { ...data });
    return cache.get(pageId);
  } catch (e) {
    console.warn(`page state load failed for ${pageId}`, e);
    return cache.get(pageId) ?? {};
  }
}

/**
 * Merge a patch into the cached snapshot and persist it debounced (~1s).
 * Warn-only: save failures never surface to the caller, except an oversized
 * snapshot (PUT 413 page_too_large) which is evicted from the merge cache
 * and published on `pageStateNotice` so the UI can say so.
 * @param {string} pageId
 * @param {Record<string, any>} patch
 */
export function savePageState(pageId, patch) {
  armUnloadFlush();
  cache.set(pageId, { ...(cache.get(pageId) ?? {}), ...patch });
  if (timers.has(pageId)) clearTimeout(timers.get(pageId));
  timers.set(
    pageId,
    setTimeout(async () => {
      timers.delete(pageId);
      try {
        await putAPI(adminApi.pageState(pageId), {
          data: cache.get(pageId) ?? {},
        });
      } catch (e) {
        if (e?.status === 413 || e?.code === "page_too_large") {
          // The server refused the snapshot: drop it so later patches
          // re-seed from fresh state instead of retrying the same payload.
          cache.delete(pageId);
          pageStateNotice.set({
            text: `Could not remember the ${pageId} page state (over the 64KB server cap) — it was discarded.`,
            at: Date.now(),
          });
        }
        console.warn(`page state save failed for ${pageId}`, e);
      }
    }, 1000),
  );
}

/**
 * Record a page visit (timestamp) — the one-line hook every page calls on
 * mount. Merges into the snapshot so deep state (expanded rows, filters)
 * is never clobbered.
 * @param {string} pageId
 */
export function recordPageVisit(pageId) {
  savePageState(pageId, { visitedAt: Date.now() });
}
