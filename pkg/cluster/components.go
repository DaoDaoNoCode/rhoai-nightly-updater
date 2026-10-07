package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// DSCState values reported by GetComponents.
const (
	DSCStatePresent = "present"
	DSCStateNoDSC   = "no-dsc" // CRD installed, no DataScienceCluster yet
	DSCStateNoCRD   = "no-crd" // operator not installed, or its CRDs not created yet
)

// dscRead is the first DataScienceCluster as read.
type dscRead struct {
	Object map[string]interface{} // nil without a DSC
	State  string                 // DSCStatePresent, DSCStateNoDSC or DSCStateNoCRD
	// Version is the API version it was read at ("v3"); Skipped are the
	// newer served versions that failed with a conversion webhook error.
	Version string
	Skipped []versionFailure
}

// readFirstDSC returns the first DataScienceCluster, read at the newest
// served version that works (servedVersions; v2 then v1 when discovery is
// unavailable), and the DSC state.
func readFirstDSC(c *Client) (dscRead, error) {
	r, err := listServed(c, dscGroup, dscListFmt, nil, dscFallbackVersions)
	if err != nil {
		return dscRead{}, fmt.Errorf("fetching DSC list: %w", err)
	}
	if r.Version == "" {
		return dscRead{State: DSCStateNoCRD}, nil
	}
	var list struct {
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.Unmarshal(r.Body, &list); err != nil {
		return dscRead{}, fmt.Errorf("parsing DSC list: %w", err)
	}
	out := dscRead{State: DSCStateNoDSC, Version: r.Version, Skipped: r.Skipped}
	if len(list.Items) > 0 {
		out.Object, out.State = list.Items[0], DSCStatePresent
	}
	return out, nil
}

// versionFallback reports a DSC read at an older version than the
// preferred one, or nil.
func (r dscRead) versionFallback() *types.DSCVersionFallback {
	if len(r.Skipped) == 0 || r.Version == "" {
		return nil
	}
	return &types.DSCVersionFallback{Version: r.Skipped[0].Version, Used: r.Version, Message: r.Skipped[0].Message}
}

// GetComponents returns DSC component statuses and deployment information.
// A cluster without a DSC (or without the DSC CRD) is a normal state, reported
// through DSCExists/DSCState rather than an error. Labels already in the cache
// are always added; includeLabels also fetches missing ones from Quay (slower).
func GetComponents(c *Client, includeLabels bool) (*types.ComponentsResponse, error) {
	resp := &types.ComponentsResponse{Components: []types.ComponentInfo{}}

	// Every read below is independent of the DSC, so they run while it is read.
	var wg sync.WaitGroup
	run := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
		}()
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
	run(func() { appDeps, appErr = getDeployments(c, "redhat-ods-applications") })
	run(func() { opDeps, opDepsErr = getDeployments(c, "redhat-ods-operator") })
	run(func() { appPods, _ = getPodsInNamespace(c, "redhat-ods-applications") })
	run(func() { opPods, _ = getPodsInNamespace(c, "redhat-ods-operator") })
	run(func() { resp.ConsoleURL = cachedConsoleURL(c) })
	run(func() { snapshot, snapshotTime = GetDeploymentSnapshot(c) })
	run(func() {
		defer close(installedOpReadyCh)
		installedOp, installedOpErr = getInstalledOperator(c)
	})

	read, err := readFirstDSC(c)
	if err != nil {
		wg.Wait()
		return nil, err
	}
	resp.DSCState = read.State
	resp.DSCExists = read.Object != nil
	if read.Object != nil {
		resp.DSCAPIVersion, _ = read.Object["apiVersion"].(string)
		resp.DSCVersionFallback = read.versionFallback()
		<-installedOpReadyCh
		resp.DSCCompatibility = checkDSCCompatibilityFor(c, read, installedOp, installedOpErr)
		addDSCComponents(resp, read.Object)
	}
	wg.Wait()

	if installedOp != nil {
		resp.OperatorVersion = installedOp.Version
		resp.OperatorPhase = installedOp.Phase
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
	matchPodsToDeployments(appPods, "redhat-ods-applications", resp.Deployments)
	matchPodsToDeployments(opPods, "redhat-ods-operator", resp.Deployments)

	// Compare against last snapshot to detect changes
	if snapshot != nil {
		resp.SnapshotTime = snapshotTime
		resp.ChangedCount = CompareDeployments(resp.Deployments, snapshot)
	}

	fetchImageLabelsForDeployments(c, resp.Deployments, includeLabels)
	return resp, nil
}

// addDSCComponents fills the DSC fields and the component list from the DSC.
func addDSCComponents(resp *types.ComponentsResponse, dsc map[string]interface{}) {
	meta, _ := dsc["metadata"].(map[string]interface{})
	resp.DSCName, _ = meta["name"].(string)

	// Extract DSC phase from status
	dscStatus, _ := dsc["status"].(map[string]interface{})
	resp.DSCPhase, _ = dscStatus["phase"].(string)

	// Extract the Ready condition reason for the DSC phase explanation
	if conditions, ok := dscStatus["conditions"].([]interface{}); ok {
		for _, cond := range conditions {
			condMap, _ := cond.(map[string]interface{})
			condType, _ := condMap["type"].(string)
			if condType == "Ready" {
				msg, _ := condMap["message"].(string)
				resp.DSCReason = msg
				break
			}
		}
	}

	// Extract component management states from spec.components
	spec, _ := dsc["spec"].(map[string]interface{})
	specComponents, _ := spec["components"].(map[string]interface{})

	// Build a map of component conditions from status.conditions
	conditionMap := make(map[string]conditionInfo)
	if conditions, ok := dscStatus["conditions"].([]interface{}); ok {
		for _, cond := range conditions {
			condMap, _ := cond.(map[string]interface{})
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
		compMap, ok := compVal.(map[string]interface{})
		if !ok {
			continue
		}
		mgmtState, _ := compMap["managementState"].(string)
		if mgmtState == "" {
			// DataScienceCluster v3 groups some components without a state
			// of their own (dashboard: standard, maasPortal); the operator
			// reports the group's state in status.components.
			statusComponents, _ := dscStatus["components"].(map[string]interface{})
			statusComp, _ := statusComponents[compName].(map[string]interface{})
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
			if strings.HasSuffix(compName, suffix) {
				compName = strings.TrimSuffix(compName, suffix)
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
func groupManagementState(comp map[string]interface{}) string {
	parts, removed := 0, 0
	for _, v := range comp {
		part, ok := v.(map[string]interface{})
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
	for _, dep := range deployments {
		if dep.Image != "" && isRHOAIImage(dep.Image) {
			refs = append(refs, dep.Image)
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
		labelCtx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
		defer cancel()
		for ref, l := range resolveImageLabels(labelCtx, getQuayAuth(c), missing, true) {
			labels[ref] = l
		}
	} else {
		for _, ref := range missing {
			if l, ok := cachedImageLabels(ref); ok {
				labels[ref] = l
			}
		}
	}
	for i := range deployments {
		l := labels[deployments[i].Image]
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

func matchPodsToDeployments(pods []types.PodInfo, namespace string, deployments []types.DeploymentInfo) {
	for i := range deployments {
		if deployments[i].Namespace != namespace {
			continue
		}
		for _, pod := range pods {
			if podMatchesDeployment(pod, deployments[i]) {
				deployments[i].Pods = append(deployments[i].Pods, pod)
			}
		}
	}
}

// podMatchesDeployment checks if a pod belongs to a deployment using the
// deployment's label selector. Falls back to name prefix if no selector.
func podMatchesDeployment(pod types.PodInfo, dep types.DeploymentInfo) bool {
	if len(dep.MatchLabels) > 0 && len(pod.Labels) > 0 {
		for k, v := range dep.MatchLabels {
			if pod.Labels[k] != v {
				return false
			}
		}
		// Label selector matched — but exclude pods not owned by a ReplicaSet
		// (e.g., CronJob pods that share the same app label).
		// ReplicaSet-managed pods always have a pod-template-hash label.
		if _, hasHash := pod.Labels["pod-template-hash"]; !hasHash {
			if pod.OwnerKind != "" && pod.OwnerKind != "ReplicaSet" {
				return false
			}
		}
		return true
	}
	return strings.HasPrefix(pod.Name, dep.Name+"-")
}

// enrichComponentFix adds fix buttons only for cases where the operator's own
// message explicitly tells us what to do. We never replace the operator's message.
func enrichComponentFix(comp *types.ComponentInfo, reason string) {
	if comp.Status == "Available" || comp.Status == "Removed" {
		return
	}
	comp.Cause = componentCause(dscCondition{Reason: reason, Message: comp.Message})

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
func componentCause(cond dscCondition) string {
	lower := strings.ToLower(cond.Message)
	switch {
	case isUpgradeGateCondition(cond):
		return "An upgrade gate holds provisioning"
	case len(parseMissingDependencies(cond.Message)) > 0:
		deps := parseMissingDependencies(cond.Message)
		return fmt.Sprintf("Prerequisite %s not installed: %s", verb(len(deps), "operator", "operators"), strings.Join(deps, ", "))
	case strings.Contains(lower, "observedgeneration < generation") || strings.Contains(lower, "status is stale"):
		return "Its module operator has not reconciled the current spec yet"
	case len(parseApplyFailures(cond.Message)) > 0:
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
	read, err := readFirstDSC(c)
	if err != nil {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Failed to check for existing DSC: %v", err),
			Logs:      logs,
			ErrorCode: errorCodeFromK8sErr(err),
		}, nil
	}
	switch read.State {
	case DSCStateNoCRD:
		return &types.OperationResponse{
			Success:   false,
			Message:   "DataScienceCluster CRD not found. Install the RHOAI operator first.",
			Logs:      logs,
			ErrorCode: "prerequisites",
		}, nil
	case DSCStatePresent:
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
		return &types.OperationResponse{Success: false, Message: "Failed to fetch version-matched DSC defaults: " + fetchErr.Error(), Logs: logs, ErrorCode: "prerequisites"}, nil
	}
	logs = append(logs, fmt.Sprintf("DSC spec source: %s (%s)", defaults.SourceDescription, defaults.Version))

	apiVersion, _ := defaults.Spec["apiVersion"].(string)
	body, err := json.Marshal(defaults.Spec)
	if err != nil {
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Failed to encode DSC: %v", err), Logs: logs, ErrorCode: "internal"}, nil
	}
	_, _, createErr := c.post("/apis/"+apiVersion+"/datascienceclusters?fieldManager=rhoai-nightly-updater", body)
	if IsK8sError(createErr, http.StatusConflict) {
		logs = append(logs, "DataScienceCluster already exists (created concurrently)")
		return &types.OperationResponse{Success: true, Message: "DataScienceCluster already exists", Logs: logs}, nil
	}
	if createErr != nil {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Failed to create DataScienceCluster: %v", createErr),
			Logs:      logs,
			ErrorCode: errorCodeFromK8sErr(createErr),
		}, nil
	}

	logs = append(logs, "OK: DataScienceCluster created")

	// Step 3: Record activity
	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "create-dsc",
		Detail:    "default-dsc with default components",
		Success:   true,
	})

	slog.Info("dsc created", "user", getUser(c))

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
		} `json:"metadata"`
		Spec struct {
			Replicas *int `json:"replicas"`
			Selector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"selector"`
			Template struct {
				Spec struct {
					Containers []struct {
						Image string `json:"image"`
					} `json:"containers"`
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
	path := namespacedPath("apps/v1", "deployments", namespace, "")
	body, _, err := c.get(path)
	if err != nil {
		return nil, fmt.Errorf("fetching deployments in %s: %w", namespace, err)
	}

	var list deploymentList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parsing deployments: %w", err)
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

		// Get the first container image
		image := ""
		if containers := item.Spec.Template.Spec.Containers; len(containers) > 0 {
			image = containers[0].Image
		}

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
		})
	}

	return deps, nil
}
