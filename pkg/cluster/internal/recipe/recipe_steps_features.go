package recipe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dashdev"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kueue"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/mlflow"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/modelregistry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/workbench"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Steps after MLflow, in the order the runner executes them. Each invokes the
// same handler as its Environment page control (or Dashboard Dev control),
// using that control's request body; Check inspects the cluster the way page
// status does so Resume can skip parts already configured.
const (
	StepMLflowSamples = "mlflow-samples"
	StepMaaS          = "maas"
	// StepModelsPrereq is what the model's access needs, once the run
	// resolved it (a recipe without an exposure): the prerequisites step
	// sets up what every way needs.
	StepModelsPrereq  = "models-prereq"
	StepModel         = "model"
	StepMaaSPublish   = "maas-publish"
	StepPlayground    = "playground"
	StepModelRegistry = "model-registry"
	StepWorkbench     = "workbench"
	StepDashboardDev  = "dashboard-dev"
)

// Indirection for status reads and step runs below; tests replace these vars.
// Each hook corresponds to a status or action entry point on the matching page.
var (
	prereqStatus     = features.GetPrerequisitesStatusFor
	prereqSetStatus  = features.GetPrerequisitesSetStatus
	runPrereqSet     = features.RunPrerequisitesSet
	turnedOnByTool   = features.TurnedOnByTool
	onComponents     = features.OnComponents
	samplesStatus    = mlflow.SamplesStatus
	runSamples       = mlflow.RunSamples
	maasStatus       = serving.GetMaaSStatus
	maasRecord       = serving.ReadMaaSRecord
	runMaaS          = serving.RunMaaSAnnotated
	modelExposures   = serving.GetModelExposures
	readModel        = readToolModel
	runModels        = serving.RunModels
	maasPublished    = serving.MaaSPublishedIn
	playgroundStatus = features.GetPlaygroundStatus
	runPlayground    = features.RunPlayground
	maasPlayground   = features.MaaSPlaygroundModelsOf
	userMaaSModels   = features.UserMaaSModels
	registryStatus   = modelregistry.ReadStatus
	registryRecord   = modelregistry.ReadRecord
	runRegistry      = modelregistry.RunAnnotated
	registryMeta     = modelregistry.ReadRegistryMeta
	dbConfigMeta     = serving.MaaSDBConfigMeta
	dashboardState   = dashdev.State
	prPublished      = prPublishedLive
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
		if f.Models.Exposure == "" {
			// The way the model is reached is resolved in the project, after
			// the MaaS step: what it needs beyond what every way needs (the
			// prerequisites step) is set up here. With MaaS, Connectivity
			// Link is there already; without it, a gateway installs it here
			// (several minutes).
			typical := 4 * time.Minute
			if f.MaaS.Enabled {
				typical = time.Minute
			}
			add(StepModelsPrereq, "What the model's access needs", typical, 15*time.Minute, checkModelsPrereq, runModelsPrereq)
		}
		add(StepModel, modelTitle(r), 5*time.Minute, 15*time.Minute, checkModel, runModel)
		if f.MaaS.Enabled && f.MaaS.Publish && f.Models.Exposure != serving.ExposureMaaS {
			add(StepMaaSPublish, "Publish the model through MaaS", 2*time.Minute, 15*time.Minute, checkMaaSPublish, runMaaSPublish)
		}
	}
	if f.Playground.Enabled {
		add(StepPlayground, "Playground", 2*time.Minute, 9*time.Minute, checkPlayground, runPlaygroundStep)
	}
	if f.ModelRegistry.Enabled {
		add(StepModelRegistry, "Model registry", 2*time.Minute, 12*time.Minute, checkModelRegistry, runModelRegistry)
	}
	addWorkbench := func() {
		add(StepWorkbench, "Workbench", 30*time.Second, 15*time.Minute, checkWorkbench, runWorkbenchStep)
	}
	if f.Workbench.Enabled && !workbenchAfterKueue(r) {
		addWorkbench()
	}
	steps = append(steps, kueueSteps(r)...)
	if workbenchAfterKueue(r) {
		// After its distributed workloads, as it runs (runOrder).
		addWorkbench()
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

// checkMaaS: the MaaS platform ready → done, whoever set it up (it is the
// cluster's one); a reason Set up refuses blocks. Done, a platform whose
// database Secret proves this run created it is recorded as the run's (a
// resume after a crash between the create and the record).
func checkMaaS(c *kube.Client, s *State) (Check, error) {
	st, err := maasStatus(c)
	if err != nil {
		return Check{}, fmt.Errorf("read the MaaS platform: %w", err)
	}
	switch {
	case st.Platform.Ready:
		// A read that fails marks nothing; the run's Set up reports it.
		_ = markMaaSCreated(c, s, st)
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
// Set up has to make one (as its dialog asks only then). The database
// connection Secret it creates carries the run's annotation from its
// creation; after the setup (also a failed one) the run records the
// platform as its own when that Secret proves it (markMaaSCreated).
func runMaaSStep(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	before, err := maasStatus(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the MaaS platform: %v. Nothing was changed.", err), kube.ErrorCodeOr(err, "prerequisites"))
	}
	req := types.MaaSRequest{Action: "setup"}
	if before.Database != nil && before.Database.Needed {
		req.Database = &types.MaaSDatabaseRequest{Kind: s.Recipe.Spec.Features.MaaS.Database}
	}
	res, runErr := runMaaS(c, req, runAnnotations(s), emit)
	rc, cancel := readWithin(c)
	defer cancel()
	after, err := maasStatus(rc)
	if err == nil {
		err = markMaaSCreated(rc, s, after)
	}
	if err != nil {
		noteUnrecorded(StepMaaS, "the MaaS platform", err, res, emit)
	}
	return res, runErr
}

// runAnnotations are the annotations a run's setups put on the
// cluster-wide objects they create: the run's ID (RecipeRunAnnotation).
func runAnnotations(s *State) map[string]string {
	if s.RunID == "" {
		return nil
	}
	return map[string]string{RecipeRunAnnotation: s.RunID}
}

// RecipeRunAnnotation marks a cluster-wide object a recipe run created:
// its value is the run's ID. It is part of the object from its creation
// (the run's setups create it so, with the user's token), and Clean up
// offers the object only while it carries it (the run record alone is
// never proof: the ServiceAccount can write it).
const RecipeRunAnnotation = "rhoai-nightly-updater/recipe-run"

// markMaaSCreated records the MaaS platform as the run's when the cluster
// proves the run created it: the platform is this tool's, and the database
// connection Secret of the database the tool made (development or
// CloudNativePG) carries the run's annotation from its creation. It reads
// only; a read that fails is returned and marks nothing.
func markMaaSCreated(c *kube.Client, s *State, st *types.MaaSStatus) error {
	if s.RunID == "" || !st.Platform.ManagedByTool {
		return nil
	}
	rec, err := maasRecord(c)
	if err != nil {
		return err
	}
	if rec.DBNamespace == "" || (rec.Database != MaaSDatabaseDevelopment && rec.Database != MaaSDatabaseCNPG) {
		return nil
	}
	meta, found, err := dbConfigMeta(c, rec.DBNamespace)
	if err != nil {
		return err
	}
	if found && meta.UID != "" && meta.Annotations[RecipeRunAnnotation] == s.RunID && serving.ToolOwned(meta) {
		s.AddCreated(types.RecipeCreatedPart{Part: types.RecipePartMaaSPlatform, UID: meta.UID})
	}
	return nil
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

// readToolModel reads the tool's catalog model in project: whether it
// exists, its state with the tool's record, and how it is reached now
// (Exposure and Gateway, as the Models tab reads them).
func readToolModel(c *kube.Client, project, name string) (types.ModelState, bool, error) {
	m, found, err := serving.ReadToolModel(c, project, name)
	if err != nil || !found {
		return types.ModelState{}, found, err
	}
	rec, err := serving.ReadModelsRecord(c)
	if err != nil {
		return types.ModelState{}, true, err
	}
	st := serving.ModelState(c, m, false, rec)
	st.Exposure, st.Gateway = serving.ModelExposureNow(c, m)
	return st, true, nil
}

// checkModel: the tool's model in the project serves (and, reached through
// MaaS, its publication is in place: publishedThroughMaaS) → done. A model
// of that name the tool did not create blocks, and so does one reached
// another way than the recipe's (the recipe does not change it: the Models
// tab deletes it and sets it up again).
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
		return blockedAt(fmt.Sprintf("Project %s has a model %s that this tool did not create, so the recipe does not change it. Nothing has run.", project, name)), nil
	case st.Terminating:
		return blockedAt(fmt.Sprintf("Model %s in %s is being deleted. Resume once it is gone.", name, project)), nil
	}
	want := kube.FirstNonEmpty(s.Recipe.Spec.Features.Models.Exposure, s.Exposure)
	if why := exposureMismatch(s, st, want); why != "" {
		return blockedAt(why), nil
	}
	if !st.Ready {
		return Check{}, nil
	}
	if want == serving.ExposureMaaS {
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

// exposureMismatch says why the existing model st is not reached the way
// the recipe wants (want; "" not known yet), or "". A model the recipe also
// publishes through MaaS reads as reached through MaaS once published.
func exposureMismatch(s *State, st types.ModelState, want string) string {
	if want == "" || st.Exposure == "" {
		return ""
	}
	if st.Exposure == want && (want != serving.ExposureGateway || s.GatewayRef == "" || st.Gateway == s.GatewayRef) {
		return ""
	}
	if m := s.Recipe.Spec.Features; st.Exposure == serving.ExposureMaaS && m.MaaS.Enabled && m.MaaS.Publish {
		return ""
	}
	words := map[string]string{serving.ExposureDefault: "through the cluster's default gateway", serving.ExposureMaaS: "through Models as a Service",
		serving.ExposureInternal: "only inside the cluster", serving.ExposureGateway: "through gateway " + st.Gateway}
	return fmt.Sprintf("Model %s in %s is already reached %s, not %s as the recipe asks; the recipe does not change it. Delete it on the Models tab and run the recipe again, or change how the recipe reaches it.",
		modelName(s.Recipe), projectOf(s), kube.FirstNonEmpty(words[st.Exposure], st.Exposure), kube.FirstNonEmpty(words[want], want))
}

// publishedThroughMaaS reports whether this tool's model name in project
// is published as the MaaS tab shows it: its MaaSModelRef is this tool's
// and Ready, the tool's access policy and subscription are Active, and
// nothing of it is half done. The model's routing alone is no publication.
func publishedThroughMaaS(c *kube.Client, project, name string) (bool, error) {
	// The whole MaaS status, as the MaaS tab's poll reads it: a waiting step reads it once per stepPoll.
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

// runMaaSPublish is the MaaS tab's Publish of the model, then the wait
// until MaaS has accepted it (checkMaaSPublish), within the step's
// deadline; it never succeeds before.
func runMaaSPublish(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	res, err := runMaaS(c, types.MaaSRequest{Action: "publish", Project: projectOf(s), Model: modelName(s.Recipe)}, nil, emit)
	if err != nil || res == nil || !res.Success {
		return res, err
	}
	for {
		chk, err := checkMaaSPublish(c, s)
		if err == nil && chk.Done {
			return res, nil
		}
		if kube.Clock().Sleep(c.Context(), stepPoll) != nil {
			return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Model %s in %s was published, but MaaS has not accepted it within the step's time. Resume waits again.", modelName(s.Recipe), projectOf(s)), Logs: res.Logs, ErrorCode: "timeout"}, nil
		}
	}
}

// --- Playground --------------------------------------------------------------

// playgroundThroughMaaS says whether the recipe's model is offered only
// through Models as a Service: the run publishes it there (the publish
// step, which takes the model out of the dashboard's Gen AI assets as the
// dashboard's MaaS checkbox does, serving maas_genai.go), or sets it up to
// be reached through MaaS (exposure maas, the recipe's or the one the run
// resolved), which publishes it as part of the model step. The dashboard's
// gen-ai API then offers it to the playground only as a MaaS model, so the
// playground step chooses that entry.
func playgroundThroughMaaS(s *State) bool {
	f := s.Recipe.Spec.Features
	return f.Models.Enabled && ((f.MaaS.Enabled && f.MaaS.Publish) || kube.FirstNonEmpty(f.Models.Exposure, s.Exposure) == serving.ExposureMaaS)
}

// maasChoice is the Models as a Service entry the playground uses for the
// recipe's model, every usable entry of it (ready, and offered by a
// subscription), and a note that says which was chosen when several are
// usable.
type maasChoice struct {
	Model  types.PlaygroundModel
	Usable []types.PlaygroundModel
	Note   string
}

// usableMaaS: the entry is ready and a MaaSSubscription offers it.
func usableMaaS(m types.PlaygroundModel) bool { return m.Ready && len(m.Subscriptions) > 0 }

// playgroundMaaSModel is the recipe's model as the playground lists it
// among the Models as a Service: an entry of a MaaSModelRef that names the
// model (never a name built here). Several MaaSModelRefs may name it: a
// usable entry is preferred, the first by name when several are. It
// returns why none can be used otherwise, with an error code.
func playgroundMaaSModel(c *kube.Client, s *State) (maasChoice, string, string) {
	project, name := projectOf(s), modelName(s.Recipe)
	all, err := maasPlayground(c, project, name)
	if err != nil {
		return maasChoice{}, fmt.Sprintf("Cannot read how Models as a Service offers model %s in %s: %v. Nothing was changed.", name, project, err), kube.ErrorCodeOr(err, "prerequisites")
	}
	var usable []types.PlaygroundModel
	for _, m := range all {
		if usableMaaS(m) {
			usable = append(usable, m)
		}
	}
	if len(usable) > 0 {
		// Only what the dashboard offers this user: maas-api lists the
		// subscriptions the user's groups and name may use, and Set up
		// checks the choice against that list.
		offered, err := userMaaSModels(c, project)
		if err != nil {
			return maasChoice{}, fmt.Sprintf("Cannot list the Models as a Service models the dashboard offers you in %s: %v. Nothing was changed.", project, err), kube.ErrorCodeOr(err, "prerequisites")
		}
		var mine []types.PlaygroundModel
		for _, m := range usable {
			if slices.ContainsFunc(offered, func(o types.PlaygroundModel) bool { return o.Name == m.Name }) {
				mine = append(mine, m)
			}
		}
		if len(mine) == 0 {
			var names []string
			for _, m := range usable {
				names = append(names, m.Name)
			}
			return maasChoice{}, fmt.Sprintf("Your Models as a Service subscriptions do not include model %s in %s (%s), so the dashboard would not set the playground up with it for you. Ask whoever manages Models as a Service to add you to a subscription that offers it (this tool's subscription %s offers the models this tool publishes; the Models as a Service tab shows who it is for), then Resume. Nothing was changed.", name, project, strings.Join(names, ", "), serving.MaaSSubscriptionName), "prerequisites"
		}
		usable = mine
	}
	// Of the entries the user may use, the one this tool's subscription
	// offers first: this tool's Publish adds the model to it.
	tool := func(m types.PlaygroundModel) bool {
		return slices.Contains(m.Subscriptions, serving.MaaSSubscriptionName)
	}
	sort.SliceStable(usable, func(i, j int) bool {
		if tool(usable[i]) != tool(usable[j]) {
			return tool(usable[i])
		}
		return usable[i].Name < usable[j].Name
	})
	switch {
	case len(all) == 0:
		return maasChoice{}, fmt.Sprintf("Model %s in %s is not published through Models as a Service (no MaaSModelRef names it), and the dashboard offers it to the playground only that way. Publish it on the Models as a Service tab, then Resume. Nothing was changed.", name, project), "prerequisites"
	case len(usable) == 0 && len(all) > 1:
		var each []string
		for _, m := range all {
			why := "phase " + kube.FirstNonEmpty(m.Status, "not reported")
			if len(m.Subscriptions) == 0 {
				why += ", offered by no subscription"
			}
			each = append(each, fmt.Sprintf("%s (%s)", m.Name, why))
		}
		return maasChoice{}, fmt.Sprintf("Model %s in %s has %d Models as a Service entries, and none is both ready and offered by a subscription: %s. Resume once the Models as a Service tab shows the model published and ready. Nothing was changed.", name, project, len(all), strings.Join(each, ", ")), "model_not_ready"
	case len(usable) == 0 && len(all[0].Subscriptions) == 0:
		return maasChoice{}, fmt.Sprintf("No Models as a Service subscription offers model %s in %s, so the playground cannot use it. Publish it again on the Models as a Service tab, then Resume. Nothing was changed.", name, project), "model_not_ready"
	case len(usable) == 0:
		return maasChoice{}, fmt.Sprintf("Model %s in %s is not ready in Models as a Service yet (%s). Resume once the Models as a Service tab shows it ready. Nothing was changed.", name, project, kube.FirstNonEmpty(all[0].Status, "not ready")), "model_not_ready"
	}
	choice := maasChoice{Model: usable[0], Usable: usable}
	if len(usable) > 1 {
		var names []string
		for _, m := range usable {
			names = append(names, m.Name)
		}
		why := "the first by name"
		if tool(usable[0]) {
			why = "the one this tool's subscription " + serving.MaaSSubscriptionName + " offers"
			if len(usable) > 1 && tool(usable[1]) {
				why += ", the first by name"
			}
		}
		choice.Note = fmt.Sprintf("Models as a Service offers model %s in %s under %d ready entries (%s); the playground uses %s, %s.", name, project, len(usable), strings.Join(names, ", "), usable[0].Name, why)
	}
	return choice, "", ""
}

// checkPlayground: complete when a playground this tool set up in the
// project is ready; blocked when a playground exists that the tool did not
// set up. Which models a ready playground offers does not depend on what
// its Set up chose: the dashboard checks the chat models only at install,
// and the playground lists, per request, the project's Gen AI models and
// the Models as a Service models the caller may use (odh-dashboard
// rhoai-3.6 packages/gen-ai/bff token_k8s_client.go generateLlamaStackConfig,
// a remote::passthrough provider; api/genai_proxy_handler.go
// GenAIProxyNSModelsHandler). So a ready playground offers the recipe's
// MaaS model too, to users whose subscriptions include it.
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

// playgroundWarnings tells, before the run, that the playground reaches the
// recipe's model through Models as a Service (playgroundThroughMaaS, as far
// as the recipe says: an exposure the run resolves to maas is known only
// when it runs).
func playgroundWarnings(r types.EnvironmentRecipe) []types.RecipeWarning {
	if !r.Spec.Features.Playground.Enabled || !playgroundThroughMaaS(&State{Recipe: r}) {
		return nil
	}
	how := "the run publishes it there"
	if r.Spec.Features.Models.Exposure == serving.ExposureMaaS {
		how = "it is reached through Models as a Service"
	}
	return []types.RecipeWarning{{Kind: types.RecipeWarningAdvice, Message: fmt.Sprintf("%s is offered to the playground only through Models as a Service (%s), so the playground uses it from there: the dashboard sets the playground up with your sign-in, as when you choose a Models as a Service model in its Gen AI studio. Chatting with it needs a Models as a Service subscription to it.", modelTitle(r), how)}}
}

// playgroundStarted reports whether this tool's playground already exists
// in the project (not being deleted); a failed read is "no", and Set up
// then checks again.
func playgroundStarted(c *kube.Client, s *State) bool {
	st, err := playgroundStatus(c, c, projectOf(s))
	if err != nil {
		return false
	}
	for _, item := range st.Items {
		if item.Namespace == projectOf(s) && item.Deployed && item.ManagedByTool && !item.Terminating {
			return true
		}
	}
	return false
}

// runPlaygroundStep runs Playground tab Set up in the project: with every
// ready chat model of the project (the tab's default), or, when the
// recipe's model is offered only through Models as a Service, with that
// model's Models as a Service entry, as when the user chooses it on the
// tab (the dashboard then calls Models as a Service with the user's own
// sign-in).
func runPlaygroundStep(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	req := types.PlaygroundRequest{Action: "setup", Project: projectOf(s)}
	// The model is chosen only for a new playground: Set up waits for one
	// this tool set up already, whatever it was set up with.
	if playgroundThroughMaaS(s) && !playgroundStarted(c, s) {
		choice, problem, code := playgroundMaaSModel(c, s)
		if problem != "" {
			return fail(problem, code)
		}
		req.Models = []types.PlaygroundModelRef{{Name: choice.Model.Name, Source: "maas"}}
		if choice.Note != "" {
			emit(types.UpdateStepEvent{Step: StepPlayground, Status: "running", Message: choice.Note})
			res, err := runPlayground(c, req, emit)
			if res != nil {
				res.Message = strings.TrimSpace(res.Message + " " + choice.Note)
				res.Logs = append(res.Logs, "NOTE: "+choice.Note)
			}
			return res, err
		}
	}
	return runPlayground(c, req, emit)
}

// --- Model registry ----------------------------------------------------------

// checkModelRegistry: this tool's registry is ready (with the sample model,
// when the recipe asks for it and the registry can take it) → done; a
// registry of its name the tool did not create blocks. Done, a registry
// that proves this run created it is recorded as the run's.
func checkModelRegistry(c *kube.Client, s *State) (Check, error) {
	st, err := registryStatus(c, c)
	if err != nil {
		return Check{}, fmt.Errorf("read the model registry: %w", err)
	}
	if st.Foreign {
		return blockedAt(fmt.Sprintf("A model registry named %s exists in %s that this tool did not create; the recipe does not change it. Nothing has run.", st.Name, st.Namespace)), nil
	}
	if st.State != "ready" || !st.ManagedByTool {
		return Check{}, nil
	}
	sampleDone := st.Sample != nil && st.Sample.Complete
	noAddress := st.Address == "" && st.AddressProblem == ""
	if s.Recipe.Spec.Features.ModelRegistry.Sample && !sampleDone && !noAddress {
		return Check{}, nil
	}
	_ = markRegistryCreated(c, s) // a read that fails marks nothing
	reason := fmt.Sprintf("Model registry %s is ready in %s.", st.Name, st.Namespace)
	if sampleDone {
		reason = fmt.Sprintf("Model registry %s is ready in %s, with the sample model %s registered.", st.Name, st.Namespace, st.Sample.Model)
	}
	return Check{Done: true, Reason: reason}, nil
}

// markRegistryCreated records the model registry as the run's when the
// cluster proves the run created it: it carries the run's annotation from
// its creation, and the tool's record owns it by its UID. It reads only.
func markRegistryCreated(c *kube.Client, s *State) error {
	if s.RunID == "" {
		return nil
	}
	meta, found, err := registryMeta(c, c)
	if err != nil || !found || meta.Annotations[RecipeRunAnnotation] != s.RunID {
		return err
	}
	rec, err := registryRecord(c)
	if err != nil {
		return err
	}
	if rec.Owns(meta) {
		s.AddCreated(types.RecipeCreatedPart{Part: types.RecipePartModelRegistry, UID: meta.UID})
	}
	return nil
}

// runModelRegistry is the Model registry tab's Set up (with the sample
// model when the recipe asks for it). A registry it creates carries the
// run's annotation from its creation, and the run records it as its own
// when that proves it (markRegistryCreated); the result says when the
// sample is registered.
func runModelRegistry(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	sample := ""
	res, runErr := runRegistry(c, types.ModelRegistryRequest{Action: "setup", Samples: s.Recipe.Spec.Features.ModelRegistry.Sample}, runAnnotations(s), func(e types.UpdateStepEvent) {
		if e.Step == "samples" && e.Status == "success" {
			sample = e.Message
		}
		emit(e)
	})
	if res != nil && res.Success && sample != "" {
		res.Message = strings.TrimSpace(res.Message + " " + sample)
	}
	rc, cancel := readWithin(c)
	defer cancel()
	if err := markRegistryCreated(rc, s); err != nil {
		noteUnrecorded(StepModelRegistry, "the model registry", err, res, emit)
	}
	return res, runErr
}

// --- Workbench ---------------------------------------------------------------

// checkWorkbench: this tool's workbench in the project exists and is not
// being deleted → done (started or not: the Workbench tab starts it; its
// hardware profile is not changed, since that restarts it); one being
// deleted is not (the step waits until it is gone and sets it up again);
// one the tool did not create, a component that is off, a platform without
// workbenches, or a project where the Workbench tab's Set up finds no
// hardware profile (workbenchProfileBlock) blocks.
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
	case item != nil && !item.Terminating && !item.ManagedByTool:
		return blockedAt(fmt.Sprintf("Project %s has a workbench %s that this tool did not create; the recipe does not change it. Nothing was changed.", project, workbench.Name)), nil
	case item != nil && !item.Terminating:
		return Check{Done: true, Reason: fmt.Sprintf("Workbench %s in %s is set up (%s).", workbench.Name, project, kube.FirstNonEmpty(item.State, "state unknown"))}, nil
	}
	return workbenchProfileBlock(s.Recipe, st), nil
}

// checkWorkbenchLater is the plan's check of a workbench step that waits for
// steps that will run (types.RecipeStepLater): it blocks when no hardware
// profile will fit the project once the run gets there, so the run does
// not stop halfway for it. The profile is chosen from what
// workbench.ProfileInputs reads; of the steps before the workbench, these
// change it:
//
//   - RHOAI and the DataScienceCluster install or change all of it, so a
//     workbench after them is checked when the run gets there;
//   - the project step labels a project it creates, or one it created
//     whose setup it completes (a resumed run),
//     kueue.openshift.io/managed=true when the dashboard's Kueue setting is
//     on (EnsureProject, as the dashboard's Create project does);
//   - with the opt-in, the Kueue steps before the workbench
//     (workbenchAfterKueue) turn the kueue component and the setting on,
//     label the project, make LocalQueue nightly-kueue and, when it is not
//     there, the tool's Queue profile (Turn on keeps one that is there, as
//     it is, disabled or not).
//
// A new project has no LocalQueue before a Kueue step but the one RHOAI
// makes in a Kueue project when the DataScienceCluster's autoCreateQueues
// is on (workbench.AutoQueue), and in a Kueue project the dashboard offers
// only Queue profiles of a LocalQueue the project has; a Queue profile
// names its LocalQueue (the HardwareProfile CRD requires a non-empty
// localQueueName, opendatahub-operator api/infrastructure/v1
// hardwareprofile_types.go L88-89, L118-120). A workbench already there is
// not changed. A read that fails is an error (the plan refuses: fail
// closed).
func checkWorkbenchLater(c *kube.Client, s *State, willRun map[string]bool) (Check, error) {
	r := s.Recipe
	p := r.Spec.Project
	if willRun[StepRHOAI] || willRun[StepDSC] || p == nil {
		return Check{}, nil
	}
	create := p.Mode == ProjectCreate
	if create && s.Project == nil && !willRun[StepProject] {
		return Check{}, nil // the project step is refused, and the plan says why
	}
	project := projectOf(s)
	if !create || s.Project != nil {
		item, _, err := workbenchIn(c, project)
		switch {
		case err != nil:
			return Check{}, err
		case item != nil && !item.Terminating:
			return Check{}, nil // there: the step finds it set up, or blocks on one the tool did not create
		}
	}
	in, err := workbenchProfileInputs(c, project)
	if err != nil {
		return Check{}, err
	}
	labelledByProjectStep := create && willRun[StepProject] && in.SettingOn && !in.Labelled
	if labelledByProjectStep {
		in.Labelled = true
	}
	if workbenchAfterKueue(r) && (willRun[StepKueuePrereq] || willRun[StepKueue]) {
		in = withKueueSetUp(in)
	}
	_, why := in.Choose()
	switch {
	case why == "":
		return Check{}, nil
	case labelledByProjectStep:
		why = fmt.Sprintf("The dashboard's Kueue setting is on, so the project step labels project %s kueue.openshift.io/managed=true (as the dashboard's Create project does) before the run creates its workbench, and then: %s", p.Name, why)
	}
	return blockedAt(strings.TrimSpace(why+" "+workbenchKueueHint(r)) + " Nothing was changed."), nil
}

// withKueueSetUp is in once the recipe's Kueue steps with the opt-in have
// run: the kueue component and the dashboard's setting on, the project
// labelled, LocalQueue nightly-kueue in it, and the tool's Queue profile
// (kueue.ProfileManifest) when there is none of its name yet.
func withKueueSetUp(in workbench.ProfileInputs) workbench.ProfileInputs {
	in.KueueComponent, in.SettingOn, in.Labelled = true, true, true
	if !slices.Contains(in.Queues, kueue.LocalQueueName) {
		in.Queues = append(slices.Clone(in.Queues), kueue.LocalQueueName)
	}
	if !slices.ContainsFunc(in.Global, func(p workbench.HardwareProfile) bool { return p.Metadata.Name == kueue.ProfileName }) {
		var tool workbench.HardwareProfile
		raw, _ := json.Marshal(kueue.ProfileManifest("", ""))
		_ = json.Unmarshal(raw, &tool)
		in.Global = append(slices.Clone(in.Global), tool)
	}
	return in
}

// workbenchProfileInputs reads what a new workbench's hardware profile is
// chosen from in project; tests may stub it.
var workbenchProfileInputs = readWorkbenchProfileInputs

func readWorkbenchProfileInputs(c *kube.Client, project string) (workbench.ProfileInputs, error) {
	dsc, err := rhoai.ReadFirstDSC(c)
	if err != nil {
		return workbench.ProfileInputs{}, fmt.Errorf("read the DataScienceCluster: %w", err)
	}
	return workbench.ReadProfileInputs(c, project, dsc)
}

// workbenchProfileBlock blocks when the Workbench tab's plan for the project
// cannot be made (workbench.BuildPlan: no hardware profile fits, or a read
// it needs failed): its Set up would refuse the same way, and the run gets
// there with the same inputs. With the project's Kueue rule as the reason
// it adds what the recipe can change (workbenchKueueHint); a read that
// failed is said as such (fail closed).
func workbenchProfileBlock(r types.EnvironmentRecipe, st *types.WorkbenchStatus) Check {
	why := strings.TrimSpace(st.PlanProblem)
	switch {
	case why == "":
		return Check{}
	case st.PlanProblemCode == "no_profile_project":
		why = strings.TrimSpace(why + " " + workbenchKueueHint(r))
	case !slices.Contains([]string{"no_profile", "no_image", "prerequisites", "validation"}, st.PlanProblemCode):
		return Check{Blocked: why + " Nothing was changed.", Fix: checkFailed()} // a read that failed
	}
	return blockedAt(why + " Nothing was changed.")
}

// workbenchKueueHint is what recipe r can change when its workbench finds
// no hardware profile in a Kueue project: create the workbench after
// distributed workloads with the dashboard's opt-in (workbenchAfterKueue).
// "" when r does so already.
func workbenchKueueHint(r types.EnvironmentRecipe) string {
	switch k := r.Spec.Features.Kueue; {
	case workbenchAfterKueue(r):
		return ""
	case k.Enabled:
		return "With spec.features.kueue.dashboardOptIn the run creates the workbench after distributed workloads, with a Queue hardware profile of the project's LocalQueue."
	}
	return "A recipe with distributed workloads with dashboardOptIn creates the workbench after them, with a Queue hardware profile of the project's LocalQueue."
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
		emit(types.UpdateStepEvent{Step: StepWorkbench, Status: "running", Message: fmt.Sprintf("Workbench %s in %s is being deleted; waiting until it is gone", workbench.Name, project)})
		if kube.Clock().Sleep(c.Context(), stepPoll) != nil {
			return fail(fmt.Sprintf("Workbench %s in %s is still being deleted. Nothing was changed; Resume waits again.", workbench.Name, project), "timeout")
		}
	}
	return runWorkbench(c, types.WorkbenchRequest{Action: "setup", Project: projectOf(s)}, emit)
}

// --- Dashboard Dev -----------------------------------------------------------

// checkDashboardDev: the recipe's build runs and the dashboard has rolled
// out → done (a Dashboard Dev session of the module's operator, or the
// legacy flow's PR image on a ready dashboard). A build that is still
// rolling out is not: the step waits for it, without deploying again. A
// pull request whose images are not published blocks (the plan says so
// before the run starts).
func checkDashboardDev(c *kube.Client, s *State) (Check, error) {
	o, err := dashboardSession(c)
	if err != nil {
		return Check{}, fmt.Errorf("read the Dashboard Dev session: %w", err)
	}
	d := s.Recipe.Spec.DashboardDev
	build := strings.Trim(strings.TrimPrefix(dashboardDevTitle(d), "Dashboard Dev "), "()")
	if o == nil {
		if done, err := legacyRuns(c, d); err != nil || done {
			if err != nil {
				return Check{}, fmt.Errorf("read the dashboard: %w", err)
			}
			return Check{Done: true, Reason: fmt.Sprintf("The dashboard runs the recipe's build (%s) and is ready.", build)}, nil
		}
	} else if sessionRuns(o, d) {
		if ready, _, err := dashboardReady(c); err == nil && ready {
			return Check{Done: true, Reason: fmt.Sprintf("Dashboard Dev runs the recipe's build (%s), and the dashboard is ready.", build)}, nil
		}
		return Check{}, nil
	}
	if d.Deploy == DashboardDevPR {
		published, why, err := prPublished(c, d.PR, d.Flavor)
		switch {
		case err != nil:
			return Check{}, fmt.Errorf("check the images of PR #%d: %w", d.PR, err)
		case !published:
			return Check{Blocked: fmt.Sprintf("Pull request #%d has no published dashboard images yet (%s). Choose a pull request whose build has finished, or run the recipe once it has.", d.PR, why), Fix: fixAt(types.RecipeFixFeatureBlocked, pageDashboardDev)}, nil
		}
	}
	return Check{}, nil
}

// legacyRuns reports whether a cluster without the dashboard module (the
// legacy flow, PR builds only) runs the recipe's PR image on a ready
// dashboard.
func legacyRuns(c *kube.Client, d types.RecipeDashboardDev) (bool, error) {
	if d.Deploy != DashboardDevPR {
		return false, nil
	}
	st, err := dashboardState(c)
	if err != nil {
		return false, err
	}
	return st.IsCustomPR && st.PRNumber == d.PR && st.PodReady, nil
}

// prPublished reports whether the images of pull request pr (flavor) are
// published for every dashboard container of the module's operator
// (dashdev.ResolveBuildTags, as Deploy resolves them), and which are not.
// Without the module (the legacy flow, or RHOAI not installed yet) it does
// not tell: Deploy checks them itself.
func prPublishedLive(c *kube.Client, pr int, flavor string) (bool, string, error) {
	_, op, err := dashdev.LoadOperator(c, false)
	if err != nil || op == nil {
		return true, "", err
	}
	images, err := dashdev.DiscoverImages(c, op)
	if err != nil {
		return false, "", err
	}
	tags := dashdev.BuildTags(DashboardDevPR, pr, flavor)
	var missing []string
	seen := map[string]bool{}
	for _, img := range images {
		if seen[img.Repository] {
			continue
		}
		seen[img.Repository] = true
		ctx, cancel := context.WithTimeout(c.Context(), buildResolveTimeout)
		_, _, found, err := dashdev.ResolveBuildTags(ctx, img.Repository, tags)
		cancel()
		if err != nil {
			return false, "", err
		}
		if !found {
			missing = append(missing, img.Repository)
		}
	}
	return len(missing) == 0, "no " + strings.Join(tags, " or ") + " in " + strings.Join(missing, ", "), nil
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
// page shows it (for the legacy flow, until the dashboard's pods are
// ready). A build that runs already (a resume) is not deployed again: only
// the wait runs.
func runDashboardDev(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	d := s.Recipe.Spec.DashboardDev
	build := strings.Trim(strings.TrimPrefix(dashboardDevTitle(d), "Dashboard Dev "), "()")
	module, err := dashboardSession(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the Dashboard Dev session: %v. Nothing was changed.", err), kube.ErrorCodeOr(err, "prerequisites"))
	}
	res := &types.OperationResponse{Success: true, Message: fmt.Sprintf("The dashboard runs the %s build.", build)}
	deployed := sessionRuns(module, d)
	if module == nil {
		deployed, err = legacyDeployed(c, d)
		if err != nil {
			return fail(fmt.Sprintf("Cannot read the dashboard: %v. Nothing was changed.", err), kube.ErrorCodeOr(err, "prerequisites"))
		}
	}
	if !deployed {
		emit(types.UpdateStepEvent{Step: StepDashboardDev, Status: "running", Message: fmt.Sprintf("Deploying the dashboard's %s build", build)})
		if d.Deploy == DashboardDevPR {
			res, err = deployPR(c, d.PR, d.Flavor)
		} else {
			res, err = deployMain(c, d.Flavor)
		}
		if err != nil || res == nil || !res.Success {
			return res, err
		}
	}
	emit(types.UpdateStepEvent{Step: StepDashboardDev, Status: "running", Message: "Waiting for the dashboard to roll out (usually 1 to 2 minutes)"})
	if module == nil {
		return waitLegacyDashboard(c, d, res)
	}
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

// legacyDeployed reports whether the legacy flow's dashboard runs the
// recipe's PR image already (ready or not).
func legacyDeployed(c *kube.Client, d types.RecipeDashboardDev) (bool, error) {
	if d.Deploy != DashboardDevPR {
		return false, nil
	}
	st, err := dashboardState(c)
	if err != nil {
		return false, err
	}
	return st.IsCustomPR && st.PRNumber == d.PR, nil
}

// waitLegacyDashboard waits, within the step's deadline, until the legacy
// flow's dashboard runs the recipe's PR image with its pods ready.
func waitLegacyDashboard(c *kube.Client, d types.RecipeDashboardDev, res *types.OperationResponse) (*types.OperationResponse, error) {
	for {
		if done, err := legacyRuns(c, d); err == nil && done {
			return &types.OperationResponse{Success: true, Message: strings.TrimSuffix(res.Message, ".") + "; the dashboard is ready.", Logs: res.Logs}, nil
		}
		if kube.Clock().Sleep(c.Context(), stepPoll) != nil {
			return &types.OperationResponse{Success: false, Message: fmt.Sprintf("%s, but the dashboard is not ready within the step's time. Resume waits again without deploying it again.", strings.TrimSuffix(res.Message, ".")), Logs: res.Logs, ErrorCode: "timeout"}, nil
		}
	}
}
