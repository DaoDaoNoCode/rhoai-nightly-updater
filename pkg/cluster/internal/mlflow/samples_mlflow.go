package mlflow

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/authz"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dashdev"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
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

	SampleMarkerKey = "rhoai-nightly-updater.sample"
	WorkspaceHeader = "X-MLflow-Workspace"
	// mlflowStatusCacheTTL keeps the samples status of a project this long;
	// any samples operation on the project drops it.
	mlflowStatusCacheTTL = 15 * time.Second
)

// SampleIDs is the order samples are added in (removal: reversed).
var SampleIDs = []string{SampleGenAIDemo, SampleTraining, SampleMCPServer, SampleSkill}

var mlflowSampleNames = map[string]string{
	SampleGenAIDemo: "Agent and prompt demo",
	SampleTraining:  "Training experiment and registered model",
	SampleMCPServer: "MCP server",
	SampleSkill:     "Agent skill",
}

var mlflowSamplesCache = kube.NewLRU[string, types.MLflowSamplesStatus](64)

// Target is MLflow in one project's workspace.
type Target struct {
	api *SampleAPI
	UI  string // MLflow's public address, no trailing slash
	// Dashboard is the RHOAI dashboard's https address, when known: links
	// open its MLflow pages, which keep the dashboard's navigation.
	Dashboard string
	Project   string
	// c and cr are the cluster and the MLflow CR the target was read from;
	// the demo checks the CR's memory limit (oldToolResources).
	c  *kube.Client
	cr mlflowCR
}

// link is a page of the MLflow UI in the target's workspace.
func (t *Target) link(route string) string {
	return t.UI + "/#" + route + "?workspace=" + url.QueryEscape(t.Project)
}

