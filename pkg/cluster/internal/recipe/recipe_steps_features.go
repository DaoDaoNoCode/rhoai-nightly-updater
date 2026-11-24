package recipe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dashdev"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/mlflow"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/modelregistry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/workbench"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Steps after MLflow, in the order the runner executes them. Each invokes the
// same handler as its Environment page control (or Dashboard Dev control),
// using that control's request body; Check inspects the cluster the way page
// status does so Resume can skip parts already configured.
const (
	StepMLflowSamples       = "mlflow-samples"
	StepMaaS                = "maas"
	StepModelsPrereq        = "models-prereq"
	StepModel               = "model"
	StepMaaSPublish         = "maas-publish"
	StepPlaygroundPrereq    = "playground-prereq"
	StepPlayground          = "playground"
	StepModelRegistryPrereq = "model-registry-prereq"
	StepModelRegistry       = "model-registry"
	StepWorkbench           = "workbench"
	StepDashboardDev        = "dashboard-dev"
)

// Indirection for status reads and step runs below; tests replace these vars.
// Each hook corresponds to a status or action entry point on the matching page.
var (
	prereqStatus     = features.GetPrerequisitesStatusFor
	samplesStatus    = mlflow.SamplesStatus
	runSamples       = mlflow.RunSamples
	maasStatus       = serving.GetMaaSStatus
	maasRecord       = serving.ReadMaaSRecord
	runMaaS          = serving.RunMaaS
	modelExposures   = serving.GetModelExposures
	readModel        = readToolModel
	runModels        = serving.RunModels
	maasPublished    = serving.MaaSPublishedIn
	playgroundStatus = features.GetPlaygroundStatus
	runPlayground    = features.RunPlayground
	registryStatus   = modelregistry.ReadStatus
	registryRecord   = modelregistry.ReadRecord
	runRegistry      = modelregistry.Run
	workbenchStatus  = workbench.GetStatus
	runWorkbench     = workbench.Run
	dashboardSession = dashdev.OverrideSummary
	deployMain       = dashdev.DeployMain
	deployPR         = dashdev.DeployPR
	waitDashboard    = dashdev.WaitDeploymentsReady
)

// featureSteps yields every post-MLflow step turned on by recipe r, in sequence.
func featureSteps(r types.EnvironmentRecipe) []Step {
	f := r.Spec.Features
	var steps []Step
	add := func(id, title string, typical, most time.Duration, check func(*kube.Client, *State) (Check, error), run func(*kube.Client, *State, func(types.UpdateStepEvent)) (*types.OperationResponse, error)) {
		steps = append(steps, Step{ID: id, Title: title, Typical: typical, Max: most, Check: check, Run: run})
	}
	if f.MLflow.Enabled && f.MLflow.Samples {
		add(StepMLflowSamples, "MLflow samples", time.Minute, 15*time.Minute, checkMLflowSamples, runMLflowSamples)
	}
	if f.MaaS.Enabled {
		// MaaS tab Set up wires Connectivity Link, database, gateway, and
		// DataScienceCluster settings that MaaS relies on.
		add(StepMaaS, "Models as a Service", 10*time.Minute, 15*time.Minute, checkMaaS, runMaaSStep)
	}
	if f.Models.Enabled {
		// On CPU workers, serving often lands in 3–6 minutes (QUICKSTART); the
		// step still obeys serving.modelReadyTimeout.
		add(StepModelsPrereq, "What Models needs", 4*time.Minute, 15*time.Minute, checkModelsPrereq, runModelsPrereq)
		add(StepModel, "Model "+modelName(r), 5*time.Minute, 15*time.Minute, checkModel, runModel)
		if f.MaaS.Enabled && f.MaaS.Publish && f.Models.Exposure != serving.ExposureMaaS {
			add(StepMaaSPublish, "Publish the model through MaaS", 2*time.Minute, 5*time.Minute, checkMaaSPublish, runMaaSPublish)
		}
	}
	if f.Playground.Enabled {
		add(StepPlaygroundPrereq, "What the playground needs", 2*time.Minute, 10*time.Minute, checkPlaygroundPrereq, runPlaygroundPrereq)
		add(StepPlayground, "Playground", 2*time.Minute, 9*time.Minute, checkPlayground, runPlaygroundStep)
	}
	if f.ModelRegistry.Enabled {
		add(StepModelRegistryPrereq, "What the model registry needs", 2*time.Minute, 10*time.Minute, checkModelRegistryPrereq, runModelRegistryPrereq)
		add(StepModelRegistry, "Model registry", 2*time.Minute, 12*time.Minute, checkModelRegistry, runModelRegistry)
	}
	if f.Workbench.Enabled {
		add(StepWorkbench, "Workbench", 30*time.Second, 2*time.Minute, checkWorkbench, runWorkbenchStep)
	}
	if d := r.Spec.DashboardDev; d.Deploy == DashboardDevMain || d.Deploy == DashboardDevPR {
		// Placed last: a paused dashboard module operator stalls RHOAI upgrades
		// until the pause ends.
		add(StepDashboardDev, dashboardDevTitle(d), 3*time.Minute, 8*time.Minute, checkDashboardDev, runDashboardDev)
	}
	return steps
}

