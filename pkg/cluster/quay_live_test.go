package cluster

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Explicit opt-in: reads the current cluster pull secret in memory and makes
// registry GET/HEAD requests. It never changes cluster or registry resources.
func TestReleaseDiscoveryLiveQuay(t *testing.T) {
	if os.Getenv("RHOAI_QUAY_LIVE_TEST") != "1" {
		t.Skip("set RHOAI_QUAY_LIVE_TEST=1 with an authenticated oc session")
	}
	resetReleaseTagCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	secret, err := exec.CommandContext(ctx, "oc", "get", "secret", "additional-pull-secret", "-n", "kube-system", "-o", "json").Output()
	if err != nil {
		t.Fatalf("read pull secret: %v", err)
	}
	c, cleanup := newMockClient(map[string]mockResponse{
		namespacedPath("v1", "secrets", "kube-system", "additional-pull-secret"): {body: string(secret)},
	})
	defer cleanup()
	var calls atomic.Int32
	original := quayHTTPClient
	counted := &http.Client{Timeout: original.Timeout, Transport: dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return original.Transport.RoundTrip(r)
	})}
	quayHTTPClient = counted
	t.Cleanup(func() { quayHTTPClient = original })
	started := time.Now()
	latest, err := FetchLatestNightly(ctx, c)
	if err != nil {
		t.Fatalf("cold latest lookup: %v", err)
	}
	if !strings.Contains(latest.Image, "@sha256:") {
		t.Fatalf("latest image is not digest pinned: %s", latest.Image)
	}
	t.Logf("cold latest: %s in %s (%d registry requests)", latest.Tag, time.Since(started), calls.Load())
	all, err := FetchNightlyTags(ctx, c, 0)
	if err != nil || len(all.Tags) == 0 {
		t.Fatalf("all versions: err=%v", err)
	}
	var got []string
	for _, tag := range all.Tags {
		got = append(got, tag.Tag)
		if !strings.Contains(tag.Image, "@sha256:") {
			t.Fatalf("version is not digest pinned: %s", tag.Tag)
		}
	}
	t.Logf("all versions: %d digest-pinned release aliases", len(got))

	t.Logf("restored version list: %v", got)
}
