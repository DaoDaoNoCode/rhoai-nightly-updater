package cluster

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/authz"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/mlflow"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/workbench"
)

// Project actions: what a signed-in user who is not a cluster-admin may
// run in one of their own data science projects (the non-admin design,
// recipes-handoff/env-features/non-admin-design-for-approval.md, section
// 1), and what each one sends with their token.
//
// pkg/api's gate (withProjectMutationAuth) runs an action for such a user
// only when an access review with their own token (a
// SelfSubjectAccessReview per request, authz.ReviewUserAccess) allows every
// request the action can send, so a user with partial rights is refused
// before anything is written, with what is missing. A cluster-admin skips
// the reviews: their "*" rule covers every request.
//
// The requests come from two places:
//   - the RBAC scan derives what the code sends to the API server
//     (Source ""): TestProjectActionPreflightMatchesTheScan fails when the
//     scan of an action's code and its rows here differ, and the golden
//     file's "project action requests" section shows them for review;
//   - what platform services send with the user's token, which the scan
//     cannot see, is listed by hand with its source at a pinned release:
//     the dashboard's gen-ai API for the playground
//     (features.GenAIRequests, Source "gen-ai") and MLflow's access reviews
//     for the samples (mlflow.AuthRules, Source "mlflow").

// The namespace a project action's request goes to.
const (
	// ScopeProject: the project the action runs in.
	ScopeProject = "project"
	// ScopeApps: RHOAI's applications namespace (its DSCInitialization's,
	// read with the facts client), where the dashboard's images, hardware
	// profiles and configuration are, which RHOAI lets every user read.
	ScopeApps = "apps"
	// ScopeCluster: cluster-scoped, only reads OpenShift grants every
	// signed-in user (basic-user): the StorageClasses, the projects list.
	ScopeCluster = "cluster"
)

// ProjectNameSelf is a Name that stands for the project itself (a read of
// its own namespace).
const ProjectNameSelf = "{project}"

// The sources of a project action's request.
const (
	SourceScan   = ""
	SourceGenAI  = "gen-ai"
	SourceMLflow = "mlflow"
)

// ProjectRequest is one request a project action can send with the user's
// token: what RBAC decides (verb, API group, resource, name) and where.
type ProjectRequest struct {
	Verb, Group, Resource, Name string
	Scope                       string
	Source                      string
}

// ProjectAction is an action of the Environment page that acts in one
// project.
type ProjectAction struct {
	// ID is "<feature>.<action>", as the capabilities endpoint names it.
	ID string
	// Feature is the route's feature ("workbench", "playground",
	// "mlflow-samples"); Action is the request body's action.
	Feature, Action string
	// Label says what it does, for people.
	Label string
	// ForUsers: a user who is not a cluster-admin may run it (design
	// section 1). Otherwise only a cluster-admin does.
	ForUsers bool
	// Requests are what it can send with the user's token. Nil for an
	// action that is not ForUsers.
	Requests []ProjectRequest
}

func scanned(verb, group, resource, name, scope string) ProjectRequest {
	return ProjectRequest{Verb: verb, Group: group, Resource: resource, Name: name, Scope: scope}
}

// The workbench's own requests. Shared by its actions where the code is.
var (
	wbGetNotebook   = scanned("get", "kubeflow.org", "notebooks", workbench.Name, ScopeProject)
	wbPatchNotebook = scanned("patch", "kubeflow.org", "notebooks", workbench.Name, ScopeProject)
	wbGetStorage    = scanned("get", "", "persistentvolumeclaims", workbench.StorageName, ScopeProject)
	wbDeleteNb      = scanned("delete", "kubeflow.org", "notebooks", workbench.Name, ScopeProject)
	wbDeleteStorage = scanned("delete", "", "persistentvolumeclaims", workbench.StorageName, ScopeProject)
	wbListNotebooks = scanned("list", "kubeflow.org", "notebooks", "", ScopeProject)
	wbListPods      = scanned("list", "", "pods", "", ScopeProject)
	wbPipelineCfg   = scanned("get", "", "secrets", workbench.PipelineSecret, ScopeProject)
	wbProjProfiles  = scanned("list", "infrastructure.opendatahub.io", "hardwareprofiles", "", ScopeProject)
	wbAppsProfiles  = scanned("list", "infrastructure.opendatahub.io", "hardwareprofiles", "", ScopeApps)
)

