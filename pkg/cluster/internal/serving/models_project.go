package serving

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// A project's model actions (the non-admin design, section 1 row "Model,
// internal, token auth off"; branch B6): a model of the catalog set up or
// deleted in one data science project, by a cluster-admin or by any user
// whose role in the project allows it (pkg/api's withProjectMutationAuth
// runs the preflight first). The model is reached only inside the
// cluster, with token authentication off (security.opendatahub.io/
// enable-auth "false", as the dashboard writes it for a model without
// "Require token authentication"). Unlike the Models tab's operation
// (RunModels, a cluster-admin's under the cluster lock), it changes nothing
// outside the project: it never routes a model through a gateway, never
// adds a project to a gateway's list, never publishes to MaaS, never makes
// a Role or RoleBinding.
//
// Narrower than the design's section 1 (rows "Model with token auth" and
// "Model behind the inference gateway"): a model reached through the
// inference gateway, or with token authentication, is a cluster-admin's
// (the Models tab). Through a gateway the route exists before anything
// can confirm that the gateway checks its callers (odh-model-controller
// makes the gateway's AuthPolicy only once a model uses it), so a user's
// setup could not keep that safe on its own; and inside the cluster token
// authentication does not authenticate the in-cluster address.
//
// Every change and every in-project read is made with c, the signed-in
// user's token; the cluster facts it needs (the DataScienceCluster,
// KServe's controller and preset, its configuration, the nodes and pods,
// the mirrors) with c.Facts(), the ServiceAccount's read-only copy, reduced
// to the requested project where they list other tenants
// (models_project_facts.go). Its entry points (setup, teardown) are the
// RBAC scan's roots.

// The project model actions (cluster.ProjectActionID's action part).
const (
	ProjectModelSetup    = "setup"
	ProjectModelTeardown = "teardown"
)

// ProjectModelAction is the project action req asks for.
func ProjectModelAction(req types.ProjectModelRequest) string {
	return req.Action
}

// ProjectModelTarget is the catalog model req acts on (its name; "" asks
// for the catalog's default), or "" for a model not in the catalog.
func ProjectModelTarget(req types.ProjectModelRequest) string {
	entry, _ := CatalogEntry(req.Model)
	return entry.Name
}

// ClusterAdminModelWords says what a project model action does not do, and
// who does it.
const ClusterAdminModelWords = "A cluster-admin deploys models reached through the inference gateway or with token authentication, on the Environment page's Models tab."

// ValidateProjectModelRequest returns why req cannot run, or "".
func ValidateProjectModelRequest(req types.ProjectModelRequest) string {
	if !ProjectNameRegex.MatchString(req.Project) {
		return "a valid project is required"
	}
	if !ValidModelName(req.Model) {
		return "unknown model"
	}
	switch req.Action {
	case ProjectModelSetup:
		// Asked for, they are refused (never set up without them).
		if req.Exposure != "" && req.Exposure != ExposureInternal || req.TokenAuth {
			return "a model is set up here only inside the cluster, without token authentication. " + ClusterAdminModelWords
		}
	case ProjectModelTeardown:
		if req.Exposure != "" || req.TokenAuth || req.ReleasedImage {
			return "teardown takes a project and a model only"
		}
	default:
		return "action must be setup or teardown"
	}
	return ""
}

// projectModelRun is one streamed project model action.
type projectModelRun struct {
	c        *kube.Client
	emit     func(types.UpdateStepEvent)
	logs     []string
	activity string
	target   string
	req      types.ProjectModelRequest
	entry    CatalogModel
}

// RunProjectModel runs a project's model action as one operation with a
// streamed step per part: setup "check", "model", "model-ready",
// "callable"; teardown "model".
func RunProjectModel(c *kube.Client, req types.ProjectModelRequest, onStep func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	entry, _ := CatalogEntry(req.Model)
	r := &projectModelRun{c: c, emit: onStep, req: req, entry: entry, activity: req.Action + "-models",
		target: fmt.Sprintf("model %s in %s", kube.FirstNonEmpty(entry.Name, req.Model), req.Project)}
	if problem := ValidateProjectModelRequest(req); problem != "" {
		return r.fail("check", problem, "validation", "invalid request")
	}
	if req.Action == ProjectModelTeardown {
		return r.teardown()
	}
	return r.setup()
}

