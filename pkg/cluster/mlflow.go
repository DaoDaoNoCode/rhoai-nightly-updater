package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	mlflowNamespace = "redhat-ods-applications"
	mlflowCRName    = "mlflow"
	mlflowAPIGroup  = "mlflow.opendatahub.io/v1"
	mlflowImageRepo = "opendatahub/mlflow"
	// mlflowPodSelector matches the MLflow server pods; the operator's helm
	// chart labels them app=mlflow<resourceSuffix> (charts/mlflow/templates/
	// deployment.yaml), and the suffix is empty for the singleton "mlflow".
	mlflowPodSelector = "app=mlflow"
	// mlflowOriginalImageAnnotation records spec.image.image as it was before
	// the first PR deploy ("" when unset, meaning the operator default), so
	// Revert restores exactly that. The mlflow-operator only ever removes its
	// own force-migrate annotation (internal/controller/migration.go), so
	// this annotation is left alone.
	mlflowOriginalImageAnnotation = "rhoai-nightly-updater/original-image"
)

var (
	// MLflowDeleteTimeout bounds how long teardown waits for the CR to go.
	// The MLflow CR has no finalizer (mlflow-operator rhoai-3.6), so this
	// normally returns at once.
	MLflowDeleteTimeout = 15 * time.Second
	MLflowDeletePoll    = time.Second

	mlflowPRTagPattern = regexp.MustCompile(`:odh-pr-(\d+)(?:@|$)`)

	// resolveMLflowPRImage resolves quay.io/opendatahub/mlflow:odh-pr-N to
	// "repo:tag@digest". odh-pr-N moves with every new PR commit, so pinning
	// the digest makes a redeploy change spec.image.image and roll out the
	// new build. CRI-O accepts tag@digest references and pulls by digest
	// (cri-o internal/storage/image_test.go "with tag and digest"). Tests
	// replace it.
	resolveMLflowPRImage = func(ctx context.Context, pr int) (string, bool, error) {
		return resolveDashboardBuild(ctx, mlflowImageRepo, fmt.Sprintf("odh-pr-%d", pr))
	}
)

type mlflowCR struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Image *struct {
			Image *string `json:"image"`
		} `json:"image"`
	} `json:"spec"`
	Status struct {
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
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
	return m.Metadata.hasToolLabel() || m.Metadata.createdByToolApply()
}

