package cluster

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func obsSetUp(t *testing.T, oc *obsCluster) {
	t.Helper()
	if res, _ := obsRun(t, oc.c, "setup"); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
}

// A second Set up records nothing new, even an object that looks like the
// stack's: only the run that added the fields records.
func TestObservability_ALaterSetUpRecordsNothing(t *testing.T) {
	oc := newObsCluster(t, true)
	obsSetUp(t, oc)
	oc.f.put(obsSecrets+"/late-cert", map[string]any{"metadata": map[string]any{"name": "late-cert", "uid": "uid-late", "creationTimestamp": time.Now().UTC().Format(time.RFC3339),
		"annotations": map[string]any{"cert-manager.io/certificate-name": "x"}}})
	obsSetUp(t, oc)
	rec, _ := obsRecord(t, oc.f)
	if _, ok := rec.Left.Secrets["late-cert"]; ok {
		t.Fatalf("recorded a later Secret: %+v", rec.Left)
	}
	// The Secret lists ask for metadata only, with no plain JSON fallback.
	for _, call := range oc.f.callsTo(http.MethodGet, obsSecrets) {
		if !strings.Contains(call.Accept, "as=PartialObjectMetadataList") || strings.HasSuffix(call.Accept, ",application/json") {
			t.Errorf("Secret list Accept = %q", call.Accept)
		}
	}
}

// A recorded disk that gained an owner since is kept.
func TestObservability_CleanUpKeepsARecordedDiskThatGainedAnOwner(t *testing.T) {
	oc := newObsCluster(t, true)
	obsSetUp(t, oc)
	name := "prometheus-data-science-monitoringstack-db-prometheus-data-science-monitoringstack-0"
	pvc := oc.f.get(obsPVCs + "/" + name)
	ensureMap(pvc, "metadata")["ownerReferences"] = []any{map[string]any{"kind": "Something", "name": "x", "uid": "u"}}
	oc.f.put(obsPVCs+"/"+name, pvc)
	if res, _ := obsRun(t, oc.c, "teardown"); !res.Success {
		t.Fatalf("teardown: %+v", res)
	}
	if !oc.f.has(obsPVCs + "/" + name) {
		t.Fatal("deleted a disk that has an owner now")
	}
}

// Same-name bucket or user this tool cannot prove it made: refused.
func TestObservability_RefusesABucketOrUserItDidNotMake(t *testing.T) {
	t.Run("bucket", func(t *testing.T) {
		oc := newObsCluster(t, true)
		oc.f.sw.buckets = map[string]bool{"pipelines": true, storage.UsageLogsBucket: true}
		res, _ := obsRun(t, oc.c, "setup")
		if res.Success || !strings.Contains(res.Message, "this tool did not make it") {
			t.Fatalf("setup: %+v", res)
		}
		if _, ok := oc.f.sw.users[storage.UsageLogsUser]; ok {
			t.Error("made the user next to a foreign bucket")
		}
		// Clean up sees the setup that stopped (its intent record) and
		// leaves the foreign bucket.
		if st, _ := GetObservabilityStatus(oc.sa); !st.ManagedByTool {
			t.Fatalf("status after the stopped setup: %+v", st)
		}
		res, _ = obsRun(t, oc.c, "teardown")
		if res.Success || !oc.f.sw.buckets[storage.UsageLogsBucket] || !strings.Contains(res.Message, "has no object") {
			t.Fatalf("teardown: %+v, buckets %v", res, oc.f.sw.buckets)
		}
	})
	t.Run("user", func(t *testing.T) {
		oc := newObsCluster(t, true)
		oc.f.sw.users[storage.UsageLogsUser] = &fakeIAMUser{keys: map[string]string{}, tags: map[string]string{}}
		res, _ := obsRun(t, oc.c, "setup")
		if res.Success || !strings.Contains(res.Message, "is not tagged") {
			t.Fatalf("setup: %+v", res)
		}
		if oc.f.sw.users[storage.UsageLogsUser].policy != "" {
			t.Error("adopted the foreign user")
		}
	})
}

