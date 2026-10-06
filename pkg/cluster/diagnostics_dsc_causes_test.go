package cluster

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	packageManifestsPath = "/apis/packages.operators.coreos.com/v1/namespaces/openshift-marketplace/packagemanifests"
	clusterCSVsPath      = "/apis/operators.coreos.com/v1alpha1/clusterserviceversions"
	componentV1alpha1    = "/apis/components.platform.opendatahub.io/v1alpha1"
	trainerOperatorPath  = "/apis/apps/v1/namespaces/redhat-ods-applications/deployments/trainer-operator-controller-manager"
	jobSetOperandPath    = "/apis/operator.openshift.io/v1/jobsetoperators/cluster"
)

func ago(d time.Duration) string { return time.Now().Add(-d).UTC().Format(time.RFC3339) }

type dcond map[string]interface{}

// dscWorld serves a cluster with a DataScienceCluster, the live catalog,
// the component API (trainers and rays), no webhooks, and the trainer
// operator Deployment.
type dscWorld struct {
	f *fakeAPI
	c *Client
}

func newDSCWorld(t *testing.T, dscConds []dcond) *dscWorld {
	t.Helper()
	f, c := newFakeAPI(t)
	dsc := map[string]interface{}{"metadata": map[string]interface{}{"name": "default-dsc"}, "status": map[string]interface{}{"conditions": dscConds}}
	f.obj("GET", dscV2List, map[string]interface{}{"items": []interface{}{dsc}})
	f.obj("GET", dsciV2List, map[string]interface{}{"items": []interface{}{map[string]interface{}{
		"metadata": map[string]string{"name": "default-dsci"}, "spec": map[string]string{"applicationsNamespace": "redhat-ods-applications"}, "status": map[string]string{"phase": "Ready"},
	}}})
	f.obj("GET", packageManifestsPath, map[string]interface{}{"items": liveCatalog()})
	f.json("GET", clusterCSVsPath, 200, `{"items":[]}`)
	serveComponentGroup(f)
	f.json("GET", componentV1alpha1, 200, `{"resources":[{"name":"trainers","kind":"Trainer"},{"name":"trainers/status","kind":"Trainer"},{"name":"rays","kind":"Ray"}]}`)
	f.json("GET", componentV1alpha1+"/trainers", 200, `{"items":[]}`)
	f.json("GET", componentV1alpha1+"/rays", 200, `{"items":[]}`)
	for _, r := range []string{"validatingwebhookconfigurations", "mutatingwebhookconfigurations"} {
		f.json("GET", "/apis/admissionregistration.k8s.io/v1/"+r, 200, `{"items":[]}`)
	}
	f.obj("GET", trainerOperatorPath, operatorDeploymentJSON("trainer-operator-controller-manager", "RollingUpdate", 1))
	f.json("GET", "/apis/operator.openshift.io/v1", 200, `{"resources":[{"name":"jobsetoperators","kind":"JobSetOperator"},{"name":"jobsetoperators/status","kind":"JobSetOperator"}]}`)
	return &dscWorld{f: f, c: c}
}

func operatorDeploymentJSON(name, strategy string, available int) map[string]interface{} {
	return map[string]interface{}{
		"metadata": map[string]interface{}{"name": name, "namespace": "redhat-ods-applications", "uid": "dep-uid", "resourceVersion": "7",
			"ownerReferences": []interface{}{map[string]string{"kind": "Platform", "name": "default"}}},
		"spec": map[string]interface{}{"replicas": 1, "strategy": map[string]string{"type": strategy},
			"template": map[string]interface{}{"metadata": map[string]interface{}{"labels": map[string]string{"app.kubernetes.io/name": name}}}},
		"status": map[string]interface{}{"availableReplicas": available},
	}
}

// installedCSV serves one installed CSV. The JobSet CSV carries its own
// alm-examples and owned CRD, as on the live cluster.
func (w *dscWorld) installedCSV(name, ns, display, phase string, since time.Duration, pkg string) {
	w.installedCSVs(csvItem(name, ns, display, phase, since, pkg, jobSetExamples, []ownedCRD{{Name: "jobsetoperators.operator.openshift.io", Kind: "JobSetOperator", Version: "v1"}}))
}

