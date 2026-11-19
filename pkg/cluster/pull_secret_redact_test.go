package cluster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An API error that quotes the object it was sent (an admission webhook or
// a validation error can) never reaches the operation's message or logs:
// the new credential (as given, its base64, decoded, the password), the
// whole config (JSON and base64) and the other registries' credentials are
// redacted, for the create and the update path.
func TestCreatePullSecret_RedactsTheCredentialFromWriteErrors(t *testing.T) {
	const auth = "bmV3dXNlcjpzM2NyM3QtdG9rZW4=" // newuser:s3cr3t-token
	other := base64.StdEncoding.EncodeToString([]byte("someone:other-registry-secret"))
	existing := pullSecretBody(t, map[string]any{"registry.example.com": map[string]any{"auth": other}})
	for _, tc := range []struct {
		name     string
		existing string
	}{{"create", ""}, {"update", existing}} {
		t.Run(tc.name, func(t *testing.T) {
			var sent string
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					if tc.existing == "" {
						w.WriteHeader(http.StatusNotFound)
						_, _ = io.WriteString(w, `{"kind":"Status","code":404,"reason":"NotFound","message":"not found"}`)
						return
					}
					_, _ = io.WriteString(w, tc.existing)
					return
				}
				body, _ := io.ReadAll(r.Body)
				sent = string(body)
				// The body, its data value and the decoded config, echoed.
				var obj struct {
					Data map[string]string `json:"data"`
				}
				_ = json.Unmarshal(body, &obj)
				cfg, _ := base64.StdEncoding.DecodeString(obj.Data[".dockerconfigjson"])
				msg, _ := json.Marshal(map[string]any{"kind": "Status", "status": "Failure", "code": 422, "reason": "Invalid",
					"message": "admission webhook denied " + string(body) + " config " + string(cfg) + " auth " + auth + " " + base64.StdEncoding.EncodeToString([]byte(auth)) + " newuser:s3cr3t-token"})
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write(msg)
			}))
			defer srv.Close()
			c := &Client{baseURL: srv.URL, token: "test-token", httpClient: srv.Client(), ctx: context.Background()}
			res, err := CreatePullSecret(c, auth)
			if err != nil || res.Success {
				t.Fatalf("got %+v, %v", res, err)
			}
			if sent == "" {
				t.Fatal("nothing was written")
			}
			out := res.Message + "\n" + strings.Join(res.Logs, "\n")
			for _, secret := range []string{auth, base64.StdEncoding.EncodeToString([]byte(auth)), "s3cr3t-token", other, "other-registry-secret"} {
				if strings.Contains(out, secret) {
					t.Errorf("the message shows %q:\n%s", secret, out)
				}
			}
			if !strings.Contains(res.Message, "admission webhook denied") || !strings.Contains(res.Message, redactedText) {
				t.Errorf("the error is not kept with the values redacted: %s", res.Message)
			}
		})
	}
}

func TestRedactSecrets(t *testing.T) {
	got := redactSecrets(`a "xx\"yy" abcdefgh abcdefghij ab`, `xx"yy`, "abcdefgh", "abcdefghij", "ab")
	if want := `a "[redacted]" [redacted] [redacted] ab`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
