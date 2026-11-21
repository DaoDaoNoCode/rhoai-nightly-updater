package kube

import (
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/clock"
)

// clk is the time source of every wait in the cluster packages (polls, retry
// backoffs, grace periods) and of the deadlines that bound them. The tests
// replace it with a fake clock (SetClockForTest), so none waits in real
// time. Timestamps that are recorded or compared with the cluster's own
// (activity entries, object ages, cache ages) keep using the time package.
var clk clock.Clock = clock.Real

// Clock returns the time source of every wait of the cluster packages
// (clk).
func Clock() clock.Clock { return clk }

// SetClockForTest makes c the time source of every wait, for a test
// binary's TestMain before any test runs, and returns the function that
// puts the previous one back. It panics outside a test binary.
func SetClockForTest(c clock.Clock) (restore func()) {
	mustBeTest("SetClockForTest")
	prev := clk
	clk = c
	return func() { clk = prev }
}

// NowRFC3339 is the wall-clock time in UTC as RFC 3339, the form of every
// timestamp the cluster packages record (activity entries, records,
// annotations). Recorded times come from the time package, not clk.
func NowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
