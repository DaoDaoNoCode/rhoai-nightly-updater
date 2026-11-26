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
	// Scope is what the action acted on: the cluster, or one project (a
	// project action). ActorRole is the role it ran with: "admin" (a
	// cluster-admin) or "user" (a signed-in user who is not one, through
	// the project actions' gate). Both are stored with every new entry;
	// entries written before they existed have neither and are read as
	// a cluster-admin's cluster action (pkg/cluster/internal/record).
	Scope     *ActivityScope `json:"scope,omitempty"`
	ActorRole string         `json:"actorRole,omitempty"`

	// Filled in when the log is read (not stored):
	// Label is a human-readable name for Action, Category groups actions
	// ("operator", "dashboard", "resources", "setup", "diagnostics",
	// "other"), and Build is the catalog build an operator action targeted,
	// as "<tag> · <first 12 digest hex>" (either part may be missing).
	Label    string `json:"label,omitempty"`
	Category string `json:"category,omitempty"`
	Build    string `json:"build,omitempty"`
}

// ActivityScope is what an activity entry's action acted on: Kind
// "cluster", or "project" with the project's name and namespace UID.
type ActivityScope struct {
	Kind       string `json:"kind"`
	Project    string `json:"project,omitempty"`
	ProjectUID string `json:"projectUID,omitempty"`
}

// The kinds of ActivityScope.
const (
	ActivityScopeCluster = "cluster"
	ActivityScopeProject = "project"
)

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
	// Project and ProjectUID name the project of a project lock (the
	// namespace and its UID); the cluster lock has neither, so its format
	// stays the one earlier versions read. Begun: the project action began
	// changing the project, so a lock that expires with it set reports the
	// action as interrupted.
	Project    string `json:"project,omitempty"`
	ProjectUID string `json:"projectUid,omitempty"`
	Begun      bool   `json:"begun,omitempty"`
	// CancelNote is set on a project lock whose action can be cancelled
	// (it checks a cancel request at safe points): what a cancel leaves.
	// Another updater pod takes a cancel request for it on that basis.
	CancelNote string `json:"cancelNote,omitempty"`
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
	// DSCManagedBy is the Argo CD application that manages the
	// DataScienceCluster (ArgoCDTracking: managed or unconfirmed).
	DSCManagedBy string `json:"dscManagedBy,omitempty"`
	// DSCArgoCD is what Argo CD said about the DataScienceCluster; absent
	// when there is nothing to report (pkg/cluster/argocd.go).
	DSCArgoCD *ArgoCDTracking `json:"dscArgoCD,omitempty"`
	Nightly   *NightlyStatus  `json:"nightly,omitempty"`
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
	// HyperShift), "managed" (on the cluster, run by Red Hat: ROSA Classic,
	// OpenShift Dedicated) or "self-managed"; absent when it could not be
	// read, and in the gitops profile, which does not read it.
	ControlPlane string `json:"controlPlane,omitempty"`
}

// SubscriptionInfo describes the OLM Subscription resource for the RHOAI operator.
type SubscriptionInfo struct {
	Name string `json:"name"`
	// Namespace is where the tool installs and reads RHOAI's Subscription
	// (SubNS); the operator runs there.
	Namespace string `json:"namespace,omitempty"`
	Source    string `json:"source"`
	Channel   string `json:"channel"`
	State     string `json:"state"`
	// ManagedBy is the Argo CD application that manages the Subscription
	// (ArgoCDTracking: managed or unconfirmed).
	ManagedBy string `json:"managedBy,omitempty"`
	// ArgoCD is what Argo CD said about the Subscription; absent when there
	// is nothing to report (pkg/cluster/argocd.go).
	ArgoCD *ArgoCDTracking `json:"argoCD,omitempty"`
}

// Argo CD tracking states (ArgoCDTracking.State).
const (
	// ArgoCDManaged: an Application lists the object among its resources.
	ArgoCDManaged = "managed"
	// ArgoCDStale: the object carries Argo CD's tracking, but no Argo CD
	// manages it (StaleReason says how the tool knows); it is treated as not
	// managed.
	ArgoCDStale = "stale"
	// ArgoCDUnconfirmed: Argo CD could not be asked, or did not answer
	// clearly (Reason); the object is treated as managed.
	ArgoCDUnconfirmed = "unconfirmed"
)

// Why Argo CD's tracking on an object is left over
// (ArgoCDTracking.StaleReason).
const (
	ArgoCDNotInstalled      = "argo-cd-not-installed"
	ArgoCDApplicationGone   = "application-missing"
	ArgoCDNotAmongResources = "not-in-resources"
)

// ArgoCDTracking is what the tool learnt from Argo CD about an object: whether
// an Application lists it (pkg/cluster/argocd.go).
type ArgoCDTracking struct {
	// Application is the application as the object's tracking annotation
	// or instance label names it ("<name>", or "<namespace>_<name>" for an
	// application outside Argo CD's own namespace), the name of the
	// Application that lists an object without them, or "(unknown)" when
	// Argo CD could not be asked about such an object.
	Application string `json:"application"`
	State       string `json:"state"`
	// Namespace is the namespace of the Application the tool found.
	Namespace   string `json:"namespace,omitempty"`
	StaleReason string `json:"staleReason,omitempty"`
	// Reason says why Argo CD could not confirm (unconfirmed).
	Reason string `json:"reason,omitempty"`
	// SyncStatus is the Application's status.sync.status (managed).
	SyncStatus string `json:"syncStatus,omitempty"`
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
	// ManagedBy is the Argo CD application that manages the CatalogSource
	// (ArgoCDTracking: managed or unconfirmed).
	ManagedBy string `json:"managedBy,omitempty"`
	// ManagedByACM: no Argo CD application manages the CatalogSource, and
	// an ACM policy enforces its spec (config-policy-controller owns
	// f:spec).
	ManagedByACM bool `json:"managedByACM,omitempty"`
	// ArgoCD is what Argo CD said about the CatalogSource; absent when there
	// is nothing to report (pkg/cluster/argocd.go).
	ArgoCD *ArgoCDTracking `json:"argoCD,omitempty"`
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
	// Registry is the internal registry's copy of the quay.io/rhoai
	// credential, which RHOAI's workbench images are pulled with on a
	// hosted control plane. Set only there, when the pull secret exists
	// and the request carries the signed-in cluster-admin's token (the
	// ServiceAccount may not read it).
	Registry *RegistryPullCopyInfo `json:"registry,omitempty"`
}

// RegistryPullCopyInfo is the state of Secret Name in RHOAI's applications
// namespace, the internal registry's copy of the quay.io/rhoai credential.
// It never holds the credential.
type RegistryPullCopyInfo struct {
	Name string `json:"name"`
	// Namespace is RHOAI's applications namespace; empty when it is not
	// known.
	Namespace string `json:"namespace,omitempty"`
	// NoQuayRHOAIEntry: the pull secret has no entry exactly for
	// quay.io/rhoai (only a generic or repository login, or none), so
	// nothing is copied and no copy counts as current.
	NoQuayRHOAIEntry bool `json:"noQuayRHOAIEntry,omitempty"`
	// NoApplicationsNamespace: RHOAI has no DSCInitialization yet, so
	// there is nowhere to copy to.
	NoApplicationsNamespace bool `json:"noApplicationsNamespace,omitempty"`
	Exists                  bool `json:"exists"`
	// Ours: the Secret carries this tool's label (only then does the tool
	// change it).
	Ours bool `json:"ours"`
	// Current: it holds only the pull secret's current quay.io/rhoai
	// credential and the registry can use it (Unusable is empty).
	Current bool `json:"current"`
	// Unusable says why the registry does not use the Secret: being
	// deleted, another type, or openshift.io/image.excludeImageSecret=true.
	Unusable string `json:"unusable,omitempty"`
	// Problem says why the state could not be read.
	Problem string `json:"problem,omitempty"`
}

// NodePullSecretInfo is the nodes' side of the quay.io/rhoai credentials on
// a cluster whose nodes pull with the cluster pull secret,
// openshift-config/pull-secret, which the updater's ServiceAccount cannot
// read (the one-click setup reads it with the signed-in user's token).
type NodePullSecretInfo struct {
	// Command adds the quay.io/rhoai entry to openshift-config/pull-secret,
	// for a cluster-admin to run (it prompts for the token).
	Command string `json:"command"`
	// RemoveCommand removes that entry again.
	RemoveCommand string `json:"removeCommand"`
	// Pulled: the nightly CatalogSource is READY on its quay.io/rhoai image,
	// so the nodes have pulled from quay.io/rhoai.
	Pulled bool `json:"pulled"`
	// AddedByTool is this tool's record that it added the quay.io/rhoai
	// entry of openshift-config/pull-secret; absent when it has none (the
	// entry, if any, was set by someone else, or the cluster has none).
	// The record is read with the ServiceAccount, which cannot read that
	// Secret: the one-click actions check the entry against it with the
	// user's token before they change anything.
	AddedByTool *NodePullSecretRecord `json:"addedByTool,omitempty"`
}

