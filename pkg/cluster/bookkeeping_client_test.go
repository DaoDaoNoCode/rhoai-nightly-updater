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

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
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
	sa := newTestClient(testClientOptions{BaseURL: srv.URL, Token: "sa-token", HTTPClient: srv.Client(), Ctx: context.Background()})
	c := newTestClient(testClientOptions{BaseURL: srv.URL, Token: "user-token", HTTPClient: srv.Client(), Ctx: context.Background(), Username: "alice"})
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

	record.RecordActivity(c, types.ActivityEntry{Action: "update", User: "alice", Success: true})
	if _, err := GetActivity(c); err != nil {
		t.Fatal(err)
	}
	if _, err := record.ReadSnapshotKey(c, "k"); err != nil {
		t.Fatal(err)
	}
	if err := record.WriteSnapshotKey(c, "k", "m", "v"); err != nil {
		t.Fatal(err)
	}
	if err := record.SaveSubscriptionSnapshot(c, map[string]any{"spec": map[string]any{"channel": "x"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := record.LoadSubscriptionSnapshot(c); err != nil {
		t.Fatal(err)
	}
	components.ReadDeploymentSnapshot(c)
	if _, err := serving.ReadModelsRecord(c); err != nil {
		t.Fatal(err)
	}
	if err := serving.WriteModelsRecord(c, serving.ModelsRecord{}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ReadPipelineRecord(c); err != nil {
		t.Fatal(err)
	}
	if err := storage.WritePipelineRecord(c, storage.PipelineRecord{}); err != nil {
		t.Fatal(err)
	}
	if _, err := features.ReadPlaygroundRecords(c); err != nil {
		t.Fatal(err)
	}
	if err := features.WritePlaygroundRecords(c, map[string]features.PlaygroundRecord{}); err != nil {
		t.Fatal(err)
	}
	if _, err := serving.ReadMaaSRecord(c); err != nil {
		t.Fatal(err)
	}
	if err := serving.WriteMaaSRecord(c, serving.MaaSRecord{}); err != nil {
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

// Bookkeeping() hands out the ServiceAccount's client with the caller's
// context and user, and the client itself when none was set.
func TestBookkeepingClient(t *testing.T) {
	ctx := t.Context()
	sa := newTestClient(testClientOptions{Token: "sa-token", Ctx: context.Background()})
	c := newTestClient(testClientOptions{Token: "user-token", Ctx: ctx, Username: "alice"})
	if got := c.Bookkeeping(); got != c {
		t.Fatal("without a bookkeeper the client keeps its own records")
	}
	c.SetBookkeeping(sa)
	b := c.WithContext(ctx).Bookkeeping()
	if b.BearerToken() != "sa-token" || b.Context() != ctx || b.Username() != "alice" || b.TestOptions().Bookkeeper != nil {
		t.Fatalf("Bookkeeping() = token %q ctx %v user %q", b.BearerToken(), b.Context() == ctx, b.Username())
	}
	if sa.Username() != "" || sa.Context() == ctx {
		t.Fatal("Bookkeeping() must not change the ServiceAccount client")
	}
}

// The source rule behind TestBookkeepingUsesTheServiceAccount: every API
// call on the updater's own ConfigMaps (in its namespace) goes through
// c.Bookkeeping(), and Bookkeeping() is used for nothing else, so it cannot
// carry another write on the ServiceAccount's token. A new record function
// that calls c.Get on its ConfigMap fails here.
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
			if !ok || fn.Body == nil || p.isTransport(fn) {
				continue
			}
			// Refused calls fail the RBAC scans (scanRequests).
			calls, _ := p.requestCalls(file, fn.Body)
			for _, rc := range calls {
				viaBookkeeping := p.isBookkeeping(rc.client)
				own := false
				for _, path := range (resolver{p: p, fn: fn}).resolve(rc.path) {
					rs, err := toRequests(rc.method, path, file)
					if err != nil {
						continue
					}
					for _, r := range rs {
						if r.Resource == "configmaps" && r.Namespace == appNSMarker {
							own = true
						}
					}
				}
				pos := p.fset.Position(rc.call.Pos())
				switch {
				case own && !viaBookkeeping:
					problems = append(problems, fmt.Sprintf("%s:%d %s: the updater's own ConfigMap is not reached through c.Bookkeeping()", file, pos.Line, fn.Name.Name))
				case viaBookkeeping && !own:
					problems = append(problems, fmt.Sprintf("%s:%d %s: c.Bookkeeping() is only for the updater's own ConfigMaps", file, pos.Line, fn.Name.Name))
				}
			}
		}
	}
	if len(problems) > 0 {
		t.Fatal(strings.Join(problems, "\n"))
	}
}