func csvItem(name, ns, display, phase string, since time.Duration, pkg, examples string, owned []ownedCRD) map[string]interface{} {
	return map[string]interface{}{
		"metadata": map[string]interface{}{"name": name, "namespace": ns, "labels": map[string]string{"operators.coreos.com/" + pkg + "." + ns: ""},
			"annotations": map[string]string{"alm-examples": examples}},
		"spec":   map[string]interface{}{"displayName": display, "customresourcedefinitions": map[string]interface{}{"owned": owned}},
		"status": map[string]string{"phase": phase, "lastTransitionTime": ago(since)},
	}
}

func (w *dscWorld) installedCSVs(items ...map[string]interface{}) {
	var list []interface{}
	for _, it := range items {
		list = append(list, it)
	}
	w.f.obj("GET", clusterCSVsPath, map[string]interface{}{"items": list})
}

func (w *dscWorld) trainerCR(conds []dcond, generation, observed int) {
	w.f.obj("GET", componentV1alpha1+"/trainers", map[string]interface{}{"items": []interface{}{map[string]interface{}{
		"metadata": map[string]interface{}{"name": "default-trainer", "generation": generation},
		"status":   map[string]interface{}{"observedGeneration": observed, "conditions": conds},
	}}})
}

func problemsByID(out checkOutput) map[string]Problem {
	m := map[string]Problem{}
	for _, p := range out.problems {
		m[p.ID] = p
	}
	return m
}

func ids(out checkOutput) []string {
	var s []string
	for _, p := range out.problems {
		s = append(s, p.ID)
	}
	return s
}

// The live state before any prerequisite was installed.
func liveMissingPrereqs() []dcond {
	return []dcond{
		{"type": "Ready", "status": "False", "reason": "Error", "message": "Some modules are not ready: trainer"},
		{"type": "TrainerReady", "status": "False", "reason": "Error", "message": msgTrainerJobSet, "lastTransitionTime": ago(time.Hour)},
		{"type": "KserveLLMInferenceServiceDependencies", "status": "False", "reason": "PreConditionFailed", "message": msgCertManager},
		{"type": "KserveLLMInferenceServiceWideEPDependencies", "status": "False", "reason": "PreConditionFailed", "severity": "Info", "message": msgWideEP},
		{"type": "KueueReady", "status": "False", "reason": "Removed", "severity": "Info", "message": "Component ManagementState is set to Removed"},
	}
}

func TestDSCCheck_MissingPrerequisitesGetCommandsAndClassification(t *testing.T) {
	w := newDSCWorld(t, liveMissingPrereqs())
	out := checkDataScienceCluster(w.c)
	byID := problemsByID(out)
	if out.check.Status != "fail" {
		t.Fatalf("check = %+v", out.check)
	}
	for _, id := range []string{"dsc-not-ready", "prerequisite-missing-job-set", "prerequisite-missing-openshift-cert-manager-operator", "prerequisite-missing-leader-worker-set"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("missing %s in %v", id, ids(out))
		}
	}
	if len(out.problems) != 4 {
		t.Fatalf("problems = %v", ids(out))
	}

	js := byID["prerequisite-missing-job-set"]
	if js.Severity != "warning" || js.AutoFixable || !strings.Contains(js.Description, "keeps the DataScienceCluster from being Ready") ||
		!strings.Contains(js.TechnicalCmd, "name: openshift-jobset-operator") || !strings.Contains(js.TechnicalCmd, "oc get jobsetoperator.operator.openshift.io/cluster") ||
		!strings.Contains(js.Fix, "the tool does not install operators") {
		t.Fatalf("job-set problem = %+v", js)
	}
	if !strings.Contains(strings.Join(js.Evidence, "\n"), "TrainerReady=False (Error): dependency not met: JobSet Operator is not installed") {
		t.Fatalf("job-set evidence = %q", js.Evidence)
	}

	// cert-manager is named by a blocking and an optional condition: it
	// blocks. The community package is mentioned, not chosen.
	cm := byID["prerequisite-missing-openshift-cert-manager-operator"]
	ev := strings.Join(cm.Evidence, "\n")
	if cm.Severity != "warning" || !strings.Contains(ev, "KserveLLMInferenceServiceDependencies") || !strings.Contains(ev, "KserveLLMInferenceServiceWideEPDependencies") ||
		!strings.Contains(ev, "package cert-manager in community-operators also matches (community, not supported by Red Hat") ||
		!strings.Contains(cm.TechnicalCmd, "source: redhat-operators") {
		t.Fatalf("cert-manager problem = %+v", cm)
	}

	// LeaderWorkerSet is only named by an Info condition: optional.
	lws := byID["prerequisite-missing-leader-worker-set"]
	if lws.Severity != "info" || !strings.Contains(lws.Description, "only gates an optional feature") {
		t.Fatalf("lws problem = %+v", lws)
	}

	dsc := byID["dsc-not-ready"]
	ev = strings.Join(dsc.Evidence, "\n")
	if !strings.Contains(ev, "TrainerReady=False (Error): dependency not met: JobSet Operator is not installed") ||
		!strings.Contains(ev, "→ cause: missing prerequisite operator Job Set Operator is not installed") ||
		strings.Contains(ev, "KueueReady") || strings.Contains(dsc.Fix, "install a missing dependency operator") {
		t.Fatalf("dsc-not-ready = %+v", dsc)
	}
	for _, id := range []string{"prerequisite-missing-job-set", "prerequisite-missing-openshift-cert-manager-operator"} {
		if !containsString(dsc.RelatedProblems, id) {
			t.Fatalf("related = %v", dsc.RelatedProblems)
		}
	}
	assertWrites(t, w.f)
}

