package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
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
	return newTestClient(testClientOptions{BaseURL: srv.URL, Token: "tok", HTTPClient: srv.Client(), Ctx: context.Background()}), s
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
		if got := kube.IsK8sNotFound(tc.err); got != tc.notFound {
			t.Errorf("isK8sNotFound(%v) = %v", tc.err, got)
		}
		if got := kube.IsK8sConflict(tc.err); got != tc.conflicts {
			t.Errorf("isK8sConflict(%v) = %v", tc.err, got)
		}
	}
}

func TestGetOptional(t *testing.T) {
	c, s := newRESTClient(t, map[string]restAnswer{
		"/there": {200, `{"a":1}`},
		"/no":    {403, forbiddenBody},
	})
	if body, found, err := c.GetOptional("/there"); !found || err != nil || string(body) != `{"a":1}` {
		t.Fatalf("existing object: body=%q found=%v err=%v", body, found, err)
	}
	if body, found, err := c.GetOptional("/gone"); found || err != nil || body != nil {
		t.Fatalf("missing object: body=%q found=%v err=%v", body, found, err)
	}
	wantBody, _, wantErr := c.Get("/no")
	body, found, err := c.GetOptional("/no")
	if found || err == nil || err.Error() != wantErr.Error() || !kube.IsK8sError(err, 403) || string(body) != string(wantBody) {
		t.Fatalf("refused read: body=%q found=%v err=%v, want get's %q %v", body, found, err, wantBody, wantErr)
	}
	want := []string{"GET /there", "GET /gone", "GET /no", "GET /no"}
	if fmt.Sprint(s.requests) != fmt.Sprint(want) {
		t.Fatalf("requests %v, want %v", s.requests, want)
	}
}

func TestGetInto(t *testing.T) {
	c, s := newRESTClient(t, map[string]restAnswer{
		"/there": {200, `{"metadata":{"name":"a"}}`},
		"/bad":   {200, `{"metadata":{"name":1}}`},
		"/no":    {403, forbiddenBody},
	})
	var obj struct {
		Metadata kube.ObjectMeta `json:"metadata"`
	}
	if err := c.GetInto("/there", &obj); err != nil || obj.Metadata.Name != "a" {
		t.Fatalf("getInto: %+v %v", obj, err)
	}
	_, _, wantErr := c.Get("/no")
	if err := c.GetInto("/no", &obj); err == nil || err.Error() != wantErr.Error() || !kube.IsK8sError(err, 403) || kube.IsDecodeError(err) {
		t.Fatalf("refused read: %v, want get's %v", err, wantErr)
	}
	wantDecode := json.Unmarshal([]byte(`{"metadata":{"name":1}}`), &obj)
	err := c.GetInto("/bad", &obj)
	if err == nil || err.Error() != wantDecode.Error() || !kube.IsDecodeError(err) {
		t.Fatalf("bad body: %v, want json.Unmarshal's %v", err, wantDecode)
	}
	if _, ok := errors.AsType[*json.UnmarshalTypeError](fmt.Errorf("parse: %w", err)); !ok {
		t.Fatalf("a wrapped decode error does not unwrap to the JSON error: %#v", err)
	}
	if fmt.Sprintf("parse: %v", err) != "parse: "+wantDecode.Error() {
		t.Fatalf("a decode error formats as %q", fmt.Sprintf("%v", err))
	}

	found, err := c.GetIntoOptional("/gone", &obj)
	if found || err != nil {
		t.Fatalf("getIntoOptional missing: found=%v err=%v", found, err)
	}
	if found, err := c.GetIntoOptional("/there", &obj); !found || err != nil {
		t.Fatalf("getIntoOptional existing: found=%v err=%v", found, err)
	}
	if found, err := c.GetIntoOptional("/no", &obj); found || err == nil || err.Error() != wantErr.Error() {
		t.Fatalf("getIntoOptional refused: found=%v err=%v", found, err)
	}
	if found, err := c.GetIntoOptional("/bad", &obj); !found || !kube.IsDecodeError(err) || err.Error() != wantDecode.Error() {
		t.Fatalf("getIntoOptional bad body: found=%v err=%v", found, err)
	}
	want := []string{"GET /there", "GET /no", "GET /no", "GET /bad", "GET /gone", "GET /there", "GET /no", "GET /bad"}
	if fmt.Sprint(s.requests) != fmt.Sprint(want) {
		t.Fatalf("requests %v, want %v", s.requests, want)
	}
}

