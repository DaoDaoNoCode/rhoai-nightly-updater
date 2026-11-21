package cluster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/operator"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// succeededCSV is the CSV the simulated OLM reports after installing.
const succeededCSV = `{"metadata":{"name":"rhods-operator.v3.5.0"},"spec":{"displayName":"Red Hat OpenShift AI","version":"3.5.0"},"status":{"phase":"Succeeded"}}`

type mockResponse struct {
	body       string
	statusCode int
}

// newMockClient creates a test Client backed by an httptest.Server.
// The handler map keys are request paths; values are the responses to return.
func newMockClient(responses map[string]mockResponse) (*Client, func()) {
	resetClusterCaches()
	server := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dashboardOperatorAbsent(w, r) {
			return
		}
		resp, ok := responses[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"kind":"Status","status":"Failure","message":"not found","reason":"NotFound","code":404}`))
			return
		}
		if resp.statusCode == 0 {
			resp.statusCode = 200
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.statusCode)
		w.Write([]byte(resp.body))
	}))

	client := newTestClient(testClientOptions{
		BaseURL:    server.URL,
		Token:      "test-token",
		HTTPClient: server.Client(),
		Ctx:        context.Background(),
	})
	// A new server may reuse an earlier test's address, which keys the cache.
	rhoai.ControlPlaneCache.Purge()

	return client, func() { server.Close(); resetClusterCaches() }
}

func TestTestPullSecret_Valid(t *testing.T) {
	// Construct a valid pull secret response
	dockerConfig := map[string]any{
		"auths": map[string]any{
			"quay.io/rhoai": map[string]any{
				"auth": exampleAuth("test:test"), // test:test
			},
		},
	}
	dockerConfigJSON, _ := json.Marshal(dockerConfig)
	dockerConfigB64 := base64.StdEncoding.EncodeToString(dockerConfigJSON)

	secretData := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"data": map[string]any{
			".dockerconfigjson": dockerConfigB64,
		},
	}
	secretJSON, _ := json.Marshal(secretData)

	client, cleanup := newMockClient(map[string]mockResponse{
		"/api/v1/namespaces/kube-system/secrets/additional-pull-secret": {
			body:       string(secretJSON),
			statusCode: 200,
		},
	})
	defer cleanup()

	result, err := TestPullSecret(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success, got failure: %s", result.Message)
	}
	if len(result.Logs) == 0 {
		t.Error("expected non-empty logs")
	}
}

func TestTestPullSecret_RejectedByQuay(t *testing.T) {
	defer registry.SetVerifyQuayCredentialsForTest(func(string) error { return &registry.QuayAuthError{StatusCode: 401, Body: "unauthorized"} })()

	dockerConfig := map[string]any{
		"auths": map[string]any{
			"quay.io/rhoai": map[string]any{
				"auth": exampleAuth("test:test"),
			},
		},
	}
	dockerConfigJSON, _ := json.Marshal(dockerConfig)
	dockerConfigB64 := base64.StdEncoding.EncodeToString(dockerConfigJSON)

	secretData := map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"data": map[string]any{".dockerconfigjson": dockerConfigB64},
	}
	secretJSON, _ := json.Marshal(secretData)

	client, cleanup := newMockClient(map[string]mockResponse{
		"/api/v1/namespaces/kube-system/secrets/additional-pull-secret": {
			body:       string(secretJSON),
			statusCode: 200,
		},
	})
	defer cleanup()

	result, err := TestPullSecret(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Error("expected failure when Quay rejects credentials")
	}
	if !strings.Contains(result.Message, "Credentials rejected") {
		t.Errorf("expected 'Credentials rejected' in message, got: %s", result.Message)
	}
}

func TestUpdate_PrerequisitesNotMet(t *testing.T) {
	// Mock: pull secret does not exist (404)
	client, cleanup := newMockClient(map[string]mockResponse{
		"/api/v1/namespaces/kube-system/secrets/additional-pull-secret": {
			body:       `{"kind":"Status","status":"Failure","message":"not found","reason":"NotFound","code":404}`,
			statusCode: 404,
		},
	})
	defer cleanup()

	result, err := Update(client, "quay.io/rhoai/test:latest", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Error("expected failure when pull secret is missing")
	}
	if result.ErrorCode != "prerequisites" {
		t.Errorf("expected errorCode 'prerequisites', got '%s'", result.ErrorCode)
	}
}

func TestRollback_AlreadyOnStable(t *testing.T) {
	stableSource := "redhat-operators"
	stableChannel := "stable-3.5"
	if v := os.Getenv("STABLE_SOURCE"); v != "" {
		stableSource = v
	}
	if v := os.Getenv("STABLE_CHANNEL"); v != "" {
		stableChannel = v
	}

	subResponse := map[string]any{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "Subscription",
		"metadata":   map[string]any{"name": rhoai.SubName, "namespace": rhoai.SubNS, "uid": "uid-sub"},
		"spec": map[string]any{
			"source":  stableSource,
			"channel": stableChannel,
		},
		"status": map[string]any{
			"state": "AtLatestKnown",
		},
	}
	subJSON, _ := json.Marshal(subResponse)

	subPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/subscriptions/%s", rhoai.SubNS, rhoai.SubName)
	client, cleanup := newMockClient(map[string]mockResponse{
		rhoai.NamespacedPath("packages.operators.coreos.com/v1", "packagemanifests", rhoai.CatalogNS, ""): stableCatalogMock(stableSource, stableChannel, "3.5.0"),
		subPath: {
			body:       string(subJSON),
			statusCode: 200,
		},
		rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", rhoai.SubNS, ""): {body: `{"items":[` + succeededCSV + `]}`},
	})
	defer cleanup()

	result, err := Reinstall(client, "stable", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success, got failure: %s", result.Message)
	}
	if result.Message != "Already on the GA release. Nothing to install." {
		t.Errorf("unexpected message: %s", result.Message)
	}
}

// requestRecord captures the method and path of an HTTP request.
type requestRecord struct {
	Method string
	Path   string
}

// newRecordingMockClient creates a test Client that records every request and
// routes responses by "METHOD path" key. Falls back to GET-only path matching
// for backward compatibility, then to 200 with empty JSON object for any
// unmatched request (to keep side-effect calls like record.Activity happy).
func newRecordingMockClient(responses map[string]mockResponse) (*Client, *[]requestRecord, func()) {
	var mu sync.Mutex
	var records []requestRecord

	server := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dashboardOperatorAbsent(w, r) {
			return
		}
		mu.Lock()
		records = append(records, requestRecord{Method: r.Method, Path: r.URL.Path})
		mu.Unlock()

		if strings.Contains(r.URL.Path, "/catalogsources/"+rhoai.CatalogName+"-verify-") {
			if r.Method == "GET" {
				fmt.Fprint(w, `{"status":{"connectionState":{"lastObservedState":"READY"}}}`)
			} else {
				fmt.Fprint(w, `{}`)
			}
			return
		}
		key := r.Method + " " + r.URL.Path
		resp, ok := responses[key]
		if !ok {
			// Fall back to path-only lookup
			resp, ok = responses[r.URL.Path]
		}
		if !ok {
			// Return empty success JSON for unmatched paths (e.g. activity configmap, snapshot, user API)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"apiVersion":"v1","kind":"ConfigMap","data":{}}`))
			return
		}
		if resp.statusCode == 0 {
			resp.statusCode = 200
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.statusCode)
		if strings.HasSuffix(r.URL.Path, "/packagemanifests") && strings.Contains(r.URL.Query().Get("labelSelector"), "-verify-") {
			resp.body = strings.ReplaceAll(resp.body, `"catalogSource":"`+rhoai.CatalogName+`"`, `"catalogSource":"`+strings.TrimPrefix(r.URL.Query().Get("labelSelector"), "catalog=")+`"`)
		}
		w.Write([]byte(resp.body))
	}))

	client := newTestClient(testClientOptions{
		BaseURL:    server.URL,
		Token:      "test-token",
		HTTPClient: server.Client(),
		Ctx:        context.Background(),
	})

	return client, &records, server.Close
}

