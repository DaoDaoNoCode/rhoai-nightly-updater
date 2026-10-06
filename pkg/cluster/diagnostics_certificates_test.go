package cluster

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

const appCertsPath = "/apis/cert-manager.io/v1/namespaces/redhat-ods-applications/certificates"

// certWorld is the live case: cert-manager's CRDs left from a manual
// install, no controller, the odh-observability Certificate never issued,
// its pods waiting for the Secret, and its webhook's CA injected from it.
func certWorld(t *testing.T) (*fakeAPI, *Client) {
	t.Helper()
	f, c := newFakeAPI(t)
	for _, ns := range append([]string{SubNS}, rhoaiPodNamespaces...) {
		f.json("GET", "/apis/cert-manager.io/v1/namespaces/"+ns+"/certificates", 200, `{"items":[]}`)
	}
	f.obj("GET", appCertsPath, map[string]interface{}{"items": []interface{}{
		map[string]interface{}{
			"metadata": map[string]string{"name": "odh-observability-webhook-cert", "namespace": "redhat-ods-applications", "creationTimestamp": ago(time.Hour)},
			"spec":     map[string]string{"secretName": "odh-observability-webhook-cert"},
		},
		map[string]interface{}{
			"metadata": map[string]string{"name": "serving-cert", "namespace": "redhat-ods-applications", "creationTimestamp": ago(time.Hour)},
			"spec":     map[string]string{"secretName": "kuberay-webhook-server-cert"},
			"status":   map[string]interface{}{"conditions": []dcond{{"type": "Ready", "status": "True"}}},
		},
	}})
	f.json("GET", "/apis/apps/v1/deployments", 200, `{"items":[]}`)
	f.json("GET", "/api/v1/namespaces/redhat-ods-applications/pods", 200, `{"items":[
		{"metadata":{"name":"odh-observability-abc"},"spec":{"volumes":[{"secret":{"secretName":"odh-observability-webhook-cert"}}]},"status":{"phase":"Pending"}},
		{"metadata":{"name":"other"},"spec":{"volumes":[{"secret":{"secretName":"odh-observability-webhook-cert"}}]},"status":{"phase":"Running"}}]}`)
	// Only the stuck odh-observability pod has the FailedMount event.
	f.handle("GET", "/api/v1/namespaces/redhat-ods-applications/events", func(r *http.Request, _ []byte) (int, string) {
		if strings.Contains(r.URL.Query().Get("fieldSelector"), "involvedObject.name=odh-observability-abc") {
			return 200, `{"items":[{"type":"Warning","reason":"FailedMount","message":"MountVolume.SetUp failed for volume \"cert\" : secret \"odh-observability-webhook-cert\" not found","lastTimestamp":"` + ago(time.Minute) + `"}]}`
		}
		return 200, `{"items":[{"type":"Warning","reason":"FailedScheduling","message":"0/3 nodes are available: 3 Insufficient cpu.","lastTimestamp":"` + ago(time.Minute) + `"}]}`
	})
	f.json("GET", "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations", 200, `{"items":[{"metadata":{"name":"odh-observability-webhook","creationTimestamp":"2026-01-01T00:00:00Z",
		"labels":{"platform.opendatahub.io/part-of":"platform"},"annotations":{"cert-manager.io/inject-ca-from":"redhat-ods-applications/odh-observability-webhook-cert"}},
		"webhooks":[{"name":"v.observability","clientConfig":{"service":{"namespace":"redhat-ods-applications","name":"odh-observability-webhook"}}}]}]}`)
	f.json("GET", "/apis/admissionregistration.k8s.io/v1/mutatingwebhookconfigurations", 200, `{"items":[]}`)
	f.obj("GET", packageManifestsPath, map[string]interface{}{"items": liveCatalog()})
	f.json("GET", clusterCSVsPath, 200, `{"items":[]}`)
	return f, c
}

