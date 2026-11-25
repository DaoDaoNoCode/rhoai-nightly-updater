package cluster

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/observability"
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

// Ownership is by the names recorded before anything is made: a bucket or
// user of a freshly drawn name that exists is refused, never adopted; a
// retry of the same setup finds its own by the recorded names.
func TestObservability_StorageByRecordedNames(t *testing.T) {
	t.Run("collision", func(t *testing.T) {
		oc := newObsCluster(t, true)
		// Someone made a bucket of the name this setup draws.
		oc.f.sw.takeBuckets = true
		res, _ := obsRun(t, oc.c, "setup")
		if res.Success || res.ErrorCode != "conflict" || !strings.Contains(res.Message, "this tool did not make it") {
			t.Fatalf("setup: %+v", res)
		}
		// Nothing was made under the drawn names: they left the record.
		if rec, ok := obsRecord(t, oc.f); ok {
			t.Fatalf("record kept the drawn names: %+v", rec)
		}
		if len(oc.f.sw.users) != 0 {
			t.Errorf("made a user: %v", oc.f.sw.userNames())
		}
	})
	t.Run("user collision", func(t *testing.T) {
		oc := newObsCluster(t, true)
		oc.f.sw.takeUsers = true
		res, _ := obsRun(t, oc.c, "setup")
		if res.Success || res.ErrorCode != "conflict" {
			t.Fatalf("setup: %+v", res)
		}
		if _, ok := obsRecord(t, oc.f); ok {
			t.Fatal("record kept the drawn names")
		}
		// The bucket this setup made for the name went again.
		for b := range oc.f.sw.buckets {
			if strings.HasPrefix(b, "rhoai-usage-logs-") {
				t.Errorf("bucket %s left", b)
			}
		}
	})
	t.Run("retry and partial clean up", func(t *testing.T) {
		oc := newObsCluster(t, true)
		oc.f.sw.failIAM["CreateAccessKey"] = 500
		if res, _ := obsRun(t, oc.c, "setup"); res.Success {
			t.Fatalf("setup: %+v", res)
		}
		rec, _ := obsRecord(t, oc.f)
		if !oc.f.sw.buckets[rec.Bucket] {
			t.Fatalf("bucket %s not made", rec.Bucket)
		}
		delete(oc.f.sw.failIAM, "CreateAccessKey")
		// The retry finds its own bucket and user by the recorded names.
		obsSetUp(t, oc)
		again, _ := obsRecord(t, oc.f)
		if again.Bucket != rec.Bucket || again.User != rec.User {
			t.Fatalf("names changed: %+v -> %+v", rec, again)
		}
		// The user goes by hand first; Clean up deletes the bucket anyway.
		delete(oc.f.sw.users, rec.User)
		if res, _ := obsRun(t, oc.c, "teardown"); !res.Success {
			t.Fatalf("teardown: %+v", res)
		}
		if oc.f.sw.buckets[rec.Bucket] {
			t.Error("bucket left")
		}
		if _, ok := obsRecord(t, oc.f); ok {
			t.Error("record left")
		}
	})
	t.Run("record without names", func(t *testing.T) {
		oc := newObsCluster(t, true)
		d := oc.f.get(obsDSCI)
		ensureMap(d, "metadata")["annotations"] = map[string]any{"rhoai-nightly-updater/monitoring": `{"at":"2026-11-25T05:00:00Z","storage":true,"added":{}}`}
		oc.f.put(obsDSCI, d)
		res, _ := obsRun(t, oc.c, "teardown")
		if res.Success || !strings.Contains(res.Message, "names no bucket or user") || !strings.Contains(res.Message, "A cluster-admin does it by hand") {
			t.Fatalf("teardown: %+v", res)
		}
		if m := oc.f.mutations(); len(m) != 0 {
			t.Fatalf("changed: %v", m)
		}
	})
}