func TestParseCSVVersion(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		wantOK bool
		major  int
		minor  int
		patch  int
		ea     int
	}{
		{
			name:   "EA build",
			input:  "rhods-operator.3.5.0-ea.1",
			wantOK: true,
			major:  3, minor: 5, patch: 0, ea: 1,
		},
		{
			name:   "GA with v prefix",
			input:  "rhods-operator.v3.5.0",
			wantOK: true,
			major:  3, minor: 5, patch: 0, ea: -1,
		},
		{
			name:   "pre-release rc",
			input:  "rhods-operator.3.5.0-rc.1",
			wantOK: true,
			major:  3, minor: 5, patch: 0, ea: -1000,
		},
		{
			name:   "GA without v prefix",
			input:  "rhods-operator.3.5.0",
			wantOK: true,
			major:  3, minor: 5, patch: 0, ea: -1,
		},
		{
			name:   "invalid name",
			input:  "invalid-name",
			wantOK: false,
		},
		{
			name:   "empty string",
			input:  "",
			wantOK: false,
		},
		{
			name:   "EA build higher number",
			input:  "rhods-operator.3.5.0-ea.5",
			wantOK: true,
			major:  3, minor: 5, patch: 0, ea: 5,
		},
		{
			name:   "different version numbers",
			input:  "rhods-operator.2.10.3-ea.2",
			wantOK: true,
			major:  2, minor: 10, patch: 3, ea: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := registry.ParseCSVVersion(tt.input)
			if ok != tt.wantOK {
				t.Fatalf("parseCSVVersion(%q) ok = %v, want %v", tt.input, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.Major != tt.major {
				t.Errorf("major = %d, want %d", got.Major, tt.major)
			}
			if got.Minor != tt.minor {
				t.Errorf("minor = %d, want %d", got.Minor, tt.minor)
			}
			if got.Patch != tt.patch {
				t.Errorf("patch = %d, want %d", got.Patch, tt.patch)
			}
			if got.EA != tt.ea {
				t.Errorf("ea = %d, want %d", got.EA, tt.ea)
			}
		})
	}
}

func TestCompareTags(t *testing.T) {
	tests := []struct {
		name string
		a    registry.Tag
		b    registry.Tag
		want string // "a>b", "a<b", "a==b"
	}{
		{
			name: "GA beats EA of same version",
			a:    registry.Tag{Raw: "ga", Major: 3, Minor: 5, Patch: 0, EA: -1},
			b:    registry.Tag{Raw: "ea", Major: 3, Minor: 5, Patch: 0, EA: 2},
			want: "a>b",
		},
		{
			name: "EA2 beats EA1",
			a:    registry.Tag{Raw: "ea2", Major: 3, Minor: 5, Patch: 0, EA: 2},
			b:    registry.Tag{Raw: "ea1", Major: 3, Minor: 5, Patch: 0, EA: 1},
			want: "a>b",
		},
		{
			name: "higher minor wins",
			a:    registry.Tag{Raw: "3.5.0", Major: 3, Minor: 5, Patch: 0, EA: -1},
			b:    registry.Tag{Raw: "3.4.1", Major: 3, Minor: 4, Patch: 1, EA: -1},
			want: "a>b",
		},
		{
			name: "pre-release below EA",
			a:    registry.Tag{Raw: "rc", Major: 3, Minor: 5, Patch: 0, EA: -1000},
			b:    registry.Tag{Raw: "ea1", Major: 3, Minor: 5, Patch: 0, EA: 1},
			want: "a<b",
		},
		{
			name: "pre-release below GA",
			a:    registry.Tag{Raw: "rc", Major: 3, Minor: 5, Patch: 0, EA: -1000},
			b:    registry.Tag{Raw: "ga", Major: 3, Minor: 5, Patch: 0, EA: -1},
			want: "a<b",
		},
		{
			name: "EA below GA",
			a:    registry.Tag{Raw: "ea", Major: 3, Minor: 5, Patch: 0, EA: 3},
			b:    registry.Tag{Raw: "ga", Major: 3, Minor: 5, Patch: 0, EA: -1},
			want: "a<b",
		},
		{
			name: "same GA versions are equal",
			a:    registry.Tag{Raw: "a", Major: 3, Minor: 5, Patch: 0, EA: -1},
			b:    registry.Tag{Raw: "b", Major: 3, Minor: 5, Patch: 0, EA: -1},
			want: "a==b",
		},
		{
			name: "higher major wins",
			a:    registry.Tag{Raw: "4.0.0", Major: 4, Minor: 0, Patch: 0, EA: -1},
			b:    registry.Tag{Raw: "3.9.9", Major: 3, Minor: 9, Patch: 9, EA: -1},
			want: "a>b",
		},
		{
			name: "higher patch wins",
			a:    registry.Tag{Raw: "3.5.2", Major: 3, Minor: 5, Patch: 2, EA: -1},
			b:    registry.Tag{Raw: "3.5.1", Major: 3, Minor: 5, Patch: 1, EA: -1},
			want: "a>b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := registry.CompareTags(tt.a, tt.b)
			switch tt.want {
			case "a>b":
				if result <= 0 {
					t.Errorf("compareTags(%s, %s) = %d, want > 0", tt.a.Raw, tt.b.Raw, result)
				}
			case "a<b":
				if result >= 0 {
					t.Errorf("compareTags(%s, %s) = %d, want < 0", tt.a.Raw, tt.b.Raw, result)
				}
			case "a==b":
				if result != 0 {
					t.Errorf("compareTags(%s, %s) = %d, want 0", tt.a.Raw, tt.b.Raw, result)
				}
			}
		})
	}
}

// pkgManifestPaths returns both the label-selector list path and the direct
// by-name path for packagemanifest mocks.
func pkgManifestPaths() (listPath, directPath string) {
	return fmt.Sprintf("/apis/packages.operators.coreos.com/v1/namespaces/%s/packagemanifests", rhoai.CatalogNS),
		fmt.Sprintf("/apis/packages.operators.coreos.com/v1/namespaces/%s/packagemanifests/%s", rhoai.CatalogNS, "rhods-operator")
}

// wrapInList wraps a single packagemanifest JSON body into a list response.
func wrapPkgManifestList(singleBody string) string {
	var obj map[string]any
	if err := json.Unmarshal([]byte(singleBody), &obj); err != nil {
		return `{"items":[]}`
	}
	obj["metadata"] = map[string]any{"name": "rhods-operator"}
	status, _ := obj["status"].(map[string]any)
	if status == nil {
		status = map[string]any{}
		obj["status"] = status
	}
	status["packageName"] = rhoai.SubName
	status["catalogSource"] = rhoai.CatalogName
	status["catalogSourceNamespace"] = rhoai.CatalogNS
	wrapped := map[string]any{"items": []any{obj}}
	b, _ := json.Marshal(wrapped)
	return string(b)
}

