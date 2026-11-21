package diagnostics

import (
	"testing"
	"time"
)

// Test hooks of the diagnostics checks. Each panics outside a test binary.

// SetWallClockForTest makes now the wall clock the checks measure object
// ages against (wallNow), for a test whose objects carry fixed dates, and
// returns the function that puts time.Now back.
func SetWallClockForTest(now func() time.Time) (restore func()) {
	mustBeTest("SetWallClockForTest")
	prev := wallNow
	wallNow = now
	return func() { wallNow = prev }
}

// mustBeTest panics unless the program is a test binary.
func mustBeTest(what string) {
	if !testing.Testing() {
		panic(what + " is only for tests")
	}
}
