package api

import (
	"net/http/httptest"
	"strings"
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
		// A digest: OLM would not pull a cached image again
		// (InferImagePullPolicy), so the check could pass without the
		// nodes' credentials.
		`{"image":"quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:` + strings.Repeat("a", 64) + `"}`,
		`{"image":"quay.io/rhoai/rhoai-fbc-fragment@sha256:` + strings.Repeat("a", 64) + `"}`,
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