func TestDetectNightlyChannel(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     string
	}{
		{
			name: "picks channel with highest version CSV",
			response: `{
				"status": {
					"channels": [
						{"name": "alpha", "currentCSV": "rhods-operator.3.4.0"},
						{"name": "beta", "currentCSV": "rhods-operator.3.5.0-ea.1"},
						{"name": "fast", "currentCSV": "rhods-operator.3.6.0"}
					]
				}
			}`,
			want: "fast",
		},
		{
			name: "single channel",
			response: `{
				"status": {
					"channels": [
						{"name": "beta", "currentCSV": "rhods-operator.3.5.0-ea.1"}
					]
				}
			}`,
			want: "beta",
		},
		{
			name:     "empty channels",
			response: `{"status": {"channels": []}}`,
			want:     "",
		},
		{
			name:     "missing status",
			response: `{}`,
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listPath, directPath := pkgManifestPaths()
			client, cleanup := newMockClient(map[string]mockResponse{
				listPath:   {body: wrapPkgManifestList(tt.response)},
				directPath: {body: tt.response},
			})
			defer cleanup()

			got, _ := operator.DetectNightlyChannel(client, "")
			if got != tt.want {
				t.Errorf("detectNightlyChannel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectNightlyChannel_APIError(t *testing.T) {
	// Mock returns 500 for the packagemanifest endpoint
	pkgPath := fmt.Sprintf("/apis/packages.operators.coreos.com/v1/namespaces/%s/packagemanifests/%s",
		rhoai.CatalogNS, "rhods-operator")
	client, cleanup := newMockClient(map[string]mockResponse{
		pkgPath: {body: `{"kind":"Status","status":"Failure","code":500}`, statusCode: 500},
	})
	defer cleanup()

	got, err := operator.DetectNightlyChannel(client, "")
	if got != "" {
		t.Errorf("detectNightlyChannel() on API error = %q, want empty string", got)
	}
	if err == nil {
		t.Error("detectNightlyChannel() on API error should return an error")
	}
}

func TestCreatePullSecret_RejectsEmptyAuth(t *testing.T) {
	client, cleanup := newMockClient(map[string]mockResponse{})
	defer cleanup()

	result, err := CreatePullSecret(client, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Error("expected failure for empty auth string")
	}
	if result.ErrorCode != "validation" {
		t.Errorf("expected errorCode 'validation', got '%s'", result.ErrorCode)
	}
	if result.Message != "Auth value is required" {
		t.Errorf("unexpected message: %s", result.Message)
	}
}

func TestCreatePullSecret_RejectsControlCharacters(t *testing.T) {
	client, cleanup := newMockClient(map[string]mockResponse{})
	defer cleanup()

	result, err := CreatePullSecret(client, exampleAuth("test:test")+"\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Error("expected failure for auth with control characters")
	}
	if result.ErrorCode != "validation" {
		t.Errorf("expected errorCode 'validation', got '%s'", result.ErrorCode)
	}
	if result.Message != "Auth value contains invalid characters" {
		t.Errorf("unexpected message: %s", result.Message)
	}
}

func TestCreatePullSecret_RejectsInvalidBase64(t *testing.T) {
	client, cleanup := newMockClient(map[string]mockResponse{})
	defer cleanup()

	result, err := CreatePullSecret(client, "not-valid-base64!!!")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Error("expected failure for invalid base64")
	}
	if result.ErrorCode != "validation" {
		t.Errorf("expected errorCode 'validation', got '%s'", result.ErrorCode)
	}
	if result.Message != "Auth value is not valid base64" {
		t.Errorf("unexpected message: %s", result.Message)
	}
}

func TestCreatePullSecret_RejectsEmptyUsernameAndPassword(t *testing.T) {
	client, cleanup := newMockClient(map[string]mockResponse{})
	defer cleanup()

	// Empty username and empty password.
	result, err := CreatePullSecret(client, exampleAuth(":"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Error("expected failure for base64 of just ':'")
	}
}

func TestCreatePullSecret_RejectsEmptyPassword(t *testing.T) {
	client, cleanup := newMockClient(map[string]mockResponse{})
	defer cleanup()

	// Empty password.
	result, err := CreatePullSecret(client, exampleAuth("user:"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Error("expected failure for base64 with empty password")
	}
}

func TestCreatePullSecret_AcceptsValidAuth(t *testing.T) {
	// Build mock responses for the success path.
	// CreatePullSecret checks if the secret exists (GET), then creates/updates it,
	// then reads it back via getPullSecret for verification.
	dockerConfig := map[string]any{
		"auths": map[string]any{
			"quay.io/rhoai": map[string]any{
				"auth": exampleAuth("user:password"), // user:password
			},
		},
	}
	dockerConfigJSON, _ := json.Marshal(dockerConfig)
	dockerConfigB64 := base64.StdEncoding.EncodeToString(dockerConfigJSON)

	secretData := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"data": map[string]any{
			".dockerconfigjson": dockerConfigB64,
		},
	}
	secretJSON, _ := json.Marshal(secretData)

	pullSecretPath := "/api/v1/namespaces/kube-system/secrets/additional-pull-secret"

	client, _, cleanup := newRecordingMockClient(map[string]mockResponse{
		pullSecretPath: {body: string(secretJSON), statusCode: 200},
	})
	defer cleanup()

	result, err := CreatePullSecret(client, exampleAuth("user:password"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success, got failure: %s\nlogs: %v", result.Message, result.Logs)
	}
}

func TestUpdate_FullRefreshFlow(t *testing.T) {
	testImage := "quay.io/rhoai/rhoai-fbc-fragment:nightly-test"

	// Build mock pull secret response
	dockerConfig := map[string]any{
		"auths": map[string]any{
			"quay.io/rhoai": map[string]any{"auth": exampleAuth("test:test")},
		},
	}
	dockerConfigJSON, _ := json.Marshal(dockerConfig)
	dockerConfigB64 := base64.StdEncoding.EncodeToString(dockerConfigJSON)

	secretData := map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"data": map[string]any{".dockerconfigjson": dockerConfigB64},
	}
	secretJSON, _ := json.Marshal(secretData)

	// Build IDMS response (has matching source)
	idmsResponse := map[string]any{
		"items": []any{
			map[string]any{
				"metadata": map[string]any{"name": "rhoai-mirror"},
				"spec": map[string]any{
					"imageDigestMirrors": []any{
						map[string]any{"source": rhoai.IDMSSource},
					},
				},
			},
		},
	}
	idmsJSON, _ := json.Marshal(idmsResponse)

	// Build CatalogSource response (already exists with old image)
	csResponse := map[string]any{
		"apiVersion": "operators.coreos.com/v1alpha1", "kind": "CatalogSource",
		"metadata": map[string]any{"name": rhoai.CatalogName, "namespace": rhoai.CatalogNS},
		"spec":     map[string]any{"image": "quay.io/rhoai/rhoai-fbc-fragment:old"},
		"status": map[string]any{
			"connectionState": map[string]any{"lastObservedState": "READY"},
		},
	}
	csJSON, _ := json.Marshal(csResponse)

	// Build Subscription response (points to nightly catalog, with installPlanRef for verify step)
	subResponse := map[string]any{
		"apiVersion": "operators.coreos.com/v1alpha1", "kind": "Subscription",
		"metadata": map[string]any{"name": rhoai.SubName, "namespace": rhoai.SubNS, "uid": "uid-sub"},
		"spec":     map[string]any{"source": rhoai.CatalogName, "channel": "fast"},
		"status": map[string]any{
			"state":        "AtLatestKnown",
			"currentCSV":   "rhods-operator.v3.5.0",
			"installedCSV": "rhods-operator.v3.5.0",
			"installPlanRef": map[string]any{
				"name": "install-plan-abc123",
			},
		},
	}
	subJSON, _ := json.Marshal(subResponse)

	// Build CSV list response
	csvResponse := map[string]any{
		"items": []any{
			map[string]any{
				"metadata": map[string]any{"name": "rhods-operator.v3.5.0"},
				"spec":     map[string]any{"displayName": "Red Hat OpenShift AI", "version": "3.5.0"},
				"status":   map[string]any{"phase": "Succeeded"},
			},
		},
	}
	csvJSON, _ := json.Marshal(csvResponse)

	// Build dashboard deployment response (non-PR, so Update does not print a warning)
	dashDeploy := map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{"opendatahub.io/managed": "true"}},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": "rhods-dashboard"}},
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{
						map[string]any{"name": "rhods-dashboard", "image": "registry.redhat.io/rhoai/odh-dashboard-rhel9:v3.5"},
					},
				},
			},
		},
	}
	dashDeployJSON, _ := json.Marshal(dashDeploy)
	emptyPodList, _ := json.Marshal(map[string]any{"items": []any{}})
	emptyDeployList, _ := json.Marshal(map[string]any{"items": []any{}})

	// Build packagemanifest response for detectNightlyChannel
	pkgManifest := map[string]any{
		"status": map[string]any{
			"channels": []any{
				map[string]any{"name": "fast", "currentCSV": "rhods-operator.v3.5.0"},
			},
		},
	}
	pkgManifestJSON, _ := json.Marshal(pkgManifest)

	// OperatorGroup response
	ogResponse := map[string]any{
		"items": []any{
			map[string]any{
				"metadata": map[string]any{"name": "redhat-ods-operator-og", "namespace": rhoai.SubNS},
			},
		},
	}
	ogJSON, _ := json.Marshal(ogResponse)

	// Path constants
	pullSecretPath := "/api/v1/namespaces/kube-system/secrets/additional-pull-secret"
	idmsPath := "/apis/config.openshift.io/v1/imagedigestmirrorsets"
	csPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/catalogsources/%s", rhoai.CatalogNS, rhoai.CatalogName)
	subPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/subscriptions/%s", rhoai.SubNS, rhoai.SubName)
	csvListPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/clusterserviceversions", rhoai.SubNS)
	csvDeletePath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/clusterserviceversions/%s", rhoai.SubNS, "rhods-operator.v3.5.0")
	dashDeployPath := "/apis/apps/v1/namespaces/redhat-ods-applications/deployments/rhods-dashboard"
	dashPodsPath := "/api/v1/namespaces/redhat-ods-applications/pods"
	pkgManifestPath := fmt.Sprintf("/apis/packages.operators.coreos.com/v1/namespaces/%s/packagemanifests/%s", rhoai.CatalogNS, "rhods-operator")
	appDeployListPath := "/apis/apps/v1/namespaces/redhat-ods-applications/deployments"
	opDeployListPath := "/apis/apps/v1/namespaces/redhat-ods-operator/deployments"
	ogPath := fmt.Sprintf("/apis/operators.coreos.com/v1/namespaces/%s/operatorgroups", rhoai.SubNS)

	client, records, cleanup := newRecordingMockClient(map[string]mockResponse{
		pullSecretPath:         {body: string(secretJSON)},
		idmsPath:               {body: string(idmsJSON)},
		csPath:                 {body: string(csJSON)},
		subPath:                {body: string(subJSON)},
		csvListPath:            {body: string(csvJSON)},
		csvDeletePath:          {body: `{"kind":"Status","status":"Success"}`},
		"GET " + csvDeletePath: {body: succeededCSV},
		dashDeployPath:         {body: string(dashDeployJSON)},
		dashPodsPath:           {body: string(emptyPodList)},
		pkgManifestPath:        {body: string(pkgManifestJSON)},
		strings.TrimSuffix(pkgManifestPath, "/"+rhoai.SubName): {body: wrapPkgManifestList(string(pkgManifestJSON))},
		appDeployListPath: {body: string(emptyDeployList)},
		opDeployListPath:  {body: string(emptyDeployList)},
		ogPath:            {body: string(ogJSON)},
	})
	defer cleanup()

	result, err := Update(client, testImage, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got failure: %s\nlogs: %v", result.Message, result.Logs)
	}

	// Verify the mock received the expected requests:
	// New flow: apply Subscription FIRST (idempotent), then delete CSV.
	// No Subscription delete — keeps cluster safe if request times out.
	var csApplied, subApplied, csvDeleted bool
	for _, rec := range *records {
		switch {
		case rec.Method == "PATCH" && rec.Path == csPath:
			csApplied = true
		case rec.Method == "PATCH" && rec.Path == subPath:
			subApplied = true
		case rec.Method == "DELETE" && rec.Path == csvDeletePath:
			csvDeleted = true
		}
	}

	if !csApplied {
		t.Error("expected CatalogSource apply (PATCH) request")
	}
	if !subApplied {
		t.Error("expected Subscription apply (PATCH) request")
	}
	if !csvDeleted {
		t.Error("expected CSV delete request")
	}

	// Verify ordering: CatalogSource apply → Subscription apply → CSV delete
	var csApplyIdx, subApplyIdx, csvDelIdx int
	for i, rec := range *records {
		switch {
		case rec.Method == "PATCH" && rec.Path == csPath && csApplyIdx == 0:
			csApplyIdx = i + 1
		case rec.Method == "PATCH" && rec.Path == subPath && subApplyIdx == 0:
			subApplyIdx = i + 1
		case rec.Method == "DELETE" && rec.Path == csvDeletePath && csvDelIdx == 0:
			csvDelIdx = i + 1
		}
	}

	if csApplyIdx == 0 || subApplyIdx == 0 || csvDelIdx == 0 {
		t.Fatal("not all expected requests were found")
	}
	if csApplyIdx >= subApplyIdx {
		t.Errorf("CatalogSource apply (idx %d) should happen before Subscription apply (idx %d)", csApplyIdx, subApplyIdx)
	}
	// The Subscription is created once, after the old CSV is gone (A01-14).
	if csvDelIdx >= subApplyIdx {
		t.Errorf("CSV delete (idx %d) should happen before Subscription apply (idx %d)", csvDelIdx, subApplyIdx)
	}

	// Verify logs mention key steps
	logText := strings.Join(result.Logs, "\n")
	for _, want := range []string{"CatalogSource applied", "Subscription applied", "deleted"} {
		if !strings.Contains(logText, want) {
			t.Errorf("logs should contain %q, got:\n%s", want, logText)
		}
	}
}

