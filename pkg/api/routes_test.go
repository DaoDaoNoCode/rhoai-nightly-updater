package api

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
		gated++
		if _, ok := operationTypes[path]; !ok {
			t.Errorf("%s has no entry in operationTypes (type and label shown to other users)", rt.Pattern)
		}

		mutationPermission = func(context.Context, string) (bool, error) { return false, nil }
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, postJSONMethod(method, path, "user:reader", "{}"))
		if w.Code != http.StatusForbidden || decodeError(t, w)["errorCode"] != "forbidden" {
			t.Errorf("%s without cluster-admin: %d %s", rt.Pattern, w.Code, w.Body.String())
		}
		if clusterMutationInProgress.Load() {
			t.Fatalf("%s took the lock for a refused request", rt.Pattern)
		}

		allowMutations(t)
		if !acquireClusterMutationLock() {
			t.Fatal("lock unexpectedly held")
		}
		w = httptest.NewRecorder()
		mux.ServeHTTP(w, postJSONMethod(method, path, "user:admin", "{}"))
		releaseClusterMutationLock()
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

func postJSONMethod(method, path, token, body string) *http.Request {
	r := postJSON(path, token, body)
	r.Method = method
	return r
}

// TestEveryMutationHandlerBeginsItsOperation: a handler that takes the lock
// must call beginOperation once the request is valid, or the cluster change
// leaves no marker (no "interrupted" warning after a crash) and no
// lastCompleted. Only the dry run, which changes nothing, uses
// setOperationTarget instead.
func TestEveryMutationHandlerBeginsItsOperation(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "handlers.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, decl := range file.Decls {
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
			if !calls["lockCluster"] {
				t.Errorf("%s does not call lockCluster", name)
			}
			want := "beginOperation"
			if name == "HandleUpdate" {
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
