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
)

func pullSecretBody(t *testing.T, auths map[string]interface{}) string {
	t.Helper()
	cfg, err := json.Marshal(map[string]interface{}{"auths": auths})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]interface{}{
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
		auths    map[string]interface{}
		wantAuth string
		wantKey  string
	}{
		{
			name: "quay.io/rhoai beats generic quay.io",
			auths: map[string]interface{}{
				"quay.io":       map[string]interface{}{"auth": "Z2VuZXJpYzp4"},
				"quay.io/rhoai": map[string]interface{}{"auth": "cmhvYWk6eQ=="},
				"docker.io":     map[string]interface{}{"auth": "ZG9ja2VyOno="},
			},
			wantAuth: "cmhvYWk6eQ==", wantKey: "quay.io/rhoai",
		},
		{
			name: "scheme and trailing slash are normalized",
			auths: map[string]interface{}{
				"quay.io":                map[string]interface{}{"auth": "Z2VuZXJpYzp4"},
				"https://quay.io/rhoai/": map[string]interface{}{"auth": "cmhvYWk6eQ=="},
			},
			wantAuth: "cmhvYWk6eQ==", wantKey: "https://quay.io/rhoai/",
		},
		{
			name: "repository-scoped entry beats generic quay.io",
			auths: map[string]interface{}{
				"quay.io":                          map[string]interface{}{"auth": "Z2VuZXJpYzp4"},
				"quay.io/rhoai/rhoai-fbc-fragment": map[string]interface{}{"auth": "cmVwbzp6"},
			},
			wantAuth: "cmVwbzp6", wantKey: "quay.io/rhoai/rhoai-fbc-fragment",
		},
		{
			name: "generic quay.io is used when nothing more specific exists",
			auths: map[string]interface{}{
				"quay.io": map[string]interface{}{"auth": "Z2VuZXJpYzp4"},
			},
			wantAuth: "Z2VuZXJpYzp4", wantKey: "quay.io",
		},
		{
			name: "username and password entries are accepted",
			auths: map[string]interface{}{
				"quay.io/rhoai": map[string]interface{}{"username": "robot", "password": "secret"},
			},
			wantAuth: base64.StdEncoding.EncodeToString([]byte("robot:secret")), wantKey: "quay.io/rhoai",
		},
		{
			name: "empty specific entry falls back to generic quay.io",
			auths: map[string]interface{}{
				"quay.io":       map[string]interface{}{"auth": "Z2VuZXJpYzp4"},
				"quay.io/rhoai": map[string]interface{}{"auth": ""},
			},
			wantAuth: "Z2VuZXJpYzp4", wantKey: "quay.io",
		},
		{
			name: "similarly named repositories do not match",
			auths: map[string]interface{}{
				"quay.io/rhoai-dev": map[string]interface{}{"auth": "ZGV2Onc="},
			},
			wantAuth: "", wantKey: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Map iteration order is random; repeat to prove determinism.
			for i := 0; i < 200; i++ {
				auth, key := selectQuayAuth(tc.auths)
				if auth != tc.wantAuth || key != tc.wantKey {
					t.Fatalf("iteration %d: got (%q, %q), want (%q, %q)", i, auth, key, tc.wantAuth, tc.wantKey)
				}
			}
		})
	}
}

func TestGetQuayAuthAndStatusUseSameCredential(t *testing.T) {
	body := pullSecretBody(t, map[string]interface{}{
		"quay.io":       map[string]interface{}{"auth": "Z2VuZXJpYzp4"},
		"quay.io/rhoai": map[string]interface{}{"auth": "cmhvYWk6eQ=="},
	})
	client, cleanup := newMockClient(map[string]mockResponse{
		"/api/v1/namespaces/kube-system/secrets/additional-pull-secret": {body: body},
	})
	defer cleanup()

	var verified []string
	orig := verifyQuayCredentials
	verifyQuayCredentials = func(auth string) error { verified = append(verified, auth); return nil }
	defer func() { verifyQuayCredentials = orig }()

	for i := 0; i < 50; i++ {
		if got := getQuayAuth(client); got != "cmhvYWk6eQ==" {
			t.Fatalf("getQuayAuth = %q, want the quay.io/rhoai credential", got)
		}
	}
	ps, err := getPullSecret(client)
	if err != nil || !ps.Valid {
		t.Fatalf("getPullSecret = %+v, %v", ps, err)
	}
	if len(verified) != 1 || verified[0] != "cmhvYWk6eQ==" {
		t.Fatalf("status verified %v, want the quay.io/rhoai credential", verified)
	}
}

