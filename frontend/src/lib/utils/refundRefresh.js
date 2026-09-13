import { postAPI } from "../api/client.js";
import { tokenActions } from "../api/paths.js";
import { refreshTokens } from "../stores/tokens.js";

// Shared pending-refund refresh trigger (refund parity follow-up): one POST
// replays the parked DELETE through the session manager's same-instance
// RefreshRefund path, then the tokens store reloads so the settled line
// renders on the next paint instead of the next 10s poll.

// Single-flight per account: concurrent refreshes for one slot join the
// same POST instead of stacking upstream DELETEs.
const inflight = new Map();
// One automatic attempt per parked instance id: the pending line fires once
// when it first renders, never per render. Operator retries use the manual
// Refresh button; the backend single-flights those too.
const autoTried = new Set();

function slotKey(idx) {
  return Number(idx);
}

/**
 * Replay the parked pending-refund DELETE for one pool slot.
 * @param {number} idx token slot index
 * @returns {Promise<any>} the {ok, message} result envelope
 */
export async function refreshRefund(idx) {
  const k = slotKey(idx);
  const running = inflight.get(k);
  if (running) return running;
  const p = (async () => {
    try {
      const res = await postAPI(tokenActions.refundRefresh(k), {});
      // A settled replay only lands in the store on the next poll; an
      // explicit reload renders the settled line immediately.
      await refreshTokens().catch(() => {});
      return res;
    } finally {
      inflight.delete(k);
    }
  })();
  inflight.set(k, p);
  return p;
}

/**
 * One automatic refresh attempt for a freshly rendered pending line.
 * No-op when nothing is parked, the instance already had its attempt, or a
 * refresh for the slot is already in flight.
 * @param {object} token dashboard tokenCard payload
 * @returns {Promise<any> | null} the attempt, or null when skipped
 */
export function autoRefreshRefund(token) {
  const pending = token?.pending_refund;
  if (!pending || autoTried.has(pending)) return null;
  const idx = token?.index ?? -1;
  if (!Number.isInteger(idx) || idx < 0) return null;
  if (inflight.has(slotKey(idx))) return null;
  autoTried.add(pending);
  return refreshRefund(idx).catch(() => null);
}
