package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const dashboardOperatorName = "dashboard-operator"
const dashboardDevAnnotation = "rhoai-nightly-updater.opendatahub.io/dashboard-dev"

// dashboardDevLastActionAnnotation records who last deployed or reverted and
// when. It lives next to the session on the dashboard-operator Deployment and,
// unlike the session, survives a revert. The platform controller applies that
// Deployment with server-side apply and never sets these annotations, so it
// leaves them alone.
const dashboardDevLastActionAnnotation = "rhoai-nightly-updater.opendatahub.io/dashboard-dev-last-action"

// platformVersionAnnotation is stamped by the platform deployer on every
// resource it applies (odh-platform-utilities framework
// controller/actions/deploy/action_deploy.go, SuffixVersion = rr.Release.Version).
const platformVersionAnnotation = "platform.opendatahub.io/version"

// dashboardOperatorDefaultReplicas is the dashboard-operator chart default
// (rhods-operator rhoai-3.6 prefetched-charts/dashboard-operator/values.yaml,
// manager.replicas: 1). Revert uses it only when the original count is unknown
// (no or corrupt session annotation) so the operator is never left at 0.
const dashboardOperatorDefaultReplicas = 1

// Dashboard build flavors. The odh-dashboard Konflux pipelines
// (.tekton/odh-dashboard-pull-request.yaml and -push.yaml) build the host image
// with BUILD_MODE=RHOAI (RHOAI logo, product name and docs links) and publish
// odh-pr-<N> (rhoai-konflux-tasks rhoai-init: PR_TAG="odh-pr-${PR_NUMBER}") and
// odh-stable (every push to main). OpenShift CI (openshift/release ci-operator
// config) builds the default BUILD_MODE=ODH and publishes pr-<N> and main.
const (
	DashboardFlavorRHOAI = "rhoai"
	DashboardFlavorODH   = "odh"
)

