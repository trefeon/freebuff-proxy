import { adminApi } from '../api/paths.js';
import { isSessionDead } from '../stores/session.js';

/**
 * Subscribe to the real-time server-sent events stream for live dashboard
 * updates (token status, session lifecycle, cooldowns, quota).
 *
 * Fallback contract: if EventSource fails (e.g. older browser, proxy drops SSE,
 * unauthenticated), onError fires and the caller continues using its polling
 * fallback. Visibility-aware: closes the stream when the tab is hidden and
 * reconnects on visible (matches usePolling idle-friendliness).
 *
 * @param {Object} handlers
 * @param {(data: any) => void} handlers.onTokens - Called with fresh tokensData
 * @param {() => void} [handlers.onOpen] - Called when stream connects
 * @param {(err: any) => void} [handlers.onError] - Called on disconnect / error
 * @returns {() => void} Cleanup function to close the stream
 */
export function useEventStream({ onTokens, onOpen, onError }) {
  if (typeof window === 'undefined' || typeof EventSource === 'undefined') {
    return () => {};
  }
  let es = null;
  let closed = false;

  function connect() {
    if (closed || isSessionDead() || document.hidden) return;
    try {
      es = new EventSource(adminApi.events);
      es.addEventListener('tokens', (e) => {
        try {
          const data = JSON.parse(e.data);
          onTokens?.(data);
        } catch (err) {
          console.debug('sse: json parse failed', err);
        }
      });
      es.onopen = () => {
        onOpen?.();
      };
      es.onerror = (err) => {
        onError?.(err);
      };
    } catch (err) {
      onError?.(err);
    }
  }

  function disconnect() {
    if (es) {
      es.close();
      es = null;
    }
  }

  function handleVisibility() {
    if (document.hidden) {
      disconnect();
    } else {
      connect();
    }
  }

  connect();
  document.addEventListener('visibilitychange', handleVisibility);

  return () => {
    closed = true;
    document.removeEventListener('visibilitychange', handleVisibility);
    disconnect();
  };
}
