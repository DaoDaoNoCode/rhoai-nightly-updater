package cluster

import "github.com/juntwang/rhoai-nightly-updater/pkg/clock"

// clk is the time source of every wait in this package (polls, retry
// backoffs, grace periods) and of the deadlines that bound them. The tests
// replace it with a fake clock (testmain_test.go), so none waits in real
// time. Timestamps that are recorded or compared with the cluster's own
// (activity entries, object ages, cache ages) keep using the time package.
var clk clock.Clock = clock.Real
