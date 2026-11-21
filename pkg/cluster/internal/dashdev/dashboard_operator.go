package dashdev

import (
	"context"
	"encoding/json"
	"errors"
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

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const SessionAnnotation = "rhoai-nightly-updater.opendatahub.io/dashboard-dev"

// dashboardDevLastActionAnnotation records who last deployed or reverted and
// when. It lives next to the session on the dashboard module's operator
// Deployment (dashboard_operator_discovery.go) and,
// unlike the session, survives a revert. The platform controller applies that
// Deployment with server-side apply and never sets these annotations, so it
// leaves them alone.
const dashboardDevLastActionAnnotation = "rhoai-nightly-updater.opendatahub.io/dashboard-dev-last-action"

// PlatformVersionAnnotation is stamped by the platform deployer on every
// resource it applies (odh-platform-utilities framework
// controller/actions/deploy/action_deploy.go, SuffixVersion = rr.Release.Version).
const PlatformVersionAnnotation = "platform.opendatahub.io/version"

// OperatorDefaultReplicas is the dashboard-operator chart default
// (rhods-operator rhoai-3.6 prefetched-charts/dashboard-operator/values.yaml,
// manager.replicas: 1). Revert uses it only when the original count is unknown
// (no or corrupt session annotation) so the operator is never left at 0.
const OperatorDefaultReplicas = 1

// Dashboard build flavors. The odh-dashboard Konflux pipelines
// (.tekton/odh-dashboard-pull-request.yaml and -push.yaml) build the host image
// with BUILD_MODE=RHOAI (RHOAI logo, product name and docs links) and publish
// odh-pr-<N> (rhoai-konflux-tasks rhoai-init: PR_TAG="odh-pr-${PR_NUMBER}") and
// odh-stable (every push to main). OpenShift CI (openshift/release ci-operator
// config) builds the default BUILD_MODE=ODH and publishes pr-<N> and main.
const (
	FlavorRHOAI = "rhoai"
	FlavorODH   = "odh"
)

var dashboardDevMu sync.Mutex
var dashboardDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// The immutable tags main builds are also published under, which Deploy
// latest main pins to so the image reference names the commit. OpenShift CI's
// postsubmit mirrors :main to :main-<first 7 hex of the commit>; Konflux tags
// every push build with the full 40-hex commit next to :odh-stable. The same
// scheme holds for every module repository.
var (
	dashboardODHCommitTagPattern = regexp.MustCompile(`^main-[0-9a-f]{7,40}$`)
)

type dashboardContainer struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	Env   []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"env"`
}

type Deployment struct {
	Metadata struct {
		Name            string            `json:"name"`
		Namespace       string            `json:"namespace"`
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

// Session is saved as JSON in SessionAnnotation on the
// dashboard-operator Deployment in the same patch that pauses it, so the
// original replica count and the session survive updater restarts.
type Session struct {
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

// Ref is the operator Deployment's namespace/name.
func (d *Deployment) Ref() string { return d.Metadata.Namespace + "/" + d.Metadata.Name }

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

// ValidFlavor reports whether flavor is empty (the default) or known.
func ValidFlavor(flavor string) bool {
	return flavor == "" || flavor == FlavorRHOAI || flavor == FlavorODH
}

// BuildTags lists the tags to try, in order, for a build. The RHOAI
// flavor falls back per component to the OpenShift CI build when Konflux did
// not publish one; the state reports each container's resulting flavor.
func BuildTags(mode string, pr int, flavor string) []string {
	if mode == "main" {
		if flavor == FlavorODH {
			return []string{"main"}
		}
		return []string{"odh-stable", "main"}
	}
	if flavor == FlavorODH {
		return []string{fmt.Sprintf("pr-%d", pr)}
	}
	return []string{fmt.Sprintf("odh-pr-%d", pr), fmt.Sprintf("pr-%d", pr)}
}

// ParseBuild classifies an image of repo as a PR or main build and
// returns its flavor. A main build may run under its floating tag or the
// commit tag Deploy pins it to. Other images (release builds) return kind "".
func ParseBuild(repo, image string) (kind string, pr int, flavor string) {
	prefix := "quay.io/" + repo + ":"
	if !strings.HasPrefix(image, prefix) {
		return "", 0, ""
	}
	tag, _, _ := strings.Cut(strings.TrimPrefix(image, prefix), "@")
	switch {
	case tag == "odh-stable", registry.CommitSHAPattern.MatchString(tag):
		return "main", 0, FlavorRHOAI
	case tag == "main", dashboardODHCommitTagPattern.MatchString(tag):
		return "main", 0, FlavorODH
	case strings.HasPrefix(tag, "odh-pr-"):
		if n, err := strconv.Atoi(strings.TrimPrefix(tag, "odh-pr-")); err == nil && n > 0 {
			return "pr", n, FlavorRHOAI
		}
	case strings.HasPrefix(tag, "pr-"):
		if n, err := strconv.Atoi(strings.TrimPrefix(tag, "pr-")); err == nil && n > 0 {
			return "pr", n, FlavorODH
		}
	}
	return "", 0, ""
}

func dashboardOwner(d Deployment) string {
	for _, o := range d.Metadata.OwnerReferences {
		if o.Controller && o.Kind == "Dashboard" && strings.HasPrefix(o.APIVersion, "components.platform.opendatahub.io/") {
			return o.UID
		}
	}
	return ""
}

func listDashboardNamespaceDeployments(c *kube.Client, ns string) ([]Deployment, error) {
	var items []Deployment
	if err := c.ListInto(rhoai.NamespacedPath("apps/v1", "deployments", ns, ""), &items); err != nil {
		return nil, err
	}
	return items, nil
}

func DiscoverImages(c *kube.Client, operator *Deployment) ([]types.DashboardDevImage, error) {
	images, _, err := discoverDashboardWorkloads(c, operator)
	return images, err
}

// discoverDashboardWorkloads returns the dashboard build containers and the
// Deployments that run them, keyed by name.
func discoverDashboardWorkloads(c *kube.Client, operator *Deployment) ([]types.DashboardDevImage, map[string]Deployment, error) {
	// The workloads run in the applications namespace, where the finder
	// found the operator.
	items, err := listDashboardNamespaceDeployments(c, operator.Metadata.Namespace)
	if err != nil {
		return nil, nil, err
	}
	owner := ""
	for _, d := range items {
		if d.Metadata.Name == DeploymentName {
			owner = dashboardOwner(d)
		}
	}
	if owner == "" {
		return nil, nil, fmt.Errorf("dashboard deployment has no Dashboard controller owner; refusing to patch unverified workloads")
	}
	var images []types.DashboardDevImage
	workloads := map[string]Deployment{}
	session, err := SessionOf(operator)
	if err != nil {
		return nil, nil, err
	}
	for _, d := range items {
		if d.Metadata.UID == operator.Metadata.UID || dashboardOwner(d) != owner {
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
						expectedName = ContainerName
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
					images = append(images, types.DashboardDevImage{Deployment: d.Metadata.Name, Container: ct.Name, EnvVar: env.Name, Repository: repo, CurrentImage: ct.Image, DefaultImage: env.Value, Ready: DeploymentReady(d), WorkloadUID: d.Metadata.UID, OwnerUID: owner})
					workloads[d.Metadata.Name] = d
				}
			}
		}
	}
	sort.Slice(images, func(i, j int) bool {
		return images[i].Deployment+images[i].Container < images[j].Deployment+images[j].Container
	})
	if len(images) == 0 {
		return nil, nil, fmt.Errorf("no Dashboard-owned containers match the image environment variables of %s", operator.Metadata.Name)
	}
	return images, workloads, nil
}

func DeploymentReady(d Deployment) bool {
	replicas := 1
	if d.Spec.Replicas != nil {
		replicas = *d.Spec.Replicas
	}
	return replicas > 0 && d.Status.ObservedGeneration >= d.Metadata.Generation && d.Status.UpdatedReplicas == replicas && d.Status.ReadyReplicas == replicas && d.Status.Replicas == replicas
}

func SessionOf(operator *Deployment) (*Session, error) {
	raw := operator.Metadata.Annotations[SessionAnnotation]
	if raw == "" {
		return nil, nil
	}
	var s Session
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, fmt.Errorf("invalid Dashboard Dev recovery annotation: %w", err)
	}
	if s.OperatorUID != operator.Metadata.UID || s.Replicas <= 0 {
		return nil, errors.New("the Dashboard Dev recovery annotation does not match this operator")
	}
	return &s, nil
}

func dashboardOperatorReplicas(operator *Deployment) int {
	if operator.Spec.Replicas == nil {
		return 1 // Kubernetes defaults an unset Deployment replica count to 1.
	}
	return *operator.Spec.Replicas
}

func dashboardOperatorImage(operator *Deployment) string {
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
func dashboardOperatorEnv(operator *Deployment) map[string]string {
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
func dashboardStaleReasons(session *Session, operator *Deployment) []string {
	var reasons []string
	current := operator.Metadata.Annotations[PlatformVersionAnnotation]
	if session.ReleaseVersion != "" && current != "" && current != session.ReleaseVersion {
		reasons = append(reasons, fmt.Sprintf("RHOAI version changed from %s to %s", session.ReleaseVersion, current))
	}
	if image := dashboardOperatorImage(operator); session.OperatorImage != "" && image != "" && image != session.OperatorImage {
		reasons = append(reasons, operator.Metadata.Name+" image changed (RHOAI was updated)")
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

func dashboardLastAction(operator *Deployment) *types.DashboardDevAction {
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

// OverrideSummary reports whether the dashboard module's operator
// is paused (by Dashboard Dev or otherwise), by whom and since when, what
// each container was set to run, and whether RHOAI was updated since. It
// reads the operator Deployment, after a (cached) discovery of which one it
// is. It returns nil when the cluster has no dashboard module (legacy
// flow, or RHOAI not installed), and an error when it cannot tell.
func OverrideSummary(c *kube.Client) (*types.DashboardOverride, error) {
	_, operator, err := LoadOperator(c, false)
	if err != nil || operator == nil {
		return nil, err
	}
	o := dashboardOverrideFromOperator(operator)
	addDashboardDeletionWarning(c, o)
	return o, nil
}

func dashboardStaleWarning(op string) string {
	return "RHOAI was updated while " + op + " is paused. The dashboard keeps its current images, and the Dashboard CR still reports the old release, so RHOAI's later upgrade stages (runlevels 31 and 32: kserve, mlflow, feast, ogx, trustyai) wait up to 10 minutes after each rhods-operator start before continuing. Revert to default to apply the new release."
}

func dashboardDeletingWarning(op string) string {
	return "A Dashboard CR is being deleted while " + op + " is paused. Only " + op + " removes its finalizer (components.platform.opendatahub.io/cleanup), so the deletion, and any dashboard removal or RHOAI uninstall waiting on it, is stuck. Revert to default to resume " + op + "; the deletion then completes."
}

func dashboardExternalPauseWarning(op string) string {
	return op + " is paused outside Dashboard Dev, so the dashboard does not follow RHOAI updates. Revert to default resumes it with the chart default of 1 replica."
}

// dashboardCRDeleting reports whether any Dashboard CR has a deletionTimestamp.
// A missing CRD means there is nothing to delete.
func dashboardCRDeleting(c *kube.Client) (bool, error) {
	r, err := rhoai.ListServed(c, rhoai.ComponentAPIGroup, "/apis/components.platform.opendatahub.io/%s/dashboards", nil, []string{"v1alpha1"})
	if kube.IsK8sNotFound(err) || (err == nil && r.Version == "") {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	body := r.Body
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
func addDashboardDeletionWarning(c *kube.Client, o *types.DashboardOverride) {
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
		o.Warnings = append([]string{dashboardDeletingWarning(o.OperatorName)}, o.Warnings...)
	}
}

func dashboardOverrideFromOperator(operator *Deployment) *types.DashboardOverride {
	o := &types.DashboardOverride{
		OperatorName:      operator.Metadata.Name,
		OperatorNamespace: operator.Metadata.Namespace,
		OperatorPaused:    dashboardOperatorReplicas(operator) == 0,
		ReleaseVersion:    operator.Metadata.Annotations[PlatformVersionAnnotation],
		LastAction:        dashboardLastAction(operator),
	}
	session, err := SessionOf(operator)
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
		o.Warnings = append(o.Warnings, dashboardStaleWarning(o.OperatorName))
	}
	if o.OperatorPaused && session == nil {
		o.Warnings = append(o.Warnings, dashboardExternalPauseWarning(o.OperatorName))
	}
	return o
}

func PopulateStateWithOperator(c *kube.Client, state *types.DashboardState, target Target, operator *Deployment, err error) {
	state.DefaultFlavor = FlavorRHOAI
	state.AvailableFlavors = []string{FlavorRHOAI, FlavorODH}
	if err != nil {
		state.OperatorError = err.Error()
		return
	}
	if operator == nil {
		state.NoOperatorReason = target.Legacy
		state.IsDevMode = state.IsCustomPR
		state.AllDevImagesReady = state.PodReady && !state.RolloutPending
		state.DevImagesMatchTarget = true
		return
	}
	state.OperatorAvailable = true
	state.OperatorName = operator.Metadata.Name
	state.OperatorNamespace = operator.Metadata.Namespace
	state.OperatorPaused = dashboardOperatorReplicas(operator) == 0
	state.Managed = !state.OperatorPaused
	state.Override = dashboardOverrideFromOperator(operator)
	addDashboardDeletionWarning(c, state.Override)
	session, err := SessionOf(operator)
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
		if image.Deployment == DeploymentName && strings.HasPrefix(image.EnvVar, "RELATED_IMAGE_ODH_MOD_ARCH_") {
			state.DeploymentMode = "Sidecar"
		}
		if !image.Ready {
			state.AllDevImagesReady = false
		}
		if image.CurrentImage != image.DefaultImage {
			state.DefaultImagesRestored = false
		}
		kind, n, flavor := ParseBuild(image.Repository, image.CurrentImage)
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
	addDashboardRolloutDiagnostics(c, operator.Metadata.Namespace, images, workloads)
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
		Name              string                `json:"name"`
		Labels            map[string]string     `json:"labels"`
		OwnerReferences   []components.OwnerRef `json:"ownerReferences"`
		CreationTimestamp string                `json:"creationTimestamp"`
		DeletionTimestamp string                `json:"deletionTimestamp"`
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
func addDashboardRolloutDiagnostics(c *kube.Client, ns string, images []types.DashboardDevImage, workloads map[string]Deployment) {
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
	var items []dashboardPod
	if err := c.ListInto("/api/v1/namespaces/"+ns+"/pods", &items); err != nil {
		if kube.IsDecodeError(err) {
			slog.Warn("cannot parse dashboard pods for rollout diagnostics", "error", err)
			return
		}
		slog.Warn("cannot list dashboard pods for rollout diagnostics", "error", err)
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
		for p := range items {
			pod := &items[p]
			if pod.Metadata.DeletionTimestamp != "" || !dashboardLabelsMatch(d.Spec.Selector.MatchLabels, pod.Metadata.Labels) {
				continue
			}
			if !components.OwnedByDeployment(c, ns, images[i].Deployment, d.Metadata.UID, pod.Metadata.Labels, components.ControllerOf(pod.Metadata.OwnerReferences)) {
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
func patchDashboardOperator(c *kube.Client, operator *Deployment, replicas int, session *Session, action *types.DashboardDevAction) error {
	var annotation any
	if session != nil {
		raw, err := json.Marshal(session)
		if err != nil {
			return err
		}
		annotation = string(raw)
	}
	annotations := map[string]any{SessionAnnotation: annotation}
	if action != nil {
		raw, err := json.Marshal(action)
		if err != nil {
			return err
		}
		annotations[dashboardDevLastActionAnnotation] = string(raw)
	}
	return c.PatchJSON(rhoai.NamespacedPath("apps/v1", "deployments", operator.Metadata.Namespace, operator.Metadata.Name), map[string]any{
		"metadata": map[string]any{"uid": operator.Metadata.UID, "resourceVersion": operator.Metadata.ResourceVersion, "annotations": annotations},
		"spec":     map[string]any{"replicas": replicas},
	})
}

func waitDashboardOperatorStopped(c *kube.Client, operator *Deployment) error {
	ns, name := operator.Metadata.Namespace, operator.Metadata.Name
	if len(operator.Spec.Selector.MatchLabels) == 0 {
		return fmt.Errorf("%s has no pod selector", name)
	}
	var selector []string
	for k, v := range operator.Spec.Selector.MatchLabels {
		selector = append(selector, k+"="+v)
	}
	sort.Strings(selector)
	ctx, cancel := kube.Clock().WithTimeout(c.Context(), 60*time.Second)
	defer cancel()
	reader := c.WithContext(ctx)
	for {
		current, err := readDashboardOperatorAt(reader, ns, name)
		if err != nil {
			return err
		}
		if current == nil || current.Metadata.UID != operator.Metadata.UID || current.Spec.Replicas == nil || *current.Spec.Replicas != 0 {
			return fmt.Errorf("%s was replaced or resumed while pausing", name)
		}
		body, _, err := reader.Get("/api/v1/namespaces/" + ns + "/pods?labelSelector=" + url.QueryEscape(strings.Join(selector, ",")))
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
			return fmt.Errorf("%s pod list is missing items; cannot confirm the controller has stopped", name)
		}
		if len(pods.Items) == 0 {
			return nil
		}
		if err := kube.Clock().Sleep(ctx, time.Second); err != nil {
			return fmt.Errorf("waiting for %s pods to terminate: %w", name, err)
		}
	}
}

// Resolve the current mutable tag to a digest so clicking Deploy again picks up
// a new build and retries cannot silently switch builds during rollout.
func ResolveBuild(ctx context.Context, repo, tag string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "HEAD", "https://quay.io/v2/"+repo+"/manifests/"+tag, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json,application/vnd.docker.distribution.manifest.list.v2+json")
	resp, err := registry.HTTPClient().Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("quay.io returned HTTP %d for %s:%s", resp.StatusCode, repo, tag)
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if !dashboardDigestPattern.MatchString(digest) {
		return "", false, fmt.Errorf("quay.io returned an invalid image digest for %s:%s", repo, tag)
	}
	return "quay.io/" + repo + ":" + tag + "@" + digest, true, nil
}

// ResolveBuildTags returns the first published tag of tags.
func ResolveBuildTags(ctx context.Context, repo string, tags []string) (image, tag string, found bool, err error) {
	for _, tag := range tags {
		image, found, err := ResolveBuild(ctx, repo, tag)
		if err != nil || found {
			return image, tag, found, err
		}
	}
	return "", "", false, nil
}

// dashboardCommitTag is the immutable tag a main build of floating tag
// floatingTag and source commit commit is also published under, or "" when
// the tag is not a main tag.
func dashboardCommitTag(floatingTag, commit string) string {
	commit = strings.ToLower(commit)
	if !registry.CommitSHAPattern.MatchString(commit) {
		return ""
	}
	switch floatingTag {
	case "main":
		return "main-" + commit[:7]
	case "odh-stable":
		return commit
	}
	return ""
}

// pinDashboardMainBuild returns image (quay.io/<repo>:<floating tag>@<digest>,
// as ResolveBuild returns it) re-tagged with the build's commit tag
// when the registry publishes that tag for the same digest, so the Deployment
// names the commit it runs. The labels are read anonymously: the opendatahub
// repositories are public. Without a commit label, without the tag, or when
// the tag has moved to another digest, the floating tag is kept.
func pinDashboardMainBuild(ctx context.Context, repo, floatingTag, image string) string {
	_, digest, ok := registry.RepoAndDigest(image)
	if !ok {
		return image
	}
	labelsCtx, cancel := context.WithTimeout(ctx, registry.LabelFetchTimeout)
	labels, err := registry.ImageLabelsWithAuth(labelsCtx, "", image, false)
	cancel()
	if err != nil {
		slog.Debug("dashboard main build keeps its floating tag: labels unreadable", "image", kube.TruncateText(image, 80), "error", err)
		return image
	}
	commitTag := dashboardCommitTag(floatingTag, labels.GitCommit)
	if commitTag == "" {
		slog.Debug("dashboard main build keeps its floating tag: no commit label", "image", kube.TruncateText(image, 80), "commit", labels.GitCommit)
		return image
	}
	pinned, found, err := ResolveBuild(ctx, repo, commitTag)
	switch {
	case err != nil:
		slog.Debug("dashboard main build keeps its floating tag: commit tag lookup failed", "image", kube.TruncateText(image, 80), "tag", commitTag, "error", err)
		return image
	case !found:
		slog.Debug("dashboard main build keeps its floating tag: commit tag not published", "image", kube.TruncateText(image, 80), "tag", commitTag)
		return image
	}
	if _, pinnedDigest, _ := registry.RepoAndDigest(pinned); pinnedDigest != digest {
		slog.Debug("dashboard main build keeps its floating tag: commit tag has another digest", "image", kube.TruncateText(image, 80), "tag", commitTag, "tagDigest", pinnedDigest)
		return image
	}
	return pinned
}

// DeployMain deploys the latest main build of the given
// flavor ("" means the RHOAI build).
func DeployMain(c *kube.Client, flavor string) (*types.OperationResponse, error) {
	return DeployBuild(c, "main", 0, flavor)
}

func dashboardFlavorLabel(flavor string) string {
	if flavor == FlavorODH {
		return "ODH build"
	}
	return "RHOAI build"
}

// dashboardBaselineImage is the image a container runs when the target build
// has none for it: the image saved when the session started, unless RHOAI was
// updated since (its default changed) or the workload was replaced, in which
// case the operator's current default, which is also what Revert converges to.
func dashboardBaselineImage(session *Session, image types.DashboardDevImage) string {
	for _, b := range session.Baseline {
		if b.Deployment == image.Deployment && b.Container == image.Container && b.UID == image.WorkloadUID && b.DefaultImage == image.DefaultImage && b.Image != "" {
			return b.Image
		}
	}
	return image.DefaultImage
}

func DeployBuild(c *kube.Client, mode string, pr int, flavor string) (*types.OperationResponse, error) {
	if flavor == "" {
		flavor = FlavorRHOAI
	}
	logs := []string{}
	fail := func(message, code string) (*types.OperationResponse, error) {
		return &types.OperationResponse{Success: false, Message: message, ErrorCode: code, Logs: logs}, nil
	}
	if !ValidFlavor(flavor) {
		return fail("Unknown dashboard build flavor "+strconv.Quote(flavor)+"; use rhoai or odh.", "validation")
	}
	dashboardDevMu.Lock()
	defer dashboardDevMu.Unlock()
	target, operator, err := LoadOperator(c, true)
	if err != nil {
		return fail(kube.SentenceStart(err.Error())+". No changes were made.", kube.ErrorCodeOr(err, "prerequisites"))
	}
	if operator == nil {
		return fail("Latest-main deployment needs the dashboard module's operator, and this cluster has no dashboard module ("+target.Legacy+"). No changes were made.", "prerequisites")
	}
	opName := operator.Metadata.Name
	logs = append(logs, fmt.Sprintf("Dashboard operator: Deployment %s (%s)", operator.Ref(), target.Operator.Via))
	session, err := SessionOf(operator)
	if err != nil {
		return fail(err.Error()+". Click Revert to default to restore operator management, then deploy again.", "prerequisites")
	}
	now := kube.NowRFC3339()
	user := kube.GetUser(c)
	newSession := session == nil
	if newSession {
		replicas := dashboardOperatorReplicas(operator)
		if replicas <= 0 {
			return fail(opName+" is already paused outside Dashboard Dev. Click Revert to default to resume it before starting a test session.", "prerequisites")
		}
		session = &Session{OperatorUID: operator.Metadata.UID, Replicas: replicas, StartedAt: now, StartedBy: user}
	}
	// Pausing while a Dashboard CR deletion is pending would block that
	// deletion on the finalizer only the dashboard operator removes (D1).
	if deleting, err := dashboardCRDeleting(c); err != nil {
		return fail("Cannot check whether the Dashboard is being deleted: "+err.Error()+". No changes were made.", kube.ErrorCodeOr(err, "prerequisites"))
	} else if deleting {
		return fail("The Dashboard CR is being deleted (dashboard removal or RHOAI uninstall in progress). Pausing "+opName+" now would block that deletion. No changes were made.", "prerequisites")
	}
	images, err := DiscoverImages(c, operator)
	if err != nil {
		return fail(err.Error(), "prerequisites")
	}
	tags := BuildTags(mode, pr, flavor)
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
			image, tag, found, err := ResolveBuildTags(c.Context(), repo, tags)
			if err == nil && found && mode == "main" {
				// One labels read and one HEAD more per container: the
				// Deployment then names the commit, not a tag that moves.
				image = pinDashboardMainBuild(c.Context(), repo, tag, image)
			}
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
	session.ReleaseVersion = operator.Metadata.Annotations[PlatformVersionAnnotation]
	session.OperatorImage = dashboardOperatorImage(operator)
	action := &types.DashboardDevAction{Action: "deploy-" + mode, By: user, At: now, Detail: "Deploying " + label}
	if err := patchDashboardOperator(c, operator, 0, session, action); err != nil {
		return fail("Could not pause "+opName+": "+err.Error(), kube.ErrorCodeFromK8sErr(err))
	}
	logs = append(logs, "Dashboard Dev recovery information saved; "+opName+" scaled to 0.")
	if err := waitDashboardOperatorStopped(c, operator); err != nil {
		return fail(err.Error()+". No dashboard images were patched. Click Revert to default to resume the operator.", "prerequisites")
	}
	var failures []string
	groups := map[string][]ImagePatch{}
	var deployments []string
	for i, binding := range bindings {
		image := images[i]
		if _, ok := groups[image.Deployment]; !ok {
			deployments = append(deployments, image.Deployment)
		}
		groups[image.Deployment] = append(groups[image.Deployment], ImagePatch{Image: image, Target: binding.TargetImage, Source: binding.Source})
	}
	for _, deployment := range deployments {
		group := groups[deployment]
		if err := PatchControlledImages(c, operator, group); err != nil {
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
	message += ". " + opName + " is paused; revert after testing."
	if !success {
		retry := "Deploy latest main"
		if mode == "pr" {
			retry = fmt.Sprintf("Deploy PR #%d", pr)
		}
		message = fmt.Sprintf("Partial deployment of %s: %s failed. Retry %s to finish, or Revert to default to resume operator reconciliation.", label, kube.CountNoun(len(failures), "deployment", "deployments"), retry)
	}
	record.Activity(c, types.ActivityEntry{Timestamp: now, User: user, Action: "deploy-dashboard-" + mode, Detail: message, Success: success})
	code := ""
	if !success {
		code = "partial_failure"
	}
	return &types.OperationResponse{Success: success, Message: message, Logs: logs, ErrorCode: code}, nil
}

type ImagePatch struct {
	Image  types.DashboardDevImage
	Target string
	Source string
}

// Patch all selected containers in a deployment atomically. Status changes can
// advance resourceVersion during rollout; retry only conflicts and repeat all
// ownership/image checks on each fresh read.
func PatchControlledImages(c *kube.Client, operator *Deployment, patches []ImagePatch) error {
	if len(patches) == 0 {
		return nil
	}
	image := patches[0].Image
	ns := operator.Metadata.Namespace
	path := rhoai.NamespacedPath("apps/v1", "deployments", ns, image.Deployment)
	for attempt := range 5 {
		current, err := readDashboardOperatorAt(c, ns, operator.Metadata.Name)
		if err != nil {
			return err
		}
		if current == nil || current.Metadata.UID != operator.Metadata.UID || current.Spec.Replicas == nil || *current.Spec.Replicas != 0 {
			return fmt.Errorf("%s is no longer paused", operator.Metadata.Name)
		}
		var d Deployment
		if err := c.GetInto(path, &d); err != nil {
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
		data, _ := json.Marshal(map[string]any{"metadata": map[string]any{"uid": d.Metadata.UID, "resourceVersion": d.Metadata.ResourceVersion}, "spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": containers}}}})
		_, _, err = c.StrategicPatch(path, data)
		if err == nil {
			return nil
		}
		if !kube.IsK8sConflict(err) || attempt == 4 {
			return err
		}
		if err := kube.Clock().Sleep(c.Context(), time.Duration(attempt+1)*100*time.Millisecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("dashboard image patch exhausted conflict retries")
}

// clearDashboardManagedAnnotations removes opendatahub.io/managed from every
// Dashboard-owned workload that has it. The old PR flow set it to "false",
// which makes the dashboard operator skip the workload (odh-platform-utilities
// pkg/deploy: managed=false is a create-only opt-out). It needs no session, so
// it also works after the session annotation was lost or the Dashboard removed.
func clearDashboardManagedAnnotations(c *kube.Client, ns, operatorName string) (cleared []string, failures []string) {
	items, err := listDashboardNamespaceDeployments(c, ns)
	if err != nil {
		return nil, []string{"list dashboard workloads: " + err.Error()}
	}
	for _, d := range items {
		if d.Metadata.Name == operatorName || dashboardOwner(d) == "" {
			continue
		}
		if _, ok := d.Metadata.Annotations["opendatahub.io/managed"]; !ok {
			continue
		}
		patch := map[string]any{"metadata": map[string]any{"uid": d.Metadata.UID, "annotations": map[string]any{"opendatahub.io/managed": nil}}}
		if err := c.PatchJSON(rhoai.NamespacedPath("apps/v1", "deployments", ns, d.Metadata.Name), patch); err != nil && !kube.IsK8sNotFound(err) {
			failures = append(failures, d.Metadata.Name+": "+err.Error())
			continue
		}
		cleared = append(cleared, d.Metadata.Name)
	}
	return cleared, failures
}
