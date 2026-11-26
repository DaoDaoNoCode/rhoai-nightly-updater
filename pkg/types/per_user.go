package types

// The answers of the per-user views (5.0.0; docs/research/per-user-views.md
// and SECURITY.md "Who sees what"). Each is a fixed type: a field added
// here is shown to every signed-in user, so it is a reviewed change. None
// of them names a node, pod, project or user, and none says anything
// about the pull secret.

// Summary is GET /api/summary: the cluster's RHOAI state every signed-in
// user may see, read by the ServiceAccount.
type Summary struct {
	// UpdaterVersion is this updater's version.
	UpdaterVersion string         `json:"updaterVersion"`
	RHOAI          SummaryRHOAI   `json:"rhoai"`
	Nightly        SummaryNightly `json:"nightly"`
	DSC            SummaryDSC     `json:"dsc"`
}

// SummaryRHOAI is the installed RHOAI operator.
type SummaryRHOAI struct {
	// Installed: the RHOAI operator's Subscription exists.
	Installed bool `json:"installed"`
	// Version and Phase are the installed CSV's; Channel is the
	// Subscription's. Empty when unknown (not installed, or installing).
	Version string `json:"version"`
	Channel string `json:"channel"`
	Phase   string `json:"phase"`
}

// SummaryNightly is the installed nightly build and the newest one of its
// stream. Both builds are nil when RHOAI is not installed from the nightly
// catalog (Problem ""), or when that could not be read (Problem
// "catalog_unreadable"); LatestBuild is nil while it is unknown.
type SummaryNightly struct {
	InstalledBuild *SummaryBuild `json:"installedBuild"`
	LatestBuild    *SummaryBuild `json:"latestBuild"`
	// NewerAvailable: the newest build of the installed stream is not the
	// installed one. Only meaningful when Problem is "".
	NewerAvailable bool `json:"newerAvailable"`
	// Problem says why the comparison is not known, as one of these codes
	// (never text from what was read): "" (known, or not a nightly
	// install), "catalog_unreadable" (the nightly CatalogSource could not
	// be read), "lookup_failed" (the newest build could not be looked up
	// yet), "no_stream" (a custom catalog image: there is no stream to
	// compare with).
	Problem string `json:"problem"`
}

// SummaryNightly.Problem codes.
const (
	NightlyProblemCatalogUnreadable = "catalog_unreadable"
	NightlyProblemLookupFailed      = "lookup_failed"
	NightlyProblemNoStream          = "no_stream"
)

// SummaryBuild is a nightly build: its stream tag ("rhoai-3.6") and build
// date ("" when unknown).
type SummaryBuild struct {
	Tag       string `json:"tag"`
	BuildDate string `json:"buildDate"`
}

// SummaryDSC is the DataScienceCluster.
type SummaryDSC struct {
	Exists bool `json:"exists"`
	// Ready: its Ready condition is True.
	Ready bool `json:"ready"`
	// Components are its components, by name: managementState as the
	// DataScienceCluster sets it ("" for a condition without a component
	// of the same name) and whether the operator reports it ready.
	Components []SummaryComponent `json:"components"`
}

// SummaryComponent is one DataScienceCluster component.
type SummaryComponent struct {
	Name            string `json:"name"`
	ManagementState string `json:"managementState"`
	Ready           bool   `json:"ready"`
}

// OperationSummary is GET /api/operation for a user who is not a
// cluster-admin: whether an operation runs, its kind's label ("Update to
// nightly"; never its target or who started it) and when it started
// (RFC 3339, "" when none runs).
type OperationSummary struct {
	Running   bool   `json:"running"`
	Label     string `json:"label"`
	StartedAt string `json:"startedAt"`
	// Operations lists what runs, as far as this user may see it: the
	// running cluster operation (the fields above, as a list entry), and
	// the running project actions in the user's own data science projects
	// (never who runs another person's).
	Operations []OperationSummaryEntry `json:"operations"`
}

// OperationSummaryEntry is one entry of OperationSummary.Operations: its
// scope ("cluster" or "project"), its kind's label and when it started
// (RFC 3339); a project action's project. Own marks the user's own action:
// only then is ID (what POST /api/operation/cancel names) set, and
// Cancellable and Cancelling may be.
type OperationSummaryEntry struct {
	Scope       string `json:"scope"`
	Label       string `json:"label"`
	StartedAt   string `json:"startedAt"`
	Project     string `json:"project,omitempty"`
	Own         bool   `json:"own,omitempty"`
	ID          string `json:"id,omitempty"`
	Cancellable bool   `json:"cancellable,omitempty"`
	CancelNote  string `json:"cancelNote,omitempty"`
	Cancelling  bool   `json:"cancelling,omitempty"`
}

// ProjectStatus is GET /api/resources/status?project= for a user who is
// not a cluster-admin: one of their data science projects, read with their
// own token. It holds that project's objects only.
type ProjectStatus struct {
	Project string `json:"project"`
	// PipelineServers are the project's pipeline servers
	// (DataSciencePipelinesApplications), this tool's and others'
	// (ManagedByTool). Nil when the project's pipeline servers cannot be
	// read (PipelineServersProblem says why).
	PipelineServers        []ResourceState `json:"pipelineServers"`
	PipelineServersProblem string          `json:"pipelineServersProblem,omitempty"`
	// PipelineServersProblemCode is the problem's kind (Problem*), "" for
	// none; likewise for the others.
	PipelineServersProblemCode string `json:"pipelineServersProblemCode,omitempty"`
	// Models are the project's LLMInferenceServices.
	Models []ModelState `json:"models"`
	// ModelCatalog are the models a project model action sets up (this
	// tool's fixed catalog; no cluster read, no capacity warning).
	ModelCatalog      []CatalogModel `json:"modelCatalog,omitempty"`
	ModelsProblem     string         `json:"modelsProblem,omitempty"`
	ModelsProblemCode string         `json:"modelsProblemCode,omitempty"`
	// InferenceServices are the project's KServe InferenceServices.
	InferenceServices            []ProjectInferenceService `json:"inferenceServices"`
	InferenceServicesProblem     string                    `json:"inferenceServicesProblem,omitempty"`
	InferenceServicesProblemCode string                    `json:"inferenceServicesProblemCode,omitempty"`
	// DashboardURL is the RHOAI dashboard's address ("" when unknown), for
	// links to its project pages.
	DashboardURL string `json:"dashboardUrl,omitempty"`
}

// ProjectStatus problem codes: the user's own account may not read the
// part, the API is not installed on the cluster, or another error.
const (
	ProblemForbiddenForUser = "forbidden_for_user"
	ProblemNotInstalled     = "not_installed"
	ProblemError            = "error"
)

// ProjectInferenceService is one InferenceService of a project.
type ProjectInferenceService struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
	// Message is its Ready condition's reason or message when not ready.
	Message string `json:"message,omitempty"`
	// URL is status.url ("" when it has none, or one the page must not
	// link to).
	URL         string `json:"url,omitempty"`
	Terminating bool   `json:"terminating,omitempty"`
}
