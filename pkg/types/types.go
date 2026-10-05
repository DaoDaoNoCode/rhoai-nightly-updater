package types

// ActivityEntry represents a single recorded user action.
type ActivityEntry struct {
	Timestamp string `json:"timestamp"`
	User      string `json:"user"`
	Action    string `json:"action"` // "update", "rollback", "create-pull-secret"
	Detail    string `json:"detail"` // e.g., the FBC image for updates
	Success   bool   `json:"success"`
}

// StatusResponse is the aggregated cluster and operator status returned by /api/status.
type StatusResponse struct {
	Cluster              ClusterInfo       `json:"cluster"`
	Subscription         SubscriptionInfo  `json:"subscription"`
	CSV                  CSVInfo           `json:"csv"`
	CatalogSource        CatalogSourceInfo `json:"catalogSource"`
	PullSecret           PullSecretInfo    `json:"pullSecret"`
	ImageMirror          ImageMirrorInfo   `json:"imageMirror"`
	InstallPlan          *InstallPlanInfo  `json:"installPlan,omitempty"`
	CatalogPod           *CatalogPodInfo   `json:"catalogPod,omitempty"`
	StableSource         string            `json:"stableSource"`
	ConsoleURL           string            `json:"consoleURL,omitempty"`
	StableChannel        string            `json:"stableChannel"`
	StableVersion        string            `json:"stableVersion,omitempty"`
	StableChannelPinned  bool              `json:"stableChannelPinned,omitempty"`
	StableDiscoveryError string            `json:"stableDiscoveryError,omitempty"`
	DSCExists            bool              `json:"dscExists"`
	Activity             []ActivityEntry   `json:"activity,omitempty"`
	Errors               []string          `json:"errors,omitempty"`
}

// ClusterInfo holds the OpenShift cluster server URL, version, and current user.
type ClusterInfo struct {
	Server  string `json:"server"`
	Version string `json:"version"`
	User    string `json:"user"`
}

// SubscriptionInfo describes the OLM Subscription resource for the RHOAI operator.
type SubscriptionInfo struct {
	Name    string `json:"name"`
	Source  string `json:"source"`
	Channel string `json:"channel"`
	State   string `json:"state"`
}

// CSVInfo describes the ClusterServiceVersion for the installed RHOAI operator.
type CSVInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Phase   string `json:"phase"`
}

// CatalogSourceInfo describes the nightly CatalogSource resource.
type CatalogSourceInfo struct {
	Exists bool   `json:"exists"`
	Name   string `json:"name"`
	Image  string `json:"image"`
	State  string `json:"state"`
}

// PullSecretInfo reports whether the additional-pull-secret exists and is valid.
type PullSecretInfo struct {
	Exists bool   `json:"exists"`
	Valid  bool   `json:"valid"`            // has correct format with quay.io/rhoai entry
	Detail string `json:"detail,omitempty"` // human-readable detail if invalid
}

// ImageMirrorInfo reports whether an ImageDigestMirrorSet exists for RHOAI images.
type ImageMirrorInfo struct {
	Exists bool   `json:"exists"`
	Name   string `json:"name"`
	Source string `json:"source"`
}

// InstallPlanInfo describes the latest OLM InstallPlan for the rhods-operator.
type InstallPlanInfo struct {
	Name     string `json:"name"`
	Phase    string `json:"phase"` // Planning, Installing, Complete, Failed
	Approved bool   `json:"approved"`
}

// CatalogPodInfo describes the catalog source pod that serves the FBC index.
type CatalogPodInfo struct {
	Name         string `json:"name"`
	Phase        string `json:"phase"` // Pending, Running
	Ready        bool   `json:"ready"`
	RestartCount int    `json:"restartCount"`
}

// UpdateRequest is the JSON body for the POST /api/update endpoint.
type UpdateRequest struct {
	Image  string `json:"image"`
	DryRun bool   `json:"dryRun"`
}