// A setup that stops after making the storage is seen and cleaned up; a
// Clean up that stopped after deleting the user deletes the bucket next.
func TestObservability_PartialSetUpAndCleanUpFinish(t *testing.T) {
	oc := newObsCluster(t, true)
	oc.f.sw.failIAM["CreateAccessKey"] = 500
	res, _ := obsRun(t, oc.c, "setup")
	if res.Success {
		t.Fatalf("setup: %+v", res)
	}
	rec, ok := obsRecord(t, oc.f)
	if !ok || !rec.Storage || len(rec.Added) != 0 {
		t.Fatalf("intent record = %+v %v", rec, ok)
	}
	if st, _ := GetObservabilityStatus(oc.sa); !st.ManagedByTool || len(st.CleanupPlan) == 0 {
		t.Fatalf("status = %+v", st)
	}
	delete(oc.f.sw.failIAM, "CreateAccessKey")
	// The user goes by hand first; the bucket stays.
	delete(oc.f.sw.users, storage.UsageLogsUser)
	if res, _ := obsRun(t, oc.c, "teardown"); !res.Success {
		t.Fatalf("teardown: %+v", res)
	}
	if oc.f.sw.buckets[storage.UsageLogsBucket] {
		t.Error("bucket left")
	}
	if _, ok := obsRecord(t, oc.f); ok {
		t.Error("record left")
	}
}

// The bucket is emptied in DeleteObjects batches, page by page.
func TestObservability_CleanUpEmptiesTheBucketInBatches(t *testing.T) {
	t.Cleanup(storage.SetUsageLogsListPageLimitForTest(2))
	oc := newObsCluster(t, true)
	obsSetUp(t, oc)
	for _, k := range []string{"a", "b", "c", "d"} {
		oc.f.sw.objects[storage.UsageLogsBucket+"/application/"+k] = []byte("x")
	}
	if res, _ := obsRun(t, oc.c, "teardown"); !res.Success {
		t.Fatalf("teardown: %+v", res)
	}
	if got := oc.f.sw.batchDeletes; len(got) != 3 || slices.Max(got) > 2 {
		t.Fatalf("batches = %v", got)
	}
	if oc.f.sw.buckets[storage.UsageLogsBucket] {
		t.Error("bucket left")
	}
}

// Clean up waits until the stack is really gone: a LokiStack on this tool's
// Secret that stays keeps the Secret, user and bucket.
func TestObservability_CleanUpWaitsForTheStackToGo(t *testing.T) {
	oc := newObsCluster(t, true)
	obsSetUp(t, oc)
	oc.f.putJSON("/apis/loki.grafana.com/v1/namespaces/"+obsNS+"/lokistacks/data-science-usage-logs-lokistack",
		`{"metadata":{"name":"data-science-usage-logs-lokistack"},"spec":{"storage":{"secret":{"name":"`+storage.UsageLogsSecret+`"}}}}`)
	res, events := obsRun(t, oc.c, "teardown")
	if res.Success || lastStatus(events, "monitoring-gc") != "failed" || !strings.Contains(res.Message, "LokiStack data-science-usage-logs-lokistack") {
		t.Fatalf("teardown: %+v", res)
	}
	if !oc.f.has(obsSecrets+"/"+storage.UsageLogsSecret) || !oc.f.sw.buckets[storage.UsageLogsBucket] {
		t.Fatal("deleted the storage while the stack still uses it")
	}
}

// A setup whose wait ran out offers Set up again (wait again).
func TestObservability_OffersToWaitAgain(t *testing.T) {
	oc := newObsCluster(t, true)
	oc.neverReady = true
	res, _ := obsRun(t, oc.c, "setup")
	if res.Success || res.ErrorCode != "not_ready" {
		t.Fatalf("setup: %+v", res)
	}
	st, _ := GetObservabilityStatus(oc.sa)
	if plan := strings.Join(st.SetupPlan, "\n"); !strings.Contains(plan, "wait again") {
		t.Fatalf("plan: %q", plan)
	}
}

// The beta default-class annotation counts as the default.
func TestObservability_BetaDefaultStorageClass(t *testing.T) {
	oc := newObsCluster(t, true)
	oc.f.putJSON(obsSCs+"/gp3-csi", `{"metadata":{"name":"gp3-csi","creationTimestamp":"2026-01-01T00:00:00Z","annotations":{"storageclass.beta.kubernetes.io/is-default-class":"true"}}}`)
	obsSetUp(t, oc)
}

