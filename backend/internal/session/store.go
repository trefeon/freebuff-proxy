package session

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"freebuff-proxy/backend/internal/upstream"
)

// storeVersion guards the legacy on-disk format; a stale file is ignored
// instead of mis-parsed. The live format is sessions_persist (SQLite); the
// JSON file is import-only.
const storeVersion = 1

// maxStoreFileSize caps the legacy store file read on import; anything
// larger is treated as empty instead of being read into memory wholesale.
const maxStoreFileSize = 8 << 20 // 8 MiB

// persistedState is the persisted shape of one token's cached session. The
// instance id + expiry are the fields that matter for restart-resume; the
// rest are carried so a resumed session keeps its country/queue view.
type persistedState struct {
	InstanceID         string                    `json:"instance_id"`
	Model              string                    `json:"model"`
	Status             string                    `json:"status"`
	ExpiresAt          time.Time                 `json:"expires_at"`
	GracePeriodEndsAt  time.Time                 `json:"grace_period_ends_at"`
	Position           int                       `json:"position"`
	QueueDepth         int                       `json:"queue_depth"`
	PollAt             time.Time                 `json:"poll_at"`
	CountryCode        string                    `json:"country_code"`
	CountryBlockReason string                    `json:"country_block_reason"`
	AccessTier         string                    `json:"access_tier,omitempty"`
	QuotaByModel       map[string]persistedQuota `json:"quota_by_model,omitempty"`
	// GlmPromo is the raw upstream glmPromo block ({dailySessions,
	// endsAt}); "" when absent (issue #178).
	GlmPromo string `json:"glm_promo,omitempty"`
	// Account blocks persisted so a restart keeps the dashboard's
	// referral banner, Freebucks card and standing until the next full
	// admission refreshes them (rework 2026-09-05): compact polls never
	// carry them, so without this the UI goes blank on every restart.
	// All optional — old files load with nils.
	Referral  *upstream.SessionReferral `json:"referral,omitempty"`
	Freebucks *upstream.FreebucksInfo   `json:"freebucks,omitempty"`
	Standing  *upstream.SessionStanding `json:"standing,omitempty"`
}

// persistedQuota is one model's live session quota persisted on disk.
type persistedQuota struct {
	Model       string             `json:"model"`
	Limit       float64            `json:"limit"`
	RecentCount float64            `json:"recent_count"`
	ResetAt     time.Time          `json:"reset_at"`
	Period      string             `json:"period"`
	Entitlement map[string]float64 `json:"entitlement,omitempty"`
}

// PersistedRun is the on-disk shape of one token's active agent run
// (issue #40): a restart resumes the run id without re-START. Keyed per
// token (hash) and agent, alongside the session state.
type PersistedRun struct {
	RunID          string `json:"run_id"`
	AgentID        string `json:"agent_id"`
	TraceSessionID string `json:"trace_session_id"`
	// ClientID is the run's codebuff_metadata["client_id"]. Additive: an old
	// file parses with "" and the run manager mints a fresh id, which only
	// costs that resumed run one client-id change.
	ClientID  string    `json:"client_id,omitempty"`
	StartedAt time.Time `json:"started_at"`
	Requests  int       `json:"requests"`
}

type storeFile struct {
	Version  int                       `json:"version"`
	Sessions map[string]persistedState `json:"sessions"`
	// Runs maps token key → agent id → persisted run (issue #40). Additive
	// since version 1: old files parse with an empty runs map, and old
	// binaries ignore the extra field.
	Runs map[string]map[string]PersistedRun `json:"runs,omitempty"`
}

// SessionBackend is the sessions_persist seam (DB unified storage). The
// blobs stay opaque JSON: callers marshal, the backend never interprets
// them. Keys are token hashes (upstream.Client.TokenKey), never raw tokens.
//
// The interface (not a concrete store) keeps this package out of the
// store dependency: archtest pins session to telemetry + upstream only,
// and the CLI wires the real backend.
type SessionBackend interface {
	SaveSession(tokenHash, sessionData, runsData string) error
	LoadSession(tokenHash string) (sessionData, runsData string, found bool, err error)
	SaveSessionRuns(tokenHash, runsData string) error
	DeleteSession(tokenHash string) error
}

