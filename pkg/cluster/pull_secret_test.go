package cluster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
)

func pullSecretBody(t *testing.T, auths map[string]any) string {
	t.Helper()
	cfg, err := json.Marshal(map[string]any{"auths": auths})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/dockerconfigjson",
		"data": map[string]string{".dockerconfigjson": base64.StdEncoding.EncodeToString(cfg)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestSelectQuayAuth_PrefersMostSpecificDeterministically(t *testing.T) {
	cases := []struct {
		name     string
		auths    map[string]any
		wantAuth string
	}{
		{
			name: "quay.io/rhoai beats generic quay.io",
			auths: map[string]any{
				"quay.io":       map[string]any{"auth": exampleAuth("generic:x")},
				"quay.io/rhoai": map[string]any{"auth": exampleAuth("rhoai:y")},
				"docker.io":     map[string]any{"auth": exampleAuth("docker:z")},
			},
			wantAuth: exampleAuth("rhoai:y"),
		},
		{
			name: "scheme and trailing slash are normalized",
			auths: map[string]any{
				"quay.io":                map[string]any{"auth": exampleAuth("generic:x")},
				"https://quay.io/rhoai/": map[string]any{"auth": exampleAuth("rhoai:y")},
			},
			wantAuth: exampleAuth("rhoai:y"),
		},
		{
			name: "repository-scoped entry beats generic quay.io",
			auths: map[string]any{
				"quay.io":                          map[string]any{"auth": exampleAuth("generic:x")},
				"quay.io/rhoai/rhoai-fbc-fragment": map[string]any{"auth": exampleAuth("repo:z")},
			},
			wantAuth: exampleAuth("repo:z"),
		},
		{
			name: "generic quay.io is used when nothing more specific exists",
			auths: map[string]any{
				"quay.io": map[string]any{"auth": exampleAuth("generic:x")},
			},
			wantAuth: exampleAuth("generic:x"),
		},
		{
			name: "username and password entries are accepted",
			auths: map[string]any{
				"quay.io/rhoai": map[string]any{"username": "robot", "password": "secret"},
			},
			wantAuth: base64.StdEncoding.EncodeToString([]byte("robot:secret")),
		},
		{
			name: "empty specific entry falls back to generic quay.io",
			auths: map[string]any{
				"quay.io":       map[string]any{"auth": exampleAuth("generic:x")},
				"quay.io/rhoai": map[string]any{"auth": ""},
			},
			wantAuth: exampleAuth("generic:x"),
		},
		{
			name: "similarly named repositories do not match",
			auths: map[string]any{
				"quay.io/rhoai-dev": map[string]any{"auth": exampleAuth("dev:w")},
			},
			wantAuth: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Map iteration order is random; repeat to prove determinism.
			for i := range 200 {
				if auth := registry.SelectQuayAuth(tc.auths); auth != tc.wantAuth {
					t.Fatalf("iteration %d: got %q, want %q", i, auth, tc.wantAuth)
				}
			}
		})
	}
}

func TestGetQuayAuthAndStatusUseSameCredential(t *testing.T) {
	body := pullSecretBody(t, map[string]any{
		"quay.io":       map[string]any{"auth": exampleAuth("generic:x")},
		"quay.io/rhoai": map[string]any{"auth": exampleAuth("rhoai:y")},
	})
	client, cleanup := newMockClient(map[string]mockResponse{
		"/api/v1/namespaces/kube-system/secrets/additional-pull-secret": {body: body},
	})
	defer cleanup()

	var verified []string
	defer registry.SetVerifyQuayCredentialsForTest(func(auth string) error { verified = append(verified, auth); return nil })()

	for range 50 {
		if got := registry.QuayAuth(client); got != exampleAuth("rhoai:y") {
			t.Fatalf("getQuayAuth = %q, want the quay.io/rhoai credential", got)
		}
	}
	ps, err := registry.PullSecret(client)
	if err != nil || !ps.Valid {
		t.Fatalf("getPullSecret = %+v, %v", ps, err)
	}
	if len(verified) != 1 || verified[0] != exampleAuth("rhoai:y") {
		t.Fatalf("status verified %v, want the quay.io/rhoai credential", verified)
	}
}

func TestGetPullSecret_GenericQuayLoginIsUsable(t *testing.T) {
	body := pullSecretBody(t, map[string]any{
		"quay.io": map[string]any{"auth": exampleAuth("generic:x")},
	})
	client, cleanup := newMockClient(map[string]mockResponse{
		"/api/v1/namespaces/kube-system/secrets/additional-pull-secret": {body: body},
	})
	defer cleanup()
	ps, err := registry.PullSecret(client)
	if err != nil || !ps.Exists || !ps.Valid {
		t.Fatalf("getPullSecret = %+v, %v; a quay.io login also grants quay.io/rhoai pulls", ps, err)
	}
}

func TestGetPullSecret_DistinguishesOutageFromRejection(t *testing.T) {
	body := pullSecretBody(t, map[string]any{
		"quay.io/rhoai": map[string]any{"auth": exampleAuth("rhoai:y")},
	})
	client, cleanup := newMockClient(map[string]mockResponse{
		"/api/v1/namespaces/kube-system/secrets/additional-pull-secret": {body: body},
	})
	defer cleanup()
	defer registry.SetVerifyQuayCredentialsForTest(func(string) error { return &registry.QuayAuthError{StatusCode: 401, Body: "unauthorized"} })()
	ps, _ := registry.PullSecret(client)
	if ps.Valid || !strings.HasPrefix(ps.Detail, "Credentials rejected by Quay") {
		t.Fatalf("401 should be a rejection, got %+v", ps)
	}

	registry.SetVerifyQuayCredentialsForTest(func(string) error { return errors.New("dial tcp: i/o timeout") })
	ps, _ = registry.PullSecret(client)
	if ps.Valid || !strings.HasPrefix(ps.Detail, "Could not verify credentials with Quay") {
		t.Fatalf("network failure should not be reported as a rejection, got %+v", ps)
	}
}

func TestVerifyQuayCredentialsCached_CachesOnlyDefinitiveAnswers(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	defer registry.SetHTTPClientForTest(&http.Client{Transport: dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Basic ")
		mu.Lock()
		calls[auth]++
		mu.Unlock()
		switch auth {
		case "good":
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"token":"t"}`)), Header: http.Header{}}, nil
		case "bad":
			return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`unauthorized`)), Header: http.Header{}}, nil
		default:
			return nil, errors.New("connection refused")
		}
	})})()
	registry.ResetQuayCredentialCacheForTest()

	for range 3 {
		if err := registry.VerifyQuayCredentialsCached("good"); err != nil {
			t.Fatalf("good credentials: %v", err)
		}
		if err := registry.VerifyQuayCredentialsCached("bad"); !registry.IsCredentialRejection(err) {
			t.Fatalf("bad credentials: want rejection, got %v", err)
		}
		if err := registry.VerifyQuayCredentialsCached("down"); err == nil || registry.IsCredentialRejection(err) {
			t.Fatalf("outage: want non-rejection error, got %v", err)
		}
	}
	if calls["good"] != 1 || calls["bad"] != 1 {
		t.Fatalf("definitive answers should be cached, calls = %v", calls)
	}
	if calls["down"] != 3 {
		t.Fatalf("transient failures must not be cached, calls = %v", calls)
	}
}

// pullSecretServer serves the pull secret and records the payload written to it.
func pullSecretServer(t *testing.T, existing string) (*Client, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var writes []map[string]any
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/secrets/additional-pull-secret"):
			mu.Lock()
			last := existing
			if n := len(writes); n > 0 {
				b, _ := json.Marshal(writes[n-1])
				last = string(b)
			}
			mu.Unlock()
			if last == "" {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `{"kind":"Status","status":"Failure","reason":"NotFound","code":404}`)
				return
			}
			io.WriteString(w, last)
		case strings.Contains(r.URL.Path, "/namespaces/kube-system/secrets"):
			var obj map[string]any
			if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
				t.Errorf("decode write: %v", err)
			}
			mu.Lock()
			writes = append(writes, obj)
			mu.Unlock()
			io.WriteString(w, `{}`)
		default:
			io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	client := newTestClient(testClientOptions{BaseURL: srv.URL, Token: "t", HTTPClient: srv.Client(), Ctx: context.Background()})
	return client, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), writes...)
	}
}

func writtenAuths(t *testing.T, obj map[string]any) map[string]any {
	t.Helper()
	data, _ := obj["data"].(map[string]any)
	raw, err := base64.StdEncoding.DecodeString(data[".dockerconfigjson"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg["auths"].(map[string]any)
}

func TestCreatePullSecret_PreservesOtherRegistries(t *testing.T) {
	existing := pullSecretBody(t, map[string]any{
		"registry.example.com":   map[string]any{"auth": exampleAuth("example:x")},
		"quay.io":                map[string]any{"auth": exampleAuth("generic:x")},
		"https://quay.io/rhoai/": map[string]any{"auth": exampleAuth("old:pass")},
	})
	client, writes := pullSecretServer(t, existing)

	result, err := CreatePullSecret(client, "bmV3OnBhc3M=")
	if err != nil || !result.Success {
		t.Fatalf("CreatePullSecret = %+v, %v", result, err)
	}
	w := writes()
	if len(w) != 1 {
		t.Fatalf("expected one write, got %d", len(w))
	}
	auths := writtenAuths(t, w[0])
	if got := registry.AuthFromEntry(auths["registry.example.com"]); got != exampleAuth("example:x") {
		t.Errorf("unrelated registry credential lost: %v", auths)
	}
	if got := registry.AuthFromEntry(auths["quay.io"]); got != exampleAuth("generic:x") {
		t.Errorf("generic quay.io credential lost: %v", auths)
	}
	if _, ok := auths["https://quay.io/rhoai/"]; ok {
		t.Errorf("old equivalent quay.io/rhoai key should be replaced: %v", auths)
	}
	if auth := registry.SelectQuayAuth(auths); auth != "bmV3OnBhc3M=" {
		t.Errorf("new credential not selected: %q", auth)
	}
}

func TestCreatePullSecret_CreatesWhenMissing(t *testing.T) {
	client, writes := pullSecretServer(t, "")
	result, err := CreatePullSecret(client, "bmV3OnBhc3M=")
	if err != nil || !result.Success {
		t.Fatalf("CreatePullSecret = %+v, %v", result, err)
	}
	w := writes()
	if len(w) != 1 {
		t.Fatalf("expected one write, got %d", len(w))
	}
	auths := writtenAuths(t, w[0])
	if len(auths) != 1 || registry.AuthFromEntry(auths["quay.io/rhoai"]) != "bmV3OnBhc3M=" {
		t.Fatalf("unexpected auths: %v", auths)
	}
}

func TestCreatePullSecret_RequiresUserAndPassword(t *testing.T) {
	client, writes := pullSecretServer(t, "")
	for _, auth := range []string{"Og==", "dXNlcjo=", "OnBhc3M="} { // ":", "user:", ":pass"
		result, err := CreatePullSecret(client, auth)
		if err != nil {
			t.Fatal(err)
		}
		if result.Success || !strings.Contains(result.Message, "username:password") {
			t.Errorf("%s: expected validation failure, got %+v", auth, result)
		}
	}
	if len(writes()) != 0 {
		t.Fatal("invalid credentials must not be written")
	}
}