// The MaaS portal at both served shapes: DataScienceCluster v3 with only
// sub-component states under dashboard (RHOAI 3.7, live 2026-11-25), and v2
// (maasConsumerPortal); the core dashboard's state does not matter.
func TestObservability_MaaSPortalAtBothShapes(t *testing.T) {
	t.Run("v3", func(t *testing.T) {
		oc := newObsCluster(t, true)
		v3 := "/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters/default-dsc"
		oc.f.groups = map[string]string{"/apis/datasciencecluster.opendatahub.io": `{"name":"datasciencecluster.opendatahub.io","versions":[{"version":"v3"},{"version":"v2"}],"preferredVersion":{"version":"v3"}}`}
		oc.f.putJSON(v3, `{"metadata":{"name":"default-dsc","resourceVersion":"5"},"spec":{"components":{"dashboard":{"maasPortal":{"managementState":"Removed"},"standard":{"managementState":"Managed"}}}},
			"status":{"components":{"dashboard":{"managementState":"Managed"}},"conditions":[{"type":"DashboardReady","status":"True"},{"type":"MaaSPortalAvailable","status":"False"}]}}`)
		st, _ := GetMaaSStatus(oc.sa)
		if st.Portal == nil || st.Portal.BlockedReason != "" {
			t.Fatalf("portal = %+v", st.Portal)
		}
		if res := obsMaaSRun(t, oc.c, types.MaaSRequest{Action: "portal-on"}); !res.Success {
			t.Fatalf("on: %+v", res)
		}
		if got := ensureMap(ensureMap(ensureMap(ensureMap(oc.f.get(v3), "spec"), "components"), "dashboard"), "maasPortal")["managementState"]; got != "Managed" {
			t.Fatalf("maasPortal = %v", got)
		}
	})
	t.Run("v2, core dashboard Removed", func(t *testing.T) {
		oc := newObsCluster(t, true)
		oc.f.putJSON(tDSC, `{"metadata":{"name":"default-dsc","resourceVersion":"5"},"spec":{"components":{"dashboard":{"managementState":"Removed"}}},
			"status":{"conditions":[{"type":"MaaSPortalAvailable","status":"False"}]}}`)
		if res := obsMaaSRun(t, oc.c, types.MaaSRequest{Action: "portal-on"}); !res.Success {
			t.Fatalf("on: %+v", res)
		}
		if got := ensureMap(ensureMap(ensureMap(ensureMap(oc.f.get(tDSC), "spec"), "components"), "dashboard"), "maasConsumerPortal")["managementState"]; got != "Managed" {
			t.Fatalf("maasConsumerPortal = %v", oc.f.get(tDSC)["spec"])
		}
	})
}

// MaaS's Clean up: a teardown that fails changes neither the portal nor the
// user IDs setting; a restore that fails makes the result fail.
func TestObservability_MaaSTeardownRestoresOnlyAfterItAndVisibly(t *testing.T) {
	oc := newObsCluster(t, true)
	oc.f.putJSON(tDSC, `{"metadata":{"name":"default-dsc","resourceVersion":"5"},"spec":{"components":{"dashboard":{"maasPortal":{"managementState":"Removed"}}}},"status":{}}`)
	if res := obsMaaSRun(t, oc.c, types.MaaSRequest{Action: "portal-on"}); !res.Success {
		t.Fatalf("on: %+v", res)
	}
	oc.f.fail["PATCH "+tDSC] = http.StatusInternalServerError
	res := obsMaaSRun(t, oc.c, types.MaaSRequest{Action: "teardown"})
	if res.Success || res.ErrorCode != "partial_failure" || !strings.Contains(res.Message, "The MaaS portal was not put back") {
		t.Fatalf("teardown: %+v", res)
	}
	delete(oc.f.fail, "PATCH "+tDSC)
	if res := obsMaaSRun(t, oc.c, types.MaaSRequest{Action: "teardown"}); !res.Success {
		t.Fatalf("teardown again: %+v", res)
	}
}

// The terminal commands keep strings as strings: the namespace's
// cluster-monitoring "true" and its empty node-selector.
func TestObservability_OperatorCommandsAreValidYAML(t *testing.T) {
	oc := newObsCluster(t, false)
	st, _ := GetObservabilityStatus(oc.sa)
	for _, p := range st.Parts {
		if p.ID == "loki-operator" {
			if !strings.Contains(p.Commands, `openshift.io/cluster-monitoring: "true"`) || !strings.Contains(p.Commands, `openshift.io/node-selector: ""`) {
				t.Fatalf("commands:\n%s", p.Commands)
			}
			return
		}
	}
	t.Fatal("no Loki part")
}