var dashboardDevMu sync.Mutex
var dashboardDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type dashboardContainer struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	Env   []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"env"`
}

type dashboardDeployment struct {
	Metadata struct {
		Name            string            `json:"name"`
		UID             string            `json:"uid"`
		ResourceVersion string            `json:"resourceVersion"`
		Generation      int64             `json:"generation"`
		Annotations     map[string]string `json:"annotations"`
		OwnerReferences []struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			UID        string `json:"uid"`
			Controller bool   `json:"controller"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Spec struct {
		Replicas *int `json:"replicas"`
		Selector struct {
			MatchLabels map[string]string `json:"matchLabels"`
		} `json:"selector"`
		Template struct {
			Spec struct {
				Containers []dashboardContainer `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration int64 `json:"observedGeneration"`
		UpdatedReplicas    int   `json:"updatedReplicas"`
		ReadyReplicas      int   `json:"readyReplicas"`
		Replicas           int   `json:"replicas"`
		Conditions         []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

// dashboardDevSession is saved as JSON in dashboardDevAnnotation on the
// dashboard-operator Deployment in the same patch that pauses it, so the
// original replica count and the session survive updater restarts.
type dashboardDevSession struct {
	OperatorUID     string                 `json:"operatorUID"`
	Replicas        int                    `json:"replicas"`
	Mode            string                 `json:"mode"`
	PR              int                    `json:"pr,omitempty"`
	Flavor          string                 `json:"flavor,omitempty"`
	Bindings        []dashboardDevBinding  `json:"bindings,omitempty"`
	TargetsRecorded bool                   `json:"targetsRecorded,omitempty"`
	StartedAt       string                 `json:"startedAt,omitempty"`
	StartedBy       string                 `json:"startedBy,omitempty"`
	UpdatedAt       string                 `json:"updatedAt,omitempty"`
	UpdatedBy       string                 `json:"updatedBy,omitempty"`
	ReleaseVersion  string                 `json:"releaseVersion,omitempty"`
	OperatorImage   string                 `json:"operatorImage,omitempty"`
	Baseline        []dashboardDevBaseline `json:"baseline,omitempty"`
}

type dashboardDevBinding struct {
	Deployment  string `json:"deployment"`
	UID         string `json:"uid"`
	Container   string `json:"container"`
	EnvVar      string `json:"envVar"`
	TargetImage string `json:"targetImage,omitempty"`
	Source      string `json:"source,omitempty"`
	Tag         string `json:"tag,omitempty"`
}

// dashboardDevBaseline is a container's release image: Image is what ran when
// the session started (or the operator default if it started later) and
// DefaultImage is the operator's RELATED_IMAGE_* value at that time.
type dashboardDevBaseline struct {
	Deployment   string `json:"deployment"`
	UID          string `json:"uid"`
	Container    string `json:"container"`
	EnvVar       string `json:"envVar"`
	Image        string `json:"image"`
	DefaultImage string `json:"defaultImage"`
}

func readDashboardOperator(c *Client) (*dashboardDeployment, error) {
	body, status, err := c.get(namespacedPath("apps/v1", "deployments", dashboardNamespace, dashboardOperatorName))
	if status == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var d dashboardDeployment
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, err
	}
	if d.Metadata.UID == "" {
		return nil, fmt.Errorf("dashboard-operator deployment has no UID")
	}
	return &d, nil
}

// Environment names define the available dashboard build components. Only the
// two upstream naming exceptions need a map; new MOD_ARCH modules are automatic.
func dashboardBuildRepo(env string) string {
	switch env {
	case "RELATED_IMAGE_ODH_DASHBOARD_IMAGE":
		return "opendatahub/odh-dashboard"
	case "RELATED_IMAGE_ODH_CORE_BFF_IMAGE":
		return "opendatahub/odh-core-bff"
	case "RELATED_IMAGE_ODH_MOD_ARCH_MODEL_REGISTRY_IMAGE":
		return "opendatahub/odh-mod-arch-modular-architecture"
	case "RELATED_IMAGE_ODH_MOD_ARCH_MAAS_IMAGE":
		return "opendatahub/mod-arch-maas"
	}
	if strings.HasPrefix(env, "RELATED_IMAGE_ODH_MOD_ARCH_") && strings.HasSuffix(env, "_IMAGE") {
		slug := strings.TrimSuffix(strings.TrimPrefix(env, "RELATED_IMAGE_ODH_MOD_ARCH_"), "_IMAGE")
		return "opendatahub/odh-mod-arch-" + strings.ToLower(strings.ReplaceAll(slug, "_", "-"))
	}
	return "" // Infrastructure dependencies and the operator image are not dashboard PR/main builds.
}

// ValidDashboardFlavor reports whether flavor is empty (the default) or known.
func ValidDashboardFlavor(flavor string) bool {
	return flavor == "" || flavor == DashboardFlavorRHOAI || flavor == DashboardFlavorODH
}

// dashboardBuildTags lists the tags to try, in order, for a build. The RHOAI
// flavor falls back per component to the OpenShift CI build when Konflux did
// not publish one; the state reports each container's resulting flavor.
func dashboardBuildTags(mode string, pr int, flavor string) []string {
	if mode == "main" {
		if flavor == DashboardFlavorODH {
			return []string{"main"}
		}
		return []string{"odh-stable", "main"}
	}
	if flavor == DashboardFlavorODH {
		return []string{fmt.Sprintf("pr-%d", pr)}
	}
	return []string{fmt.Sprintf("odh-pr-%d", pr), fmt.Sprintf("pr-%d", pr)}
}

// parseDashboardBuild classifies an image of repo as a PR or main build and
// returns its flavor and tag. Other images (release builds) return kind "".
func parseDashboardBuild(repo, image string) (kind string, pr int, flavor, tag string) {
	prefix := "quay.io/" + repo + ":"
	if !strings.HasPrefix(image, prefix) {
		return "", 0, "", ""
	}
	tag = strings.SplitN(strings.TrimPrefix(image, prefix), "@", 2)[0]
	switch {
	case tag == "odh-stable":
		return "main", 0, DashboardFlavorRHOAI, tag
	case tag == "main":
		return "main", 0, DashboardFlavorODH, tag
	case strings.HasPrefix(tag, "odh-pr-"):
		if n, err := strconv.Atoi(strings.TrimPrefix(tag, "odh-pr-")); err == nil && n > 0 {
			return "pr", n, DashboardFlavorRHOAI, tag
		}
	case strings.HasPrefix(tag, "pr-"):
		if n, err := strconv.Atoi(strings.TrimPrefix(tag, "pr-")); err == nil && n > 0 {
			return "pr", n, DashboardFlavorODH, tag
		}
	}
	return "", 0, "", ""
}

func dashboardOwner(d dashboardDeployment) string {
	for _, o := range d.Metadata.OwnerReferences {
		if o.Controller && o.Kind == "Dashboard" && strings.HasPrefix(o.APIVersion, "components.platform.opendatahub.io/") {
			return o.UID
		}
	}
	return ""
}

func listDashboardNamespaceDeployments(c *Client) ([]dashboardDeployment, error) {
	body, _, err := c.get(namespacedPath("apps/v1", "deployments", dashboardNamespace, ""))
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []dashboardDeployment `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func discoverDashboardImages(c *Client, operator *dashboardDeployment) ([]types.DashboardDevImage, error) {
	images, _, err := discoverDashboardWorkloads(c, operator)
	return images, err
}

// discoverDashboardWorkloads returns the dashboard build containers and the
// Deployments that run them, keyed by name.
func discoverDashboardWorkloads(c *Client, operator *dashboardDeployment) ([]types.DashboardDevImage, map[string]dashboardDeployment, error) {
	items, err := listDashboardNamespaceDeployments(c)
	if err != nil {
		return nil, nil, err
	}
	owner := ""
	for _, d := range items {
		if d.Metadata.Name == dashboardDeploymentName {
			owner = dashboardOwner(d)
		}
	}
	if owner == "" {
		return nil, nil, fmt.Errorf("dashboard deployment has no Dashboard controller owner; refusing to patch unverified workloads")
	}
	var images []types.DashboardDevImage
	workloads := map[string]dashboardDeployment{}
	session, err := dashboardSession(operator)
	if err != nil {
		return nil, nil, err
	}
	for _, d := range items {
		if d.Metadata.Name == dashboardOperatorName || dashboardOwner(d) != owner {
			continue
		}
		for _, ct := range d.Spec.Template.Spec.Containers {
			for _, manager := range operator.Spec.Template.Spec.Containers {
				for _, env := range manager.Env {
					repo := dashboardBuildRepo(env.Name)
					if repo == "" || env.Value == "" {
						continue
					}
					name := strings.TrimSuffix(strings.TrimPrefix(env.Name, "RELATED_IMAGE_ODH_MOD_ARCH_"), "_IMAGE")
					expectedName := strings.ToLower(strings.ReplaceAll(name, "_", "-")) + "-ui"
					if env.Name == "RELATED_IMAGE_ODH_DASHBOARD_IMAGE" {
						expectedName = dashboardContainerName
					}
					if env.Name == "RELATED_IMAGE_ODH_CORE_BFF_IMAGE" {
						expectedName = "core-bff"
					}
					savedBinding := false
					if session != nil {
						for _, b := range session.Bindings {
							if b.Deployment == d.Metadata.Name && b.UID == d.Metadata.UID && b.Container == ct.Name && b.EnvVar == env.Name {
								savedBinding = true
							}
						}
					}
					if ct.Image != env.Value && ct.Name != expectedName && !savedBinding {
						continue
					}
					images = append(images, types.DashboardDevImage{Deployment: d.Metadata.Name, Container: ct.Name, EnvVar: env.Name, Repository: repo, CurrentImage: ct.Image, DefaultImage: env.Value, Ready: dashboardDeploymentReady(d), WorkloadUID: d.Metadata.UID, OwnerUID: owner})
					workloads[d.Metadata.Name] = d
				}
			}
		}
	}
	sort.Slice(images, func(i, j int) bool {
		return images[i].Deployment+images[i].Container < images[j].Deployment+images[j].Container
	})
	if len(images) == 0 {
		return nil, nil, fmt.Errorf("no Dashboard-owned containers match dashboard-operator image environment variables")
	}
	return images, workloads, nil
}

func dashboardDeploymentReady(d dashboardDeployment) bool {
	replicas := 1
	if d.Spec.Replicas != nil {
		replicas = *d.Spec.Replicas
	}
	return replicas > 0 && d.Status.ObservedGeneration >= d.Metadata.Generation && d.Status.UpdatedReplicas == replicas && d.Status.ReadyReplicas == replicas && d.Status.Replicas == replicas
}

func dashboardSession(operator *dashboardDeployment) (*dashboardDevSession, error) {
	raw := operator.Metadata.Annotations[dashboardDevAnnotation]
	if raw == "" {
		return nil, nil
	}
	var s dashboardDevSession
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, fmt.Errorf("invalid Dashboard Dev recovery annotation: %w", err)
	}
	if s.OperatorUID != operator.Metadata.UID || s.Replicas <= 0 {
		return nil, fmt.Errorf("Dashboard Dev recovery annotation does not match this operator")
	}
	return &s, nil
}

func dashboardOperatorReplicas(operator *dashboardDeployment) int {
	if operator.Spec.Replicas == nil {
		return 1 // Kubernetes defaults an unset Deployment replica count to 1.
	}
	return *operator.Spec.Replicas
}

func dashboardOperatorImage(operator *dashboardDeployment) string {
	containers := operator.Spec.Template.Spec.Containers
	for _, ct := range containers {
		if ct.Name == "manager" {
			return ct.Image
		}
	}
	if len(containers) > 0 {
		return containers[0].Image
	}
	return ""
}

// dashboardOperatorEnv returns the operator's dashboard build image variables.
func dashboardOperatorEnv(operator *dashboardDeployment) map[string]string {
	env := map[string]string{}
	for _, ct := range operator.Spec.Template.Spec.Containers {
		for _, e := range ct.Env {
			if dashboardBuildRepo(e.Name) != "" && e.Value != "" {
				env[e.Name] = e.Value
			}
		}
	}
	return env
}

// dashboardStaleReasons explains how the installed release differs from the
// one the session started on. The platform controller re-applies the
// dashboard-operator Deployment on every RHOAI update but keeps the live
// replica count (framework MergeDeployments: "the live value always wins" for
// spec.replicas), so a paused operator stays paused while its image and
// RELATED_IMAGE_* values move to the new release.
func dashboardStaleReasons(session *dashboardDevSession, operator *dashboardDeployment) []string {
	var reasons []string
	current := operator.Metadata.Annotations[platformVersionAnnotation]
	if session.ReleaseVersion != "" && current != "" && current != session.ReleaseVersion {
		reasons = append(reasons, fmt.Sprintf("RHOAI version changed from %s to %s", session.ReleaseVersion, current))
	}
	if image := dashboardOperatorImage(operator); session.OperatorImage != "" && image != "" && image != session.OperatorImage {
		reasons = append(reasons, "dashboard-operator image changed (RHOAI was updated)")
	}
	env := dashboardOperatorEnv(operator)
	changed := map[string]bool{}
	for _, b := range session.Baseline {
		if value, ok := env[b.EnvVar]; ok && b.DefaultImage != "" && value != b.DefaultImage {
			changed[b.EnvVar] = true
		}
	}
	if len(changed) > 0 {
		reasons = append(reasons, fmt.Sprintf("release images changed for %d dashboard components", len(changed)))
	}
	return reasons
}

func dashboardLastAction(operator *dashboardDeployment) *types.DashboardDevAction {
	raw := operator.Metadata.Annotations[dashboardDevLastActionAnnotation]
	if raw == "" {
		return nil
	}
	var action types.DashboardDevAction
	if err := json.Unmarshal([]byte(raw), &action); err != nil || action.Action == "" {
		return nil
	}
	return &action
}

// DashboardOverrideSummary reports whether the dashboard-operator is paused
// (by Dashboard Dev or otherwise), by whom and since when, what each container
// was set to run, and whether RHOAI was updated since. It reads only the
// dashboard-operator Deployment. It returns nil when that Deployment does not
// exist (RHOAI 2.x or not installed).
func DashboardOverrideSummary(c *Client) (*types.DashboardOverride, error) {
	operator, err := readDashboardOperator(c)
	if err != nil || operator == nil {
		return nil, err
	}
	o := dashboardOverrideFromOperator(operator)
	addDashboardDeletionWarning(c, o)
	return o, nil
}

const (
	dashboardStaleWarning         = "RHOAI was updated while dashboard-operator is paused. The dashboard keeps its current images, and the Dashboard CR still reports the old release, so RHOAI's later upgrade stages (runlevels 31 and 32: kserve, mlflow, feast, ogx, trustyai) wait up to 10 minutes after each rhods-operator start before continuing. Revert to default to apply the new release."
	dashboardDeletingWarning      = "A Dashboard CR is being deleted while dashboard-operator is paused. Only dashboard-operator removes its finalizer (components.platform.opendatahub.io/cleanup), so the deletion, and any dashboard removal or RHOAI uninstall waiting on it, is stuck. Revert to default to resume dashboard-operator; the deletion then completes."
	dashboardExternalPauseWarning = "dashboard-operator is paused outside Dashboard Dev, so the dashboard does not follow RHOAI updates. Revert to default resumes it with the chart default of 1 replica."
)

// dashboardCRDeleting reports whether any Dashboard CR has a deletionTimestamp.
// A missing CRD means there is nothing to delete.
func dashboardCRDeleting(c *Client) (bool, error) {
	body, _, err := c.get(clusterPath("components.platform.opendatahub.io/v1alpha1", "dashboards", ""))
	if IsK8sError(err, http.StatusNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				DeletionTimestamp string `json:"deletionTimestamp"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return false, err
	}
	for _, item := range list.Items {
		if item.Metadata.DeletionTimestamp != "" {
			return true, nil
		}
	}
	return false, nil
}

// addDashboardDeletionWarning flags deadlock D1 (see RHOAI operator notes):
// a Dashboard CR deletion waiting on a paused dashboard-operator. It costs a
// read only while the operator is paused.
func addDashboardDeletionWarning(c *Client, o *types.DashboardOverride) {
	if o == nil || !o.OperatorPaused || c == nil {
		return
	}
	deleting, err := dashboardCRDeleting(c)
	if err != nil {
		slog.Warn("cannot check Dashboard CR deletion", "error", err)
		return
	}
	if deleting {
		o.DashboardDeleting = true
		o.Warnings = append([]string{dashboardDeletingWarning}, o.Warnings...)
	}
}

func dashboardOverrideFromOperator(operator *dashboardDeployment) *types.DashboardOverride {
	o := &types.DashboardOverride{
		OperatorPaused: dashboardOperatorReplicas(operator) == 0,
		ReleaseVersion: operator.Metadata.Annotations[platformVersionAnnotation],
		LastAction:     dashboardLastAction(operator),
	}
	session, err := dashboardSession(operator)
	if err != nil {
		o.SessionError = err.Error()
	}
	if session != nil {
		o.SessionRecorded = true
		o.Mode = session.Mode
		o.PRNumber = session.PR
		o.Flavor = session.Flavor
		o.StartedAt = session.StartedAt
		o.StartedBy = session.StartedBy
		o.UpdatedAt = session.UpdatedAt
		o.UpdatedBy = session.UpdatedBy
		o.ReleaseVersionAtStart = session.ReleaseVersion
		o.StaleReasons = dashboardStaleReasons(session, operator)
		o.Stale = len(o.StaleReasons) > 0
		for _, b := range session.Bindings {
			o.Components = append(o.Components, types.DashboardOverrideComponent{Deployment: b.Deployment, Container: b.Container, Image: b.TargetImage, Source: b.Source, Tag: b.Tag})
		}
	}
	o.Active = o.OperatorPaused || session != nil || err != nil
	if o.Stale && o.OperatorPaused {
		o.Warnings = append(o.Warnings, dashboardStaleWarning)
	}
	if o.OperatorPaused && session == nil {
		o.Warnings = append(o.Warnings, dashboardExternalPauseWarning)
	}
	return o
}

func populateDashboardDevStateWithOperator(c *Client, state *types.DashboardState, operator *dashboardDeployment, err error) {
	state.DefaultFlavor = DashboardFlavorRHOAI
	state.AvailableFlavors = []string{DashboardFlavorRHOAI, DashboardFlavorODH}
	if err != nil {
		state.OperatorError = err.Error()
		return
	}
	if operator == nil {
		state.IsDevMode = state.IsCustomPR
		state.AllDevImagesReady = state.PodReady && !state.RolloutPending
		state.DevImagesMatchTarget = true
		return
	}
	state.OperatorAvailable = true
	state.OperatorPaused = dashboardOperatorReplicas(operator) == 0
	state.Managed = !state.OperatorPaused
	state.Override = dashboardOverrideFromOperator(operator)
	addDashboardDeletionWarning(c, state.Override)
	session, err := dashboardSession(operator)
	if err != nil {
		state.OperatorError = err.Error()
		return
	}
	if session != nil {
		state.IsDevMode = true
		state.DevMode = session.Mode
		state.PRNumber = session.PR
		state.Flavor = session.Flavor
	}
	images, workloads, err := discoverDashboardWorkloads(c, operator)
	if err != nil {
		state.OperatorError = err.Error()
		return
	}
	state.AllDevImagesReady = true
	state.DevImagesMatchTarget = true
	state.DefaultImagesRestored = true
	state.PRContainers = nil
	state.DeploymentMode = "Standalone"
	matchedBindings := map[string]bool{}
	for i := range images {
		image := &images[i]
		image.MatchesTarget = true
		if session != nil {
			for _, binding := range session.Bindings {
				if binding.Deployment == image.Deployment && binding.Container == image.Container {
					image.TargetImage = binding.TargetImage
					image.TargetSource = binding.Source
					image.TargetTag = binding.Tag
					if binding.TargetImage != "" {
						image.MatchesTarget = binding.UID == image.WorkloadUID && image.CurrentImage == binding.TargetImage
						matchedBindings[binding.Deployment+"/"+binding.Container] = true
					}
				}
			}
			for _, b := range session.Baseline {
				if b.Deployment == image.Deployment && b.Container == image.Container {
					image.BaselineImage = b.Image
				}
			}
			// Sessions created before target digests were saved still need to
			// distinguish a release host from successfully patched main modules.
			if !session.TargetsRecorded && session.Mode == "main" {
				image.TargetImage = "quay.io/" + image.Repository + ":main"
				image.MatchesTarget = image.CurrentImage == image.TargetImage || strings.HasPrefix(image.CurrentImage, image.TargetImage+"@")
			}
			if session.TargetsRecorded && session.Mode == "main" && image.TargetImage == "" {
				image.MatchesTarget = false
			}
		}
		if !image.MatchesTarget {
			state.DevImagesMatchTarget = false
		}
		if image.Deployment == dashboardDeploymentName && strings.HasPrefix(image.EnvVar, "RELATED_IMAGE_ODH_MOD_ARCH_") {
			state.DeploymentMode = "Sidecar"
		}
		if !image.Ready {
			state.AllDevImagesReady = false
		}
		if image.CurrentImage != image.DefaultImage {
			state.DefaultImagesRestored = false
		}
		kind, n, flavor, _ := parseDashboardBuild(image.Repository, image.CurrentImage)
		switch {
		case image.CurrentImage == image.DefaultImage:
			image.Running = "release"
		case kind != "":
			image.Running = kind
			image.RunningPR = n
			image.Flavor = flavor
		default:
			image.Running = "other"
		}
		if kind == "main" && image.CurrentImage != image.DefaultImage {
			state.IsDevMode = true
			if state.DevMode == "" {
				state.DevMode = "main"
			}
		}
		if kind == "pr" {
			state.PRContainers = append(state.PRContainers, image.Container)
			if session == nil {
				state.PRNumber = n
				state.DevMode = "pr"
				state.IsDevMode = true
			}
		}
	}
	state.IsCustomPR = len(state.PRContainers) > 0
	if session != nil && session.TargetsRecorded {
		for _, binding := range session.Bindings {
			if binding.TargetImage != "" && !matchedBindings[binding.Deployment+"/"+binding.Container] {
				state.DevImagesMatchTarget = false
			}
		}
	}
	if state.DefaultImagesRestored && session == nil {
		state.IsDevMode = false
		state.DevMode = ""
	}
	addDashboardRolloutDiagnostics(c, images, workloads)
	for _, image := range images {
		if image.RolloutStuck && !state.RolloutStuck {
			state.RolloutStuck = true
			state.StuckReason = image.Deployment + "/" + image.Container + ": " + image.RolloutMessage
			if image.WaitingReason != "" {
				state.StuckReason += " (" + image.WaitingReason + ")"
			}
		}
	}
	state.DevImages = images
}

type dashboardPod struct {
	Metadata struct {
		Name              string            `json:"name"`
		Labels            map[string]string `json:"labels"`
		CreationTimestamp string            `json:"creationTimestamp"`
		DeletionTimestamp string            `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec struct {
		Containers []struct {
			Name  string `json:"name"`
			Image string `json:"image"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
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
				Waiting *struct {
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"waiting"`
				Terminated *struct {
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"terminated"`
			} `json:"state"`
			LastState struct {
				Terminated *struct {
					Reason string `json:"reason"`
				} `json:"terminated"`
			} `json:"lastState"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

// addDashboardRolloutDiagnostics reports why a workload is not ready. The
// stuck signal is the Deployment's own Progressing condition: once
// spec.progressDeadlineSeconds (default 600) passes without progress, the
// deployment controller sets Progressing=False with reason
// ProgressDeadlineExceeded (kubernetes.io/docs/concepts/workloads/controllers/deployment/#failed-deployment).
// It is server-side state, so it survives page reloads and updater restarts.
func addDashboardRolloutDiagnostics(c *Client, images []types.DashboardDevImage, workloads map[string]dashboardDeployment) {
	needPods := false
	for i := range images {
		d := workloads[images[i].Deployment]
		for _, cond := range d.Status.Conditions {
			if cond.Type == "Progressing" && cond.Status == "False" && cond.Reason == "ProgressDeadlineExceeded" {
				images[i].RolloutStuck = true
				images[i].RolloutMessage = cond.Message
				if images[i].RolloutMessage == "" {
					images[i].RolloutMessage = cond.Reason
				}
			}
		}
		if !images[i].Ready {
			needPods = true
		}
	}
	if !needPods || c == nil {
		return
	}
	// One namespace-wide list, only while something is not ready.
	body, _, err := c.get("/api/v1/namespaces/" + dashboardNamespace + "/pods")
	if err != nil {
		slog.Warn("cannot list dashboard pods for rollout diagnostics", "error", err)
		return
	}
	var pods struct {
		Items []dashboardPod `json:"items"`
	}
	if err := json.Unmarshal(body, &pods); err != nil {
		slog.Warn("cannot parse dashboard pods for rollout diagnostics", "error", err)
		return
	}
	for i := range images {
		if images[i].Ready {
			continue
		}
		d := workloads[images[i].Deployment]
		if len(d.Spec.Selector.MatchLabels) == 0 {
			continue
		}
		var best *dashboardPod
		bestScore := -1
		for p := range pods.Items {
			pod := &pods.Items[p]
			if pod.Metadata.DeletionTimestamp != "" || !dashboardLabelsMatch(d.Spec.Selector.MatchLabels, pod.Metadata.Labels) {
				continue
			}
			score := 0
			for _, ct := range pod.Spec.Containers {
				if ct.Name == images[i].Container && ct.Image == images[i].CurrentImage {
					score += 2 // Pod of the newest template.
				}
			}
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.Name == images[i].Container && !cs.Ready {
					score++
				}
			}
			if score > bestScore || (score == bestScore && best != nil && pod.Metadata.CreationTimestamp > best.Metadata.CreationTimestamp) {
				best, bestScore = pod, score
			}
		}
		if best == nil {
			continue
		}
		images[i].PodName = best.Metadata.Name
		for _, cs := range best.Status.ContainerStatuses {
			if cs.Name != images[i].Container {
				continue
			}
			images[i].Restarts = cs.RestartCount
			switch {
			case cs.State.Waiting != nil:
				images[i].WaitingReason = cs.State.Waiting.Reason
				images[i].WaitingMessage = cs.State.Waiting.Message
				if cs.State.Waiting.Reason == "CrashLoopBackOff" && cs.LastState.Terminated != nil && cs.LastState.Terminated.Reason != "" {
					images[i].WaitingMessage = "last exit: " + cs.LastState.Terminated.Reason
				}
			case cs.State.Terminated != nil:
				images[i].WaitingReason = cs.State.Terminated.Reason
				images[i].WaitingMessage = cs.State.Terminated.Message
			}
		}
		if images[i].WaitingReason == "" {
			for _, cond := range best.Status.Conditions {
				if cond.Type == "PodScheduled" && cond.Status == "False" {
					images[i].WaitingReason = cond.Reason
					images[i].WaitingMessage = cond.Message
				}
			}
		}
	}
}

func dashboardLabelsMatch(selector, labels map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// patchDashboardOperator sets the replica count, the session annotation (nil
// removes it) and the last-action record in one merge patch. uid and
// resourceVersion make it fail with a conflict instead of acting on a stale read.
func patchDashboardOperator(c *Client, operator *dashboardDeployment, replicas int, session *dashboardDevSession, action *types.DashboardDevAction) error {
	var annotation interface{}
	if session != nil {
		raw, err := json.Marshal(session)
		if err != nil {
			return err
		}
		annotation = string(raw)
	}
	annotations := map[string]interface{}{dashboardDevAnnotation: annotation}
	if action != nil {
		raw, err := json.Marshal(action)
		if err != nil {
			return err
		}
		annotations[dashboardDevLastActionAnnotation] = string(raw)
	}
	data, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"uid": operator.Metadata.UID, "resourceVersion": operator.Metadata.ResourceVersion, "annotations": annotations},
		"spec":     map[string]interface{}{"replicas": replicas},
	})
	if err != nil {
		return err
	}
	_, _, err = c.patch(namespacedPath("apps/v1", "deployments", dashboardNamespace, dashboardOperatorName), data)
	return err
}

func waitDashboardOperatorStopped(c *Client, operator *dashboardDeployment) error {
	if len(operator.Spec.Selector.MatchLabels) == 0 {
		return fmt.Errorf("dashboard-operator has no pod selector")
	}
	var selector []string
	for k, v := range operator.Spec.Selector.MatchLabels {
		selector = append(selector, k+"="+v)
	}
	sort.Strings(selector)
	ctx, cancel := context.WithTimeout(c.ctx, 60*time.Second)
	defer cancel()
	reader := *c
	reader.ctx = ctx
	for {
		current, err := readDashboardOperator(&reader)
		if err != nil {
			return err
		}
		if current == nil || current.Metadata.UID != operator.Metadata.UID || current.Spec.Replicas == nil || *current.Spec.Replicas != 0 {
			return fmt.Errorf("dashboard-operator was replaced or resumed while pausing")
		}
		body, _, err := reader.get("/api/v1/namespaces/" + dashboardNamespace + "/pods?labelSelector=" + url.QueryEscape(strings.Join(selector, ",")))
		if err != nil {
			return err
		}
		var pods struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(body, &pods); err != nil {
			return err
		}
		if pods.Items == nil {
			return fmt.Errorf("dashboard-operator pod list is missing items; cannot confirm the controller has stopped")
		}
		if len(pods.Items) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for dashboard-operator pods to terminate: %w", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// Resolve the current mutable tag to a digest so clicking Deploy again picks up
// a new build and retries cannot silently switch builds during rollout.
func resolveDashboardBuild(ctx context.Context, repo, tag string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "HEAD", "https://quay.io/v2/"+repo+"/manifests/"+tag, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json,application/vnd.docker.distribution.manifest.list.v2+json")
	resp, err := quayHTTPClient.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("Quay returned HTTP %d for %s:%s", resp.StatusCode, repo, tag)
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if !dashboardDigestPattern.MatchString(digest) {
		return "", false, fmt.Errorf("Quay returned an invalid image digest for %s:%s", repo, tag)
	}
	return "quay.io/" + repo + ":" + tag + "@" + digest, true, nil
}

// resolveDashboardBuildTags returns the first published tag of tags.
func resolveDashboardBuildTags(ctx context.Context, repo string, tags []string) (image, tag string, found bool, err error) {
	for _, tag := range tags {
		image, found, err := resolveDashboardBuild(ctx, repo, tag)
		if err != nil || found {
			return image, tag, found, err
		}
	}
	return "", "", false, nil
}

// DeployDashboardMainWithFlavor deploys the latest main build of the given
// flavor ("" means the RHOAI build).
func DeployDashboardMainWithFlavor(c *Client, flavor string) (*types.OperationResponse, error) {
	return deployDashboardBuild(c, "main", 0, flavor)
}

func dashboardFlavorLabel(flavor string) string {
	if flavor == DashboardFlavorODH {
		return "ODH build"
	}
	return "RHOAI build"
}

// dashboardBaselineImage is the image a container runs when the target build
// has none for it: the image saved when the session started, unless RHOAI was
// updated since (its default changed) or the workload was replaced, in which
// case the operator's current default, which is also what Revert converges to.
func dashboardBaselineImage(session *dashboardDevSession, image types.DashboardDevImage) string {
	for _, b := range session.Baseline {
		if b.Deployment == image.Deployment && b.Container == image.Container && b.UID == image.WorkloadUID && b.DefaultImage == image.DefaultImage && b.Image != "" {
			return b.Image
		}
	}
	return image.DefaultImage
}

func deployDashboardBuild(c *Client, mode string, pr int, flavor string) (*types.OperationResponse, error) {
	if flavor == "" {
		flavor = DashboardFlavorRHOAI
	}
	logs := []string{}
	fail := func(message, code string) (*types.OperationResponse, error) {
		return &types.OperationResponse{Success: false, Message: message, ErrorCode: code, Logs: logs}, nil
	}
	if !ValidDashboardFlavor(flavor) {
		return fail("Unknown dashboard build flavor "+strconv.Quote(flavor)+"; use rhoai or odh.", "validation")
	}
	dashboardDevMu.Lock()
	defer dashboardDevMu.Unlock()
	operator, err := readDashboardOperator(c)
	if err != nil {
		return fail("Cannot read dashboard-operator: "+err.Error(), "prerequisites")
	}
	if operator == nil {
		return fail("Latest-main deployment requires dashboard-operator.", "prerequisites")
	}
	session, err := dashboardSession(operator)
	if err != nil {
		return fail(err.Error()+". Click Revert to default to restore operator management, then deploy again.", "prerequisites")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	user := getUser(c)
	newSession := session == nil
	if newSession {
		replicas := dashboardOperatorReplicas(operator)
		if replicas <= 0 {
			return fail("dashboard-operator is already paused outside Dashboard Dev. Click Revert to default to resume it before starting a test session.", "prerequisites")
		}
		session = &dashboardDevSession{OperatorUID: operator.Metadata.UID, Replicas: replicas, StartedAt: now, StartedBy: user}
	}
	// Pausing while a Dashboard CR deletion is pending would block that
	// deletion on the finalizer only dashboard-operator removes (D1).
	if deleting, err := dashboardCRDeleting(c); err != nil {
		return fail("Cannot check whether the Dashboard is being deleted: "+err.Error()+". No changes were made.", "prerequisites")
	} else if deleting {
		return fail("The Dashboard CR is being deleted (dashboard removal or RHOAI uninstall in progress). Pausing dashboard-operator now would block that deletion. No changes were made.", "prerequisites")
	}
	images, err := discoverDashboardImages(c, operator)
	if err != nil {
		return fail(err.Error(), "prerequisites")
	}
	tags := dashboardBuildTags(mode, pr, flavor)
	label := "latest main"
	if mode == "pr" {
		label = fmt.Sprintf("PR #%d", pr)
	}
	label += " (" + dashboardFlavorLabel(flavor) + ")"
	type checked struct {
		i     int
		image string
		tag   string
		found bool
		err   error
	}
	results := make(chan checked, len(images))
	for i, image := range images {
		go func(i int, repo string) {
			image, tag, found, err := resolveDashboardBuildTags(c.ctx, repo, tags)
			results <- checked{i, image, tag, found, err}
		}(i, image.Repository)
	}
	resolved := make([]checked, len(images))
	for range images {
		r := <-results
		resolved[r.i] = r
	}
	// Baselines: the images running before the first pause, refreshed for
	// components whose release default changed since.
	var baseline []dashboardDevBaseline
	for _, image := range images {
		original := image.CurrentImage
		if !newSession {
			original = dashboardBaselineImage(session, image)
		}
		baseline = append(baseline, dashboardDevBaseline{Deployment: image.Deployment, UID: image.WorkloadUID, Container: image.Container, EnvVar: image.EnvVar, Image: original, DefaultImage: image.DefaultImage})
	}
	session.Baseline = baseline
	// Every container gets an explicit target so switching builds never leaves
	// a container on the previous session's build.
	var bindings []dashboardDevBinding
	built := 0
	for i, r := range resolved {
		image := images[i]
		if r.err != nil {
			return fail("Cannot verify "+image.Repository+": "+r.err.Error()+". No changes were made.", "network")
		}
		binding := dashboardDevBinding{Deployment: image.Deployment, UID: image.WorkloadUID, Container: image.Container, EnvVar: image.EnvVar}
		if r.found {
			built++
			binding.TargetImage, binding.Source, binding.Tag = r.image, mode, r.tag
		} else {
			if mode == "main" {
				return fail("Missing main image for "+image.Repository+" (tags "+strings.Join(tags, ", ")+"). No changes were made.", "validation")
			}
			binding.TargetImage, binding.Source = baseline[i].Image, "baseline"
		}
		bindings = append(bindings, binding)
	}
	if built == 0 {
		return fail("No installed dashboard components have an image for "+strings.Join(tags, " or ")+". Ensure CI has published the PR images.", "validation")
	}
	session.Mode = mode
	session.PR = pr
	session.Flavor = flavor
	session.Bindings = bindings
	session.TargetsRecorded = true
	session.UpdatedAt = now
	session.UpdatedBy = user
	session.ReleaseVersion = operator.Metadata.Annotations[platformVersionAnnotation]
	session.OperatorImage = dashboardOperatorImage(operator)
	action := &types.DashboardDevAction{Action: "deploy-" + mode, By: user, At: now, Detail: "Deploying " + label}
	if err := patchDashboardOperator(c, operator, 0, session, action); err != nil {
		return fail("Could not pause dashboard-operator: "+err.Error(), errorCodeFromK8sErr(err))
	}
	logs = append(logs, "Dashboard Dev recovery information saved; dashboard-operator scaled to 0.")
	if err := waitDashboardOperatorStopped(c, operator); err != nil {
		return fail(err.Error()+". No dashboard images were patched. Click Revert to default to resume the operator.", "prerequisites")
	}
	var failures []string
	groups := map[string][]dashboardImagePatch{}
	var deployments []string
	for i, binding := range bindings {
		image := images[i]
		if _, ok := groups[image.Deployment]; !ok {
			deployments = append(deployments, image.Deployment)
		}
		groups[image.Deployment] = append(groups[image.Deployment], dashboardImagePatch{Image: image, Target: binding.TargetImage, Source: binding.Source})
	}
	for _, deployment := range deployments {
		group := groups[deployment]
		if err := patchControlledDashboardImages(c, operator, group); err != nil {
			failure := deployment + ": " + err.Error()
			failures = append(failures, failure)
			logs = append(logs, "Failed: "+failure)
			continue
		}
		for _, patch := range group {
			note := ""
			if patch.Source == "baseline" {
				note = " (no build for " + strings.Join(tags, "/") + "; release image)"
			}
			logs = append(logs, patch.Image.Deployment+"/"+patch.Image.Container+": "+patch.Target+note)
		}
	}
	success := len(failures) == 0
	message := fmt.Sprintf("Deployed %s: %d of %d dashboard containers run the new build", label, built, len(images))
	if built < len(images) {
		message += fmt.Sprintf("; the other %d have no build for it and run the release image", len(images)-built)
	}
	message += ". Dashboard-operator is paused; revert after testing."
	if !success {
		retry := "Deploy latest main"
		if mode == "pr" {
			retry = fmt.Sprintf("Deploy PR #%d", pr)
		}
		message = fmt.Sprintf("Partial deployment of %s: %d deployment(s) failed. Retry %s to finish, or Revert to default to resume operator reconciliation.", label, len(failures), retry)
	}
	RecordActivity(c, types.ActivityEntry{Timestamp: now, User: user, Action: "deploy-dashboard-" + mode, Detail: message, Success: success})
	code := ""
	if !success {
		code = "partial_failure"
	}
	return &types.OperationResponse{Success: success, Message: message, Logs: logs, ErrorCode: code}, nil
}

type dashboardImagePatch struct {
	Image  types.DashboardDevImage
	Target string
	Source string
}

// Patch all selected containers in a deployment atomically. Status changes can
// advance resourceVersion during rollout; retry only conflicts and repeat all
// ownership/image checks on each fresh read.
func patchControlledDashboardImages(c *Client, operator *dashboardDeployment, patches []dashboardImagePatch) error {
	if len(patches) == 0 {
		return nil
	}
	image := patches[0].Image
	path := namespacedPath("apps/v1", "deployments", dashboardNamespace, image.Deployment)
	for attempt := 0; attempt < 5; attempt++ {
		current, err := readDashboardOperator(c)
		if err != nil {
			return err
		}
		if current == nil || current.Metadata.UID != operator.Metadata.UID || current.Spec.Replicas == nil || *current.Spec.Replicas != 0 {
			return fmt.Errorf("dashboard-operator is no longer paused")
		}
		body, _, err := c.get(path)
		if err != nil {
			return err
		}
		var d dashboardDeployment
		if err := json.Unmarshal(body, &d); err != nil {
			return err
		}
		var containers []map[string]string
		for _, patch := range patches {
			if patch.Image.Deployment != image.Deployment || d.Metadata.UID != patch.Image.WorkloadUID || dashboardOwner(d) != patch.Image.OwnerUID {
				return fmt.Errorf("workload was replaced or is no longer owned by the same Dashboard")
			}
			matched := false
			for _, ct := range d.Spec.Template.Spec.Containers {
				if ct.Name != patch.Image.Container {
					continue
				}
				matched = true
				if ct.Image != patch.Image.CurrentImage && ct.Image != patch.Target {
					return fmt.Errorf("%s image changed during deployment; refusing to overwrite it", ct.Name)
				}
				if ct.Image != patch.Target {
					containers = append(containers, map[string]string{"name": ct.Name, "image": patch.Target})
				}
			}
			if !matched {
				return fmt.Errorf("container %s no longer exists", patch.Image.Container)
			}
		}
		if len(containers) == 0 {
			return nil
		}
		data, _ := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{"uid": d.Metadata.UID, "resourceVersion": d.Metadata.ResourceVersion}, "spec": map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{"containers": containers}}}})
		_, _, err = c.strategicPatch(path, data)
		if err == nil {
			return nil
		}
		if !IsK8sError(err, http.StatusConflict) || attempt == 4 {
			return err
		}
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 100 * time.Millisecond):
		}
	}
	return fmt.Errorf("dashboard image patch exhausted conflict retries")
}

// clearDashboardManagedAnnotations removes opendatahub.io/managed from every
// Dashboard-owned workload that has it. The old PR flow set it to "false",
// which makes the dashboard-operator skip the workload (odh-platform-utilities
// pkg/deploy: managed=false is a create-only opt-out). It needs no session, so
// it also works after the session annotation was lost or the Dashboard removed.
func clearDashboardManagedAnnotations(c *Client) (cleared []string, failures []string) {
	items, err := listDashboardNamespaceDeployments(c)
	if err != nil {
		return nil, []string{"list dashboard workloads: " + err.Error()}
	}
	for _, d := range items {
		if d.Metadata.Name == dashboardOperatorName || dashboardOwner(d) == "" {
			continue
		}
		if _, ok := d.Metadata.Annotations["opendatahub.io/managed"]; !ok {
			continue
		}
		data, _ := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{"uid": d.Metadata.UID, "annotations": map[string]interface{}{"opendatahub.io/managed": nil}}})
		if _, _, err := c.patch(namespacedPath("apps/v1", "deployments", dashboardNamespace, d.Metadata.Name), data); err != nil && !IsK8sError(err, http.StatusNotFound) {
			failures = append(failures, d.Metadata.Name+": "+err.Error())
			continue
		}
		cleared = append(cleared, d.Metadata.Name)
	}
	return cleared, failures
}

