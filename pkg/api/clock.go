package api

import "github.com/juntwang/rhoai-nightly-updater/pkg/clock"

// clk is the time source of this package's waits and windows: the retries
// of a failed marker clear, the lease heartbeat and its loss threshold, and
// the mutation rate limit. Tests replace it with a fake clock
// (resetPackageState), so none of them waits in real time. Each user takes
// the clock when it starts (a retry loop, a heartbeat, a limiter), so a
// test's replacement never races a goroutine already running. The lease's
// heartbeatAt is stamped and compared with clk too: the heartbeat counts
// its loss threshold from that stamp, so both must be on one clock (in
// production clk is the wall clock, which other pods compare with). Other
// timestamps written to the cluster (operation start times, markers) keep
// using the time package.
var clk clock.Clock = clock.Real
