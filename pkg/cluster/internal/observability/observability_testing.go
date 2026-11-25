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
