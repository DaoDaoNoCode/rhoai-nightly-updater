package cluster

// Which token each request carries, and so which RBAC must grant it.
//
// Every change runs with the signed-in user's own token (pkg/api
// withMutationAuth builds the client with cluster.NewUserClient), so the
// API server checks it against that user's RBAC. The app's ServiceAccount
// keeps only:
//   - the reads of every handler outside withMutationAuth (the admin-only
//     reads, the cluster summary and the registry; pkg/api route_tiers.go),
//     and the probes;
//   - the background jobs main starts (the MaaS prune job);
//   - the updater's own records, reached through c.Bookkeeping() from any
//     path, and the lease and marker that pkg/api writes.
//
// Since 5.0.0 the reads of tier user-token (a user's projects, the
// Playground and MLflow samples status, a user's project) use the
// signed-in user's token too: their own functions are rbacUserTokenReads,
// and the access reviews the gen-ai API and MLflow make for them check the
// user (userTokenReviewRequests), not the ServiceAccount.
//
// deploy/template.yaml must grant exactly the ServiceAccount's part
// (rbac_coverage_test.go's two template tests), and no change may reach
// the ServiceAccount's token (TestMutationPathsActWithTheUsersToken). The gitops profile is checked
// against deploy/rbac-gitops.yaml by rbac_gitops_test.go: its Update still
// acts with the ServiceAccount, which its admission policy binds.

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// saReachPruned are nodes the reference graph reaches from a read only
// because a function value is named next to a read's (it is never called
// there). Each entry says why it is not reached at run time.
var saReachPruned = map[string]string{
	// The samples table (samples_mlflow.go mlflowSamples) names each
	// sample's add, remove and regenerate next to its status; the status
	// (GetMLflowSamplesStatus) calls only the status functions.
	mlflowPkg + ".addGenAIDemo":        "samples table: a change, run by RunMLflowSamples only",
	mlflowPkg + ".regenerateGenAIDemo": "samples table: a change, run by RunMLflowSamples only",
	mlflowPkg + ".addTraining":         "samples table: a change, run by RunMLflowSamples only",
	mlflowPkg + ".removeTraining":      "samples table: a change, run by RunMLflowSamples only",
	mlflowPkg + ".addMCP":              "samples table: a change, run by RunMLflowSamples only",
	mlflowPkg + ".removeMCP":           "samples table: a change, run by RunMLflowSamples only",
	mlflowPkg + ".addSkill":            "samples table: a change, run by RunMLflowSamples only",
	mlflowPkg + ".removeSkill":         "samples table: a change, run by RunMLflowSamples only",
	apiPkg + ".StartGitOpsPolicyWatch": "returns at once outside the gitops profile (rbac_coverage_test.go rbacGitOpsOnlyCalls)",
}

// mutationHandlerVars are the handler variables of pkg/api built with
// withMutationAuth: their work runs with the user's token.
func (pg *profileGraph) mutationHandlerVars() []string {
	var out []string
	for node, refs := range pg.graph {
		if strings.HasPrefix(node, "var "+apiPkg+".") && refs[apiPkg+".withMutationAuth"] {
			out = append(out, node)
		}
	}
	sort.Strings(out)
	return out
}

// saReach is what the full profile reaches with the ServiceAccount's
// token: main's roots without the mutation handlers (their client is the
// user's) and without the gitops profile's routes (that profile has its
// own RBAC).
func (pg *profileGraph) saReach() map[string]bool {
	pruned := map[string]bool{"var " + apiPkg + ".gitOpsRoutes": true}
	for _, v := range pg.mutationHandlerVars() {
		pruned[v] = true
	}
	for n := range saReachPruned {
		pruned[n] = true
	}
	return pg.graph.reach(pg.roots(), pruned)
}

var saRequestsMemo []rbacRequest

