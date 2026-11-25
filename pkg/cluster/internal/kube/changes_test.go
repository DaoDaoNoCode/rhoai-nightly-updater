package kube

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
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
// run, creates a review the API server does not store, or the API server
// answered 4xx (refused before it was applied). A 5xx may have been applied.
func TestChangesRecordsWhatMayHaveChangedTheCluster(t *testing.T) {
	srv := statusServer(t)
	for _, tc := range []struct {
		name, method, path string
		query              url.Values
		made               bool
	}{
		{"get", http.MethodGet, "/api/v1/namespaces/a", nil, false},
		{"head", http.MethodHead, "/api/v1/namespaces/a", nil, false},
		{"create", http.MethodPost, "/api/v1/namespaces/a/configmaps/status-201", nil, true},
		{"patch", http.MethodPatch, "/api/v1/namespaces/a/configmaps/x", nil, true},
		{"put", http.MethodPut, "/api/v1/namespaces/a/configmaps/x", nil, true},
		{"delete", http.MethodDelete, "/api/v1/namespaces/a/configmaps/x", nil, true},
		{"server error", http.MethodDelete, "/api/v1/namespaces/a/configmaps/status-500", nil, true},
		{"unavailable", http.MethodPatch, "/api/v1/namespaces/a/configmaps/status-503", nil, true},
		{"forbidden", http.MethodDelete, "/api/v1/namespaces/a/configmaps/status-403", nil, false},
		{"not found", http.MethodDelete, "/api/v1/namespaces/a/configmaps/status-404", nil, false},
		{"conflict", http.MethodPost, "/api/v1/namespaces/a/configmaps/status-409", nil, false},
		{"invalid (a failed json-patch test)", http.MethodPatch, "/api/v1/namespaces/a/configmaps/status-422", nil, false},
		{"throttled", http.MethodPatch, "/api/v1/namespaces/a/configmaps/status-429", nil, false},
		{"dry run", http.MethodPatch, "/api/v1/namespaces/a/configmaps/x", url.Values{"dryRun": {"All"}}, false},
		{"dry run in the path", http.MethodDelete, "/api/v1/namespaces/a/configmaps/x?dryRun=All", nil, false},
		{"self subject access review", http.MethodPost, "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", nil, false},
		{"openshift subject access review", http.MethodPost, "/apis/authorization.openshift.io/v1/subjectaccessreviews", nil, false},
		{"token review", http.MethodPost, "/apis/authentication.k8s.io/v1/tokenreviews", nil, false},
		{"a resource of another group named like a review", http.MethodPost, "/apis/example.com/v1/tokenreviews", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ch := trackedClient(t, srv.URL)
			_, _, _ = c.Do(tc.method, tc.path, "application/json", []byte(`{}`), tc.query)
			if ch.Made() != tc.made {
				t.Fatalf("Made() = %v, want %v", ch.Made(), tc.made)
			}
		})
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

// Every HTTP request pkg/cluster sends with a method that may write goes
// through the API server client (doOnce records it) or calls NoteRequest
// before it is sent, so a change's rate-limit slot is never given back after
// a write the tracker missed. Each request built with http.NewRequest is a
// GET or HEAD, or the code up to the Do that sends it calls
// kube.NoteRequest(req).
func TestEveryWritingHTTPCallIsRecorded(t *testing.T) {
	newRequest := regexp.MustCompile(`http\.NewRequest(?:WithContext)?\([^,]*,\s*([^,]+),`)
	send := regexp.MustCompile(`\.Do\(req\)`)
	noted := regexp.MustCompile(`(?m)^\s*kube\.NoteRequest\(req\)\s*$`)
	readOnly := regexp.MustCompile(`^("GET"|"HEAD"|http\.MethodGet|http\.MethodHead)$`)
	checked, writes := 0, 0
	err := filepath.WalkDir(filepath.Join("..", ".."), func(file string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") || strings.HasSuffix(file, "testing.go") ||
			strings.HasSuffix(file, filepath.Join("kube", "client.go")) {
			return err
		}
		src, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		text := string(src)
		for _, m := range newRequest.FindAllStringSubmatchIndex(text, -1) {
			checked++
			method := strings.TrimSpace(text[m[2]:m[3]])
			if readOnly.MatchString(method) {
				continue
			}
			writes++
			rest := text[m[1]:]
			do := send.FindStringIndex(rest)
			if do == nil || !noted.MatchString(rest[:do[0]]) {
				t.Errorf("%s sends %s requests without kube.NoteRequest(req) before its Do", file, method)
			}
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
