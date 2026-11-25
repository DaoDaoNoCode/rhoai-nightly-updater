package observability

import (
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// Test hooks. Each panics outside a test binary.

// RestoreMaaSExtrasForTest is restoreMaaSExtras.
func RestoreMaaSExtrasForTest(c *kube.Client) (logs []string, problem string) {
	if !testing.Testing() {
		panic("RestoreMaaSExtrasForTest is only for tests")
	}
	return restoreMaaSExtras(c)
}

// SetDashDevPausedForTest makes a recorded Dashboard Dev session keep the
// dashboard operator operator (namespace/name; "" for none) at 0 replicas
// for the portal's note, and returns the function that puts the real check
// back.
func SetDashDevPausedForTest(operator string) (restore func()) {
	if !testing.Testing() {
		panic("SetDashDevPausedForTest is only for tests")
	}
	prev := dashDevPausedOperator
	dashDevPausedOperator = func(*kube.Client) string { return operator }
	return func() { dashDevPausedOperator = prev }
}
