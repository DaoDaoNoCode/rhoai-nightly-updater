package types

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
)

// Environment recipes, schema v1alpha1: a declarative description of a dev
// cluster (an RHOAI build, the DataScienceCluster, a project and the
// Environment features) that the updater sets up in one run. It is
// release-agnostic: no component keys, CSV names, channels or DSC API
// versions; the runner discovers those as the Environment page does. It
// holds no secrets: the one-time cluster setup (pull secret, image mirror)
// stays manual, and MaaS's external database (its URL holds a password) is
// refused. Files are YAML or JSON; the backend reads both strictly
// (pkg/cluster internal/recipe Validate), so every field has a json and a
// yaml tag of the same name.

// EnvironmentRecipe is a whole recipe.
type EnvironmentRecipe struct {
	// APIVersion is "rhoai-nightly-updater/v1alpha1".
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	// Kind is "EnvironmentRecipe".
	Kind     string         `json:"kind" yaml:"kind"`
	Metadata RecipeMetadata `json:"metadata" yaml:"metadata"`
	Spec     RecipeSpec     `json:"spec" yaml:"spec"`
}

// RecipeMetadata names a recipe.
type RecipeMetadata struct {
	// Name is a DNS-1123 label of at most 63 characters.
	Name string `json:"name" yaml:"name"`
	// Description is at most 256 characters.
	Description string `json:"description,omitempty" yaml:"description"`
}

// RecipeSpec is what a recipe sets up, in the runner's fixed order.
type RecipeSpec struct {
	RHOAI              RecipeRHOAI              `json:"rhoai" yaml:"rhoai"`
	DataScienceCluster RecipeDataScienceCluster `json:"dataScienceCluster" yaml:"dataScienceCluster"`
	// Project is the project the features use; nil when none needs one.
	Project      *RecipeProject     `json:"project,omitempty" yaml:"project"`
	Features     RecipeFeatures     `json:"features" yaml:"features"`
	DashboardDev RecipeDashboardDev `json:"dashboardDev" yaml:"dashboardDev"`
}

// RecipeRHOAI is the RHOAI operator build.
type RecipeRHOAI struct {
	Build RecipeBuild `json:"build" yaml:"build"`
	// IfInstalled is "keep" (an installed operator stays as it is; the
	// default) or "update" (it is updated to Build).
	IfInstalled string `json:"ifInstalled,omitempty" yaml:"ifInstalled"`
}

// RecipeBuild is the RHOAI build: "latest" (the newest nightly), {tag:
// "rhoai-3.6"} (the newest nightly of a line) or {image:
// "quay.io/rhoai/rhoai-fbc-fragment@sha256:..."} (one catalog image).
// Exactly one is set.
type RecipeBuild struct {
	Latest bool   `json:"-" yaml:"-"`
	Tag    string `json:"tag,omitempty" yaml:"tag"`
	Image  string `json:"image,omitempty" yaml:"image"`
}

// recipeBuildLatest is RecipeBuild's string form.
const recipeBuildLatest = "latest"

// recipeBuildObject is RecipeBuild's object form.
type recipeBuildObject struct {
	Tag   string `json:"tag,omitempty" yaml:"tag"`
	Image string `json:"image,omitempty" yaml:"image"`
}

func recipeBuildString(s string) (RecipeBuild, error) {
	if s != recipeBuildLatest {
		return RecipeBuild{}, fmt.Errorf("build %q: use latest, {tag: ...} or {image: ...}", s)
	}
	return RecipeBuild{Latest: true}, nil
}

// UnmarshalJSON reads "latest" or an object with tag or image, refusing
// any other member (names match exactly).
func (b *RecipeBuild) UnmarshalJSON(data []byte) error {
	var s string
	if jsonv2.Unmarshal(data, &s) == nil {
		build, err := recipeBuildString(s)
		*b = build
		return err
	}
	var obj recipeBuildObject
	if err := jsonv2.Unmarshal(data, &obj, jsonv2.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("build: use latest, {tag: ...} or {image: ...}: %w", err)
	}
	*b = RecipeBuild{Tag: obj.Tag, Image: obj.Image}
	return nil
}

// MarshalJSON writes "latest" or the object form.
func (b RecipeBuild) MarshalJSON() ([]byte, error) {
	if b.Latest {
		return json.Marshal(recipeBuildLatest)
	}
	return json.Marshal(recipeBuildObject{Tag: b.Tag, Image: b.Image})
}

