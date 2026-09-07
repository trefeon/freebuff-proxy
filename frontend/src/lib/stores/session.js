import { writable } from "svelte/store";

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
/**
 * Shared admin auth state for default admin token and require login.
 */
export const authState = writable({
  isDefaultAdminToken: false,
  requireLogin: true,
  hasPassword: true,
});

export function updateAuthState(partial) {
  authState.update((s) => ({ ...s, ...partial }));
}

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

/** Reset the session latch after a successful login. */
export function resetSessionState() {
  sessionDead = false;
  sessionExpired.set(false);
}
