package api

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// HandleS3StorageSetup deploys the S3 storage (SeaweedFS in namespace seaweedfs) with a bucket for
// pipeline artifacts.
var HandleS3StorageSetup = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	beginOperation(w, "")
	slog.Info("mutation", "op", "setup-s3")
	result, err := cluster.SetupS3Storage(c)
	if err != nil {
		slog.Error("setup-s3 failed", "error", err)
		writeOperationError(w, "S3 storage setup failed", err)
		return
	}
	writeOperationResult(w, result, "setup-s3")
})

// runSecureStorage runs Secure S3 storage; tests replace it.
var runSecureStorage = cluster.SecureS3Storage

// HandleS3StorageSecureStream runs Secure S3 storage: a new admin key with
// IAM writes on, then a key of its own for every pipeline server this tool
// recorded (storage an earlier version set up gave its admin key to every
// pipeline project), streaming each step via SSE: "check", "rollout", then
// "server:<namespace>/<name>" per pipeline server.
var HandleS3StorageSecureStream = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	var req struct {
		// RemoveLeftoverKeys are the leftover keys the confirmation listed.
		RemoveLeftoverKeys []string `json:"removeLeftoverKeys,omitempty"`
	}
	if !decodeBody(w, r, 16384, &req, true) {
		return
	}
	if !validLeftoverUsers(w, req.RemoveLeftoverKeys) {
		return
	}
	beginOperation(w, "")
	slog.Info("mutation", "op", "secure-s3", "removeLeftoverKeys", req.RemoveLeftoverKeys)
	_, err := streamOperation(w, "secure-s3", func(emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		return runSecureStorage(c, req.RemoveLeftoverKeys, emit)
	})
	if err != nil {
		slog.Error("secure-s3 failed", "error", err)
	}
})

// validLeftoverUsers checks the leftover keys a request names: at most 200
// SeaweedFS user names of this tool (rnu-dspa-*, IAM name characters).
func validLeftoverUsers(w http.ResponseWriter, users []string) bool {
	if len(users) > 200 {
		writeError(w, "at most 200 leftover keys", http.StatusBadRequest, "validation")
		return false
	}
	for _, u := range users {
		if !leftoverUserRegex.MatchString(u) {
			writeError(w, "invalid leftover key name", http.StatusBadRequest, "validation")
			return false
		}
	}
	return true
}

var leftoverUserRegex = regexp.MustCompile(`^rnu-dspa-[a-z0-9.-]{1,55}$`)

// runRemoveLeftoverKeys runs Remove leftover keys; tests replace it.
var runRemoveLeftoverKeys = cluster.RemoveLeftoverS3Keys

// HandleS3RemoveLeftoverKeys deletes the SeaweedFS users of pipeline
// servers that no longer exist (Remove leftover keys): only the confirmed
// ones that are still leftovers.
var HandleS3RemoveLeftoverKeys = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	var req struct {
		Users []string `json:"users"`
	}
	if !decodeBody(w, r, 16384, &req, false) {
		return
	}
	if len(req.Users) == 0 {
		writeError(w, "users is required", http.StatusBadRequest, "validation")
		return
	}
	if !validLeftoverUsers(w, req.Users) {
		return
	}
	beginOperation(w, "")
	slog.Info("mutation", "op", "remove-leftover-s3-keys", "users", req.Users)
	result, err := runRemoveLeftoverKeys(c, req.Users)
	if err != nil {
		slog.Error("remove-leftover-s3-keys failed", "error", err)
		writeOperationError(w, "Remove leftover keys failed", err)
		return
	}
	writeOperationResult(w, result, "remove-leftover-s3-keys")
})

// readS3AdminLogin reads the SeaweedFS admin UI login; tests replace it.
var readS3AdminLogin = cluster.S3AdminLogin

// HandleS3AdminPassword returns the S3 storage's credentials: the admin UI
// user and its own password, the S3 access key and the S3 secret key, for
// the Pipelines tab's connection details. They are read from Secret
// seaweedfs-s3-credentials on each request, answered only to a
// cluster-admin, never cached (Cache-Control: no-store) and never logged:
// the log line names the user, not the value.
var HandleS3AdminPassword = withAdminRead(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	login, err := readS3AdminLogin(c)
	var unavailable *cluster.S3AdminLoginUnavailable
	switch {
	case errors.As(err, &unavailable):
		writeError(w, unavailable.Reason, http.StatusConflict, "unavailable")
		return
	case err != nil:
		slog.Error("s3 admin password read failed", "error", err)
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "Cannot read the admin password: "+err.Error(), code, errorCode)
		return
	}
	slog.Info("credential read", "op", "s3-admin-password", "user", resolveUsername(r))
	writeJSON(w, login, "s3-admin-password")
})

// runMigrateStorage runs Migrate storage; tests replace it.
var runMigrateStorage = cluster.MigrateS3Storage

