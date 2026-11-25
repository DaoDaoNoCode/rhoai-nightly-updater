package kube

import (
	"context"
	"net/http"
	"testing"
)

// TestClientOptions are every field of a Client, for tests: the fake API
// server's address and HTTP client, the context, the user, whose token it
// carries, and the clients set with SetBookkeeping and SetSignedInUser.
// Unset fields stay zero, as in a struct literal.
type TestClientOptions struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
	Ctx        context.Context
	Username   string
	// AsUser: the token is the signed-in user's own (NewUserClient).
	AsUser       bool
	Bookkeeper   *Client
	SignedInUser *Client
	StatusRead   bool
	NoRetry      func(error) bool
}

// NewTestClient is the Client a test talks to its fake cluster with. It
// panics outside a test binary (testing.Testing), so production code
// builds every Client with NewClientWithContext or NewUserClient.
func NewTestClient(o TestClientOptions) *Client {
	mustBeTest("NewTestClient")
	return &Client{
		baseURL:      o.BaseURL,
		token:        o.Token,
		httpClient:   o.HTTPClient,
		ctx:          o.Ctx,
		username:     o.Username,
		bookkeeper:   o.Bookkeeper,
		asUser:       o.AsUser,
		signedInUser: o.SignedInUser,
		statusRead:   o.StatusRead,
		noRetry:      o.NoRetry,
	}
}

// TestOptions returns c's fields, for a test that changes some of them and
// builds the client again with NewTestClient. It panics outside a test
// binary.
func (c *Client) TestOptions() TestClientOptions {
	mustBeTest("Client.TestOptions")
	return TestClientOptions{
		BaseURL:      c.baseURL,
		Token:        c.token,
		HTTPClient:   c.httpClient,
		Ctx:          c.ctx,
		Username:     c.username,
		AsUser:       c.asUser,
		Bookkeeper:   c.bookkeeper,
		SignedInUser: c.signedInUser,
		StatusRead:   c.statusRead,
		NoRetry:      c.noRetry,
	}
}

// SetTransportForTest makes t the transport of the clients
// NewClientWithContext and NewUserClient make from now on (a fake API
// server's) and returns the function that puts the previous one back. It
// panics outside a test binary.
func SetTransportForTest(t *http.Transport) (restore func()) {
	mustBeTest("SetTransportForTest")
	prev := sharedTransport
	sharedTransport = t
	return func() { sharedTransport = prev }
}

// mustBeTest panics unless the program is a test binary.
func mustBeTest(what string) {
	if !testing.Testing() {
		panic(what + " is only for tests")
	}
}
