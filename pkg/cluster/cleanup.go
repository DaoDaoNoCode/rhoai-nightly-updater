package cluster

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The teardowns a clean-up runs; tests replace them.
var (
	cleanupPipelineServer = TeardownPipelineServer
	cleanupStorage        = TeardownS3Storage
	cleanupMLflow         = TeardownMLflow
	cleanupModels         = TeardownModels
	cleanupGateway        = TeardownInferenceGateway
)

// CleanupStepID is the step id of each part of a clean-up, as streamed:
// "pipeline-server:<project>", "storage", "mlflow", "model:<project>" and
// "inference-gateway".
func CleanupStepID(part, project string) string {
	if project != "" {
		return part + ":" + project
	}
	return part
}

type cleanupPart struct {
	id, title string
	run       func() (*types.OperationResponse, error)
}

// cleanupParts lists the parts of a clean-up in the order they run: the
// pipeline servers first (the storage teardown refuses while one uses the
// storage), then the storage, then MLflow, which depends on neither, then
// the models and, after them, the inference gateway they route through
// (its teardown refuses while a model uses it).
func cleanupParts(c *Client, req types.CleanupRequest) []cleanupPart {
	var parts []cleanupPart
	for _, project := range req.PipelineServers {
		parts = append(parts, cleanupPart{
			id:    CleanupStepID("pipeline-server", project),
			title: fmt.Sprintf("the pipeline server in '%s'", project),
			run:   func() (*types.OperationResponse, error) { return cleanupPipelineServer(c, project) },
		})
	}
	if req.Storage {
		parts = append(parts, cleanupPart{
			id:    CleanupStepID("storage", ""),
			title: "the S3 storage",
			run: func() (*types.OperationResponse, error) {
				return cleanupStorage(c, types.S3TeardownRequest{DeleteNamespace: req.DeleteNamespace})
			},
		})
	}
	if req.MLflow {
		parts = append(parts, cleanupPart{id: CleanupStepID("mlflow", ""), title: "MLflow", run: func() (*types.OperationResponse, error) { return cleanupMLflow(c) }})
	}
	for _, project := range req.Models {
		parts = append(parts, cleanupPart{
			id:    CleanupStepID("model", project),
			title: fmt.Sprintf("the model in '%s'", project),
			run:   func() (*types.OperationResponse, error) { return cleanupModels(c, project) },
		})
	}
	if req.InferenceGateway {
		parts = append(parts, cleanupPart{id: CleanupStepID("inference-gateway", ""), title: "the inference gateway", run: func() (*types.OperationResponse, error) { return cleanupGateway(c) }})
	}
	return parts
}

// CleanUp deletes what this tool created, as one operation: the pipeline
// servers in the given projects, then the S3 storage (with storage an
// earlier version left, and namespace seaweedfs when asked), then MLflow,
// then the models in the given projects, then the inference gateway.
// Each part runs the part's own teardown, with all its guards (only what
// this tool created, never while something still uses it). Each part is a
// streamed step; the first part that does not succeed stops the run, and
// the result names what was deleted and what was not.
func CleanUp(c *Client, req types.CleanupRequest, onStep func(UpdateStepEvent)) (*types.OperationResponse, error) {
	emit := func(e UpdateStepEvent) {
		if onStep != nil {
			onStep(e)
		}
	}
	parts := cleanupParts(c, req)
	var logs, done []string
	for i, p := range parts {
		if err := c.ctx.Err(); err != nil {
			return cleanupStopped(c, parts[i:], done, logs, fmt.Sprintf("The clean-up was stopped before %s: %v.", p.title, err), "cancelled"), nil
		}
		start := time.Now()
		emit(UpdateStepEvent{Step: p.id, Status: "running", Message: fmt.Sprintf("Deleting %s...", p.title)})
		res, err := p.run()
		if err != nil {
			res = &types.OperationResponse{Success: false, Message: err.Error(), ErrorCode: "internal"}
		}
		logs = append(logs, res.Logs...)
		if !res.Success {
			emit(UpdateStepEvent{Step: p.id, Status: "failed", Message: res.Message, ErrorCode: res.ErrorCode})
			return cleanupStopped(c, parts[i+1:], done, logs, res.Message, res.ErrorCode), nil
		}
		emit(UpdateStepEvent{Step: p.id, Status: "success", Message: res.Message, Detail: time.Since(start).Round(time.Second).String()})
		done = append(done, p.title)
	}
	slog.Info("clean-up", "parts", len(parts), "user", getUser(c))
	recordCleanupActivity(c, strings.Join(done, ", "), true)
	msg := "Nothing was selected."
	if len(done) > 0 {
		msg = "Deleted " + joinTitles(done) + "."
	}
	return &types.OperationResponse{Success: true, Message: msg, Logs: logs}, nil
}

// cleanupStopped is the result of a clean-up that stopped: why, what was
// deleted before, and what was not done.
func cleanupStopped(c *Client, notDone []cleanupPart, done, logs []string, reason, code string) *types.OperationResponse {
	msg := strings.TrimSpace(reason)
	if len(done) > 0 {
		msg += " Deleted before it stopped: " + joinTitles(done) + "."
	}
	if len(notDone) > 0 {
		titles := make([]string, len(notDone))
		for i, p := range notDone {
			titles[i] = p.title
		}
		msg += " Not done: " + joinTitles(titles) + "."
	}
	recordCleanupActivity(c, "stopped: "+reason, false)
	return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: firstNonEmpty(code, "partial_failure")}
}

// joinTitles joins "a", "b" and "c" as "a, b and c".
func joinTitles(titles []string) string {
	if len(titles) <= 1 {
		return strings.Join(titles, "")
	}
	return strings.Join(titles[:len(titles)-1], ", ") + " and " + titles[len(titles)-1]
}

func recordCleanupActivity(c *Client, detail string, success bool) {
	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "cleanup",
		Detail:    detail,
		Success:   success,
	})
}
