package dashboard

// Admin wire contract: typed request/response shapes for the admin JSON
// surface plus the OpenAPI registry the emitter consumes.
//
// Rule: every JSON endpoint in admin_manifest.json has exactly one
// AdminAPIPaths row; the row's Request/Response are the zero values of the
// structs the handlers actually encode/decode, so the emitted openapi.json
// cannot drift from the wire. Struct fields stay in JSON-alphabetical order
// where the shape replaced a map[string]any literal, keeping the encoding
// byte-identical to the map it replaced (Go sorts map keys on marshal).
//
//go:generate go run ../../cmd/openapi-emit -manifest admin_manifest.json -out data/openapi.json
//
// Frontend types: npx -y openapi-typescript@7.13.0 backend/internal/dashboard/data/openapi.json -o frontend/src/lib/api/openapi.d.ts
// followed by npx -y prettier@3.9.6 --write frontend/src/lib/api/openapi.d.ts
// (pinned versions keep the check-in byte-reproducible; types file only, no component changes).

import (
	"encoding/json"

	"freebuff-proxy/backend/internal/config"
)

// AdminAPIQuery is one documented query parameter.
type AdminAPIQuery struct {
	Name        string
	Description string
	Required    bool
}

// AdminAPIPath is one admin endpoint's OpenAPI contract. Kind selects the
// response body: "json" (a JSON schema from Request/Response), "sse" (a
// text/event-stream with no JSON schema), or "page" (the HTML SPA shell or
// a static asset, no JSON schema). Request/Response hold zero values of the
// wire structs (unexported dashboard types are fine — the emitter reflects
// over them from inside this package); nil Request means no documented body.
type AdminAPIPath struct {
	Method      string
	Path        string
	OperationID string
	Summary     string
	Auth        string
	Kind        string
	Request     any
	Response    any
	Query       []AdminAPIQuery
}

// Admin API path kinds.
const (
	AdminAPIKindJSON = "json"
	AdminAPIKindPage = "page"
	AdminAPIKindSSE  = "sse"
)

// ResultEnvelope is the single admin wire shape: every admin endpoint ships
// {ok, message} with an optional machine-readable code (alias of the render
// helper's envelope so the registry can name it).
type ResultEnvelope = resultEnvelope

// --- auth ---

// LoginRequest is the POST /admin/login credential (form field or JSON).
type LoginRequest struct {
	Token string `json:"token"`
}

// LoginBusyResponse is the 503 answer when the login surface is saturated.
type LoginBusyResponse struct {
	Error string `json:"error"`
}

// LogoutResponse is the POST /admin/logout answer.
type LogoutResponse struct {
	OK bool `json:"ok"`
}

// AuthStatusResponse is the GET /admin/api/auth/status answer.
type AuthStatusResponse struct {
	Authenticated       bool `json:"authenticated"`
	HasPassword         bool `json:"has_password"`
	IsDefaultAdminToken bool `json:"is_default_admin_token"`
	RequireLogin        bool `json:"require_login"`
}

// ChangePasswordRequest is the POST /admin/api/change-password body.
type ChangePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// ChangePasswordResponse is the change-password success answer.
type ChangePasswordResponse struct {
	Message string `json:"message"`
	OK      bool   `json:"ok"`
}

// RequireLoginRequest is the POST /admin/api/require-login body (either key
// selects the target; require_login wins when both are present).
type RequireLoginRequest struct {
	Enabled      *bool `json:"enabled,omitempty"`
	RequireLogin *bool `json:"require_login,omitempty"`
}

// RequireLoginResponse is the require-login success answer.
type RequireLoginResponse struct {
	Message      string `json:"message"`
	OK           bool   `json:"ok"`
	RequireLogin bool   `json:"require_login"`
}

// LoginStartResponse is the POST /admin/login/start answer: the device-flow
// poll key plus the browser URL.
type LoginStartResponse struct {
	ExpiresAt   string `json:"expires_at"`
	Fingerprint string `json:"fingerprint"`
	FlowID      string `json:"flow_id"`
	LoginURL    string `json:"login_url"`
}