// Store persists cached session state so a proxy restart can resume an
// unexpired upstream session instead of admitting a fresh billable one.
// sessions_persist (via backend) is the sole truth: every Save/Load/Delete
// goes through the backend under the token hash. The path is the legacy
// JSON state file, kept as an import-only fallback: on first use its
// entries fold into the backend (store-first on collision, WARN) and the
// file archives to .bak. The file is never written.
//
// All methods are safe for concurrent use. A nil backend is memory-only
// (tests, DB-unavailable degrade): same-instance behavior is unchanged,
// cross-restart resume needs a backend.
type Store struct {
	path    string
	backend SessionBackend

	mu   sync.Mutex
	data map[string]persistedState
	runs map[string]map[string]PersistedRun // token key → agent id → run (issue #40)
	// fetched marks keys already reconciled against the backend, so a
	// memory miss consults the DB exactly once instead of on every Load.
	fetched map[string]bool
	// imported marks the one-time legacy-file fold (reset to retry while
	// the file stays unreadable with a non-NotExist error).
	imported bool
}

// NewStore builds a memory-only store with a legacy import source at path.
// The file is read lazily on first use; it is never written.
func NewStore(path string) *Store {
	return &Store{path: path}
}

// NewStoreWithBackend builds a store persisting through backend
// (sessions_persist, the sole truth) with a legacy import source at path.
// The file is read lazily on the first Load; NewStoreWithBackend never
// fails (a missing/unreadable file is treated as empty).
func NewStoreWithBackend(path string, backend SessionBackend) *Store {
	return &Store{path: path, backend: backend}
}

// ensureImportedLocked folds the legacy JSON file into memory (and the
// backend when set) exactly once. Precedence is store-first: a hash the
// backend already holds keeps the backend row (collision WARN, incoming
// file entry dropped); only hashes absent from the backend are pushed.
// After a fully successful push the source archives to .bak (never
// deleted); a path already ending in .bak never renames. Caller holds s.mu.
func (s *Store) ensureImportedLocked() {
	if s.imported {
		return
	}
	s.imported = true
	if s.data == nil {
		s.data = make(map[string]persistedState)
	}
	if s.runs == nil {
		s.runs = make(map[string]map[string]PersistedRun)
	}
	if s.fetched == nil {
		s.fetched = make(map[string]bool)
	}
	if s.path == "" {
		return
	}

	// Reject oversized files before reading them into memory.
	if fi, err := os.Stat(s.path); err == nil && fi.Size() > maxStoreFileSize {
		slog.Warn("session store: legacy file too large, ignoring", "path", s.path, "bytes", fi.Size())
		return
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// First run: a missing file is a valid empty store.
			return
		}
		// Leave imported=false so the next access retries instead of
		// permanently freezing an empty view. The backend (when set)
		// stays authoritative regardless.
		s.imported = false
		slog.Warn("session store: legacy file unreadable, will retry on next access", "path", s.path, "err", err)
		return
	}
	var file storeFile
	if err := json.Unmarshal(data, &file); err != nil {
		slog.Warn("session store: legacy file parse failed, ignoring", "path", s.path, "err", err)
		return
	}
	if file.Version != storeVersion {
		slog.Warn("session store: legacy file version mismatch, ignoring", "path", s.path, "version", file.Version)
		return
	}

	// Union of session + run keys: a run without cached session state
	// still carries resume value.
	keys := make(map[string]struct{}, len(file.Sessions)+len(file.Runs))
	for key := range file.Sessions {
		keys[key] = struct{}{}
	}
	for key := range file.Runs {
		keys[key] = struct{}{}
	}
	failed := false
	for key := range keys {
		if key == "" {
			continue
		}
		ps, hasSess := file.Sessions[key]
		if hasSess && ps.Status == "active" && ps.InstanceID == "" {
			// An "active" entry without an instance id cannot be resumed
			// and would poison the resume path; drop it on import.
			slog.Warn("session store: dropping legacy active entry with empty instance id", "path", s.path, "key", key)
			hasSess = false
		}
		var runMap map[string]PersistedRun
		if agents, ok := file.Runs[key]; ok && len(agents) > 0 {
			runMap = make(map[string]PersistedRun, len(agents))
			for agentID, pr := range agents {
				// A run entry without an id cannot be resumed; drop it.
				if pr.RunID == "" {
					continue
				}
				runMap[agentID] = pr
			}
			if len(runMap) == 0 {
				runMap = nil
			}
		}
		if !hasSess && len(runMap) == 0 {
			continue
		}
		if s.backend == nil {
			if hasSess {
				s.data[key] = ps
			}
			if len(runMap) > 0 {
				s.runs[key] = runMap
			}
			s.fetched[key] = true
			continue
		}
		sessJSON := ""
		if hasSess {
			raw, err := json.Marshal(ps)
			if err != nil {
				slog.Warn("session store: legacy entry unmarshalable, skipping", "key", key, "err", err)
				failed = true
				continue
			}
			sessJSON = string(raw)
		}
		runsJSON := ""
		if len(runMap) > 0 {
			raw, err := json.Marshal(runMap)
			if err != nil {
				slog.Warn("session store: legacy runs unmarshalable, skipping", "key", key, "err", err)
				failed = true
				continue
			}
			runsJSON = string(raw)
		}
		prevSess, prevRuns, found, err := s.backend.LoadSession(key)
		if err != nil {
			slog.Warn("session store: backend read during legacy import failed; entry kept in memory only", "key", key, "err", err)
			if hasSess {
				s.data[key] = ps
			}
			if len(runMap) > 0 {
				s.runs[key] = runMap
			}
			failed = true
			continue
		}
		if found {
			// Store-first: the backend row wins. Identical re-imports
			// stay silent; a genuine split-brain warns.
			if prevSess != sessJSON || prevRuns != runsJSON {
				slog.Warn("session store: legacy import collision, dashboard store wins", "path", s.path, "token_hash", key)
			}
			continue
		}
		if err := s.backend.SaveSession(key, sessJSON, runsJSON); err != nil {
			slog.Warn("session store: legacy import write failed; entry kept in memory only", "key", key, "err", err)
			if hasSess {
				s.data[key] = ps
			}
			if len(runMap) > 0 {
				s.runs[key] = runMap
			}
			failed = true
			continue
		}
		if hasSess {
			s.data[key] = ps
		}
		if len(runMap) > 0 {
			s.runs[key] = runMap
		}
		s.fetched[key] = true
	}
	if s.backend != nil && !failed && !strings.HasSuffix(s.path, ".bak") {
		bak := s.path + ".bak"
		// Windows rename fails over an existing target: clear a stale .bak
		// first (the archive is replaced, the legacy source never deleted).
		_ = os.Remove(bak)
		if err := os.Rename(s.path, bak); err != nil {
			// The rows are already in the backend, so a stranded file is
			// harmless: the next import finds identical rows and stays
			// silent instead of duplicating.
			slog.Warn("session store: legacy archive failed (rows already imported; file left in place)", "path", s.path, "err", err)
		}
	}
}

