package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Preconditions for setting a DSC component to Removed.
//
// Removing a component makes rhods-operator delete the module CR. The CR's
// finalizer is removed by the module's own operator, and the operator is
// kept running only while the CR exists (rhods-operator rhoai-3.6
// modules_controller_actions.go cleanupDisabledModules). If that operator is
// not running, the CR, and then the Platform finalizer, wait forever
// (RHOAI operator notes D1, D7, D8). So the tool only removes a component
// whose finalizer owner is up.

// refusedComponents are never disabled by the tool, with the reason shown.
var refusedComponents = map[string]string{
	"dashboard": "Removing the dashboard deletes the Dashboard CR, whose finalizer only dashboard-operator removes. If Dashboard Dev has paused " +
		"dashboard-operator, the deletion hangs. Change it in the OpenShift console after reverting Dashboard Dev.",
	"mlflowoperator": "The MLflowOperator CR cannot be deleted while any MLflow CR exists (its finalizer waits for them), so removing this " +
		"component hangs. Tear down MLflow first (Test resources page), then change it in the OpenShift console.",
	"aipipelines": "Pipeline servers (DSPAs) keep a finalizer that only the pipelines operator removes. Removing this component while DSPAs exist " +
		"leaves them and their projects stuck in Terminating. Delete the pipeline servers first, then change it in the OpenShift console.",
}

