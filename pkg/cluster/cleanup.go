package cluster

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/recipe"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The teardowns a clean-up runs; tests replace them.
var (
	cleanupPipelineServer = TeardownPipelineServer
	cleanupStorage        = TeardownS3Storage
	cleanupMLflow         = TeardownMLflow
	cleanupModels         = func(c *Client, project string) (*types.OperationResponse, error) {
		return serving.TeardownModels(c, project)
	}
	cleanupGateway      = serving.TeardownInferenceGateway
	cleanupPlayground   = features.TeardownPlayground
	cleanupUnpublish    = serving.UnpublishMaaSModel
	cleanupMaaSPlatform = serving.TeardownMaaSPlatform
	cleanupComponents   = features.RestoreComponents
	cleanupProject      = recipe.DeleteProject
	// recipeProjectProblem is recipe.ProjectTeardownProblem; tests replace it.
	recipeProjectProblem = recipe.ProjectTeardownProblem
)

// CleanupStepID is the step id of each part of a clean-up, as streamed:
// "pipeline-server:<project>", "storage", "mlflow", "playground:<project>",
// "model:<project>", "inference-gateway", "maas-unpublish:<project>"
// (first), "maas-platform", "recipe-project:<project>", and "components"
// (last).
func CleanupStepID(part, project string) string {
	if project != "" {
		return part + ":" + project
	}
	return part
}

type cleanupPart struct {
	id, title string
	run       func() (*types.OperationResponse, error)
	// says: the part's own result message is what the clean-up's result
	// says it did (it may have changed nothing: a component kept, a
	// project already gone), instead of "Deleted <title>".
	says bool
}

