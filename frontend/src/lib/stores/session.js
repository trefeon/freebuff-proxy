import { writable } from 'svelte/store';

/**
 * Global session-expiry state shared by the API client, the polling helper,
 * and the App shell banner.
 *
 * Invariant: an expired session must NEVER cause a full-page reload from
 * background code (that reload loop is what issue #197 reports). Detection
 * happens in lib/api/client.js, which calls markSessionExpired() instead of
 * navigating; the banner in App.svelte is the only recovery surface, and its
 * Log in button is an explicit user action.
 */

/**
 * True while the current page's admin session is known to be dead.
 * Drives the "Session expired" banner in App.svelte.
 */
export const sessionExpired = writable(false);

// Module-level latch: once a 401 / auth redirect is observed, background
// polling halts for the life of the page. Dismissing the banner hides it but
// does NOT resume hammering a dead endpoint; recovery is an explicit
// re-login (full navigation resets this module) or a manual reload.
let sessionDead = false;

export function markSessionExpired() {
  if (sessionDead) return;
  sessionDead = true;
  sessionExpired.set(true);
}

/** Polling gate: true once expiry has been observed on this page load. */
export function isSessionDead() {
  return sessionDead;
}

export function dismissSessionExpired() {
  sessionExpired.set(false);
}

/**
 * Reload-loop guard for any remaining automatic (non-user-initiated)
 * navigation to /admin/login: at most ONE attempt per 30s window; afterwards
 * callers must fall back to the banner instead of navigating. Returns true
 * exactly when the caller may navigate (and records that attempt).
 */
const AUTO_REDIRECT_COOLDOWN_MS = 30_000;
let lastAutoRedirectAt = -Infinity;

export function autoRedirectAllowed(now = Date.now()) {
  if (now - lastAutoRedirectAt < AUTO_REDIRECT_COOLDOWN_MS) return false;
  lastAutoRedirectAt = now;
  return true;
}