// The bucket is emptied in DeleteObjects batches, page by page.
func TestObservability_CleanUpEmptiesTheBucketInBatches(t *testing.T) {
	t.Cleanup(storage.SetUsageLogsListPageLimitForTest(2))
	oc := newObsCluster(t, true)
	obsSetUp(t, oc)
	rec, _ := obsRecord(t, oc.f)
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		oc.f.sw.objects[rec.Bucket+"/application/"+k] = []byte("x")
	}
	if res, _ := obsRun(t, oc.c, "teardown"); !res.Success {
		t.Fatalf("teardown: %+v", res)
	}
	if got := oc.f.sw.batchDeletes; len(got) != 3 || slices.Max(got) > 2 {
		t.Fatalf("batches = %v", got)
	}
	if oc.f.sw.buckets[rec.Bucket] {
		t.Error("bucket left")
	}
}

// A DeleteObjects answer that names failures, or is no DeleteResult, stops
// Clean up with the bucket kept.
func TestObservability_BatchDeleteFailuresAreErrors(t *testing.T) {
	for _, tc := range []struct{ mode, want string }{{"error", "not deleted"}, {"root", "not a DeleteResult"}} {
		t.Run(tc.mode, func(t *testing.T) {
			oc := newObsCluster(t, true)
			obsSetUp(t, oc)
			rec, _ := obsRecord(t, oc.f)
			oc.f.sw.objects[rec.Bucket+"/application/a"] = []byte("x")
			oc.f.sw.batchDeleteMode = tc.mode
			res, _ := obsRun(t, oc.c, "teardown")
			if res.Success || !strings.Contains(res.Message, tc.want) || !oc.f.sw.buckets[rec.Bucket] {
				t.Fatalf("teardown: %+v", res)
			}
		})
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
	rec, _ := obsRecord(t, oc.f)
	if !oc.f.has(obsSecrets+"/"+storage.UsageLogsSecret) || !oc.f.sw.buckets[rec.Bucket] {
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

// A Secret is the stack's only when a Certificate whose owner chain ends at
// the Monitoring CR names it; prometheus-web-tls-ca is listed, not deleted.
func TestObservability_CertificateSecretsNeedTheOwnerChain(t *testing.T) {
	oc := newObsCluster(t, true)
	// Made with the stack, but by a Certificate nobody of the stack owns.
	oc.f.beforeServe = func(prev func(string, string)) func(string, string) {
		return func(method, path string) {
			prev(method, path)
			if method == http.MethodGet && strings.Contains(path, "/monitorings") && oc.metrics && !oc.f.has(obsSecrets+"/stray-cert") {
				now := time.Now().UTC().Format(time.RFC3339)
				oc.f.put("/apis/cert-manager.io/v1/namespaces/"+obsNS+"/certificates/stray", map[string]any{"metadata": map[string]any{"name": "stray", "uid": "uid-stray"}, "spec": map[string]any{"secretName": "stray-cert"}})
				oc.f.put(obsSecrets+"/stray-cert", map[string]any{"metadata": map[string]any{"name": "stray-cert", "uid": "uid-stray-cert", "creationTimestamp": now, "annotations": map[string]any{"cert-manager.io/certificate-name": "stray"}}})
				oc.f.put(obsSecrets+"/prometheus-web-tls-ca", map[string]any{"metadata": map[string]any{"name": "prometheus-web-tls-ca", "uid": "uid-ca", "creationTimestamp": now}})
			}
		}
	}(oc.f.beforeServe)
	obsSetUp(t, oc)
	rec, _ := obsRecord(t, oc.f)
	if _, ok := rec.Left.Secrets["stray-cert"]; ok {
		t.Fatalf("recorded a Secret of a Certificate outside the stack: %+v", rec.Left)
	}
	if _, ok := rec.Left.Secrets["prometheus-web-tls-ca"]; ok {
		t.Fatal("recorded the web CA")
	}
	res, _ := obsRun(t, oc.c, "teardown")
	if !res.Success || !strings.Contains(res.Message, "oc delete secret -n redhat-ods-monitoring prometheus-web-tls-ca") {
		t.Fatalf("teardown: %+v", res)
	}
	if !oc.f.has(obsSecrets+"/stray-cert") || !oc.f.has(obsSecrets+"/prometheus-web-tls-ca") {
		t.Fatal("deleted a Secret without evidence")
	}
	if !strings.Contains(res.Message, "oc delete secret -n redhat-ods-monitoring prometheus-web-tls-ca stray-cert") {
		t.Fatalf("the unrecorded Secrets are not listed: %s", res.Message)
	}
}

// Set up extending an earlier setup keeps what it recorded: each field's
// value before, the disks it recorded and its time.
func TestObservability_ExtendingASetUpKeepsItsRecord(t *testing.T) {
	oc := newObsCluster(t, true)
	// An earlier setup of this tool added metrics.storage and recorded a disk.
	d := oc.f.get(obsDSCI)
	ensureMap(ensureMap(ensureMap(d, "spec"), "monitoring"), "metrics")["storage"] = map[string]any{"size": "5Gi", "retention": "90d"}
	ensureMap(d, "metadata")["annotations"] = map[string]any{"rhoai-nightly-updater/monitoring": `{"at":"2026-11-25T04:00:00Z","added":{"metrics.storage":{"previous":null,"set":{"size":"5Gi","retention":"90d"}}},"left":{"pvcs":{"old-disk":"uid-old"}}}`}
	oc.f.put(obsDSCI, d)
	obsSetUp(t, oc)
	rec, _ := obsRecord(t, oc.f)
	if rec.At != "2026-11-25T04:00:00Z" || rec.Left.PVCs["old-disk"] != "uid-old" || string(rec.Added["metrics.storage"].Previous) != "null" || rec.Added["usageLogs"].Set == nil {
		t.Fatalf("record = %+v", rec)
	}
}

// A Monitoring CR that cannot be read blocks Set up (and MaaS's checkbox),
// retryably, when usage logs would be added.
func TestObservability_UnreadableMonitoringBlocksSetUp(t *testing.T) {
	oc := newObsCluster(t, true)
	oc.f.fail["GET /apis/services.platform.opendatahub.io/v1alpha1/monitorings"] = http.StatusServiceUnavailable
	st, _ := GetObservabilityStatus(oc.sa)
	if !strings.Contains(st.SetupBlockedReason, "Cannot read RHOAI's Monitoring CR") {
		t.Fatalf("blocked = %q", st.SetupBlockedReason)
	}
	ms, _ := GetMaaSStatus(oc.sa)
	if ms.UsageLogs == nil || !strings.Contains(ms.UsageLogs.SetUpBlockedReason, "Cannot read RHOAI's Monitoring CR") {
		t.Fatalf("MaaS usage logs = %+v", ms.UsageLogs)
	}
	res, _ := obsRun(t, oc.c, "setup")
	if res.Success || res.ErrorCode == "" || len(oc.f.mutations()) != 0 {
		t.Fatalf("setup: %+v", res)
	}
}

// Putting the portal back after MaaS's teardown: a DataScienceCluster that
// cannot be read is a partial failure, not "no portal".
func TestObservability_MaaSRestoreReadFailureIsVisible(t *testing.T) {
	oc := newObsCluster(t, true)
	oc.f.putJSON(tDSC, `{"metadata":{"name":"default-dsc","resourceVersion":"5"},"spec":{"components":{"dashboard":{"maasPortal":{"managementState":"Removed"}}}},"status":{}}`)
	if res := obsMaaSRun(t, oc.c, types.MaaSRequest{Action: "portal-on"}); !res.Success {
		t.Fatalf("on: %+v", res)
	}
	oc.f.fail["GET /apis/datasciencecluster.opendatahub.io/v2/datascienceclusters"] = http.StatusServiceUnavailable
	logs, problem := observability.RestoreMaaSExtrasForTest(oc.c)
	if problem == "" || !strings.Contains(problem, "Cannot read the DataScienceCluster") {
		t.Fatalf("logs %v problem %q", logs, problem)
	}
}

// Putting user IDs and the portal back: a value only the API server's
// defaulting filled in (no manager owns it) is "unset", and goes back to
// absent; one someone wrote is put back as it was.
func TestObservability_RestoreRemovesWhatWasOnlyDefaulted(t *testing.T) {
	t.Run("captureUser defaulted", func(t *testing.T) {
		oc := newObsCluster(t, true)
		oc.f.putJSON(obsTenant, `{"metadata":{"name":"default-tenant","resourceVersion":"3"},"spec":{"telemetry":{"logs":{"captureUser":false}}}}`)
		if res := obsMaaSRun(t, oc.c, types.MaaSRequest{Action: "capture-user-on"}); !res.Success {
			t.Fatalf("on: %+v", res)
		}
		if got := ensureMap(ensureMap(oc.f.get(obsTenant), "metadata"), "annotations")["rhoai-nightly-updater/capture-user-previous"]; got != "unset" {
			t.Fatalf("recorded %v", got)
		}
		if res := obsMaaSRun(t, oc.c, types.MaaSRequest{Action: "capture-user-off"}); !res.Success {
			t.Fatalf("off: %+v", res)
		}
		if _, ok := ensureMap(ensureMap(ensureMap(oc.f.get(obsTenant), "spec"), "telemetry"), "logs")["captureUser"]; ok {
			t.Fatalf("captureUser not removed: %v", oc.f.get(obsTenant)["spec"])
		}
	})
	t.Run("captureUser written false", func(t *testing.T) {
		oc := newObsCluster(t, true)
		oc.f.putJSON(obsTenant, `{"metadata":{"name":"default-tenant","resourceVersion":"3","managedFields":[{"manager":"someone","operation":"Update","fieldsV1":{"f:spec":{"f:telemetry":{"f:logs":{"f:captureUser":{}}}}}}]},"spec":{"telemetry":{"logs":{"captureUser":false}}}}`)
		obsMaaSRun(t, oc.c, types.MaaSRequest{Action: "capture-user-on"})
		obsMaaSRun(t, oc.c, types.MaaSRequest{Action: "capture-user-off"})
		if got := ensureMap(ensureMap(ensureMap(oc.f.get(obsTenant), "spec"), "telemetry"), "logs")["captureUser"]; got != false {
			t.Fatalf("captureUser = %v", got)
		}
	})
	t.Run("portal defaulted", func(t *testing.T) {
		oc := newObsCluster(t, true)
		oc.f.putJSON(tDSC, `{"metadata":{"name":"default-dsc","resourceVersion":"5"},"spec":{"components":{"dashboard":{"maasPortal":{"managementState":"Removed"}}}},"status":{}}`)
		obsMaaSRun(t, oc.c, types.MaaSRequest{Action: "portal-on"})
		if st, _ := GetMaaSStatus(oc.sa); st.Portal.Previous != "unset" {
			t.Fatalf("previous = %q", st.Portal.Previous)
		}
		if res := obsMaaSRun(t, oc.c, types.MaaSRequest{Action: "portal-off"}); !res.Success {
			t.Fatalf("off: %+v", res)
		}
		if _, ok := ensureMap(ensureMap(ensureMap(ensureMap(oc.f.get(tDSC), "spec"), "components"), "dashboard"), "maasPortal")["managementState"]; ok {
			t.Fatalf("managementState not removed: %v", oc.f.get(tDSC)["spec"])
		}
	})
}

// A record without names: Clean up lists every step for a cluster-admin.
func TestObservability_RecordWithoutNamesListsEveryStep(t *testing.T) {
	oc := newObsCluster(t, true)
	d := oc.f.get(obsDSCI)
	ensureMap(ensureMap(ensureMap(d, "spec"), "monitoring"), "usageLogs")["size"] = "1x.demo"
	ensureMap(d, "metadata")["annotations"] = map[string]any{"rhoai-nightly-updater/monitoring": `{"at":"2026-11-25T05:00:00Z","storage":true,"added":{"usageLogs":{"previous":null,"set":{"size":"1x.demo"}}},"left":{"pvcs":{"wal-data-science-usage-logs-lokistack-ingester-0":"u"},"secrets":{"data-science-collector-ca-cert":"s"}}}`}
	oc.f.put(obsDSCI, d)
	res, _ := obsRun(t, oc.c, "teardown")
	for _, want := range []string{
		`oc patch dscinitialization --all --type=merge -p '{"spec":{"monitoring":{"usageLogs":null}}}'`,
		"wait until RHOAI's operator deleted the stack",
		"oc delete pvc -n redhat-ods-monitoring wal-data-science-usage-logs-lokistack-ingester-0",
		"oc delete secret -n redhat-ods-monitoring data-science-collector-ca-cert",
		"oc delete secret -n redhat-ods-monitoring rhoai-nightly-updater-usage-logs",
		"SeaweedFS admin UI",
		"oc annotate dscinitialization --all rhoai-nightly-updater/monitoring-",
	} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message lacks %q:\n%s", want, res.Message)
		}
	}
	if res.Success || len(oc.f.mutations()) != 0 {
		t.Fatalf("teardown: %+v %v", res, oc.f.mutations())
	}
	st, _ := GetObservabilityStatus(oc.sa)
	if st.CleanupBlockedReason == "" {
		t.Fatal("Clean up offered")
	}
}

// maas-api's Loki read proxy (LOKI_READ_ENDPOINT on its container, which
// maas-controller sets while Secret usage-logs-loki exists and then points
// the usage PersesDatasources at maas-api) gets a warning, told from the
// maas-api Deployment's variable names only: no value reaches the status,
// and no PersesDatasource is read.
func TestObservability_WarnsWhenMaaSAPIReadsLoki(t *testing.T) {
	const secretURL = "https://hub-loki.example.internal:8080/api/logs/v1/application"
	deployment := "/apis/apps/v1/namespaces/redhat-ai-gateway-infra/deployments/maas-api"
	for _, tc := range []struct {
		name       string
		containers []any // nil: no maas-api Deployment
		warn       bool
	}{
		{"no maas-api", nil, false},
		{"proxy off", []any{map[string]any{"name": "maas-api", "env": []any{map[string]any{"name": "API_KEY_MAX_EXPIRATION_DAYS", "value": "90"}}}}, false},
		{"variable on another container", []any{
			map[string]any{"name": "maas-api"},
			map[string]any{"name": "sidecar", "env": []any{map[string]any{"name": "LOKI_READ_ENDPOINT", "value": secretURL}}},
		}, false},
		{"proxy on", []any{map[string]any{"name": "maas-api", "env": []any{
			map[string]any{"name": "LOKI_READ_ENDPOINT", "value": secretURL},
			map[string]any{"name": "LOKI_READ_TOKEN", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "usage-logs-loki", "key": "LOKI_READ_TOKEN"}}},
		}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oc := newObsCluster(t, true)
			obsSetUp(t, oc)
			if tc.containers != nil {
				oc.f.put(deployment, map[string]any{"metadata": map[string]any{"name": "maas-api", "namespace": "redhat-ai-gateway-infra"},
					"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": tc.containers}}}})
			}
			st, err := GetObservabilityStatus(oc.sa)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(st)
			if strings.Contains(string(raw), "hub-loki") {
				t.Fatalf("the variable's value reached the status: %s", raw)
			}
			noPersesDatasourceRequest(t, oc.f)
			for _, p := range st.Parts {
				if p.ID != "dashboards" {
					continue
				}
				if !tc.warn && p.Warning != "" {
					t.Fatalf("warning without the proxy: %s", p.Warning)
				}
				if tc.warn && (!strings.Contains(p.Warning, "LOKI_READ_ENDPOINT") || !strings.Contains(p.Warning, "500 AUTH_FAILURE") || !strings.Contains(p.Warning, "usage-logs-loki") || !strings.Contains(p.Warning, "usage-logs-all")) {
					t.Fatalf("warning = %q", p.Warning)
				}
				return
			}
			t.Fatal("no dashboards part")
		})
	}
}

