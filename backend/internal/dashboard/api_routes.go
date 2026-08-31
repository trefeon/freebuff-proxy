package dashboard

// AdminRoute is one row of the admin route table: the HTTP method and the
// Go 1.22 mux path pattern the gateway registers, plus the auth level the
// server wraps around its handler. Path may carry single-segment wildcards
// ({id}, {key}).
type AdminRoute struct {
	Method string
	Path   string
	Auth   string
}

// Auth levels select the middleware stacks the server has always used.
// POST rows are additionally wired through the CSRF gate (adminCSRF): every
// state-changing admin route carried it before this table existed.
const (
	// AuthNone: no auth wrapper (login page, logout, static assets).
	AuthNone = "none"
	// AuthDashboard: session-cookie gate (dashboardAuth); open when
	// ADMIN_TOKEN is unset.
	AuthDashboard = "dashboard"
	// AuthSensitive: dashboardAuth + adminSensitive (loopback-only while
	// the deployment is effectively unauthenticated).
	AuthSensitive = "sensitive"
	// AuthAdminToken: requireAdminToken + adminSensitive — Bearer
	// ADMIN_TOKEN gate for automation endpoints (/admin/reload).
	AuthAdminToken = "adminToken"
)

// AdminRoutes is the single enumeration of the admin HTTP surface. The
// server registers every row from here (server.registerAdminRoutes); the
// SPA endpoint map (frontend/src/lib/api/paths.js) is checked against it
// by TestAdminRoutesParity, so a route added here without a client or
// registered in the SPA without a gateway row is a test failure.
//
// Auth semantics, matching the wiring this table replaced:
//   - login/logout: no session required (login must be reachable without a
//     cookie; logout must clear expired sessions). POST /admin/login still
//     carries CSRF: a cross-origin POST with wrong tokens would otherwise
//     burn the victim's per-IP login-attempt budget.
//   - assets: public — the login page (served without a cookie) references
//     them, so they must NOT sit behind dashboardAuth.
//   - sensitive: raw .env read/write, logs, token management and
//     automation; loopback-only while ADMIN_TOKEN is unset or still the
//     factory default.
//   - adminToken: /admin/reload is a curl/automation endpoint, so it takes
//     the Bearer ADMIN_TOKEN gate instead of the session cookie.
var AdminRoutes = [...]AdminRoute{
	{Method: "POST", Path: "/admin/reload", Auth: AuthAdminToken},
	{Method: "GET", Path: "/admin/login", Auth: AuthNone},
	{Method: "POST", Path: "/admin/login", Auth: AuthNone},
	{Method: "GET", Path: "/admin/logout", Auth: AuthNone},
	{Method: "POST", Path: "/admin/logout", Auth: AuthNone},

	{Method: "GET", Path: "/admin/api/overview", Auth: AuthDashboard},
	{Method: "GET", Path: "/admin/api/tokens", Auth: AuthDashboard},
	{Method: "GET", Path: "/admin/api/models", Auth: AuthDashboard},
	{Method: "GET", Path: "/admin/api/traces", Auth: AuthDashboard},
	{Method: "GET", Path: "/admin/api/setup", Auth: AuthDashboard},
	{Method: "GET", Path: "/admin/api/config", Auth: AuthSensitive},
	{Method: "GET", Path: "/admin/api/config/meta", Auth: AuthDashboard},
	{Method: "GET", Path: "/admin/api/logs", Auth: AuthSensitive},
	{Method: "GET", Path: "/admin/api/metrics", Auth: AuthDashboard},
	{Method: "GET", Path: "/admin/api/version", Auth: AuthDashboard},
	{Method: "GET", Path: "/admin/api/upstream-drift", Auth: AuthDashboard},
	{Method: "GET", Path: "/admin/api/events", Auth: AuthDashboard},
	{Method: "GET", Path: "/admin/api/auth/status", Auth: AuthDashboard},

	{Method: "GET", Path: "/admin", Auth: AuthDashboard},
	{Method: "GET", Path: "/admin/", Auth: AuthDashboard},
	{Method: "GET", Path: "/admin/tokens", Auth: AuthDashboard},
	{Method: "GET", Path: "/admin/models", Auth: AuthDashboard},
	{Method: "GET", Path: "/admin/traces", Auth: AuthDashboard},
	{Method: "GET", Path: "/admin/setup", Auth: AuthDashboard},
	{Method: "GET", Path: "/admin/playground", Auth: AuthDashboard},
	{Method: "GET", Path: "/admin/config", Auth: AuthSensitive},
	{Method: "GET", Path: "/admin/logs", Auth: AuthSensitive},
	{Method: "GET", Path: "/admin/metrics", Auth: AuthDashboard},

	{Method: "POST", Path: "/admin/playground/chat", Auth: AuthSensitive},
	{Method: "POST", Path: "/admin/login/start", Auth: AuthSensitive},
	{Method: "GET", Path: "/admin/login/status", Auth: AuthSensitive},
	{Method: "POST", Path: "/admin/config", Auth: AuthSensitive},
	{Method: "POST", Path: "/admin/tokens/{id}/unlock", Auth: AuthSensitive},
	{Method: "POST", Path: "/admin/tokens/{id}/lock", Auth: AuthSensitive},
	{Method: "POST", Path: "/admin/tokens/{id}/unlock-lock", Auth: AuthSensitive},
	{Method: "POST", Path: "/admin/bridge-tokens/{key}/lock", Auth: AuthSensitive},
	{Method: "POST", Path: "/admin/bridge-tokens/{key}/unlock", Auth: AuthSensitive},
	{Method: "POST", Path: "/admin/tokens/{id}/finish", Auth: AuthSensitive},
	{Method: "POST", Path: "/admin/tokens/{id}/drop-session", Auth: AuthSensitive},
	{Method: "POST", Path: "/admin/tokens/{id}/test", Auth: AuthSensitive},
	{Method: "POST", Path: "/admin/tokens/{id}/session", Auth: AuthSensitive},
	{Method: "POST", Path: "/admin/tokens/test-all", Auth: AuthSensitive},
	{Method: "POST", Path: "/admin/tokens/add", Auth: AuthSensitive},
	{Method: "POST", Path: "/admin/tokens/remove", Auth: AuthSensitive},
	{Method: "POST", Path: "/admin/tokens/swap", Auth: AuthSensitive},
	{Method: "POST", Path: "/admin/mode", Auth: AuthSensitive},
	{Method: "POST", Path: "/admin/diag", Auth: AuthSensitive},
	{Method: "POST", Path: "/admin/api/change-password", Auth: AuthSensitive},
	{Method: "POST", Path: "/admin/smoke", Auth: AuthSensitive},

	{Method: "GET", Path: "/admin/assets/", Auth: AuthNone},
}