// ReinstallRequest is the JSON body for the POST /api/rollback endpoint.
// If TargetType is empty or "stable", the operator is reinstalled from the stable catalog.
// If TargetType is "nightly", the operator is reinstalled with the specified FBC Image.
type ReinstallRequest struct {
	TargetType string `json:"targetType"` // "stable", "nightly", or "custom"
	Image      string `json:"image,omitempty"`
	Channel    string `json:"channel,omitempty"` // optional channel override (e.g., "stable-3.5", "beta")
}

// CreatePullSecretRequest is the JSON body for the POST /api/setup/pull-secret endpoint.
type CreatePullSecretRequest struct {
	Auth string `json:"auth"` // base64-encoded quay.io/rhoai credentials
}

// OperationResponse is the standard response for mutating API operations.
type OperationResponse struct {
	Success   bool     `json:"success"`
	Message   string   `json:"message"`
	Logs      []string `json:"logs"`
	ErrorCode string   `json:"errorCode,omitempty"` // "unauthorized", "forbidden", "prerequisites", "network", "validation"
}

// LatestNightlyResponse contains the latest nightly tag and full image reference.
type LatestNightlyResponse struct {
	Tag   string `json:"tag"`
	Image string `json:"image"`
	Error string `json:"error,omitempty"`
}

// NightlyTag pairs a version tag with its full image reference.
type NightlyTag struct {
	Tag       string `json:"tag"`
	Image     string `json:"image"`               // full image with digest
	BuildDate string `json:"buildDate,omitempty"` // when the image was built
}

// NightlyTagsResponse wraps a list of recent nightly tags.
type NightlyTagsResponse struct {
	Tags []NightlyTag `json:"tags"`
}

// ComponentInfo describes one DSC component's management state and health.
type ComponentInfo struct {
	Name            string `json:"name"`
	ManagementState string `json:"managementState"` // "Managed", "Removed", "Unmanaged"
	Status          string `json:"status"`          // "Available", "Degraded", "Progressing", "Unknown", "Deleting"
	Message         string `json:"message,omitempty"`
	FixAction       string `json:"fixAction,omitempty"`      // fix action ID for ApplyFix
	FixTitle        string `json:"fixTitle,omitempty"`       // button text (plain English)
	FixDescription  string `json:"fixDescription,omitempty"` // one-line explanation
	FixConfirm      string `json:"fixConfirm,omitempty"`     // confirmation modal body
}

// DeploymentInfo holds readiness and image metadata for a Kubernetes Deployment.
type DeploymentInfo struct {
	Name                string            `json:"name"`
	Namespace           string            `json:"namespace"`
	Ready               int               `json:"ready"`
	Desired             int               `json:"desired"`
	Available           int               `json:"available"`
	Image               string            `json:"image"` // first container image
	UnavailableReplicas int               `json:"unavailableReplicas"`
	UpdatedReplicas     int               `json:"updatedReplicas"`
	RolloutStuck        bool              `json:"rolloutStuck"`
	RolloutMessage      string            `json:"rolloutMessage,omitempty"`
	ChangeStatus        string            `json:"changeStatus,omitempty"` // "updated", "new", or "" (unchanged)
	MatchLabels         map[string]string `json:"-"`                      // for pod matching, not serialized
	Pods                []PodInfo         `json:"pods,omitempty"`
	GitCommit           string            `json:"gitCommit,omitempty"`
	GitURL              string            `json:"gitURL,omitempty"`
	CommitDate          string            `json:"commitDate,omitempty"` // when the commit was merged
	BuildDate           string            `json:"buildDate,omitempty"`
	Version             string            `json:"version,omitempty"`
}

// DSCCompatibility reports invalid field names and differences from version-matched component defaults.
type DSCCompatibility struct {
	OperatorVersion   string   `json:"operatorVersion,omitempty"`
	Branch            string   `json:"branch,omitempty"`
	SourceURL         string   `json:"sourceURL,omitempty"`
	InvalidFields     []string `json:"invalidFields"`
	MissingComponents []string `json:"missingComponents"`
	ExtraComponents   []string `json:"extraComponents"`
	ValidationError   string   `json:"validationError,omitempty"`
	DefaultsError     string   `json:"defaultsError,omitempty"`
}

