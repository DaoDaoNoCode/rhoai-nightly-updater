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
	runRegistry      = modelregistry.RunAnnotated
	registryMeta     = modelregistry.ReadRegistryMeta
	dbConfigMeta     = serving.MaaSDBConfigMeta
	annotateDBConfig = serving.AnnotateMaaSDBConfig
	dashboardReady   = dashdev.DeploymentsReadyNow
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
		add(StepModel, modelTitle(r), 5*time.Minute, 15*time.Minute, checkModel, runModel)
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

// modelName is the catalog name of the recipe's model ("" is the catalog's
// default).
func modelName(r types.EnvironmentRecipe) string {
	if entry, ok := serving.CatalogEntry(r.Spec.Features.Models.Model); ok {
		return entry.Name
	}
	return r.Spec.Features.Models.Model
}

// modelTitle is the model step's title: the model's display name, as the
// Models tab shows it.
func modelTitle(r types.EnvironmentRecipe) string {
	if entry, ok := serving.CatalogEntry(r.Spec.Features.Models.Model); ok && entry.DisplayName != "" {
		return "Model " + entry.DisplayName
	}
	return "Model " + modelName(r)
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

// addableSamples sorts the project's samples as the MLflow tab's Add all
// sees them: ids it adds (their state allows Add), repairs it leaves (the
// demo on an MLflow an earlier version of this tool limited to 2Gi: its
// Add changes MLflow and restarts it instead), and unknown ones whose
// state could not be read ("name: why"), which are never counted as there.
func addableSamples(st *types.MLflowSamplesStatus) (ids, repairs, unknown []string) {
	for _, s := range st.Samples {
		switch {
		case s.State == "unknown":
			unknown = append(unknown, kube.FirstNonEmpty(s.Name, s.ID)+": "+kube.FirstNonEmpty(s.Message, "its state could not be read"))
		case !slices.Contains(s.Actions, "add"):
		case s.Repair != "":
			repairs = append(repairs, s.Name)
		default:
			ids = append(ids, s.ID)
		}
	}
	return ids, repairs, unknown
}

// checkMLflowSamples: nothing left to add in the project, and every
// sample's state read → done. A sample whose state could not be read is
// not done: the step runs and says so.
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
		return Check{}, nil // RunSamples reports why
	}
	ids, repairs, unknown := addableSamples(st)
	if len(ids) > 0 || len(unknown) > 0 {
		return Check{}, nil
	}
	return Check{Done: true, Reason: samplesInMessage(project, repairs)}, nil
}

// samplesInMessage says the samples are in project, and which one needs a
// change to MLflow first.
func samplesInMessage(project string, repairs []string) string {
	msg := fmt.Sprintf("The MLflow samples are in %s.", project)
	if len(repairs) > 0 {
		msg = fmt.Sprintf("The MLflow samples are in %s, except %s, which needs a change to MLflow first: add it on the MLflow tab.", project, strings.Join(repairs, ", "))
	}
	return msg
}

// runMLflowSamples is the MLflow tab's Add all in the project. A sample
// whose state could not be read fails the step (Resume checks again): it
// is not counted as added.
func runMLflowSamples(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	project := projectOf(s)
	st, err := samplesStatus(c, c, project)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the MLflow samples in %s: %v. Nothing was changed.", project, err), kube.ErrorCodeOr(err, "prerequisites"))
	}
	if !st.Available {
		return fail(kube.FirstNonEmpty(st.Reason, "MLflow is not ready.")+" Nothing was changed; Resume tries again.", "prerequisites")
	}
	ids, repairs, unknown := addableSamples(st)
	res := &types.OperationResponse{Success: true, Message: samplesInMessage(project, repairs)}
	if len(ids) > 0 {
		if res, err = runSamples(c, types.MLflowSamplesRequest{Project: project, Action: "add", Samples: ids}, emit); err != nil || res == nil || !res.Success {
			return res, err
		}
		if len(repairs) > 0 {
			res.Message = strings.TrimSpace(res.Message + fmt.Sprintf(" Not added: %s, which needs a change to MLflow first; add it on the MLflow tab.", strings.Join(repairs, ", ")))
		}
	}
	if len(unknown) > 0 {
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Cannot tell whether these MLflow samples are in %s: %s. Resume checks them again.", project, strings.Join(unknown, "; ")), Logs: res.Logs, ErrorCode: "unknown"}, nil
	}
	return res, nil
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

