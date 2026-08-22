import { onMount, onDestroy } from 'svelte';
import { isSessionDead } from '../stores/session.js';

/**
 * Set up visibility-aware polling. Pauses when the tab is hidden,
 * resumes when visible. Calls fetchFn immediately on mount.
 *
 * Overlap guard: while a previous fetchFn call is still in flight, a tick is
 * skipped rather than stacking concurrent requests (slow fetches could
 * otherwise overlap and out-of-order completions clobber newer state).
 *
 * @param {() => Promise<void>} fetchFn - Async function to call on each tick
 * @param {number} intervalMs - Polling interval in milliseconds
 */
export function usePolling(fetchFn, intervalMs) {
  let timer;
  let running = false;

  async function tick() {
    if (running || isSessionDead()) return; // dead session or previous fetch still pending — skip this tick
    running = true;
    try {
      await fetchFn();
    } finally {
      running = false;
    }
  }

  function start() {
    clearInterval(timer);
    timer = setInterval(tick, intervalMs);
  }

  function stop() {
    clearInterval(timer);
  }

  function handleVisibility() {
    if (document.hidden) {
      stop();
    } else {
      start();
    }
  }

  onMount(() => {
    tick();
    start();
    document.addEventListener('visibilitychange', handleVisibility);
  });

  onDestroy(() => {
    stop();
    document.removeEventListener('visibilitychange', handleVisibility);
  });
}