// A missing Subscription no longer stops "Reinstall to stable" (R5-F5): a
// crash may have removed it. The stable target comes from the catalog, so
// here the failure is the unreadable catalog, not the Subscription.
func TestReinstall_Subscription404(t *testing.T) {
	subPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/subscriptions/%s", rhoai.SubNS, rhoai.SubName)
	client, cleanup := newMockClient(map[string]mockResponse{
		subPath: {
			body:       `{"kind":"Status","status":"Failure","message":"subscriptions.operators.coreos.com \"rhods-operator\" not found","reason":"NotFound","code":404}`,
			statusCode: 404,
		},
	})
	defer cleanup()

	result, err := Reinstall(client, "stable", "", "")
	if err != nil || result.Success || !strings.Contains(result.Message, "Cannot determine the GA release to install") {
		t.Fatalf("result = %+v, %v", result, err)
	}
}

// An unhealthy install on the stable target is reinstalled; see
// TestReinstallStableTargets.

func TestReinstall_StableCSVSucceeded(t *testing.T) {
	stableSource := "redhat-operators"
	stableChannel := "stable-3.5"
	if v := os.Getenv("STABLE_SOURCE"); v != "" {
		stableSource = v
	}
	if v := os.Getenv("STABLE_CHANNEL"); v != "" {
		stableChannel = v
	}

	// Subscription pointing to stable source
	subResponse := map[string]any{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "Subscription",
		"metadata":   map[string]any{"name": rhoai.SubName, "namespace": rhoai.SubNS, "uid": "uid-sub"},
		"spec": map[string]any{
			"source":  stableSource,
			"channel": stableChannel,
		},
		"status": map[string]any{"state": "AtLatestKnown"},
	}
	subJSON, _ := json.Marshal(subResponse)

	// CSV list with a Succeeded CSV
	csvResponse := map[string]any{
		"items": []any{
			map[string]any{
				"metadata": map[string]any{"name": "rhods-operator.v3.5.0"},
				"spec":     map[string]any{"displayName": "Red Hat OpenShift AI", "version": "3.5.0"},
				"status":   map[string]any{"phase": "Succeeded"},
			},
		},
	}
	csvJSON, _ := json.Marshal(csvResponse)

	subPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/subscriptions/%s", rhoai.SubNS, rhoai.SubName)
	csvListPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/clusterserviceversions", rhoai.SubNS)

	client, cleanup := newMockClient(map[string]mockResponse{
		rhoai.NamespacedPath("packages.operators.coreos.com/v1", "packagemanifests", rhoai.CatalogNS, ""): stableCatalogMock(stableSource, stableChannel, "3.5.0"),
		subPath:     {body: string(subJSON)},
		csvListPath: {body: string(csvJSON)},
	})
	defer cleanup()

	result, err := Reinstall(client, "stable", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success, got failure: %s", result.Message)
	}
	if result.Message != "Already on the GA release. Nothing to install." {
		t.Errorf("expected the already-on-GA message, got: %s", result.Message)
	}
}

