package types

// ActivityEntry represents a single recorded user action.
type ActivityEntry struct {
	Timestamp string `json:"timestamp"`
	User      string `json:"user"`
	Action    string `json:"action"` // "update", "rollback", "create-pull-secret"
	Detail    string `json:"detail"` // e.g., the FBC image for updates
	Success   bool   `json:"success"`
	// Reason is why a failed operation failed or was refused (stored).
	Reason string `json:"reason,omitempty"`

	// Filled in when the log is read (not stored):
	// Label is a human-readable name for Action, Category groups actions
	// ("operator", "dashboard", "resources", "setup", "diagnostics",
	// "other"), and Build is the catalog build an operator action targeted,
	// as "<tag> · <first 12 digest hex>" (either part may be missing).
	Label    string `json:"label,omitempty"`
	Category string `json:"category,omitempty"`
	Build    string `json:"build,omitempty"`
}

// OperationMarker records a running cluster operation in a ConfigMap so a
// restarted updater can tell that its predecessor died mid-operation.
type OperationMarker struct {
	Type      string `json:"type"`
	Label     string `json:"label"`
	User      string `json:"user"`
	Target    string `json:"target,omitempty"`
	StartedAt string `json:"startedAt"` // RFC 3339
	Pod       string `json:"pod,omitempty"`
	// ID is the operation's ID. BootID identifies the server process that
	// wrote the marker: a container restarted in the same pod keeps the pod
	// name (Pod) but gets a new BootID.
	ID     string `json:"id,omitempty"`
	BootID string `json:"bootId,omitempty"`
}

// OperationLease is the cross-pod operation lock, kept next to the
// operation marker. The process that holds it renews HeartbeatAt while its
// operation runs; another updater process (a replacement pod started while
// this one still drains) treats a lease with a recent heartbeat as a
// running operation and refuses to start its own. Step, StepStatus and
// Message are the latest progress event, so the other pod can show it.
// Times are RFC 3339.
type OperationLease struct {
	ID          string `json:"id"`
	BootID      string `json:"bootId"`
	Pod         string `json:"pod,omitempty"`
	User        string `json:"user"`
	Type        string `json:"type"`
	Label       string `json:"label"`
	Target      string `json:"target,omitempty"`
	StartedAt   string `json:"startedAt"`
	HeartbeatAt string `json:"heartbeatAt"`
	Step        string `json:"step,omitempty"`
	StepStatus  string `json:"stepStatus,omitempty"`
	Message     string `json:"message,omitempty"`
}

// CompletedOperation is the most recent finished cluster operation, as
// reported by GET /api/operation (lastCompleted) and kept next to the
// operation marker so it survives a restart. Times are RFC 3339.
type CompletedOperation struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Label      string `json:"label"`
	User       string `json:"user"`
	Target     string `json:"target"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt"`
	Success    bool   `json:"success"`
	Message    string `json:"message"`
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
	DashboardURL         string            `json:"dashboardURL,omitempty"`
	StableChannel        string            `json:"stableChannel"`
	StableVersion        string            `json:"stableVersion,omitempty"`
	StableChannelPinned  bool              `json:"stableChannelPinned,omitempty"`
	StableDiscoveryError string            `json:"stableDiscoveryError,omitempty"`
	DSCExists            bool              `json:"dscExists"`
	// DSCManagedBy is the Argo CD application that tracks the
	// DataScienceCluster.
	DSCManagedBy string         `json:"dscManagedBy,omitempty"`
	Nightly      *NightlyStatus `json:"nightly,omitempty"`
	// CatalogServes is the operator build the nightly catalog serves on the
	// Subscription's channel, compared with the installed operator. Only set
	// while Argo CD manages the catalog or the Subscription: Git then picks
	// the catalog image, which can run ahead of the installed operator.
	CatalogServes *CatalogServes  `json:"catalogServes,omitempty"`
	Activity      []ActivityEntry `json:"activity,omitempty"`
	Errors        []string        `json:"errors,omitempty"`
}

// ClusterInfo holds the OpenShift cluster server URL, version, and current user.
type ClusterInfo struct {
	Server  string `json:"server"`
	Version string `json:"version"`
	User    string `json:"user"`
	// ControlPlane is "hosted" (a hosted control plane: ROSA HCP,
	// HyperShift) or "self-managed"; absent when it could not be read, and
	// in the gitops profile, which does not read it.
	ControlPlane string `json:"controlPlane,omitempty"`
}

// SubscriptionInfo describes the OLM Subscription resource for the RHOAI operator.
type SubscriptionInfo struct {
	Name    string `json:"name"`
	Source  string `json:"source"`
	Channel string `json:"channel"`
	State   string `json:"state"`
	// ManagedBy is the Argo CD application that tracks the Subscription.
	ManagedBy string `json:"managedBy,omitempty"`
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
	// ManagedBy is the Argo CD application that tracks the CatalogSource.
	ManagedBy string `json:"managedBy,omitempty"`
	// ManagedByACM: no Argo CD application tracks the CatalogSource, and an
	// ACM policy enforces its spec (config-policy-controller owns f:spec).
	ManagedByACM bool `json:"managedByACM,omitempty"`
}

// PullSecretInfo reports whether the additional-pull-secret exists and is valid.
type PullSecretInfo struct {
	Exists bool   `json:"exists"`
	Valid  bool   `json:"valid"`            // has correct format with quay.io/rhoai entry
	Detail string `json:"detail,omitempty"` // human-readable detail if invalid
	// Nodes is set when the nodes may not pull with additional-pull-secret:
	// a self-managed cluster, or one whose control plane could not be read
	// (cluster.controlPlane absent). Absent on a hosted control plane and in
	// the gitops profile.
	Nodes *NodePullSecretInfo `json:"nodes,omitempty"`
}

// NodePullSecretInfo is the nodes' side of the quay.io/rhoai credentials on
// a cluster whose nodes pull with the cluster pull secret,
// openshift-config/pull-secret, which the updater cannot read.
type NodePullSecretInfo struct {
	// Command adds the quay.io/rhoai entry to openshift-config/pull-secret,
	// for a cluster-admin to run (it prompts for the token).
	Command string `json:"command"`
	// Pulled: the nightly CatalogSource is READY on its quay.io/rhoai image,
	// so the nodes have pulled from quay.io/rhoai.
	Pulled bool `json:"pulled"`
}

// NodePullCheckRequest is the JSON body of POST /api/setup/node-pull-check.
type NodePullCheckRequest struct {
	Image string `json:"image"`
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
	// RevertDashboardDev ends an active Dashboard Dev session before the
	// update; without it the update is refused (errorCode dashboard_dev_active).
	RevertDashboardDev bool `json:"revertDashboardDev,omitempty"`
}

// RefreshRequest is the optional JSON body for POST /api/refresh/stream.
type RefreshRequest struct {
	RevertDashboardDev bool `json:"revertDashboardDev,omitempty"`
}

