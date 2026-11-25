package kube

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// statusServer answers every request 200, or the status that a last path
// segment "status-<code>" names.
func statusServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := http.StatusOK
		if code, ok := strings.CutPrefix(path.Base(r.URL.Path), "status-"); ok {
			status, _ = strconv.Atoi(code)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"kind":"Status","code":` + strconv.Itoa(status) + `}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func trackedClient(t *testing.T, baseURL string) (*Client, *Changes) {
	t.Helper()
	ctx, ch := TrackChanges(t.Context())
	return NewTestClient(TestClientOptions{BaseURL: baseURL, HTTPClient: http.DefaultClient, Ctx: ctx, Token: "t"}), ch
}

// A request counts as a change unless it only reads, is a server-side dry
// run (for a DELETE with DeleteOptions, as the options say), creates a
// review the API server does not store, or the API server refused it
// before it was applied. A 5xx may have been applied, and so may a DELETE
// of a collection answered with an error of one of its items.
func TestChangesRecordsWhatMayHaveChangedTheCluster(t *testing.T) {
	srv := statusServer(t)
	const cm = "/api/v1/namespaces/a/configmaps"
	dryOpts := `{"kind":"DeleteOptions","apiVersion":"v1","dryRun":["All"],"preconditions":{"uid":"u"}}`
	realOpts := `{"kind":"DeleteOptions","apiVersion":"v1","preconditions":{"uid":"u"}}`
	for _, tc := range []struct {
		name, method, path string
		query              url.Values
		body               string
		made               bool
	}{
		{"get", http.MethodGet, cm + "/x", nil, "", false},
		{"head", http.MethodHead, cm + "/x", nil, "", false},
		{"create", http.MethodPost, cm + "/status-201", nil, "{}", true},
		{"patch", http.MethodPatch, cm + "/x", nil, "{}", true},
		{"put", http.MethodPut, cm + "/x", nil, "{}", true},
		{"delete", http.MethodDelete, cm + "/x", nil, "", true},
		{"delete with options", http.MethodDelete, cm + "/x", nil, realOpts, true},
		{"server error", http.MethodDelete, cm + "/status-500", nil, "", true},
		{"unavailable", http.MethodPatch, cm + "/status-503", nil, "{}", true},
		{"gateway timeout", http.MethodPatch, cm + "/status-504", nil, "{}", true},
		{"forbidden", http.MethodDelete, cm + "/status-403", nil, "", false},
		{"not found", http.MethodDelete, cm + "/status-404", nil, "", false},
		{"conflict", http.MethodPost, cm + "/status-409", nil, "{}", false},
		{"invalid (a failed json-patch test)", http.MethodPatch, cm + "/status-422", nil, "{}", false},
		{"throttled", http.MethodPatch, cm + "/status-429", nil, "{}", false},
		{"a 4xx no refusal is known by (410)", http.MethodPatch, cm + "/status-410", nil, "{}", true},
		{"a namespace's delete refused", http.MethodDelete, "/api/v1/namespaces/status-403", nil, "", false},
		{"dry run (query)", http.MethodPatch, cm + "/x", url.Values{"dryRun": {"All"}}, "{}", false},
		{"dry run in the path", http.MethodPost, cm + "?dryRun=All", nil, "{}", false},
		{"delete dry run without options (query)", http.MethodDelete, cm + "/x", url.Values{"dryRun": {"All"}}, "", false},
		{"delete dry run in the options", http.MethodDelete, cm + "/x", nil, dryOpts, false},
		{"self subject access review", http.MethodPost, "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", nil, "{}", false},
		{"openshift subject access review", http.MethodPost, "/apis/authorization.openshift.io/v1/subjectaccessreviews", nil, "{}", false},
		{"token review", http.MethodPost, "/apis/authentication.k8s.io/v1/tokenreviews", nil, "{}", false},
		{"a resource of another group named like a review", http.MethodPost, "/apis/example.com/v1/tokenreviews", nil, "{}", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ch := trackedClient(t, srv.URL)
			var body []byte
			if tc.body != "" {
				body = []byte(tc.body)
			}
			_, _, _ = c.Do(tc.method, tc.path, "application/json", body, tc.query)
			if ch.Made() != tc.made {
				t.Fatalf("Made() = %v, want %v", ch.Made(), tc.made)
			}
		})
	}
}

