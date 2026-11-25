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

// HandleMLflowSamplesStatus reports the MLflow samples of one of the
// user's projects, read with their own token (tier user-token;
// cluster.GetMLflowSamplesStatus: named reads only, cached briefly per
// user).
var HandleMLflowSamplesStatus = withUserRead(func(sa, u *cluster.Client, w http.ResponseWriter, r *http.Request) {
	project, ok := sampleProject(w, r)
	if !ok {
		return
	}
	st, err := getMLflowSamplesStatus(sa, u, project)
	if errors.Is(err, cluster.ErrNotDSProject) {
		writeError(w, "Cannot read the MLflow samples: "+err.Error()+".", http.StatusForbidden, cluster.ErrorCodeForbiddenForUser)
		return
	}
	if err != nil {
		writeUserReadError(w, "Cannot read the MLflow samples", err)
		return
	}
	writeJSON(w, st, "mlflow-samples")
})

// getMLflowSamplesStatus reads the samples status; tests replace it.
var getMLflowSamplesStatus = cluster.GetMLflowSamplesStatus

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
var HandleMLflowSamplesStream = withProjectMutationAuth(projectActionRoutes["/api/resources/samples/mlflow/stream"], func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
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
	if !beginProject(w, fmt.Sprintf("%s %s in %s", req.Action, strings.Join(req.Samples, ", "), req.Project)) {
		return
	}
	slog.Info("mutation", "op", "mlflow-samples", "action", req.Action, "samples", req.Samples, "project", req.Project)
	_, runErr := streamOperation(w, "mlflow-samples", func(emit func(cluster.UpdateStepEvent)) (*types.OperationResponse, error) {
		return runMLflowSamples(c, req, emit)
	})
	if runErr != nil {
		slog.Error("mlflow-samples failed", "error", runErr)
	}
})
