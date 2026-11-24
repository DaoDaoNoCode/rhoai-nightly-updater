package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// H2: Recipe Clean up for MaaS platform and model-registry parts the run
// created must include the run id plus each reviewed object's UID; under
// the cluster lock, every such part is re-checked immediately prior to
// teardown—if it was replaced in the meantime, cleanup refuses and skips
// removal. Environment-page Clean up with both fields empty still removes
// resources as before; supplying only a UID or only the run id is rejected.
func TestCleanUp_RecipeCreatedPartsAreCheckedAgainBeforeTheirTeardown(t *testing.T) {
	origReg, origMaaS, origProblem := cleanupModelRegistry, cleanupMaaSPlatform, recipeCreatedPartProblem
	t.Cleanup(func() {
		cleanupModelRegistry, cleanupMaaSPlatform, recipeCreatedPartProblem = origReg, origMaaS, origProblem
	})
	var torn, checked []string
	cleanupModelRegistry = func(*Client) (*types.OperationResponse, error) {
		torn = append(torn, "model-registry")
		return &types.OperationResponse{Success: true}, nil
	}
	cleanupMaaSPlatform = func(*Client) (*types.OperationResponse, error) {
		torn = append(torn, "maas-platform")
		return &types.OperationResponse{Success: true}, nil
	}
	live := map[string]string{types.RecipePartModelRegistry: "reg-2", types.RecipePartMaaSPlatform: "secret-1"} // cluster now holds a different registry UID
	recipeCreatedPartProblem = func(_, _ *Client, runID, part, uid string) string {
		checked = append(checked, runID+"/"+part+"/"+uid)
		if live[part] != uid {
			return "replaced or changed"
		}
		return ""
	}
	run := func(req types.CleanupRequest) map[string]*types.OperationResponse {
		torn, checked = nil, nil
		out := map[string]*types.OperationResponse{}
		for _, p := range cleanupParts(nil, req) {
			res, err := p.run()
			if err != nil {
				t.Fatal(err)
			}
			out[p.id] = res
		}
		return out
	}

	got := run(types.CleanupRequest{ModelRegistry: true, MaaSPlatform: true, RecipeRunID: "run-1", ModelRegistryUID: "reg-1", MaaSPlatformUID: "secret-1"})
	if got["model-registry"].Success || got["model-registry"].ErrorCode != "conflict" || !got["maas-platform"].Success {
		t.Errorf("results %+v %+v", got["model-registry"], got["maas-platform"])
	}
	if strings.Join(torn, " ") != "maas-platform" || strings.Join(checked, " ") != "run-1/model-registry/reg-1 run-1/maas-platform/secret-1" {
		t.Errorf("torn down %v, checked %v", torn, checked)
	}

	run(types.CleanupRequest{ModelRegistry: true, MaaSPlatform: true})
	if strings.Join(torn, " ") != "model-registry maas-platform" || len(checked) != 0 {
		t.Errorf("Environment page clean-up: torn %v, checked %v", torn, checked)
	}

	for _, req := range []types.CleanupRequest{{ModelRegistry: true, ModelRegistryUID: "reg-2"}, {MaaSPlatform: true, RecipeRunID: "run-1"}} {
		got := run(req)
		for id, res := range got {
			if res.Success || !strings.Contains(res.Message, "names only one") {
				t.Errorf("%s with half a review: %+v", id, res)
			}
		}
		if len(torn) != 0 {
			t.Errorf("half a review tore down %v", torn)
		}
	}
}

// The distributed workloads setup a recipe run created is checked again
// before its teardown, as the other cluster-wide parts are: replaced since
// the review, it is refused and not torn down.
func TestCleanUp_RecipeKueueIsCheckedAgainBeforeItsTeardown(t *testing.T) {
	origKueue, origProblem := cleanupKueue, recipeCreatedPartProblem
	t.Cleanup(func() { cleanupKueue, recipeCreatedPartProblem = origKueue, origProblem })
	torn := 0
	cleanupKueue = func(*Client) (*types.OperationResponse, error) {
		torn++
		return &types.OperationResponse{Success: true}, nil
	}
	recipeCreatedPartProblem = func(_, _ *Client, runID, part, uid string) string {
		if part != types.RecipePartKueue || runID != "run-1" || uid != "cq-1" {
			return "replaced or changed"
		}
		return ""
	}
	for _, tc := range []struct {
		uid  string
		torn int
	}{{"cq-1", 1}, {"cq-0", 0}} {
		torn = 0
		for _, p := range cleanupParts(nil, types.CleanupRequest{Kueue: true, RecipeRunID: "run-1", KueueUID: tc.uid}) {
			res, err := p.run()
			if err != nil || (tc.torn == 0) == res.Success {
				t.Errorf("%s: %+v %v", tc.uid, res, err)
			}
		}
		if torn != tc.torn {
			t.Errorf("%s: torn down %d times", tc.uid, torn)
		}
	}
}
