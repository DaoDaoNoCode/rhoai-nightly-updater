package serving

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Test hooks for models, the inference gateway and MaaS. Each sets what a
// test needs and returns the function that puts the previous value back;
// each panics outside a test binary.

// SetRecordPruneActivityForTest makes record receive the activity entries
// of the background MaaS prune.
func SetRecordPruneActivityForTest(record func(*kube.Client, types.ActivityEntry)) (restore func()) {
	mustBeTest("SetRecordPruneActivityForTest")
	prev := recordPruneActivity
	recordPruneActivity = record
	return func() { recordPruneActivity = prev }
}

// SetModelReadyTimeoutForTest sets how long a model setup waits for Ready.
func SetModelReadyTimeoutForTest(d time.Duration) (restore func()) {
	mustBeTest("SetModelReadyTimeoutForTest")
	prev := ModelReadyTimeout
	ModelReadyTimeout = d
	return func() { ModelReadyTimeout = prev }
}

// SetModelCallHTTPClientForTest makes client send the model test calls.
func SetModelCallHTTPClientForTest(client *http.Client) (restore func()) {
	mustBeTest("SetModelCallHTTPClientForTest")
	prev := modelCallHTTPClient
	modelCallHTTPClient = client
	return func() { modelCallHTTPClient = prev }
}

// SetLookupReleasedImageForTest makes lookup find the newest released
// image of a repository.
func SetLookupReleasedImageForTest(lookup func(ctx context.Context, repo string) (ReleasedImage, error)) (restore func()) {
	mustBeTest("SetLookupReleasedImageForTest")
	prev := lookupReleasedImage
	lookupReleasedImage = lookup
	return func() { lookupReleasedImage = prev }
}

// SetRedHatCatalogURLForTest sets the Red Hat container catalog's API URL.
func SetRedHatCatalogURLForTest(url string) (restore func()) {
	mustBeTest("SetRedHatCatalogURLForTest")
	prev := redHatCatalogURL
	redHatCatalogURL = url
	return func() { redHatCatalogURL = prev }
}

// mustBeTest panics unless the program is a test binary.
func mustBeTest(what string) {
	if !testing.Testing() {
		panic(what + " is only for tests")
	}
}
