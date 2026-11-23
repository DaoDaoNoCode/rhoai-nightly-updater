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
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dsc"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/mlflow"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/operator"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Below, run steps follow runner order. Except for the project step, each
// invokes the same handler as its UI control (Install latest nightly,
// Create DataScienceCluster, prerequisite setup for a feature, Environment
// page setup), so recipes cannot alter anything someone could not set in
// the app.
const (
	StepRHOAI           = "rhoai"
	StepDSC             = "dsc"
	StepProject         = "project"
	StepPipelinesPrereq = "pipelines-prereq"
	StepS3              = "s3"
	StepPipelineServer  = "pipeline-server"
	StepMLflowPrereq    = "mlflow-prereq"
	StepMLflow          = "mlflow"
)

// State is per-run data that flows across steps and what gets recorded:
// the recipe, the build pinned at start, and the project when identified.
type State struct {
	Recipe  types.EnvironmentRecipe
	Pinned  *types.RecipePinnedBuild
	Project *types.RecipeProjectRef
	// Components names DataScienceCluster pieces enabled by prerequisite
	// steps; the runner retains the slice when Resume runs.
	Components []string
}

// AddComponents logs newly enabled components (deduplicated, sorted).
func (s *State) AddComponents(keys ...string) {
	for _, k := range keys {
		if !slices.Contains(s.Components, k) {
			s.Components = append(s.Components, k)
		}
	}
	slices.Sort(s.Components)
}

// Ownership reflects what a run provisioned, matching current cluster state:
// per step, whether its slice still counts as created by this run, and a
// Clean up request (POST /api/resources/cleanup/stream) for those slices,
// or nil when nothing qualifies. Only the run record supplies lookup
// names—nothing else:
//   - the project counts while it remains the recipe-created one
//     (deletable via the tool label, recipe annotation, UID stored on the
//     recipes project record), still bears the run-recorded UID, and is not
//     terminating; clean-up includes that UID (RecipeProjectUIDs);
//   - DataScienceCluster components the run enabled and that still match
//     that configuration (liveComponents); a prerequisite step reads as
//     created while any of its feature components do.
//
// S3 storage, MLflow, and pipeline servers are never attributed to the
// run: storage and MLflow are cluster-wide, and the cluster does not track
// creator identity—claims of run ownership could be faked. A pipeline server
// in the recipe project is cleaned with the project; others fall under
// Clean up everything on the Environment page.
func Ownership(c *kube.Client, run *types.RecipeRun) (map[string]bool, *types.CleanupRequest) {
	created := map[string]bool{}
	if run == nil {
		return created, nil
	}
	req := &types.CleanupRequest{}
	if p := run.Project; p != nil && p.Name != "" && p.Owned && p.UID != "" {
		// Match only the project this run created (UID): if it was
		// removed and recreated later by another run, it is not ours.
		if found, _, err := findProject(c, p.Name); err == nil && found.deletable() && !found.meta.Terminating() && found.meta.UID == p.UID {
			created[StepProject] = true
			req.RecipeProjects = []string{p.Name}
			req.RecipeProjectUIDs = map[string]string{p.Name: p.UID}
		}
	}
	for key, feature := range liveComponents(c, run.Components) {
		req.Components = append(req.Components, key)
		created[prereqStepOf[feature]] = true
	}
	delete(created, "")
	slices.Sort(req.Components)
	if len(req.RecipeProjects) == 0 && len(req.Components) == 0 {
		return created, nil
	}
	return created, req
}

// prereqStepOf maps a feature name to its prerequisite step id.
var prereqStepOf = map[string]string{"pipelines": StepPipelinesPrereq, "mlflow": StepMLflowPrereq}

// liveComponents returns component keys the run still owns, each paired
// with the enabling feature: Managed on the DataScienceCluster and
// present in the tool's prerequisite record (what Clean up reverts). Failed
// reads yield an empty map (Clean up everything remains available).
func liveComponents(c *kube.Client, keys []string) map[string]string {
	if len(keys) == 0 {
		return nil
	}
	rec, err := features.ReadPrereqRecord(c)
	if err != nil {
		return nil
	}
	read, err := rhoai.ReadFirstDSC(c)
	if err != nil || read.State != rhoai.DSCStatePresent {
		return nil
	}
	spec, _ := read.Object["spec"].(map[string]any)
	comps, _ := spec["components"].(map[string]any)
	out := map[string]string{}
	for _, k := range keys {
		comp, _ := comps[k].(map[string]any)
		if r, recorded := rec.Components[k]; recorded && comp["managementState"] == "Managed" {
			out[k] = r.Feature
		}
	}
	return out
}