// LoginPendingResponse is the pending login/status answer: status alone.
type LoginPendingResponse struct {
	Status string `json:"status"`
}

// LoginCompletedResponse is the completed login/status answer: it always
// carries token_index (even index 0 — no omitempty) plus the upstream user
// name on the final poll.
type LoginCompletedResponse struct {
	Status     string `json:"status"`
	TokenIndex int    `json:"token_index"`
	User       string `json:"user,omitempty"`
}

// LoginErrorResponse is the login/status answer when persisting the new
// token failed.
type LoginErrorResponse struct {
	Message string `json:"message"`
	Status  string `json:"status"`
}

// --- settings / pages / config ---

// SettingsEntry is one GET /admin/api/settings row: the effective display
// value plus the precedence tier that provides it.
type SettingsEntry struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	Source      string `json:"source"` // env|db|file|default
	RestartOnly bool   `json:"restart_only"`
	Secret      bool   `json:"secret"`
}

// SettingsListResponse is the GET /admin/api/settings answer.
type SettingsListResponse struct {
	Degraded bool               `json:"degraded"`
	Settings []SettingsEntry    `json:"settings"`
	Migrate  *MigrateStatusInfo `json:"migrate,omitempty"`
}

// MigrateStatusInfo is the boot-time smart-migration report carried on the
// settings payload (read-cheap: the store captures it once at Open, the
// handler serves it from memory plus the already-fetched settings rows —
// no per-request migration work). FromVersion is the detected PRAGMA
// user_version stamp before migration (0: fresh init, no DB file); Applied
// lists the goose versions that actually executed that boot (empty on a
// strict no-op re-boot); Marker reports the env-to-DB marker row
// (config:migrated_env_v1) being present; Noop reports zero boot writes
// (goose chain already at latest and the marker already set). Regenerate
// data/openapi.json via go generate ./backend/internal/dashboard/ after
// touching this shape (the emitter inlines it into SettingsListResponse).
type MigrateStatusInfo struct {
	FromVersion int   `json:"from_version"`
	ToVersion   int   `json:"to_version"`
	Applied     []int `json:"applied"`
	Fresh       bool  `json:"fresh"`
	Marker      bool  `json:"marker"`
	Noop        bool  `json:"noop"`
}

// SettingsPostRequest is the POST /admin/api/settings body.
type SettingsPostRequest struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

// SettingsPostResponse is the settings-save answer.
type SettingsPostResponse struct {
	Code        string   `json:"code"`
	Message     string   `json:"message"`
	OK          bool     `json:"ok"`
	RestartOnly []string `json:"restart_only"`
}

// PageStateRequest is the PUT /admin/api/pages/{id} body (data must be a
// JSON object, 64KB cap).
type PageStateRequest struct {
	Data json.RawMessage `json:"data"`
}

// PageStateResponse is the GET /admin/api/pages/{id} answer (absent row
// reads back {"data":{}}).
type PageStateResponse struct {
	Data any `json:"data"`
}

// ConfigSaveResponse is the POST /admin/config answer: restart_only names
// the saved keys that need a restart (absent on plain saves).
type ConfigSaveResponse struct {
	Message     string   `json:"message"`
	OK          bool     `json:"ok"`
	RestartOnly []string `json:"restart_only,omitempty"`
}

// VersionResponse is the GET /admin/api/version answer.
type VersionResponse struct {
	CurrentVersion string `json:"current_version"`
	HasUpdate      bool   `json:"has_update"`
	LatestVersion  string `json:"latest_version"`
	UpdateURL      string `json:"update_url"`
}

// TokenAddRequest is the POST /admin/tokens/add body (raw upstream token).
type TokenAddRequest struct {
	Token string `json:"token"`
}

