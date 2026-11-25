package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestClientRequestsWireFormat(t *testing.T) {
	type seen struct {
		method, path, query, contentType, accept, auth, body string
	}
	var got seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Content-Type"), r.Header.Get("Accept"), r.Header.Get("Authorization"), string(b)}
		if r.URL.Path == "/fail" {
			w.WriteHeader(409)
			io.WriteString(w, `{"kind":"Status","status":"Failure","message":"conflict","reason":"Conflict"}`)
			return
		}
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	c := &Client{baseURL: srv.URL, token: "tok", httpClient: srv.Client(), ctx: context.Background()}

	cases := []struct {
		name string
		call func() error
		want seen
	}{
		{"get", func() error { _, _, err := c.get("/a?labelSelector=x%3Dy"); return err },
			seen{"GET", "/a", "labelSelector=x%3Dy", "", "application/json", "Bearer tok", ""}},
		{"post", func() error { _, _, err := c.post("/a", []byte(`{"x":1}`)); return err },
			seen{"POST", "/a", "", "application/json", "application/json", "Bearer tok", `{"x":1}`}},
		{"put", func() error { _, _, err := c.put("/a", []byte(`{}`)); return err },
			seen{"PUT", "/a", "", "application/json", "application/json", "Bearer tok", `{}`}},
		{"patch", func() error { _, _, err := c.patch("/a", []byte(`{}`)); return err },
			seen{"PATCH", "/a", "", "application/merge-patch+json", "application/json", "Bearer tok", `{}`}},
		{"strategicPatch", func() error { _, _, err := c.strategicPatch("/a", []byte(`{}`)); return err },
			seen{"PATCH", "/a", "", "application/strategic-merge-patch+json", "application/json", "Bearer tok", `{}`}},
		{"apply", func() error { _, _, err := c.apply("/a", map[string]int{"x": 1}); return err },
			seen{"PATCH", "/a", "fieldManager=rhoai-nightly-updater&force=true", "application/apply-patch+yaml", "application/json", "Bearer tok", `{"x":1}`}},
		{"dryRunApply", func() error { _, _, err := c.dryRunApply("/a", map[string]int{"x": 1}); return err },
			seen{"PATCH", "/a", "dryRun=All&fieldManager=rhoai-nightly-updater&force=true", "application/apply-patch+yaml", "application/json", "Bearer tok", `{"x":1}`}},
		{"delete", func() error { _, err := c.delete("/a"); return err },
			seen{"DELETE", "/a", "", "", "application/json", "Bearer tok", ""}},
	}
	for _, tc := range cases {
		if err := tc.call(); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s:\n got  %+v\n want %+v", tc.name, got, tc.want)
		}
	}

	body, status, err := c.get("/fail")
	if status != 409 || !IsK8sError(err, 409) || string(body) == "" {
		t.Fatalf("error response: status=%d err=%v body=%q", status, err, body)
	}
	if status, err := c.delete("/fail"); status != 409 || !IsK8sError(err, 409) {
		t.Fatalf("delete error: status=%d err=%v", status, err)
	}
}

// isDryRun reports whether the API server runs the request as a
// server-side dry run, reading its options where the API server does. A
// DELETE with a body takes all its DeleteOptions, dryRun included, from the
// body and ignores the query: k8s.io/apiserver
// pkg/endpoints/handlers/delete.go decodes the query only when the body is
// empty, in DeleteResource at :95-128 and DeleteCollection at :272-302
// (v0.35 to v0.37; :95-126 and :270-300 in v0.32 and v0.33).
// Every other request takes dryRun from the query (create, update and patch
// options are query parameters). The fakes decide dry runs with it.
func isDryRun(method string, u *url.URL, body []byte) bool {
	if method == http.MethodDelete && len(body) > 0 {
		var opts struct {
			DryRun []string `json:"dryRun"`
		}
		return json.Unmarshal(body, &opts) == nil && slices.Contains(opts.DryRun, "All")
	}
	return u.Query().Get("dryRun") == "All"
}

