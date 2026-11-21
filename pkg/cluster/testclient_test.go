package cluster

import (
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// testClientOptions are the parts of a test's Client (TestClientOptions):
// the fake API server's address and HTTP client, the context, and whose
// token it carries. Unset fields stay zero, as in a struct literal.
type testClientOptions = kube.TestClientOptions

// newTestClient is the Client a test talks to its fake cluster with. Tests
// build every Client here, not with a struct literal, so the Client's
// fields can change (or move to another package) without touching them.
func newTestClient(o testClientOptions) *Client {
	return kube.NewTestClient(o)
}

// editTestClient changes fields of c in place, as a test that assigned
// them did: edit gets c's fields (TestOptions) and changes some.
func editTestClient(c *Client, edit func(o *testClientOptions)) {
	o := c.TestOptions()
	edit(&o)
	*c = *kube.NewTestClient(o)
}
