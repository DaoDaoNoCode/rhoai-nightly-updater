package cluster

// The cluster facts client (kube.Client.Facts): a change reads a few
// cluster-wide facts that are no user data with the ServiceAccount's
// token, through a read-only copy of its client. These tests prove that
// the copy only reads, that the ServiceAccount's RBAC scan covers every
// read it makes (so deploy/template.yaml already grants them: the facts
// add no permission), and that it never hands its token to another server.

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/types"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// factsQueries are the Client methods that may be called on c.Facts()
// directly: none sends a request.
var factsQueries = map[string]bool{"ActsAsUser": true, "BaseURL": true, "Context": true, "Username": true}

// factsCalls finds every call of kube.Client.Facts in tp. A facts client
// must be passed directly to the function that reads with it (or asked one
// of factsQueries): roots are those functions (and function variables),
// so the reference graph follows what they read; problems are the uses it
// cannot follow (stored in a variable, a request method called on it).
func factsCalls(tp *typedPkg) (roots, problems []string) {
	isFacts := func(e ast.Expr) *ast.CallExpr {
		call, ok := ast.Unparen(e).(*ast.CallExpr)
		if !ok {
			return nil
		}
		sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
		if !ok {
			return nil
		}
		s := tp.info.Selections[sel]
		if s == nil {
			return nil
		}
		if fn, ok := s.Obj().(*types.Func); ok && funcKey(fn) == kubePkg+".Client.Facts" {
			return call
		}
		return nil
	}
	callee := func(fun ast.Expr) string {
		var obj types.Object
		switch f := ast.Unparen(fun).(type) {
		case *ast.Ident:
			obj = tp.info.Uses[f]
		case *ast.SelectorExpr:
			if s := tp.info.Selections[f]; s != nil {
				obj = s.Obj()
			} else {
				obj = tp.info.Uses[f.Sel]
			}
		}
		switch o := obj.(type) {
		case *types.Func:
			return funcKey(o)
		case *types.Var:
			if v, ok := packageLevelVar(o); ok {
				return varKey(v)
			}
		}
		return ""
	}
	names := make([]string, 0, len(tp.files))
	for name := range tp.files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f := tp.files[name]
		followed := map[*ast.CallExpr]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			for _, arg := range call.Args {
				if fc := isFacts(arg); fc != nil {
					followed[fc] = true
					if key := callee(call.Fun); key != "" {
						roots = append(roots, key)
					} else {
						problems = append(problems, fmt.Sprintf("%s:%d: c.Facts() is passed to a call the scan cannot name; call a function or method directly", name, tp.fset.Position(call.Pos()).Line))
					}
				}
			}
			if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
				if fc := isFacts(sel.X); fc != nil {
					followed[fc] = true
					if !factsQueries[sel.Sel.Name] {
						problems = append(problems, fmt.Sprintf("%s:%d: c.Facts().%s: pass c.Facts() to the function that reads with it, so the RBAC scan follows the read", name, tp.fset.Position(call.Pos()).Line, sel.Sel.Name))
					}
				}
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			if e, ok := n.(ast.Expr); ok {
				if fc := isFacts(e); fc != nil && !followed[fc] {
					problems = append(problems, fmt.Sprintf("%s:%d: c.Facts() is stored or returned; pass it directly to the function that reads with it, so the RBAC scan follows the read", name, tp.fset.Position(fc.Pos()).Line))
				}
			}
			return true
		})
	}
	return roots, problems
}

// The facts roots the code has today: the collector finds them, and the
// ServiceAccount's scan (saReach) starts from them too.
var wantFactsRoots = []string{
	rhoaiPkg + ".ReadFirstDSC",
	rhoaiPkg + ".ReadControlPlane",
	rhoaiPkg + ".readRHOAINamespaces",
	mlflowPkg + ".ReadCR",
	clusterPkg + "/internal/dashdev.readComponentURL",
	clusterPkg + "/internal/features.playgroundClusterParts",
	"var " + clusterPkg + "/internal/features.genAIBaseURL",
	recordPkg + ".namespaceUID",
}

