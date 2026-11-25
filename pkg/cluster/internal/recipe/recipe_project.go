package recipe

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/authz"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Recipe-owned OpenShift/Kubernetes project namespaces.
//
// By default this updater never creates projects; it only does when a recipe
// sets spec.project.mode to create. Creation tracks the dashboard's Create
// project path and uses the signed-in user's token so cluster templates and
// the caller's admin binding apply as they would in the UI:
//
//   - odh-dashboard frontend/src/api/k8s/projects.ts createProject (main at
//     7a6f65a, 2026-11-21) creates a ProjectRequest (project.openshift.io/v1,
//     metadata.name, displayName, description), then calls its backend's
//     GET /api/namespaces/<name>/0;
//   - backend/src/routes/api/namespaces/namespaceUtils.ts applyNamespaceChange,
//     case DSG_CREATION, merge-patches the Namespace with the label
//     opendatahub.io/dashboard=true (KnownLabels.DASHBOARD_RESOURCE), plus
//     kueue.openshift.io/managed=true unless the OdhDashboardConfig sets
//     disableKueue. It refuses names starting with openshift or kube.
//
// Labels follow dashboard rules: kueue.openshift.io/managed is applied only
// when the dashboard would (kueueManaged consults OdhDashboardConfig); the
// updater additionally sets managed-by and projectAnnotation to the recipe
// name.
//
// The ProjectRequest's name and UID are persisted in the snapshot ConfigMap at
// projectRecordKey ahead of labeling, so a partial run still remembers
// ownership. Labels go on via JSON patch with an initial UID test, which
// prevents tagging a namespace recreated under the same name. Teardown deletes
// the project only if UID, tool label, and recipe annotation still agree,
// deletion is not underway, and anything listable inside is gone or explicitly
// confirmed (DeleteProject).
const (
	projectAnnotation = "rhoai-nightly-updater/recipe"
	// dashboardProjectLabel is the odh-dashboard data-science project marker
	// (KnownLabels.DASHBOARD_RESOURCE; envkit.DSProjects filters on it).
	dashboardProjectLabel = "opendatahub.io/dashboard"
	// kueueProjectLabel is the same key as odh-dashboard KnownLabels.KUEUE_MANAGED.
	kueueProjectLabel = "kueue.openshift.io/managed"

	projectRecordKey     = "recipeProject"
	projectRecordManager = "rhoai-nightly-updater-recipe-projects"
	// maxRecordedProjects caps how many projects the snapshot retains.
	maxRecordedProjects = 50
)

// ErrorCodeConfirmContents is returned when project teardown must have the
// user acknowledge remaining contents before deletion proceeds.
const ErrorCodeConfirmContents = "confirm_project_contents"

// projectRecord is what gets stored under the snapshot key.
type projectRecord struct {
	Projects []types.RecipeOwnedProject `json:"projects"`
}

func readProjectRecord(c *kube.Client) (projectRecord, error) {
	var rec projectRecord
	raw, err := record.ReadSnapshotKey(c, projectRecordKey)
	if err != nil {
		return rec, fmt.Errorf("read the record of the projects recipes created: %w", err)
	}
	if raw == "" {
		return rec, nil
	}
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return rec, fmt.Errorf("parse the record of the projects recipes created: %w", err)
	}
	return rec, nil
}