// ReinstallRequest is the JSON body for the POST /api/rollback endpoint.
// If TargetType is empty or "stable", the operator is reinstalled from the stable catalog.
// If TargetType is "nightly", the operator is reinstalled with the specified FBC Image.
type ReinstallRequest struct {
	TargetType string `json:"targetType"` // "stable", "nightly", or "custom"
	Image      string `json:"image,omitempty"`
	Channel    string `json:"channel,omitempty"` // optional channel override (e.g., "stable-3.5", "beta")
	// AllowDowngrade confirms a target older than the installed operator;
	// without it such a reinstall is refused (errorCode
	// downgrade_requires_confirmation).
	AllowDowngrade bool `json:"allowDowngrade,omitempty"`
	// RevertDashboardDev ends an active Dashboard Dev session first.
	RevertDashboardDev bool `json:"revertDashboardDev,omitempty"`
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
	Tag       string `json:"tag"`
	Image     string `json:"image"`
	Digest    string `json:"digest,omitempty"`    // sha256:... of Image, when resolved
	BuildDate string `json:"buildDate,omitempty"` // build-date label of the FBC image
	Error     string `json:"error,omitempty"`
}

// NightlyBuild describes one nightly FBC catalog build.
type NightlyBuild struct {
	Image           string `json:"image"`                     // full image reference
	Tag             string `json:"tag,omitempty"`             // stream tag, e.g. "rhoai-3.6"
	Digest          string `json:"digest,omitempty"`          // "sha256:..."
	BuildDate       string `json:"buildDate,omitempty"`       // build-date label of the FBC image
	DashboardCommit string `json:"dashboardCommit,omitempty"` // odh-dashboard commit in this build, once known
	DashboardGitURL string `json:"dashboardGitURL,omitempty"`
}

// NightlyStatus compares the installed nightly catalog with the newest build
// of the same stream (tag). It is only set while the operator Subscription
// uses the nightly CatalogSource.
type NightlyStatus struct {
	Installed *NightlyBuild `json:"installed,omitempty"`
	Latest    *NightlyBuild `json:"latest,omitempty"`
	// UpdateAvailable is installed digest != latest digest of the same tag;
	// absent when either digest is unknown.
	UpdateAvailable *bool  `json:"updateAvailable,omitempty"`
	CheckedAt       string `json:"checkedAt,omitempty"` // when Latest was read from Quay (RFC3339)
	Error           string `json:"error,omitempty"`     // why Latest is missing
}

// CatalogServes describes the head of the nightly catalog's channel, read
// from its PackageManifest.
type CatalogServes struct {
	Channel       string `json:"channel"`
	CSV           string `json:"csv"`                     // currentCSV of the channel
	OperatorImage string `json:"operatorImage,omitempty"` // containerImage annotation of that CSV
	Built         string `json:"built,omitempty"`         // createdAt annotation of that CSV
	// Installed reports whether the installed operator is this build (same
	// CSV name, operator image and build time). Two nightly builds of the
	// same version have the same CSV name, and OLM does not install the
	// newer one by itself. Absent when the installed CSV does not say.
	Installed *bool `json:"installed,omitempty"`
	// Updating is true while OLM replaces the catalog's registry pod after
	// its image changed: the head above is still the previous build's, so
	// Installed is absent.
	Updating bool `json:"updating,omitempty"`
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
	// Cause is a short classified cause of a not-ready component (a missing
	// prerequisite operator, an upgrade gate, ...); Diagnostics has the fix.
	Cause          string `json:"cause,omitempty"`
	FixAction      string `json:"fixAction,omitempty"`      // fix action ID for ApplyFix
	FixTitle       string `json:"fixTitle,omitempty"`       // button text (plain English)
	FixDescription string `json:"fixDescription,omitempty"` // one-line explanation
	FixConfirm     string `json:"fixConfirm,omitempty"`     // confirmation modal body
}

// DeploymentInfo holds readiness and image metadata for a Kubernetes Deployment.
type DeploymentInfo struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Ready     int    `json:"ready"`
	Desired   int    `json:"desired"`
	Available int    `json:"available"`
	Image     string `json:"image"` // the main container's image, as the Deployment names it
	// LabelImage is the digest-pinned reference the git labels were (or
	// would be) read from: Image when it is pinned, else the digest a running
	// pod reports for a floating tag. Empty for images that carry no labels.
	LabelImage          string            `json:"labelImage,omitempty"`
	UnavailableReplicas int               `json:"unavailableReplicas"`
	UpdatedReplicas     int               `json:"updatedReplicas"`
	RolloutStuck        bool              `json:"rolloutStuck"`
	RolloutMessage      string            `json:"rolloutMessage,omitempty"`
	ChangeStatus        string            `json:"changeStatus,omitempty"` // "updated", "new", or "" (unchanged)
	MatchLabels         map[string]string `json:"-"`                      // for pod matching, not serialized
	UID                 string            `json:"-"`                      // for pod matching (adopted ReplicaSets), not serialized
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
	// DefaultsSource is "csv" (alm-examples of the installed CSV) or "github" (branch sample).
	DefaultsSource string `json:"defaultsSource,omitempty"`
	// DefaultsAPIVersion is the apiVersion of the defaults, always the one
	// the DataScienceCluster was read at: component names differ between
	// API versions, so other versions are never compared.
	DefaultsAPIVersion string `json:"defaultsAPIVersion,omitempty"`
	// ResetRemovals are the enabled components "reset-defaults" would set to
	// Removed or drop.
	ResetRemovals []string `json:"resetRemovals,omitempty"`
	// RemovalBlocks are the components a repair would remove that must not
	// be removed now. "reset-defaults" refuses while any of ResetRemovals is
	// listed, "remove-extra-components" while any enabled extra component is.
	RemovalBlocks []DSCRemovalBlock `json:"removalBlocks,omitempty"`
}

// DSCVersionFallback says that the DataScienceCluster was read at an older
// API version because the API server could not convert it to a newer served
// one (the operator's conversion webhook failed).
type DSCVersionFallback struct {
	Version string `json:"version"` // the version that failed, e.g. "v3"
	Used    string `json:"used"`    // the version it was read at, e.g. "v2"
	Message string `json:"message"` // the API server's error message
}

// DSCRemovalBlock explains why a DSC component must not be set to Removed now.
type DSCRemovalBlock struct {
	Component string   `json:"component"`
	Reasons   []string `json:"reasons"`
}

