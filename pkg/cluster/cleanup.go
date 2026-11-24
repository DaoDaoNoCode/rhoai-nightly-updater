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
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Teardown hooks invoked during a clean-up; tests may substitute them.
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
	// Bound to recipe.ProjectTeardownProblem; swappable in tests.
	recipeProjectProblem = recipe.ProjectTeardownProblem
	// Bound to storage.ConnectionWorkbenchesProblem; swappable in tests.
	pipelineWorkbenchesProblem = storage.ConnectionWorkbenchesProblem
)

// cleanupConfirmCode reports whether code refuses a clean-up only until the
// user confirms what it lists.
func cleanupConfirmCode(code string) bool {
	return code == recipe.ErrorCodeConfirmContents || code == storage.ErrorCodeConfirmWorkbenches
}

// CleanupStepID identifies each clean-up slice in the stream, e.g.
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
	// When true, the overall clean-up message quotes this part's own result
	// (which may report no change—component retained, project already
	// absent) rather than wording like "Deleted <title>".
	says bool
}

// cleanupParts returns clean-up slices in execution order: pipeline servers
// first (storage teardown blocks while one still references storage), then
// storage, then MLflow (independent of both), then playgrounds (which
// depend on models), then models and—after those—the inference gateway they
// traffic through (its teardown blocks while a model still routes via it).
// A MaaS-published model is unpublished ahead of everything else (Models
// teardown blocks while publication remains), and the MaaS platform runs
// near the end except for recipe-provisioned projects, which run once
// in-project resources are gone, and DataScienceCluster components this
// tool enabled, which revert once nothing in the clean-up still needs them.
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
				return cleanupPipelineServer(c, project, req.ConfirmUnrecorded[project], req.ConfirmWorkbenches[project])
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
		// Scheduled after every in-project slice (MaaS publications,
		// pipeline servers, playgrounds, models) so project teardown runs
		// once those finish; each carries its own confirmations.
		parts = append(parts, cleanupPart{
			id:    CleanupStepID("recipe-project", project),
			title: fmt.Sprintf("project '%s', which a recipe created", project),
			run: func() (*types.OperationResponse, error) {
				return cleanupProject(c, project, req.RecipeProjectUIDs[project], req.ConfirmProjectContents[project])
			},
			says: true,
		})
	}
	if len(req.Components) > 0 {
		// Final step: component consumers are already removed.
		parts = append(parts, cleanupPart{id: CleanupStepID("components", ""), title: "the DataScienceCluster components this tool turned on", run: func() (*types.OperationResponse, error) { return cleanupComponents(c, req.Components) }, says: true})
	}
	return parts
}

// CleanUp removes resources this tool created as a single operation: pipeline
// servers in the named projects, then S3 storage (including legacy storage
// from an earlier version and the seaweedfs namespace when requested), then
// MLflow, then playgrounds and models in those projects, then the inference
// gateway.
// Each slice runs its dedicated teardown with the usual guards (only what
// this tool created, never while a dependent still exists). Progress streams
// per slice; the run halts on the first failure, and the response states
// what was removed versus what was left intact.
func CleanUp(c *Client, req types.CleanupRequest, onStep func(UpdateStepEvent)) (*types.OperationResponse, error) {
	emit := func(e UpdateStepEvent) {
		if onStep != nil {
			onStep(e)
		}
	}
	parts := cleanupParts(c, req)
	var done, said []string
	// When a recipe project would be rejected (not the reviewed instance,
	// no longer owned by the recipe, or still holding unconfirmed content),
	// the entire clean-up aborts before any deletion; that slice re-checks
	// immediately before delete. All projects are validated up front so every
	// confirmation prompt names its targets together.
	type projectCheck struct {
		project, problem, code string
		left                   []string
	}
	var checks []projectCheck
	var problems []string
	refusalCode := ""
	for _, project := range req.RecipeProjects {
		problem, code, left, err := recipeProjectProblem(c, project, req.RecipeProjectUIDs[project], req.ConfirmProjectContents[project])
		if err != nil {
			problem, code, left = fmt.Sprintf("Cannot check project %s: %v.", project, err), kube.ErrorCodeOr(err, "prerequisites"), nil
		}
		checks = append(checks, projectCheck{project: project, problem: problem, code: code, left: left})
		if problem == "" {
			continue
		}
		problems = append(problems, problem)
		// Refusals beyond “needs confirmation” keep a distinct error code.
		if refusalCode == "" || cleanupConfirmCode(refusalCode) && !cleanupConfirmCode(code) {
			refusalCode = code
		}
	}
	// A pipeline server whose dashboard Connection workbenches use goes
	// only with the user's confirmation of each of them (its teardown
	// removes the Connection from them first): checked up front too, so a
	// refusal deletes nothing. The step's detail lists the workbenches that
	// use it now, one per line, for the UI to ask about.
	for _, project := range req.PipelineServers {
		problem, code, current := pipelineWorkbenchesProblem(c, project, req.ConfirmWorkbenches[project])
		if problem == "" {
			continue
		}
		problems = append(problems, problem)
		if refusalCode == "" || cleanupConfirmCode(refusalCode) && !cleanupConfirmCode(code) {
			refusalCode = code
		}
		detail := ""
		if code == storage.ErrorCodeConfirmWorkbenches {
			detail = strings.Join(current, "\n")
		}
		emit(UpdateStepEvent{Step: CleanupStepID("pipeline-server", project), Status: "failed", Message: problem, ErrorCode: code, Detail: detail})
	}
	for _, chk := range checks {
		// Detail enumerates non-tool resources still in the project, one
		// line each, in the shape confirmProjectContents expects (names
		// cannot contain newlines): the UI swaps its confirmation prompt
		// for these lists. A confirmation refusal therefore lists every
		// project holding any such resource, including one whose contents
		// were fully confirmed: a retry must confirm again.
		switch {
		case chk.problem != "":
			detail := ""
			if chk.code == recipe.ErrorCodeConfirmContents {
				detail = strings.Join(chk.left, "\n")
			}
			emit(UpdateStepEvent{Step: CleanupStepID("recipe-project", chk.project), Status: "failed", Message: chk.problem, ErrorCode: chk.code, Detail: detail})
		case cleanupConfirmCode(refusalCode) && len(chk.left) > 0:
			emit(UpdateStepEvent{Step: CleanupStepID("recipe-project", chk.project), Status: "failed", ErrorCode: recipe.ErrorCodeConfirmContents,
				Message: fmt.Sprintf("Project %s holds what this tool did not create, all of it confirmed: %s. It was not deleted, as the clean-up asks to confirm what another project holds.", chk.project, strings.Join(chk.left, ", ")),
				Detail:  strings.Join(chk.left, "\n")})
		}
	}
	if len(problems) > 0 {
		problem := strings.Join(problems, " ")
		recordCleanupActivity(c, "refused: "+problem, false)
		return &types.OperationResponse{Success: false, Message: problem + " Nothing was deleted.", ErrorCode: refusalCode}, nil
	}
	// Orphaned MaaS publication projects trigger gateway cleanup first
	// (maas_prune.go), regardless of selection.
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

// cleanupSummary builds the user-facing outcome: deleted slices first, then
// messages from self-describing slices (components, recipe projects) so
// retained or already-absent items are not described as deleted.
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

// cleanupStopped assembles the response when a clean-up halts mid-run: cause,
// what completed beforehand, and what never ran.
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
