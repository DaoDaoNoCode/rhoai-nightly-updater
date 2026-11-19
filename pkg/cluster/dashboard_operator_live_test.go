package cluster

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

// Opt-in verification of the actual installed operator and public Quay builds.
// The cluster is read only; all Kubernetes requests go to a GET-only fixture API.
func TestDashboardBuildDiscoveryLive(t *testing.T) {
	if os.Getenv("RHOAI_DASHBOARD_LIVE_TEST") != "1" {
		t.Skip("set RHOAI_DASHBOARD_LIVE_TEST=1 for read-only cluster/Quay verification")
	}
	// Every read goes through `oc get --raw` (GET only), so the discovery
	// of the dashboard module's operator runs against the live cluster.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("unexpected live-test mutation: %s", r.Method)
			w.WriteHeader(405)
			return
		}
		body, err := exec.Command("oc", "get", "--raw", r.URL.RequestURI()).Output()
		if err != nil {
			w.WriteHeader(404)
			fmt.Fprintf(w, `{"kind":"Status","code":404,"message":%q}`, err.Error())
			return
		}
		w.Write(body)
	}))
	defer server.Close()
	c := &Client{baseURL: server.URL, httpClient: server.Client(), ctx: context.Background()}
	target, operator, err := loadDashboardOperator(c, true)
	if err != nil || operator == nil {
		t.Fatalf("dashboard module operator: %+v %v", target, err)
	}
	t.Logf("dashboard module operator: %s (%s)", operator.ref(), target.Operator.Via)
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
			// Both flavors of "latest main" must exist: the default RHOAI build
			// (odh-stable) and the OpenShift CI build (main).
			main, _, found, err := resolveDashboardBuildTags(context.Background(), imageRepo, dashboardBuildTags("main", 0, DashboardFlavorRHOAI)[:1])
			if err == nil && !found {
				err = fmt.Errorf("odh-stable image missing")
			}
			if err == nil {
				_, _, found, err = resolveDashboardBuildTags(context.Background(), imageRepo, dashboardBuildTags("main", 0, DashboardFlavorODH))
				if err == nil && !found {
					err = fmt.Errorf("main image missing")
				}
			}
			prFound := false
			if err == nil && pr != "" {
				n, _ := strconv.Atoi(pr)
				var tag string
				_, tag, prFound, err = resolveDashboardBuildTags(context.Background(), imageRepo, dashboardBuildTags("pr", n, DashboardFlavorRHOAI))
				if prFound {
					component += " (" + tag + ")"
				}
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
		t.Logf("%s: odh-stable and main verified; PR image available=%v", r.component, r.prFound)
	}
	t.Logf("%d installed dashboard containers verified; %d have the requested PR build", len(images), prCount)
	if pr != "" && prCount == 0 {
		t.Error("no installed components have the requested PR build")
	}
}
