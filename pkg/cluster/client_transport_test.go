package cluster

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAPIServerTransportUsesHTTP2AndReusesConnections(t *testing.T) {
	var newConns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Proto)
	}))
	srv.EnableHTTP2 = true
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			newConns.Add(1)
		}
	}
	srv.StartTLS()
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	tlsConfig, _ := apiServerTLSConfig(func(string) string { return "" }, func(string) ([]byte, error) { return nil, errors.New("absent") })
	tlsConfig.RootCAs = pool
	client := &http.Client{Transport: newAPIServerTransport(tlsConfig)}

	// Warm one connection, then fan out like GetStatus does.
	get := func() string {
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Error(err)
			return ""
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if proto := get(); proto != "HTTP/2.0" {
		t.Fatalf("expected HTTP/2.0, got %q", proto)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if proto := get(); proto != "HTTP/2.0" {
				t.Errorf("expected HTTP/2.0, got %q", proto)
			}
		}()
	}
	wg.Wait()
	if n := newConns.Load(); n != 1 {
		t.Fatalf("expected 1 TLS connection for 13 requests, got %d", n)
	}
}

func testCAPEM(t *testing.T) []byte {
	t.Helper()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}

func TestAPIServerTLSConfig(t *testing.T) {
	caPEM := testCAPEM(t)
	files := func(present map[string][]byte) func(string) ([]byte, error) {
		return func(path string) ([]byte, error) {
			if b, ok := present[path]; ok {
				return b, nil
			}
			return nil, errors.New("no such file")
		}
	}
	cases := []struct {
		name         string
		env          map[string]string
		files        map[string][]byte
		wantInsecure bool
		wantRoots    string // "custom", "system", "empty"
		wantWarning  string
	}{
		{name: "in-cluster CA", files: map[string][]byte{inClusterCAFile: caPEM}, wantRoots: "custom"},
		{name: "dev mode uses system roots", env: map[string]string{"DEV_MODE": "true"}, wantRoots: "system"},
		{name: "GO_TEST no longer disables verification", env: map[string]string{"GO_TEST": "true"}, wantRoots: "system"},
		{name: "outside cluster without dev mode uses system roots", wantRoots: "system"},
		{name: "explicit insecure opt-in", env: map[string]string{"DEV_MODE": "true", "DEV_INSECURE_TLS": "true"}, wantInsecure: true, wantRoots: "system", wantWarning: "DISABLED"},
		{name: "insecure opt-in ignored outside dev mode", env: map[string]string{"DEV_INSECURE_TLS": "true"}, wantRoots: "system"},
		{name: "insecure opt-in ignored in cluster", env: map[string]string{"DEV_MODE": "true", "DEV_INSECURE_TLS": "true"}, files: map[string][]byte{inClusterCAFile: caPEM}, wantRoots: "custom"},
		{name: "KUBE_CA_FILE", env: map[string]string{"DEV_MODE": "true", "KUBE_CA_FILE": "/tmp/ca.pem"}, files: map[string][]byte{"/tmp/ca.pem": caPEM}, wantRoots: "custom"},
		{name: "unreadable KUBE_CA_FILE fails closed", env: map[string]string{"DEV_MODE": "true", "DEV_INSECURE_TLS": "true", "KUBE_CA_FILE": "/tmp/missing.pem"}, wantRoots: "empty", wantWarning: "unusable"},
		{name: "KUBE_CA_FILE without PEM fails closed", env: map[string]string{"KUBE_CA_FILE": "/tmp/bad.pem"}, files: map[string][]byte{"/tmp/bad.pem": []byte("junk")}, wantRoots: "empty", wantWarning: "unusable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, warning := apiServerTLSConfig(func(k string) string { return tc.env[k] }, files(tc.files))
			if cfg.InsecureSkipVerify != tc.wantInsecure {
				t.Errorf("InsecureSkipVerify = %v, want %v", cfg.InsecureSkipVerify, tc.wantInsecure)
			}
			switch tc.wantRoots {
			case "system":
				if cfg.RootCAs != nil {
					t.Errorf("expected system roots (nil RootCAs)")
				}
			case "custom":
				if cfg.RootCAs == nil || cfg.RootCAs.Equal(x509.NewCertPool()) {
					t.Errorf("expected a pool with the configured CA")
				}
			case "empty":
				if cfg.RootCAs == nil || !cfg.RootCAs.Equal(x509.NewCertPool()) {
					t.Errorf("expected an empty pool (fail closed)")
				}
			}
			if tc.wantWarning == "" && warning != "" {
				t.Errorf("unexpected warning %q", warning)
			}
			if tc.wantWarning != "" && !strings.Contains(warning, tc.wantWarning) {
				t.Errorf("warning %q does not contain %q", warning, tc.wantWarning)
			}
		})
	}
}
