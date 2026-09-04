/**
 * Central endpoint map for the freebuff-proxy dashboard SPA.
 *
 * Every admin route this app talks to is declared here once. Components
 * import from this file instead of hardcoding paths, so a path or method
 * change is a single edit here. Paths are full literals (never composed),
 * so the server-side route-parity check can scan this file and verify each
 * endpoint is registered by the gateway.
 */

/** URL prefix of the dashboard SPA and its admin API. */
export const adminRoot = "/admin";

/** JSON admin API endpoints (GET). */
export const adminApi = {
  overview: "/admin/api/overview",
  tokens: "/admin/api/tokens",
  models: "/admin/api/models",
  config: "/admin/api/config",
  configMeta: "/admin/api/config/meta",
  logs: "/admin/api/logs",
  metrics: "/admin/api/metrics",
  traces: "/admin/api/traces",
  setup: "/admin/api/setup",
  version: "/admin/api/version",
  authStatus: "/admin/api/auth/status",
  changePassword: "/admin/api/change-password",
  requireLogin: "/admin/api/require-login",
  notices: "/admin/api/notices",
  upstreamDrift: "/admin/api/upstream-drift",
  events: "/admin/api/events",
  loginStatus: "/admin/login/status",
};

/**
 * Admin mutation endpoints (POST). GET /admin/login is the SPA's own route
 * (Login.svelte), not a JSON endpoint; /admin/login/status is a GET poll and
 * lives in adminApi above.
 */
export const adminActions = {
  login: "/admin/login",
  logout: "/admin/logout",
  loginStart: "/admin/login/start",
  mode: "/admin/mode",
  smoke: "/admin/smoke",
  diag: "/admin/diag",
  configSave: "/admin/config",
  tokenAdd: "/admin/tokens/add",
  tokenRemove: "/admin/tokens/remove",
  tokenSwap: "/admin/tokens/swap",
  tokenTestAll: "/admin/tokens/test-all",
  restart: "/admin/restart",
};

/** SPA shell routes (the gateway also serves these directly). */
export const adminShell = {
  root: "/admin",
  playground: "/admin/playground",
};

/**
 * Per-token action endpoints: /admin/tokens/{idx}/{action}.
 * Actions: unlock, unlock-lock, lock, finish, test, session.
 */
export const tokenActions = {
  unlock: (idx) => `/admin/tokens/${idx}/unlock`,
  unlockLock: (idx) => `/admin/tokens/${idx}/unlock-lock`,
  lock: (idx) => `/admin/tokens/${idx}/lock`,
  finish: (idx) => `/admin/tokens/${idx}/finish`,
  dropSession: (idx) => `/admin/tokens/${idx}/drop-session`,
  test: (idx) => `/admin/tokens/${idx}/test`,
  session: (idx) => `/admin/tokens/${idx}/session`,
};