// Check holds the outcome of a step's read-only precondition probe.
type Check struct {
	// Done means the step's target already exists; the run skips execution.
	Done bool
	// Blocked explains why the step cannot proceed (run halts); empty when
	// allowed.
	Blocked string
	// Reason describes cluster state when Done is true.
	Reason string
	// Fix points the operator at where to unblock the step.
	Fix *types.RecipeFix
}

// fixAt builds a RecipeFix for code, targeting page (empty page if none).
func fixAt(code, page string) *types.RecipeFix {
	return &types.RecipeFix{Code: code, Page: page}
}

// In-app routes that RecipeFix values reference.
const (
	pageStatus       = "/"
	pageComponents   = "/components"
	pageDashboardDev = "/dashboard-dev"
	pageEnvironment  = "/environment"
)

// Step describes a single run stage. Check is read-only; Run performs setup
// and may assume Check reported not done and not blocked. Run returns
// *types.OperationResponse for the outcome (Success false halts the run);
// the error slot carries context cancellation or an failure Run could not
// encode (see CONTRIBUTING, Errors and operation results).
type Step struct {
	ID, Title    string
	Typical, Max time.Duration
	Check        func(c *kube.Client, s *State) (Check, error)
	Run          func(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error)
}

// Seconds exposes typical and maximum durations in whole seconds—the same
// numbers shown on the plan and in the run record.
func (s Step) Seconds() (typical, most int) {
	return int(s.Typical / time.Second), int(s.Max / time.Second)
}

// Steps lists a recipe's stages in execution order: RHOAI build,
// DataScienceCluster, optional project, then per-feature prerequisite and
// setup steps.
func Steps(r types.EnvironmentRecipe) []Step {
	steps := []Step{
		{ID: StepRHOAI, Title: "RHOAI", Typical: 7 * time.Minute, Max: 15 * time.Minute, Check: checkRHOAI, Run: runRHOAI},
		{ID: StepDSC, Title: "DataScienceCluster", Typical: time.Minute, Max: 7 * time.Minute, Check: checkDSC, Run: runDSC},
	}
	if r.Spec.Project != nil {
		steps = append(steps, Step{ID: StepProject, Title: "Project " + r.Spec.Project.Name, Typical: 10 * time.Second, Max: time.Minute, Check: checkProjectStep, Run: EnsureProject})
	}
	f := r.Spec.Features
	if f.Pipelines.Enabled {
		steps = append(steps,
			Step{ID: StepPipelinesPrereq, Title: "What Pipelines needs", Typical: 2 * time.Minute, Max: 10 * time.Minute, Check: checkPipelinesPrereq, Run: runPipelinesPrereq},
			Step{ID: StepS3, Title: "S3 storage", Typical: time.Minute, Max: 5 * time.Minute, Check: checkS3, Run: runS3},
			Step{ID: StepPipelineServer, Title: "Pipeline server", Typical: 2 * time.Minute, Max: 10 * time.Minute, Check: checkPipelineServer, Run: runPipelineServer},
		)
	}
	if f.MLflow.Enabled {
		steps = append(steps,
			Step{ID: StepMLflowPrereq, Title: "What MLflow needs", Typical: 2 * time.Minute, Max: 10 * time.Minute, Check: checkMLflowPrereq, Run: runMLflowPrereq},
			Step{ID: StepMLflow, Title: "MLflow", Typical: 2 * time.Minute, Max: 10 * time.Minute, Check: checkMLflow, Run: runMLflow},
		)
	}
	return steps
}

// fail is a step's refusal or failure.
func fail(msg, code string) (*types.OperationResponse, error) {
	return &types.OperationResponse{Success: false, Message: msg, ErrorCode: code}, nil
}

// --- RHOAI -------------------------------------------------------------------

// halfInstalledMessage refuses to go on over an operator that is neither
// absent nor installed: an Update that was interrupted, or an install OLM
// has not finished. The recipe does not guess what to do with it.
const halfInstalledMessage = "The RHOAI operator is half installed (%s). A recipe does not go on from here: check the Status page, or RUNBOOK section 6 (an interrupted operation), then run the recipe again."

