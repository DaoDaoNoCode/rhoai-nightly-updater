package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
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
	"dashboard": "Removing the dashboard deletes the Dashboard CR, whose finalizer only the dashboard module's operator removes. If Dashboard Dev has paused " +
		"that operator, the deletion hangs. Change it in the OpenShift console after reverting Dashboard Dev.",
	"mlflowoperator": "The MLflowOperator CR cannot be deleted while any MLflow CR exists (its finalizer waits for them), so removing this " +
		"component hangs. Delete MLflow first (Environment page), then change it in the OpenShift console.",
	"aipipelines": "Pipeline servers (DSPAs) keep a finalizer that only the pipelines operator removes. Removing this component while DSPAs exist " +
		"leaves them and their projects stuck in Terminating. Delete the pipeline servers first, then change it in the OpenShift console.",
}

func deploymentReady(c *Client, namespace, name string) (bool, string) {
	var d struct {
		Spec struct {
			Replicas *int `json:"replicas"`
		} `json:"spec"`
		Status struct {
			ReadyReplicas int `json:"readyReplicas"`
		} `json:"status"`
	}
	found, err := c.GetIntoOptional(rhoai.NamespacedPath("apps/v1", "deployments", namespace, name), &d)
	if err != nil {
		if kube.IsDecodeError(err) {
			return false, fmt.Sprintf("could not parse Deployment %s/%s", namespace, name)
		}
		return false, fmt.Sprintf("could not read Deployment %s/%s: %v", namespace, name, err)
	}
	if !found {
		return false, fmt.Sprintf("Deployment %s/%s does not exist", namespace, name)
	}
	if d.Status.ReadyReplicas < 1 {
		return false, fmt.Sprintf("Deployment %s/%s has no ready pod", namespace, name)
	}
	return true, ""
}

// moduleCR returns the module CR of a component (its name and
// finalizers), or found=false. Discovery and list errors are returned, so a
// precondition is never skipped because the CR could not be read.
// kindUnknown: a module API group is served, but none of its kinds (in
// either module group) is named after the module, so the tool cannot tell
// which CR (if any) serves the module.
func moduleCR(c *Client, discover func() (rhoai.ComponentAPI, error), component string) (name string, finalizers []string, deleting, found, kindUnknown bool, err error) {
	api, err := discover()
	if err != nil {
		return "", nil, false, false, false, err
	}
	if api.Served() {
		if _, ok := api.KindForModule(component); !ok {
			return "", nil, false, false, true, nil
		}
	}
	items, err := listModuleCRs(c, discover, component)
	if err != nil || len(items) == 0 {
		return "", nil, false, false, false, err
	}
	m := items[0].Metadata
	return m.Name, m.Finalizers, m.DeletionTimestamp != "", true, false, nil
}