// fetchLocked reconciles one key against the backend on a memory miss.
// The backend row (when present) populates both the session and runs maps
// so a later persist writes the row back whole instead of clobbering the
// half it did not load. Returns true when the backend could not be read
// (callers must not converge-delete a row they never saw). Caller holds
// s.mu.
func (s *Store) fetchLocked(key string) bool {
	if s.backend == nil || s.fetched[key] {
		return false
	}
	s.fetched[key] = true
	sessBlob, runsBlob, found, err := s.backend.LoadSession(key)
	if err != nil {
		slog.Warn("session store: backend load failed, serving memory view", "err", err)
		return true
	}
	if !found {
		return false
	}
	if sessBlob != "" {
		var ps persistedState
		if err := json.Unmarshal([]byte(sessBlob), &ps); err != nil {
			slog.Warn("session store: backend session blob unparsable, ignoring", "err", err)
		} else if ps.Status == "active" && ps.InstanceID == "" {
			slog.Warn("session store: backend holds active entry with empty instance id, ignoring")
		} else {
			s.data[key] = ps
		}
	}
	if runsBlob != "" {
		var agents map[string]PersistedRun
		if err := json.Unmarshal([]byte(runsBlob), &agents); err != nil {
			slog.Warn("session store: backend runs blob unparsable, ignoring", "err", err)
		} else {
			runMap := make(map[string]PersistedRun, len(agents))
			for agentID, pr := range agents {
				if pr.RunID == "" {
					continue
				}
				runMap[agentID] = pr
			}
			if len(runMap) > 0 {
				s.runs[key] = runMap
			}
		}
	}
	return false
}

