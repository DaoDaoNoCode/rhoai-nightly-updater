package envkit

import (
	"fmt"
	"log/slog"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// FeatureComponent is the DataScienceCluster component that runs an
// Environment feature's operator.
type FeatureComponent struct {
	// Feature names it for people ("Data Science Pipelines").
	Feature string
	// Keys are its names in spec.components, by DataScienceCluster API
	// version: v1 calls the pipelines component datasciencepipelines, v2
	// and later aipipelines (rhods-operator api/datasciencecluster). The
	// DataScienceCluster is read at the newest version that works, and the
	// key it has is used.
	Keys []string
	// V1Key is the key DataScienceCluster v1 uses, when it differs (a
	// component the DataScienceCluster does not name yet is written with
	// it at v1, else with keys[0]).
	V1Key string
}

// KeyIn is the key the component is written with in the DataScienceCluster
// as read: the one it names, else the read version's own.
func (fc FeatureComponent) KeyIn(r rhoai.DSCRead) string {
	for _, key := range fc.Keys {
		if _, found := ComponentManagement(r.Object, key); found {
			return key
		}
	}
	if r.Version == "v1" && fc.V1Key != "" {
		return fc.V1Key
	}
	return fc.Keys[0]
}

var (
	PipelinesComponent = FeatureComponent{Feature: "Data Science Pipelines", Keys: []string{"aipipelines", "datasciencepipelines"}, V1Key: "datasciencepipelines"}
	MLflowComponent    = FeatureComponent{Feature: "MLflow", Keys: []string{"mlflowoperator"}}
)

// ComponentManagement is a component's managementState in a
// DataScienceCluster: spec.components.<key>.managementState, else the
// state the operator reports in status.components (DataScienceCluster v3
// groups some components without a state of their own). found is false
// when the DataScienceCluster does not name the component at all.
func ComponentManagement(dsc map[string]any, key string) (state string, found bool) {
	spec, _ := dsc["spec"].(map[string]any)
	specComponents, _ := spec["components"].(map[string]any)
	status, _ := dsc["status"].(map[string]any)
	statusComponents, _ := status["components"].(map[string]any)
	specComp, inSpec := specComponents[key].(map[string]any)
	statusComp, inStatus := statusComponents[key].(map[string]any)
	if !inSpec && !inStatus {
		return "", false
	}
	state, _ = specComp["managementState"].(string)
	if state == "" {
		state, _ = statusComp["managementState"].(string)
	}
	return state, true
}

// ComponentBlock says why a feature cannot run on this cluster, from the
// DataScienceCluster as read, or returns nil. The operator deploys a
// component only when it is Managed (rhods-operator
// api/common ManagementSpec: Managed or Removed). It keeps a removed
// component's CRDs (pkg/controller/actions/gc DefaultUnremovables), so an
// installed CRD does not mean that anything runs the CRs: setting up a
// pipeline server or MLflow then creates a CR that never starts. A
// DataScienceCluster that does not name the component cannot tell, so
// nothing is blocked.
func ComponentBlock(r rhoai.DSCRead, fc FeatureComponent) *types.ComponentBlock {
	switch r.State {
	case rhoai.DSCStateNoCRD:
		return &types.ComponentBlock{Component: fc.Keys[0],
			Reason: fmt.Sprintf("%s is not available: RHOAI is not installed on this cluster (it has no DataScienceCluster API). A cluster-admin installs it on the Status page.", fc.Feature)}
	case rhoai.DSCStateNoDSC:
		return &types.ComponentBlock{Component: fc.Keys[0],
			Reason: fmt.Sprintf("%s is not turned on: this cluster has no DataScienceCluster. A cluster-admin creates one on the Components page.", fc.Feature)}
	}
	meta, _ := r.Object["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	for _, key := range fc.Keys {
		state, found := ComponentManagement(r.Object, key)
		if !found {
			continue
		}
		if state == "Managed" {
			return nil
		}
		shown := state
		if shown == "" {
			shown = "not set"
		}
		return &types.ComponentBlock{Component: key, DSC: name, State: state,
			Reason: fmt.Sprintf("%s is turned off on this cluster: component %s is %s in DataScienceCluster %s, so nothing would run it. A cluster-admin turns it on by setting it to Managed.", fc.Feature, key, shown, name)}
	}
	return nil
}

// SetupComponentBlock reads the DataScienceCluster for a setup: the reason
// the feature's component is not turned on, or nil. A DataScienceCluster
// that cannot be read blocks nothing: the setup's own checks (the CRD)
// still apply.
func SetupComponentBlock(c *kube.Client, fc FeatureComponent) *types.ComponentBlock {
	r, err := rhoai.ReadFirstDSC(c)
	if err != nil {
		slog.Debug("cannot read the DataScienceCluster for a setup check", "feature", fc.Feature, "error", err)
		return nil
	}
	return ComponentBlock(r, fc)
}
