package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// GetComponents fetches DSC component statuses and deployment information.
// GetComponents returns DSC components and deployment info.
// If includeLabels is true, it also fetches git commit/build info from Quay (slower).
func GetComponents(c *Client, includeLabels bool) (*types.ComponentsResponse, error) {
	resp := &types.ComponentsResponse{}

	// Step 1: Get the DataScienceCluster (try v2 first, fall back to v1 for older RHOAI versions)
	dscBody, _, err := c.get("/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters")
	if err != nil {
		dscBody, _, err = c.get("/apis/datasciencecluster.opendatahub.io/v1/datascienceclusters")
		if err != nil {
			return nil, fmt.Errorf("fetching DSC list: %w", err)
		}
	}

	var dscList map[string]interface{}
	if err := json.Unmarshal(dscBody, &dscList); err != nil {
		return nil, fmt.Errorf("parsing DSC list: %w", err)
	}

	items, _ := dscList["items"].([]interface{})
	if len(items) == 0 {
		return nil, fmt.Errorf("no DataScienceCluster found")
	}

	dsc, _ := items[0].(map[string]interface{})
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

	// Step 2: Get deployments from redhat-ods-applications namespace
	appDeps, err := getDeployments(c, "redhat-ods-applications")
	if err == nil {
		resp.Deployments = append(resp.Deployments, appDeps...)
	}

	// Also get deployments from redhat-ods-operator namespace
	opDeps, err := getDeployments(c, "redhat-ods-operator")
	if err == nil {
		resp.Deployments = append(resp.Deployments, opDeps...)
	}

	// Match pods to deployments
	matchPodsToDeployments(c, "redhat-ods-applications", resp.Deployments)
	matchPodsToDeployments(c, "redhat-ods-operator", resp.Deployments)

	// Add console URL for log links
	resp.ConsoleURL = getConsoleURL(c)

	// Compare against last snapshot to detect changes
	snapshot, snapshotTime := GetDeploymentSnapshot(c)
	if snapshot != nil {
		resp.SnapshotTime = snapshotTime
		resp.ChangedCount = CompareDeployments(resp.Deployments, snapshot)
	}

	// Fetch image labels (git commit, build date, etc.) — only if requested.
	if includeLabels {
		fetchImageLabelsForDeployments(c, resp.Deployments)
	}

	return resp, nil
}

// fetchImageLabelsForDeployments populates GitCommit/GitURL/BuildDate/Version
// on each deployment by fetching OCI image config labels from Quay.
func fetchImageLabelsForDeployments(c *Client, deployments []types.DeploymentInfo) {
	// Deduplicate by image reference
	type fetchEntry struct {
		imageRef string
		indices  []int // indices into deployments slice
	}
	seen := make(map[string]*fetchEntry)
	for i, dep := range deployments {
		if dep.Image == "" || !isRHOAIImage(dep.Image) {
			continue
		}
		if entry, ok := seen[dep.Image]; ok {
			entry.indices = append(entry.indices, i)
		} else {
			seen[dep.Image] = &fetchEntry{
				imageRef: dep.Image,
				indices:  []int{i},
			}
		}
	}

	if len(seen) == 0 {
		return
	}

	labelCtx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
	defer cancel()

	// Fetch labels in parallel (up to 5 concurrent)
	type labelResult struct {
		imageRef string
		labels   *ImageLabels
	}

	results := make(chan labelResult, len(seen))
	sem := make(chan struct{}, 5) // concurrency limiter

	for _, entry := range seen {
		go func(ref string) {
			sem <- struct{}{}        // acquire
			defer func() { <-sem }() // release

			imageCtx, imageCancel := context.WithTimeout(labelCtx, 8*time.Second)
			defer imageCancel()

			labels, err := GetImageLabels(imageCtx, c, ref)
			if err != nil {
				slog.Warn("failed to fetch image labels", "image", truncateForLog(ref), "error", err)
				results <- labelResult{imageRef: ref, labels: nil}
				return
			}
			results <- labelResult{imageRef: ref, labels: labels}
		}(entry.imageRef)
	}

	// Collect results
	for range seen {
		res := <-results
		if res.labels == nil {
			continue
		}
		if entry, ok := seen[res.imageRef]; ok {
			for _, idx := range entry.indices {
				deployments[idx].GitCommit = res.labels.GitCommit
				deployments[idx].GitURL = res.labels.GitURL
				deployments[idx].CommitDate = res.labels.CommitDate
				deployments[idx].BuildDate = res.labels.BuildDate
				deployments[idx].Version = res.labels.Version
			}
		}
	}
}