func TestDSCCheck_ReadyWithOptionalPrerequisiteOnly(t *testing.T) {
	w := newDSCWorld(t, []dcond{
		{"type": "Ready", "status": "True"},
		{"type": "KserveLLMInferenceServiceWideEPDependencies", "status": "False", "reason": "PreConditionFailed", "severity": "Info", "message": "LeaderWorkerSet not installed"},
	})
	out := checkDataScienceCluster(w.c)
	if out.check.Status != "pass" || strings.Join(ids(out), ",") != "prerequisite-missing-leader-worker-set" ||
		!strings.Contains(out.check.Detail, "optional prerequisites not installed: Leader Worker Set Operator") {
		t.Fatalf("out = %+v %v", out.check, ids(out))
	}
}

func TestDSCCheck_HealthyClusterReadsNoCatalog(t *testing.T) {
	w := newDSCWorld(t, []dcond{{"type": "Ready", "status": "True"}, {"type": "TrainerReady", "status": "True"}})
	out := checkDataScienceCluster(w.c)
	if out.check.Status != "pass" || len(out.problems) != 0 {
		t.Fatalf("out = %+v %v", out.check, ids(out))
	}
	if n := len(w.f.requests("GET", packageManifestsPath)); n != 0 {
		t.Fatalf("listed package manifests %d times on a healthy cluster", n)
	}
}

func TestDSCCheck_InstalledButOperandMissing(t *testing.T) {
	w := newDSCWorld(t, liveMissingPrereqs()[:2])
	w.installedCSV("jobset-operator.v1.0.1", "openshift-jobset-operator", "Job Set Operator", "Succeeded", time.Hour, "job-set")
	out := checkDataScienceCluster(w.c)
	byID := problemsByID(out)
	p, ok := byID["prerequisite-operand-job-set"]
	if !ok || p.AutoFixable || !strings.Contains(p.Title, "its JobSetOperator/cluster does not exist") ||
		!strings.HasPrefix(p.TechnicalCmd, "oc get jobsetoperator.operator.openshift.io/cluster >/dev/null 2>&1 || oc create -f - <<'EOF'") {
		t.Fatalf("problems = %v, operand = %+v", ids(out), p)
	}
	if _, ok := byID["module-operator-backoff-trainer"]; ok {
		t.Fatal("a restart is offered although the operand is missing")
	}
	if !strings.Contains(strings.Join(byID["dsc-not-ready"].Evidence, "\n"), "installed, but JobSetOperator/cluster does not exist") {
		t.Fatalf("dsc evidence = %q", byID["dsc-not-ready"].Evidence)
	}
}