// persistLocked writes key's current memory view to the backend: both
// blobs when present, a row delete when both are gone. A nil backend is a
// no-op (memory-only). Backend errors only warn — the in-memory update is
// kept so the store stays consistent for this process. Caller holds s.mu.
func (s *Store) persistLocked(key string) {
	if s.backend == nil {
		return
	}
	sessJSON := ""
	if ps, ok := s.data[key]; ok {
		raw, err := json.Marshal(ps)
		if err != nil {
			slog.Warn("session store: marshal failed, backend write skipped", "err", err)
			return
		}
		sessJSON = string(raw)
	}
	runsJSON := ""
	if agents, ok := s.runs[key]; ok && len(agents) > 0 {
		raw, err := json.Marshal(agents)
		if err != nil {
			slog.Warn("session store: runs marshal failed, backend write skipped", "err", err)
			return
		}
		runsJSON = string(raw)
	}
	var err error
	if sessJSON == "" && runsJSON == "" {
		err = s.backend.DeleteSession(key)
	} else {
		err = s.backend.SaveSession(key, sessJSON, runsJSON)
	}
	if err != nil {
		slog.Warn("session store: backend write failed (in-memory update kept)", "err", err)
	}
}

// Load returns the persisted cached state for key, or nil when absent or
// already expired beyond the grace window. Load never performs upstream
// calls; it only filters obviously-dead entries.
func (s *Store) Load(key string) *cachedState {
	if key == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureImportedLocked()
	if _, ok := s.data[key]; !ok {
		s.fetchLocked(key)
	}
	ps, ok := s.data[key]
	if !ok {
		return nil
	}
	// Drop entries whose grace window is already closed: resuming them is
	// impossible and keeping them only delays the inevitable re-create.
	if !ps.GracePeriodEndsAt.IsZero() && time.Now().After(ps.GracePeriodEndsAt) {
		delete(s.data, key)
		s.persistLocked(key)
		return nil
	}
	cs := &cachedState{
		status:             ps.Status,
		instanceID:         ps.InstanceID,
		model:              ps.Model,
		expiresAt:          ps.ExpiresAt,
		gracePeriodEndsAt:  ps.GracePeriodEndsAt,
		position:           ps.Position,
		queueDepth:         ps.QueueDepth,
		pollAt:             ps.PollAt,
		countryCode:        ps.CountryCode,
		countryBlockReason: ps.CountryBlockReason,
		accessTier:         ps.AccessTier,
		glmPromo:           ps.GlmPromo,
		referral:           ps.Referral,
		freebucks:          ps.Freebucks,
		standing:           ps.Standing,
	}
	if len(ps.QuotaByModel) > 0 {
		cs.quotaByModel = make(map[string]upstream.ModelQuota, len(ps.QuotaByModel))
		for k, q := range ps.QuotaByModel {
			mq := upstream.ModelQuota{
				Model:       q.Model,
				Limit:       q.Limit,
				RecentCount: q.RecentCount,
				ResetAt:     q.ResetAt,
				Period:      q.Period,
			}
			if len(q.Entitlement) > 0 {
				mq.Entitlement = make(map[string]float64, len(q.Entitlement))
				for ek, ev := range q.Entitlement {
					mq.Entitlement[ek] = ev
				}
			}
			cs.quotaByModel[k] = mq
		}
	}
	return cs
}

// Save persists cs under key. A nil cs removes the key. Disabled sessions
// (no instance id, no expiry) are not persisted: there is nothing to resume.
func (s *Store) Save(key string, cs *cachedState) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureImportedLocked()
	// This process now owns the key: later Loads must not re-fetch a
	// backend row over the update (the push below keeps them in sync).
	s.fetched[key] = true

	if cs == nil || (cs.instanceID == "" && cs.status != "queued") {
		delete(s.data, key)
		s.persistLocked(key)
		return
	}
	s.data[key] = persistedState{
		InstanceID:         cs.instanceID,
		Model:              cs.model,
		Status:             cs.status,
		ExpiresAt:          cs.expiresAt,
		GracePeriodEndsAt:  cs.gracePeriodEndsAt,
		Position:           cs.position,
		QueueDepth:         cs.queueDepth,
		PollAt:             cs.pollAt,
		CountryCode:        cs.countryCode,
		CountryBlockReason: cs.countryBlockReason,
		AccessTier:         cs.accessTier,
		GlmPromo:           cs.glmPromo,
	}
	if len(cs.quotaByModel) > 0 {
		ps := s.data[key]
		ps.QuotaByModel = make(map[string]persistedQuota, len(cs.quotaByModel))
		for k, q := range cs.quotaByModel {
			pq := persistedQuota{
				Model:       q.Model,
				Limit:       q.Limit,
				RecentCount: q.RecentCount,
				ResetAt:     q.ResetAt,
				Period:      q.Period,
			}
			if len(q.Entitlement) > 0 {
				pq.Entitlement = make(map[string]float64, len(q.Entitlement))
				for ek, ev := range q.Entitlement {
					pq.Entitlement[ek] = ev
				}
			}
			ps.QuotaByModel[k] = pq
		}
		s.data[key] = ps
	}
	// Account blocks: value-copy the scalar structs; deep-copy Freebucks
	// (its price maps are mutated in place by the schedule applier, so a
	// shared reference would race the live state).
	if cs.referral != nil {
		ps := s.data[key]
		r := *cs.referral
		ps.Referral = &r
		s.data[key] = ps
	}
	if cs.freebucks != nil {
		ps := s.data[key]
		ps.Freebucks = cloneFreebucksInfo(cs.freebucks)
		s.data[key] = ps
	}
	if cs.standing != nil {
		ps := s.data[key]
		st := *cs.standing
		if len(st.NextSteps) > 0 {
			st.NextSteps = append([]upstream.StandingNextStep(nil), st.NextSteps...)
		}
		ps.Standing = &st
		s.data[key] = ps
	}
	s.persistLocked(key)
}

