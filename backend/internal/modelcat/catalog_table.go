// about the FreeBuff free catalog. One row per model in upstream
// SUPPORTED_FREEBUFF_MODELS (reference/freebuff/common/src/constants/
// freebuff-models.ts, pinned snapshot 92c4f5e, vendor 0.0.171).
//
// Every package that needs a per-model fact — registry (served/paused gate,
// withdrawn-model copy), convert (effort ladders), pool (premium pool and
// per-model caps), upstream (mock session quotas), server (default model,
// /v1/models rows) — reads it from here instead of maintaining its own
// table. An upstream sync is one table edit below plus a rerun of the
// catalog parity test (catalog_test.go), which re-reads the pinned snapshot
// and fails on drift. Agent-root maps (FREE_MODE_AGENT_MODELS,
// FREEBUFF_ROOT_AGENT_ID_BY_MODEL) are wire-level agent assignments, not
// model facts, and stay in backend/internal/registry under their own parity test.
package modelcat

// ModelInfo describes one catalog model and every proxy fact about it.
type ModelInfo struct {
	// ID is the wire model id (provider/model).
	ID string
	// DisplayName is the upstream catalog display name (used in the
	// withdrawn-model refusal copy, mirroring freebuffWithdrawnModelMessage).
	DisplayName string
	// Served gates /v1/models and the chat handlers: the ids this gateway
	// serves or advertises. Paused models are never Served.
	Served bool
	// PausedReplacement is non-empty exactly when upstream
	// FREEBUFF_PAUSED_FREE_MODEL_IDS lists the model: recognized but
	// admission-refused. It names the model the refusal copy recommends.
	PausedReplacement string
	// Premium marks FREEBUFF_PREMIUM_MODEL_IDS membership (the shared daily
	// premium pool). Fable 5 is premium-flagged upstream but metered by its
	// own global pool (FREEBUFF_LIMITED_OFFER_MODEL_IDS), not the shared
	// pool, so it is NOT marked Premium here.
	Premium bool
	// Cap is the FREEBUFF_PER_MODEL_SESSION_CAPS daily ceiling (0 = none).
	Cap int
	// CapPool is the upstream pool id for Cap ("" when uncapped).
	CapPool string
	// ContextWindow mirrors FREEBUFF_MODEL_CONTEXT_WINDOWS in tokens; 0
	// means upstream falls back to DefaultContextWindow.
	ContextWindow int
	// Efforts is the upstream reasoning-effort ladder (nil = route accepts
	// and ignores reasoning_effort, so conversion uses the default ladder).
	Efforts []string
	// Tagline is the upstream catalog description (used in picker & catalog).
	Tagline string
	// Notice is the upstream warning or special offer (e.g. AI training, promo).
	Notice string
	// Badges are the capability/freshness chips (e.g. Reasoning: max*, Images, NEW).
	Badges []string
}

