package cluster

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// MLflow samples: sample data the tool adds to one project's MLflow
// workspace, and removes again by name. MLflow workspaces are namespaces
// (mlflow-kubernetes-plugins v2.1.1 workspace_plugin/provider.py), chosen
// per request with the X-MLflow-Workspace header (mlflow
// utils/workspace_utils.py:9).
//
// Everything a sample creates that takes tags carries sampleMarkerKey; the
// status and removal check it, so an object of a sample's name that someone
// else created is reported as "foreign" and never changed or deleted.

const (
	SampleGenAIDemo = "genai-demo"
	SampleTraining  = "training"
	SampleMCPServer = "mcp-server"
	SampleSkill     = "skill"

	sampleMarkerKey       = "rhoai-nightly-updater.sample"
	mlflowWorkspaceHeader = "X-MLflow-Workspace"
	// mlflowStatusCacheTTL keeps the samples status of a project this long;
	// any samples operation on the project drops it.
	mlflowStatusCacheTTL = 15 * time.Second
)

// mlflowSampleIDs is the order samples are added in (removal: reversed).
var mlflowSampleIDs = []string{SampleGenAIDemo, SampleTraining, SampleMCPServer, SampleSkill}

var mlflowSampleNames = map[string]string{
	SampleGenAIDemo: "Agent and prompt demo",
	SampleTraining:  "Training experiment and registered model",
	SampleMCPServer: "MCP server",
	SampleSkill:     "Agent skill",
}

var mlflowSamplesCache = newLRU[string, types.MLflowSamplesStatus](64)

// mlflowTarget is MLflow in one project's workspace.
type mlflowTarget struct {
	api *sampleAPI
	ui  string // MLflow's public address, no trailing slash
	// dashboard is the RHOAI dashboard's https address, when known: links
	// open its MLflow pages, which keep the dashboard's navigation.
	dashboard string
	project   string
	// c and cr are the cluster and the MLflow CR the target was read from;
	// the demo checks the CR's memory limit (oldToolResources).
	c  *Client
	cr mlflowCR
}

// link is a page of the MLflow UI in the target's workspace.
func (t *mlflowTarget) link(route string) string {
	return t.ui + "/#" + route + "?workspace=" + url.QueryEscape(t.project)
}

// experimentLink is an experiment's page: the dashboard's
// (odh-dashboard rhoai-3.6 frontend/src/routes/pipelines/mlflow.ts
// mlflowExperimentRoute), or MLflow's own UI when the dashboard's address
// is not known.
func (t *mlflowTarget) experimentLink(id string) string {
	if t.dashboard == "" {
		return t.link("/experiments/" + id)
	}
	q := url.Values{"workspace": {t.project}, "searchFilter": {""}}
	return t.dashboard + "/develop-train/mlflow/experiments/" + url.PathEscape(id) + "/runs?" + q.Encode()
}

// mlflowCRReady mirrors getMLflowStatus: the Ready or Available condition.
func mlflowCRReady(cr mlflowCR) bool {
	for _, cond := range cr.Status.Conditions {
		if cond.Type == "Ready" || cond.Type == "Available" {
			return cond.Status == "True"
		}
	}
	return false
}

// mlflowNoStatusURLReason: the CR reports no https address in status.url.
const mlflowNoStatusURLReason = "This MLflow reports no https address in its CR (status.url), which sample data needs: it is not supported on this MLflow release. The MLflow link above may still work."

// mlflowSampleTarget returns MLflow for project, or why the samples are
// unavailable. The address is the CR's status.url, the address the operator
// reports and odh-dashboard links to (see mlflowCR).
func mlflowSampleTarget(c *Client, project string) (*mlflowTarget, string, error) {
	cr, err := getMLflowCR(c)
	switch {
	case IsK8sError(err, 404):
		return nil, "MLflow is not deployed. Set it up first.", nil
	case err != nil:
		return nil, "", fmt.Errorf("read the MLflow CR: %w", err)
	case cr.Metadata.terminating():
		return nil, "MLflow is being deleted.", nil
	case !mlflowCRReady(cr):
		return nil, "MLflow is not ready.", nil
	}
	// The samples call MLflow only at status.url, never at the Route the
	// tab falls back to for its link (getMLflowStatus): releases before the
	// data-science gateway report no status.url, and their MLflow was not
	// checked against these calls. This is the one rule; the page shows its
	// reason as is.
	base := mlflowUIURL(cr.Status.URL, "")
	if base == "" {
		return nil, mlflowNoStatusURLReason, nil
	}
	return &mlflowTarget{
		api: &sampleAPI{
			ctx: c.ctx, base: base, token: c.token, server: "MLflow",
			headers: map[string]string{mlflowWorkspaceHeader: project},
		},
		ui: base, project: project, c: c, cr: cr, dashboard: httpsBase(cachedDashboardURL(c)),
	}, "", nil
}