// modelName returns the recipe model's catalog label; empty string means the
// catalog default.
func modelName(r types.EnvironmentRecipe) string {
	if entry, ok := serving.CatalogEntry(r.Spec.Features.Models.Model); ok {
		return entry.Name
	}
	return r.Spec.Features.Models.Model
}

func dashboardDevTitle(d types.RecipeDashboardDev) string {
	if d.Deploy == DashboardDevPR {
		return fmt.Sprintf("Dashboard Dev (PR #%d)", d.PR)
	}
	return "Dashboard Dev (main)"
}

// blockedAt builds a Check whose block reason appears on the Environment page.
func blockedAt(reason string) Check {
	return Check{Blocked: reason, Fix: fixAt(types.RecipeFixFeatureBlocked, pageEnvironment)}
}

// readWithin supplies a client for post-setup reads that should live beyond the
// step context (runPrereq reloads its record the same way).
func readWithin(c *kube.Client) (*kube.Client, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Context()), prereqRecordReadTimeout)
	return c.WithContext(ctx), cancel
}

// --- MLflow samples ----------------------------------------------------------

// addableSamples lists samples MLflow tab Add all would select: those whose
// state offers Add. Repair-needed rows are skipped (demo MLflow held at 2Gi by
// an older release)—Add would restart MLflow after reconfiguring instead of
// attaching the sample.
func addableSamples(st *types.MLflowSamplesStatus) (ids, repairs []string) {
	for _, s := range st.Samples {
		if !slices.Contains(s.Actions, "add") {
			continue
		}
		if s.Repair != "" {
			repairs = append(repairs, s.Name)
			continue
		}
		ids = append(ids, s.ID)
	}
	return ids, repairs
}

// checkMLflowSamples: complete when no addable samples remain in the project.
func checkMLflowSamples(c *kube.Client, s *State) (Check, error) {
	project := projectOf(s)
	st, err := samplesStatus(c, c, project)
	if errors.Is(err, mlflow.ErrNotDSProject) {
		return blockedAt(fmt.Sprintf("Project %s is not one of your data science projects. Nothing was changed.", project)), nil
	}
	if err != nil {
		return Check{}, fmt.Errorf("read the MLflow samples in %s: %w", project, err)
	}
	if !st.Available {
		return Check{}, nil // RunSamples will supply the explanation
	}
	if ids, _ := addableSamples(st); len(ids) == 0 {
		return Check{Done: true, Reason: fmt.Sprintf("The MLflow samples are in %s.", project)}, nil
	}
	return Check{}, nil
}

