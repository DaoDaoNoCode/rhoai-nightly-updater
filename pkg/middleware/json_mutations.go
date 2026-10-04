package middleware

import (
	"mime"
	"net/http"
	"strings"
)

// RequireJSONForMutations rejects state-changing /api/ requests whose
// Content-Type is not application/json. Browsers can send cross-site POSTs
// without a CORS preflight only with "simple" content types, so this blocks
// cross-site request forgery even where no session cookie protects the API
// (for example the local DEV_MODE backend that falls back to DEV_TOKEN).
func RequireJSONForMutations(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if strings.HasPrefix(r.URL.Path, "/api/") {
				mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
				if err != nil || mediaType != "application/json" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnsupportedMediaType)
					_, _ = w.Write([]byte(`{"error":"requests that change state must use Content-Type: application/json","errorCode":"unsupported_media_type"}`))
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
