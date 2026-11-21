package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/argo"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// GetComponents returns DSC component statuses and deployment information.
// A cluster without a DSC (or without the DSC CRD) is a normal state, reported
// through DSCExists/DSCState rather than an error. Labels already in the cache
// are always added; includeLabels also fetches missing ones from Quay (slower).
func GetComponents(c *Client, includeLabels bool) (*types.ComponentsResponse, error) {
	return getComponents(c, componentRegistryReads{labels: fetchImageLabelsForDeployments, dscSample: fetchDSCSample}, includeLabels)
}

// componentRegistryReads are the reads of GetComponents that the gitops
// profile leaves out: the images' OCI labels (from Quay, and commit dates
// from GitHub) and the GitHub DSC sample. nil fields are skipped.
type componentRegistryReads struct {
	labels    func(c *Client, deployments []types.DeploymentInfo, fetch bool)
	dscSample dscSampleFetcher
}

// GetComponentsGitOps is GetComponents in the gitops profile, which
// contacts no host but the API server (docs/GITOPS.md, section 11): no
// image labels (ImageLabelsOff), and the DSC defaults from the installed
// CSV's alm-examples only. The operator build comes from the installed CSV
// as in both profiles (OperatorImage, OperatorBuilt).
func GetComponentsGitOps(c *Client) (*types.ComponentsResponse, error) {
	return getComponents(c, componentRegistryReads{}, false)
}

func getComponents(c *Client, reads componentRegistryReads, includeLabels bool) (*types.ComponentsResponse, error) {
	resp := &types.ComponentsResponse{Components: []types.ComponentInfo{}, ImageLabelsOff: reads.labels == nil}

	// Every read below is independent of the DSC, so they run while it is read.
	var wg sync.WaitGroup
	run := func(fn func()) {
		wg.Go(func() {
			fn()
		})
	}
	var (
		appDeps, opDeps    []types.DeploymentInfo
		appErr, opDepsErr  error
		appPods, opPods    []types.PodInfo
		snapshot           map[string]string
		snapshotTime       string
		installedOp        *installedOperator
		installedOpErr     error
		installedOpReadyCh = make(chan struct{})
	)
	// The applications namespace is the DSCInitialization's; unknown, its
	// Deployments are left out and the response says why.
	resp.OperatorNamespace = rhoai.SubNS
	var appNS string
	var appNSErr error
	run(func() {
		if appNS, appNSErr = rhoai.AppsNamespace(c); appNSErr != nil {
			appErr = appNSErr
			return
		}
		var inner sync.WaitGroup
		inner.Go(func() { appDeps, appErr = getDeployments(c, appNS) })
		inner.Go(func() { appPods, _ = getPodsInNamespace(c, appNS) })
		inner.Wait()
	})
	run(func() { opDeps, opDepsErr = getDeployments(c, rhoai.SubNS) })
	run(func() { opPods, _ = getPodsInNamespace(c, rhoai.SubNS) })
	run(func() { resp.ConsoleURL = cachedConsoleURL(c) })
	run(func() { snapshot, snapshotTime = GetDeploymentSnapshot(c) })
	run(func() {
		defer close(installedOpReadyCh)
		installedOp, installedOpErr = getInstalledOperator(c)
	})

	read, err := rhoai.ReadFirstDSC(c)
	if err != nil {
		wg.Wait()
		return nil, err
	}
	resp.DSCState = read.State
	resp.DSCExists = read.Object != nil
	if read.Object != nil {
		resp.DSCAPIVersion, _ = read.Object["apiVersion"].(string)
		resp.DSCVersionFallback = read.VersionFallback()
		<-installedOpReadyCh
		resp.DSCCompatibility = checkDSCCompatibilityFor(c, read, installedOp, installedOpErr, reads.dscSample)
		addDSCComponents(resp, read.Object)
	}
	wg.Wait()

	if installedOp != nil {
		resp.OperatorVersion = installedOp.Version
		resp.OperatorPhase = installedOp.Phase
		resp.OperatorImage = installedOp.ContainerImage
		resp.OperatorBuilt = installedOp.CreatedAt
	}
	if appErr == nil {
		resp.Deployments = append(resp.Deployments, appDeps...)
	}
	if opDepsErr == nil {
		resp.Deployments = append(resp.Deployments, opDeps...)
	}
	if resp.Deployments == nil {
		resp.Deployments = []types.DeploymentInfo{}
	}
	switch {
	case appNSErr == nil:
		resp.ApplicationsNamespace = appNS
		matchPodsToDeployments(c, appPods, appNS, resp.Deployments)
	case !errors.Is(appNSErr, rhoai.ErrNoDSCInitialization): // none yet is not an error
		resp.ApplicationsNamespaceError = appNSErr.Error()
	}
	matchPodsToDeployments(c, opPods, rhoai.SubNS, resp.Deployments)

	// Compare against last snapshot to detect changes
	if snapshot != nil {
		resp.SnapshotTime = snapshotTime
		resp.ChangedCount = CompareDeployments(resp.Deployments, snapshot)
	}

	if reads.labels != nil {
		reads.labels(c, resp.Deployments, includeLabels)
	}
	return resp, nil
}

