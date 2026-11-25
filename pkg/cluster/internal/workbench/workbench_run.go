package workbench

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

var projectName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// When req cannot run, Validate states the reason; otherwise it returns "".
func Validate(req types.WorkbenchRequest) string {
	switch req.Action {
	case "setup", "start", "stop", "teardown", "delete-storage":
	default:
		return "action must be setup, start, stop, teardown or delete-storage"
	}
	if len(req.Project) > 63 || !projectName.MatchString(req.Project) {
		return "a valid project is required"
	}
	if req.DeleteStorage && req.Action != "teardown" {
		return "deleteStorage goes with teardown"
	}
	return ""
}

// Teardown blocks at most this long while a Notebook's finalizers finish;
// odh-notebook-controller usually clears them within seconds—tests use a shorter cap.
var GoneWait = 2 * time.Minute

type run struct {
	c        *kube.Client
	emit     func(types.UpdateStepEvent)
	logs     []string
	activity string
	target   string
}

func (r *run) step(id, status, msg string) {
	r.logs = append(r.logs, msg)
	if r.emit != nil {
		r.emit(types.UpdateStepEvent{Step: id, Status: status, Message: msg})
	}
}

func (r *run) fail(id, msg, code, why string) (*types.OperationResponse, error) {
	if r.emit != nil {
		r.emit(types.UpdateStepEvent{Step: id, Status: "failed", Message: msg, ErrorCode: code})
	}
	r.logs = append(r.logs, msg)
	slog.Warn("workbench operation stopped", "step", id, "reason", why, "user", kube.GetUser(r.c))
	record.Action(r.c, r.activity, fmt.Sprintf("%s (%s)", r.target, why), false)
	return &types.OperationResponse{Success: false, Message: msg, Logs: r.logs, ErrorCode: code}, nil
}

func (r *run) done(msg string) (*types.OperationResponse, error) {
	record.Action(r.c, r.activity, r.target, true)
	return &types.OperationResponse{Success: true, Message: msg, Logs: r.logs}, nil
}

// Run performs setup, start, stop, or delete on the project workbench in a
// single streamed operation: setup runs "check", "storage", "workbench";
// start and stop only "workbench"; teardown runs "workbench" then "storage"
// when DeleteStorage is set. Client c uses the signed-in user's token.
func Run(c *kube.Client, req types.WorkbenchRequest, onStep func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	r := &run{c: c, emit: onStep, activity: req.Action + "-workbench", target: "workbench in " + req.Project}
	if msg := Validate(req); msg != "" {
		return r.fail("check", msg, "validation", "invalid request")
	}
	switch req.Action {
	case "setup":
		return r.setup(req.Project)
	case "start":
		return r.start(req.Project)
	case "stop":
		return r.stop(req.Project)
	case "delete-storage":
		return r.deleteKeptStorage(req.Project)
	}
	return r.teardown(req.Project, req.DeleteStorage)
}