func TestCertificatesCheck_CertManagerMissing(t *testing.T) {
	f, c := certWorld(t)
	out := checkCertificates(c)
	byID := problemsByID(out)
	if out.check.Status != "fail" || len(out.problems) != 2 {
		t.Fatalf("out = %+v %v", out.check, ids(out))
	}
	p := byID["certificate-not-ready-redhat-ods-applications-odh-observability-webhook-cert"]
	if !strings.Contains(p.Title, "cert-manager is not installed or not running") ||
		strings.Join(p.covers, ",") != "Pod redhat-ods-applications/odh-observability-abc,ValidatingWebhookConfiguration odh-observability-webhook" ||
		strings.Join(p.RelatedProblems, ",") != "prerequisite-missing-openshift-cert-manager-operator" {
		t.Fatalf("certificate problem = %+v", p)
	}
	pre := byID["prerequisite-missing-openshift-cert-manager-operator"]
	if pre.Severity != "warning" || !pre.mergeable || !strings.Contains(pre.Description, "RHOAI Certificates are not issued without it") ||
		!strings.Contains(strings.Join(pre.Evidence, "\n"), "Certificate redhat-ods-applications/odh-observability-webhook-cert") {
		t.Fatalf("prerequisite = %+v", pre)
	}
	assertWrites(t, f)
}

// A pod that mounts the Secret but fails for another reason is not folded
// into the Certificate; one whose container names the missing Secret is.
func TestCertificatesCheck_OnlyPodsBlockedOnThisSecret(t *testing.T) {
	f, c := certWorld(t)
	f.json("GET", "/api/v1/namespaces/redhat-ods-applications/pods", 200, `{"items":[
		{"metadata":{"name":"odh-observability-abc"},"spec":{"volumes":[{"secret":{"secretName":"odh-observability-webhook-cert"}}]},"status":{"phase":"Pending"}},
		{"metadata":{"name":"unschedulable"},"spec":{"volumes":[{"secret":{"secretName":"odh-observability-webhook-cert"}}]},"status":{"phase":"Pending"}},
		{"metadata":{"name":"env-ref"},"spec":{"volumes":[{"secret":{"secretName":"odh-observability-webhook-cert"}}]},"status":{"phase":"Pending",
			"containerStatuses":[{"name":"c","state":{"waiting":{"reason":"CreateContainerConfigError","message":"secret \"odh-observability-webhook-cert\" not found"}}}]}},
		{"metadata":{"name":"other-secret"},"spec":{"volumes":[{"secret":{"secretName":"something-else"}}]},"status":{"phase":"Pending"}}]}`)
	out := checkCertificates(c)
	p := problemsByID(out)["certificate-not-ready-redhat-ods-applications-odh-observability-webhook-cert"]
	if got := strings.Join(p.covers, ","); got != "Pod redhat-ods-applications/env-ref,Pod redhat-ods-applications/odh-observability-abc,ValidatingWebhookConfiguration odh-observability-webhook" {
		t.Fatalf("covers = %s", got)
	}
	if !strings.Contains(strings.Join(p.Evidence, "\n"), "show no error about it (their own problems are kept): Pod redhat-ods-applications/unschedulable") {
		t.Fatalf("evidence = %q", p.Evidence)
	}
	// One pod list per namespace, also with two Certificates in it.
	f.obj("GET", appCertsPath, map[string]interface{}{"items": []interface{}{
		map[string]interface{}{"metadata": map[string]string{"name": "a", "namespace": "redhat-ods-applications", "creationTimestamp": ago(time.Hour)}, "spec": map[string]string{"secretName": "s1"}},
		map[string]interface{}{"metadata": map[string]string{"name": "b", "namespace": "redhat-ods-applications", "creationTimestamp": ago(time.Hour)}, "spec": map[string]string{"secretName": "s2"}},
	}})
	before := len(f.requests("GET", "/api/v1/namespaces/redhat-ods-applications/pods"))
	checkCertificates(c)
	if n := len(f.requests("GET", "/api/v1/namespaces/redhat-ods-applications/pods")) - before; n != 1 {
		t.Fatalf("pods listed %d times for one namespace", n)
	}
}

func TestCertificatesCheck_OwnFailingCondition(t *testing.T) {
	f, c := certWorld(t)
	f.json("GET", "/apis/apps/v1/deployments", 200, `{"items":[{"metadata":{"name":"cert-manager","namespace":"cert-manager"},"status":{"availableReplicas":1}}]}`)
	f.obj("GET", appCertsPath, map[string]interface{}{"items": []interface{}{map[string]interface{}{
		"metadata": map[string]string{"name": "c1", "namespace": "redhat-ods-applications", "creationTimestamp": ago(time.Hour)},
		"spec":     map[string]string{"secretName": "s1"},
		"status":   map[string]interface{}{"conditions": []dcond{{"type": "Ready", "status": "False", "reason": "IssuerNotFound", "message": "issuer odh-ca not found"}}},
	}}})
	out := checkCertificates(c)
	if len(out.problems) != 1 || !strings.Contains(out.problems[0].Title, "issuer odh-ca not found") || len(out.problems[0].RelatedProblems) != 0 {
		t.Fatalf("problems = %+v", out.problems)
	}
	if n := len(f.requests("GET", packageManifestsPath)); n != 0 {
		t.Fatal("the catalog was read although cert-manager runs")
	}
}