// TestFactsClientOnlyReads: the facts client sends nothing but GET and
// HEAD, at run time (every write method fails with ErrFactsReadOnly
// before a request leaves) and in the code (every request the RBAC scan
// derives in what a facts client is passed to is a get, list or watch).
// It hands its token to nobody: its BearerToken is empty, and nothing it
// reaches calls BearerToken or keys a cache by a token (kube.TokenKey).
func TestFactsClientOnlyReads(t *testing.T) {
	t.Run("at run time", func(t *testing.T) {
		f, sa := newFakeAPI(t)
		f.json("GET", "/api/v1/namespaces/x", http.StatusOK, `{"metadata":{"name":"x"}}`)
		user := newTestClient(testClientOptions{BaseURL: sa.BaseURL(), Token: "user-token", HTTPClient: sa.TestOptions().HTTPClient, Ctx: context.Background(), AsUser: true})
		user.SetBookkeeping(sa)
		user.SetFacts(sa)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for name, fc := range map[string]*Client{
			"Facts":                      user.Facts(),
			"Facts().WithContext":        user.Facts().WithContext(ctx),
			"Facts().NoRetryWhen":        user.Facts().NoRetryWhen(func(error) bool { return true }),
			"Facts().Bookkeeping":        user.Facts().Bookkeeping(),
			"WithContext(...).Facts":     user.WithContext(ctx).Facts(),
			"the ServiceAccount's Facts": sa.Facts(),
		} {
			before := f.requestCount()
			writes := map[string]func() error{
				"Post":           func() error { _, _, err := fc.Post("/api/v1/namespaces", []byte("{}")); return err },
				"Put":            func() error { _, _, err := fc.Put("/api/v1/namespaces/x", []byte("{}")); return err },
				"Patch":          func() error { _, _, err := fc.Patch("/api/v1/namespaces/x", []byte("{}")); return err },
				"StrategicPatch": func() error { _, _, err := fc.StrategicPatch("/api/v1/namespaces/x", []byte("{}")); return err },
				"Apply":          func() error { _, _, err := fc.Apply("/api/v1/namespaces/x", map[string]any{}); return err },
				"DryRunApply":    func() error { _, _, err := fc.DryRunApply("/api/v1/namespaces/x", map[string]any{}); return err },
				"Delete":         func() error { _, err := fc.Delete("/api/v1/namespaces/x"); return err },
				"PatchJSON":      func() error { return fc.PatchJSON("/api/v1/namespaces/x", map[string]any{}) },
				"DeleteIfExists": func() error { _, err := fc.DeleteIfExists("/api/v1/namespaces/x"); return err },
			}
			for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions, "WATCHME"} {
				writes["Do "+m] = func() error { _, _, err := fc.Do(m, "/api/v1/namespaces/x", "", nil, url.Values{}); return err }
			}
			for what, write := range writes {
				if err := write(); !errors.Is(err, kube.ErrFactsReadOnly) {
					t.Errorf("%s: %s returned %v, want ErrFactsReadOnly", name, what, err)
				}
			}
			if n := f.requestCount(); n != before {
				t.Errorf("%s: %d writes reached the API server", name, n-before)
			}
			if _, _, err := fc.Get("/api/v1/namespaces/x"); err != nil {
				t.Errorf("%s: a read failed: %v", name, err)
			}
			if fc.BearerToken() != "" || fc.ActsAsUser() || !fc.ReadOnly() || fc.SignedInUser() != nil {
				t.Errorf("%s: token %q, as user %v, read-only %v, signed-in user %v", name, fc.BearerToken(), fc.ActsAsUser(), fc.ReadOnly(), fc.SignedInUser())
			}
		}
		// The user's own client is not read-only and keeps its token.
		if user.ReadOnly() || user.BearerToken() != "user-token" {
			t.Errorf("the user's client changed: read-only %v, token %q", user.ReadOnly(), user.BearerToken())
		}
	})

	pg := loadProfileGraph(t)
	if len(pg.factsProblems) > 0 {
		t.Fatalf("uses of c.Facts() the RBAC scan cannot follow:\n%s", strings.Join(pg.factsProblems, "\n"))
	}
	reach := pg.graph.reach(pg.factsRoots, nil)

	t.Run("in the code", func(t *testing.T) {
		reqs, _ := serviceAccountRequestsIn(t, pg.clusterFuncs(reach))
		if len(reqs) == 0 {
			t.Fatal("no request derived from the facts roots; the check proves nothing")
		}
		reads := map[string]bool{"get": true, "list": true, "watch": true}
		for _, r := range reqs {
			if !reads[r.Verb] {
				t.Errorf("a facts client reaches %s (%s)", r.key(), r.Site)
			}
		}
	})

	t.Run("its token goes nowhere", func(t *testing.T) {
		for _, sink := range []string{kubePkg + ".Client.BearerToken", kubePkg + ".TokenKey"} {
			if reach[sink] {
				t.Errorf("a facts client reaches %s:\n  %s", sink, strings.Join(pg.graph.pathTo(pg.factsRoots, nil, sink), "\n  -> "))
			}
		}
	})
}

// The facts roots are found, and the ServiceAccount's scan reaches them
// and what they read, also when only a change (a mutation handler, which
// saReach otherwise leaves out) calls them: TestTemplateRBACCoversEveryServiceAccountCall
// then proves deploy/template.yaml grants each of their reads.
func TestFactsReachIsInTheServiceAccountScan(t *testing.T) {
	pg := loadProfileGraph(t)
	for _, want := range wantFactsRoots {
		if !slices.Contains(pg.factsRoots, want) {
			t.Errorf("%s is not a facts root; found %v", want, pg.factsRoots)
		}
	}
	sa := pg.saReach()
	for n := range pg.graph.reach(pg.factsRoots, nil) {
		if !sa[n] {
			t.Errorf("%s is reached by a facts client but not by the ServiceAccount's scan", n)
		}
	}
	// The scan without the facts roots, for the log: what only a facts
	// client reaches today.
	pruned := map[string]bool{"var " + apiPkg + ".gitOpsRoutes": true}
	for _, v := range pg.mutationHandlerVars() {
		pruned[v] = true
	}
	for n := range saReachPruned {
		pruned[n] = true
	}
	without := pg.graph.reach(pg.roots(), pruned)
	var only []string
	for n := range sa {
		if !without[n] {
			only = append(only, n)
		}
	}
	sort.Strings(only)
	t.Logf("facts roots: %v; reached only through a facts client: %d nodes", pg.factsRoots, len(only))
}