// genAIRequests are the gen-ai API's requests of the playground calls.
func genAIRequests(calls ...string) []ProjectRequest {
	var out []ProjectRequest
	for _, r := range features.GenAIRequests {
		for _, c := range calls {
			if slices.Contains(r.Calls, c) {
				out = append(out, ProjectRequest{Verb: r.Verb, Group: r.Group, Resource: r.Resource, Name: r.Name, Scope: ScopeProject, Source: SourceGenAI})
				break
			}
		}
	}
	return out
}

// mlflowRequests are MLflow's access reviews of a samples action.
func mlflowRequests(action string) []ProjectRequest {
	var out []ProjectRequest
	for _, r := range mlflow.ActionReviews(action) {
		out = append(out, ProjectRequest{Verb: r.Verb, Group: mlflow.AuthGroup, Resource: r.Resource, Name: r.Name, Scope: ScopeProject, Source: SourceMLflow})
	}
	return out
}

// The playground's own requests (the RBAC scan's): its server, read for
// ownership; the set up also checks the project and the tool's models.
var playgroundServer = scanned("get", "ogx.io", "ogxservers", "lsd-genai-playground", ScopeProject)

var playgroundScanned = []ProjectRequest{
	scanned("get", "", "namespaces", ProjectNameSelf, ScopeProject),
	playgroundServer,
	scanned("get", "serving.kserve.io", "llminferenceservices", "granite-embedding-125m-english", ScopeProject),
	scanned("get", "serving.kserve.io", "llminferenceservices", "qwen25-05b-instruct", ScopeProject),
	scanned("get", "serving.kserve.io", "llminferenceservices", "qwen3-1-7b", ScopeProject),
	scanned("get", "serving.kserve.io", "llminferenceservices", "smollm2-135m-instruct", ScopeProject),
}

// The MLflow samples' own requests (the RBAC scan's): the projects list
// the run checks the project against.
var samplesScanned = []ProjectRequest{
	scanned("list", "project.openshift.io", "projects", "", ScopeCluster),
}

