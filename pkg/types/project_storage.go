package types

// ProjectStorageRequest is the body of POST /api/resources/project-s3/stream:
// set up, repair or delete the S3 storage of one data science project.
type ProjectStorageRequest struct {
	Project string `json:"project"`
	// Action is setup, repair or delete.
	Action string `json:"action"`
	// DeleteData, with delete, is the name of the storage's volume
	// (nightly-s3-data), typed by the user: the volume and its data go too.
	DeleteData string `json:"deleteData,omitempty"`
}

// ProjectStorageObject is one object of a project's S3 storage.
type ProjectStorageObject struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Present bool   `json:"present"`
	// Owned: its UID is the one this tool recorded.
	Owned bool `json:"owned"`
	// Problem says why it could not be read ("" when it was).
	Problem string `json:"problem,omitempty"`
}

// ProjectStorageStatus is the S3 storage of one data science project
// (GET /api/resources/project-s3?project=), read with the user's token.
type ProjectStorageStatus struct {
	Project string `json:"project"`
	// Name is the storage's Deployment and Service.
	Name string `json:"name"`
	// Endpoint is its in-cluster S3 address, Bucket its one bucket.
	Endpoint string `json:"endpoint"`
	Bucket   string `json:"bucket"`
	// DataVolume is the PersistentVolumeClaim that holds its data (a
	// delete removes it only when its name is typed).
	DataVolume string `json:"dataVolume"`
	// Deployed: one of its objects exists.
	Deployed bool `json:"deployed"`
	// ManagedByTool: every object present is the one this tool recorded.
	ManagedByTool bool `json:"managedByTool"`
	// Recorded: this tool has a record of a storage in the project.
	Recorded    bool   `json:"recorded"`
	Ready       bool   `json:"ready"`
	Terminating bool   `json:"terminating"`
	Message     string `json:"message"`
	// Missing are objects of a storage that are gone (Repair makes them).
	Missing []string `json:"missing,omitempty"`
	// NetworkPolicyProblem says how its NetworkPolicy differs from what
	// setup applies (Repair applies it again).
	NetworkPolicyProblem string                 `json:"networkPolicyProblem,omitempty"`
	Objects              []ProjectStorageObject `json:"objects"`
	// UsedBy are the pipeline servers of the project that use it (a delete
	// is refused while there are any); UsersProblem says why they could
	// not be read.
	UsedBy       []string `json:"usedBy,omitempty"`
	UsersProblem string   `json:"usersProblem,omitempty"`
	// Problem (ProblemCode) says why the record could not be read.
	Problem     string `json:"problem,omitempty"`
	ProblemCode string `json:"problemCode,omitempty"`
}

// ProjectPipelineServerRequest is the body of POST
// /api/resources/project-pipeline-server/stream: set up or delete the
// pipeline server of one data science project on the project's own S3
// storage.
type ProjectPipelineServerRequest struct {
	Project string `json:"project"`
	// Action is setup or teardown.
	Action string `json:"action"`
	// ConfirmWorkbenches, with teardown: the workbenches (name to the UID
	// reviewed) the user confirmed taking the pipeline server's dashboard
	// Connection off.
	ConfirmWorkbenches map[string]string `json:"confirmWorkbenches,omitempty"`
}

// RecordedProjectStorage is a project S3 storage in this tool's record:
// its project, who set it up, and its volume.
type RecordedProjectStorage struct {
	Project    string `json:"project"`
	By         string `json:"by,omitempty"`
	DataVolume string `json:"dataVolume"`
}