// checkRHOAI: no operator → run; an operator whose CSV succeeded is kept
// (ifInstalled keep), or is done when the nightly catalog already serves
// the pinned build (ifInstalled update); anything in between is blocked.
// A Dashboard Dev session blocks an install or update, as it blocks Update.
func checkRHOAI(c *kube.Client, s *State) (Check, error) {
	sub, err := rhoai.ReadSubscription(c)
	if err != nil {
		return Check{}, fmt.Errorf("read the RHOAI Subscription: %w", err)
	}
	csv, err := rhoai.CSVForInstalledName(c, sub.InstalledCSV)
	if err != nil {
		return Check{}, fmt.Errorf("read the RHOAI operator: %w", err)
	}
	installed := csv.Name != ""
	switch {
	case !sub.Found && !installed:
		// Not installed: install.
	case csv.Phase == "Succeeded" && sub.Found:
		if s.Recipe.Spec.RHOAI.IfInstalled != IfInstalledUpdate {
			return Check{Done: true, Reason: fmt.Sprintf("RHOAI %s is installed; the recipe keeps it (ifInstalled: keep).", csv.Version)}, nil
		}
		cs, err := rhoai.GetCatalogSource(c)
		if err != nil {
			return Check{}, fmt.Errorf("read the nightly catalog: %w", err)
		}
		if s.Pinned != nil && cs.Exists && strings.HasSuffix(cs.Image, "@"+s.Pinned.Digest) {
			return Check{Done: true, Reason: fmt.Sprintf("RHOAI %s runs the recipe's build (%s).", csv.Version, s.Pinned.Image)}, nil
		}
	default:
		what := fmt.Sprintf("Subscription %s, CSV phase %s", map[bool]string{true: "present", false: "missing"}[sub.Found], kube.FirstNonEmpty(csv.Phase, "unknown"))
		return Check{Blocked: fmt.Sprintf(halfInstalledMessage, what), Fix: fixAt(types.RecipeFixOperatorHalfInstalled, pageStatus)}, nil
	}
	active, op, err := dashdev.ActiveOperator(c)
	if err != nil {
		return Check{}, fmt.Errorf("check for a Dashboard Dev session: %w", err)
	}
	if active {
		return Check{Blocked: fmt.Sprintf("A Dashboard Dev session is active (%s), and installing or updating RHOAI under it would keep the dashboard paused. Revert the dashboard on the Dashboard Dev page first. Nothing was changed.", dashdev.PausedOperatorText(op)), Fix: fixAt(types.RecipeFixDashboardDevActive, pageDashboardDev)}, nil
	}
	return Check{}, nil
}

// runRHOAI installs or updates to the pinned build with Update, the path
// of the Status page's Install latest nightly. It never ends a Dashboard
// Dev session (OperationOptions zero: no RevertDashboardDev).
func runRHOAI(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	if s.Pinned == nil || s.Pinned.Image == "" {
		return fail("The recipe's build was not resolved when the run started. Run the recipe again. Nothing was changed.", "prerequisites")
	}
	if problem := PinnedBuildProblem(s.Recipe.Spec.RHOAI.Build, s.Pinned); problem != "" {
		return fail(problem, "validation")
	}
	return operator.UpdateStreamWithOptions(c, s.Pinned.Image, operator.OperationOptions{}, emit)
}

// --- DataScienceCluster ------------------------------------------------------

// Bounds of the DataScienceCluster step. Right after the operator's CSV
// succeeded its webhook may not serve yet, so a create that fails that way
// is tried again for dscCreateRetry; then the step waits dscConditionsWait
// for the DSC to report conditions (the operator reconciles it). It does
// not wait for the whole DSC to be Ready: on a default DSC a component can
// report missing dependencies until a feature's prerequisites are in, and
// each feature waits for its own components. Tests set them with
// SetDSCTimingForTest.
var (
	dscCreateRetry      = 2 * time.Minute
	dscCreateRetryDelay = 10 * time.Second
	dscConditionsWait   = 5 * time.Minute
	dscConditionsPoll   = 5 * time.Second
)

// checkDSC: a DSC exists → done. Without one the step creates the release
// default, or (dataScienceCluster.create false) is blocked.
func checkDSC(c *kube.Client, s *State) (Check, error) {
	read, err := rhoai.ReadFirstDSC(c)
	if err != nil {
		return Check{}, fmt.Errorf("read the DataScienceCluster: %w", err)
	}
	if read.State == rhoai.DSCStatePresent {
		return Check{Done: true, Reason: fmt.Sprintf("DataScienceCluster %s exists.", dscName(read.Object))}, nil
	}
	if !s.Recipe.Spec.DataScienceCluster.Create {
		return Check{Blocked: "There is no DataScienceCluster, and the recipe does not create one (dataScienceCluster.create: false). Create it on the Components page, or set dataScienceCluster.create: true.", Fix: fixAt(types.RecipeFixDSCMissing, pageComponents)}, nil
	}
	return Check{}, nil
}

