package cli

import (
	"log/slog"

	"freebuff-proxy/backend/internal/pool"
	history "freebuff-proxy/backend/internal/store"
)

// poolMaturityStore adapts the history store to pool.MaturityStore
// (ADR-0026): automation blobs ride the tokens table keyed by token hash,
// so the pool never imports the store package. Nil-safe at every level: a
// nil adapter or nil inner store keeps the pool persistence-free, mirroring
// poolHistorySink.
type poolMaturityStore struct {
	st *history.Store
}

// Compile gate: the adapter must satisfy the pool boundary.
var _ pool.MaturityStore = (*poolMaturityStore)(nil)

func (s *poolMaturityStore) SaveMaturity(tokenHash string, stateJSON string, streakJSON []byte) error {
	if s == nil || s.st == nil {
		return nil
	}
	return s.st.SaveTokenMaturity(tokenHash, stateJSON, streakJSON)
}

func (s *poolMaturityStore) LoadMaturity(tokenHash string) (string, []byte, bool, error) {
	if s == nil || s.st == nil {
		return "", nil, false, nil
	}
	return s.st.LoadTokenMaturity(tokenHash)
}

// maturityRestorePool is the pool half of the restore seam: satisfied by
// *pool.Pool in Serve, by fakes in tests.
type maturityRestorePool interface {
	RestoreMaturity() error
}

// restoreMaturityFromStore reloads persisted automation state into the pool
// (ADR-0026 boot wiring). Warn-only: any failure keeps the boot green on
// the in-memory path, and a nil pool no-ops.
func restoreMaturityFromStore(logger *slog.Logger, p maturityRestorePool) {
	if p == nil {
		return
	}
	if err := p.RestoreMaturity(); err != nil {
		logger.Warn("maturity restore skipped; automation starts fresh", "err", err)
		return
	}
	logger.Info("maturity state restored")
}