// runMLflowSamples runs MLflow tab Add all for the project.
func runMLflowSamples(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	project := projectOf(s)
	st, err := samplesStatus(c, c, project)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the MLflow samples in %s: %v. Nothing was changed.", project, err), kube.ErrorCodeOr(err, "prerequisites"))
	}
	if !st.Available {
		return fail(kube.FirstNonEmpty(st.Reason, "MLflow is not ready.")+" Nothing was changed; Resume tries again.", "prerequisites")
	}
	ids, repairs := addableSamples(st)
	if len(ids) == 0 {
		return &types.OperationResponse{Success: true, Message: fmt.Sprintf("The MLflow samples are in %s.", project)}, nil
	}
	res, err := runSamples(c, types.MLflowSamplesRequest{Project: project, Action: "add", Samples: ids}, emit)
	if res != nil && len(repairs) > 0 {
		res.Message = strings.TrimSpace(res.Message + fmt.Sprintf(" Not added: %s, which needs a change to MLflow first; add it on the MLflow tab.", strings.Join(repairs, ", ")))
	}
	return res, err
}

// --- Models as a Service -----------------------------------------------------

// checkMaaS: complete when the cluster's single MaaS platform is ready (any
// provisioner); blocked when Set up reports a denial reason.
func checkMaaS(c *kube.Client, s *State) (Check, error) {
	st, err := maasStatus(c)
	if err != nil {
		return Check{}, fmt.Errorf("read the MaaS platform: %w", err)
	}
	switch {
	case st.Platform.Ready:
		return Check{Done: true, Reason: "The MaaS platform is ready."}, nil
	case st.Platform.SetupBlockedReason != "":
		return blockedAt(st.Platform.SetupBlockedReason), nil
	}
	if why := maasDatabaseBlocked(st, s.Recipe.Spec.Features.MaaS.Database); why != "" {
		return blockedAt(why), nil
	}
	return Check{}, nil
}

// maasDatabaseBlocked describes why the recipe database cannot be created when
// Set up is supposed to provision it.
func maasDatabaseBlocked(st *types.MaaSStatus, kind string) string {
	if st.Database == nil || !st.Database.Needed {
		return ""
	}
	for _, ch := range st.Database.Choices {
		if ch.ID == kind && ch.BlockedReason != "" {
			return fmt.Sprintf("The MaaS database %s cannot be set up: %s", kind, ch.BlockedReason)
		}
	}
	return ""
}

// runMaaSStep follows MaaS tab Set up and sends the recipe database when Set up
// must create one (the UI asks only then). If the platform shifts from external
// to tool-managed—even after partial failure—the run records it as tool-created.
func runMaaSStep(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	before, err := maasStatus(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the MaaS platform: %v. Nothing was changed.", err), kube.ErrorCodeOr(err, "prerequisites"))
	}
	req := types.MaaSRequest{Action: "setup"}
	if before.Database != nil && before.Database.Needed {
		req.Database = &types.MaaSDatabaseRequest{Kind: s.Recipe.Spec.Features.MaaS.Database}
	}
	res, runErr := runMaaS(c, req, emit)
	if !before.Platform.ManagedByTool {
		rc, cancel := readWithin(c)
		defer cancel()
		after, err := maasStatus(rc)
		var rec serving.MaaSRecord
		if err == nil {
			rec, err = maasRecord(rc)
		}
		switch {
		case err != nil:
			noteUnrecorded(StepMaaS, "the MaaS platform", err, res, emit)
		case after.Platform.ManagedByTool:
			s.AddCreated(types.RecipeCreatedPart{Part: types.RecipePartMaaSPlatform, UID: rec.POCCredsUID})
		}
	}
	return res, runErr
}

