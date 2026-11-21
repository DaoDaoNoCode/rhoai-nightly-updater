package cluster

import (
	"net/http"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
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
			repo, digest, ok := registry.RepoAndDigest(tt.imageID)
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
	// on redirects (Quay returns 302 to S3 pre-signed URLs). The test
	// tested a copy of it before.
	req, _ := http.NewRequestWithContext(t.Context(), "GET", "https://example.com/blob", nil)
	req.Header.Set("Authorization", "Bearer test-token")

	err := registry.StripAuthOnRedirect(req, []*http.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth := req.Header.Get("Authorization"); auth != "" {
		t.Errorf("expected Authorization header to be stripped, got %q", auth)
	}
}

// The commit comes from vcs-ref first (git.commit as fallback) and the
// repository from git.url, vcs-url or io.openshift.build.source-location, in
// that order. OpenShift CI images have no git.url, and their
// org.opencontainers.image.revision is not the source commit, so it is never read.
func TestParseImageLabels(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   registry.ImageLabels
	}{
		{"konflux", map[string]string{
			"git.commit": "1111111111111111111111111111111111111111", "git.url": "https://github.com/opendatahub-io/odh-dashboard",
			"vcs-ref": "1111111111111111111111111111111111111111", "build-date": "2026-10-29T01:02:03Z", "version": "3.6.0",
		}, registry.ImageLabels{GitCommit: "1111111111111111111111111111111111111111", GitURL: "https://github.com/opendatahub-io/odh-dashboard", BuildDate: "2026-10-29T01:02:03Z", Version: "3.6.0"}},
		// build-date and version are the UBI base's: the build date is the config's created time.
		{"openshift-ci", map[string]string{
			"vcs-ref": "2222222222222222222222222222222222222222", "io.openshift.build.commit.id": "2222222222222222222222222222222222222222",
			"io.openshift.build.source-location": "https://github.com/opendatahub-io/odh-dashboard",
			"org.opencontainers.image.revision":  "9999999999999999999999999999999999999999", "build-date": "2026-10-29T04:05:06Z", "version": "9.8",
		}, registry.ImageLabels{GitCommit: "2222222222222222222222222222222222222222", GitURL: "https://github.com/opendatahub-io/odh-dashboard", BuildDate: "2026-10-29T22:55:20Z"}},
		// A PR build: the OpenShift CI keys are present but empty, so no commit.
		{"openshift-ci-pr-build", map[string]string{
			"vcs-ref": "", "io.openshift.build.commit.id": "", "io.openshift.build.source-location": "",
			"org.opencontainers.image.revision": "9999999999999999999999999999999999999999", "build-date": "2026-10-29T13:34:30Z", "version": "1",
		}, registry.ImageLabels{BuildDate: "2026-10-29T22:55:20Z"}},
		{"openshift-ci-without-created", map[string]string{
			"vcs-ref": "2222222222222222222222222222222222222222", "io.openshift.build.commit.id": "2222222222222222222222222222222222222222",
			"build-date": "2026-10-29T04:05:06Z", "version": "9.8",
		}, registry.ImageLabels{GitCommit: "2222222222222222222222222222222222222222"}},
		{"vcs-url-before-source-location", map[string]string{
			"git.commit": "3333333333333333333333333333333333333333", "vcs-url": "https://github.com/example/vcs",
			"io.openshift.build.source-location": "https://github.com/example/openshift",
		}, registry.ImageLabels{GitCommit: "3333333333333333333333333333333333333333", GitURL: "https://github.com/example/vcs"}},
		{"git-url-wins", map[string]string{
			"git.commit": "4444444444444444444444444444444444444444", "git.url": "https://github.com/example/git", "vcs-url": "https://github.com/example/vcs",
		}, registry.ImageLabels{GitCommit: "4444444444444444444444444444444444444444", GitURL: "https://github.com/example/git"}},
		{"revision-alone-is-not-a-commit", map[string]string{"org.opencontainers.image.revision": "5555555555555555555555555555555555555555"}, registry.ImageLabels{}},
		// The UI links the repository: an image's labels cannot make it a
		// javascript: or plain http link; the next https label is used.
		{"javascript-url-dropped", map[string]string{
			"git.commit": "6666666666666666666666666666666666666666", "git.url": "javascript:alert(document.cookie)//",
		}, registry.ImageLabels{GitCommit: "6666666666666666666666666666666666666666"}},
		{"http-url-dropped-for-the-next-https-one", map[string]string{
			"git.commit": "6666666666666666666666666666666666666666", "git.url": "http://github.com/example/git", "vcs-url": "https://github.com/example/vcs",
		}, registry.ImageLabels{GitCommit: "6666666666666666666666666666666666666666", GitURL: "https://github.com/example/vcs"}},
		{"url-with-user-info-dropped", map[string]string{
			"git.commit": "6666666666666666666666666666666666666666", "git.url": "https://github.com@evil.example/x",
		}, registry.ImageLabels{GitCommit: "6666666666666666666666666666666666666666"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			created := "2026-10-29T22:55:20.781805405Z"
			if tc.name == "openshift-ci-without-created" {
				created = ""
			}
			if got := registry.ParseImageLabels(tc.labels, created); *got != tc.want {
				t.Fatalf("got %+v, want %+v", *got, tc.want)
			}
		})
	}
}

// Labels are always read from quay.io, so the registry is matched as a
// prefix: a reference that only contains "rhoai/" or
// "quay.io/opendatahub/" somewhere is not a build image.
func TestBuildImageRegistryPrefix(t *testing.T) {
	digest := "@sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		ref          string
		rhoai, build bool
	}{
		{"registry.redhat.io/rhoai/odh-dashboard-rhel9" + digest, true, true},
		{"quay.io/rhoai/odh-dashboard-rhel9:rhoai-3.6", true, true},
		{"docker-pullable://registry.redhat.io/rhoai/odh-dashboard-rhel9" + digest, true, true},
		{"quay.io/opendatahub/odh-dashboard:main", false, true},
		{"quay.io.evil.example/rhoai/odh-dashboard-rhel9:x", false, false},
		{"evil.example/x/rhoai/odh-dashboard-rhel9:x", false, false},
		{"quay.io/someone/rhoai/odh-dashboard-rhel9:x", false, false},
		{"evil.example/quay.io/opendatahub/odh-dashboard:main", false, false},
		{"registry.access.redhat.com/ubi9/nginx-124:latest", false, false},
	} {
		if got := registry.IsRHOAIImage(tc.ref); got != tc.rhoai {
			t.Errorf("isRHOAIImage(%q) = %v, want %v", tc.ref, got, tc.rhoai)
		}
		if got := components.IsBuildImage(tc.ref); got != tc.build {
			t.Errorf("isBuildImage(%q) = %v, want %v", tc.ref, got, tc.build)
		}
	}
}
