package cluster

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The project actions' requests, as the RBAC scan derives them from their
// code (project_actions.go): the preflight must review exactly what the
// code can send with the user's token.

// projectActionRoots are the entry points of each project action's code:
// the scan of an action covers what they reach in the reference graph.
// A route's dispatcher (workbench.Run, features.RunPlayground,
// mlflow.RunSamples) is not a root when it dispatches to several actions:
// the action's own method is.
var projectActionRoots = map[string][]string{
	"workbench.setup":          {clusterPkg + "/internal/workbench.run.setup"},
	"workbench.start":          {clusterPkg + "/internal/workbench.run.start"},
	"workbench.stop":           {clusterPkg + "/internal/workbench.run.stop"},
	"workbench.teardown":       {clusterPkg + "/internal/workbench.run.teardown"},
	"workbench.delete-storage": {clusterPkg + "/internal/workbench.run.deleteKeptStorage"},
	"playground.setup":         {clusterPkg + "/internal/features.playgroundRun.setup"},
	"playground.teardown":      {clusterPkg + "/internal/features.playgroundRun.teardown"},
	// The samples' Kubernetes requests are the run's own (its project
	// check); MLflow's access reviews are the hand list.
	"mlflow-samples.add":    {mlflowPkg + ".RunSamples"},
	"mlflow-samples.remove": {mlflowPkg + ".RunSamples"},
}

// projectActionPruned are what a project action's code names but does not
// run for a user who is not a cluster-admin, with why. The facts roots
// (what c.Facts() is passed to) are pruned as well: they read with the
// ServiceAccount's token.
var projectActionPruned = map[string]string{
	registryPkg + ".EnsureRegistryPullCopyFromPullSecret": "workbench registryPullCopy: a project user's setup does not write the internal registry's copy (it is in the applications namespace)",
	mlflowPkg + ".Target.raiseOldMemoryLimit":             "demoAddOrRegenerate: refused to a project user (demoLowMemoryForUserMessage); it patches the cluster-wide MLflow CR",
}

// projectClusterReads are the cluster-scoped requests a project action may
// send with the user's token: reads OpenShift grants every signed-in user,
// and a read of the project's own namespace.
var projectClusterReads = map[string]string{
	"list storage.k8s.io/storageclasses name=": "OpenShift basic-user (openshift-apiserver release-4.17 policy.go:460, bound to system:authenticated at :766)",
	"list project.openshift.io/projects name=": "OpenShift basic-user (openshift-apiserver release-4.17 policy.go:461, bound at :766)",
	"get /namespaces name=" + ProjectNameSelf:  "the project's own namespace: Kubernetes view/edit/admin read \"this namespace\" (v1.31.0 policy.go:341-344)",
}

// projectActionScan is the scan of action id's code: its requests (key
// without the namespace) and the problems of the scan.
func projectActionScan(t *testing.T, id string) []rbacRequest {
	t.Helper()
	roots, ok := projectActionRoots[id]
	if !ok {
		t.Fatalf("project action %s has no roots in projectActionRoots", id)
	}
	pg := loadProfileGraph(t)
	pruned := map[string]bool{}
	for n := range projectActionPruned {
		pruned[n] = true
	}
	for _, f := range pg.factsRoots {
		pruned[f] = true
	}
	p := loadClusterSource(t)
	p.projectAction = true
	reqs, _, problems := p.scan(pg.clusterFuncs(pg.graph.reach(roots, pruned)), false)
	if len(problems) > 0 {
		t.Fatalf("cannot derive the requests of %s:\n%s", id, strings.Join(problems, "\n"))
	}
	return reqs
}

// actionKey is a request's key without its namespace: the golden file
// records runtime namespaces as "*", so the scan cannot tell the project
// from the applications namespace; the table's Scope does.
func actionKey(verb, group, resource, name string) string {
	return fmt.Sprintf("%s %s/%s name=%s", verb, group, resource, name)
}