// templateSARequests are the requests deploy/template.yaml must grant the
// ServiceAccount.
func templateSARequests(t *testing.T) []rbacRequest {
	t.Helper()
	if saRequestsMemo == nil {
		pg := loadProfileGraph(t)
		reqs, _ := serviceAccountRequestsIn(t, pg.clusterFuncs(pg.saReach()))
		reqs = append(reqs, bookkeepingRequests(t)...)
		saRequestsMemo = reqs
	}
	return append([]rbacRequest(nil), saRequestsMemo...)
}

// userTokenReviewRequests are the access reviews other servers make for
// the status reads of tier user-token, which since 5.0.0 send the
// signed-in user's token: the Playground status's
// (genAIDashboardRequests) and MLflow's for the samples status (get and
// list checks).
func userTokenReviewRequests() []rbacRequest {
	out := playgroundServerRequests()
	for _, r := range samplesReviewRequests() {
		if r.Verb == "get" || r.Verb == "list" {
			out = append(out, r)
		}
	}
	return out
}

// The ServiceAccount is granted none of those reviews on their account:
// a review the template still grants is one the ServiceAccount's own reads
// need (the models and OGX servers that use a component, for example).
// The ones only those statuses needed (the samples' MLflow names, the
// gen-ai custom endpoints ConfigMap) are gone.
func TestUserTokenReviewsAreNotTheServiceAccounts(t *testing.T) {
	saReqs := templateSARequests(t)
	grants := templateGrants(t)
	for _, r := range userTokenReviewRequests() {
		granted := false
		for _, g := range grants {
			granted = granted || g.allows(r)
		}
		if !granted {
			continue
		}
		needed := false
		for _, s := range saReqs {
			needed = needed || (s.Verb == r.Verb && s.Group == r.Group && s.Resource == r.Resource && (s.Name == r.Name || s.Name == anyNS || r.Name == ""))
		}
		if !needed {
			t.Errorf("the template grants %s for a status read made with the user's token (%s)", r.key(), r.Site)
		}
	}
	for _, r := range []rbacRequest{
		{Verb: "get", Group: "", Resource: "configmaps", Namespace: "p", Name: "gen-ai-aa-custom-model-endpoints"},
		{Verb: "get", Group: mlflowAuthGroup, Resource: "experiments", Namespace: "p", Name: "nightly-sample-training"},
		{Verb: "get", Group: mlflowAuthGroup, Resource: "skills", Namespace: "p", Name: "nightly-sample-mcp-builder"},
	} {
		for _, g := range grants {
			if g.allows(r) {
				t.Errorf("%s grants %s, which only a user's status read needs", g.source, r.key())
			}
		}
	}
}

// pathTo is a shortest path from roots to target in the graph, avoiding
// pruned, for failure messages.
func (g refGraph) pathTo(roots []string, pruned map[string]bool, target string) []string {
	parent := map[string]string{}
	seen := map[string]bool{}
	queue := []string{}
	for _, r := range roots {
		if !pruned[r] {
			seen[r] = true
			queue = append(queue, r)
		}
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if n == target {
			var path []string
			for cur := n; ; cur = parent[cur] {
				path = append([]string{cur}, path...)
				if _, ok := parent[cur]; !ok {
					break
				}
			}
			return path
		}
		next := make([]string, 0, len(g[n]))
		for m := range g[n] {
			next = append(next, m)
		}
		sort.Strings(next)
		for _, m := range next {
			if !seen[m] && !pruned[m] {
				seen[m] = true
				parent[m] = n
				queue = append(queue, m)
			}
		}
	}
	return nil
}

// mutationMachinery is what withMutationAuth runs around a change with the
// ServiceAccount on purpose: the wrapper itself (it builds both clients),
// and the lease, marker and lock bookkeeping.
var mutationMachinery = []string{
	apiPkg + ".withMutationAuth", apiPkg + ".mutationAuth",
	apiPkg + ".lockCluster", apiPkg + ".beginOperation", apiPkg + ".releaseClusterMutationLock",
}

