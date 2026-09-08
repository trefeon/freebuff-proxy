package modelcat

import (
	"slices"
)

func byID(id string) *ModelInfo {
	for i := range Catalog {
		if Catalog[i].ID == id {
			return &Catalog[i]
		}
	}
	return nil
}

// DisplayName returns the upstream catalog display name for id, or id when
// the model is unknown (mirrors freebuffWithdrawnModelMessage's fallback).
func DisplayName(id string) string {
	if m := byID(id); m != nil {
		return m.DisplayName
	}
	return id
}

// Tagline returns the upstream catalog description for id.
func Tagline(id string) string {
	if m := byID(id); m != nil {
		return m.Tagline
	}
	return ""
}

// Notice returns the upstream warning or special offer for id.
func Notice(id string) string {
	if m := byID(id); m != nil {
		return m.Notice
	}
	return ""
}

// Badges returns the capability/freshness chips for id.
func Badges(id string) []string {
	if m := byID(id); m != nil {
		return slices.Clone(m.Badges)
	}
	return nil
}

// IsServed reports whether id passes the ServedModels gate.
func IsServed(id string) bool {
	if m := byID(id); m != nil {
		return m.Served
	}
	return false
}

// IsPaused reports whether id is upstream-recognized but withdrawn
// (FREEBUFF_PAUSED_FREE_MODEL_IDS): refused at admission, never served.
func IsPaused(id string) bool {
	if m := byID(id); m != nil {
		return m.PausedReplacement != ""
	}
	return false
}

// PausedReplacement returns the model the withdrawn-model refusal copy
// recommends for id ("" when id is not paused).
func PausedReplacement(id string) string {
	if m := byID(id); m != nil {
		return m.PausedReplacement
	}
	return ""
}

// WithdrawnModelMessage mirrors upstream freebuffWithdrawnModelMessage
// (freebuff-models.ts:1685-1697): names the model asked for and what to use
// instead — the client that sends this id is a released binary whose picker
// still lists it, so "unavailable" alone leaves the user staring at a row
// that looks fine and does not work.
func WithdrawnModelMessage(id string) string {
	replacement := PausedReplacement(id)
	if replacement == "" {
		return DisplayName(id) + " is no longer available in Freebuff."
	}
	return DisplayName(id) + " is no longer available in Freebuff. We recommend using " + DisplayName(replacement) + " instead."
}

// IsPremium reports whether id is in the shared daily premium pool
// (FREEBUFF_PREMIUM_MODEL_IDS)
func IsPremium(id string) bool {
	if m := byID(id); m != nil {
		return m.Premium
	}
	return false
}

// SharedPremiumModels returns the ids metered by the shared daily premium
// pool: Luna + Muse Spark 1.2 since 2026-09-07 (1.3 withdrawn that day;
// solar left the pool when its entitlement went unmetered; gemini is
// Pro-paywalled).
// GLM 5.3 Flash is unmetered.
func SharedPremiumModels() []string {
	var out []string
	for i := range Catalog {
		if Catalog[i].Premium {
			out = append(out, Catalog[i].ID)
		}
	}
	return out
}

// ContextWindow returns the model's context window in tokens, or
// DefaultContextWindow when the model has no observed entry.
func ContextWindow(id string) int {
	if m := byID(id); m != nil && m.ContextWindow > 0 {
		return m.ContextWindow
	}
	return DefaultContextWindow
}

// PausedMap builds the paused-model map (id → replacement id) from the
// catalog, mirroring upstream FREEBUFF_PAUSED_FREE_MODEL_IDS.
func PausedMap() map[string]string {
	out := make(map[string]string, len(Catalog))
	for i := range Catalog {
		if Catalog[i].PausedReplacement != "" {
			out[Catalog[i].ID] = Catalog[i].PausedReplacement
		}
	}
	return out
}

// ServedMap builds the ServedModels gate map (id → true) from the catalog.
func ServedMap() map[string]bool {
	out := make(map[string]bool, len(Catalog))
	for i := range Catalog {
		if Catalog[i].Served {
			out[Catalog[i].ID] = true
		}
	}
	return out
}

// ServedIDs returns the served model ids in catalog order.
func ServedIDs() []string {
	var out []string
	for i := range Catalog {
		if Catalog[i].Served {
			out = append(out, Catalog[i].ID)
		}
	}
	return out
}

// ServedHelpText formats the served model list for error messages.
func ServedHelpText() string {
	ids := ServedIDs()
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ", "
		}
		out += id
	}
	return out
}
