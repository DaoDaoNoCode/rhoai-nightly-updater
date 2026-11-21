package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	dashboardDeploymentName = "rhods-dashboard"
	dashboardContainerName  = "rhods-dashboard"
	prImagePrefix           = "quay.io/opendatahub/odh-dashboard:pr-"
)

// prContainerRepos maps container names in the rhods-dashboard deployment
// to their Quay repos where PR images are published.
var prContainerRepos = map[string]string{
	"rhods-dashboard":   "opendatahub/odh-dashboard",
	"model-registry-ui": "opendatahub/odh-mod-arch-modular-architecture",
	"gen-ai-ui":         "opendatahub/odh-mod-arch-gen-ai",
	"maas-ui":           "opendatahub/mod-arch-maas",
	"mlflow-ui":         "opendatahub/odh-mod-arch-mlflow",
	"eval-hub-ui":       "opendatahub/odh-mod-arch-eval-hub",
	"automl-ui":         "opendatahub/odh-mod-arch-automl",
	"autorag-ui":        "opendatahub/odh-mod-arch-autorag",
	"agent-ops-ui":      "opendatahub/odh-mod-arch-agent-ops",
	"core-bff":          "opendatahub/odh-core-bff",
}

// moduleContainers identifies containers that run as standalone Deployments
// in standalone mode (rather than as sidecars in the main dashboard pod).
// In standalone mode each module has its own Deployment named after the container.
var moduleContainers = map[string]bool{
	"model-registry-ui": true,
	"gen-ai-ui":         true,
	"maas-ui":           true,
	"mlflow-ui":         true,
	"eval-hub-ui":       true,
	"automl-ui":         true,
	"autorag-ui":        true,
	"agent-ops-ui":      true,
}

// containerEnvVarMap maps container names to their RELATED_IMAGE env var
// names in the operator deployment (for reverting).
var containerEnvVarMap = map[string]string{
	"rhods-dashboard":   "RELATED_IMAGE_ODH_DASHBOARD_IMAGE",
	"model-registry-ui": "RELATED_IMAGE_ODH_MOD_ARCH_MODEL_REGISTRY_IMAGE",
	"gen-ai-ui":         "RELATED_IMAGE_ODH_MOD_ARCH_GEN_AI_IMAGE",
	"maas-ui":           "RELATED_IMAGE_ODH_MOD_ARCH_MAAS_IMAGE",
	"mlflow-ui":         "RELATED_IMAGE_ODH_MOD_ARCH_MLFLOW_IMAGE",
	"eval-hub-ui":       "RELATED_IMAGE_ODH_MOD_ARCH_EVAL_HUB_IMAGE",
	"automl-ui":         "RELATED_IMAGE_ODH_MOD_ARCH_AUTOML_IMAGE",
	"autorag-ui":        "RELATED_IMAGE_ODH_MOD_ARCH_AUTORAG_IMAGE",
	"agent-ops-ui":      "RELATED_IMAGE_ODH_MOD_ARCH_AGENT_OPS_IMAGE",
	"core-bff":          "RELATED_IMAGE_ODH_CORE_BFF_IMAGE",
}

// ErrDashboardNotDeployed is returned by GetDashboardState when the
// rhods-dashboard Deployment does not exist.
var ErrDashboardNotDeployed = errors.New("RHOAI Dashboard is not deployed")

// DashboardStateError maps a GetDashboardState error to an HTTP status, an
// errorCode and a message, so "not deployed" is not confused with RBAC, API
// or network failures. The not-deployed message keeps the historical
// "failed to get dashboard state" prefix the page matches on.
func DashboardStateError(err error) (status int, code, message string) {
	var netErr net.Error
	switch {
	case errors.Is(err, ErrDashboardNotDeployed):
		return http.StatusNotFound, "dashboard_not_deployed", "failed to get dashboard state: " + err.Error()
	case kube.IsK8sError(err, http.StatusUnauthorized):
		return http.StatusBadGateway, "unauthorized", "Cannot read dashboard state: the Kubernetes API rejected the updater's credentials (HTTP 401)."
	case kube.IsK8sError(err, http.StatusForbidden):
		return http.StatusForbidden, "forbidden", "Cannot read dashboard state: the updater is not allowed to read the rhods-dashboard Deployment (HTTP 403)."
	case kube.IsK8sError(err, http.StatusTooManyRequests):
		return http.StatusTooManyRequests, "rate_limited", "Cannot read dashboard state: the Kubernetes API is throttling requests (HTTP 429). Retry shortly."
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return http.StatusGatewayTimeout, "timeout", "Cannot read dashboard state: the Kubernetes API did not respond in time."
	case kube.IsNetworkError(err):
		return http.StatusServiceUnavailable, "network", "Cannot read dashboard state: the Kubernetes API is unreachable."
	case kube.IsK8sError(err, 0):
		return http.StatusBadGateway, "upstream_error", "Cannot read dashboard state: " + err.Error()
	default:
		return http.StatusInternalServerError, "internal", "Cannot read dashboard state: " + err.Error()
	}
}

