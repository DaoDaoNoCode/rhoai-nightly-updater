package cluster

import (
	"context"
	"net/http"
)

// testClientOptions are the parts of a test's Client: the fake API
// server's address and HTTP client, the context, and whose token it
// carries. Unset fields stay zero, as in a struct literal.
type testClientOptions struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
	Ctx        context.Context
	Username   string
	// AsUser: the token is the signed-in user's own (NewUserClient).
	AsUser bool
}

// newTestClient is the Client a test talks to its fake cluster with. Tests
// build every Client here, not with a struct literal, so the Client's
// fields can change (or move to another package) without touching them.
func newTestClient(o testClientOptions) *Client {
	return &Client{
		baseURL:    o.BaseURL,
		token:      o.Token,
		httpClient: o.HTTPClient,
		ctx:        o.Ctx,
		username:   o.Username,
		asUser:     o.AsUser,
	}
}
