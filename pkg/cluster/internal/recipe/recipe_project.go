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

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The recipe-owned project.
//
// The tool never creates a project, with one exception: a recipe with
// spec.project.mode create. It creates the project the way the dashboard's
// Create project does, with the signed-in user's token, so the cluster's
// project template and the requester's admin binding apply:
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
// The recipe sets the dashboard's labels the same way: the Kueue label
// exactly when the dashboard would, read from the dashboard's own
// OdhDashboardConfig (kueueManaged); in addition the tool's label and the
// annotation projectAnnotation naming the recipe.
//
// The tool records the project's name and the UID the ProjectRequest
// answered with (projectRecordKey of the snapshot ConfigMap, written
// before the labels, so a run cut off between the two still knows the
// project is its own), and the label patch is a JSON patch that tests that
// UID first, so it never labels a namespace made again under the name.
// Clean up deletes it only while it still has that UID, the tool's label
// and the recipe annotation, is not being deleted, and holds nothing else
// the tool can list, unless the user confirmed those (DeleteProject).
const (
	projectAnnotation = "rhoai-nightly-updater/recipe"
	// dashboardProjectLabel is odh-dashboard's label of its projects
	// (KnownLabels.DASHBOARD_RESOURCE; the selector of envkit.DSProjects).
	dashboardProjectLabel = "opendatahub.io/dashboard"
	// kueueProjectLabel is odh-dashboard's KnownLabels.KUEUE_MANAGED.
	kueueProjectLabel = "kueue.openshift.io/managed"

	projectRecordKey     = "recipeProject"
	projectRecordManager = "rhoai-nightly-updater-recipe-projects"
	// maxRecordedProjects bounds the record.
	maxRecordedProjects = 50
)

// ErrorCodeConfirmContents is the errorCode of a project clean-up that
// needs the user to confirm what the project still holds.
const ErrorCodeConfirmContents = "confirm_project_contents"

// projectRecord is the snapshot key's value.
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

// recorded returns the record of project name.
func (r projectRecord) recorded(name string) (types.RecipeOwnedProject, bool) {
	for _, p := range r.Projects {
		if p.Name == name {
			return p, true
		}
	}
	return types.RecipeOwnedProject{}, false
}

// with is r with p recorded (replacing a record of the same name).
func (r projectRecord) with(p types.RecipeOwnedProject) projectRecord {
	out := projectRecord{Projects: slices.DeleteFunc(slices.Clone(r.Projects), func(o types.RecipeOwnedProject) bool { return o.Name == p.Name })}
	out.Projects = append(out.Projects, p)
	return out
}

// without is r without a record of name.
func (r projectRecord) without(name string) projectRecord {
	return projectRecord{Projects: slices.DeleteFunc(slices.Clone(r.Projects), func(o types.RecipeOwnedProject) bool { return o.Name == name })}
}

func namespacePath(name string) string { return rhoai.ClusterPath("v1", "namespaces", name) }

// refusedProjectName says why a recipe must not create (or delete) project
// name, "" when it may: a name of Kubernetes, OpenShift, Red Hat products
// or the tool's storage (reservedProjectName), the updater's own
// namespace, the RHOAI operator's, and RHOAI's applications and monitoring
// namespaces as the DSCInitialization names them. When the
// DSCInitialization cannot be read it returns the error (fail closed);
// without one RHOAI has no such namespace yet.
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

// projectFix: the recipe's spec.project is to change (or, for a project
// being deleted, to wait).
func projectFix() *types.RecipeFix { return fixAt(types.RecipeFixProjectForeign, "") }

// foreignProjectMessage refuses a project the recipe did not create.
func foreignProjectMessage(name string) string {
	return fmt.Sprintf("Project %s exists and was not created by a recipe. Choose another name, or set project.mode: existing to use it as it is. Nothing was changed.", name)
}

// projectFound is a project as checked: the namespace's metadata, and
// whether the recipe created it (its UID is recorded).
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

// complete reports whether a project the recipe created carries every
// label and annotation the recipe sets.
func (p projectFound) complete() bool {
	return p.meta.HasToolLabel() && p.meta.Labels[dashboardProjectLabel] == "true" && p.meta.Annotations[projectAnnotation] != ""
}

// deletable reports whether Clean up may delete a project found as p: the
// recipe's (recorded UID), with the tool's label and the recipe
// annotation, which only a writer of the namespace (not the
// ServiceAccount, which can write the record) can set.
func (p projectFound) deletable() bool {
	return p.found && p.ours && p.meta.HasToolLabel() && p.meta.Annotations[projectAnnotation] != ""
}

// checkProjectStep is the project step's check. A project of mode existing
// must be a data science project of the user's; the recipe owns nothing of
// it. A project of mode create is done when the recipe created it (its UID
// is recorded) and it carries the labels; it is refused when it is someone
// else's, is being deleted, or has a reserved name.
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
		// Gone (or never made): a project a run recorded earlier is not
		// this one; it is created again.
		s.Project = nil
		return Check{}, nil
	case !found.ours:
		return Check{Blocked: foreignProjectMessage(p.Name), Fix: projectFix()}, nil
	case found.meta.Terminating():
		return Check{Blocked: fmt.Sprintf("Project %s, which a recipe created, is being deleted; wait until it is gone, then run the recipe again.", p.Name), Fix: projectFix()}, nil
	}
	s.Project = &types.RecipeProjectRef{Name: p.Name, UID: found.meta.UID, Owned: true}
	if !found.complete() {
		return Check{}, nil // the labels are set again
	}
	return Check{Done: true, Reason: fmt.Sprintf("Project %s was created by a recipe.", p.Name)}, nil
}

