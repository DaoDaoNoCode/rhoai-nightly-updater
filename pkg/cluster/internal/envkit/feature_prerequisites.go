package envkit

import (
	"fmt"

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
	// On is the managementState that turns it on: "" means Managed. RHOAI
	// 3.x accepts only Unmanaged for kueue (rhods-operator rhoai-3.6
	// internal/webhook/datasciencecluster/v3/validating.go: "Managed is
	// no longer supported as a managementState").
	On string
}

// OnState is the managementState that turns the component on.
func (fc FeatureComponent) OnState() string {
	if fc.On != "" {
		return fc.On
	}
	return "Managed"
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
	// ModelRegistryComponent runs the model registry operator. RHOAI 3.6's
	// DataScienceCluster v3 calls it aiHub (instancesNamespace), which also
	// runs the Model Catalog; v2, and every release before 3.6, call it
	// modelregistry (registriesNamespace). The conversion maps one to the
	// other (rhods-operator rhoai-3.6 api/datasciencecluster/v2/
	// datasciencecluster_conversion.go). The key the DataScienceCluster as
	// read has is used, so no release is named.
	ModelRegistryComponent = FeatureComponent{Feature: "Model registry", Keys: []string{"aiHub", "modelregistry"}, V1Key: "modelregistry"}
	// KueueComponent integrates the Red Hat build of Kueue: on is
	// Unmanaged, the only on state RHOAI 3.x accepts (RHOAI docs, Managing
	// OpenShift AI, "Manage workloads with Kueue": Managed is deprecated,
	// Unmanaged is supported, Removed disables).
	KueueComponent = FeatureComponent{Feature: "Kueue component", Keys: []string{"kueue"}, On: "Unmanaged"}
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
		if state == fc.OnState() {
			return nil
		}
		shown := state
		if shown == "" {
			shown = "not set"
		}
		return &types.ComponentBlock{Component: key, DSC: name, State: state,
			Reason: fmt.Sprintf("%s is turned off on this cluster: component %s is %s in DataScienceCluster %s, so nothing would run it. A cluster-admin turns it on by setting it to %s.", fc.Feature, key, shown, name, fc.OnState())}
	}
	return nil
}

// ErrorCodePrerequisitesUnknown: whether a setup's prerequisites are met
// could not be read, so it was refused before any change (fail closed).
const ErrorCodePrerequisitesUnknown = "prerequisites_unknown"

// SetupComponentBlock reads the DataScienceCluster for a setup: why the
// feature's component keeps it from running, with its errorCode
// ("prerequisites"), or "" when it may run.
//
// The DataScienceCluster is a cluster fact, read with the facts client
// (c.Facts(), the ServiceAccount's) through ReadFirstDSC, which tries the
// served versions in turn (rhoai.ListServed). It moves on to the next
// version only when a version is not served (404) or its objects cannot be
// converted: HTTP 500, or 429 "storage is (re)initializing", whose message
// names the conversion webhook (rhoai.IsConversionWebhookError; the 429's
// Retry-After is not waited for). So RHOAI 3.6's v3 next to an older
// operator blocks nothing on its own. Any other error at a version (a
// generic 500, 403, a network error) stops the read, as does a conversion
// failure at every version; the setup is then refused with
// ErrorCodePrerequisitesUnknown: whether the component is on is unknown,
// and a CR of a component that is off is created and never starts. The
// reason and the log give the read's HTTP status and a fixed category, not
// its text (the ServiceAccount's error may name what a user does not see).
func SetupComponentBlock(c *kube.Client, fc FeatureComponent) (reason, errorCode string) {
	r, err := rhoai.ReadFirstDSC(c.Facts())
	if err != nil {
		kube.LogFactReadFailure("cannot read the DataScienceCluster for a setup check; the setup is refused", "DataScienceCluster", err, "feature", fc.Feature)
		return fmt.Sprintf("Cannot read whether %s is turned on: the DataScienceCluster could not be read (%s).", fc.Feature, kube.FactReadProblem(err)), ErrorCodePrerequisitesUnknown
	}
	if b := ComponentBlock(r, fc); b != nil {
		return b.Reason, "prerequisites"
	}
	return "", ""
}

// SetupBlockActivity is the activity log's word for a SetupComponentBlock
// refusal with errorCode.
func SetupBlockActivity(errorCode string) string {
	if errorCode == ErrorCodePrerequisitesUnknown {
		return "component state unknown"
	}
	return "component not managed"
}
