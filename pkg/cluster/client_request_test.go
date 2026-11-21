package cluster

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
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
	c := newTestClient(testClientOptions{BaseURL: srv.URL, Token: "tok", HTTPClient: srv.Client(), Ctx: context.Background()})

	cases := []struct {
		name string
		call func() error
		want seen
	}{
		{"get", func() error { _, _, err := c.Get("/a?labelSelector=x%3Dy"); return err },
			seen{"GET", "/a", "labelSelector=x%3Dy", "", "application/json", "Bearer tok", ""}},
		{"post", func() error { _, _, err := c.Post("/a", []byte(`{"x":1}`)); return err },
			seen{"POST", "/a", "", "application/json", "application/json", "Bearer tok", `{"x":1}`}},
		{"put", func() error { _, _, err := c.Put("/a", []byte(`{}`)); return err },
			seen{"PUT", "/a", "", "application/json", "application/json", "Bearer tok", `{}`}},
		{"patch", func() error { _, _, err := c.Patch("/a", []byte(`{}`)); return err },
			seen{"PATCH", "/a", "", "application/merge-patch+json", "application/json", "Bearer tok", `{}`}},
		{"strategicPatch", func() error { _, _, err := c.StrategicPatch("/a", []byte(`{}`)); return err },
			seen{"PATCH", "/a", "", "application/strategic-merge-patch+json", "application/json", "Bearer tok", `{}`}},
		{"apply", func() error { _, _, err := c.Apply("/a", map[string]int{"x": 1}); return err },
			seen{"PATCH", "/a", "fieldManager=rhoai-nightly-updater&force=true", "application/apply-patch+yaml", "application/json", "Bearer tok", `{"x":1}`}},
		{"dryRunApply", func() error { _, _, err := c.DryRunApply("/a", map[string]int{"x": 1}); return err },
			seen{"PATCH", "/a", "dryRun=All&fieldManager=rhoai-nightly-updater&force=true", "application/apply-patch+yaml", "application/json", "Bearer tok", `{"x":1}`}},
		{"delete", func() error { _, err := c.Delete("/a"); return err },
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

	body, status, err := c.Get("/fail")
	if status != 409 || !kube.IsK8sError(err, 409) || string(body) == "" {
		t.Fatalf("error response: status=%d err=%v body=%q", status, err, body)
	}
	if status, err := c.Delete("/fail"); status != 409 || !kube.IsK8sError(err, 409) {
		t.Fatalf("delete error: status=%d err=%v", status, err)
	}
}