// saTokenSources are the only ways code gets the ServiceAccount's token or
// a client with it: a change must reach none of them.
var saTokenSources = []string{
	apiPkg + ".getClusterToken",
	clusterPkg + ".NewClientWithContext",
	kubePkg + ".NewClientWithContext",
}

// No change can act with the ServiceAccount's token: from the handlers
// built with withMutationAuth, past the wrapper's own bookkeeping, the
// graph reaches no source of that token. (A change's records still use it,
// through c.Bookkeeping(), which TestOwnConfigMapsAreReachedOnlyThroughBookkeeping
// limits to the updater's own ConfigMaps.)
func TestMutationPathsActWithTheUsersToken(t *testing.T) {
	pg := loadProfileGraph(t)
	roots := pg.mutationHandlerVars()
	if len(roots) < 30 {
		t.Fatalf("only %d mutation handlers found: %v", len(roots), roots)
	}
	pruned := map[string]bool{}
	for _, n := range mutationMachinery {
		if _, ok := pg.graph[n]; !ok {
			t.Fatalf("%s is not in the graph; update mutationMachinery", n)
		}
		pruned[n] = true
	}
	reach := pg.graph.reach(roots, pruned)
	for _, src := range saTokenSources {
		if _, ok := pg.graph[src]; !ok {
			t.Errorf("%s is not in the graph; update saTokenSources", src)
		}
		if reach[src] {
			t.Errorf("a change reaches the ServiceAccount's token through %s:\n  %s", src, strings.Join(pg.graph.pathTo(roots, pruned, src), "\n  -> "))
		}
	}
	// The check can fail: the wrapper itself does build a ServiceAccount
	// client (the bookkeeping one).
	if !pg.graph.reach(roots, nil)[apiPkg+".getClusterToken"] {
		t.Fatal("the graph does not see the wrapper's own ServiceAccount client; the check above proves nothing")
	}
}

// What a leaked ServiceAccount token can still change: only the updater's
// own records in its namespace, and objects named in the template (the
// MaaS prune job's). No unnamed write anywhere else, no write on Secrets,
// RBAC, workloads or namespaces.
func TestServiceAccountWritesAreNamedOrItsOwnRecords(t *testing.T) {
	writes := map[string]bool{"create": true, "update": true, "patch": true, "delete": true, "deletecollection": true, "*": true, "escalate": true, "bind": true, "impersonate": true}
	var lines []string
	for _, g := range templateGrants(t) {
		if g.source == "built-in basic-user" {
			continue
		}
		for _, v := range g.rule.Verbs {
			if !writes[v] {
				continue
			}
			ownRecords := g.namespace == appNSMarker && fmt.Sprint(g.rule.Resources) == "[configmaps]"
			lines = append(lines, fmt.Sprintf("%s %v/%v %v ns=%q", v, g.rule.APIGroups, g.rule.Resources, g.rule.ResourceNames, g.namespace))
			if !ownRecords && len(g.rule.ResourceNames) == 0 {
				t.Errorf("%s: unnamed %s on %v/%v outside the updater's own records", g.source, v, g.rule.APIGroups, g.rule.Resources)
			}
			for _, res := range g.rule.Resources {
				switch res {
				case "secrets", "roles", "rolebindings", "clusterroles", "clusterrolebindings", "serviceaccounts", "namespaces",
					"deployments", "pods", "jobs", "subscriptions", "installplans", "clusterserviceversions", "catalogsources",
					"validatingwebhookconfigurations", "mutatingwebhookconfigurations", "customresourcedefinitions", "datascienceclusters",
					// The OAuth client users sign in with, and their tokens:
					// only install.sh and the chart's short-lived hooks write
					// them (SECURITY.md, The OAuthClient).
					"oauthclients", "oauthaccesstokens", "useroauthaccesstokens":
					t.Errorf("%s: %s on %s", g.source, v, res)
				}
			}
		}
	}
	sort.Strings(lines)
	t.Logf("the ServiceAccount's write grants:\n%s", strings.Join(lines, "\n"))
}