func matchPodsToDeployments(c *Client, namespace string, deployments []types.DeploymentInfo) {
	pods, _ := getPodsInNamespace(c, namespace)
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

// checkComponentCRReady queries the actual component CR to see if it reports
// READY:True, regardless of what the DSC condition says. This detects stale
// conditions after EA↔GA transitions.

// enrichComponentFix adds fix buttons only for cases where the operator's own
// message explicitly tells us what to do. We never replace the operator's message.
func enrichComponentFix(comp *types.ComponentInfo, reason string) {
	if comp.Status == "Available" || comp.Status == "Removed" {
		return
	}

	switch {
	// Operator says "deprecated, please set it to Removed" — offer the button
	case comp.Name == "llamastackoperator" && strings.Contains(comp.Message, "deprecated"):
		comp.FixAction = "disable-component:" + comp.Name
		comp.FixTitle = "Disable LlamaStack"
		comp.FixConfirm = "This will set llamastackoperator to Removed in your DataScienceCluster, as the operator message recommends."

	// Operator says "missing required annotations" on MaaS gateway — documented fix
	case comp.Name == "maasprerequisites" && strings.Contains(comp.Message, "missing required annotations"):
		comp.FixAction = "fix-maas-gateway-annotation"
		comp.FixTitle = "Fix gateway"
		comp.FixConfirm = "This will add the opendatahub.io/managed annotation to the MaaS gateway. This is a documented requirement for MaaS."
	}
}

type conditionInfo struct {
	status  string
	reason  string
	message string
}

func getDeployments(c *Client, namespace string) ([]types.DeploymentInfo, error) {
	path := namespacedPath("apps/v1", "deployments", namespace, "")
	body, _, err := c.get(path)
	if err != nil {
		return nil, fmt.Errorf("fetching deployments in %s: %w", namespace, err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parsing deployments: %w", err)
	}

	items, _ := result["items"].([]interface{})
	var deps []types.DeploymentInfo
	for _, item := range items {
		obj, _ := item.(map[string]interface{})
		meta, _ := obj["metadata"].(map[string]interface{})
		name, _ := meta["name"].(string)

		spec, _ := obj["spec"].(map[string]interface{})
		replicas := 1
		if r, ok := spec["replicas"].(float64); ok {
			replicas = int(r)
		}

		status, _ := obj["status"].(map[string]interface{})
		readyReplicas := 0
		if r, ok := status["readyReplicas"].(float64); ok {
			readyReplicas = int(r)
		}
		availableReplicas := 0
		if r, ok := status["availableReplicas"].(float64); ok {
			availableReplicas = int(r)
		}
		unavailableReplicas := 0
		if r, ok := status["unavailableReplicas"].(float64); ok {
			unavailableReplicas = int(r)
		}
		updatedReplicas := 0
		if r, ok := status["updatedReplicas"].(float64); ok {
			updatedReplicas = int(r)
		}

		// Check for stuck rollout via Progressing condition
		rolloutStuck := false
		rolloutMessage := ""
		if conditions, ok := status["conditions"].([]interface{}); ok {
			for _, cond := range conditions {
				condMap, _ := cond.(map[string]interface{})
				condType, _ := condMap["type"].(string)
				condStatus, _ := condMap["status"].(string)
				condReason, _ := condMap["reason"].(string)
				if condType == "Progressing" && condStatus == "False" && condReason == "ProgressDeadlineExceeded" {
					rolloutStuck = true
					rolloutMessage, _ = condMap["message"].(string)
					break
				}
			}
		}

		// Get the first container image
		image := ""
		if tmpl, ok := spec["template"].(map[string]interface{}); ok {
			if tmplSpec, ok := tmpl["spec"].(map[string]interface{}); ok {
				if containers, ok := tmplSpec["containers"].([]interface{}); ok && len(containers) > 0 {
					if container, ok := containers[0].(map[string]interface{}); ok {
						image, _ = container["image"].(string)
					}
				}
			}
		}

		// Extract label selector for pod matching
		var matchLabels map[string]string
		if selector, ok := spec["selector"].(map[string]interface{}); ok {
			if ml, ok := selector["matchLabels"].(map[string]interface{}); ok {
				matchLabels = make(map[string]string)
				for k, v := range ml {
					if vs, ok := v.(string); ok {
						matchLabels[k] = vs
					}
				}
			}
		}

		deps = append(deps, types.DeploymentInfo{
			Name:                name,
			Namespace:           namespace,
			Ready:               readyReplicas,
			Desired:             replicas,
			Available:           availableReplicas,
			UnavailableReplicas: unavailableReplicas,
			UpdatedReplicas:     updatedReplicas,
			RolloutStuck:        rolloutStuck,
			RolloutMessage:      rolloutMessage,
			Image:               image,
			MatchLabels:         matchLabels,
		})
	}

	return deps, nil
}