// noteUnrecorded adds to the step message, live progress, and logs that creation
// could not be confirmed (recipe Clean up skips it; Environment Clean up
// everything still deletes it).
func noteUnrecorded(step, what string, err error, res *types.OperationResponse, emit func(types.UpdateStepEvent)) {
	note := fmt.Sprintf("The run cannot tell whether it created %s (%v), so the recipe's Clean up does not offer it; Clean up everything on the Environment page does.", what, err)
	slog.Warn("recipe step: created part not recorded", "step", step, "error", err)
	emit(types.UpdateStepEvent{Step: step, Status: "running", Message: note})
	if res != nil {
		res.Message = strings.TrimSpace(res.Message + " " + note)
		res.Logs = append(res.Logs, "WARN: "+note)
	}
}

// --- Models ------------------------------------------------------------------

// exposureOf determines how traffic would reach the run's model: state on the
// run, spec on the recipe, or—if neither is set—the Models tab suggestion for
// that catalog model in the project. Does not modify s.
func exposureOf(c *kube.Client, s *State) (exposure, gatewayRef, problem, code string) {
	if s.Exposure != "" {
		return s.Exposure, s.GatewayRef, "", ""
	}
	if e := s.Recipe.Spec.Features.Models.Exposure; e != "" {
		return e, "", "", ""
	}
	project := projectOf(s)
	ex, msg, code := modelExposures(c, project, modelName(s.Recipe))
	if msg != "" {
		return "", "", fmt.Sprintf("Cannot tell how the model in %s would be reached: %s", project, msg), kube.FirstNonEmpty(code, "prerequisites")
	}
	for _, opt := range ex.Options {
		if opt.Key == ex.Default && ex.Default != "" {
			return opt.Exposure, opt.GatewayRef, "", ""
		}
	}
	why := strings.TrimSpace(ex.RecommendGatewayReason)
	if len(ex.Options) > 0 {
		why = strings.TrimSpace(ex.Options[0].Reason + " " + why)
	}
	return "", "", fmt.Sprintf("No way to reach the model from %s is recommended now. %s Set spec.features.models.exposure (internal: only inside the cluster).", project, kube.FirstNonEmpty(why, "No gateway can carry it.")), "prerequisites"
}

// ExposureKept tests whether a run record's exposure (and gateway) is valid for
// model setup so resume can reuse it: recipe exposures or Models tab "gateway"
// with namespace/name. The record sits in the updater's ConfigMap, which the
// ServiceAccount may write, so other values get recomputed on resume.
func ExposureKept(exposure, gatewayRef string) bool {
	return exposure != "" && serving.ValidateModelsRequest(types.ModelsRequest{Action: "setup", Project: "p", Exposure: exposure, GatewayRef: gatewayRef}) == ""
}

// pinExposure resolves how the model is reached and saves that for later steps
// and resume.
func pinExposure(c *kube.Client, s *State) (string, string) {
	exposure, ref, problem, code := exposureOf(c, s)
	if problem == "" {
		s.Exposure, s.GatewayRef = exposure, ref
	}
	return problem, code
}

// checkModelsPrereq: complete when prerequisites for the model exposure are satisfied.
func checkModelsPrereq(c *kube.Client, s *State) (Check, error) {
	exposure, _, problem, _ := exposureOf(c, s)
	if problem != "" {
		return blockedAt(problem), nil
	}
	return checkPrereqFor(c, "models", exposure)
}

// runModelsPrereq runs Models tab Set up for prerequisites bound to the exposure
// used when deploying the model (gateways may hook Connectivity Link).
func runModelsPrereq(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	if problem, code := pinExposure(c, s); problem != "" {
		return fail(problem+" Nothing was changed.", code)
	}
	return runPrereqFor(c, s, StepModelsPrereq, "models", s.Exposure, emit)
}

// readToolModel loads the catalog model in project, reporting whether it exists
// and state merged with the tool's stored record.
func readToolModel(c *kube.Client, project, name string) (types.ModelState, bool, error) {
	m, found, err := serving.ReadToolModel(c, project, name)
	if err != nil || !found {
		return types.ModelState{}, found, err
	}
	rec, err := serving.ReadModelsRecord(c)
	if err != nil {
		return types.ModelState{}, true, err
	}
	return serving.ModelState(c, m, false, rec), true, nil
}