// cleanupParts lists the parts of a clean-up in the order they run: the
// pipeline servers first (the storage teardown refuses while one uses the
// storage), then the storage, then MLflow, which depends on neither, then
// the playgrounds (which use the models), then the models and, after them, the inference gateway they route through
// (its teardown refuses while a model uses it). A model published to MaaS
// is unpublished before anything else (the Models teardown refuses while it
// is published), and the MaaS platform goes last but for a project a
// recipe created, which goes once the parts that live in it are gone, and
// the DataScienceCluster components this tool turned on, which go back to
// what they were once nothing of the clean-up uses them.
func cleanupParts(c *Client, req types.CleanupRequest) []cleanupPart {
	var parts []cleanupPart
	for _, project := range req.MaaSPublished {
		parts = append(parts, cleanupPart{
			id:    CleanupStepID("maas-unpublish", project),
			title: fmt.Sprintf("the MaaS publication of the model in '%s'", project),
			run:   func() (*types.OperationResponse, error) { return cleanupUnpublish(c, project) },
		})
	}
	for _, project := range req.PipelineServers {
		parts = append(parts, cleanupPart{
			id:    CleanupStepID("pipeline-server", project),
			title: fmt.Sprintf("the pipeline server in '%s'", project),
			run: func() (*types.OperationResponse, error) {
				return cleanupPipelineServer(c, project, req.ConfirmUnrecorded[project])
			},
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
	for _, project := range req.Playgrounds {
		parts = append(parts, cleanupPart{
			id:    CleanupStepID("playground", project),
			title: fmt.Sprintf("the playground in '%s'", project),
			run:   func() (*types.OperationResponse, error) { return cleanupPlayground(c, project) },
		})
	}
	for _, project := range req.Models {
		parts = append(parts, cleanupPart{
			id:    CleanupStepID("model", project),
			title: fmt.Sprintf("the model in '%s'", project),
			run:   func() (*types.OperationResponse, error) { return cleanupModels(c, project) },
		})
	}
	if req.InferenceGateway {
		parts = append(parts, cleanupPart{id: CleanupStepID("inference-gateway", ""), title: "the inference gateway", run: func() (*types.OperationResponse, error) { return cleanupGateway(c, len(req.Models) > 0) }})
	}
	if req.MaaSPlatform {
		parts = append(parts, cleanupPart{id: CleanupStepID("maas-platform", ""), title: "the MaaS platform", run: func() (*types.OperationResponse, error) { return cleanupMaaSPlatform(c) }})
	}
	for _, project := range req.RecipeProjects {
		// After every part that lives in a project (MaaS publications,
		// pipeline servers, playgrounds, models), so its own teardown
		// runs first; each with its own confirmations.
		parts = append(parts, cleanupPart{
			id:    CleanupStepID("recipe-project", project),
			title: fmt.Sprintf("project '%s', which a recipe created", project),
			run: func() (*types.OperationResponse, error) {
				return cleanupProject(c, project, req.ConfirmProjectContents[project])
			},
			says: true,
		})
	}
	if len(req.Components) > 0 {
		// Last: what used the components is gone by now.
		parts = append(parts, cleanupPart{id: CleanupStepID("components", ""), title: "the DataScienceCluster components this tool turned on", run: func() (*types.OperationResponse, error) { return cleanupComponents(c, req.Components) }, says: true})
	}
	return parts
}

// CleanUp deletes what this tool created, as one operation: the pipeline
// servers in the given projects, then the S3 storage (with storage an
// earlier version left, and namespace seaweedfs when asked), then MLflow,
// then the playgrounds and the models in the given projects, then the
// inference gateway.
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
	var done, said []string
	// A recipe project that would be refused (not the recipe's any more,
	// or holding what the user did not confirm) refuses the whole clean-up
	// before anything is deleted; its part checks again before deleting.
	// Every project is checked, so each one to confirm is named at once.
	var problems []string
	refusalCode := ""
	for _, project := range req.RecipeProjects {
		problem, code, unconfirmed, err := recipeProjectProblem(c, project, req.ConfirmProjectContents[project])
		if err != nil {
			problem, code = fmt.Sprintf("Cannot check project %s: %v.", project, err), kube.ErrorCodeOr(err, "prerequisites")
		}
		if problem == "" {
			continue
		}
		// Detail lists what is to confirm, one per line, exactly as
		// confirmProjectContents takes it for the project (names cannot
		// hold a line break): the browser shows the list and sends it
		// back.
		emit(UpdateStepEvent{Step: CleanupStepID("recipe-project", project), Status: "failed", Message: problem, ErrorCode: code, Detail: strings.Join(unconfirmed, "\n")})
		problems = append(problems, problem)
		// A refusal that is not only a confirmation to give names its
		// own code.
		if refusalCode == "" || refusalCode == recipe.ErrorCodeConfirmContents {
			refusalCode = code
		}
	}
	if len(problems) > 0 {
		problem := strings.Join(problems, " ")
		recordCleanupActivity(c, "refused: "+problem, false)
		return &types.OperationResponse{Success: false, Message: problem + " Nothing was deleted.", ErrorCode: refusalCode}, nil
	}
	// Projects of MaaS publications that are gone leave the MaaS gateway
	// first (maas_prune.go), whatever is selected.
	logs := serving.ReleaseGoneMaaSProjects(c)
	for i, p := range parts {
		if err := c.Context().Err(); err != nil {
			return cleanupStopped(c, parts[i:], done, said, logs, fmt.Sprintf("The clean-up was stopped before %s: %v.", p.title, err), "cancelled"), nil
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
			return cleanupStopped(c, parts[i+1:], done, said, logs, res.Message, res.ErrorCode), nil
		}
		emit(UpdateStepEvent{Step: p.id, Status: "success", Message: res.Message, Detail: time.Since(start).Round(time.Second).String()})
		if p.says {
			said = append(said, strings.TrimSpace(res.Message))
		} else {
			done = append(done, p.title)
		}
	}
	slog.Info("clean-up", "parts", len(parts), "user", kube.GetUser(c))
	msg := cleanupSummary(done, said)
	recordCleanupActivity(c, msg, true)
	return &types.OperationResponse{Success: true, Message: msg, Logs: logs}, nil
}

// cleanupSummary says what a clean-up did: the parts it deleted, then
// what the parts that speak for themselves (the components, a recipe
// project) said, so nothing kept or already gone reads as deleted.
func cleanupSummary(done, said []string) string {
	var out []string
	if len(done) > 0 {
		out = append(out, "Deleted "+kube.JoinTitles(done)+".")
	}
	for _, s := range said {
		if s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return "Nothing was selected."
	}
	return strings.Join(out, " ")
}

// cleanupStopped is the result of a clean-up that stopped: why, what was
// deleted before, and what was not done.
func cleanupStopped(c *Client, notDone []cleanupPart, done, said, logs []string, reason, code string) *types.OperationResponse {
	msg := strings.TrimSpace(reason)
	if len(done) > 0 {
		msg += " Deleted before it stopped: " + kube.JoinTitles(done) + "."
	}
	for _, s := range said {
		if s = strings.TrimSpace(s); s != "" {
			msg += " " + s
		}
	}
	if len(notDone) > 0 {
		titles := make([]string, len(notDone))
		for i, p := range notDone {
			titles[i] = p.title
		}
		msg += " Not done: " + kube.JoinTitles(titles) + "."
	}
	recordCleanupActivity(c, "stopped: "+reason, false)
	return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: kube.FirstNonEmpty(code, "partial_failure")}
}

func recordCleanupActivity(c *Client, detail string, success bool) {
	record.Activity(c, types.ActivityEntry{
		Timestamp: kube.NowRFC3339(),
		User:      kube.GetUser(c),
		Action:    "cleanup",
		Detail:    detail,
		Success:   success,
	})
}
