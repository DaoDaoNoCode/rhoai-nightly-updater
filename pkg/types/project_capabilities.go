package types

// ProjectCapabilities is GET /api/resources/projects/capabilities: what
// the signed-in user may do in one of their data science projects (the
// project actions). Advisory: the gate of each action checks again, and
// the API server decides every request.
type ProjectCapabilities struct {
	Project string `json:"project"`
	// Role is "admin" (a cluster-admin: every action, no review) or
	// "user".
	Role string `json:"role"`
	// Actions are the project actions by ID ("workbench.setup", ...).
	Actions map[string]ProjectCapability `json:"actions"`
}

// ProjectCapability is one project action's answer.
type ProjectCapability struct {
	Label string `json:"label"`
	// Allowed: the user may run it now (their RBAC allows every request,
	// and nothing on the cluster blocks it).
	Allowed bool `json:"allowed"`
	// Missing are the requests the user's RBAC does not allow, each with
	// who can grant it.
	Missing []string `json:"missing"`
	// BlockedBy are the prerequisite codes that block it on this cluster
	// ("prerequisites", "prerequisites_unknown").
	BlockedBy []string `json:"blockedBy"`
	// Reason says why it is not allowed, for people; "" when it is.
	Reason string `json:"reason,omitempty"`
	// Code is the reason's errorCode ("admin_only",
	// "missing_permissions", or a BlockedBy code).
	Code string `json:"code,omitempty"`
}