func TestIsDryRun(t *testing.T) {
	for _, tc := range []struct {
		body, query string
		want        bool
	}{
		{`{"dryRun":["All"]}`, "", true},
		{`{"preconditions":{"uid":"u"}}`, "dryRun=All", false},
		{"", "dryRun=All", true},
		{`not json`, "dryRun=All", false},
	} {
		if got := isDryRun(http.MethodDelete, &url.URL{RawQuery: tc.query}, []byte(tc.body)); got != tc.want {
			t.Errorf("isDryRun(DELETE, %q, %s) = %v, want %v", tc.query, tc.body, got, tc.want)
		}
	}
	if !isDryRun(http.MethodPatch, &url.URL{RawQuery: "dryRun=All"}, []byte(`{"dryRun":["x"]}`)) {
		t.Error("a PATCH's dryRun is its query's")
	}
}

// A DELETE with DeleteOptions takes all of them from its body: the API
// server ignores the query then (isDryRun). The client refuses to send one
// with a DeleteOptions key in its query (a dryRun there would delete) with
// an error that says it was not sent, and deleteWithOptions puts the dry
// run in the options.
func TestDeleteDryRunIsInTheOptions(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.URL.RawQuery+" "+string(body))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	c := &Client{baseURL: srv.URL, token: "tok", httpClient: srv.Client(), ctx: context.Background()}
	opts := []byte(`{"kind":"DeleteOptions","apiVersion":"v1","preconditions":{"uid":"u"}}`)
	if _, _, err := c.do(http.MethodDelete, "/api/v1/namespaces/a/configmaps/x", "application/json", opts, url.Values{"dryRun": {"All"}}); err == nil {
		t.Fatal("a DELETE with options and a dryRun query was sent")
	}
	if _, _, err := c.do(http.MethodDelete, "/api/v1/namespaces/a/configmaps/x?dryRun=All", "application/json", opts, nil); err == nil {
		t.Fatal("a DELETE with options and a dryRun in its path was sent")
	}
	for _, key := range []string{"dryRun", "propagationPolicy", "gracePeriodSeconds", "orphanDependents"} {
		_, _, err := c.do(http.MethodDelete, "/api/v1/namespaces/a/configmaps/x", "application/json", opts, url.Values{key: {"0"}})
		if _, ok := errors.AsType[*UnsentRequestError](err); !ok || !strings.Contains(err.Error(), key) ||
			!strings.Contains(err.Error(), "this updater did not send the request") || !strings.Contains(err.Error(), "nothing was changed") ||
			!strings.HasPrefix(refusedBy(err), "This updater") {
			t.Errorf("%s in the query: %v, refused by %q", key, err, refusedBy(err))
		}
	}
	if len(got) != 0 {
		t.Fatalf("sent %v", got)
	}
	if refusedBy(&K8sError{Status: 422}) != "The API server" {
		t.Error("an API server's refusal is named as the updater's")
	}
	// A collection's ListOptions stay in the query, and a DELETE without a
	// body takes its options from the query.
	if _, _, err := c.do(http.MethodDelete, "/api/v1/namespaces/a/configmaps", "application/json", []byte(`{"dryRun":["All"]}`), url.Values{"labelSelector": {"a=b"}}); err != nil {
		t.Fatalf("a collection delete with a labelSelector was refused: %v", err)
	}
	if _, _, err := c.do(http.MethodDelete, "/api/v1/namespaces/a/configmaps/x", "", nil, url.Values{"dryRun": {"All"}}); err != nil {
		t.Fatalf("a DELETE without a body and with a dryRun query was refused: %v", err)
	}
	got = nil
	if err := deleteWithOptions(c, "/api/v1/namespaces/a/configmaps/x", map[string]any{"preconditions": map[string]any{"uid": "u"}}, true); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.HasPrefix(got[0], " ") || !strings.Contains(got[0], `"dryRun":["All"]`) || !strings.Contains(got[0], `"uid":"u"`) ||
		!strings.Contains(got[0], `"kind":"DeleteOptions"`) || !strings.Contains(got[0], `"apiVersion":"v1"`) {
		t.Fatalf("dry run sent %v", got)
	}
	if err := deleteWithOptions(c, "/api/v1/namespaces/a/configmaps/x", map[string]any{"preconditions": map[string]any{"uid": "u"}}, false); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || strings.Contains(got[1], "dryRun") || !strings.Contains(got[1], `"uid":"u"`) {
		t.Fatalf("delete sent %v", got)
	}
}
