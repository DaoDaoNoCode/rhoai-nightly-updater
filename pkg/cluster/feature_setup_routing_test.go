package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Models' prerequisites depend on how the model is reached: through a
// gateway they add Connectivity Link and its Kuadrant CR; inside the
// cluster only (or MaaS, whose own tab sets up its platform) they do not.
func TestPrerequisites_ModelsPlanDependsOnTheExposure(t *testing.T) {
	pc := newPrereqCluster(t)
	pc.sim.packages["rhcl-operator"] = rhclFakePackage()
	pc.putPackage("rhcl-operator")
	for _, tc := range []struct {
		exposure string
		rhcl     bool
	}{{"", false}, {exposureInternal, false}, {exposureMaaS, false}, {exposureDefault, true}, {exposureGateway, true}} {
		st, err := GetPrerequisitesStatusFor(pc.c, tc.exposure)
		if err != nil {
			t.Fatal(err)
		}
		plan := strings.Join(st.Features["models"].SetupPlan, "\n")
		ids := []string{}
		for _, p := range st.Features["models"].Parts {
			ids = append(ids, p.ID)
		}
		got := strings.Contains(plan, "Install Red Hat Connectivity Link") && strings.Contains(plan, "Create the Kuadrant CR kuadrant-system/kuadrant") && strings.Contains(strings.Join(ids, ","), "rhcl,kuadrant")
		if got != tc.rhcl {
			t.Errorf("%q: plan %q parts %v", tc.exposure, plan, ids)
		}
		// Only Models changes.
		if p := st.Features["pipelines"].SetupPlan; strings.Contains(strings.Join(p, " "), "Connectivity") {
			t.Errorf("pipelines plan: %v", p)
		}
	}
	if msg := ValidatePrerequisitesRequest(types.PrerequisitesRequest{Feature: "pipelines", Exposure: exposureDefault}); msg == "" {
		t.Error("an exposure for pipelines was accepted")
	}
	if msg := ValidatePrerequisitesRequest(types.PrerequisitesRequest{Feature: "models", Exposure: "everywhere"}); msg == "" {
		t.Error("an unknown exposure was accepted")
	}
}

// Set up with a gateway exposure installs Connectivity Link after
// cert-manager, then creates the Kuadrant CR once its API is served.
func TestPrerequisites_ModelsThroughAGatewayInstallsConnectivityLink(t *testing.T) {
	pc := newPrereqCluster(t)
	pc.sim.packages["rhcl-operator"] = rhclFakePackage()
	pc.putPackage("rhcl-operator")
	prev := pc.sim.onInstalled
	pc.sim.onInstalled = func(pkg, ns, from, to string) {
		prev(pkg, ns, from, to)
		if pkg == "rhcl-operator" {
			pc.f.groups = map[string]string{"/apis/kuadrant.io": kuadrantGroupDoc}
		}
	}
	pc.f.fail["GET /apis/kuadrant.io/v1/kuadrants"] = 404
	var events []UpdateStepEvent
	res, err := RunPrerequisites(pc.c, types.PrerequisitesRequest{Feature: "models", Exposure: exposureDefault}, func(e UpdateStepEvent) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success {
		t.Fatalf("setup: %+v (%s)", res, finalSteps(events))
	}
	if got := finalSteps(events); !strings.Contains(got, "cert-manager=success rhcl=success kuadrant=success ready=success") {
		t.Errorf("steps: %s", got)
	}
	if !pc.f.has("/apis/kuadrant.io/v1beta1/namespaces/kuadrant-system/kuadrants/kuadrant") {
		t.Error("no Kuadrant CR")
	}
}