func TestReinstall_NightlyCatalogAndChannel(t *testing.T) {
	nightlyImage := "quay.io/rhoai/rhoai-fbc-fragment:nightly-20260101"

	// Subscription pointing to nightly catalog (so it does not short-circuit on "already stable")
	subResponse := map[string]any{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "Subscription",
		"metadata":   map[string]any{"name": rhoai.SubName, "namespace": rhoai.SubNS, "uid": "uid-sub"},
		"spec": map[string]any{
			"source":  rhoai.CatalogName,
			"channel": "fast",
		},
		"status": map[string]any{"state": "AtLatestKnown", "currentCSV": "rhods-operator.v3.5.0", "installedCSV": "rhods-operator.v3.5.0"},
	}
	subJSON, _ := json.Marshal(subResponse)

	// CSV list with an existing CSV
	csvResponse := map[string]any{
		"items": []any{
			map[string]any{
				"metadata": map[string]any{"name": "rhods-operator.v3.5.0"},
				"spec":     map[string]any{"displayName": "Red Hat OpenShift AI", "version": "3.5.0"},
				"status":   map[string]any{"phase": "Succeeded"},
			},
		},
	}
	csvJSON, _ := json.Marshal(csvResponse)

	// Package manifest response: nightly catalog has multiple channels
	pkgManifest := map[string]any{
		"status": map[string]any{
			"channels": []any{
				map[string]any{"name": "fast", "currentCSV": "rhods-operator.v3.4.0"},
				map[string]any{"name": "beta", "currentCSV": "rhods-operator.v3.5.0-ea.1"},
				map[string]any{"name": "stable-3.5", "currentCSV": "rhods-operator.v3.5.0"},
			},
		},
	}
	pkgManifestJSON, _ := json.Marshal(pkgManifest)

	// Empty webhook lists
	emptyList, _ := json.Marshal(map[string]any{"items": []any{}})

	subPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/subscriptions/%s", rhoai.SubNS, rhoai.SubName)
	csvListPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/clusterserviceversions", rhoai.SubNS)
	csvDeletePath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/clusterserviceversions/%s", rhoai.SubNS, "rhods-operator.v3.5.0")
	csPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/catalogsources/%s", rhoai.CatalogNS, rhoai.CatalogName)
	pkgManifestListPath, pkgManifestPath := pkgManifestPaths()
	vwhPath := "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations"
	mwhPath := "/apis/admissionregistration.k8s.io/v1/mutatingwebhookconfigurations"
	crd1Path := "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/datascienceclusters.datasciencecluster.opendatahub.io"
	crd2Path := "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/dscinitializations.dscinitialization.opendatahub.io"
	appDeployListPath := "/apis/apps/v1/namespaces/redhat-ods-applications/deployments"
	opDeployListPath := "/apis/apps/v1/namespaces/redhat-ods-operator/deployments"

	csReadyResponse, _ := json.Marshal(map[string]any{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "CatalogSource",
		"metadata":   map[string]any{"name": rhoai.CatalogName, "namespace": rhoai.CatalogNS},
		"spec":       map[string]any{"image": nightlyImage},
		"status":     map[string]any{"connectionState": map[string]any{"lastObservedState": "READY"}},
	})

	client, records, cleanup := newRecordingMockClient(map[string]mockResponse{
		subPath:                {body: string(subJSON)},
		csvListPath:            {body: string(csvJSON)},
		csvDeletePath:          {body: `{"kind":"Status","status":"Success"}`},
		"GET " + csvDeletePath: {body: succeededCSV},
		csPath:                 {body: string(csReadyResponse)},
		pkgManifestListPath:    {body: wrapPkgManifestList(string(pkgManifestJSON))},
		pkgManifestPath:        {body: string(pkgManifestJSON)},
		vwhPath:                {body: string(emptyList)},
		mwhPath:                {body: string(emptyList)},
		crd1Path:               {body: `{"kind":"CustomResourceDefinition"}`},
		crd2Path:               {body: `{"kind":"CustomResourceDefinition"}`},
		appDeployListPath:      {body: string(emptyList)},
		opDeployListPath:       {body: string(emptyList)},
	})
	defer cleanup()

	result, err := Reinstall(client, "nightly", nightlyImage, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got failure: %s\nlogs: %v", result.Message, result.Logs)
	}

	// Verify CatalogSource was created (PATCH = server-side apply)
	var csApplied bool
	for _, rec := range *records {
		if rec.Method == "PATCH" && rec.Path == csPath {
			csApplied = true
			break
		}
	}
	if !csApplied {
		t.Error("expected CatalogSource apply (PATCH) request for nightly image")
	}

	// Verify Subscription was created (PATCH)
	var subApplied bool
	for _, rec := range *records {
		if rec.Method == "PATCH" && rec.Path == subPath {
			subApplied = true
			break
		}
	}
	if !subApplied {
		t.Error("expected Subscription apply (PATCH) request")
	}

	// Verify logs mention the detected nightly channel
	logText := strings.Join(result.Logs, "\n")
	if !strings.Contains(logText, "Verified replacement channel: stable-3.5") {
		t.Errorf("logs should contain detected nightly channel 'stable-3.5', got:\n%s", logText)
	}

	// Verify logs mention CatalogSource creation
	if !strings.Contains(logText, "CatalogSource created with nightly image") {
		t.Errorf("logs should mention CatalogSource creation, got:\n%s", logText)
	}
}

// buildRefreshMocks returns the common mock paths and JSON responses used across
// RefreshOperator tests. Callers can override individual entries before passing
// the map to newRecordingMockClient.
func buildRefreshMocks() (map[string]mockResponse, map[string]string) {
	csvListPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/clusterserviceversions", rhoai.SubNS)
	csvDeletePath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/clusterserviceversions/%s", rhoai.SubNS, "rhods-operator.v3.5.0")
	subPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/subscriptions/%s", rhoai.SubNS, rhoai.SubName)
	appDeployListPath := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments", "redhat-ods-applications")
	opDeployListPath := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments", "redhat-ods-operator")

	csvResponse := map[string]any{
		"items": []any{
			map[string]any{
				"metadata": map[string]any{"name": "rhods-operator.v3.5.0"},
				"spec":     map[string]any{"displayName": "Red Hat OpenShift AI", "version": "3.5.0"},
				"status":   map[string]any{"phase": "Succeeded"},
			},
		},
	}
	csvJSON, _ := json.Marshal(csvResponse)

	subResponse := map[string]any{
		"apiVersion": "operators.coreos.com/v1alpha1", "kind": "Subscription",
		"metadata": map[string]any{"name": rhoai.SubName, "namespace": rhoai.SubNS, "uid": "uid-sub"},
		"spec":     map[string]any{"source": rhoai.CatalogName, "channel": "fast"},
		"status":   map[string]any{"state": "AtLatestKnown", "currentCSV": "rhods-operator.v3.5.0", "installedCSV": "rhods-operator.v3.5.0"},
	}
	subJSON, _ := json.Marshal(subResponse)

	emptyDeployList, _ := json.Marshal(map[string]any{"items": []any{}})

	paths := map[string]string{
		"csvList":       csvListPath,
		"csvDelete":     csvDeletePath,
		"sub":           subPath,
		"appDeployList": appDeployListPath,
		"opDeployList":  opDeployListPath,
	}

	responses := map[string]mockResponse{
		csvListPath:            {body: string(csvJSON)},
		csvDeletePath:          {body: `{"kind":"Status","status":"Success"}`},
		"GET " + csvDeletePath: {body: succeededCSV},
		subPath:                {body: string(subJSON)},
		appDeployListPath:      {body: string(emptyDeployList)},
		opDeployListPath:       {body: string(emptyDeployList)},
		// The channel still installs the installed CSV, so Refresh does not
		// become an upgrade.
		"/apis/packages.operators.coreos.com/v1/namespaces/" + rhoai.CatalogNS + "/packagemanifests": {body: `{"items":[{"metadata":{"name":"` + rhoai.SubName + `"},"status":{"packageName":"` + rhoai.SubName + `","catalogSource":"` + rhoai.CatalogName + `","catalogSourceNamespace":"` + rhoai.CatalogNS + `","channels":[{"name":"fast","currentCSV":"rhods-operator.v3.5.0"}]}}]}`},
	}

	return responses, paths
}

func TestRefreshOperator_Success(t *testing.T) {
	responses, paths := buildRefreshMocks()

	client, records, cleanup := newRecordingMockClient(responses)
	defer cleanup()

	result, err := RefreshOperator(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got failure: %s\nlogs: %v", result.Message, result.Logs)
	}

	// Verify the expected K8s API calls were made
	var csvDeleted, subDeleted, subApplied bool
	for _, rec := range *records {
		switch {
		case rec.Method == "DELETE" && rec.Path == paths["csvDelete"]:
			csvDeleted = true
		case rec.Method == "DELETE" && rec.Path == paths["sub"]:
			subDeleted = true
		case rec.Method == "PATCH" && rec.Path == paths["sub"]:
			subApplied = true
		}
	}

	if !csvDeleted {
		t.Error("expected CSV delete request")
	}
	if !subDeleted {
		t.Error("expected Subscription delete request")
	}
	if !subApplied {
		t.Error("expected Subscription apply (PATCH) request")
	}

	// Verify ordering: Sub delete -> CSV delete -> Sub apply (PATCH). With the
	// Subscription still present OLM would start reinstalling as soon as the
	// CSV is gone, and deleting the Subscription then would cut that short.
	var csvDelIdx, subDelIdx, subApplyIdx int
	for i, rec := range *records {
		switch {
		case rec.Method == "DELETE" && rec.Path == paths["csvDelete"] && csvDelIdx == 0:
			csvDelIdx = i + 1
		case rec.Method == "DELETE" && rec.Path == paths["sub"] && subDelIdx == 0:
			subDelIdx = i + 1
		case rec.Method == "PATCH" && rec.Path == paths["sub"] && subApplyIdx == 0:
			subApplyIdx = i + 1
		}
	}

	if csvDelIdx == 0 || subDelIdx == 0 || subApplyIdx == 0 {
		t.Fatal("not all expected requests were found")
	}
	if subDelIdx >= csvDelIdx {
		t.Errorf("Subscription delete (idx %d) should happen before CSV delete (idx %d)", subDelIdx, csvDelIdx)
	}
	if csvDelIdx >= subApplyIdx {
		t.Errorf("CSV delete (idx %d) should happen before Subscription apply (idx %d)", csvDelIdx, subApplyIdx)
	}

	// Verify logs contain the key steps
	logText := strings.Join(result.Logs, "\n")
	for _, want := range []string{"Deleting CSV", "Subscription deleted", "Subscription recreated"} {
		if !strings.Contains(logText, want) {
			t.Errorf("logs should contain %q, got:\n%s", want, logText)
		}
	}
}