// ComponentsResponse groups DSC component statuses with deployment details.
type ComponentsResponse struct {
	DSCCompatibility *DSCCompatibility `json:"dscCompatibility,omitempty"`
	Components       []ComponentInfo   `json:"components"`
	Deployments      []DeploymentInfo  `json:"deployments"`
	DSCName          string            `json:"dscName"`
	DSCPhase         string            `json:"dscPhase"`
	DSCReason        string            `json:"dscReason,omitempty"`
	SnapshotTime     string            `json:"snapshotTime,omitempty"`
	ChangedCount     int               `json:"changedCount"`
	ConsoleURL       string            `json:"consoleURL,omitempty"`
}

// ContainerInfo holds per-container status within a pod.
type ContainerInfo struct {
	Name     string `json:"name"`
	Ready    bool   `json:"ready"`
	Restarts int    `json:"restarts"`
	State    string `json:"state"`            // "running", "waiting", "terminated"
	Reason   string `json:"reason,omitempty"` // e.g. "CrashLoopBackOff"
}

// PodInfo holds status details for a single pod used in debug output.
type PodInfo struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	Phase             string            `json:"phase"`
	Node              string            `json:"node"`
	Ready             bool              `json:"ready"`
	Restarts          int               `json:"restarts"`
	Image             string            `json:"image"`
	ImageID           string            `json:"imageID"`
	Age               string            `json:"age"`
	Containers        []ContainerInfo   `json:"containers,omitempty"`
	Labels            map[string]string `json:"-"` // for matching, not serialized
	OwnerKind         string            `json:"-"` // for matching, not serialized
	PodTemplateHash   string            `json:"podTemplateHash,omitempty"`
	SchedulingReason  string            `json:"schedulingReason,omitempty"`
	SchedulingMessage string            `json:"schedulingMessage,omitempty"`
	GitCommit         string            `json:"gitCommit,omitempty"`
	GitURL            string            `json:"gitURL,omitempty"`
	BuildDate         string            `json:"buildDate,omitempty"`
	Version           string            `json:"version,omitempty"`
}

// RelatedImage represents a component image found inside an FBC catalog bundle.
type RelatedImage struct {
	Name       string `json:"name"`
	Image      string `json:"image"`
	Category   string `json:"category,omitempty"` // "core", "workbench", "pipeline", "training", "infra", "other"
	GitCommit  string `json:"gitCommit,omitempty"`
	GitURL     string `json:"gitURL,omitempty"`
	CommitDate string `json:"commitDate,omitempty"`
	BuildDate  string `json:"buildDate,omitempty"`
	Version    string `json:"version,omitempty"`
}

// FBCContentResponse contains the extracted component images from an FBC catalog.
type FBCContentResponse struct {
	Tag           string         `json:"tag"`
	Image         string         `json:"image"`
	BundleName    string         `json:"bundleName,omitempty"`
	RelatedImages []RelatedImage `json:"relatedImages"`
	Categories    map[string]int `json:"categories,omitempty"`
	Error         string         `json:"error,omitempty"`
}

// DebugResponse groups pod information across RHOAI operator namespaces.
type DebugResponse struct {
	OperatorPods    []PodInfo `json:"operatorPods"`
	ApplicationPods []PodInfo `json:"applicationPods"`
	MarketplacePods []PodInfo `json:"marketplacePods"`
	Warnings        []string  `json:"warnings,omitempty"`
}

