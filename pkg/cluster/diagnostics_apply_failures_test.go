package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Verbatim condition messages from RHOAI 3.6 (E2).
const (
	msgImmutableSelector = `failure deploying resource redhat-ods-applications/kuberay-operator: apply failed apps/v1, Kind=Deployment: unable to patch apps/v1, Kind=Deployment redhat-ods-applications/kuberay-operator: Deployment.apps "kuberay-operator" is invalid: spec.selector: Invalid value: {"matchLabels":{"app.kubernetes.io/component":"kuberay-operator","app.kubernetes.io/name":"kuberay"}}: field is immutable`
	msgTwoControllers    = `failure deploying resource redhat-ods-applications/aihub-controller-manager-metrics-monitor: apply failed monitoring.coreos.com/v1, Kind=ServiceMonitor: unable to patch monitoring.coreos.com/v1, Kind=ServiceMonitor redhat-ods-applications/aihub-controller-manager-metrics-monitor: ServiceMonitor.monitoring.coreos.com "aihub-controller-manager-metrics-monitor" is invalid: metadata.ownerReferences: Invalid value: [{"apiVersion":"config.opendatahub.io/v1alpha1","kind":"Platform","name":"default","uid":"1","controller":true},{"apiVersion":"components.platform.opendatahub.io/v1alpha1","kind":"AIHub","name":"default-aihub","uid":"2","controller":true}]: Only one reference can have Controller set to true. Found "true" in references for Platform/default and AIHub/default-aihub`
	msgSchemaMismatch    = `failure deploying resource /default-trustyai: apply failed components.platform.opendatahub.io/v1alpha1, Kind=TrustyAI: unable to patch components.platform.opendatahub.io/v1alpha1, Kind=TrustyAI /default-trustyai: failed to create typed live object (/default-trustyai; components.platform.opendatahub.io/v1alpha1, Kind=TrustyAI): errors: .spec.eval.lmeval.permitCodeExecution: expected boolean, got &{deny}`
	msgTypedPatch        = `failure deploying resource /default-trustyai: apply failed components.platform.opendatahub.io/v1alpha1, Kind=TrustyAI: failed to create typed patch object (/default-trustyai; components.platform.opendatahub.io/v1alpha1, Kind=TrustyAI): .spec.eval.lmeval.permitOnline: expected string, got &value.valueUnstructured{Value:true}`
)

func TestParseApplyFailures(t *testing.T) {
	got := parseApplyFailures(msgImmutableSelector)
	if len(got) != 1 || got[0].Class != applyImmutable || got[0].Namespace != "redhat-ods-applications" || got[0].Name != "kuberay-operator" ||
		got[0].GroupVersion != "apps/v1" || got[0].Kind != "Deployment" || strings.Join(got[0].Fields, ",") != "spec.selector" {
		t.Fatalf("immutable: %+v", got)
	}
	got = parseApplyFailures(msgTwoControllers)
	if len(got) != 1 || got[0].Class != applyControllerOwner || got[0].Kind != "ServiceMonitor" || got[0].group() != "monitoring.coreos.com" ||
		got[0].Owners != "Platform/default and AIHub/default-aihub" {
		t.Fatalf("controller owners: %+v", got)
	}
	got = parseApplyFailures(msgSchemaMismatch)
	if len(got) != 1 || got[0].Class != applySchemaMismatch || got[0].Namespace != "" || got[0].Name != "default-trustyai" || got[0].Kind != "TrustyAI" ||
		strings.Join(got[0].Fields, ",") != ".spec.eval.lmeval.permitCodeExecution: expected boolean, got &{deny}" {
		t.Fatalf("schema: %+v", got)
	}
	got = parseApplyFailures(msgTypedPatch)
	if len(got) != 1 || got[0].Class != applySchemaMismatch || !strings.HasPrefix(got[0].Fields[0], ".spec.eval.lmeval.permitOnline: expected string, got ") {
		t.Fatalf("typed patch: %+v", got)
	}
	// Two failures in one message are both found, each with its own error.
	got = parseApplyFailures("Some modules failed: " + msgImmutableSelector + "; " + msgTwoControllers)
	if len(got) != 2 || got[0].Class != applyImmutable || got[1].Class != applyControllerOwner || strings.Contains(got[0].Detail, "ServiceMonitor") {
		t.Fatalf("two: %+v", got)
	}

	for _, msg := range []string{
		"",
		"Some modules are not ready: trainer",
		"dependency not met: JobSet Operator is not installed.",
		// An apply failure of another kind is not one of these three.
		`failure deploying resource ns/x: apply failed apps/v1, Kind=Deployment: Internal error occurred: failed calling webhook`,
		`Deployment "x" is invalid: spec.selector: Invalid value: {}: field is immutable`, // not an apply failure report
	} {
		if got := parseApplyFailures(msg); len(got) != 0 {
			t.Errorf("%q: %+v", msg, got)
		}
	}
}

