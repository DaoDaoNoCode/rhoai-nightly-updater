package cluster

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/observability"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	obsNS      = "redhat-ods-monitoring"
	obsDSCI    = dsciV2List + "/default-dsci"
	obsMon     = "/apis/services.platform.opendatahub.io/v1alpha1/monitorings/default-monitoring"
	obsSCs     = "/apis/storage.k8s.io/v1/storageclasses"
	obsSecrets = "/api/v1/namespaces/" + obsNS + "/secrets"
	obsPVCs    = "/api/v1/namespaces/" + obsNS + "/persistentvolumeclaims"
)

func obsPackage(head, channel string) fakePackage {
	return fakePackage{source: "redhat-operators", defaultChannel: channel, order: []string{channel}, installModes: []string{"AllNamespaces"},
		channels: map[string]fakeChannel{channel: {head: head, entries: []string{head}}}}
}

// obsCluster is a cluster with RHOAI's monitoring Managed and nothing of
// the usage logs: the DSCInitialization's spec.monitoring has an empty
// metrics, the S3 storage is set up and secured, gp3-csi is the default
// StorageClass, and the four operators are installed (allOps). RHOAI's
// operator (react) deploys the stack when the fields are set: the
// Monitoring CR reports it, and the PVCs and Secrets its parts make appear,
// as on RHOAI 3.7.0-ea.1 (n8 spike); removing the fields reports them not
// configured, and the PVCs and the certificate Secret stay.
type obsCluster struct {
	f             *resourceFake
	sa, c         *Client
	sim           *fakeOLMSim
	pending       bool
	metrics, logs bool
	gen           int
	// neverReady: the usage logs stay not available (Loki does not schedule).
	neverReady bool
}

func newObsCluster(t *testing.T, allOps bool) *obsCluster {
	t.Helper()
	f, sa := newResourceFake(t)
	oc := &obsCluster{f: f, sa: sa, gen: 1}
	f.putJSON(obsDSCI, `{"metadata":{"name":"default-dsci","resourceVersion":"7"},"spec":{"applicationsNamespace":"redhat-ods-applications",
		"monitoring":{"managementState":"Managed","namespace":"`+obsNS+`","metrics":{}}}}`)
	putReadyManagedS3(f)
	f.putJSON(obsSCs+"/gp3-csi", `{"metadata":{"name":"gp3-csi","creationTimestamp":"2026-01-01T00:00:00Z","annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}`)
	f.putJSON(obsSCs+"/other", `{"metadata":{"name":"other","creationTimestamp":"2026-01-02T00:00:00Z"}}`)
	f.putJSON(obsSecrets+"/before-setup", `{"metadata":{"name":"before-setup","creationTimestamp":"2026-01-01T00:00:00Z"}}`)
	oc.sim = newFakeOLMSim(f)
	for pkg, p := range map[string]fakePackage{"openshift-cert-manager-operator": certManagerFakePackage(), "cluster-observability-operator": obsPackage("1.5.5", "stable"),
		"opentelemetry-product": obsPackage("0.158.0", "stable"), "loki-operator": obsPackage("6.7.0", "stable-6.7")} {
		oc.sim.packages[pkg] = p
		putFakePackage(f, pkg, p)
	}
	if allOps {
		oc.sim.installed("openshift-cert-manager-operator", "cert-manager-operator", "openshift-cert-manager-operator", "stable-v1", "1.20.1")
		oc.sim.installed("cluster-observability-operator", "openshift-cluster-observability-operator", "cluster-observability-operator", "stable", "1.5.5")
		oc.sim.installed("opentelemetry-product", "openshift-opentelemetry-operator", "opentelemetry-product", "stable", "0.158.0")
		oc.sim.installed("loki-operator", "openshift-operators-redhat", "loki-operator", "stable-6.7", "6.7.0")
	}
	oc.syncMonitoring()
	f.onMergePatch = func(r *http.Request, k fakeKey, patch, next map[string]any) {
		if k.plural != "dscinitializations" || r.URL.Query().Get("dryRun") != "" {
			return
		}
		mon := ensureMap(ensureMap(next, "spec"), "monitoring")
		metrics, _ := mon["metrics"].(map[string]any)
		oc.metrics, oc.logs, oc.pending = metrics["storage"] != nil, mon["usageLogs"] != nil, true
	}
	f.beforeServe = func(method, path string) {
		oc.sim.step(method, path)
		if method == http.MethodGet && strings.Contains(path, "/monitorings") && oc.pending {
			oc.pending = false
			oc.gen++
			oc.syncMonitoring()
		}
	}
	oc.c = userClientOf(sa, fakeUserToken)
	oc.c.SetUsername("kube:admin")
	return oc
}

// del deletes the object at path, as its owner's garbage collection does.
func (oc *obsCluster) del(path string) {
	k, _ := parseFakePath(path)
	oc.f.mu.Lock()
	delete(oc.f.objects, k)
	oc.f.mu.Unlock()
}