func writeProjectRecord(c *kube.Client, rec projectRecord) error {
	if len(rec.Projects) > maxRecordedProjects {
		rec.Projects = rec.Projects[len(rec.Projects)-maxRecordedProjects:]
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := record.WriteSnapshotKey(c, projectRecordKey, projectRecordManager, string(data)); err != nil {
		return fmt.Errorf("record the project the recipe created: %w", err)
	}
	return nil
}

// recorded looks up the stored entry for project name, if any.
func (r projectRecord) recorded(name string) (types.RecipeOwnedProject, bool) {
	for _, p := range r.Projects {
		if p.Name == name {
			return p, true
		}
	}
	return types.RecipeOwnedProject{}, false
}

// with copies r but stores p, dropping any prior entry with p's name.
func (r projectRecord) with(p types.RecipeOwnedProject) projectRecord {
	out := projectRecord{Projects: slices.DeleteFunc(slices.Clone(r.Projects), func(o types.RecipeOwnedProject) bool { return o.Name == p.Name })}
	out.Projects = append(out.Projects, p)
	return out
}

// without returns r with the entry for name removed.
func (r projectRecord) without(name string) projectRecord {
	return projectRecord{Projects: slices.DeleteFunc(slices.Clone(r.Projects), func(o types.RecipeOwnedProject) bool { return o.Name == name })}
}

func namespacePath(name string) string { return rhoai.ClusterPath("v1", "namespaces", name) }

// refusedProjectName yields a reason a recipe cannot create or delete project
// name, or "" when allowed. Blocked names include platform and product
// reservations (reservedProjectName), this updater's namespace, the RHOAI
// operator namespace, and applications/monitoring namespaces from
// DSCInitialization. If DSCInitialization cannot be read the error is
// returned (fail closed); when none exists RHOAI may not have those namespaces
// yet.
func refusedProjectName(c *kube.Client, name string) (string, error) {
	switch {
	case reservedProjectName(name):
		return fmt.Sprintf("Project %q is a name of the platform or of this updater; a recipe does not create or delete it. Choose another name.", name), nil
	case name == record.GetActivityNamespace():
		return fmt.Sprintf("Project %q is the updater's own namespace. Choose another name.", name), nil
	case name == rhoai.SubNS:
		return fmt.Sprintf("Project %q is the RHOAI operator's namespace. Choose another name.", name), nil
	}
	ns, err := rhoai.DiscoverNamespaces(c)
	switch {
	case errors.Is(err, rhoai.ErrNoDSCInitialization):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("cannot tell whether %s is a namespace of RHOAI: %w", name, err)
	case name == ns.Applications || name == ns.Monitoring:
		return fmt.Sprintf("Project %q is a namespace of RHOAI (its DSCInitialization names it). Choose another name.", name), nil
	}
	return "", nil
}

// projectFix points the user at changing spec.project—or waiting if deletion
// is still in flight.
func projectFix() *types.RecipeFix { return fixAt(types.RecipeFixProjectForeign, "") }

// foreignProjectMessage explains that an existing project was not recipe-created.
func foreignProjectMessage(name string) string {
	return fmt.Sprintf("Project %s exists and was not created by a recipe. Choose another name, or set project.mode: existing to use it as it is. Nothing was changed.", name)
}

// projectFound captures namespace metadata from a lookup plus whether the
// recipe owns it (recorded UID matches).
type projectFound struct {
	meta  kube.ObjectMeta
	found bool
	ours  bool
}

func findProject(c *kube.Client, name string) (projectFound, projectRecord, error) {
	rec, err := readProjectRecord(c)
	if err != nil {
		return projectFound{}, rec, err
	}
	meta, found, err := kube.ReadObjectMeta(c, namespacePath(name))
	if err != nil {
		return projectFound{}, rec, fmt.Errorf("read namespace %s: %w", name, err)
	}
	p := projectFound{meta: meta, found: found}
	if r, ok := rec.recorded(name); ok && found && r.UID == meta.UID {
		p.ours = true
	}
	return p, rec, nil
}

// complete is true when a recipe-owned project has all labels and annotations
// this step applies.
func (p projectFound) complete() bool {
	return p.meta.HasToolLabel() && p.meta.Labels[dashboardProjectLabel] == "true" && p.meta.Annotations[projectAnnotation] != ""
}

// deletable is true when Clean up may remove p: recipe ownership (recorded UID),
// the tool managed-by label, and the recipe annotation—markers only a
// namespace writer can set, not the ServiceAccount that owns the snapshot
// record.
func (p projectFound) deletable() bool {
	return p.found && p.ours && p.meta.HasToolLabel() && p.meta.Annotations[projectAnnotation] != ""
}

// checkProjectStep validates the project portion of a recipe. Mode existing
// requires an existing user data-science project with no recipe ownership.
// Mode create succeeds once the recipe-created namespace (recorded UID) has
// required labels; it blocks on foreign namespaces, terminating projects, or
// reserved names.
func checkProjectStep(c *kube.Client, s *State) (Check, error) {
	p := s.Recipe.Spec.Project
	if p == nil {
		return Check{Done: true, Reason: "The recipe uses no project."}, nil
	}
	if p.Mode == ProjectExisting {
		ok, err := envkit.IsDSProject(c, p.Name)
		if err != nil {
			return Check{}, fmt.Errorf("list the data science projects: %w", err)
		}
		if !ok {
			return Check{Blocked: fmt.Sprintf("Project %s is not a data science project (or does not exist). Create it in the RHOAI dashboard, or set project.mode: create. Nothing was changed.", p.Name), Fix: projectFix()}, nil
		}
		s.Project = &types.RecipeProjectRef{Name: p.Name}
		return Check{Done: true, Reason: fmt.Sprintf("Uses your data science project %s.", p.Name)}, nil
	}
	why, err := refusedProjectName(c, p.Name)
	if err != nil {
		return Check{}, err
	}
	if why != "" {
		return Check{Blocked: why + " Nothing was changed.", Fix: projectFix()}, nil
	}
	found, _, err := findProject(c, p.Name)
	if err != nil {
		return Check{}, err
	}
	switch {
	case !found.found:
		// Missing (or never existed): an earlier recorded project is not
		// present; creation will run again.
		s.Project = nil
		return Check{}, nil
	case !found.ours:
		return Check{Blocked: foreignProjectMessage(p.Name), Fix: projectFix()}, nil
	case found.meta.Terminating():
		return Check{Blocked: fmt.Sprintf("Project %s, which a recipe created, is being deleted; wait until it is gone, then run the recipe again.", p.Name), Fix: projectFix()}, nil
	}
	s.Project = &types.RecipeProjectRef{Name: p.Name, UID: found.meta.UID, Owned: true}
	if !found.complete() {
		return Check{}, nil // labels will be reconciled on ensure
	}
	// Done only once the steps that use it can see it (userSeesProject);
	// until then Run waits for it.
	if seen, err := userSeesProject(c, p.Name); err != nil || !seen {
		return Check{}, err
	}
	return Check{Done: true, Reason: fmt.Sprintf("Project %s was created by a recipe.", p.Name)}, nil
}

// How long the project step waits for a project it created (or found
// created) to be among the signed-in user's data science projects, and how
// often it looks. The project-scoped steps after it (the MLflow samples,
// the pipeline server, the workbench, the model, the playground) check
// that list (authz.IsUserDSProject), and several of them start as soon as
// the project step ended.
//
// OpenShift answers a user's project LIST from the openshift-apiserver's
// project authorization cache, not from the namespaces themselves. Each
// openshift-apiserver re-synchronizes that cache from its namespace and RBAC
// informers once a second (openshift/openshift-apiserver release-4.21 at
// bcfcb015fdb7b195ea1ccb3e9f710520ab5843d5,
// pkg/cmd/openshift-apiserver/openshiftapiserver/openshift_apiserver.go:608-611,
// "period := 1 * time.Second" and ProjectAuthorizationCache.Run(period);
// pkg/project/auth/cache.go:283-287, Run runs synchronize every period
// with utilwait.Forever). A project just created, its admin RoleBinding
// and the dashboard label patched on it are listed once that replica's
// informers saw them and its next synchronization ran; a LIST may reach
// any replica. The poll is that period; the wait is 30 periods, a margin
// for informers that lag on a loaded cluster.
var (
	projectVisiblePoll = time.Second
	projectVisibleWait = 30 * time.Second
)

// userSeesProject: name is among the signed-in user's data science
// projects, as the later steps check (authz.IsUserDSProject, with the
// user's own client).
func userSeesProject(c *kube.Client, name string) (bool, error) {
	u := c.SignedInUser()
	if u == nil {
		return false, errors.New("there is no signed-in user's client to list your data science projects")
	}
	seen, err := authz.IsUserDSProject(u, name)
	if err != nil {
		return false, fmt.Errorf("list your data science projects: %w", err)
	}
	return seen, nil
}

// waitUserSeesProject waits, on the cluster packages' clock, until name is
// among the signed-in user's data science projects (projectVisibleWait).
// It returns "" once it is, or why the step fails: the list cannot be read,
// or the project is still not in it. err is the step's context ending.
func waitUserSeesProject(c *kube.Client, name string, emit func(types.UpdateStepEvent)) (msg, code string, err error) {
	deadline := kube.Clock().Now().Add(projectVisibleWait)
	for waited := false; ; waited = true {
		seen, err := userSeesProject(c, name)
		switch {
		case err != nil:
			return fmt.Sprintf("Project %s is set up, but checking that it is among your data science projects failed: %v. Resume checks it again.", name, err), kube.ErrorCodeOr(err, "prerequisites"), nil
		case seen:
			return "", "", nil
		case !kube.Clock().Now().Before(deadline):
			return fmt.Sprintf("Project %s is set up, but OpenShift still does not list it among your data science projects after %s (a new project usually appears there within seconds). The steps that use it need it there. Check that you can open it in the RHOAI dashboard, then Resume.", name, projectVisibleWait), "timeout", nil
		case !waited:
			emit(types.UpdateStepEvent{Step: StepProject, Status: "running", Message: fmt.Sprintf("Waiting for project %s to appear among your data science projects", name)})
		}
		if err := kube.Clock().Sleep(c.Context(), projectVisiblePoll); err != nil {
			return "", "", err
		}
	}
}

// EnsureProject provisions a mode-create project like the dashboard, using the
// signed-in user's token (see package comment), persists ownership, and
// applies labels. Previously created recipe projects keep their UID and only
// need label reconciliation; anything else is rejected per checkProjectStep
// (fail closed).
func EnsureProject(c *kube.Client, s *State, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	p := s.Recipe.Spec.Project
	fail := func(msg, code string) (*types.OperationResponse, error) {
		record.Action(c, "create-recipe-project", fmt.Sprintf("project=%s (%s)", projectName(p), msg), false)
		return &types.OperationResponse{Success: false, Message: msg, ErrorCode: code}, nil
	}
	chk, err := checkProjectStep(c, s)
	switch {
	case err != nil:
		return fail(fmt.Sprintf("Cannot check project %s: %v. Nothing was changed.", projectName(p), err), kube.ErrorCodeOr(err, "prerequisites"))
	case chk.Blocked != "":
		return fail(chk.Blocked, "prerequisites")
	case chk.Done:
		return &types.OperationResponse{Success: true, Message: chk.Reason}, nil
	case p == nil || p.Mode != ProjectCreate:
		return fail("The recipe does not create its project.", "validation")
	}
	name := p.Name
	// Resolve whether Kueue labeling matches dashboard settings before create.
	kueue, err := kueueManaged(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the dashboard's settings to label project %s as the dashboard does: %v. Nothing was changed.", name, err), kube.ErrorCodeOr(err, "prerequisites"))
	}
	var uid string
	if s.Project != nil && s.Project.Owned && s.Project.UID != "" {
		uid = s.Project.UID // already created and recorded; labels may be incomplete
	} else {
		emit(types.UpdateStepEvent{Step: StepProject, Status: "running", Message: fmt.Sprintf("Creating project %s", name)})
		created, err := requestProject(c, name, p.DisplayName, s.Recipe.Metadata.Description)
		if kube.IsK8sConflict(err) {
			return fail(foreignProjectMessage(name), "prerequisites")
		}
		if err != nil {
			return fail(fmt.Sprintf("Creating project %s failed: %v. Nothing was changed.", name, err), kube.ErrorCodeOr(err, "prerequisites"))
		}
		// UID from the Project in the ProjectRequest response (delegated Create
		// returns the new project, fetched by name in the same call)—the
		// identifier used in the snapshot, the label patch UID test, and
		// teardown preconditions.
		uid = created.UID
		if uid == "" {
			return fail(fmt.Sprintf("Project %s was created, but the API server's answer has no UID, so it is not recorded as the recipe's and Clean up will not delete it. Delete it in the console, then run the recipe again.", name), "prerequisites")
		}
		rec, err := readProjectRecord(c)
		if err == nil {
			err = writeProjectRecord(c, rec.with(types.RecipeOwnedProject{Name: name, UID: uid, Recipe: s.Recipe.Metadata.Name, CreatedAt: kube.NowRFC3339()}))
		}
		if err != nil {
			return fail(fmt.Sprintf("Project %s was created, but recording it as the recipe's failed (%v), so Clean up will not delete it. Delete it in the console, then run the recipe again.", name, err), kube.ErrorCodeOr(err, "prerequisites"))
		}
		s.Project = &types.RecipeProjectRef{Name: name, UID: uid, Owned: true}
	}
	if err := labelProject(c, name, uid, s.Recipe.Metadata.Name, kueue); err != nil {
		return fail(fmt.Sprintf("Project %s was created, but setting its labels failed: %v. Resume sets them again.", name, err), kube.ErrorCodeOr(err, "prerequisites"))
	}
	// The step ends once the user's project list has the project: the
	// steps after it read that list, and some start right away.
	if msg, code, err := waitUserSeesProject(c, name, emit); err != nil {
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Stopped while waiting for project %s to appear among your data science projects.", name), ErrorCode: "timeout"}, err
	} else if msg != "" {
		return fail(msg, code)
	}
	record.Action(c, "create-recipe-project", fmt.Sprintf("project=%s recipe=%s", name, s.Recipe.Metadata.Name), true)
	return &types.OperationResponse{Success: true, Message: fmt.Sprintf("Created project %s.", name), Logs: []string{"OK: Project " + name + " created (uid " + uid + ")"}}, nil
}

func projectName(p *types.RecipeProject) string {
	if p == nil {
		return ""
	}
	return p.Name
}

// requestProject creates a project through a ProjectRequest (what `oc
// new-project` and the dashboard do), and returns the metadata of the
// Project it answers with.
func requestProject(c *kube.Client, name, displayName, description string) (kube.ObjectMeta, error) {
	obj := map[string]any{
		"apiVersion": "project.openshift.io/v1",
		"kind":       "ProjectRequest",
		"metadata":   map[string]any{"name": name},
	}
	if displayName != "" {
		obj["displayName"] = displayName
	}
	if description != "" {
		obj["description"] = description
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return kube.ObjectMeta{}, err
	}
	body, _, err := c.Post(rhoai.ClusterPath("project.openshift.io/v1", "projectrequests", ""), data)
	if err != nil {
		return kube.ObjectMeta{}, err
	}
	return kube.ParseObjectMeta(body)
}

// The dashboard's settings: OdhDashboardConfig odh-dashboard-config in
// RHOAI's applications namespace (odh-dashboard backend/src/utils/
// resourceUtils.ts DASHBOARD_CONFIG, opendatahub.io/v1alpha). The
// dashboard merges it over its defaults (constants.ts blankDashboardCR),
// where dashboardConfig.disableKueue is true.
const (
	dashboardConfigAPI  = "opendatahub.io/v1alpha"
	dashboardConfigName = "odh-dashboard-config"
)

// kueueManaged reports whether the dashboard's Create project would label
// a new project kueue.openshift.io/managed=true: when its
// dashboardConfig.disableKueue is false (namespaceUtils.ts
// applyNamespaceChange, DSG_CREATION). Unset is the dashboard's default,
// true; no OdhDashboardConfig (or no RHOAI applications namespace) means
// no dashboard to follow, so no label. Read with the signed-in user's
// token; a read that fails otherwise is returned (fail closed).
func kueueManaged(c *kube.Client) (bool, error) {
	apps, err := rhoai.AppsNamespace(c)
	if errors.Is(err, rhoai.ErrNoDSCInitialization) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var cfg struct {
		Spec struct {
			DashboardConfig struct {
				DisableKueue *bool `json:"disableKueue"`
			} `json:"dashboardConfig"`
		} `json:"spec"`
	}
	found, err := c.GetIntoOptional(rhoai.NamespacedPath(dashboardConfigAPI, "odhdashboardconfigs", apps, dashboardConfigName), &cfg)
	if err != nil {
		return false, fmt.Errorf("read OdhDashboardConfig %s/%s: %w", apps, dashboardConfigName, err)
	}
	if !found || cfg.Spec.DashboardConfig.DisableKueue == nil {
		return false, nil
	}
	return !*cfg.Spec.DashboardConfig.DisableKueue, nil
}

// labelProject sets the dashboard's labels (as its backend does), the
// tool's label and the recipe annotation on namespace name, with a JSON
// patch whose first operation tests the namespace's UID: a namespace made
// again under the name (another UID) is not labelled.
func labelProject(c *kube.Client, name, uid, recipeName string, kueue bool) error {
	labels := map[string]string{dashboardProjectLabel: "true", kube.ManagedByLabelKey: kube.ManagedByLabelValue}
	if kueue {
		labels[kueueProjectLabel] = "true"
	}
	rawUID, _ := json.Marshal(uid)
	var err error
	for range labelPatchAttempts {
		var (
			meta  kube.ObjectMeta
			found bool
		)
		meta, found, err = kube.ReadObjectMeta(c, namespacePath(name))
		switch {
		case err != nil:
			return err
		case !found || meta.UID != uid:
			return fmt.Errorf("namespace %s is not the one the recipe created (its UID changed)", name)
		}
		// The resourceVersion read is a precondition (409 when another
		// writer changed the namespace since): an absent map is added
		// whole, which must not replace one added meanwhile.
		rawRV, _ := json.Marshal(meta.ResourceVersion)
		ops := []kube.JSONPatchOp{{Op: "test", Path: "/metadata/uid", Value: rawUID}, {Op: "replace", Path: "/metadata/resourceVersion", Value: rawRV}}
		ops = append(ops, setMapOps("/metadata/labels", meta.Labels == nil, labels)...)
		ops = append(ops, setMapOps("/metadata/annotations", meta.Annotations == nil, map[string]string{projectAnnotation: recipeName})...)
		patch, mErr := json.Marshal(ops)
		if mErr != nil {
			return mErr
		}
		_, _, err = c.Do(http.MethodPatch, namespacePath(name), "application/json-patch+json", patch, url.Values{"fieldManager": {envkit.ToolFieldManager}})
		if !kube.IsK8sConflict(err) {
			return err
		}
	}
	return err
}

// labelPatchAttempts bounds labelProject's tries when another writer
// changes the namespace between its read and its patch.
const labelPatchAttempts = 3

// setMapOps are the JSON patch operations that set entries of the string
// map at path (the whole map when it is absent).
func setMapOps(path string, absent bool, entries map[string]string) []kube.JSONPatchOp {
	if absent {
		raw, _ := json.Marshal(entries)
		return []kube.JSONPatchOp{{Op: "add", Path: path, Value: raw}}
	}
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var ops []kube.JSONPatchOp
	for _, k := range keys {
		raw, _ := json.Marshal(entries[k])
		ops = append(ops, kube.JSONPatchOp{Op: "add", Path: path + "/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(k), Value: raw})
	}
	return ops
}

// ProjectTeardownProblem says why Clean up does not delete recipe project
// name now, with its errorCode, or "" when it may: the project must be the
// one the user reviewed (uid, the UID the browser showed: the record and
// the namespace both still have it), carry the tool's label, and hold
// nothing the tool did not create that the user did not confirm
// (projectLeftovers). A project that is gone, or being deleted, needs
// nothing. A read that fails refuses (fail closed). CleanUp runs it
// before any part, and DeleteProject again just before the delete.
// Whenever it lists the project's leftovers it also returns all of them,
// confirmed or not, as confirmProjectContents takes them: a refusal names
// the unconfirmed ones, and the browser replaces its confirmation with the
// whole list.
func ProjectTeardownProblem(c *kube.Client, name, uid string, confirmed []string) (string, string, []string, error) {
	why, err := refusedProjectName(c, name)
	if err != nil {
		return "", "", nil, err
	}
	if why != "" {
		return why + " Clean up does not delete it.", "validation", nil, nil
	}
	if uid == "" {
		return fmt.Sprintf("The clean-up does not say which project %s you reviewed (its UID), so Clean up does not delete it. Reload the page and try again.", name), "validation", nil, nil
	}
	found, rec, err := findProject(c, name)
	if err != nil {
		return "", "", nil, err
	}
	r, recorded := rec.recorded(name)
	switch {
	case !recorded:
		return fmt.Sprintf("Project %s was not created by a recipe (there is no record of it), so Clean up does not delete it.", name), "validation", nil, nil
	case r.UID != uid || (found.found && found.meta.UID != uid):
		return fmt.Sprintf("Project %s is not the one you reviewed: it was deleted and made again since, so Clean up does not delete it. Reload the page to see the project there now.", name), "prerequisites", nil, nil
	case !found.found || found.meta.Terminating():
		return "", "", nil, nil
	case !found.deletable():
		return fmt.Sprintf("Project %s no longer carries this tool's label and the recipe annotation, so Clean up does not delete it.", name), "prerequisites", nil, nil
	}
	left, err := projectLeftovers(c, name)
	if err != nil {
		return "", "", nil, fmt.Errorf("this tool cannot tell what else is in project %s: %w", name, err)
	}
	var unconfirmed []string
	for _, l := range left {
		if !slices.Contains(confirmed, l) {
			unconfirmed = append(unconfirmed, l)
		}
	}
	if len(unconfirmed) > 0 {
		return fmt.Sprintf("Project %s still holds what this tool did not create: %s. Deleting the project deletes them too; confirm them to go ahead.", name, strings.Join(unconfirmed, ", ")), ErrorCodeConfirmContents, left, nil
	}
	return "", "", left, nil
}

// ProjectLeftovers lists what is in recipe project name that this tool did
// not create, for the confirmation; ErrorCodeConfirmContents refusals name
// the same strings.
func ProjectLeftovers(c *kube.Client, name string) ([]string, error) {
	return projectLeftovers(c, name)
}

// projectLeftovers names what is in project ns that this tool did not
// create, as far as it can list: workbenches (Notebooks), InferenceServices
// and LLMInferenceServices, PersistentVolumeClaims and pods. Objects with
// the tool's label, objects being deleted, the pods and claims of the
// tool's pipeline server (DSPO labels its pods dspa=<name>, and sets the
// DSPA as owner of what it creates) and the pods of the tool's models
// (KServe's part-of and name labels) are left out. A kind whose API is not
// served has none. A read that fails is returned (fail closed).
//
// The trust model: what leaves an object out is a label or field that an
// editor of the project can set (the tool's label, dspa=<name>, KServe's
// part-of and name labels on a pod of the tool's model). These are
// trusted: whoever forges one on an object can already delete that
// object, so the forgery only lets this tool delete, with the project,
// the forger's own object without asking. Nothing else in the project is
// listed either (Secrets, ConfigMaps, Deployments, ...): it goes with the
// project unlisted.
func projectLeftovers(c *kube.Client, ns string) ([]string, error) {
	toolDSPAs := map[string]bool{}
	toolUIDs := map[string]bool{}
	dspas, err := storage.ListDSPAs(c, ns)
	if err != nil && !errors.Is(err, storage.ErrDSPACRDMissing) {
		return nil, fmt.Errorf("list the pipeline servers in %s: %w", ns, err)
	}
	for _, d := range dspas {
		if d.ManagedByTool() {
			toolDSPAs[d.Meta.Name] = true
			toolUIDs[d.Meta.UID] = true
		}
	}
	ownedByTool := func(m kube.ObjectMeta) bool {
		for _, o := range m.OwnerReferences {
			if toolUIDs[o.UID] {
				return true
			}
		}
		return false
	}
	// The tool's models (LLMInferenceServices with its label), filled in as
	// they are listed, before the pods. KServe runs a model in pods of a
	// Deployment it owns and labels them part-of llminferenceservice and
	// name <model>, so a pod of the tool's model is the tool's.
	toolModels := map[string]bool{}
	toolModelPod := func(m kube.ObjectMeta) bool {
		return m.Labels["app.kubernetes.io/part-of"] == "llminferenceservice" && toolModels[m.Labels["app.kubernetes.io/name"]]
	}
	var out []string
	for _, l := range []struct{ kind, path string }{
		{"Notebook", rhoai.NamespacedPath("kubeflow.org/v1", "notebooks", ns, "")},
		{"InferenceService", rhoai.NamespacedPath("serving.kserve.io/v1beta1", "inferenceservices", ns, "")},
		{"LLMInferenceService", rhoai.NamespacedPath("serving.kserve.io/v1alpha2", "llminferenceservices", ns, "")},
		{"PersistentVolumeClaim", rhoai.NamespacedPath("v1", "persistentvolumeclaims", ns, "")},
		{"Pod", rhoai.NamespacedPath("v1", "pods", ns, "")},
	} {
		var list struct {
			Items []struct {
				Metadata kube.ObjectMeta `json:"metadata"`
			} `json:"items"`
		}
		found, err := c.GetIntoOptional(l.path, &list)
		if err != nil {
			if kube.IsDecodeError(err) {
				return nil, fmt.Errorf("parse the %s list: %w", l.kind, err)
			}
			return nil, fmt.Errorf("list the %ss in %s: %w", l.kind, ns, err)
		}
		if !found {
			continue // the API is not served
		}
		for _, it := range list.Items {
			m := it.Metadata
			if l.kind == "LLMInferenceService" && m.HasToolLabel() {
				toolModels[m.Name] = true
			}
			if m.HasToolLabel() || m.Terminating() || ownedByTool(m) || (l.kind == "Pod" && (toolDSPAs[m.Labels["dspa"]] || toolModelPod(m))) {
				continue
			}
			out = append(out, l.kind+" "+m.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// DeleteProject is Clean up's part for a recipe project: it deletes the
// namespace with the UID the user reviewed (uid, which the record also
// holds) as a precondition (kube.DeleteWithPreconditions), after
// ProjectTeardownProblem allows it, and forgets the record. A project
// that is gone or being deleted only loses its record.
func DeleteProject(c *kube.Client, name, uid string, confirmed []string) (*types.OperationResponse, error) {
	fail := func(msg, code string) (*types.OperationResponse, error) {
		record.Action(c, "delete-recipe-project", fmt.Sprintf("project=%s (%s)", name, msg), false)
		return &types.OperationResponse{Success: false, Message: msg, ErrorCode: code}, nil
	}
	problem, code, _, err := ProjectTeardownProblem(c, name, uid, confirmed)
	if err != nil {
		return fail(fmt.Sprintf("Kept project %s: %v.", name, err), kube.ErrorCodeOr(err, "prerequisites"))
	}
	if problem != "" {
		return fail(problem, code)
	}
	found, rec, err := findProject(c, name)
	if err != nil {
		return fail(fmt.Sprintf("Kept project %s: %v.", name, err), kube.ErrorCodeOr(err, "prerequisites"))
	}
	r, _ := rec.recorded(name)
	msg := fmt.Sprintf("Project %s is gone.", name)
	switch {
	case r.UID != uid || (found.found && found.meta.UID != uid):
		return fail(fmt.Sprintf("Kept project %s: it changed while it was checked (it was deleted and made again). Reload the page to see the project there now.", name), "prerequisites")
	case found.found && found.meta.Terminating():
		msg = fmt.Sprintf("Project %s is being deleted.", name)
	case found.found && !found.deletable():
		return fail(fmt.Sprintf("Kept project %s: it changed while it was checked (the tool's label or the recipe annotation).", name), "prerequisites")
	case found.found:
		// The reviewed UID (the record's too) and the resourceVersion
		// just checked as the preconditions: a namespace made again under
		// the name, or one whose label or annotation was removed since
		// this read, is not deleted (409).
		if _, err := kube.DeleteWithPreconditions(c, namespacePath(name), uid, found.meta.ResourceVersion); err != nil && !kube.IsK8sNotFound(err) {
			return fail(fmt.Sprintf("Deleting project %s failed: %v.", name, err), kube.ErrorCodeOr(err, "prerequisites"))
		}
		msg = fmt.Sprintf("Deleted project %s; OpenShift removes what it holds.", name)
	}
	if err := writeProjectRecord(c, rec.without(name)); err != nil {
		return fail(fmt.Sprintf("%s Forgetting it failed: %v.", msg, err), kube.ErrorCodeOr(err, "prerequisites"))
	}
	record.Action(c, "delete-recipe-project", "project="+name, true)
	return &types.OperationResponse{Success: true, Message: msg, Logs: []string{"OK: " + msg}}, nil
}

// OwnedProjects are the projects recipes created that the cluster has now:
// each recorded project whose namespace exists with the recorded UID, the
// tool's label and the recipe annotation (deletable), and is not being
// deleted, sorted by name. A recorded project that is gone, was made again
// under the name or lost its label is not listed: nothing is shown from
// the record alone. Read-only (the record is written only under the
// cluster lock: ForgetGoneProjects). A read that fails is returned, and
// nothing is listed (fail closed).
func OwnedProjects(c *kube.Client) ([]types.RecipeOwnedProject, error) {
	rec, err := readProjectRecord(c)
	if err != nil {
		return nil, err
	}
	var out []types.RecipeOwnedProject
	for _, r := range rec.Projects {
		meta, found, err := kube.ReadObjectMeta(c, namespacePath(r.Name))
		if err != nil {
			return nil, fmt.Errorf("read namespace %s: %w", r.Name, err)
		}
		p := projectFound{meta: meta, found: found, ours: found && meta.UID == r.UID}
		if p.deletable() && !meta.Terminating() {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ForgetGoneProjects drops from the record the projects that are gone: no
// namespace of the name, or one with another UID (made again; the one the
// recipe created is gone). A project being deleted, or whose namespace
// cannot be read, is kept (fail closed). It writes the record only when it
// forgets one, and returns the names it forgot. The record is one key
// written whole, so it runs only under the cluster lock, as the project
// step and Clean up write it: the background check (pkg/api
// maas_prune_job.go) runs it.
func ForgetGoneProjects(c *kube.Client) ([]string, error) {
	rec, err := readProjectRecord(c)
	if err != nil {
		return nil, err
	}
	kept := rec
	var forgot []string
	for _, r := range rec.Projects {
		meta, found, err := kube.ReadObjectMeta(c, namespacePath(r.Name))
		if err != nil || (found && meta.UID == r.UID) {
			continue
		}
		kept = kept.without(r.Name)
		forgot = append(forgot, r.Name)
	}
	if len(forgot) == 0 {
		return nil, nil
	}
	if err := writeProjectRecord(c, kept); err != nil {
		return nil, err
	}
	return forgot, nil
}
