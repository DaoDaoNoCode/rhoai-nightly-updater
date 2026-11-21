package operator

import (
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/catalog"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// UseTestTimings makes the operation waits short, for a test binary: the
// catalog's (catalog.UseTestTimings), the InstallPlan poll 100ms every 10ms,
// the propagation, refresh clean-up and channel retry waits 10ms, the
// Subscription retry backoffs 10ms and 10ms, the install wait 400ms, the
// ResolutionFailed, bundle pull and CSV Failed graces 60ms, the CSV
// Succeeded settle 30ms and the CSV deletion wait 50ms. It panics outside a
// test binary.
func UseTestTimings() {
	mustBeTest("UseTestTimings")
	catalog.UseTestTimings()
	InstallPlanPollTimeout = 100 * time.Millisecond
	InstallPlanPollInterval = 10 * time.Millisecond
	PropagationWait = 10 * time.Millisecond
	RefreshCleanupWait = 10 * time.Millisecond
	SubRetryBackoffs = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	ChannelRetryDelay = 10 * time.Millisecond
	InstallTimeout = 400 * time.Millisecond
	ResolutionFailedGrace = 60 * time.Millisecond
	BundlePullFailureGrace = 60 * time.Millisecond
	CSVFailedGrace = 60 * time.Millisecond
	CSVSucceededSettle = 30 * time.Millisecond
	CSVDeletionTimeout = 50 * time.Millisecond
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
	prevWait, prevPoll := UpgradeGateWait, UpgradeGatePoll
	UpgradeGateWait, UpgradeGatePoll = wait, poll
	return func() { UpgradeGateWait, UpgradeGatePoll = prevWait, prevPoll }
}

// SetCSVDeletionTimeoutForTest makes d the bound of the wait for a deleted
// CSV to disappear and returns the function that puts the previous one
// back. It panics outside a test binary.
func SetCSVDeletionTimeoutForTest(d time.Duration) (restore func()) {
	mustBeTest("SetCSVDeletionTimeoutForTest")
	prev := CSVDeletionTimeout
	CSVDeletionTimeout = d
	return func() { CSVDeletionTimeout = prev }
}

// SetInstallTimeoutForTest makes d the bound of the wait for OLM to install
// the new operator and returns the function that puts the previous one
// back. It panics outside a test binary.
func SetInstallTimeoutForTest(d time.Duration) (restore func()) {
	mustBeTest("SetInstallTimeoutForTest")
	prev := InstallTimeout
	InstallTimeout = d
	return func() { InstallTimeout = prev }
}

func mustBeTest(what string) {
	if !testing.Testing() {
		panic(what + " is only for tests")
	}
}
