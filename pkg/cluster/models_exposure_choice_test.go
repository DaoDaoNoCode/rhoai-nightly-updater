package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// A gateway whose token checks are not known (labelled
// opendatahub.io/managed=false, or outside openshift-ingress where this tool
// cannot read AuthPolicies) is offered but never recommended.
func TestModelExposures_NeverRecommendsAGatewayWithUnknownTokenChecks(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true}); !res.Success {
		t.Fatalf("gateway: %+v", res)
	}
	gw := mc.f.get(testGatewayPath)
	ensureMap(ensureMap(gw, "metadata"), "labels")[odhManagedLabel] = "false"
	ex := exposures(t, mc, "")
	if a := optionByKey(t, ex, exposureDefault); !a.Available || !strings.Contains(a.Warning, "managed=false") || ex.Default != "" {
		t.Fatalf("default %s, option %+v", ex.Default, a)
	}
	// A setup that names no way is refused rather than routed there.
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj"}); res.Success || mc.f.has(testModelPath) {
		t.Fatalf("setup without a choice: %+v", res)
	}
	// Chosen explicitly, it is used.
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Exposure: exposureDefault}); !res.Success {
		t.Fatalf("explicit setup: %+v", res)
	}
}

// KServe's default gateway is used only when its configuration names it:
// unreadable or unset, the default route is not offered (and no project is
// added to the tool's gateway's list for it); the tool's gateway can still
// be chosen by name.
func TestModelExposures_UnknownKServeDefaultIsNotAssumed(t *testing.T) {
	for name, breakIt := range map[string]func(mc *modelsCluster){
		"unreadable": func(mc *modelsCluster) { mc.f.fail["GET "+testKServeConfig] = 500 },
		"unset": func(mc *modelsCluster) {
			k, _ := parseFakePath(testKServeConfig)
			delete(mc.f.objects, k)
		},
	} {
		t.Run(name, func(t *testing.T) {
			mc := newModelsCluster(t)
			if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true}); !res.Success {
				t.Fatalf("gateway: %+v", res)
			}
			breakIt(mc)
			ex := exposures(t, mc, "")
			if a := optionByKey(t, ex, exposureDefault); a.Available || ex.Default == exposureDefault {
				t.Fatalf("default route offered: %+v", a)
			}
			byName := optionByKey(t, ex, "gateway:openshift-ingress/openshift-ai-inference")
			if !byName.Available || len(byName.Changes) != 1 {
				t.Fatalf("by name: %+v", byName)
			}
			before := len(mc.f.mutations())
			if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Exposure: exposureDefault}); res.Success || len(mc.f.mutations()[before:]) != 0 {
				t.Fatalf("default route: %+v %v", res, mc.f.mutations()[before:])
			}
			wantAllowed(t, mc, "openshift-ingress")
			if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Exposure: exposureGateway, GatewayRef: "openshift-ingress/openshift-ai-inference"}); !res.Success {
				t.Fatalf("by name: %+v", res)
			}
			wantAllowed(t, mc, "openshift-ingress", "proj")
		})
	}
}
