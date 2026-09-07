package modelcat

import (
	"time"
)

// DefaultModelID mirrors upstream DEFAULT_FREEBUFF_MODEL_ID, pinned to
// FREEBUFF_MODELS[0] (GLM 5.3 Flash leads the picker again as of 2026-09-05,
// retaking the lead from DeepSeek V4 Flash 09-02→09-05; see upstream
// freebuff-models.ts's note on the move). It is what the upstream CLI
// resolves a blank model pick to, and what paused-model refusals recommend.
const DefaultModelID = "z-ai/glm-5.3-flash"

// FallbackModelID mirrors upstream FALLBACK_FREEBUFF_MODEL_ID: the model
// guaranteed available on EVERY tier that unavailable picks are coerced to.
// mimo is region-universal — premium-pool models (luna) only
// resolve on full-tier accounts, so any proxy-side "no model" default must
// be mimo, never the premium picker lead.
const FallbackModelID = "mimo/mimo-v2.5"

// LimitedModelID mirrors upstream LIMITED_FREEBUFF_MODEL_ID: the model
// guaranteed available on the limited tier.
const LimitedModelID = "mimo/mimo-v2.5"

// IsLimitedTierAllowed reports whether the model is available on the limited tier
// without requiring special referral grants.
func IsLimitedTierAllowed(id string) bool {
	return id == LimitedModelID
}

// Glm52ModelID is the referral-reward model, metered by its own promo pool
// rather than the shared premium pool.
const Glm52ModelID = "z-ai/glm-5.2"

// Glm53ModelID is an UNMETERED standard row since 2026-08-28 (left the shared
// daily premium pool: it bills $0.000249/msg via the Merge lane, cheaper than
// any other served row, so it joins MiMo and DeepSeek V4 Flash with no
// ceiling) AND the proxy default since 2026-08-30 (vendor moved the model to
// the picker lead — always available, unmetered, cheapest row). Laddered
// 2026-08-30 as ['low','high','max'] (mediumless; upstream defaultEffort
// 'max'). The premium flag must always agree with the upstream
// FREEBUFF_PREMIUM_MODEL_IDS list (isFreebuffPremiumModelId and the
// FREEBUFF_STANDARD_MODEL_IDS derivation disagree if not).
const Glm53ModelID = "z-ai/glm-5.3-flash"

// SolarPro4ModelID mirrors FREEBUFF_SOLAR_PRO_4_MODEL_ID: the Upstage row,
// UNMETERED since 2026-09-04 (entitlement fullAccess.premium=false;
// "unmetered at full access" per the availability copy). Previously shared
// premium pool; the per-model count cap closed 2026-09-01 (upstream
// 051fd4d9) and pool metering followed it out.
const SolarPro4ModelID = "upstage/solar-pro4"

// PremiumSessionLimit mirrors upstream FREEBUFF_PREMIUM_SESSION_LIMIT: the
// base premium sessions per Pacific day (5; back to 5 on 2026-09-07, the day
// Levels were retired upstream — the 5 → 4 retune existed so a Level could
// add the difference back, and the pre-Levels revert constants are deleted).
// Moot for a metered account (Freebucks is the meter); rollback-safety
// value, not a live limit.
const PremiumSessionLimit = 5

// GLMSessionLength mirrors upstream FREEBUFF_GLM_V52_SESSION_LENGTH_MS: GLM
// sessions are exactly one hour of wall-clock time, regardless of the global
// free-session length.
const GLMSessionLength = time.Hour

// DefaultContextWindow mirrors upstream FREEBUFF_DEFAULT_CONTEXT_WINDOW:
// assumed for any model absent from FREEBUFF_MODEL_CONTEXT_WINDOWS.
const DefaultContextWindow = 128*1024 + 1024 // 131_072
