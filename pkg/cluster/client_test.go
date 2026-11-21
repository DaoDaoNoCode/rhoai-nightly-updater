package cluster

import (
	"net/url"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// The API server's address comes from KUBERNETES_SERVICE_HOST and _PORT,
// which hold an IPv6 literal on an IPv6 (or IPv6-primary dual-stack)
// cluster. The URL must bracket it, as client-go's rest.InClusterConfig
// does with net.JoinHostPort.
func TestNewClientAPIServerURL(t *testing.T) {
	for _, tc := range []struct{ host, port, want string }{
		{"172.30.0.1", "443", "https://172.30.0.1:443"},
		{"fd02::1", "443", "https://[fd02::1]:443"},
		{"", "", "https://kubernetes.default.svc:443"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			t.Setenv("KUBERNETES_SERVICE_HOST", tc.host)
			t.Setenv("KUBERNETES_SERVICE_PORT", tc.port)
			c := kube.NewClientWithContext(t.Context(), "token")
			if c.BaseURL() != tc.want {
				t.Fatalf("baseURL = %q, want %q", c.BaseURL(), tc.want)
			}
			if u, err := url.Parse(c.BaseURL() + "/api"); err != nil || u.Port() != "443" {
				t.Fatalf("not a usable URL: %q (%v)", c.BaseURL(), err)
			}
		})
	}
}
