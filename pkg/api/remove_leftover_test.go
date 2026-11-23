package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Remove leftover keys takes only names of this tool's SeaweedFS users, and
// only from a user allowed to change the cluster.
func TestRemoveLeftoverKeys_RefusesBadNamesAndUsersWithoutPermission(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	orig := runRemoveLeftoverKeys
	t.Cleanup(func() { runRemoveLeftoverKeys = orig })
	var got [][]string
	runRemoveLeftoverKeys = func(_ *cluster.Client, users []string) (*types.OperationResponse, error) {
		got = append(got, users)
		return &types.OperationResponse{Success: true, Message: "ok"}, nil
	}
	long := "rnu-dspa-" + strings.Repeat("a", 56)
	for i, body := range []string{`{}`, `{"users":[]}`, `{"users":["admin"]}`, `{"users":["rnu-dspa-"]}`, `{"users":["rnu-dspa-A"]}`,
		`{"users":["rnu-dspa-x/../y"]}`, `{"users":["` + long + `"]}`, `{"users":["rnu-dspa-ok\n"]}`} {
		w := httptest.NewRecorder()
		HandleS3RemoveLeftoverKeys(w, postJSON(t, "/api/resources/s3/leftover-keys/remove", "user:u"+strings.Repeat("x", i), body))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	HandleS3RemoveLeftoverKeys(w, postJSON(t, "/api/resources/s3/leftover-keys/remove", "user:ok", `{"users":["rnu-dspa-ok"]}`))
	if w.Code != http.StatusOK || len(got) != 1 {
		t.Fatalf("valid request: %d %s, runs %v", w.Code, w.Body.String(), got)
	}
	mutationPermission = func(context.Context, string) (bool, error) { return false, nil }
	w = httptest.NewRecorder()
	HandleS3RemoveLeftoverKeys(w, postJSON(t, "/api/resources/s3/leftover-keys/remove", "user:mallory", `{"users":["rnu-dspa-ok"]}`))
	if w.Code == http.StatusOK || len(got) != 1 {
		t.Errorf("without permission: %d, runs %d", w.Code, len(got))
	}
}
