package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// HandleS3StorageSetup deploys the S3 storage (SeaweedFS in namespace seaweedfs) with a bucket for
// pipeline artifacts. The route and the operation kind keep their names from earlier versions.
var HandleS3StorageSetup = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	beginOperation(w, "")
	slog.Info("mutation", "op", "setup-minio")
	result, err := cluster.SetupS3Storage(c)
	if err != nil {
		slog.Error("setup-minio failed", "error", err)
		writeError(w, "S3 storage setup failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "setup-minio")
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
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("panic in migrate-storage handler", "error", rec)
		}
	}()

	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req types.MigrateStorageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeError(w, "invalid request body", http.StatusBadRequest, "validation")
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

	sseWriter, err := NewSSEWriter(w)
	if err != nil {
		writeError(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	defer sseWriter.Close()

	done := make(chan struct{})
	defer close(done)
	go sseHeartbeat(sseWriter, done)

	target := "migrate"
	if req.RemoveOldStorage {
		target = "remove old storage"
		if req.DeleteNamespace {
			target += " and namespace minio"
		}
	}
	beginOperation(w, target)
	slog.Info("mutation", "op", "migrate-storage", "target", target, "confirmProjects", req.ConfirmProjects)

	result, migrateErr := runMigrateStorage(c, cluster.MigrateOptions{RemoveOldStorage: req.RemoveOldStorage, DeleteNamespace: req.DeleteNamespace, ConfirmProjects: req.ConfirmProjects}, forwardSteps(sseWriter))
	sendOperationResult(sseWriter, result, migrateErr)
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
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req types.S3TeardownRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeError(w, "invalid request body", http.StatusBadRequest, "validation")
		return
	}
	beginOperation(w, "")
	slog.Info("mutation", "op", "teardown-minio", "deleteNamespace", req.DeleteNamespace)
	result, err := cluster.TeardownS3Storage(c, req)
	if err != nil {
		slog.Error("teardown-minio failed", "error", err)
		writeError(w, "S3 storage teardown failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "teardown-minio")
})

// HandlePipelineServerSetup creates a pipeline server (DSPA) in the specified project.
var HandlePipelineServerSetup = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req types.PipelineServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Project == "" {
		writeError(w, "project is required", http.StatusBadRequest, "validation")
		return
	}
	if !namespaceRegex.MatchString(req.Project) {
		writeError(w, "invalid project name", http.StatusBadRequest, "validation")
		return
	}
	beginOperation(w, req.Project)
	slog.Info("mutation", "op", "setup-pipeline-server", "project", req.Project, "recreate", req.Recreate)
	setup := cluster.SetupPipelineServer
	if req.Recreate {
		setup = cluster.RecreatePipelineServer
	}
	result, err := setup(c, req.Project)
	if err != nil {
		slog.Error("setup-pipeline-server failed", "error", err)
		writeError(w, "pipeline server setup failed", http.StatusInternalServerError)
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
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req types.PipelineServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Project == "" {
		writeError(w, "project is required", http.StatusBadRequest, "validation")
		return
	}
	if !namespaceRegex.MatchString(req.Project) {
		writeError(w, "invalid project name", http.StatusBadRequest, "validation")
		return
	}
	beginOperation(w, req.Project)
	slog.Info("mutation", "op", "teardown-pipeline-server", "project", req.Project)
	result, err := cluster.TeardownPipelineServer(c, req.Project)
	if err != nil {
		slog.Error("teardown-pipeline-server failed", "error", err)
		writeError(w, "pipeline server teardown failed", http.StatusInternalServerError)
		return
	}
	writeOperationResult(w, result, "teardown-pipeline-server")
})
