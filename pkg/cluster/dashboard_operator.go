package cluster

import (
	"context"
	"encoding/json"
	"fmt"
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

var dashboardDevMu sync.Mutex
var dashboardDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

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
				Containers []struct {
					Name  string `json:"name"`
					Image string `json:"image"`
					Env   []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"env"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration int64 `json:"observedGeneration"`
		UpdatedReplicas    int   `json:"updatedReplicas"`
		ReadyReplicas      int   `json:"readyReplicas"`
		Replicas           int   `json:"replicas"`
	} `json:"status"`
}

type dashboardDevSession struct {
	OperatorUID     string                `json:"operatorUID"`
	Replicas        int                   `json:"replicas"`
	Mode            string                `json:"mode"`
	PR              int                   `json:"pr,omitempty"`
	Bindings        []dashboardDevBinding `json:"bindings,omitempty"`
	TargetsRecorded bool                  `json:"targetsRecorded,omitempty"`
}

type dashboardDevBinding struct {
	Deployment  string `json:"deployment"`
	UID         string `json:"uid"`
	Container   string `json:"container"`
	EnvVar      string `json:"envVar"`
	TargetImage string `json:"targetImage,omitempty"`
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

func dashboardOwner(d dashboardDeployment) string {
	for _, o := range d.Metadata.OwnerReferences {
		if o.Controller && o.Kind == "Dashboard" && strings.HasPrefix(o.APIVersion, "components.platform.opendatahub.io/") {
			return o.UID
		}
	}
	return ""
}

func discoverDashboardImages(c *Client, operator *dashboardDeployment) ([]types.DashboardDevImage, error) {
	path := namespacedPath("apps/v1", "deployments", dashboardNamespace, "")
	body, _, err := c.get(path)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []dashboardDeployment `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	owner := ""
	for _, d := range list.Items {
		if d.Metadata.Name == dashboardDeploymentName {
			owner = dashboardOwner(d)
		}
	}
	if owner == "" {
		return nil, fmt.Errorf("dashboard deployment has no Dashboard controller owner; refusing to patch unverified workloads")
	}
	var images []types.DashboardDevImage
	session, err := dashboardSession(operator)
	if err != nil {
		return nil, err
	}
	for _, d := range list.Items {
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
				}
			}
		}
	}
	sort.Slice(images, func(i, j int) bool {
		return images[i].Deployment+images[i].Container < images[j].Deployment+images[j].Container
	})
	if len(images) == 0 {
		return nil, fmt.Errorf("no Dashboard-owned containers match dashboard-operator image environment variables")
	}
	return images, nil
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

func populateDashboardDevState(c *Client, state *types.DashboardState) {
	operator, err := readDashboardOperator(c)
	populateDashboardDevStateWithOperator(c, state, operator, err)
}

func populateDashboardDevStateWithOperator(c *Client, state *types.DashboardState, operator *dashboardDeployment, err error) {
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
	state.OperatorPaused = operator.Spec.Replicas != nil && *operator.Spec.Replicas == 0
	state.Managed = !state.OperatorPaused
	session, err := dashboardSession(operator)
	if err != nil {
		state.OperatorError = err.Error()
		return
	}
	if session != nil {
		state.IsDevMode = true
		state.DevMode = session.Mode
		state.PRNumber = session.PR
	}
	images, err := discoverDashboardImages(c, operator)
	if err != nil {
		state.OperatorError = err.Error()
		return
	}
	state.DevImages = images
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
					if binding.TargetImage != "" {
						image.MatchesTarget = binding.UID == image.WorkloadUID && image.CurrentImage == binding.TargetImage
						matchedBindings[binding.Deployment+"/"+binding.Container] = true
					}
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
		prefix := "quay.io/" + image.Repository + ":"
		if !strings.HasPrefix(image.CurrentImage, prefix) {
			continue
		}
		tag := strings.SplitN(strings.TrimPrefix(image.CurrentImage, prefix), "@", 2)[0]
		if tag == "main" && image.CurrentImage != image.DefaultImage {
			state.IsDevMode = true
			if state.DevMode == "" {
				state.DevMode = "main"
			}
		}
		if strings.HasPrefix(tag, "pr-") {
			n, err := strconv.Atoi(strings.TrimPrefix(tag, "pr-"))
			if err != nil {
				continue
			}
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
}

func patchDashboardOperator(c *Client, operator *dashboardDeployment, replicas int, session *dashboardDevSession) error {
	var annotation interface{}
	if session != nil {
		raw, err := json.Marshal(session)
		if err != nil {
			return err
		}
		annotation = string(raw)
	}
	data, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"uid": operator.Metadata.UID, "resourceVersion": operator.Metadata.ResourceVersion, "annotations": map[string]interface{}{dashboardDevAnnotation: annotation}},
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

func DeployDashboardMain(c *Client) (*types.OperationResponse, error) {
	return deployDashboardBuild(c, "main", 0)
}

func deployDashboardBuild(c *Client, mode string, pr int) (*types.OperationResponse, error) {
	dashboardDevMu.Lock()
	defer dashboardDevMu.Unlock()
	logs := []string{}
	fail := func(message, code string) (*types.OperationResponse, error) {
		return &types.OperationResponse{Success: false, Message: message, ErrorCode: code, Logs: logs}, nil
	}
	operator, err := readDashboardOperator(c)
	if err != nil {
		return fail("Cannot read dashboard-operator: "+err.Error(), "prerequisites")
	}
	if operator == nil {
		return fail("Latest-main deployment requires dashboard-operator.", "prerequisites")
	}
	session, err := dashboardSession(operator)
	if err != nil {
		return fail(err.Error(), "prerequisites")
	}
	if session == nil {
		replicas := 1
		if operator.Spec.Replicas != nil {
			replicas = *operator.Spec.Replicas
		}
		if replicas <= 0 {
			return fail("dashboard-operator is already paused outside Dashboard Dev. Resume it before starting a test session.", "prerequisites")
		}
		session = &dashboardDevSession{OperatorUID: operator.Metadata.UID, Replicas: replicas}
	}
	images, err := discoverDashboardImages(c, operator)
	if err != nil {
		return fail(err.Error(), "prerequisites")
	}
	tag := "main"
	if mode == "pr" {
		tag = fmt.Sprintf("pr-%d", pr)
	}
	type checked struct {
		i     int
		image string
		found bool
		err   error
	}
	results := make(chan checked, len(images))
	for i, image := range images {
		go func(i int, repo string) {
			image, found, err := resolveDashboardBuild(c.ctx, repo, tag)
			results <- checked{i, image, found, err}
		}(i, image.Repository)
	}
	resolved := make([]checked, len(images))
	for range images {
		r := <-results
		resolved[r.i] = r
	}
	var targets []checked
	for i, r := range resolved {
		if r.err != nil {
			return fail("Cannot verify "+images[i].Repository+": "+r.err.Error()+". No changes were made.", "network")
		}
		if !r.found {
			logs = append(logs, images[i].Deployment+"/"+images[i].Container+": "+tag+" not built; unchanged")
			if mode == "main" {
				return fail("Missing main image for "+images[i].Repository+". No changes were made.", "validation")
			}
			continue
		}
		targets = append(targets, r)
	}
	if len(targets) == 0 {
		return fail("No installed dashboard components have an image for "+tag+". Ensure CI has published the PR images.", "validation")
	}
	session.Mode = mode
	session.PR = pr
	session.Bindings = nil
	session.TargetsRecorded = true
	for i, image := range images {
		session.Bindings = append(session.Bindings, dashboardDevBinding{Deployment: image.Deployment, UID: image.WorkloadUID, Container: image.Container, EnvVar: image.EnvVar, TargetImage: resolved[i].image})
	}
	if err := patchDashboardOperator(c, operator, 0, session); err != nil {
		return fail("Could not pause dashboard-operator: "+err.Error(), errorCodeFromK8sErr(err))
	}
	logs = append(logs, "Dashboard Dev recovery information saved; dashboard-operator scaled to 0.")
	if err := waitDashboardOperatorStopped(c, operator); err != nil {
		return fail(err.Error()+". No dashboard images were patched. Click Revert to default to resume the operator.", "prerequisites")
	}
	var failures []string
	groups := map[string][]dashboardImagePatch{}
	var deployments []string
	for _, r := range targets {
		image := images[r.i]
		if _, ok := groups[image.Deployment]; !ok {
			deployments = append(deployments, image.Deployment)
		}
		groups[image.Deployment] = append(groups[image.Deployment], dashboardImagePatch{Image: image, Target: r.image})
	}
	patched := 0
	for _, deployment := range deployments {
		group := groups[deployment]
		if err := patchControlledDashboardImages(c, operator, group); err != nil {
			failure := deployment + ": " + err.Error()
			failures = append(failures, failure)
			logs = append(logs, "Failed: "+failure)
			continue
		}
		patched += len(group)
		for _, patch := range group {
			logs = append(logs, patch.Image.Deployment+"/"+patch.Image.Container+": "+patch.Target)
		}
	}
	success := len(failures) == 0
	message := fmt.Sprintf("Deployed %s to %d dashboard containers. Dashboard-operator is paused; revert after testing.", tag, len(targets))
	if !success {
		retry := "Deploy latest main"
		if mode == "pr" {
			retry = fmt.Sprintf("Deploy PR #%d", pr)
		}
		message = fmt.Sprintf("Partial deployment: %d/%d containers updated. Retry %s to finish, or Revert to default to resume operator reconciliation.", patched, len(targets), retry)
	}
	RecordActivity(c, types.ActivityEntry{Timestamp: time.Now().UTC().Format(time.RFC3339), User: getUser(c), Action: "deploy-dashboard-" + mode, Detail: message, Success: success})
	code := ""
	if !success {
		code = "partial_failure"
	}
	return &types.OperationResponse{Success: success, Message: message, Logs: logs, ErrorCode: code}, nil
}

func patchControlledDashboardImage(c *Client, operator *dashboardDeployment, image types.DashboardDevImage, target string) error {
	return patchControlledDashboardImages(c, operator, []dashboardImagePatch{{Image: image, Target: target}})
}

type dashboardImagePatch struct {
	Image  types.DashboardDevImage
	Target string
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

func revertDashboardOperator(c *Client, operator *dashboardDeployment) (*types.OperationResponse, error) {
	dashboardDevMu.Lock()
	defer dashboardDevMu.Unlock()
	// Re-read after acquiring the shared deploy/revert lock.
	operator, err := readDashboardOperator(c)
	if err != nil || operator == nil {
		return nil, fmt.Errorf("read dashboard-operator before revert: %v", err)
	}
	session, err := dashboardSession(operator)
	if err != nil {
		return &types.OperationResponse{Success: false, Message: err.Error(), ErrorCode: "prerequisites"}, nil
	}
	if session == nil {
		// Upgrade compatibility: old PR deployments have no saved session. A
		// running operator can still resume management after clearing their annotation.
		replicas := 1
		if operator.Spec.Replicas != nil {
			replicas = *operator.Spec.Replicas
		}
		if replicas <= 0 {
			return &types.OperationResponse{Success: false, Message: "No saved Dashboard Dev session. This operator was paused outside the tool; its original replica count is unknown.", ErrorCode: "prerequisites"}, nil
		}
		session = &dashboardDevSession{OperatorUID: operator.Metadata.UID, Replicas: replicas}
	}
	// Remove the old PR flow's unmanaged annotation only on verified Dashboard workloads.
	images, err := discoverDashboardImages(c, operator)
	var failures []string
	if err != nil {
		failures = append(failures, err.Error())
	} else {
		seen := map[string]bool{}
		for _, image := range images {
			if seen[image.Deployment] {
				continue
			}
			seen[image.Deployment] = true
			data, _ := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{"uid": image.WorkloadUID, "annotations": map[string]interface{}{"opendatahub.io/managed": nil}}})
			_, _, err := c.patch(namespacedPath("apps/v1", "deployments", dashboardNamespace, image.Deployment), data)
			if err != nil {
				failures = append(failures, image.Deployment+": "+err.Error())
			}
		}
	}
	// Always attempt to resume, including when a workload was deleted or a patch failed.
	var keep *dashboardDevSession
	if len(failures) > 0 {
		keep = session
	}
	if err := patchDashboardOperator(c, operator, session.Replicas, keep); err != nil {
		return &types.OperationResponse{Success: false, Message: "Failed to resume dashboard-operator: " + err.Error() + ". Retry Revert to default.", ErrorCode: errorCodeFromK8sErr(err)}, nil
	}
	message := fmt.Sprintf("Dashboard-operator restored to %d replicas. It will reconcile all dashboard images to the installed release.", session.Replicas)
	code := ""
	if len(failures) > 0 {
		message += " Some workload annotations could not be restored: " + strings.Join(failures, "; ") + ". Retry revert."
		code = "partial_failure"
	}
	RecordActivity(c, types.ActivityEntry{Timestamp: time.Now().UTC().Format(time.RFC3339), User: getUser(c), Action: "revert-dashboard", Detail: message, Success: len(failures) == 0})
	return &types.OperationResponse{Success: len(failures) == 0, Message: message, ErrorCode: code, Logs: []string{message}}, nil
}