// ComponentsResponse groups DSC component statuses with deployment details.
type ComponentsResponse struct {
	DSCCompatibility *DSCCompatibility `json:"dscCompatibility,omitempty"`
	Components       []ComponentInfo   `json:"components"`
	Deployments      []DeploymentInfo  `json:"deployments"`
	DSCName          string            `json:"dscName"`
	// DSCExists is false when no DataScienceCluster exists; DSCState then
	// says why: "no-dsc" (CRD installed, no DSC yet) or "no-crd" (operator
	// not installed or its CRD not created yet). DSCState is "present" otherwise.
	DSCExists       bool   `json:"dscExists"`
	DSCState        string `json:"dscState"`
	OperatorVersion string `json:"operatorVersion,omitempty"` // installed rhods-operator CSV version
	OperatorPhase   string `json:"operatorPhase,omitempty"`   // its CSV phase, e.g. "Succeeded"
	// OperatorImage and OperatorBuilt are the installed CSV's containerImage
	// and createdAt annotations: the operator build, read from the cluster.
	OperatorImage string `json:"operatorImage,omitempty"`
	OperatorBuilt string `json:"operatorBuilt,omitempty"`
	// ImageLabelsOff: the per-image build details (commit, build date,
	// version) are not read, because the gitops profile contacts no registry.
	ImageLabelsOff bool   `json:"imageLabelsOff,omitempty"`
	DSCPhase       string `json:"dscPhase"`
	DSCReason      string `json:"dscReason,omitempty"`
	// DSCAPIVersion is the apiVersion the DSC was read at, for example
	// "datasciencecluster.opendatahub.io/v3".
	DSCAPIVersion string `json:"dscAPIVersion,omitempty"`
	// DSCVersionFallback is set when the DSC could not be read at the
	// preferred version and was read at an older one.
	DSCVersionFallback *DSCVersionFallback `json:"dscVersionFallback,omitempty"`
	SnapshotTime       string              `json:"snapshotTime,omitempty"`
	ChangedCount       int                 `json:"changedCount"`
	ConsoleURL         string              `json:"consoleURL,omitempty"`
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
	OwnerName         string            `json:"-"` // the controlling owner's name, for matching
	OwnerUID          string            `json:"-"` // the controlling owner's UID, for matching
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
	// UnmanagedPipelineProjects lists visible projects that already have a
	// pipeline server this tool did not create. Setup is refused there.
	UnmanagedPipelineProjects []string `json:"unmanagedPipelineProjects,omitempty"`
	// UnmanagedPipelineServers is the health of those pipeline servers
	// (ManagedByTool false): whether a server works does not depend on who
	// created it. The tool never changes or deletes them.
	UnmanagedPipelineServers []ResourceState `json:"unmanagedPipelineServers,omitempty"`
	// LegacyStorage is the S3 storage an earlier version of this tool left
	// in namespace minio, until Migrate storage (or teardown) removes it.
	LegacyStorage *LegacyStorageState `json:"legacyStorage,omitempty"`
	// ForeignLegacyNamespace: namespace minio exists but this tool did not
	// make it (no tool label; made by a project request, or by someone
	// else), so whatever is in it is not treated as an earlier version's
	// storage: Migrate storage never uses it, and setup is not blocked.
	ForeignLegacyNamespace bool `json:"foreignLegacyNamespace,omitempty"`
	// PipelinesBlocked and MLflowBlocked say why the feature cannot be set
	// up on this cluster (the DataScienceCluster does not turn on the
	// component that runs it), or are nil. Setup refuses for the same reason.
	PipelinesBlocked *ComponentBlock `json:"pipelinesBlocked,omitempty"`
	MLflowBlocked    *ComponentBlock `json:"mlflowBlocked,omitempty"`
	// DashboardURL is the RHOAI dashboard's address ("" when unknown), for
	// links such as its Projects page.
	DashboardURL string `json:"dashboardUrl,omitempty"`
	// InferenceGateway is the cluster's inference gateway, which models
	// route through by default (Gateway openshift-ingress/openshift-ai-inference).
	InferenceGateway InferenceGatewayState `json:"inferenceGateway"`
	// Models is what the Models feature needs, and the models it set up.
	Models ModelsStatus `json:"models"`
}

// InferenceGatewayState is the cluster's inference gateway: GatewayClass,
// options ConfigMap and Gateway openshift-ai-inference. Deployed is true
// when the Gateway exists, whoever created it; ManagedByTool when this tool
// did. UsedBy lists the LLMInferenceServices ("namespace/name") that route
// through it: it is not deleted while one does.
type InferenceGatewayState struct {
	ResourceState
	// ExternalURL is https://<hostname> of the listener for callers outside
	// the cluster ("" when it has none).
	ExternalURL string `json:"externalUrl,omitempty"`
	// PlannedURL is the address set up would give the gateway, while it
	// is not set up ("" when the cluster's apps domain cannot be read).
	PlannedURL string `json:"plannedUrl,omitempty"`
	// Address is the load balancer address the gateway reports.
	Address string `json:"address,omitempty"`
	// DNS is "published" once the external hostname's DNSRecord is
	// published in every zone, "pending" before, "" when unknown.
	DNS string `json:"dns,omitempty"`
	// AuthPolicy is the gateway's authentication, which odh-model-controller
	// adds once a model uses the gateway: "enforced", "not-enforced",
	// "missing", or "" when unknown.
	AuthPolicy string `json:"authPolicy,omitempty"`
	// AllowedProjects are the namespaces the gateway's listeners list by
	// name (allowedRoutes from Selector, kubernetes.io/metadata.name In
	// [...], RHOAI's documented values array; from Same lists the
	// gateway's own namespace). Nil when the listeners choose namespaces
	// otherwise (every namespace, or labels).
	AllowedProjects []string `json:"allowedProjects,omitempty"`
	// AllProjectsAllowed: the listeners accept routes from every namespace
	// (from All).
	AllProjectsAllowed bool `json:"allProjectsAllowed,omitempty"`
	// NotAllowedModelProjects are the namespaces of LLMInferenceServices
	// that route through the gateway (the tool's or anyone's, such as a
	// model deployed from the dashboard) that the gateway does not allow:
	// their routes do not attach. For this tool's gateway, Allow adds them.
	NotAllowedModelProjects []string `json:"notAllowedModelProjects,omitempty"`
	// NotAllowedModels are those models, "namespace/name": their routes are
	// rejected (NotAllowedByListeners), so they never become Ready.
	NotAllowedModels []string `json:"notAllowedModels,omitempty"`
}

