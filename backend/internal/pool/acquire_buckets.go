// acquire_buckets.go - failover error-bucket helpers: typed error
// extractors (as*) and the shortest-window picker (bestRateLimit).
// Pure move from pool.go; no behavior change.
package pool

import (
	"errors"

	"freebuff-proxy/backend/internal/upstream"
)

// asRateLimit extracts a RateLimitError from err (nil when absent).
func asRateLimit(err error) *upstream.RateLimitError {
	var rle *upstream.RateLimitError
	if errors.As(err, &rle) {
		return rle
	}
	return nil
}

// asIpCapped extracts an IpCappedError from err (nil when absent).
func asIpCapped(err error) *upstream.IpCappedError {
	var ice *upstream.IpCappedError
	if errors.As(err, &ice) {
		return ice
	}
	return nil
}

// asBan extracts a BanError from err (nil when absent).
func asBan(err error) *upstream.BanError {
	var be *upstream.BanError
	if errors.As(err, &be) {
		return be
	}
	return nil
}

// asCountryBlocked extracts a CountryBlockedError from err (nil when
// absent).
func asCountryBlocked(err error) *upstream.CountryBlockedError {
	var cbe *upstream.CountryBlockedError
	if errors.As(err, &cbe) {
		return cbe
	}
	return nil
}

// asLimitedIp extracts a LimitedIpError from err (nil when absent).
func asLimitedIp(err error) *upstream.LimitedIpError {
	var lie *upstream.LimitedIpError
	if errors.As(err, &lie) {
		return lie
	}
	return nil
}

// bestRateLimit picks the rate-limit error with the shortest retry
// window (the token that unblocks earliest bounds the wait).
func bestRateLimit(entries []*upstream.RateLimitError) *upstream.RateLimitError {
	best := entries[0]
	for _, e := range entries[1:] {
		if e.RetryAfter < best.RetryAfter {
			best = e
		}
	}
	return best
}