// --- MLflow REST shapes (mlflow/protos/service.proto, model_registry.proto) ---

type mlflowTag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type mlflowExperiment struct {
	ExperimentID   string      `json:"experiment_id"`
	Name           string      `json:"name"`
	LifecycleStage string      `json:"lifecycle_stage"`
	Tags           []mlflowTag `json:"tags"`
}

func (e *mlflowExperiment) tag(key string) (string, bool) {
	for _, t := range e.Tags {
		if t.Key == key {
			return t.Value, true
		}
	}
	return "", false
}

func (e *mlflowExperiment) active() bool { return e.LifecycleStage == "active" }

// experimentByName returns the experiment, deleted ones included
// (GetExperimentByName reads ViewType.ALL: handlers.py:1896-1912,
// sqlalchemy_store.py:929-950), or nil when there is none.
func (t *mlflowTarget) experimentByName(name string) (*mlflowExperiment, error) {
	var out struct {
		Experiment mlflowExperiment `json:"experiment"`
	}
	err := t.api.sendJSON("GET", "/api/2.0/mlflow/experiments/get-by-name", url.Values{"experiment_name": {name}}, nil, &out)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out.Experiment, nil
}

// sampleRefusal is a sample the tool will not change: an object of its
// name exists that the tool did not create.
type sampleRefusal struct{ msg string }

func (r *sampleRefusal) Error() string { return r.msg }

func foreignMessage(what string) string {
	return what + " exists but was not created by this tool (it lacks the " + sampleMarkerKey + " tag), so it is left unchanged."
}

// statusFromError reports a status read that failed.
func statusFromError(s types.SampleStatus, err error) types.SampleStatus {
	s.State = "unknown"
	s.Message = "Cannot read the sample's state: " + err.Error() + "." + deniedHint(err, false)
	s.Actions = []string{}
	return s
}

// mlflowSample is one sample: its status and its actions. remove is nil
// for the GenAI demo, which the tool may not delete (no demo-delete right,
// see addGenAIDemo).
type mlflowSample struct {
	status     func(t *mlflowTarget) types.SampleStatus
	add        func(t *mlflowTarget, log func(string)) (string, error)
	remove     func(t *mlflowTarget, log func(string)) (string, error)
	regenerate func(t *mlflowTarget, log func(string)) (string, error)
}

// mlflowSamples returns the samples by ID. It is a function, not a
// package variable filled by init functions, so the gitops profile's
// reference graph (gitops_offline_test.go) does not reach MLflow.
func mlflowSamples() map[string]mlflowSample {
	return map[string]mlflowSample{
		SampleGenAIDemo: {status: demoStatus, add: addGenAIDemo, regenerate: regenerateGenAIDemo},
		SampleTraining:  {status: trainingStatus, add: addTraining, remove: removeTraining},
		SampleMCPServer: {status: mcpStatus, add: addMCP, remove: removeMCP},
		SampleSkill:     {status: skillStatus, add: addSkill, remove: removeSkill},
	}
}