func joinRequests(parts ...[]ProjectRequest) []ProjectRequest {
	var out []ProjectRequest
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// projectActions are the project actions, in the order the capabilities
// endpoint lists them.
var projectActions = []ProjectAction{
	{ID: "workbench.setup", Feature: "workbench", Action: "setup", Label: "Set up the workbench", ForUsers: true, Requests: []ProjectRequest{
		scanned("create", "", "persistentvolumeclaims", "", ScopeProject),
		scanned("create", "kubeflow.org", "notebooks", "", ScopeProject),
		wbDeleteStorage, wbDeleteNb,
		scanned("get", "", "namespaces", ProjectNameSelf, ScopeProject),
		wbGetStorage, wbPipelineCfg,
		scanned("get", "", "secrets", "rhoai-nightly-updater-s3", ScopeProject),
		wbGetNotebook,
		scanned("get", "opendatahub.io", "odhdashboardconfigs", "odh-dashboard-config", ScopeApps),
		wbListPods,
		scanned("list", "image.openshift.io", "imagestreams", "", ScopeApps),
		wbAppsProfiles, wbProjProfiles,
		wbListNotebooks,
		scanned("list", "kueue.x-k8s.io", "localqueues", "", ScopeProject),
		scanned("list", "storage.k8s.io", "storageclasses", "", ScopeCluster),
	}},
	{ID: "workbench.start", Feature: "workbench", Action: "start", Label: "Start the workbench", ForUsers: true, Requests: []ProjectRequest{
		wbPipelineCfg, wbGetNotebook, wbProjProfiles, wbPatchNotebook,
	}},
	{ID: "workbench.stop", Feature: "workbench", Action: "stop", Label: "Stop the workbench", ForUsers: true, Requests: []ProjectRequest{
		wbGetNotebook, wbPatchNotebook,
	}},
	{ID: "workbench.teardown", Feature: "workbench", Action: "teardown", Label: "Delete the workbench", ForUsers: true, Requests: []ProjectRequest{
		wbDeleteStorage, wbDeleteNb, wbGetStorage, wbGetNotebook, wbListPods, wbListNotebooks,
	}},
	{ID: "workbench.delete-storage", Feature: "workbench", Action: "delete-storage", Label: "Delete the workbench's kept storage", ForUsers: true, Requests: []ProjectRequest{
		wbDeleteStorage, wbGetStorage, wbGetNotebook, wbListPods, wbListNotebooks,
	}},
	{ID: "playground.setup", Feature: "playground", Action: "setup", Label: "Set up the playground", ForUsers: true,
		Requests: joinRequests(playgroundScanned, genAIRequests("status", "install"))},
	{ID: "playground.teardown", Feature: "playground", Action: "teardown", Label: "Delete the playground", ForUsers: true,
		Requests: joinRequests([]ProjectRequest{playgroundServer}, genAIRequests("status", "delete"))},
	{ID: "mlflow-samples.add", Feature: "mlflow-samples", Action: "add", Label: "Add MLflow sample data", ForUsers: true,
		Requests: joinRequests(samplesScanned, mlflowRequests("add"))},
	{ID: "mlflow-samples.remove", Feature: "mlflow-samples", Action: "remove", Label: "Remove MLflow sample data", ForUsers: true,
		Requests: joinRequests(samplesScanned, mlflowRequests("remove"))},
	// Regenerate replaces the demo's data: not in the design's list of
	// what users may run (section 1: add, remove), so a cluster-admin's.
	{ID: "mlflow-samples.regenerate", Feature: "mlflow-samples", Action: "regenerate", Label: "Regenerate the MLflow demo"},
}

// ProjectActions are the project actions (a copy).
func ProjectActions() []ProjectAction {
	return slices.Clone(projectActions)
}

// ProjectActionID is the ID of feature's action.
func ProjectActionID(feature, action string) string { return feature + "." + action }

// ProjectActionByID is the project action with this ID.
func ProjectActionByID(id string) (ProjectAction, bool) {
	for _, a := range projectActions {
		if a.ID == id {
			return a, true
		}
	}
	return ProjectAction{}, false
}

// ErrAppsNamespaceUnknown: an action reads in RHOAI's applications
// namespace, which could not be read (fail closed).
var ErrAppsNamespaceUnknown = errors.New("RHOAI's applications namespace could not be read")

// accessRequest is r for project, with apps the applications namespace.
func (r ProjectRequest) accessRequest(project, apps string) (AccessRequest, error) {
	out := AccessRequest{Verb: r.Verb, Group: r.Group, Resource: r.Resource, Name: r.Name}
	if r.Name == ProjectNameSelf {
		out.Name = project
	}
	switch r.Scope {
	case ScopeProject:
		out.Namespace = project
	case ScopeApps:
		if apps == "" {
			return AccessRequest{}, ErrAppsNamespaceUnknown
		}
		out.Namespace = apps
	case ScopeCluster:
	default:
		return AccessRequest{}, fmt.Errorf("unknown scope %q", r.Scope)
	}
	return out, nil
}

// AccessRequest is authz.AccessRequest.
type AccessRequest = authz.AccessRequest

// MissingPermission is a request the user's RBAC does not allow, with who
// can grant it.
type MissingPermission struct {
	Request AccessRequest `json:"request"`
	// Text says it for people: the request and who can grant it.
	Text string `json:"text"`
}

// ReviewProjectActions reviews the requests of the actions ids in project
// with the user's own token (u; apps is RHOAI's applications namespace)
// and returns what each action misses (no entry: allowed). Each distinct
// request is reviewed once, however many actions send it. It fails closed:
// an unknown action, an unknown applications namespace that an action
// needs, or any review that fails is an error, and nothing is allowed.
func ReviewProjectActions(u *Client, ids []string, project, apps string) (map[string][]MissingPermission, error) {
	type key = AccessRequest
	index := map[key]int{}
	var distinct []AccessRequest
	per := map[string][]key{}
	for _, id := range ids {
		a, ok := ProjectActionByID(id)
		if !ok || !a.ForUsers {
			return nil, fmt.Errorf("%q is not a project action users may run", id)
		}
		for _, r := range a.Requests {
			req, err := r.accessRequest(project, apps)
			if err != nil {
				return nil, err
			}
			if _, seen := index[req]; !seen {
				index[req] = len(distinct)
				distinct = append(distinct, req)
			}
			if !slices.Contains(per[id], req) {
				per[id] = append(per[id], req)
			}
		}
	}
	denied, err := authz.ReviewUserAccess(u, distinct)
	if err != nil {
		return nil, err
	}
	no := map[key]bool{}
	for _, d := range denied {
		no[d] = true
	}
	out := map[string][]MissingPermission{}
	for _, id := range ids {
		for _, req := range per[id] {
			if no[req] {
				out[id] = append(out[id], MissingPermission{Request: req, Text: missingText(req, project)})
			}
		}
	}
	return out, nil
}

// missingText says a missing request and who can grant it.
func missingText(r AccessRequest, project string) string {
	if r.Namespace == project {
		return fmt.Sprintf("%s: ask an admin of project %s to give you the edit role there.", r, project)
	}
	return fmt.Sprintf("%s: RHOAI lets every signed-in user read this, but your account may not; ask a cluster-admin.", r)
}

// MissingSummary is the refusal of an action whose requests are missing:
// what, who can grant it, and that nothing was changed.
func MissingSummary(label, project string, missing []MissingPermission) string {
	return MissingSummaryFor(label, project, missing) + " Nothing was changed."
}

// MissingSummaryFor is what a capability says of an action whose requests
// are missing: the same as the gate's refusal, without "Nothing was
// changed" (nothing was tried).
func MissingSummaryFor(label, project string, missing []MissingPermission) string {
	texts := make([]string, 0, len(missing))
	for _, m := range missing {
		texts = append(texts, m.Text)
	}
	return fmt.Sprintf("%s in %s needs permissions your OpenShift account does not have: %s", label, project, strings.Join(texts, " "))
}

// projectFeatureComponents are the DataScienceCluster components a
// feature's project actions need.
func projectFeatureComponents(feature string) []envkit.FeatureComponent {
	switch feature {
	case "workbench":
		return []envkit.FeatureComponent{workbench.Component}
	case "mlflow-samples":
		return []envkit.FeatureComponent{envkit.MLflowComponent}
	case "playground":
		return features.PlaygroundComponents
	}
	return nil
}

// ProjectFeatureBlock is why feature's project actions cannot run on this
// cluster, with its errorCode ("prerequisites", or
// envkit.ErrorCodePrerequisitesUnknown when the DataScienceCluster cannot
// be read: fail closed), "" when they may. It reads the
// DataScienceCluster with the facts client (envkit.SetupComponentBlock).
func ProjectFeatureBlock(c *Client, feature string) (string, string) {
	for _, fc := range projectFeatureComponents(feature) {
		if reason, code := envkit.SetupComponentBlock(c, fc); reason != "" {
			return reason, code
		}
	}
	return "", ""
}

// NeedsAppsNamespace reports whether one of the actions ids sends a
// request to RHOAI's applications namespace.
func NeedsAppsNamespace(ids []string) bool {
	for _, id := range ids {
		a, _ := ProjectActionByID(id)
		for _, r := range a.Requests {
			if r.Scope == ScopeApps {
				return true
			}
		}
	}
	return false
}

// ProjectAppsNamespace is RHOAI's applications namespace, as its
// DSCInitialization names it, read with c's facts client (the
// ServiceAccount's; rhoai.DiscoverNamespaces). An error (RHOAI not
// installed, the read failed) says only how the read failed, not the
// ServiceAccount's error text.
func ProjectAppsNamespace(c *Client) (string, error) {
	ns, err := rhoai.DiscoverNamespaces(c)
	if err != nil {
		if errors.Is(err, rhoai.ErrNoDSCInitialization) {
			return "", fmt.Errorf("%w: RHOAI has no DSCInitialization on this cluster", ErrAppsNamespaceUnknown)
		}
		kube.LogFactReadFailure("cannot read RHOAI's applications namespace for a project action", "DSCInitialization", err)
		return "", fmt.Errorf("%w (%s)", ErrAppsNamespaceUnknown, kube.FactReadProblem(err))
	}
	if ns.Applications == "" {
		return "", ErrAppsNamespaceUnknown
	}
	return ns.Applications, nil
}