// runMaaSStep is the MaaS tab's Set up, with the recipe's database when
// Set up has to make one (as its dialog asks only then). When the platform
// was not this tool's before and is now (also after a failure part way),
// the run marks it as its own on the platform itself (markMaaSCreated).
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
		part, err := markMaaSCreated(rc, s)
		switch {
		case err != nil:
			noteUnrecorded(StepMaaS, "the MaaS platform", err, res, emit)
		case part != nil:
			s.AddCreated(*part)
		}
	}
	return res, runErr
}

// RecipeRunAnnotation marks a cluster-wide object a recipe run created:
// its value is the run's ID. It is written with the user's token when the
// run creates the object, and Clean up offers the object only while it
// carries it (the run record alone is never proof: the ServiceAccount can
// write it).
const RecipeRunAnnotation = "rhoai-nightly-updater/recipe-run"

// markMaaSCreated marks the MaaS platform the run set up as the run's: the
// database connection Secret maas-db-config that Set up wrote for the
// recipe's database (development or CloudNativePG) gets the run's
// annotation, and its UID is the created part's. nil when the platform is
// not this tool's after all.
func markMaaSCreated(c *kube.Client, s *State) (*types.RecipeCreatedPart, error) {
	after, err := maasStatus(c)
	if err != nil {
		return nil, err
	}
	if !after.Platform.ManagedByTool {
		return nil, nil
	}
	if s.RunID == "" {
		return nil, errors.New("the run has no ID")
	}
	rec, err := maasRecord(c)
	if err != nil {
		return nil, err
	}
	if rec.DBNamespace == "" || (rec.Database != MaaSDatabaseDevelopment && rec.Database != MaaSDatabaseCNPG) {
		return nil, errors.New("this tool's MaaS record names no database it made")
	}
	meta, found, err := dbConfigMeta(c, rec.DBNamespace)
	switch {
	case err != nil:
		return nil, err
	case !found || !serving.ToolOwned(meta) || meta.UID == "":
		return nil, fmt.Errorf("Secret maas-db-config in %s is not this tool's", rec.DBNamespace)
	}
	if err := annotateDBConfig(c, rec.DBNamespace, meta.UID, RecipeRunAnnotation, s.RunID); err != nil {
		return nil, err
	}
	return &types.RecipeCreatedPart{Part: types.RecipePartMaaSPlatform, UID: meta.UID}, nil
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

// exposureOf is how the run's model is reached: the recipe's own (always:
// a resume never keeps another), the one the run resolved (a gateway only
// while the Models tab still offers it), or else the one the Models tab
// recommends now for the model in the project. GatewayRef goes only with
// exposure "gateway" (ValidateModelsRequest). It does not change s.
func exposureOf(c *kube.Client, s *State) (exposure, gatewayRef, problem, code string) {
	if e := s.Recipe.Spec.Features.Models.Exposure; e != "" {
		return e, "", "", ""
	}
	if s.Exposure != "" && s.Exposure != serving.ExposureGateway {
		return s.Exposure, "", "", ""
	}
	project := projectOf(s)
	ex, msg, code := modelExposures(c, project, modelName(s.Recipe))
	if msg != "" {
		return "", "", fmt.Sprintf("Cannot tell how the model in %s would be reached: %s", project, msg), kube.FirstNonEmpty(code, "prerequisites")
	}
	if s.Exposure == serving.ExposureGateway {
		for _, opt := range ex.Options {
			if opt.Exposure == serving.ExposureGateway && opt.GatewayRef == s.GatewayRef && opt.Available {
				return opt.Exposure, opt.GatewayRef, "", ""
			}
		}
	}
	for _, opt := range ex.Options {
		if opt.Key == ex.Default && ex.Default != "" {
			if opt.Exposure != serving.ExposureGateway {
				return opt.Exposure, "", "", ""
			}
			return opt.Exposure, opt.GatewayRef, "", ""
		}
	}
	why := strings.TrimSpace(ex.RecommendGatewayReason)
	if len(ex.Options) > 0 {
		why = strings.TrimSpace(ex.Options[0].Reason + " " + why)
	}
	return "", "", fmt.Sprintf("No way to reach the model from %s is recommended now. %s Choose how the model is reached in the recipe: Only inside the cluster always works.", project, kube.FirstNonEmpty(why, "No gateway can carry it.")), "prerequisites"
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

// checkModel: the tool's model in the project serves (and, reached through
// MaaS, its publication is in place: publishedThroughMaaS) → done; a model
// of that name the tool did not create blocks.
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
	if kube.FirstNonEmpty(s.Recipe.Spec.Features.Models.Exposure, s.Exposure) == serving.ExposureMaaS {
		published, err := publishedThroughMaaS(c, project, name)
		if err != nil {
			return Check{}, fmt.Errorf("read the MaaS publication of %s in %s: %w", name, project, err)
		}
		if !published {
			return Check{}, nil
		}
		return Check{Done: true, Reason: fmt.Sprintf("Model %s in %s serves, published through MaaS.", name, project)}, nil
	}
	return Check{Done: true, Reason: fmt.Sprintf("Model %s in %s serves.", name, project)}, nil
}

// publishedThroughMaaS reports whether this tool's model name in project
// is published as the MaaS tab shows it: its MaaSModelRef is this tool's
// and Ready, the tool's access policy and subscription are Active, and
// nothing of it is half done. The model's routing alone is no publication.
func publishedThroughMaaS(c *kube.Client, project, name string) (bool, error) {
	st, err := maasStatus(c)
	if err != nil {
		return false, err
	}
	for _, it := range st.Items {
		if it.Namespace == project && it.Name == name {
			return it.Published && it.Ready && !it.Partly, nil
		}
	}
	return false, nil
}

// stepPoll is how often a step reads the cluster while it waits for what
// a setup started (a model to serve, a workbench to go). Tests set it to
// zero.
var stepPoll = 10 * time.Second

// runModel is the Models tab's Set up of the model in the project, reached
// the way the run resolved (with exposure maas it is published through
// MaaS as part of it, as the Models tab does). It succeeds only once the
// model serves (and is published, through MaaS), within the step's own
// deadline; otherwise it fails, and Resume waits again.
func runModel(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	if problem, code := pinExposure(c, s); problem != "" {
		return fail(problem+" Nothing was changed.", code)
	}
	m := s.Recipe.Spec.Features.Models
	res, err := runModels(c, types.ModelsRequest{Action: "setup", Project: projectOf(s), Model: modelName(s.Recipe), ReleasedImage: m.ReleasedImage, Exposure: s.Exposure, GatewayRef: s.GatewayRef}, emit)
	if err != nil || res == nil || !res.Success {
		return res, err
	}
	for {
		chk, err := checkModel(c, s)
		switch {
		case err == nil && chk.Done:
			return res, nil
		case err == nil && chk.Blocked != "":
			return &types.OperationResponse{Success: false, Message: chk.Blocked, Logs: res.Logs, ErrorCode: "prerequisites"}, nil
		}
		if kube.Clock().Sleep(c.Context(), stepPoll) != nil {
			what := "serving"
			if s.Exposure == serving.ExposureMaaS {
				what = "serving and published through MaaS"
			}
			return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Model %s in %s was set up but is not %s within the step's time. Resume waits again.", modelName(s.Recipe), projectOf(s), what), Logs: res.Logs, ErrorCode: "timeout"}, nil
		}
	}
}

// checkMaaSPublish: the model's publication through MaaS is in place →
// done.
func checkMaaSPublish(c *kube.Client, s *State) (Check, error) {
	project, name := projectOf(s), modelName(s.Recipe)
	published, err := publishedThroughMaaS(c, project, name)
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

// runModelRegistry is the Model registry tab's Set up (with the sample
// model when the recipe asks for it). A registry it creates carries the
// run's annotation from its creation, and the run records the UID the
// tool recorded for it; the result says when the sample is registered.
func runModelRegistry(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	before, err := registryStatus(c, c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the model registry: %v. Nothing was changed.", err), kube.ErrorCodeOr(err, "prerequisites"))
	}
	creates := before.State == "absent" && before.Namespace != "" && s.RunID != ""
	var annotations map[string]string
	if creates {
		annotations = map[string]string{RecipeRunAnnotation: s.RunID}
	}
	sample := ""
	res, runErr := runRegistry(c, types.ModelRegistryRequest{Action: "setup", Samples: s.Recipe.Spec.Features.ModelRegistry.Sample}, annotations, func(e types.UpdateStepEvent) {
		if e.Step == "samples" && e.Status == "success" {
			sample = e.Message
		}
		emit(e)
	})
	if res != nil && res.Success && sample != "" {
		res.Message = strings.TrimSpace(res.Message + " " + sample)
	}
	if creates {
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

// checkWorkbench: this tool's workbench in the project exists and is not
// being deleted → done (started or not: the Workbench tab starts it); one
// being deleted is not (the step waits until it is gone and sets it up
// again); one the tool did not create, a component that is off, or a
// platform without workbenches blocks.
func checkWorkbench(c *kube.Client, s *State) (Check, error) {
	project := projectOf(s)
	item, st, err := workbenchIn(c, project)
	switch {
	case err != nil:
		return Check{}, err
	case st.Unsupported != "":
		return blockedAt(st.Unsupported), nil
	case st.Blocked != nil:
		return Check{Blocked: st.Blocked.Reason, Fix: fixAt(types.RecipeFixFeatureBlocked, pageComponents)}, nil
	case item == nil || item.Terminating:
		return Check{}, nil
	case !item.ManagedByTool:
		return blockedAt(fmt.Sprintf("Project %s has a workbench %s that this tool did not create; the recipe does not change it. Nothing was changed.", project, workbench.Name)), nil
	}
	return Check{Done: true, Reason: fmt.Sprintf("Workbench %s in %s is set up (%s).", workbench.Name, project, kube.FirstNonEmpty(item.State, "state unknown"))}, nil
}

// workbenchIn reads the Workbench tab's status of project and its
// workbench nightly-workbench (nil when there is none).
func workbenchIn(c *kube.Client, project string) (*types.WorkbenchState, *types.WorkbenchStatus, error) {
	st, err := workbenchStatus(c, c, project)
	if err != nil {
		return nil, nil, fmt.Errorf("read the workbench in %s: %w", project, err)
	}
	if slices.Contains(st.ItemsUnreadable, project) {
		return nil, nil, fmt.Errorf("the workbench in %s cannot be read: %s", project, kube.FirstNonEmpty(st.ItemsProblem, "unknown"))
	}
	for i := range st.Items {
		if st.Items[i].Namespace == project && st.Items[i].Deployed {
			return &st.Items[i], st, nil
		}
	}
	return nil, st, nil
}

// runWorkbenchStep is the Workbench tab's Set up in the project; it
// attaches the tool's S3 Connection when the project has the tool's
// pipeline server, as that tab does. A workbench being deleted is waited
// for first (Set up would take it for one that is set up), within the
// step's deadline.
func runWorkbenchStep(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	project := projectOf(s)
	for {
		item, _, err := workbenchIn(c, project)
		if err != nil {
			return fail(fmt.Sprintf("Cannot read the workbench in %s: %v. Nothing was changed.", project, err), kube.ErrorCodeOr(err, "prerequisites"))
		}
		if item == nil || !item.Terminating {
			break
		}
		emit(types.UpdateStepEvent{Step: StepWorkbench, Status: "running", Message: fmt.Sprintf("Workbench %s in %s is being deleted; waiting until it is gone...", workbench.Name, project)})
		if kube.Clock().Sleep(c.Context(), stepPoll) != nil {
			return fail(fmt.Sprintf("Workbench %s in %s is still being deleted. Nothing was changed; Resume waits again.", workbench.Name, project), "timeout")
		}
	}
	return runWorkbench(c, types.WorkbenchRequest{Action: "setup", Project: projectOf(s)}, emit)
}

// --- Dashboard Dev -----------------------------------------------------------

// checkDashboardDev: a Dashboard Dev session runs the recipe's build and
// the dashboard has rolled out → done. A session of the build that is
// still rolling out is not: the step waits for it (without deploying
// again).
func checkDashboardDev(c *kube.Client, s *State) (Check, error) {
	o, err := dashboardSession(c)
	if err != nil {
		return Check{}, fmt.Errorf("read the Dashboard Dev session: %w", err)
	}
	d := s.Recipe.Spec.DashboardDev
	if !sessionRuns(o, d) {
		return Check{}, nil
	}
	ready, _, err := dashboardReady(c)
	if err != nil || !ready {
		return Check{}, nil
	}
	return Check{Done: true, Reason: fmt.Sprintf("Dashboard Dev runs the recipe's build (%s), and the dashboard is ready.", strings.Trim(strings.TrimPrefix(dashboardDevTitle(d), "Dashboard Dev "), "()"))}, nil
}

// sessionRuns reports whether the Dashboard Dev session o runs the build d
// names.
func sessionRuns(o *types.DashboardOverride, d types.RecipeDashboardDev) bool {
	if o == nil || !o.Active || !o.SessionRecorded || o.Mode != d.Deploy || kube.FirstNonEmpty(o.Flavor, dashdev.FlavorRHOAI) != d.Flavor {
		return false
	}
	return d.Deploy != DashboardDevPR || o.PRNumber == d.PR
}

// runDashboardDev is the Dashboard Dev page's Deploy (main or the pull
// request), then the wait for the dashboard to roll out, made here as the
// page shows it. A session that runs the build already (a resume) is not
// deployed again: only the wait runs.
func runDashboardDev(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	d := s.Recipe.Spec.DashboardDev
	build := strings.Trim(strings.TrimPrefix(dashboardDevTitle(d), "Dashboard Dev "), "()")
	module, err := dashboardSession(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the Dashboard Dev session: %v. Nothing was changed.", err), kube.ErrorCodeOr(err, "prerequisites"))
	}
	res := &types.OperationResponse{Success: true, Message: fmt.Sprintf("The dashboard runs the %s build.", build)}
	if !sessionRuns(module, d) {
		emit(types.UpdateStepEvent{Step: StepDashboardDev, Status: "running", Message: fmt.Sprintf("Deploying the dashboard's %s build...", build)})
		if d.Deploy == DashboardDevPR {
			res, err = deployPR(c, d.PR, d.Flavor)
		} else {
			res, err = deployMain(c, d.Flavor)
		}
		if err != nil || res == nil || !res.Success || module == nil {
			// Without the dashboard module (the legacy flow) there is no
			// operator session to wait on.
			return res, err
		}
	}
	emit(types.UpdateStepEvent{Step: StepDashboardDev, Status: "running", Message: "Waiting for the dashboard to roll out (usually 1 to 2 minutes)..."})
	if _, err := waitDashboard(c); err != nil {
		if ctxErr := c.Context().Err(); ctxErr != nil && !errors.Is(ctxErr, context.DeadlineExceeded) {
			return &types.OperationResponse{Success: false, Message: "Stopped while waiting for the dashboard.", Logs: res.Logs}, ctxErr
		}
		code := "timeout"
		if errors.Is(err, dashdev.ErrDeploymentsFailed) {
			code = "prerequisites"
		}
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("%s, but the dashboard is not ready: %v. Resume waits again without deploying it again.", strings.TrimSuffix(res.Message, "."), err), Logs: res.Logs, ErrorCode: kube.ErrorCodeOr(err, code)}, nil
	}
	return &types.OperationResponse{Success: true, Message: strings.TrimSuffix(res.Message, ".") + "; the dashboard is ready.", Logs: res.Logs}, nil
}
