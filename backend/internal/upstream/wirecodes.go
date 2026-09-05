// The upstream body-code vocabulary as typed constants. Every refusal code
// the classification matrix (classifyError) and the ban/country-block
// parsers match on is defined once here as a WireCode. classifyError matches
// on the constants, so the wire vocabulary lives in one place.
package upstream

// WireCode is an upstream refusal/body marker. It is a string, so it is
// directly comparable and usable anywhere a body marker is matched.
type WireCode string

const (
	// WireCodeDeploymentOutsideHours: free tier is outside operating hours.
	WireCodeDeploymentOutsideHours WireCode = "deployment_outside_hours"
	// WireCodeFreeModeRunFanout: the account's concurrent-run counter looked
	// like proxy fanout. Also the RateLimitError.Status for the refusal.
	WireCodeFreeModeRunFanout WireCode = "free_mode_run_fanout"
	// WireCodeFreeModeCapacityDeferred: free-tier transient capacity queue.
	WireCodeFreeModeCapacityDeferred WireCode = "free_mode_capacity_deferred"
	// WireCodeSessionLimitReached: the ACCOUNT is over its concurrent-tab
	// budget (409).
	WireCodeSessionLimitReached WireCode = "session_limit_reached"
	// WireCodeFreeModeCLIRequired: the request lacked the CLI envelope.
	WireCodeFreeModeCLIRequired WireCode = "free_mode_cli_required"
	// WireCodeCountryBlocked: free mode not available from the egress region.
	WireCodeCountryBlocked WireCode = "country_blocked"
	// WireCodeIpCapped: too many distinct users on the egress IP.
	WireCodeIpCapped WireCode = "ip_capped"
	// WireCodeWaitingRoomQueued: transient admission race.
	WireCodeWaitingRoomQueued WireCode = "waiting_room_queued"
	// WireCodeWaitingRoomRequired: the account must walk the pre-session
	// ad-chain + streak flow (428).
	WireCodeWaitingRoomRequired WireCode = "waiting_room_required"
	// WireCodeSessionModelMismatch: the session row is bound to a different
	// model (with a "limited" marker for the egress-IP case).
	WireCodeSessionModelMismatch WireCode = "session_model_mismatch"
	// WireCodeFreeModeInvalidAgentModel: the (agent, model) pair is not in
	// the allowlist. Also the RateLimitError.Status for the refusal.
	WireCodeFreeModeInvalidAgentModel WireCode = "free_mode_invalid_agent_model"
	// WireCodeSessionSuperseded: another instance took over the account (409).
	WireCodeSessionSuperseded WireCode = "session_superseded"
	// WireCodeTurnSpendLimit: upstream killed a runaway turn (429
	// per-turn spend ceiling, usually a stuck agent loop).
	WireCodeTurnSpendLimit WireCode = "turn_spend_limit"
	// WireCodeFreebuffUpdateRequired: the CLI app version is out of date.
	WireCodeFreebuffUpdateRequired WireCode = "freebuff_update_required"
	// WireCodeSessionExpired: the free session has expired.
	WireCodeSessionExpired WireCode = "session_expired"
	// WireCodeModelLocked: the model is locked for this account.
	WireCodeModelLocked WireCode = "model_locked"
	// WireCodeFreeModeLegacyLunaAgent: the legacy Luna agent id was used.
	WireCodeFreeModeLegacyLunaAgent WireCode = "free_mode_legacy_luna_agent"
	// WireCodeFreeModeLegacyLuna: the legacy Luna id was used.
	WireCodeFreeModeLegacyLuna WireCode = "free_mode_legacy_luna"
	// WireCodeRunIDNotFound: the run id is gone.
	WireCodeRunIDNotFound WireCode = "runid not found"
	// WireCodeRunIDNotRunning: the run is not running.
	WireCodeRunIDNotRunning WireCode = "runid not running"
	// WireCodeInsufficientQuota: upstream load saturation (the body marker
	// for a load-shedding refusal).
	WireCodeInsufficientQuota WireCode = "insufficient_quota"
	// WireCodeLimitBurstRate: burst-rate cap hit (another load-shedding
	// body marker).
	WireCodeLimitBurstRate WireCode = "limit_burst_rate"
	// WireCodePeakHours: peak-hours cap ("peak hours" body marker).
	WireCodePeakHours WireCode = "peak hours"
	// WireCodePeakHoursStatus is the RateLimitError.Status for the
	// peak-hours refusal (underscore form).
	WireCodePeakHoursStatus WireCode = "peak_hours"
	// WireCodeLoadShedding is the RateLimitError.Status for a load-shedding
	// refusal (the body markers are insufficient_quota / limit_burst_rate).
	WireCodeLoadShedding WireCode = "load_shedding"
	// WireCodeRateLimited: generic rate limit.
	WireCodeRateLimited WireCode = "rate_limited"
	// WireCodeSpendLimited: spend ceiling reached.
	WireCodeSpendLimited WireCode = "spend_limited"
	// WireCodeBanned: the account is temporarily banned (the canonical
	// {"status":"banned"} marker).
	WireCodeBanned WireCode = "banned"
	// WireCodeAccountSuspended: hard-ban shape
	// ({"error":"account_suspended",...}).
	WireCodeAccountSuspended WireCode = "account_suspended"
)