func dscName(obj map[string]any) string {
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if name, ok := meta["name"].(string); ok {
			return name
		}
	}
	return ""
}

// runDSC creates the installed release's default DataScienceCluster
// (dsc.CreateDefault: the Components page's Create), trying a transient
// failure again, then waits until the operator reports its conditions.
func runDSC(c *kube.Client, _ *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	emit(types.UpdateStepEvent{Step: StepDSC, Status: "running", Message: "Creating the release's default DataScienceCluster..."})
	retryCtx, cancel := kube.Clock().WithTimeout(c.Context(), dscCreateRetry)
	defer cancel()
	var res *types.OperationResponse
	for {
		var err error
		res, err = dsc.CreateDefault(c)
		if err != nil {
			return res, err
		}
		if res.Success || !transientCreateFailure(res) {
			break
		}
		emit(types.UpdateStepEvent{Step: StepDSC, Status: "running", Message: "The operator is not ready to take the DataScienceCluster yet; trying again...", Detail: res.Message})
		if kube.Clock().Sleep(retryCtx, dscCreateRetryDelay) != nil {
			if err := c.Context().Err(); err != nil {
				return res, err
			}
			break
		}
	}
	if !res.Success {
		return res, nil
	}
	emit(types.UpdateStepEvent{Step: StepDSC, Status: "running", Message: "Waiting for the operator to report the DataScienceCluster's conditions..."})
	if err := waitDSCConditions(c); err != nil {
		if ctxErr := c.Context().Err(); ctxErr != nil {
			return &types.OperationResponse{Success: false, Message: "Stopped while waiting for the DataScienceCluster.", Logs: res.Logs}, ctxErr
		}
		return &types.OperationResponse{Success: false, Message: res.Message + ", but " + err.Error() + ". Resume waits again.", Logs: res.Logs, ErrorCode: "timeout"}, nil
	}
	return res, nil
}

// transientCreateFailure: the create failed in a way that a moment later
// may not (the API server or the operator's webhook was not reachable).
func transientCreateFailure(res *types.OperationResponse) bool {
	if res.ErrorCode == "network" {
		return true
	}
	for _, s := range []string{"failed calling webhook", "no endpoints available", "connection refused", "the server is currently unable to handle the request"} {
		if strings.Contains(res.Message, s) {
			return true
		}
	}
	return false
}

// errDSCNoConditions: the operator did not report the DSC's conditions in
// time.
var errDSCNoConditions = errors.New("the operator has not reported its conditions yet")

// waitDSCConditions polls until the DataScienceCluster has status
// conditions, or dscConditionsWait passes.
func waitDSCConditions(c *kube.Client) error {
	ctx, cancel := kube.Clock().WithTimeout(c.Context(), dscConditionsWait)
	defer cancel()
	cc := c.WithContext(ctx)
	for {
		read, err := rhoai.ReadFirstDSC(cc)
		if err == nil && read.State == rhoai.DSCStatePresent {
			if st, ok := read.Object["status"].(map[string]any); ok {
				if conds, ok := st["conditions"].([]any); ok && len(conds) > 0 {
					return nil
				}
			}
		}
		if ctx.Err() != nil || kube.Clock().Sleep(ctx, dscConditionsPoll) != nil {
			if err := c.Context().Err(); err != nil {
				return err
			}
			return fmt.Errorf("%w (waited %s)", errDSCNoConditions, dscConditionsWait)
		}
	}
}

// --- feature prerequisites ---------------------------------------------------

// checkPrereq: the feature's prerequisites are ready → done; a reason the
// setup cannot make them ready blocks.
func checkPrereq(c *kube.Client, feature string) (Check, error) {
	st, err := features.GetPrerequisitesStatusFor(c, "")
	if err != nil {
		return Check{}, fmt.Errorf("read what %s needs: %w", feature, err)
	}
	fp := st.Features[feature]
	switch {
	case fp.Ready:
		return Check{Done: true, Reason: "Everything it needs is set up."}, nil
	case fp.SetupBlockedReason != "":
		return Check{Blocked: fp.SetupBlockedReason, Fix: fixAt(types.RecipeFixFeatureBlocked, pageEnvironment)}, nil
	}
	return Check{}, nil
}