func TestRefreshOperator_RetriesSubscriptionCreation(t *testing.T) {
	csvListPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/clusterserviceversions", rhoai.SubNS)
	csvDeletePath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/clusterserviceversions/%s", rhoai.SubNS, "rhods-operator.v3.5.0")
	subPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/subscriptions/%s", rhoai.SubNS, rhoai.SubName)
	appDeployListPath := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments", "redhat-ods-applications")
	opDeployListPath := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments", "redhat-ods-operator")

	csvResponse := map[string]any{
		"items": []any{
			map[string]any{
				"metadata": map[string]any{"name": "rhods-operator.v3.5.0"},
				"spec":     map[string]any{"displayName": "Red Hat OpenShift AI", "version": "3.5.0"},
				"status":   map[string]any{"phase": "Succeeded"},
			},
		},
	}
	csvJSON, _ := json.Marshal(csvResponse)

	subResponse := map[string]any{
		"apiVersion": "operators.coreos.com/v1alpha1", "kind": "Subscription",
		"metadata": map[string]any{"name": rhoai.SubName, "namespace": rhoai.SubNS, "uid": "uid-sub"},
		"spec":     map[string]any{"source": rhoai.CatalogName, "channel": "fast"},
		"status":   map[string]any{"state": "AtLatestKnown", "currentCSV": "rhods-operator.v3.5.0", "installedCSV": "rhods-operator.v3.5.0"},
	}
	subJSON, _ := json.Marshal(subResponse)

	emptyDeployList, _ := json.Marshal(map[string]any{"items": []any{}})

	// Custom server: fail the first 2 PATCH requests to the sub path, succeed on 3rd
	var mu sync.Mutex
	var records []requestRecord
	patchAttempts := 0

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dashboardOperatorAbsent(w, r) {
			return
		}
		mu.Lock()
		records = append(records, requestRecord{Method: r.Method, Path: r.URL.Path})
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")

		// Handle PATCH to sub path with failures on first 2 attempts
		if r.Method == "PATCH" && r.URL.Path == subPath {
			mu.Lock()
			patchAttempts++
			attempt := patchAttempts
			mu.Unlock()
			if attempt <= 2 {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"kind":"Status","status":"Failure","message":"internal error","reason":"InternalError","code":500}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write(subJSON)
			return
		}

		// Route other requests
		key := r.Method + " " + r.URL.Path
		responses := map[string]mockResponse{
			"GET " + csvListPath:       {body: string(csvJSON)},
			"GET " + subPath:           {body: string(subJSON)},
			"DELETE " + csvDeletePath:  {body: `{"kind":"Status","status":"Success"}`},
			"GET " + csvDeletePath:     {body: succeededCSV},
			"DELETE " + subPath:        {body: `{"kind":"Status","status":"Success"}`},
			"GET " + appDeployListPath: {body: string(emptyDeployList)},
			"GET " + opDeployListPath:  {body: string(emptyDeployList)},
			"GET /apis/packages.operators.coreos.com/v1/namespaces/" + rhoai.CatalogNS + "/packagemanifests": {body: `{"items":[{"metadata":{"name":"` + rhoai.SubName + `"},"status":{"packageName":"` + rhoai.SubName + `","catalogSource":"` + rhoai.CatalogName + `","catalogSourceNamespace":"` + rhoai.CatalogNS + `","channels":[{"name":"fast","currentCSV":"rhods-operator.v3.5.0"}]}}]}`},
		}
		resp, ok := responses[key]
		if !ok {
			// Fallback: return empty success for unmatched (activity configmap, etc.)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"apiVersion":"v1","kind":"ConfigMap","data":{}}`))
			return
		}
		if resp.statusCode == 0 {
			resp.statusCode = 200
		}
		w.WriteHeader(resp.statusCode)
		w.Write([]byte(resp.body))
	}))
	defer server.Close()

	client := newTestClient(testClientOptions{
		BaseURL:    server.URL,
		Token:      "test-token",
		HTTPClient: server.Client(),
		Ctx:        context.Background(),
	})

	result, err := RefreshOperator(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success after retry, got failure: %s\nlogs: %v", result.Message, result.Logs)
	}

	// Verify that 3 PATCH requests were made to the sub path
	mu.Lock()
	actualAttempts := patchAttempts
	mu.Unlock()
	if actualAttempts != 3 {
		t.Errorf("expected 3 subscription apply attempts, got %d", actualAttempts)
	}

	// Verify logs mention the retry
	logText := strings.Join(result.Logs, "\n")
	if !strings.Contains(logText, "Subscription recreated") {
		t.Errorf("logs should contain 'Subscription recreated', got:\n%s", logText)
	}
}

func TestRefreshOperator_NoCSV(t *testing.T) {
	// Return an empty CSV list so getCSV returns csv.Name == ""
	csvListPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/clusterserviceversions", rhoai.SubNS)
	emptyCSVResponse := map[string]any{
		"items": []any{},
	}
	emptyCSVJSON, _ := json.Marshal(emptyCSVResponse)

	client, _, cleanup := newRecordingMockClient(map[string]mockResponse{
		csvListPath: {body: string(emptyCSVJSON)},
	})
	defer cleanup()

	result, err := RefreshOperator(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Error("expected success=true when no CSV (informational, not an error)")
	}
	if !strings.Contains(result.Message, "No RHOAI operator CSV found") {
		t.Errorf("expected message about no CSV, got: %s", result.Message)
	}
}

func TestRefreshOperator_ContextCancellation(t *testing.T) {
	responses, paths := buildRefreshMocks()

	// Cancel the operation as soon as the CSV is being deleted, i.e. after the
	// operator has started to be removed.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var recs []requestRecord
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dashboardOperatorAbsent(w, r) {
			return
		}
		mu.Lock()
		recs = append(recs, requestRecord{Method: r.Method, Path: r.URL.Path})
		mu.Unlock()
		if r.Method == "DELETE" && r.URL.Path == paths["csvDelete"] {
			cancel()
		}
		key := r.Method + " " + r.URL.Path
		resp, ok := responses[key]
		if !ok {
			resp, ok = responses[r.URL.Path]
		}
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.Write([]byte(`{"apiVersion":"v1","kind":"ConfigMap","data":{}}`))
			return
		}
		if resp.statusCode == 0 {
			resp.statusCode = 200
		}
		w.WriteHeader(resp.statusCode)
		w.Write([]byte(resp.body))
	}))
	defer server.Close()

	client := newTestClient(testClientOptions{BaseURL: server.URL, Token: "test-token", HTTPClient: server.Client(), Ctx: ctx})
	result, _ := RefreshOperator(client)
	if result == nil || result.Success {
		t.Fatalf("expected failure on context cancellation, got %+v", result)
	}
	if !strings.Contains(result.Message, "restored") {
		t.Errorf("expected the previous Subscription to be restored, got: %s", result.Message)
	}

	// The saved Subscription must be re-applied after the CSV deletion began.
	mu.Lock()
	defer mu.Unlock()
	csvDeleted, restored := false, false
	for _, rec := range recs {
		if rec.Method == "DELETE" && rec.Path == paths["csvDelete"] {
			csvDeleted = true
		}
		if csvDeleted && rec.Method == "PATCH" && rec.Path == paths["sub"] {
			restored = true
		}
	}
	if !csvDeleted || !restored {
		t.Errorf("csvDeleted=%v restored=%v; requests: %v", csvDeleted, restored, recs)
	}
}

func TestRefreshOperator_MissingSubscriptionChangesNothing(t *testing.T) {
	responses, paths := buildRefreshMocks()
	delete(responses, paths["sub"])
	responses["GET "+paths["sub"]] = mockResponse{statusCode: 404, body: `{"kind":"Status","status":"Failure","reason":"NotFound","code":404}`}
	client, records, cleanup := newRecordingMockClient(responses)
	defer cleanup()

	result, err := RefreshOperator(client)
	if err != nil || result.Success || result.ErrorCode != "prerequisites" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, rec := range *records {
		if rec.Method == "DELETE" || rec.Method == "PATCH" && strings.Contains(rec.Path, "/operators.coreos.com/") {
			t.Fatalf("nothing may be changed without a usable Subscription, got %s %s", rec.Method, rec.Path)
		}
	}
}

// --- UpdateStream tests ---

