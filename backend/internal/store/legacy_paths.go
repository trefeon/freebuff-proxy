package store

import (
	"os"
	"path/filepath"
)

// LegacyHistoryCandidates lists pre-existing legacy dashboard DBs that may
//
// Releases before the unified DB kept state next to the working directory:
// freebuff-history.db beside SESSION_STATE_FILE, and later data/freebuff.db
// under old bind mounts (docker-canonical: /app/state). When the canonical
// DB path moves (fresh db_data volume at /app/data), the data must still be
// found: these helpers list every pre-existing legacy location so boot can
// fold each one into the new DB. Existence-gated: entries name files that
// must already be there, so the constants are inert on machines (Windows
// desktop, fresh installs) where those paths never exist. Nothing here
// deletes: session files archive to .bak, history files stay in place.
func LegacyHistoryCandidates(newPath, stateFile string) []string {
	var ordered []string
	if abs, err := filepath.Abs(stateFile); err == nil {
		dir := filepath.Dir(abs)
		ordered = append(ordered,
			filepath.Join(dir, "freebuff-history.db"),
			filepath.Join(dir, "data", "freebuff.db"),
		)
	}
	ordered = append(ordered,
		"/app/state/freebuff-history.db",
		"/app/state/data/freebuff.db",
	)
	return existingFilesExcept(ordered, newPath)
}

// LegacySessionCandidates lists pre-existing session JSON files worth
// importing: the configured path first, then the docker-canonical bind
// location from old-compose layouts.
func LegacySessionCandidates(stateFile string) []string {
	return existingFilesExcept([]string{
		stateFile,
		"/app/state/.freebuff-session-state.json",
	}, "")
}

func existingFilesExcept(paths []string, except string) []string {
	var exceptAbs string
	if except != "" {
		if abs, err := filepath.Abs(except); err == nil {
			exceptAbs = abs
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil || abs == exceptAbs || seen[abs] {
			continue
		}
		seen[abs] = true
		st, err := os.Stat(abs)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		out = append(out, abs)
	}
	return out
}
