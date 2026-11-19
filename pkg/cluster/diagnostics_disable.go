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
// whose finalizer owner is up, and refuses one it cannot check.

// refusedComponents are never disabled by the tool, with the reason shown.
var refusedComponents = map[string]string{
	"dashboard": "Removing the dashboard deletes the Dashboard CR, whose finalizer only dashboard-operator removes. If Dashboard Dev has paused " +
		"dashboard-operator, the deletion hangs. Change it in the OpenShift console after reverting Dashboard Dev.",
	"mlflowoperator": "The MLflowOperator CR cannot be deleted while any MLflow CR exists (its finalizer waits for them), so removing this " +
		"component hangs. Delete MLflow first (Environment page), then change it in the OpenShift console.",
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
// the CR could not be read. kindUnknown: the component API is served, but
// none of its kinds is named after the module, so the tool cannot tell
// which CR (if any) serves the module.
func moduleCR(c *Client, discover func() (componentAPI, error), component string) (finalizers []string, deleting, found, kindUnknown bool, err error) {
	api, err := discover()
	if err != nil {
		return nil, false, false, false, err
	}
	if api.Version != "" {
		if _, ok := api.resourceForModule(component); !ok {
			return nil, false, false, true, nil
		}
	}
	items, err := listModuleCRs(c, discover, component)
	if err != nil || len(items) == 0 {
		return nil, false, false, false, err
	}
	m := items[0].Metadata
	return m.Finalizers, m.DeletionTimestamp != "", true, false, nil
}

// removalChecker evaluates the preconditions for setting components to
// Removed. It caches the component API, the installed operator's
// Deployments, the RHOAI conversion CRDs and the Services behind them, so a
// change that removes several components reads them once.
type removalChecker struct {
	c           *Client
	api         func() (componentAPI, error)
	opLoaded    bool
	opBlocker   string
	convLoaded  bool
	convCRDs    []conversionCRD
	convErr     error
	svcOwner    map[string]string
	svcOwnerErr map[string]error
	// What uses MaaS (maasUse), read once.
	maasLoaded bool
	maas       maasUse
	maasErr    error
}

func newRemovalChecker(c *Client) *removalChecker {
	return &removalChecker{c: c, api: cachedComponentAPI(c), svcOwner: map[string]string{}, svcOwnerErr: map[string]error{}}
}

// platformOperatorBlocker says why the RHOAI operator cannot apply a
// change now ("" when it can), read once per checker.
func (rc *removalChecker) platformOperatorBlocker() string {
	if !rc.opLoaded {
		rc.opLoaded = true
		rc.opBlocker = operatorDeploymentsBlocker(rc.c)
	}
	return rc.opBlocker
}

// operatorDeploymentsBlocker says why the installed RHOAI operator is not
// running ("" when it is): every Deployment the installed CSV installs
// (spec.install.spec.deployments, read at runtime rather than assuming a
// name) must have a ready pod. A CSV that cannot be read, or that lists no
// Deployment, blocks (fail closed).
func operatorDeploymentsBlocker(c *Client) string {
	op, err := getInstalledOperator(c)
	if err != nil {
		return "the RHOAI operator must be running, but its installed CSV could not be read to find its Deployment: " + err.Error()
	}
	if len(op.Deployments) == 0 {
		return fmt.Sprintf("the RHOAI operator must be running, but the installed CSV %s lists no Deployment, so the tool cannot check it", nonEmpty(op.Name, "(unknown)"))
	}
	var why []string
	for _, d := range op.Deployments {
		if ok, msg := deploymentReady(c, SubNS, d); !ok {
			why = append(why, msg)
		}
	}
	if len(why) > 0 {
		return "the RHOAI operator must be running to apply the change and clean up after it: " + strings.Join(why, "; ")
	}
	return ""
}

// blockers returns the reasons the component must not be set to Removed
// now. Removing a component deletes its module CR; once the CR is gone the
// platform deletes the module operator and the Services it serves (RHOAI
// operator notes §2.3), but never the CRDs (CRDs get no owner references).
func (rc *removalChecker) blockers(component string) []string {
	if reason, refused := refusedComponents[component]; refused {
		return []string{reason}
	}
	blockers := rc.moduleBlockers(component)
	if b := rc.conversionBlocker(component); b != "" {
		blockers = append(blockers, b)
	}
	return blockers
}

// teardownBlockers are the preconditions for a module a hard reset removes
// and brings back: the module and its operator must be able to clean up
// (moduleBlockers). The standing refusals do not apply (their reasons are
// checked one by one: dashboard-operator by moduleBlockers, MLflow by its
// in-use check, pipeline servers only stop working meanwhile), and a
// conversion webhook only fails while the module is down: the CRDs whose
// webhook it serves are returned (the plan lists them), not blockers. A
// module the reset leaves Removed gets blockers instead.
func (rc *removalChecker) teardownBlockers(component string) (blockers, conversionCRDs []string) {
	blockers = rc.moduleBlockers(component)
	crds, err := rc.conversionCRDsOf(component)
	if err != nil {
		blockers = append(blockers, fmt.Sprintf("could not check which CRD conversion webhooks %s serves: %v", component, err))
	}
	return blockers, crds
}

// unknownModuleBlocker words a module the tool cannot check: no kind of the
// component API is named after it, so the tool cannot tell which CR serves
// it nor which operator cleans it up.
func unknownModuleBlocker(component string) string {
	return fmt.Sprintf("can't tell which module serves it (no %s kind is named after %s), so the tool can't confirm an operator is running to clean up and won't remove it. Check in the OpenShift console, then change it there", componentAPIGroup, component)
}

// unknownOperatorBlocker words a module CR whose operator the tool does not
// know: it cannot check that the operator runs (to remove the CR's
// finalizers and clean up its operands), so it refuses (fail closed)
// rather than skip the check.
func unknownOperatorBlocker() string {
	return "can't confirm its operator is running to clean up, so the tool won't remove it. Check in the OpenShift console, then change it there"
}

// partBlockers are the preconditions for removing a sub-component while its
// module stays (kserve.nim while kserve stays Managed): the module CR is not
// deleted, so only the operators that act on the change must run, and the
// module CR must not be deleting already. Read errors and modules the tool
// cannot check block.
func (rc *removalChecker) partBlockers(component string) []string {
	c := rc.c
	var blockers []string
	if why := rc.platformOperatorBlocker(); why != "" {
		blockers = append(blockers, why)
	}
	_, deleting, found, kindUnknown, err := moduleCR(c, rc.api, component)
	switch {
	case err != nil:
		blockers = append(blockers, fmt.Sprintf("could not read the %s module CR: %v", component, err))
	case kindUnknown:
		blockers = append(blockers, unknownModuleBlocker(component))
	case deleting:
		blockers = append(blockers, fmt.Sprintf("the %s module CR is already being deleted; see the Platform modules check", component))
	case found:
		op, known := moduleOperators[component]
		switch {
		case !known:
			blockers = append(blockers, unknownOperatorBlocker())
		case op.name != "rhods-operator":
			if ok, why := deploymentReady(c, op.namespace, op.name); !ok {
				blockers = append(blockers, fmt.Sprintf("its operator %s/%s cleans up the sub-component, but %s; fix that operator first", op.namespace, op.name, why))
			}
		}
	}
	return blockers
}

// moduleBlockers are the preconditions about the module CR and the
// operators that remove it.
func (rc *removalChecker) moduleBlockers(component string) []string {
	c := rc.c
	var blockers []string
	if why := rc.platformOperatorBlocker(); why != "" {
		blockers = append(blockers, why)
	}
	finalizers, deleting, found, kindUnknown, err := moduleCR(c, rc.api, component)
	op, knownOp := moduleOperators[component]
	switch {
	case err != nil:
		blockers = append(blockers, fmt.Sprintf("could not read the %s module CR: %v", component, err))
	case kindUnknown:
		blockers = append(blockers, unknownModuleBlocker(component))
	case deleting:
		blockers = append(blockers, fmt.Sprintf("the %s module CR is already being deleted; see the Platform modules check", component))
	case found && !knownOp:
		blockers = append(blockers, unknownOperatorBlocker())
	}
	// The module operator removes the finalizers of the module CR and of
	// its operands (TrustyAIService, RayCluster, FeatureStore, TrainJob,
	// ...). If it is not running, they wait forever (D1, D7, D8). The
	// ServiceAccount cannot list those operands, so the operator must be
	// running whether or not the module CR itself has finalizers. The entry
	// "rhods-operator" is the RHOAI operator itself, checked above.
	if knownOp && op.name != "rhods-operator" && err == nil && found && !deleting {
		if ok, why := deploymentReady(c, op.namespace, op.name); !ok {
			what := "operands"
			if len(finalizers) > 0 {
				what = fmt.Sprintf("module CR (finalizer %s) and its operands", strings.Join(finalizers, ", "))
			}
			blockers = append(blockers, fmt.Sprintf("its operator %s/%s removes the finalizers of the %s, but %s; fix that operator first or the removal hangs", op.namespace, op.name, what, why))
		}
	}
	return blockers
}

// conversionBlocker refuses removing a component that serves a CRD
// conversion webhook ("" when it serves none). The tool cannot list the
// objects of those CRDs (its ServiceAccount reads no arbitrary resources),
// so the reason gives the commands that do, and what to do when there are
// none.
func (rc *removalChecker) conversionBlocker(component string) string {
	crds, convErr := rc.conversionCRDsOf(component)
	switch {
	case convErr != nil:
		return fmt.Sprintf("could not check which CRD conversion webhooks %s serves: %v", component, convErr)
	case len(crds) > 0:
		cmds := make([]string, 0, len(crds))
		for _, crd := range crds {
			cmds = append(cmds, "oc get "+crd+" -A")
		}
		return fmt.Sprintf("%s serves the conversion webhook of %s %s. Removing it deletes that webhook's Service but not the %s, so objects of %s could then not be read at another version, garbage-collected or deleted with their namespace. "+
			"This tool cannot list those objects, so it does not remove %s. Check for them with: %s. "+
			"If any exist, delete them first. If none exist, set %s to Removed yourself in the OpenShift console (edit the DataScienceCluster)",
			component, verb(len(crds), "CRD", "CRDs"), strings.Join(crds, ", "), verb(len(crds), "CRD", "CRDs"), verb(len(crds), "that CRD", "those CRDs"),
			component, strings.Join(cmds, "; "), component)
	}
	return ""
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
