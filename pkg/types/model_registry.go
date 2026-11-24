package types

// ModelRegistryStatus describes the Model registry tab on the Environment page
// (GET /api/resources/model-registry): this application's registry, retrieved
// with the authenticated user's bearer token.
type ModelRegistryStatus struct {
	// Name identifies the registry this application provisions (nightly-registry).
	Name string `json:"name"`
	// Namespace is the Kubernetes namespace RHOAI uses for model registries (the
	// DataScienceCluster's instancesNamespace or registriesNamespace); an empty
	// string until the component publishes one.
	Namespace string `json:"namespace,omitempty"`
	// Component is the DataScienceCluster sub-component that operates model
	// registries here ("aiHub" or "modelregistry"); empty when undetermined.
	Component string `json:"component,omitempty"`
	// Blocked carries the reason registry creation must wait for the component
	// to be enabled; nil if nothing prevents it.
	Blocked *ComponentBlock `json:"blocked,omitempty"`
	// State takes one of "absent", "foreign" (same name but not owned by this
	// tool), "terminating", "starting", "ready", or "unknown" (status
	// unreadable—Message explains why).
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
	// ManagedByTool: true when this tool created the registry (UID matches the
	// stored record and the tool label is present).
	ManagedByTool bool `json:"managedByTool,omitempty"`
	// Foreign: a registry of that name exists that this tool did not
	// create; State is still its own lifecycle state.
	Foreign bool `json:"foreign,omitempty"`
	// Address is the REST endpoint for the registry via the cluster gateway
	// (https://<routing.opendatahub.io/external-address-rest>); empty if the
	// operator exposes none.
	Address string `json:"address,omitempty"`
	// AddressProblem says why the Service's address is not used (the user's
	// token is sent only to the cluster's own gateway, at the registry's
	// path, through a Service the registry owns); "" when it is used or
	// there is none.
	AddressProblem string `json:"addressProblem,omitempty"`
	// DashboardURL is the RHOAI dashboard base URL used when building links.
	DashboardURL string `json:"dashboardUrl,omitempty"`
	// Sample reflects the sample model in the registry; nil when the registry
	// is not ready or no gateway address is available.
	Sample *ModelRegistrySample `json:"sample,omitempty"`
}

// ModelRegistrySample records the sample model registered by this application.
type ModelRegistrySample struct {
	// Model is the registered model's name; Version is its version's name.
	Model   string `json:"model"`
	Version string `json:"version"`
	// Registered means the model is present in the registry; State is
	// "LIVE" or "ARCHIVED".
	// Complete: the model, its version and the version's artifact all
	// exist and are LIVE.
	Complete   bool   `json:"complete,omitempty"`
	Registered bool   `json:"registered"`
	State      string `json:"state,omitempty"`
	// ID is the registered model identifier (for dashboard links).
	ID string `json:"id,omitempty"`
	// URI points to storage for the sample's model weights.
	URI string `json:"uri"`
	// Problem describes why the sample could not be fetched ("" on success).
	Problem string `json:"problem,omitempty"`
}

// ModelRegistryRequest is the JSON body sent to POST
// /api/resources/model-registry/stream.
type ModelRegistryRequest struct {
	// Action must be "setup" or "teardown".
	Action string `json:"action"`
	// Samples: during setup, also register the sample model when the registry
	// exposes a gateway address.
	Samples bool `json:"samples,omitempty"`
}