// ModelsStatus is what setting up a model needs, and the models of the
// tool's catalog in data science projects.
type ModelsStatus struct {
	// Blocked says why no model can be set up (KServe is off, or its
	// LLMInferenceService dependencies are not met), or is nil.
	Blocked *ComponentBlock `json:"blocked,omitempty"`
	// Dependencies is the DataScienceCluster condition
	// KserveLLMInferenceServiceDependencies: "True", "False", or "" when
	// it is not reported (unknown).
	Dependencies string `json:"dependencies,omitempty"`
	// Preset is the CPU preset (LLMInferenceServiceConfig) a model uses,
	// found at run time; PresetProblem says why it was not found.
	Preset        string `json:"preset,omitempty"`
	PresetProblem string `json:"presetProblem,omitempty"`
	// ImagePlan is the image a new model would run: "preset" (the preset's
	// own), "released" (the newest released Red Hat image, on a nightly
	// cluster that cannot pull the preset's), "override" (VLLM_CPU_IMAGE),
	// or "" when unknown; ImagePlanReason says why.
	ImagePlan       string `json:"imagePlan,omitempty"`
	ImagePlanReason string `json:"imagePlanReason,omitempty"`
	// Catalog is what setup offers, in the order the page lists it.
	Catalog []CatalogModel `json:"catalog"`
	// Items are the catalog's models in data science projects: this tool's,
	// and those of the same name it did not create.
	Items []ModelState `json:"items,omitempty"`
}

// CatalogModel is a model the Models feature sets up.
type CatalogModel struct {
	// Name is the LLMInferenceService's name, and the model name callers
	// send.
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	// URI is the Hugging Face repository, pinned to a revision
	// (hf://<org>/<repo>:<commit>).
	URI string `json:"uri"`
	CPU string `json:"cpu"`
	// MemoryRequest is what the scheduler reserves; Memory is the limit.
	MemoryRequest string `json:"memoryRequest"`
	Memory        string `json:"memory"`
	// Type is "chat" or "embedding".
	Type string `json:"type"`
	// ToolCalling: a chat model served with tool calling turned on.
	ToolCalling bool `json:"toolCalling,omitempty"`
	// EmbeddingDimension is an embedding model's vector size.
	EmbeddingDimension int `json:"embeddingDimension,omitempty"`
	// UseCase is what it is for, as the dashboard shows it
	// (opendatahub.io/genai-use-case); Description is its
	// openshift.io/description.
	UseCase     string `json:"useCase"`
	Description string `json:"description"`
	// Args are the vLLM arguments the model is served with
	// (VLLM_ADDITIONAL_ARGS).
	Args string `json:"args"`
	// DownloadSize is roughly what is downloaded from Hugging Face
	// ("270 MB").
	DownloadSize string `json:"downloadSize"`
	// Default: the model a request that names none sets up.
	Default bool `json:"default,omitempty"`
	// CapacityWarning says that no worker seems to have room for this model
	// now, or is "" (room found, or unknown).
	CapacityWarning string `json:"capacityWarning,omitempty"`
}

// ModelState is one LLMInferenceService of the catalog in a project
// (Namespace, Name).
type ModelState struct {
	ResourceState
	DisplayName string `json:"displayName,omitempty"`
	// URL is the model's address (status.url), the base of its OpenAI API.
	URL string `json:"url,omitempty"`
	// ImageKind is "preset", "released" or "override"; ImageVersion is the
	// released image's version.
	ImageKind    string `json:"imageKind,omitempty"`
	ImageVersion string `json:"imageVersion,omitempty"`
	// HardwareProfile is the dashboard hardware profile the model is
	// recorded under ("<namespace>/<name>"), or "" when none fits it.
	HardwareProfile string `json:"hardwareProfile,omitempty"`
	// PresetImageMissing: the preset's image cannot be pulled because the
	// registry does not have it (a nightly build's image is not published
	// yet); setting the model up again with the newest released image
	// fixes it.
	PresetImageMissing bool `json:"presetImageMissing,omitempty"`
	// MaaSPublished: the model routes through the MaaS gateway (published
	// on the MaaS tab), so it is used through MaaS and its direct address
	// answers 404.
	MaaSPublished bool `json:"maasPublished,omitempty"`
	// TokenSecret is the model's token Secret, which this tool made with
	// ServiceAccount TokenServiceAccount as the dashboard's "Require token
	// authentication" does; "" when it has none (set up by an earlier
	// build: setting it up again makes it).
	TokenSecret         string `json:"tokenSecret,omitempty"`
	TokenServiceAccount string `json:"tokenServiceAccount,omitempty"`
	// TokenAuthOff: the model carries security.opendatahub.io/enable-auth
	// "false" (someone turned token authentication off in the dashboard).
	TokenAuthOff bool `json:"tokenAuthOff,omitempty"`
}

// ModelsRequest is the body of POST /api/resources/models/stream.
type ModelsRequest struct {
	// Action is "setup", "teardown", or "allow": add Project to the
	// projects this tool's inference gateway allows (no model).
	Action string `json:"action"`
	// Gateway: set up (or delete) the inference gateway. Setup does it
	// before the model, teardown after.
	Gateway bool `json:"gateway,omitempty"`
	// Project is the model's project ("" when the request has no model),
	// or the project Allow adds.
	Project string `json:"project,omitempty"`
	// Model is the catalog name; "" means the catalog's default
	// (qwen25-05b-instruct). Setup and teardown act on that model only.
	Model string `json:"model,omitempty"`
	// ReleasedImage (setup only): run the newest released Red Hat vLLM CPU
	// image instead of the preset's.
	ReleasedImage bool `json:"releasedImage,omitempty"`
}

// PlaygroundStatus is what the Gen AI playground feature needs and the
// playgrounds in data science projects (GET /api/resources/playground).
// The cluster parts are read with the app's ServiceAccount; everything about
// a project's playground and models comes from the RHOAI dashboard's own
// gen-ai API.
type PlaygroundStatus struct {
	// Blocked says why no playground can be set up (the DataScienceCluster
	// does not run the playground's operator), or is nil.
	Blocked *ComponentBlock `json:"blocked,omitempty"`
	// OperatorReady is the DataScienceCluster's readiness condition of that
	// component: "True", "False", or "" when not reported (unknown).
	OperatorReady string `json:"operatorReady,omitempty"`
	// OperatorMessage is that condition's message.
	OperatorMessage string `json:"operatorMessage,omitempty"`
	// GenAIUI is the dashboard's Gen AI studio backend (Deployment
	// gen-ai-ui): "available", "unavailable", "missing", or "" (unknown).
	GenAIUI string `json:"genAiUi,omitempty"`
	// APIProblem says why the dashboard's gen-ai API cannot be used now (no
	// dashboard address, the API answered something unexpected, it refused
	// the request), or is "".
	APIProblem string `json:"apiProblem,omitempty"`
	// APICode is the problem's kind: "no_token", "no_dashboard",
	// "api_missing", "forbidden", "unreachable", or "".
	APICode string `json:"apiCode,omitempty"`
	// Items are the playgrounds in the data science projects, and the
	// projects where this tool requested one.
	Items []PlaygroundState `json:"items,omitempty"`
	// Project is the project Models are listed for ("" for none).
	Project string `json:"project,omitempty"`
	// Models are the project's models the playground can use, as the
	// dashboard lists them (its own models, then Models as a Service).
	Models []PlaygroundModel `json:"models,omitempty"`
	// ModelsProblem says why the project's models could not be listed.
	ModelsProblem string `json:"modelsProblem,omitempty"`
	// Truncated is true when the cluster has more data science projects than
	// the status reads; the playgrounds of the others are not shown.
	Truncated bool `json:"truncated,omitempty"`
}

