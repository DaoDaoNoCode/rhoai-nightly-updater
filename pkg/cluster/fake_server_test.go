package cluster

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"syscall"
)

// The shared fake API servers (newResourceFake, newFakeAPI, newMockClient,
// ...) are real net/http servers and clients that talk over an in-memory
// connection (net.Pipe) instead of TLS on a loopback socket. Under -race a
// TLS handshake per fake and loopback socket I/O were most of the package's
// test time. Only the bytes on the wire change: requests and responses go
// through net/http's own client and server code, with streaming, request
// contexts, timeouts, redirects and keep-alive as before.
//
// newFakeServer replaces httptest.NewTLSServer and newFakePlainServer
// httptest.NewServer. Each fake gets a host of its own, never reused
// (https://api-<n>.fake.test or http://...), so URL-keyed caches cannot
// mix two fakes, and srv.Client() (or any client built on its Transport)
// dials it in memory. For an https URL the transport's DialTLSContext
// returns the plain in-memory connection: "The returned net.Conn is
// assumed to already be past the TLS handshake" (net/http Transport,
// DialTLSContext; the transport has no proxy), so the request is plain
// HTTP/1.1 under an https URL.
// The handler sees r.TLS == nil; no handler in this package reads it.
// Other hosts are dialed for real, as before. A closed fake refuses new
// connections (ECONNREFUSED), as a closed loopback server does.
//
// The servers a single test starts itself keep httptest, among them the
// tests of TLS itself (client_transport_test.go) and those whose production
// code builds its own transport (authz_test.go: the in-cluster config).
// CLUSTER_FAKES_LOOPBACK=1 puts every fake back on httptest.NewTLSServer /
// NewServer (TLS and loopback sockets), to compare the two.
func newFakeServer(h http.Handler) *httptest.Server { return startFakeServer(h, "https") }

// newFakePlainServer is newFakeServer with an http:// URL, for the fakes
// that were httptest.NewServer.
func newFakePlainServer(h http.Handler) *httptest.Server { return startFakeServer(h, "http") }

var fakeNet = struct {
	sync.Mutex
	next  int
	hosts map[string]*pipeListener
}{hosts: map[string]*pipeListener{}}

func startFakeServer(h http.Handler, scheme string) *httptest.Server {
	if os.Getenv("CLUSTER_FAKES_LOOPBACK") == "1" {
		if scheme == "https" {
			return httptest.NewTLSServer(h)
		}
		return httptest.NewServer(h)
	}
	fakeNet.Lock()
	fakeNet.next++
	host := fmt.Sprintf("api-%d.fake.test", fakeNet.next)
	port := "443"
	if scheme == "http" {
		port = "80"
	}
	li := &pipeListener{addr: fakeAddr(net.JoinHostPort(host, port)), conns: make(chan net.Conn), done: make(chan struct{})}
	fakeNet.hosts[host] = li
	fakeNet.Unlock()

	// httptest.NewUnstartedServer with this listener instead of a loopback one.
	srv := &httptest.Server{Listener: li, Config: &http.Server{Handler: h}}
	srv.Start()
	srv.URL = scheme + "://" + host
	tr := srv.Client().Transport.(*http.Transport)
	tr.DialContext = dialFake
	tr.DialTLSContext = dialFake
	return srv
}

// dialFake connects to the fake of the address's host, or dials the
// network for any other host.
func dialFake(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	fakeNet.Lock()
	li := fakeNet.hosts[host]
	fakeNet.Unlock()
	if li == nil {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	return li.dial(ctx, network, addr)
}

// pipeListener hands one end of a net.Pipe to the server per dial.
type pipeListener struct {
	addr  fakeAddr
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *pipeListener) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	refused := &net.OpError{Op: "dial", Net: network, Addr: fakeAddr(addr), Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	select {
	case <-l.done:
		return nil, refused
	default:
	}
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.done:
	case <-ctx.Done():
		client.Close()
		server.Close()
		return nil, &net.OpError{Op: "dial", Net: network, Addr: fakeAddr(addr), Err: ctx.Err()}
	}
	client.Close()
	server.Close()
	return nil, refused
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return l.addr }

type fakeAddr string

func (fakeAddr) Network() string  { return "tcp" }
func (a fakeAddr) String() string { return string(a) }