func obsCond(typ string, on bool, off string) map[string]any {
	if on {
		return map[string]any{"type": typ, "status": "True", "reason": "Available"}
	}
	return map[string]any{"type": typ, "status": "False", "reason": off, "severity": "Info", "message": "not configured in Monitoring CR"}
}

// syncMonitoring is RHOAI's operator after a change of the fields.
func (oc *obsCluster) syncMonitoring() {
	conds := []any{
		obsCond("MonitoringDependenciesReady", true, ""),
		obsCond("MonitoringStackAvailable", oc.metrics, "MetricsNotConfigured"),
		obsCond("PersesAvailable", oc.metrics, "MetricsAndTracesNotConfigured"),
		obsCond("OpenTelemetryCollectorAvailable", oc.metrics, "MetricsAndTracesNotConfigured"),
		obsCond("UsageLogsLokiStackAvailable", oc.logs && !oc.neverReady, "UsageLogsNotConfigured"),
		obsCond("UsageLogsCollectorAvailable", oc.logs, "UsageLogsNotConfigured"),
		obsCond("Ready", true, ""),
	}
	for _, c := range conds {
		c.(map[string]any)["observedGeneration"] = oc.gen
	}
	if oc.neverReady && oc.logs {
		conds[4] = map[string]any{"type": "UsageLogsLokiStackAvailable", "status": "False", "reason": "Pending", "message": "ingester pending", "observedGeneration": oc.gen}
	}
	oc.f.put(obsMon, map[string]any{"metadata": map[string]any{"name": "default-monitoring", "uid": "uid-monitoring", "generation": oc.gen}, "status": map[string]any{"conditions": conds}})
	owned := func(kind, name, uid string) []any {
		return []any{map[string]any{"kind": kind, "name": name, "uid": uid}}
	}
	collectors := "/apis/opentelemetry.io/v1beta1/namespaces/" + obsNS + "/opentelemetrycollectors/"
	certs := "/apis/cert-manager.io/v1/namespaces/" + obsNS + "/certificates/"
	oc.del(collectors + "data-science-collector")
	oc.del(collectors + "data-science-usage-logs")
	oc.del(certs + "data-science-collector-ca")
	if oc.metrics {
		oc.f.put(collectors+"data-science-collector", map[string]any{"metadata": map[string]any{"name": "data-science-collector", "uid": "uid-coll", "ownerReferences": owned("Monitoring", "default-monitoring", "uid-monitoring")}})
		oc.f.put(certs+"data-science-collector-ca", map[string]any{"metadata": map[string]any{"name": "data-science-collector-ca", "uid": "uid-cert", "ownerReferences": owned("OpenTelemetryCollector", "data-science-collector", "uid-coll")},
			"spec": map[string]any{"secretName": "data-science-collector-ca-cert"}})
	}
	if oc.logs {
		oc.f.put(collectors+"data-science-usage-logs", map[string]any{"metadata": map[string]any{"name": "data-science-usage-logs", "uid": "uid-ulc"}})
	}
	now := time.Now().UTC().Format(time.RFC3339)
	put := func(path, name string, extra map[string]any) {
		if oc.f.has(path + "/" + name) {
			return
		}
		meta := map[string]any{"name": name, "uid": "uid-" + name, "creationTimestamp": now}
		for k, v := range extra {
			meta[k] = v
		}
		oc.f.put(path+"/"+name, map[string]any{"metadata": meta, "spec": map[string]any{"resources": map[string]any{"requests": map[string]any{"storage": "10Gi"}}, "storageClassName": "gp3-csi"}})
	}
	if oc.metrics {
		put(obsPVCs, "prometheus-data-science-monitoringstack-db-prometheus-data-science-monitoringstack-0", nil)
		put(obsSecrets, "data-science-collector-ca-cert", map[string]any{"annotations": map[string]any{"cert-manager.io/certificate-name": "data-science-collector-ca"}})
		// Not the stack's: no evidence; made in the same window.
		put(obsSecrets, "someone-elses-secret", nil)
		put(obsPVCs, "someone-elses-pvc", nil)
		put(obsSecrets, "data-science-perses-dockercfg-x", map[string]any{"annotations": map[string]any{"kubernetes.io/service-account.name": "data-science-perses"}})
		put(obsSecrets, "owned-by-the-stack", map[string]any{"ownerReferences": []any{map[string]any{"kind": "LokiStack", "name": "x", "uid": "u"}}})
	}
	if oc.logs {
		put(obsPVCs, "storage-data-science-usage-logs-lokistack-ingester-0", nil)
	}
}

func obsRun(t *testing.T, c *Client, action string) (*types.OperationResponse, []UpdateStepEvent) {
	t.Helper()
	var events []UpdateStepEvent
	res, err := RunObservability(c, types.ObservabilityRequest{Action: action}, func(e UpdateStepEvent) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}
	return res, events
}

