package operator

import "testing"

// The install wait outlasts OLM's own bundle unpack timeout, so a bundle
// that cannot be pulled for a while ends in OLM's verdict
// (BundleUnpackFailed) or recovers before the wait gives up. UseTestTimings
// shortens installTimeout for the other tests; this one reads the default.
func TestInstallTimeoutOutlastsOLMUnpackTimeout(t *testing.T) {
	if installTimeout <= olmBundleUnpackTimeout {
		t.Fatalf("installTimeout %s does not outlast OLM's bundle unpack timeout %s", installTimeout, olmBundleUnpackTimeout)
	}
}
