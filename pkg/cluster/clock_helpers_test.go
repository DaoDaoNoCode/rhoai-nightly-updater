package cluster

import (
	"net/http"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/clock/clocktest"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// fakeCacheClock makes c tell its entries' ages by a fresh fake clock for
// the test, so the test moves the cache past a lifetime with Advance. The
// cache is emptied, since entries stored by the wall clock would not
// compare with the fake time.
func fakeCacheClock[K comparable, V any](t *testing.T, c *kube.LRU[K, V]) *clocktest.Fake {
	t.Helper()
	f := clocktest.NewFake()
	c.Purge()
	prev := c.SetNowForTest(f.Now)
	t.Cleanup(func() {
		c.Purge()
		c.SetNowForTest(prev)
	})
	return f
}

// slowTransport is an API server that takes latency, on clk, to answer each
// request: the request waits on the fake clock and is abandoned with its
// context's error when its context ends first, as a real request is when
// its deadline passes. A request that waited its latency goes to base.
type slowTransport struct {
	base    http.RoundTripper
	latency time.Duration
}

func (s slowTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := kube.Clock().Sleep(r.Context(), s.latency); err != nil {
		return nil, err
	}
	return s.base.RoundTrip(r)
}

// withLatency makes every request of c take latency on clk.
func withLatency(c *Client, latency time.Duration) *Client {
	cp := *c
	hc := *c.TestOptions().HTTPClient
	base := hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	hc.Transport = slowTransport{base: base, latency: latency}
	editTestClient(&cp, func(o *testClientOptions) { o.HTTPClient = &hc })
	return &cp
}