// PlaygroundState is the playground in a project (Namespace), as the
// dashboard reports it. ManagedByTool is true when this tool requested it.
type PlaygroundState struct {
	ResourceState
	// Phase is the phase the dashboard reports ("Ready", "Initializing",
	// "Failed", ...), or "" when it reports none.
	Phase string `json:"phase,omitempty"`
	// Requested is true when this tool's record says it requested a
	// playground in the project (also when none exists any more).
	Requested bool `json:"requested,omitempty"`
}

// PlaygroundModel is a model the dashboard offers the playground.
type PlaygroundModel struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	// Source is "namespace" (a model deployed in the project) or "maas"
	// (Models as a Service).
	Source string `json:"source"`
	// Status is the dashboard's word for a project model ("Running", ...);
	// MaaS models report only ready or not.
	Status string `json:"status,omitempty"`
	Ready  bool   `json:"ready"`
	// Chat is false for an embedding model, which the playground cannot
	// chat with.
	Chat bool `json:"chat"`
	// EmbeddingDimension is set for an embedding model of this tool's
	// catalog in the project: Set up adds it to the playground as an
	// embedding model of that dimension.
	EmbeddingDimension int `json:"embeddingDimension,omitempty"`
}

// PlaygroundModelRef is a model a playground setup uses.
type PlaygroundModelRef struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

// PlaygroundRequest is the body of POST /api/resources/playground/stream.
type PlaygroundRequest struct {
	// Action is "setup" or "teardown".
	Action  string `json:"action"`
	Project string `json:"project"`
	// Models (setup only) are the models to use; none means every ready
	// chat model deployed in the project, and this tool's ready embedding
	// models there.
	Models []PlaygroundModelRef `json:"models,omitempty"`
}

// MaaSStatus is the Models as a Service feature (GET /api/resources/maas):
// the cluster's MaaS platform, part by part, and this tool's models with
// whether they are published to MaaS.
type MaaSStatus struct {
	// Blocked says why the platform cannot be set up here (no
	// DataScienceCluster, or its aigateway component is off), or is nil.
	Blocked *ComponentBlock `json:"blocked,omitempty"`
	// Platform is the platform as a whole: Ready when every part is ready,
	// Deployed when MaaS runs (modelsAsAService Managed), ManagedByTool when
	// this tool set up any part.
	Platform ResourceState `json:"platform"`
	// Parts are the documented prerequisites and pieces, in setup order.
	Parts []MaaSPart `json:"parts"`
	// URL is https://maas.<apps domain>, the MaaS gateway's address ("" when
	// the apps domain cannot be read).
	URL string `json:"url,omitempty"`
	// Subscription is the MaaSSubscription a published model of this tool
	// is offered under (the API key is minted for it).
	Subscription string `json:"subscription"`
	// DashboardURL is the RHOAI dashboard's address, for its MaaS pages.
	DashboardURL string `json:"dashboardUrl,omitempty"`
	// SetupPlan lists, in order, every change Set up would make now
	// (operators installed, objects created, DataScienceCluster fields
	// changed); empty when it has nothing to do.
	SetupPlan []string `json:"setupPlan,omitempty"`
	// Items are this tool's models in data science projects, published or
	// not.
	Items []MaaSModel `json:"items,omitempty"`
}

// MaaSAction is a change a platform part offers on its own (POST
// /api/resources/maas/stream with Action ID), shown in a confirmation
// that lists Changes first.
type MaaSAction struct {
	// ID is "upgrade-rhcl", "rhcl-memory", "authorino-tls" or
	// "remove-tier-policies".
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Summary string   `json:"summary"`
	Changes []string `json:"changes"`
	Danger  bool     `json:"danger,omitempty"`
}

// MaaSPart is one part of the MaaS platform.
type MaaSPart struct {
	// ID is "rhcl", "kuadrant", "cnpg", "database", "gateway",
	// "authorino-tls", "tier-policies", "dsc", "maas-api" or "tenant", in
	// setup order.
	ID    string `json:"id"`
	Title string `json:"title"`
	// State is "ready", "missing" (setup creates it), "waiting", "problem"
	// (Action or Commands say how to fix it), "manual" (a cluster-admin
	// does it; Commands says how) or "unknown".
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
	// ManagedByTool: this tool created (or set) it; Setup and Remove
	// change it. NotManaged: it exists but someone else made it; it is used
	// as it is and never changed or deleted.
	ManagedByTool bool `json:"managedByTool,omitempty"`
	NotManaged    bool `json:"notManaged,omitempty"`
	// Warning is a problem that does not block setup.
	Warning string `json:"warning,omitempty"`
	// Commands are the documented commands a cluster-admin runs instead of
	// Set up or Action (or, for a "manual" part, the only way).
	Commands string `json:"commands,omitempty"`
	// Actions are fixes the part offers on its own.
	Actions []MaaSAction `json:"actions,omitempty"`
	// DocURL is the MaaS documentation page for the part.
	DocURL string `json:"docUrl,omitempty"`
}

// MaaSModel is one of this tool's models (Namespace, Name) and its MaaS
// publication.
type MaaSModel struct {
	ResourceState
	DisplayName string `json:"displayName,omitempty"`
	// ModelReady: the model itself (LLMInferenceService) is ready.
	ModelReady bool `json:"modelReady"`
	// Published: this tool published it (its MaaSModelRef exists).
	Published bool `json:"published"`
	// Partly: what is left of a publication undone half-way (Message says
	// what); Unpublish finishes it. Nothing about it settles on its own.
	Partly bool `json:"partly,omitempty"`
	// Phase is the MaaSModelRef's phase ("Ready", ...).
	Phase string `json:"phase,omitempty"`
	// Endpoint is the MaaSModelRef's status.endpoint.
	Endpoint string `json:"endpoint,omitempty"`
	// Access and Subscription are the phases of the tool's MaaSAuthPolicy
	// and MaaSSubscription ("Active", ...).
	Access       string `json:"access,omitempty"`
	Subscription string `json:"subscription,omitempty"`
}

// MaaSRequest is the body of POST /api/resources/maas/stream.
type MaaSRequest struct {
	// Action is "setup" or "teardown" (the platform), "publish" or
	// "unpublish" (the tool's model Model in Project), or a part's own fix:
	// "upgrade-rhcl", "rhcl-memory", "authorino-tls" or
	// "remove-tier-policies".
	Action  string `json:"action"`
	Project string `json:"project,omitempty"`
	// Model is the catalog name of a chat model (publish and unpublish
	// only); "" means the catalog's default (qwen25-05b-instruct).
	Model string `json:"model,omitempty"`
}

