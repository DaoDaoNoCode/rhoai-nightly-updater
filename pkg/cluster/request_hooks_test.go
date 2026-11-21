package cluster

import (
	"io"
	"net/http"
	"sync"
)

// requestHooks run test code at two defined points of a client's requests,
// for a test that needs something (a cancelled context, a lost lock) to
// happen while a request is made or right after it answered:
//
//   - before runs just before a request goes to the transport. A context
//     cancelled here fails the request every time: net/http's
//     Transport.roundTrip returns context.Cause(ctx) for a done context
//     before it sends anything (net/http/transport.go).
//   - answered runs once the client closed the response body; c.do reads
//     the body in full before it closes it, so the request succeeded.
//
// A server handler is no such point: a context cancelled while the handler
// runs races the handler's answer, as persistConn.roundTrip selects between
// the response and ctx.Done() (net/http/transport.go), and the request
// fails in some runs and succeeds in others.
type requestHooks struct {
	before   func(*http.Request)
	answered func(*http.Request)
}

// withRequestHooks returns a copy of c whose requests run h.
func withRequestHooks(c *Client, h requestHooks) *Client {
	cp := *c
	hc := *c.httpClient
	base := hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	hc.Transport = hookTransport{base: base, hooks: h}
	cp.httpClient = &hc
	return &cp
}

type hookTransport struct {
	base  http.RoundTripper
	hooks requestHooks
}

func (t hookTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.hooks.before != nil {
		t.hooks.before(r)
	}
	resp, err := t.base.RoundTrip(r)
	if err != nil || t.hooks.answered == nil {
		return resp, err
	}
	resp.Body = &closeHookBody{ReadCloser: resp.Body, onClose: func() { t.hooks.answered(r) }}
	return resp, nil
}

// closeHookBody runs onClose once, after the body was closed.
type closeHookBody struct {
	io.ReadCloser
	once    sync.Once
	onClose func()
}

func (b *closeHookBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.onClose)
	return err
}