// UnmarshalYAML reads "latest" or a mapping with tag or image. The decoder
// that calls it applies its own settings to unmarshal, so a decoder with
// KnownFields refuses other keys here too.
func (b *RecipeBuild) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if unmarshal(&s) == nil {
		build, err := recipeBuildString(s)
		*b = build
		return err
	}
	var obj recipeBuildObject
	if err := unmarshal(&obj); err != nil {
		return fmt.Errorf("build: use latest, {tag: ...} or {image: ...}: %w", err)
	}
	*b = RecipeBuild{Tag: obj.Tag, Image: obj.Image}
	return nil
}

// RecipeDataScienceCluster is the DataScienceCluster.
type RecipeDataScienceCluster struct {
	// Create: create the installed release's default DataScienceCluster
	// when there is none. The recipe names no components.
	Create bool `json:"create" yaml:"create"`
}

// RecipeProject is the project the features use.
type RecipeProject struct {
	// Mode is "create" (the recipe creates the project, owns it and
	// removes it in Clean up) or "existing" (a data science project the
	// user already has; the recipe owns nothing of it).
	Mode string `json:"mode" yaml:"mode"`
	// Name is a DNS-1123 label of at most 63 characters.
	Name string `json:"name" yaml:"name"`
	// DisplayName is the project's display name (mode create), at most
	// 256 characters.
	DisplayName string `json:"displayName,omitempty" yaml:"displayName"`
}

// RecipeFeatures are the Environment features the recipe sets up. A
// feature that is off is left as it is.
type RecipeFeatures struct {
	// Pipelines: S3 storage and a pipeline server in the project.
	Pipelines  RecipeFeature `json:"pipelines" yaml:"pipelines"`
	MLflow     RecipeMLflow  `json:"mlflow" yaml:"mlflow"`
	Models     RecipeModels  `json:"models" yaml:"models"`
	Playground RecipeFeature `json:"playground" yaml:"playground"`
	MaaS       RecipeMaaS    `json:"maas" yaml:"maas"`
	// ModelRegistry and Workbench are optional: a recipe written before
	// they existed leaves them out, and they stay off.
	ModelRegistry RecipeModelRegistry `json:"modelRegistry,omitzero" yaml:"modelRegistry"`
	// Workbench: the tool's workbench in the project, with the tool's S3
	// Connection when the project has the tool's pipeline server.
	Workbench RecipeFeature `json:"workbench,omitzero" yaml:"workbench"`
	// Kueue: distributed workloads in the project, as the Distributed
	// workloads tab sets them up. Optional, off when left out.
	Kueue RecipeKueue `json:"kueue,omitzero" yaml:"kueue"`
}

// RecipeKueue is distributed workloads (Kueue): the tool's ResourceFlavor,
// ClusterQueue (for the project only) and LocalQueue, the project label,
// and the Ray samples and the dashboard's Kueue setting when asked for.
type RecipeKueue struct {
	Enabled bool `json:"enabled" yaml:"enabled"`
	// RayCluster and RayJob: the samples, run by KubeRay (the ray
	// component); RayImage is their image ("" the tab's default).
	RayCluster bool   `json:"rayCluster,omitempty" yaml:"rayCluster"`
	RayJob     bool   `json:"rayJob,omitempty" yaml:"rayJob"`
	RayImage   string `json:"rayImage,omitempty" yaml:"rayImage"`
	// DashboardOptIn turns the dashboard's Kueue setting on (cluster-wide:
	// every new project becomes a Kueue project) and adds the Queue
	// hardware profile. Off by default.
	DashboardOptIn bool `json:"dashboardOptIn,omitempty" yaml:"dashboardOptIn"`
}

// RecipeModelRegistry is the tool's model registry (cluster-wide, in the
// namespace RHOAI keeps registries in).
type RecipeModelRegistry struct {
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Sample: also register the sample model.
	Sample bool `json:"sample,omitempty" yaml:"sample"`
}

// RecipeFeature is a feature without options.
type RecipeFeature struct {
	Enabled bool `json:"enabled" yaml:"enabled"`
}

// RecipeMLflow is MLflow.
type RecipeMLflow struct {
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Samples: also run the MLflow samples in the project.
	Samples bool `json:"samples,omitempty" yaml:"samples"`
}