func prNumberFromImage(image string) int {
	if m := mlflowPRTagPattern.FindStringSubmatch(image); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// prOverride reports whether a PR image is active and what Revert restores
// ("" means removing spec.image.image so the operator default applies).
func (m mlflowCR) prOverride() (active bool, revertImage string) {
	if orig, ok := m.Metadata.Annotations[mlflowOriginalImageAnnotation]; ok {
		return true, orig
	}
	return prNumberFromImage(m.image()) > 0, ""
}

func getMLflowCR(c *Client) (mlflowCR, error) {
	body, _, err := c.get(clusterPath(mlflowAPIGroup, "mlflows", mlflowCRName))
	if err != nil {
		return mlflowCR{}, err
	}
	var cr mlflowCR
	if err := json.Unmarshal(body, &cr); err != nil {
		return mlflowCR{}, fmt.Errorf("parse MLflow CR: %w", err)
	}
	return cr, nil
}

// mlflowOwnedPVCs lists PVCs in the MLflow namespace whose owner is the CR,
// which the garbage collector deletes together with it.
func mlflowOwnedPVCs(c *Client, uid string) ([]string, error) {
	body, _, err := c.get(namespacedPath("v1", "persistentvolumeclaims", mlflowNamespace, ""))
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata objectMeta `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	var names []string
	for _, item := range list.Items {
		if item.Metadata.ownedBy(uid) {
			names = append(names, item.Metadata.Name)
		}
	}
	return names, nil
}

func getMLflowStatus(c *Client) types.ResourceState {
	state := types.ResourceState{Namespace: mlflowNamespace}

	cr, err := getMLflowCR(c)
	if IsK8sError(err, 404) {
		state.Message = "Not deployed"
		return state
	}
	if err != nil {
		state.Message = "Cannot read MLflow status: " + err.Error()
		return state
	}
	state.Deployed = true
	state.ManagedByTool = cr.managedByTool()
	state.Terminating = cr.Metadata.terminating()
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
				state.Message = firstNonEmpty(truncateMessage(cond.Message), cond.Reason, "Not ready")
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

	var (
		wg    sync.WaitGroup
		host  string
		issue workloadPodIssue
		pvcs  []string
	)
	wg.Add(3)
	go func() { defer wg.Done(); host = routeHost(c, mlflowNamespace, "mlflow") }()
	go func() {
		defer wg.Done()
		if !state.Ready && !state.Terminating {
			issue, _ = inspectPods(c, mlflowNamespace, mlflowPodSelector, "")
		}
	}()
	go func() {
		defer wg.Done()
		if state.ManagedByTool {
			pvcs, _ = mlflowOwnedPVCs(c, cr.Metadata.UID)
		}
	}()
	wg.Wait()
	if host != "" {
		state.UIRoute = "https://" + host
	}
	applyPodIssue(&state, issue)
	state.DataPVCs = pvcs
	return state
}

func recordMLflowActivity(c *Client, action, detail string, success bool) {
	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    action,
		Detail:    detail,
		Success:   success,
	})
}

// SetupMLflow creates the MLflow CR in redhat-ods-applications. It leaves
// spec.image unset so the operator deploys its platform default
// (RELATED_IMAGE_ODH_MLFLOW_IMAGE, the RHOAI build, falling back to
// MLFLOW_IMAGE; mlflow-operator internal/config/config.go and
// internal/controller/helm.go).
func SetupMLflow(c *Client) (*types.OperationResponse, error) {
	logs := []string{}

	existing := getMLflowStatus(c)
	if existing.Deployed {
		return &types.OperationResponse{
			Success: false, Message: "MLflow is already deployed.",
			Logs: logs, ErrorCode: "validation",
		}, nil
	}

	logs = append(logs, "Creating MLflow CR...")
	mlflowCR := map[string]interface{}{
		"apiVersion": mlflowAPIGroup,
		"kind":       "MLflow",
		"metadata": map[string]interface{}{
			"name":   mlflowCRName,
			"labels": toolLabels(),
		},
		"spec": map[string]interface{}{
			"replicas": 1,
			"migration": map[string]interface{}{
				"mode": "Automatic",
			},
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{"cpu": "500m", "memory": "1Gi"},
				"limits":   map[string]interface{}{"cpu": "2", "memory": "2Gi"},
			},
			"storage": map[string]interface{}{
				"accessModes": []string{"ReadWriteOnce"},
				"resources": map[string]interface{}{
					"requests": map[string]interface{}{"storage": "10Gi"},
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
	if _, _, err := c.post(withToolFieldManager(clusterPath(mlflowAPIGroup, "mlflows", "")), data); err != nil {
		msg := fmt.Sprintf("Failed to create MLflow CR: %v", err)
		code := errorCodeFromK8sErr(err)
		switch {
		case IsK8sError(err, 404):
			msg = "The MLflow CRD is not installed. Set the mlflowoperator component to Managed in the DataScienceCluster first."
			code = "prerequisites"
		case IsK8sError(err, 409):
			msg = "MLflow was created by someone else in the meantime. Refresh the status."
			code = "conflict"
		}
		recordMLflowActivity(c, "setup-mlflow", fmt.Sprintf("CR creation failed: %v", err), false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}
	logs = append(logs, "OK: MLflow CR created")

	slog.Info("mlflow setup", "user", getUser(c))
	recordMLflowActivity(c, "setup-mlflow", "MLflow CR created in "+mlflowNamespace, true)

	return &types.OperationResponse{
		Success: true, Message: "MLflow CR created. The operator will deploy MLflow shortly.",
		Logs: logs,
	}, nil
}

// TeardownMLflow deletes the MLflow CR if this tool created it. The operator
// owns mlflow-pvc through the CR, so the garbage collector deletes the
// tracking database and artifacts with it.
func TeardownMLflow(c *Client) (*types.OperationResponse, error) {
	logs := []string{}
	fail := func(msg, code, activity string) (*types.OperationResponse, error) {
		recordMLflowActivity(c, "teardown-mlflow", activity, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}

	cr, err := getMLflowCR(c)
	if IsK8sError(err, 404) {
		logs = append(logs, "MLflow CR already absent")
		return &types.OperationResponse{Success: true, Message: "MLflow is not deployed; nothing to remove.", Logs: logs}, nil
	}
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the MLflow CR; nothing was deleted: %v", err), errorCodeFromK8sErr(err), fmt.Sprintf("CR read failed: %v", err))
	}
	if !cr.managedByTool() {
		return fail("This MLflow instance was not created by this tool, so it was not deleted.", "not_managed", "not managed")
	}
	if cr.Metadata.terminating() {
		logs = append(logs, "MLflow CR is already being deleted")
		return &types.OperationResponse{Success: false, Message: "MLflow is still being deleted." + mlflowFinalizerNote(cr), Logs: logs, ErrorCode: "in_progress"}, nil
	}

	pvcs, pvcErr := mlflowOwnedPVCs(c, cr.Metadata.UID)
	logs = append(logs, "Deleting MLflow CR...")
	crPath := clusterPath(mlflowAPIGroup, "mlflows", mlflowCRName)
	if _, delErr := deleteWithUID(c, crPath, cr.Metadata.UID); delErr != nil && !IsK8sError(delErr, 404) {
		code := errorCodeFromK8sErr(delErr)
		if IsK8sError(delErr, 409) {
			code = "conflict"
		}
		return fail(fmt.Sprintf("Failed to delete MLflow CR: %v", delErr), code, fmt.Sprintf("CR delete failed: %v", delErr))
	}
	logs = append(logs, "OK: MLflow CR deleted")

	dataNote := " Its PVC (tracking database, model registry and artifacts) is garbage-collected with it."
	if pvcErr == nil && len(pvcs) > 0 {
		dataNote = fmt.Sprintf(" %s %v (tracking database, model registry and artifacts) %s garbage-collected with it.", verb(len(pvcs), "PVC", "PVCs"), pvcs, verb(len(pvcs), "is", "are"))
	}
	slog.Info("mlflow teardown", "user", getUser(c))
	recordMLflowActivity(c, "teardown-mlflow", mlflowNamespace, true)

	gone, waitErr := waitForDeletion(c, crPath, MLflowDeleteTimeout, MLflowDeletePoll)
	if waitErr != nil {
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("MLflow deletion was requested, but whether the CR is gone cannot be checked: %v.%s", waitErr, dataNote),
			Logs: logs, ErrorCode: firstNonEmpty(errorCodeFromK8sErr(waitErr), "in_progress"),
		}, nil
	}
	if !gone {
		note := ""
		if latest, err := getMLflowCR(c); err == nil {
			note = mlflowFinalizerNote(latest)
		}
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("MLflow deletion was requested but the CR still exists after %s.%s Re-run teardown to check again.%s", MLflowDeleteTimeout, note, dataNote),
			Logs: logs, ErrorCode: "in_progress",
		}, nil
	}
	return &types.OperationResponse{Success: true, Message: "MLflow removed." + dataNote, Logs: logs}, nil
}

// patchMLflowCR merge-patches the CR, guarded by resourceVersion so a
// concurrent change is reported instead of overwritten.
func patchMLflowCR(c *Client, cr mlflowCR, annotations map[string]interface{}, image interface{}) error {
	meta := map[string]interface{}{"resourceVersion": cr.Metadata.ResourceVersion}
	if len(annotations) > 0 {
		meta["annotations"] = annotations
	}
	patch, _ := json.Marshal(map[string]interface{}{
		"metadata": meta,
		"spec":     map[string]interface{}{"image": map[string]interface{}{"image": image}},
	})
	_, _, err := c.patch(clusterPath(mlflowAPIGroup, "mlflows", mlflowCRName), patch)
	return err
}

// DeployMLflowPR points the MLflow CR at the PR image, pinned by digest. The
// image in place before the first PR deploy is saved for Revert.
func DeployMLflowPR(c *Client, prNumber int) (*types.OperationResponse, error) {
	logs := []string{}
	fail := func(msg, code, activity string) (*types.OperationResponse, error) {
		recordMLflowActivity(c, "deploy-mlflow-pr", fmt.Sprintf("PR #%d (%s)", prNumber, activity), false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}

	if prNumber <= 0 {
		return &types.OperationResponse{
			Success: false, Message: "PR number must be greater than 0",
			Logs: logs, ErrorCode: "validation",
		}, nil
	}

	cr, err := getMLflowCR(c)
	if IsK8sError(err, 404) {
		return fail("MLflow is not deployed. Set it up first.", "prerequisites", "not deployed")
	}
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the MLflow CR: %v", err), errorCodeFromK8sErr(err), "CR read failed")
	}
	if cr.Metadata.terminating() {
		return fail("MLflow is being deleted.", "terminating", "terminating")
	}

	tag := fmt.Sprintf("odh-pr-%d", prNumber)
	logs = append(logs, fmt.Sprintf("Resolving quay.io/%s:%s...", mlflowImageRepo, tag))
	image, found, err := resolveMLflowPRImage(c.ctx, prNumber)
	if err != nil {
		return fail(fmt.Sprintf("Cannot verify the PR image on Quay: %v. No changes were made.", err), "network", "resolve failed")
	}
	if !found {
		return fail(fmt.Sprintf("PR image not found: %s. Ensure the PR has a published image.", tag), "validation", "image not found")
	}
	logs = append(logs, fmt.Sprintf("Target image: %s", image))

	if cr.image() == image {
		logs = append(logs, "MLflow already runs this build")
		recordMLflowActivity(c, "deploy-mlflow-pr", fmt.Sprintf("PR #%d (%s, unchanged)", prNumber, image), true)
		return &types.OperationResponse{Success: true, Message: fmt.Sprintf("MLflow already runs the latest PR #%d build.", prNumber), Logs: logs}, nil
	}

	annotations := map[string]interface{}{}
	if _, saved := cr.Metadata.Annotations[mlflowOriginalImageAnnotation]; !saved {
		original := cr.image()
		if prNumberFromImage(original) > 0 {
			// A PR image set by an older version: the original is unknown,
			// so Revert returns to the operator default.
			original = ""
		}
		annotations[mlflowOriginalImageAnnotation] = original
	}
	if err := patchMLflowCR(c, cr, annotations, image); err != nil {
		code := errorCodeFromK8sErr(err)
		msg := fmt.Sprintf("Failed to patch MLflow CR: %v", err)
		if IsK8sError(err, 409) {
			code = "conflict"
			msg = "MLflow changed while the PR was being deployed. Nothing was changed; refresh and retry."
		}
		return fail(msg, code, "patch failed")
	}
	logs = append(logs, "OK: MLflow CR patched with PR image")

	slog.Info("mlflow PR deployed", "pr", prNumber, "image", image, "user", getUser(c))
	recordMLflowActivity(c, "deploy-mlflow-pr", fmt.Sprintf("PR #%d (%s)", prNumber, image), true)

	return &types.OperationResponse{
		Success: true, Message: fmt.Sprintf("MLflow PR #%d image deployed.", prNumber),
		Logs: logs,
	}, nil
}

// RevertMLflowImage restores the image saved by the first PR deploy, or, when
// none was saved, removes spec.image.image so the operator default applies.
func RevertMLflowImage(c *Client) (*types.OperationResponse, error) {
	logs := []string{}
	fail := func(msg, code, activity string) (*types.OperationResponse, error) {
		recordMLflowActivity(c, "revert-mlflow", activity, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}

	cr, err := getMLflowCR(c)
	if IsK8sError(err, 404) {
		return fail("MLflow is not deployed.", "prerequisites", "not deployed")
	}
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the MLflow CR: %v", err), errorCodeFromK8sErr(err), "CR read failed")
	}
	active, revertImage := cr.prOverride()
	if !active {
		return fail("No PR image is active, so there is nothing to revert.", "validation", "no override")
	}

	var image interface{} // nil removes spec.image.image
	target := "the operator default image"
	if revertImage != "" {
		image = revertImage
		target = revertImage
	}
	if err := patchMLflowCR(c, cr, map[string]interface{}{mlflowOriginalImageAnnotation: nil}, image); err != nil {
		code := errorCodeFromK8sErr(err)
		msg := fmt.Sprintf("Failed to revert MLflow image: %v", err)
		if IsK8sError(err, 409) {
			code = "conflict"
			msg = "MLflow changed during the revert. Nothing was changed; refresh and retry."
		}
		return fail(msg, code, fmt.Sprintf("patch failed: %v", err))
	}
	logs = append(logs, fmt.Sprintf("OK: Image reverted to %s", target))

	slog.Info("mlflow image reverted", "target", target, "user", getUser(c))
	recordMLflowActivity(c, "revert-mlflow", target, true)

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
	return fmt.Sprintf(" It is waiting on %s %s; the controller that added %s must be running to remove %s.", verb(len(cr.Metadata.Finalizers), "finalizer", "finalizers"), strings.Join(cr.Metadata.Finalizers, ", "), verb(len(cr.Metadata.Finalizers), "it", "them"), verb(len(cr.Metadata.Finalizers), "it", "them"))
}