func deploymentReady(c *Client, namespace, name string) (bool, string) {
	body, _, err := c.get(namespacedPath("apps/v1", "deployments", namespace, name))
	if IsK8sError(err, http.StatusNotFound) {
		return false, fmt.Sprintf("Deployment %s/%s does not exist", namespace, name)
	}
	if err != nil {
		return false, fmt.Sprintf("could not read Deployment %s/%s: %v", namespace, name, err)
	}
	var d struct {
		Spec struct {
			Replicas *int `json:"replicas"`
		} `json:"spec"`
		Status struct {
			ReadyReplicas int `json:"readyReplicas"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return false, fmt.Sprintf("could not parse Deployment %s/%s", namespace, name)
	}
	if d.Status.ReadyReplicas < 1 {
		return false, fmt.Sprintf("Deployment %s/%s has no ready pod", namespace, name)
	}
	return true, ""
}

// moduleCR returns the module CR of a component, or found=false. Discovery
// and list errors are returned, so a precondition is never skipped because
// the CR could not be read.
func moduleCR(c *Client, component string) (finalizers []string, deleting bool, found bool, err error) {
	items, err := listModuleCRs(c, cachedComponentAPI(c), component)
	if err != nil || len(items) == 0 {
		return nil, false, false, err
	}
	m := items[0].Metadata
	return m.Finalizers, m.DeletionTimestamp != "", true, nil
}

// removalChecker evaluates the preconditions for setting components to
// Removed. It caches the RHOAI conversion CRDs and the Services behind them,
// so a DSC repair that removes several components lists them once.
type removalChecker struct {
	c           *Client
	convLoaded  bool
	convCRDs    []conversionCRD
	convErr     error
	svcOwner    map[string]string
	svcOwnerErr map[string]error
}

func newRemovalChecker(c *Client) *removalChecker {
	return &removalChecker{c: c, svcOwner: map[string]string{}, svcOwnerErr: map[string]error{}}
}

// disableBlockers returns the reasons the component must not be removed now.
func disableBlockers(c *Client, component string) []string {
	return newRemovalChecker(c).blockers(component)
}

// blockers returns the reasons the component must not be set to Removed
// now. Removing a component deletes its module CR; once the CR is gone the
// platform deletes the module operator and the Services it serves (RHOAI
// operator notes §2.3), but never the CRDs (CRDs get no owner references).
func (rc *removalChecker) blockers(component string) []string {
	c := rc.c
	if reason, refused := refusedComponents[component]; refused {
		return []string{reason}
	}
	var blockers []string
	if ok, why := deploymentReady(c, SubNS, "rhods-operator"); !ok {
		blockers = append(blockers, "rhods-operator must be running to clean up a removed component: "+why)
	}
	finalizers, deleting, found, err := moduleCR(c, component)
	op, knownOp := moduleOperators[component]
	switch {
	case err != nil:
		blockers = append(blockers, fmt.Sprintf("could not read the %s module CR: %v", component, err))
	case deleting:
		blockers = append(blockers, fmt.Sprintf("the %s module CR is already being deleted; see the Platform modules check", component))
	case found && len(finalizers) > 0 && !knownOp:
		blockers = append(blockers, fmt.Sprintf("the %s module CR has finalizer %s and the tool does not know which operator removes it", component, strings.Join(finalizers, ", ")))
	}
	// The module operator removes the finalizers of the module CR and of
	// its operands (TrustyAIService, RayCluster, FeatureStore, TrainJob,
	// ...). If it is not running, they wait forever (D1, D7, D8). The
	// ServiceAccount cannot list those operands, so the operator must be
	// running whether or not the module CR itself has finalizers.
	if knownOp && op.name != "rhods-operator" && err == nil && found && !deleting {
		if ok, why := deploymentReady(c, op.namespace, op.name); !ok {
			what := "operands"
			if len(finalizers) > 0 {
				what = fmt.Sprintf("module CR (finalizer %s) and its operands", strings.Join(finalizers, ", "))
			}
			blockers = append(blockers, fmt.Sprintf("its operator %s/%s removes the finalizers of the %s, but %s; fix that operator first or the removal hangs", op.namespace, op.name, what, why))
		}
	}
	crds, convErr := rc.conversionCRDsOf(component)
	switch {
	case convErr != nil:
		blockers = append(blockers, fmt.Sprintf("could not check which CRD conversion webhooks %s serves: %v", component, convErr))
	case len(crds) > 0:
		blockers = append(blockers, fmt.Sprintf("CRD(s) %s convert their objects through a webhook Service of %s. Removing the component deletes that Service but not the CRDs, so reading their objects at another version, garbage collection and namespace deletion would fail (the dead mcpservers conversion on this cluster is the same case). "+
			"Delete those objects first (the tool cannot list them), then change the component in the OpenShift console", strings.Join(crds, ", "), component))
	}
	return blockers
}

// conversionCRDsOf returns the RHOAI CRDs whose conversion webhook Service
// belongs to the component: the Service's ownerReference to a
// components.platform.opendatahub.io CR or its platform.opendatahub.io/part-of
// label names the component (live: trustyai-service-operator-webhook-service
// is owned by TrustyAI/default-trustyai), or, for a Service that is already
// gone, its name contains the component name (mcp-lifecycle-operator-webhook-service
// for mcplifecycleoperator), the same match the stale-conversion check uses.
func (rc *removalChecker) conversionCRDsOf(component string) ([]string, error) {
	if !rc.convLoaded {
		rc.convLoaded = true
		crds, err := listRHOAIConversionCRDs(rc.c)
		if err != nil && !IsK8sError(err, http.StatusNotFound) {
			rc.convErr = err
		}
		rc.convCRDs = crds
	}
	if rc.convErr != nil {
		return nil, rc.convErr
	}
	var out []string
	for _, crd := range rc.convCRDs {
		ref := crd.conversionService()
		if ref == "" {
			continue
		}
		owner, err := rc.serviceComponent(ref)
		if err != nil {
			return nil, err
		}
		_, name, _ := strings.Cut(ref, "/")
		if owner == component || strings.Contains(strings.ReplaceAll(name, "-", ""), component) {
			out = append(out, crd.Metadata.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// serviceComponent returns the component that owns a Service ("" when it
// does not exist or names none).
func (rc *removalChecker) serviceComponent(ref string) (string, error) {
	if owner, ok := rc.svcOwner[ref]; ok {
		return owner, rc.svcOwnerErr[ref]
	}
	ns, name, _ := strings.Cut(ref, "/")
	owner := ""
	body, _, err := rc.c.get(namespacedPath("v1", "services", ns, name))
	switch {
	case IsK8sError(err, http.StatusNotFound):
		err = nil
	case err != nil:
		err = fmt.Errorf("read Service %s: %w", ref, err)
	default:
		var svc struct {
			Metadata struct {
				Labels          map[string]string `json:"labels"`
				OwnerReferences []struct {
					APIVersion string `json:"apiVersion"`
					Kind       string `json:"kind"`
				} `json:"ownerReferences"`
			} `json:"metadata"`
		}
		if jerr := json.Unmarshal(body, &svc); jerr != nil {
			err = fmt.Errorf("parse Service %s: %w", ref, jerr)
			break
		}
		for _, o := range svc.Metadata.OwnerReferences {
			if strings.HasPrefix(o.APIVersion, componentAPIGroup+"/") {
				owner = strings.ToLower(o.Kind)
			}
		}
		if owner == "" {
			if p := svc.Metadata.Labels["platform.opendatahub.io/part-of"]; p != "platform" {
				owner = p
			}
		}
	}
	rc.svcOwner[ref], rc.svcOwnerErr[ref] = owner, err
	return owner, err
}