// buildUpdateStreamMocks returns mock responses and path constants for UpdateStream tests.
// The mocks represent a happy-path scenario where all K8s API calls succeed and the
// CatalogSource is already READY.
func buildUpdateStreamMocks(testImage string) (map[string]mockResponse, map[string]string) {
	// Pull secret
	dockerConfig := map[string]any{
		"auths": map[string]any{
			"quay.io/rhoai": map[string]any{"auth": exampleAuth("test:test")},
		},
	}
	dockerConfigJSON, _ := json.Marshal(dockerConfig)
	dockerConfigB64 := base64.StdEncoding.EncodeToString(dockerConfigJSON)
	secretData := map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"data": map[string]any{".dockerconfigjson": dockerConfigB64},
	}
	secretJSON, _ := json.Marshal(secretData)

	// IDMS
	idmsResponse := map[string]any{
		"items": []any{
			map[string]any{
				"metadata": map[string]any{"name": "rhoai-mirror"},
				"spec": map[string]any{
					"imageDigestMirrors": []any{
						map[string]any{"source": rhoai.IDMSSource},
					},
				},
			},
		},
	}
	idmsJSON, _ := json.Marshal(idmsResponse)

	// CatalogSource (READY)
	csResponse := map[string]any{
		"apiVersion": "operators.coreos.com/v1alpha1", "kind": "CatalogSource",
		"metadata": map[string]any{"name": rhoai.CatalogName, "namespace": rhoai.CatalogNS},
		"spec":     map[string]any{"image": testImage},
		"status": map[string]any{
			"connectionState": map[string]any{"lastObservedState": "READY"},
		},
	}
	csJSON, _ := json.Marshal(csResponse)

	// Subscription (with installPlanRef in status)
	subResponse := map[string]any{
		"apiVersion": "operators.coreos.com/v1alpha1", "kind": "Subscription",
		"metadata": map[string]any{"name": rhoai.SubName, "namespace": rhoai.SubNS, "uid": "uid-sub"},
		"spec":     map[string]any{"source": rhoai.CatalogName, "channel": "fast"},
		"status": map[string]any{
			"state":        "AtLatestKnown",
			"currentCSV":   "rhods-operator.v3.5.0",
			"installedCSV": "rhods-operator.v3.5.0",
			"installPlanRef": map[string]any{
				"name": "install-plan-abc123",
			},
		},
	}
	subJSON, _ := json.Marshal(subResponse)

	// CSV list
	csvResponse := map[string]any{
		"items": []any{
			map[string]any{
				"metadata": map[string]any{"name": "rhods-operator.v3.5.0"},
				"spec":     map[string]any{"displayName": "Red Hat OpenShift AI", "version": "3.5.0"},
				"status":   map[string]any{"phase": "Succeeded"},
			},
		},
	}
	csvJSON, _ := json.Marshal(csvResponse)

	// Package manifest
	pkgManifest := map[string]any{
		"status": map[string]any{
			"channels": []any{
				map[string]any{"name": "fast", "currentCSV": "rhods-operator.v3.5.0"},
			},
		},
	}
	pkgManifestJSON, _ := json.Marshal(pkgManifest)

	// Dashboard deployment (non-PR)
	dashDeploy := map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{"opendatahub.io/managed": "true"}},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": "rhods-dashboard"}},
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{
						map[string]any{"name": "rhods-dashboard", "image": "registry.redhat.io/rhoai/odh-dashboard-rhel9:v3.5"},
					},
				},
			},
		},
	}
	dashDeployJSON, _ := json.Marshal(dashDeploy)

	emptyPodList, _ := json.Marshal(map[string]any{"items": []any{}})
	emptyDeployList, _ := json.Marshal(map[string]any{"items": []any{}})

	// OperatorGroup response
	ogResponse := map[string]any{
		"items": []any{
			map[string]any{
				"metadata": map[string]any{"name": "redhat-ods-operator-og", "namespace": rhoai.SubNS},
			},
		},
	}
	ogJSON, _ := json.Marshal(ogResponse)

	// Paths
	pullSecretPath := "/api/v1/namespaces/kube-system/secrets/additional-pull-secret"
	idmsPath := "/apis/config.openshift.io/v1/imagedigestmirrorsets"
	csPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/catalogsources/%s", rhoai.CatalogNS, rhoai.CatalogName)
	subPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/subscriptions/%s", rhoai.SubNS, rhoai.SubName)
	csvListPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/clusterserviceversions", rhoai.SubNS)
	csvDeletePath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/clusterserviceversions/%s", rhoai.SubNS, "rhods-operator.v3.5.0")
	dashDeployPath := "/apis/apps/v1/namespaces/redhat-ods-applications/deployments/rhods-dashboard"
	dashPodsPath := "/api/v1/namespaces/redhat-ods-applications/pods"
	pkgManifestPath := fmt.Sprintf("/apis/packages.operators.coreos.com/v1/namespaces/%s/packagemanifests/%s", rhoai.CatalogNS, "rhods-operator")
	appDeployListPath := "/apis/apps/v1/namespaces/redhat-ods-applications/deployments"
	opDeployListPath := "/apis/apps/v1/namespaces/redhat-ods-operator/deployments"
	ogPath := fmt.Sprintf("/apis/operators.coreos.com/v1/namespaces/%s/operatorgroups", rhoai.SubNS)

	paths := map[string]string{
		"pullSecret":    pullSecretPath,
		"idms":          idmsPath,
		"cs":            csPath,
		"sub":           subPath,
		"csvList":       csvListPath,
		"csvDelete":     csvDeletePath,
		"dashDeploy":    dashDeployPath,
		"dashPods":      dashPodsPath,
		"pkgManifest":   pkgManifestPath,
		"appDeployList": appDeployListPath,
		"opDeployList":  opDeployListPath,
		"og":            ogPath,
	}

	responses := map[string]mockResponse{
		pullSecretPath:         {body: string(secretJSON)},
		idmsPath:               {body: string(idmsJSON)},
		csPath:                 {body: string(csJSON)},
		subPath:                {body: string(subJSON)},
		csvListPath:            {body: string(csvJSON)},
		csvDeletePath:          {body: `{"kind":"Status","status":"Success"}`},
		"GET " + csvDeletePath: {body: succeededCSV},
		dashDeployPath:         {body: string(dashDeployJSON)},
		dashPodsPath:           {body: string(emptyPodList)},
		pkgManifestPath:        {body: string(pkgManifestJSON)},
		strings.TrimSuffix(pkgManifestPath, "/"+rhoai.SubName): {body: wrapPkgManifestList(string(pkgManifestJSON))},
		appDeployListPath: {body: string(emptyDeployList)},
		opDeployListPath:  {body: string(emptyDeployList)},
		ogPath:            {body: string(ogJSON)},
	}

	return responses, paths
}

func TestUpdateStream(t *testing.T) {
	testImage := "quay.io/rhoai/rhoai-fbc-fragment:nightly-test"
	responses, _ := buildUpdateStreamMocks(testImage)

	client, _, cleanup := newRecordingMockClient(responses)
	defer cleanup()

	// Collect emitted step events
	var mu sync.Mutex
	var events []UpdateStepEvent
	emit := func(event UpdateStepEvent) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}

	result, err := operator.UpdateStream(client, testImage, emit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got failure: %s\nlogs: %v", result.Message, result.Logs)
	}

	// Verify the expected step sequence
	expectedSteps := []string{
		"validate_prerequisites",
		"save_snapshot",
		"apply_catalog_source",
		"wait_catalog_ready",
		"detect_channel",
		"delete_csv",
		"apply_subscription",
		"verify_installplan",
	}

	// Collect unique step names in order of first appearance
	mu.Lock()
	eventsCopy := make([]UpdateStepEvent, len(events))
	copy(eventsCopy, events)
	mu.Unlock()

	var stepSequence []string
	seen := map[string]bool{}
	for _, ev := range eventsCopy {
		if !seen[ev.Step] {
			seen[ev.Step] = true
			stepSequence = append(stepSequence, ev.Step)
		}
	}

	if len(stepSequence) != len(expectedSteps) {
		t.Fatalf("expected %d unique steps, got %d: %v", len(expectedSteps), len(stepSequence), stepSequence)
	}
	for i, want := range expectedSteps {
		if stepSequence[i] != want {
			t.Errorf("step %d: expected %q, got %q (full sequence: %v)", i, want, stepSequence[i], stepSequence)
		}
	}

	// Verify each step has at least one "success" (or "skipped") status event
	for _, step := range expectedSteps {
		found := false
		for _, ev := range eventsCopy {
			if ev.Step == step && (ev.Status == "success" || ev.Status == "skipped") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("step %q never reached 'success' or 'skipped' status", step)
		}
	}

	// Verify the function returns a successful OperationResponse with logs
	if result.Message == "" {
		t.Error("expected non-empty message")
	}
	if len(result.Logs) == 0 {
		t.Error("expected non-empty logs")
	}
}

