package cluster

import "testing"

func TestS3BucketEndpoint(t *testing.T) {
	inCluster := s3Endpoint()
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"production ignores the override", map[string]string{"DEV_S3_ENDPOINT": "http://127.0.0.1:18333"}, inCluster},
		{"dev mode without override", map[string]string{"DEV_MODE": "true"}, inCluster},
		{"dev mode loopback IPv4", map[string]string{"DEV_MODE": "true", "DEV_S3_ENDPOINT": "http://127.0.0.1:18333"}, "http://127.0.0.1:18333"},
		{"dev mode trailing slash", map[string]string{"DEV_MODE": "true", "DEV_S3_ENDPOINT": "http://127.0.0.1:18333/"}, "http://127.0.0.1:18333"},
		{"dev mode localhost", map[string]string{"DEV_MODE": "true", "DEV_S3_ENDPOINT": "http://localhost:18333"}, "http://localhost:18333"},
		{"dev mode loopback IPv6", map[string]string{"DEV_MODE": "true", "DEV_S3_ENDPOINT": "http://[::1]:18333"}, "http://[::1]:18333"},
		{"non-loopback host refused", map[string]string{"DEV_MODE": "true", "DEV_S3_ENDPOINT": "http://10.0.0.5:8333"}, inCluster},
		{"https refused", map[string]string{"DEV_MODE": "true", "DEV_S3_ENDPOINT": "https://127.0.0.1:18333"}, inCluster},
		{"missing port refused", map[string]string{"DEV_MODE": "true", "DEV_S3_ENDPOINT": "http://127.0.0.1"}, inCluster},
		{"path refused", map[string]string{"DEV_MODE": "true", "DEV_S3_ENDPOINT": "http://127.0.0.1:18333/pipelines"}, inCluster},
		{"credentials refused", map[string]string{"DEV_MODE": "true", "DEV_S3_ENDPOINT": "http://u:p@127.0.0.1:18333"}, inCluster},
		{"lookalike host refused", map[string]string{"DEV_MODE": "true", "DEV_S3_ENDPOINT": "http://127.0.0.1.example.com:18333"}, inCluster},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := s3BucketEndpoint(func(k string) string { return tc.env[k] })
			if got != tc.want {
				t.Errorf("s3BucketEndpoint() = %q, want %q", got, tc.want)
			}
		})
	}
}