// revertDashboardOperator returns the dashboard to operator management from
// any state: with or without a session, with a corrupt session, after RHOAI
// was updated, and after a partial deploy. Resuming the operator is enough:
// the dashboard-operator applies its manifests with server-side apply and
// ForceOwnership (odh-platform-utilities pkg/deploy), which takes the image
// fields back and rolls every workload to the installed release. Repeating it
// is safe.
func revertDashboardOperator(c *Client, _ *dashboardDeployment) (*types.OperationResponse, error) {
	result, _ := revertDashboardOperatorTo(c)
	return result, nil
}

// revertDashboardOperatorTo also returns the replica count it resumed the
// operator to (0 when it could not resume it).
func revertDashboardOperatorTo(c *Client) (*types.OperationResponse, int) {
	dashboardDevMu.Lock()
	defer dashboardDevMu.Unlock()
	var logs []string
	fail := func(message, code string) (*types.OperationResponse, int) {
		return &types.OperationResponse{Success: false, Message: message, ErrorCode: code, Logs: logs}, 0
	}
	cleared, failures := clearDashboardManagedAnnotations(c)
	for _, name := range cleared {
		logs = append(logs, name+": removed opendatahub.io/managed so the operator manages it again")
	}
	for attempt := 0; ; attempt++ {
		// Re-read after acquiring the shared deploy/revert lock and on conflicts.
		operator, err := readDashboardOperator(c)
		if err != nil {
			code := errorCodeFromK8sErr(err)
			if code == "" {
				code = "prerequisites"
			}
			return fail("Cannot read dashboard-operator: "+err.Error()+". Retry Revert to default.", code)
		}
		if operator == nil {
			return fail("dashboard-operator Deployment not found. The RHOAI platform recreates it with its default replica count; retry Revert to default once it exists.", "prerequisites")
		}
		session, sessionErr := dashboardSession(operator)
		replicas := dashboardOperatorReplicas(operator)
		switch {
		case session != nil:
			replicas = session.Replicas
		case replicas <= 0:
			reason := "no Dashboard Dev session is recorded"
			if sessionErr != nil {
				reason = sessionErr.Error()
			}
			replicas = dashboardOperatorDefaultReplicas
			logs = append(logs, fmt.Sprintf("Original replica count unknown (%s); resuming with the chart default of %d.", reason, replicas))
		}
		if session != nil {
			if reasons := dashboardStaleReasons(session, operator); len(reasons) > 0 {
				logs = append(logs, "RHOAI changed during the session ("+strings.Join(reasons, "; ")+"); the operator will deploy the new release images.")
			}
		}
		// Keep a valid session while workload annotations still need cleanup
		// so the page keeps offering Revert. A corrupt one is dropped.
		var keep *dashboardDevSession
		if len(failures) > 0 {
			keep = session
		}
		now := time.Now().UTC().Format(time.RFC3339)
		detail := fmt.Sprintf("Resumed dashboard-operator (%d replicas)", replicas)
		action := &types.DashboardDevAction{Action: "revert", By: getUser(c), At: now, Detail: detail}
		if err := patchDashboardOperator(c, operator, replicas, keep, action); err != nil {
			if IsK8sError(err, http.StatusConflict) && attempt < 2 {
				continue
			}
			return fail("Failed to resume dashboard-operator: "+err.Error()+". Retry Revert to default.", errorCodeFromK8sErr(err))
		}
		message := fmt.Sprintf("Dashboard-operator restored to %d replicas. It will reconcile all dashboard images to the installed release.", replicas)
		code := ""
		if len(failures) > 0 {
			message += " Some workload annotations could not be restored: " + strings.Join(failures, "; ") + ". Retry revert."
			code = "partial_failure"
		}
		logs = append(logs, message)
		RecordActivity(c, types.ActivityEntry{Timestamp: now, User: getUser(c), Action: "revert-dashboard", Detail: message, Success: len(failures) == 0})
		return &types.OperationResponse{Success: len(failures) == 0, Message: message, ErrorCode: code, Logs: logs}, replicas
	}
}