// NodePullSecretRecord is this tool's record of the quay.io/rhoai entry it
// added to openshift-config/pull-secret. It never holds the token.
type NodePullSecretRecord struct {
	// At and By: when and by whom the entry was written.
	At string `json:"at,omitempty"`
	By string `json:"by,omitempty"`
	// SameToken: the recorded entry has the token saved under Pull secret
	// now (false: that token was replaced since, so Add updates the entry).
	SameToken bool `json:"sameToken"`
}

// NodeSetupRequest is the JSON body of POST /api/setup/nodes/plan and
// POST /api/setup/nodes/stream: one of the one-click setup actions
// ("mirror-create", "mirror-remove", "pull-secret-add",
// "pull-secret-remove"). The stream also sends back the object version the
// plan was made on (ResourceVersion, UID), so a change made after the user
// reviewed the plan stops it with nothing changed.
type NodeSetupRequest struct {
	Action          string `json:"action"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
	UID             string `json:"uid,omitempty"`
	// ReplaceExisting: for "pull-secret-add", the user acknowledged that a
	// quay.io/rhoai entry this tool did not add is replaced, its token is
	// not kept and the tool cannot restore it. Without it such an entry is
	// never replaced (RefusalCode "foreign_entry").
	ReplaceExisting bool `json:"replaceExisting,omitempty"`
}

// NodeSetupPlan is what a one-click setup action would do, worked out with
// the signed-in user's own token and a server-side dry run. It never holds
// a credential.
type NodeSetupPlan struct {
	Action string `json:"action"`
	// ControlPlane as in ClusterInfo, read again with the user's token;
	// "" when unknown.
	ControlPlane string `json:"controlPlane,omitempty"`
	// Ready: the action can run (the dry run passed). False with
	// NothingToDo, or with Refusal.
	Ready       bool `json:"ready"`
	NothingToDo bool `json:"nothingToDo,omitempty"`
	// State is what is on the cluster now, in words.
	State string `json:"state"`
	// Changes lists exactly what is created, changed or deleted.
	Changes []string `json:"changes,omitempty"`
	// NodeImpact is what the nodes do when it rolls out.
	NodeImpact string `json:"nodeImpact,omitempty"`
	// Undo says how to take it back.
	Undo string `json:"undo,omitempty"`
	// Pools names the machine config pools that will not take the change
	// yet (paused or degraded).
	Pools []string `json:"pools,omitempty"`
	// Refusal says why it cannot be done here; RefusalCode is "hosted",
	// "managed", "unknown_topology", "unknown_platform" (the pull secret
	// actions when the managed-cluster webhooks cannot be listed),
	// "forbidden" (RBAC or an admission webhook refused the dry run),
	// "policy_conflict" (Create the image mirror: an entry for the source
	// sets mirrorSourcePolicy NeverContactSource, which the MCO would refuse
	// next to the default), "parent_scope" (Create the image mirror: a mirror object maps a parent
	// scope of registry.redhat.io/rhoai that the nodes use for it), "foreign"
	// (an object of that name that is not this setup's), "foreign_entry" (Add: a quay.io/rhoai entry
	// this tool did not add, with another token; Replaceable),
	// "not_owned" (Remove: the entry is not the one this tool added),
	// "unreadable", "external_secrets", "no_token" or "quay".
	Refusal     string `json:"refusal,omitempty"`
	RefusalCode string `json:"refusalCode,omitempty"`
	// ResourceVersion and UID of the object the plan was made on, sent back
	// with the stream request.
	ResourceVersion string `json:"resourceVersion,omitempty"`
	UID             string `json:"uid,omitempty"`
	// EntrySetBy, for the pull secret actions once the Secret was read: who
	// set its quay.io/rhoai entry, "tool" (this tool's record matches it),
	// "other" (an entry this tool did not add, or one changed since) or
	// "none" (no entry).
	EntrySetBy string `json:"entrySetBy,omitempty"`
	// ExistingKeys are the auths keys of the quay.io/rhoai entries there:
	// registry names only, never their values.
	ExistingKeys []string `json:"existingKeys,omitempty"`
	// Replaceable: Add was refused for an entry this tool did not add
	// ("foreign_entry"); a request with ReplaceExisting replaces it.
	Replaceable bool `json:"replaceable,omitempty"`
	// Replacing: the plan replaces an entry this tool did not add, as the
	// request acknowledged.
	Replacing bool `json:"replacing,omitempty"`
}

// NodePullCheckRequest is the JSON body of POST /api/setup/node-pull-check.
type NodePullCheckRequest struct {
	Image string `json:"image"`
}

// ImageMirrorInfo reports whether an ImageDigestMirrorSet exists for RHOAI images.
type ImageMirrorInfo struct {
	Exists bool `json:"exists"`
	// Kind is ImageDigestMirrorSet or ImageContentSourcePolicy, the object
	// that mirrors the source; "" when there is none.
	Kind   string `json:"kind,omitempty"`
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

// UpdateStepEvent represents a single step in the update pipeline (and the
// other streamed operations): one event of their step stream.
type UpdateStepEvent struct {
	Step      string `json:"step"`
	Status    string `json:"status"` // "running", "success", "failed", "skipped"
	Message   string `json:"message"`
	Detail    string `json:"detail,omitempty"`
	ErrorCode string `json:"errorCode,omitempty"`
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

// DSCManager is the Components page's view of the DataScienceCluster
// (GET /api/components/dsc): every component and sub-component the
// installed CRD defines, with the value in the DSC, the release default
// and what the operator reports, and the differences from the release
// default that are real problems.
type DSCManager struct {
	// Exists is false when there is no DataScienceCluster (State says why:
	// "no-dsc" or "no-crd"); nothing else is filled then.
	Exists bool   `json:"exists"`
	State  string `json:"state"`
	Name   string `json:"name,omitempty"`
	UID    string `json:"uid,omitempty"`
	// APIVersion is the version the DSC was read at
	// ("datasciencecluster.opendatahub.io/v3"); StorageVersion is the CRD's
	// storage version ("v3"). Changes are made only when they match.
	APIVersion     string `json:"apiVersion,omitempty"`
	StorageVersion string `json:"storageVersion,omitempty"`
	// DSCCount is the number of DataScienceClusters (the operator allows one).
	DSCCount int    `json:"dscCount"`
	Phase    string `json:"phase,omitempty"`
	// Release is status.release: the platform ("OpenShift AI Self-Managed")
	// and version the operator reports.
	ReleaseName    string `json:"releaseName,omitempty"`
	ReleaseVersion string `json:"releaseVersion,omitempty"`
	// OperatorVersion is the installed CSV's version, whose alm-examples
	// hold the release default.
	OperatorVersion string `json:"operatorVersion,omitempty"`
	DefaultsSource  string `json:"defaultsSource,omitempty"`
	// DefaultsError says why there is no release default to compare with;
	// SchemaError why the CRD schema could not be read.
	DefaultsError string `json:"defaultsError,omitempty"`
	SchemaError   string `json:"schemaError,omitempty"`
	// ReadOnlyReason says why this page changes nothing in the DSC now
	// (Argo CD, an ACM policy, a deletion, a version the CRD does not
	// store, ...); "" when changes are possible.
	ReadOnlyReason string   `json:"readOnlyReason,omitempty"`
	ManagedBy      string   `json:"managedBy,omitempty"` // Argo CD application
	ACMEnforced    bool     `json:"acmEnforced,omitempty"`
	Deleting       bool     `json:"deleting,omitempty"`
	Finalizers     []string `json:"finalizers,omitempty"`
	// Conditions are the DSC's overall conditions (Ready,
	// ProvisioningSucceeded, ComponentsReady, ModulesReady).
	Conditions []DSCConditionView `json:"conditions"`
	// UpgradeGates are conditions that hold provisioning for an admin
	// acknowledgement (ConfigMap odh-upgrade-acks).
	UpgradeGates []string      `json:"upgradeGates,omitempty"`
	Nodes        []DSCNode     `json:"nodes"`
	Mismatches   []DSCMismatch `json:"mismatches"`
	// Comparison says what the comparison with the release default
	// covered, so the page says "nothing missing" only when it was checked.
	Comparison DSCComparison `json:"comparison"`
	// PendingHardReset is a hard reset that deleted the DataScienceCluster
	// and did not create its replacement (a failure, a timeout, a restart
	// of the tool): shown only while no DataScienceCluster exists, so
	// "Finish the hard reset" creates exactly the object it planned.
	PendingHardReset *DSCPendingHardReset `json:"pendingHardReset,omitempty"`
	// PendingHardResetError says why a recorded hard reset could not be
	// read.
	PendingHardResetError string `json:"pendingHardResetError,omitempty"`
}

// DSCHardResetRecord is what a hard reset records before it deletes the
// DataScienceCluster (the operation ConfigMap, key dscHardReset): the exact
// object it creates, so the reset can be finished after a failure, a
// timeout or a restart of the tool.
type DSCHardResetRecord struct {
	Name       string `json:"name"`
	OldUID     string `json:"oldUID"`
	APIVersion string `json:"apiVersion"`
	// Object is the DataScienceCluster to create, as JSON; YAML the same
	// object as the plan showed it.
	Object string `json:"object"`
	YAML   string `json:"yaml"`
	// OperatorCSV is the installed CSV whose alm-examples gave the object.
	OperatorCSV     string `json:"operatorCSV"`
	OperatorVersion string `json:"operatorVersion,omitempty"`
	User            string `json:"user,omitempty"`
	StartedAt       string `json:"startedAt"`
	// ModuleOperators are the module operator Deployments of the module CRs
	// the old DSC owned, as "<namespace>/<name>/<uid>": the platform deletes
	// each once its module CR is gone, and the new DSC is created only
	// after that.
	ModuleOperators []string `json:"moduleOperators,omitempty"`
}

// DSCPendingHardReset is a recorded hard reset that can be finished.
type DSCPendingHardReset struct {
	Name            string `json:"name"`
	APIVersion      string `json:"apiVersion"`
	NewObject       string `json:"newObject"`
	OperatorVersion string `json:"operatorVersion,omitempty"`
	User            string `json:"user,omitempty"`
	StartedAt       string `json:"startedAt"`
	// Token binds "Finish the hard reset" to this object.
	Token string `json:"token"`
}

// DSCFinishRequest is the body of POST /api/components/dsc/hard-reset/finish.
type DSCFinishRequest struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

// DSCConditionView is one status condition of the DataScienceCluster.
type DSCConditionView struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// DSCNode is a component, or a sub-component nested at any depth, of
// spec.components as the installed CRD defines it.
type DSCNode struct {
	Path   string `json:"path"`   // "kserve.nim"
	Name   string `json:"name"`   // "nim"
	Parent string `json:"parent"` // "kserve"; "" for a component
	Depth  int    `json:"depth"`  // 0 for a component
	// Toggle: the object has its own managementState. A group without one
	// (dashboard, data) only holds sub-components.
	Toggle bool `json:"toggle"`
	// Allowed are the managementState values the CRD accepts (its enum).
	Allowed       []string `json:"allowed,omitempty"`
	SchemaDefault string   `json:"schemaDefault,omitempty"`
	// Spec is the value in the DSC ("" when unset; SpecSet false).
	Spec    string `json:"spec,omitempty"`
	SpecSet bool   `json:"specSet"`
	// Default is the release default's value (alm-examples).
	Default    string `json:"default,omitempty"`
	DefaultSet bool   `json:"defaultSet"`
	// Actual is status.components' managementState for it, when the
	// operator reports one (sub-components appear there as top-level keys).
	Actual    string            `json:"actual,omitempty"`
	Condition *DSCConditionView `json:"condition,omitempty"`
	// Changed: a value the user chose that differs from the release default.
	Changed bool `json:"changed,omitempty"`
	// Unknown: a field in the DSC that the installed CRD does not define
	// (an "unknown-field" mismatch with this path), shown in the tree under
	// the nearest node above it; Name is its path below that node.
	Unknown bool `json:"unknown,omitempty"`
}

// DSCComparison is what the comparison with the release default covered.
type DSCComparison struct {
	// DefaultsRead: the release default was read, so values missing from
	// the DSC and values changed from the default were looked for
	// (DSCManager.DefaultsError says why not).
	DefaultsRead bool `json:"defaultsRead"`
	// SchemaRead: the CRD's schema was read, so fields it does not define
	// were looked for (DSCManager.SchemaError says why not).
	SchemaRead bool `json:"schemaRead"`
	// DefaultsCSV is the installed CSV whose alm-examples hold the release
	// default.
	DefaultsCSV string `json:"defaultsCSV,omitempty"`
	// CRDComponents, DefaultComponents and DSCComponents are the components
	// (keys of spec.components) the installed CRD defines, the release
	// default lists and the DSC lists, each sorted; DefaultComponents is
	// empty without a release default.
	CRDComponents     []string `json:"crdComponents"`
	DefaultComponents []string `json:"defaultComponents"`
	DSCComponents     []string `json:"dscComponents"`
}

// DSCMismatch is a difference from the release default that is a real
// problem.
type DSCMismatch struct {
	// Kind: "unset" (the release default sets it, the DSC does not: the
	// operator treats it as Removed), "parent-removed" (Managed under a
	// parent that is not Managed: it deploys nothing) or "unknown-field"
	// (the installed CRD does not define it).
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Default string `json:"default,omitempty"`
	Parent  string `json:"parent,omitempty"`
	Message string `json:"message"`
	// Fixable: Fix adds (or removes) it.
	Fixable bool `json:"fixable"`
}

// DSCPlan is what a change to the DataScienceCluster would do, computed
// from a fresh read (GET /api/components/dsc/plan). Token binds the
// change: the server refuses it when the plan computed again differs.
type DSCPlan struct {
	Action     string `json:"action"` // "toggle", "fix", "soft-reset", "hard-reset"
	Name       string `json:"name"`
	UID        string `json:"uid,omitempty"`
	APIVersion string `json:"apiVersion,omitempty"`
	// Refusal says why the change is not possible; Code classifies it
	// ("gitops_managed", "parent_removed", "nothing_to_do", ...).
	Refusal string `json:"refusal,omitempty"`
	Code    string `json:"code,omitempty"`
	// ParentPath is the parent to set to Managed too (toggle to Managed
	// under a parent that is not Managed).
	ParentPath string      `json:"parentPath,omitempty"`
	Changes    []DSCChange `json:"changes"`
	// Blocked are the components whose removal must not happen now; the
	// change is refused while any is listed.
	Blocked  []DSCRemovalBlock `json:"blocked,omitempty"`
	Warnings []string          `json:"warnings,omitempty"`
	// Hard reset: what stops working while the components are down
	// (each resource once, with the components it needs), and the CRDs
	// that cannot be read at another version meanwhile (the component
	// serves their conversion webhook).
	StopsWorking   []DSCStopsWorking  `json:"stopsWorking,omitempty"`
	ConversionCRDs []DSCConversionCRD `json:"conversionCRDs,omitempty"`
	// Hard reset: the enabled components, those the default leaves
	// Removed, and the object it creates (YAML, with the values the
	// API server's defaulting fills in from the installed CRD).
	TornDown    []string `json:"tornDown,omitempty"`
	StayRemoved []string `json:"stayRemoved,omitempty"`
	NewObject   string   `json:"newObject,omitempty"`
	// Hard reset: how long it waits for the old DataScienceCluster to be
	// gone before it stops without creating anything.
	DeleteTimeoutSeconds int `json:"deleteTimeoutSeconds,omitempty"`
	// Resets: the fields under spec.components whose value the reset
	// keeps (the CRD's validation rules do not let it change), where the
	// DataScienceCluster sets them.
	ProtectedFields []string `json:"protectedFields,omitempty"`
	// Resets: KeepEnabled is the option the plan was made with (part of
	// the token). TurnedOn are the toggles the user turned on: Managed (or
	// Unmanaged) in the DataScienceCluster and deployed, while the release
	// default leaves them off, with the parents they need. Kept are those
	// the reset keeps at their live value (TurnedOn with KeepEnabled, else
	// none).
	KeepEnabled bool     `json:"keepEnabled,omitempty"`
	TurnedOn    []string `json:"turnedOn,omitempty"`
	Kept        []string `json:"kept,omitempty"`
	Token       string   `json:"token,omitempty"`
}

// DSCChange is one change of a plan.
type DSCChange struct {
	Path string `json:"path"`
	// From and To are the values ("" with FromSet/ToSet false: unset).
	From    string `json:"from,omitempty"`
	FromSet bool   `json:"fromSet"`
	To      string `json:"to,omitempty"`
	ToSet   bool   `json:"toSet"`
	// Toggle: a managementState. Effect: "deploys" or "removes" when what
	// the operator deploys changes (also through a parent), else "".
	Toggle bool   `json:"toggle"`
	Effect string `json:"effect,omitempty"`
}

// DSCToggleRequest is the body of POST /api/components/dsc/management.
type DSCToggleRequest struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// From is the value the user saw ("" unset); To the value to set.
	From string `json:"from"`
	To   string `json:"to"`
	// WithParent also sets the parents that are not Managed to Managed.
	WithParent bool `json:"withParent"`
	// UID and Token are those of the plan the user reviewed (GET
	// /api/components/dsc/plan?action=toggle): the change is refused when
	// the plan computed again differs.
	UID   string `json:"uid"`
	Token string `json:"token"`
}

// DSCPlanRequest is the body of POST /api/components/dsc/fix, /reset and
// /hard-reset/stream: the plan the user reviewed.
type DSCPlanRequest struct {
	Name  string `json:"name"`
	UID   string `json:"uid"`
	Token string `json:"token"`
	// Confirm is the DSC name typed by the user (hard reset only).
	Confirm string `json:"confirm,omitempty"`
	// KeepEnabled (soft and hard reset, required there): keep the
	// components the user turned on (DSCPlan.Kept), as the reviewed plan
	// was made.
	KeepEnabled *bool `json:"keepEnabled,omitempty"`
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

// DSCStopsWorking is a resource that stops working while a hard reset has
// the components it needs down ("pipeline server team/dspa").
type DSCStopsWorking struct {
	Resource   string   `json:"resource"`
	Components []string `json:"components"`
}

// DSCConversionCRD is a CRD whose conversion webhook a component serves:
// while the component is down, its objects cannot be read at another
// version.
type DSCConversionCRD struct {
	CRD       string `json:"crd"`
	Component string `json:"component"`
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
	// ApplicationsNamespace is RHOAI's applications namespace, read from the
	// DSCInitialization ("" when there is none yet). OperatorNamespace is
	// where the RHOAI operator runs (its Subscription's namespace).
	ApplicationsNamespace string `json:"applicationsNamespace,omitempty"`
	OperatorNamespace     string `json:"operatorNamespace,omitempty"`
	// ApplicationsNamespaceError says why the applications namespace could
	// not be read; its Deployments are missing from the list then.
	ApplicationsNamespaceError string `json:"applicationsNamespaceError,omitempty"`
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
	OperatorAvailable bool   `json:"operatorAvailable"`
	OperatorPaused    bool   `json:"operatorPaused"`
	OperatorError     string `json:"operatorError,omitempty"`
	// OperatorName and OperatorNamespace name the dashboard module's
	// operator Deployment, discovered at run time (set when it is
	// available). NoOperatorReason says why the cluster has no dashboard
	// module (the legacy flow). Namespace is the applications namespace.
	OperatorName            string              `json:"operatorName,omitempty"`
	OperatorNamespace       string              `json:"operatorNamespace,omitempty"`
	NoOperatorReason        string              `json:"noOperatorReason,omitempty"`
	Namespace               string              `json:"namespace,omitempty"`
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
	// OperatorName and OperatorNamespace name the paused Deployment: the
	// dashboard module's operator, discovered at run time.
	OperatorName      string `json:"operatorName"`
	OperatorNamespace string `json:"operatorNamespace"`
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
	// RecipeProjects are the projects recipes created that the cluster has
	// now (Clean up everything offers them); RecipeProjectsError says why
	// they could not be read (none is offered then).
	RecipeProjects      []RecipeOwnedProject `json:"recipeProjects,omitempty"`
	RecipeProjectsError string               `json:"recipeProjectsError,omitempty"`
	// OutsidePipelineServers are this tool's pipeline servers in projects
	// that are not (or no longer) Data Science projects (the dashboard's
	// label was removed): the project picker does not list them, but they
	// are on the cluster and may use the S3 storage. Delete and Clean up
	// remove them.
	OutsidePipelineServers []ResourceState `json:"outsidePipelineServers,omitempty"`
	// LegacyStorage is the S3 storage an earlier version of this tool left
	// in namespace minio, until Migrate storage (or teardown) removes it.
	LegacyStorage *LegacyStorageState `json:"legacyStorage,omitempty"`
	// S3Keys says which keys the S3 storage's clients use (nil when the
	// storage is not this tool's, not deployed, or still to be migrated).
	S3Keys *S3KeysState `json:"s3Keys,omitempty"`
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
	// AuthorinoTLSOff: the AuthPolicy is enforced, but Connectivity Link's
	// Authorino serves its listener without TLS, which RHOAI's setup of the
	// gateway's authentication turns on; calls through the gateway can then
	// fail with HTTP 500. AuthorinoTLSCommands are RHOAI's steps for a
	// cluster-admin ("" when they cannot be shown; Warning says why).
	AuthorinoTLSOff      bool   `json:"authorinoTlsOff,omitempty"`
	AuthorinoTLSCommands string `json:"authorinoTlsCommands,omitempty"`
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

	// --- Models routing: names on this tool's gateway's list ---

	// UnusedProjects are the projects on this tool's gateway's list (not
	// its own namespace) that no model routes through it from: their
	// editors can still attach routes to the gateway. Action "disallow"
	// takes one off. GoneProjects are those of them that no longer exist:
	// a new project of that name would be admitted at once.
	UnusedProjects []string `json:"unusedProjects,omitempty"`
	GoneProjects   []string `json:"goneProjects,omitempty"`
	// AddedProjects are the projects on the list that this tool added (its
	// record): deleting the last model routed through the gateway from one
	// takes it off the list.
	AddedProjects []string `json:"addedProjects,omitempty"`
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
	// Outside are this tool's models in projects that are not (or no
	// longer) Data Science projects (ResourcesStatus.OutsidePipelineServers).
	Outside []ModelState `json:"outside,omitempty"`
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
	// VLLMArgs is the vLLM options the tool's model runs with (its main
	// container's VLLM_ADDITIONAL_ARGS; "" for none), which can differ from
	// the catalog's after a catalog change until it is set up again; nil
	// for a model the tool did not create.
	VLLMArgs *string `json:"vllmArgs,omitempty"`
	// PresetImageMissing: the preset's image cannot be pulled because the
	// registry does not have it (a nightly build's image is not published
	// yet); setting the model up again with the newest released image
	// fixes it.
	PresetImageMissing bool `json:"presetImageMissing,omitempty"`
	// PresetImageUncertain (with PresetImageMissing): the pull has failed
	// for longer than a setup waits, and a registry answered that the image
	// does not exist while another refused the node's credentials, so the
	// image may be missing or the node may lack the pull secret.
	PresetImageUncertain bool `json:"presetImageUncertain,omitempty"`
	// MaaSPublished: the model routes through the MaaS gateway (published
	// on the MaaS tab), so it is used through MaaS and its direct address
	// answers 404.
	MaaSPublished bool `json:"maasPublished,omitempty"`
	// MaaSOnlySetup: the model was set up on the Models tab to be reached
	// through MaaS only, and is published as part of that setup. Deleting
	// it unpublishes it first; Unpublish leaves it with no route (only
	// inside the cluster).
	MaaSOnlySetup bool `json:"maasOnlySetup,omitempty"`
	// TokenSecret is the model's token Secret, which this tool made with
	// ServiceAccount TokenServiceAccount as the dashboard's "Require token
	// authentication" does; "" when it has none (set up by an earlier
	// build: setting it up again makes it).
	TokenSecret         string `json:"tokenSecret,omitempty"`
	TokenServiceAccount string `json:"tokenServiceAccount,omitempty"`
	// TokenAuthOff: the model carries security.opendatahub.io/enable-auth
	// "false" (someone turned token authentication off in the dashboard).
	TokenAuthOff bool `json:"tokenAuthOff,omitempty"`

	// --- Models routing (how the model is reached) ---

	// Exposure is how the model is reached, read from its spec.router:
	// "default-gateway" (route and gateway {}), "maas" (refs name the MaaS
	// gateway), "gateway" (refs name another gateway), "internal" (no
	// route), or "" when it cannot be told.
	Exposure string `json:"exposure,omitempty"`
	// Gateway is the gateway the model routes through ("namespace/name"):
	// KServe's default for "default-gateway", the first ref otherwise.
	Gateway string `json:"gateway,omitempty"`
	// GatewayAuth is the AuthPolicy odh-model-controller puts on that
	// gateway (<gateway>-authn): "enforced", "not-enforced", "missing",
	// "maas" (the MaaS gateway, whose auth MaaS does), or "" (unknown).
	GatewayAuth string `json:"gatewayAuth,omitempty"`
	// InternalURL is the model's cluster-internal address (status.addresses).
	InternalURL string `json:"internalUrl,omitempty"`
	// ServedName is the name the model answers to (spec.model.name): the
	// "model" a request sends.
	ServedName string `json:"servedName,omitempty"`
	// ServedNameWithheld: spec.model.name has characters that are not safe
	// in commands (safeServedName), so ServedName is empty and no command
	// or call check uses it.
	ServedNameWithheld bool `json:"servedNameWithheld,omitempty"`
	// CallCheck is the last "Can it be called?" check of this model by this
	// app (nil: not checked since the app started).
	CallCheck *ModelCallCheck `json:"callCheck,omitempty"`
}

// ModelsRequest is the body of POST /api/resources/models/stream.
type ModelsRequest struct {
	// Action is "setup", "teardown", "allow": add Project to the
	// projects this tool's inference gateway allows (no model), or "check":
	// call the model Model in Project once (Exposure below).
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

	// --- Models routing ---

	// Exposure (setup with a project) is how the model is reached:
	// "default-gateway", "maas", "gateway" (GatewayRef) or "internal"; ""
	// is the choice GET /api/resources/models/exposures recommends.
	// Action "check" calls the model once ("Can it be called?").
	Exposure string `json:"exposure,omitempty"`
	// GatewayRef is the gateway of Exposure "gateway": "namespace/name".
	GatewayRef string `json:"gatewayRef,omitempty"`
}

// WorkbenchStatus is the Workbench feature (GET /api/resources/workbench):
// what a workbench needs on the cluster, this tool's workbenches, and for
// the chosen project the workbench there and what Set up would create.
// Everything about a project is read with the signed-in admin's own token.
type WorkbenchStatus struct {
	// Project is the project the answer is about ("" for none).
	Project string `json:"project,omitempty"`
	// Blocked says why no workbench can be set up (the workbenches
	// component is off), or is nil; Unsupported says why this platform
	// cannot have one (RHOAI 2.x), or is "".
	Blocked     *ComponentBlock `json:"blocked,omitempty"`
	Unsupported string          `json:"unsupported,omitempty"`
	// Items are this tool's workbenches (in the projects it recorded one)
	// and the chosen project's nightly-workbench, whoever made it.
	Items []WorkbenchState `json:"items,omitempty"`
	// ItemsProblem says why this tool's workbenches could not be read;
	// ItemsUnreadable are the projects whose workbench could not be read
	// (their state is unknown: never shown as absent).
	ItemsProblem    string   `json:"itemsProblem,omitempty"`
	ItemsUnreadable []string `json:"itemsUnreadable,omitempty"`
	// Plan is what Set up would create in Project (nil when it cannot:
	// PlanProblem says why, PlanProblemCode is "no_profile", "no_image",
	// "validation", "forbidden", ...).
	Plan            *WorkbenchPlan `json:"plan,omitempty"`
	PlanProblem     string         `json:"planProblem,omitempty"`
	PlanProblemCode string         `json:"planProblemCode,omitempty"`
}

// WorkbenchPlan is what Set up would create, chosen as the dashboard's
// Create workbench form would choose it.
type WorkbenchPlan struct {
	// Image is the image's display name, ImageStream and ImageTag the
	// ImageStream tag it runs.
	Image       string `json:"image"`
	ImageStream string `json:"imageStream"`
	ImageTag    string `json:"imageTag"`
	// ImageDeprecated: its ImageStream is deprecated (the dashboard labels
	// it Deprecated), chosen because every image the dashboard offers is.
	ImageDeprecated bool `json:"imageDeprecated,omitempty"`
	// HardwareProfile is the profile's display name, HardwareProfileName
	// its "<namespace>/<name>"; CPU and Memory are what the workbench
	// requests (the profile's defaults).
	HardwareProfile     string `json:"hardwareProfile"`
	HardwareProfileName string `json:"hardwareProfileName"`
	CPU                 string `json:"cpu,omitempty"`
	Memory              string `json:"memory,omitempty"`
	// Queue is the Kueue LocalQueue the profile sends the workbench to
	// ("" for none).
	Queue string `json:"queue,omitempty"`
	// StorageSize and StorageClass are its storage's ("" class: the
	// cluster's default).
	StorageSize  string `json:"storageSize"`
	StorageClass string `json:"storageClass,omitempty"`
	// Connection: the tool's S3 Connection is attached; Pipelines: the
	// pipeline server's settings are mounted (Elyra); MLflow: it is
	// linked to MLflow.
	Connection bool `json:"connection,omitempty"`
	Pipelines  bool `json:"pipelines,omitempty"`
	MLflow     bool `json:"mlflow,omitempty"`
}

// WorkbenchState is one workbench named nightly-workbench (Namespace).
type WorkbenchState struct {
	ResourceState
	// State is how the dashboard shows it: "starting", "running",
	// "stopping", "stopped" or "failed".
	State string `json:"state,omitempty"`
	// URL is where it opens (the dashboard's address and
	// /notebook/<project>/<name>); "" when unknown.
	URL string `json:"url,omitempty"`
	// Image is the image's display name and tag, HardwareProfile the
	// profile's name, as the workbench records them.
	Image string `json:"image,omitempty"`
	// ImageDeprecated: the ImageStream it runs is deprecated
	// (opendatahub.io/deprecated), which the dashboard labels Deprecated.
	ImageDeprecated bool   `json:"imageDeprecated,omitempty"`
	HardwareProfile string `json:"hardwareProfile,omitempty"`
	// CPU and Memory are what its container requests.
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	// ConnectionAttached: its connections annotation names the tool's S3
	// Connection.
	ConnectionAttached bool `json:"connectionAttached,omitempty"`
	// Storage is its storage nightly-workbench-storage (nil when absent).
	Storage *WorkbenchStorage `json:"storage,omitempty"`
}

// WorkbenchStorage is a workbench's storage, a PersistentVolumeClaim.
type WorkbenchStorage struct {
	Name          string `json:"name"`
	Size          string `json:"size,omitempty"`
	StorageClass  string `json:"storageClass,omitempty"`
	ManagedByTool bool   `json:"managedByTool"`
	// UsedBy are the other workbenches and pods that mount it ("workbench
	// my-wb", "pod x"): deleting it is refused while there are any.
	// UsedByProblem says why they could not be read.
	UsedBy        []string `json:"usedBy,omitempty"`
	UsedByProblem string   `json:"usedByProblem,omitempty"`
}

// WorkbenchRequest is the body of POST /api/resources/workbench/stream.
type WorkbenchRequest struct {
	// Action is "setup", "start", "stop", "teardown" or "delete-storage"
	// (the storage kept from a deleted workbench).
	Action  string `json:"action"`
	Project string `json:"project"`
	// DeleteStorage (teardown) also deletes its storage: its files are lost.
	DeleteStorage bool `json:"deleteStorage,omitempty"`
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
	// Models (this tool's playground only) are the models its Set up asked
	// the dashboard for, from this tool's record; none when the record
	// does not say (a playground set up before the record kept them).
	Models []PlaygroundModelRef `json:"models,omitempty"`
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
	// Subscriptions are the MaaS subscriptions a MaaS model is offered
	// under (the dashboard's gen-ai /maas/models). A playground chat request
	// for a MaaS model names one, or the dashboard cannot mint the MaaS key
	// it calls the model with.
	Subscriptions []string `json:"subscriptions,omitempty"`
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
	// Database is the database choice; Access the audience of this tool's
	// subscription (nil while it does not exist).
	Database *MaaSDatabase      `json:"database,omitempty"`
	Access   *MaaSAudienceState `json:"access,omitempty"`
	// SubscriptionPriority is the priority of this tool's subscription (nil
	// while it does not exist or is not this tool's).
	SubscriptionPriority *MaaSSubscriptionPriority `json:"subscriptionPriority,omitempty"`
	// NoUnlimited: the cluster's MaaSSubscription API has no unlimited
	// model reference (read from its CRD; false when that cannot be told).
	NoUnlimited bool `json:"noUnlimited,omitempty"`
	// Removing are this tool's publications whose project or model is
	// gone: the background check removes what is left of them on its own.
	// RemovalProblem is the error of its last try, when that failed.
	Removing       []MaaSRemoval `json:"removing,omitempty"`
	RemovalProblem string        `json:"removalProblem,omitempty"`
	// UsageLogs is the usage logs and dashboards as MaaS sees them, and
	// Portal the MaaS portal (nil when this RHOAI has none).
	UsageLogs *MaaSUsageLogs `json:"usageLogs,omitempty"`
	Portal    *MaaSPortal    `json:"portal,omitempty"`
}

// MaaSUsageLogs is the MaaS tab's "Usage logs and dashboards" row.
type MaaSUsageLogs struct {
	// State is "ready", "missing", "waiting" or "unknown", Message says it.
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
	// Warning is what the default tenant reports about them (its Degraded
	// condition), when it does.
	Warning string `json:"warning,omitempty"`
	// SetUpBlockedReason says why Set up cannot set them up now (the S3
	// storage), "" when it can: Set up's checkbox is ticked then.
	SetUpBlockedReason string `json:"setUpBlockedReason,omitempty"`
	// CaptureUser is MaasTenantConfig default-tenant's
	// spec.telemetry.logs.captureUser (nil: not set, MaaS records no user
	// IDs); CaptureUserByTool: this tool set it (Turn off puts back
	// CaptureUserPrevious: "unset", "false"). TenantFound: the tenant exists.
	CaptureUser         *bool  `json:"captureUser,omitempty"`
	CaptureUserByTool   bool   `json:"captureUserByTool,omitempty"`
	CaptureUserPrevious string `json:"captureUserPrevious,omitempty"`
	TenantFound         bool   `json:"tenantFound,omitempty"`
}

// MaaSPortal is the MaaS tab's portal row: the DataScienceCluster's
// dashboard.maasPortal.
type MaaSPortal struct {
	// ManagementState as read ("" not set); On: Managed.
	ManagementState string `json:"managementState,omitempty"`
	On              bool   `json:"on"`
	// Available is the DataScienceCluster's MaaSPortalAvailable ("True",
	// "False", "" not reported), Message its message.
	Available string `json:"available,omitempty"`
	Message   string `json:"message,omitempty"`
	// URL is the portal's address (the dashboard's gateway domain).
	URL string `json:"url,omitempty"`
	// SetByTool: this tool turned it on (Turn off puts back Previous).
	SetByTool bool   `json:"setByTool,omitempty"`
	Previous  string `json:"previous,omitempty"`
	// BlockedReason says why it cannot be turned on or off here.
	BlockedReason string `json:"blockedReason,omitempty"`
}

// MaaSSubscriptionPriority is the spec.priority of this tool's
// MaaSSubscription. Every write sets 0, the lowest value maas-api honours;
// an earlier version wrote 10, or a value below the other subscriptions'.
type MaaSSubscriptionPriority struct {
	// Value is spec.priority as written (0 when unset: the CRD's default).
	Value int64 `json:"value"`
	// Fix sets it to 0 ("subscription-priority"); nil while it is 0.
	Fix *MaaSAction `json:"fix,omitempty"`
	// Ties are the other MaaSSubscriptions in models-as-a-service that
	// maas-api ranks level with it (spec.priority 0, unset or negative,
	// which maas-api reads as 0): for a caller in one of them and in this
	// tool's, it takes the one whose largest single token limit on any of
	// its models is larger (the window is not compared; unlimited counts as
	// the largest), then the first by name.
	Ties []MaaSPriorityTie `json:"ties,omitempty"`
}

// MaaSPriorityTie is another subscription at priority 0 for maas-api, with
// its spec.priority as written (0 when unset; a negative one is read as 0).
type MaaSPriorityTie struct {
	Name     string `json:"name"`
	Priority int64  `json:"priority"`
}

// MaaSRemoval is a publication of this tool that is gone, and why.
type MaaSRemoval struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Reason    string `json:"reason"`
}

// MaaSAction is a change a platform part offers on its own (POST
// /api/resources/maas/stream with Action ID), shown in a confirmation
// that lists Changes first.
type MaaSAction struct {
	// ID is "upgrade-rhcl", "rhcl-memory", "rhcl-deny-on-failure",
	// "authorino-tls", "remove-tier-policies" or "subscription-priority".
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
	// MaaSOnlySetup: the Models tab set it up to be reached through MaaS
	// only, so Unpublish leaves it with no route (only inside the cluster).
	MaaSOnlySetup bool `json:"maasOnlySetup,omitempty"`
	// Partly: what is left of a publication undone half-way (Message says
	// what); Unpublish finishes it. Nothing about it settles on its own.
	Partly bool `json:"partly,omitempty"`
	// Removing: its publication is gone (the project is being deleted);
	// the background check removes what is left, so it offers nothing.
	Removing bool `json:"removing,omitempty"`
	// RoutingPending: this tool has no MaaSModelRef for it (deleted outside
	// the tool) while the model still routes through the MaaS gateway, so
	// its direct address does not answer; "Restore direct routing" puts
	// back the gateways and Gen AI metadata it had before publishing.
	RoutingPending bool `json:"routingPending,omitempty"`
	// Phase is the MaaSModelRef's phase ("Ready", ...).
	Phase string `json:"phase,omitempty"`
	// Endpoint is the MaaSModelRef's status.endpoint.
	Endpoint string `json:"endpoint,omitempty"`
	// Access and Subscription are the phases of the tool's MaaSAuthPolicy
	// and MaaSSubscription ("Active", ...).
	Access       string `json:"access,omitempty"`
	Subscription string `json:"subscription,omitempty"`
	// Limit is the model's entry in this tool's subscription (published only).
	Limit *MaaSLimit `json:"limit,omitempty"`
	// UsageWarning: a chat model of this tool whose vLLM arguments lack
	// --enable-force-include-usage (set up by an earlier version), so MaaS
	// counts its streamed responses as 0 tokens; says how to fix it.
	UsageWarning string `json:"usageWarning,omitempty"`
}

// MaaSRequest is the body of POST /api/resources/maas/stream.
type MaaSRequest struct {
	// Action is "setup" or "teardown" (the platform), "publish" or
	// "unpublish" (the tool's model Model in Project), a part's own fix:
	// "upgrade-rhcl", "rhcl-memory", "authorino-tls" or
	// "remove-tier-policies", or "subscription-priority" (this tool's
	// subscription to priority 0).
	Action  string `json:"action"`
	Project string `json:"project,omitempty"`
	// Model is the catalog name of a chat model (publish and unpublish
	// only); "" means the catalog's default (qwen25-05b-instruct).
	Model string `json:"model,omitempty"`
	// Database, Access, Limit and ConfirmKeysLost: MaaSDatabaseRequest and
	// MaaSAudience below (the database choice and the subscription's
	// settings).
	Database        *MaaSDatabaseRequest `json:"database,omitempty"`
	Access          *MaaSAudience        `json:"access,omitempty"`
	Limit           *MaaSLimit           `json:"limit,omitempty"`
	ConfirmKeysLost bool                 `json:"confirmKeysLost,omitempty"`
	// UsageLogs (setup only): set up the usage logs and dashboards (the
	// Observability tab's Set up) first. CaptureUser (setup only): record
	// user IDs in the usage logs once MaaS is ready (MaasTenantConfig
	// spec.telemetry.logs.captureUser). The actions "capture-user-on",
	// "capture-user-off", "portal-on" and "portal-off" take no field.
	UsageLogs   bool `json:"usageLogs,omitempty"`
	CaptureUser bool `json:"captureUser,omitempty"`
}

// --- MaaS: the database choice and the subscription's settings ------------

// MaaSDatabaseRequest is the database Set up creates when maas-db-config
// does not exist yet, or the one "change-database" switches to.
type MaaSDatabaseRequest struct {
	// Kind is "external" (the user's PostgreSQL at ConnectionURL),
	// "development" (the MaaS docs' scripts/setup-database.sh: one pod, no
	// volume) or "cnpg" (CloudNativePG, this tool's opinion).
	Kind string `json:"kind"`
	// ConnectionURL (external only) holds a password: it is written only to
	// Secret maas-db-config, never returned, logged or recorded.
	ConnectionURL string `json:"connectionUrl,omitempty"`
}

// MaaSDatabase is what GET /api/resources/maas says about the database.
type MaaSDatabase struct {
	// Current is "" (none yet: Set up asks), "adopted" (a maas-db-config
	// this tool did not write, used as it is), or the Kind this tool set up.
	Current   string `json:"current"`
	Namespace string `json:"namespace"`
	// Needed: Set up must be told which database to create.
	Needed bool `json:"needed,omitempty"`
	// Default is the choice the Set up dialog preselects.
	Default string               `json:"default"`
	Choices []MaaSDatabaseChoice `json:"choices"`
	// ChangeBlockedReason says why "change-database" cannot run now ("" when
	// it can, or when the database is not this tool's).
	ChangeBlockedReason string `json:"changeBlockedReason,omitempty"`
}

// MaaSDatabaseChoice is one database Set up (or Change database) can make.
type MaaSDatabaseChoice struct {
	ID string `json:"id"`
	// Plan is every change choosing it makes now.
	Plan          []string `json:"plan"`
	BlockedReason string   `json:"blockedReason,omitempty"`
}

// MaaSAudience is who may use this tool's published models: its
// MaaSSubscription's owner and its MaaSAuthPolicy's subjects.
type MaaSAudience struct {
	// Everyone: group system:authenticated (every signed-in user); Groups
	// and Users are then empty.
	Everyone bool `json:"everyone"`
	// Groups and Users are names as the identity provider sends them, which
	// MaaS matches exactly (an OpenShift group, an OIDC group like /admins;
	// a user name or a service account,
	// system:serviceaccount:<namespace>:<name>).
	Groups []string `json:"groups,omitempty"`
	Users  []string `json:"users,omitempty"`
}

// MaaSAudienceState is the audience of this tool's subscription as read.
type MaaSAudienceState struct {
	MaaSAudience
	// GroupProblems: groups that have no OpenShift Group, or that could not
	// be checked.
	GroupProblems []MaaSGroupProblem `json:"groupProblems,omitempty"`
}

// MaaSGroupProblem is a group of the audience that has no OpenShift Group
// (Missing), or whose Group could not be read (Message says why).
type MaaSGroupProblem struct {
	Name    string `json:"name"`
	Missing bool   `json:"missing,omitempty"`
	Message string `json:"message"`
	// Commands create the Group and add a user ("" when the name cannot
	// be an OpenShift Group's: only the identity provider has it).
	Commands string `json:"commands,omitempty"`
}

// MaaSLimit is a published model's token limit in this tool's
// subscription: exactly one of Unlimited and Rates.
type MaaSLimit struct {
	// Preset is "free" (100 tokens a minute, the docs' free sample),
	// "premium" (100,000 tokens a day, their premium sample), "unlimited"
	// (usage still metered) or "custom" (Rates). In a request it decides,
	// except for "custom"; in a response it is derived.
	Preset    string          `json:"preset"`
	Unlimited bool            `json:"unlimited,omitempty"`
	Rates     []MaaSTokenRate `json:"rates,omitempty"`
}

// MaaSTokenRate is one tokenRateLimits entry: Limit tokens per Window
// (MaaSSubscription CRD: limit 1 to 1,000,000,000; window ^[1-9]\d{0,3}(s|m|h)$).
type MaaSTokenRate struct {
	Limit  int64  `json:"limit"`
	Window string `json:"window"`
}

// PrerequisitesStatus is what each Environment feature needs on the cluster
// before its own setup can run (GET /api/resources/prerequisites), by
// feature: "pipelines", "mlflow", "models", "playground".
type PrerequisitesStatus struct {
	Features map[string]FeaturePrerequisites `json:"features"`
	// ModelsExposure is the way to reach a model that Models was planned
	// for (the request's exposure, "" none): the page ignores an answer
	// for another one.
	ModelsExposure string `json:"modelsExposure"`
}

// FeaturePrerequisites is one feature's cluster prerequisites: its
// DataScienceCluster components and the operators they need.
type FeaturePrerequisites struct {
	// Ready: every part is ready; the feature's own setup can run.
	Ready bool `json:"ready"`
	// Parts are the prerequisites in setup order, as the MaaS tab's parts.
	Parts []MaaSPart `json:"parts"`
	// SetupPlan lists, in order, every change Set up makes now for them
	// (components turned on, operators installed); empty when none.
	SetupPlan []string `json:"setupPlan,omitempty"`
	// SetupBlockedReason says why Set up cannot make them ready (the
	// DataScienceCluster is managed by Argo CD, there is none, a catalog
	// lacks a package, a read failed), or is "".
	SetupBlockedReason string `json:"setupBlockedReason,omitempty"`
	// Restorable are the components this tool turned on for the feature
	// and still owns: Clean up offers to turn them back.
	Restorable []RestorableComponent `json:"restorable,omitempty"`
}

// RestorableComponent is a DataScienceCluster component this tool set to
// Managed, and what it was before.
type RestorableComponent struct {
	// Component is its key in spec.components ("kserve").
	Component string `json:"component"`
	// Previous is its managementState before ("" when it was not set).
	Previous string `json:"previous,omitempty"`
	// Feature is the feature it was turned on for.
	Feature string `json:"feature"`
}

// PrerequisitesRequest is the body of POST
// /api/resources/prerequisites/stream: set up what Feature needs.
type PrerequisitesRequest struct {
	Feature string `json:"feature"`
	// Exposure (models only) is how the model about to be set up is
	// reached: "default-gateway" and "gateway" add Red Hat Connectivity
	// Link and its Kuadrant CR, which authenticate calls on a gateway.
	Exposure string `json:"exposure,omitempty"`
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

// ComponentNotIdentified is CleanupRequest.ComponentIDs' value for a
// reviewed component whose prerequisites record entry had no enablement
// ID (written before IDs).
const ComponentNotIdentified = "none"

// CleanupRequest is the body of POST /api/resources/cleanup/stream: what
// to delete of what this tool created, in this order.
type CleanupRequest struct {
	// PipelineServers are the projects whose pipeline server goes;
	// ConfirmUnrecorded maps each project whose unrecorded pipeline server
	// the user confirmed as this tool's to that server's UID
	// (PipelineServerRequest.ConfirmUnrecorded).
	PipelineServers   []string          `json:"pipelineServers,omitempty"`
	ConfirmUnrecorded map[string]string `json:"confirmUnrecorded,omitempty"`
	// ConfirmWorkbenches maps a project of PipelineServers to the
	// workbenches the user confirmed removing the tool's dashboard
	// Connection from, each by name to the UID reviewed
	// (PipelineServerRequest.ConfirmWorkbenches).
	ConfirmWorkbenches map[string]map[string]string `json:"confirmWorkbenches,omitempty"`
	// Storage deletes the S3 storage (and storage an earlier version left).
	Storage bool `json:"storage,omitempty"`
	// DeleteNamespace also deletes namespace seaweedfs (needs Storage).
	DeleteNamespace bool `json:"deleteNamespace,omitempty"`
	// MLflow deletes the MLflow server.
	MLflow bool `json:"mlflow,omitempty"`
	// ModelRegistry deletes the model registry this tool created (with
	// its registered models and its default database).
	ModelRegistry bool `json:"modelRegistry,omitempty"`
	// Kueue deletes this tool's distributed workloads setup: its samples,
	// queues and project label, and turns back its dashboard opt-in.
	Kueue bool `json:"kueue,omitempty"`
	// Observability removes the usage logs and dashboards this tool set up
	// (the Observability tab's Clean up); it runs before the S3 storage,
	// which keeps their data.
	Observability bool `json:"observability,omitempty"`
	// Models are the projects whose models of the tool's catalog go (all
	// of them);
	// InferenceGateway deletes the inference gateway after them.
	Models           []string `json:"models,omitempty"`
	InferenceGateway bool     `json:"inferenceGateway,omitempty"`
	// Playgrounds are the projects whose playground (requested by this
	// tool) goes, before the models it uses.
	Playgrounds []string `json:"playgrounds,omitempty"`
	// Workbenches are the projects whose workbench (nightly-workbench, made
	// by this tool) goes; WorkbenchStorage are the projects whose storage
	// (nightly-workbench-storage) goes, with the workbench or, for storage
	// kept from a deleted workbench, on its own: its files are lost.
	Workbenches      []string `json:"workbenches,omitempty"`
	WorkbenchStorage []string `json:"workbenchStorage,omitempty"`
	// MaaSPublished are the projects whose models this tool unpublishes from
	// Models as a Service first, before anything else goes; MaaSPlatform
	// removes the MaaS platform this tool set up, last.
	MaaSPublished []string `json:"maasPublished,omitempty"`
	MaaSPlatform  bool     `json:"maasPlatform,omitempty"`
	// Components are the DataScienceCluster components ("kserve") this
	// tool turned on that go back to what they were, last, each only while
	// this tool still owns the field and nothing on the cluster uses it.
	Components []string `json:"components,omitempty"`
	// ComponentIDs: a recipe's Clean up names the enablement it reviewed
	// of each of Components (the prerequisites record's ID, or
	// ComponentNotIdentified for a record entry written before IDs). One
	// turned on again since has another ID (an entry without one that now
	// has one, too) and is not turned back, checked under the cluster lock
	// right before; with ComponentIDs set, a component it does not name is
	// not turned back either. Without ComponentIDs (the Environment page's
	// Clean up) each is checked as before.
	ComponentIDs map[string]string `json:"componentIds,omitempty"`
	// RecipeProjects are projects recipes created, each deleted after the
	// parts that live in it and before the components: only while it
	// carries the recipe's label and the UID the recipe recorded.
	RecipeProjects []string `json:"recipeProjects,omitempty"`
	// RecipeProjectUIDs maps each of RecipeProjects to the UID of the
	// project the user reviewed (RecipeOwnedProject.UID): it is deleted
	// only while the record and the namespace still have that UID, so one
	// deleted and made again since is kept. Every recipe project needs one.
	RecipeProjectUIDs map[string]string `json:"recipeProjectUIDs,omitempty"`
	// RecipeRunID, MaaSPlatformUID and ModelRegistryUID: a recipe's Clean
	// up of the MaaS platform and the model registry its run created names
	// the run and the UID of each object it reviewed (the database Secret
	// maas-db-config; the ModelRegistry). The part is removed only while
	// the object still has that UID and the run's annotation, checked
	// again under the cluster lock; otherwise it is refused. Empty for the
	// Environment page's own Clean up.
	RecipeRunID      string `json:"recipeRunId,omitempty"`
	MaaSPlatformUID  string `json:"maasPlatformUid,omitempty"`
	ModelRegistryUID string `json:"modelRegistryUid,omitempty"`
	// KueueUID is the UID of the ClusterQueue of the distributed workloads
	// setup a recipe's run created (with RecipeRunID), checked the same way.
	KueueUID string `json:"kueueUid,omitempty"`
	// ConfirmProjectContents maps a recipe project to what it still holds
	// that this tool did not create ("Pod x", "PersistentVolumeClaim y",
	// ...), as a refusal listed them: a project is deleted with them only
	// when every one is confirmed for that project. A refusal lists all of
	// them, for every project that has any, confirmed or not.
	ConfirmProjectContents map[string][]string `json:"confirmProjectContents,omitempty"`
}

// S3TeardownRequest is the body of POST /api/resources/s3/teardown.
type S3TeardownRequest struct {
	// DeleteNamespace also deletes namespace seaweedfs, with everything in
	// it, once the storage's objects are gone (only when this tool created
	// it: it carries the tool label and a tool manager wrote it).
	DeleteNamespace bool `json:"deleteNamespace,omitempty"`
}

// MigrateStorageRequest is the body of POST /api/resources/s3/migrate/stream.
type MigrateStorageRequest struct {
	// RemoveOldStorage runs the last part: removing the old storage once the
	// migration is done. Without it Migrate storage sets up the new storage,
	// copies the objects and moves the pipeline servers.
	RemoveOldStorage bool `json:"removeOldStorage,omitempty"`
	// DeleteNamespace also deletes namespace minio (with RemoveOldStorage,
	// and only when this tool created it).
	DeleteNamespace bool `json:"deleteNamespace,omitempty"`
	// ConfirmProjects are the projects whose unrecorded pipeline server the
	// user confirmed as this tool's (LegacyStorageState.Unrecorded); Migrate
	// only.
	ConfirmProjects []string `json:"confirmProjects,omitempty"`
}

// S3KeysState is the key state of the S3 storage. Secured storage runs
// with IAM writes on, self-service off and an admin key that is in no
// project; each pipeline server this tool records then has a SeaweedFS
// user and key of its own. Storage an earlier version set up gave its
// admin key to every pipeline project: Secure S3 storage moves it to the
// secured state (POST /api/resources/s3/secure/stream).
type S3KeysState struct {
	// Secured is true once the storage runs with the secured IAM settings
	// and a new admin key.
	Secured bool `json:"secured"`
	// NeedsSecuring is true when the storage is not secured, or a recorded
	// pipeline server still uses a key that is not its own (Secure S3
	// storage stopped part-way): the storage's state is "Not secured".
	NeedsSecuring bool `json:"needsSecuring"`
	// OwnKey are the recorded pipeline servers (namespace/name) that use a
	// key of their own, read live: the access key ID in their Secret is one
	// of their SeaweedFS user's keys.
	OwnKey []string `json:"ownKey,omitempty"`
	// OldKey are the recorded pipeline servers that still use the old
	// shared key: Secure S3 storage gives each its own.
	OldKey []string `json:"oldKey,omitempty"`
	// KeepOldKey are the other pipeline servers on the storage, each with
	// why the tool leaves its key ("ns/name (not recorded as this
	// by this tool)" or "(not set up by this tool)"): after Secure S3
	// storage their old key no longer works.
	KeepOldKey []string `json:"keepOldKey,omitempty"`
	// LeftoverUsers are SeaweedFS users this tool made (rnu-dspa-*) whose
	// project has no pipeline server at all (no DSPA that is not being
	// deleted, whoever made it); their keys still work. Only Remove
	// leftover keys and Secure S3 storage delete them, after a confirmation
	// that lists them. Empty when anything cannot be read (fail closed).
	LeftoverUsers []string `json:"leftoverUsers,omitempty"`
	// Leftovers are LeftoverUsers with their project and whether its
	// Connection still holds one of their keys.
	Leftovers []S3LeftoverKey `json:"leftovers,omitempty"`
	// Users names the SeaweedFS user of each recorded pipeline server
	// (namespace/name to rnu-dspa-<project>, hashed for long names).
	Users map[string]string `json:"users,omitempty"`
	// CheckError says why which key a pipeline server uses could not be
	// read (the state is then not changed by it).
	CheckError string `json:"checkError,omitempty"`
	// SecureBlockedReason explains why Secure S3 storage cannot run now.
	SecureBlockedReason string `json:"secureBlockedReason,omitempty"`
}

// S3LeftoverKey is a SeaweedFS user of a project with no pipeline server.
type S3LeftoverKey struct {
	User    string `json:"user"`
	Project string `json:"project"`
	// ConnectionInUse is true when the project's Connection
	// rhoai-nightly-updater-s3 still holds one of the user's keys (or that
	// cannot be read): workbenches and model deployments using it lose S3
	// access when the key is removed.
	ConnectionInUse bool `json:"connectionInUse"`
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
	// Unrecorded are those of Repoint whose UID this tool has no record of
	// (set up before 3.0.1, or labelled by a project editor): Migrate
	// storage writes the new storage's keys into their project only when
	// the user confirms the project (MigrateStorageRequest.ConfirmProjects).
	Unrecorded []string `json:"unrecorded,omitempty"`
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
	// Leftovers (the S3 storage) are this tool's storage objects still in
	// its namespace once the Deployment is gone, as "Kind name": what a
	// teardown that did not finish left. Delete is offered for them.
	Leftovers []string `json:"leftovers,omitempty"`
	// Unrecorded (a pipeline server) is true when it carries this tool's
	// label but this tool has no record of its UID (set up before 3.0.1, or
	// labelled by someone else, which a project editor can do): Set up again
	// adopts it, and Migrate storage moves it only when its project is
	// confirmed.
	Unrecorded bool `json:"unrecorded,omitempty"`
	// UID (an unrecorded pipeline server) is the object's UID: what the
	// user's confirmation names (PipelineServerRequest.ConfirmUnrecorded).
	UID string `json:"uid,omitempty"`
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
	// ConnectionWorkbenches are the workbenches of the project whose
	// opendatahub.io/connections annotation names that Connection, read
	// with the user's token along with ConnectionState (present only);
	// deleting the pipeline server removes it from them first.
	// ConnectionWorkbenchesMessage says why they could not be read.
	ConnectionWorkbenches        []ConnectionWorkbench `json:"connectionWorkbenches,omitempty"`
	ConnectionWorkbenchesMessage string                `json:"connectionWorkbenchesMessage,omitempty"`
	// ConnectionEnvOnlyWorkbenches load that Connection through envFrom
	// without naming it in the annotation (attached the RHOAI 2.x way):
	// nothing takes it off them, and they fail to start once it is
	// deleted.
	ConnectionEnvOnlyWorkbenches []string `json:"connectionEnvOnlyWorkbenches,omitempty"`
}

// KeptPVC is a PersistentVolumeClaim and its size.
type KeptPVC struct {
	Name string `json:"name"`
	Size string `json:"size,omitempty"`
}

// ConnectionWorkbench is a workbench (Notebook) that uses the tool's
// dashboard Connection: a confirmation names it with the UID the user
// reviewed, so one deleted and made again since under the same name is not
// confirmed.
type ConnectionWorkbench struct {
	Name string `json:"name"`
	UID  string `json:"uid"`
	// Running: the workbench is not stopped. Taking the Connection off it
	// changes its pod template, which restarts its pod (work not saved to
	// disk is lost).
	Running bool `json:"running,omitempty"`
}

// PipelineServerRequest is the JSON body for pipeline server setup/teardown.
type PipelineServerRequest struct {
	Project string `json:"project"`
	// Recreate (setup only) replaces the tool's pipeline server named
	// nightly-dspa with one named dspa.
	Recreate bool `json:"recreate,omitempty"`
	// ConfirmUnrecorded (teardown and Recreate) is the UID of the pipeline
	// server the user confirmed as this tool's although this tool has no
	// record of it (ResourceState.Unrecorded and UID). Only a server with
	// that UID is deleted without the record; another one (made again
	// since) is not.
	ConfirmUnrecorded string `json:"confirmUnrecorded,omitempty"`
	// ConfirmWorkbenches (teardown and Recreate) maps each workbench of
	// the project the user confirmed removing the tool's dashboard
	// Connection from to the UID they reviewed
	// (ResourceState.ConnectionWorkbenches): a workbench that uses it and
	// is not named with its current UID refuses the request before
	// anything changes.
	ConfirmWorkbenches map[string]string `json:"confirmWorkbenches,omitempty"`
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

// --- Models routing (GET /api/resources/models/exposures) -------------------

// ModelExposures are the ways a model of the catalog can be reached from
// a project, each with whether it can be used now, and the one this tool
// recommends.
type ModelExposures struct {
	Project string `json:"project"`
	Model   string `json:"model"`
	// Default is the Key of the recommended option ("" when none fits:
	// RecommendGateway then says to set up the inference gateway).
	Default string `json:"default"`
	// DefaultReason says, in plain words, why that option is recommended.
	DefaultReason string                `json:"defaultReason,omitempty"`
	Options       []ModelExposureOption `json:"options"`
	// KServeDefaultGateway is the gateway KServe routes a model through by
	// default ("namespace/name", from inferenceservice-config), or "".
	KServeDefaultGateway string `json:"kserveDefaultGateway,omitempty"`
	// AuthReady: Red Hat Connectivity Link's AuthPolicy API is served and
	// its Kuadrant CR exists, so calls through a gateway are authenticated.
	AuthReady bool `json:"authReady"`
	// RecommendGateway: setting up this tool's inference gateway (the
	// Models tab's own part) is the way to reach the model from outside
	// the cluster; RecommendGatewayReason says why.
	RecommendGateway       bool   `json:"recommendGateway,omitempty"`
	RecommendGatewayReason string `json:"recommendGatewayReason,omitempty"`
}

// ModelExposureOption is one way to reach the model.
type ModelExposureOption struct {
	// Key identifies the option: "default-gateway", "maas", "internal",
	// or "gateway:<namespace>/<name>".
	Key string `json:"key"`
	// Exposure and GatewayRef are what a setup request sends for it.
	Exposure   string `json:"exposure"`
	GatewayRef string `json:"gatewayRef,omitempty"`
	Title      string `json:"title"`
	// Description says what it means for the user, in plain words.
	Description string `json:"description"`
	// Available: it can be chosen (once NeedsAuth is set up, when set);
	// Reason says why not.
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	// Fix names where the user makes an unavailable option available:
	// "inference-gateway" (set up the inference gateway on the Models tab)
	// or "maas-tab" (set up MaaS on its tab); "" none.
	Fix string `json:"fix,omitempty"`
	// NeedsAuth: calls are authenticated by Connectivity Link, which is not
	// set up yet: Set up on the tab installs it first.
	NeedsAuth bool `json:"needsAuth,omitempty"`
	// Warning is something the user should know before choosing it.
	Warning string `json:"warning,omitempty"`
	// Changes are what choosing it changes besides the model (a project
	// added to a gateway's list, the model published to MaaS).
	Changes []string `json:"changes,omitempty"`
	// Address is where the model will answer ("" when unknown).
	Address string `json:"address,omitempty"`
}

// ModelCallCheck is the result of one "Can it be called?" check: a
// minimal request through the model's route, with the model's own token.
type ModelCallCheck struct {
	// Result is "ok" (it answered), "model-name" (404 from the model's
	// server: the request's model name is not the served one), "route"
	// (404 without a body: nothing answers on that path), "auth" (401),
	// "forbidden" (403), "gateway-error" (500 from a gateway, empty or
	// Connectivity Link's own "Internal Server Error.": its authorization
	// failed, not the model), "server-error" (5xx), "unreachable" (no answer),
	// "maas" (reached through MaaS with an API key, which this tool does
	// not create: MaaS's own readiness is reported), "not-checked" (this
	// app cannot call it; Message says why), or "unexpected".
	Result string `json:"result"`
	OK     bool   `json:"ok"`
	// Status is the HTTP status (0 without an answer).
	Status int `json:"status,omitempty"`
	// Message says what happened and what to do, in plain words.
	Message string `json:"message"`
	// URL is the address that was called (no token in it).
	URL string `json:"url,omitempty"`
	// At is when it ran (RFC 3339).
	At string `json:"at"`
}