// checkModel: complete when a tool-managed project model is serving (and MaaS-
// published if exposure is MaaS); blocked when an unrelated same-name model exists.
func checkModel(c *kube.Client, s *State) (Check, error) {
	project, name := projectOf(s), modelName(s.Recipe)
	st, found, err := readModel(c, project, name)
	if err != nil {
		return Check{}, fmt.Errorf("read model %s in %s: %w", name, project, err)
	}
	switch {
	case !found:
		return Check{}, nil
	case !st.ManagedByTool:
		return blockedAt(fmt.Sprintf("Project %s has a model %s that this tool did not create, so the recipe does not change it. Nothing was changed.", project, name)), nil
	case st.Terminating:
		return blockedAt(fmt.Sprintf("Model %s in %s is being deleted. Resume once it is gone.", name, project)), nil
	case !st.Ready:
		return Check{}, nil
	}
	exposure := kube.FirstNonEmpty(s.Exposure, s.Recipe.Spec.Features.Models.Exposure)
	if exposure == serving.ExposureMaaS && !st.MaaSPublished {
		return Check{}, nil
	}
	return Check{Done: true, Reason: fmt.Sprintf("Model %s in %s serves.", name, project)}, nil
}

// runModel calls Models tab Set up for the project model with resolved reachability
// (MaaS exposure publishes via MaaS in one pass, matching the tab).
func runModel(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	if s.Exposure == "" {
		if problem, code := pinExposure(c, s); problem != "" {
			return fail(problem+" Nothing was changed.", code)
		}
	}
	m := s.Recipe.Spec.Features.Models
	return runModels(c, types.ModelsRequest{Action: "setup", Project: projectOf(s), Model: modelName(s.Recipe), ReleasedImage: m.ReleasedImage, Exposure: s.Exposure, GatewayRef: s.GatewayRef}, emit)
}

// checkMaaSPublish: complete when the model is already published through MaaS.
func checkMaaSPublish(c *kube.Client, s *State) (Check, error) {
	project, name := projectOf(s), modelName(s.Recipe)
	published, err := maasPublished(c, project, name)
	if err != nil {
		return Check{}, fmt.Errorf("read whether %s in %s is published: %w", name, project, err)
	}
	if published {
		return Check{Done: true, Reason: fmt.Sprintf("Model %s in %s is published through MaaS.", name, project)}, nil
	}
	return Check{}, nil
}

// runMaaSPublish executes MaaS tab Publish on the model.
func runMaaSPublish(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	return runMaaS(c, types.MaaSRequest{Action: "publish", Project: projectOf(s), Model: modelName(s.Recipe)}, emit)
}

// --- Playground --------------------------------------------------------------

func checkPlaygroundPrereq(c *kube.Client, _ *State) (Check, error) {
	return checkPrereqFor(c, "playground", "")
}

// runPlaygroundPrereq performs Set up for playground prerequisites.
func runPlaygroundPrereq(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	return runPrereqFor(c, s, StepPlaygroundPrereq, "playground", "", emit)
}

// checkPlayground: complete when a tool-provisioned project playground is ready;
// blocked when a playground exists that the tool did not set up.
func checkPlayground(c *kube.Client, s *State) (Check, error) {
	project := projectOf(s)
	st, err := playgroundStatus(c, c, project)
	if err != nil {
		return Check{}, fmt.Errorf("read the playground in %s: %w", project, err)
	}
	for _, item := range st.Items {
		if item.Namespace != project || !item.Deployed {
			continue
		}
		switch {
		case !item.ManagedByTool:
			return blockedAt(fmt.Sprintf("Project %s already has a playground that this tool did not set up; the recipe does not change it. Nothing was changed.", project)), nil
		case item.Ready:
			return Check{Done: true, Reason: fmt.Sprintf("The playground in %s is ready.", project)}, nil
		}
	}
	return Check{}, nil
}

