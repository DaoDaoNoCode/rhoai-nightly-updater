package authz

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// blockingReviews is an API server whose access reviews wait until the
// test lets each one answer: arrivals reports each review that reached it,
// and a token on answer lets one review answer (allowed).
type blockingReviews struct {
	arrivals chan struct{}
	answer   chan struct{}
}

func newBlockingReviews(t *testing.T) *blockingReviews {
	t.Helper()
	b := &blockingReviews{arrivals: make(chan struct{}, 1000), answer: make(chan struct{}, 1000)}
	fakeAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		b.arrivals <- struct{}{}
		select {
		case <-b.answer:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, `{"status":{"allowed":true}}`)
	})
	return b
}

func reqs(n int) []AccessRequest {
	out := make([]AccessRequest, n)
	for i := range out {
		out[i] = AccessRequest{Verb: "get", Resource: "pods", Namespace: "p", Name: fmt.Sprintf("pod-%d", i)}
	}
	return out
}

// waitArrivals waits until n reviews reached the server.
func waitArrivals(b *blockingReviews, n int) {
	for range n {
		<-b.arrivals
	}
}

// One call sends at most AccessReviewConcurrency reviews at once, and the
// process at most maxReviewsInFlight: with every review held at the
// server, the next one is sent only when one answers. A call cancelled
// while it waits for a slot holds none.
func TestAccessReviewsAreBounded(t *testing.T) {
	b := newBlockingReviews(t)
	// One call of 20: 8 at the server, none more until one answers.
	var wg sync.WaitGroup
	wg.Go(func() {
		if _, err := ReviewUserAccess(kube.NewUserClient(context.Background(), "user-token"), reqs(20)); err != nil {
			t.Error(err)
		}
	})
	waitArrivals(b, AccessReviewConcurrency)
	if n := len(reviewsInFlight); n != AccessReviewConcurrency {
		t.Fatalf("one call holds %d process slots, want %d", n, AccessReviewConcurrency)
	}
	for range 20 {
		b.answer <- struct{}{}
	}
	wg.Wait()
	waitArrivals(b, 20-AccessReviewConcurrency)
	if n := len(reviewsInFlight); n != 0 {
		t.Fatalf("%d process slots held after the call", n)
	}

	// Six calls of 8: 32 at the server, the process bound; the others wait.
	for range 6 {
		wg.Go(func() {
			if _, err := ReviewUserAccess(kube.NewUserClient(context.Background(), "user-token"), reqs(8)); err != nil {
				t.Error(err)
			}
		})
	}
	waitArrivals(b, maxReviewsInFlight)
	if n := len(reviewsInFlight); n != maxReviewsInFlight {
		t.Fatalf("%d process slots held, want the bound %d", n, maxReviewsInFlight)
	}
	// A call cancelled while it waits for a slot: no slot taken, an error.
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan error, 1)
	go func() {
		_, err := ReviewUserAccess(kube.NewUserClient(ctx, "user-token"), reqs(3))
		cancelled <- err
	}()
	cancel()
	if err := <-cancelled; err == nil || !errors.Is(err, ErrAccessReviewFailed) {
		t.Fatalf("a cancelled call: %v", err)
	}
	if n := len(reviewsInFlight); n != maxReviewsInFlight {
		t.Fatalf("a cancelled call changed the slots: %d", n)
	}
	// One answer frees one slot: exactly one more review is sent.
	b.answer <- struct{}{}
	waitArrivals(b, 1)
	for range 6*8 - 1 {
		b.answer <- struct{}{}
	}
	wg.Wait()
	if n := len(reviewsInFlight); n != 0 {
		t.Fatalf("%d process slots held at the end", n)
	}
}