func TestDSCCheck_SatisfiedDependencyOffersRestart(t *testing.T) {
	w := newDSCWorld(t, liveMissingPrereqs()[:2])
	w.installedCSV("jobset-operator.v1.0.1", "openshift-jobset-operator", "Job Set Operator", "Succeeded", time.Hour, "job-set")
	w.f.json("GET", jobSetOperandPath, 200, `{"metadata":{"name":"cluster"}}`)
	w.trainerCR([]dcond{{"type": "Ready", "status": "False", "reason": "Error", "message": msgTrainerJobSet, "lastTransitionTime": ago(time.Hour)}}, 3, 3)
	out := checkDataScienceCluster(w.c)
	byID := problemsByID(out)
	b, ok := byID["module-operator-backoff-trainer"]
	if !ok || !b.AutoFixable || b.AutoFixAction != "restart-module-operator:trainer" || b.Severity != "warning" ||
		!strings.Contains(b.ConfirmMessage, "kubectl.kubernetes.io/restartedAt") || strings.Join(b.AffectedObjects, ",") != "Deployment redhat-ods-applications/trainer-operator-controller-manager" {
		t.Fatalf("problems = %v, backoff = %+v", ids(out), b)
	}
	if ev := strings.Join(b.Evidence, "\n"); !strings.Contains(ev, "Trainer default-trainer: Ready=False") || !strings.Contains(ev, "JobSetOperator/cluster exists") {
		t.Fatalf("evidence = %s", ev)
	}
	if _, ok := byID["prerequisite-missing-job-set"]; ok {
		t.Fatal("an installed prerequisite is reported missing")
	}
	dsc := byID["dsc-not-ready"]
	if !containsString(dsc.RelatedProblems, "module-operator-backoff-trainer") ||
		!strings.Contains(strings.Join(dsc.Evidence, "\n"), "the trainer module operator has not retried") {
		t.Fatalf("dsc-not-ready = %+v", dsc)
	}
	assertWrites(t, w.f)
}

// The operand comes from the installed CSV, not from the preferred catalog
// entry: with the community cert-manager installed (no singleton operand),
// the Red Hat package's CertManager/cluster is not asked for.
func TestDSCCheck_OperandFromInstalledCSVPackage(t *testing.T) {
	w := newDSCWorld(t, []dcond{
		{"type": "Ready", "status": "False", "reason": "Error", "message": "Some modules are not ready: kserve"},
		{"type": "KserveLLMInferenceServiceDependencies", "status": "False", "reason": "PreConditionFailed", "message": msgCertManager},
	})
	w.installedCSVs(csvItem("cert-manager.v1.16.5", "openshift-operators", "cert-manager", "Succeeded", time.Hour, "cert-manager", `[]`, nil))
	out := checkDataScienceCluster(w.c)
	for _, id := range ids(out) {
		if strings.HasPrefix(id, "prerequisite-") {
			t.Fatalf("installed community cert-manager reported as %s", id)
		}
	}
	if len(w.f.requests("GET", "/apis/operator.openshift.io/v1alpha1")) != 0 {
		t.Fatal("looked up the Red Hat package's operand")
	}

	// The installed CSV's own example is used even when the catalog's differs.
	w2 := newDSCWorld(t, liveMissingPrereqs()[:2])
	w2.installedCSVs(csvItem("jobset-operator.v0.9.0", "openshift-jobset-operator", "Job Set Operator", "Succeeded", time.Hour, "job-set",
		`[{"apiVersion":"operator.openshift.io/v1","kind":"JobSetOperator","metadata":{"name":"cluster"},"spec":{"managementState":"Managed","logLevel":"Debug"}}]`, nil))
	p := problemsByID(checkDataScienceCluster(w2.c))["prerequisite-operand-job-set"]
	if !strings.Contains(p.TechnicalCmd, `"logLevel": "Debug"`) {
		t.Fatalf("operand command = %s", p.TechnicalCmd)
	}
}

func TestDSCCheck_JustInstalledDependencyIsNotBackoff(t *testing.T) {
	w := newDSCWorld(t, liveMissingPrereqs()[:2])
	w.installedCSV("jobset-operator.v1.0.1", "openshift-jobset-operator", "Job Set Operator", "Succeeded", 30*time.Second, "job-set")
	w.f.json("GET", jobSetOperandPath, 200, `{}`)
	out := checkDataScienceCluster(w.c)
	if _, ok := problemsByID(out)["module-operator-backoff-trainer"]; ok {
		t.Fatalf("problems = %v", ids(out))
	}
	if ev := strings.Join(problemsByID(out)["dsc-not-ready"].Evidence, "\n"); !strings.Contains(ev, "is installed now; the module picks it up on its next retry") {
		t.Fatalf("evidence = %s", ev)
	}
}