func obsRecord(t *testing.T, f *resourceFake) (observability.Record, bool) {
	t.Helper()
	raw, _ := ensureMap(f.get(obsDSCI), "metadata")["annotations"].(map[string]any)[observability.RecordAnnotation].(string)
	if raw == "" {
		return observability.Record{}, false
	}
	var rec observability.Record
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatal(err)
	}
	return rec, true
}

func obsMonitoring(f *resourceFake) map[string]any {
	return ensureMap(ensureMap(f.get(obsDSCI), "spec"), "monitoring")
}

func TestObservability_StatusOnAFreshCluster(t *testing.T) {
	oc := newObsCluster(t, true)
	st, err := GetObservabilityStatus(oc.sa)
	if err != nil {
		t.Fatal(err)
	}
	if st.Namespace != obsNS || st.Configured || st.ManagedByTool || st.Ready || st.SetupBlockedReason != "" || st.Blocked != nil {
		t.Fatalf("status = %+v", st)
	}
	states := map[string]string{}
	for _, p := range st.Parts {
		states[p.ID] = p.State
	}
	for id, want := range map[string]string{"cert-manager": "ready", "cluster-observability-operator": "ready", "opentelemetry": "ready", "loki-operator": "ready",
		"usage-logs-storage": "missing", "monitoring-settings": "missing", "monitoring-stack": "missing", "usage-logs": "missing", "dashboards": "missing"} {
		if states[id] != want {
			t.Errorf("part %s = %q, want %q (%v)", id, states[id], want, states)
		}
	}
	plan := strings.Join(st.SetupPlan, "\n")
	for _, want := range []string{"bucket rhoai-usage-logs", "SeaweedFS user rnu-usage-logs", "Secret redhat-ods-monitoring/rhoai-nightly-updater-usage-logs",
		"add spec.monitoring.metrics.storage (size 5Gi, retention 90d) and spec.monitoring.usageLogs (size 1x.demo", "Wait until"} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan lacks %q:\n%s", want, plan)
		}
	}
	if m := oc.f.mutations(); len(m) != 0 || len(oc.f.sw.iamCalls()) != 0 {
		t.Fatalf("the status changed something: %v %v", m, oc.f.sw.iamCalls())
	}
}

