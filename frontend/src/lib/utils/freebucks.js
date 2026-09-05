/**
 * Freebucks meter intents for spawn pickers (mirrors freebucksRowIntent in
 * upstream cli/src/utils/freebucks.ts, issue #350 port).
 *
 * Three outcomes, ordered by what the pick costs the reader:
 * - paywall — pool AND wallet together cannot cover the price: refuse where
 *   the balance is already on screen (disable the option/button).
 * - confirm — affordable but spends something that does not come back (a
 *   live session, or wallet Freebucks): enrich the confirm dialog.
 * - allow — everything else, including every row on an unmetered account.
 */

export function freebucksOf(token) {
  return token?.freebucks ?? null;
}

export function modelPrice(fb, modelId) {
  return fb?.prices?.[modelId];
}

export function spawnIntent(token, modelId) {
  const fb = freebucksOf(token);
  const price = modelPrice(fb, modelId);
  if (!fb || price === undefined)
    return { kind: "allow", price, walletSpend: 0 };
  const balance = fb.balance ?? fb.Balance ?? 0;
  if (fb.quota_exempt ?? fb.quotaExempt ?? false)
    return { kind: "allow", price, walletSpend: 0 };
  // `<` not `<=`: a balance that exactly equals the price BUYS the session.
  if (balance < price) return { kind: "paywall", price, walletSpend: 0 };
  const dailyRemaining = fb.daily?.remaining ?? fb.Daily?.Remaining ?? 0;
  const walletSpend = Math.max(0, price - dailyRemaining);
  const activeModel = token?.session_model;
  const endsSession = activeModel !== undefined && activeModel !== modelId;
  if (endsSession || walletSpend > 0)
    return { kind: "confirm", price, walletSpend };
  return { kind: "allow", price, walletSpend: 0 };
}

/** Confirm-dialog line for a confirm intent (mirrors askLineFor). */
export function intentAskLine(intent, activeModel) {
  if (intent.kind !== "confirm") return null;
  if (intent.walletSpend > 0)
    return `Today's Freebucks pool is spent — this uses ${intent.walletSpend} from the wallet${activeModel ? " and ends the live session" : ""}.`;
  return `This ends the live session and starts a new one (price ${intent.price}).`;
}
