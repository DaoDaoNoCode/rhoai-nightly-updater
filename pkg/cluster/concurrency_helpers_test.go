package cluster

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing/synctest"
)

// Tests of concurrency (how many calls run at once, whether calls share a
// fetch, whether one slow call holds up the others) run in a
// testing/synctest bubble with in-memory fakes: synctest.Wait returns when
// every goroutine of the test is blocked, so the test sees the calls that
// are in flight together without waiting for them, and the bubble's fake
// time passes only when nothing else can run. Fakes behind a real socket
// cannot be used there (a goroutine in network I/O never counts as
// blocked), hence handlerTransport.

// handlerTransport serves requests with h in memory, without a server.
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

// callGate holds the calls that pass through it until the test releases
// them, and counts how many are held at once.
type callGate struct {
	mu      sync.Mutex
	open    chan struct{}
	held    int
	maxHeld int
}

func newCallGate() *callGate { return &callGate{open: make(chan struct{})} }

// pass blocks the calling goroutine until the next release.
func (g *callGate) pass() {
	g.mu.Lock()
	g.held++
	g.maxHeld = max(g.maxHeld, g.held)
	open := g.open
	g.mu.Unlock()
	<-open
	g.mu.Lock()
	g.held--
	g.mu.Unlock()
}

// release lets every held call through; later calls are held again.
func (g *callGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	close(g.open)
	g.open = make(chan struct{})
}

// releaseUntilDone runs fn (in a synctest bubble) and, each time every
// goroutine is blocked, lets the calls held at g through, until fn returns.
// When nothing is held it waits for fn, so the bubble's time can pass. It
// returns the number of releases: the length of fn's longest chain of calls
// that had to wait for each other.
func (g *callGate) releaseUntilDone(fn func()) (rounds int) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	for {
		synctest.Wait()
		select {
		case <-done:
			return rounds
		default:
		}
		if held, _ := g.counts(); held == 0 {
			<-done
			return rounds
		}
		g.release()
		rounds++
	}
}

// counts returns how many calls are held now and the most held at once.
func (g *callGate) counts() (held, maxHeld int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held, g.maxHeld
}