func TestUpdateStream_CatalogSourceTimeout(t *testing.T) {
	testImage := "quay.io/rhoai/rhoai-fbc-fragment:nightly-timeout"

	// Build pull secret
	dockerConfig := map[string]any{
		"auths": map[string]any{
			"quay.io/rhoai": map[string]any{"auth": exampleAuth("test:test")},
		},
	}
	dockerConfigJSON, _ := json.Marshal(dockerConfig)
	dockerConfigB64 := base64.StdEncoding.EncodeToString(dockerConfigJSON)
	secretData := map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"data": map[string]any{".dockerconfigjson": dockerConfigB64},
	}
	secretJSON, _ := json.Marshal(secretData)

	// IDMS
	idmsResponse := map[string]any{
		"items": []any{
			map[string]any{
				"metadata": map[string]any{"name": "rhoai-mirror"},
				"spec": map[string]any{
					"imageDigestMirrors": []any{
						map[string]any{"source": rhoai.IDMSSource},
					},
				},
			},
		},
	}
	idmsJSON, _ := json.Marshal(idmsResponse)

	// CatalogSource that always returns TRANSIENT_FAILURE
	csResponse := map[string]any{
		"apiVersion": "operators.coreos.com/v1alpha1", "kind": "CatalogSource",
		"metadata": map[string]any{"name": rhoai.CatalogName, "namespace": rhoai.CatalogNS},
		"spec":     map[string]any{"image": testImage},
		"status": map[string]any{
			"connectionState": map[string]any{"lastObservedState": "TRANSIENT_FAILURE"},
		},
	}
	csJSON, _ := json.Marshal(csResponse)

	// Dashboard deployment (non-PR)
	dashDeploy := map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{"opendatahub.io/managed": "true"}},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": "rhods-dashboard"}},
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{
						map[string]any{"name": "rhods-dashboard", "image": "registry.redhat.io/rhoai/odh-dashboard-rhel9:v3.5"},
					},
				},
			},
		},
	}
	dashDeployJSON, _ := json.Marshal(dashDeploy)
	emptyPodList, _ := json.Marshal(map[string]any{"items": []any{}})
	emptyDeployList, _ := json.Marshal(map[string]any{"items": []any{}})

	// OperatorGroup response
	ogResponse := map[string]any{
		"items": []any{
			map[string]any{
				"metadata": map[string]any{"name": "redhat-ods-operator-og", "namespace": rhoai.SubNS},
			},
		},
	}
	ogJSON, _ := json.Marshal(ogResponse)

	pullSecretPath := "/api/v1/namespaces/kube-system/secrets/additional-pull-secret"
	idmsPath := "/apis/config.openshift.io/v1/imagedigestmirrorsets"
	csPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/catalogsources/%s", rhoai.CatalogNS, rhoai.CatalogName)
	dashDeployPath := "/apis/apps/v1/namespaces/redhat-ods-applications/deployments/rhods-dashboard"
	dashPodsPath := "/api/v1/namespaces/redhat-ods-applications/pods"
	appDeployListPath := "/apis/apps/v1/namespaces/redhat-ods-applications/deployments"
	opDeployListPath := "/apis/apps/v1/namespaces/redhat-ods-operator/deployments"
	ogPath := fmt.Sprintf("/apis/operators.coreos.com/v1/namespaces/%s/operatorgroups", rhoai.SubNS)

	// Use a custom server so we have control over the context
	var mu sync.Mutex
	var recs []requestRecord
	responses := map[string]mockResponse{
		pullSecretPath:    {body: string(secretJSON)},
		idmsPath:          {body: string(idmsJSON)},
		csPath:            {body: string(csJSON)},
		dashDeployPath:    {body: string(dashDeployJSON)},
		dashPodsPath:      {body: string(emptyPodList)},
		appDeployListPath: {body: string(emptyDeployList)},
		opDeployListPath:  {body: string(emptyDeployList)},
		ogPath:            {body: string(ogJSON)},
	}

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dashboardOperatorAbsent(w, r) {
			return
		}
		mu.Lock()
		recs = append(recs, requestRecord{Method: r.Method, Path: r.URL.Path})
		mu.Unlock()

		if strings.Contains(r.URL.Path, "/catalogsources/"+rhoai.CatalogName+"-verify-") {
			fmt.Fprint(w, `{"status":{"connectionState":{"lastObservedState":"READY"}}}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/packagemanifests") {
			source := strings.TrimPrefix(r.URL.Query().Get("labelSelector"), "catalog=")
			fmt.Fprint(w, strings.ReplaceAll(wrapPkgManifestList(`{"status":{"channels":[{"name":"fast","currentCSV":"rhods-operator.v3.5.0"}]}}`), `"catalogSource":"`+rhoai.CatalogName+`"`, `"catalogSource":"`+source+`"`))
			return
		}
		key := r.Method + " " + r.URL.Path
		resp, ok := responses[key]
		if !ok {
			resp, ok = responses[r.URL.Path]
		}
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"apiVersion":"v1","kind":"ConfigMap","data":{}}`))
			return
		}
		if resp.statusCode == 0 {
			resp.statusCode = 200
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.statusCode)
		w.Write([]byte(resp.body))
	}))
	defer server.Close()

	// The operation's deadline comes before the catalog's own timeout.
	ctx, cancel := kube.Clock().WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	client := newTestClient(testClientOptions{
		BaseURL:    server.URL,
		Token:      "test-token",
		HTTPClient: server.Client(),
		Ctx:        ctx,
	})

	var events []UpdateStepEvent
	emit := func(event UpdateStepEvent) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}

	result, _ := operator.UpdateStream(client, testImage, emit)

	// The operation should fail (either context cancelled or timeout)
	if result.Success {
		t.Fatal("expected failure when CatalogSource stays in TRANSIENT_FAILURE")
	}

	// Verify step 4 (wait_catalog_ready) emitted at least one "running" event
	mu.Lock()
	eventsCopy := make([]UpdateStepEvent, len(events))
	copy(eventsCopy, events)
	mu.Unlock()

	runningCount := 0
	var failedFound bool
	for _, ev := range eventsCopy {
		if ev.Step == "wait_catalog_ready" {
			if ev.Status == "running" {
				runningCount++
			}
			if ev.Status == "failed" {
				failedFound = true
			}
		}
	}

	if runningCount == 0 {
		t.Error("expected at least one 'running' event for wait_catalog_ready step")
	}
	if !failedFound {
		t.Error("expected a 'failed' event for wait_catalog_ready step")
	}
}

func TestUpdateStream_BackwardCompat(t *testing.T) {
	// This test verifies that the old Update() function still works correctly
	// alongside the new UpdateStream() function.
	testImage := "quay.io/rhoai/rhoai-fbc-fragment:nightly-compat"
	responses, _ := buildUpdateStreamMocks(testImage)

	// Keep installPlanRef so verify_installplan step completes quickly
	subResponse := map[string]any{
		"apiVersion": "operators.coreos.com/v1alpha1", "kind": "Subscription",
		"metadata": map[string]any{"name": rhoai.SubName, "namespace": rhoai.SubNS, "uid": "uid-sub"},
		"spec":     map[string]any{"source": rhoai.CatalogName, "channel": "fast"},
		"status": map[string]any{
			"state":          "AtLatestKnown",
			"currentCSV":     "rhods-operator.v3.5.0",
			"installedCSV":   "rhods-operator.v3.5.0",
			"installPlanRef": map[string]any{"name": "install-plan-compat"},
		},
	}
	subJSON, _ := json.Marshal(subResponse)
	subPath := fmt.Sprintf("/apis/operators.coreos.com/v1alpha1/namespaces/%s/subscriptions/%s", rhoai.SubNS, rhoai.SubName)
	responses[subPath] = mockResponse{body: string(subJSON)}

	client, _, cleanup := newRecordingMockClient(responses)
	defer cleanup()

	result, err := Update(client, testImage, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success from old Update(), got failure: %s\nlogs: %v", result.Message, result.Logs)
	}

	// Verify it returns a proper OperationResponse with logs
	if result.Message == "" {
		t.Error("expected non-empty message from Update()")
	}
	if len(result.Logs) == 0 {
		t.Error("expected non-empty logs from Update()")
	}

	// Verify key log entries exist
	logText := strings.Join(result.Logs, "\n")
	for _, want := range []string{"CatalogSource applied", "Subscription applied"} {
		if !strings.Contains(logText, want) {
			t.Errorf("Update() logs should contain %q, got:\n%s", want, logText)
		}
	}
}