// Review fix 3: a condition message is not trusted. An object whose
// namespace, name, kind or group/version breaks the Kubernetes rules is
// ignored, so nothing from it reaches a copy-paste command.
func TestParseApplyFailuresRejectsMalformedObjects(t *testing.T) {
	tmpl := `failure deploying resource %s/%s: apply failed %s, Kind=%s: Deployment.apps "x" is invalid: spec.selector: Invalid value: {}: field is immutable`
	for _, tc := range [][4]string{
		{"redhat-ods-applications", "x;id;#", "apps/v1", "Deployment"},
		{"redhat-ods-applications", "$(id)", "apps/v1", "Deployment"},
		{"redhat-ods-applications", "`id`", "apps/v1", "Deployment"},
		{"redhat-ods-applications", "a'b", "apps/v1", "Deployment"},
		{"ns;rm", "x", "apps/v1", "Deployment"},
		{"$(id)", "x", "apps/v1", "Deployment"},
		{"UPPER", "x", "apps/v1", "Deployment"},
		{"redhat-ods-applications", "Upper", "apps/v1", "Deployment"},
		{"redhat-ods-applications", "x", "apps/v1;id", "Deployment"},
		{"redhat-ods-applications", "x", "Apps/V1", "Deployment"},
		{"redhat-ods-applications", "x", "apps/v1", "Deploy_ment"},
		{"redhat-ods-applications", strings.Repeat("a", 254), "apps/v1", "Deployment"},
	} {
		msg := fmt.Sprintf(tmpl, tc[0], tc[1], tc[2], tc[3])
		if got := parseApplyFailures(msg); len(got) != 0 {
			t.Errorf("%v accepted: %+v", tc, got)
		}
	}
	// Spaces and newlines do not even form a match.
	for _, msg := range []string{
		`failure deploying resource ns/a b: apply failed apps/v1, Kind=Deployment: field is immutable`,
		"failure deploying resource ns/a\nb: apply failed apps/v1, Kind=Deployment: field is immutable",
	} {
		for _, f := range parseApplyFailures(msg) {
			if strings.ContainsAny(f.Name, " \n") {
				t.Errorf("%q: %+v", msg, f)
			}
		}
	}
	// A valid cluster-scoped and a valid core object still parse.
	if got := parseApplyFailures(fmt.Sprintf(tmpl, "", "a.b-c", "v1", "Service")); len(got) != 1 {
		t.Errorf("core object rejected: %+v", got)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"kuberay-operator":            "kuberay-operator",
		"deployment.apps":             "deployment.apps",
		"name=rhods-operator":         "name=rhods-operator",
		"x;id;#":                      `'x;id;#'`,
		"$(id)":                       `'$(id)'`,
		"`id`":                        "'`id`'",
		"a b":                         "'a b'",
		"a\nb":                        "'a\nb'",
		"it's":                        `'it'\''s'`,
		"":                            "''",
		"jsonpath={.metadata.labels}": "'jsonpath={.metadata.labels}'",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
	if got := shellCommand("oc", "delete", "x;id"); got != "oc delete 'x;id'" {
		t.Errorf("shellCommand = %s", got)
	}
}

func applyFailureCR(name string, ready string, msgs ...string) map[string]interface{} {
	conds := []map[string]string{{"type": "Ready", "status": ready, "reason": "Error", "message": "x"}}
	for _, m := range msgs {
		conds = append(conds, map[string]string{"type": "ProvisioningSucceeded", "status": "False", "reason": "Error", "message": m})
	}
	return map[string]interface{}{"metadata": map[string]string{"name": name}, "status": map[string]interface{}{"conditions": conds}}
}

func TestCheckApplyFailures(t *testing.T) {
	f, c := newFakeAPI(t)
	serveComponentGroup(f)
	f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1", 200,
		`{"resources":[{"name":"rays","kind":"Ray"},{"name":"rays/status","kind":"Ray"},{"name":"trustyais","kind":"TrustyAI"},{"name":"aihubs","kind":"AIHub"},{"name":"newmodules","kind":"NewModule"}]}`)
	// The DSC aggregates the module messages: each object is reported once.
	f.obj("GET", "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters", map[string]interface{}{
		"items": []interface{}{applyFailureCR("default-dsc", "False", msgImmutableSelector, msgSchemaMismatch)},
	})
	f.obj("GET", "/apis/config.opendatahub.io/v1alpha1/platforms", map[string]interface{}{
		"items": []interface{}{applyFailureCR("default", "True", msgTwoControllers)},
	})
	f.obj("GET", "/apis/components.platform.opendatahub.io/v1alpha1/rays", map[string]interface{}{"items": []interface{}{applyFailureCR("default-ray", "False", msgImmutableSelector)}})
	f.obj("GET", "/apis/components.platform.opendatahub.io/v1alpha1/trustyais", map[string]interface{}{"items": []interface{}{applyFailureCR("default-trustyai", "True", msgSchemaMismatch)}})
	f.obj("GET", "/apis/components.platform.opendatahub.io/v1alpha1/aihubs", map[string]interface{}{"items": []interface{}{applyFailureCR("default-aihub", "True")}})
	f.obj("GET", "/apis/components.platform.opendatahub.io/v1alpha1/newmodules", map[string]interface{}{"items": []interface{}{
		applyFailureCR("default-newmodule", "True", strings.ReplaceAll(msgImmutableSelector, "kuberay-operator", "new-operator"))}})
	var crdLookups []string
	f.handle("GET", "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", func(r *http.Request, _ []byte) (int, string) {
		crdLookups = append(crdLookups, r.URL.Query().Get("fieldSelector"))
		b, _ := json.Marshal(map[string]interface{}{"items": []interface{}{map[string]interface{}{"metadata": map[string]interface{}{
			"name": "trustyais.components.platform.opendatahub.io",
			"managedFields": []interface{}{
				map[string]interface{}{"manager": "kubectl-edit", "operation": "Update", "time": "2026-09-01T10:00:00Z", "fieldsV1": map[string]interface{}{"f:metadata": map[string]interface{}{}}},
				map[string]interface{}{"manager": "catalog", "operation": "Update", "time": "2026-10-05T12:00:00Z", "fieldsV1": map[string]interface{}{"f:spec": map[string]interface{}{"f:versions": map[string]interface{}{}}}},
			},
		}}}})
		return 200, string(b)
	})

	out := checkApplyFailures(c.WithContext(context.Background()))
	if out.check.Status != "fail" || len(out.problems) != 4 {
		t.Fatalf("check %+v, %d problems: %+v", out.check, len(out.problems), out.problems)
	}
	byID := map[string]Problem{}
	for _, p := range out.problems {
		byID[p.ID] = p
		if p.AutoFixable || p.Fix == "" || p.TechnicalCmd == "" || len(p.AffectedObjects) == 0 || len(p.Evidence) == 0 {
			t.Errorf("incomplete problem %+v", p)
		}
	}

	imm, ok := byID["operator-apply-failed-deployment-redhat-ods-applications-kuberay-operator"]
	if !ok {
		t.Fatalf("ids %v", keysOf(byID))
	}
	// Reported by the Ray CR and the DSC (both not Ready): critical, the
	// module CR first, and its module operator from the mapping.
	if imm.Severity != "critical" || !strings.Contains(imm.Evidence[0], "Reported by Ray default-ray, DataScienceCluster default-dsc") ||
		!strings.Contains(imm.TechnicalCmd, "oc delete deployment.apps kuberay-operator -n redhat-ods-applications") ||
		!strings.Contains(imm.TechnicalCmd, "oc rollout restart deployment/"+moduleOperators["ray"].name+" -n "+moduleOperators["ray"].namespace) ||
		!strings.Contains(imm.Title, "spec.selector") || !strings.Contains(imm.Fix, "pods stop until the operator recreates it") {
		t.Errorf("immutable: %+v", imm)
	}

	owner := byID["operator-apply-failed-servicemonitor-redhat-ods-applications-aihub-controller-manager-metrics-monitor"]
	if owner.Severity != "warning" || !strings.Contains(owner.TechnicalCmd, "oc delete servicemonitor.monitoring.coreos.com aihub-controller-manager-metrics-monitor -n redhat-ods-applications") ||
		!strings.Contains(owner.TechnicalCmd, "oc delete pod -n "+SubNS+" -l name="+SubName) || !strings.Contains(strings.Join(owner.Evidence, "\n"), "Platform/default and AIHub/default-aihub") {
		t.Errorf("controller owner: %+v", owner)
	}

	schema := byID["operator-apply-failed-trustyai--default-trustyai"]
	ev := strings.Join(schema.Evidence, "\n")
	if schema.Severity != "critical" || !strings.Contains(ev, "permitCodeExecution: expected boolean, got &{deny}") ||
		!strings.Contains(ev, "spec.versions last written by catalog (Update) at 2026-10-05T12:00:00Z") || !strings.Contains(ev, "OLM's catalog operator") ||
		!strings.Contains(schema.Fix, "trustyais.components.platform.opendatahub.io") || strings.Contains(schema.TechnicalCmd, "oc delete") ||
		!strings.Contains(strings.Join(schema.AffectedObjects, ","), "CustomResourceDefinition trustyais.components.platform.opendatahub.io") {
		t.Errorf("schema: %+v", schema)
	}
	if len(crdLookups) != 1 || crdLookups[0] != "metadata.name=trustyais.components.platform.opendatahub.io" {
		t.Errorf("CRD lookups %v", crdLookups)
	}

	// A module the tool does not know: generic restart guidance.
	unknown := byID["operator-apply-failed-deployment-redhat-ods-applications-new-operator"]
	if !strings.Contains(unknown.Fix, "the operator that reconciles NewModule default-newmodule") || strings.Contains(unknown.TechnicalCmd, "rollout restart") {
		t.Errorf("unknown module: %+v", unknown)
	}
}