// EnsureProject creates the recipe's project (mode create) as the
// dashboard does, with the signed-in user's token (see the package's notes
// above), records it, and labels it. A project the recipe created before
// keeps its UID and gets its labels set again; any other refuses (fail
// closed: checkProjectStep's rules).
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
	// The dashboard's Kueue choice, before anything is created.
	kueue, err := kueueManaged(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the dashboard's settings to label project %s as the dashboard does: %v. Nothing was changed.", name, err), kube.ErrorCodeOr(err, "prerequisites"))
	}
	var uid string
	if s.Project != nil && s.Project.Owned && s.Project.UID != "" {
		uid = s.Project.UID // created before (its UID recorded); only its labels are missing
	} else {
		emit(types.UpdateStepEvent{Step: StepProject, Status: "running", Message: fmt.Sprintf("Creating project %s...", name)})
		created, err := requestProject(c, name, p.DisplayName, s.Recipe.Metadata.Description)
		if kube.IsK8sConflict(err) {
			return fail(foreignProjectMessage(name), "prerequisites")
		}
		if err != nil {
			return fail(fmt.Sprintf("Creating project %s failed: %v. Nothing was changed.", name, err), kube.ErrorCodeOr(err, "prerequisites"))
		}
		// The UID of the Project the API server answered the request with
		// (openshift-apiserver projectrequest/delegated Create returns the
		// project it created, read by name in the same request): the
		// identity the record, the label patch's test and Clean up's
		// precondition all use.
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
// name now, with its errorCode, or "" when it may: the project must be
// recorded as a recipe's with its current UID, carry the tool's label, and
// hold nothing the tool did not create that the user did not confirm
// (projectLeftovers). A project that is gone, or being deleted, needs
// nothing. A read that fails refuses (fail closed). CleanUp runs it
// before any part, and DeleteProject again just before the delete. With
// ErrorCodeConfirmContents it also returns what the user has not confirmed,
// as confirmProjectContents takes it.
func ProjectTeardownProblem(c *kube.Client, name string, confirmed []string) (string, string, []string, error) {
	why, err := refusedProjectName(c, name)
	if err != nil {
		return "", "", nil, err
	}
	if why != "" {
		return why + " Clean up does not delete it.", "validation", nil, nil
	}
	found, rec, err := findProject(c, name)
	if err != nil {
		return "", "", nil, err
	}
	r, recorded := rec.recorded(name)
	switch {
	case !recorded:
		return fmt.Sprintf("Project %s was not created by a recipe (there is no record of it), so Clean up does not delete it.", name), "validation", nil, nil
	case !found.found || found.meta.Terminating():
		return "", "", nil, nil
	case found.meta.UID != r.UID:
		return fmt.Sprintf("Project %s is not the one a recipe created (it was deleted and made again), so Clean up does not delete it.", name), "prerequisites", nil, nil
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
		return fmt.Sprintf("Project %s still holds what this tool did not create: %s. Deleting the project deletes them too; confirm them to go ahead.", name, strings.Join(unconfirmed, ", ")), ErrorCodeConfirmContents, unconfirmed, nil
	}
	return "", "", nil, nil
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
// the tool's label, objects being deleted, and the pods and claims of the
// tool's pipeline server (DSPO labels its pods dspa=<name>, and sets the
// DSPA as owner of what it creates) are left out. A kind whose API is not
// served has none. A read that fails is returned (fail closed).
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
			if m.HasToolLabel() || m.Terminating() || ownedByTool(m) || (l.kind == "Pod" && toolDSPAs[m.Labels["dspa"]]) {
				continue
			}
			out = append(out, l.kind+" "+m.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// DeleteProject is Clean up's part for a recipe project: it deletes the
// namespace with its recorded UID as a precondition (kube.DeleteWithUID),
// after ProjectTeardownProblem allows it, and forgets the record. A project
// that is gone or being deleted only loses its record.
func DeleteProject(c *kube.Client, name string, confirmed []string) (*types.OperationResponse, error) {
	fail := func(msg, code string) (*types.OperationResponse, error) {
		record.Action(c, "delete-recipe-project", fmt.Sprintf("project=%s (%s)", name, msg), false)
		return &types.OperationResponse{Success: false, Message: msg, ErrorCode: code}, nil
	}
	problem, code, _, err := ProjectTeardownProblem(c, name, confirmed)
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
	case found.found && found.meta.Terminating():
		msg = fmt.Sprintf("Project %s is being deleted.", name)
	case found.found && (!found.deletable() || found.meta.UID != r.UID):
		return fail(fmt.Sprintf("Kept project %s: it changed while it was checked (its UID, the tool's label or the recipe annotation).", name), "prerequisites")
	case found.found:
		// The recorded UID and the resourceVersion just checked as the
		// preconditions: a namespace made again under the name, or one
		// whose label or annotation was removed since this read, is not
		// deleted (409).
		if _, err := kube.DeleteWithPreconditions(c, namespacePath(name), r.UID, found.meta.ResourceVersion); err != nil && !kube.IsK8sNotFound(err) {
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
