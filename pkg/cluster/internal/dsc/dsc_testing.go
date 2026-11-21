package dsc

import (
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// Test hooks for the DataScienceCluster's writes and their guards. Each
// panics outside a test binary.

// NamespaceOwnerBlockerForTest runs the hard reset's namespace guard
// (namespaceOwnerBlocker) on its own. The guard keeps its unexported name:
// "dsc_hard_reset_teardown.go:namespaceOwnerBlocker" names its user-token
// reads in the RBAC scan (rbac_coverage_test.go).
func NamespaceOwnerBlockerForTest(c *kube.Client) string {
	mustBeTest("NamespaceOwnerBlockerForTest")
	return namespaceOwnerBlocker(c)
}

// mustBeTest panics unless the program is a test binary.
func mustBeTest(what string) {
	if !testing.Testing() {
		panic(what + " is only for tests")
	}
}
