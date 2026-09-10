package cli

import (
	"freebuff-proxy/backend/internal/config"
	history "freebuff-proxy/backend/internal/store"
)

// migrateEnvToDB is the env-to-DB migration: the first boot whose settings
// table lacks the marker row imports every effective knob (process env wins
// over the .env file over JSON -config over defaults — the cfg passed in is
// already resolved in that precedence by LoadOpts) into config: overlay rows,
// then sets the marker. It returns the imported row count; a present marker
// (or a nil store) is a no-op returning 0. A failure before the marker is
// set retries on the next boot: half-imported rows are harmless because the
// same effective values re-export identically, and explicit process env keeps
// winning over every row at runtime.
func migrateEnvToDB(st *history.Store, cfg config.Config) (int, error) {
	if st == nil {
		return 0, nil
	}
	if _, ok, err := st.GetSetting(config.MigrationMarkerRow); err != nil {
		return 0, err
	} else if ok {
		return 0, nil
	}
	rows := config.EffectiveOverlayMap(cfg)
	for _, def := range config.Catalog() {
		if err := st.SetSetting(config.OverlayRowKey(def.Key), rows[def.Key]); err != nil {
			return 0, err
		}
	}
	if err := st.SetSetting(config.MigrationMarkerRow, config.MigrationMarkerValue); err != nil {
		return 0, err
	}
	return len(rows), nil
}
