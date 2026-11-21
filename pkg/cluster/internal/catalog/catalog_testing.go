package catalog

import (
	"testing"
	"time"
)

// UseTestTimings makes the catalog waits short, for a test binary: the
// READY timeout 100ms, the poll interval and the PackageManifest wait 10ms,
// the image pull grace 30ms. It panics outside a test binary.
func UseTestTimings() {
	mustBeTest("UseTestTimings")
	ReadyTimeout = 100 * time.Millisecond
	PollInterval = 10 * time.Millisecond
	PackageManifestPropagationWait = 10 * time.Millisecond
	ImagePullGrace = 30 * time.Millisecond
}

// SetReadyTimeoutForTest makes d how long a CatalogSource gets to become
// READY and returns the function that puts the previous one back. It panics
// outside a test binary.
func SetReadyTimeoutForTest(d time.Duration) (restore func()) {
	mustBeTest("SetReadyTimeoutForTest")
	prev := ReadyTimeout
	ReadyTimeout = d
	return func() { ReadyTimeout = prev }
}

func mustBeTest(what string) {
	if !testing.Testing() {
		panic(what + " is only for tests")
	}
}
