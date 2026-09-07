package server

// Per-page UI state endpoints (slice 4): display-only dashboard snapshots
// that survive a gateway restart (last-visited hash, expanded rows, filter
// text). The SPA stays fetch-only: every page loads its snapshot on mount
// and saves it back debounced; failures are warn-only client-side.
//
//	GET /admin/api/pages/{id}  {data} (absent row -> {}, never 404)
//	PUT /admin/api/pages/{id}  {data} upserts (opaque JSON, 64KB cap)
//
// Unknown ids 404 against the allowlist below. A nil store (DB failed at
// boot) degrades GET to {} and 503s PUT — the same split the settings
// overlay uses, so a write never silently lands nowhere. PUT carries the
// CSRF gate like every other state-changing admin row.

import (
	"encoding/json"
	"net/http"
)

const maxPageStateBytes = 64 << 10

// validPageIDs is the pages_state allowlist: the 8 mounted dashboard pages
// (matching the SPA nav ids) plus "shell" — chrome state that is not a page
// itself (currently just {lastHash}, restored by App.svelte on boot).
var validPageIDs = map[string]bool{
	"overview": true,
	"tokens":   true,
	"maturity": true,
	"quota":    true,
	"models":   true,
	"logs":     true,
	"settings": true,
	"devtools": true,
	"shell":    true,
}

func (a *adminHandlers) handlePageStateGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validPageIDs[id] {
		a.dash.RenderResult(w, http.StatusNotFound, false, "Unknown page "+id+".", "unknown_page")
		return
	}
	if a.settings == nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
		return
	}
	data, ok, err := a.settings.GetPageState(id)
	if err != nil {
		a.dash.RenderResult(w, http.StatusInternalServerError, false, "Failed to read page state: "+err.Error(), "persist_failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if !ok || data == "" {
		_, _ = w.Write([]byte(`{"data":{}}`))
		return
	}
	// Stored rows are validated JSON on PUT, so echoing verbatim is safe.
	_, _ = w.Write([]byte(`{"data":` + data + `}`))
}

func (a *adminHandlers) handlePageStatePut(w http.ResponseWriter, r *http.Request) {
	if a.settings == nil {
		a.dash.RenderResult(w, http.StatusServiceUnavailable, false,
			"Page state store unavailable — the dashboard runs live-only (the DB failed to open at boot).", "pages_unavailable")
		return
	}
	id := r.PathValue("id")
	if !validPageIDs[id] {
		a.dash.RenderResult(w, http.StatusNotFound, false, "Unknown page "+id+".", "unknown_page")
		return
	}
	// Envelope slack above the 64KB data cap: the cap applies to data, not
	// the {"data":...} wrapper.
	r.Body = http.MaxBytesReader(w, r.Body, (maxPageStateBytes + 8<<10))
	var req struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.dash.RenderResult(w, http.StatusBadRequest, false, "Invalid JSON body (want {data}).", "bad_request")
		return
	}
	if len(req.Data) == 0 {
		a.dash.RenderResult(w, http.StatusBadRequest, false, "Missing data (want {data}).", "bad_request")
		return
	}
	if len(req.Data) > maxPageStateBytes {
		a.dash.RenderResult(w, http.StatusRequestEntityTooLarge, false, "Page state exceeds 64KB.", "page_too_large")
		return
	}
	if err := a.settings.PutPageState(id, string(req.Data)); err != nil {
		a.dash.RenderResult(w, http.StatusInternalServerError, false, "Failed to persist page state: "+err.Error(), "persist_failed")
		return
	}
	a.dash.RenderResult(w, http.StatusOK, true, "Page state saved.", "page_saved")
}