// readOwned loads the project workbench and ownership record, returning a
// step failure when the notebook is missing or not managed by this tool.
func (r *run) readOwned(id, project string) (Notebook, *types.OperationResponse) {
	nb, found, err := readNotebook(r.c, project)
	if err != nil {
		res, _ := r.fail(id, fmt.Sprintf("Cannot read workbench %s in %s: %v.", Name, project, err), kube.ErrorCodeFromK8sErr(err), "read failed")
		return nb, res
	}
	if !found {
		res, _ := r.fail(id, fmt.Sprintf("Project %s has no workbench %s.", project, Name), "validation", "absent")
		return nb, res
	}
	rec, err := ReadRecord(r.c)
	if err != nil {
		res, _ := r.fail(id, err.Error()+" Nothing was changed.", kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal"), "record read failed")
		return nb, res
	}
	if !rec.OwnsNotebook(nb.Metadata) {
		res, _ := r.fail(id, notOurs(project), "not_managed", "not this tool's")
		return nb, res
	}
	if nb.Metadata.Terminating() {
		res, _ := r.fail(id, fmt.Sprintf("Workbench %s in %s is being deleted.", Name, project), "terminating", "terminating")
		return nb, res
	}
	return nb, nil
}

func notOurs(project string) string {
	return fmt.Sprintf("Workbench %s in %s was not created by this tool (or was deleted and made again), so this tool does not change or delete it. Use it in the dashboard.", Name, project)
}

func (r *run) setup(project string) (*types.OperationResponse, error) {
	c := r.c
	r.step("check", "running", fmt.Sprintf("Checking what a workbench in %s needs", project))
	dsc, err := rhoai.ReadFirstDSC(c)
	if err != nil {
		return r.fail("check", fmt.Sprintf("Cannot read the DataScienceCluster: %v. Nothing was changed.", err), kube.ErrorCodeFromK8sErr(err), "dsc read failed")
	}
	if b := envkit.ComponentBlock(dsc, Component); b != nil {
		return r.fail("check", b.Reason+" Nothing was changed.", "prerequisites", "component off")
	}
	if why := Unsupported(dsc); why != "" {
		return r.fail("check", why+" Nothing was changed.", "unsupported", "rhoai 2.x")
	}
	rec, err := ReadRecord(c)
	if err != nil {
		return r.fail("check", err.Error()+" Nothing was changed.", kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal"), "record read failed")
	}
	nb, nbFound, err := readNotebook(c, project)
	if err != nil {
		return r.fail("check", fmt.Sprintf("Cannot read workbench %s in %s: %v. Nothing was changed.", Name, project, err), kube.ErrorCodeFromK8sErr(err), "read failed")
	}
	if nbFound {
		if !rec.OwnsNotebook(nb.Metadata) {
			return r.fail("check", notOurs(project)+" Nothing was changed.", "not_managed", "not this tool's")
		}
		r.step("check", "success", "The workbench is already set up.")
		// The internal registry's copy of the token, which an existing
		// workbench needs to pull its image again (ROSA HCP).
		copyLogs := registry.EnsureRegistryPullCopyFromPullSecret(c)
		r.logs = append(r.logs, copyLogs...)
		if registry.CopyChanged(copyLogs) {
			return r.done(fmt.Sprintf("Workbench %s in %s is already set up; the token was copied for the internal registry, which pulls its image.", Name, project))
		}
		return r.done(fmt.Sprintf("Workbench %s in %s is already set up; nothing was changed.", Name, project))
	}
	pvc, pvcFound, err := readStorage(c, project)
	if err != nil {
		return r.fail("check", fmt.Sprintf("Cannot read PersistentVolumeClaim %s in %s: %v. Nothing was changed.", StorageName, project, err), kube.ErrorCodeFromK8sErr(err), "read failed")
	}
	switch {
	case pvcFound && !rec.OwnsStorage(pvc.Metadata):
		return r.fail("check", fmt.Sprintf("PersistentVolumeClaim %s in %s already exists and was not created by this tool, so it is not used for the workbench. Delete or rename it in the dashboard, then set up again. Nothing was changed.", StorageName, project), "not_managed", "foreign storage")
	case pvcFound && pvc.Metadata.Terminating():
		return r.fail("check", fmt.Sprintf("PersistentVolumeClaim %s in %s is being deleted; set up again once it is gone. Nothing was changed.", StorageName, project), "terminating", "storage terminating")
	}
	if pvcFound {
		if msg, code := r.keptStorageInUse(project, rec); msg != "" {
			return r.fail("check", msg+" Nothing was changed.", code, "kept storage in use")
		}
	}
	plan, msg, code := BuildPlan(c, project, dsc)
	if msg != "" {
		return r.fail("check", msg+" Nothing was changed.", code, "plan failed")
	}
	pvcObj, nbObj := PVCManifest(plan), NotebookManifest(plan)
	if !pvcFound {
		if _, err := create(c, rhoai.NamespacedPath("v1", "persistentvolumeclaims", project, ""), pvcObj, true); err != nil {
			return r.fail("check", fmt.Sprintf("The API server rejects PersistentVolumeClaim %s: %v. Nothing was changed.", StorageName, err), kube.ErrorCodeFromK8sErr(err), "storage dry run failed")
		}
	}
	// The Notebook's dry run goes through odh-notebook-controller's
	// webhook, which (as for the dashboard's own dry run and for a real
	// create) makes ConfigMaps workbench-trusted-ca-bundle and
	// pipeline-runtime-images and Secret ds-pipeline-config in the
	// project when they are missing: SECURITY.md, "Workbench".
	dry, err := create(c, rhoai.NamespacedPath(notebookAPI, "notebooks", project, ""), nbObj, true)
	if err != nil {
		return r.fail("check", fmt.Sprintf("The API server rejects workbench %s: %v. No workbench or storage was created.%s", Name, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "workbench dry run failed")
	}
	// A passing dry run hits every Notebook webhook; under RHOAI 3 the
	// controller adds kube-rbac-proxy for inject-auth—without that container
	// serving falls back to the 2.x path and the tool fails closed.
	if !hasContainer(dry, kubeRBACProxy) {
		return r.fail("check", "This cluster's workbench controller does not add kube-rbac-proxy to a workbench (it is not RHOAI 3's), so this tool does not set one up. Create a workbench in the dashboard instead. No workbench or storage was created.", "unsupported", "no kube-rbac-proxy")
	}
	r.step("check", "success", describe(plan))

	if pvcFound {
		r.step("storage", "success", fmt.Sprintf("Using the storage this tool kept: %s.", StorageName))
	} else {
		r.step("storage", "running", fmt.Sprintf("Creating storage %s (%s)", StorageName, plan.StorageSize))
		body, err := create(c, rhoai.NamespacedPath("v1", "persistentvolumeclaims", project, ""), pvcObj, false)
		if err != nil {
			return r.fail("storage", fmt.Sprintf("Cannot create PersistentVolumeClaim %s in %s: %v. Nothing was changed.", StorageName, project, err), kube.ErrorCodeFromK8sErr(err), "storage create failed")
		}
		meta, _ := kube.ParseObjectMeta(body)
		if err := updateRecord(c, project, func(e *Recorded) { e.StorageUID, e.At, e.By = meta.UID, kube.NowRFC3339(), kube.GetUser(c) }); err != nil {
			if _, delErr := kube.DeleteWithUID(c, storagePath(project), meta.UID); delErr != nil && !kube.IsK8sNotFound(delErr) {
				return r.fail("storage", fmt.Sprintf("PersistentVolumeClaim %s was created, but this tool could not record it (%v) nor delete it again (%v). Delete it yourself (oc delete pvc %s -n %s), then set up again.", StorageName, err, delErr, StorageName, project), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal"), "record failed")
			}
			return r.fail("storage", fmt.Sprintf("PersistentVolumeClaim %s was created, but this tool could not record it as its own (%v), so it was deleted again. Set up again to retry.", StorageName, err), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal"), "record failed")
		}
		r.step("storage", "success", fmt.Sprintf("Created storage %s.", StorageName))
	}

	// Again right before the create: a workbench that mounted the kept
	// storage in the meantime keeps it (fail closed).
	if pvcFound {
		if msg, code := r.keptStorageInUse(project, rec); msg != "" {
			return r.fail("workbench", msg+" No workbench was created.", code, "kept storage in use")
		}
	}
	// ROSA HCP: the workbench image is pulled through the internal
	// registry, which needs its own copy of the quay.io/rhoai token
	// (registry.EnsureRegistryPullCopyFromPullSecret); a problem with it is
	// a warning in the logs, the workbench is created anyway.
	r.logs = append(r.logs, registry.EnsureRegistryPullCopyFromPullSecret(c)...)
	r.step("workbench", "running", fmt.Sprintf("Creating workbench %s", Name))
	body, err := create(c, rhoai.NamespacedPath(notebookAPI, "notebooks", project, ""), nbObj, false)
	if err != nil {
		return r.fail("workbench", fmt.Sprintf("Cannot create workbench %s in %s: %v. Its storage %s stays; set up again to retry.", Name, project, err, StorageName), kube.ErrorCodeFromK8sErr(err), "create failed")
	}
	meta, _ := kube.ParseObjectMeta(body)
	if err := updateRecord(c, project, func(e *Recorded) { e.UID, e.At, e.By = meta.UID, kube.NowRFC3339(), kube.GetUser(c) }); err != nil {
		if _, delErr := kube.DeleteWithUID(c, notebookPath(project), meta.UID); delErr != nil && !kube.IsK8sNotFound(delErr) {
			return r.fail("workbench", fmt.Sprintf("Workbench %s was created, but this tool could not record it (%v) nor delete it again (%v). Delete it in the dashboard, then set up again.", Name, err, delErr), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal"), "record failed")
		}
		return r.fail("workbench", fmt.Sprintf("Workbench %s was created, but this tool could not record it as its own (%v), so it was deleted again. Set up again to retry.", Name, err), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal"), "record failed")
	}
	r.step("workbench", "success", fmt.Sprintf("Created workbench %s; it is starting (usually 1 to 3 minutes while the image is pulled).", Name))
	slog.Info("workbench setup", "project", project, "user", kube.GetUser(c))
	return r.done(fmt.Sprintf("Workbench %s is starting in %s with %s (%s) on hardware profile %s.%s", Name, project, plan.Image.DisplayName, plan.Image.Tag, plan.Profile.DisplayName, deprecatedNote(plan.Image)))
}

// DeprecatedImageNote says that the image is deprecated, as the dashboard
// labels it, and why this tool chose it anyway.
const DeprecatedImageNote = "The dashboard labels this image Deprecated (it gets no new versions): no current workbench image is ready on this cluster."

// deprecatedNote is " "+DeprecatedImageNote for a deprecated image, else "".
func deprecatedNote(img ImageChoice) string {
	if !img.Deprecated {
		return ""
	}
	return " " + DeprecatedImageNote
}

// describe builds the check-step summary of what setup will provision.
func describe(p Plan) string {
	parts := []string{fmt.Sprintf("Image %s (%s)", p.Image.DisplayName, p.Image.Tag), "hardware profile " + p.Profile.DisplayName, "storage " + p.StorageSize}
	if p.Connection {
		parts = append(parts, "connection "+ConnectionSecret)
	}
	return strings.Join(parts, ", ") + "." + deprecatedNote(p.Image)
}

// create issues a POST for obj—optionally as a server-side dry run when
// dryRun is true—under this tool's field manager, and returns the API body.
func create(c *kube.Client, collection string, obj map[string]any, dryRun bool) ([]byte, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	query := url.Values{"fieldManager": {envkit.ToolFieldManager}}
	if dryRun {
		query.Set("dryRun", "All")
	}
	body, _, err := c.Do(http.MethodPost, collection, "application/json", data, query)
	return body, err
}

// hasContainer reports whether the Notebook parsed from body lists a container called name.
func hasContainer(body []byte, name string) bool {
	var nb Notebook
	if json.Unmarshal(body, &nb) != nil {
		return false
	}
	return slices.ContainsFunc(nb.Spec.Template.Spec.Containers, func(ct struct {
		Name      string `json:"name"`
		Image     string `json:"image"`
		Resources struct {
			Requests map[string]any `json:"requests"`
		} `json:"resources"`
	}) bool {
		return ct.Name == name
	})
}

// patch applies JSON patch operations to the workbench after an initial UID
// test so a resource recreated since the read is left unchanged.
func patch(c *kube.Client, project, uid string, ops []kube.JSONPatchOp) error {
	rawUID, _ := json.Marshal(uid)
	body, err := json.Marshal(append([]kube.JSONPatchOp{{Op: "test", Path: "/metadata/uid", Value: rawUID}}, ops...))
	if err != nil {
		return err
	}
	_, _, err = c.Do(http.MethodPatch, notebookPath(project), "application/json-patch+json", body, url.Values{"fieldManager": {envkit.ToolFieldManager}})
	return err
}

const stoppedPointer = "/metadata/annotations/" + StoppedAnnotation

// StopOps mirrors the dashboard stop patch (api/k8s/notebooks.ts getStopPatch):
// sets the stop annotation to now in ISO 8601 with no fractional seconds.
func StopOps(now time.Time) []kube.JSONPatchOp {
	v, _ := json.Marshal(now.UTC().Format("2006-01-02T15:04:05Z"))
	return []kube.JSONPatchOp{{Op: "add", Path: stoppedPointer, Value: v}}
}

// StartOps mirrors dashboard start (startNotebook, NotebookTableRow.tsx
// L114-122): clears the stop annotation; if pipelines are usable in the project
// but the workbench lacks their settings mount, adds Elyra volume plus
// container 0's mount; with MLflow enabled and no MLflow link on the
// workbench, adds one (getMlflowInstancePatch).
func StartOps(nb Notebook, pipelines, mlflow, profileGone bool) []kube.JSONPatchOp {
	ops := []kube.JSONPatchOp{{Op: "remove", Path: stoppedPointer}}
	// getDeletedHardwareProfilePatches (concepts/hardwareProfiles/utils.ts
	// L81-88): a workbench whose hardware profile no longer exists loses
	// its name and namespace annotations on Start.
	if _, named := nb.Metadata.Annotations[profileNameAnnotation]; named && profileGone {
		ops = append(ops, kube.JSONPatchOp{Op: "remove", Path: "/metadata/annotations/opendatahub.io~1hardware-profile-name"})
		if _, ok := nb.Metadata.Annotations[profileNamespaceAnnotation]; ok {
			ops = append(ops, kube.JSONPatchOp{Op: "remove", Path: "/metadata/annotations/opendatahub.io~1hardware-profile-namespace"})
		}
	}
	has := slices.ContainsFunc(nb.Spec.Template.Spec.Volumes, func(v Volume) bool {
		return v.Secret != nil && v.Secret.SecretName == PipelineSecret
	})
	if pipelines && !has {
		vol, _ := json.Marshal(elyraVolume())
		mount, _ := json.Marshal(elyraMount())
		ops = append(ops,
			kube.JSONPatchOp{Op: "add", Path: "/spec/template/spec/volumes/-", Value: vol},
			kube.JSONPatchOp{Op: "add", Path: "/spec/template/spec/containers/0/volumeMounts/-", Value: mount})
	}
	if _, linked := nb.Metadata.Annotations["opendatahub.io/mlflow-instance"]; mlflow && !linked {
		v, _ := json.Marshal("mlflow")
		ops = append(ops, kube.JSONPatchOp{Op: "add", Path: "/metadata/annotations/opendatahub.io~1mlflow-instance", Value: v})
	}
	return ops
}

func (r *run) stop(project string) (*types.OperationResponse, error) {
	r.step("workbench", "running", fmt.Sprintf("Stopping workbench %s in %s", Name, project))
	nb, res := r.readOwned("workbench", project)
	if res != nil {
		return res, nil
	}
	if stopped(nb.Metadata) {
		r.step("workbench", "success", "The workbench is already stopped.")
		return r.done(fmt.Sprintf("Workbench %s in %s is already stopped.", Name, project))
	}
	if err := patch(r.c, project, nb.Metadata.UID, StopOps(kube.Clock().Now())); err != nil {
		return r.fail("workbench", fmt.Sprintf("Cannot stop workbench %s in %s: %v.", Name, project, err), kube.ErrorCodeFromK8sErr(err), "stop failed")
	}
	r.step("workbench", "success", "Stopped. Its files stay in its storage; Start brings it back.")
	return r.done(fmt.Sprintf("Workbench %s in %s is stopping.", Name, project))
}

func (r *run) start(project string) (*types.OperationResponse, error) {
	r.step("workbench", "running", fmt.Sprintf("Starting workbench %s in %s", Name, project))
	nb, res := r.readOwned("workbench", project)
	if res != nil {
		return res, nil
	}
	if !stopped(nb.Metadata) {
		r.step("workbench", "success", "The workbench is already running.")
		return r.done(fmt.Sprintf("Workbench %s in %s is already running.", Name, project))
	}
	pipelines, err := pipelineConfigExists(r.c, project)
	if err != nil {
		return r.fail("workbench", fmt.Sprintf("Cannot read Secret %s in %s: %v. Nothing was changed.", PipelineSecret, project, err), kube.ErrorCodeFromK8sErr(err), "read failed")
	}
	mlflow := false
	if dsc, err := rhoai.ReadFirstDSC(r.c); err == nil {
		mlflow = componentOn(dsc, "mlflowoperator")
	}
	gone, err := profileGone(r.c, nb)
	if err != nil {
		return r.fail("workbench", fmt.Sprintf("Cannot read the workbench's hardware profile: %v. Nothing was changed.", err), kube.ErrorCodeFromK8sErr(err), "read failed")
	}
	if err := patch(r.c, project, nb.Metadata.UID, StartOps(nb, pipelines, mlflow, gone)); err != nil {
		return r.fail("workbench", fmt.Sprintf("Cannot start workbench %s in %s: %v.", Name, project, err), kube.ErrorCodeFromK8sErr(err), "start failed")
	}
	r.step("workbench", "success", "Starting (usually 1 to 3 minutes).")
	return r.done(fmt.Sprintf("Workbench %s in %s is starting.", Name, project))
}

// teardown removes this tool's workbench via its recorded UID and waits up to
// GoneWait for finalizers; when deleteStorage is set it also removes storage
// only if that PVC is owned here. Objects another actor created are never deleted.
func (r *run) teardown(project string, deleteStorage bool) (*types.OperationResponse, error) {
	c := r.c
	r.step("workbench", "running", fmt.Sprintf("Deleting workbench %s in %s", Name, project))
	rec, err := ReadRecord(c)
	if err != nil {
		return r.fail("workbench", err.Error()+" Nothing was deleted.", kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal"), "record read failed")
	}
	nb, found, err := readNotebook(c, project)
	if err != nil {
		return r.fail("workbench", fmt.Sprintf("Cannot read workbench %s in %s: %v. Nothing was deleted.", Name, project, err), kube.ErrorCodeFromK8sErr(err), "read failed")
	}
	var pvc PVC
	var pvcFound bool
	if deleteStorage {
		if pvc, pvcFound, err = readStorage(c, project); err != nil {
			return r.fail("workbench", fmt.Sprintf("Cannot read PersistentVolumeClaim %s in %s: %v. Nothing was deleted.", StorageName, project, err), kube.ErrorCodeFromK8sErr(err), "read failed")
		}
		if pvcFound && !rec.OwnsStorage(pvc.Metadata) {
			return r.fail("workbench", fmt.Sprintf("PersistentVolumeClaim %s in %s was not created by this tool, so this tool does not delete it. Nothing was deleted.", StorageName, project), "not_managed", "foreign storage")
		}
		// Up front too, so a storage another workbench uses refuses the
		// whole deletion before anything goes.
		if pvcFound {
			if msg, code := r.storageInUseCheck(project, rec); msg != "" {
				return r.fail("workbench", msg+" Nothing was deleted.", code, "storage in use")
			}
		}
	}
	switch {
	case found && !rec.OwnsNotebook(nb.Metadata):
		return r.fail("workbench", notOurs(project)+" Nothing was deleted.", "not_managed", "not this tool's")
	case found:
		// Notebook finalizers are cleared solely by odh-notebook-controller;
		// deleting while that component is off would hang indefinitely.
		if dsc, err := rhoai.ReadFirstDSC(c); err == nil {
			if b := envkit.ComponentBlock(dsc, Component); b != nil {
				return r.fail("workbench", b.Reason+" The workbench's controller removes its finalizers, so it cannot be deleted now. Nothing was deleted.", "prerequisites", "component off")
			}
		}
		if _, err := kube.DeleteWithUID(c, notebookPath(project), nb.Metadata.UID); err != nil && !kube.IsK8sNotFound(err) {
			return r.fail("workbench", fmt.Sprintf("Cannot delete workbench %s in %s: %v.", Name, project, err), kube.ErrorCodeFromK8sErr(err), "delete failed")
		}
		gone, err := kube.WaitForGone(c, GoneWait, 2*time.Second, func(cc *kube.Client) (bool, error) {
			now, found, err := readNotebook(cc, project)
			return !found || now.Metadata.UID != nb.Metadata.UID, err
		})
		if err != nil || !gone {
			why := "it is still being deleted"
			if err != nil {
				why = err.Error()
			}
			return r.fail("workbench", fmt.Sprintf("Workbench %s in %s was deleted, but it is not gone yet (%s). Its controller removes it; delete again later to finish.", Name, project, why), "timeout", "not gone")
		}
		r.step("workbench", "success", fmt.Sprintf("Deleted workbench %s.", Name))
	default:
		r.step("workbench", "success", fmt.Sprintf("Project %s has no workbench %s.", project, Name))
	}
	if err := updateRecord(c, project, func(e *Recorded) { e.UID = "" }); err != nil {
		slog.Warn("cannot drop the workbench from the record", "project", project, "error", err)
	}
	if !deleteStorage {
		msg := fmt.Sprintf("Deleted workbench %s in %s.", Name, project)
		if e, ok := rec.Get(project); ok && e.StorageUID != "" {
			msg += fmt.Sprintf(" Its storage %s was kept: setting up again uses it.", StorageName)
		}
		return r.done(msg)
	}
	r.step("storage", "running", fmt.Sprintf("Deleting storage %s", StorageName))
	if !pvcFound {
		r.step("storage", "success", fmt.Sprintf("Project %s has no storage %s.", project, StorageName))
	} else {
		if msg, code := r.storageInUseCheck(project, rec); msg != "" {
			return r.fail("storage", fmt.Sprintf("The workbench was deleted, but its storage was kept: %s", msg), code, "storage in use")
		}
		if _, err := kube.DeleteWithUID(c, storagePath(project), pvc.Metadata.UID); err != nil && !kube.IsK8sNotFound(err) {
			code := kube.ErrorCodeFromK8sErr(err)
			if kube.IsK8sConflict(err) {
				code = "conflict"
			}
			return r.fail("storage", fmt.Sprintf("The workbench was deleted, but storage %s was not: %v. Delete again to retry.", StorageName, err), code, "storage delete failed")
		}
		r.step("storage", "success", fmt.Sprintf("Deleted storage %s; its files are gone.", StorageName))
	}
	if err := updateRecord(c, project, func(e *Recorded) { e.StorageUID = "" }); err != nil {
		slog.Warn("cannot drop the workbench storage from the record", "project", project, "error", err)
	}
	return r.done(fmt.Sprintf("Deleted workbench %s and its storage in %s.", Name, project))
}

// deleteKeptStorage deletes the storage a deleted workbench left (step
// "storage"): only while no workbench of the name uses it, and only when it
// carries the tool's label and the recorded UID.
func (r *run) deleteKeptStorage(project string) (*types.OperationResponse, error) {
	c := r.c
	r.step("storage", "running", fmt.Sprintf("Deleting storage %s in %s", StorageName, project))
	_, nbFound, err := readNotebook(c, project)
	if err != nil {
		return r.fail("storage", fmt.Sprintf("Cannot read workbench %s in %s: %v. Nothing was deleted.", Name, project, err), kube.ErrorCodeFromK8sErr(err), "read failed")
	}
	if nbFound {
		return r.fail("storage", fmt.Sprintf("Workbench %s in %s still exists and uses this storage. Delete the workbench and tick its storage instead. Nothing was deleted.", Name, project), "validation", "workbench exists")
	}
	rec, err := ReadRecord(c)
	if err != nil {
		return r.fail("storage", err.Error()+" Nothing was deleted.", kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal"), "record read failed")
	}
	pvc, found, err := readStorage(c, project)
	if err != nil {
		return r.fail("storage", fmt.Sprintf("Cannot read PersistentVolumeClaim %s in %s: %v. Nothing was deleted.", StorageName, project, err), kube.ErrorCodeFromK8sErr(err), "read failed")
	}
	switch {
	case !found:
		r.step("storage", "success", fmt.Sprintf("Project %s has no storage %s.", project, StorageName))
	case !rec.OwnsStorage(pvc.Metadata):
		return r.fail("storage", fmt.Sprintf("PersistentVolumeClaim %s in %s was not created by this tool, so this tool does not delete it. Nothing was deleted.", StorageName, project), "not_managed", "foreign storage")
	default:
		if msg, code := r.storageInUseCheck(project, rec); msg != "" {
			return r.fail("storage", msg+" Nothing was deleted.", code, "storage in use")
		}
		if _, err := kube.DeleteWithUID(c, storagePath(project), pvc.Metadata.UID); err != nil && !kube.IsK8sNotFound(err) {
			return r.fail("storage", fmt.Sprintf("Cannot delete storage %s in %s: %v.", StorageName, project, err), kube.ErrorCodeFromK8sErr(err), "storage delete failed")
		}
		r.step("storage", "success", fmt.Sprintf("Deleted storage %s; its files are gone.", StorageName))
	}
	if err := updateRecord(c, project, func(e *Recorded) { e.StorageUID = "" }); err != nil {
		slog.Warn("cannot drop the workbench storage from the record", "project", project, "error", err)
	}
	return r.done(fmt.Sprintf("Deleted storage %s in %s.", StorageName, project))
}

// DeleteStorage is the Clean up part for storage kept from a deleted
// workbench.
func DeleteStorage(c *kube.Client, project string) (*types.OperationResponse, error) {
	return Run(c, types.WorkbenchRequest{Action: "delete-storage", Project: project}, nil)
}

// Teardown is the Clean up entry point: drops the project workbench and,
// when deleteStorage is true, its storage—the same path as Run's teardown.
func Teardown(c *kube.Client, project string, deleteStorage bool) (*types.OperationResponse, error) {
	return Run(c, types.WorkbenchRequest{Action: "teardown", Project: project, DeleteStorage: deleteStorage}, nil)
}

// withoutEntry strips this tool's Connection for project out of value and
// reports whether such an entry appeared (webhook token format: commas, "ns/name").
func withoutEntry(value, project string) (string, bool) {
	var kept []string
	found := false
	for _, part := range strings.Split(value, ",") {
		ns, name, ok := strings.Cut(strings.TrimSpace(part), "/")
		if ok && strings.TrimSpace(ns) == project && strings.TrimSpace(name) == ConnectionSecret {
			found = true
			continue
		}
		if strings.TrimSpace(part) != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, ","), found
}

// profileGone: the hardware profile the workbench's annotations name no
// longer exists (the dashboard's binding state Deleted).
func profileGone(c *kube.Client, nb Notebook) (bool, error) {
	name, ns := nb.Metadata.Annotations[profileNameAnnotation], nb.Metadata.Annotations[profileNamespaceAnnotation]
	if name == "" {
		return false, nil
	}
	if ns == "" {
		ns = nb.Metadata.Namespace
	}
	profiles, err := listProfiles(c, ns)
	if err != nil {
		return false, err
	}
	for _, p := range profiles {
		if p.Metadata.Name == name {
			return false, nil
		}
	}
	return true, nil
}

// storageUsers are the workbenches and pods of project that mount
// nightly-workbench-storage, other than this tool's workbench (the
// Notebook with the recorded UID and its pods, labelled
// notebook-name=nightly-workbench): deleting the storage would take it
// from them, so it is refused while there are any.
func storageUsers(c *kube.Client, project, recordedUID string) ([]string, error) {
	notebooks, err := listNotebooks(c, project)
	if err != nil {
		return nil, err
	}
	var users []string
	for _, nb := range notebooks {
		if nb.Metadata.UID == recordedUID && recordedUID != "" {
			continue
		}
		if mountsStorage(nb.Spec.Template.Spec.Volumes) {
			users = append(users, "workbench "+nb.Metadata.Name)
		}
	}
	pods, err := listProjectPods(c, project)
	if err != nil {
		return nil, err
	}
	for _, p := range pods {
		if p.Metadata.Labels["notebook-name"] == Name || p.Metadata.Labels["notebook-name"] != "" && slices.Contains(users, "workbench "+p.Metadata.Labels["notebook-name"]) {
			continue
		}
		if mountsStorage(p.Spec.Volumes) {
			users = append(users, "pod "+p.Metadata.Name)
		}
	}
	return users, nil
}

func mountsStorage(volumes []Volume) bool {
	return slices.ContainsFunc(volumes, func(v Volume) bool {
		return v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == StorageName
	})
}

// storageInUse is the refusal when the storage has other users, or "".
func storageInUse(users []string) string {
	if len(users) == 0 {
		return ""
	}
	from := "them"
	if len(users) == 1 {
		from = "that " + strings.SplitN(users[0], " ", 2)[0]
	}
	return fmt.Sprintf("Storage %s is used by %s; remove it from %s in the dashboard first.", StorageName, strings.Join(users, ", "), from)
}

// storageInUseCheck runs storageUsers right before a delete of the
// storage: the refusal and its code, or "" when nothing else uses it.
// Users that cannot be read refuse too (fail closed).
func (r *run) storageInUseCheck(project string, rec Record) (string, string) {
	e, _ := rec.Get(project)
	users, err := storageUsers(r.c, project, e.UID)
	if err != nil {
		return fmt.Sprintf("Cannot check what uses storage %s in %s: %v.", StorageName, project, err), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal")
	}
	if msg := storageInUse(users); msg != "" {
		return msg, "storage_in_use"
	}
	return "", ""
}

// keptStorageInUse is Set up's refusal when the kept storage is mounted by
// another workbench or pod (a new workbench could not mount it on
// single-node storage), or "". Users that cannot be read refuse too.
func (r *run) keptStorageInUse(project string, rec Record) (string, string) {
	e, _ := rec.Get(project)
	users, err := storageUsers(r.c, project, e.UID)
	if err != nil {
		return fmt.Sprintf("Cannot check what uses storage %s in %s: %v.", StorageName, project, err), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal")
	}
	if len(users) == 0 {
		return "", ""
	}
	from := "them"
	if len(users) == 1 {
		_, from, _ = strings.Cut(users[0], " ")
	}
	return fmt.Sprintf("Storage %s is used by %s; a new workbench could not mount it (single-node storage). Remove it from %s in the dashboard first.", StorageName, strings.Join(users, ", "), from), "storage_in_use"
}
