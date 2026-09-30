package cluster

import "context"

// Context returns the lifetime of this client's cluster operations.
func (c *Client) Context() context.Context { return c.ctx }
