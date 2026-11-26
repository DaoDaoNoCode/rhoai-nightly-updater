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
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
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
	// SourceProjectAdmin is the check that the user is an admin of the
	// project (the non-admin design, amendment G1): a request the action
	// never sends, which the admin role grants and edit does not.
	SourceProjectAdmin = "project-admin"
)

// projectAdminCheck is the request whose review tells an admin of the
// project from an editor: create RoleBindings, which Kubernetes' admin
// role has and edit lacks (kubernetes v1.31.0 bootstrappolicy policy.go
// 288-296).
var projectAdminCheck = ProjectRequest{Verb: "create", Group: "rbac.authorization.k8s.io", Resource: "rolebindings", Scope: ScopeProject, Source: SourceProjectAdmin}

// isProjectAdminCheck reports whether r, as reviewed, is that check.
func isProjectAdminCheck(r AccessRequest) bool {
	return r.Verb == projectAdminCheck.Verb && r.Group == projectAdminCheck.Group && r.Resource == projectAdminCheck.Resource && r.Name == ""
}

// ProjectRequest is one request a project action can send with the user's
// token: what RBAC decides (verb, API group, resource, name) and where.
type ProjectRequest struct {
	Verb, Group, Resource, Name string
	Scope                       string
	Source                      string
	// When, if set, is the condition under which the action makes the
	// request on this cluster and project (WhenKueueQueues); the preflight
	// reviews it only then.
	When string
}

// WhenKueueQueues: a workbench setup reads the project's LocalQueues only
// when the project is Kueue-managed and Kueue is available to the
// dashboard (workbench.QueuesNeeded).
const WhenKueueQueues = "kueue-queues"

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
	// AdminRefusal, when set, is why a cluster-admin may not run it (the
	// owner's decision: a cluster-admin does not create project S3
	// storage, and sets pipeline servers up on the shared storage).
	AdminRefusal string
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
		scanned("get", "", "secrets", storage.ProjectConnectionSecretName, ScopeProject),
		wbGetNotebook,
		scanned("get", "opendatahub.io", "odhdashboardconfigs", "odh-dashboard-config", ScopeApps),
		wbListPods,
		scanned("list", "image.openshift.io", "imagestreams", "", ScopeApps),
		wbAppsProfiles, wbProjProfiles,
		wbListNotebooks,
		{Verb: "list", Group: "kueue.x-k8s.io", Resource: "localqueues", Scope: ScopeProject, When: WhenKueueQueues},
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
	// An add that MLflow refuses half-way removes the sample again through
	// its remove path (mlflow rollBackRefusedAdd), so the add needs what
	// the remove needs too: a user who may add but not delete is refused
	// before anything is made, not left with half a sample.
	{ID: "mlflow-samples.add", Feature: "mlflow-samples", Action: "add", Label: "Add MLflow sample data", ForUsers: true,
		Requests: joinRequests(samplesScanned, mlflowRequests("add"), mlflowRequests("remove"))},
	{ID: "mlflow-samples.remove", Feature: "mlflow-samples", Action: "remove", Label: "Remove MLflow sample data", ForUsers: true,
		Requests: joinRequests(samplesScanned, mlflowRequests("remove"))},
	// Regenerate replaces the demo's data: not in the design's list of
	// what users may run (section 1: add, remove), so a cluster-admin's.
	{ID: "mlflow-samples.regenerate", Feature: "mlflow-samples", Action: "regenerate", Label: "Regenerate the MLflow demo"},
	// Project S3 storage: set up, repair and delete by admins of the
	// project only (G1; projectAdminCheck). A cluster-admin repairs and
	// deletes one, and does not set one up.
	{ID: "project-s3.setup", Feature: "project-s3", Action: "setup", Label: "Set up the project's S3 storage", ForUsers: true,
		AdminRefusal: storage.AdminCannotCreateProjectStorage, Requests: joinRequests(projectS3Apply, []ProjectRequest{projectAdminCheck})},
	{ID: "project-s3.repair", Feature: "project-s3", Action: "repair", Label: "Repair the project's S3 storage", ForUsers: true,
		Requests: joinRequests(projectS3Apply, []ProjectRequest{projectAdminCheck})},
	{ID: "project-s3.delete", Feature: "project-s3", Action: "delete", Label: "Delete the project's S3 storage", ForUsers: true,
		Requests: joinRequests(projectS3Delete, []ProjectRequest{projectAdminCheck})},
	// Its pipeline server: admins and editors of the project.
	{ID: "project-pipeline-server.setup", Feature: "project-pipeline-server", Action: "setup", Label: "Set up the pipeline server on the project's S3 storage", ForUsers: true,
		AdminRefusal: storage.AdminUsesSharedStorage, Requests: projectPipelineSetup},
	{ID: "project-pipeline-server.teardown", Feature: "project-pipeline-server", Action: "teardown", Label: "Delete the pipeline server on the project's S3 storage", ForUsers: true,
		Requests: projectPipelineTeardown},
}