// RecipeModels is a model of the tool's catalog in the project.
type RecipeModels struct {
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Model is a model name of the tool's catalog; "" is its default.
	Model string `json:"model,omitempty" yaml:"model"`
	// Exposure is "default-gateway", "maas" or "internal"; "" takes the
	// recommended way, resolved when the step runs.
	Exposure string `json:"exposure,omitempty" yaml:"exposure"`
	// ReleasedImage: serve with the released runtime image.
	ReleasedImage bool `json:"releasedImage,omitempty" yaml:"releasedImage"`
}

// RecipeMaaS is Models as a Service.
type RecipeMaaS struct {
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Database is "development" (the default) or "cnpg". "external" is
	// refused: its connection URL holds a password.
	Database string `json:"database,omitempty" yaml:"database"`
	// Publish: publish the project's model through MaaS.
	Publish bool `json:"publish,omitempty" yaml:"publish"`
}

// RecipeDashboardDev is a Dashboard Dev build, deployed last.
type RecipeDashboardDev struct {
	// Deploy is "none" (the default), "main" or "pr".
	Deploy string `json:"deploy,omitempty" yaml:"deploy"`
	// PR is the odh-dashboard pull request, with Deploy "pr" only.
	PR int `json:"pr,omitempty" yaml:"pr"`
	// Flavor is "rhoai" (the default) or "odh".
	Flavor string `json:"flavor,omitempty" yaml:"flavor"`
}

// RecipeProblem is one reason a recipe is refused.
type RecipeProblem struct {
	// Field is the recipe field, as a path ("spec.project.name"); "" for
	// the source as a whole.
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
	// Fix says where the problem is fixed, for the UI's link; nil when
	// the recipe itself is to change (Field says where).
	Fix *RecipeFix `json:"fix,omitempty"`
}

// RecipeFix is where a plan's problem, or a blocked step, is fixed: a
// stable code the UI maps to its words and link (never parsed from the
// message), and the app page that fixes it, when one does.
type RecipeFix struct {
	Code string `json:"code"`
	// Page is the app's route ("/", "/components", ...); "" when nothing
	// in the app fixes it.
	Page string `json:"page,omitempty"`
}

// The codes of RecipeFix.
const (
	// The one-time cluster setup (pull secret, image mirror) is missing or
	// broken: the Status page.
	RecipeFixOneTimeSetupMissing = "one_time_setup_missing"
	// Argo CD or an ACM policy manages the RHOAI operator: change it in
	// Git, not in the app.
	RecipeFixArgoManaged = "argo_managed"
	// The recipe's build cannot be resolved on Quay: try again, or choose
	// another build.
	RecipeFixBuildUnresolvable = "build_unresolvable"
	// The build a run recorded is not the recipe's: resume with the build
	// resolved again.
	RecipeFixPinnedBuildRefused = "pinned_build_refused"
	// The recipe's project exists and is not the recipe's, is being
	// deleted, or has a reserved name: change spec.project.
	RecipeFixProjectForeign = "project_foreign"
	// This updater runs the gitops profile: recipes need the full one.
	RecipeFixProfileGitOps = "profile_gitops"
	// A Dashboard Dev session is active: revert it on the Dashboard Dev
	// page.
	RecipeFixDashboardDevActive = "dashboard_dev_active"
	// The RHOAI operator is half installed: the Status page.
	RecipeFixOperatorHalfInstalled = "operator_half_installed"
	// There is no DataScienceCluster and the recipe creates none: the
	// Components page.
	RecipeFixDSCMissing = "dsc_missing"
	// A feature's setup refuses (its prerequisites, the storage, a
	// pipeline server the tool did not create, MLflow): the Environment
	// page.
	RecipeFixFeatureBlocked = "feature_blocked"
	// A read of the cluster failed: try again.
	RecipeFixCheckFailed = "check_failed"
)

// RecipeRequest is the body of POST /api/recipes/plan and /api/recipes/run:
// the recipe as a JSON object, read strictly (pkg/cluster internal/recipe
// Validate).
type RecipeRequest struct {
	Recipe json.RawMessage `json:"recipe"`
}

// RecipeResumeRequest is the body of POST /api/recipes/resume.
type RecipeResumeRequest struct {
	// RunID is the run to resume: the recorded run's ID.
	RunID string `json:"runId"`
	// ReresolveBuild resolves the recipe's build again (a newer nightly of
	// the line) instead of keeping the one the run pinned.
	ReresolveBuild bool `json:"reresolveBuild,omitempty"`
}

// RecipeRunAccepted is the 202 answer of POST /api/recipes/run and resume.
type RecipeRunAccepted struct {
	RunID string `json:"runId"`
}

