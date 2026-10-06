package cluster

import (
	"encoding/base64"
	"strings"
	"testing"
)

// CRI-O (containers/image) decodes "auth" with base64.StdEncoding only, so an
// unpadded value must be stored in its padded form or node pulls fail.
func TestCreatePullSecret_StoresPaddedAuth(t *testing.T) {
	padded := exampleAuth("user:pas") // ends in one "="
	unpadded := strings.TrimRight(padded, "=")
	exact := exampleAuth("user:pass") // a multiple of 3 bytes: no padding
	for _, tc := range []struct{ name, input, want string }{
		{"padded input kept", padded, padded},
		{"unpadded input padded", unpadded, padded},
		{"no padding needed", exact, exact},
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
