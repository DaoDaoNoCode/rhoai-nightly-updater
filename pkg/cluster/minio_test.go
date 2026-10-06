package cluster

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func TestGetMinIOStatus_NamespaceNotFound(t *testing.T) {
	// No routes registered for minio namespace -> namespace GET returns 404
	client, cleanup := newMockClient(map[string]mockResponse{
		"/api/v1/namespaces/minio": {
			body:       `{"kind":"Status","status":"Failure","message":"not found","reason":"NotFound","code":404}`,
			statusCode: 404,
		},
	})
	defer cleanup()

	state := getMinIOStatus(client)
	if state.Deployed {
		t.Error("expected Deployed to be false when namespace does not exist")
	}
	if state.Message != "Not deployed" {
		t.Errorf("expected message 'Not deployed', got %q", state.Message)
	}
}

func TestGetMinIOStatus_NamespaceTerminating(t *testing.T) {
	nsResponse := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]interface{}{"name": "minio"},
		"status":     map[string]interface{}{"phase": "Terminating"},
	}
	nsJSON, _ := json.Marshal(nsResponse)

	client, cleanup := newMockClient(map[string]mockResponse{
		"/api/v1/namespaces/minio": {
			body:       string(nsJSON),
			statusCode: 200,
		},
	})
	defer cleanup()

	state := getMinIOStatus(client)
	if state.Deployed {
		t.Error("expected Deployed to be false when namespace is terminating")
	}
	if state.Message != "Terminating" {
		t.Errorf("expected message 'Terminating', got %q", state.Message)
	}
}

func TestGetMinIOStatus_DeploymentReady(t *testing.T) {
	nsResponse := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]interface{}{"name": "minio"},
		"status":     map[string]interface{}{"phase": "Active"},
	}
	nsJSON, _ := json.Marshal(nsResponse)

	deployResponse := map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]interface{}{"name": "minio", "namespace": "minio"},
		"status": map[string]interface{}{
			"readyReplicas": 1,
			"replicas":      1,
		},
	}
	deployJSON, _ := json.Marshal(deployResponse)

	deployPath := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", "minio", "minio")

	client, cleanup := newMockClient(map[string]mockResponse{
		"/api/v1/namespaces/minio": {
			body:       string(nsJSON),
			statusCode: 200,
		},
		deployPath: {
			body:       string(deployJSON),
			statusCode: 200,
		},
	})
	defer cleanup()

	state := getMinIOStatus(client)
	if !state.Deployed {
		t.Error("expected Deployed to be true when deployment exists")
	}
	if !state.Ready {
		t.Error("expected Ready to be true when readyReplicas >= 1")
	}
	if state.Message != "Running" {
		t.Errorf("expected message 'Running', got %q", state.Message)
	}
}

// ---------------------------------------------------------------------------
// minioCredentials tests
// ---------------------------------------------------------------------------

// freshMinioClient serves no minio-secret, as on a first-time setup.
func freshMinioClient(t *testing.T) *Client {
	t.Helper()
	t.Setenv("MINIO_ROOT_USER", "")
	t.Setenv("MINIO_ROOT_PASSWORD", "")
	client, cleanup := newMockClient(map[string]mockResponse{})
	t.Cleanup(cleanup)
	return client
}

func mustMinioCredentials(t *testing.T, c *Client) (string, string) {
	t.Helper()
	user, pw, err := minioCredentials(c)
	if err != nil {
		t.Fatalf("minioCredentials: %v", err)
	}
	return user, pw
}

func TestMinioCredentials_DifferentOnEachFirstSetup(t *testing.T) {
	client := freshMinioClient(t)
	seen := make(map[string]bool)
	const iterations = 10
	for i := 0; i < iterations; i++ {
		_, pw := mustMinioCredentials(t, client)
		seen[pw] = true
	}
	if len(seen) < 2 {
		t.Errorf("expected different passwords across %d calls, but got %d unique value(s)", iterations, len(seen))
	}
}

func TestMinioCredentials_HexCharacters(t *testing.T) {
	// The generated password is a hex-encoded random byte slice (0-9, a-f).
	_, pw := mustMinioCredentials(t, freshMinioClient(t))
	if len(pw) != 32 { // 16 bytes -> 32 hex chars
		t.Errorf("expected password length 32, got %d", len(pw))
	}
	for _, ch := range pw {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			t.Errorf("password contains unexpected character %q", ch)
		}
	}
}

