package middleware

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// LocalOnly rejects requests whose Host or Origin is not this machine on one
// of the given ports. The DEV_MODE backend acts with the developer's own
// token on every request, so a website that rebinds its DNS name to
// 127.0.0.1 (DNS rebinding) must not be able to drive it: such requests
// carry the attacker's host name in Host and Origin.
func LocalOnly(ports []string, next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, p := range ports {
		for _, h := range []string{"127.0.0.1", "localhost", "::1"} {
			allowed[net.JoinHostPort(h, p)] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[strings.ToLower(r.Host)] {
			writeForbidden(w, "invalid Host header")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !allowed[strings.ToLower(hostWithPort(u))] {
				writeForbidden(w, "invalid Origin header")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func hostWithPort(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	if u.Scheme == "https" {
		return net.JoinHostPort(u.Hostname(), "443")
	}
	return net.JoinHostPort(u.Hostname(), "80")
}

func writeForbidden(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"error":"` + msg + `","errorCode":"forbidden_host"}`))
}