// runPlaygroundStep runs Playground tab Set up in the project with every ready
// chat model included—the tab's default.
func runPlaygroundStep(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	return runPlayground(c, types.PlaygroundRequest{Action: "setup", Project: projectOf(s)}, emit)
}

// --- Model registry ----------------------------------------------------------

func checkModelRegistryPrereq(c *kube.Client, _ *State) (Check, error) {
	return checkPrereqFor(c, "model-registry", "")
}

// runModelRegistryPrereq carries out Set up for model-registry prerequisites.
func runModelRegistryPrereq(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	return runPrereqFor(c, s, StepModelRegistryPrereq, "model-registry", "", emit)
}

// checkModelRegistry: complete when a tool-owned registry is ready (sample model
// too when requested and supported); blocked for a same-name registry the tool
// did not create.
func checkModelRegistry(c *kube.Client, s *State) (Check, error) {
	st, err := registryStatus(c, c)
	if err != nil {
		return Check{}, fmt.Errorf("read the model registry: %w", err)
	}
	if st.Foreign {
		return blockedAt(fmt.Sprintf("A model registry named %s exists in %s that this tool did not create; the recipe does not change it. Nothing was changed.", st.Name, st.Namespace)), nil
	}
	if st.State != "ready" || !st.ManagedByTool {
		return Check{}, nil
	}
	sampleDone := st.Sample != nil && st.Sample.Complete
	noAddress := st.Address == "" && st.AddressProblem == ""
	if s.Recipe.Spec.Features.ModelRegistry.Sample && !sampleDone && !noAddress {
		return Check{}, nil
	}
	return Check{Done: true, Reason: fmt.Sprintf("Model registry %s is ready in %s.", st.Name, st.Namespace)}, nil
}

// runModelRegistry drives Model registry tab Set up (sample model optional per
// recipe). Registries created here tie to the run through the UID the tool
// persisted.
func runModelRegistry(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	before, err := registryStatus(c, c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the model registry: %v. Nothing was changed.", err), kube.ErrorCodeOr(err, "prerequisites"))
	}
	res, runErr := runRegistry(c, types.ModelRegistryRequest{Action: "setup", Samples: s.Recipe.Spec.Features.ModelRegistry.Sample}, emit)
	if before.State == "absent" && before.Namespace != "" {
		rc, cancel := readWithin(c)
		defer cancel()
		rec, err := registryRecord(rc)
		switch {
		case err != nil:
			noteUnrecorded(StepModelRegistry, "the model registry", err, res, emit)
		case rec[before.Namespace+"/"+modelregistry.Name].UID != "":
			s.AddCreated(types.RecipeCreatedPart{Part: types.RecipePartModelRegistry, UID: rec[before.Namespace+"/"+modelregistry.Name].UID})
		}
	}
	return res, runErr
}

// --- Workbench ---------------------------------------------------------------

// checkWorkbench: complete when a tool-created project workbench exists (running
// or idle—the tab can start it); blocked for foreign workbenches, disabled
// components, or unsupported platforms.
func checkWorkbench(c *kube.Client, s *State) (Check, error) {
	project := projectOf(s)
	st, err := workbenchStatus(c, c, project)
	if err != nil {
		return Check{}, fmt.Errorf("read the workbench in %s: %w", project, err)
	}
	switch {
	case st.Unsupported != "":
		return blockedAt(st.Unsupported), nil
	case st.Blocked != nil:
		return Check{Blocked: st.Blocked.Reason, Fix: fixAt(types.RecipeFixFeatureBlocked, pageComponents)}, nil
	case slices.Contains(st.ItemsUnreadable, project):
		return Check{}, fmt.Errorf("the workbench in %s cannot be read: %s", project, kube.FirstNonEmpty(st.ItemsProblem, "unknown"))
	}
	for _, item := range st.Items {
		if item.Namespace != project || !item.Deployed {
			continue
		}
		if !item.ManagedByTool {
			return blockedAt(fmt.Sprintf("Project %s has a workbench %s that this tool did not create; the recipe does not change it. Nothing was changed.", project, workbench.Name)), nil
		}
		return Check{Done: true, Reason: fmt.Sprintf("Workbench %s in %s is set up (%s).", workbench.Name, project, kube.FirstNonEmpty(item.State, "state unknown"))}, nil
	}
	return Check{}, nil
}

