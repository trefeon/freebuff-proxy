package dashboard

import (
	_ "embed"
	"encoding/json"
)

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
//
//go:embed admin_manifest.json
var adminManifestRaw []byte

// AdminRoutes is the single enumeration of the admin HTTP surface, loaded
// from the embedded manifest (issue #285): the manifest is the only source
// of the method/path/auth contract and every other representation (the SPA
// paths.js map, the dashboard dataFor switch, the server registration)
// must agree with it — the parity test fails on disagreement.
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
var AdminRoutes = mustAdminRoutes()

// manifestRow is one raw manifest row.
type manifestRow struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Auth   string `json:"auth"`
	JS     *struct {
		Export string `json:"export"`
		Key    string `json:"key"`
	} `json:"js"`
}

// AdminManifestEntry is one manifest row plus its SPA client binding: the
// JS export map (adminApi / adminActions / adminShell / tokenActions) and
// the member key the SPA addresses this endpoint by ("" when the SPA has
// no client entry for the row).
type AdminManifestEntry struct {
	AdminRoute
	JSExport string
	JSKey    string
}

// ParseAdminManifest decodes the embedded manifest. Exported for tests and
// for tooling that needs the same contract the server registers.
func ParseAdminManifest() ([]AdminRoute, error) {
	entries, err := ParseAdminManifestEntries()
	if err != nil {
		return nil, err
	}
	out := make([]AdminRoute, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.AdminRoute)
	}
	return out, nil
}

// ParseAdminManifestEntries decodes the manifest WITH the SPA client
// bindings (the parity test consumes these).
func ParseAdminManifestEntries() ([]AdminManifestEntry, error) {
	var rows []manifestRow
	if err := json.Unmarshal(adminManifestRaw, &rows); err != nil {
		return nil, err
	}
	out := make([]AdminManifestEntry, 0, len(rows))
	for _, row := range rows {
		e := AdminManifestEntry{
			AdminRoute: AdminRoute{Method: row.Method, Path: row.Path, Auth: row.Auth},
		}
		if row.JS != nil {
			e.JSExport = row.JS.Export
			e.JSKey = row.JS.Key
		}
		out = append(out, e)
	}
	return out, nil
}

func mustAdminRoutes() []AdminRoute {
	routes, err := ParseAdminManifest()
	if err != nil {
		panic("dashboard: invalid embedded admin manifest: " + err.Error())
	}
	return routes
}
