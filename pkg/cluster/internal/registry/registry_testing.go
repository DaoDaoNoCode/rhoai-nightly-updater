package registry

import (
	"net/http"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// Test hooks of the registry and GitHub reads. Each panics outside a test
// binary; those that change something return the function that puts the
// previous value back.

// SetHTTPClientForTest makes c the client of every Quay and GitHub call.
func SetHTTPClientForTest(c *http.Client) (restore func()) {
	mustBeTest("SetHTTPClientForTest")
	prev := quayHTTPClient
	quayHTTPClient = c
	return func() { quayHTTPClient = prev }
}

// SetCopyFromPullSecretForTest makes f the copy the flows make for the
// internal registry (EnsureRegistryPullCopyFromPullSecret).
func SetCopyFromPullSecretForTest(f func(c *kube.Client) ([]string, string, string)) (restore func()) {
	mustBeTest("SetCopyFromPullSecretForTest")
	prev := copyFromPullSecret
	copyFromPullSecret = f
	return func() { copyFromPullSecret = prev }
}

// SetVerifyQuayCredentialsForTest makes verify the check of the pull
// secret's quay.io/rhoai credential (instead of asking Quay).
func SetVerifyQuayCredentialsForTest(verify func(basicAuth string) error) (restore func()) {
	mustBeTest("SetVerifyQuayCredentialsForTest")
	prev := verifyQuayCredentials
	verifyQuayCredentials = verify
	return func() { verifyQuayCredentials = prev }
}

// ResetQuayCredentialCacheForTest forgets every cached Quay verdict on a
// credential.
func ResetQuayCredentialCacheForTest() {
	mustBeTest("ResetQuayCredentialCacheForTest")
	quayCredentialCacheMu.Lock()
	quayCredentialCache = map[[32]byte]quayCredentialVerdict{}
	quayCredentialCacheMu.Unlock()
}

// SetNightlyStatusFreshForTest makes d how long a nightly comparison is
// served without a refresh.
func SetNightlyStatusFreshForTest(d time.Duration) (restore func()) {
	mustBeTest("SetNightlyStatusFreshForTest")
	prev := nightlyStatusFresh
	nightlyStatusFresh = d
	return func() { nightlyStatusFresh = prev }
}

// SetNightlyStatusStaleMaxForTest makes d the age past which a cached
// nightly comparison is not shown.
func SetNightlyStatusStaleMaxForTest(d time.Duration) (restore func()) {
	mustBeTest("SetNightlyStatusStaleMaxForTest")
	prev := nightlyStatusStaleMax
	nightlyStatusStaleMax = d
	return func() { nightlyStatusStaleMax = prev }
}

// WaitBackgroundForTest waits for the nightly status refreshes and dashboard
// commit reads running in the background.
func WaitBackgroundForTest() {
	mustBeTest("WaitBackgroundForTest")
	nightlyBackground.Wait()
}

// ResetTagScanForTest forgets the cached release tag scan.
func ResetTagScanForTest() {
	mustBeTest("ResetTagScanForTest")
	tagScanCacheMu.Lock()
	tagScanCache = nil
	tagScanCacheMu.Unlock()
}

// TagScanLenForTest is the number of tags in the cached release tag scan.
func TagScanLenForTest() int {
	mustBeTest("TagScanLenForTest")
	tagScanCacheMu.RLock()
	defer tagScanCacheMu.RUnlock()
	return len(tagScanCache)
}

// ResetCachesForTest empties every registry and GitHub cache, the GitHub
// rate-limit backoff and the release tag scan.
func ResetCachesForTest() {
	mustBeTest("ResetCachesForTest")
	QuayTokenCache.Purge()
	LabelCache.Purge()
	commitDateCache.Purge()
	TagDigestCache.Purge()
	FBCContentCache.Purge()
	nightlyStatusCache.Purge()
	dashboardWarmAttempts.Purge()
	prCache.Purge()
	compareCache.Purge()
	commitCache.Purge()
	ComponentImageCache.Purge()
	branchHeadCache.Purge()
	githubBackoff.Lock()
	githubBackoff.until = time.Time{}
	githubBackoff.Unlock()
	tagScanCacheMu.Lock()
	tagScanCache = nil
	tagScanCacheMu.Unlock()
}

func mustBeTest(what string) {
	if !testing.Testing() {
		panic(what + " is only for tests")
	}
}