// The actions of a planned step.
const (
	RecipeStepRun     = "run"     // the step will set something up
	RecipeStepSkip    = "skip"    // already set up
	RecipeStepBlocked = "blocked" // the run would stop here; Reason says why
	RecipeStepLater   = "later"   // checked when the run gets there (it depends on an earlier step)
)

// RecipePlan is the answer of POST /api/recipes/plan: what a run would do.
// The run refuses while Problems is not empty; nothing was changed.
type RecipePlan struct {
	Recipe   EnvironmentRecipe `json:"recipe"`
	Steps    []RecipePlanStep  `json:"steps"`
	Problems []RecipeProblem   `json:"problems"`
	// Pinned is the build the run installs when the RHOAI step runs.
	Pinned *RecipePinnedBuild `json:"pinned,omitempty"`
	// TypicalSeconds and MaxSeconds add up the steps that run (or are
	// checked later).
	TypicalSeconds int `json:"typicalSeconds"`
	MaxSeconds     int `json:"maxSeconds"`
	// Warnings say what the run changes beyond the recipe's own parts and
	// do not refuse it (the dashboard's cluster-wide Kueue setting; other
	// setups in the project Kueue labels).
	Warnings []RecipeWarning `json:"warnings,omitempty"`
}

// The kinds of a plan's warning (RecipeWarning.Kind).
const (
	// RecipeWarningAdvice is advice before the run (a safer choice).
	RecipeWarningAdvice = "advice"
	// RecipeWarningCluster is a change the run makes to what the whole
	// cluster shares.
	RecipeWarningCluster = "cluster"
)

// RecipeWarning is one of a plan's warnings.
type RecipeWarning struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// RecipePlanStep is one step of a plan.
type RecipePlanStep struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Action string `json:"action"` // RecipeStepRun, Skip, Blocked or Later
	Reason string `json:"reason,omitempty"`
	// Fix: where a blocked step is fixed.
	Fix *RecipeFix `json:"fix,omitempty"`
	// TypicalSeconds and MaxSeconds: how long the step usually takes, and
	// at most.
	TypicalSeconds int `json:"typicalSeconds"`
	MaxSeconds     int `json:"maxSeconds"`
}

// RecipePinnedBuild is the RHOAI build a run installs, resolved once when
// the run starts, so a resume does not move to a newer nightly.
type RecipePinnedBuild struct {
	Tag    string `json:"tag,omitempty"`
	Image  string `json:"image"`
	Digest string `json:"digest"`
}

// RecipeProjectRef is the project of a run: its name, and with mode create
// the UID of the namespace the recipe created (once it did).
type RecipeProjectRef struct {
	Name  string `json:"name"`
	UID   string `json:"uid,omitempty"`
	Owned bool   `json:"owned,omitempty"`
}

// The states of a recipe run.
const (
	RecipeRunRunning   = "running"
	RecipeRunSucceeded = "succeeded"
	RecipeRunFailed    = "failed"
	RecipeRunStopped   = "stopped"
)

// The statuses of a run's step.
const (
	RecipeStepPending = "pending"
	RecipeStepRunning = "running"
	RecipeStepSuccess = "success"
	RecipeStepFailed  = "failed"
	RecipeStepSkipped = "skipped"
)

