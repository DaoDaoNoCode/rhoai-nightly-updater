package record

import (
	"testing"
	"time"
)

// SetActivityWriteTimeoutForTest makes d the bound of one RecordActivity
// call and returns the function that puts the previous one back. It panics
// outside a test binary.
func SetActivityWriteTimeoutForTest(d time.Duration) (restore func()) {
	mustBeTest("SetActivityWriteTimeoutForTest")
	prev := ActivityWriteTimeout
	ActivityWriteTimeout = d
	return func() { ActivityWriteTimeout = prev }
}

// SetRestoreCSVBudgetForTest makes d the restore's CSV deletion budget and
// returns the function that puts the previous one back. It panics outside a
// test binary.
func SetRestoreCSVBudgetForTest(d time.Duration) (restore func()) {
	mustBeTest("SetRestoreCSVBudgetForTest")
	prev := RestoreCSVBudget
	RestoreCSVBudget = d
	return func() { RestoreCSVBudget = prev }
}

func mustBeTest(what string) {
	if !testing.Testing() {
		panic(what + " is only for tests")
	}
}