func TestDSCCheck_OperandForbiddenSaysMakeUpgrade(t *testing.T) {
	w := newDSCWorld(t, liveMissingPrereqs()[:2])
	w.installedCSV("jobset-operator.v1.0.1", "openshift-jobset-operator", "Job Set Operator", "Succeeded", time.Hour, "job-set")
	w.f.status("GET", jobSetOperandPath, http.StatusForbidden, "Forbidden")
	out := checkDataScienceCluster(w.c)
	byID := problemsByID(out)
	// An operand the tool cannot read does not authorize a restart.
	if _, ok := byID["module-operator-backoff-trainer"]; ok {
		t.Fatalf("restart offered for an unverified operand: %v", ids(out))
	}
	p, ok := byID["prerequisite-operand-job-set"]
	if !ok || p.AutoFixable || !strings.Contains(strings.Join(p.Evidence, "\n"), "make upgrade") ||
		!strings.Contains(p.Fix, "offers no restart") || !strings.HasPrefix(p.TechnicalCmd, "oc get jobsetoperator.operator.openshift.io/cluster") {
		t.Fatalf("problems = %v, operand = %+v", ids(out), p)
	}
	if ev := strings.Join(byID["dsc-not-ready"].Evidence, "\n"); !strings.Contains(ev, "whether JobSetOperator/cluster exists could not be checked") {
		t.Fatalf("dsc evidence = %s", ev)
	}
}

// ageStaleObservations moves every remembered stale observation back by d,
// standing in for the time between two scans.
func ageStaleObservations(d time.Duration) {
	staleSeen.Lock()
	defer staleSeen.Unlock()
	for k, v := range staleSeen.m {
		v.first = v.first.Add(-d)
		staleSeen.m[k] = v
	}
}

func TestDSCCheck_StaleModuleIsBackoffOnlyAcrossScans(t *testing.T) {
	w := newDSCWorld(t, []dcond{
		{"type": "Ready", "status": "False", "reason": "Error", "message": "Some modules are not ready: ray"},
		// An old condition says nothing about when the spec changed.
		{"type": "RayReady", "status": "False", "reason": "NotReady", "message": msgRayStale, "lastTransitionTime": ago(time.Hour)},
	})
	ray := func(generation int) {
		w.f.obj("GET", componentV1alpha1+"/rays", map[string]interface{}{"items": []interface{}{map[string]interface{}{
			"metadata": map[string]interface{}{"name": "default-ray", "uid": "ray-uid", "generation": generation},
			"status":   map[string]interface{}{"observedGeneration": 3, "conditions": []dcond{{"type": "Ready", "status": "True", "lastTransitionTime": ago(time.Hour)}}},
		}}})
	}
	ray(4)
	w.f.obj("GET", "/apis/apps/v1/namespaces/redhat-ods-applications/deployments/ray-module-operator-controller-manager", operatorDeploymentJSON("ray-module-operator-controller-manager", "RollingUpdate", 1))
	backoff := func() (Problem, bool) {
		p, ok := problemsByID(checkDataScienceCluster(w.c))["module-operator-backoff-ray"]
		return p, ok
	}

	// First scan: the lag is new to the tool, however old the conditions.
	if _, ok := backoff(); ok {
		t.Fatal("a restart was offered on the first scan that saw the lag")
	}
	// A second scan soon after: still too early.
	ageStaleObservations(time.Minute)
	if _, ok := backoff(); ok {
		t.Fatal("a restart was offered one minute after the lag was first seen")
	}
	// The same generation still unobserved more than moduleStaleAfter later.
	ageStaleObservations(moduleStaleAfter)
	b, ok := backoff()
	if !ok || !b.AutoFixable || !strings.Contains(strings.Join(b.Evidence, "\n"), "generation 4 but its operator last reported observedGeneration 3, unchanged across scans") {
		t.Fatalf("backoff = %+v", b)
	}
	if n := len(w.f.requests("GET", packageManifestsPath)); n != 0 {
		t.Fatalf("listed package manifests without a dependency message")
	}

	// A new spec change (generation 5) starts the wait again.
	ray(5)
	if _, ok := backoff(); ok {
		t.Fatal("a restart was offered right after a spec change")
	}
	// Caught up: the observation is forgotten.
	ray(3)
	backoff()
	staleSeen.Lock()
	n := 0
	for k := range staleSeen.m {
		if strings.HasPrefix(k, w.c.baseURL+"|") {
			n++
		}
	}
	staleSeen.Unlock()
	if n != 0 {
		t.Fatalf("%d observations kept after the operator caught up", n)
	}
}