// ExperimentLink is an experiment's page: the dashboard's
// (odh-dashboard rhoai-3.6 frontend/src/routes/pipelines/mlflow.ts
// mlflowExperimentRoute), or MLflow's own UI when the dashboard's address
// is not known.
func (t *Target) ExperimentLink(id string) string {
	if t.Dashboard == "" {
		return t.link("/experiments/" + id)
	}
	q := url.Values{"workspace": {t.Project}, "searchFilter": {""}}
	return t.Dashboard + "/develop-train/mlflow/experiments/" + url.PathEscape(id) + "/runs?" + q.Encode()
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

// SampleTarget returns MLflow for project, or why the samples are
// unavailable. The address is the CR's status.url, the address the operator
// reports and odh-dashboard links to (see mlflowCR).
func SampleTarget(c *kube.Client, project string) (*Target, string, error) {
	return mlflowSampleTargetAs(c, project, c.BearerToken())
}

// mlflowSampleTargetAs is mlflowSampleTarget whose MLflow calls carry
// token: c finds MLflow, token is who asks it. The MLflow CR (cluster
// scoped) is a cluster fact, read with the facts client (c.Facts(), the
// ServiceAccount's): a project user's own token may not read it.
func mlflowSampleTargetAs(c *kube.Client, project, token string) (*Target, string, error) {
	cr, err := ReadCR(c.Facts())
	switch {
	case kube.IsK8sNotFound(err):
		return nil, "MLflow is not deployed. Set it up first.", nil
	case err != nil:
		return nil, "", fmt.Errorf("read the MLflow CR: %w", err)
	case cr.Metadata.Terminating():
		return nil, "MLflow is being deleted.", nil
	case !mlflowCRReady(cr):
		return nil, "MLflow is not ready.", nil
	}
	// The samples call MLflow only at status.url, never at the Route the
	// tab falls back to for its link (getMLflowStatus): releases before the
	// data-science gateway report no status.url, and their MLflow was not
	// checked against these calls. This is the one rule; the page shows its
	// reason as is.
	base := UIURL(cr.Status.URL, "")
	if base == "" {
		return nil, mlflowNoStatusURLReason, nil
	}
	return &Target{
		api: &SampleAPI{
			Ctx: c.Context(), Base: base, Token: token, Server: "MLflow",
			headers: map[string]string{WorkspaceHeader: project},
		},
		UI: base, Project: project, c: c, cr: cr, Dashboard: httpsBase(dashdev.CachedURL(c)),
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
func (t *Target) experimentByName(name string) (*mlflowExperiment, error) {
	var out struct {
		Experiment mlflowExperiment `json:"experiment"`
	}
	err := t.api.sendJSON("GET", "/api/2.0/mlflow/experiments/get-by-name", url.Values{"experiment_name": {name}}, nil, &out)
	if IsNotFound(err) {
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
	return what + " exists but was not created by this tool (it lacks the " + SampleMarkerKey + " tag), so it is left unchanged."
}

// statusFromError reports a status read that failed.
func statusFromError(s types.SampleStatus, err error) types.SampleStatus {
	s.State = "unknown"
	s.Message = "Cannot read the sample's state: " + err.Error() + "." + deniedReadHint(err)
	s.Actions = []string{}
	return s
}

// mlflowSample is one sample: its status and its actions. remove is nil
// for the GenAI demo, which the tool may not delete (no demo-delete right,
// see addGenAIDemo).
type mlflowSample struct {
	status     func(t *Target) types.SampleStatus
	add        func(t *Target, log func(string)) (string, error)
	remove     func(t *Target, log func(string)) (string, error)
	regenerate func(t *Target, log func(string)) (string, error)
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

// SamplesStatus reports the MLflow samples of project. It makes
// only named reads (one per sample, a few for the training sample) and is
// cached for mlflowStatusCacheTTL per user and project, so it stays out of
// the main status poll.
//
// The reads go to MLflow with the signed-in user's own token (u), so
// MLflow's own access reviews decide what they see (mlflow-kubernetes-
// plugins: every request is reviewed with the caller's token, see
// samples_rbac_test.go). project must be one of the user's data science
// projects (ListUserDSProjects), checked before the cache. The
// ServiceAccount (sa) only finds MLflow: its CR's status.url and the
// dashboard's address, which are no user data.
func SamplesStatus(sa, u *kube.Client, project string) (*types.MLflowSamplesStatus, error) {
	ok, err := authz.IsUserDSProject(u, project)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: '%s' is not one of your data science projects", ErrNotDSProject, project)
	}
	gen := mlflowSamplesGen.current(project)
	// The token and the project's invalidation count key the cache: one
	// user's answer is never another's, and a samples operation on the
	// project makes every user's cached answer unreachable.
	key := fmt.Sprintf("%s|%d|%s", kube.TokenKey(u.BearerToken()), gen, project)
	if cached, ok := mlflowSamplesCache.Get(key); ok {
		return &cached, nil
	}
	t, reason, err := mlflowSampleTargetAs(sa, project, u.BearerToken())
	if err != nil {
		return nil, err
	}
	st := types.MLflowSamplesStatus{Project: project, Samples: []types.SampleStatus{}}
	if t == nil {
		st.Reason = reason
	} else {
		st.Available, st.MLflowURL = true, t.UI
		st.Samples = make([]types.SampleStatus, len(SampleIDs))
		samples := mlflowSamples()
		var wg sync.WaitGroup
		for i, id := range SampleIDs {
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
		mlflowSamplesCache.Add(key, st, mlflowStatusCacheTTL)
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

// InvalidateSamplesStatus drops the cached samples status of project (the
// count is part of every key), and keeps a read that is under way from
// caching what it read before.
func InvalidateSamplesStatus(project string) {
	mlflowSamplesGen.bump(project)
}

// ValidateSamplesRequest checks a request before the operation
// starts; it returns the problem, or "".
func ValidateSamplesRequest(req types.MLflowSamplesRequest) string {
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

// RunSamples adds, removes or regenerates the requested samples in
// one project, one step per sample. A failed sample does not stop the
// others; the result fails when any did.
func RunSamples(c *kube.Client, req types.MLflowSamplesRequest, onStep func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	logs := []string{}
	emit := func(e types.UpdateStepEvent) {
		logs = append(logs, e.Message)
		if onStep != nil {
			onStep(e)
		}
	}
	defer InvalidateSamplesStatus(req.Project)
	activity := fmt.Sprintf("%s %s in %s", req.Action, strings.Join(req.Samples, ","), req.Project)
	fail := func(step, msg, code string) (*types.OperationResponse, error) {
		emit(types.UpdateStepEvent{Step: step, Status: "failed", Message: msg, ErrorCode: code})
		record.Action(c, "mlflow-samples", activity, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}
	if problem := ValidateSamplesRequest(req); problem != "" {
		return fail("check", problem, "validation")
	}

	emit(types.UpdateStepEvent{Step: "check", Status: "running", Message: fmt.Sprintf("Checking MLflow for project '%s'", req.Project)})
	// The project must be one of the signed-in user's data science
	// projects (the projects API with their own token, as the samples
	// status checks), before anything is read with the ServiceAccount.
	u := c.SignedInUser()
	if u == nil {
		return fail("check", "There is no signed-in user's client to list your data science projects with. Nothing was changed.", "internal")
	}
	ok, err := authz.IsUserDSProject(u, req.Project)
	if err != nil {
		return fail("check", fmt.Sprintf("Cannot list your data science projects: %v", err), kube.ErrorCodeFromK8sErr(err))
	}
	if !ok {
		return fail("check", fmt.Sprintf("'%s' is not one of your data science projects.", req.Project), "validation")
	}
	t, reason, err := SampleTarget(c, req.Project)
	if err != nil {
		return fail("check", fmt.Sprintf("Cannot read MLflow: %v", err), kube.ErrorCodeFromK8sErr(err))
	}
	if t == nil {
		return fail("check", reason, "prerequisites")
	}
	emit(types.UpdateStepEvent{Step: "check", Status: "success", Message: "MLflow is ready at " + t.UI})

	want := map[string]bool{}
	for _, id := range req.Samples {
		want[id] = true
	}
	order := append([]string{}, SampleIDs...)
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
		fn := map[string]func(*Target, func(string)) (string, error){"add": s.add, "remove": s.remove, "regenerate": s.regenerate}[req.Action]
		name := mlflowSampleNames[id]
		if fn == nil {
			failed = append(failed, name)
			code = kube.FirstNonEmpty(code, "validation")
			emit(types.UpdateStepEvent{Step: id, Status: "failed", Message: fmt.Sprintf("%s: %s is not available for this sample.", name, req.Action), ErrorCode: "validation"})
			continue
		}
		emit(types.UpdateStepEvent{Step: id, Status: "running", Message: fmt.Sprintf("%s: %s", name, req.Action)})
		msg, err := fn(t, func(line string) { emit(types.UpdateStepEvent{Step: id, Status: "running", Message: line}) })
		if err != nil {
			failed = append(failed, name)
			stepCode := SampleErrorCode(err)
			var refusal *sampleRefusal
			switch {
			case errors.As(err, &refusal):
				stepCode = "not_managed"
			case errors.Is(err, errNoDemoGenerator), errors.Is(err, errNoMCPRegistry), errors.Is(err, errNoSkillRegistry):
				stepCode = "unsupported"
			case errors.Is(err, errMLflowRestarting):
				stepCode = "in_progress"
			}
			code = kube.FirstNonEmpty(code, stepCode)
			emit(types.UpdateStepEvent{Step: id, Status: "failed", Message: fmt.Sprintf("%s: %v%s", name, err, deniedHint(err, c.ActsAsUser())), ErrorCode: stepCode})
			continue
		}
		emit(types.UpdateStepEvent{Step: id, Status: "success", Message: fmt.Sprintf("%s: %s", name, msg)})
	}
	slog.Info("mlflow samples", "action", req.Action, "samples", req.Samples, "project", req.Project, "failed", failed, "user", kube.GetUser(c))
	if len(failed) > 0 {
		record.Action(c, "mlflow-samples", activity+" (failed: "+strings.Join(failed, ", ")+")", false)
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Failed: %s. See the steps for the reason.", strings.Join(failed, ", ")), Logs: logs, ErrorCode: code}, nil
	}
	record.Action(c, "mlflow-samples", activity, true)
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
