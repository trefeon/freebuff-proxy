import { getEnvValue } from "./env.js";

/**
 * DEVTOOLS_ENABLED gate (issue #287): the single predicate shared by the
 * sidebar's Dev Tools tab, the DevTools page self-check, and the Tokens
 * per-token session-spawn toolbar. True when the .env value is the literal
 * 'true' or '1' (case-insensitive), matching the old regex/trims at each site.
 *
 * @param {string} envContent - Raw .env document text.
 * @returns {boolean}
 */
export function isDevToolsEnabled(envContent) {
  const val = (getEnvValue(envContent, "DEVTOOLS_ENABLED") || "").toLowerCase();
  return val === "true" || val === "1";
}
