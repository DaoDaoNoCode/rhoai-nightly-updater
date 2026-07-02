package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

const requestIDHeader = "X-Request-Id"

// RequestID wraps an http.Handler and assigns a unique request ID to every
// request. If the incoming request already carries an X-Request-Id header
// (e.g., from a load balancer), it is preserved; otherwise a new random
// hex ID is generated. The ID is set on the response header in both cases.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if id == "" {
			id = generateID()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r)
	})
}

func generateID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