// addDSCComponents fills the DSC fields and the component list from the DSC.
func addDSCComponents(resp *types.ComponentsResponse, dsc map[string]any) {
	meta, _ := dsc["metadata"].(map[string]any)
	resp.DSCName, _ = meta["name"].(string)

	// Extract DSC phase from status
	dscStatus, _ := dsc["status"].(map[string]any)
	resp.DSCPhase, _ = dscStatus["phase"].(string)

	// Extract the Ready condition reason for the DSC phase explanation
	if conditions, ok := dscStatus["conditions"].([]any); ok {
		for _, cond := range conditions {
			condMap, _ := cond.(map[string]any)
			condType, _ := condMap["type"].(string)
			if condType == "Ready" {
				msg, _ := condMap["message"].(string)
				resp.DSCReason = msg
				break
			}
		}
	}

	// Extract component management states from spec.components
	spec, _ := dsc["spec"].(map[string]any)
	specComponents, _ := spec["components"].(map[string]any)

	// Build a map of component conditions from status.conditions
	conditionMap := make(map[string]conditionInfo)
	if conditions, ok := dscStatus["conditions"].([]any); ok {
		for _, cond := range conditions {
			condMap, _ := cond.(map[string]any)
			condType, _ := condMap["type"].(string)
			condStatus, _ := condMap["status"].(string)
			condReason, _ := condMap["reason"].(string)
			condMessage, _ := condMap["message"].(string)
			conditionMap[strings.ToLower(condType)] = conditionInfo{
				status:  condStatus,
				reason:  condReason,
				message: condMessage,
			}
		}
	}

	// Build component list from spec — report exactly what the operator says
	for compName, compVal := range specComponents {
		compMap, ok := compVal.(map[string]any)
		if !ok {
			continue
		}
		mgmtState, _ := compMap["managementState"].(string)
		if mgmtState == "" {
			// DataScienceCluster v3 groups some components without a state
			// of their own (dashboard: standard, maasPortal); the operator
			// reports the group's state in status.components.
			statusComponents, _ := dscStatus["components"].(map[string]any)
			statusComp, _ := statusComponents[compName].(map[string]any)
			mgmtState, _ = statusComp["managementState"].(string)
		}
		if mgmtState == "" {
			mgmtState = groupManagementState(compMap)
		}
		if mgmtState == "" {
			mgmtState = "Unknown"
		}

		compStatus := "Unknown"
		compReason := ""
		compMessage := ""

		readyKey := strings.ToLower(compName) + "ready"
		if ci, ok := conditionMap[readyKey]; ok {
			compReason = ci.reason
			compMessage = ci.message
			if ci.status == "True" {
				compStatus = "Available"
			} else if ci.reason == "Removed" || mgmtState == "Removed" || strings.Contains(ci.message, "set to Removed") {
				compStatus = "Removed"
			} else {
				compStatus = ci.reason
				if compStatus == "" {
					compStatus = "Not Ready"
				}
			}
		} else if mgmtState == "Removed" {
			compStatus = "Removed"
		}

		comp := types.ComponentInfo{
			Name:            compName,
			ManagementState: mgmtState,
			Status:          compStatus,
			Message:         compMessage,
		}
		enrichComponentFix(&comp, compReason)
		resp.Components = append(resp.Components, comp)
	}

	// Surface non-spec conditions that are failing (e.g., modelsasservice, maas-prerequisites)
	specNames := make(map[string]bool)
	for _, c := range resp.Components {
		specNames[strings.ToLower(c.Name)] = true
	}
	systemConditions := map[string]bool{
		"ready": true, "componentsready": true, "modulesready": true,
		"provisioningsucceeded": true, "provisioningprogress": true,
	}
	for key, ci := range conditionMap {
		if !strings.HasSuffix(key, "ready") && !strings.HasSuffix(key, "available") {
			continue
		}
		if systemConditions[key] {
			continue
		}
		// Extract component name from condition type
		compName := key
		for _, suffix := range []string{"ready", "available"} {
			if before, ok := strings.CutSuffix(compName, suffix); ok {
				compName = before
				break
			}
		}
		if specNames[compName] || compName == "" {
			continue
		}
		if ci.status == "True" || strings.Contains(ci.message, "set to Removed") {
			continue
		}

		comp := types.ComponentInfo{
			Name:    compName,
			Status:  ci.reason,
			Message: ci.message,
		}
		if comp.Status == "" {
			comp.Status = "Not Ready"
		}
		enrichComponentFix(&comp, ci.reason)
		resp.Components = append(resp.Components, comp)
	}
	// Map iteration order is random; keep the response stable between polls.
	sort.Slice(resp.Components, func(i, j int) bool { return resp.Components[i].Name < resp.Components[j].Name })
}