// Catalog is the full free-catalog table, in upstream SUPPORTED_FREEBUFF_MODELS
// order. Re-verify every row against the pinned snapshot on sync; the parity
// test enforces it.
var Catalog = []ModelInfo{
	{ID: "stealth/ox-alpha", DisplayName: "Ox Alpha",
		PausedReplacement: "z-ai/glm-5.3-flash", ContextWindow: 1_000_000,
		Efforts: []string{"low", "high", "max"}},
	{ID: "deepseek/deepseek-v4-pro", DisplayName: "DeepSeek V4 Pro",
		PausedReplacement: "z-ai/glm-5.3-flash", ContextWindow: 1_048_576,
		Efforts: []string{"low", "high", "max"}},
	{ID: "minimax/minimax-m3", DisplayName: "MiniMax M3",
		PausedReplacement: "z-ai/glm-5.3-flash", ContextWindow: 524_288,
		Efforts: []string{"high"}},
	{ID: "openai/gpt-5.6-luna", DisplayName: "GPT-5.6 Luna",
		Tagline: "Strong all-around", Badges: []string{"Reasoning: high", "Images"},
		Served: true, Premium: true, ContextWindow: 1_000_000,
		Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
	{ID: "upstage/solar-pro4", DisplayName: "Solar Pro 4",
		Tagline: "Fast & Direct", Notice: "Labor Day weekend (through Sep 7 PT)",
		Served: true, ContextWindow: 500_000},
	// google/gemini-3.8-flash returned 2026-09-04 behind the Pro paywall,
	// Web-only (upstream FREEBUFF_PRO_ONLY_CATALOG_MODEL_IDS): removed from
	// FREEBUFF_PAUSED_FREE_MODEL_IDS, but NOT back in FREEBUFF_MODELS, so no
	// CLI/Desktop surface may serve it. The proxy has no Pro surface, so the
	// row stays recognized-but-never-served: no PausedReplacement (upstream
	// no longer pauses it), no Served. No context entry upstream; the $0.50
	// per-session spend ceiling is upstream-enforced, so unmodeled.
	{ID: "google/gemini-3.8-flash", DisplayName: "Gemini 3.8 Flash",
		Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
	// meta/muse-spark-1.3-contributor joined every surface 2026-09-04 and
	// was WITHDRAWN 2026-09-07: Meta returns `404 model_not_found` for it on
	// every key, every attempt, while 1.2 answers (see its FREEBUFF_MODELS
	// row). Paused rather than deleted so the installed binaries that hold
	// the id are coerced instead of refused. Keeps its frozen row facts
	// (display name, ladder, 1M window) for the refusal copy; Served and
	// Premium are off — a paused row consumes no pool.
	{ID: "meta/muse-spark-1.3-contributor", DisplayName: "Muse Spark 1.3",
		Tagline: "Queues, then falls back", Badges: []string{"Reasoning: xhigh", "NEW"},
		Notice:            "May use data for AI training",
		PausedReplacement: "z-ai/glm-5.3-flash", ContextWindow: 1_000_000,
		Efforts: []string{"minimal", "low", "medium", "high", "xhigh"}},
	// meta/muse-spark-1.2-contributor takes 1.3's place on every surface
	// since 2026-09-07 (upstream SUPPORTED_FREEBUFF_MODELS + FREEBUFF_MODELS,
	// last on purpose — the one row that may answer on a different model
	// when its shared ceiling is full). Premium shared-pool row; answered 5
	// of 5 on all four keys in the probe that found 1.3 dead. Contributor
	// terms: Meta trains on prompts and completions (dataUse 'training').
	// Full ladder, defaultEffort xhigh. 1,000,000 like Luna (Meta publishes
	// 1,048,576 for every Muse Spark variant; upstream keys this id, so the
	// parity context check compares it directly).
	{ID: "meta/muse-spark-1.2-contributor", DisplayName: "Muse Spark 1.2",
		Tagline: "Queue", Badges: []string{"Reasoning: xhigh"},
		Notice: "May use data for AI training",
		Served: true, Premium: true, ContextWindow: 1_000_000,
		Efforts: []string{"minimal", "low", "medium", "high", "xhigh"}},
	{ID: "z-ai/glm-5.2", DisplayName: "GLM 5.2",
		Tagline: "Referral reward", Badges: []string{"Referral only"},
		Notice:            "Unlocked via referral code",
		PausedReplacement: "z-ai/glm-5.3-flash"},
	{ID: "z-ai/glm-5.3-flash", DisplayName: "GLM 5.3 Flash",
		Tagline: "Deep reasoning", Badges: []string{"Reasoning: max*", "Images", "NEW"},
		Served: true, ContextWindow: 1_000_000,
		Efforts: []string{"low", "high", "max"}},
	{ID: "deepseek/deepseek-v4-flash", DisplayName: "DeepSeek V4 Flash 07/31",
		Tagline: "Smart & Fast", Badges: []string{"Reasoning: high", "NEW"},
		Notice: "May use data for AI training",
		Served: true, ContextWindow: 1_048_576,
		Efforts: []string{"low", "high", "max"}},
	{ID: "mimo/mimo-v2.5", DisplayName: "MiMo 2.5",
		Tagline: "Balanced", Badges: []string{"Images"},
		Served: true, Efforts: []string{"high"}},
	// anthropic/claude-fable-5 stays in the catalog (upstream SUPPORTED list,
	// parity test) but is NOT served: it is a paid-API model metered by its
	// own global offer pool that free accounts cannot reach, so advertising
	// it would surface admissions that always fail.
	{ID: "anthropic/claude-fable-5", DisplayName: "Claude Fable 5",
		Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
}
