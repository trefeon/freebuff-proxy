package session

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// storeVersion guards the on-disk format; bump it when the schema changes so
// a stale file is ignored instead of mis-parsed.
const storeVersion = 1

// persistedState is the on-disk shape of one token's cached session. The
// instance id + expiry are the fields that matter for restart-resume; the
// rest are carried so a resumed session keeps its tier/country/queue view.
type persistedState struct {
	InstanceID         string    `json:"instance_id"`
	Model              string    `json:"model"`
	Status             string    `json:"status"`
	ExpiresAt          time.Time `json:"expires_at"`
	GracePeriodEndsAt  time.Time `json:"grace_period_ends_at"`
	Position           int       `json:"position"`
	QueueDepth         int       `json:"queue_depth"`
	PollAt             time.Time `json:"poll_at"`
	AccessTier         string    `json:"access_tier"`
	CountryCode        string    `json:"country_code"`
	CountryBlockReason string    `json:"country_block_reason"`
}

type storeFile struct {
	Version  int                       `json:"version"`
	Sessions map[string]persistedState `json:"sessions"`
}

// Store persists cached session state to a single JSON file so a proxy
// restart can resume an unexpired upstream session instead of burning a new
// session slot. Keys are token hashes (upstream.Client.TokenKey), never raw
// tokens. All methods are safe for concurrent use; writes are atomic
// (temp file + rename) and the file is created with mode 0600.
type Store struct {
	path string

	mu     sync.Mutex
	data   map[string]persistedState
	loaded bool
}

// NewStore builds a store backed by path. The file is read lazily on the
// first Load; NewStore never fails (a missing/unreadable file is treated as
// empty and a later Save overwrites it).
func NewStore(path string) *Store {
	return &Store{path: path}
}

func (s *Store) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	s.data = make(map[string]persistedState)

	data, err := os.ReadFile(s.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("session store: read failed", "path", s.path, "err", err)
		}
		return
	}
	var file storeFile
	if err := json.Unmarshal(data, &file); err != nil {
		slog.Warn("session store: parse failed, ignoring", "path", s.path, "err", err)
		return
	}
	if file.Version != storeVersion {
		slog.Warn("session store: version mismatch, ignoring", "path", s.path, "version", file.Version)
		return
	}
	if file.Sessions != nil {
		s.data = file.Sessions
	}
}

// Load returns the persisted cached state for key, or nil when absent or
// already expired beyond the grace window. Load never performs upstream
// calls; it only filters obviously-dead entries.
func (s *Store) Load(key string) *cachedState {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	ps, ok := s.data[key]
	if !ok {
		return nil
	}
	// Drop entries whose grace window is already closed: resuming them is
	// impossible and keeping them only delays the inevitable re-create.
	if !ps.GracePeriodEndsAt.IsZero() && time.Now().After(ps.GracePeriodEndsAt) {
		delete(s.data, key)
		return nil
	}
	return &cachedState{
		status:             ps.Status,
		instanceID:         ps.InstanceID,
		model:              ps.Model,
		expiresAt:          ps.ExpiresAt,
		gracePeriodEndsAt:  ps.GracePeriodEndsAt,
		position:           ps.Position,
		queueDepth:         ps.QueueDepth,
		pollAt:             ps.PollAt,
		accessTier:         ps.AccessTier,
		countryCode:        ps.CountryCode,
		countryBlockReason: ps.CountryBlockReason,
	}
}

// Save persists cs under key. A nil cs removes the key. Disabled sessions
// (no instance id, no expiry) are not persisted: there is nothing to resume.
func (s *Store) Save(key string, cs *cachedState) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()

	if cs == nil || (cs.instanceID == "" && cs.status != "queued") {
		delete(s.data, key)
		s.flushLocked()
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
		AccessTier:         cs.accessTier,
		CountryCode:        cs.countryCode,
		CountryBlockReason: cs.countryBlockReason,
	}
	s.flushLocked()
}

// Remove drops key from the store (session invalidated/ended at runtime).
func (s *Store) Remove(key string) {
	s.Save(key, nil)
}

// flushLocked writes the current map atomically. Caller holds s.mu.
func (s *Store) flushLocked() {
	file := storeFile{Version: storeVersion, Sessions: s.data}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		slog.Warn("session store: marshal failed", "path", s.path, "err", err)
		return
	}

	dir := filepath.Dir(s.path)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			slog.Warn("session store: mkdir failed", "dir", dir, "err", err)
			return
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Warn("session store: write failed", "path", s.path, "err", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		slog.Warn("session store: rename failed", "path", s.path, "err", err)
		_ = os.Remove(tmp)
	}
}
