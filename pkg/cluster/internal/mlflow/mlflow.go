package mlflow

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dashdev"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	CRName          = "mlflow"
	APIGroup        = "mlflow.opendatahub.io/v1"
	mlflowImageRepo = "opendatahub/mlflow"
	// mlflowPodSelector matches the MLflow server pods; the operator's helm
	// chart labels them app=mlflow<resourceSuffix> (charts/mlflow/templates/
	// deployment.yaml), and the suffix is empty for the singleton "mlflow".
	mlflowPodSelector = "app=mlflow"
	// OriginalImageAnnotation records spec.image.image as it was before
	// the first PR deploy ("" when unset, meaning the operator default), so
	// Revert restores exactly that. The mlflow-operator only ever removes its
	// own force-migrate annotation (internal/controller/migration.go), so
	// this annotation is left alone.
	OriginalImageAnnotation = "rhoai-nightly-updater/original-image"
)

var (
	// deleteTimeout bounds how long teardown waits for the CR to go.
	// The MLflow CR has no finalizer (mlflow-operator rhoai-3.6), so this
	// normally returns at once.
	deleteTimeout = 15 * time.Second
	deletePoll    = time.Second

	mlflowPRTagPattern = regexp.MustCompile(`:odh-pr-(\d+)(?:@|$)`)

	// resolveMLflowPRImage resolves quay.io/opendatahub/mlflow:odh-pr-N to
	// "repo:tag@digest". odh-pr-N moves with every new PR commit, so pinning
	// the digest makes a redeploy change spec.image.image and roll out the
	// new build. CRI-O accepts tag@digest references and pulls by digest
	// (cri-o internal/storage/image_test.go "with tag and digest"). Tests
	// replace it.
	resolveMLflowPRImage = func(ctx context.Context, pr int) (string, bool, error) {
		return dashdev.ResolveBuild(ctx, mlflowImageRepo, fmt.Sprintf("odh-pr-%d", pr))
	}
)

