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

// A project's model actions (the non-admin design, section 1 rows "Model,
// internal, token auth off", "Model with token auth" and "Model behind the
// inference gateway"; branch B6): a model of the catalog set up or deleted
// in one data science project, by a cluster-admin or by any user whose
// role in the project allows it (pkg/api's withProjectMutationAuth runs
// the preflight first). Unlike the Models tab's operation (RunModels, a
// cluster-admin's under the cluster lock), it changes nothing outside the
// project: it never adds a project to a gateway's list, never publishes to
// MaaS, never installs Connectivity Link, never writes the internal
// registry. Every change and every in-project read is made with c, the
// signed-in user's token; the cluster facts it needs (the
// DataScienceCluster, KServe's controller and preset, its configuration,
// the gateway, the nodes and pods, the mirrors) with c.Facts(), the
// ServiceAccount's read-only copy, reduced to the requested project where
// they list other tenants (models_project_facts.go).
//
// The three actions have their own entry points (setup, setupToken,
// teardown), which the RBAC scan's roots name: a setup without token
// authentication never reaches the code that makes the model's Role and
// RoleBinding, so a project editor's preflight does not ask for them.
//   - setup: reached only inside the cluster, its token authentication off
//     (security.opendatahub.io/enable-auth "false", as the dashboard sets
//     it for a model without "Require token authentication"); project
//     admins and editors.
//   - setupToken: with the dashboard's token authentication (a
//     ServiceAccount, a Role, a RoleBinding and a token Secret, owned by
//     the model, models_token.go), reached inside the cluster or through
//     the cluster's default inference gateway when it already admits the
//     project (ProjectGatewayFact); the Role and RoleBinding need the
//     project's admin role.
//   - teardown: the tool's model (by its recorded UID) only; garbage
//     collection removes its token objects with it (ownerReferences), so
//     an editor can delete a model a project admin set up with them.

// The project model actions (cluster.ProjectActionID's action part).
const (
	ProjectModelSetup      = "setup"
	ProjectModelSetupToken = "setup-token"
	ProjectModelTeardown   = "teardown"
)