func TestCertificatesCheck_NoAPIAndYoungCertificates(t *testing.T) {
	_, c := newFakeAPI(t)
	if out := checkCertificates(c); out.check.Status != "pass" || !strings.Contains(out.check.Detail, "cert-manager API is not installed") {
		t.Fatalf("out = %+v", out.check)
	}
	f, c := certWorld(t)
	f.obj("GET", appCertsPath, map[string]interface{}{"items": []interface{}{map[string]interface{}{
		"metadata": map[string]string{"name": "new", "namespace": "redhat-ods-applications", "creationTimestamp": ago(10 * time.Second)},
	}}})
	if out := checkCertificates(c); out.check.Status != "pass" || len(out.problems) != 0 {
		t.Fatalf("out = %+v %v", out.check, ids(out))
	}
	f.status("GET", appCertsPath, http.StatusForbidden, "Forbidden")
	if out := checkCertificates(c); out.check.Status != "warn" || !strings.Contains(out.check.Detail, "make upgrade") {
		t.Fatalf("out = %+v", out.check)
	}
}

func TestLinkProblems_FoldsSymptomsIntoRootCause(t *testing.T) {
	root := Problem{ID: "certificate-not-ready-x", Title: "Certificate x is not issued", RelatedProblems: []string{"prerequisite-missing-a", "gone"},
		covers: []string{"Pod ns/p1", "Pod ns/p2", "ValidatingWebhookConfiguration hook-a"}}
	problems := []Problem{
		{ID: "dsc-not-ready", RelatedProblems: []string{"prerequisite-missing-a", "dsc-not-ready"}},
		root,
		{ID: "prerequisite-missing-a"},
		{ID: "pod-stuck-creating-ns-p", Title: "p: 2 pods stuck", AffectedObjects: []string{"Pod ns/p1", "Pod ns/p2"}},
		{ID: "pod-crashloop-ns-q", Title: "q crash-looping", AffectedObjects: []string{"Pod ns/p1", "Pod ns/q1"}},
		{ID: "pod-crashloop-ns-p", Title: "p1 crash-looping", AffectedObjects: []string{"Pod ns/p1"}},
		{ID: "webhook-service-missing", Title: "2 webhooks", AffectedObjects: []string{"ValidatingWebhookConfiguration hook-a", "MutatingWebhookConfiguration hook-b"}},
	}
	out := linkProblems(problems)
	var got []string
	for _, p := range out {
		got = append(got, p.ID)
	}
	// A crash loop is not a missing-Secret symptom: kept even when its pod is covered.
	if strings.Join(got, ",") != "dsc-not-ready,certificate-not-ready-x,prerequisite-missing-a,pod-crashloop-ns-q,pod-crashloop-ns-p,webhook-service-missing" {
		t.Fatalf("ids = %v", got)
	}
	if !containsString(out[1].Evidence, "Also explains: p: 2 pods stuck") || strings.Join(out[1].RelatedProblems, ",") != "prerequisite-missing-a" {
		t.Fatalf("root = %+v", out[1])
	}
	if strings.Join(out[0].RelatedProblems, ",") != "prerequisite-missing-a" {
		t.Fatalf("related = %v", out[0].RelatedProblems)
	}
	wh := out[5]
	if !strings.Contains(strings.Join(wh.Evidence, "\n"), `Explained by "Certificate x is not issued": ValidatingWebhookConfiguration hook-a`) || !containsString(wh.RelatedProblems, root.ID) {
		t.Fatalf("webhook problem = %+v", wh)
	}

	// Fully covered webhook guidance is folded too.
	out = linkProblems([]Problem{root, {ID: "webhook-service-missing", Title: "1 webhook", AffectedObjects: []string{"ValidatingWebhookConfiguration hook-a"}}})
	if len(out) != 1 || !containsString(out[0].Evidence, "Also explains: 1 webhook") {
		t.Fatalf("out = %+v", out)
	}
}

