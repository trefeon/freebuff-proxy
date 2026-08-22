/**
 * Core API fetch client for freebuff-proxy dashboard.
 * Handles HTTP requests, headers, and error unwrapping.
 *
 * Auth failures (401 / redirect to /admin/login) NEVER navigate: they raise
 * SessionExpiredError and set the global session-expired flag
 * (lib/stores/session.js) so App.svelte can show the re-login banner.
 * Background polling must never reload the page (issue #197).
 */

import { markSessionExpired } from '../stores/session.js';

/** Thrown by fetchAPI/postAPI when the admin session is no longer valid. */
export class SessionExpiredError extends Error {
  constructor(message = 'Session expired') {
    super(message);
    this.name = 'SessionExpiredError';
  }
}

function handleAuthFailure(message) {
  markSessionExpired();
  throw new SessionExpiredError(message);
}

/**
 * Fetch JSON from an admin API endpoint. On auth failure, sets the global
 * session-expired flag and throws SessionExpiredError (no page navigation).
 * @param {string} path - API path (e.g. '/admin/api/overview')
 * @param {RequestInit} [opts] - Additional fetch options
 * @returns {Promise<any>} Parsed JSON response
 * @throws {SessionExpiredError} When the session is no longer valid
 * @throws {Error} On non-OK status or network failure
 */
export async function fetchAPI(path, opts = {}) {
  const res = await fetch(path, {
    ...opts,
    headers: {
      'Accept': 'application/json',
      'X-Requested-With': 'fetch',
      ...opts.headers,
    },
  });

  // dashboardAuth answers unauthenticated admin API requests with a 302 to
  // /admin/login; fetch follows it and res.json() would then throw a
  // 'Unexpected token <' HTML parse error. Detect the redirect explicitly.
  if (res.redirected && new URL(res.url).pathname.endsWith('/admin/login')) {
    handleAuthFailure('Session expired');
  }

  if (res.status === 401) {
    handleAuthFailure('Unauthorized');
  }

  if (!res.ok) {
    const text = await res.text().catch(() => '');
    throw new Error(text || `HTTP ${res.status}`);
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
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
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
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams(fields),
  });
}