// ProjectModelAction is the project action req asks for: a setup with
// token authentication is its own action.
func ProjectModelAction(req types.ProjectModelRequest) string {
	if req.Action == ProjectModelSetup && req.TokenAuth {
		return ProjectModelSetupToken
	}
	return req.Action
}

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
		switch req.Exposure {
		case "", ExposureInternal:
		case ExposureDefault:
			// Through a gateway without token authentication anyone who
			// reaches the gateway could call the model.
			if !req.TokenAuth {
				return "a model reached through the inference gateway needs token authentication"
			}
		default:
			return "exposure must be internal or default-gateway"
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
	switch ProjectModelAction(req) {
	case ProjectModelSetupToken:
		return r.setupToken()
	case ProjectModelTeardown:
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
// the hardware profile, room and quota, the records' caps. token: the
// setup makes the model's token authentication.
func (r *projectModelRun) check(token bool) (*modelPlan, []string, *types.OperationResponse, error) {
	c, project, entry := r.c, r.req.Project, r.entry
	r.step("check", "running", fmt.Sprintf("Checking what %s needs in %s", entry.DisplayName, project))
	stop := func(msg, code, why string) (*modelPlan, []string, *types.OperationResponse, error) {
		res, err := r.fail("check", msg, code, why)
		return nil, nil, res, err
	}
	labels, msg, code := ProjectNamespace(c, project)
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
	want := kube.FirstNonEmpty(r.req.Exposure, ExposureInternal)
	exp := ExposurePlan{Exposure: want}
	if found {
		maasRec, err := ReadMaaSRecord(c)
		if err != nil {
			return stop(readText(c, "MaaS publications", err)+" Nothing was changed.", kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal"), "maas record read failed")
		}
		if ModelHasMaaSRef(existing) || maasRec.IsPublished(project, entry.Name) {
			return stop(fmt.Sprintf("%s in %s is published to Models as a Service, which a cluster-admin manages: they unpublish it on the MaaS tab first. Nothing was changed.", entry.Name, project), "in_use", "published to maas")
		}
		def, _ := readKServeGateway(c.Facts())
		cur, curGW := modelExposureOf(existing, def)
		if cur != want {
			return stop(fmt.Sprintf("%s in %s is already set up to be reached %s. To reach it another way, delete it and set it up again with that choice. Nothing was changed.", entry.Name, project, exposureWords(exposurePlanOf(cur, curGW))), "validation", "exposure differs")
		}
		if !token && existing.Metadata.Annotations[EnableAuthAnnot] != "false" {
			return stop(fmt.Sprintf("%s in %s has token authentication; setting it up again keeps it, which needs the project's admin role (its Role and RoleBinding). Ask an admin of project %s, or delete the model and deploy it without token authentication. Nothing was changed.", entry.Name, project, project), "missing_permissions", "token auth on")
		}
	}
	switch want {
	case ExposureInternal:
		// Without token authentication it is off, as the dashboard writes
		// it. With it, Connectivity Link must be set up (KServe does not
		// start a model with token authentication without its AuthPolicy
		// API): a setup never turns the asked-for authentication off
		// unnoticed (fail closed, also when that cannot be read).
		exp.authOff = !token
		if token {
			ok, _, err := authReadiness(c.Facts())
			if err != nil {
				kube.LogFactReadFailure("cannot read whether Connectivity Link is set up for a project's model", "Kuadrant", err)
				return stop(fmt.Sprintf("Token authentication needs Red Hat Connectivity Link, and whether it is set up cannot be checked now (%s). Deploy without token authentication, or try again. Nothing was changed.", kube.FactReadProblem(err)), envkit.ErrorCodePrerequisitesUnknown, "auth unknown")
			}
			if !ok {
				return stop("Token authentication needs Red Hat Connectivity Link, which is not set up on this cluster (KServe does not start a model with token authentication without it). Deploy without token authentication, or ask a cluster-admin to set it up. Nothing was changed.", "prerequisites", "auth missing")
			}
		}
	case ExposureDefault:
		// Every setup through the gateway, a model set up again too: the
		// gateway must still admit the project and check the callers'
		// tokens.
		if fact, _ := ProjectGatewayFact(c, project, labels); !fact.Admits {
			return stop(GatewayFactWords(project, fact)+" Or choose 'Only inside the cluster'. Nothing was changed.", "prerequisites", "gateway "+fact.Problem)
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
	p, notes, res, err := r.check(false)
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
	created := !p.found
	if res, err := r.applyModel(p); res != nil || err != nil {
		return res, err
	}
	r.step("model", "success", fmt.Sprintf("LLMInferenceService %s/%s applied, on preset %s with %s. Reached %s. Without token authentication (%s: \"false\", as the dashboard sets it): any pod whose network reaches %s can call it.", p.project, p.entry.Name, p.preset, p.image.describe(), exposureWords(p.exp), EnableAuthAnnot, p.project))
	return r.ready(p, created, func(model LLMISvcObject, _ ModelsRecord, progress func(string)) types.ModelCallCheck {
		return r.callInternal(model, progress)
	})
}

// setupToken sets up a model with the dashboard's token authentication,
// reached inside the cluster or through the cluster's default inference
// gateway when it already admits the project.
func (r *projectModelRun) setupToken() (*types.OperationResponse, error) {
	c := r.c
	p, notes, res, err := r.check(true)
	if p == nil {
		return res, err
	}
	modelUID := ""
	if p.found {
		modelUID = p.current.UID
	}
	tok, msg, code := planModelToken(c, p.project, p.entry.Name, modelUID, p.rec[ModelRecordKey(p.project, p.entry.Name)].TokenSecretUID)
	if msg != "" {
		return r.fail("check", msg, code, "token check failed")
	}
	p.token = tok
	if res, err := r.dryRunModel(p); res != nil || err != nil {
		return res, err
	}
	created := !p.found
	if _, msg, code := applyModelToken(c, p.project, p.entry.Name, p.current.UID, p.token, true); msg != "" {
		return r.fail("check", msg+" Nothing was changed.", code, "token dry run failed")
	}
	r.step("check", "success", strings.TrimSpace("Everything the model needs is there. "+strings.Join(notes, " ")))
	if res, err := r.beforeWrite(); res != nil || err != nil {
		return res, err
	}
	if res, err := r.applyModel(p); res != nil || err != nil {
		return res, err
	}
	if msg, code := (&modelsRun{c: c}).applyToken(p); msg != "" {
		if p.exp.Exposure == ExposureDefault {
			return r.gatewayUnconfirmed(p, created, "model", msg)
		}
		return r.fail("model", msg, code, "token apply failed")
	}
	r.step("model", "success", fmt.Sprintf("LLMInferenceService %s/%s applied, on preset %s with %s. Reached %s.%s", p.project, p.entry.Name, p.preset, p.image.describe(), exposureWords(p.exp), tokenWords(p.project, p.entry.Name, p.token)))
	return r.ready(p, created, func(model LLMISvcObject, rec ModelsRecord, progress func(string)) types.ModelCallCheck {
		def, _ := readKServeGateway(c.Facts())
		exposure, gw := modelExposureOf(model, def)
		switch exposure {
		case ExposureInternal:
			return r.callInternal(model, progress)
		case ExposureDefault:
			served := kube.FirstNonEmpty(model.Spec.Model.Name, model.Metadata.Name)
			if !SafeServedName(served) {
				return types.ModelCallCheck{Result: "not-checked", Message: "Not checked: " + unsafeServedNameWarning}
			}
			return callModelVia(c, model, p.project, p.entry.Name, served, p.entry.Type, exposure, gw, rec, progress, func() string {
				if c.ProjectUser() {
					// Authorino's namespace and settings are the
					// ServiceAccount's reads.
					return " Check that Red Hat Connectivity Link's Authorino runs; a cluster-admin can check it."
				}
				return gatewayErrorCause(c.Facts())
			})
		}
		return types.ModelCallCheck{Result: "not-checked", Message: "Not checked: the model is reached in a way a cluster-admin set up."}
	})
}

// callInternal calls the model at its in-cluster address once.
func (r *projectModelRun) callInternal(model LLMISvcObject, progress func(string)) types.ModelCallCheck {
	served := kube.FirstNonEmpty(model.Spec.Model.Name, model.Metadata.Name)
	if !SafeServedName(served) {
		return types.ModelCallCheck{Result: "not-checked", Message: "Not checked: " + unsafeServedNameWarning}
	}
	return callInternalModel(r.c, model, r.req.Project, r.entry.Name, served, r.entry.Type, progress)
}

// ready waits for the model, then calls it once (call). Its safe points
// are the polls of the wait: a cancel there leaves the model, recorded as
// the tool's. Through the gateway the setup succeeds only once the call
// check confirmed that the gateway refuses a caller without a token;
// otherwise a model this setup created is deleted again and one set up
// before is reported (gatewayUnconfirmed).
func (r *projectModelRun) ready(p *modelPlan, created bool, call func(LLMISvcObject, ModelsRecord, func(string)) types.ModelCallCheck) (*types.OperationResponse, error) {
	c := r.c
	gateway := p.exp.Exposure == ExposureDefault
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
	var msg, code, why string
	switch {
	case res.cancel != nil:
		if gateway && created {
			if note := r.rollBack(p); note != "" {
				return r.cancelled("model-ready", res.cancel, note)
			}
			return r.cancelled("model-ready", res.cancel, "The model this setup created was deleted again: its token checks through the gateway were not confirmed.")
		}
		return r.cancelled("model-ready", res.cancel, "The model stays set up, recorded as this tool's: Delete removes it.")
	case res.err != nil:
		msg, code, why = fmt.Sprintf("Stopped waiting for the model: %v.%s", res.err, deleteIt), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(res.err), "cancelled"), "model wait failed"
	case res.imageMissing:
		msg, code, why = "The model cannot start: the cluster preset's image is not in the registry (this nightly build's runtime image is not published yet). Set it up again with the newest released Red Hat image, which replaces only the model's image. Technical details: "+res.problem, "image_unavailable", "preset image missing"
	case res.imageMaybeMissing:
		msg, code, why = "The model cannot start: "+res.problem+" A registry answered that the image does not exist (\"manifest unknown\"): if this nightly build's runtime image is not published yet, set it up again with the newest released Red Hat image.", "image_unavailable", "preset image may be missing"
	case res.problem != "":
		msg, code, why = "The model cannot start: "+res.problem+deleteIt+" Or fix the cause and set it up again.", "not_ready", "model not starting"
	case !res.ready && res.pulling != "":
		msg, code, why = fmt.Sprintf("Created, but not ready after %s: its image pull is still failing. %s It stays set up; it starts once the pull succeeds.", ClockText(timeout), res.pulling), "not_ready", "image pull failing"
	case !res.ready:
		if gateway {
			return r.gatewayUnconfirmed(p, created, "model-ready", fmt.Sprintf("The model is not ready after %s, so whether the gateway checks the callers' tokens could not be confirmed.", ClockText(timeout)))
		}
		r.step("model-ready", "success", fmt.Sprintf("Created, but not ready after %s. It may still start; its state shows here.", ClockText(timeout)))
		r.step("callable", "success", "Not checked: the model is not ready yet.")
		return r.done(fmt.Sprintf("%s in %s is still starting.", p.entry.DisplayName, p.project))
	}
	if msg != "" {
		if gateway {
			return r.gatewayUnconfirmed(p, created, "model-ready", msg+" Whether the gateway checks the callers' tokens could not be confirmed.")
		}
		return r.fail("model-ready", msg, code, why)
	}
	r.step("model-ready", "success", "Ready.")
	r.step("callable", "running", "Calling the model once, as a user would")
	rec, err := ReadModelsRecord(c)
	if err != nil {
		rec = p.rec
	}
	model, found, err := ReadToolModel(c, p.project, p.entry.Name)
	var check types.ModelCallCheck
	switch {
	case err != nil:
		check = types.ModelCallCheck{Result: "not-checked", Message: "Not checked: the model cannot be read (" + err.Error() + ")."}
	case !found:
		check = types.ModelCallCheck{Result: "not-checked", Message: "Not checked: the model does not exist any more."}
	default:
		check = call(model, rec, func(line string) { r.step("callable", "running", line) })
		check.At = kube.NowRFC3339()
		rememberCallCheck(c, p.project, p.entry.Name, model.Metadata.UID, check)
	}
	if gateway && (!check.OK || !check.AnonymousRefused) {
		return r.gatewayUnconfirmed(p, created, "callable", strings.TrimSpace(check.Message+" The gateway's refusal of a caller without a token was not confirmed."))
	}
	if !check.OK && check.Result != "not-checked" {
		return r.fail("callable", callFailureWords(fmt.Sprintf("%s is ready in %s", p.entry.DisplayName, p.project), check)+" It stays set up.", "not_callable", "model not callable ("+check.Result+")")
	}
	r.step("callable", "success", check.Message)
	return r.done(fmt.Sprintf("%s is ready in %s. %s", p.entry.DisplayName, p.project, check.Message))
}

// gatewayUnconfirmed ends a setup through the gateway whose token checks
// were not confirmed (the call check's request without a token was not
// refused, or the model could not be called): a model this setup created
// is deleted again, by its UID, and forgotten (its token objects go with
// it), so it is never left reachable through the gateway unchecked; a
// model set up before is reported and left as it is.
func (r *projectModelRun) gatewayUnconfirmed(p *modelPlan, created bool, step, why string) (*types.OperationResponse, error) {
	if !created {
		return r.fail(step, why+" The model was set up before this run and is left as it is: delete it, or ask a cluster-admin to check the gateway's token authentication (Red Hat Connectivity Link and RHOAI's AuthPolicy on the gateway).", "not_authenticated", "gateway token checks not confirmed")
	}
	if note := r.rollBack(p); note != "" {
		return r.fail(step, why+" "+note, "not_authenticated", "gateway token checks not confirmed")
	}
	return r.fail(step, why+" The model this setup created was deleted again (its token objects go with it), so it is not left reachable through the gateway unchecked. Deploy it inside the cluster, or ask a cluster-admin to check the gateway's token authentication.", "not_authenticated", "gateway token checks not confirmed")
}

// rollBack deletes the model this setup created (its recorded UID) and
// forgets it; "" when done, else what is left, in words.
func (r *projectModelRun) rollBack(p *modelPlan) string {
	path, _ := ToolModelPath(p.project, p.entry.Name)
	if _, err := kube.DeleteWithUID(r.c, path, p.current.UID); err != nil && !kube.IsK8sNotFound(err) {
		return fmt.Sprintf("The model this setup created could not be deleted again (%v): delete it now (Delete, or oc delete llminferenceservice %s -n %s).", err, p.entry.Name, p.project)
	}
	if err := forgetModels(r.c, ModelRecordKey(p.project, p.entry.Name)); err != nil {
		slog.Warn("cannot update the models record; the background check forgets the entry", "error", kube.FactReadProblem(err))
	}
	return ""
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
	msg := fmt.Sprintf("Model %s deleted from %s; KServe removed its Deployment, Service and route, and its token objects go with it (they are owned by the model). The project stays.", name, project)
	r.step("model", "success", msg)
	return r.done(msg)
}
