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

// SetDashDevActiveForTest makes a Dashboard Dev session active with the
// dashboard operator operator (namespace/name; "" for none) for the portal's
// note, and returns the function that puts the real check back.
func SetDashDevActiveForTest(operator string) (restore func()) {
	if !testing.Testing() {
		panic("SetDashDevActiveForTest is only for tests")
	}
	prev := dashDevActiveOperator
	dashDevActiveOperator = func(*kube.Client) (bool, string, error) { return operator != "", operator, nil }
	return func() { dashDevActiveOperator = prev }
}
