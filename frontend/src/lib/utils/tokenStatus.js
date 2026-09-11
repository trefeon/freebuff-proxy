import { get } from "svelte/store";
import { tr } from "../i18n.js";

// Shared token-status helpers for TokenCard + TokenCardMobile. The two cards
// carried identical copies of every function here; only their collapsed
// markup differs (a <tr> cannot responsively become a stacked card, so the
// fork is structural). Pure functions of the token payload plus the page
// clock — the cards keep their markup and the reactive tick.

function t() {
  return get(tr);
}

/** Ban chip, or null when the account is not banned. Claims priority. */
export function banBadge(token) {
  if (token.ban_type === "hard") {
    return {
      label: t()("banned — appeal required"),
      tone: "critical",
      pulse: true,
    };
  }
  if (token.ban_type === "temporary") {
    return { label: t()("banned (temporary)"), tone: "bad" };
  }
  return null;
}

/** Primary status chip for one pooled token. */
export function statusFor(token) {
  const ban = banBadge(token);
  if (ban) return ban;
  if (token.locked) return { label: t()("locked"), tone: "warn" };
  if (token.cooldown_active) return { label: t()("cooldown"), tone: "warn" };
  const s = token.session_status || "";
  if (s === "active")
    return { label: t()("leased"), tone: "good", pulse: true };
  if (s === "queued") return { label: t()("queued"), tone: "info" };
  if (s === "banned") return { label: t()("banned"), tone: "bad" };
  if (s === "expired") return { label: t()("expired"), tone: "idle" };
  if (s === "grace") return { label: t()("grace drain"), tone: "warn" };
  return { label: t()("idle"), tone: "idle" };
}

/** Risk-level string to LED tone. */
export function riskTone(risk) {
  switch (risk) {
    case "low":
      return "good";
    case "moderate":
      return "warn";
    case "high":
    case "critical":
      return "bad";
    default:
      return "idle";
  }
}

/**
 * Risk chip (moved from the standalone At-risk cards): shown when the
 * account carries a risk flag and no ban badge already claims the row.
 */
export function riskBadgeFor(token) {
  if (banBadge(token)) return null;
  if (token.risk_level && token.risk_level !== "low") {
    return {
      label: token.risk_level,
      tone: riskTone(token.risk_level),
      pulse: token.risk_level === "critical",
    };
  }
  return null;
}

/** Live cooldown countdown against the page clock (ms epoch). */
export function cooldownLabel(token, now) {
  if (!token.cooldown_active || !token.cooldown_until) return "—";
  const ms = new Date(token.cooldown_until).getTime() - now;
  if (ms <= 0) return "expiring";
  const s = Math.floor(ms / 1000);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  if (h >= 24) {
    const d = Math.floor(h / 24);
    const hr = h % 24;
    return hr > 0 ? `${d}d ${hr}h` : `${d}d`;
  }
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${sec}s`;
  return `${sec}s`;
}
