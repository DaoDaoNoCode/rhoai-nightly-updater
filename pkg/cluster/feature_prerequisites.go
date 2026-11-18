package cluster

import (
	"fmt"
	"log/slog"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// featureComponent is the DataScienceCluster component that runs an
// Environment feature's operator.
type featureComponent struct {
	// feature names it for people ("Data Science Pipelines").
	feature string
	// keys are its names in spec.components, by DataScienceCluster API
	// version: v1 calls the pipelines component datasciencepipelines, v2
	// and later aipipelines (rhods-operator api/datasciencecluster). The
	// DataScienceCluster is read at the newest version that works, and the
	// key it has is used.
	keys []string
	// v1Key is the key DataScienceCluster v1 uses, when it differs (a
	// component the DataScienceCluster does not name yet is written with
	// it at v1, else with keys[0]).
	v1Key string
}

// keyIn is the key the component is written with in the DataScienceCluster
// as read: the one it names, else the read version's own.
func (fc featureComponent) keyIn(r dscRead) string {
	for _, key := range fc.keys {
		if _, found := componentManagement(r.Object, key); found {
			return key
		}
	}
	if r.Version == "v1" && fc.v1Key != "" {
		return fc.v1Key
	}
	return fc.keys[0]
}

var (
	pipelinesComponent = featureComponent{feature: "Data Science Pipelines", keys: []string{"aipipelines", "datasciencepipelines"}, v1Key: "datasciencepipelines"}
	mlflowComponent    = featureComponent{feature: "MLflow", keys: []string{"mlflowoperator"}}
)

// componentManagement is a component's managementState in a
// DataScienceCluster: spec.components.<key>.managementState, else the
// state the operator reports in status.components (DataScienceCluster v3
// groups some components without a state of their own). found is false
// when the DataScienceCluster does not name the component at all.
func componentManagement(dsc map[string]interface{}, key string) (state string, found bool) {
	spec, _ := dsc["spec"].(map[string]interface{})
	specComponents, _ := spec["components"].(map[string]interface{})
	status, _ := dsc["status"].(map[string]interface{})
	statusComponents, _ := status["components"].(map[string]interface{})
	specComp, inSpec := specComponents[key].(map[string]interface{})
	statusComp, inStatus := statusComponents[key].(map[string]interface{})
	if !inSpec && !inStatus {
		return "", false
	}
	state, _ = specComp["managementState"].(string)
	if state == "" {
		state, _ = statusComp["managementState"].(string)
	}
	return state, true
}

// componentBlock says why a feature cannot run on this cluster, from the
// DataScienceCluster as read, or returns nil. The operator deploys a
// component only when it is Managed (rhods-operator
// api/common ManagementSpec: Managed or Removed). It keeps a removed
// component's CRDs (pkg/controller/actions/gc DefaultUnremovables), so an
// installed CRD does not mean that anything runs the CRs: setting up a
// pipeline server or MLflow then creates a CR that never starts. A
// DataScienceCluster that does not name the component cannot tell, so
// nothing is blocked.
func componentBlock(r dscRead, fc featureComponent) *types.ComponentBlock {
	switch r.State {
	case DSCStateNoCRD:
		return &types.ComponentBlock{Component: fc.keys[0],
			Reason: fmt.Sprintf("%s is not available: RHOAI is not installed on this cluster (it has no DataScienceCluster API). A cluster-admin installs it on the Status page.", fc.feature)}
	case DSCStateNoDSC:
		return &types.ComponentBlock{Component: fc.keys[0],
			Reason: fmt.Sprintf("%s is not turned on: this cluster has no DataScienceCluster. A cluster-admin creates one on the Components page.", fc.feature)}
	}
	meta, _ := r.Object["metadata"].(map[string]interface{})
	name, _ := meta["name"].(string)
	for _, key := range fc.keys {
		state, found := componentManagement(r.Object, key)
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
			Reason: fmt.Sprintf("%s is turned off on this cluster: component %s is %s in DataScienceCluster %s, so nothing would run it. A cluster-admin turns it on by setting it to Managed.", fc.feature, key, shown, name)}
	}
	return nil
}

// setupComponentBlock reads the DataScienceCluster for a setup: the reason
// the feature's component is not turned on, or nil. A DataScienceCluster
// that cannot be read blocks nothing: the setup's own checks (the CRD)
// still apply.
func setupComponentBlock(c *Client, fc featureComponent) *types.ComponentBlock {
	r, err := readFirstDSC(c)
	if err != nil {
		slog.Debug("cannot read the DataScienceCluster for a setup check", "feature", fc.feature, "error", err)
		return nil
	}
	return componentBlock(r, fc)
}
