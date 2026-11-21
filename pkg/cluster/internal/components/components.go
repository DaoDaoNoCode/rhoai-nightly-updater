package components

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// AddDSCComponents fills the DSC fields and the component list from the DSC.
func AddDSCComponents(resp *types.ComponentsResponse, dsc map[string]any) {
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

// FetchImageLabels populates GitCommit/GitURL/CommitDate/BuildDate/Version
// on each deployment from its image's OCI config labels. Cached labels are
// always used; fetch also reads missing ones from Quay with one pull-secret
// read for the whole batch.
func FetchImageLabels(c *kube.Client, deployments []types.DeploymentInfo, fetch bool) {
	var refs []string
	lookup := make([]string, len(deployments))
	for i, dep := range deployments {
		if ref := LabelImageRef(dep); ref != "" {
			lookup[i] = ref
			deployments[i].LabelImage = ref
			refs = append(refs, ref)
		}
	}
	if len(refs) == 0 {
		return
	}
	labels := make(map[string]*registry.ImageLabels, len(refs))
	var missing []string
	for _, ref := range refs {
		if l, ok := registry.CachedImageLabels(ref); ok && (l.CommitDate != "" || l.GitURL == "" || l.GitCommit == "") {
			labels[ref] = l
		} else {
			missing = append(missing, ref)
		}
	}
	if fetch && len(missing) > 0 {
		labelCtx, cancel := context.WithTimeout(c.Context(), 30*time.Second)
		defer cancel()
		maps.Copy(labels, registry.ResolveImageLabels(labelCtx, registry.QuayAuth(c), missing, true))
	} else {
		for _, ref := range missing {
			if l, ok := registry.CachedImageLabels(ref); ok {
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

type DeploymentContainer struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

// sidecarContainers are never the container a Deployment is about.
var sidecarContainers = map[string]bool{"kube-rbac-proxy": true, "oauth-proxy": true, "kube-auth-proxy": true, "ca-bundle-watcher": true}

// MainContainerImage is the image a Deployment is about: the container named
// like the Deployment (rhods-dashboard lists kube-rbac-proxy and core-bff
// before rhods-dashboard), else the first container that is not a known
// sidecar, else the first container.
func MainContainerImage(name string, containers []DeploymentContainer) string {
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

// LabelImageRef is the digest-pinned reference whose labels describe dep's
// image: the Deployment's own image when it is pinned, else the image ID of
// a ready pod that runs that very reference (a Deployment that names a
// floating tag, such as the MLflow server's quay.io/opendatahub/mlflow:odh-stable,
// resolves to a digest only on its pods; a pod of an earlier rollout names
// another reference and is skipped, whatever its position in the list).
// Empty for images that are not RHOAI or ODH builds (a UBI nginx has no
// commit to show) and for a floating tag without a ready pod.
func LabelImageRef(dep types.DeploymentInfo) string {
	if dep.Image == "" || !IsBuildImage(dep.Image) {
		return ""
	}
	if _, _, ok := registry.RepoAndDigest(dep.Image); ok {
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
		if r, _, ok := registry.RepoAndDigest(pod.ImageID); ok && r == repo {
			return pod.ImageID
		}
	}
	return ""
}

// IsBuildImage reports whether an image is built from an RHOAI or ODH
// repository, so its labels carry a commit: registry.redhat.io/rhoai (and
// its quay.io mirror) or quay.io/opendatahub.
func IsBuildImage(imageRef string) bool {
	return registry.IsRHOAIImage(imageRef) || strings.HasPrefix(strings.TrimPrefix(imageRef, "docker-pullable://"), "quay.io/opendatahub/")
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

func MatchPodsToDeployments(c *kube.Client, pods []types.PodInfo, namespace string, deployments []types.DeploymentInfo) {
	for i := range deployments {
		if deployments[i].Namespace != namespace {
			continue
		}
		for _, pod := range pods {
			if PodMatchesDeployment(c, pod, deployments[i]) {
				deployments[i].Pods = append(deployments[i].Pods, pod)
			}
		}
	}
}

// OwnerRef is the part of metadata.ownerReferences pod matching (and the
// hard reset's owner checks) read.
type OwnerRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller *bool  `json:"controller"`
}

// ControllerOf returns the controlling owner reference (controller: true),
// or the first reference when none is marked.
func ControllerOf(refs []OwnerRef) OwnerRef {
	var first OwnerRef
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

// OwnedByDeployment reports whether a pod belongs to the Deployment
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
func OwnedByDeployment(c *kube.Client, namespace, deployment, deploymentUID string, podLabels map[string]string, owner OwnerRef) bool {
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

// PodMatchesDeployment reports whether a pod belongs to a Deployment: the
// selector matches and the pod's controlling ReplicaSet is the Deployment's.
func PodMatchesDeployment(c *kube.Client, pod types.PodInfo, dep types.DeploymentInfo) bool {
	for k, v := range dep.MatchLabels {
		if pod.Labels[k] != v {
			return false
		}
	}
	return OwnedByDeployment(c, dep.Namespace, dep.Name, dep.UID, pod.Labels, OwnerRef{Kind: pod.OwnerKind, Name: pod.OwnerName, UID: pod.OwnerUID})
}

// replicaSetOwnerCache maps a ReplicaSet (cluster and UID) to the UID of its
// controlling Deployment ("" for none, or when it is gone). A ReplicaSet's
// controller changes only on adoption or release, so a short TTL bounds how
// long a change goes unseen, while the pods of one ReplicaSet and every
// poll in that window share one lookup.
var replicaSetOwnerCache = kube.NewLRU[string, string](512)

// replicaSetOwnerTTL is how long a ReplicaSet's controller is cached.
var replicaSetOwnerTTL = time.Minute

// replicaSetController returns the UID of the Deployment that controls the
// ReplicaSet name (uid) in namespace, or "" when none does or it is gone.
// The ServiceAccount may only list ReplicaSets (deploy/template.yaml and
// deploy/rbac-gitops.yaml grant "list", not "get"), so it lists with a
// metadata.name field selector, which every resource type supports
// (Kubernetes docs, Field Selectors), and checks the UID, so a ReplicaSet
// recreated under the same name is not mistaken for the pod's.
func replicaSetController(c *kube.Client, namespace, name, uid string) (string, error) {
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
				OwnerReferences []OwnerRef `json:"ownerReferences"`
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
	replicaSetOwnerCache.Add(key, owner, replicaSetOwnerTTL)
	return owner, nil
}

// enrichComponentFix adds fix buttons only for cases where the operator's own
// message explicitly tells us what to do. We never replace the operator's message.
func enrichComponentFix(comp *types.ComponentInfo, reason string) {
	if comp.Status == "Available" || comp.Status == "Removed" {
		return
	}
	comp.Cause = CauseOf(rhoai.DSCCondition{Reason: reason, Message: comp.Message})

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

// CauseOf is a short cause for a not-ready component's condition,
// classified from its message like the DataScienceCluster diagnostics do
// (without their cluster lookups); "" when it is not classified.
func CauseOf(cond rhoai.DSCCondition) string {
	lower := strings.ToLower(cond.Message)
	switch {
	case IsUpgradeGateCondition(cond):
		return "An upgrade gate holds provisioning"
	case len(ParseMissingDependencies(cond.Message)) > 0:
		deps := ParseMissingDependencies(cond.Message)
		return fmt.Sprintf("Prerequisite %s not installed: %s", kube.Verb(len(deps), "operator", "operators"), strings.Join(deps, ", "))
	case strings.Contains(lower, "observedgeneration < generation") || strings.Contains(lower, "status is stale"):
		return "Its module operator has not reconciled the current spec yet"
	case len(ParseApplyFailures(cond.Message)) > 0:
		for _, f := range ParseApplyFailures(cond.Message) {
			if f.Leftover() {
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
					Containers []DeploymentContainer `json:"containers"`
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

func Deployments(c *kube.Client, namespace string) ([]types.DeploymentInfo, error) {
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

		image := MainContainerImage(item.Metadata.Name, item.Spec.Template.Spec.Containers)

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

func NothingToDo(msg string, logs []string) *types.OperationResponse {
	return &types.OperationResponse{Success: false, Message: "Nothing to do: " + msg, Logs: logs, ErrorCode: "nothing_to_do"}
}
