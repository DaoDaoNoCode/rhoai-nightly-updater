package types

// ObservabilityStatus is the Observability tab (GET
// /api/resources/observability): RHOAI's monitoring (the DSCInitialization's
// spec.monitoring and the Monitoring CR) with the usage logs MaaS writes and
// the dashboards that show them, part by part.
type ObservabilityStatus struct {
	// Blocked says why it cannot be set up here (no RHOAI, no
	// DSCInitialization, monitoring turned off), or is nil.
	Blocked *ComponentBlock `json:"blocked,omitempty"`
	// Namespace is RHOAI's monitoring namespace, as the DSCInitialization
	// names it (spec.monitoring.namespace).
	Namespace string `json:"namespace,omitempty"`
	// Configured: the DSCInitialization configures usage logs
	// (spec.monitoring.usageLogs), whoever did. ManagedByTool: this tool's
	// setup added fields of spec.monitoring (its annotation on the
	// DSCInitialization says which). External: usage logs are configured,
	// not by this tool: shown as they are, never changed.
	Configured    bool `json:"configured"`
	ManagedByTool bool `json:"managedByTool"`
	External      bool `json:"external,omitempty"`
	// Ready: every part is ready.
	Ready bool `json:"ready"`
	// Parts are the operators, the usage-logs storage, the DSCInitialization
	// fields, the monitoring stack and the dashboards, in setup order.
	Parts []MaaSPart `json:"parts"`
	// SetupPlan lists every change Set up would make now, in order; empty
	// when it has nothing to do. SetupBlockedReason says why it cannot run.
	SetupPlan          []string `json:"setupPlan,omitempty"`
	SetupBlockedReason string   `json:"setupBlockedReason,omitempty"`
	// CleanupPlan lists what Clean up changes and deletes, CleanupKept what
	// it leaves in place; CleanupBlockedReason why it cannot run now.
	CleanupPlan          []string `json:"cleanupPlan,omitempty"`
	CleanupKept          []string `json:"cleanupKept,omitempty"`
	CleanupBlockedReason string   `json:"cleanupBlockedReason,omitempty"`
	// Volumes are the PersistentVolumeClaims in the monitoring namespace
	// (the disks the stack uses), and VolumesTotal their requested size.
	Volumes      []ObservabilityVolume `json:"volumes,omitempty"`
	VolumesTotal string                `json:"volumesTotal,omitempty"`
	// Conditions are the Monitoring CR's conditions, as read.
	Conditions []ObservabilityCondition `json:"conditions,omitempty"`
	// DashboardURL is the RHOAI dashboard's address (Observe & monitor).
	DashboardURL string `json:"dashboardUrl,omitempty"`
}

// ObservabilityVolume is one PersistentVolumeClaim of the monitoring
// namespace.
type ObservabilityVolume struct {
	Name         string `json:"name"`
	Size         string `json:"size"`
	StorageClass string `json:"storageClass,omitempty"`
	// SetUpByTool: this tool's setup caused it (its record on the
	// DSCInitialization names it): Clean up deletes it.
	SetUpByTool bool `json:"setUpByTool,omitempty"`
}

// ObservabilityCondition is one condition of the Monitoring CR.
type ObservabilityCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// ObservabilityRequest is POST /api/resources/observability/stream: Action
// "setup" or "teardown".
type ObservabilityRequest struct {
	Action string `json:"action"`
}