func TestObservability_SetUpAndCleanUp(t *testing.T) {
	oc := newObsCluster(t, true)
	res, events := obsRun(t, oc.c, "setup")
	if !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	if got := lastStatus(events, observability.StepReady); got != "success" {
		t.Fatalf("ready step = %q", got)
	}
	// The fields, as the docs write them, on the default StorageClass.
	assertJSONEqual(t, obsMonitoring(oc.f), `{"managementState":"Managed","namespace":"`+obsNS+`","metrics":{"storage":{"size":"5Gi","retention":"90d"}},
		"usageLogs":{"size":"1x.demo","storage":{"type":"s3","secretName":"rhoai-nightly-updater-usage-logs","credentialMode":"static","storageClassName":"gp3-csi"}}}`)
	// Every DSCInitialization write: strict, this tool's field manager, guarded.
	for _, call := range oc.f.callsTo(http.MethodPatch, obsDSCI) {
		if call.Query.Get("fieldValidation") != "Strict" || call.Query.Get("fieldManager") != observability.FieldManager || !strings.Contains(call.Body, `"resourceVersion"`) || call.Token != fakeUserToken {
			t.Errorf("DSCInitialization patch %+v", call)
		}
	}
	rec, ok := obsRecord(t, oc.f)
	if !ok || len(rec.Added) != 2 || string(rec.Added["usageLogs"].Previous) != "null" || rec.Before == nil || rec.Left == nil {
		t.Fatalf("record = %+v", rec)
	}
	if strings.Join(rec.Before.Secrets, ",") != "before-setup,"+storage.UsageLogsSecret {
		t.Errorf("before = %v", rec.Before.Secrets)
	}
	// What the stack leaves: its disks and the certificate, not the
	// ServiceAccount's Secret, an owned one, or this tool's own.
	if len(rec.Left.PVCs) != 2 || rec.Left.Secrets["data-science-collector-ca-cert"] == "" || len(rec.Left.Secrets) != 1 || rec.Left.Certificates["data-science-collector-ca"] != "uid-cert" {
		t.Fatalf("left = %+v", rec.Left)
	}
	// The Secret: this tool's, the seven documented keys, a key of its own.
	secret := oc.f.get(obsSecrets + "/" + storage.UsageLogsSecret)
	if secret == nil || !strings.Contains(mustJSON(secret), toolLabelJSON[1:len(toolLabelJSON)-1]) {
		t.Fatalf("secret = %v", secret)
	}
	data := ensureMap(secret, "data")
	if len(data) != 7 || secretValue(t, oc.f, obsSecrets+"/"+storage.UsageLogsSecret, "bucketnames") != rec.Bucket || !strings.HasPrefix(rec.Bucket, "rhoai-usage-logs-") ||
		secretValue(t, oc.f, obsSecrets+"/"+storage.UsageLogsSecret, "endpoint") != storage.S3Endpoint() {
		t.Fatalf("secret data = %v", data)
	}
	id := secretValue(t, oc.f, obsSecrets+"/"+storage.UsageLogsSecret, "access_key_id")
	if oc.f.sw.userOf(id) != rec.User || !oc.f.sw.buckets[rec.Bucket] || !strings.HasPrefix(rec.User, "rnu-usage-logs-") {
		t.Fatalf("key %s is %q's; buckets %v", id, oc.f.sw.userOf(id), oc.f.sw.buckets)
	}
	// Status after: set up by this tool, Clean up plans the data loss.
	st, err := GetObservabilityStatus(oc.sa)
	if err != nil || !st.ManagedByTool || !st.Ready || st.CleanupBlockedReason != "" {
		t.Fatalf("status after setup = %+v %v", st, err)
	}
	if plan := strings.Join(st.CleanupPlan, "\n"); !strings.Contains(plan, "Delete the 2 PersistentVolumeClaims") || !strings.Contains(plan, "bucket rhoai-usage-logs") {
		t.Errorf("cleanup plan:\n%s", plan)
	}
	// Set up again changes nothing (after the first time, which moves the
	// Secret's field ownership to server-side apply: EnsureToolObject).
	if res, _ := obsRun(t, oc.c, "setup"); !res.Success {
		t.Fatalf("setup again: %+v", res)
	}
	before := len(oc.f.mutations())
	if res, _ := obsRun(t, oc.c, "setup"); !res.Success || len(oc.f.mutations()) != before {
		t.Fatalf("setup again: %+v, %v", res, oc.f.mutations()[before:])
	}

	// Usage logs were written to the bucket.
	oc.f.sw.objects[rec.Bucket+"/application/chunk"] = []byte("x")
	// A disk someone made again under a recorded name is kept.
	oc.f.put(obsPVCs+"/storage-data-science-usage-logs-lokistack-ingester-0", map[string]any{"metadata": map[string]any{"name": "storage-data-science-usage-logs-lokistack-ingester-0", "uid": "someone-elses", "creationTimestamp": time.Now().UTC().Format(time.RFC3339)}})

	res, _ = obsRun(t, oc.c, "teardown")
	if !res.Success {
		t.Fatalf("teardown: %+v", res)
	}
	assertJSONEqual(t, obsMonitoring(oc.f), `{"managementState":"Managed","namespace":"`+obsNS+`","metrics":{}}`)
	if _, ok := obsRecord(t, oc.f); ok {
		t.Error("the record is still there")
	}
	for path, want := range map[string]bool{
		obsPVCs + "/prometheus-data-science-monitoringstack-db-prometheus-data-science-monitoringstack-0": false,
		obsPVCs + "/storage-data-science-usage-logs-lokistack-ingester-0":                                 true,
		obsSecrets + "/data-science-collector-ca-cert":                                                    false,
		obsSecrets + "/data-science-perses-dockercfg-x":                                                   true,
		obsSecrets + "/before-setup":                                                                      true,
		obsSecrets + "/someone-elses-secret":                                                              true,
		obsPVCs + "/someone-elses-pvc":                                                                    true,
		obsSecrets + "/" + storage.UsageLogsSecret:                                                        false,
	} {
		if oc.f.has(path) != want {
			t.Errorf("%s exists = %v, want %v", path, !want, want)
		}
	}
	if !strings.Contains(res.Message, "was made again or changed owner") {
		t.Errorf("message = %s", res.Message)
	}
	if _, ok := oc.f.sw.users[rec.User]; ok || oc.f.sw.buckets[rec.Bucket] {
		t.Errorf("SeaweedFS user or bucket left: %v %v", oc.f.sw.userNames(), oc.f.sw.buckets)
	}
	// Operators stay.
	for _, m := range oc.f.mutations() {
		if strings.HasPrefix(m, "DELETE") && strings.Contains(m, "operators.coreos.com") {
			t.Errorf("an operator was removed: %s", m)
		}
	}
	// Clean up again: nothing to do.
	if res, _ := obsRun(t, oc.c, "teardown"); !res.Success || !strings.Contains(res.Message, "Nothing to clean up") {
		t.Fatalf("teardown again: %+v", res)
	}
}