// GetMLflowSamplesStatus reports the MLflow samples of project. It makes
// only named reads (one per sample, a few for the training sample) and is
// cached for mlflowStatusCacheTTL, so it stays out of the main status poll.
//
// project must be a Data Science project, checked before the cache, like
// the playground's status and every samples operation: the reads go to
// MLflow with the ServiceAccount's token, so a request must not choose any
// namespace.
func GetMLflowSamplesStatus(c *Client, project string) (*types.MLflowSamplesStatus, error) {
	ok, err := isDSProject(c, project)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: '%s'", ErrNotDSProject, project)
	}
	if cached, ok := mlflowSamplesCache.Get(project); ok {
		return &cached, nil
	}
	gen := mlflowSamplesGen.current(project)
	t, reason, err := mlflowSampleTarget(c, project)
	if err != nil {
		return nil, err
	}
	st := types.MLflowSamplesStatus{Project: project, Samples: []types.SampleStatus{}}
	if t == nil {
		st.Reason = reason
	} else {
		st.Available, st.MLflowURL = true, t.ui
		st.Samples = make([]types.SampleStatus, len(mlflowSampleIDs))
		samples := mlflowSamples()
		var wg sync.WaitGroup
		for i, id := range mlflowSampleIDs {
			wg.Go(func() {
				s := samples[id].status(t)
				s.ID, s.Name = id, mlflowSampleNames[id]
				if s.Actions == nil {
					s.Actions = []string{}
				}
				st.Samples[i] = s
			})
		}
		wg.Wait()
	}
	// A samples operation that ended during this read invalidated the
	// project: what was read may predate it, so it is returned but not kept.
	if mlflowSamplesGen.current(project) == gen {
		mlflowSamplesCache.Add(project, st, mlflowStatusCacheTTL)
	}
	return &st, nil
}

// samplesGenerations counts the invalidations of each project's samples
// status. A status read caches its result only when the count did not
// change while it read.
type samplesGenerations struct {
	mu  sync.Mutex
	gen map[string]uint64
}

var mlflowSamplesGen = &samplesGenerations{gen: map[string]uint64{}}

func (g *samplesGenerations) current(project string) uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.gen[project]
}

func (g *samplesGenerations) bump(project string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gen[project]++
}

// InvalidateSamplesStatus drops the cached samples status of project, and
// keeps a read that is under way from caching what it read before.
func InvalidateSamplesStatus(project string) {
	mlflowSamplesGen.bump(project)
	mlflowSamplesCache.Remove(project)
}

// ValidateMLflowSamplesRequest checks a request before the operation
// starts; it returns the problem, or "".
func ValidateMLflowSamplesRequest(req types.MLflowSamplesRequest) string {
	switch req.Action {
	case "add", "remove", "regenerate":
	default:
		return "action must be add, remove or regenerate"
	}
	if len(req.Samples) == 0 {
		return "samples is required"
	}
	seen := map[string]bool{}
	for _, id := range req.Samples {
		if _, ok := mlflowSampleNames[id]; !ok {
			return fmt.Sprintf("unknown sample %q", id)
		}
		if seen[id] {
			return fmt.Sprintf("sample %q is listed twice", id)
		}
		seen[id] = true
		if req.Action == "regenerate" && id != SampleGenAIDemo {
			return "only genai-demo can be regenerated"
		}
		if req.Action == "remove" && id == SampleGenAIDemo {
			return "the GenAI demo cannot be removed by the updater: remove it in the MLflow UI"
		}
	}
	return ""
}

// ErrNotDSProject: the project named in a request is not a Data Science
// project.
var ErrNotDSProject = errors.New("not a Data Science project")

// isDSProject reports whether project is a Data Science project (the
// projects the Environment page lists).
func isDSProject(c *Client, project string) (bool, error) {
	projects, err := GetDSProjects(c)
	if err != nil {
		return false, err
	}
	if slices.Contains(projects, project) {
		return true, nil
	}
	return false, nil
}

