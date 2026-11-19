package cluster

import (
	"fmt"
	"strings"
	"testing"
)

const (
	nodeReadinessPullSecretPath = "/api/v1/namespaces/kube-system/secrets/additional-pull-secret"
	nodeReadinessIDMSPath       = "/apis/config.openshift.io/v1/imagedigestmirrorsets"
)

func nodeReadinessStatusBody(code int) string {
	return fmt.Sprintf(`{"kind":"Status","status":"Failure","message":"x","code":%d}`, code)
}

// A pull secret that cannot be read is not reported as missing.
func TestVerifyNodeReadiness_PullSecretReadFails(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		wantCode string
	}{
		{"forbidden", 403, "forbidden"},
		{"server error", 500, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, cleanup := newMockClient(map[string]mockResponse{
				nodeReadinessPullSecretPath: {body: nodeReadinessStatusBody(tc.status), statusCode: tc.status},
			})
			defer cleanup()

			result, err := VerifyNodeReadiness(client)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Success {
				t.Fatal("expected failure")
			}
			if strings.Contains(result.Message, "missing") {
				t.Errorf("a read failure must not say the pull secret is missing: %q", result.Message)
			}
			if !strings.Contains(result.Message, "Could not read the pull secret") {
				t.Errorf("message = %q, want the read failure", result.Message)
			}
			if result.ErrorCode != tc.wantCode {
				t.Errorf("ErrorCode = %q, want %q", result.ErrorCode, tc.wantCode)
			}
		})
	}
}

func TestVerifyNodeReadiness_PullSecretMissing(t *testing.T) {
	client, cleanup := newMockClient(map[string]mockResponse{
		nodeReadinessPullSecretPath: {body: nodeReadinessStatusBody(404), statusCode: 404},
	})
	defer cleanup()

	result, err := VerifyNodeReadiness(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success || result.ErrorCode != "prerequisites" || !strings.Contains(result.Message, "missing") {
		t.Errorf("got success=%v code=%q message=%q, want the missing pull secret", result.Success, result.ErrorCode, result.Message)
	}
}

// An IDMS list that fails is not reported as an unconfigured mirror.
func TestVerifyNodeReadiness_IDMSReadFails(t *testing.T) {
	orig := verifyQuayCredentials
	verifyQuayCredentials = func(string) error { return nil }
	defer func() { verifyQuayCredentials = orig }()

	cases := []struct {
		name     string
		status   int
		wantCode string
	}{
		{"forbidden", 403, "forbidden"},
		{"server error", 500, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, cleanup := newMockClient(map[string]mockResponse{
				nodeReadinessPullSecretPath: {body: pullSecretBody(t, map[string]interface{}{
					"quay.io/rhoai": map[string]interface{}{"auth": exampleAuth("test:test")},
				})},
				nodeReadinessIDMSPath: {body: nodeReadinessStatusBody(tc.status), statusCode: tc.status},
			})
			defer cleanup()

			result, err := VerifyNodeReadiness(client)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Success {
				t.Fatal("expected failure")
			}
			if strings.Contains(result.Message, "not configured") {
				t.Errorf("a read failure must not say the IDMS is not configured: %q", result.Message)
			}
			if !strings.Contains(result.Message, "Could not read the image mirror") {
				t.Errorf("message = %q, want the read failure", result.Message)
			}
			if result.ErrorCode != tc.wantCode {
				t.Errorf("ErrorCode = %q, want %q", result.ErrorCode, tc.wantCode)
			}
		})
	}
}

func TestVerifyNodeReadiness_IDMSMissing(t *testing.T) {
	orig := verifyQuayCredentials
	verifyQuayCredentials = func(string) error { return nil }
	defer func() { verifyQuayCredentials = orig }()

	client, cleanup := newMockClient(map[string]mockResponse{
		nodeReadinessPullSecretPath: {body: pullSecretBody(t, map[string]interface{}{
			"quay.io/rhoai": map[string]interface{}{"auth": exampleAuth("test:test")},
		})},
		nodeReadinessIDMSPath: {body: `{"items":[]}`},
	})
	defer cleanup()

	result, err := VerifyNodeReadiness(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success || result.ErrorCode != "prerequisites" || !strings.Contains(result.Message, "not configured") {
		t.Errorf("got success=%v code=%q message=%q, want the unconfigured IDMS", result.Success, result.ErrorCode, result.Message)
	}
}