// ComponentBlock is a DataScienceCluster component a feature needs that is
// not turned on.
type ComponentBlock struct {
	// Component is its key in the DataScienceCluster's spec.components
	// ("aipipelines", "mlflowoperator").
	Component string `json:"component"`
	// DSC is the DataScienceCluster's name; "" when there is none.
	DSC string `json:"dsc,omitempty"`
	// State is its managementState as read ("Removed"); "" when there is
	// no DataScienceCluster or no state.
	State string `json:"state,omitempty"`
	// Reason says it in plain words, with who can fix it.
	Reason string `json:"reason"`
}

// CleanupRequest is the body of POST /api/resources/cleanup/stream: what
// to delete of what this tool created, in this order.
type CleanupRequest struct {
	// PipelineServers are the projects whose pipeline server goes.
	PipelineServers []string `json:"pipelineServers,omitempty"`
	// Storage deletes the S3 storage (and storage an earlier version left).
	Storage bool `json:"storage,omitempty"`
	// DeleteNamespace also deletes namespace seaweedfs (needs Storage).
	DeleteNamespace bool `json:"deleteNamespace,omitempty"`
	// MLflow deletes the MLflow server.
	MLflow bool `json:"mlflow,omitempty"`
	// Models are the projects whose models of the tool's catalog go (all
	// of them);
	// InferenceGateway deletes the inference gateway after them.
	Models           []string `json:"models,omitempty"`
	InferenceGateway bool     `json:"inferenceGateway,omitempty"`
	// Playgrounds are the projects whose playground (requested by this
	// tool) goes, before the models it uses.
	Playgrounds []string `json:"playgrounds,omitempty"`
	// MaaSPublished are the projects whose models this tool unpublishes from
	// Models as a Service first, before anything else goes; MaaSPlatform
	// removes the MaaS platform this tool set up, last.
	MaaSPublished []string `json:"maasPublished,omitempty"`
	MaaSPlatform  bool     `json:"maasPlatform,omitempty"`
}

// S3TeardownRequest is the body of POST /api/resources/minio/teardown.
type S3TeardownRequest struct {
	// DeleteNamespace also deletes namespace seaweedfs, with everything in
	// it, once the storage's objects are gone (only when this tool created
	// it: it carries the tool label and a tool manager wrote it).
	DeleteNamespace bool `json:"deleteNamespace,omitempty"`
}

// MigrateStorageRequest is the body of POST /api/resources/minio/migrate/stream.
type MigrateStorageRequest struct {
	// RemoveOldStorage runs the last part: removing the old storage once the
	// migration is done. Without it Migrate storage sets up the new storage,
	// copies the objects and moves the pipeline servers.
	RemoveOldStorage bool `json:"removeOldStorage,omitempty"`
	// DeleteNamespace also deletes namespace minio (with RemoveOldStorage,
	// and only when this tool created it).
	DeleteNamespace bool `json:"deleteNamespace,omitempty"`
}

// LegacyStorageState describes the S3 storage an earlier version of this
// tool set up in namespace minio: MinIO, or SeaweedFS behind the MinIO-era
// names. Only objects this tool created there are listed.
type LegacyStorageState struct {
	Namespace string `json:"namespace"`
	// Server is the server that runs there, "SeaweedFS" or "MinIO", or ""
	// when neither Deployment is left.
	Server string `json:"server,omitempty"`
	// Ready is true when the server has a ready replica.
	Ready bool `json:"ready"`
	// Endpoint is the in-cluster S3 address its clients use.
	Endpoint string `json:"endpoint"`
	// Objects are the objects this tool created there ("Deployment
	// seaweedfs", ...), which migration and teardown delete.
	Objects []string `json:"objects"`
	// DataPVCs are its data volumes among them, with their sizes.
	DataPVCs []KeptPVC `json:"dataPVCs,omitempty"`
	// UncopiedPVCs are data volumes no running server serves, so their
	// objects cannot be copied (MinIO's volume kept after SeaweedFS
	// replaced MinIO).
	UncopiedPVCs []string `json:"uncopiedPVCs,omitempty"`
	// UsedBy are the pipeline servers (namespace/name) whose object storage
	// still points at it.
	UsedBy []string `json:"usedBy,omitempty"`
	// Terminating is true while namespace minio is being deleted.
	Terminating bool `json:"terminating,omitempty"`
	// Stage is the next part of Migrate storage: "migrate" (set up the new
	// storage, copy the objects, move the pipeline servers) or "remove"
	// (the new storage is ready and no pipeline server of this tool uses
	// the old one: delete it).
	Stage string `json:"stage"`
	// Repoint are the pipeline servers (namespace/name) this tool created
	// that use the old storage: Migrate storage moves them.
	Repoint []string `json:"repoint,omitempty"`
	// NotMoved are the other pipeline servers on the old storage, each with
	// why the tool leaves it ("ns/name (not created by this tool)").
	NotMoved []string `json:"notMoved,omitempty"`
	// RemoveBlockedReason explains why the old storage cannot be removed
	// yet (a pipeline server still uses it).
	RemoveBlockedReason string `json:"removeBlockedReason,omitempty"`
	// NamespaceDeletable is true when namespace minio carries this tool's
	// label and the tool's managers wrote it: removing the old storage then
	// offers to delete it, as the user's informed choice.
	NamespaceDeletable bool `json:"namespaceDeletable"`
	// OtherObjects are the Deployments and PVCs in the namespace this tool
	// did not create, which deleting the namespace would delete too (the
	// tool cannot list every kind).
	OtherObjects []string `json:"otherObjects,omitempty"`
	// NamespaceKeptReason says why the namespace is not offered for
	// deletion.
	NamespaceKeptReason string `json:"namespaceKeptReason,omitempty"`
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
	// Name is the object name when it is not fixed (pipeline server DSPA name).
	Name string `json:"name,omitempty"`
	// ManagedByTool is true when this tool created the resource. Teardown is
	// offered only for managed resources.
	ManagedByTool bool `json:"managedByTool"`
	// Terminating is true while the resource is being deleted.
	Terminating bool `json:"terminating,omitempty"`
	// SetupBlockedReason explains why setup cannot run (for example a
	// namespace that exists but was not created by this tool).
	SetupBlockedReason string `json:"setupBlockedReason,omitempty"`
	// TeardownBlockedReason explains why teardown is unavailable.
	TeardownBlockedReason string `json:"teardownBlockedReason,omitempty"`
	// UsedBy lists the pipeline servers ("namespace/name") that store their
	// artifacts in the S3 storage, whoever created them: the storage cannot
	// be deleted while one is there.
	UsedBy []string `json:"usedBy,omitempty"`
	// WaitingReason is the pod-level reason the resource is not ready, such
	// as ImagePullBackOff, CrashLoopBackOff or Unschedulable.
	WaitingReason string `json:"waitingReason,omitempty"`
	// TerminalError is true when the resource will not become ready without
	// user action. Clients should stop fast polling.
	TerminalError bool `json:"terminalError,omitempty"`
	// DataPVCs lists the PersistentVolumeClaims (and so the data) that
	// teardown deletes.
	DataPVCs []string `json:"dataPVCs,omitempty"`
	// PROverride is true when an MLflow PR image deployed by this tool (or an
	// odh-pr-N image) is active, independent of readiness.
	PROverride bool `json:"prOverride,omitempty"`
	// PRNumber is the MLflow PR number of the active override, when known.
	PRNumber int `json:"prNumber,omitempty"`
	// RevertImage is the image Revert restores. Empty with PROverride set
	// means the operator default (spec.image.image removed).
	RevertImage string `json:"revertImage,omitempty"`
	// NamespaceDeletable: Tear down may also delete the resource's
	// namespace, which this tool created (the S3 storage's seaweedfs).
	NamespaceDeletable bool `json:"namespaceDeletable,omitempty"`
	// NamespaceKeptReason says why the namespace is not offered for deletion.
	NamespaceKeptReason string `json:"namespaceKeptReason,omitempty"`
	// OtherObjects are the Deployments and PVCs in that namespace this tool
	// did not create (it cannot list every kind).
	OtherObjects []string `json:"otherObjects,omitempty"`
	// Warning is a security or upgrade notice that needs the user's action
	// but does not stop the resource from working (for example the S3
	// storage running another image than this version deploys).
	Warning string `json:"warning,omitempty"`
	// UIUser is the login name of the resource's web UI, when it has a
	// fixed one (the SeaweedFS admin UI).
	UIUser string `json:"uiUser,omitempty"`
	// MigrationPending is true on the S3 storage while storage from an
	// earlier version of this tool is left in namespace minio (see
	// ResourcesStatus.LegacyStorage): Migrate storage moves its data and
	// pipeline servers, and then removes it.
	MigrationPending bool `json:"migrationPending,omitempty"`
	// RepairNeeded explains what re-running setup (Repair) would fix: the
	// server does not serve through its Service or Route, is scaled to
	// zero, or a migration left cleanup undone. With Ready false the
	// resource is incomplete; with Ready true it works but is degraded.
	RepairNeeded string `json:"repairNeeded,omitempty"`
	// HealthUnknown is true when the resource exists but reports no
	// readiness this tool can read (no condition, or one it cannot
	// interpret): its health is unknown, which is not a failure.
	HealthUnknown bool `json:"healthUnknown,omitempty"`
	// NonStandardName is true on a pipeline server this tool set up under
	// its earlier name (nightly-dspa): workbench pipelines (Elyra) need the
	// DSPA named dspa, so the Pipelines tab offers to recreate it.
	NonStandardName bool `json:"nonStandardName,omitempty"`
	// Connection is the dashboard S3 Connection (a Secret in the project)
	// the tool created with this pipeline server, or "".
	Connection string `json:"connection,omitempty"`
	// ConnectionState is what the Secret itself says, read only for the
	// selected project (GET /api/resources/status?project=): "present"
	// (the tool's), "missing" (none; deleted, for example in the
	// dashboard), "foreign" (a Secret of that name someone else made) or
	// "unreadable" (ConnectionMessage says why). "" means not read.
	ConnectionState   string `json:"connectionState,omitempty"`
	ConnectionMessage string `json:"connectionMessage,omitempty"`
}