// HandleS3StorageMigrateStream moves the S3 storage an earlier version left
// in namespace minio to namespace seaweedfs (Migrate storage), or with
// removeOldStorage removes the old storage afterwards, streaming each step
// via SSE.
var HandleS3StorageMigrateStream = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	var req types.MigrateStorageRequest
	if !decodeBody(w, r, 4096, &req, true) {
		return
	}
	if req.DeleteNamespace && !req.RemoveOldStorage {
		writeError(w, "deleteNamespace needs removeOldStorage", http.StatusBadRequest, "validation")
		return
	}
	if len(req.ConfirmProjects) > 0 && req.RemoveOldStorage {
		writeError(w, "confirmProjects is for Migrate only", http.StatusBadRequest, "validation")
		return
	}
	if len(req.ConfirmProjects) > 200 {
		writeError(w, "at most 200 projects", http.StatusBadRequest, "validation")
		return
	}
	seen := map[string]bool{}
	for _, p := range req.ConfirmProjects {
		if !namespaceRegex.MatchString(p) || seen[p] {
			writeError(w, "invalid or repeated project in confirmProjects", http.StatusBadRequest, "validation")
			return
		}
		seen[p] = true
	}

	target := "migrate"
	if req.RemoveOldStorage {
		target = "remove old storage"
		if req.DeleteNamespace {
			target += " and namespace minio"
		}
	}
	beginOperation(w, target)
	slog.Info("mutation", "op", "migrate-storage", "target", target, "confirmProjects", req.ConfirmProjects)

	_, migrateErr := streamOperation(w, "migrate-storage", func(emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		return runMigrateStorage(c, cluster.MigrateOptions{RemoveOldStorage: req.RemoveOldStorage, DeleteNamespace: req.DeleteNamespace, ConfirmProjects: req.ConfirmProjects}, emit)
	})
	if migrateErr != nil {
		slog.Error("migrate-storage failed", "error", migrateErr)
	}
})

// HandleS3StorageTeardown deletes the S3 storage objects this tool created, including those an earlier
// version left in namespace minio; the namespaces are kept.
var HandleS3StorageTeardown = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	var req types.S3TeardownRequest
	if !decodeBody(w, r, 4096, &req, true) {
		return
	}
	beginOperation(w, "")
	slog.Info("mutation", "op", "teardown-s3", "deleteNamespace", req.DeleteNamespace)
	result, err := cluster.TeardownS3Storage(c, req)
	if err != nil {
		slog.Error("teardown-s3 failed", "error", err)
		writeOperationError(w, "S3 storage teardown failed", err)
		return
	}
	writeOperationResult(w, result, "teardown-s3")
})

// HandlePipelineServerSetup creates a pipeline server (DSPA) in the specified project.
var HandlePipelineServerSetup = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	var req types.PipelineServerRequest
	if err := readBody(w, r, 4096, &req, false); err != nil || req.Project == "" {
		writeError(w, "project is required", http.StatusBadRequest, "validation")
		return
	}
	if !namespaceRegex.MatchString(req.Project) {
		writeError(w, "invalid project name", http.StatusBadRequest, "validation")
		return
	}
	beginOperation(w, req.Project)
	slog.Info("mutation", "op", "setup-pipeline-server", "project", req.Project, "recreate", req.Recreate)
	var result *types.OperationResponse
	var err error
	if req.Recreate {
		result, err = cluster.RecreatePipelineServer(c, req.Project, req.ConfirmUnrecorded, req.ConfirmWorkbenches)
	} else {
		result, err = cluster.SetupPipelineServer(c, req.Project)
	}
	if err != nil {
		slog.Error("setup-pipeline-server failed", "error", err)
		writeOperationError(w, "pipeline server setup failed", err)
		return
	}
	writeOperationResult(w, result, "setup-pipeline-server")
})

// HandlePipelineServerTeardown removes the pipeline server from the specified project.
var HandlePipelineServerTeardown = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	var req types.PipelineServerRequest
	if err := readBody(w, r, 4096, &req, false); err != nil || req.Project == "" {
		writeError(w, "project is required", http.StatusBadRequest, "validation")
		return
	}
	if !namespaceRegex.MatchString(req.Project) {
		writeError(w, "invalid project name", http.StatusBadRequest, "validation")
		return
	}
	beginOperation(w, req.Project)
	slog.Info("mutation", "op", "teardown-pipeline-server", "project", req.Project, "confirmUnrecorded", req.ConfirmUnrecorded)
	result, err := cluster.TeardownPipelineServer(c, req.Project, req.ConfirmUnrecorded, req.ConfirmWorkbenches)
	if err != nil {
		slog.Error("teardown-pipeline-server failed", "error", err)
		writeOperationError(w, "pipeline server teardown failed", err)
		return
	}
	writeOperationResult(w, result, "teardown-pipeline-server")
})
