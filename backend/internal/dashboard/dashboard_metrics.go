package dashboard

import (
	"strconv"
	"strings"
)

// --- metrics ---

type metricSample struct {
	Requests int64
	Retries  int64
	Rotation int64
}

const maxMetricSamples = 120

type metricTrend struct {
	Direction  string  `json:"direction"` // "up", "down", "flat"
	Percentage float64 `json:"percentage"`
}

type perTokenMetrics struct {
	Token                int    `json:"token"`
	Requests24h          int    `json:"requests_24h"`
	TransientRetries     int64  `json:"transient_retries"`
	FingerprintRotations int64  `json:"fingerprint_rotations"`
	SpendDay             int64  `json:"spend_day"`
	RiskLevel            string `json:"risk_level"`
}

type metricsData struct {
	TransientRetries     int64             `json:"transient_retries"`
	FingerprintRotations int64             `json:"fingerprint_rotations"`
	RequestsTotal        int64             `json:"requests_total"`
	Models               int               `json:"models"`
	SampleCount          int               `json:"sample_count"`
	RequestsSpark        string            `json:"requests_spark"`
	RetriesSpark         string            `json:"retries_spark"`
	RequestsTrend        metricTrend       `json:"requests_trend"`
	RetriesTrend         metricTrend       `json:"retries_trend"`
	PerTokens            []perTokenMetrics `json:"per_tokens"`
}

func (d *Dashboard) metricsData() metricsData {
	ps := d.pool.PoolSnapshot()
	md := metricsData{
		TransientRetries:     ps.TransientRetries,
		FingerprintRotations: ps.FingerprintRotations,
		RequestsTotal:        int64(ps.RequestsServed),
		// The served gate keeps /admin/metrics consistent with /v1/models,
		// /healthz and /admin/overview (the raw registry carries god-only /
		// eval rows such as luna-es that are not servable).
		Models: len(servedModels(d.reg)),
	}
	d.metricsMu.Lock()
	d.metricHist = append(d.metricHist, metricSample{Requests: md.RequestsTotal, Retries: ps.TransientRetries, Rotation: ps.FingerprintRotations})
	if len(d.metricHist) > maxMetricSamples {
		d.metricHist = d.metricHist[len(d.metricHist)-maxMetricSamples:]
	}
	hist := make([]metricSample, len(d.metricHist))
	copy(hist, d.metricHist)
	d.metricsMu.Unlock()
	md.SampleCount = len(hist)

	requests := make([]float64, len(hist))
	retries := make([]float64, len(hist))
	for i, s := range hist {
		requests[i] = float64(s.Requests)
		retries[i] = float64(s.Retries)
	}
	// NOTE: sparklineSVG inlines color into the SVG stroke *attribute*, where
	// CSS var() cannot resolve (and --fp-amber/--fp-teal are not even defined
	// in app.css) — so concrete theme hexes are required or polylines render
	// invisible. Keep in sync with frontend/src/app.css (--fp-accent/--fp-info).
	md.RequestsSpark = sparklineSVG(requests, "#e3a857", "requests served over time")
	md.RetriesSpark = sparklineSVG(retries, "#7dd3fc", "transient retries over time")

	// Trend: compare last 10 samples vs previous 10.
	md.RequestsTrend = computeTrend(hist, true)
	md.RetriesTrend = computeTrend(hist, false)

	// Per-token breakdown from pool snapshot.
	for _, tok := range ps.Tokens {
		md.PerTokens = append(md.PerTokens, perTokenMetrics{
			Token:                tok.Token,
			Requests24h:          tok.Messages24h,
			TransientRetries:     tok.TransientRetries,
			FingerprintRotations: tok.FingerprintRotations,
			SpendDay:             tok.SpendDay,
			RiskLevel:            tok.RiskLevel,
		})
	}
	return md
}

// computeTrend compares the sum of the last 10 samples to the previous 10.
// useRequests selects the Requests column (true) or Retries (false).
func computeTrend(hist []metricSample, useRequests bool) metricTrend {
	const window = 10
	n := len(hist)
	if n < 2*window {
		return metricTrend{Direction: "flat"}
	}
	recent := hist[n-window:]
	previous := hist[n-2*window : n-window]

	var recentSum, previousSum int64
	for i := range window {
		if useRequests {
			recentSum += recent[i].Requests
			previousSum += previous[i].Requests
		} else {
			recentSum += recent[i].Retries
			previousSum += previous[i].Retries
		}
	}

	if previousSum == 0 {
		if recentSum == 0 {
			return metricTrend{Direction: "flat"}
		}
		return metricTrend{Direction: "up", Percentage: 100}
	}
	pct := float64(recentSum-previousSum) / float64(previousSum) * 100
	if pct > 5 {
		return metricTrend{Direction: "up", Percentage: pct}
	} else if pct < -5 {
		return metricTrend{Direction: "down", Percentage: pct}
	}
	return metricTrend{Direction: "flat", Percentage: pct}
}

func sparklineSVG(values []float64, color, label string) string {
	const w, h = 260, 44
	if len(values) < 2 {
		return `<svg viewBox="0 0 ` + strconv.Itoa(w) + ` ` + strconv.Itoa(h) + `" role="img" aria-label="` + label + `"><polyline points="0,` + strconv.Itoa(h-2) + ` ` + strconv.Itoa(w) + `,` + strconv.Itoa(h-2) + `" fill="none" stroke="` + color + `" stroke-width="1.5"/></svg>`
	}
	min, max := values[0], values[0]
	for _, v := range values[1:] {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	span := max - min
	if span == 0 {
		span = 1
	}
	var sb strings.Builder
	sb.WriteString(`<svg viewBox="0 0 ` + strconv.Itoa(w) + ` ` + strconv.Itoa(h) + `" role="img" aria-label="` + label + `" preserveAspectRatio="none"><polyline points="`)
	for i, v := range values {
		x := float64(i) * float64(w) / float64(len(values)-1)
		y := float64(h-2) - (v-min)/span*float64(h-4)
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(strconv.FormatFloat(x, 'f', 1, 64) + "," + strconv.FormatFloat(y, 'f', 1, 64))
	}
	sb.WriteString(`" fill="none" stroke="` + color + `" stroke-width="1.5"/></svg>`)
	return sb.String()
}
