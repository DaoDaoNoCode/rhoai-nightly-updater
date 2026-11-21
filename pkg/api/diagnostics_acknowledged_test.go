package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The acknowledged fix requires the binding the user reviewed (the
// problem's acknowledgeBinding) and passes it on unchanged.
func TestDiagnosticsFixAcknowledgedRequiresTheBinding(t *testing.T) {
	setupDevMode(t)
	allowMutations(t)
	mux := http.NewServeMux()
	Register(mux)
	var got []cluster.AckBinding
	prev := applyFixAcknowledged
	t.Cleanup(func() { applyFixAcknowledged = prev })
	applyFixAcknowledged = func(_ *cluster.Client, id string, b cluster.AckBinding) (*types.OperationResponse, error) {
		got = append(got, b)
		return &types.OperationResponse{Success: true, Message: id}, nil
	}
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"problemId":"fix-leftover-fields:TrustyAI/default-trustyai","acknowledged":true}`, http.StatusBadRequest},
		{`{"problemId":"fix-leftover-fields:TrustyAI/default-trustyai","acknowledged":true,"binding":{"uid":"u","resourceVersion":"1"}}`, http.StatusBadRequest},
		{`{"problemId":"fix-leftover-fields:TrustyAI/default-trustyai","acknowledged":false,"binding":{"uid":"u","resourceVersion":"1","hash":"h"}}`, http.StatusBadRequest},
		{`{"problemId":"fix-leftover-fields:TrustyAI/default-trustyai","acknowledged":true,"binding":{"uid":"u","resourceVersion":"1","hash":"h"}}`, http.StatusOK},
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, postJSON(t, "/api/diagnostics/fix/acknowledged", "user:admin", tc.body))
		if w.Code != tc.want {
			t.Errorf("%s: %d %s", tc.body, w.Code, w.Body.String())
		}
		resetPackageState(t)
	}
	if len(got) != 1 || got[0] != (cluster.AckBinding{UID: "u", ResourceVersion: "1", Hash: "h"}) {
		t.Fatalf("bindings passed on: %+v", got)
	}
}