// KeptPVC is a PersistentVolumeClaim and its size.
type KeptPVC struct {
	Name string `json:"name"`
	Size string `json:"size,omitempty"`
}

// PipelineServerRequest is the JSON body for pipeline server setup/teardown.
type PipelineServerRequest struct {
	Project string `json:"project"`
	// Recreate (setup only) replaces the tool's pipeline server named
	// nightly-dspa with one named dspa.
	Recreate bool `json:"recreate,omitempty"`
}

// DiagnosticsFixRequest is the JSON body for POST /api/diagnostics/fix.
type DiagnosticsFixRequest struct {
	ProblemID string `json:"problemId"`
}

// PRContainsResponse answers GET /api/build-explorer/contains: which nightly
// builds contain a merged pull request (?pr=N) or a commit (?commit=SHA).
// A PR search fills PR, Title, URL, MergeCommit and MergedAt; a commit search
// fills Commit, CommitDate, CommitTitle and CommitURL.
type PRContainsResponse struct {
	Repo        string `json:"repo"`         // repository searched, e.g. "opendatahub-io/odh-dashboard"
	PR          int    `json:"pr,omitempty"` // the PR number (PR search)
	Title       string `json:"title,omitempty"`
	URL         string `json:"url,omitempty"`         // the PR page
	MergeCommit string `json:"mergeCommit,omitempty"` // merge_commit_sha of the merged PR
	MergedAt    string `json:"mergedAt,omitempty"`
	// Commit search: the full SHA GitHub resolved, its committer date, the
	// first line of its message and its page.
	Commit      string            `json:"commit,omitempty"`
	CommitDate  string            `json:"commitDate,omitempty"`
	CommitTitle string            `json:"commitTitle,omitempty"`
	CommitURL   string            `json:"commitURL,omitempty"`
	Builds      []PRContainsBuild `json:"builds"`
	// Stable describes upstream opendatahub-io/odh-dashboard's stable branch,
	// the step between main and the nightlies, when a search for that
	// repository found the PR or commit in no build: whether stable has it
	// yet and when stable was last updated. Absent for other repositories,
	// when a build contains it, and when GitHub could not be asked.
	Stable *StableBranch `json:"stable,omitempty"`
	// RateLimited is set when GitHub's rate limit stopped some lookups; their
	// builds are "unknown". RetryAfterSeconds says when to try again.
	RateLimited       bool `json:"rateLimited,omitempty"`
	RetryAfterSeconds int  `json:"retryAfterSeconds,omitempty"`
}

// StableBranch is the state of a repository's stable branch for a search.
type StableBranch struct {
	Branch    string `json:"branch"`              // "stable"
	Head      string `json:"head"`                // its commit, 40 hex
	UpdatedAt string `json:"updatedAt,omitempty"` // committer date of that commit, RFC 3339
	Contains  bool   `json:"contains"`            // stable has the PR's merge commit, or the commit
}

