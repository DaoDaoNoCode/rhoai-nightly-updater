package dashdev

import (
	"testing"
	"time"
)

// SetOperatorTimingForTest bounds RevertForOperation's
// wait for the dashboard operator by timeout, polling every interval, and
// returns the function that puts the previous bounds back. It panics
// outside a test binary.
func SetOperatorTimingForTest(timeout, interval time.Duration) (restore func()) {
	mustBeTest("setDashboardOperatorTimingForTest")
	prevTimeout, prevInterval := dashboardOperatorReadyTimeout, dashboardOperatorPollInterval
	dashboardOperatorReadyTimeout, dashboardOperatorPollInterval = timeout, interval
	return func() { dashboardOperatorReadyTimeout, dashboardOperatorPollInterval = prevTimeout, prevInterval }
}

// SetBuildContainerTimeoutForTest makes d the bound of one
// container's build lookup and returns the function that puts the previous
// one back. It panics outside a test binary.
func SetBuildContainerTimeoutForTest(d time.Duration) (restore func()) {
	mustBeTest("setDashboardBuildContainerTimeoutForTest")
	prev := BuildContainerTimeout
	BuildContainerTimeout = d
	return func() { BuildContainerTimeout = prev }
}

func mustBeTest(what string) {
	if !testing.Testing() {
		panic(what + " is only for tests")
	}
}

// SetDeploymentsReadyTimingForTest sets WaitDeploymentsReady's timeout and
// poll and returns the function that puts the previous ones back. It
// panics outside a test binary.
func SetDeploymentsReadyTimingForTest(timeout, poll time.Duration) (restore func()) {
	mustBeTest("SetDeploymentsReadyTimingForTest")
	prevTimeout, prevPoll := DeploymentsReadyTimeout, deploymentsReadyPoll
	DeploymentsReadyTimeout, deploymentsReadyPoll = timeout, poll
	return func() { DeploymentsReadyTimeout, deploymentsReadyPoll = prevTimeout, prevPoll }
}
