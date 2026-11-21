package operator

import (
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/catalog"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// UseTestTimings makes the operation waits short, for a test binary: the
// catalog's (catalog.UseTestTimings), the InstallPlan poll every 10ms,
// the propagation, refresh clean-up and channel retry waits 10ms, the
// Subscription retry backoffs 10ms and 10ms, the install wait 400ms, the
// ResolutionFailed, bundle pull and CSV Failed graces 60ms, the CSV
// Succeeded settle 30ms and the CSV deletion wait 50ms. It panics outside a
// test binary.
func UseTestTimings() {
	mustBeTest("UseTestTimings")
	catalog.UseTestTimings()
	InstallPlanPollInterval = 10 * time.Millisecond
	propagationWait = 10 * time.Millisecond
	refreshCleanupWait = 10 * time.Millisecond
	SubRetryBackoffs = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	channelRetryDelay = 10 * time.Millisecond
	installTimeout = 400 * time.Millisecond
	resolutionFailedGrace = 60 * time.Millisecond
	bundlePullFailureGrace = 60 * time.Millisecond
	csvFailedGrace = 60 * time.Millisecond
	CSVSucceededSettle = 30 * time.Millisecond
	csvDeletionTimeout = 50 * time.Millisecond
}

// SetLookupTargetBundleForTest makes lookup the read of the bundle an FBC
// image ships (instead of the registry) and returns the function that puts
// the previous one back. It panics outside a test binary.
func SetLookupTargetBundleForTest(lookup func(c *kube.Client, image string) (string, error)) (restore func()) {
	mustBeTest("SetLookupTargetBundleForTest")
	prev := lookupTargetBundle
	lookupTargetBundle = lookup
	return func() { lookupTargetBundle = prev }
}

// SetUpgradeGateTimingForTest bounds the wait for the new operator's
// upgrade gates by wait, polling every poll, and returns the function that
// puts the previous bounds back. It panics outside a test binary.
func SetUpgradeGateTimingForTest(wait, poll time.Duration) (restore func()) {
	mustBeTest("SetUpgradeGateTimingForTest")
	prevWait, prevPoll := UpgradeGateWait, upgradeGatePoll
	UpgradeGateWait, upgradeGatePoll = wait, poll
	return func() { UpgradeGateWait, upgradeGatePoll = prevWait, prevPoll }
}

// SetCSVDeletionTimeoutForTest makes d the bound of the wait for a deleted
// CSV to disappear and returns the function that puts the previous one
// back. It panics outside a test binary.
func SetCSVDeletionTimeoutForTest(d time.Duration) (restore func()) {
	mustBeTest("SetCSVDeletionTimeoutForTest")
	prev := csvDeletionTimeout
	csvDeletionTimeout = d
	return func() { csvDeletionTimeout = prev }
}

// SetInstallTimeoutForTest makes d the bound of the wait for OLM to install
// the new operator and returns the function that puts the previous one
// back. It panics outside a test binary.
func SetInstallTimeoutForTest(d time.Duration) (restore func()) {
	mustBeTest("SetInstallTimeoutForTest")
	prev := installTimeout
	installTimeout = d
	return func() { installTimeout = prev }
}

func mustBeTest(what string) {
	if !testing.Testing() {
		panic(what + " is only for tests")
	}
}
