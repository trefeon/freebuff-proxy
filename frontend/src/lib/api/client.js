/**
 * Core API fetch client for freebuff-proxy dashboard.
 * Handles HTTP requests, headers, and error unwrapping.
 *
 * CSRF: state-changing requests echo the server's double-submit nonce
 * (`fb_csrf` cookie) as the X-CSRF-Token header. When the cookie is absent
 * the header is omitted and the server keeps its Origin/Sec-Fetch-Site
 * checks; the server sets the cookie on login and on any admin response
 * where it is still missing, so the wrapper re-reads it per request.
 *
 * Auth failures (401 / redirect to the login page) NEVER navigate: they raise
 * SessionExpiredError and set the global session-expired flag
 * (lib/stores/session.js) so App.svelte can show the re-login banner.
 * Background polling must never reload the page (issue #197).
 */

import { markSessionExpired } from "../stores/session.js";
import { adminRoot } from "./paths.js";

/** Thrown by fetchAPI/postAPI when the admin session is no longer valid. */
export class SessionExpiredError extends Error {
  constructor(message = "Session expired") {
    super(message);
    this.name = "SessionExpiredError";
  }
}

function handleAuthFailure(message) {
  markSessionExpired();
  throw new SessionExpiredError(message);
}

/**
 * Read the double-submit CSRF nonce (`fb_csrf`) from the cookie jar. The
 * server sets it on successful login and on any admin response where it is
 * still absent; state-changing requests echo it back as X-CSRF-Token.
 * @returns {string} Nonce value, or '' when the cookie is absent.
 */
export function csrfToken() {
  if (typeof document === "undefined") return "";
  const m = document.cookie.match(/(?:^|;\s*)fb_csrf=([^;]*)/);
  return m ? decodeURIComponent(m[1]) : "";
}

/**
 * Build the CSRF header map for the given method. GET/HEAD never carry it;
 * when the nonce cookie is absent the header is omitted entirely and the
 * server keeps its Origin/Sec-Fetch-Site checks.
 * @param {string} [method]
 * @returns {Record<string, string>}
 */
export function csrfHeader(method = "GET") {
  const m = String(method).toUpperCase();
  if (m === "GET" || m === "HEAD") return {};
  const token = csrfToken();
  return token ? { "X-CSRF-Token": token } : {};
}

/**
 * Fetch JSON from an admin API endpoint. On auth failure, sets the global
 * session-expired flag and throws SessionExpiredError (no page navigation).
 * @param {string} path - API path from lib/api/paths.js
 * @param {RequestInit} [opts] - Additional fetch options
 * @returns {Promise<any>} Parsed JSON response
 * @throws {SessionExpiredError} When the session is no longer valid
 * @throws {Error} On non-OK status or network failure
 */
export async function fetchAPI(path, opts = {}) {
  const res = await fetch(path, {
    ...opts,
    headers: {
      Accept: "application/json",
      "X-Requested-With": "fetch",
      ...csrfHeader(opts.method),
      ...opts.headers,
    },
  });

  // dashboardAuth answers unauthenticated admin API requests with a 302 to
  // the login route; fetch follows it (on the dev server the final hop is the
  // SPA's own login route) and res.json() would then throw a
  // 'Unexpected token <' HTML parse error. Detect ANY redirect that lands
  // under the admin prefix as an auth failure.
  if (res.redirected && new URL(res.url).pathname.startsWith(`${adminRoot}/`)) {
    handleAuthFailure("Session expired");
  }

  if (res.status === 401) {
    handleAuthFailure("Unauthorized");
  }

  if (!res.ok) {
    // Admin endpoints emit one envelope ({ok,message[,code]}); surface the
    // human message instead of a raw JSON blob. Fall back to status text.
    const text = await res.text().catch(() => "");
    let msg;
    try {
      const parsed = JSON.parse(text);
      msg = parsed?.message ?? "";
    } catch {
      msg = text;
    }
    throw new Error(msg || `HTTP ${res.status}`);
  }

  return res.json();
}

/**
 * POST JSON to an admin API endpoint.
 * @param {string} path
 * @param {any} body
 * @returns {Promise<any>}
 */
export async function postAPI(path, body) {
  return fetchAPI(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: body != null ? JSON.stringify(body) : undefined,
  });
}

/**
 * POST form data to an admin endpoint.
 * @param {string} path
 * @param {Record<string, string>} fields
 * @returns {Promise<Response>} Raw response (login/config use non-JSON responses)
 */
export async function postForm(path, fields) {
  return fetch(path, {
    method: "POST",
    headers: {
      "Content-Type": "application/x-www-form-urlencoded",
      ...csrfHeader("POST"),
    },
    body: new URLSearchParams(fields),
  });
}
