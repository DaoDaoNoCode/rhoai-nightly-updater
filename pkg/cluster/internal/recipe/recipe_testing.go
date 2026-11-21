package recipe

import (
	"testing"
	"time"
)

// SetDSCTimingForTest sets the DataScienceCluster step's create retry, its
// delay, and the wait for conditions with its poll, and returns the
// function that puts the previous values back. It panics outside a test
// binary.
func SetDSCTimingForTest(retry, retryDelay, wait, poll time.Duration) (restore func()) {
	if !testing.Testing() {
		panic("SetDSCTimingForTest is only for tests")
	}
	pr, pd, pw, pp := dscCreateRetry, dscCreateRetryDelay, dscConditionsWait, dscConditionsPoll
	dscCreateRetry, dscCreateRetryDelay, dscConditionsWait, dscConditionsPoll = retry, retryDelay, wait, poll
	return func() { dscCreateRetry, dscCreateRetryDelay, dscConditionsWait, dscConditionsPoll = pr, pd, pw, pp }
}
