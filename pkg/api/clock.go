package api

import "github.com/juntwang/rhoai-nightly-updater/pkg/clock"

// clk is the time source of this package's waits and windows: the retries
// of a failed marker clear, the lease heartbeat and its loss threshold, and
// the mutation rate limit. Tests replace it with a fake clock
// (resetPackageState), so none of them waits in real time. Each user takes
// the clock when it starts (a retry loop, a heartbeat, a limiter), so a
// test's replacement never races a goroutine already running. Timestamps
// written to the cluster or compared with other pods' (lease heartbeatAt,
// operation start times) keep using the time package.
var clk clock.Clock = clock.Real
