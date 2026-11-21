package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dashdev"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// deployedDashboardDev is a Dashboard Dev fixture after Deploy latest main,
// with a client whose every request first runs onRequest (nil: nothing).
func deployedDashboardDev(t *testing.T, onRequest func(r *http.Request)) (*dashboardDevFixture, *Client) {
	t.Helper()
	t.Cleanup(dashdev.SetDeploymentsReadyTimingForTest(time.Minute, time.Second))
	mockDashboardRegistry(t, nil)
	f, c := newDashboardDevFixture(t)
	if result, err := DeployDashboardMain(c); err != nil || !result.Success {
		t.Fatalf("deploy: %+v %v", result, err)
	}
	srv := newFakePlainServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if onRequest != nil {
			onRequest(r)
		}
		f.serve(w, r)
	}))
	t.Cleanup(srv.Close)
	return f, newTestClient(testClientOptions{BaseURL: srv.URL, HTTPClient: srv.Client(), Ctx: context.Background()})
}

// setWorkloadRollout gives the fixture's Deployment name a generation and a
// status (JSON).
func setWorkloadRollout(t *testing.T, f *dashboardDevFixture, name string, generation int64, status string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.workloads[name]
	d.Metadata.Generation = generation
	d.Status = dashdev.Deployment{}.Status
	if err := json.Unmarshal([]byte(status), &d.Status); err != nil {
		t.Fatal(err)
	}
	f.workloads[name] = d
}

const rolledOut = `{"observedGeneration":2,"replicas":1,"updatedReplicas":1,"readyReplicas":1}`

// isPoll reports whether r is the read of the dashboard module's operator
// that starts each poll (the discovery before it is cached after the
// first).
func isPoll(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/namespaces/"+fixtureAppsNS+"/deployments/"+fixtureOperatorName)
}

// The new build rolls out: not yet observed, then observed with no ready
// replica, then ready. The wait polls until every Deployment that runs a
// build container is ready.
func TestWaitDashboardDeploymentsReady_PollsUntilReady(t *testing.T) {
	var f *dashboardDevFixture
	polls := 0
	f, c := deployedDashboardDev(t, func(r *http.Request) {
		if !isPoll(r) {
			return
		}
		polls++
		switch polls {
		case 2:
			setWorkloadRollout(t, f, "notebooks-ui", 2, `{"observedGeneration":2,"replicas":2,"updatedReplicas":1,"readyReplicas":1}`)
		case 3:
			setWorkloadRollout(t, f, "notebooks-ui", 2, rolledOut)
		}
	})
	setWorkloadRollout(t, f, "notebooks-ui", 2, `{"observedGeneration":1,"replicas":1,"updatedReplicas":1,"readyReplicas":1}`)
	images, err := dashdev.WaitDeploymentsReady(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 3 || polls != 3 {
		t.Fatalf("%d images after %d polls: %+v", len(images), polls, images)
	}
}

// Waiting cannot help: the Deployment controller gave up on the rollout,
// a pod cannot be created, or Dashboard Dev was reverted. Each ends at the
// first poll with ErrDeploymentsFailed and says why.
func TestWaitDashboardDeploymentsReady_StopsAtATerminalState(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *dashboardDevFixture, c *Client)
		want  string
	}{
		{"progress deadline exceeded", func(t *testing.T, f *dashboardDevFixture, _ *Client) {
			setWorkloadRollout(t, f, "notebooks-ui", 2, `{"observedGeneration":2,"replicas":2,"updatedReplicas":1,"readyReplicas":1,
				"conditions":[{"type":"Progressing","status":"False","reason":"ProgressDeadlineExceeded","message":"ReplicaSet \"notebooks-ui-7d\" has timed out progressing."}]}`)
		}, `Deployment notebooks-ui: ReplicaSet "notebooks-ui-7d" has timed out progressing.`},
		{"replica failure", func(t *testing.T, f *dashboardDevFixture, _ *Client) {
			setWorkloadRollout(t, f, dashdev.DeploymentName, 2, `{"observedGeneration":2,"replicas":1,"updatedReplicas":0,"readyReplicas":1,
				"conditions":[{"type":"Progressing","status":"True","reason":"ReplicaSetUpdated"},{"type":"ReplicaFailure","status":"True","reason":"FailedCreate","message":"pods is forbidden: exceeded quota"}]}`)
		}, "Deployment " + dashdev.DeploymentName + ": pods is forbidden: exceeded quota"},
		{"reverted", func(t *testing.T, _ *dashboardDevFixture, c *Client) {
			if result, err := RevertDashboardImage(c); err != nil || !result.Success {
				t.Fatalf("revert: %+v %v", result, err)
			}
		}, "Dashboard Dev is no longer active (" + fixtureOperatorName + " was resumed or reverted)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			polls := 0
			f, c := deployedDashboardDev(t, func(r *http.Request) {
				if isPoll(r) {
					polls++
				}
			})
			tc.setup(t, f, c)
			polls = 0
			_, err := dashdev.WaitDeploymentsReady(c)
			if !errors.Is(err, dashdev.ErrDeploymentsFailed) || errors.Is(err, dashdev.ErrDeploymentsNotReady) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want ErrDeploymentsFailed with %q", err, tc.want)
			}
			if polls != 1 {
				t.Fatalf("%d polls, want 1", polls)
			}
		})
	}
}

// Never ready: the wait ends after its timeout on the fake clock, naming
// the Deployments that are not ready. A failed condition of an earlier
// generation (the controller has not seen the new template yet) is not
// final.
func TestWaitDashboardDeploymentsReady_TimesOut(t *testing.T) {
	polls := 0
	f, c := deployedDashboardDev(t, func(r *http.Request) {
		if isPoll(r) {
			polls++
		}
	})
	t.Cleanup(dashdev.SetDeploymentsReadyTimingForTest(30*time.Second, 10*time.Second))
	setWorkloadRollout(t, f, "notebooks-ui", 3, `{"observedGeneration":2,"replicas":1,"updatedReplicas":0,"readyReplicas":1,
		"conditions":[{"type":"Progressing","status":"False","reason":"ProgressDeadlineExceeded","message":"an earlier rollout timed out"}]}`)
	polls = 0
	start := kube.Clock().Now()
	_, err := dashdev.WaitDeploymentsReady(c)
	if !errors.Is(err, dashdev.ErrDeploymentsNotReady) || !strings.Contains(err.Error(), "after 30s: not ready: notebooks-ui (0 of 1 replicas ready and updated)") {
		t.Fatalf("err %v", err)
	}
	if waited := kube.Clock().Since(start); waited < 30*time.Second {
		t.Fatalf("gave up after %s", waited)
	}
	if polls < 3 || polls > 4 {
		t.Fatalf("%d polls in 30s at a 10s poll", polls)
	}
}

// An ended client context ends the wait with its error.
func TestWaitDashboardDeploymentsReady_ContextEnded(t *testing.T) {
	_, c := deployedDashboardDev(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := dashdev.WaitDeploymentsReady(c.WithContext(ctx)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}