// wantFactsRequests are the requests a facts client makes today (verb,
// group/resource and name; the namespaces are found at run time): each new
// cluster fact the ServiceAccount reads for a change is a reviewed change
// of this list (non-admin design 3.1), not only a verb the scan allows.
var wantFactsRequests = []string{
	// KServe's configuration in the applications namespace: its default
	// gateway and where a model's download goes (a project's model
	// action, serving models_project.go).
	"get /configmaps name=inferenceservice-config",
	// Whether a project with users' records still exists, for the records
	// caps (record.ProjectExists): never shown, never a list.
	"get /namespaces name=*",
	"get apps/deployments name=gen-ai-ui",
	// KServe's LLMInferenceService controller: the CPU preset's prefix.
	"get apps/deployments name=llmisvc-controller-manager",
	"get config.openshift.io/infrastructures name=cluster",
	"get mlflow.opendatahub.io/mlflows name=mlflow",
	// Authorino's listener, behind a gateway's error on a model's call
	// check.
	"get operator.authorino.kuadrant.io/authorinos name=authorino",
	// Whether RHOAI is a nightly build (its Subscription's catalog): the
	// model's image plan.
	"get operators.coreos.com/subscriptions name=rhods-operator",
	"get route.openshift.io/routes name=rhods-dashboard",
	// The CPU preset (its name is the controller's prefix + the suffix).
	"get serving.kserve.io/llminferenceserviceconfigs name=*",
	"get serving.kserve.io/llminferenceserviceconfigs name=config-llm-template-cpu",
	// The room for a model (reduced to fits / may not fit,
	// serving.ModelCapacityFact) and a young node's pull grace.
	"list /nodes name=",
	"list /pods name=",
	"list admissionregistration.k8s.io/validatingwebhookconfigurations name=",
	"list components.platform.opendatahub.io/dashboards name=",
	// Whether a mirror serves the preset's image (the image plan).
	"list config.openshift.io/imagedigestmirrorsets name=",
	"list config.openshift.io/imagetagmirrorsets name=",
	"list datasciencecluster.opendatahub.io/datascienceclusters name=",
	"list dscinitialization.opendatahub.io/dscinitializations name=",
	// Whether the default inference gateway admits the project (reduced to
	// yes or no for that project, serving.ProjectGatewayFact) and whether
	// Connectivity Link checks its callers.
	"list gateway.networking.k8s.io/gateways name=",
	"list kuadrant.io/kuadrants name=",
	// Whether a MaaSModelRef in the project targets a model (reduced to yes
	// or no for that project, serving.ProjectMaaSRefFact): the one MaaS
	// read, in one project, before a project's model is deleted.
	"list maas.opendatahub.io/maasmodelrefs name=",
}

// The facts client's requests, pinned by resource and name; and what it
// must never read for a user: no Secret, no list of the namespaces, nothing
// of MaaS (whose objects list other tenants' models).
func TestFactsRequestsArePinned(t *testing.T) {
	pg := loadProfileGraph(t)
	reqs, _ := serviceAccountRequestsIn(t, pg.clusterFuncs(pg.graph.reach(pg.factsRoots, nil)))
	seen := map[string]bool{}
	for _, r := range reqs {
		seen[r.Verb+" "+r.Group+"/"+r.Resource+" name="+r.Name] = true
		switch {
		case r.Resource == "secrets":
			t.Errorf("a facts client reads a Secret: %s (%s)", r.key(), r.Site)
		case r.Resource == "namespaces" && r.Name == "":
			t.Errorf("a facts client lists namespaces: %s (%s)", r.key(), r.Site)
		// The one MaaS read: a list of MaaSModelRefs in one namespace (the
		// project), reduced to a yes or no (serving.ProjectMaaSRefFact).
		case strings.HasPrefix(r.Group, "maas.opendatahub.io") && (r.Verb != "list" || r.Resource != "maasmodelrefs" || r.Namespace == ""):
			t.Errorf("a facts client reads MaaS: %s (%s)", r.key(), r.Site)
		}
	}
	got := make([]string, 0, len(seen))
	for k := range seen {
		got = append(got, k)
	}
	sort.Strings(got)
	if !slices.Equal(got, wantFactsRequests) {
		t.Errorf("the facts client's requests changed; review the new fact (non-admin design 3.1, SECURITY.md \"Cluster facts of a change\"), then update wantFactsRequests:\ngot  %q\nwant %q", got, wantFactsRequests)
	}
}
