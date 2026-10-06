package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
)

// inCluster runs a test like the deployed pod: no DEV_MODE and a cached
// ServiceAccount token.
func inCluster(t *testing.T) {
	t.Helper()
	setupDevMode(t)
	t.Setenv("DEV_MODE", "")
	t.Setenv("DEV_TOKEN", "")
	cachedTokenMu.Lock()
	cachedTokenValue, cachedTokenAt = "sa-token", time.Now()
	cachedTokenMu.Unlock()
	t.Cleanup(func() {
		cachedTokenMu.Lock()
		cachedTokenValue = ""
		cachedTokenMu.Unlock()
	})
}

// TestOnlyTheProxySessionTokenIsTrusted: in the cluster oauth-proxy sends
// X-Forwarded-Access-Token and overwrites Authorization with Basic
// credentials (--pass-basic-auth defaults to true), so a bearer token did
// not come through the proxy and is refused. DEV_MODE keeps accepting one.
func TestOnlyTheProxySessionTokenIsTrusted(t *testing.T) {
	for _, tc := range []struct {
		name            string
		dev             bool
		forwarded, auth string
		status          int
		user            string
	}{
		{"proxy request", false, "user:alice", "Basic YWxpY2U6", 200, "alice"},
		{"bearer only", false, "", "Bearer user:mallory", 401, ""},
		{"bearer next to the proxy token", false, "user:alice", "Bearer user:mallory", 401, ""},
		{"lower-case bearer", false, "", "bearer user:mallory", 401, ""},
		{"no token", false, "", "", 401, ""},
		{"dev bearer", true, "", "Bearer user:dev", 200, "dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.dev {
				setupDevMode(t)
			} else {
				inCluster(t)
			}
			for _, wrapper := range []string{"withAuth", "withMutationAuth"} {
				var got string
				fn := func(_ *cluster.Client, w http.ResponseWriter, r *http.Request) {
					got = resolveUsername(r)
					w.WriteHeader(http.StatusOK)
				}
				h := withAuth(fn)
				if wrapper == "withMutationAuth" {
					allowMutations(t)
					h = withMutationAuth(fn)
				}
				r := postJSON("/api/proxy-token-"+wrapper, tc.forwarded, "{}")
				if tc.auth != "" {
					r.Header.Set("Authorization", tc.auth)
				}
				w := httptest.NewRecorder()
				h(w, r)
				if w.Code != tc.status || got != tc.user {
					t.Errorf("%s: status %d user %q: %s", wrapper, w.Code, got, w.Body.String())
				}
				if tc.status == 401 && decodeError(t, w)["errorCode"] != "unauthorized" {
					t.Errorf("%s: %s", wrapper, w.Body.String())
				}
				mutationLimiter = newRateLimiter(30 * time.Second)
			}
		})
	}
}

// TestOAuthProxyForwardsOnlyItsOwnSessionToken pins the oauth-proxy flags
// that the mutation gate relies on (see checkPermission): the proxy passes
// the session's token, and accepts no client bearer tokens.
func TestOAuthProxyForwardsOnlyItsOwnSessionToken(t *testing.T) {
	data, err := os.ReadFile("../../deploy/template.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var tmpl struct {
		Objects []struct {
			Kind string `yaml:"kind"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Name string   `yaml:"name"`
							Args []string `yaml:"args"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		} `yaml:"objects"`
	}
	if err := yaml.Unmarshal(data, &tmpl); err != nil {
		t.Fatal(err)
	}
	var args []string
	for _, o := range tmpl.Objects {
		if o.Kind != "Deployment" {
			continue
		}
		for _, c := range o.Spec.Template.Spec.Containers {
			if c.Name == "oauth-proxy" {
				args = c.Args
			}
		}
	}
	if len(args) == 0 {
		t.Fatal("no oauth-proxy container in the template")
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--pass-access-token=true") {
		t.Error("oauth-proxy must pass the session's token (--pass-access-token=true)")
	}
	for _, forbidden := range []string{"--openshift-delegate-urls", "--pass-user-bearer-token", "--skip-auth-regex", "--htpasswd-file"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("oauth-proxy sets %s, which lets requests through without this tool's own OAuth session", forbidden)
		}
	}
}
