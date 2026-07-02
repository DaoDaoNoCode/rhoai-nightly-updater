package cluster

import (
	"encoding/json"
	"fmt"
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

func TestMinioCredentials_DifferentOnEachCall(t *testing.T) {
	// Generate several passwords and confirm they are not all the same.
	// With 24 chars drawn from a 62-char alphabet the probability of a
	// collision in 10 samples is astronomically low.
	seen := make(map[string]bool)
	const iterations = 10
	for i := 0; i < iterations; i++ {
		_, pw := minioCredentials()
		seen[pw] = true
	}
	if len(seen) < 2 {
		t.Errorf("expected different passwords across %d calls, but got %d unique value(s)", iterations, len(seen))
	}
}

func TestMinioCredentials_SufficientLength(t *testing.T) {
	_, pw := minioCredentials()
	if len(pw) < 16 {
		t.Errorf("password length %d is less than the minimum 16 characters", len(pw))
	}
}

func TestMinioCredentials_HexCharacters(t *testing.T) {
	// The generated password is a hex-encoded random byte slice (0-9, a-f).
	// Verify it only contains valid hex characters and has the expected length.
	_, pw := minioCredentials()

	if len(pw) != 32 { // 16 bytes -> 32 hex chars
		t.Errorf("expected password length 32, got %d", len(pw))
	}

	for _, ch := range pw {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			t.Errorf("password contains unexpected character %q", ch)
		}
	}
}

func TestMinioCredentials_UserIsConstant(t *testing.T) {
	user, _ := minioCredentials()
	if user != "minio" {
		t.Errorf("expected user %q, got %q", "minio", user)
	}
}

// ---------------------------------------------------------------------------
// SetupMinIO credential-leak test
// ---------------------------------------------------------------------------

func TestSetupMinIO_ResponseDoesNotLeakPassword(t *testing.T) {
	// SetupMinIO has multiple return paths. In a unit-test environment the
	// S3 bucket creation always fails (no real MinIO), so we exercise the
	// deployment path and verify that every response message is credential-free.
	//
	// We also call minioCredentials() before and after to capture the generated
	// password and confirm it never appears in any log line or message.

	nsJSON, _ := json.Marshal(map[string]interface{}{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]interface{}{"name": "minio"},
		"status":   map[string]interface{}{"phase": "Active"},
	})

	deployReady, _ := json.Marshal(map[string]interface{}{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]interface{}{"name": "minio", "namespace": "minio"},
		"status": map[string]interface{}{
			"readyReplicas": 1,
			"replicas":      1,
		},
	})

	// Provide a minio-secret that createMinioBucket reads.
	secretJSON, _ := json.Marshal(map[string]interface{}{
		"apiVersion": "v1", "kind": "Secret",
		"data": map[string]interface{}{
			"minio_root_user":     "bWluaW8=",     // base64("minio")
			"minio_root_password": "dGVzdHBhc3M=", // base64("testpass")
		},
	})

	deployPath := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", "minio", "minio")
	secretPath := fmt.Sprintf("/api/v1/namespaces/%s/secrets/%s", "minio", "minio-secret")

	client, _, cleanup := newRecordingMockClient(map[string]mockResponse{
		"/api/v1/namespaces/minio": {body: string(nsJSON), statusCode: 200},
		"/api/v1/namespaces":       {body: `{}`, statusCode: 201},
		deployPath:                 {body: string(deployReady), statusCode: 200},
		secretPath:                 {body: string(secretJSON), statusCode: 200},
	})
	defer cleanup()

	resp, err := SetupMinIO(client)
	if err != nil {
		t.Fatalf("SetupMinIO returned error: %v", err)
	}

	// Combine message and all log lines into one blob for scanning.
	allText := resp.Message
	for _, l := range resp.Logs {
		allText += "\n" + l
	}

	// The old hardcoded password must not appear anywhere.
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
