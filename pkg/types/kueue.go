package types

// KueueStatus is the payload for the Environment page's Distributed workloads
// tab (GET /api/resources/kueue?project=): cluster Kueue, queues and project
// label managed here, sample workloads, dashboard opt-in — all fetched with the
// authenticated user's token.
type KueueStatus struct {
	// Project names the project where this tool's setup lives (per its record),
	// or else the project being queried; empty when neither applies.
	Project string `json:"project,omitempty"`
	// SetUpIn appears when setup runs in a different project than the one
	// queried (otherwise empty): at most one setup per cluster.
	SetUpIn string `json:"setUpIn,omitempty"`
	// Blocked means the DataScienceCluster kueue component is not Unmanaged (UI
	// prerequisites above the tab must enable it first).
	Blocked *ComponentBlock `json:"blocked,omitempty"`
	// RayBlocked means the ray component that backs the samples is disabled.
	RayBlocked *ComponentBlock `json:"rayBlocked,omitempty"`
	// Instance holds RHOAI's Kueue CR as observed on the cluster.
	Instance KueueInstance `json:"instance"`
	// Refusal explains why Set up cannot run (fail closed: v1beta2 absent,
	// labelPolicy None, a failed read, and so on); empty when setup is allowed.
	Refusal     string `json:"refusal,omitempty"`
	RefusalCode string `json:"refusalCode,omitempty"`
	// Objects lists ResourceFlavor, ClusterQueue, and LocalQueue in that
	// sequence, each with status even when the object is missing.
	Objects []KueueObject `json:"objects"`
	// Quota summarizes the ClusterQueue limits (e.g. "6 CPUs and 19Gi of memory").
	Quota string `json:"quota"`
	// Label reflects the project's kueue.openshift.io/managed label.
	Label *KueueLabel `json:"label,omitempty"`
	// DefaultQueue is true when the project has a LocalQueue named "default"
	// (after labelling, Kueue routes new pod-producing objects there).
	DefaultQueue bool `json:"defaultQueue,omitempty"`
	// Samples lists RayCluster then RayJob, in that order.
	Samples []KueueSample `json:"samples"`
	// OptIn covers the dashboard Kueue toggle and the Queue hardware profile.
	OptIn KueueOptIn `json:"optIn"`
	// DefaultRayImage is the prefilled image for sample workloads.
	DefaultRayImage string `json:"defaultRayImage"`
	// ItemsProblem names anything that failed to load; empty when reads succeeded.
	ItemsProblem string `json:"itemsProblem,omitempty"`
	// DashboardURL is the RHOAI dashboard base URL for hyperlinking.
	DashboardURL string `json:"dashboardUrl,omitempty"`
}

// KueueInstance captures the cluster Kueue CR (kueue.openshift.io/v1) plus the
// DataScienceCluster KueueReady condition.
type KueueInstance struct {
	Found     bool   `json:"found"`
	Available bool   `json:"available"`
	Message   string `json:"message,omitempty"`
	// LabelPolicy mirrors spec.config.workloadManagement.labelPolicy (empty if unset).
	LabelPolicy string `json:"labelPolicy,omitempty"`
	// KueueReady mirrors the DataScienceCluster KueueReady condition ("True",
	// "False", or empty when not reported).
	KueueReady string `json:"kueueReady,omitempty"`
	// V1beta2 indicates whether the apiserver serves kueue.x-k8s.io/v1beta2.
	V1beta2 bool `json:"v1beta2"`
	// Ready is true when KueueReady is True and the Kueue CR is Available.
	Ready bool `json:"ready"`
}

// KueueObject represents one queue-related object owned or tracked by the tool.
type KueueObject struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Present   bool   `json:"present"`
	// ManagedByTool marks objects this tool labelled and recorded by UID.
	ManagedByTool bool `json:"managedByTool,omitempty"`
	// Foreign marks a same-named object the tool did not create.
	Foreign bool `json:"foreign,omitempty"`
	Active  bool `json:"active,omitempty"`
	// Message carries the Active condition message when the object is inactive.
	Message     string `json:"message,omitempty"`
	Terminating bool   `json:"terminating,omitempty"`
}

// KueueLabel describes the project's kueue.openshift.io/managed label.
type KueueLabel struct {
	Present bool `json:"present"`
	// AddedByTool is set when this tool applied the label (Clean up strips it).
	AddedByTool bool `json:"addedByTool,omitempty"`
}

// KueueSample is either a RayCluster or RayJob sample workload.
type KueueSample struct {
	// Kind is either "raycluster" or "rayjob".
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	Present       bool   `json:"present"`
	ManagedByTool bool   `json:"managedByTool,omitempty"`
	Foreign       bool   `json:"foreign,omitempty"`
	Terminating   bool   `json:"terminating,omitempty"`
	Image         string `json:"image,omitempty"`
	// State is the Kueue Workload phase as the dashboard maps it (Queued,
	// Inadmissible, Admitted, Running, Complete, Failed, Evicted, Preempted);
	// empty until a Workload exists.
	State   string `json:"state,omitempty"`
	Message string `json:"message,omitempty"`
	// RayState is the Ray resource's native status (RayCluster status.state or
	// RayJob status.jobDeploymentStatus).
	RayState string `json:"rayState,omitempty"`
}

// KueueOptIn holds the dashboard Kueue switch and the tool's Queue profile.
type KueueOptIn struct {
	// DisableKueue is the value of odh-dashboard-config
	// spec.dashboardConfig.disableKueue (nil means unset; the dashboard treats that as true).
	DisableKueue *bool `json:"disableKueue,omitempty"`
	// SetByTool means this tool set the flag to false and still controls it.
	SetByTool bool `json:"setByTool,omitempty"`
	// Previous records the value before this tool's opt-in ("unset" or "true").
	Previous string `json:"previous,omitempty"`
	// Profile is the Queue hardware profile object.
	Profile KueueObject `json:"profile"`
	// On is true when this tool performed the opt-in per the stored record.
	On bool `json:"on,omitempty"`
}

// KueueRequest is the JSON body for POST /api/resources/kueue/stream.
type KueueRequest struct {
	// Action is one of "setup", "teardown", "delete-sample", "optin", or "optout".
	Action  string `json:"action"`
	Project string `json:"project,omitempty"`
	// RayCluster and RayJob select which samples setup creates; Sample names the
	// target for delete-sample ("raycluster" or "rayjob").
	RayCluster bool   `json:"rayCluster,omitempty"`
	RayJob     bool   `json:"rayJob,omitempty"`
	RayImage   string `json:"rayImage,omitempty"`
	Sample     string `json:"sample,omitempty"`
}