// A DELETE of a collection is applied item by item: an answer of a single
// object's refusal (403, 404, 409, 422) may come after other items were
// deleted, so only 401 and 429 leave it uncounted.
func TestChangesCollectionDeletes(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/api/v1/namespaces/a/configmaps", true},
		{"/apis/apps/v1/namespaces/a/deployments", true},
		{"/apis/config.openshift.io/v1/imagedigestmirrorsets", true},
		{"/api/v1/namespaces", true},
		{"/api/v1/namespaces/a", false},
		{"/api/v1/namespaces/a/configmaps/x", false},
		{"/apis/apps/v1/namespaces/a/deployments/x/scale", false},
		{"/apis/config.openshift.io/v1/imagedigestmirrorsets/m", false},
		{"/healthz", true},
	} {
		if got := collectionPath(tc.path); got != tc.want {
			t.Errorf("collectionPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
	for status, refused := range map[int]bool{401: true, 429: true, 400: false, 403: false, 404: false, 409: false, 422: false, 500: false} {
		if got := refusedBeforeApplied(http.MethodDelete, "/api/v1/namespaces/a/configmaps", status); got != refused {
			t.Errorf("collection delete answered %d: refused = %v, want %v", status, got, refused)
		}
	}
}

// A DELETE with DeleteOptions takes all of them from its body: the API
// server ignores the query then (k8s.io/apiserver handlers/delete.go
// :95-128, v0.35+). The client refuses to send one with a DeleteOptions key
// in its query (a dryRun there would delete) with an error that says it
// was not sent, and DeleteWithOptions puts the dry run in the options.
func TestDeleteDryRunIsInTheOptions(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.URL.RawQuery+" "+string(body))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	c, ch := trackedClient(t, srv.URL)
	opts := []byte(`{"kind":"DeleteOptions","apiVersion":"v1","preconditions":{"uid":"u"}}`)
	if _, _, err := c.Do(http.MethodDelete, "/api/v1/namespaces/a/configmaps/x", "application/json", opts, url.Values{"dryRun": {"All"}}); err == nil {
		t.Fatal("a DELETE with options and a dryRun query was sent")
	}
	if _, _, err := c.Do(http.MethodDelete, "/api/v1/namespaces/a/configmaps/x?dryRun=All", "application/json", opts, nil); err == nil {
		t.Fatal("a DELETE with options and a dryRun in its path was sent")
	}
	for _, key := range []string{"propagationPolicy", "gracePeriodSeconds", "orphanDependents"} {
		_, _, err := c.Do(http.MethodDelete, "/api/v1/namespaces/a/configmaps/x", "application/json", opts, url.Values{key: {"0"}})
		if _, ok := errors.AsType[*UnsentRequestError](err); !ok || !strings.Contains(err.Error(), key) || !strings.HasPrefix(RefusedBy(err), "This updater") {
			t.Errorf("%s in the query: %v, refused by %q", key, err, RefusedBy(err))
		}
	}
	if len(got) != 0 || ch.Made() {
		t.Fatalf("sent %v, made %v", got, ch.Made())
	}
	if RefusedBy(errors.New("k8s API error 422")) != "The API server" {
		t.Error("an API server's refusal is named as the updater's")
	}
	// A collection's ListOptions stay in the query.
	if _, _, err := c.Do(http.MethodDelete, "/api/v1/namespaces/a/configmaps", "application/json", []byte(`{"dryRun":["All"]}`), url.Values{"labelSelector": {"a=b"}}); err != nil {
		t.Fatalf("a collection delete with a labelSelector was refused: %v", err)
	}
	got = nil
	if _, _, err := DeleteWithOptions(c, "/api/v1/namespaces/a/configmaps/x", map[string]any{"preconditions": map[string]any{"uid": "u"}}, true); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.HasPrefix(got[0], " ") || !strings.Contains(got[0], `"dryRun":["All"]`) || !strings.Contains(got[0], `"uid":"u"`) || ch.Made() {
		t.Fatalf("dry run sent %v, made %v", got, ch.Made())
	}
	if _, _, err := DeleteWithOptions(c, "/api/v1/namespaces/a/configmaps/x", map[string]any{"preconditions": map[string]any{"uid": "u"}}, false); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || strings.Contains(got[1], "dryRun") || !ch.Made() {
		t.Fatalf("delete sent %v, made %v", got, ch.Made())
	}
	for _, tc := range []struct {
		body, query string
		want        bool
	}{
		{`{"dryRun":["All"]}`, "", true},
		{`{"preconditions":{"uid":"u"}}`, "dryRun=All", false},
		{"", "dryRun=All", true},
		{`not json`, "dryRun=All", false},
	} {
		if got := IsDryRun(http.MethodDelete, &url.URL{RawQuery: tc.query}, []byte(tc.body)); got != tc.want {
			t.Errorf("IsDryRun(DELETE, %q, %s) = %v, want %v", tc.query, tc.body, got, tc.want)
		}
	}
	if !IsDryRun(http.MethodPatch, &url.URL{RawQuery: "dryRun=All"}, []byte(`{"dryRun":["x"]}`)) {
		t.Error("a PATCH's dryRun is its query's")
	}
}

// Made loads pending before made: a write that ends between the two loads
// (it stores made, then leaves pending) is seen.
func TestChangesMadeSeesAWriteEndingDuringTheCall(t *testing.T) {
	arrived, answer, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(arrived)
		<-answer
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	c, ch := trackedClient(t, srv.URL)
	go func() {
		_, _ = c.Delete("/api/v1/namespaces/a/configmaps/x")
		close(done)
	}()
	<-arrived
	afterPendingLoad = func() {
		close(answer)
		<-done
	}
	t.Cleanup(func() { afterPendingLoad = nil })
	if !ch.Made() {
		t.Fatal("a write that ended during Made was missed")
	}
}

// A write that got no answer (connection refused, a timeout) may have been
// applied.
func TestChangesCountsAWriteWithoutAnAnswer(t *testing.T) {
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	c, ch := trackedClient(t, "http://"+addr)
	if _, err := c.Delete("/api/v1/namespaces/a"); err == nil {
		t.Fatal("the closed port answered")
	}
	if !ch.Made() {
		t.Fatal("a write without an answer was not counted")
	}
}

// A write counts from the moment it is sent: until its answer comes, Made
// is true (a decision taken meanwhile keeps the slot). The API server's 404
// then shows it changed nothing.
func TestChangesCountsAWriteInFlight(t *testing.T) {
	arrived, answer := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(arrived)
		<-answer
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	c, ch := trackedClient(t, srv.URL)
	done := make(chan struct{})
	go func() {
		_, _ = c.Delete("/api/v1/namespaces/a")
		close(done)
	}()
	<-arrived
	if !ch.Made() {
		t.Error("a write in flight was not counted")
	}
	close(answer)
	<-done
	if ch.Made() {
		t.Error("a write the API server refused (404) still counts")
	}
}

// The updater's own records (Bookkeeping) never count; a copy of the client
// with a context derived from the operation's counts as the client does.
func TestChangesBookkeepingDoesNotCount(t *testing.T) {
	srv := statusServer(t)
	ctx, ch := TrackChanges(t.Context())
	sa := NewTestClient(TestClientOptions{BaseURL: srv.URL, HTTPClient: http.DefaultClient, Ctx: context.Background(), Token: "sa"})
	c := NewTestClient(TestClientOptions{BaseURL: srv.URL, HTTPClient: http.DefaultClient, Ctx: ctx, Token: "user", AsUser: true, Bookkeeper: sa})
	if _, _, err := c.Bookkeeping().Patch("/api/v1/namespaces/updater/configmaps/activity", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if ch.Made() {
		t.Fatal("a bookkeeping write counted as a change")
	}
	derived, cancel := context.WithCancel(context.WithoutCancel(c.Context()))
	defer cancel()
	if _, err := c.WithContext(derived).Delete("/api/v1/namespaces/a/configmaps/x"); err != nil {
		t.Fatal(err)
	}
	if !ch.Made() {
		t.Fatal("a write with a context derived from the operation's did not count")
	}
}

// NoteRequest (S3, gen-ai, MLflow, a model) counts every write, whatever it
// is answered, and no read; without a tracker it does nothing.
func TestNoteRequest(t *testing.T) {
	ctx, ch := TrackChanges(t.Context())
	get, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://s3.invalid/", nil)
	NoteRequest(get)
	if ch.Made() {
		t.Fatal("a read counted")
	}
	post, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://s3.invalid/", nil)
	NoteRequest(post)
	if !ch.Made() {
		t.Fatal("a write did not count")
	}
	untracked, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://s3.invalid/", nil)
	NoteRequest(untracked)
	var none *Changes
	if none.Made() {
		t.Fatal("a nil Changes reports a change")
	}
}

// Every HTTP request pkg/cluster builds with a method that may write is
// recorded before it is sent, so a change's rate-limit slot is never given
// back after a write the tracker missed. Read from the syntax: each
// http.NewRequest or http.NewRequestWithContext whose method is not a GET
// or HEAD constant is assigned to a variable, and in its function a call
// to NoteRequest or startAPIRequest that takes that variable comes after
// it and before every Do that sends it, of which there is at least one. An
// http.Request built as a composite literal is refused.
func TestEveryWritingHTTPCallIsRecorded(t *testing.T) {
	checked, writes := 0, 0
	err := filepath.WalkDir(filepath.Join("..", ".."), func(file string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") || strings.HasSuffix(file, "testing.go") {
			return err
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			c, w := checkRequestsRecorded(t, fset, fn.Body)
			checked, writes = checked+c, writes+w
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 || writes == 0 {
		t.Fatalf("%d request sites, %d that may write: the walk looks in the wrong place", checked, writes)
	}
}

// qualified reports whether e is pkg.<one of names>.
func qualified(e ast.Expr, pkg string, names ...string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg && slices.Contains(names, sel.Sel.Name)
}

// checkRequestsRecorded checks the requests built in body; it returns how
// many it found and how many of them may write.
func checkRequestsRecorded(t *testing.T, fset *token.FileSet, body *ast.BlockStmt) (checked, writes int) {
	t.Helper()
	mentions := func(args []ast.Expr, name string) bool {
		found := false
		for _, a := range args {
			ast.Inspect(a, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == name {
					found = true
				}
				return !found
			})
		}
		return found
	}
	ast.Inspect(body, func(n ast.Node) bool {
		if lit, ok := n.(*ast.CompositeLit); ok && qualified(lit.Type, "http", "Request") {
			t.Errorf("%s: an http.Request built as a literal; use http.NewRequestWithContext", fset.Position(lit.Pos()))
		}
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok || !qualified(call.Fun, "http", "NewRequest", "NewRequestWithContext") {
			return true
		}
		checked++
		method := call.Args[0]
		if qualified(call.Fun, "http", "NewRequestWithContext") {
			method = call.Args[1]
		}
		if lit, ok := method.(*ast.BasicLit); ok && (lit.Value == `"GET"` || lit.Value == `"HEAD"`) || qualified(method, "http", "MethodGet", "MethodHead") {
			return true
		}
		writes++
		where := fset.Position(call.Pos())
		id, ok := assign.Lhs[0].(*ast.Ident)
		if !ok || id.Name == "_" {
			t.Errorf("%s: a request that may write is not kept in a variable", where)
			return true
		}
		var noted, sent []token.Pos
		ast.Inspect(body, func(m ast.Node) bool {
			c, ok := m.(*ast.CallExpr)
			if !ok || c.Pos() < call.End() {
				return true
			}
			switch fun := c.Fun.(type) {
			case *ast.SelectorExpr:
				if fun.Sel.Name == "Do" && len(c.Args) == 1 && mentions(c.Args, id.Name) {
					sent = append(sent, c.Pos())
				}
				if qualified(fun, "kube", "NoteRequest") && mentions(c.Args, id.Name) {
					noted = append(noted, c.Pos())
				}
			case *ast.Ident:
				if (fun.Name == "NoteRequest" || fun.Name == "startAPIRequest") && mentions(c.Args, id.Name) {
					noted = append(noted, c.Pos())
				}
			}
			return true
		})
		if len(sent) == 0 {
			t.Errorf("%s: request %s that may write is not sent with Do in its function", where, id.Name)
		}
		for _, do := range sent {
			if len(noted) == 0 || noted[0] > do {
				t.Errorf("%s: request %s that may write is sent at %s without kube.NoteRequest(%s) before", where, id.Name, fset.Position(do), id.Name)
			}
		}
		return true
	})
	return checked, writes
}
