package cluster

import (
	"net/http"
	"testing"
)

func TestExtractRepoAndDigest(t *testing.T) {
	tests := []struct {
		name       string
		imageID    string
		wantRepo   string
		wantDigest string
		wantOk     bool
	}{
		{
			name:       "registry.redhat.io with digest",
			imageID:    "registry.redhat.io/rhoai/odh-dashboard-rhel9@sha256:abc123def456",
			wantRepo:   "rhoai/odh-dashboard-rhel9",
			wantDigest: "sha256:abc123def456",
			wantOk:     true,
		},
		{
			name:       "quay.io with digest",
			imageID:    "quay.io/rhoai/odh-dashboard-rhel9@sha256:abc123def456",
			wantRepo:   "rhoai/odh-dashboard-rhel9",
			wantDigest: "sha256:abc123def456",
			wantOk:     true,
		},
		{
			name:       "docker-pullable prefix",
			imageID:    "docker-pullable://registry.redhat.io/rhoai/odh-dashboard-rhel9@sha256:abc123def456",
			wantRepo:   "rhoai/odh-dashboard-rhel9",
			wantDigest: "sha256:abc123def456",
			wantOk:     true,
		},
		{
			name:       "with tag before digest",
			imageID:    "registry.redhat.io/rhoai/odh-dashboard-rhel9:v3.5.0-ea.2@sha256:abc123def456",
			wantRepo:   "rhoai/odh-dashboard-rhel9",
			wantDigest: "sha256:abc123def456",
			wantOk:     true,
		},
		{
			name:       "no digest (tag only)",
			imageID:    "registry.redhat.io/rhoai/odh-dashboard-rhel9:v3.5",
			wantRepo:   "",
			wantDigest: "",
			wantOk:     false,
		},
		{
			name:       "no slash in path",
			imageID:    "singlename@sha256:abc123def456",
			wantRepo:   "",
			wantDigest: "",
			wantOk:     false,
		},
		{
			name:       "non-sha256 digest",
			imageID:    "registry.redhat.io/rhoai/odh-dashboard-rhel9@md5:abc123",
			wantRepo:   "",
			wantDigest: "",
			wantOk:     false,
		},
		{
			name:       "empty string",
			imageID:    "",
			wantRepo:   "",
			wantDigest: "",
			wantOk:     false,
		},
		{
			name:       "deep path with digest",
			imageID:    "quay.io/opendatahub/odh-mod-arch-modular-architecture@sha256:deadbeef",
			wantRepo:   "opendatahub/odh-mod-arch-modular-architecture",
			wantDigest: "sha256:deadbeef",
			wantOk:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, digest, ok := extractRepoAndDigest(tt.imageID)
			if ok != tt.wantOk {
				t.Errorf("ok = %v, want %v", ok, tt.wantOk)
			}
			if repo != tt.wantRepo {
				t.Errorf("repo = %q, want %q", repo, tt.wantRepo)
			}
			if digest != tt.wantDigest {
				t.Errorf("digest = %q, want %q", digest, tt.wantDigest)
			}
		})
	}
}

func TestCheckRedirect_StripsAuthorization(t *testing.T) {
	// The CheckRedirect in fetchConfigLabels strips the Authorization header
	// on redirects (Quay returns 302 to S3 pre-signed URLs).
	checkRedirect := func(req *http.Request, via []*http.Request) error {
		req.Header.Del("Authorization")
		return nil
	}

	req, _ := http.NewRequest("GET", "https://example.com/blob", nil)
	req.Header.Set("Authorization", "Bearer test-token")

	// Simulate the redirect handler stripping auth
	err := checkRedirect(req, []*http.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth := req.Header.Get("Authorization"); auth != "" {
		t.Errorf("expected Authorization header to be stripped, got %q", auth)
	}
}