// TokenSwapRequest is the POST /admin/tokens/swap body: index pairs in any
// of the accepted key shapes (i/j, from/to, index) plus the passthrough
// action/direction strings the route handler forwards.
type TokenSwapRequest struct {
	Action string `json:"action,omitempty"`
	Dir    string `json:"direction,omitempty"`
	From   *int   `json:"from,omitempty"`
	I      *int   `json:"i,omitempty"`
	Idx    *int   `json:"index,omitempty"`
	J      *int   `json:"j,omitempty"`
	To     *int   `json:"to,omitempty"`
}

// TokenRemoveRequest is the POST /admin/tokens/remove body: the pool index
// to drop (either key; absent removes the last token).
type TokenRemoveRequest struct {
	Index *int `json:"index,omitempty"`
	Token *int `json:"token,omitempty"`
}

// SpawnSessionRequest is the POST /admin/tokens/{id}/session body.
type SpawnSessionRequest struct {
	Model string `json:"model,omitempty"`
}

// MaturityUpdateRequest is the POST /admin/tokens/{id}/maturity body
// (absent fields leave that dimension untouched).
type MaturityUpdateRequest struct {
	Enabled    *bool  `json:"enabled,omitempty"`
	Mode       string `json:"mode,omitempty"`
	Target     *int   `json:"target,omitempty"`
	TouchModel string `json:"touch_model,omitempty"`
}

// --- system ---

// ModeSwitchRequest is the POST /admin/mode body.
type ModeSwitchRequest struct {
	Mode string `json:"mode"`
}

// ReloadResponse is the POST /admin/reload answer.
type ReloadResponse struct {
	AuthTokens int    `json:"auth_tokens"`
	Message    string `json:"message"`
	Status     string `json:"status"`
}

// RestartResponse is the POST /admin/restart answer.
type RestartResponse struct {
	Message string `json:"message"`
	OK      bool   `json:"ok"`
}

// DiagResponse is the POST /admin/diag answer.
type DiagResponse struct {
	Checks []DiagCheck `json:"checks"`
}

// SmokeRequest is the POST /admin/smoke body (bridge mode relays Token).
type SmokeRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Token  string `json:"token"`
}

// SmokeResponse is the POST /admin/smoke answer (zero-cost upstream probe
// with a bounded SSE preview).
type SmokeResponse struct {
	Model   string    `json:"model"`
	Ms      int64     `json:"ms"`
	OK      bool      `json:"ok"`
	Phases  []PhaseKV `json:"phases"`
	Preview string    `json:"preview"`
	Token   string    `json:"token"`
}

// SmokeDisabledResponse is the POST /admin/smoke 404 answer when the
// DEVTOOLS_ENABLED gate is off.
type SmokeDisabledResponse struct {
	Message string `json:"message"`
	OK      bool   `json:"ok"`
}

// PlaygroundRequest is the POST /admin/playground/chat body: a prompt run
// through the real chat handler with streaming forced (SSE answer).
type PlaygroundRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