// runWorkbenchStep runs Workbench tab Set up in the project and attaches the tool
// S3 Connection when the project already has the tool pipeline server, as the
// tab does.
func runWorkbenchStep(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	return runWorkbench(c, types.WorkbenchRequest{Action: "setup", Project: projectOf(s)}, emit)
}

// --- Dashboard Dev -----------------------------------------------------------

// checkDashboardDev: complete when an active Dashboard Dev session serves the recipe build.
func checkDashboardDev(c *kube.Client, s *State) (Check, error) {
	o, err := dashboardSession(c)
	if err != nil {
		return Check{}, fmt.Errorf("read the Dashboard Dev session: %w", err)
	}
	d := s.Recipe.Spec.DashboardDev
	if o == nil || !o.Active || !o.SessionRecorded || o.Mode != d.Deploy || kube.FirstNonEmpty(o.Flavor, dashdev.FlavorRHOAI) != d.Flavor {
		return Check{}, nil
	}
	if d.Deploy == DashboardDevPR && o.PRNumber != d.PR {
		return Check{}, nil
	}
	return Check{Done: true, Reason: fmt.Sprintf("Dashboard Dev runs the recipe's build (%s).", strings.TrimPrefix(dashboardDevTitle(d), "Dashboard Dev "))}, nil
}

// runDashboardDev mirrors Dashboard Dev page Deploy (main or pull request), then
// waits for dashboard rollout in the order the UI shows.
func runDashboardDev(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	d := s.Recipe.Spec.DashboardDev
	module, err := dashboardSession(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the Dashboard Dev session: %v. Nothing was changed.", err), kube.ErrorCodeOr(err, "prerequisites"))
	}
	emit(types.UpdateStepEvent{Step: StepDashboardDev, Status: "running", Message: fmt.Sprintf("Deploying the dashboard's %s build...", strings.Trim(strings.TrimPrefix(dashboardDevTitle(d), "Dashboard Dev "), "()"))})
	var res *types.OperationResponse
	if d.Deploy == DashboardDevPR {
		res, err = deployPR(c, d.PR, d.Flavor)
	} else {
		res, err = deployMain(c, d.Flavor)
	}
	if err != nil || res == nil || !res.Success || module == nil {
		// Legacy path without a dashboard module has no operator session to wait on.
		return res, err
	}
	emit(types.UpdateStepEvent{Step: StepDashboardDev, Status: "running", Message: "Waiting for the dashboard to roll out (usually 1 to 2 minutes)..."})
	if _, err := waitDashboard(c); err != nil {
		if ctxErr := c.Context().Err(); ctxErr != nil {
			return &types.OperationResponse{Success: false, Message: "Stopped while waiting for the dashboard.", Logs: res.Logs}, ctxErr
		}
		code := "timeout"
		if errors.Is(err, dashdev.ErrDeploymentsFailed) {
			code = "prerequisites"
		}
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("%s, but the dashboard is not ready: %v. The Dashboard Dev page shows when it is; a resume does not deploy it again.", strings.TrimSuffix(res.Message, "."), err), Logs: res.Logs, ErrorCode: kube.ErrorCodeOr(err, code)}, nil
	}
	return &types.OperationResponse{Success: true, Message: strings.TrimSuffix(res.Message, ".") + "; the dashboard is ready.", Logs: res.Logs}, nil
}