// The root user is random per install too (minio-<12 hex>), so the access
// key is not guessable.
func TestMinioCredentials_RandomUser(t *testing.T) {
	first, _ := mustMinioCredentials(t, freshMinioClient(t))
	second, _ := mustMinioCredentials(t, freshMinioClient(t))
	if !regexp.MustCompile(`^minio-[0-9a-f]{12}$`).MatchString(first) || first == second {
		t.Errorf("users %q, %q: want two different random minio-<hex> users", first, second)
	}
}

func TestMinioCredentials_ReusesExistingSecret(t *testing.T) {
	t.Setenv("MINIO_ROOT_USER", "")
	t.Setenv("MINIO_ROOT_PASSWORD", "")
	client, cleanup := newMockClient(map[string]mockResponse{
		"/api/v1/namespaces/minio/secrets/minio-secret": {body: `{"data":{"minio_root_user":"` + exampleAuth("admin") + `","minio_root_password":"` + exampleAuth("keepme") + `"}}`},
	})
	defer cleanup()
	for i := 0; i < 3; i++ {
		if user, pw := mustMinioCredentials(t, client); user != "admin" || pw != "keepme" {
			t.Fatalf("re-running setup must keep existing credentials, got %q/%q", user, pw)
		}
	}
}

func TestMinioCredentials_EnvOverridesSecret(t *testing.T) {
	t.Setenv("MINIO_ROOT_USER", "")
	t.Setenv("MINIO_ROOT_PASSWORD", "fromenv")
	client, cleanup := newMockClient(map[string]mockResponse{
		"/api/v1/namespaces/minio/secrets/minio-secret": {body: `{"data":{"minio_root_user":"` + exampleAuth("admin") + `","minio_root_password":"` + exampleAuth("keepme") + `"}}`},
	})
	defer cleanup()
	if user, pw := mustMinioCredentials(t, client); user != "admin" || pw != "fromenv" {
		t.Fatalf("got %q/%q", user, pw)
	}
}

func TestMinioCredentials_ReadErrorDoesNotRotate(t *testing.T) {
	t.Setenv("MINIO_ROOT_USER", "")
	t.Setenv("MINIO_ROOT_PASSWORD", "")
	client, cleanup := newMockClient(map[string]mockResponse{
		"/api/v1/namespaces/minio/secrets/minio-secret": {statusCode: 500, body: `{"kind":"Status","status":"Failure","message":"boom"}`},
	})
	defer cleanup()
	if _, _, err := minioCredentials(client); err == nil {
		t.Fatal("an unreadable secret must stop setup instead of generating new credentials")
	}
}

// ---------------------------------------------------------------------------
// SetupMinIO credential-leak test
// ---------------------------------------------------------------------------

func TestSetupMinIO_ResponseDoesNotLeakPassword(t *testing.T) {
	// Exercise the success path and the bucket-failure path and verify that
	// no response message or log line reveals the stored password.
	for _, bucketErr := range []error{nil, fmt.Errorf("S3 PUT bucket returned 403")} {
		fastMinIOTimings(t)
		minioBucketCreator = func(*Client, string) error { return bucketErr }
		f, client := newResourceFake(t)
		readyAfterApply(f)
		f.putJSON("/api/v1/namespaces/minio/secrets/minio-secret", `{"metadata":{"labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}},"data":{"minio_root_user":"`+exampleAuth("minio")+`","minio_root_password":"`+exampleAuth("EXAMPLE-pass")+`"}}`)

		resp, err := SetupMinIO(client)
		if err != nil {
			t.Fatalf("SetupMinIO returned error: %v", err)
		}
		if resp.Success != (bucketErr == nil) {
			t.Fatalf("unexpected result %+v", resp)
		}
		assertNoCredentialLeak(t, resp.Message, resp.Logs)
	}
}

func assertNoCredentialLeak(t *testing.T, message string, logs []string) {
	t.Helper()
	// Combine message and all log lines into one blob for scanning.
	allText := message
	for _, l := range logs {
		allText += "\n" + l
	}

	// Neither the stored password nor the old hardcoded one may appear.
	if strings.Contains(allText, "EXAMPLE-pass") {
		t.Errorf("response contains the stored password: %s", allText)
	}
	if strings.Contains(allText, "minio123") {
		t.Errorf("response contains hardcoded password 'minio123': %s", allText)
	}

	// No "credentials: user / pass" pattern should appear.
	lower := strings.ToLower(allText)
	for _, banned := range []string{"credentials:", "/ minio"} {
		if strings.Contains(lower, banned) {
			t.Errorf("response appears to leak credentials (contains %q): %s", banned, allText)
		}
	}
}
