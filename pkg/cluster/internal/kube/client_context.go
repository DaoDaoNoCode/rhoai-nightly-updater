package kube

import "context"

// Context returns the lifetime of this client's cluster operations.
func (c *Client) Context() context.Context { return c.ctx }

// BaseURL returns the API server's address, which keys the caches of
// cluster state (one address is one cluster).
func (c *Client) BaseURL() string { return c.baseURL }

// BearerToken returns the token the client's requests carry: for the few
// calls that are not API server requests and must act with the same
// identity (the dashboard's gen-ai API, MLflow), or that key a cache by it.
// A facts client (Facts) gives none: its token only reads the cluster
// facts, never calls another server on anyone's behalf.
func (c *Client) BearerToken() string {
	if c.readOnly {
		return ""
	}
	return c.token
}

// Username returns the logged-in user identity SetUsername set, empty when
// none was set.
func (c *Client) Username() string { return c.username }
