package types

// ProjectFacts are the cluster facts a project action reads with the
// updater's ServiceAccount, through its read-only facts client
// (cluster.Client.Facts): what a project user's own token may not read but
// is no user data and names no other project (SECURITY.md, "Cluster facts
// of a change"). A fixed type: a field added here is a reviewed change. A
// fact that would list other tenants is only ever added reduced to an
// answer for the requested project. Problems are fixed codes, never the
// text of a read.
type ProjectFacts struct {
	// ApplicationsNamespace is RHOAI's applications namespace, as its
	// DSCInitialization names it; "" when RHOAI has none (not installed)
	// or it could not be read (Problems).
	ApplicationsNamespace string `json:"applicationsNamespace"`
	// DSC is the DataScienceCluster's state (ProjectFactsDSC...), "" when
	// it could not be read (Problems).
	DSC string `json:"dsc"`
	// Components are the DataScienceCluster's components, as the summary
	// gives them: managementState and whether the operator reports the
	// component ready.
	Components []SummaryComponent `json:"components"`
	// ControlPlane is whether the cluster's control plane is hosted (ROSA
	// HCP, HyperShift): ProjectFactsControlPlaneHosted or
	// ProjectFactsControlPlaneNotHosted, "" when unknown (Problems).
	ControlPlane string `json:"controlPlane"`
	// Problems are the facts that could not be read
	// (ProjectFactsProblem...).
	Problems []string `json:"problems"`
}

// ProjectFacts.DSC values.
const (
	ProjectFactsDSCPresent      = "present"
	ProjectFactsDSCNone         = "none"          // its API is served, no DataScienceCluster exists
	ProjectFactsDSCNotInstalled = "not_installed" // no DataScienceCluster API
)

// ProjectFacts.ControlPlane values.
const (
	ProjectFactsControlPlaneHosted    = "hosted"
	ProjectFactsControlPlaneNotHosted = "not_hosted"
)

// ProjectFacts.Problems codes.
const (
	ProjectFactsProblemApplicationsNamespace = "applications_namespace_unreadable"
	ProjectFactsProblemDSC                   = "dsc_unreadable"
	ProjectFactsProblemControlPlane          = "control_plane_unknown"
)