func TestGetPullSecret_GenericQuayLoginIsUsable(t *testing.T) {
	body := pullSecretBody(t, map[string]interface{}{
		"quay.io": map[string]interface{}{"auth": "Z2VuZXJpYzp4"},
	})
	client, cleanup := newMockClient(map[string]mockResponse{
		"/api/v1/namespaces/kube-system/secrets/additional-pull-secret": {body: body},
	})
	defer cleanup()
	ps, err := getPullSecret(client)
	if err != nil || !ps.Exists || !ps.Valid {
		t.Fatalf("getPullSecret = %+v, %v; a quay.io login also grants quay.io/rhoai pulls", ps, err)
	}
}

func TestGetPullSecret_DistinguishesOutageFromRejection(t *testing.T) {
	body := pullSecretBody(t, map[string]interface{}{
		"quay.io/rhoai": map[string]interface{}{"auth": "cmhvYWk6eQ=="},
	})
	client, cleanup := newMockClient(map[string]mockResponse{
		"/api/v1/namespaces/kube-system/secrets/additional-pull-secret": {body: body},
	})
	defer cleanup()
	orig := verifyQuayCredentials
	defer func() { verifyQuayCredentials = orig }()

	verifyQuayCredentials = func(string) error { return &quayAuthError{StatusCode: 401, Body: "unauthorized"} }
	ps, _ := getPullSecret(client)
	if ps.Valid || !strings.HasPrefix(ps.Detail, "Credentials rejected by Quay") {
		t.Fatalf("401 should be a rejection, got %+v", ps)
	}

	verifyQuayCredentials = func(string) error { return errors.New("dial tcp: i/o timeout") }
	ps, _ = getPullSecret(client)
	if ps.Valid || !strings.HasPrefix(ps.Detail, "Could not verify credentials with Quay") {
		t.Fatalf("network failure should not be reported as a rejection, got %+v", ps)
	}
}

func TestVerifyQuayCredentialsCached_CachesOnlyDefinitiveAnswers(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	original := quayHTTPClient
	quayHTTPClient = &http.Client{Transport: dscSampleTransport(func(r *http.Request) (*http.Response, error) {
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
	})}
	defer func() { quayHTTPClient = original }()
	quayCredentialCacheMu.Lock()
	quayCredentialCache = map[[32]byte]quayCredentialVerdict{}
	quayCredentialCacheMu.Unlock()

	for i := 0; i < 3; i++ {
		if err := verifyQuayCredentialsCached("good"); err != nil {
			t.Fatalf("good credentials: %v", err)
		}
		if err := verifyQuayCredentialsCached("bad"); !isQuayCredentialRejection(err) {
			t.Fatalf("bad credentials: want rejection, got %v", err)
		}
		if err := verifyQuayCredentialsCached("down"); err == nil || isQuayCredentialRejection(err) {
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
func pullSecretServer(t *testing.T, existing string) (*Client, func() []map[string]interface{}) {
	t.Helper()
	var mu sync.Mutex
	var writes []map[string]interface{}
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
			var obj map[string]interface{}
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
	client := &Client{baseURL: srv.URL, token: "t", httpClient: srv.Client(), ctx: context.Background()}
	return client, func() []map[string]interface{} {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]interface{}(nil), writes...)
	}
}

func writtenAuths(t *testing.T, obj map[string]interface{}) map[string]interface{} {
	t.Helper()
	data, _ := obj["data"].(map[string]interface{})
	raw, err := base64.StdEncoding.DecodeString(data[".dockerconfigjson"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg["auths"].(map[string]interface{})
}

func TestCreatePullSecret_PreservesOtherRegistries(t *testing.T) {
	existing := pullSecretBody(t, map[string]interface{}{
		"registry.example.com":   map[string]interface{}{"auth": "ZXhhbXBsZTp4"},
		"quay.io":                map[string]interface{}{"auth": "Z2VuZXJpYzp4"},
		"https://quay.io/rhoai/": map[string]interface{}{"auth": "b2xkOnBhc3M="},
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
	if got := authFromEntry(auths["registry.example.com"]); got != "ZXhhbXBsZTp4" {
		t.Errorf("unrelated registry credential lost: %v", auths)
	}
	if got := authFromEntry(auths["quay.io"]); got != "Z2VuZXJpYzp4" {
		t.Errorf("generic quay.io credential lost: %v", auths)
	}
	if _, ok := auths["https://quay.io/rhoai/"]; ok {
		t.Errorf("old equivalent quay.io/rhoai key should be replaced: %v", auths)
	}
	if auth, key := selectQuayAuth(auths); auth != "bmV3OnBhc3M=" || key != "quay.io/rhoai" {
		t.Errorf("new credential not selected: (%q, %q)", auth, key)
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
	if len(auths) != 1 || authFromEntry(auths["quay.io/rhoai"]) != "bmV3OnBhc3M=" {
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