// The project storage's own requests (the RBAC scan's), by its objects'
// names.
func projectS3Requests(verbs ...string) []ProjectRequest {
	var out []ProjectRequest
	for _, o := range []struct{ group, resource, name string }{
		{"", "persistentvolumeclaims", storage.ProjectS3PVC},
		{"", "secrets", storage.ProjectS3Secret},
		{"networking.k8s.io", "networkpolicies", storage.ProjectS3NetworkPolicy},
		{"apps", "deployments", storage.ProjectS3Deployment},
		{"", "services", storage.ProjectS3Service},
	} {
		for _, v := range verbs {
			out = append(out, scanned(v, o.group, o.resource, o.name, ScopeProject))
		}
	}
	return out
}

var (
	projectS3Apply = joinRequests(projectS3Requests("create", "get", "patch", "delete"), []ProjectRequest{
		scanned("create", "", "persistentvolumeclaims", "", ScopeProject),
		scanned("create", "", "secrets", "", ScopeProject),
		scanned("create", "networking.k8s.io", "networkpolicies", "", ScopeProject),
		scanned("create", "apps", "deployments", "", ScopeProject),
		scanned("create", "", "services", "", ScopeProject),
	})
	projectS3Delete = joinRequests(projectS3Requests("get", "delete"), []ProjectRequest{
		scanned("list", "datasciencepipelinesapplications.opendatahub.io", "datasciencepipelinesapplications", "", ScopeProject),
	})
	projectPipelineSetup = []ProjectRequest{
		scanned("create", "", "secrets", "", ScopeProject),
		scanned("create", "", "secrets", storage.DSPASecretName, ScopeProject),
		scanned("create", "", "secrets", storage.ProjectConnectionSecretName, ScopeProject),
		scanned("create", "datasciencepipelinesapplications.opendatahub.io", "datasciencepipelinesapplications", "", ScopeProject),
		scanned("create", "datasciencepipelinesapplications.opendatahub.io", "datasciencepipelinesapplications", storage.DSPAName, ScopeProject),
		scanned("delete", "", "secrets", storage.DSPASecretName, ScopeProject),
		scanned("delete", "", "secrets", storage.ProjectConnectionSecretName, ScopeProject),
		scanned("delete", "datasciencepipelinesapplications.opendatahub.io", "datasciencepipelinesapplications", storage.DSPAName, ScopeProject),
		scanned("get", "", "secrets", storage.DSPASecretName, ScopeProject),
		scanned("get", "", "secrets", storage.ProjectConnectionSecretName, ScopeProject),
		scanned("get", "", "secrets", storage.ProjectS3Secret, ScopeProject),
		scanned("get", "", "persistentvolumeclaims", storage.ProjectS3PVC, ScopeProject),
		scanned("get", "", "services", storage.ProjectS3Service, ScopeProject),
		scanned("get", "networking.k8s.io", "networkpolicies", storage.ProjectS3NetworkPolicy, ScopeProject),
		scanned("get", "apps", "deployments", storage.ProjectS3Deployment, ScopeProject),
		scanned("get", "datasciencepipelinesapplications.opendatahub.io", "datasciencepipelinesapplications", storage.DSPAName, ScopeProject),
		scanned("list", "datasciencepipelinesapplications.opendatahub.io", "datasciencepipelinesapplications", "", ScopeProject),
		scanned("patch", "", "secrets", storage.DSPASecretName, ScopeProject),
		scanned("patch", "", "secrets", storage.ProjectConnectionSecretName, ScopeProject),
		scanned("patch", "datasciencepipelinesapplications.opendatahub.io", "datasciencepipelinesapplications", storage.DSPAName, ScopeProject),
	}
	projectPipelineTeardown = []ProjectRequest{
		scanned("delete", "", "secrets", storage.DSPASecretName, ScopeProject),
		scanned("delete", "", "secrets", storage.ProjectConnectionSecretName, ScopeProject),
		scanned("delete", "datasciencepipelinesapplications.opendatahub.io", "datasciencepipelinesapplications", storage.DSPAName, ScopeProject),
		scanned("get", "", "secrets", storage.DSPASecretName, ScopeProject),
		scanned("get", "", "secrets", storage.ProjectConnectionSecretName, ScopeProject),
		scanned("get", "datasciencepipelinesapplications.opendatahub.io", "datasciencepipelinesapplications", storage.DSPAName, ScopeProject),
		scanned("list", "datasciencepipelinesapplications.opendatahub.io", "datasciencepipelinesapplications", "", ScopeProject),
		scanned("list", "kubeflow.org", "notebooks", "", ScopeProject),
		scanned("patch", "kubeflow.org", "notebooks", "", ScopeProject),
	}
)

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