type mlflowCR struct {
	Metadata kube.ObjectMeta `json:"metadata"`
	Spec     struct {
		Image *struct {
			Image *string `json:"image"`
		} `json:"image"`
		Resources json.RawMessage `json:"resources"`
	} `json:"spec"`
	Status struct {
		// URL is the public address the operator reports (on current
		// releases the data-science gateway, https://rh-ai.<apps>/mlflow,
		// served by an HTTPRoute; there is no Route). odh-dashboard reads the
		// same field for its external MLflow link (gen-ai BFF,
		// integrations/mlflow/mlflow_cr.go DiscoverMLflowExternalURL).
		URL        string `json:"url"`
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

// UIURL is MLflow's public address: the CR's status.url when it is an
// https URL, else the Route named "mlflow" (releases before the gateway).
//
// The address goes into the client settings users paste into a shell, so
// only a plain https address (kube.SnippetURL) is used.
func UIURL(statusURL, routeHost string) string {
	if u, err := url.Parse(strings.TrimSpace(statusURL)); err == nil && u.Scheme == "https" && u.Host != "" {
		if s := strings.TrimRight(u.String(), "/"); kube.SnippetURL(s) {
			return s
		}
		return ""
	}
	if routeHost != "" && kube.SnippetURL("https://"+routeHost) {
		return "https://" + routeHost
	}
	return ""
}

func (m mlflowCR) image() string {
	if m.Spec.Image == nil || m.Spec.Image.Image == nil {
		return ""
	}
	return *m.Spec.Image.Image
}

// managedByTool reports whether this tool created the CR: labelled by this
// version, or server-side applied at creation by older versions. A CR created
// from the console or dashboard is never claimed, because the MLflow CRD only
// allows the single name "mlflow" and deleting it garbage-collects mlflow-pvc.
func (m mlflowCR) managedByTool() bool {
	return m.Metadata.HasToolLabel() || m.Metadata.OwnsFieldPath([]string{envkit.ToolFieldManager}, "f:spec")
}

// oldToolResources: spec.resources is exactly what SetupMLflow set before
// a80121f (requests 500m/1Gi, limits 2/2Gi). The 2Gi limit got the server
// OOMKilled while MLflow generated its demo (live on 3.6); the operator's
// chart defaults (limits 4 CPU / 3Gi) apply once spec.resources is unset.
// Anything else is someone's own choice and is left alone.
func (m mlflowCR) oldToolResources() bool {
	if len(m.Spec.Resources) == 0 {
		return false
	}
	var r map[string]map[string]any
	if json.Unmarshal(m.Spec.Resources, &r) != nil || len(r) != 2 {
		return false
	}
	is := func(q map[string]any, cpu, mem string) bool {
		return len(q) == 2 && fmt.Sprint(q["cpu"]) == cpu && fmt.Sprint(q["memory"]) == mem
	}
	return is(r["requests"], "500m", "1Gi") && is(r["limits"], "2", "2Gi")
}

func prNumberFromImage(image string) int {
	if m := mlflowPRTagPattern.FindStringSubmatch(image); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// prOverride reports whether a PR image is active and what Revert restores
// ("" means removing spec.image.image so the operator default applies). A
// PR image is active only while spec.image.image is one: an image someone
// set by hand after a PR deploy is theirs, and the saved annotation is then
// stale (staleOriginal), so Revert must not undo their change.
func (m mlflowCR) prOverride() (active bool, revertImage string) {
	if prNumberFromImage(m.image()) == 0 {
		return false, ""
	}
	return true, m.Metadata.Annotations[OriginalImageAnnotation]
}

// staleOriginal: the image saved before a PR deploy is still on the CR,
// but no PR image runs any more.
func (m mlflowCR) staleOriginal() bool {
	_, saved := m.Metadata.Annotations[OriginalImageAnnotation]
	return saved && prNumberFromImage(m.image()) == 0
}

func ReadCR(c *kube.Client) (mlflowCR, error) {
	var cr mlflowCR
	if err := c.GetInto(rhoai.ClusterPath(APIGroup, "mlflows", CRName), &cr); err != nil {
		if kube.IsDecodeError(err) {
			return mlflowCR{}, fmt.Errorf("parse MLflow CR: %w", err)
		}
		return mlflowCR{}, err
	}
	return cr, nil
}

// mlflowNamespace is where the MLflow operator creates the server: the
// applications namespace (mlflow-operator internal/config/config.go,
// APPLICATIONS_NAMESPACE, which the platform sets to the DSCInitialization's).
func mlflowNamespace(c *kube.Client) (string, error) {
	return rhoai.AppsNamespace(c)
}

// mlflowOwnedPVCs lists PVCs in the MLflow namespace ns whose owner is the
// CR, which the garbage collector deletes together with it.
func mlflowOwnedPVCs(c *kube.Client, ns, uid string) ([]string, error) {
	var items []struct {
		Metadata kube.ObjectMeta `json:"metadata"`
	}
	if err := c.ListInto(rhoai.NamespacedPath("v1", "persistentvolumeclaims", ns, ""), &items); err != nil {
		return nil, err
	}
	var names []string
	for _, item := range items {
		if item.Metadata.OwnedBy(uid) {
			names = append(names, item.Metadata.Name)
		}
	}
	return names, nil
}

// Status is MLflow's state; a CR that cannot be read is reported in its
// message (see ReadStatus for the error).
func Status(c *kube.Client) types.ResourceState {
	state, _ := ReadStatus(c)
	return state
}

// ReadStatus is Status with the error of a CR read that failed (other
// than not found), for callers that must tell an expired sign-in or a
// refusal (401, 403) from MLflow not being ready.
func ReadStatus(c *kube.Client) (types.ResourceState, error) {
	ns, nsErr := mlflowNamespace(c)
	state := types.ResourceState{Namespace: ns}

	cr, err := ReadCR(c)
	if kube.IsK8sNotFound(err) {
		state.Message = "Not deployed"
		return state, nil
	}
	if err != nil {
		state.Message = statusReadFailed + err.Error()
		return state, err
	}
	state.Deployed = true
	state.ManagedByTool = cr.managedByTool()
	state.Terminating = cr.Metadata.Terminating()
	state.CurrentImage = cr.image()
	// Reported regardless of readiness, so Revert stays available when a PR
	// image breaks the pod.
	state.PROverride, state.RevertImage = cr.prOverride()
	if state.PROverride {
		state.PRNumber = prNumberFromImage(state.CurrentImage)
	}
	if !state.ManagedByTool {
		state.TeardownBlockedReason = "This MLflow instance was not created by this tool (for example it was created from the console or dashboard), so the tool will not delete it."
	}

	for _, cond := range cr.Status.Conditions {
		if cond.Type == "Ready" || cond.Type == "Available" {
			if cond.Status == "True" {
				state.Ready = true
				state.Message = "Running"
			} else {
				state.Message = kube.FirstNonEmpty(kube.TruncateText(cond.Message, 300), cond.Reason, "Not ready")
			}
			break
		}
	}
	if state.Message == "" {
		state.Message = "Provisioning"
	}
	if state.Terminating {
		state.Message = "Terminating"
	}
	if nsErr != nil {
		// The server's Route, pods and PVC are in the applications
		// namespace; without it they are not read.
		state.Message += fmt.Sprintf(" (the namespace it runs in is unknown: %v)", nsErr)
		state.UIRoute = UIURL(cr.Status.URL, "")
		return state, nil
	}

	var (
		wg    sync.WaitGroup
		host  string
		issue envkit.PodIssue
		pvcs  []string
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		// The Route only matters on releases whose CR reports no URL.
		if UIURL(cr.Status.URL, "") == "" {
			host = rhoai.RouteHost(c, ns, "mlflow")
		}
	}()
	go func() {
		defer wg.Done()
		if !state.Ready && !state.Terminating {
			issue, _ = envkit.InspectPods(c, ns, mlflowPodSelector, "")
		}
	}()
	go func() {
		defer wg.Done()
		if state.ManagedByTool {
			pvcs, _ = mlflowOwnedPVCs(c, ns, cr.Metadata.UID)
		}
	}()
	wg.Wait()
	state.UIRoute = UIURL(cr.Status.URL, host)
	envkit.ApplyPodIssue(&state, issue)
	state.DataPVCs = pvcs
	return state, nil
}

// Setup creates the (cluster-scoped) MLflow CR; the operator runs the
// server in the applications namespace. It leaves
// spec.image unset so the operator deploys its platform default
// (RELATED_IMAGE_ODH_MLFLOW_IMAGE, the RHOAI build, falling back to
// MLFLOW_IMAGE; mlflow-operator internal/config/config.go and
// internal/controller/helm.go).
func Setup(c *kube.Client) (*types.OperationResponse, error) {
	logs := []string{}

	existing := Status(c)
	if existing.Deployed {
		return &types.OperationResponse{
			Success: false, Message: "MLflow is already deployed.",
			Logs: logs, ErrorCode: "validation",
		}, nil
	}
	// A component that is not Managed leaves its CRD behind: the CR would
	// be created and never start.
	if block := envkit.SetupComponentBlock(c, envkit.MLflowComponent); block != nil {
		record.Action(c, "setup-mlflow", "component not managed", false)
		return &types.OperationResponse{Success: false, Message: block.Reason + " Nothing was changed.", Logs: logs, ErrorCode: "prerequisites"}, nil
	}

	logs = append(logs, "Creating MLflow CR...")
	mlflowCR := map[string]any{
		"apiVersion": APIGroup,
		"kind":       "MLflow",
		"metadata": map[string]any{
			"name":   CRName,
			"labels": envkit.ToolLabels(),
		},
		"spec": map[string]any{
			"replicas": 1,
			"migration": map[string]any{
				"mode": "Automatic",
			},
			// No "resources": the operator then uses its chart's defaults
			// (mlflow-operator internal/controller/helm.go passes
			// spec.resources only when set; charts/mlflow/values.yaml ships
			// requests 1 CPU / 2Gi, limits 4 CPU / 3Gi on rhoai-3.6). The
			// server runs 8 workers that idle near 1.9Gi together; the 2Gi
			// limit this CR used to set got the container OOMKilled while
			// MLflow generated its demo data (seen live on 3.6).
			"storage": map[string]any{
				"accessModes": []string{"ReadWriteOnce"},
				"resources": map[string]any{
					"requests": map[string]any{"storage": "10Gi"},
				},
			},
			"backendStoreUri":      "sqlite:////mlflow/mlflow.db",
			"registryStoreUri":     "sqlite:////mlflow/mlflow.db",
			"artifactsDestination": "file:///mlflow/artifacts",
			"serveArtifacts":       true,
		},
	}

	// Create, never apply: the CR is a singleton, and a forced apply would
	// take over one that someone created a moment ago.
	data, _ := json.Marshal(mlflowCR)
	if _, _, err := c.Post(envkit.WithToolFieldManager(rhoai.ClusterPath(APIGroup, "mlflows", "")), data); err != nil {
		msg := fmt.Sprintf("Failed to create MLflow CR: %v", err)
		code := kube.ErrorCodeFromK8sErr(err)
		switch {
		case kube.IsK8sNotFound(err):
			msg = "The MLflow CRD is not installed. Set the mlflowoperator component to Managed in the DataScienceCluster first."
			code = "prerequisites"
		case kube.IsK8sConflict(err):
			msg = "MLflow was created by someone else in the meantime. Refresh the status."
			code = "conflict"
		}
		record.Action(c, "setup-mlflow", fmt.Sprintf("CR creation failed: %v", err), false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}
	logs = append(logs, "OK: MLflow CR created")

	slog.Info("mlflow setup", "user", kube.GetUser(c))
	detail := "MLflow CR created"
	if ns, err := mlflowNamespace(c); err == nil {
		detail += "; the server runs in " + ns
	}
	record.Action(c, "setup-mlflow", detail, true)

	return &types.OperationResponse{
		Success: true, Message: "MLflow CR created. The operator will deploy MLflow shortly.",
		Logs: logs,
	}, nil
}

// Teardown deletes the MLflow CR if this tool created it. The operator
// owns mlflow-pvc through the CR, so the garbage collector deletes the
// tracking database and artifacts with it.
func Teardown(c *kube.Client) (*types.OperationResponse, error) {
	logs := []string{}
	fail := func(msg, code, activity string) (*types.OperationResponse, error) {
		record.Action(c, "teardown-mlflow", activity, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}

	cr, err := ReadCR(c)
	if kube.IsK8sNotFound(err) {
		logs = append(logs, "MLflow CR already absent")
		return &types.OperationResponse{Success: true, Message: "MLflow is not deployed; nothing to remove.", Logs: logs}, nil
	}
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the MLflow CR; nothing was deleted: %v", err), kube.ErrorCodeFromK8sErr(err), fmt.Sprintf("CR read failed: %v", err))
	}
	if !cr.managedByTool() {
		return fail("This MLflow instance was not created by this tool, so it was not deleted.", "not_managed", "not managed")
	}
	if cr.Metadata.Terminating() {
		logs = append(logs, "MLflow CR is already being deleted")
		return &types.OperationResponse{Success: false, Message: "MLflow is still being deleted." + mlflowFinalizerNote(cr), Logs: logs, ErrorCode: "in_progress"}, nil
	}

	// The PVC names are only a note: the garbage collector deletes them
	// with the CR, wherever they are.
	ns, pvcErr := mlflowNamespace(c)
	var pvcs []string
	if pvcErr == nil {
		pvcs, pvcErr = mlflowOwnedPVCs(c, ns, cr.Metadata.UID)
	}
	logs = append(logs, "Deleting MLflow CR...")
	crPath := rhoai.ClusterPath(APIGroup, "mlflows", CRName)
	if _, delErr := kube.DeleteWithUID(c, crPath, cr.Metadata.UID); delErr != nil && !kube.IsK8sNotFound(delErr) {
		code := kube.ErrorCodeFromK8sErr(delErr)
		if kube.IsK8sConflict(delErr) {
			code = "conflict"
		}
		return fail(fmt.Sprintf("Failed to delete MLflow CR: %v", delErr), code, fmt.Sprintf("CR delete failed: %v", delErr))
	}
	logs = append(logs, "OK: MLflow CR deleted")

	dataNote := " Its PVC (tracking database, model registry and artifacts) is garbage-collected with it."
	if pvcErr == nil && len(pvcs) > 0 {
		dataNote = fmt.Sprintf(" %s %v (tracking database, model registry and artifacts) %s garbage-collected with it.", kube.Verb(len(pvcs), "PVC", "PVCs"), pvcs, kube.Verb(len(pvcs), "is", "are"))
	}
	slog.Info("mlflow teardown", "user", kube.GetUser(c))
	record.Action(c, "teardown-mlflow", "MLflow CR deleted", true)

	gone, waitErr := kube.WaitForDeletion(c, crPath, deleteTimeout, deletePoll)
	if waitErr != nil {
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("MLflow deletion was requested, but whether the CR is gone cannot be checked: %v.%s", waitErr, dataNote),
			Logs: logs, ErrorCode: kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(waitErr), "in_progress"),
		}, nil
	}
	if !gone {
		note := ""
		if latest, err := ReadCR(c); err == nil {
			note = mlflowFinalizerNote(latest)
		}
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("MLflow deletion was requested but the CR still exists after %s.%s Re-run teardown to check again.%s", deleteTimeout, note, dataNote),
			Logs: logs, ErrorCode: "in_progress",
		}, nil
	}
	return &types.OperationResponse{Success: true, Message: "MLflow removed." + dataNote, Logs: logs}, nil
}

// patchMLflowCR merge-patches the CR, guarded by resourceVersion so a
// concurrent change is reported instead of overwritten. spec nil leaves the
// spec alone.
func patchMLflowCR(c *kube.Client, cr mlflowCR, annotations, spec map[string]any) error {
	meta := map[string]any{"resourceVersion": cr.Metadata.ResourceVersion}
	if len(annotations) > 0 {
		meta["annotations"] = annotations
	}
	body := map[string]any{"metadata": meta}
	if spec != nil {
		body["spec"] = spec
	}
	return c.PatchJSON(rhoai.ClusterPath(APIGroup, "mlflows", CRName), body)
}

// imageSpec is the spec patch that sets spec.image.image (nil removes it).
func imageSpec(image any) map[string]any {
	return map[string]any{"image": map[string]any{"image": image}}
}

// DeployPR points the MLflow CR at the PR image, pinned by digest. The
// image in place before the first PR deploy is saved for Revert.
func DeployPR(c *kube.Client, prNumber int) (*types.OperationResponse, error) {
	logs := []string{}
	fail := func(msg, code, activity string) (*types.OperationResponse, error) {
		record.Action(c, "deploy-mlflow-pr", fmt.Sprintf("PR #%d (%s)", prNumber, activity), false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}

	if prNumber <= 0 {
		return &types.OperationResponse{
			Success: false, Message: "PR number must be greater than 0",
			Logs: logs, ErrorCode: "validation",
		}, nil
	}

	cr, err := ReadCR(c)
	if kube.IsK8sNotFound(err) {
		return fail("MLflow is not deployed. Set it up first.", "prerequisites", "not deployed")
	}
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the MLflow CR: %v", err), kube.ErrorCodeFromK8sErr(err), "CR read failed")
	}
	if cr.Metadata.Terminating() {
		return fail("MLflow is being deleted.", "terminating", "terminating")
	}

	tag := fmt.Sprintf("odh-pr-%d", prNumber)
	logs = append(logs, fmt.Sprintf("Resolving quay.io/%s:%s...", mlflowImageRepo, tag))
	image, found, err := resolveMLflowPRImage(c.Context(), prNumber)
	if err != nil {
		return fail(fmt.Sprintf("Cannot verify the PR image on Quay: %v. No changes were made.", err), "network", "resolve failed")
	}
	if !found {
		return fail(fmt.Sprintf("PR image not found: %s. Ensure the PR has a published image.", tag), "validation", "image not found")
	}
	logs = append(logs, fmt.Sprintf("Target image: %s", image))

	if cr.image() == image {
		logs = append(logs, "MLflow already runs this build")
		record.Action(c, "deploy-mlflow-pr", fmt.Sprintf("PR #%d (%s, unchanged)", prNumber, image), true)
		return &types.OperationResponse{Success: true, Message: fmt.Sprintf("MLflow already runs the latest PR #%d build.", prNumber), Logs: logs}, nil
	}

	annotations := map[string]any{}
	// A saved image is replaced when no PR image runs now: someone set the
	// current image after the last PR deploy, and Revert restores that one.
	if _, saved := cr.Metadata.Annotations[OriginalImageAnnotation]; !saved || cr.staleOriginal() {
		original := cr.image()
		if prNumberFromImage(original) > 0 {
			// A PR image set by an older version: the original is unknown,
			// so Revert returns to the operator default.
			original = ""
		}
		annotations[OriginalImageAnnotation] = original
	}
	if err := patchMLflowCR(c, cr, annotations, imageSpec(image)); err != nil {
		code := kube.ErrorCodeFromK8sErr(err)
		msg := fmt.Sprintf("Failed to patch MLflow CR: %v", err)
		if kube.IsK8sConflict(err) {
			code = "conflict"
			msg = "MLflow changed while the PR was being deployed. Nothing was changed; refresh and retry."
		}
		return fail(msg, code, "patch failed")
	}
	logs = append(logs, "OK: MLflow CR patched with PR image")

	slog.Info("mlflow PR deployed", "pr", prNumber, "image", image, "user", kube.GetUser(c))
	record.Action(c, "deploy-mlflow-pr", fmt.Sprintf("PR #%d (%s)", prNumber, image), true)

	return &types.OperationResponse{
		Success: true, Message: fmt.Sprintf("MLflow PR #%d image deployed.", prNumber),
		Logs: logs,
	}, nil
}

// RevertImage restores the image saved by the first PR deploy, or, when
// none was saved, removes spec.image.image so the operator default applies.
func RevertImage(c *kube.Client) (*types.OperationResponse, error) {
	logs := []string{}
	fail := func(msg, code, activity string) (*types.OperationResponse, error) {
		record.Action(c, "revert-mlflow", activity, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}

	cr, err := ReadCR(c)
	if kube.IsK8sNotFound(err) {
		return fail("MLflow is not deployed.", "prerequisites", "not deployed")
	}
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the MLflow CR: %v", err), kube.ErrorCodeFromK8sErr(err), "CR read failed")
	}
	active, revertImage := cr.prOverride()
	if !active {
		msg := "No PR image is active, so there is nothing to revert."
		if cr.staleOriginal() {
			// The image was set by hand after the PR deploy: keep it, and drop
			// the saved image so nothing restores it later.
			if err := patchMLflowCR(c, cr, map[string]any{OriginalImageAnnotation: nil}, nil); err != nil {
				return fail(fmt.Sprintf("No PR image is active; the image saved before the PR deploy could not be dropped: %v", err), kube.ErrorCodeFromK8sErr(err), "no override, stale annotation kept")
			}
			current := cr.image()
			if current == "" {
				current = "the operator default"
			}
			msg = fmt.Sprintf("No PR image is active: MLflow runs %s, set after the PR deploy, so nothing was reverted. The image saved before the PR deploy no longer applies and was dropped.", current)
		}
		return fail(msg, "validation", "no override")
	}

	var image any // nil removes spec.image.image
	target := "the operator default image"
	if revertImage != "" {
		image = revertImage
		target = revertImage
	}
	if err := patchMLflowCR(c, cr, map[string]any{OriginalImageAnnotation: nil}, imageSpec(image)); err != nil {
		code := kube.ErrorCodeFromK8sErr(err)
		msg := fmt.Sprintf("Failed to revert MLflow image: %v", err)
		if kube.IsK8sConflict(err) {
			code = "conflict"
			msg = "MLflow changed during the revert. Nothing was changed; refresh and retry."
		}
		return fail(msg, code, fmt.Sprintf("patch failed: %v", err))
	}
	logs = append(logs, fmt.Sprintf("OK: Image reverted to %s", target))

	slog.Info("mlflow image reverted", "target", target, "user", kube.GetUser(c))
	record.Action(c, "revert-mlflow", target, true)

	return &types.OperationResponse{
		Success: true, Message: fmt.Sprintf("MLflow image reverted to %s.", target),
		Logs: logs,
	}, nil
}

// mlflowFinalizerNote names the finalizers holding a deleting MLflow CR. The
// mlflow-operator (rhoai-3.6) sets none on MLflow CRs, so any finalizer here
// belongs to someone else and needs its owner running.
func mlflowFinalizerNote(cr mlflowCR) string {
	if len(cr.Metadata.Finalizers) == 0 {
		return ""
	}
	return fmt.Sprintf(" It is waiting on %s %s; the controller that added %s must be running to remove %s.", kube.Verb(len(cr.Metadata.Finalizers), "finalizer", "finalizers"), strings.Join(cr.Metadata.Finalizers, ", "), kube.Verb(len(cr.Metadata.Finalizers), "it", "them"), kube.Verb(len(cr.Metadata.Finalizers), "it", "them"))
}
