package gitops

import "testing"

// Test hooks for the gitops profile. Each panics outside a test binary.

// PolicyTemplateForTest is the admission policy template the app was
// started with (SetPolicyTemplate).
func PolicyTemplateForTest() []byte {
	mustBeTest("GitOpsPolicyTemplateForTest")
	return gitOpsPolicyTemplate
}

// mustBeTest panics unless the program is a test binary.
func mustBeTest(what string) {
	if !testing.Testing() {
		panic(what + " is only for tests")
	}
}
