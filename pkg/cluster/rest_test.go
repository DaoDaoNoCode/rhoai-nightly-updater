package cluster

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// restServer answers each path with a fixed status and body, and records
// the requests ("METHOD path?query").
type restServer struct {
	answers  map[string]restAnswer
	requests []string
}

type restAnswer struct {
	status int
	body   string
}

func newRESTClient(t *testing.T, answers map[string]restAnswer) (*Client, *restServer) {
	t.Helper()
	s := &restServer{answers: answers}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		req := r.Method + " " + r.URL.RequestURI()
		if len(b) > 0 {
			req += " " + string(b)
		}
		s.requests = append(s.requests, req)
		a, ok := s.answers[r.URL.Path]
		if !ok {
			a = restAnswer{404, `{"kind":"Status","status":"Failure","message":"not found","reason":"NotFound","code":404}`}
		}
		w.WriteHeader(a.status)
		io.WriteString(w, a.body)
	}))
	t.Cleanup(srv.Close)
	return &Client{baseURL: srv.URL, token: "tok", httpClient: srv.Client(), ctx: context.Background()}, s
}

const forbiddenBody = `{"kind":"Status","status":"Failure","message":"no","reason":"Forbidden","code":403}`

func TestIsK8sNotFoundAndConflict(t *testing.T) {
	cases := []struct {
		err                 error
		notFound, conflicts bool
	}{
		{&K8sError{Status: 404}, true, false},
		{fmt.Errorf("read: %w", &K8sError{Status: 404}), true, false},
		{&K8sError{Status: 409}, false, true},
		{fmt.Errorf("create: %w", &K8sError{Status: 409}), false, true},
		{&K8sError{Status: 403}, false, false},
		{&sampleAPIError{Status: 404}, false, false},
		{errors.New("404"), false, false},
		{nil, false, false},
	}
	for _, tc := range cases {
		if got := isK8sNotFound(tc.err); got != tc.notFound {
			t.Errorf("isK8sNotFound(%v) = %v", tc.err, got)
		}
		if got := isK8sConflict(tc.err); got != tc.conflicts {
			t.Errorf("isK8sConflict(%v) = %v", tc.err, got)
		}
	}
}

func TestDeleteIfExists(t *testing.T) {
	c, s := newRESTClient(t, map[string]restAnswer{
		"/there": {200, `{}`},
		"/no":    {403, forbiddenBody},
	})
	if existed, err := c.deleteIfExists("/there"); !existed || err != nil {
		t.Fatalf("existing object: existed=%v err=%v", existed, err)
	}
	if existed, err := c.deleteIfExists("/gone"); existed || err != nil {
		t.Fatalf("missing object: existed=%v err=%v", existed, err)
	}
	_, wantErr := c.delete("/no")
	existed, err := c.deleteIfExists("/no")
	if existed || err == nil || err.Error() != wantErr.Error() || !IsK8sError(err, 403) {
		t.Fatalf("refused delete: existed=%v err=%v, want delete's error %v", existed, err, wantErr)
	}
	want := []string{"DELETE /there", "DELETE /gone", "DELETE /no", "DELETE /no"}
	if fmt.Sprint(s.requests) != fmt.Sprint(want) {
		t.Fatalf("requests %v, want %v", s.requests, want)
	}
}

// Every *Client method of rest.go is a REST helper the RBAC scans know
// (helperMethods): a new one that is not would hide its requests from them.
func TestRESTHelpersAreKnownToTheRBACScan(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "rest.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil {
			continue
		}
		if _, known := helperMethods[fn.Name.Name]; !known {
			t.Errorf("rest.go: (*Client).%s is not in helperMethods (rbac_coverage_test.go)", fn.Name.Name)
		}
	}
}