// groupManagementState summarizes the managementState of a grouped
// component's parts: Managed when any part is Managed, Removed when every
// part is Removed, "" otherwise (no parts, or other states).
func groupManagementState(comp map[string]any) string {
	parts, removed := 0, 0
	for _, v := range comp {
		part, ok := v.(map[string]any)
		if !ok {
			continue
		}
		state, ok := part["managementState"].(string)
		if !ok {
			continue
		}
		parts++
		switch state {
		case "Managed":
			return "Managed"
		case "Removed":
			removed++
		}
	}
	if parts > 0 && removed == parts {
		return "Removed"
	}
	return ""
}

// fetchImageLabelsForDeployments populates GitCommit/GitURL/CommitDate/BuildDate/Version
// on each deployment from its image's OCI config labels. Cached labels are
// always used; fetch also reads missing ones from Quay with one pull-secret
// read for the whole batch.
func fetchImageLabelsForDeployments(c *Client, deployments []types.DeploymentInfo, fetch bool) {
	var refs []string
	lookup := make([]string, len(deployments))
	for i, dep := range deployments {
		if ref := labelImageRef(dep); ref != "" {
			lookup[i] = ref
			deployments[i].LabelImage = ref
			refs = append(refs, ref)
		}
	}
	if len(refs) == 0 {
		return
	}
	labels := make(map[string]*ImageLabels, len(refs))
	var missing []string
	for _, ref := range refs {
		if l, ok := cachedImageLabels(ref); ok && (l.CommitDate != "" || l.GitURL == "" || l.GitCommit == "") {
			labels[ref] = l
		} else {
			missing = append(missing, ref)
		}
	}
	if fetch && len(missing) > 0 {
		labelCtx, cancel := context.WithTimeout(c.Context(), 30*time.Second)
		defer cancel()
		maps.Copy(labels, resolveImageLabels(labelCtx, getQuayAuth(c), missing, true))
	} else {
		for _, ref := range missing {
			if l, ok := cachedImageLabels(ref); ok {
				labels[ref] = l
			}
		}
	}
	for i := range deployments {
		l := labels[lookup[i]]
		if l == nil {
			continue
		}
		deployments[i].GitCommit = l.GitCommit
		deployments[i].GitURL = l.GitURL
		deployments[i].CommitDate = l.CommitDate
		deployments[i].BuildDate = l.BuildDate
		deployments[i].Version = l.Version
	}
}

type deploymentContainer struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

// sidecarContainers are never the container a Deployment is about.
var sidecarContainers = map[string]bool{"kube-rbac-proxy": true, "oauth-proxy": true, "kube-auth-proxy": true, "ca-bundle-watcher": true}