// RecipeRun is the record of the last recipe run (key recipeRun of the
// operation ConfigMap), written when a step starts and ends.
type RecipeRun struct {
	ID string `json:"id"`
	// OperationID is the cluster operation of the run (the lock's), BootID
	// and Pod the updater process that runs it.
	OperationID string `json:"operationId"`
	BootID      string `json:"bootId"`
	Pod         string `json:"pod,omitempty"`
	User        string `json:"user"`
	StartedAt   string `json:"startedAt"`
	// ResumedAt is when the run was last resumed.
	ResumedAt  string `json:"resumedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
	State      string `json:"state"`
	// StopReason says why a run that is not running ended: "requested"
	// (Stop after this step), "restart" (the updater was restarting), or
	// the step's failure.
	StopReason string `json:"stopReason,omitempty"`
	// ErrorCode is the code of why the run stopped or failed:
	// sign_in_again, session_expired, forbidden, lock_lost, internal, or
	// the failed step's.
	ErrorCode string             `json:"errorCode,omitempty"`
	Recipe    EnvironmentRecipe  `json:"recipe"`
	Pinned    *RecipePinnedBuild `json:"pinned,omitempty"`
	Project   *RecipeProjectRef  `json:"project,omitempty"`
	Steps     []RecipeRunStep    `json:"steps"`
	// Exposure is how the run's model is reached, as the Models
	// prerequisites step resolved it ("default-gateway", "maas", "internal",
	// or "gateway" with GatewayRef), kept so a resume sets the model up the
	// same way; "" until then.
	Exposure   string `json:"exposure,omitempty"`
	GatewayRef string `json:"gatewayRef,omitempty"`
	// CreatedParts are the cluster-wide parts this run (any of its
	// attempts) created: they were not this tool's before its step. Its
	// Clean up offers those still this tool's, never anything else.
	CreatedParts []RecipeCreatedPart `json:"createdParts,omitempty"`
	// Components are the DataScienceCluster components this run (any of
	// its attempts) turned on (the keys its prerequisite steps added to
	// the prerequisites record). Its Clean up offers those still as the
	// run set them.
	Components []string `json:"components,omitempty"`
	// ComponentIDs are the prerequisites record's enablement IDs of
	// Components (absent for a run or a record from before IDs): the run
	// claims only the enablement its step turned on.
	ComponentIDs map[string]string `json:"componentIds,omitempty"`
}

// The cluster-wide parts a run can create (RecipeCreatedPart.Part).
const (
	RecipePartMaaSPlatform  = "maas-platform"
	RecipePartModelRegistry = "model-registry"
	// RecipePartKueue is the distributed workloads setup; its UID is its
	// ClusterQueue's.
	RecipePartKueue = "kueue"
)

// RecipeCreatedPart is a cluster-wide part a run created: the MaaS
// platform or the model registry. UID names the object the step created
// when there is one (the registry; the MaaS development database's
// credentials Secret), so one made again since, by anyone, is not the
// run's.
type RecipeCreatedPart struct {
	Part string `json:"part"`
	UID  string `json:"uid,omitempty"`
}

// RecipeRunStep is one step of a run.
type RecipeRunStep struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
	ErrorCode  string `json:"errorCode,omitempty"`
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
	// TypicalSeconds and MaxSeconds: how long the step usually takes, and
	// at most (the plan's figures).
	TypicalSeconds int `json:"typicalSeconds"`
	MaxSeconds     int `json:"maxSeconds"`
	// Created: what the step sets up exists on the cluster as this run's,
	// worked out when GET /api/recipes/run answers a run that is not
	// running (internal/recipe Ownership); never stored in the record.
	Created bool `json:"created,omitempty"`
}

// RecipeRunStatus is the answer of GET /api/recipes/run.
type RecipeRunStatus struct {
	// Run is the last run, nil when none was recorded.
	Run *RecipeRun `json:"run"`
	// Running: the run is running now, in this updater process or
	// (Remote) in another one (Pod).
	Running bool   `json:"running"`
	Remote  bool   `json:"remote,omitempty"`
	Pod     string `json:"pod,omitempty"`
	// Interrupted: the record says running, but the updater process that
	// ran it is gone (crash, kill, node loss). Resume continues it.
	Interrupted bool `json:"interrupted,omitempty"`
	// StopRequested: Stop after this step was asked for.
	StopRequested bool `json:"stopRequested,omitempty"`
	// Step is the latest progress of the running step (from the
	// operation), when it runs.
	Step *UpdateStepEvent `json:"step,omitempty"`
	// Cleanup is the clean-up request for what the run created (POST
	// /api/resources/cleanup/stream), as the cluster shows it now, when
	// the run is not running and there is any (internal/recipe
	// CleanupRequest); the user confirms it, and the project's contents
	// when Clean up asks.
	Cleanup *CleanupRequest `json:"cleanup,omitempty"`
	// CleanupKueueSetting: removing the offered distributed workloads
	// setup also turns the dashboard's Kueue setting back, which the run
	// turned on (the Clean up dialog says so).
	CleanupKueueSetting bool `json:"cleanupKueueSetting,omitempty"`
	// Projects are the projects recipes created (this run's and earlier
	// runs') that the cluster has now, as Clean up may delete them, when no
	// run is running; ProjectsError says why they could not be read.
	Projects      []RecipeOwnedProject `json:"projects,omitempty"`
	ProjectsError string               `json:"projectsError,omitempty"`
}

// RecipeOwnedProject is a project a recipe created: Clean up deletes it
// only while it still has this UID and the recipe label.
type RecipeOwnedProject struct {
	Name      string `json:"name"`
	UID       string `json:"uid"`
	Recipe    string `json:"recipe"`
	CreatedAt string `json:"createdAt"`
}
