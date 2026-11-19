package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// sampleProject reads and validates the ?project= of a samples status read.
func sampleProject(w http.ResponseWriter, r *http.Request) (string, bool) {
	project := r.URL.Query().Get("project")
	if !namespaceRegex.MatchString(project) {
		writeError(w, "a valid project is required", http.StatusBadRequest, "validation")
		return "", false
	}
	return project, true
}

// HandleMLflowSamplesStatus reports the MLflow samples of one project
// (cluster.GetMLflowSamplesStatus: named reads only, cached briefly).
var HandleMLflowSamplesStatus = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	project, ok := sampleProject(w, r)
	if !ok {
		return
	}
	st, err := cluster.GetMLflowSamplesStatus(c, project)
	if errors.Is(err, cluster.ErrNotDSProject) {
		writeError(w, "Cannot read the MLflow samples: "+err.Error(), http.StatusBadRequest, "validation")
		return
	}
	if err != nil {
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "Cannot read the MLflow samples: "+err.Error(), code, errorCode)
		return
	}
	writeJSON(w, st, "mlflow-samples")
})

// HandlePipelineSampleSpec serves the sample pipeline's file. It makes no
// cluster request: the user uploads and runs it with their own token.
var HandlePipelineSampleSpec = withAuth(func(_ *cluster.Client, w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, cluster.PipelineSampleSpec(), "pipeline-sample")
})

// runMLflowSamples runs a samples operation; tests replace it.
var runMLflowSamples = cluster.RunMLflowSamples

// HandleMLflowSamplesStream adds, removes or regenerates MLflow samples in
// one project, streaming a step per sample. The samples are MLflow API
// calls, not Kubernetes writes, so there is no dry run.
var HandleMLflowSamplesStream = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	var req types.MLflowSamplesRequest
	if !decodeBody(w, r, 4096, &req, false) {
		return
	}
	if !namespaceRegex.MatchString(req.Project) {
		writeError(w, "a valid project is required", http.StatusBadRequest, "validation")
		return
	}
	if problem := cluster.ValidateMLflowSamplesRequest(req); problem != "" {
		writeError(w, problem, http.StatusBadRequest, "validation")
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("panic in mlflow-samples handler", "error", rec)
		}
	}()
	sseWriter, err := NewSSEWriter(w)
	if err != nil {
		writeError(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	defer sseWriter.Close()
	done := make(chan struct{})
	defer close(done)
	go sseHeartbeat(sseWriter, done)

	beginOperation(w, fmt.Sprintf("%s %s in %s", req.Action, strings.Join(req.Samples, ", "), req.Project))
	slog.Info("mutation", "op", "mlflow-samples", "action", req.Action, "samples", req.Samples, "project", req.Project)
	result, runErr := runMLflowSamples(c, req, forwardSteps(sseWriter))
	sendOperationResult(sseWriter, result, runErr)
	if runErr != nil {
		slog.Error("mlflow-samples failed", "error", runErr)
	}
})