func scannedKeys(t *testing.T, id string) []string {
	var out []string
	for _, r := range projectActionScan(t, id) {
		name := r.Name
		if r.Resource == "namespaces" && r.Group == "" && name == anyNS {
			name = ProjectNameSelf
		}
		out = append(out, actionKey(r.Verb, r.Group, r.Resource, name))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func tableKeys(a ProjectAction) []string {
	var out []string
	for _, r := range a.Requests {
		if r.Source == SourceScan {
			out = append(out, actionKey(r.Verb, r.Group, r.Resource, r.Name))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// TestProjectActionPreflightMatchesTheScan: each project action users may
// run reviews exactly the requests the scan derives from its code, no gap
// and no extra. A new API call in a project action fails it until the
// table (project_actions.go) and the golden file are updated.
func TestProjectActionPreflightMatchesTheScan(t *testing.T) {
	for _, a := range ProjectActions() {
		if !a.ForUsers {
			if len(a.Requests) > 0 {
				t.Errorf("%s is a cluster-admin's, yet lists requests", a.ID)
			}
			continue
		}
		got, want := tableKeys(a), scannedKeys(t, a.ID)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: the preflight table differs from the scan of its code:\n%s", a.ID, lineDiff(strings.Join(want, "\n"), strings.Join(got, "\n")))
		}
	}
	for id := range projectActionRoots {
		if a, ok := ProjectActionByID(id); !ok || !a.ForUsers {
			t.Errorf("projectActionRoots lists %s, which is not a project action users may run", id)
		}
	}
}

// TestProjectActionsReachNoClusterRequestOnTheUserToken: no project
// action sends a cluster-scoped request with the user's token, but the
// reads every user may make (projectClusterReads); every other request
// goes to the project or RHOAI's applications namespace.
func TestProjectActionsReachNoClusterRequestOnTheUserToken(t *testing.T) {
	for _, a := range ProjectActions() {
		for _, r := range a.Requests {
			key := actionKey(r.Verb, r.Group, r.Resource, r.Name)
			switch r.Scope {
			case ScopeProject, ScopeApps:
				if r.Resource == "namespaces" && r.Group == "" && r.Name != ProjectNameSelf {
					t.Errorf("%s: %s reads a namespace other than the project", a.ID, key)
				}
			case ScopeCluster:
				if _, ok := projectClusterReads[key]; !ok {
					t.Errorf("%s: %s is cluster-scoped and not a read every user may make", a.ID, key)
				}
			default:
				t.Errorf("%s: %s has no scope", a.ID, key)
			}
		}
		for _, r := range projectActionScanIfUsers(t, a) {
			if r.Namespace == "" && r.Resource != "namespaces" {
				if _, ok := projectClusterReads[actionKey(r.Verb, r.Group, r.Resource, r.Name)]; !ok {
					t.Errorf("%s: its code sends the cluster-scoped %s", a.ID, r.key())
				}
			}
		}
	}
}

func projectActionScanIfUsers(t *testing.T, a ProjectAction) []rbacRequest {
	if !a.ForUsers {
		return nil
	}
	return projectActionScan(t, a.ID)
}

// gateRequests are what the project actions' gate and capabilities send
// with the user's token for every action, before the action's own
// requests (pkg/api withProjectMutationAuth, HandleProjectCapabilities):
// fixed, listed in the golden file for review with the rest.
var gateRequests = []string{
	"get user.openshift.io/users name=~ (identity, authz.go LookupUserWithToken)",
	"create authorization.openshift.io/subjectaccessreviews */*/* cluster-wide (the role, authz.go CheckUserPermissionWithToken)",
	"get oauth.openshift.io/useroauthaccesstokens name=<the token's object> (the sign-in, authz.go ReadSessionToken)",
	"list project.openshift.io/projects labelSelector=opendatahub.io/dashboard=true (the user's project, user_projects.go ListUserDSProjects)",
	"get /namespaces name=<the project> (its UID, the lock's key, access_review.go ProjectUID)",
	"create authorization.k8s.io/selfsubjectaccessreviews, one per request below (the preflight, access_review.go reviewOne)",
	"get /namespaces name=<the project>, get opendatahub.io/odhdashboardconfigs in apps (workbench setup only: whether it reads LocalQueues, workbench_status.go QueuesNeeded)",
}

// writeProjectActionRequests is the golden file's "project action
// requests" section: the gate's own requests, then per action users may
// run the requests the scan derives from its code, each with the class of
// its namespace (project, apps: RHOAI's applications namespace from the
// facts, cluster: cluster-scoped), and the hand-listed ones with their
// source. A scanned request in a literal namespace other than the
// project's fails: a project action acts in the project.
func writeProjectActionRequests(t *testing.T, b *strings.Builder) {
	t.Helper()
	actions := ProjectActions()
	sort.Slice(actions, func(i, j int) bool { return actions[i].ID < actions[j].ID })
	n := 0
	for _, a := range actions {
		if a.ForUsers {
			n++
		}
	}
	fmt.Fprintf(b, "\n## project action requests (%d actions)\n", n)
	fmt.Fprintf(b, "### gate requests (every action) (%d)\n", len(gateRequests))
	for _, g := range gateRequests {
		b.WriteString(g + "\n")
	}
	for _, a := range actions {
		if !a.ForUsers {
			continue
		}
		scopes := map[string][]string{}
		when := map[string]string{}
		for _, r := range a.Requests {
			if r.Source == SourceScan {
				k := actionKey(r.Verb, r.Group, r.Resource, r.Name)
				scopes[k] = append(scopes[k], r.Scope)
				if r.When != "" {
					when[k] = " when=" + r.When
				}
			}
		}
		var rows []string
		for _, r := range projectActionScan(t, a.ID) {
			name := r.Name
			if r.Resource == "namespaces" && r.Group == "" && name == anyNS {
				name = ProjectNameSelf
			}
			k := actionKey(r.Verb, r.Group, r.Resource, name)
			switch r.Namespace {
			case anyNS:
				for _, sc := range scopes[k] {
					rows = append(rows, fmt.Sprintf("%s ns=%s scan%s", k, sc, when[k]))
				}
				if len(scopes[k]) == 0 {
					rows = append(rows, fmt.Sprintf("%s ns=? scan", k))
				}
			case "":
				// Cluster-scoped in the API: a read every user may make,
				// or the project's own namespace (by its name: RBAC
				// decides it in the project).
				sc := ScopeCluster
				if name == ProjectNameSelf {
					sc = ScopeProject
				}
				rows = append(rows, fmt.Sprintf("%s ns=%s scan", k, sc))
			default:
				t.Errorf("%s: the scan derives %s in the literal namespace %q, not the project", a.ID, k, r.Namespace)
			}
		}
		for _, r := range a.Requests {
			if r.Source != SourceScan {
				rows = append(rows, fmt.Sprintf("%s ns=%s %s", actionKey(r.Verb, r.Group, r.Resource, r.Name), r.Scope, r.Source))
			}
		}
		slices.Sort(rows)
		rows = slices.Compact(rows)
		fmt.Fprintf(b, "### %s (%d)\n", a.ID, len(rows))
		for _, r := range rows {
			b.WriteString(r + "\n")
		}
	}
}
