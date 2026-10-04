package cluster

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// legacyDashboardServer simulates a cluster without dashboard-operator. deployments
// maps Deployment names in redhat-ods-applications to their container names.
func legacyDashboardServer(t *testing.T, deployments map[string][]string) (*Client, func() map[string][]string) {
	t.Helper()
	var mu sync.Mutex
	patched := map[string][]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		const prefix = "/apis/apps/v1/namespaces/redhat-ods-applications/deployments/"
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/deployments/rhods-operator") {
			var env []map[string]string
			for container, name := range containerEnvVarMap {
				env = append(env, map[string]string{"name": name, "value": "release-" + container})
			}
			b, _ := json.Marshal(map[string]interface{}{"spec": map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{"containers": []interface{}{map[string]interface{}{"env": env}}}}}})
			w.Write(b)
			return
		}
		if strings.HasPrefix(r.URL.Path, prefix) {
			name := strings.TrimPrefix(r.URL.Path, prefix)
			containers, ok := deployments[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `{"kind":"Status","status":"Failure","reason":"NotFound","code":404}`)
				return
			}
			if r.Method == http.MethodPatch {
				var patch struct {
					Spec struct {
						Template struct {
							Spec struct {
								Containers []struct {
									Name string `json:"name"`
								} `json:"containers"`
							} `json:"spec"`
						} `json:"template"`
					} `json:"spec"`
				}
				if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
					t.Errorf("decode patch: %v", err)
				}
				mu.Lock()
				for _, c := range patch.Spec.Template.Spec.Containers {
					patched[name] = append(patched[name], c.Name)
				}
				mu.Unlock()
				io.WriteString(w, `{}`)
				return
			}
			var cs []map[string]string
			for _, c := range containers {
				cs = append(cs, map[string]string{"name": c, "image": "old-" + c})
			}
			b, _ := json.Marshal(map[string]interface{}{"spec": map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{"containers": cs}}}})
			w.Write(b)
			return
		}
		io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	return &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: context.Background()}, func() map[string][]string {
		mu.Lock()
		defer mu.Unlock()
		out := map[string][]string{}
		for k, v := range patched {
			out[k] = append([]string(nil), v...)
			sort.Strings(out[k])
		}
		return out
	}
}

// allPRImagesPublished makes every PR image appear to exist on Quay.
func allPRImagesPublished(t *testing.T) {
	t.Helper()
	original := quayHTTPClient
	quayHTTPClient = &http.Client{Transport: dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	t.Cleanup(func() { quayHTTPClient = original })
}

func TestLegacyDeployPR_SidecarPatchesOnlyInstalledContainers(t *testing.T) {
	allPRImagesPublished(t)
	client, patched := legacyDashboardServer(t, map[string][]string{
		"rhods-dashboard": {"rhods-dashboard", "kube-rbac-proxy", "gen-ai-ui", "model-registry-ui"},
	})
	result, err := DeployPRImage(client, 42)
	if err != nil || !result.Success {
		t.Fatalf("DeployPRImage = %+v, %v", result, err)
	}
	got := patched()
	want := []string{"gen-ai-ui", "model-registry-ui", "rhods-dashboard"}
	if strings.Join(got["rhods-dashboard"], ",") != strings.Join(want, ",") || len(got) != 1 {
		t.Fatalf("patched %v, want only %v in rhods-dashboard", got, want)
	}
}

func TestLegacyDeployPR_StandaloneSkipsMissingModules(t *testing.T) {
	allPRImagesPublished(t)
	client, patched := legacyDashboardServer(t, map[string][]string{
		"rhods-dashboard":   {"rhods-dashboard", "kube-rbac-proxy"},
		"model-registry-ui": {"model-registry-ui"},
		"gen-ai-ui":         {"some-other-name"},
	})
	result, err := DeployPRImage(client, 42)
	if err != nil || !result.Success {
		t.Fatalf("DeployPRImage = %+v, %v", result, err)
	}
	got := patched()
	if strings.Join(got["rhods-dashboard"], ",") != "rhods-dashboard" {
		t.Errorf("main deployment patched with %v; core-bff is not installed and must not be added", got["rhods-dashboard"])
	}
	if strings.Join(got["model-registry-ui"], ",") != "model-registry-ui" {
		t.Errorf("installed module not patched: %v", got)
	}
	if _, ok := got["gen-ai-ui"]; ok {
		t.Errorf("module deployment without a matching container must not be patched: %v", got)
	}
	if len(got) != 2 {
		t.Errorf("unexpected patches: %v", got)
	}
}

func TestLegacyRevert_SkipsUninstalledContainers(t *testing.T) {
	client, patched := legacyDashboardServer(t, map[string][]string{
		"rhods-dashboard":   {"rhods-dashboard", "kube-rbac-proxy"},
		"model-registry-ui": {"model-registry-ui"},
	})
	result, err := RevertDashboardImage(client)
	if err != nil || !result.Success {
		t.Fatalf("RevertDashboardImage = %+v, %v", result, err)
	}
	got := patched()
	if strings.Join(got["rhods-dashboard"], ",") != "rhods-dashboard" || strings.Join(got["model-registry-ui"], ",") != "model-registry-ui" || len(got) != 2 {
		t.Fatalf("unexpected revert patches: %v", got)
	}
}

func TestLegacyDeployPR_NothingInstalledMakesNoChanges(t *testing.T) {
	original := quayHTTPClient
	quayHTTPClient = &http.Client{Transport: dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		status := 404
		if strings.Contains(r.URL.Path, "/odh-mod-arch-agent-ops/") {
			status = 200
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	t.Cleanup(func() { quayHTTPClient = original })
	client, patched := legacyDashboardServer(t, map[string][]string{
		"rhods-dashboard": {"rhods-dashboard", "gen-ai-ui"},
	})
	result, err := DeployPRImage(client, 42)
	if err != nil || result.Success || result.ErrorCode != "validation" {
		t.Fatalf("DeployPRImage = %+v, %v", result, err)
	}
	if got := patched(); len(got) != 0 {
		t.Fatalf("no deployment should be patched, got %v", got)
	}
}