func TestRestartAssessment(t *testing.T) {
	webhookOn := func(f *fakeAPI) {
		f.json("GET", "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations", 200, `{"items":[{"metadata":{"name":"trainer-webhook","labels":{"platform.opendatahub.io/part-of":"trainer"}},
			"webhooks":[{"name":"vtrainer.kb.io","clientConfig":{"service":{"namespace":"redhat-ods-applications","name":"trainer-webhook-svc"}}}]}]}`)
		f.json("GET", "/api/v1/namespaces/redhat-ods-applications/services/trainer-webhook-svc", 200, `{"spec":{"selector":{"app.kubernetes.io/name":"trainer-operator-controller-manager"}}}`)
	}
	tests := []struct {
		name      string
		strategy  string
		available int
		webhook   bool
		wantOK    bool
		wantText  string
	}{
		{"rolling, no webhook", "RollingUpdate", 1, false, true, "starts a new pod and stops the old one"},
		{"rolling, serves a Fail webhook", "RollingUpdate", 1, true, true, "keeps the old pod serving"},
		{"Recreate, serves a Fail webhook", "Recreate", 1, true, false, "would be rejected meanwhile"},
		{"Recreate, no webhook", "Recreate", 1, false, true, "pauses for the few seconds"},
		{"no available pod", "RollingUpdate", 0, false, false, "has no available pod"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newDSCWorld(t, nil)
			if tt.webhook {
				webhookOn(w.f)
			}
			w.f.obj("GET", trainerOperatorPath, operatorDeploymentJSON("trainer-operator-controller-manager", tt.strategy, tt.available))
			d, why := findModuleOperator(w.c, "trainer", "redhat-ods-applications")
			if d == nil {
				t.Fatal(why)
			}
			a := assessRestart(w.c, *d)
			if a.OK != tt.wantOK || !strings.Contains(a.Note+a.Reason, tt.wantText) {
				t.Fatalf("assessment = %+v", a)
			}
		})
	}
}

func TestMaxUnavailablePods_MatchesDeploymentController(t *testing.T) {
	for _, tt := range []struct {
		replicas      int
		surge, unav   interface{}
		want          int
		wantParseable bool
	}{
		{1, nil, nil, 0, true},                // 25% surge rounds up to 1, 25% unavailable down to 0
		{1, float64(0), "25%", 1, true},       // both 0: maxUnavailable becomes 1
		{1, "0%", "0%", 1, true},              // same with percentages
		{4, "25%", "25%", 1, true},            // 1 and 1
		{2, float64(1), float64(0), 0, true},  // surge first
		{3, "50%", "50%", 1, true},            // surge 2, unavailable 1
		{1, float64(0), "nonsense", 1, false}, // unparseable: assume the worst
	} {
		d := operatorDeployment{Replicas: tt.replicas, MaxSurge: tt.surge, MaxUnavailable: tt.unav}
		got, ok := d.maxUnavailablePods()
		if got != tt.want || ok != tt.wantParseable {
			t.Errorf("replicas=%d surge=%v unavailable=%v: got %d,%v want %d,%v", tt.replicas, tt.surge, tt.unav, got, ok, tt.want, tt.wantParseable)
		}
	}
}

// replicas=1, maxSurge=0, maxUnavailable=25%: Kubernetes takes the only pod
// down first, so a Fail webhook it serves would go unserved.
func TestRestartAssessment_SurgeZeroWithFailWebhookRefuses(t *testing.T) {
	w := newDSCWorld(t, nil)
	dep := operatorDeploymentJSON("trainer-operator-controller-manager", "RollingUpdate", 1)
	dep["spec"].(map[string]interface{})["strategy"] = map[string]interface{}{"type": "RollingUpdate", "rollingUpdate": map[string]interface{}{"maxSurge": 0, "maxUnavailable": "25%"}}
	w.f.obj("GET", trainerOperatorPath, dep)
	w.f.json("GET", "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations", 200, `{"items":[{"metadata":{"name":"trainer-webhook","labels":{"platform.opendatahub.io/part-of":"trainer"}},
		"webhooks":[{"name":"vtrainer.kb.io","failurePolicy":"Fail","clientConfig":{"service":{"namespace":"redhat-ods-applications","name":"svc"}}}]}]}`)
	w.f.json("GET", "/api/v1/namespaces/redhat-ods-applications/services/svc", 200, `{"spec":{"selector":{"app.kubernetes.io/name":"trainer-operator-controller-manager"}}}`)
	d, why := findModuleOperator(w.c, "trainer", "redhat-ods-applications")
	if d == nil {
		t.Fatal(why)
	}
	a := assessRestart(w.c, *d)
	if a.OK || !strings.Contains(a.Reason, "would be rejected meanwhile") || !strings.Contains(a.Reason, "maxSurge 0 and maxUnavailable 25%") {
		t.Fatalf("assessment = %+v", a)
	}
}

