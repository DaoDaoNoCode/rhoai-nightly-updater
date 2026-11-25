package storage

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// An IAM read is a POST (the IAM query API) but changes nothing: a failed
// ListUsers, ListAccessKeys or ListUserTags leaves the operation's changes
// empty, so a refusal after it gives the rate-limit slot back. An IAM write
// counts whatever it is answered, and so does an S3 write.
func TestIAMReadsAreNotChanges(t *testing.T) {
	var actions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		actions = append(actions, r.Method+" "+form.Get("Action"))
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	keys := S3Keys{AccessKey: "a", SecretKey: "s"}
	for _, tc := range []struct {
		name string
		call func(s *seaweedClient) error
		made bool
	}{
		{"ListUsers", func(s *seaweedClient) error { _, err := s.ListUsers(); return err }, false},
		{"ListAccessKeys", func(s *seaweedClient) error { _, _, err := s.ListAccessKeys("u"); return err }, false},
		{"ListUserTags", func(s *seaweedClient) error { _, _, err := s.ListUserTags("u"); return err }, false},
		{"CreateUser", func(s *seaweedClient) error { return s.CreateUser("u") }, true},
		{"DeleteAccessKey", func(s *seaweedClient) error { return s.DeleteAccessKey("u", "id") }, true},
		{"S3 PUT", func(s *seaweedClient) error {
			_, _, err := s.object(http.MethodPut, "b", "k", nil, []byte("x"))
			return err
		}, true},
		{"S3 GET", func(s *seaweedClient) error { _, _, err := s.object(http.MethodGet, "b", "k", nil, nil); return err }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, changes := kube.TrackChanges(t.Context())
			s := newSeaweedClient(ctx, srv.URL, keys)
			if err := tc.call(s); err == nil && tc.name != "S3 PUT" && tc.name != "S3 GET" {
				t.Fatal("the 500 was not an error")
			}
			if changes.Made() != tc.made {
				t.Fatalf("Made() = %v, want %v (requests %v)", changes.Made(), tc.made, actions)
			}
		})
	}
}