func (r *projectModelRun) step(id, status, msg string) {
	r.logs = append(r.logs, msg)
	if r.emit != nil {
		r.emit(types.UpdateStepEvent{Step: id, Status: status, Message: msg})
	}
}

// fail ends the run on step id. Its log line names no user (the activity
// entry keeps who did what).
func (r *projectModelRun) fail(id, msg, code, activity string) (*types.OperationResponse, error) {
	if r.emit != nil {
		r.emit(types.UpdateStepEvent{Step: id, Status: "failed", Message: msg, ErrorCode: code})
	}
	r.logs = append(r.logs, msg)
	slog.Warn("project model action stopped", "step", id, "reason", activity)
	record.Action(r.c, r.activity, fmt.Sprintf("%s (%s)", r.target, activity), false)
	return &types.OperationResponse{Success: false, Message: msg, Logs: r.logs, ErrorCode: code}, nil
}

// CancelledCode is the errorCode of a project model action that stopped
// at a safe point because a cancel was requested.
const CancelledCode = "cancelled"

// cancelled ends the run at a safe point because a cancel was requested
// (record.CancelRequested): step id failed with the reason and what stays,
// and the activity entry failed with the reason.
func (r *projectModelRun) cancelled(id string, cancel *record.ActionCancelledError, kept string) (*types.OperationResponse, error) {
	msg := cancel.Reason() + " before this step. " + kept
	if r.emit != nil {
		r.emit(types.UpdateStepEvent{Step: id, Status: "failed", Message: msg, ErrorCode: CancelledCode})
	}
	r.logs = append(r.logs, msg)
	slog.Info("project model action cancelled", "step", id)
	record.Cancelled(r.c, r.activity, r.target, cancel)
	return &types.OperationResponse{Success: false, Message: msg, Logs: r.logs, ErrorCode: CancelledCode}, nil
}

func (r *projectModelRun) done(msg string) (*types.OperationResponse, error) {
	slog.Info("project model action", "action", r.req.Action, "project", r.req.Project, "model", r.entry.Name)
	record.Action(r.c, r.activity, r.target, true)
	return &types.OperationResponse{Success: true, Message: msg, Logs: r.logs}, nil
}

// readText says a failed read of this tool's records (the
// ServiceAccount's) to c's user: for a project user by its HTTP status or
// category only (the error is logged), for a cluster-admin as it is.
func readText(c *kube.Client, what string, err error) string {
	if c.ProjectUser() {
		kube.LogFactReadFailure("cannot read this tool's record for a project user's model action", what, err)
		return fmt.Sprintf("Cannot read this tool's record of its %s (%s).", what, kube.FactReadProblem(err))
	}
	return err.Error() + "."
}

// projectModelsBlock is why no model can be set up from the
// DataScienceCluster (read with the facts client), with its errorCode, or
// "": KServe is off, or the LLMInferenceService dependencies are reported
// missing. A DataScienceCluster that cannot be read refuses (fail closed).
// The words carry neither the DataScienceCluster's name nor its condition
// message (the ServiceAccount's reads).
func projectModelsBlock(c *kube.Client) (string, string) {
	dsc, err := rhoai.ReadFirstDSC(c.Facts())
	if err != nil {
		kube.LogFactReadFailure("cannot read the DataScienceCluster for a project's model", "DataScienceCluster", err)
		return fmt.Sprintf("Cannot read whether model serving is turned on (%s).", kube.FactReadProblem(err)), envkit.ErrorCodePrerequisitesUnknown
	}
	block, _ := ModelsBlock(dsc)
	switch {
	case block == nil:
		return "", ""
	case envkit.ComponentBlock(dsc, KServeComponent) != nil:
		return "Model serving (KServe) is not turned on on this cluster. A cluster-admin turns it on (Components page).", "prerequisites"
	}
	return "KServe cannot serve LLMInferenceServices yet: the DataScienceCluster reports that their dependencies are missing. A cluster-admin installs them (Components page).", "prerequisites"
}