// Review fix 7: one problem per object. Two failure classes reported for
// the same object (by different CRs) merge into one problem with both
// classes, all fields, every reporter and a per-object ID.
func TestApplyFailuresOneProblemPerObject(t *testing.T) {
	f, c := newFakeAPI(t)
	immutable := strings.ReplaceAll(strings.ReplaceAll(msgImmutableSelector, "kuberay-operator", "shared"), "spec.selector", "spec.template.metadata.labels")
	immutable2 := strings.ReplaceAll(msgImmutableSelector, "kuberay-operator", "shared")
	owner := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(msgTwoControllers, "aihub-controller-manager-metrics-monitor", "shared"),
		"monitoring.coreos.com/v1, Kind=ServiceMonitor", "apps/v1, Kind=Deployment"), "ServiceMonitor.monitoring.coreos.com", "Deployment.apps")
	f.obj("GET", "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters", map[string]interface{}{
		"items": []interface{}{applyFailureCR("default-dsc", "True", immutable, owner)},
	})
	f.obj("GET", "/apis/config.opendatahub.io/v1alpha1/platforms", map[string]interface{}{
		"items": []interface{}{applyFailureCR("default", "False", immutable2)},
	})
	out := checkApplyFailures(c.WithContext(context.Background()))
	if len(out.problems) != 1 {
		t.Fatalf("%d problems: %+v", len(out.problems), out.problems)
	}
	p := out.problems[0]
	ev := strings.Join(p.Evidence, "\n")
	if p.ID != "operator-apply-failed-deployment-redhat-ods-applications-shared" || p.Severity != "critical" ||
		!strings.Contains(p.Title, "must be recreated: spec.template.metadata.labels, spec.selector cannot be changed") || !strings.Contains(p.Title, "two controller owners") ||
		!strings.Contains(ev, "Reported by Platform default, DataScienceCluster default-dsc") || !strings.Contains(ev, "Controller references: Platform/default and AIHub/default-aihub") ||
		strings.Count(ev, "Failure deploying") != 3 || strings.Count(p.TechnicalCmd, "oc delete deployment.apps shared") != 1 {
		t.Fatalf("problem %+v", p)
	}
}

func TestCheckApplyFailuresHealthyAndUnreadable(t *testing.T) {
	f, c := newFakeAPI(t)
	f.obj("GET", "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters", map[string]interface{}{
		"items": []interface{}{applyFailureCR("default-dsc", "False", "Some modules are not ready: trainer")},
	})
	out := checkApplyFailures(c.WithContext(context.Background()))
	if out.check.Status != "pass" || len(out.problems) != 0 || !strings.Contains(out.check.Detail, "1 CR checked") {
		t.Fatalf("healthy: %+v %+v", out.check, out.problems)
	}
	f.status("GET", "/apis/config.opendatahub.io/v1alpha1/platforms", 403, "Forbidden")
	if out := checkApplyFailures(c.WithContext(context.Background())); out.check.Status != "warn" || !strings.Contains(out.check.Detail, "Platform") {
		t.Fatalf("unreadable: %+v", out.check)
	}
}

func keysOf(m map[string]Problem) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