func checkPipelinesPrereq(c *kube.Client, _ *State) (Check, error) {
	return checkPrereq(c, "pipelines")
}
func checkMLflowPrereq(c *kube.Client, _ *State) (Check, error) { return checkPrereq(c, "mlflow") }

// runPipelinesPrereq is Set up what Pipelines needs.
func runPipelinesPrereq(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	return runPrereq(c, s, StepPipelinesPrereq, "pipelines", emit)
}

// runMLflowPrereq is Set up what MLflow needs.
func runMLflowPrereq(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	return runPrereq(c, s, StepMLflowPrereq, "mlflow", emit)
}

// prereqRecordReadTimeout bounds the read of the prerequisites record
// after the setup, which also runs when the step's context has ended.
const prereqRecordReadTimeout = 10 * time.Second

// runPrereq runs Set up what feature needs and records the
// DataScienceCluster components it turned on (State.Components): the keys
// of feature that its record of the prerequisites it set up gained, also
// when the setup then failed (it records a component before it turns it
// on, and Clean up turns back only what it still owns). When either read
// of that record fails nothing is claimed, and the step says so (its
// message, its progress and the log): the run's Clean up then does not
// offer them, Clean up everything still does.
func runPrereq(c *kube.Client, s *State, step, feature string, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	before, readErr := features.ReadPrereqRecord(c)
	res, err := features.RunPrerequisites(c, types.PrerequisitesRequest{Feature: feature}, emit)
	if readErr == nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Context()), prereqRecordReadTimeout)
		defer cancel()
		var after prereqRecordKeys
		if after, readErr = readPrereqKeys(c.WithContext(ctx)); readErr == nil {
			for key, f := range after {
				if _, had := before.Components[key]; !had && f == feature {
					s.AddComponents(key)
				}
			}
		}
	}
	if readErr != nil {
		note := fmt.Sprintf("The run cannot tell which DataScienceCluster components this step turned on (%v), so the recipe's Clean up does not offer them; Clean up everything on the Environment page does.", readErr)
		slog.Warn("recipe step: components not recorded", "step", step, "error", readErr)
		emit(types.UpdateStepEvent{Step: step, Status: "running", Message: note})
		if res != nil {
			res.Message = strings.TrimSpace(res.Message + " " + note)
			res.Logs = append(res.Logs, "WARN: "+note)
		}
	}
	return res, err
}

// prereqRecordKeys are the components of the prerequisites record, with
// the feature that turned each on.
type prereqRecordKeys map[string]string

func readPrereqKeys(c *kube.Client) (prereqRecordKeys, error) {
	rec, err := features.ReadPrereqRecord(c)
	if err != nil {
		return nil, err
	}
	out := prereqRecordKeys{}
	for key, comp := range rec.Components {
		out[key] = comp.Feature
	}
	return out, nil
}

// --- Pipelines ---------------------------------------------------------------

// checkS3: the tool's storage deployed and ready → done; storage that is
// not the tool's, or a reason setup refuses, blocks.
func checkS3(c *kube.Client, _ *State) (Check, error) {
	st := storage.S3Status(c)
	switch {
	case st.Deployed && st.ManagedByTool && st.Ready:
		return Check{Done: true, Reason: "The S3 storage is set up and ready."}, nil
	case st.SetupBlockedReason != "":
		return Check{Blocked: st.SetupBlockedReason, Fix: fixAt(types.RecipeFixFeatureBlocked, pageEnvironment)}, nil
	}
	return Check{}, nil
}

// runS3 is the Pipelines tab's Set up of the S3 storage, which waits for it.
func runS3(c *kube.Client, _ *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	emit(types.UpdateStepEvent{Step: StepS3, Status: "running", Message: "Setting up the S3 storage..."})
	return storage.SetupS3(c)
}

func projectOf(s *State) string {
	if s.Project != nil && s.Project.Name != "" {
		return s.Project.Name
	}
	return projectName(s.Recipe.Spec.Project)
}

