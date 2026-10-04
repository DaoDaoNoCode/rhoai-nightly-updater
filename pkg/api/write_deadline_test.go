package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
)

func TestLongMutationOutlastsServerWriteTimeout(t *testing.T) {
	setupDevMode(t)
	original := mutationPermission
	mutationPermission = func(context.Context, string) (bool, error) { return true, nil }
	t.Cleanup(func() { mutationPermission = original })

	h := withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond) // longer than the server's WriteTimeout
		writeJSON(w, map[string]bool{"success": true}, "long-op")
	})
	srv := httptest.NewUnstartedServer(h)
	srv.Config.WriteTimeout = 100 * time.Millisecond
	srv.Start()
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/api/long-op", nil)
	req.Header.Set("X-Forwarded-Access-Token", "user-token")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("long mutation was cut off: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 || string(body) != "{\"success\":true}\n" {
		t.Fatalf("status=%d body=%q err=%v", resp.StatusCode, body, err)
	}
}