// removalChecker evaluates the preconditions for setting components to
// Removed. It caches the component API, the installed operator's
// Deployments, the RHOAI conversion CRDs and the Services behind them, so a
// change that removes several components reads them once.
type removalChecker struct {
	c           *Client
	api         func() (rhoai.ComponentAPI, error)
	operators   *moduleOperatorFinder
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
	api := cachedComponentAPI(c)
	return &removalChecker{c: c, api: api, operators: newModuleOperatorFinder(c, api), svcOwner: map[string]string{}, svcOwnerErr: map[string]error{}}
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
		return fmt.Sprintf("the RHOAI operator must be running, but the installed CSV %s lists no Deployment, so the tool cannot check it", kube.NonEmpty(op.Name, "(unknown)"))
	}
	var why []string
	for _, d := range op.Deployments {
		if ok, msg := deploymentReady(c, rhoai.SubNS, d); !ok {
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
	return fmt.Sprintf("can't tell which module serves it (no %s kind is named after %s), so the tool can't confirm an operator is running to clean up and won't remove it. Check in the OpenShift console, then change it there", strings.Join(rhoai.ModuleAPIGroups, " or "), component)
}

// unknownOperatorBlocker words a module CR whose operator the tool cannot
// identify (moduleOperatorFinder): it cannot check that the operator runs
// (to remove the CR's finalizers and clean up its operands), so it refuses
// (fail closed) rather than skip the check.
func unknownOperatorBlocker() string {
	return "can't confirm its operator is running to clean up, so the tool won't remove it. Check in the OpenShift console, then change it there"
}

// moduleOperatorBlocker returns the operator of the module CR crName, or
// why it cannot be checked: a read error (named) or an operator the finder
// cannot identify (the unknown-operator wording) blocks, fail closed.
func (rc *removalChecker) moduleOperatorBlocker(component, crName string) (*moduleOperator, string) {
	op, _, err := rc.operators.find(component, crName)
	switch {
	case err != nil:
		return nil, fmt.Sprintf("could not identify its operator (%v), so the tool can't confirm it is running to clean up and won't remove it", err)
	case op == nil:
		return nil, unknownOperatorBlocker()
	}
	return op, ""
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
	name, _, deleting, found, kindUnknown, err := moduleCR(c, rc.api, component)
	switch {
	case err != nil:
		blockers = append(blockers, fmt.Sprintf("could not read the %s module CR: %v", component, err))
	case kindUnknown:
		blockers = append(blockers, unknownModuleBlocker(component))
	case deleting:
		blockers = append(blockers, fmt.Sprintf("the %s module CR is already being deleted; see the Platform modules check", component))
	case found:
		op, why := rc.moduleOperatorBlocker(component, name)
		switch {
		case why != "":
			blockers = append(blockers, why)
		case !op.RHOAI: // the RHOAI operator is checked above
			if ok, why := deploymentReady(c, op.Namespace, op.Name); !ok {
				blockers = append(blockers, fmt.Sprintf("its operator %s cleans up the sub-component, but %s; fix that operator first", op.ref(), why))
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
	name, finalizers, deleting, found, kindUnknown, err := moduleCR(c, rc.api, component)
	switch {
	case err != nil:
		blockers = append(blockers, fmt.Sprintf("could not read the %s module CR: %v", component, err))
	case kindUnknown:
		blockers = append(blockers, unknownModuleBlocker(component))
	case deleting:
		blockers = append(blockers, fmt.Sprintf("the %s module CR is already being deleted; see the Platform modules check", component))
	case found:
		// The module operator removes the finalizers of the module CR and
		// of its operands (TrustyAIService, RayCluster, FeatureStore,
		// TrainJob, ...). If it is not running, they wait forever (D1, D7,
		// D8). The ServiceAccount cannot list those operands, so the
		// operator must be running whether or not the module CR itself has
		// finalizers. The RHOAI operator itself is checked above.
		op, why := rc.moduleOperatorBlocker(component, name)
		switch {
		case why != "":
			blockers = append(blockers, why)
		case !op.RHOAI:
			if ok, why := deploymentReady(c, op.Namespace, op.Name); !ok {
				what := "operands"
				if len(finalizers) > 0 {
					what = fmt.Sprintf("module CR (finalizer %s) and its operands", strings.Join(finalizers, ", "))
				}
				blockers = append(blockers, fmt.Sprintf("its operator %s removes the finalizers of the %s, but %s; fix that operator first or the removal hangs", op.ref(), what, why))
			}
		}
	}
	return blockers
}

// conversionBlocker refuses removing a component that serves the
// conversion webhook of a CRD with objects ("" when it serves none, or
// none of those CRDs has objects). Removing the component deletes the
// webhook's Service but not the CRDs, so their objects could then not be
// converted, garbage-collected or deleted with their namespace.
//
// The objects are listed with the signed-in user's own client
// (SignedInUser), never the ServiceAccount's: the CRDs are of any API
// group (mcpservers.mcp.x-k8s.io for mcplifecycleoperator), which the
// ServiceAccount must not be granted. The user is a cluster-admin (the
// plan's withAdminRead and the change's withMutationAuth check that).
// The Components page's repair preview (GET /api/components) carries it
// too in the full profile (pkg/api attachSignedInUser); the gitops
// profile never acts with the user's token. Without that client, or when
// a list fails, it refuses (fail closed): the tool never takes "could not
// list" for "none".
func (rc *removalChecker) conversionBlocker(component string) string {
	crds, convErr := rc.conversionCRDsServedBy(component)
	switch {
	case convErr != nil:
		return fmt.Sprintf("could not check which CRD conversion webhooks %s serves: %v", component, convErr)
	case len(crds) == 0:
		return ""
	}
	names := make([]string, 0, len(crds))
	gets := make([]string, 0, len(crds))
	for _, crd := range crds {
		names = append(names, crd.Metadata.Name)
		gets = append(gets, "oc get "+crd.Metadata.Name+" -A")
	}
	intro := fmt.Sprintf("%s serves the conversion webhook of %s %s. Removing it deletes that webhook's Service but not the %s, so objects of %s could then not be read at another version, garbage-collected or deleted with their namespace.",
		component, kube.Verb(len(crds), "CRD", "CRDs"), strings.Join(names, ", "), kube.Verb(len(crds), "CRD", "CRDs"), kube.Verb(len(crds), "that CRD", "those CRDs"))
	u := rc.c.SignedInUser()
	if u == nil {
		return intro + fmt.Sprintf(" The tool lists those objects only with your own sign-in, which this request does not carry (in the gitops profile no request does), so it cannot tell whether any exist and does not remove %s here. Check for them with: %s.",
			component, strings.Join(gets, "; "))
	}
	var found, failed []string
	for _, crd := range crds {
		objs, err := listConversionObjects(u, crd)
		switch {
		case err != nil:
			failed = append(failed, fmt.Sprintf("The objects of %s could not be listed with your sign-in: %s. Check for them with: oc get %s -A.",
				crd.Metadata.Name, strings.TrimSuffix(err.Error(), "."), crd.Metadata.Name))
		case len(objs.names) > 0:
			found = append(found, objs.sentence(crd))
		}
	}
	if len(found) == 0 && len(failed) == 0 {
		return ""
	}
	out := append([]string{intro}, found...)
	out = append(out, failed...)
	if len(found) > 0 {
		out = append(out, fmt.Sprintf("The tool does not remove %s until none remain.", component))
	}
	if len(failed) > 0 {
		out = append(out, fmt.Sprintf("The tool cannot tell whether any exist, so it does not remove %s.", component))
	}
	return strings.Join(out, " ")
}

// conversionObjectsLimit is the one page of a CRD's objects the removal
// check lists: enough to tell "none" from "some" and to name a few, cheap
// on a cluster with many. conversionObjectsShown are named per CRD.
const (
	conversionObjectsLimit = 20
	conversionObjectsShown = 5
)

// conversionObjects is the first page of a CRD's objects.
type conversionObjects struct {
	names []string // "namespace/name" ("name" when cluster-scoped), sorted
	// more: the API server returned metadata.continue, so more exist.
	more bool
	// remaining is metadata.remainingItemCount (nil when not set): an
	// estimate of the objects after this page.
	remaining *int64
}

// listConversionObjects lists the first page of a CRD's objects in every
// namespace with the client u (the signed-in user's).
//
// It lists at the CRD's storage version when that is served (listPath): the
// API server sends to the conversion webhook only the objects not already
// at the requested version (k8s.io/apiextensions-apiserver
// pkg/apiserver/conversion/webhook_converter.go getObjectsToConvert), so
// objects stored at the storage version need no conversion. Objects still
// stored at an older version (status.storedVersions) are converted; when
// that fails, so does the list, and the check refuses.
//
// The count follows the list semantics of k8s.io/apimachinery
// pkg/apis/meta/v1/types.go (https://kubernetes.io/docs/reference/using-api/api-concepts/#retrieving-large-results-sets-in-chunks):
// ListOptions.limit returns at most that many items and sets
// metadata.continue when more exist ("clients should only use the presence
// of the continue field to determine whether more results are available";
// a server may ignore limit and return all); ListMeta.remainingItemCount
// is meant for "*estimating* the size of a collection. Clients should not
// rely on the remainingItemCount to be set or to be exact." So the count is
// exact without continue, "N or more" with it, and remainingItemCount is
// only quoted as an estimate. A page without items but with continue
// (possible only for filtered lists, which this one is not) is not taken
// for "none".
//
// Only a list of the CRD's objects is taken for "none": an answer without
// "items", with "items": null, or of another kind than the CRD's list kind
// fails, so the check refuses. Every list carries "items" (the API
// conventions, "Lists and Simple kinds": "All lists use the required items
// field to contain the array of objects they return. Any kind that has the
// items field must be a list kind"; k8s.io/apimachinery
// pkg/apis/meta/v1/types.go List and unstructured/unstructured_list.go
// UnstructuredList declare `json:"items"` without omitempty, and
// unstructured/helpers.go doEncode, which encodes a custom resource list,
// writes it as [] when there are none). The list's kind is the CRD's own
// (listKind: status.acceptedNames.listKind, which a CRD may set to any
// name, "<Kind>List" being only the default); a CRD that names none
// leaves nothing to check a kind against, so a kind then refuses. The kind
// is only checked when the answer has one: an API server always sets it on
// a list, so its absence is not taken for a different kind.
func listConversionObjects(u *Client, crd conversionCRD) (conversionObjects, error) {
	path := crd.listPath()
	if path == "" {
		return conversionObjects{}, fmt.Errorf("the CRD serves no version the tool could list it at")
	}
	body, _, err := u.Do(http.MethodGet, path, "", nil, url.Values{"limit": {strconv.Itoa(conversionObjectsLimit)}})
	if err != nil {
		return conversionObjects{}, err
	}
	var list struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Continue           string `json:"continue"`
			RemainingItemCount *int64 `json:"remainingItemCount"`
		} `json:"metadata"`
		// A pointer, so an answer without items (or with null) is told
		// from an empty list.
		Items *[]struct {
			Metadata struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return conversionObjects{}, fmt.Errorf("could not parse the list: %w", err)
	}
	if list.Kind != "" {
		switch want := crd.listKind(); {
		case want == "":
			return conversionObjects{}, fmt.Errorf("the CRD names no list kind to check the API server's answer (a %s) against", list.Kind)
		case list.Kind != want:
			return conversionObjects{}, fmt.Errorf("the API server's answer is a %s, not a list of %s (%s)", list.Kind, crd.Metadata.Name, want)
		}
	}
	if list.Items == nil {
		return conversionObjects{}, fmt.Errorf("the API server's answer has no items, so it is not a list")
	}
	items := *list.Items
	if len(items) == 0 && list.Metadata.Continue != "" {
		return conversionObjects{}, fmt.Errorf("the API server returned no objects but says more exist")
	}
	out := conversionObjects{more: list.Metadata.Continue != "", remaining: list.Metadata.RemainingItemCount}
	for _, it := range items {
		name := it.Metadata.Name
		if it.Metadata.Namespace != "" {
			name = it.Metadata.Namespace + "/" + name
		}
		out.names = append(out.names, name)
	}
	sort.Strings(out.names)
	return out, nil
}

// sentence words the objects found for the refusal: how many, the first
// few, and how to delete them.
func (o conversionObjects) sentence(crd conversionCRD) string {
	n := len(o.names)
	crdName := crd.Metadata.Name
	count := kube.CountNoun(n, "object", "objects") + " of " + crdName + " " + kube.Verb(n, "exists", "exist")
	if o.more {
		count = fmt.Sprintf("%d or more objects of %s exist", n, crdName)
		if o.remaining != nil && *o.remaining > 0 {
			count += fmt.Sprintf(" (about %d, the API server's estimate)", int64(n)+*o.remaining)
		}
	}
	shown := o.names
	rest := ""
	switch {
	case len(shown) > conversionObjectsShown && o.more:
		shown, rest = shown[:conversionObjectsShown], " and more"
	case len(shown) > conversionObjectsShown:
		rest = fmt.Sprintf(" and %d more", len(shown)-conversionObjectsShown)
		shown = shown[:conversionObjectsShown]
	case o.more:
		rest = " and more"
	}
	del := "oc delete " + crdName + " -n <namespace> <name>"
	if crd.Spec.Scope == "Cluster" {
		del = "oc delete " + crdName + " <name>"
	}
	s := fmt.Sprintf("%s: %s%s.", count, strings.Join(shown, ", "), rest)
	if rest != "" {
		s += " List them all with: oc get " + crdName + " -A."
	}
	return s + fmt.Sprintf(" Delete %s first: %s.", kube.Verb(n, "it", "them"), del)
}

// conversionCRDsOf returns the names of the CRDs conversionCRDsServedBy
// returns.
func (rc *removalChecker) conversionCRDsOf(component string) ([]string, error) {
	crds, err := rc.conversionCRDsServedBy(component)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(crds))
	for _, crd := range crds {
		out = append(out, crd.Metadata.Name)
	}
	return out, nil
}

// conversionCRDsServedBy returns the RHOAI CRDs whose conversion webhook
// Service belongs to the component, sorted by name: the Service's
// ownerReference to a components.platform.opendatahub.io CR or its
// platform.opendatahub.io/part-of label names the component (live:
// trustyai-service-operator-webhook-service is owned by
// TrustyAI/default-trustyai), or, for a Service that is already gone, its
// name contains the component name (mcp-lifecycle-operator-webhook-service
// for mcplifecycleoperator), the same match the stale-conversion check uses.
func (rc *removalChecker) conversionCRDsServedBy(component string) ([]conversionCRD, error) {
	if !rc.convLoaded {
		rc.convLoaded = true
		crds, err := listRHOAIConversionCRDs(rc.c)
		if err != nil && !kube.IsK8sNotFound(err) {
			rc.convErr = err
		}
		rc.convCRDs = crds
	}
	if rc.convErr != nil {
		return nil, rc.convErr
	}
	var out []conversionCRD
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
			out = append(out, crd)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
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
	body, found, err := rc.c.GetOptional(rhoai.NamespacedPath("v1", "services", ns, name))
	switch {
	case err != nil:
		err = fmt.Errorf("read Service %s: %w", ref, err)
	case !found:
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
			if isModuleAPIVersion(o.APIVersion) {
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