// listInto's decode errors are, byte for byte, those of json.Unmarshal
// into the unnamed list struct the call sites declared.
func TestListInto(t *testing.T) {
	type item struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	bodies := map[string]string{
		"/ok":        `{"kind":"List","items":[{"metadata":{"name":"a"}},{"metadata":{"name":"b"}}]}`,
		"/empty":     `{"kind":"List"}`,
		"/null":      `{"items":null}`,
		"/wrongtype": `{"items":[{"metadata":{"name":1}}]}`,
		"/notlist":   `{"items":"x"}`,
		"/syntax":    `{"items":[`,
		"/notjson":   `<html>`,
	}
	answers := map[string]restAnswer{"/no": {403, forbiddenBody}}
	for p, b := range bodies {
		answers[p] = restAnswer{200, b}
	}
	c, _ := newRESTClient(t, answers)
	for p, b := range bodies {
		var list struct {
			Items []item `json:"items"`
		}
		wantErr := json.Unmarshal([]byte(b), &list)
		var items []item
		err := c.ListInto(p, &items)
		switch {
		case wantErr == nil && err != nil:
			t.Errorf("%s: %v", p, err)
		case wantErr != nil && (err == nil || err.Error() != wantErr.Error() || !kube.IsDecodeError(err)):
			t.Errorf("%s: error %v, want %v", p, err, wantErr)
		case wantErr == nil && fmt.Sprint(items) != fmt.Sprint(list.Items):
			t.Errorf("%s: items %v, want %v", p, items, list.Items)
		}
		if p == "/wrongtype" && !strings.Contains(err.Error(), "Go struct field .items.0.metadata.name") {
			t.Errorf("unnamed list struct path missing from %q", err)
		}
	}
	_, _, wantErr := c.Get("/no")
	var items []json.RawMessage
	if err := c.ListInto("/no", &items); err == nil || err.Error() != wantErr.Error() || kube.IsDecodeError(err) {
		t.Fatalf("refused list: %v, want get's %v", err, wantErr)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("listInto accepted a non-slice")
		}
	}()
	var notSlice struct{}
	_ = c.ListInto("/ok", &notSlice)
}

func TestPatchJSON(t *testing.T) {
	c, s := newRESTClient(t, map[string]restAnswer{
		"/there": {200, `{}`},
		"/no":    {403, forbiddenBody},
	})
	if err := c.PatchJSON("/there", map[string]any{"metadata": map[string]any{"resourceVersion": "7"}}); err != nil {
		t.Fatal(err)
	}
	_, _, wantErr := c.Patch("/no", []byte(`{}`))
	if err := c.PatchJSON("/no", map[string]any{}); err == nil || err.Error() != wantErr.Error() || !kube.IsK8sError(err, 403) {
		t.Fatalf("refused patch: %v, want patch's %v", err, wantErr)
	}
	bad := map[string]any{"replicas": math.Inf(1)}
	_, wantMarshal := json.Marshal(bad)
	if err := c.PatchJSON("/there", bad); err == nil || wantMarshal == nil || err.Error() != wantMarshal.Error() {
		t.Fatalf("unmarshalable patch: %v, want json.Marshal's %v", err, wantMarshal)
	}
	want := []string{`PATCH /there {"metadata":{"resourceVersion":"7"}}`, "PATCH /no {}", "PATCH /no {}"}
	if fmt.Sprint(s.requests) != fmt.Sprint(want) {
		t.Fatalf("requests %v, want %v (no request for a patch that does not marshal)", s.requests, want)
	}
}

func TestDeleteIfExists(t *testing.T) {
	c, s := newRESTClient(t, map[string]restAnswer{
		"/there": {200, `{}`},
		"/no":    {403, forbiddenBody},
	})
	if existed, err := c.DeleteIfExists("/there"); !existed || err != nil {
		t.Fatalf("existing object: existed=%v err=%v", existed, err)
	}
	if existed, err := c.DeleteIfExists("/gone"); existed || err != nil {
		t.Fatalf("missing object: existed=%v err=%v", existed, err)
	}
	_, wantErr := c.Delete("/no")
	existed, err := c.DeleteIfExists("/no")
	if existed || err == nil || err.Error() != wantErr.Error() || !kube.IsK8sError(err, 403) {
		t.Fatalf("refused delete: existed=%v err=%v, want delete's error %v", existed, err, wantErr)
	}
	want := []string{"DELETE /there", "DELETE /gone", "DELETE /no", "DELETE /no"}
	if fmt.Sprint(s.requests) != fmt.Sprint(want) {
		t.Fatalf("requests %v, want %v", s.requests, want)
	}
}

// Every *Client method of internal/kube/rest.go is a REST helper the RBAC scans know
// (helperMethods): a new one that is not would hide its requests from them.
func TestRESTHelpersAreKnownToTheRBACScan(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "internal/kube/rest.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil {
			continue
		}
		star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		if id, ok := star.X.(*ast.Ident); !ok || id.Name != "Client" {
			continue
		}
		if _, known := helperMethods[fn.Name.Name]; !known {
			t.Errorf("rest.go: (*Client).%s is not in helperMethods (rbac_coverage_test.go)", fn.Name.Name)
		}
	}
}
