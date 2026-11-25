package record

import (
	"testing"
	"time"
)

// SetActivityWriteTimeoutForTest makes d the bound of one Activity
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

// SetActivityBackoffForTest makes backoff the wait before each retry of
// an activity write after a conflict, and returns the function that puts
// the previous one back. It panics outside a test binary.
func SetActivityBackoffForTest(backoff func(attempt int) time.Duration) (restore func()) {
	mustBeTest("SetActivityBackoffForTest")
	prev := activityBackoff
	activityBackoff = backoff
	return func() { activityBackoff = prev }
}

// MaxActivityAttempts is how many times one activity write is tried.
const MaxActivityAttempts = maxConflictRetries

func mustBeTest(what string) {
	if !testing.Testing() {
		panic(what + " is only for tests")
	}
}

// SetLockBackoffForTest makes backoff the wait before each retry of a
// conditional write of the operation record after a conflict, and returns
// the function that puts the previous one back. It panics outside a test
// binary.
func SetLockBackoffForTest(backoff func(attempt int) time.Duration) (restore func()) {
	mustBeTest("SetLockBackoffForTest")
	prev := lockBackoff
	lockBackoff = backoff
	return func() { lockBackoff = prev }
}

// SetBeforeActivityWaitForTest makes fn run before each activity entry
// waits for this process's other entries, and returns the function that
// puts the previous one back. It panics outside a test binary.
func SetBeforeActivityWaitForTest(fn func()) (restore func()) {
	mustBeTest("SetBeforeActivityWaitForTest")
	prev := beforeActivityWait
	beforeActivityWait = fn
	return func() { beforeActivityWait = prev }
}