func TestObservability_SetUpRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		change     func(oc *obsCluster)
	}{
		{"no default StorageClass", "no default StorageClass", func(oc *obsCluster) {
			oc.f.putJSON(obsSCs+"/gp3-csi", `{"metadata":{"name":"gp3-csi"}}`)
		}},
		{"storage not secured", "needs secured S3 storage", func(oc *obsCluster) { putS3DeploymentArgs(oc.f, 1, unsecuredArgsJSON) }},
		{"no storage", "not set up: set it up on the S3 storage tab first", func(oc *obsCluster) {
			oc.f.mu.Lock()
			for k := range oc.f.objects {
				if k.ns == storage.S3Namespace || (k.plural == "namespaces" && k.name == storage.S3Namespace) {
					delete(oc.f.objects, k)
				}
			}
			oc.f.mu.Unlock()
		}},
		{"usage logs configured by someone else", "not by this tool", func(oc *obsCluster) {
			oc.f.putJSON(obsDSCI, `{"metadata":{"name":"default-dsci","resourceVersion":"7"},"spec":{"monitoring":{"managementState":"Managed","namespace":"`+obsNS+`","usageLogs":{"storage":{"type":"s3","secretName":"theirs"}}}}}`)
		}},
		{"monitoring turned off", "monitoring is turned off", func(oc *obsCluster) {
			oc.f.putJSON(obsDSCI, `{"metadata":{"name":"default-dsci","resourceVersion":"7"},"spec":{"monitoring":{"managementState":"Removed","namespace":"`+obsNS+`"}}}`)
		}},
		{"a Secret of the name not by this tool", "was not created by this tool", func(oc *obsCluster) {
			oc.f.putJSON(obsSecrets+"/"+storage.UsageLogsSecret, `{"metadata":{"name":"`+storage.UsageLogsSecret+`"}}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oc := newObsCluster(t, true)
			tc.change(oc)
			res, _ := obsRun(t, oc.c, "setup")
			if res.Success || !strings.Contains(res.Message, tc.want) {
				t.Fatalf("setup = %+v, want a refusal with %q", res, tc.want)
			}
			if m := oc.f.mutations(); len(m) != 0 || len(oc.f.sw.iamCalls()) != 0 {
				t.Fatalf("a refusal changed something: %v %v", m, oc.f.sw.iamCalls())
			}
		})
	}
}

func TestObservability_CleanUpRefusesFieldsChangedSinceSetup(t *testing.T) {
	oc := newObsCluster(t, true)
	if res, _ := obsRun(t, oc.c, "setup"); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	d := oc.f.get(obsDSCI)
	ensureMap(ensureMap(ensureMap(ensureMap(d, "spec"), "monitoring"), "usageLogs"), "storage")["storageClassName"] = "fast"
	oc.f.put(obsDSCI, d)
	before := len(oc.f.mutations())
	res, _ := obsRun(t, oc.c, "teardown")
	if res.Success || !strings.Contains(res.Message, "changed since this tool's setup") || len(oc.f.mutations()) != before {
		t.Fatalf("teardown = %+v, writes %v", res, oc.f.mutations()[before:])
	}
	st, _ := GetObservabilityStatus(oc.sa)
	if st.CleanupBlockedReason == "" {
		t.Error("the status offers Clean up")
	}
}

func TestObservability_CleanUpWithoutARecordChangesNothing(t *testing.T) {
	oc := newObsCluster(t, true)
	res, _ := obsRun(t, oc.c, "teardown")
	if !res.Success || !strings.Contains(res.Message, "not set up by this tool") || len(oc.f.mutations()) != 0 {
		t.Fatalf("teardown = %+v %v", res, oc.f.mutations())
	}
}

func TestObservability_InstallsLokiNextToROSAsNamespace(t *testing.T) {
	oc := newObsCluster(t, false)
	// ROSA: openshift-operators-redhat exists, without an OperatorGroup.
	oc.f.putJSON("/api/v1/namespaces/openshift-operators-redhat", `{"metadata":{"name":"openshift-operators-redhat"}}`)
	st, err := GetObservabilityStatus(oc.sa)
	if err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(st.SetupPlan, "\n")
	if !strings.Contains(plan, "Install Loki Operator 6.7.0: OperatorGroup openshift-operators-redhat/openshift-operators-redhat, Subscription openshift-operators-redhat/loki-operator (package loki-operator, channel stable-6.7") ||
		strings.Contains(plan, "Namespace openshift-operators-redhat") {
		t.Fatalf("plan:\n%s", plan)
	}
	if !strings.Contains(plan, "Install cert-manager Operator for Red Hat OpenShift") {
		t.Fatalf("cert-manager missing from the plan:\n%s", plan)
	}
	res, events := obsRun(t, oc.c, "setup")
	if !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	order := []string{}
	for _, e := range events {
		if e.Status == "running" && (len(order) == 0 || order[len(order)-1] != e.Step) {
			order = append(order, e.Step)
		}
	}
	if got := strings.Join(order, ","); !strings.HasPrefix(got, "observability-check,cert-manager,cluster-observability-operator,opentelemetry,loki-operator,usage-logs-storage") {
		t.Errorf("steps = %s", got)
	}
	ns := oc.f.get("/api/v1/namespaces/openshift-opentelemetry-operator")
	if ensureMap(ensureMap(ns, "metadata"), "labels")["openshift.io/cluster-monitoring"] != "true" {
		t.Errorf("OpenTelemetry namespace = %v", ns)
	}
	if labels := ensureMap(ensureMap(oc.f.get("/api/v1/namespaces/openshift-operators-redhat"), "metadata"), "labels"); len(labels) != 0 {
		t.Errorf("ROSA's namespace was changed: %v", labels)
	}
}

func TestObservability_CollectorStuckWithoutCertificates(t *testing.T) {
	oc := newObsCluster(t, true)
	if res, _ := obsRun(t, oc.c, "setup"); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	oc.f.putJSON("/api/v1/namespaces/"+obsNS+"/pods/data-science-collector-collector-0", `{"metadata":{"name":"data-science-collector-collector-0","creationTimestamp":"2000-01-01T00:00:00Z"},
		"spec":{"volumes":[{"name":"ta","secret":{"secretName":"data-science-collector-ta-client-cert"}}]},
		"status":{"phase":"Pending","containerStatuses":[{"state":{"waiting":{"reason":"ContainerCreating"}}}]}}`)
	oc.f.putJSON("/api/v1/namespaces/"+obsNS+"/events/e1", `{"metadata":{"name":"e1"},"reason":"FailedMount","involvedObject":{"name":"data-science-collector-collector-0"},
		"message":"MountVolume.SetUp failed for volume \"ta\" : secret \"data-science-collector-ta-client-cert\" not found"}`)
	st, err := GetObservabilityStatus(oc.sa)
	if err != nil {
		t.Fatal(err)
	}
	var part types.MaaSPart
	for _, p := range st.Parts {
		if p.ID == "metrics-collector" {
			part = p
		}
	}
	if part.State != "problem" || !strings.Contains(part.Message, "data-science-collector-ta-client-cert") || !strings.Contains(part.Message, "product bug") ||
		!strings.Contains(part.Commands, "Not in RHOAI docs; worked in our test") || !strings.Contains(part.Commands, "oc delete pod -n openshift-opentelemetry-operator -l app.kubernetes.io/name=opentelemetry-operator,control-plane=controller-manager") {
		t.Fatalf("part = %+v", part)
	}
	// Without the kubelet's event, a pod that only starts slowly is no problem.
	oc.f.mu.Lock()
	delete(oc.f.objects, fakeKey{gv: "v1", ns: obsNS, plural: "events", name: "e1"})
	oc.f.mu.Unlock()
	st, _ = GetObservabilityStatus(oc.sa)
	for _, p := range st.Parts {
		if p.ID == "metrics-collector" && p.State != "ready" {
			t.Fatalf("without the event: %+v", p)
		}
	}
}

func TestObservability_DashboardsNotInPersesYet(t *testing.T) {
	oc := newObsCluster(t, true)
	if res, _ := obsRun(t, oc.c, "setup"); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	oc.f.putJSON("/apis/perses.dev/v1alpha1/namespaces/"+obsNS+"/persesdashboards/dashboard-1-llm-d", `{"metadata":{"name":"dashboard-1-llm-d"},"status":{"conditions":[{"type":"Available","status":"False","reason":"PersesBackendError","message":"connection refused"}]}}`)
	oc.f.putJSON("/apis/perses.dev/v1alpha1/namespaces/"+obsNS+"/persesdashboards/usage", `{"metadata":{"name":"usage"},"status":{"conditions":[{"type":"Available","status":"True"}]}}`)
	// A failing datasource is not read: only the dashboards are.
	oc.f.putJSON("/apis/perses.dev/v1alpha1/namespaces/"+obsNS+"/persesdatasources/usage-logs-all", `{"metadata":{"name":"usage-logs-all"},"status":{"conditions":[{"type":"Available","status":"False","reason":"PersesBackendError","message":"connection refused"}]}}`)
	st, _ := GetObservabilityStatus(oc.sa)
	for _, p := range st.Parts {
		if p.ID == "dashboards" {
			if p.State != "waiting" || !strings.Contains(p.Message, "dashboard dashboard-1-llm-d (PersesBackendError: connection refused)") || !strings.Contains(p.Message, "retries them by itself") || strings.Contains(p.Message, "datasource") {
				t.Fatalf("dashboards = %+v", p)
			}
			noPersesDatasourceRequest(t, oc.f)
			return
		}
	}
	t.Fatal("no dashboards part")
}

// noPersesDatasourceRequest fails when any request reached PersesDatasources.
func noPersesDatasourceRequest(t *testing.T, f *resourceFake) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c.Path, "persesdatasources") {
			t.Fatalf("request to PersesDatasources: %s %s", c.Method, c.Path)
		}
	}
}

func TestObservability_S3StorageStaysWhileUsageLogsUseIt(t *testing.T) {
	oc := newObsCluster(t, true)
	if res, _ := obsRun(t, oc.c, "setup"); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	before := len(oc.f.mutations())
	res, err := TeardownS3Storage(oc.c, types.S3TeardownRequest{})
	if err != nil || res.Success || !strings.Contains(res.Message, "clean up Observability first") || len(oc.f.mutations()) != before {
		t.Fatalf("S3 teardown = %+v %v", res, err)
	}
}

const obsTenant = "/apis/maas.opendatahub.io/v1alpha1/namespaces/models-as-a-service/maastenantconfigs/default-tenant"

func obsMaaSRun(t *testing.T, c *Client, req types.MaaSRequest) *types.OperationResponse {
	t.Helper()
	res, err := RunMaaS(c, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestObservability_MaaSRecordsUserIDsAndPutsThemBack(t *testing.T) {
	oc := newObsCluster(t, true)
	oc.f.putJSON(obsTenant, `{"metadata":{"name":"default-tenant","resourceVersion":"3"},"spec":{}}`)
	if res := obsMaaSRun(t, oc.c, types.MaaSRequest{Action: "capture-user-off"}); res.Success || res.ErrorCode != "not_managed" {
		t.Fatalf("off without this tool's record: %+v", res)
	}
	if res := obsMaaSRun(t, oc.c, types.MaaSRequest{Action: "capture-user-on"}); !res.Success {
		t.Fatalf("on: %+v", res)
	}
	tc := oc.f.get(obsTenant)
	if v := ensureMap(ensureMap(ensureMap(tc, "spec"), "telemetry"), "logs")["captureUser"]; v != true {
		t.Fatalf("captureUser = %v", v)
	}
	if ensureMap(ensureMap(tc, "metadata"), "annotations")["rhoai-nightly-updater/capture-user-previous"] != "unset" {
		t.Fatalf("record = %v", ensureMap(tc, "metadata"))
	}
	for _, call := range oc.f.callsTo(http.MethodPatch, obsTenant) {
		if call.Token != fakeUserToken || call.Query.Get("fieldManager") != "rhoai-nightly-updater-capture-user" || call.Query.Get("fieldValidation") != "Strict" {
			t.Errorf("tenant patch %+v", call)
		}
	}
	st, err := GetMaaSStatus(oc.sa)
	if err != nil || st.UsageLogs == nil || st.UsageLogs.CaptureUser == nil || !*st.UsageLogs.CaptureUser || !st.UsageLogs.CaptureUserByTool || st.UsageLogs.State != "missing" {
		t.Fatalf("status = %+v %v", st.UsageLogs, err)
	}
	if res := obsMaaSRun(t, oc.c, types.MaaSRequest{Action: "capture-user-off"}); !res.Success {
		t.Fatalf("off: %+v", res)
	}
	tc = oc.f.get(obsTenant)
	if _, ok := ensureMap(ensureMap(tc, "spec"), "telemetry")["logs"].(map[string]any)["captureUser"]; ok {
		t.Fatalf("captureUser not unset: %v", tc["spec"])
	}
	if _, ok := ensureMap(ensureMap(tc, "metadata"), "annotations")["rhoai-nightly-updater/capture-user-previous"]; ok {
		t.Fatal("the record stays")
	}
}

func TestObservability_MaaSPortalTurnsOnAndBack(t *testing.T) {
	oc := newObsCluster(t, true)
	oc.f.putJSON(tDSC, `{"metadata":{"name":"default-dsc","resourceVersion":"5","managedFields":[{"manager":"someone","operation":"Update","fieldsV1":{"f:spec":{"f:components":{"f:dashboard":{"f:maasPortal":{"f:managementState":{}}}}}}}]},"spec":{"components":{"dashboard":{"managementState":"Managed","maasPortal":{"managementState":"Removed"}}}},
		"status":{"conditions":[{"type":"MaaSPortalAvailable","status":"False","message":"Submodule ManagementState is set to Removed"}]}}`)
	oc.f.putJSON("/apis/components.platform.opendatahub.io/v1alpha1/dashboards/default-dashboard", `{"metadata":{"name":"default-dashboard"},"spec":{"gateway":{"domain":"rh-ai.apps.example.com"}}}`)
	st, _ := GetMaaSStatus(oc.sa)
	if st.Portal == nil || st.Portal.On || st.Portal.Available != "False" || st.Portal.BlockedReason != "" {
		t.Fatalf("portal before = %+v", st.Portal)
	}
	if res := obsMaaSRun(t, oc.c, types.MaaSRequest{Action: "portal-on"}); !res.Success {
		t.Fatalf("on: %+v", res)
	}
	st, _ = GetMaaSStatus(oc.sa)
	if !st.Portal.On || !st.Portal.SetByTool || st.Portal.Previous != "Removed" || st.Portal.URL != "https://rh-ai.apps.example.com/maas-portal/" {
		t.Fatalf("portal after = %+v", st.Portal)
	}
	// Clean up MaaS puts it back first (here MaaS itself is not set up).
	if res := obsMaaSRun(t, oc.c, types.MaaSRequest{Action: "teardown"}); !res.Success || !slices.Contains(res.Logs, "OK: MaaS portal as before") {
		t.Fatalf("teardown = %+v", res)
	}
	dash := ensureMap(ensureMap(ensureMap(oc.f.get(tDSC), "spec"), "components"), "dashboard")
	if ensureMap(dash, "maasPortal")["managementState"] != "Removed" {
		t.Fatalf("portal = %v", dash)
	}
}

func TestObservability_MaaSRequestValidation(t *testing.T) {
	for req, want := range map[*types.MaaSRequest]string{
		{Action: "portal-on", Project: "p"}:            "takes no field",
		{Action: "publish", UsageLogs: true}:           "takes no usage logs choice",
		{Action: "capture-user-on", CaptureUser: true}: "takes no field",
	} {
		if got := ValidateMaaSRequest(*req); !strings.Contains(got, want) {
			t.Errorf("%+v: %q", *req, got)
		}
	}
	if got := ValidateMaaSRequest(types.MaaSRequest{Action: "setup", UsageLogs: true, CaptureUser: true}); got != "" {
		t.Errorf("setup with usage logs: %q", got)
	}
}

func TestObservability_MaaSSetupWithUsageLogsStopsWhenTheyCannot(t *testing.T) {
	oc := newObsCluster(t, true)
	putS3DeploymentArgs(oc.f, 1, unsecuredArgsJSON)
	var steps []string
	res, err := RunMaaS(oc.c, types.MaaSRequest{Action: "setup", UsageLogs: true}, func(e UpdateStepEvent) { steps = append(steps, e.Step+"="+e.Status) })
	if err != nil || res.Success || !strings.Contains(res.Message, "MaaS was not set up") {
		t.Fatalf("setup = %+v %v", res, err)
	}
	if got := strings.Join(steps, " "); !strings.HasPrefix(got, "observability-check=running observability-check=failed") {
		t.Fatalf("steps = %s", got)
	}
	if m := oc.f.mutations(); len(m) != 0 {
		t.Fatalf("changed: %v", m)
	}
}

// Clean up everything: MaaS publications first, then ..., the usage logs,
// then the S3 storage that keeps their data, and the MaaS platform after.
func TestCleanUp_UsageLogsGoBeforeTheS3Storage(t *testing.T) {
	ran := fakeTeardowns(t, nil)
	prevObs, prevUnpub, prevPlat := cleanupObservability, cleanupUnpublish, cleanupMaaSPlatform
	t.Cleanup(func() { cleanupObservability, cleanupUnpublish, cleanupMaaSPlatform = prevObs, prevUnpub, prevPlat })
	cleanupObservability = func(*Client) (*types.OperationResponse, error) {
		*ran = append(*ran, "observability")
		return &types.OperationResponse{Success: true, Message: "Usage logs removed."}, nil
	}
	cleanupUnpublish = func(_ *Client, project string) (*types.OperationResponse, error) {
		*ran = append(*ran, "maas-unpublish:"+project)
		return &types.OperationResponse{Success: true}, nil
	}
	cleanupMaaSPlatform = func(*Client) (*types.OperationResponse, error) {
		*ran = append(*ran, "maas-platform")
		return &types.OperationResponse{Success: true}, nil
	}
	_, c := newResourceFake(t)
	res, err := CleanUp(c, types.CleanupRequest{MaaSPublished: []string{"p"}, PipelineServers: []string{"a"}, Observability: true, Storage: true, MaaSPlatform: true}, nil)
	if err != nil || !res.Success {
		t.Fatalf("got %+v, %v", res, err)
	}
	if got := strings.Join(*ran, " "); got != "maas-unpublish:p pipeline-server:a observability storage maas-platform" {
		t.Fatalf("order: %s", got)
	}
}

func TestCleanUp_RefusesTheS3StorageWhileUsageLogsUseItAndAreNotSelected(t *testing.T) {
	ran := fakeTeardowns(t, nil)
	prev := usageLogsUseStorage
	t.Cleanup(func() { usageLogsUseStorage = prev })
	for _, tc := range []struct {
		name string
		used bool
		err  error
		want string
	}{
		{"used", true, nil, "select Usage logs and dashboards too"},
		{"cannot tell", false, errors.New("forbidden"), "Cannot tell whether RHOAI's usage logs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			*ran = nil
			usageLogsUseStorage = func(*Client) (bool, error) { return tc.used, tc.err }
			_, c := newResourceFake(t)
			var events []UpdateStepEvent
			res, err := CleanUp(c, types.CleanupRequest{Storage: true, MLflow: true}, func(e UpdateStepEvent) { events = append(events, e) })
			if err != nil || res.Success || !strings.Contains(res.Message, tc.want) || !strings.HasSuffix(res.Message, "Nothing was deleted.") || len(*ran) != 0 {
				t.Fatalf("got %+v, ran %v", res, *ran)
			}
			if len(events) == 0 || events[0].Step != "storage" || events[0].Status != "failed" {
				t.Errorf("events: %+v", events)
			}
		})
	}
	// Selected together, no check is needed: the usage logs go first.
	usageLogsUseStorage = func(*Client) (bool, error) { t.Fatal("checked"); return false, nil }
	prevObs := cleanupObservability
	t.Cleanup(func() { cleanupObservability = prevObs })
	cleanupObservability = func(*Client) (*types.OperationResponse, error) { return &types.OperationResponse{Success: true}, nil }
	_, c := newResourceFake(t)
	if res, _ := CleanUp(c, types.CleanupRequest{Storage: true, Observability: true}, nil); !res.Success {
		t.Fatalf("together: %+v", res)
	}
}
