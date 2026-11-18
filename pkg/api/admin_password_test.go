package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
)

const adminPasswordPath = "/api/resources/minio/admin-password"

// stubAdminLogin replaces the Secret read and counts the calls.
func stubAdminLogin(t *testing.T, user, password string, err error) *int {
	t.Helper()
	orig := readS3AdminLogin
	t.Cleanup(func() { readS3AdminLogin = orig })
	calls := 0
	readS3AdminLogin = func(*cluster.Client) (cluster.S3Login, error) {
		calls++
		if err != nil {
			return cluster.S3Login{}, err
		}
		return cluster.S3Login{User: user, Password: password, AccessKey: "access-" + password, SecretKey: "secret-" + password}, nil
	}
	return &calls
}

// captureLogs sends slog output to a buffer for the rest of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return &buf
}

func getAdminPassword(token string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	Register(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, postJSONMethod(http.MethodGet, adminPasswordPath, token, ""))
	return w
}

func TestAdminPassword(t *testing.T) {
	const secret = "s3-secret-key-do-not-log"

	t.Run("a cluster-admin gets it, uncached and unlogged", func(t *testing.T) {
		setupDevMode(t)
		allowMutations(t)
		logs := captureLogs(t)
		calls := stubAdminLogin(t, "admin", secret, nil)
		w := getAdminPassword("user:admin")
		if w.Code != http.StatusOK || *calls != 1 {
			t.Fatalf("status=%d calls=%d body=%s", w.Code, *calls, w.Body.String())
		}
		var body map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["user"] != "admin" || body["password"] != secret || body["accessKey"] != "access-"+secret || body["secretKey"] != "secret-"+secret {
			t.Fatalf("body=%s err=%v", w.Body.String(), err)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", cc)
		}
		if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), "access-") || strings.Contains(logs.String(), "secret-") {
			t.Errorf("the password was logged: %s", logs.String())
		}
		if !strings.Contains(logs.String(), "s3-admin-password") || !strings.Contains(logs.String(), "user=admin") {
			t.Errorf("the read is not recorded with the user: %s", logs.String())
		}
	})

	t.Run("only to the user who asked: a non-admin gets 403 and nothing is read", func(t *testing.T) {
		setupDevMode(t)
		mutationPermission = func(context.Context, string) (bool, error) { return false, nil }
		calls := stubAdminLogin(t, "admin", secret, nil)
		w := getAdminPassword("user:reader")
		if body := decodeError(t, w); w.Code != http.StatusForbidden || body["errorCode"] != "forbidden" {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if *calls != 0 || strings.Contains(w.Body.String(), secret) {
			t.Fatalf("read the Secret for a non-admin (calls=%d): %s", *calls, w.Body.String())
		}
	})

	t.Run("expired session: 401 and nothing is read", func(t *testing.T) {
		setupDevMode(t)
		allowMutations(t)
		calls := stubAdminLogin(t, "admin", secret, nil)
		if w := getAdminPassword(expiredToken); w.Code != http.StatusUnauthorized || *calls != 0 {
			t.Fatalf("status=%d calls=%d", w.Code, *calls)
		}
	})

	t.Run("permission check unavailable: 503 and nothing is read", func(t *testing.T) {
		setupDevMode(t)
		mutationPermission = func(context.Context, string) (bool, error) { return false, errors.New("connection refused") }
		calls := stubAdminLogin(t, "admin", secret, nil)
		w := getAdminPassword("user:admin")
		if body := decodeError(t, w); w.Code != http.StatusServiceUnavailable || body["errorCode"] != "authorization_unavailable" || *calls != 0 {
			t.Fatalf("status=%d calls=%d body=%s", w.Code, *calls, w.Body.String())
		}
	})

	t.Run("storage not set up by this tool: 409 with the reason", func(t *testing.T) {
		setupDevMode(t)
		allowMutations(t)
		stubAdminLogin(t, "", "", &cluster.S3AdminLoginUnavailable{Reason: "The S3 storage is not set up."})
		w := getAdminPassword("user:admin")
		if body := decodeError(t, w); w.Code != http.StatusConflict || body["errorCode"] != "unavailable" || body["error"] != "The S3 storage is not set up." {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", cc)
		}
	})

	t.Run("is a read: no lock, no operation", func(t *testing.T) {
		setupDevMode(t)
		allowMutations(t)
		stubAdminLogin(t, "admin", secret, nil)
		if !acquireClusterMutationLock() {
			t.Fatal("lock unexpectedly held")
		}
		defer releaseClusterMutationLock()
		if w := getAdminPassword("user:admin"); w.Code != http.StatusOK {
			t.Fatalf("refused while another operation runs: %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("off in the gitops profile, before any read", func(t *testing.T) {
		setupDevMode(t)
		calls := stubAdminLogin(t, "admin", secret, nil)
		mux := gitOpsMux(t)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, postJSONMethod(http.MethodGet, adminPasswordPath, "user:admin", ""))
		if body := decodeError(t, w); w.Code != http.StatusForbidden || body["errorCode"] != "profile_gitops" || *calls != 0 {
			t.Fatalf("status=%d calls=%d body=%s", w.Code, *calls, w.Body.String())
		}
	})
}