// mainContainerImage is the image a Deployment is about: the container named
// like the Deployment (rhods-dashboard lists kube-rbac-proxy and core-bff
// before rhods-dashboard), else the first container that is not a known
// sidecar, else the first container.
func mainContainerImage(name string, containers []deploymentContainer) string {
	for _, c := range containers {
		if c.Name == name {
			return c.Image
		}
	}
	for _, c := range containers {
		if !sidecarContainers[c.Name] {
			return c.Image
		}
	}
	if len(containers) > 0 {
		return containers[0].Image
	}
	return ""
}

// labelImageRef is the digest-pinned reference whose labels describe dep's
// image: the Deployment's own image when it is pinned, else the image ID of
// a ready pod that runs that very reference (a Deployment that names a
// floating tag, such as the MLflow server's quay.io/opendatahub/mlflow:odh-stable,
// resolves to a digest only on its pods; a pod of an earlier rollout names
// another reference and is skipped, whatever its position in the list).
// Empty for images that are not RHOAI or ODH builds (a UBI nginx has no
// commit to show) and for a floating tag without a ready pod.
func labelImageRef(dep types.DeploymentInfo) string {
	if dep.Image == "" || !isBuildImage(dep.Image) {
		return ""
	}
	if _, _, ok := extractRepoAndDigest(dep.Image); ok {
		return dep.Image
	}
	repo := imageRepoOf(dep.Image)
	if repo == "" {
		return ""
	}
	for _, pod := range dep.Pods {
		if !pod.Ready || pod.Image != dep.Image {
			continue
		}
		if r, _, ok := extractRepoAndDigest(pod.ImageID); ok && r == repo {
			return pod.ImageID
		}
	}
	return ""
}

// isBuildImage reports whether an image is built from an RHOAI or ODH
// repository, so its labels carry a commit: registry.redhat.io/rhoai (and
// its quay.io mirror) or quay.io/opendatahub.
func isBuildImage(imageRef string) bool {
	return isRHOAIImage(imageRef) || strings.HasPrefix(strings.TrimPrefix(imageRef, "docker-pullable://"), "quay.io/opendatahub/")
}

// imageRepoOf is the repository path of a reference with a tag and no
// digest, as extractRepoAndDigest spells it: "quay.io/opendatahub/mlflow:odh-stable"
// gives "opendatahub/mlflow".
func imageRepoOf(ref string) string {
	ref = strings.TrimPrefix(ref, "docker-pullable://")
	if colon := strings.LastIndex(ref, ":"); colon > strings.LastIndex(ref, "/") {
		ref = ref[:colon]
	}
	slash := strings.Index(ref, "/")
	if slash < 0 {
		return ""
	}
	return ref[slash+1:]
}

func matchPodsToDeployments(c *Client, pods []types.PodInfo, namespace string, deployments []types.DeploymentInfo) {
	for i := range deployments {
		if deployments[i].Namespace != namespace {
			continue
		}
		for _, pod := range pods {
			if podMatchesDeployment(c, pod, deployments[i]) {
				deployments[i].Pods = append(deployments[i].Pods, pod)
			}
		}
	}
}

// ownerRef is the part of metadata.ownerReferences pod matching (and the
// hard reset's owner checks) read.
type ownerRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller *bool  `json:"controller"`
}

// controllerOf returns the controlling owner reference (controller: true),
// or the first reference when none is marked.
func controllerOf(refs []ownerRef) ownerRef {
	var first ownerRef
	for i, ref := range refs {
		if ref.Controller != nil && *ref.Controller {
			return ref
		}
		if i == 0 {
			first = ref
		}
	}
	return first
}

// ownedByDeployment reports whether a pod belongs to the Deployment
// (deploymentUID, in namespace), given its labels and its controlling owner.
//
// The fast path needs no API call: the Deployment controller names each
// ReplicaSet it creates "<deployment>-<pod-template-hash>" and gives it and
// its pods that pod-template-hash label (Kubernetes docs, Deployments: "the
// name of the ReplicaSet is always formatted as [DEPLOYMENT-NAME]-[HASH]").
// That is a naming convention, not a guarantee: a Deployment also adopts
// existing ReplicaSets its selector matches, and adoption does not rename
// them (pkg/controller/deployment/deployment_controller.go:531-556 at
// v1.35.0 claims them with ClaimReplicaSets; ReplicaSet names cannot
// change). So a pod whose controller is a ReplicaSet with another name is
// checked against that ReplicaSet's controller UID (replicaSetController).
// A label selector alone is not enough: selectors may overlap
// (trustyai-operator-module-controller-manager selects only
// control-plane=controller-manager, which many operators' pods carry).
// When the ReplicaSet cannot be read, the pod is not matched (fail closed).
func ownedByDeployment(c *Client, namespace, deployment, deploymentUID string, podLabels map[string]string, owner ownerRef) bool {
	if owner.Kind != "ReplicaSet" {
		return false
	}
	if hash := podLabels["pod-template-hash"]; hash != "" && owner.Name == deployment+"-"+hash {
		return true
	}
	if c == nil || deploymentUID == "" || owner.UID == "" {
		return false
	}
	controller, err := replicaSetController(c, namespace, owner.Name, owner.UID)
	if err != nil {
		slog.Debug("pod not matched to its deployment: its ReplicaSet's owner cannot be read", "namespace", namespace, "replicaset", owner.Name, "deployment", deployment, "error", err)
		return false
	}
	return controller == deploymentUID
}

