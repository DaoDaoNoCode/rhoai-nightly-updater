package cluster

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The admin UI password is the S3 secret key, read from the tool's Secret
// only when asked for.
func TestS3AdminLogin(t *testing.T) {
	t.Run("storage the tool set up", func(t *testing.T) {
		f, c := newResourceFake(t)
		putReadyManagedS3(f)
		user, password, err := S3AdminLogin(c)
		if err != nil || user != "admin" || password != "pass" {
			t.Fatalf("S3AdminLogin = %q, %q, %v", user, password, err)
		}
		if n := len(f.mutations()); n != 0 {
			t.Errorf("reading the password changed %d objects: %v", n, f.mutations())
		}
	})

	unavailable := func(t *testing.T, c *Client, want string) {
		t.Helper()
		_, password, err := S3AdminLogin(c)
		var u *S3AdminLoginUnavailable
		if !errors.As(err, &u) || !strings.Contains(u.Reason, want) || password != "" {
			t.Fatalf("S3AdminLogin = %q, %v; want unavailable with %q", password, err, want)
		}
	}
	t.Run("not set up", func(t *testing.T) {
		f, c := newResourceFake(t)
		unavailable(t, c, "not set up")
		for _, r := range f.requests {
			if strings.Contains(r, "/secrets/") {
				t.Errorf("read a Secret without the storage: %s", r)
			}
		}
	})
	t.Run("namespace the tool did not create", func(t *testing.T) {
		f, c := newResourceFake(t)
		putReadyManagedS3(f)
		putS3Namespace(f, `{}`)
		unavailable(t, c, "not created by this tool")
		for _, r := range f.requests {
			if strings.Contains(r, "/secrets/") {
				t.Errorf("read a Secret in a foreign namespace: %s", r)
			}
		}
	})
	t.Run("Secret the tool did not write", func(t *testing.T) {
		f, c := newResourceFake(t)
		putReadyManagedS3(f)
		f.putJSON(s3SecretPath, `{"metadata":{},"data":{"AWS_SECRET_ACCESS_KEY":"`+exampleAuth("foreign")+`"}}`)
		unavailable(t, c, "was not created by this tool")
	})
	t.Run("Secret missing", func(t *testing.T) {
		f, c := newResourceFake(t)
		putS3Namespace(f, toolLabelJSON)
		unavailable(t, c, "Repair re-creates it")
	})
	t.Run("Secret without the key", func(t *testing.T) {
		f, c := newResourceFake(t)
		putS3Namespace(f, toolLabelJSON)
		f.putJSON(s3SecretPath, `{"metadata":{"labels":`+toolLabelJSON+`},"data":{}}`)
		unavailable(t, c, "has no AWS_SECRET_ACCESS_KEY")
	})
	t.Run("read error", func(t *testing.T) {
		f, c := newResourceFake(t)
		putReadyManagedS3(f)
		f.fail["GET "+s3SecretPath] = 403
		_, _, err := S3AdminLogin(c)
		var u *S3AdminLoginUnavailable
		if err == nil || errors.As(err, &u) || !IsK8sError(err, 403) {
			t.Fatalf("err = %v; want the API error", err)
		}
	})
}

// The status never carries the password: it is read only by S3AdminLogin.
func TestResourcesStatusDoesNotCarryTheAdminPassword(t *testing.T) {
	f, c := newResourceFake(t)
	putReadyManagedS3(f)
	st, err := GetResourcesStatus(c)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), `"pass"`) || strings.Contains(string(b), exampleAuth("pass")) {
		t.Fatalf("status carries the password: %s", b)
	}
	for _, r := range f.requests {
		if strings.Contains(r, "/secrets/"+s3Secret) {
			t.Errorf("status read the storage's Secret: %s", r)
		}
	}
}