// DashboardState describes the current state of the rhods-dashboard deployment.
type DashboardState struct {
	OperatorAvailable       bool                `json:"operatorAvailable"`
	OperatorPaused          bool                `json:"operatorPaused"`
	OperatorError           string              `json:"operatorError,omitempty"`
	IsDevMode               bool                `json:"isDevMode"`
	DevMode                 string              `json:"devMode,omitempty"`
	DevImages               []DashboardDevImage `json:"devImages,omitempty"`
	AllDevImagesReady       bool                `json:"allDevImagesReady"`
	DevImagesMatchTarget    bool                `json:"devImagesMatchTarget"`
	DefaultImagesRestored   bool                `json:"defaultImagesRestored"`
	CurrentImage            string              `json:"currentImage"`
	DeploymentMode          string              `json:"deploymentMode"`
	IsCustomPR              bool                `json:"isCustomPR"`
	PRNumber                int                 `json:"prNumber,omitempty"`
	PRContainers            []string            `json:"prContainers,omitempty"`
	Managed                 bool                `json:"managed"`
	PodStatus               string              `json:"podStatus"`
	PodReady                bool                `json:"podReady"`
	ContainersReady         int                 `json:"containersReady"`
	ContainersTotal         int                 `json:"containersTotal"`
	Pods                    []PodInfo           `json:"pods,omitempty"`
	RolloutPending          bool                `json:"rolloutPending"`
	SchedulingFailureReason string              `json:"schedulingFailureReason,omitempty"`
	CanAssistRollout        bool                `json:"canAssistRollout"`
	DashboardURL            string              `json:"dashboardURL,omitempty"`
	// Override describes a Dashboard Dev session or an otherwise paused
	// dashboard-operator (who, when, what, and whether RHOAI changed since).
	Override *DashboardOverride `json:"override,omitempty"`
	// Flavor is the build flavor of the active session: "rhoai" or "odh".
	Flavor           string   `json:"flavor,omitempty"`
	DefaultFlavor    string   `json:"defaultFlavor,omitempty"`
	AvailableFlavors []string `json:"availableFlavors,omitempty"`
	// RolloutStuck is true when a Dashboard Dev workload reports the
	// ProgressDeadlineExceeded condition; StuckReason names the workload and cause.
	RolloutStuck bool   `json:"rolloutStuck"`
	StuckReason  string `json:"stuckReason,omitempty"`
}

type DashboardDevImage struct {
	Deployment    string `json:"deployment"`
	Container     string `json:"container"`
	EnvVar        string `json:"envVar"`
	Repository    string `json:"repository"`
	CurrentImage  string `json:"currentImage"`
	DefaultImage  string `json:"defaultImage"`
	Ready         bool   `json:"ready"`
	TargetImage   string `json:"targetImage,omitempty"`
	MatchesTarget bool   `json:"matchesTarget"`
	WorkloadUID   string `json:"-"`
	OwnerUID      string `json:"-"`
	// TargetSource is what the session asked this container to run: "pr",
	// "main" or "baseline" (no build for the target; release image).
	TargetSource string `json:"targetSource,omitempty"`
	TargetTag    string `json:"targetTag,omitempty"`
	// Running classifies CurrentImage: "release" (operator default), "pr",
	// "main" or "other"; RunningPR and Flavor ("rhoai"/"odh") describe PR/main builds.
	Running       string `json:"running,omitempty"`
	RunningPR     int    `json:"runningPR,omitempty"`
	Flavor        string `json:"flavor,omitempty"`
	BaselineImage string `json:"baselineImage,omitempty"`
	// Rollout diagnostics, from the Deployment's Progressing condition and its
	// newest not-ready pod.
	RolloutStuck   bool   `json:"rolloutStuck,omitempty"`
	RolloutMessage string `json:"rolloutMessage,omitempty"`
	WaitingReason  string `json:"waitingReason,omitempty"`
	WaitingMessage string `json:"waitingMessage,omitempty"`
	Restarts       int    `json:"restarts,omitempty"`
	PodName        string `json:"podName,omitempty"`
}