// cloneFreebucksInfo deep-copies the map/slice fields ApplyFreebucksPriceChanges
// mutates in place, so the persisted snapshot cannot race live state.
func cloneFreebucksInfo(fb *upstream.FreebucksInfo) *upstream.FreebucksInfo {
	if fb == nil {
		return nil
	}
	out := *fb
	if len(fb.Prices) > 0 {
		out.Prices = make(map[string]float64, len(fb.Prices))
		for k, v := range fb.Prices {
			out.Prices[k] = v
		}
	}
	if len(fb.PriceNotices) > 0 {
		out.PriceNotices = make(map[string]string, len(fb.PriceNotices))
		for k, v := range fb.PriceNotices {
			out.PriceNotices[k] = v
		}
	}
	if len(fb.PriceChanges) > 0 {
		out.PriceChanges = append([]upstream.FreebucksPriceChange(nil), fb.PriceChanges...)
	}
	return &out
}

// Remove drops key from the store (session invalidated/ended at runtime).
// When expectedInstanceID is non-empty the entry is only removed if its
// stored instance id matches, so a stale invalidation cannot clobber a newer
// resumed session; an empty expectedInstanceID removes unconditionally.
func (s *Store) Remove(key, expectedInstanceID string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureImportedLocked()
	if _, ok := s.data[key]; !ok {
		// The entry may live only in the backend (restart before any
		// Load): fetch before comparing, and never converge-delete a
		// row the backend refused to show.
		if s.fetchLocked(key) {
			return
		}
		if _, ok := s.data[key]; !ok {
			return
		}
	}
	if expectedInstanceID != "" && s.data[key].InstanceID != expectedInstanceID {
		return
	}
	delete(s.data, key)
	s.persistLocked(key)
}

// SaveRun persists one active run for token key under agentID (issue #40).
// A run with an empty id is dropped. Best-effort: a failed backend write
// only warns — the run is simply re-STARTed after a restart.
func (s *Store) SaveRun(key, agentID string, pr PersistedRun) {
	if key == "" || agentID == "" || pr.RunID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureImportedLocked()
	if _, ok := s.runs[key]; !ok {
		s.fetchLocked(key)
	}
	s.fetched[key] = true
	agents := s.runs[key]
	if agents == nil {
		agents = make(map[string]PersistedRun)
		s.runs[key] = agents
	}
	agents[agentID] = pr
	s.persistLocked(key)
}

// LoadRun returns the persisted run for token key + agentID, or nil when
// absent. The caller (runs manager) decides whether the run is fresh enough
// to adopt; LoadRun never performs upstream calls.
func (s *Store) LoadRun(key, agentID string) *PersistedRun {
	if key == "" || agentID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureImportedLocked()
	if _, ok := s.runs[key][agentID]; !ok {
		s.fetchLocked(key)
	}
	pr, ok := s.runs[key][agentID]
	if !ok {
		return nil
	}
	copy := pr
	return &copy
}

// RemoveRun drops the persisted run for token key + agentID (FINISHed
// upstream or superseded by a fresh START).
func (s *Store) RemoveRun(key, agentID string) {
	if key == "" || agentID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureImportedLocked()
	if _, ok := s.runs[key]; !ok {
		if s.fetchLocked(key) {
			return
		}
	}
	agents, ok := s.runs[key]
	if !ok {
		return
	}
	if _, ok := agents[agentID]; !ok {
		return
	}
	delete(agents, agentID)
	if len(agents) == 0 {
		delete(s.runs, key)
	}
	s.persistLocked(key)
}
