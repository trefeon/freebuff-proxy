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

export const MODEL_METADATA = {
  "upstage/solar-pro4": {
    displayName: "Solar Pro 4",
    tagline: "Fast & Direct",
    badges: [],
  },
  "z-ai/glm-5.3-flash": {
    displayName: "GLM 5.3 Flash",
    tagline: "Deep reasoning",
    badges: ["Reasoning: max*", "Images", "NEW"],
  },
  "mimo/mimo-v2.5": {
    displayName: "MiMo 2.5",
    tagline: "Balanced",
    badges: ["Images"],
  },
  "deepseek/deepseek-v4-flash": {
    displayName: "DeepSeek V4 Flash 07/31",
    tagline: "Smart & Fast",
    badges: ["Reasoning: high", "NEW"],
    disclaimer: "May use data for AI training",
  },
  "meta/muse-spark-1.3-contributor": {
    displayName: "Muse Spark 1.3",
    tagline: "Queues, then falls back",
    badges: ["Reasoning: xhigh", "NEW"],
    disclaimer: "May use data for AI training",
  },
  "openai/gpt-5.6-luna": {
    displayName: "GPT-5.6 Luna",
    tagline: "Strong all-around",
    badges: ["Reasoning: high", "Images"],
  },
  "z-ai/glm-5.2": {
    displayName: "GLM 5.2",
    tagline: "Referral reward",
    badges: ["Referral only"],
  },
};

export function formatFreebucks(v) {
  if (v == null || v === "") return "0";
  const n = Number(v);
  if (Number.isNaN(n)) return String(v);
  if (Number.isInteger(n)) return String(n);
  return String(Math.round(n * 100) / 100);
}

// "4h 12m", "38m", "2d 5h" — until the daily pool refills; "now" once it
// has passed. Port of upstream freebucksResetCountdown (same shape as the
// Web/Desktop pickers; issue #354).
export function freebucksResetCountdown(resetAt, nowMs) {
  const remainingMs = Date.parse(resetAt) - nowMs;
  if (!Number.isFinite(remainingMs) || remainingMs <= 0) return "now";
  const totalMinutes = Math.ceil(remainingMs / 60000);
  const days = Math.floor(totalMinutes / 1440);
  const hours = Math.floor((totalMinutes % 1440) / 60);
  const minutes = totalMinutes % 60;
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${minutes}m`;
  return `${minutes}m`;
}

// "$25", "$4.20", "$0" — whole dollars until the figure is small enough
// that the cents are the story. Port of upstream formatAllowanceUsd
// (issue #354).
export function formatAllowanceUsd(usd) {
  const safe = Math.max(0, Number(usd) || 0);
  if (safe >= 10) return `$${Math.round(safe)}`;
  if (safe >= 1) return `$${safe.toFixed(1).replace(/\.0$/, "")}`;
  return `$${safe.toFixed(2)}`;
}

// The one-line header for a metered account:
// `7.5/10 Freebucks daily · resets in 4h 12m · 5 in wallet · $258 monthly
// usage left` (countdown only when a clock is given, wallet only when there
// is something in it, monthly only when the server sent it — never a `$0`
// that reads as spent). Port of upstream freebucksHeaderLine (issue #354);
// `t` is the translator (defaults to identity = upstream-exact English).
export function freebucksHeaderLine(fb, nowMs, t = (s) => s) {
  if (!fb?.daily) return "";
  const parts = [
    `${formatFreebucks(fb.daily.remaining)}/${formatFreebucks(fb.daily.limit)} ${t("Freebucks daily")}`,
  ];
  if (nowMs !== undefined && fb.daily.reset_at) {
    parts.push(
      `${t("resets in")} ${freebucksResetCountdown(fb.daily.reset_at, nowMs)}`,
    );
  }
  const walletBalance = fb.wallet?.balance ?? 0;
  if (walletBalance > 0) {
    parts.push(`${formatFreebucks(walletBalance)} ${t("in wallet")}`);
  }
  if (fb.monthly != null && fb.monthly.remaining != null) {
    parts.push(
      `${formatAllowanceUsd(fb.monthly.remaining)} ${t("monthly usage left")}`,
    );
  }
  return parts.join(" · ");
}
export function freebucksPriceLabel(price) {
  return `${formatFreebucks(price)} Freebucks/hr`;
}

export function modelDisplayInfo(modelId, freebucks) {
  const meta = MODEL_METADATA[modelId] || {
    displayName: modelId,
    tagline: "",
    badges: [],
    disclaimer: "",
  };
  const price = freebucks?.prices?.[modelId] ?? 0;
  const customNotice = freebucks?.price_notices?.[modelId];
  const notice = customNotice || meta.disclaimer || "";
  const balance = freebucks?.balance ?? freebucks?.Balance ?? 0;
  const exempt = freebucks?.quota_exempt ?? freebucks?.quotaExempt ?? false;
  const canStart = exempt || balance >= price;
  const shortfall = Math.max(0, price - balance);
  return {
    id: modelId,
    displayName: meta.displayName,
    tagline: meta.tagline,
    badges: meta.badges,
    notice,
    price,
    canStart,
    shortfall: formatFreebucks(shortfall),
  };
}

export function sortModelsByPrice(modelIds, freebucks) {
  if (!Array.isArray(modelIds)) return [];
  const priceOf = (id) => freebucks?.prices?.[id] ?? Number.POSITIVE_INFINITY;
  const nameOf = (id) => MODEL_METADATA[id]?.displayName || id;
  return [...modelIds].sort(
    (a, b) => priceOf(a) - priceOf(b) || nameOf(a).localeCompare(nameOf(b)),
  );
}