// RunMLflowSamples adds, removes or regenerates the requested samples in
// one project, one step per sample. A failed sample does not stop the
// others; the result fails when any did.
func RunMLflowSamples(c *Client, req types.MLflowSamplesRequest, onStep func(UpdateStepEvent)) (*types.OperationResponse, error) {
	logs := []string{}
	emit := func(e UpdateStepEvent) {
		logs = append(logs, e.Message)
		if onStep != nil {
			onStep(e)
		}
	}
	defer InvalidateSamplesStatus(req.Project)
	activity := fmt.Sprintf("%s %s in %s", req.Action, strings.Join(req.Samples, ","), req.Project)
	fail := func(step, msg, code string) (*types.OperationResponse, error) {
		emit(UpdateStepEvent{Step: step, Status: "failed", Message: msg, ErrorCode: code})
		recordActivity(c, "mlflow-samples", activity, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}
	if problem := ValidateMLflowSamplesRequest(req); problem != "" {
		return fail("check", problem, "validation")
	}

	emit(UpdateStepEvent{Step: "check", Status: "running", Message: fmt.Sprintf("Checking MLflow for project '%s'...", req.Project)})
	ok, err := isDSProject(c, req.Project)
	if err != nil {
		return fail("check", fmt.Sprintf("Cannot list the projects: %v", err), errorCodeFromK8sErr(err))
	}
	if !ok {
		return fail("check", fmt.Sprintf("'%s' is not a Data Science project.", req.Project), "validation")
	}
	t, reason, err := mlflowSampleTarget(c, req.Project)
	if err != nil {
		return fail("check", fmt.Sprintf("Cannot read MLflow: %v", err), errorCodeFromK8sErr(err))
	}
	if t == nil {
		return fail("check", reason, "prerequisites")
	}
	emit(UpdateStepEvent{Step: "check", Status: "success", Message: "MLflow is ready at " + t.ui})

	want := map[string]bool{}
	for _, id := range req.Samples {
		want[id] = true
	}
	order := append([]string{}, mlflowSampleIDs...)
	if req.Action == "remove" {
		for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
			order[i], order[j] = order[j], order[i]
		}
	}
	samples := mlflowSamples()
	var failed []string
	code := ""
	for _, id := range order {
		if !want[id] {
			continue
		}
		s := samples[id]
		fn := map[string]func(*mlflowTarget, func(string)) (string, error){"add": s.add, "remove": s.remove, "regenerate": s.regenerate}[req.Action]
		name := mlflowSampleNames[id]
		if fn == nil {
			failed = append(failed, name)
			code = firstNonEmpty(code, "validation")
			emit(UpdateStepEvent{Step: id, Status: "failed", Message: fmt.Sprintf("%s: %s is not available for this sample.", name, req.Action), ErrorCode: "validation"})
			continue
		}
		emit(UpdateStepEvent{Step: id, Status: "running", Message: fmt.Sprintf("%s: %s...", name, req.Action)})
		msg, err := fn(t, func(line string) { emit(UpdateStepEvent{Step: id, Status: "running", Message: line}) })
		if err != nil {
			failed = append(failed, name)
			stepCode := sampleErrorCode(err)
			var refusal *sampleRefusal
			switch {
			case errors.As(err, &refusal):
				stepCode = "not_managed"
			case errors.Is(err, errNoDemoGenerator), errors.Is(err, errNoMCPRegistry), errors.Is(err, errNoSkillRegistry):
				stepCode = "unsupported"
			case errors.Is(err, errMLflowRestarting):
				stepCode = "in_progress"
			}
			code = firstNonEmpty(code, stepCode)
			emit(UpdateStepEvent{Step: id, Status: "failed", Message: fmt.Sprintf("%s: %v%s", name, err, deniedHint(err, c.asUser)), ErrorCode: stepCode})
			continue
		}
		emit(UpdateStepEvent{Step: id, Status: "success", Message: fmt.Sprintf("%s: %s", name, msg)})
	}
	slog.Info("mlflow samples", "action", req.Action, "samples", req.Samples, "project", req.Project, "failed", failed, "user", getUser(c))
	if len(failed) > 0 {
		recordActivity(c, "mlflow-samples", activity+" (failed: "+strings.Join(failed, ", ")+")", false)
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Failed: %s. See the steps for the reason.", strings.Join(failed, ", ")), Logs: logs, ErrorCode: code}, nil
	}
	recordActivity(c, "mlflow-samples", activity, true)
	return &types.OperationResponse{Success: true, Message: fmt.Sprintf("Samples %s in project '%s'.", verbPast(req.Action), req.Project), Logs: logs}, nil
}

func verbPast(action string) string {
	switch action {
	case "add":
		return "added"
	case "remove":
		return "removed"
	case "regenerate":
		return "regenerated"
	case "run":
		return "started"
	}
	return action
}

// httpsBase is u without a trailing slash when it is an https address, else "".
func httpsBase(u string) string {
	if !strings.HasPrefix(u, "https://") {
		return ""
	}
	return strings.TrimRight(u, "/")
}