// check plans the model (no write): the project, model serving, the API,
// the CPU preset, the model of that name and how it is reached, the image,
// the hardware profile, room and quota, the records' caps.
func (r *projectModelRun) check() (*modelPlan, []string, *types.OperationResponse, error) {
	c, project, entry := r.c, r.req.Project, r.entry
	r.step("check", "running", fmt.Sprintf("Checking what %s needs in %s", entry.DisplayName, project))
	stop := func(msg, code, why string) (*modelPlan, []string, *types.OperationResponse, error) {
		res, err := r.fail("check", msg, code, why)
		return nil, nil, res, err
	}
	_, msg, code := ProjectNamespace(c, project)
	if msg != "" {
		return stop(msg+" Nothing was changed.", code, "project check failed")
	}
	if msg, code := projectModelsBlock(c); msg != "" {
		return stop(msg+" Nothing was changed.", code, "models blocked")
	}
	if v := rhoai.ServedVersions(c, llmisvcGroup, nil); v.Discovered && !slices.Contains(v.Versions, "v1alpha2") {
		return stop(fmt.Sprintf("This cluster's KServe does not serve LLMInferenceService %s (served: %s), which is the shape this tool writes. Nothing was changed.", llmisvcAPIVersion, strings.Join(v.Versions, ", ")), "prerequisites", "api version")
	}
	preset := discoverCPUPreset(c.Facts())
	if preset.problem != "" {
		msg := preset.problem
		switch {
		case preset.err != nil && c.ProjectUser():
			kube.LogFactReadFailure("cannot read KServe's CPU preset for a project's model", "CPU preset", preset.err)
			msg = fmt.Sprintf("Cannot read KServe's CPU preset (%s).", kube.FactReadProblem(preset.err))
		case preset.notReady && c.ProjectUser():
			// The preset's condition is the ServiceAccount's read.
			msg = "KServe's CPU preset is not ready. A cluster-admin checks it (Components page)."
		}
		return stop(msg+" Nothing was changed.", "prerequisites", "no preset")
	}
	existing, found, err := ReadToolModel(c, project, entry.Name)
	if err != nil {
		return stop(fmt.Sprintf("Cannot read LLMInferenceService %s/%s: %v. Nothing was changed.", project, entry.Name, err), kube.ErrorCodeFromK8sErr(err), "model read failed")
	}
	rec, err := ReadModelsRecord(c)
	if err != nil {
		return stop(readText(c, "models", err)+" Nothing was changed.", kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal"), "record read failed")
	}
	if found && !rec.Owns(existing.Metadata) {
		return stop(fmt.Sprintf("Project %s already has an LLMInferenceService named %s that this tool did not create. It was left unchanged.", project, entry.Name), "not_managed", "model foreign")
	}
	if found && existing.Metadata.Terminating() {
		return stop(fmt.Sprintf("The model %s in %s is still being deleted. Wait until it is gone, then set it up again.", entry.Name, project), "terminating", "model terminating")
	}
	exp := ExposurePlan{Exposure: ExposureInternal, authOff: true}
	if found {
		maasRec, err := ReadMaaSRecord(c)
		if err != nil {
			return stop(readText(c, "MaaS publications", err)+" Nothing was changed.", kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal"), "maas record read failed")
		}
		if ModelHasMaaSRef(existing) || maasRec.IsPublished(project, entry.Name) {
			return stop(fmt.Sprintf("%s in %s is published to Models as a Service, which a cluster-admin manages: they unpublish it on the MaaS tab first. Nothing was changed.", entry.Name, project), "in_use", "published to maas")
		}
		def, _ := readKServeGateway(c.Facts())
		if cur, curGW := modelExposureOf(existing, def); cur != ExposureInternal {
			return stop(fmt.Sprintf("%s in %s is set up to be reached %s, which a cluster-admin manages (Models tab). Nothing was changed.", entry.Name, project, exposureWords(exposurePlanOf(cur, curGW))), "validation", "exposure differs")
		}
		if existing.Metadata.Annotations[EnableAuthAnnot] != "false" {
			return stop(fmt.Sprintf("%s in %s has token authentication, which a cluster-admin manages (Models tab); setting it up here would turn it off. Delete it and deploy it again here, or ask a cluster-admin. Nothing was changed.", entry.Name, project), "validation", "token auth on")
		}
	}
	var prev *LLMISvcObject
	if found {
		prev = &existing
	}
	img, err := PlanModelImage(c.Facts(), preset.image, r.req.ReleasedImage, prev, true)
	if err != nil {
		return stop(err.Error()+" Nothing was changed.", "prerequisites", "no image")
	}
	if img.Reason != "" {
		r.step("check", "running", "Image: "+img.Reason)
	}
	ephemeral := modelVolumeIsEmptyDir(c.Facts())
	hw, why, refusal := pickHardwareProfile(c, project, entry)
	if refusal != "" {
		return stop(refusal+" Nothing was changed.", "validation", "no hardware profile")
	}
	if hw.Name != "" {
		r.step("check", "running", fmt.Sprintf("Hardware profile: %s/%s, as the dashboard records it.", hw.Namespace, hw.Name))
	} else {
		r.step("check", "running", "Hardware profile: none, because "+why+".")
	}
	var notes []string
	if words := CapacityFactWords(entry, ModelCapacityFact(c, entry, project)); words != "" {
		notes = append(notes, words+" The setup still runs.")
	}
	if warning := quotaWarning(c, project, entry, ephemeral); warning != "" {
		notes = append(notes, warning)
	}
	if c.ProjectUser() {
		if msg, code := recordCapRefusal(c, rec, project, entry.Name); msg != "" {
			return stop(msg+" Nothing was changed.", code, "records full")
		}
	}
	return &modelPlan{entry: entry, project: project, preset: preset.name, image: img, hardware: hw,
		obj: ModelManifest(project, entry, preset.name, img, hw, false, ephemeral, exp), current: existing.Metadata, found: found, rec: rec,
		ephemeral: ephemeral, exp: exp}, notes, nil, nil
}

// dryRunModel checks the model's write with the API server (admission,
// quota, the user's RBAC), before anything is written.
func (r *projectModelRun) dryRunModel(p *modelPlan) (*types.OperationResponse, error) {
	if msg, code := (&modelsRun{c: r.c}).applyModel(p, true); msg != "" {
		return r.fail("check", msg, code, "model dry run failed")
	}
	return nil, nil
}

// beforeWrite is the safe point before the first write: a cancel there
// leaves nothing.
func (r *projectModelRun) beforeWrite() (*types.OperationResponse, error) {
	if cancel := record.CancelRequested(r.c); cancel != nil {
		return r.cancelled("model", cancel, "Nothing was changed.")
	}
	return nil, nil
}

// applyModel writes the model; a model it created is recorded as the
// tool's, or deleted again (applyModel).
func (r *projectModelRun) applyModel(p *modelPlan) (*types.OperationResponse, error) {
	r.step("model", "running", fmt.Sprintf("Creating %s in %s (%s)", p.entry.DisplayName, p.project, p.image.describe()))
	if msg, code := (&modelsRun{c: r.c}).applyModel(p, false); msg != "" {
		return r.fail("model", msg, code, "model apply failed")
	}
	return nil, nil
}

// setup sets up a model reached only inside the cluster, with token
// authentication off: no Role, no RoleBinding, no token Secret.
func (r *projectModelRun) setup() (*types.OperationResponse, error) {
	p, notes, res, err := r.check()
	if p == nil {
		return res, err
	}
	if res, err := r.dryRunModel(p); res != nil || err != nil {
		return res, err
	}
	r.step("check", "success", strings.TrimSpace("Everything the model needs is there. "+strings.Join(notes, " ")))
	if res, err := r.beforeWrite(); res != nil || err != nil {
		return res, err
	}
	if res, err := r.applyModel(p); res != nil || err != nil {
		return res, err
	}
	r.step("model", "success", fmt.Sprintf("LLMInferenceService %s/%s applied, on preset %s with %s. Reached %s. Without token authentication (%s: \"false\", as the dashboard sets it): any pod whose network reaches %s can call it.", p.project, p.entry.Name, p.preset, p.image.describe(), exposureWords(p.exp), EnableAuthAnnot, p.project))
	return r.ready(p)
}

// callInternal calls the model at its in-cluster address once.
func (r *projectModelRun) callInternal(model LLMISvcObject, progress func(string)) types.ModelCallCheck {
	served := kube.FirstNonEmpty(model.Spec.Model.Name, model.Metadata.Name)
	if !SafeServedName(served) {
		return types.ModelCallCheck{Result: "not-checked", Message: "Not checked: " + unsafeServedNameWarning}
	}
	return callInternalModel(r.c, model, r.req.Project, r.entry.Name, served, r.entry.Type, progress)
}

// ready waits for the model, then calls it once at its in-cluster
// address. Its safe points are the polls of the wait: a cancel there
// leaves the model, recorded as the tool's. The call check after the wait
// runs to its end (bounded by modelCallRetryWindow).
func (r *projectModelRun) ready(p *modelPlan) (*types.OperationResponse, error) {
	c := r.c
	r.step("model-ready", "running", "Waiting for the model to start (typically 3 to 5 minutes the first time)")
	timeout := modelReadyTimeout
	if dl, ok := c.Context().Deadline(); ok {
		if left := kube.Clock().Until(dl) - 30*time.Second; left < timeout {
			timeout = left
		}
	}
	progress := func(line string) { r.step("model-ready", "running", line) }
	res := waitModelReady(c, p.project, p.entry.Name, p.image.Kind == "preset", false, timeout, progress)
	const deleteIt = " It was created; Delete removes it."
	switch {
	case res.cancel != nil:
		return r.cancelled("model-ready", res.cancel, "The model stays set up, recorded as this tool's: Delete removes it.")
	case res.err != nil:
		return r.fail("model-ready", fmt.Sprintf("Stopped waiting for the model: %v.%s", res.err, deleteIt), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(res.err), "cancelled"), "model wait failed")
	case res.imageMissing:
		return r.fail("model-ready", "The model cannot start: the cluster preset's image is not in the registry (this nightly build's runtime image is not published yet). Set it up again with the newest released Red Hat image, which replaces only the model's image. Technical details: "+res.problem, "image_unavailable", "preset image missing")
	case res.imageMaybeMissing:
		return r.fail("model-ready", "The model cannot start: "+res.problem+" A registry answered that the image does not exist (\"manifest unknown\"): if this nightly build's runtime image is not published yet, set it up again with the newest released Red Hat image.", "image_unavailable", "preset image may be missing")
	case res.problem != "":
		return r.fail("model-ready", "The model cannot start: "+res.problem+deleteIt+" Or fix the cause and set it up again.", "not_ready", "model not starting")
	case !res.ready && res.pulling != "":
		return r.fail("model-ready", fmt.Sprintf("Created, but not ready after %s: its image pull is still failing. %s It stays set up; it starts once the pull succeeds.", ClockText(timeout), res.pulling), "not_ready", "image pull failing")
	case !res.ready:
		r.step("model-ready", "success", fmt.Sprintf("Created, but not ready after %s. It may still start; its state shows here.", ClockText(timeout)))
		r.step("callable", "success", "Not checked: the model is not ready yet.")
		return r.done(fmt.Sprintf("%s in %s is still starting.", p.entry.DisplayName, p.project))
	}
	r.step("model-ready", "success", "Ready.")
	r.step("callable", "running", "Calling the model once, as a user would")
	model, found, err := ReadToolModel(c, p.project, p.entry.Name)
	var check types.ModelCallCheck
	switch {
	case err != nil:
		check = types.ModelCallCheck{Result: "not-checked", Message: "Not checked: the model cannot be read (" + err.Error() + ")."}
	case !found:
		check = types.ModelCallCheck{Result: "not-checked", Message: "Not checked: the model does not exist any more."}
	default:
		check = r.callInternal(model, func(line string) { r.step("callable", "running", line) })
		check.At = kube.NowRFC3339()
		rememberCallCheck(c, p.project, p.entry.Name, model.Metadata.UID, check)
	}
	if !check.OK && check.Result != "not-checked" {
		return r.fail("callable", callFailureWords(fmt.Sprintf("%s is ready in %s", p.entry.DisplayName, p.project), check)+" It stays set up.", "not_callable", "model not callable ("+check.Result+")")
	}
	r.step("callable", "success", check.Message)
	return r.done(fmt.Sprintf("%s is ready in %s. %s", p.entry.DisplayName, p.project, check.Message))
}

// teardown deletes the tool's model (its recorded UID) and waits until
// KServe's finalizer has removed it. Its token objects go with it
// (garbage collection: they are owned by the model). A model published to
// MaaS is a cluster-admin's to unpublish first.
func (r *projectModelRun) teardown() (*types.OperationResponse, error) {
	c, project, name := r.c, r.req.Project, r.entry.Name
	r.step("model", "running", fmt.Sprintf("Deleting %s in %s", name, project))
	model, found, err := ReadToolModel(c, project, name)
	if err != nil {
		return r.fail("model", fmt.Sprintf("Cannot read LLMInferenceService %s/%s; nothing was deleted: %v.", project, name, err), kube.ErrorCodeFromK8sErr(err), "read failed")
	}
	rec, err := ReadModelsRecord(c)
	if err != nil {
		return r.fail("model", readText(c, "models", err)+" Nothing was deleted.", kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal"), "record read failed")
	}
	key := ModelRecordKey(project, name)
	if !found {
		if err := forgetModels(c, key); err != nil {
			slog.Warn("cannot update the models record", "error", kube.FactReadProblem(err))
		}
		r.step("model", "success", fmt.Sprintf("No %s of this tool in %s; nothing to delete.", name, project))
		return r.done(fmt.Sprintf("No %s of this tool in %s; nothing to delete.", name, project))
	}
	if !rec.Owns(model.Metadata) {
		return r.fail("model", fmt.Sprintf("The model %s in %s was not created by this tool, so it was not deleted.", name, project), "not_managed", "not managed")
	}
	maasRec, err := ReadMaaSRecord(c)
	if err != nil {
		return r.fail("model", readText(c, "MaaS publications", err)+" Nothing was deleted.", kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal"), "maas record read failed")
	}
	if ModelHasMaaSRef(model) || maasRec.IsPublished(project, name) {
		return r.fail("model", fmt.Sprintf("%s in %s is published to Models as a Service. A cluster-admin unpublishes it on the MaaS tab first; nothing was deleted.", name, project), "in_use", "published to maas")
	}
	// A publication someone made without this tool: a MaaSModelRef in the
	// project that targets the model (a yes or no, read by the
	// ServiceAccount). Unreadable refuses too (fail closed).
	switch targeted, err := ProjectMaaSRefFact(c, project, name); {
	case err != nil:
		return r.fail("model", fmt.Sprintf("Cannot check whether a MaaS model reference in %s points at %s (%s); nothing was deleted. Try again.", project, name, kube.FactReadProblem(err)), envkit.ErrorCodePrerequisitesUnknown, "maas refs unreadable")
	case targeted:
		return r.fail("model", fmt.Sprintf("A MaaS model reference in %s points at %s, so it is published to Models as a Service. Whoever published it (or a cluster-admin) removes that first; nothing was deleted.", project, name), "in_use", "maas ref targets model")
	}
	path, _ := ToolModelPath(project, name)
	if !model.Metadata.Terminating() {
		if _, err := kube.DeleteWithUID(c, path, model.Metadata.UID); err != nil && !kube.IsK8sNotFound(err) {
			code := kube.ErrorCodeFromK8sErr(err)
			if kube.IsK8sConflict(err) {
				code = "conflict"
			}
			return r.fail("model", fmt.Sprintf("Failed to delete LLMInferenceService %s/%s: %v.", project, name, err), code, "delete failed")
		}
	}
	gone, err := kube.WaitForDeletion(c, path, modelDeleteTimeout, modelDeletePoll)
	if err != nil {
		return r.fail("model", fmt.Sprintf("The model %s/%s is being deleted, but whether it is gone cannot be checked: %v.", project, name, err), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "in_progress"), "wait failed")
	}
	if !gone {
		return r.fail("model", fmt.Sprintf("The model %s/%s is still being deleted after %s. KServe's LLMInferenceService controller removes it once it has deleted the model's Deployment, Service and route. Delete again to check.", project, name, ClockText(modelDeleteTimeout)), "in_progress", "still deleting")
	}
	if err := forgetModels(c, key); err != nil {
		slog.Warn("cannot update the models record; the background check forgets the entry", "error", kube.FactReadProblem(err))
	}
	msg := fmt.Sprintf("Model %s deleted from %s; KServe removed its Deployment and Service, and any token objects a cluster-admin's setup made go with it (they are owned by the model). The project stays.", name, project)
	r.step("model", "success", msg)
	return r.done(msg)
}