// PRContainsBuild is the answer for one nightly build.
type PRContainsBuild struct {
	Image     string `json:"image"` // FBC catalog image (digest-pinned)
	Tag       string `json:"tag,omitempty"`
	Installed bool   `json:"installed,omitempty"` // the build the nightly CatalogSource uses
	// Commit and CommitRepo come from the component image's vcs-ref and
	// git.url labels, e.g. "red-hat-data-services/odh-dashboard".
	Commit     string `json:"commit,omitempty"`
	CommitRepo string `json:"commitRepo,omitempty"`
	// Result is "contains", "not_contained" or "unknown".
	Result string `json:"result"`
	// Reason explains the result. For "not_contained": "built_before_merge"
	// or "built_before_commit" (the build is older than what it is searched
	// for, so nothing was read about it). For "unknown": "rate_limited",
	// "commit_not_found" (GitHub does not know the merge commit or commit),
	// "no_component_image", "no_commit_label", "unexpected_repo",
	// "build_unreadable" or "github_error".
	Reason     string `json:"reason,omitempty"`
	Message    string `json:"message,omitempty"`
	CompareURL string `json:"compareURL,omitempty"` // GitHub compare page: merge commit (or commit)...build commit
}

// DashboardBuildsResponse answers GET /api/dashboard/builds: the commit of
// every PR or main build a dashboard container runs, its distance from the
// branch it tracks, and whether a newer build exists under the same tag.
type DashboardBuildsResponse struct {
	Builds []DashboardContainerBuild `json:"builds"`
	// GitHub is rate limited: divergence fields may be unknown until RetryAfterSeconds.
	RateLimited       bool `json:"rateLimited,omitempty"`
	RetryAfterSeconds int  `json:"retryAfterSeconds,omitempty"`
}

// DashboardContainerBuild describes the PR or main build one container runs.
type DashboardContainerBuild struct {
	Deployment string `json:"deployment"`
	Container  string `json:"container"`
	Image      string `json:"image"`   // the running image, as in DashboardDevImage.CurrentImage
	Running    string `json:"running"` // "pr" or "main"
	PR         int    `json:"pr,omitempty"`
	Flavor     string `json:"flavor,omitempty"` // "rhoai" or "odh"
	// From the running image's labels.
	Commit     string `json:"commit,omitempty"`     // 40 hex
	Repo       string `json:"repo,omitempty"`       // "opendatahub-io/odh-dashboard" (owner/name from the git URL label)
	CommitURL  string `json:"commitURL,omitempty"`  // https://github.com/<repo>/commit/<commit>
	BuildDate  string `json:"buildDate,omitempty"`  // build-date label, RFC 3339
	CommitDate string `json:"commitDate,omitempty"` // GitHub committer date (cached forever per SHA; omitted when unknown)
	// Divergence from the branch the build tracks: "main" builds against the
	// repo's main branch, "pr" builds against the PR's head commit.
	Divergence *DashboardBuildDivergence `json:"divergence,omitempty"`
	// Newer is set when the floating tag this build came from now points to
	// another digest: the build Deploy would pick up now.
	Newer *DashboardNewerBuild `json:"newer,omitempty"`
	// Reason explains missing fields: "no_commit_label", "registry_error", "unknown_repo".
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// DashboardBuildDivergence is how a build's commit relates to its base.
type DashboardBuildDivergence struct {
	Base     string `json:"base"`               // "main" or "pr-<N>"
	BaseHead string `json:"baseHead,omitempty"` // SHA of main / of the PR head when checked
	// Status describes the build against the base: "identical", "behind"
	// (the base has commits the build lacks), "ahead" (the build has commits
	// the base lacks), "diverged" or "unknown".
	Status     string `json:"status"`
	Behind     int    `json:"behind"`               // commits on the base the build lacks
	Ahead      int    `json:"ahead"`                // commits in the build the base lacks
	CompareURL string `json:"compareURL,omitempty"` // https://github.com/<repo>/compare/<commit>...<baseHead or base>
	PRState    string `json:"prState,omitempty"`    // PR builds: "open", "merged", "closed"
	PRTitle    string `json:"prTitle,omitempty"`
	CheckedAt  string `json:"checkedAt,omitempty"` // RFC 3339, when GitHub was asked (cache time)
	// Reason for "unknown": "rate_limited", "commit_not_found", "github_error", "pr_not_found".
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// DashboardNewerBuild is the build the floating tag points to now.
type DashboardNewerBuild struct {
	Image     string `json:"image"` // digest-pinned, as Deploy would write it
	Tag       string `json:"tag"`   // the floating tag checked (odh-stable, main, odh-pr-N, pr-N)
	Commit    string `json:"commit,omitempty"`
	CommitURL string `json:"commitURL,omitempty"`
	BuildDate string `json:"buildDate,omitempty"`
}

// Sample resources (Environment page): MLflow samples and a sample pipeline
// run, per project. See pkg/cluster/samples*.go.

// SampleStatus is one MLflow sample in a project.
type SampleStatus struct {
	// ID is "genai-demo", "training", "mcp-server" or "skill".
	ID   string `json:"id"`
	Name string `json:"name"`
	// State is "absent", "present", "partial" (Missing names what is
	// missing), "removed" (soft-deleted; Add restores it), "foreign" (an
	// object of the sample's name the tool did not create; left alone),
	// "unsupported" (this MLflow has no such API) or "unknown" (Message
	// says why).
	State   string   `json:"state"`
	Message string   `json:"message,omitempty"`
	Missing []string `json:"missing,omitempty"`
	// Link opens the sample in the MLflow UI, when there is one.
	Link string `json:"link,omitempty"`
	// Actions the state allows: "add", "remove", "regenerate".
	Actions []string `json:"actions"`
	// Repair, when set, is a change to the cluster that Add and Regenerate
	// make first instead of the sample (the demo on an MLflow CR with the
	// 2Gi limit an earlier version set); the confirmation names it.
	Repair string `json:"repair,omitempty"`
}

// MLflowSamplesStatus is GET /api/resources/samples/mlflow?project=.
type MLflowSamplesStatus struct {
	Project string `json:"project"`
	// Available is false while MLflow is not deployed or not ready, or has
	// no URL; Reason says which, and Samples is empty.
	Available bool           `json:"available"`
	Reason    string         `json:"reason,omitempty"`
	MLflowURL string         `json:"mlflowURL,omitempty"`
	Samples   []SampleStatus `json:"samples"`
}

// PipelineSampleSpec is GET /api/resources/samples/pipeline/spec: the
// sample pipeline, for the user to upload and run with their own token.
type PipelineSampleSpec struct {
	// Name is the pipeline's name; FileName the name to save YAML as.
	Name        string `json:"name"`
	FileName    string `json:"fileName"`
	Description string `json:"description"`
	// Image is the image both steps run (SAMPLE_PIPELINE_IMAGE overrides
	// the default).
	Image string `json:"image"`
	// YAML is the compiled pipeline (KFP IR).
	YAML string `json:"yaml"`
}

// MLflowSamplesRequest is the body of POST /api/resources/samples/mlflow/stream.
type MLflowSamplesRequest struct {
	Project string `json:"project"`
	// Action is "add", "remove" or "regenerate" (genai-demo only).
	Action string `json:"action"`
	// Samples are SampleStatus IDs, in any order; they run in the order
	// genai-demo, training, mcp-server, skill (remove: the reverse).
	Samples []string `json:"samples"`
}