// The whole report for the live case: the DSC names cert-manager, the
// Certificate needs it, its pods wait for the Secret and its webhook has
// no endpoints. One prerequisite problem (merged), one Certificate
// problem, and no separate pod or webhook problem.
func TestDiagnoseCluster_CertificateRootCause(t *testing.T) {
	f, c := certWorld(t)
	f.obj("GET", dscV2List, map[string]interface{}{"items": []interface{}{map[string]interface{}{
		"metadata": map[string]string{"name": "default-dsc"},
		"status": map[string]interface{}{"conditions": []dcond{
			{"type": "Ready", "status": "False", "reason": "Error", "message": "Some modules are not ready: kserve"},
			{"type": "KserveLLMInferenceServiceDependencies", "status": "False", "reason": "PreConditionFailed", "message": msgCertManager},
		}},
	}}})
	f.json("GET", "/api/v1/namespaces/redhat-ods-applications/pods", 200, `{"items":[{"metadata":{"name":"odh-observability-abc","namespace":"redhat-ods-applications",
		"creationTimestamp":"`+ago(time.Hour)+`","labels":{"pod-template-hash":"abc","app":"odh-observability"},
		"ownerReferences":[{"kind":"ReplicaSet","name":"odh-observability-abc","controller":true}]},
		"spec":{"volumes":[{"secret":{"secretName":"odh-observability-webhook-cert"}}]},
		"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"True"}],"containerStatuses":[{"name":"c","state":{"waiting":{"reason":"ContainerCreating"}}}]}}]}`)
	f.json("GET", "/api/v1/namespaces/redhat-ods-applications/events", 200, `{"items":[{"type":"Warning","reason":"FailedMount",
		"message":"MountVolume.SetUp failed for volume \"cert\" : secret \"odh-observability-webhook-cert\" not found","lastTimestamp":"`+ago(time.Minute)+`"}]}`)
	f.json("GET", "/api/v1/namespaces/redhat-ods-applications/services/odh-observability-webhook", 200, `{"metadata":{"creationTimestamp":"`+ago(time.Hour)+`"},"spec":{"selector":{"app":"odh-observability"}}}`)
	f.json("GET", "/apis/discovery.k8s.io/v1/namespaces/redhat-ods-applications/endpointslices", 200, `{"items":[]}`)

	resp, err := RunDiagnostics(c)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range resp.Problems {
		got = append(got, p.ID)
		if strings.HasPrefix(p.ID, "pod-") && strings.Contains(p.ID, "odh-observability") {
			t.Errorf("pod symptom reported separately: %+v", p)
		}
		if p.ID == "webhook-service-missing" || p.ID == "stale-webhooks" {
			t.Errorf("webhook symptom reported separately: %+v", p)
		}
	}
	cert := findProblem(resp, "certificate-not-ready-redhat-ods-applications-odh-observability-webhook-cert")
	pre := findProblem(resp, "prerequisite-missing-openshift-cert-manager-operator")
	if cert == nil || pre == nil {
		t.Fatalf("problems = %v", got)
	}
	if ev := strings.Join(cert.Evidence, "\n"); !strings.Contains(ev, "Also explains: odh-observability: 1 pod stuck in ContainerCreating") || !strings.Contains(ev, "Also explains: 1 RHOAI webhook configuration calls a Service") {
		t.Fatalf("certificate evidence = %q", cert.Evidence)
	}
	ev := strings.Join(pre.Evidence, "\n")
	if !strings.Contains(ev, "KserveLLMInferenceServiceDependencies") || !strings.Contains(ev, "Not issued without a running cert-manager") {
		t.Fatalf("merged prerequisite evidence = %s", ev)
	}
	dsc := findProblem(resp, "dsc-not-ready")
	if dsc == nil || !containsString(dsc.RelatedProblems, pre.ID) {
		t.Fatalf("dsc-not-ready = %+v", dsc)
	}
}

func TestMergeProblem(t *testing.T) {
	dst := Problem{ID: "p", Severity: "info", Evidence: []string{"a"}, mergeable: true}
	mergeProblem(&dst, Problem{ID: "p", Severity: "warning", Evidence: []string{"a", "b"}, AffectedObjects: []string{"X y"}, RelatedProblems: []string{"r"}})
	if dst.Severity != "warning" || strings.Join(dst.Evidence, ",") != "a,b" || len(dst.AffectedObjects) != 1 || len(dst.RelatedProblems) != 1 {
		t.Fatalf("dst = %+v", dst)
	}
}