// AdminAPIPaths is the OpenAPI registry: one row per admin_manifest.json
// entry, in manifest order. The emitter (backend/cmd/openapi-emit) fails on
// any manifest row missing here, so the table and the manifest ship as one
// commit — same invariant as the server route mapper.
func AdminAPIPaths() []AdminAPIPath {
	liveView := []AdminAPIQuery{{Name: "view", Description: "view=live returns the hot-poll subset (live numbers only); absent returns the full shape"}}
	return []AdminAPIPath{
		{Method: "POST", Path: "/admin/reload", OperationID: "reload", Summary: "Reload configuration from disk and apply live (Bearer ADMIN_TOKEN)", Auth: "adminToken", Kind: AdminAPIKindJSON, Response: ReloadResponse{}},
		{Method: "GET", Path: "/admin/login", OperationID: "loginPage", Summary: "Login page shell (HTML)", Auth: "none", Kind: AdminAPIKindPage},
		{Method: "POST", Path: "/admin/login", OperationID: "login", Summary: "Dashboard login (sets the fb_admin session cookie; 302 to /admin on success)", Auth: "none", Kind: AdminAPIKindJSON, Request: LoginRequest{}, Response: ResultEnvelope{}},
		{Method: "POST", Path: "/admin/logout", OperationID: "logout", Summary: "Dashboard logout (clears the fb_admin session cookie)", Auth: "none", Kind: AdminAPIKindJSON, Response: LogoutResponse{}},

		{Method: "GET", Path: "/admin/api/overview", OperationID: "getOverview", Summary: "Overview view model (pool, models, quota, upstream sync)", Auth: "dashboard", Kind: AdminAPIKindJSON, Response: overviewData{}, Query: liveView},
		{Method: "GET", Path: "/admin/api/tokens", OperationID: "getTokens", Summary: "Token cards view model", Auth: "dashboard", Kind: AdminAPIKindJSON, Response: tokensData{}, Query: liveView},
		{Method: "GET", Path: "/admin/api/models", OperationID: "getModels", Summary: "Served-model catalog view model", Auth: "dashboard", Kind: AdminAPIKindJSON, Response: modelsData{}},
		{Method: "GET", Path: "/admin/api/traces", OperationID: "getTraces", Summary: "Recent chat traces", Auth: "dashboard", Kind: AdminAPIKindJSON, Response: tracesData{}},
		{Method: "GET", Path: "/admin/api/setup", OperationID: "getSetup", Summary: "Setup wizard view model", Auth: "dashboard", Kind: AdminAPIKindJSON, Response: setupData{}},
		{Method: "GET", Path: "/admin/api/config", OperationID: "getConfig", Summary: "Effective config (sensitive: raw .env read)", Auth: "sensitive", Kind: AdminAPIKindJSON, Response: configData{}},
		{Method: "GET", Path: "/admin/api/config/meta", OperationID: "getConfigMeta", Summary: "Configuration catalog (the settings form schema)", Auth: "dashboard", Kind: AdminAPIKindJSON, Response: []config.KeyDef{}},
		{Method: "GET", Path: "/admin/api/settings", OperationID: "getSettings", Summary: "DB settings overlay: effective values plus source tiers", Auth: "dashboard", Kind: AdminAPIKindJSON, Response: SettingsListResponse{}},
		{Method: "POST", Path: "/admin/api/settings", OperationID: "saveSetting", Summary: "Validate, persist and hot-apply one overlay knob", Auth: "sensitive", Kind: AdminAPIKindJSON, Request: SettingsPostRequest{}, Response: SettingsPostResponse{}},
		{Method: "DELETE", Path: "/admin/api/settings/{key}", OperationID: "resetSetting", Summary: "Drop one overlay row (falls back to .env/env/default)", Auth: "sensitive", Kind: AdminAPIKindJSON, Response: ResultEnvelope{}},
		{Method: "GET", Path: "/admin/api/pages/{id}", OperationID: "getPageState", Summary: "Per-page UI snapshot (absent row reads back empty, never 404)", Auth: "dashboard", Kind: AdminAPIKindJSON, Response: PageStateResponse{}},
		{Method: "PUT", Path: "/admin/api/pages/{id}", OperationID: "putPageState", Summary: "Upsert one per-page UI snapshot (data must be a JSON object)", Auth: "sensitive", Kind: AdminAPIKindJSON, Request: PageStateRequest{}, Response: ResultEnvelope{}},
		{Method: "GET", Path: "/admin/api/logs", OperationID: "getLogs", Summary: "Recent log ring view model", Auth: "sensitive", Kind: AdminAPIKindJSON, Response: logsData{}, Query: []AdminAPIQuery{{Name: "level", Description: "Log level filter"}, {Name: "msg", Description: "Message substring filter"}}},
		{Method: "GET", Path: "/admin/api/quota/history", OperationID: "getQuotaHistory", Summary: "Per-model quota samples", Auth: "dashboard", Kind: AdminAPIKindJSON, Response: quotaHistoryData{}, Query: []AdminAPIQuery{{Name: "token", Description: "Pool token index (-1 disables)", Required: true}, {Name: "model", Description: "Model id", Required: true}, {Name: "since", Description: "Unix-millis lower bound"}, {Name: "limit", Description: "Max samples (default 500)"}}},
		{Method: "GET", Path: "/admin/api/maturity/history", OperationID: "getMaturityHistory", Summary: "Streak/standing transitions", Auth: "dashboard", Kind: AdminAPIKindJSON, Response: maturityHistoryData{}, Query: []AdminAPIQuery{{Name: "token", Description: "Pool token index", Required: true}, {Name: "since", Description: "Unix-millis lower bound"}, {Name: "limit", Description: "Max events (default 200)"}}},
		{Method: "GET", Path: "/admin/api/logs/history", OperationID: "getLogsHistory", Summary: "Persisted log records", Auth: "sensitive", Kind: AdminAPIKindJSON, Response: logsHistoryData{}, Query: []AdminAPIQuery{{Name: "level", Description: "Log level filter"}, {Name: "msg", Description: "Message substring filter"}, {Name: "req_id", Description: "Request id filter"}, {Name: "since", Description: "Unix-millis lower bound"}, {Name: "until", Description: "Unix-millis upper bound"}, {Name: "limit", Description: "Max records (default 500)"}}},
		{Method: "GET", Path: "/admin/api/metrics", OperationID: "getMetrics", Summary: "Latency and throughput view model", Auth: "dashboard", Kind: AdminAPIKindJSON, Response: metricsData{}},
		{Method: "GET", Path: "/admin/api/version", OperationID: "getVersion", Summary: "Running version plus update check", Auth: "dashboard", Kind: AdminAPIKindJSON, Response: VersionResponse{}, Query: []AdminAPIQuery{{Name: "force", Description: "force=true invalidates the cached update check"}}},
		{Method: "GET", Path: "/admin/api/events", OperationID: "events", Summary: "Server-sent event stream (dashboard live updates)", Auth: "dashboard", Kind: AdminAPIKindSSE},
		{Method: "GET", Path: "/admin/api/auth/status", OperationID: "getAuthStatus", Summary: "Dashboard auth state (login mode, factory-default check)", Auth: "dashboard", Kind: AdminAPIKindJSON, Response: AuthStatusResponse{}},
		{Method: "GET", Path: "/admin/api/notices", OperationID: "getNotices", Summary: "Upstream announcements and live broadcasts", Auth: "dashboard", Kind: AdminAPIKindJSON, Response: NoticesResponse{}},

		{Method: "GET", Path: "/admin", OperationID: "spaRoot", Summary: "SPA shell (HTML)", Auth: "dashboard", Kind: AdminAPIKindPage},
		{Method: "GET", Path: "/admin/", OperationID: "spaRootSlash", Summary: "SPA shell (HTML)", Auth: "dashboard", Kind: AdminAPIKindPage},
		{Method: "GET", Path: "/admin/tokens", OperationID: "spaTokens", Summary: "SPA shell (HTML)", Auth: "dashboard", Kind: AdminAPIKindPage},
		{Method: "GET", Path: "/admin/models", OperationID: "spaModels", Summary: "SPA shell (HTML)", Auth: "dashboard", Kind: AdminAPIKindPage},
		{Method: "GET", Path: "/admin/traces", OperationID: "spaTraces", Summary: "SPA shell (HTML)", Auth: "dashboard", Kind: AdminAPIKindPage},
		{Method: "GET", Path: "/admin/setup", OperationID: "spaSetup", Summary: "SPA shell (HTML)", Auth: "dashboard", Kind: AdminAPIKindPage},
		{Method: "GET", Path: "/admin/playground", OperationID: "spaPlayground", Summary: "SPA shell (HTML)", Auth: "dashboard", Kind: AdminAPIKindPage},
		{Method: "GET", Path: "/admin/config", OperationID: "spaConfig", Summary: "SPA shell (HTML)", Auth: "sensitive", Kind: AdminAPIKindPage},
		{Method: "GET", Path: "/admin/logs", OperationID: "spaLogs", Summary: "SPA shell (HTML)", Auth: "sensitive", Kind: AdminAPIKindPage},
		{Method: "GET", Path: "/admin/metrics", OperationID: "spaMetrics", Summary: "SPA shell (HTML)", Auth: "dashboard", Kind: AdminAPIKindPage},

		{Method: "POST", Path: "/admin/playground/chat", OperationID: "playgroundChat", Summary: "Prompt through the real chat handler with streaming forced (SSE answer)", Auth: "sensitive", Kind: AdminAPIKindSSE, Request: PlaygroundRequest{}},
		{Method: "POST", Path: "/admin/login/start", OperationID: "loginStart", Summary: "Start a device-login browser flow (?isolated=false for the stable fingerprint)", Auth: "sensitive", Kind: AdminAPIKindJSON, Response: LoginStartResponse{}, Query: []AdminAPIQuery{{Name: "isolated", Description: "isolated=false requests the stable machine fingerprint"}}},
		{Method: "GET", Path: "/admin/login/status", OperationID: "loginStatus", Summary: "Poll one device-login flow (pending: status alone; completed: token_index plus user)", Auth: "sensitive", Kind: AdminAPIKindJSON, Response: LoginCompletedResponse{}, Query: []AdminAPIQuery{{Name: "fingerprint", Description: "Full fingerprint id from login/start", Required: true}}},
		{Method: "POST", Path: "/admin/config", OperationID: "saveConfig", Summary: "Write the raw .env text, reload and report restart-only/overridden keys", Auth: "sensitive", Kind: AdminAPIKindJSON, Request: "", Response: ConfigSaveResponse{}},
		{Method: "POST", Path: "/admin/tokens/{id}/unlock", OperationID: "tokenUnlock", Summary: "Return one token to rotation", Auth: "sensitive", Kind: AdminAPIKindJSON, Response: ResultEnvelope{}},
		{Method: "POST", Path: "/admin/tokens/{id}/lock", OperationID: "tokenLock", Summary: "Take one token out of rotation", Auth: "sensitive", Kind: AdminAPIKindJSON, Response: ResultEnvelope{}},
		{Method: "POST", Path: "/admin/tokens/{id}/unlock-lock", OperationID: "tokenUnlockLock", Summary: "Unlock then immediately re-lock (cooldown reset)", Auth: "sensitive", Kind: AdminAPIKindJSON, Response: ResultEnvelope{}},
		{Method: "POST", Path: "/admin/tokens/{id}/maturity", OperationID: "tokenMaturity", Summary: "Set per-token streak-maturity automation", Auth: "sensitive", Kind: AdminAPIKindJSON, Request: MaturityUpdateRequest{}, Response: ResultEnvelope{}},
		{Method: "POST", Path: "/admin/tokens/{id}/maturity/touch", OperationID: "tokenMaturityTouch", Summary: "Fire one manual maturity touch outside the daily slot", Auth: "sensitive", Kind: AdminAPIKindJSON, Response: ResultEnvelope{}},
		{Method: "POST", Path: "/admin/tokens/{id}/maturity/warn-reset", OperationID: "tokenMaturityWarnReset", Summary: "Clear one token's non-advance warning (re-arms the daily loop)", Auth: "sensitive", Kind: AdminAPIKindJSON, Response: ResultEnvelope{}},
		{Method: "POST", Path: "/admin/bridge-tokens/{key}/lock", OperationID: "bridgeTokenLock", Summary: "Lock one bridge-mode entry", Auth: "sensitive", Kind: AdminAPIKindJSON, Response: ResultEnvelope{}},
		{Method: "POST", Path: "/admin/bridge-tokens/{key}/unlock", OperationID: "bridgeTokenUnlock", Summary: "Unlock one bridge-mode entry", Auth: "sensitive", Kind: AdminAPIKindJSON, Response: ResultEnvelope{}},
		{Method: "POST", Path: "/admin/tokens/{id}/finish", OperationID: "tokenFinish", Summary: "FINISH one token's upstream runs", Auth: "sensitive", Kind: AdminAPIKindJSON, Response: ResultEnvelope{}},
		{Method: "POST", Path: "/admin/tokens/{id}/drop-session", OperationID: "tokenDropSession", Summary: "Drop one token's upstream session", Auth: "sensitive", Kind: AdminAPIKindJSON, Response: ResultEnvelope{}},
		{Method: "POST", Path: "/admin/tokens/{id}/test", OperationID: "tokenTest", Summary: "Zero-cost upstream probe of one token", Auth: "sensitive", Kind: AdminAPIKindJSON, Response: ResultEnvelope{}},
		{Method: "POST", Path: "/admin/tokens/{id}/session", OperationID: "tokenSpawnSession", Summary: "Ensure one token's upstream session for a model", Auth: "sensitive", Kind: AdminAPIKindJSON, Request: SpawnSessionRequest{}, Response: ResultEnvelope{}},
		{Method: "POST", Path: "/admin/tokens/test-all", OperationID: "tokenTestAll", Summary: "Probe every pooled token (?auto=1 returns the throttled snapshot)", Auth: "sensitive", Kind: AdminAPIKindJSON, Response: []TokenTestOutcome{}, Query: []AdminAPIQuery{{Name: "auto", Description: "auto=1 serves the throttled quota snapshot without probing"}}},
		{Method: "POST", Path: "/admin/tokens/add", OperationID: "tokenAdd", Summary: "Add one upstream token to the pool and persist to .env", Auth: "sensitive", Kind: AdminAPIKindJSON, Request: TokenAddRequest{}, Response: ResultEnvelope{}},
		{Method: "POST", Path: "/admin/tokens/remove", OperationID: "tokenRemove", Summary: "Remove one pool token (absent index removes the last)", Auth: "sensitive", Kind: AdminAPIKindJSON, Request: TokenRemoveRequest{}, Response: ResultEnvelope{}},
		{Method: "POST", Path: "/admin/tokens/swap", OperationID: "tokenSwap", Summary: "Swap two pool positions", Auth: "sensitive", Kind: AdminAPIKindJSON, Request: TokenSwapRequest{}, Response: ResultEnvelope{}},
		{Method: "POST", Path: "/admin/mode", OperationID: "modeSwitch", Summary: "Switch bridge/pooled mode (loopback rules apply)", Auth: "sensitive", Kind: AdminAPIKindJSON, Request: ModeSwitchRequest{}, Response: ResultEnvelope{}},
		{Method: "POST", Path: "/admin/diag", OperationID: "diag", Summary: "Configuration and upstream reachability checks", Auth: "sensitive", Kind: AdminAPIKindJSON, Response: DiagResponse{}},
		{Method: "POST", Path: "/admin/api/change-password", OperationID: "changePassword", Summary: "Rotate the admin dashboard password", Auth: "sensitive", Kind: AdminAPIKindJSON, Request: ChangePasswordRequest{}, Response: ChangePasswordResponse{}},
		{Method: "POST", Path: "/admin/api/require-login", OperationID: "requireLogin", Summary: "Enable/disable dashboard login (open mode is loopback-only)", Auth: "sensitive", Kind: AdminAPIKindJSON, Request: RequireLoginRequest{}, Response: RequireLoginResponse{}},
		{Method: "POST", Path: "/admin/smoke", OperationID: "smoke", Summary: "Zero-cost upstream probe with SSE preview (DEVTOOLS_ENABLED gate)", Auth: "sensitive", Kind: AdminAPIKindJSON, Request: SmokeRequest{}, Response: SmokeResponse{}},
		{Method: "POST", Path: "/admin/restart", OperationID: "restart", Summary: "Validate config and restart the gateway process", Auth: "sensitive", Kind: AdminAPIKindJSON, Response: RestartResponse{}},
		{Method: "GET", Path: "/admin/assets/", OperationID: "assets", Summary: "Static dashboard assets", Auth: "none", Kind: AdminAPIKindPage},
	}
}