func TestFindModuleOperator_GenericPlatformOwnedDeployment(t *testing.T) {
	f, c := newFakeAPI(t)
	items := []interface{}{
		operatorDeploymentJSON("newmod-controller-manager", "RollingUpdate", 1),
		operatorDeploymentJSON("unrelated", "RollingUpdate", 1),
	}
	f.obj("GET", "/apis/apps/v1/namespaces/opendatahub/deployments", map[string]interface{}{"items": items})
	d, why := findModuleOperator(c, "newmod", "opendatahub")
	if d == nil || d.Name != "newmod-controller-manager" || !strings.Contains(d.Via, "named after module newmod") {
		t.Fatalf("d = %+v, why = %s", d, why)
	}
	if d, why := findModuleOperator(c, "absent", "opendatahub"); d != nil || !strings.Contains(why, "no Platform-owned Deployment") {
		t.Fatalf("d = %+v, why = %s", d, why)
	}
}

func TestApplyFixRestartModuleOperator(t *testing.T) {
	setup := func(t *testing.T) *dscWorld {
		w := newDSCWorld(t, liveMissingPrereqs()[:2])
		w.installedCSV("jobset-operator.v1.0.1", "openshift-jobset-operator", "Job Set Operator", "Succeeded", time.Hour, "job-set")
		w.f.json("GET", jobSetOperandPath, 200, `{}`)
		return w
	}
	t.Run("patches the pod template with UID and resourceVersion preconditions", func(t *testing.T) {
		w := setup(t)
		var sent map[string]interface{}
		w.f.handle("PATCH", trainerOperatorPath, func(r *http.Request, body []byte) (int, string) {
			_ = json.Unmarshal(body, &sent)
			if ct := r.Header.Get("Content-Type"); ct != "application/merge-patch+json" {
				t.Errorf("content type %s", ct)
			}
			return 200, `{}`
		})
		res, err := ApplyFix(w.c, "restart-module-operator:trainer")
		if err != nil || !res.Success {
			t.Fatalf("res = %+v, %v", res, err)
		}
		meta := sent["metadata"].(map[string]interface{})
		ann := sent["spec"].(map[string]interface{})["template"].(map[string]interface{})["metadata"].(map[string]interface{})["annotations"].(map[string]interface{})
		if meta["uid"] != "dep-uid" || meta["resourceVersion"] != "7" || ann[restartAnnotation] == nil || len(sent) != 2 {
			t.Fatalf("patch = %v", sent)
		}
		assertWrites(t, w.f, "PATCH "+trainerOperatorPath)
	})
	t.Run("a changed Deployment is not restarted", func(t *testing.T) {
		w := setup(t)
		w.f.status("PATCH", trainerOperatorPath, http.StatusConflict, "Conflict")
		res, _ := ApplyFix(w.c, "restart-module-operator:trainer")
		if res.Success || res.ErrorCode != "conflict" {
			t.Fatalf("res = %+v", res)
		}
	})
	t.Run("re-checks the condition first", func(t *testing.T) {
		w := newDSCWorld(t, []dcond{{"type": "Ready", "status": "True"}, {"type": "TrainerReady", "status": "True"}})
		res, _ := ApplyFix(w.c, "restart-module-operator:trainer")
		if res.Success || res.ErrorCode != "nothing_to_do" {
			t.Fatalf("res = %+v", res)
		}
		assertWrites(t, w.f)
	})
	t.Run("refuses when the restart would drop a Fail webhook", func(t *testing.T) {
		w := setup(t)
		w.f.obj("GET", trainerOperatorPath, operatorDeploymentJSON("trainer-operator-controller-manager", "Recreate", 1))
		w.f.json("GET", "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations", 200, `{"items":[{"metadata":{"name":"trainer-webhook","labels":{"platform.opendatahub.io/part-of":"trainer"}},
			"webhooks":[{"name":"vtrainer.kb.io","failurePolicy":"Fail","clientConfig":{"service":{"namespace":"redhat-ods-applications","name":"svc"}}}]}]}`)
		w.f.json("GET", "/api/v1/namespaces/redhat-ods-applications/services/svc", 200, `{"spec":{"selector":{"app.kubernetes.io/name":"trainer-operator-controller-manager"}}}`)
		res, _ := ApplyFix(w.c, "restart-module-operator:trainer")
		if res.Success || res.ErrorCode != "prerequisites" {
			t.Fatalf("res = %+v", res)
		}
		assertWrites(t, w.f)
	})
	t.Run("rejects a malformed module", func(t *testing.T) {
		_, c := newFakeAPI(t)
		res, _ := ApplyFix(c, "restart-module-operator:../x")
		if res.Success || res.ErrorCode != "validation" {
			t.Fatalf("res = %+v", res)
		}
	})
}