// DashboardDevActive reports whether the dashboard is overridden: the
// dashboard-operator is paused (by Dashboard Dev or otherwise), a session or
// a corrupt session annotation exists, or a Dashboard-owned workload carries
// opendatahub.io/managed. Without dashboard-operator (RHOAI 3.4 and older) it
// reports the legacy flow's managed=false annotation, its saved original, or a
// PR/main build on rhods-dashboard.
func DashboardDevActive(c *Client) (bool, error) {
	operator, err := readDashboardOperator(c)
	if err != nil {
		return false, err
	}
	if operator == nil {
		return legacyDashboardDevActive(c)
	}
	if dashboardOverrideFromOperator(operator).Active {
		return true, nil
	}
	items, err := listDashboardNamespaceDeployments(c)
	if err != nil {
		return false, err
	}
	for _, d := range items {
		if _, ok := d.Metadata.Annotations["opendatahub.io/managed"]; ok && d.Metadata.Name != dashboardOperatorName && dashboardOwner(d) != "" {
			return true, nil
		}
	}
	return false, nil
}

func legacyDashboardDevActive(c *Client) (bool, error) {
	body, _, err := c.get(namespacedPath("apps/v1", "deployments", dashboardNamespace, dashboardDeploymentName))
	if IsK8sError(err, http.StatusNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var d dashboardDeployment
	if err := json.Unmarshal(body, &d); err != nil {
		return false, err
	}
	if _, saved := d.Metadata.Annotations[legacyOriginalManagedAnnotation]; saved || d.Metadata.Annotations["opendatahub.io/managed"] == "false" {
		return true, nil
	}
	for _, ct := range d.Spec.Template.Spec.Containers {
		if repo, ok := prContainerRepos[ct.Name]; ok {
			if kind, _, _, _ := parseDashboardBuild(repo, ct.Image); kind != "" {
				return true, nil
			}
		}
	}
	return false, nil
}

// Bounds for RevertDashboardDevForOperation; variables so tests can shorten them.
var (
	dashboardOperatorReadyTimeout = 3 * time.Minute
	dashboardOperatorPollInterval = 2 * time.Second
)

// RevertDashboardDevForOperation returns the dashboard to operator management
// before an operation that needs a working dashboard-operator (RHOAI update,
// reinstall, refresh, dashboard removal, uninstall): with the operator paused,
// a Dashboard CR deletion never completes and RHOAI updates do not reach the
// dashboard. It does nothing when no override is active, otherwise reverts and
// waits, bounded by dashboardOperatorReadyTimeout and the client's context,
// until the operator is scaled up and Ready. Safe to repeat from any state.
func RevertDashboardDevForOperation(c *Client) error {
	active, err := DashboardDevActive(c)
	if err != nil {
		return fmt.Errorf("check Dashboard Dev state: %w", err)
	}
	if !active {
		return nil
	}
	operator, err := readDashboardOperator(c)
	if err != nil {
		return fmt.Errorf("read dashboard-operator: %w", err)
	}
	if operator == nil {
		result, err := RevertDashboardImage(c)
		if err != nil {
			return fmt.Errorf("revert legacy dashboard override: %w", err)
		}
		if !result.Success {
			return fmt.Errorf("revert legacy dashboard override: %s", result.Message)
		}
		return nil
	}
	result, replicas := revertDashboardOperatorTo(c)
	if !result.Success {
		return fmt.Errorf("revert Dashboard Dev: %s", result.Message)
	}
	return waitDashboardOperatorReady(c, replicas)
}

func waitDashboardOperatorReady(c *Client, replicas int) error {
	ctx, cancel := context.WithTimeout(c.ctx, dashboardOperatorReadyTimeout)
	defer cancel()
	reader := *c
	reader.ctx = ctx
	var last string
	for {
		operator, err := readDashboardOperator(&reader)
		switch {
		case err != nil:
			last = err.Error()
		case operator == nil:
			last = "dashboard-operator Deployment not found"
		case dashboardOperatorReplicas(operator) == 0:
			return fmt.Errorf("dashboard-operator was paused again while waiting for it to resume")
		case dashboardDeploymentReady(*operator):
			if got := dashboardOperatorReplicas(operator); got != replicas {
				slog.Info("dashboard-operator ready with a different replica count than restored", "restored", replicas, "current", got)
			}
			return nil
		default:
			last = fmt.Sprintf("%d/%d replicas ready", operator.Status.ReadyReplicas, dashboardOperatorReplicas(operator))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("dashboard-operator did not become ready (%s): %w", last, ctx.Err())
		case <-time.After(dashboardOperatorPollInterval):
		}
	}
}
