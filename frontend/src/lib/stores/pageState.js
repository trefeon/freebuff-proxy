/**
 * Per-page dashboard state persisted in the gateway's pages_state table.
 *
 * The SPA stays fetch-only (no localStorage): every page loads its snapshot
 * on mount and saves it back debounced (~1s). All failures are warn-only —
 * a persistence outage must never break the page. Snapshots are opaque JSON
 * objects keyed by page id (see lib/nav.js ids plus the "shell" chrome key
 * holding { lastHash }); the server rejects unknown ids and caps data at
 * 64KB.
 */

import { fetchAPI, putAPI } from "../api/client.js";
import { adminApi } from "../api/paths.js";

/** Last successfully loaded/saved snapshot per page (local-first merge base). */
const cache = new Map();

/** Pending debounced save timer per page. */
const timers = new Map();

/**
 * Load a page snapshot. Never throws: failures warn and resolve to the
 * cached snapshot (or {} when nothing was ever loaded).
 * @param {string} pageId
 * @returns {Promise<Record<string, any>>}
 */
export async function loadPageState(pageId) {
  try {
    const res = await fetchAPI(adminApi.pageState(pageId));
    const data =
      res && typeof res.data === "object" && res.data !== null ? res.data : {};
    // Local-first: a debounced save queued before the load resolves wins
    // over the stale server read (visit recorded at mount, deep state
    // restored from the same payload).
    cache.set(pageId, { ...data, ...(cache.get(pageId) ?? {}) });
    return cache.get(pageId);
  } catch (e) {
    console.warn(`page state load failed for ${pageId}`, e);
    return cache.get(pageId) ?? {};
  }
}

/**
 * Merge a patch into the cached snapshot and persist it debounced (~1s).
 * Warn-only: save failures never surface to the caller.
 * @param {string} pageId
 * @param {Record<string, any>} patch
 */
export function savePageState(pageId, patch) {
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