func TestDSCCheck_UpgradeGates(t *testing.T) {
	w := newDSCWorld(t, []dcond{
		{"type": "Ready", "status": "False", "reason": "Error", "message": msgGateResolve},
		{"type": "ModulesReady", "status": "False", "reason": "AdminAckRequired", "message": msgGateAck},
	})
	w.f.json("GET", "/api/v1/namespaces/redhat-ods-operator/configmaps/odh-upgrade-acks", 200, `{"data":{"kserve-v2":"false","old":"true"}}`)
	out := checkDataScienceCluster(w.c)
	byID := problemsByID(out)
	g, ok := byID[dscGateProblemID]
	if !ok || g.Severity != "critical" || g.AutoFixable {
		t.Fatalf("problems = %v", ids(out))
	}
	if g.TechnicalCmd != `oc patch configmap odh-upgrade-acks -n redhat-ods-operator --type merge -p '{"data":{"kserve-v2":"true"}}'` {
		t.Fatalf("cmd = %s", g.TechnicalCmd)
	}
	if !strings.Contains(g.Description, "acknowledging does not help") || !strings.Contains(strings.Join(g.Evidence, "\n"), `Not acknowledged: kserve-v2="false"`) {
		t.Fatalf("gate = %+v", g)
	}
	dsc := byID["dsc-not-ready"]
	if !containsString(dsc.RelatedProblems, dscGateProblemID) || !strings.Contains(strings.Join(dsc.Evidence, "\n"), "ModulesReady=False (AdminAckRequired): Waiting for upgrade gates to be acknowledged → cause: an upgrade gate holds provisioning") {
		t.Fatalf("dsc = %+v", dsc)
	}
}

func TestDSCCheck_UnclassifiedKeepsGenericFix(t *testing.T) {
	w := newDSCWorld(t, []dcond{
		{"type": "Ready", "status": "False", "reason": "Error", "message": "Some components are not ready"},
		{"type": "WorkbenchesReady", "status": "False", "reason": "Error", "message": "something unexpected"},
	})
	out := checkDataScienceCluster(w.c)
	dsc := problemsByID(out)["dsc-not-ready"]
	if !strings.Contains(strings.Join(dsc.Evidence, "\n"), "WorkbenchesReady=False (Error): something unexpected → cause not classified") ||
		!strings.Contains(dsc.Fix, "For WorkbenchesReady, which the tool does not classify") || len(dsc.RelatedProblems) != 0 {
		t.Fatalf("dsc = %+v", dsc)
	}
}

func TestDSCCheck_CatalogUnreadableStillReports(t *testing.T) {
	w := newDSCWorld(t, liveMissingPrereqs()[:2])
	w.f.status("GET", packageManifestsPath, http.StatusForbidden, "Forbidden")
	out := checkDataScienceCluster(w.c)
	p, ok := problemsByID(out)["prerequisite-missing-jobset"]
	if !ok || !strings.Contains(strings.Join(p.Evidence, "\n"), "Could not read the catalogs") || !strings.Contains(p.Fix, "OperatorHub") {
		t.Fatalf("problems = %v, p = %+v", ids(out), p)
	}
}

func TestComponentCause(t *testing.T) {
	for _, tt := range []struct {
		cond dscCondition
		want string
	}{
		{dscCondition{Message: msgTrainerJobSet}, "Prerequisite operator not installed: JobSet Operator"},
		{dscCondition{Message: msgWideEP}, "Prerequisite operators not installed: LeaderWorkerSet, cert-manager operator"},
		{dscCondition{Reason: "AdminAckRequired", Message: msgGateAck}, "An upgrade gate holds provisioning"},
		{dscCondition{Message: msgRayStale}, "Its module operator has not reconciled the current spec yet"},
		{dscCondition{Message: "boom"}, ""},
	} {
		if got := componentCause(tt.cond); got != tt.want {
			t.Errorf("componentCause(%q) = %q, want %q", tt.cond.Message, got, tt.want)
		}
	}
}
