package api

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
)

// TestEveryNonGetRouteIsGated sends every registered non-GET route through
// the mux: a user without cluster-admin gets 403 before anything runs, and
// a cluster-admin gets 409 while another operation holds the lock.
func TestEveryNonGetRouteIsGated(t *testing.T) {
	setupDevMode(t)
	mux := http.NewServeMux()
	Register(mux)
	registered := map[string]bool{}
	gated := 0
	for _, rt := range routes {
		method, path, ok := strings.Cut(rt.Pattern, " ")
		if !ok || !strings.HasPrefix(path, "/api/") {
			t.Fatalf("route %q needs a method and an /api/ path", rt.Pattern)
		}
		if registered[rt.Pattern] {
			t.Fatalf("route %q registered twice", rt.Pattern)
		}
		registered[rt.Pattern] = true
		if method == http.MethodGet || rt.Pattern == "POST /api/pageview" {
			continue
		}
		lockFree := lockFreeMutationRoutes[rt.Pattern]
		gated++
		if _, ok := operationTypes[path]; !ok {
			t.Errorf("%s has no entry in operationTypes (type and label shown to other users)", rt.Pattern)
		}

		// A project action's route (withProjectMutationAuth) refuses a
		// user who is not a cluster-admin in a project that is not theirs
		// (resetPackageState: none is); every other route refuses them.
		body, wantCode := "{}", "forbidden"
		if _, project := projectActionRoutes[path]; project {
			body, wantCode = projectRouteBodies[path], cluster.ErrorCodeForbiddenForUser
		}
		// Cancel is open to an action's starter: a user who names no
		// action of theirs gets "no such action" (TestCancelOnlyStarterOrAdmin).
		if rt.Pattern == "POST /api/operation/cancel" {
			body, wantCode = `{"id":"not-theirs"}`, "not_found"
		}
		wantStatus := map[string]int{"not_found": http.StatusNotFound}[wantCode]
		if wantStatus == 0 {
			wantStatus = http.StatusForbidden
		}
		mutationPermission = func(context.Context, string) (bool, error) { return false, nil }
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, postJSONMethod(t, method, path, "user:reader", body))
		if w.Code != wantStatus || decodeError(t, w)["errorCode"] != wantCode {
			t.Errorf("%s without cluster-admin: %d %s", rt.Pattern, w.Code, w.Body.String())
		}
		if locks.clusterHeld() {
			t.Fatalf("%s took the lock for a refused request", rt.Pattern)
		}

		allowMutations(t)
		if !acquireClusterMutationLock() {
			t.Fatal("lock unexpectedly held")
		}
		w = httptest.NewRecorder()
		mux.ServeHTTP(w, postJSONMethod(t, method, path, "user:admin", body))
		releaseClusterMutationLock()
		if lockFree != "" {
			continue // TestRecipeStopTakesNoLockAndMakesNoClusterCall
		}
		if w.Code != http.StatusConflict || decodeError(t, w)["errorCode"] != "cluster_busy" {
			t.Errorf("%s while another operation runs: %d %s", rt.Pattern, w.Code, w.Body.String())
		}
	}
	if gated < 20 {
		t.Fatalf("only %d gated routes found", gated)
	}
	for path := range operationTypes {
		if !registered["POST "+path] {
			t.Errorf("operationTypes lists %s, which is not a registered POST route", path)
		}
	}
}

// projectRouteBodies are valid bodies of the project actions' routes.
var projectRouteBodies = map[string]string{
	"/api/resources/workbench/stream":      `{"action":"setup","project":"team-a"}`,
	"/api/resources/playground/stream":     `{"action":"teardown","project":"team-a"}`,
	"/api/resources/samples/mlflow/stream": `{"action":"add","project":"team-a","samples":["training"]}`,
}

// lockFreeMutationRoutes are the gated routes that take no cluster lock,
// with why. Each has a test that it changes nothing in the cluster and
// takes no lock (TestRecipeStopTakesNoLockAndMakesNoClusterCall,
// TestCancelOnlyStarterOrAdmin); a cancel's only call is the mutation
// gate's access review with the caller's own token.
var lockFreeMutationRoutes = map[string]string{
	"POST /api/recipes/stop":     "asks the running recipe run, which holds the lock, to stop after its step",
	"POST /api/operation/cancel": "asks a running project action, which holds its lock, to stop at its next safe point",
}

func postJSONMethod(t testing.TB, method, path, token, body string) *http.Request {
	r := postJSON(t, path, token, body)
	r.Method = method
	return r
}

// TestEveryMutationHandlerBeginsItsOperation: a handler that takes the lock
// must call beginOperation once the request is valid, or the cluster change
// leaves no marker (no "interrupted" warning after a crash) and no
// lastCompleted. Only the dry run, which changes nothing, uses
// setOperationTarget instead.
func TestEveryMutationHandlerBeginsItsOperation(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var decls []ast.Decl
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		decls = append(decls, file.Decls...)
	}
	checked := 0
	for _, decl := range decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			if len(vs.Values) != 1 {
				continue
			}
			call, ok := vs.Values[0].(*ast.CallExpr)
			if !ok || !isIdent(call.Fun, "withMutationAuth") {
				continue
			}
			name := vs.Names[0].Name
			calls := map[string]bool{}
			ast.Inspect(call.Args[0], func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if id, ok := c.Fun.(*ast.Ident); ok {
						calls[id.Name] = true
					}
				}
				return true
			})
			checked++
			// A recipe run holds the lock beyond its request
			// (lockRecipeRunner) and begins it once planned
			// (beginRecipeRun, which calls beginHeldOperation). Stop
			// takes no lock (lockFreeMutationRoutes).
			if name == "HandleRecipeStop" {
				continue
			}
			if calls["lockRecipeRunner"] {
				if !calls["beginRecipeRun"] {
					t.Errorf("%s takes the runner lock but does not call beginRecipeRun", name)
				}
				continue
			}
			if !calls["lockCluster"] {
				t.Errorf("%s does not call lockCluster", name)
			}
			want := "beginOperation"
			// Dry runs and plans change nothing: no marker, no lastCompleted.
			if name == "HandleUpdate" || name == "HandleUpdateGitOps" || name == "HandleNodeSetupPlan" || name == "HandleRecipePlan" {
				want = "setOperationTarget"
			}
			if !calls[want] {
				t.Errorf("%s does not call %s", name, want)
			}
		}
	}
	if checked < 20 {
		t.Fatalf("only %d mutation handlers found", checked)
	}
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}
