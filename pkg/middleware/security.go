package middleware

import "net/http"

// ContentSecurityPolicy is the app's CSP. default-src does not cover
// base-uri, form-action or frame-ancestors, so they are set explicitly: no
// <base> element (the app has none), forms submit only to this origin (the
// app's forms post nothing elsewhere), no framing (as X-Frame-Options:
// DENY), and no plugins.
const ContentSecurityPolicy = "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
	"base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'"

// SecurityHeaders wraps an http.Handler and sets security-related
// response headers on every request. Handlers may replace Cache-Control:
// the static files (StaticFiles) and the event streams do.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", ContentSecurityPolicy)
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains; preload")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		next.ServeHTTP(w, r)
	})
}