// ActionReview is the preflight's answer for one action: what the user's
// RBAC does not allow (none: allowed), or Err when it could not be told
// (fail closed: the action is not allowed).
type ActionReview struct {
	Missing []MissingPermission
	Err     error
}

// ReviewProjectActions reviews the requests of the actions ids in project
// with the user's own token (u; apps is RHOAI's applications namespace, ""
// when it is not known) and returns each action's answer. Each distinct
// request is reviewed once, however many actions send it, and a
// conditional request (When) only where the action makes it. An action
// whose requests cannot be resolved (it needs the applications namespace,
// or its condition cannot be read) gets its own Err, the others are still
// reviewed. It fails closed: an action users may not run, or a review that
// fails, is an error for the whole call.
func ReviewProjectActions(u *Client, ids []string, project, apps string) (map[string]ActionReview, error) {
	type key = AccessRequest
	out := map[string]ActionReview{}
	index := map[key]int{}
	var distinct []AccessRequest
	per := map[string][]key{}
	type cond struct {
		v   bool
		err error
	}
	conds := map[string]cond{}
	condition := func(when string) (bool, error) {
		if c, ok := conds[when]; ok {
			return c.v, c.err
		}
		var c cond
		switch when {
		case WhenKueueQueues:
			c.v, c.err = workbench.QueuesNeeded(u, project, apps)
		default:
			c.err = fmt.Errorf("unknown condition %q", when)
		}
		conds[when] = c
		return c.v, c.err
	}
actions:
	for _, id := range ids {
		a, ok := ProjectActionByID(id)
		if !ok || !a.ForUsers {
			return nil, fmt.Errorf("%q is not a project action users may run", id)
		}
		var reqs []key
		for _, r := range a.Requests {
			if r.When != "" {
				if apps == "" {
					out[id] = ActionReview{Err: ErrAppsNamespaceUnknown}
					continue actions
				}
				needed, err := condition(r.When)
				if err != nil {
					out[id] = ActionReview{Err: err}
					continue actions
				}
				if !needed {
					continue
				}
			}
			req, err := r.accessRequest(project, apps)
			if err != nil {
				out[id] = ActionReview{Err: err}
				continue actions
			}
			if !slices.Contains(reqs, req) {
				reqs = append(reqs, req)
			}
		}
		per[id] = reqs
		for _, req := range reqs {
			if _, seen := index[req]; !seen {
				index[req] = len(distinct)
				distinct = append(distinct, req)
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
	for id, reqs := range per {
		r := ActionReview{}
		for _, req := range reqs {
			if no[req] {
				r.Missing = append(r.Missing, MissingPermission{Request: req, Text: missingText(req, project)})
			}
		}
		out[id] = r
	}
	return out, nil
}

// missingText says a missing request and who can grant it.
func missingText(r AccessRequest, project string) string {
	if isProjectAdminCheck(r) && r.Namespace == project {
		return fmt.Sprintf("Only an admin of project %s sets up, repairs and deletes its S3 storage (the OpenShift admin role there); ask an admin of project %s.", project, project)
	}
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
	case "project-pipeline-server":
		return []envkit.FeatureComponent{envkit.PipelinesComponent}
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
