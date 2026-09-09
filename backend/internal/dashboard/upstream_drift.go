// Package dashboard — upstream drift embed.
//
// The CI workflow .github/workflows/upstream-drift.yml refreshes
// upstream_drift.json after every check run. The dashboard embeds it at
// compile time and surfaces it on the Overview payload (upstream_sync) so
// the user can see whether the proxy is up to date with CodebuffAI/freebuff
// without the runtime making a live network call (the project is winding
// down — we want the indicator to work even with no network access at
// boot). There is no standalone route: the SPA reads overview.upstream_sync.
//
// The shape mirrors the JSON the check-upstream.sh script emits; the
// dashboard treats `null` as "no drift report yet" (the file is shipped
// with the build, then updated by CI on every drift run). The report also
// carries wiregen_sha (the snapshots manifest SHA from
// backend/internal/wirefacts/testdata/wire/snapshots.json): it rides the
// raw embed inside the Overview payload, so clients can tell which
// generator input the running build's _gen files were built from.

package dashboard

import (
	_ "embed"
)

//go:embed data/upstream_drift.json
var upstreamDriftJSON []byte