// DashboardOverride summarizes a paused dashboard-operator. It is cheap to
// compute (one read of the dashboard-operator Deployment) so any page can show it.
type DashboardOverride struct {
	// Active is true while the dashboard-operator is paused or a Dashboard Dev
	// session is recorded; the dashboard then does not follow RHOAI updates.
	Active         bool `json:"active"`
	OperatorPaused bool `json:"operatorPaused"`
	// SessionRecorded is false when the operator was paused outside the tool
	// or the session annotation was lost; SessionError explains a corrupt one.
	SessionRecorded bool   `json:"sessionRecorded"`
	SessionError    string `json:"sessionError,omitempty"`
	Mode            string `json:"mode,omitempty"`
	PRNumber        int    `json:"prNumber,omitempty"`
	Flavor          string `json:"flavor,omitempty"`
	StartedAt       string `json:"startedAt,omitempty"`
	StartedBy       string `json:"startedBy,omitempty"`
	UpdatedAt       string `json:"updatedAt,omitempty"`
	UpdatedBy       string `json:"updatedBy,omitempty"`
	// ReleaseVersionAtStart is the platform version stamped on the
	// dashboard-operator when the session started; ReleaseVersion is the current one.
	ReleaseVersionAtStart string `json:"releaseVersionAtStart,omitempty"`
	ReleaseVersion        string `json:"releaseVersion,omitempty"`
	// Stale is true when RHOAI was updated while paused: the operator would
	// now deploy different images than when the session started.
	Stale        bool     `json:"stale"`
	StaleReasons []string `json:"staleReasons,omitempty"`
	// DashboardDeleting is true when a Dashboard CR is being deleted while the
	// operator is paused: its finalizer waits until the operator resumes.
	DashboardDeleting bool `json:"dashboardDeleting"`
	// Warnings are user-facing explanations of the risks above.
	Warnings   []string                     `json:"warnings,omitempty"`
	Components []DashboardOverrideComponent `json:"components,omitempty"`
	LastAction *DashboardDevAction          `json:"lastAction,omitempty"`
}

// DashboardOverrideComponent is one container a session targets.
type DashboardOverrideComponent struct {
	Deployment string `json:"deployment"`
	Container  string `json:"container"`
	Image      string `json:"image,omitempty"`
	Source     string `json:"source,omitempty"`
	Tag        string `json:"tag,omitempty"`
}

// DashboardDevAction records the last Dashboard Dev deploy or revert start
// ("deploy-pr", "deploy-main" or "revert"); At is server time (RFC 3339, UTC).
// The outcome is in the operation response and the activity log.
type DashboardDevAction struct {
	Action string `json:"action"`
	By     string `json:"by"`
	At     string `json:"at"`
	Detail string `json:"detail,omitempty"`
}

// DeployPRRequest is the JSON body for the POST /api/dashboard/deploy-pr endpoint.
type DeployPRRequest struct {
	PR int `json:"pr"`
	// Flavor selects the dashboard build: "rhoai" (Konflux, BUILD_MODE=RHOAI,
	// the default) or "odh" (OpenShift CI). The MLflow endpoint ignores it.
	Flavor string `json:"flavor,omitempty"`
}

// DeployMainRequest is the optional JSON body for POST /api/dashboard/deploy-main.
type DeployMainRequest struct {
	Flavor string `json:"flavor,omitempty"`
}

// ResourcesStatus reports the state of test infrastructure.
type ResourcesStatus struct {
	MinIO           ResourceState   `json:"minio"`
	MLflow          ResourceState   `json:"mlflow"`
	PipelineServers []ResourceState `json:"pipelineServers"`
}

// ResourceState describes the deployment state of a single resource group.
type ResourceState struct {
	Deployed     bool   `json:"deployed"`
	Ready        bool   `json:"ready"`
	Message      string `json:"message,omitempty"`
	Namespace    string `json:"namespace,omitempty"`
	APIRoute     string `json:"apiRoute,omitempty"`
	UIRoute      string `json:"uiRoute,omitempty"`
	CurrentImage string `json:"currentImage,omitempty"`
}

// PipelineServerRequest is the JSON body for pipeline server setup/teardown.
type PipelineServerRequest struct {
	Project string `json:"project"`
}

// DiagnosticsFixRequest is the JSON body for POST /api/diagnostics/fix.
type DiagnosticsFixRequest struct {
	ProblemID string `json:"problemId"`
}
