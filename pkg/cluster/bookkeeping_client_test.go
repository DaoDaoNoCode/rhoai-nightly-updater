package cluster

import (
	"context"
	"fmt"
	"go/ast"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// tokenRecorder is a fake API server that records, per request, the bearer
// token it carried. Every request succeeds with an empty ConfigMap, a
// missing object is never reported, so each record function runs its whole
// read-modify-write.
type tokenRecorder struct {
	mu   sync.Mutex
	seen []string // "<token> <method> <path>"
}

func (rec *tokenRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.seen = append(rec.seen, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")+" "+r.Method+" "+r.URL.Path)
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"x","resourceVersion":"1"},"data":{}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// userClientWithBookkeeper is a client that acts with a user's token and
// keeps its records with the ServiceAccount's, as pkg/api builds it for a
// mutation.
func userClientWithBookkeeper(srv *httptest.Server) *Client {
	sa := &Client{baseURL: srv.URL, token: "sa-token", httpClient: srv.Client(), ctx: context.Background()}
	c := &Client{baseURL: srv.URL, token: "user-token", httpClient: srv.Client(), ctx: context.Background(), username: "alice"}
	c.SetBookkeeping(sa)
	return c
}

// Every record of the updater (activity, snapshot keys, the feature records
// and the operation record) is read and written with the ServiceAccount's
// token, also when the operation acts with the user's.
func TestBookkeepingUsesTheServiceAccount(t *testing.T) {
	t.Setenv("NAMESPACE", "test-ns")
	rec := &tokenRecorder{}
	srv := rec.server(t)
	c := userClientWithBookkeeper(srv)

	RecordActivity(c, types.ActivityEntry{Action: "update", User: "alice", Success: true})
	if _, err := GetActivity(c); err != nil {
		t.Fatal(err)
	}
	if _, err := readSnapshotKey(c, "k"); err != nil {
		t.Fatal(err)
	}
	if err := writeSnapshotKey(c, "k", "m", "v"); err != nil {
		t.Fatal(err)
	}
	if err := saveSubscriptionSnapshot(c, map[string]any{"spec": map[string]any{"channel": "x"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSubscriptionSnapshot(c); err != nil {
		t.Fatal(err)
	}
	GetDeploymentSnapshot(c)
	if _, err := readModelsRecord(c); err != nil {
		t.Fatal(err)
	}
	if err := writeModelsRecord(c, modelsRecord{}); err != nil {
		t.Fatal(err)
	}
	if _, err := readPipelineRecord(c); err != nil {
		t.Fatal(err)
	}
	if err := writePipelineRecord(c, pipelineRecord{}); err != nil {
		t.Fatal(err)
	}
	if _, err := readPlaygroundRecords(c); err != nil {
		t.Fatal(err)
	}
	if err := writePlaygroundRecords(c, map[string]playgroundRecord{}); err != nil {
		t.Fatal(err)
	}
	if _, err := readMaaSRecord(c); err != nil {
		t.Fatal(err)
	}
	if err := writeMaaSRecord(c, maasRecord{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadOperationRecord(c); err != nil {
		t.Fatal(err)
	}
	if err := SaveOperationMarker(c, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteOperationLease(c, &types.OperationLease{ID: "op"}, func(*types.OperationLease) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseOperationLease(c, LeaseOwner{ID: "op"}); err != nil {
		t.Fatal(err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.seen) == 0 {
		t.Fatal("no request was made")
	}
	for _, s := range rec.seen {
		token, rest, _ := strings.Cut(s, " ")
		if token != "sa-token" {
			t.Errorf("%s was sent with %q, want the ServiceAccount's token", rest, token)
		}
	}
}

// bookkeeping() hands out the ServiceAccount's client with the caller's
// context and user, and the client itself when none was set.
func TestBookkeepingClient(t *testing.T) {
	ctx := t.Context()
	sa := &Client{token: "sa-token", ctx: context.Background()}
	c := &Client{token: "user-token", ctx: ctx, username: "alice"}
	if got := c.bookkeeping(); got != c {
		t.Fatal("without a bookkeeper the client keeps its own records")
	}
	c.SetBookkeeping(sa)
	b := c.WithContext(ctx).bookkeeping()
	if b.token != "sa-token" || b.ctx != ctx || b.username != "alice" || b.bookkeeper != nil {
		t.Fatalf("bookkeeping() = token %q ctx %v user %q", b.token, b.ctx == ctx, b.username)
	}
	if sa.username != "" || sa.ctx == ctx {
		t.Fatal("bookkeeping() must not change the ServiceAccount client")
	}
}

// The source rule behind TestBookkeepingUsesTheServiceAccount: every API
// call on the updater's own ConfigMaps (in its namespace) goes through
// c.bookkeeping(), and bookkeeping() is used for nothing else, so it cannot
// carry another write on the ServiceAccount's token. A new record function
// that calls c.get on its ConfigMap fails here.
func TestOwnConfigMapsAreReachedOnlyThroughBookkeeping(t *testing.T) {
	p := loadClusterSource(t)
	var problems []string
	files := make([]string, 0, len(p.files))
	for name := range p.files {
		files = append(files, name)
	}
	sort.Strings(files)
	for _, file := range files {
		for _, decl := range p.files[file].Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || requestSourceFiles[file] {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				method, pathArg := requestMethod(sel.Sel.Name), call.Args[0]
				if method == "do" && len(call.Args) >= 2 {
					method, pathArg = rawMethods[selectorName(call.Args[0])], call.Args[1]
				}
				if _, known := methodVerbs[method]; !known && method != "get" {
					return true
				}
				viaBookkeeping := false
				if inner, ok := sel.X.(*ast.CallExpr); ok && selectorName(inner.Fun) == "bookkeeping" {
					viaBookkeeping = true
				}
				own := false
				for _, path := range (resolver{p: p, fn: fn}).resolve(pathArg) {
					rs, err := toRequests(method, path, file)
					if err != nil {
						continue
					}
					for _, r := range rs {
						if r.Resource == "configmaps" && r.Namespace == appNSMarker {
							own = true
						}
					}
				}
				pos := p.fset.Position(call.Pos())
				switch {
				case own && !viaBookkeeping:
					problems = append(problems, fmt.Sprintf("%s:%d %s: the updater's own ConfigMap is not reached through c.bookkeeping()", file, pos.Line, fn.Name.Name))
				case viaBookkeeping && !own:
					problems = append(problems, fmt.Sprintf("%s:%d %s: c.bookkeeping() is only for the updater's own ConfigMaps", file, pos.Line, fn.Name.Name))
				}
				return true
			})
		}
	}
	if len(problems) > 0 {
		t.Fatal(strings.Join(problems, "\n"))
	}
}
