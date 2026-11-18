package api

import (
	"net/http/httptest"
	"testing"
)

// The nodes' pull check creates a CatalogSource for the image it is given,
// so it takes only the images Update takes, and refuses others before any
// cluster call (the dev-mode cluster address is not reachable here).
func TestNodePullCheckValidatesTheImage(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	for _, body := range []string{
		`{}`,
		`{"image":""}`,
		`{"image":"docker.io/evil/fbc:1"}`,
		`{"image":"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6; rm -rf /"}`,
		`not json`,
	} {
		w := httptest.NewRecorder()
		HandleNodePullCheck(w, postJSON("/api/setup/node-pull-check", "tok", body))
		if w.Code != 400 {
			t.Errorf("%s: status %d %s", body, w.Code, w.Body.String())
		}
		if clusterMutationInProgress.Load() {
			t.Fatalf("%s: the lock was not released", body)
		}
	}
}