// podMatchesDeployment reports whether a pod belongs to a Deployment: the
// selector matches and the pod's controlling ReplicaSet is the Deployment's.
func podMatchesDeployment(c *Client, pod types.PodInfo, dep types.DeploymentInfo) bool {
	for k, v := range dep.MatchLabels {
		if pod.Labels[k] != v {
			return false
		}
	}
	return ownedByDeployment(c, dep.Namespace, dep.Name, dep.UID, pod.Labels, ownerRef{Kind: pod.OwnerKind, Name: pod.OwnerName, UID: pod.OwnerUID})
}

// replicaSetOwnerCache maps a ReplicaSet (cluster and UID) to the UID of its
// controlling Deployment ("" for none, or when it is gone). A ReplicaSet's
// controller changes only on adoption or release, so a short TTL bounds how
// long a change goes unseen, while the pods of one ReplicaSet and every
// poll in that window share one lookup.
var replicaSetOwnerCache = kube.NewLRU[string, string](512)

// ReplicaSetOwnerTTL is how long a ReplicaSet's controller is cached.
var ReplicaSetOwnerTTL = time.Minute

// replicaSetController returns the UID of the Deployment that controls the
// ReplicaSet name (uid) in namespace, or "" when none does or it is gone.
// The ServiceAccount may only list ReplicaSets (deploy/template.yaml and
// deploy/rbac-gitops.yaml grant "list", not "get"), so it lists with a
// metadata.name field selector, which every resource type supports
// (Kubernetes docs, Field Selectors), and checks the UID, so a ReplicaSet
// recreated under the same name is not mistaken for the pod's.
func replicaSetController(c *Client, namespace, name, uid string) (string, error) {
	key := c.BaseURL() + "/" + uid
	if owner, ok := replicaSetOwnerCache.Get(key); ok {
		return owner, nil
	}
	q := url.Values{"fieldSelector": {"metadata.name=" + name}}
	body, _, err := c.Do(http.MethodGet, rhoai.NamespacedPath("apps/v1", "replicasets", namespace, ""), "", nil, q)
	if err != nil {
		return "", err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				UID             string     `json:"uid"`
				OwnerReferences []ownerRef `json:"ownerReferences"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return "", fmt.Errorf("parse ReplicaSet %s/%s: %w", namespace, name, err)
	}
	owner := ""
	for _, rs := range list.Items {
		if rs.Metadata.UID != uid {
			continue
		}
		for _, ref := range rs.Metadata.OwnerReferences {
			if ref.Controller != nil && *ref.Controller && ref.Kind == "Deployment" {
				owner = ref.UID
			}
		}
	}
	replicaSetOwnerCache.Add(key, owner, ReplicaSetOwnerTTL)
	return owner, nil
}

// enrichComponentFix adds fix buttons only for cases where the operator's own
// message explicitly tells us what to do. We never replace the operator's message.
func enrichComponentFix(comp *types.ComponentInfo, reason string) {
	if comp.Status == "Available" || comp.Status == "Removed" {
		return
	}
	comp.Cause = componentCause(rhoai.DSCCondition{Reason: reason, Message: comp.Message})

	// Operator says "deprecated, please set it to Removed" — offer the button.
	// (A MaaS gateway "fix" used to be offered here for a "missing required
	// annotations" message that no current operator emits; its patch set
	// opendatahub.io/managed=false, which stops the operator reconciling the
	// gateway rather than meeting a documented requirement, so it was removed.)
	if comp.Name == "llamastackoperator" && strings.Contains(comp.Message, "deprecated") {
		comp.FixAction = "disable-component:" + comp.Name
		comp.FixTitle = "Disable LlamaStack"
		comp.FixConfirm = "This will set llamastackoperator to Removed in your DataScienceCluster, as the operator message recommends."
	}
}

// componentCause is a short cause for a not-ready component's condition,
// classified from its message like the DataScienceCluster diagnostics do
// (without their cluster lookups); "" when it is not classified.
func componentCause(cond rhoai.DSCCondition) string {
	lower := strings.ToLower(cond.Message)
	switch {
	case isUpgradeGateCondition(cond):
		return "An upgrade gate holds provisioning"
	case len(parseMissingDependencies(cond.Message)) > 0:
		deps := parseMissingDependencies(cond.Message)
		return fmt.Sprintf("Prerequisite %s not installed: %s", kube.Verb(len(deps), "operator", "operators"), strings.Join(deps, ", "))
	case strings.Contains(lower, "observedgeneration < generation") || strings.Contains(lower, "status is stale"):
		return "Its module operator has not reconciled the current spec yet"
	case len(parseApplyFailures(cond.Message)) > 0:
		for _, f := range parseApplyFailures(cond.Message) {
			if f.leftover() {
				return "An object is left over from an earlier release"
			}
		}
		return "The operator cannot update one of its objects"
	case strings.Contains(lower, "conversion webhook"):
		return "A CRD conversion webhook cannot be called"
	}
	return ""
}

type conditionInfo struct {
	status  string
	reason  string
	message string
}

// CreateDefaultDSC creates a DataScienceCluster with default component configuration.
// If a DSC already exists, it returns success without modification.
//
// The DSC is created with a plain create, never an apply: on Managed RHOAI the
// operator creates default-dsc itself at startup (bootstrap.RunLeaderElectionInit,
// platform ManagedRhoai only), and an apply racing it would take over and
// overwrite its fields. A create that loses the race gets 409 AlreadyExists,
// and the operator's validating webhook rejects a second DSC under another
// name ("Only one instance of DataScienceCluster object is allowed").
func CreateDefaultDSC(c *Client) (*types.OperationResponse, error) {
	logs := []string{}

	// Step 1: Check if a DSC already exists
	logs = append(logs, "Checking for existing DataScienceCluster...")
	read, err := rhoai.ReadFirstDSC(c)
	if err != nil {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Failed to check for existing DSC: %v", err),
			Logs:      logs,
			ErrorCode: kube.ErrorCodeFromK8sErr(err),
		}, nil
	}
	if g, err := argo.ReadOperatorGitOps(c); err != nil {
		return &types.OperationResponse{Success: false, Message: "Cannot check whether Argo CD manages the operator: " + err.Error(), Logs: logs, ErrorCode: kube.ErrorCodeFromK8sErr(err)}, nil
	} else if app := kube.NonEmpty(g.SubscriptionApp, g.CatalogApp); app != "" {
		// Argo CD only: an Argo CD application that installs the operator
		// (the odh-gitops chart) renders the DSC too. An ACM policy that
		// enforces the catalog says nothing about the DSC.
		return &types.OperationResponse{Success: false, ErrorCode: kube.ErrorCodeGitOpsManaged, Logs: logs,
			Message: fmt.Sprintf("Argo CD application %q manages the RHOAI operator here and creates the DataScienceCluster from Git; one made by hand would be adopted and overwritten on its next sync. Define the DataScienceCluster in Git instead. Nothing was changed.", app)}, nil
	}
	switch read.State {
	case rhoai.DSCStateNoCRD:
		return &types.OperationResponse{
			Success:   false,
			Message:   "DataScienceCluster CRD not found. Install the RHOAI operator first.",
			Logs:      logs,
			ErrorCode: "prerequisites",
		}, nil
	case rhoai.DSCStatePresent:
		logs = append(logs, "DataScienceCluster already exists")
		return &types.OperationResponse{
			Success: true,
			Message: "DataScienceCluster already exists",
			Logs:    logs,
		}, nil
	}

	// Step 2: Use the sample shipped with the installed operator version.
	logs = append(logs, "Creating default DataScienceCluster...")
	defaults, fetchErr := fetchDefaultDSCSpec(c, "")
	if fetchErr != nil {
		return &types.OperationResponse{Success: false, Message: "Failed to fetch version-matched DSC defaults: " + fetchErr.Error(), Logs: logs, ErrorCode: kube.NonEmpty(kube.ErrorCodeFromK8sErr(fetchErr), "prerequisites")}, nil
	}
	logs = append(logs, fmt.Sprintf("DSC spec source: %s (%s)", defaults.SourceDescription, defaults.Version))

	apiVersion, _ := defaults.Spec["apiVersion"].(string)
	body, err := json.Marshal(defaults.Spec)
	if err != nil {
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Failed to encode DSC: %v", err), Logs: logs, ErrorCode: "internal"}, nil
	}
	_, _, createErr := c.Post("/apis/"+apiVersion+"/datascienceclusters?fieldManager=rhoai-nightly-updater", body)
	if kube.IsK8sConflict(createErr) {
		logs = append(logs, "DataScienceCluster already exists (created concurrently)")
		return &types.OperationResponse{Success: true, Message: "DataScienceCluster already exists", Logs: logs}, nil
	}
	if createErr != nil {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Failed to create DataScienceCluster: %v", createErr),
			Logs:      logs,
			ErrorCode: kube.ErrorCodeFromK8sErr(createErr),
		}, nil
	}

	logs = append(logs, "OK: DataScienceCluster created")

	// Step 3: Record activity
	record.RecordActivity(c, types.ActivityEntry{
		Timestamp: record.NowRFC3339(),
		User:      kube.GetUser(c),
		Action:    "create-dsc",
		Detail:    "default-dsc with default components",
		Success:   true,
	})

	slog.Info("dsc created", "user", kube.GetUser(c))

	return &types.OperationResponse{
		Success: true,
		Message: "DataScienceCluster created with default components",
		Logs:    logs,
	}, nil
}

// deploymentList is the part of a Deployment list GetComponents reads.
// Decoding into typed structs instead of maps cuts CPU about 3x and
// allocations about 50x on the live 800 KB list (perf03 bench).
type deploymentList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"metadata"`
		Spec struct {
			Replicas *int `json:"replicas"`
			Selector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"selector"`
			Template struct {
				Spec struct {
					Containers []deploymentContainer `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
		Status struct {
			ReadyReplicas       int `json:"readyReplicas"`
			AvailableReplicas   int `json:"availableReplicas"`
			UnavailableReplicas int `json:"unavailableReplicas"`
			UpdatedReplicas     int `json:"updatedReplicas"`
			Conditions          []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

func getDeployments(c *Client, namespace string) ([]types.DeploymentInfo, error) {
	path := rhoai.NamespacedPath("apps/v1", "deployments", namespace, "")
	var list deploymentList
	if err := c.GetInto(path, &list); err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parsing deployments: %w", err)
		}
		return nil, fmt.Errorf("fetching deployments in %s: %w", namespace, err)
	}

	var deps []types.DeploymentInfo
	for _, item := range list.Items {
		replicas := 1
		if item.Spec.Replicas != nil {
			replicas = *item.Spec.Replicas
		}

		// Check for stuck rollout via Progressing condition
		rolloutStuck := false
		rolloutMessage := ""
		for _, cond := range item.Status.Conditions {
			if cond.Type == "Progressing" && cond.Status == "False" && cond.Reason == "ProgressDeadlineExceeded" {
				rolloutStuck = true
				rolloutMessage = cond.Message
				break
			}
		}

		image := mainContainerImage(item.Metadata.Name, item.Spec.Template.Spec.Containers)

		deps = append(deps, types.DeploymentInfo{
			Name:                item.Metadata.Name,
			Namespace:           namespace,
			Ready:               item.Status.ReadyReplicas,
			Desired:             replicas,
			Available:           item.Status.AvailableReplicas,
			UnavailableReplicas: item.Status.UnavailableReplicas,
			UpdatedReplicas:     item.Status.UpdatedReplicas,
			RolloutStuck:        rolloutStuck,
			RolloutMessage:      rolloutMessage,
			Image:               image,
			MatchLabels:         item.Spec.Selector.MatchLabels,
			UID:                 item.Metadata.UID,
		})
	}

	return deps, nil
}
