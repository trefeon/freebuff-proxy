import { writable } from "svelte/store";
import { isSessionDead } from "./session.js";

// Shared query store behind every rewired page. One owner per endpoint: a
// refcounted poll loop (visibility-aware, overlap-guarded, dead-session
// gated) plus an optional push subscription (SSE). Pages render from `data`
// and never wire their own timers.
//
// Full/live split: endpoints that serve a cheap live view pass `fetchLive`
// with a `merge(cached, live)` that overlays it on the last full snapshot
// (the Overview/Tokens static-key pattern, generalized). A full refresh runs
// on first poll, every FULL_EVERY_POLLS polls, every FULL_EVERY_MS, and on
// every `refresh()` call (page mutations call it instead of waiting a tick).

const FULL_EVERY_POLLS = 30;
const FULL_EVERY_MS = 5 * 60 * 1000;

/**
 * @param {Object} opts
 * @param {number} opts.intervalMs - hot poll interval
 * @param {() => Promise<any>} opts.fetchFull - full-shape fetch
 * @param {(() => Promise<any>)} [opts.fetchLive] - cheap live-view fetch
 * @param {(cached: any, live: any) => any} [opts.merge] - overlay live onto cached
 * @param {(next: (v: any) => void) => (() => void) | void} [opts.subscribe] - push subscription, returns cleanup
 */
export function createQueryStore({
  intervalMs,
  fetchFull,
  fetchLive,
  merge,
  subscribe,
}) {
  const data = writable(null);
  const error = writable("");

  let consumers = 0;
  let timer = null;
  let busy = false;
  let polls = 0;
  let lastFullAt = 0;
  let unsubPush = null;

  function remember(full) {
    lastFullAt = Date.now();
    data.set(full);
  }

  async function poll() {
    if (busy || isSessionDead()) return;
    busy = true;
    try {
      polls += 1;
      const stale =
        lastFullAt === 0 ||
        polls % FULL_EVERY_POLLS === 0 ||
        Date.now() - lastFullAt > FULL_EVERY_MS;
      if (!fetchLive || !merge || stale) {
        remember(await fetchFull());
      } else {
        const live = await fetchLive();
        data.update((cached) => merge(cached, live));
      }
      error.set("");
    } catch (e) {
      if (!isSessionDead()) error.set(e?.message ?? String(e));
    } finally {
      busy = false;
    }
  }

  function startInterval() {
    clearInterval(timer);
    timer = setInterval(poll, intervalMs);
  }

  function stopInterval() {
    clearInterval(timer);
    timer = null;
  }

  function handleVisibility() {
    if (document.hidden) stopInterval();
    else {
      poll();
      startInterval();
    }
  }

  function start() {
    polls = 0;
    lastFullAt = 0;
    poll();
    startInterval();
    document.addEventListener("visibilitychange", handleVisibility);
    if (subscribe) {
      try {
        unsubPush = subscribe((v) => remember(v)) ?? null;
      } catch {
        unsubPush = null;
      }
    }
  }

  function stop() {
    stopInterval();
    document.removeEventListener("visibilitychange", handleVisibility);
    unsubPush?.();
    unsubPush = null;
  }

  /**
   * Reference-counted activation: each mounted page calls this once and
   * calls the returned release on unmount. The loop runs while at least one
   * consumer holds it.
   */
  function ensure() {
    consumers += 1;
    if (consumers === 1) start();
    return () => {
      consumers = Math.max(0, consumers - 1);
      if (consumers === 0) stop();
    };
  }

  /** Force a full refresh now (page mutations call this). */
  function refresh() {
    lastFullAt = 0;
    return poll();
  }

  return { data, error, ensure, refresh };
}
