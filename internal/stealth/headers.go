package stealth

import "net/http"

// proxyHeaders lists headers that identify HTTP clients as proxies or
// automation tools. Real browsers never send these.
var proxyHeaders = []string{
	"X-Forwarded-For",
	"X-Forwarded-Proto",
	"X-Forwarded-Host",
	"X-Real-IP",
	"X-Proxy-User-IP",
	"Via",
	"X-Via",
	"Proxy-Connection",
	"X-Proxy-Agent",
	"X-Request-ID",
	"CF-Connecting-IP",
	"CF-IPCountry",
	"CF-Ray",
	"CF-Visitor",
	"True-Client-IP",
	"X-Originating-IP",
	"X-Remote-IP",
	"X-Remote-Addr",
	"X-Client-IP",
	"X-Host",
	"X-Correlation-ID",
	"X-Trace-ID",
	"X-Amzn-Trace-Id",
	"X-Cache",
	"X-Served-By",
}

// SanitizeHeaders removes proxy-identifying headers from h.
func SanitizeHeaders(h http.Header) {
	for _, hdr := range proxyHeaders {
		h.Del(hdr)
	}
}

// ApplyProfileHeaders sets browser-typical headers on h from the profile.
// Only non-empty profile fields are written. Sec-Fetch-* headers are set to
// values appropriate for a cross-origin API request (not a navigation):
//   - Sec-Fetch-Site: cross-site (the request targets a different origin)
//   - Sec-Fetch-Mode: cors       (API POST, not navigate)
//   - Sec-Fetch-Dest: empty      (not a document/script/image load)
//
// Upgrade-Insecure-Requests is deliberately NOT set: real browsers only send
// it on top-level navigation requests (GET to a page), never on API POSTs.
// Its presence on a POST is a fingerprinting signal.
//
// Sec-CH-UA-Mobile is set to "?0" for Chromium profiles because real desktop
// Chrome always sends it; its absence is detectable.
func ApplyProfileHeaders(h http.Header, p *Profile) {
	if p.UserAgent != "" {
		h.Set("User-Agent", p.UserAgent)
	}
	if p.SecChUA != "" {
		h.Set("Sec-CH-UA", p.SecChUA)
		h.Set("Sec-CH-UA-Mobile", "?0")
	}
	if p.SecChUAPlatform != "" {
		h.Set("Sec-CH-UA-Platform", p.SecChUAPlatform)
	}
	if p.AcceptLanguage != "" {
		h.Set("Accept-Language", p.AcceptLanguage)
	}
	if p.AcceptEncoding != "" {
		h.Set("Accept-Encoding", p.AcceptEncoding)
	}
	h.Set("Sec-Fetch-Site", "cross-site")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Dest", "empty")
}

// SanitizeAndApply removes proxy headers and applies browser profile headers.
func SanitizeAndApply(h http.Header, p *Profile) {
	SanitizeHeaders(h)
	ApplyProfileHeaders(h, p)
}
