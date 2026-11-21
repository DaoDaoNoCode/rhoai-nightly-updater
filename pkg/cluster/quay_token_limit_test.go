package cluster

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
)

// endlessToken is a 64 MiB token response body that does not close its
// string: `{"token":"aaaa...` (the cap ends the test, not the code under
// test).
type endlessToken struct{ read int64 }

const endlessTokenSize = 64 << 20

func (e *endlessToken) Read(p []byte) (int, error) {
	if e.read >= endlessTokenSize {
		return 0, io.EOF
	}
	n := 0
	if e.read == 0 {
		n = copy(p, `{"token":"`)
	}
	for ; n < len(p) && e.read+int64(n) < endlessTokenSize; n++ {
		p[n] = 'a'
	}
	e.read += int64(n)
	return n, nil
}

// The token response is read with a bound, like the error response and
// every other external read: a body that does not end is not buffered
// whole. A real token is about 2.6 KB (quayTokenBatch).
func TestQuayTokenResponseIsBounded(t *testing.T) {
	body := &endlessToken{}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(body), Header: http.Header{}, Request: r}, nil
	})}
	_, err := registry.FetchQuayToken(context.Background(), client, "", []string{registry.FBCRepo})
	if err == nil || !strings.Contains(err.Error(), "decoding auth response") {
		t.Fatalf("err = %v", err)
	}
	if body.read > 2<<20 {
		t.Fatalf("read %d MiB of a token response", body.read>>20)
	}
	// A normal token still decodes.
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"token":"` + strings.Repeat("t", 3000) + `"}`)), Header: http.Header{}, Request: r}, nil
	})
	if tok, err := registry.FetchQuayToken(context.Background(), client, "", []string{registry.FBCRepo}); err != nil || len(tok) != 3000 {
		t.Fatalf("token %d bytes, %v", len(tok), err)
	}
}
