package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Opt-in verification of the actual installed operator and public Quay builds.
// The cluster is read only; all Kubernetes requests go to a GET-only fixture API.
func TestDashboardBuildDiscoveryLive(t *testing.T) {
	if os.Getenv("RHOAI_DASHBOARD_LIVE_TEST") != "1" {
		t.Skip("set RHOAI_DASHBOARD_LIVE_TEST=1 for read-only cluster/Quay verification")
	}
	body, err := exec.Command("oc", "get", "deployments", "-n", dashboardNamespace, "-o", "json").Output()
	if err != nil {
		t.Fatal("cannot read installed dashboard deployments: ", err)
	}
	var list struct {
		Items []dashboardDeployment `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	var operator *dashboardDeployment
	for i := range list.Items {
		if list.Items[i].Metadata.Name == dashboardOperatorName {
			operator = &list.Items[i]
		}
	}
	if operator == nil {
		t.Fatal("dashboard-operator is not installed")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("unexpected live-test mutation: %s", r.Method)
			w.WriteHeader(405)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/deployments") || strings.HasSuffix(r.URL.Path, "/deployments/") {
			w.Write(body)
			return
		}
		w.WriteHeader(404)
	}))
	defer server.Close()
	c := &Client{baseURL: server.URL, httpClient: server.Client(), ctx: context.Background()}
	images, err := discoverDashboardImages(c, operator)
	if err != nil {
		t.Fatal(err)
	}
	pr := os.Getenv("RHOAI_DASHBOARD_TEST_PR")
	type checked struct {
		component string
		main      string
		prFound   bool
		err       error
	}
	results := make(chan checked, len(images))
	for _, image := range images {
		go func(imageRepo, component string) {
			main, found, err := resolveDashboardBuild(context.Background(), imageRepo, "main")
			if err == nil && !found {
				err = fmt.Errorf("main image missing")
			}
			prFound := false
			if err == nil && pr != "" {
				_, prFound, err = resolveDashboardBuild(context.Background(), imageRepo, "pr-"+pr)
			}
			results <- checked{component, main, prFound, err}
		}(image.Repository, image.Deployment+"/"+image.Container)
	}
	prCount := 0
	for range images {
		r := <-results
		if r.err != nil {
			t.Errorf("%s: %v", r.component, r.err)
			continue
		}
		if r.prFound {
			prCount++
		}
		t.Logf("%s: main verified; PR image available=%v", r.component, r.prFound)
	}
	t.Logf("%d installed dashboard containers verified; %d have the requested PR build", len(images), prCount)
	if pr != "" && prCount == 0 {
		t.Error("no installed components have the requested PR build")
	}
}
