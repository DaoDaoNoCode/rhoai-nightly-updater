package cluster

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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