// Metadata-only reads use the form of what they read: an object read asked
// for as a list (the live bug: 500 "content is not a list") or a list asked
// for as one object is refused, by the fake as by the API server.
func TestObservability_MetadataReadsUseTheirOwnForm(t *testing.T) {
	oc := newObsCluster(t, true)
	if _, _, err := kube.ReadObjectMeta(oc.c.MetadataOnly(), obsSecrets+"/before-setup"); err == nil || !strings.Contains(err.Error(), "content is not a list") {
		t.Fatalf("an object read with the list form: %v", err)
	}
	var items []map[string]any
	if err := oc.c.MetadataOnlyObject().ListInto(obsSecrets, &items); err == nil {
		t.Fatal("a list read with the object form was answered")
	}
	obsSetUp(t, oc)
	if res, _ := obsRun(t, oc.c, "teardown"); !res.Success {
		t.Fatalf("teardown: %+v", res)
	}
	for _, call := range oc.f.callsTo(http.MethodGet, "/apis/opentelemetry.io/v1beta1/namespaces/"+obsNS+"/opentelemetrycollectors/data-science-usage-logs") {
		if !strings.Contains(call.Accept, "as=PartialObjectMetadata;") {
			t.Errorf("collector read Accept = %q", call.Accept)
		}
	}
}