// checkPipelineServer: the tool's pipeline server in the project is ready
// → done; one the tool did not create blocks (odh-dashboard shows only a
// project's first one).
func checkPipelineServer(c *kube.Client, s *State) (Check, error) {
	project := projectOf(s)
	dspas, err := storage.ListDSPAs(c, project)
	if errors.Is(err, storage.ErrDSPACRDMissing) {
		return Check{}, nil // Set up reports it
	}
	if err != nil {
		return Check{}, fmt.Errorf("list the pipeline servers in %s: %w", project, err)
	}
	for _, d := range dspas {
		if !d.ManagedByTool() {
			return Check{Blocked: fmt.Sprintf("Project %s has a pipeline server this tool did not create (%s); the dashboard shows only one per project. Nothing was changed.", project, d.Meta.Name), Fix: fixAt(types.RecipeFixFeatureBlocked, pageEnvironment)}, nil
		}
		if storage.PipelineServerState(d, "").Ready {
			return Check{Done: true, Reason: fmt.Sprintf("The pipeline server in %s is ready.", project)}, nil
		}
	}
	return Check{}, nil
}

// runPipelineServer is the Pipelines tab's Set up, then the wait the page
// makes in the browser, made here.
func runPipelineServer(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	project := projectOf(s)
	emit(types.UpdateStepEvent{Step: StepPipelineServer, Status: "running", Message: fmt.Sprintf("Setting up the pipeline server in %s...", project)})
	res, err := storage.SetupPipelineServer(c, project)
	if err != nil || !res.Success {
		return res, err
	}
	emit(types.UpdateStepEvent{Step: StepPipelineServer, Status: "running", Message: fmt.Sprintf("Waiting for the pipeline server in %s to be ready (usually 1 to 3 minutes)...", project)})
	if _, err := storage.WaitPipelineServerReady(c, project); err != nil {
		if ctxErr := c.Context().Err(); ctxErr != nil {
			return &types.OperationResponse{Success: false, Message: "Stopped while waiting for the pipeline server.", Logs: res.Logs}, ctxErr
		}
		code := "timeout"
		if errors.Is(err, storage.ErrPipelineServerFailed) {
			code = "prerequisites"
		}
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("The pipeline server in %s was set up but is not ready: %v. Resume waits again.", project, err), Logs: res.Logs, ErrorCode: kube.ErrorCodeOr(err, code)}, nil
	}
	return &types.OperationResponse{Success: true, Message: fmt.Sprintf("The pipeline server in %s is ready.", project), Logs: res.Logs}, nil
}

// --- MLflow ------------------------------------------------------------------

// checkMLflow: MLflow ready → done (whoever set it up: it is the
// cluster's one MLflow).
func checkMLflow(c *kube.Client, _ *State) (Check, error) {
	st, err := mlflow.ReadStatus(c)
	if err != nil {
		return Check{}, fmt.Errorf("read MLflow: %w", err)
	}
	if st.Ready {
		return Check{Done: true, Reason: "MLflow is ready."}, nil
	}
	if st.SetupBlockedReason != "" && !st.Deployed {
		return Check{Blocked: st.SetupBlockedReason, Fix: fixAt(types.RecipeFixFeatureBlocked, pageEnvironment)}, nil
	}
	return Check{}, nil
}

// runMLflow is the MLflow tab's Set up (when MLflow is not deployed), then
// the wait the page makes in the browser. Only the Set up makes MLflow the
// run's: one found deployed is waited for, not taken over.
func runMLflow(c *kube.Client, _ *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	var logs []string
	st, err := mlflow.ReadStatus(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read MLflow: %v. Nothing was changed.", err), kube.ErrorCodeOr(err, "prerequisites"))
	}
	if !st.Deployed {
		emit(types.UpdateStepEvent{Step: StepMLflow, Status: "running", Message: "Setting up MLflow..."})
		res, err := mlflow.Setup(c)
		if err != nil || !res.Success {
			return res, err
		}
		logs = res.Logs
	}
	emit(types.UpdateStepEvent{Step: StepMLflow, Status: "running", Message: "Waiting for MLflow to be ready (usually 1 to 3 minutes)..."})
	if _, err := mlflow.WaitReady(c); err != nil {
		if ctxErr := c.Context().Err(); ctxErr != nil {
			return &types.OperationResponse{Success: false, Message: "Stopped while waiting for MLflow.", Logs: logs}, ctxErr
		}
		code := "timeout"
		if errors.Is(err, mlflow.ErrCannotStart) {
			code = "prerequisites"
		}
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("MLflow was set up but is not ready: %v. Resume waits again.", err), Logs: logs, ErrorCode: kube.ErrorCodeOr(err, code)}, nil
	}
	return &types.OperationResponse{Success: true, Message: "MLflow is ready.", Logs: logs}, nil
}