// GetDashboardState returns the current state of the rhods-dashboard deployment.
func GetDashboardState(c *Client) (*types.DashboardState, error) {
	ns, err := rhoai.AppsNamespace(c)
	if errors.Is(err, rhoai.ErrNoDSCInitialization) {
		return nil, fmt.Errorf("%w (no DSCInitialization, so RHOAI has no applications namespace)", ErrDashboardNotDeployed)
	}
	if err != nil {
		return nil, fmt.Errorf("find the dashboard: %w", err)
	}
	deployPath := rhoai.NamespacedPath("apps/v1", "deployments", ns, dashboardDeploymentName)
	var deploy struct {
		Metadata struct {
			UID         string            `json:"uid"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			Replicas *int `json:"replicas"`
			Selector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"selector"`
			Template struct {
				Spec struct {
					Containers []struct {
						Name  string `json:"name"`
						Image string `json:"image"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	found, err := c.GetIntoOptional(deployPath, &deploy)
	if err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse dashboard deployment: %w", err)
		}
		return nil, fmt.Errorf("get dashboard deployment: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("%w (no %s Deployment in %s)", ErrDashboardNotDeployed, dashboardDeploymentName, ns)
	}

	state := &types.DashboardState{Namespace: ns}
	target, operator, operatorErr := loadDashboardOperator(c, false)

	// Detect deployment mode from container list
	var containerNames []string
	for _, ct := range deploy.Spec.Template.Spec.Containers {
		containerNames = append(containerNames, ct.Name)
	}
	standalone := isStandaloneMode(containerNames)
	if standalone {
		state.DeploymentMode = "Standalone"
	} else {
		state.DeploymentMode = "Sidecar"
	}

	// Check containers in the main deployment for PR images
	for _, container := range deploy.Spec.Template.Spec.Containers {
		if container.Name == dashboardContainerName {
			state.CurrentImage = container.Image
		}
		if repo, ok := prContainerRepos[container.Name]; ok {
			if kind, n, _ := parseDashboardBuild(repo, container.Image); kind == "pr" {
				state.PRContainers = append(state.PRContainers, container.Name)
				if state.PRNumber == 0 {
					state.PRNumber = n
				}
			}
		}
	}

	// In standalone mode, also check each standalone module deployment
	if standalone && operator == nil {
		for containerName := range moduleContainers {
			repo, ok := prContainerRepos[containerName]
			if !ok {
				continue
			}
			image := getStandaloneModuleImage(c, ns, containerName)
			if image == "" {
				continue
			}
			if kind, n, _ := parseDashboardBuild(repo, image); kind == "pr" {
				state.PRContainers = append(state.PRContainers, containerName)
				if state.PRNumber == 0 {
					state.PRNumber = n
				}
			}
		}
	}

	state.IsCustomPR = len(state.PRContainers) > 0

	// Check managed annotation (nil-safe)
	state.Managed = true
	if deploy.Metadata.Annotations != nil {
		if val, ok := deploy.Metadata.Annotations["opendatahub.io/managed"]; ok {
			state.Managed = val != "false"
		}
	}

	// Get dashboard pods using the deployment's label selector
	dashPods := getDashboardPods(c, ns, dashboardDeploymentName, deploy.Metadata.UID, deploy.Spec.Selector.MatchLabels)
	state.Pods = dashPods

	// Build the expected image for the primary dashboard container from the deployment spec
	specImage := ""
	for _, c := range deploy.Spec.Template.Spec.Containers {
		if c.Name == dashboardContainerName {
			specImage = c.Image
			break
		}
	}

	expectedReplicas := 1
	if deploy.Spec.Replicas != nil {
		expectedReplicas = *deploy.Spec.Replicas
	}

	if len(dashPods) == 0 {
		state.PodStatus = "No pods"
		state.RolloutPending = specImage != ""
	} else {
		allReady := true
		imageMatch := true
		for _, pod := range dashPods {
			if !pod.Ready {
				allReady = false
			}
			if specImage != "" && pod.Image != "" && pod.Image != specImage {
				imageMatch = false
			}
		}

		// Fewer pods than spec.replicas is a rollout that has not
		// finished either: kubectl rollout status waits while
		// status.updatedReplicas < spec.replicas (k8s.io/kubectl
		// pkg/polymorphichelpers/rollout_status.go).
		if !allReady || !imageMatch || len(dashPods) != expectedReplicas {
			state.RolloutPending = true
			for _, pod := range dashPods {
				if !pod.Ready || (specImage != "" && pod.Image != specImage) {
					state.PodStatus = pod.Phase
					state.ContainersTotal = len(pod.Containers)
					if state.ContainersTotal == 0 {
						state.ContainersTotal = len(deploy.Spec.Template.Spec.Containers)
					}
					ready := 0
					for _, ct := range pod.Containers {
						if ct.Ready {
							ready++
						}
					}
					state.ContainersReady = ready
					break
				}
			}
			if state.PodStatus == "" {
				state.PodStatus = "Rolling out"
			}
			state.PodReady = false
		} else {
			// Single pod, ready, image matches spec — truly ready
			pod := dashPods[0]
			state.PodStatus = pod.Phase
			state.PodReady = pod.Ready
			state.ContainersTotal = len(pod.Containers)
			ready := 0
			for _, ct := range pod.Containers {
				if ct.Ready {
					ready++
				}
			}
			state.ContainersReady = ready
		}
	}

	// Check for scheduling failures when rollout is pending
	if state.RolloutPending {
		hasSchedulingFailure := false
		hasReadyPod := false
		for _, pod := range dashPods {
			if pod.SchedulingReason != "" {
				state.SchedulingFailureReason = pod.SchedulingReason
				hasSchedulingFailure = true
			}
			if pod.Ready {
				hasReadyPod = true
			}
		}
		state.CanAssistRollout = hasSchedulingFailure && hasReadyPod
	}

	// The dashboard's address: the Dashboard CR's status.url, else the Route.
	state.DashboardURL = dashboardBaseURL(c)

	populateDashboardDevStateWithOperator(c, state, target, operator, operatorErr)
	return state, nil
}

// DeployPRImageWithFlavor deploys a dashboard PR build of the given flavor
// ("" means the RHOAI build). Clusters without dashboard-operator (RHOAI 2.x)
// use the legacy flow, which only knows the OpenShift CI pr-N builds.
func DeployPRImageWithFlavor(c *Client, prNumber int, flavor string) (*types.OperationResponse, error) {
	if !ValidDashboardFlavor(flavor) {
		return &types.OperationResponse{Success: false, Message: "Unknown dashboard build flavor " + strconv.Quote(flavor) + "; use rhoai or odh.", ErrorCode: "validation"}, nil
	}
	if prNumber <= 0 {
		return &types.OperationResponse{Success: false, Message: "PR number must be greater than 0", ErrorCode: "validation"}, nil
	}
	// Which flow: the dashboard module's operator, or (only when the
	// cluster definitely has no dashboard module) the legacy flow. Unknown
	// refuses: the legacy flow on a module cluster patches what the module
	// operator resets.
	target, err := discoverDashboardTarget(c)
	if err != nil {
		return &types.OperationResponse{Success: false, Message: kube.SentenceStart(err.Error()) + ". No changes were made.", ErrorCode: kube.NonEmpty(kube.ErrorCodeFromK8sErr(err), "prerequisites")}, nil
	}
	if target.Operator != nil {
		return deployDashboardBuild(c, "pr", prNumber, flavor)
	}
	ns := target.Namespace
	if ns == "" {
		return &types.OperationResponse{Success: false, Message: kube.SentenceStart(rhoai.ErrNoDSCInitialization.Error()) + ". No changes were made.", ErrorCode: "prerequisites"}, nil
	}
	logs := []string{"This cluster has no dashboard module (" + target.Legacy + "); the legacy flow patches " + dashboardDeploymentName + " in " + ns + "."}
	if flavor == DashboardFlavorRHOAI {
		logs = append(logs, "The legacy flow deploys the OpenShift CI pr-N (ODH) builds.")
	}

	tag := fmt.Sprintf("pr-%d", prNumber)
	logs = append(logs, fmt.Sprintf("Checking Quay for PR #%d images...", prNumber))

	// Check all repos in parallel
	type checkResult struct {
		container string
		image     string
		status    string // "found", "not built", "error"
	}
	resultCh := make(chan checkResult, len(prContainerRepos))
	for containerName, repo := range prContainerRepos {
		go func(name, r string) {
			image := fmt.Sprintf("quay.io/%s:%s", r, tag)
			manifestURL := fmt.Sprintf("https://quay.io/v2/%s/manifests/%s", r, tag)
			ctx, cancel := context.WithTimeout(c.Context(), 10*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "HEAD", manifestURL, nil)
			if err != nil {
				resultCh <- checkResult{name, "", "error"}
				return
			}
			req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.v2+json,application/vnd.docker.distribution.manifest.list.v2+json")
			resp, err := registry.HTTPClient().Do(req)
			if err != nil {
				resultCh <- checkResult{name, "", "error"}
				return
			}
			resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusOK:
				resultCh <- checkResult{name, image, "found"}
			case http.StatusNotFound:
				resultCh <- checkResult{name, "", "not built"}
			default:
				resultCh <- checkResult{name, "", "error"}
			}
		}(containerName, repo)
	}

	found := map[string]string{}
	var checkErrors int
	for i := 0; i < len(prContainerRepos); i++ {
		r := <-resultCh
		switch r.status {
		case "found":
			found[r.container] = r.image
			logs = append(logs, fmt.Sprintf("  %s: found", r.container))
		case "not built":
			logs = append(logs, fmt.Sprintf("  %s: not built", r.container))
		case "error":
			checkErrors++
			logs = append(logs, fmt.Sprintf("  %s: check failed (network error)", r.container))
		}
	}

	if checkErrors > 0 {
		return &types.OperationResponse{Success: false, Message: "Could not verify all PR images on Quay. Retry after the registry issue is resolved; no images were patched.", Logs: logs, ErrorCode: "network"}, nil
	}
	if len(found) == 0 {
		record.RecordActivity(c, types.ActivityEntry{
			Timestamp: record.NowRFC3339(),
			User:      kube.GetUser(c),
			Action:    "deploy-pr",
			Detail:    fmt.Sprintf("PR #%d (no images found)", prNumber),
			Success:   false,
		})
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("No PR images found for pr-%d on Quay. Ensure the PR CI has completed.", prNumber),
			Logs: logs, ErrorCode: "validation",
		}, nil
	}

	logs = append(logs, fmt.Sprintf("Found %d/%d images", len(found), len(prContainerRepos)))

	// Only containers that exist in this installation are patched; a strategic
	// merge patch would otherwise add a bare container to the pod.
	coreImages, moduleImages, failedModules, err := splitLegacyDashboardTargets(c, ns, found, &logs)
	if err != nil {
		return nil, fmt.Errorf("detect deployment mode: %w", err)
	}
	targets := len(coreImages) + len(moduleImages) + len(failedModules)
	if targets == 0 {
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("No installed dashboard components have an image for pr-%d. No changes were made.", prNumber),
			Logs: logs, ErrorCode: "validation",
		}, nil
	}

	// Patch the main dashboard deployment with core images
	if len(coreImages) > 0 {
		annotations, err := legacyDeployAnnotations(c, ns)
		if err != nil {
			return &types.OperationResponse{
				Success: false, Message: fmt.Sprintf("Cannot read %s before patching: %v. No changes were made.", dashboardDeploymentName, err),
				Logs: logs, ErrorCode: kube.ErrorCodeFromK8sErr(err),
			}, nil
		}
		var containerPatches []map[string]any
		for name, image := range coreImages {
			containerPatches = append(containerPatches, map[string]any{
				"name":  name,
				"image": image,
			})
		}
		patchData, err := json.Marshal(map[string]any{
			"metadata": map[string]any{
				"annotations": annotations,
			},
			"spec": map[string]any{
				"template": map[string]any{
					"spec": map[string]any{
						"containers": containerPatches,
					},
				},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("marshal patch: %w", err)
		}
		deployPath := rhoai.NamespacedPath("apps/v1", "deployments", ns, dashboardDeploymentName)
		_, _, err = c.StrategicPatch(deployPath, patchData)
		if err != nil {
			record.RecordActivity(c, types.ActivityEntry{
				Timestamp: record.NowRFC3339(),
				User:      kube.GetUser(c),
				Action:    "deploy-pr",
				Detail:    fmt.Sprintf("PR #%d (patch failed: %v)", prNumber, err),
				Success:   false,
			})
			return &types.OperationResponse{
				Success: false, Message: fmt.Sprintf("Failed to patch dashboard deployment: %v", err),
				Logs: logs, ErrorCode: kube.ErrorCodeFromK8sErr(err),
			}, nil
		}
	}

	// Patch standalone module deployments
	patched := make(map[string]string, targets)
	maps.Copy(patched, coreImages)
	for name, image := range moduleImages {
		if err := patchStandaloneModule(c, ns, name, image); err != nil {
			slog.Warn("failed to patch standalone module", "module", name, "error", err)
			logs = append(logs, fmt.Sprintf("  %s: patch failed (%v)", name, err))
			failedModules = append(failedModules, name)
		} else {
			patched[name] = image
			logs = append(logs, fmt.Sprintf("  %s: standalone deployment patched", name))
		}
	}

	containerNames := make([]string, 0, len(patched))
	for name := range patched {
		containerNames = append(containerNames, name)
	}
	sort.Strings(containerNames)
	logs = append(logs, fmt.Sprintf("OK: Patched containers: %s", strings.Join(containerNames, ", ")))

	slog.Info("dashboard PR deployed", "pr", prNumber, "containers", len(patched), "user", kube.GetUser(c))

	sort.Strings(failedModules)
	record.RecordActivity(c, types.ActivityEntry{
		Timestamp: record.NowRFC3339(),
		User:      kube.GetUser(c),
		Action:    "deploy-pr",
		Detail:    fmt.Sprintf("PR #%d (%d/%d containers patched; failed modules: %s)", prNumber, len(patched), targets, strings.Join(failedModules, ", ")),
		Success:   len(failedModules) == 0,
	})
	if len(failedModules) > 0 {
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("PR #%d partially deployed: %d/%d containers patched. Failed modules: %s. Retry to finish deployment.", prNumber, len(patched), targets, strings.Join(failedModules, ", ")), Logs: logs, ErrorCode: "partial_failure"}, nil
	}

	return &types.OperationResponse{
		Success: true,
		Message: fmt.Sprintf("PR #%d deployed (%d containers patched).", prNumber, len(patched)),
		Logs:    logs,
	}, nil
}

// splitLegacyDashboardTargets maps container images onto the workloads that
// actually run those containers in this installation. In standalone mode a
// module runs as its own Deployment; otherwise every container lives in the
// main dashboard Deployment. Containers that are not installed are skipped so
// strategic merge patches never add new containers. Modules whose Deployment
// cannot be read are returned as failures.
func splitLegacyDashboardTargets(c *Client, ns string, images map[string]string, logs *[]string) (core, modules map[string]string, failed []string, err error) {
	mainContainers, err := getDeploymentContainerNames(c, ns, dashboardDeploymentName)
	if err != nil {
		return nil, nil, nil, err
	}
	standalone := isStandaloneMode(mainContainers)
	if standalone {
		*logs = append(*logs, "Deployment mode: Standalone")
	} else {
		*logs = append(*logs, "Deployment mode: Sidecar")
	}
	inMain := map[string]bool{}
	for _, name := range mainContainers {
		inMain[name] = true
	}

	names := make([]string, 0, len(images))
	for name := range images {
		names = append(names, name)
	}
	sort.Strings(names)

	core = map[string]string{}
	modules = map[string]string{}
	for _, name := range names {
		if standalone && moduleContainers[name] {
			moduleNames, moduleErr := getDeploymentContainerNames(c, ns, name)
			switch {
			case kube.IsK8sNotFound(moduleErr):
				*logs = append(*logs, fmt.Sprintf("  %s: module not installed; unchanged", name))
			case moduleErr != nil:
				*logs = append(*logs, fmt.Sprintf("  %s: cannot read module deployment (%v)", name, moduleErr))
				failed = append(failed, name)
			case !slices.Contains(moduleNames, name):
				*logs = append(*logs, fmt.Sprintf("  %s: container not found in its deployment; unchanged", name))
			default:
				modules[name] = images[name]
			}
			continue
		}
		if !inMain[name] {
			*logs = append(*logs, fmt.Sprintf("  %s: not part of %s; unchanged", name, dashboardDeploymentName))
			continue
		}
		core[name] = images[name]
	}
	return core, modules, failed, nil
}

// RevertDashboardImage reverts ALL dashboard containers to their
// operator-managed images: it resumes the dashboard module's operator, or,
// only on a cluster without a dashboard module, reverts the legacy flow's
// patches. When the operator cannot be identified it changes nothing.
func RevertDashboardImage(c *Client) (*types.OperationResponse, error) {
	target, err := discoverDashboardTarget(c)
	if err != nil {
		return &types.OperationResponse{Success: false, Message: kube.SentenceStart(err.Error()) + ". No changes were made; retry Revert to default.", ErrorCode: kube.NonEmpty(kube.ErrorCodeFromK8sErr(err), "prerequisites")}, nil
	}
	if target.Operator != nil {
		result, _ := revertDashboardOperatorTo(c, target)
		return result, nil
	}
	ns := target.Namespace
	if ns == "" {
		return &types.OperationResponse{Success: false, Message: kube.SentenceStart(rhoai.ErrNoDSCInitialization.Error()) + ", so there is no dashboard to revert. No changes were made.", ErrorCode: "prerequisites"}, nil
	}
	logs := []string{"This cluster has no dashboard module (" + target.Legacy + "); reverting the legacy flow's changes to " + dashboardDeploymentName + " in " + ns + "."}

	logs = append(logs, "Looking up original images from operator env vars...")
	originalImages, imgErr := getOriginalImages(c)
	if imgErr != nil {
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("Failed to read operator images: %v", imgErr),
			Logs: logs, ErrorCode: "prerequisites",
		}, nil
	}
	logs = append(logs, fmt.Sprintf("Found %d original images", len(originalImages)))

	// Only installed containers are reverted; see splitLegacyDashboardTargets.
	coreImages, moduleImages, failedModules, modeErr := splitLegacyDashboardTargets(c, ns, originalImages, &logs)
	if modeErr != nil {
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("Failed to detect deployment mode: %v", modeErr),
			Logs: logs, ErrorCode: "prerequisites",
		}, nil
	}

	coreNames := make([]string, 0, len(coreImages))
	for name := range coreImages {
		coreNames = append(coreNames, name)
	}
	sort.Strings(coreNames)

	// Revert the main dashboard deployment
	var containerPatches []map[string]any
	for _, name := range coreNames {
		logs = append(logs, fmt.Sprintf("  %s: %s", name, kube.TruncateText(coreImages[name], 80)))
		containerPatches = append(containerPatches, map[string]any{
			"name":  name,
			"image": coreImages[name],
		})
	}

	deployPath := rhoai.NamespacedPath("apps/v1", "deployments", ns, dashboardDeploymentName)
	restored, managedNote, err := legacyRevertAnnotations(c, ns)
	if err != nil {
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("Cannot read %s before reverting: %v", dashboardDeploymentName, err),
			Logs: logs, ErrorCode: kube.ErrorCodeFromK8sErr(err),
		}, nil
	}
	revertPatch := map[string]any{
		"metadata": map[string]any{
			"annotations": restored,
		},
	}
	if len(containerPatches) > 0 {
		revertPatch["spec"] = map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": containerPatches,
				},
			},
		}
	}
	patchData, err := json.Marshal(revertPatch)
	if err != nil {
		return nil, fmt.Errorf("marshal revert patch: %w", err)
	}
	_, _, err = c.StrategicPatch(deployPath, patchData)
	if err != nil {
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("Failed to patch deployment: %v", err),
			Logs: logs, ErrorCode: kube.ErrorCodeFromK8sErr(err),
		}, nil
	}
	logs = append(logs, fmt.Sprintf("OK: %d container images restored", len(containerPatches)))
	switch value, set := restored["opendatahub.io/managed"]; {
	case managedNote != "":
		logs = append(logs, "Warning:"+managedNote)
	case !set:
	case value == nil:
		logs = append(logs, "OK: opendatahub.io/managed removed (it was not set before the PR deployment)")
	default:
		logs = append(logs, fmt.Sprintf("OK: opendatahub.io/managed restored to its original value %q", value))
	}

	// Revert standalone module deployments
	totalReverted := len(containerPatches)
	for name, image := range moduleImages {
		if err := patchStandaloneModule(c, ns, name, image); err != nil {
			slog.Warn("failed to revert standalone module", "module", name, "error", err)
			logs = append(logs, fmt.Sprintf("  %s: revert failed (%v)", name, err))
			failedModules = append(failedModules, name)
		} else {
			totalReverted++
			logs = append(logs, fmt.Sprintf("  %s: %s", name, kube.TruncateText(image, 80)))
		}
	}

	slog.Info("dashboard reverted", "containers", totalReverted, "user", kube.GetUser(c))

	record.RecordActivity(c, types.ActivityEntry{
		Timestamp: record.NowRFC3339(),
		User:      kube.GetUser(c),
		Action:    "revert-dashboard",
		Detail:    fmt.Sprintf("%d containers restored; failed modules: %s", totalReverted, strings.Join(failedModules, ", ")),
		Success:   len(failedModules) == 0,
	})
	if len(failedModules) > 0 {
		sort.Strings(failedModules)
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Dashboard partially restored: %d containers restored. Failed modules: %s. Retry to finish restoring.%s", totalReverted, strings.Join(failedModules, ", "), managedNote), Logs: logs, ErrorCode: "partial_failure"}, nil
	}

	return &types.OperationResponse{
		Success: true,
		Message: fmt.Sprintf("All %d containers restored to operator-managed images.%s", totalReverted, managedNote),
		Logs:    logs,
	}, nil
}

// legacyOriginalManagedAnnotation saves the opendatahub.io/managed value that
// rhods-dashboard had before the first legacy PR deployment set it to "false",
// so revert restores it instead of forcing "true". With "true" the operator
// resets replicas and resources on every reconcile (opendatahub-operator
// v2.25.0 pkg/controller/actions/deploy/action_deploy.go).
const legacyOriginalManagedAnnotation = "rhoai-nightly-updater.opendatahub.io/original-managed"

type legacyManagedRecord struct {
	Present bool   `json:"present"`
	Value   string `json:"value,omitempty"`
}

func getDeploymentAnnotations(c *Client, ns, name string) (map[string]string, error) {
	var d struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := c.GetInto(rhoai.NamespacedPath("apps/v1", "deployments", ns, name), &d); err != nil {
		return nil, err
	}
	return d.Metadata.Annotations, nil
}

// legacyDeployAnnotations returns the annotations a legacy PR deployment sets:
// managed=false and, on the first deployment only, the original value:
// absent, "true" or "false". A "false" the user set before the deployment is
// recorded like any other value, so revert keeps it.
func legacyDeployAnnotations(c *Client, ns string) (map[string]any, error) {
	current, err := getDeploymentAnnotations(c, ns, dashboardDeploymentName)
	if err != nil {
		return nil, err
	}
	annotations := map[string]any{"opendatahub.io/managed": "false"}
	if _, saved := current[legacyOriginalManagedAnnotation]; !saved {
		record := legacyManagedRecord{}
		if value, ok := current["opendatahub.io/managed"]; ok {
			record = legacyManagedRecord{Present: true, Value: value}
		}
		raw, err := json.Marshal(record)
		if err != nil {
			return nil, err
		}
		annotations[legacyOriginalManagedAnnotation] = string(raw)
	}
	return annotations, nil
}

// legacyRevertAnnotations returns the annotation patch that restores
// opendatahub.io/managed to the value saved by legacyDeployAnnotations (null
// removes it) and drops the record. Without a usable record the value before
// the PR deployment is unknown, so opendatahub.io/managed is left as it is,
// and note (otherwise "") tells the user what to check.
func legacyRevertAnnotations(c *Client, ns string) (patch map[string]any, note string, err error) {
	current, err := getDeploymentAnnotations(c, ns, dashboardDeploymentName)
	if err != nil {
		return nil, "", err
	}
	value, managedSet := current["opendatahub.io/managed"]
	raw, saved := current[legacyOriginalManagedAnnotation]
	var record legacyManagedRecord
	switch {
	case saved && json.Unmarshal([]byte(raw), &record) == nil:
		var managed any
		if record.Present {
			managed = record.Value
		}
		return map[string]any{"opendatahub.io/managed": managed, legacyOriginalManagedAnnotation: nil}, "", nil
	case saved:
		// Unreadable record: drop it, keep the current value.
		patch = map[string]any{legacyOriginalManagedAnnotation: nil}
		if managedSet {
			note = fmt.Sprintf(" The saved original value of opendatahub.io/managed on %s could not be read (%s=%q), so its current value %q was left unchanged.", dashboardDeploymentName, legacyOriginalManagedAnnotation, raw, value)
		}
	default:
		patch = map[string]any{}
		if managedSet {
			note = fmt.Sprintf(" No record of the opendatahub.io/managed value %s had before the PR deployment was found, so its current value %q was left unchanged.", dashboardDeploymentName, value)
		}
	}
	if note != "" && value == "false" {
		note += fmt.Sprintf(" While it is \"false\" the operator does not update the dashboard; if this tool set it, remove it with `oc annotate deployment/%s -n %s opendatahub.io/managed-`.", dashboardDeploymentName, ns)
	}
	return patch, note, nil
}

// getOriginalImages reads all dashboard container images from the operator's
// RELATED_IMAGE_* env vars: the Deployment the installed CSV installs in
// its namespace (SubNS), which reconciles the dashboard on releases without
// a dashboard module.
func getOriginalImages(c *Client) (map[string]string, error) {
	csv, err := components.ReadInstalledOperator(c)
	if err != nil {
		return nil, fmt.Errorf("read the installed RHOAI CSV: %w", err)
	}
	if len(csv.Deployments) != 1 {
		return nil, fmt.Errorf("the installed CSV %s installs %d Deployments, so which one holds the dashboard images is unknown", csv.Name, len(csv.Deployments))
	}
	operatorPath := rhoai.NamespacedPath("apps/v1", "deployments", rhoai.SubNS, csv.Deployments[0])
	var deploy struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Env []struct {
							Name  string `json:"name"`
							Value string `json:"value"`
						} `json:"env"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := c.GetInto(operatorPath, &deploy); err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse operator deployment: %w", err)
		}
		return nil, fmt.Errorf("get operator deployment: %w", err)
	}

	// Build env var lookup
	envVars := map[string]string{}
	for _, container := range deploy.Spec.Template.Spec.Containers {
		for _, env := range container.Env {
			if env.Value != "" {
				envVars[env.Name] = env.Value
			}
		}
	}

	// Map container names to their original images
	result := map[string]string{}
	for containerName, envVarName := range containerEnvVarMap {
		if image, ok := envVars[envVarName]; ok {
			result[containerName] = image
		}
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("no RELATED_IMAGE env vars found in operator")
	}

	// Warn about missing containers
	for containerName := range containerEnvVarMap {
		if _, ok := result[containerName]; !ok {
			slog.Warn("no operator env var for container, will not be reverted", "container", containerName, "envVar", containerEnvVarMap[containerName])
		}
	}

	return result, nil
}

// getDashboardPods returns the pods of the Deployment (deploymentUID) that
// match its label selector, with pod.Image set to the rhods-dashboard
// container (not the first container).
func getDashboardPods(c *Client, ns, deployment, deploymentUID string, matchLabels map[string]string) []types.PodInfo {
	// Build label selector query string
	var parts []string
	for k, v := range matchLabels {
		parts = append(parts, k+"="+v)
	}
	selector := strings.Join(parts, ",")

	path := fmt.Sprintf("/api/v1/namespaces/%s/pods?labelSelector=%s", ns, url.QueryEscape(selector))
	var items []struct {
		Metadata struct {
			Name            string            `json:"name"`
			Namespace       string            `json:"namespace"`
			Labels          map[string]string `json:"labels"`
			OwnerReferences []ownerRef        `json:"ownerReferences"`
		} `json:"metadata"`
		Spec struct {
			NodeName   string `json:"nodeName"`
			Containers []struct {
				Name  string `json:"name"`
				Image string `json:"image"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			Phase      string `json:"phase"`
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
			ContainerStatuses []struct {
				Name         string `json:"name"`
				Ready        bool   `json:"ready"`
				RestartCount int    `json:"restartCount"`
				State        struct {
					Running *struct{} `json:"running"`
					Waiting *struct {
						Reason string `json:"reason"`
					} `json:"waiting"`
					Terminated *struct {
						Reason string `json:"reason"`
					} `json:"terminated"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := c.ListInto(path, &items); err != nil {
		if kube.IsDecodeError(err) {
			slog.Warn("failed to parse dashboard pod list", "error", err)
			return nil
		}
		slog.Warn("failed to list dashboard pods", "error", err)
		return nil
	}

	var pods []types.PodInfo
	for _, item := range items {
		// Only the Deployment's own pods: its selector may also match others.
		if !ownedByDeployment(c, ns, deployment, deploymentUID, item.Metadata.Labels, controllerOf(item.Metadata.OwnerReferences)) {
			continue
		}
		pod := types.PodInfo{
			Name:      item.Metadata.Name,
			Namespace: item.Metadata.Namespace,
			Phase:     item.Status.Phase,
			Node:      item.Spec.NodeName,
		}

		// Use the rhods-dashboard container image specifically
		for _, ct := range item.Spec.Containers {
			if ct.Name == dashboardContainerName {
				pod.Image = ct.Image
				break
			}
		}

		allReady := true
		totalRestarts := 0
		if len(item.Status.ContainerStatuses) > 0 {
			for _, cs := range item.Status.ContainerStatuses {
				state := "running"
				reason := ""
				if cs.State.Waiting != nil {
					state = "waiting"
					reason = cs.State.Waiting.Reason
				} else if cs.State.Terminated != nil {
					state = "terminated"
					reason = cs.State.Terminated.Reason
				}
				pod.Containers = append(pod.Containers, types.ContainerInfo{
					Name:     cs.Name,
					Ready:    cs.Ready,
					Restarts: cs.RestartCount,
					State:    state,
					Reason:   reason,
				})
				if !cs.Ready {
					allReady = false
				}
				totalRestarts += cs.RestartCount
			}
		} else {
			// containerStatuses is empty for Pending pods before kubelet reports;
			// fall back to spec containers so the UI shows "0/N" instead of "0/0".
			allReady = false
			for _, ct := range item.Spec.Containers {
				pod.Containers = append(pod.Containers, types.ContainerInfo{
					Name:  ct.Name,
					Ready: false,
					State: "waiting",
				})
			}
		}

		// Extract scheduling failure reason from pod conditions
		for _, cond := range item.Status.Conditions {
			if cond.Type == "PodScheduled" && cond.Status == "False" {
				pod.SchedulingReason = cond.Reason
				pod.SchedulingMessage = cond.Message
				break
			}
		}

		pod.Ready = allReady && len(item.Status.ContainerStatuses) > 0
		pod.Restarts = totalRestarts
		pods = append(pods, pod)
	}

	return pods
}

// isStandaloneMode checks whether the dashboard is running in standalone mode
// by looking at the container list from the main deployment. If no module
// container is present, the cluster is running standalone.
func isStandaloneMode(containers []string) bool {
	for _, name := range containers {
		if moduleContainers[name] {
			return false
		}
	}
	return true
}

// getDeploymentContainerNames returns the list of container names in a deployment.
func getDeploymentContainerNames(c *Client, namespace, deployName string) ([]string, error) {
	path := rhoai.NamespacedPath("apps/v1", "deployments", namespace, deployName)
	var deploy struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name string `json:"name"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := c.GetInto(path, &deploy); err != nil {
		return nil, err
	}
	var names []string
	for _, c := range deploy.Spec.Template.Spec.Containers {
		names = append(names, c.Name)
	}
	return names, nil
}

// getStandaloneModuleImage reads a standalone module deployment and returns
// its container image. Returns empty string if the deployment doesn't exist.
func getStandaloneModuleImage(c *Client, ns, containerName string) string {
	path := rhoai.NamespacedPath("apps/v1", "deployments", ns, containerName)
	body, statusCode, err := c.Get(path)
	if err != nil || statusCode == 404 {
		return ""
	}
	var deploy struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name  string `json:"name"`
						Image string `json:"image"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if json.Unmarshal(body, &deploy) != nil {
		return ""
	}
	for _, ct := range deploy.Spec.Template.Spec.Containers {
		if ct.Name == containerName {
			return ct.Image
		}
	}
	return ""
}

// patchStandaloneModule patches a standalone module deployment's container image.
func patchStandaloneModule(c *Client, ns, containerName, image string) error {
	patchData, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []map[string]any{
						{"name": containerName, "image": image},
					},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("marshal module patch: %w", err)
	}
	path := rhoai.NamespacedPath("apps/v1", "deployments", ns, containerName)
	_, _, err = c.StrategicPatch(path, patchData)
	return err
}
