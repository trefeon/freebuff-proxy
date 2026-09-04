// Package updatecheck provides the dashboard's release-update indicator
// (issue #50b): a cached lookup of the latest GitHub release tag for the
// repo, plus a semver-ish comparison against the running version. The
// lookup is deliberately non-blocking for the dashboard — the first render
// after a 6h cache expiry (or after a failed attempt's backoff window)
// performs one bounded HTTP GET (3s timeout); a failure degrades to
// "no update" and backs off for the same 6h window instead of retrying on
// every render.
package updatecheck

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultRepo is the upstream repo whose releases the indicator checks.
const DefaultRepo = "trefeon/freebuff-proxy"

// CacheTTL is how long a fetched latest-release tag is reused (issue #50:
// "cached 6h").
const CacheTTL = 6 * time.Hour

// fetchTimeout bounds one GitHub API call (the dashboard must never block
// on it).
const fetchTimeout = 3 * time.Second

// Checker is a concurrency-safe, in-memory-cached latest-release lookup.
// The zero value is not usable — use New.
type Checker struct {
	repo   string
	client *http.Client
	logger *slog.Logger // decision Debug sink (nil = slog.Default())

	mu       sync.Mutex
	latest   string
	fetched  time.Time
	fetching bool
}

// New builds a checker for repo (owner/name). client is used for the
// GitHub API call; nil uses http.DefaultClient with fetchTimeout.
func New(repo string, client *http.Client) *Checker {
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}
	return &Checker{repo: repo, client: client, logger: slog.Default()}
}

// SetLogger replaces the checker's log sink (nil restores slog.Default).
// Used by tests and by hosts that want the decision Debug on a custom logger.
func (c *Checker) SetLogger(l *slog.Logger) {
	if l == nil {
		l = slog.Default()
	}
	c.logger = l
}

// Invalidate clears the cache timestamp so the next Latest call re-fetches.
func (c *Checker) Invalidate() {
	c.mu.Lock()
	c.fetched = time.Time{}
	c.mu.Unlock()
}

// Latest returns the latest release tag (e.g. "v0.9.3") from the in-memory
// cache, fetching it when the last attempt — successful or failed — is
// older than CacheTTL. A fetch failure returns the previously cached tag
// (or "") with the error and still records the attempt, so subsequent
// calls back off for CacheTTL instead of re-fetching. The cache is
// refreshed single-flight so concurrent renders share one GET. Each lookup
// emits a Debug line with the decision (cached|fetched|failed) and the
// lookup duration (T18).
func (c *Checker) Latest(ctx context.Context) (string, error) {
	start := time.Now()
	c.mu.Lock()
	if time.Since(c.fetched) < CacheTTL {
		tag := c.latest
		c.mu.Unlock()
		c.logger.Debug("update check decision", "decision", "cached", "ms", time.Since(start).Milliseconds())
		return tag, nil
	}
	if c.fetching {
		// Another render is mid-fetch: wait for it rather than stacking a
		// second GET. Honor ctx cancellation so a canceled waiter does not
		// spin on the in-flight fetch.
		for c.fetching {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				c.mu.Lock()
				tag := c.latest
				c.mu.Unlock()
				c.logger.Debug("update check decision", "decision", "cached", "ms", time.Since(start).Milliseconds())
				return tag, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
			c.mu.Lock()
		}
		tag := c.latest
		c.mu.Unlock()
		c.logger.Debug("update check decision", "decision", "cached", "ms", time.Since(start).Milliseconds())
		return tag, nil
	}
	c.fetching = true
	c.mu.Unlock()

	tag, err := c.fetchLatest(ctx)

	c.mu.Lock()
	c.fetching = false
	if err == nil && tag != "" {
		c.latest = tag
		c.fetched = time.Now()
	} else {
		// Keep the previous value and stamp the attempt: the CacheTTL window
		// now also covers failed lookups (first-ever failure included), so
		// the dashboard's frequent polls back off instead of hammering
		// api.github.com on every render (review P2).
		c.fetched = time.Now()
	}
	got := c.latest
	c.mu.Unlock()
	decision := "fetched"
	if err != nil || tag == "" {
		decision = "failed"
	}
	c.logger.Debug("update check decision", "decision", decision, "ms", time.Since(start).Milliseconds())
	return got, err
}

// fetchLatest GETs https://api.github.com/repos/<repo>/releases/latest and
// extracts tag_name.
func (c *Checker) fetchLatest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/repos/"+c.repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "freebuff-proxy-updatecheck/1.0")
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("github releases/latest: status %d", resp.StatusCode)
	}
	var decoded struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&decoded); err != nil {
		return "", err
	}
	return strings.TrimSpace(decoded.TagName), nil
}

// UpdateAvailable reports whether latest is newer than current ("" current
// means dev builds — never "update available" for a dev build, since its
// version cannot be compared). Both tags are compared with CompareVersions.
func UpdateAvailable(current, latest string) bool {
	if current == "" || current == "dev" || latest == "" {
		return false
	}
	return CompareVersions(latest, current) > 0
}

// CompareVersions compares two version strings ("v0.9.3", "0.10.1",
// "1.2.3-beta.1") numerically on their numeric dot-components; a missing
// component counts as 0 (v0.9 == v0.9.0). Non-numeric suffixes after the
// third component are ignored for ordering (a beta is treated equal to its
// release — the dashboard only needs to detect NEWER releases, and GitHub
// tags the release with a plain vX.Y.Z). Returns -1/0/1; malformed input
// compares as 0 against anything (the indicator degrades to no-update).
func CompareVersions(a, b string) int {
	pa, oka := parseVersion(a)
	pb, okb := parseVersion(b)
	if !oka || !okb {
		return 0
	}
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	return 0
}

// parseVersion splits "v1.2.3-rc.1" into [1,2,3] (numeric dot-components
// only; the suffix and any non-numeric trailing parts are dropped).
func parseVersion(v string) ([]int, bool) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	// Cut any pre-release/build suffix (e.g. "-rc.1", "+build").
	if idx := strings.IndexAny(v, "-+"); idx >= 0 {
		v = v[:idx]
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}
