package cluster

import (
	"encoding/base64"
	"testing"
)

// CRI-O (containers/image) decodes "auth" with base64.StdEncoding only, so an
// unpadded value must be stored in its padded form or node pulls fail.
func TestCreatePullSecret_StoresPaddedAuth(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"padded input kept", "dXNlcjpwYXM=", "dXNlcjpwYXM="},
		{"unpadded input padded", "dXNlcjpwYXM", "dXNlcjpwYXM="},
		{"no padding needed", "dXNlcjpwYXNz", "dXNlcjpwYXNz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, writes := pullSecretServer(t, "")
			result, err := CreatePullSecret(client, tc.input)
			if err != nil || !result.Success {
				t.Fatalf("CreatePullSecret = %+v, %v", result, err)
			}
			w := writes()
			if len(w) != 1 {
				t.Fatalf("expected one write, got %d", len(w))
			}
			got := authFromEntry(writtenAuths(t, w[0])["quay.io/rhoai"])
			if got != tc.want {
				t.Fatalf("stored auth = %q, want %q", got, tc.want)
			}
			if _, err := base64.StdEncoding.DecodeString(got); err != nil {
				t.Fatalf("stored auth is not StdEncoding-decodable: %v", err)
			}
		})
	}
}
