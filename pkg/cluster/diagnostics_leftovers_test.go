package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// Verbatim condition messages of the live case (RHOAI 3.6.0, 2026-11-19).
const (
	msgStoredTypes    = "failure deploying resource /default-trustyai: apply failed components.platform.opendatahub.io/v1alpha1, Kind=TrustyAI: unable to patch components.platform.opendatahub.io/v1alpha1, Kind=TrustyAI /default-trustyai: failed to create manager for existing fields: failed to convert new object (/default-trustyai; components.platform.opendatahub.io/v1alpha1, Kind=TrustyAI) to smd typed: errors:\n  .spec.eval.lmeval.permitOnline: expected string, got &value.valueUnstructured{Value:false}\n  .spec.eval.lmeval.permitCodeExecution: expected string, got &value.valueUnstructured{Value:false}"
	msgTrustySelector = `Failed to deploy operator resources: failure deploying resource redhat-ods-applications/trustyai-service-operator-controller-manager: apply failed apps/v1, Kind=Deployment: unable to patch apps/v1, Kind=Deployment redhat-ods-applications/trustyai-service-operator-controller-manager: Deployment.apps "trustyai-service-operator-controller-manager" is invalid: spec.selector: Invalid value: {"matchLabels":{"app.kubernetes.io/name":"trustyai-service-operator","control-plane":"controller-manager"}}: field is immutable`
	msgInformer       = `failed waiting for *v1alpha1.TrustyAI Informer to sync`

	trustyList  = "/apis/components.platform.opendatahub.io/v1alpha1/trustyais"
	trustyCRD   = "trustyais.components.platform.opendatahub.io"
	trustyOpDep = "/apis/apps/v1/namespaces/redhat-ods-applications/deployments/trustyai-operator-module-controller-manager"
	serviceDep  = "/apis/apps/v1/namespaces/redhat-ods-applications/deployments/trustyai-service-operator-controller-manager"
)

// leftoverWorld is the live cluster: the DSC and the TrustyAI module CR,
// its CRD, the module operator and the Deployment with the old selector.
type leftoverWorld struct {
	t   *testing.T
	f   *fakeAPI
	c   *Client
	mu  sync.Mutex
	dsc map[string]any
	cr  map[string]any
	dep map[string]any // nil: deleted
	// recreate: after a delete, the operator creates the Deployment again.
	recreate bool
	deletes  []fakeReq
}

func newLeftoverWorld(t *testing.T, dscMsgs []string, crMsgs []string, lmeval map[string]any) *leftoverWorld {
	t.Helper()
	f, c := newFakeAPI(t)
	w := &leftoverWorld{t: t, f: f, c: c.WithContext(context.Background()), recreate: true}
	serveComponentGroup(f)
	f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1", 200, `{"resources":[{"name":"trustyais","kind":"TrustyAI"},{"name":"trustyais/status","kind":"TrustyAI"}]}`)
	conds := func(ready string, msgs []string) []any {
		out := []any{map[string]any{"type": "Ready", "status": ready, "reason": "Error", "message": strings.Join(msgs, "; ")}}
		for _, m := range msgs {
			out = append(out, map[string]any{"type": "ProvisioningSucceeded", "status": "False", "reason": "Error", "message": m})
		}
		return out
	}
	dscReady, crReady := "True", "True"
	if len(dscMsgs) > 0 {
		dscReady = "False"
	}
	if len(crMsgs) > 0 {
		crReady = "False"
	}
	w.dsc = map[string]any{"apiVersion": "datasciencecluster.opendatahub.io/v2", "kind": "DataScienceCluster",
		"metadata": map[string]any{"name": "default-dsc", "uid": "dsc-uid"}, "status": map[string]any{"conditions": conds(dscReady, dscMsgs)}}
	w.cr = map[string]any{"apiVersion": "components.platform.opendatahub.io/v1alpha1", "kind": "TrustyAI",
		"metadata": map[string]any{"name": "default-trustyai", "uid": "trusty-uid", "resourceVersion": "100"},
		"spec":     map[string]any{"eval": map[string]any{"lmeval": lmeval}},
		"status":   map[string]any{"conditions": conds(crReady, crMsgs)}}
	f.handle("GET", dscV2List, func(*http.Request, []byte) (int, string) { return 200, w.list(w.dsc) })
	f.handle("GET", trustyList, func(*http.Request, []byte) (int, string) { return 200, w.list(w.cr) })
	f.handle("GET", trustyList+"/default-trustyai", func(*http.Request, []byte) (int, string) {
		w.mu.Lock()
		defer w.mu.Unlock()
		b, _ := json.Marshal(w.cr)
		return 200, string(b)
	})
	f.handle("PATCH", trustyList+"/default-trustyai", w.patchCR)
	f.handle("PATCH", dscV2List+"/default-dsc", func(*http.Request, []byte) (int, string) { return 200, w.list(w.dsc) })
	f.json("GET", dsciV2List, 200, `{"items":[{"metadata":{"name":"default-dsci"},"spec":{"applicationsNamespace":"redhat-ods-applications"}}]}`)
	serveLive36ModuleOperators(f)
	w.serveCRD("platform.opendatahub.io", lmevalSchema(true))
	// The module operator, running.
	f.json("GET", trustyOpDep, 200, `{"metadata":{"name":"trustyai-operator-module-controller-manager","namespace":"redhat-ods-applications","uid":"op"},"spec":{"replicas":1,"template":{"metadata":{"labels":{"app":"trustyai-module"}}}},"status":{"availableReplicas":1,"readyReplicas":1}}`)
	f.json("GET", "/api/v1/namespaces/redhat-ods-applications/pods", 200, `{"items":[]}`)
	f.json("GET", "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations", 200, `{"items":[]}`)
	f.json("GET", "/apis/admissionregistration.k8s.io/v1/mutatingwebhookconfigurations", 200, `{"items":[]}`)
	// The Deployment with the old selector, owned by the module CR.
	w.dep = w.serviceDeployment("old-uid")
	f.handle("GET", serviceDep, func(*http.Request, []byte) (int, string) {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.dep == nil {
			return 404, k8sNotFound
		}
		b, _ := json.Marshal(w.dep)
		return 200, string(b)
	})
	f.handle("DELETE", serviceDep, func(r *http.Request, body []byte) (int, string) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.deletes = append(w.deletes, fakeReq{Method: r.Method, Query: r.URL.RawQuery, Body: string(body)})
		if r.URL.Query().Get("dryRun") != "All" {
			w.dep = nil
			if w.recreate {
				w.dep = w.serviceDeployment("new-uid")
			}
		}
		return 200, `{"kind":"Status","status":"Success"}`
	})
	return w
}

func (w *leftoverWorld) serviceDeployment(uid string) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": "trustyai-service-operator-controller-manager", "namespace": "redhat-ods-applications", "uid": uid, "resourceVersion": "7",
			"ownerReferences": []any{map[string]any{"apiVersion": "components.platform.opendatahub.io/v1alpha1", "kind": "TrustyAI", "name": "default-trustyai", "uid": "trusty-uid", "controller": true}}},
		"spec":   map[string]any{"replicas": 1, "template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"control-plane": "controller-manager"}}}},
		"status": map[string]any{"readyReplicas": 1},
	}
}

func (w *leftoverWorld) list(obj map[string]any) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"items": []any{obj}})
	return string(b)
}

// patchCR applies a JSON patch to the module CR (a dry run changes
// nothing), as the API server does: a failed test is a 422.
func (w *leftoverWorld) patchCR(r *http.Request, body []byte) (int, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var ops []kube.JSONPatchOp
	if err := json.Unmarshal(body, &ops); err != nil {
		return 400, `{"kind":"Status","code":400}`
	}
	next, err := kube.ApplyJSONPatch(w.cr, ops)
	if _, ok := errors.AsType[*kube.JSONPatchTestError](err); ok {
		return 422, apiServerPatchTestFailed
	}
	if err != nil {
		return 422, `{"kind":"Status","status":"Failure","code":422,"message":"` + err.Error() + `"}`
	}
	if r.URL.Query().Get("dryRun") != "All" {
		bumpResourceVersion(next)
		w.cr = next
	}
	b, _ := json.Marshal(next)
	return 200, string(b)
}

// lmevalSchema is the installed CRD's schema of spec.eval.lmeval (RHOAI
// 3.6 components.platform.opendatahub.io_trustyais.yaml): strings with an
// enum and a default, or (withDefault false) without the default.
func lmevalSchema(withDefault bool) map[string]any {
	field := func() map[string]any {
		m := map[string]any{"type": "string", "enum": []any{"allow", "deny"}}
		if withDefault {
			m["default"] = "deny"
		}
		return m
	}
	return map[string]any{"type": "object", "properties": map[string]any{
		"spec": map[string]any{"type": "object", "properties": map[string]any{
			"eval": map[string]any{"type": "object", "properties": map[string]any{
				"lmeval": map[string]any{"type": "object", "properties": map[string]any{
					"permitOnline": field(), "permitCodeExecution": field()}}}}}}}}
}

// serveCRD serves the TrustyAI CRD whose spec.versions manager wrote last.
func (w *leftoverWorld) serveCRD(manager string, schema map[string]any) {
	w.serveCRDManagers([]string{manager}, schema)
}

// serveCRDManagers serves the TrustyAI CRD whose spec.versions the managers
// wrote at the same time; an empty name: no entry owns spec.versions.
func (w *leftoverWorld) serveCRDManagers(managers []string, schema map[string]any) {
	var entries []any
	for _, m := range managers {
		if m == "" {
			continue
		}
		entries = append(entries, map[string]any{"manager": m, "operation": "Apply", "time": "2026-11-01T10:00:00Z", "fieldsV1": map[string]any{"f:spec": map[string]any{"f:versions": map[string]any{}}}})
	}
	// An older write by the RHOAI operator never counts once another
	// manager wrote later.
	entries = append(entries, map[string]any{"manager": platformCRDManager, "operation": "Apply", "time": "2026-10-01T10:00:00Z", "fieldsV1": map[string]any{"f:metadata": map[string]any{}}})
	crd := map[string]any{
		"metadata": map[string]any{"name": trustyCRD, "managedFields": entries},
		"spec":     map[string]any{"versions": []any{map[string]any{"name": "v1alpha1", "served": true, "storage": true, "schema": map[string]any{"openAPIV3Schema": schema}}}},
	}
	w.f.obj("/apis/apiextensions.k8s.io/v1/customresourcedefinitions", map[string]any{"items": []any{crd}})
}

func (w *leftoverWorld) scan() checkOutput {
	w.t.Helper()
	out := checkLeftovers(w.c)
	out.check.Name = leftoversCheckName
	return out
}

func onlyProblem(t *testing.T, out checkOutput) Problem {
	t.Helper()
	if len(out.problems) != 1 {
		t.Fatalf("%d problems: %+v (check %+v)", len(out.problems), out.problems, out.check)
	}
	return out.problems[0]
}

func storedFalse() map[string]any {
	return map[string]any{"permitOnline": false, "permitCodeExecution": false}
}

// recreateAck scans and returns the acknowledgement binding of the problem
// whose fix recreates the live Deployment with the old selector, as the Fix dialog sends it; a placeholder when the scan offers
// no acknowledged fix (the fix refuses before it compares).
func recreateAck(t *testing.T, w *leftoverWorld) AckBinding {
	t.Helper()
	const id = "recreate-leftover:Deployment/redhat-ods-applications/trustyai-service-operator-controller-manager"
	for _, p := range w.scan().problems {
		if p.AutoFixAction == id && p.AcknowledgeBinding != nil {
			return *p.AcknowledgeBinding
		}
	}
	return AckBinding{Hash: "not-offered"}
}

// scannedWithBinding is the only problem of a scan, which must offer an
// acknowledgement binding (as the Fix dialog sends it back).
func scannedWithBinding(t *testing.T, w *leftoverWorld) Problem {
	t.Helper()
	p := onlyProblem(t, w.scan())
	if p.AcknowledgeBinding == nil {
		t.Fatalf("no binding: %+v", p)
	}
	return p
}

func TestParseStoredTypeFailure(t *testing.T) {
	got := parseApplyFailures(msgStoredTypes)
	if len(got) != 1 || got[0].Class != applyStoredType || got[0].Kind != "TrustyAI" || got[0].Name != "default-trustyai" || got[0].Namespace != "" ||
		strings.Join(got[0].Fields, "|") != ".spec.eval.lmeval.permitOnline: expected string, got &value.valueUnstructured{Value:false}|.spec.eval.lmeval.permitCodeExecution: expected string, got &value.valueUnstructured{Value:false}" {
		t.Fatalf("%+v", got)
	}
	if !got[0].leftover() || got[0].problemID() != "leftover-fields-trustyai-default-trustyai" {
		t.Errorf("leftover %v, id %s", got[0].leftover(), got[0].problemID())
	}
	sel := parseApplyFailures(msgTrustySelector)
	if len(sel) != 1 || !sel[0].leftover() || sel[0].problemID() != "leftover-recreate-deployment-redhat-ods-applications-trustyai-service-operator-controller-manager" {
		t.Fatalf("%+v", sel)
	}
	// The live condition explains the component in the Components table.
	if got := componentCause(dscCondition{Message: msgStoredTypes}); got != "An object is left over from an earlier release" {
		t.Errorf("component cause %q", got)
	}
	// The DataScienceCluster problem links to this check's problem.
	cc := &classifiedCondition{Cond: dscCondition{Type: "ProvisioningSucceeded", Status: "False", Message: msgStoredTypes}}
	cc.classify(map[string]*dependency{}, &[]*dependency{})
	if strings.Join(cc.ApplyIDs, ",") != "leftover-fields-trustyai-default-trustyai" {
		t.Errorf("DSC links %v", cc.ApplyIDs)
	}
}

// The live case (a): booleans stored where the CRD now has a string enum
// with a default. The plan sets the default and shows each field; even a
// stored false needs an acknowledgement (nothing says false means the
// default), bound to the object and the statement; the stuck module
// operator links to it.
func TestLeftoverStoredFieldsScan(t *testing.T) {
	w := newLeftoverWorld(t, []string{msgStoredTypes}, nil, storedFalse())
	w.f.json("GET", "/api/v1/namespaces/redhat-ods-applications/pods", 200, `{"items":[{"metadata":{"name":"trustyai-operator-module-controller-manager-abc","namespace":"redhat-ods-applications","creationTimestamp":"2026-11-10T00:00:00Z","labels":{"pod-template-hash":"abc"},
		"ownerReferences":[{"kind":"ReplicaSet","name":"trustyai-operator-module-controller-manager-abc","controller":true}]},
		"status":{"phase":"Running","containerStatuses":[{"name":"manager","restartCount":1061,"state":{"waiting":{"reason":"CrashLoopBackOff"}},"lastState":{"terminated":{"reason":"Error","exitCode":1,"message":"`+msgInformer+`"}}}]}}]}`)
	out := w.scan()
	p := onlyProblem(t, out)
	if out.check.Status != "fail" || p.ID != "leftover-fields-trustyai-default-trustyai" || p.Severity != "critical" || !p.AutoFixable ||
		p.AutoFixAction != "fix-leftover-fields:TrustyAI/default-trustyai" || p.Title != "TrustyAI default-trustyai has field values from an earlier release" ||
		!strings.Contains(p.Acknowledge, `.spec.eval.lmeval.permitOnline (false -> "deny"), .spec.eval.lmeval.permitCodeExecution (false -> "deny")`) ||
		!strings.Contains(p.Acknowledge, "may not mean the same") {
		t.Fatalf("problem %+v", p)
	}
	if b := p.AcknowledgeBinding; b == nil || b.UID != "trusty-uid" || b.ResourceVersion != "100" || *b != *newAckBinding("trusty-uid", "100", p.Acknowledge) {
		t.Fatalf("binding %+v", p.AcknowledgeBinding)
	}
	if len(p.FieldChanges) != 2 {
		t.Fatalf("field changes %+v", p.FieldChanges)
	}
	for _, fc := range p.FieldChanges {
		if fc.Change != "set-default" || fc.Now != "false" || fc.After != `"deny"` || !fc.Review || fc.Schema != "string, one of allow, deny, default deny, optional" {
			t.Errorf("field change %+v", fc)
		}
	}
	ev := strings.Join(p.Evidence, "\n")
	for _, want := range []string{"Reported by DataScienceCluster default-dsc", "to smd typed", "Field .spec.eval.lmeval.permitOnline: stored false; the installed CRD says string",
		"last written by the RHOAI operator (field manager platform.opendatahub.io)", "crash-looping (1061 restarts)", msgInformer} {
		if !strings.Contains(ev, want) {
			t.Errorf("evidence lacks %q:\n%s", want, ev)
		}
	}
	if !strings.Contains(p.ConfirmMessage, `.spec.eval.lmeval.permitOnline: false -> "deny"`) || !strings.Contains(p.TechnicalCmd, "oc patch trustyai.components.platform.opendatahub.io default-trustyai --type=json") {
		t.Errorf("confirm %q, cmd %q", p.ConfirmMessage, p.TechnicalCmd)
	}
	// The crash-looping module operator links to it.
	pods := checkRHOAIPods(w.c)
	linked := linkProblems(append(pods.problems, p))
	crash := findProblem(&DiagnosticsResponse{Problems: linked}, "pod-crashloop-redhat-ods-applications-trustyai-operator-module-controller-manager")
	if crash == nil || !slices.Contains(crash.RelatedProblems, p.ID) || !strings.Contains(strings.Join(crash.Evidence, "\n"), "Likely caused by") {
		t.Errorf("crash problem %+v", crash)
	}
	// The apply-failures check leaves the object to this check.
	if apply := checkApplyFailures(w.c); len(apply.problems) != 0 {
		t.Errorf("also reported by %s: %+v", applyFailuresCheckName, apply.problems)
	}
}

// The fix: a dry run, then one tested JSON patch with the user's token;
// the operator is not nudged. A re-scan while the DSC still shows the old
// error offers Retry now, which adds and removes an annotation.
func TestLeftoverStoredFieldsFixAndRetry(t *testing.T) {
	w := newLeftoverWorld(t, []string{msgStoredTypes}, nil, storedFalse())
	p := scannedWithBinding(t, w)
	res, err := ApplyFixAcknowledged(w.c, "fix-leftover-fields:TrustyAI/default-trustyai", *p.AcknowledgeBinding)
	if err != nil || !res.Success || !strings.Contains(res.Message, `.spec.eval.lmeval.permitOnline false -> "deny"`) {
		t.Fatalf("%+v %v", res, err)
	}
	patches := w.f.requests("PATCH", trustyList+"/default-trustyai")
	if len(patches) != 2 || !strings.Contains(patches[0].Query, "dryRun=All") || strings.Contains(patches[1].Query, "dryRun") ||
		!strings.Contains(patches[1].Query, "fieldValidation=Strict") {
		t.Fatalf("patches %+v", patches)
	}
	var ops []map[string]any
	_ = json.Unmarshal([]byte(patches[1].Body), &ops)
	if len(ops) != 6 || ops[0]["op"] != "test" || ops[0]["path"] != "/metadata/uid" || ops[1]["path"] != "/metadata/resourceVersion" || ops[1]["value"] != "100" ||
		ops[2]["op"] != "test" || ops[2]["value"] != false || ops[3]["op"] != "replace" || ops[3]["value"] != "deny" {
		t.Fatalf("ops %+v", ops)
	}
	assertWrites(t, w.f, "PATCH "+trustyList+"/default-trustyai", "PATCH "+trustyList+"/default-trustyai")
	lmeval := w.cr["spec"].(map[string]any)["eval"].(map[string]any)["lmeval"].(map[string]any)
	if lmeval["permitOnline"] != "deny" || lmeval["permitCodeExecution"] != "deny" {
		t.Fatalf("object %+v", lmeval)
	}

	// The DSC still shows its last error: Retry now.
	p = onlyProblem(t, w.scan())
	if p.AutoFixAction != "retry-dsc-reconcile:default-dsc" || p.Severity != "warning" || !strings.Contains(p.Title, "already fixed") || p.FieldChanges[0].Change != "valid" {
		t.Fatalf("retry problem %+v", p)
	}
	// The same field fix again changes nothing.
	again, _ := ApplyFix(w.c, "fix-leftover-fields:TrustyAI/default-trustyai")
	if again.ErrorCode != "nothing_to_do" || len(w.f.requests("PATCH", trustyList+"/default-trustyai")) != 2 {
		t.Fatalf("again %+v", again)
	}
	res, _ = ApplyFix(w.c, p.AutoFixAction)
	dscPatches := w.f.requests("PATCH", dscV2List+"/default-dsc")
	if !res.Success || len(dscPatches) != 2 || !strings.Contains(dscPatches[0].Body, `"uid":"dsc-uid"`) || !strings.Contains(dscPatches[0].Body, leftoverRetryAnno) ||
		!strings.Contains(dscPatches[1].Body, `"`+leftoverRetryAnno+`":null`) {
		t.Fatalf("retry %+v, patches %+v", res, dscPatches)
	}
}

// Every value replaced (false and true alike) needs the acknowledgement,
// and without one nothing changes.
func TestLeftoverStoredFieldsReviewNeedsAcknowledgement(t *testing.T) {
	w := newLeftoverWorld(t, []string{msgStoredTypes}, nil, map[string]any{"permitOnline": true, "permitCodeExecution": false})
	p := onlyProblem(t, w.scan())
	if !strings.Contains(p.Acknowledge, `.spec.eval.lmeval.permitOnline (true -> "deny"), .spec.eval.lmeval.permitCodeExecution (false -> "deny")`) {
		t.Fatalf("acknowledge %q", p.Acknowledge)
	}
	res, _ := ApplyFix(w.c, p.AutoFixAction)
	if res.Success || res.ErrorCode != "acknowledgement_required" || !strings.Contains(res.Message, `.spec.eval.lmeval.permitCodeExecution: the stored false may not mean the same as "deny"`) {
		t.Fatalf("%+v", res)
	}
	assertWrites(t, w.f)
	res, _ = ApplyFixAcknowledged(w.c, p.AutoFixAction, *p.AcknowledgeBinding)
	if !res.Success {
		t.Fatalf("%+v", res)
	}
}

// The acknowledgement binds to what the user reviewed: another UID or
// resourceVersion, or another statement (a value changed between the scan
// and the fix), is a conflict and nothing is written.
func TestLeftoverStoredFieldsAcknowledgementBinding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(w *leftoverWorld, b *AckBinding)
		reason string
	}{
		{"recreated (UID)", func(w *leftoverWorld, _ *AckBinding) {
			w.cr["metadata"].(map[string]any)["uid"] = "trusty-uid-2"
		}, "its UID"},
		{"changed (resourceVersion)", func(w *leftoverWorld, _ *AckBinding) {
			w.cr["metadata"].(map[string]any)["resourceVersion"] = "101"
		}, "its resourceVersion"},
		{"other reasons", func(w *leftoverWorld, _ *AckBinding) {
			w.cr["spec"].(map[string]any)["eval"].(map[string]any)["lmeval"].(map[string]any)["permitOnline"] = true
		}, "what needs your acknowledgement"},
		{"forged hash", func(_ *leftoverWorld, b *AckBinding) { b.Hash = "0000" }, "what needs your acknowledgement"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newLeftoverWorld(t, []string{msgStoredTypes}, nil, storedFalse())
			p := scannedWithBinding(t, w)
			b := *p.AcknowledgeBinding
			w.mu.Lock()
			tc.edit(w, &b)
			w.mu.Unlock()
			res, _ := ApplyFixAcknowledged(w.c, p.AutoFixAction, b)
			if res.Success || res.ErrorCode != "conflict" || !strings.Contains(res.Message, tc.reason) || !strings.Contains(res.Message, "Nothing was changed") {
				t.Fatalf("%+v", res)
			}
			assertWrites(t, w.f)
		})
	}
}

// Without a default, an optional field is removed; a required one has no
// automatic fix.
func TestLeftoverStoredFieldsNoDefault(t *testing.T) {
	w := newLeftoverWorld(t, []string{msgStoredTypes}, nil, storedFalse())
	w.serveCRD("platform.opendatahub.io", lmevalSchema(false))
	p := onlyProblem(t, w.scan())
	if p.FieldChanges[0].Change != "remove" || p.FieldChanges[0].After != "" || !p.FieldChanges[0].Review || !strings.Contains(p.ConfirmMessage, "false -> removed") ||
		!strings.Contains(p.Acknowledge, "(false -> removed)") {
		t.Fatalf("%+v %q", p.FieldChanges, p.Acknowledge)
	}
	if res, _ := ApplyFix(w.c, p.AutoFixAction); res.ErrorCode != "acknowledgement_required" {
		t.Fatalf("removal without acknowledgement: %+v", res)
	}
	res, _ := ApplyFixAcknowledged(w.c, p.AutoFixAction, *p.AcknowledgeBinding)
	lmeval := w.cr["spec"].(map[string]any)["eval"].(map[string]any)["lmeval"].(map[string]any)
	if !res.Success || len(lmeval) != 0 {
		t.Fatalf("%+v %+v", res, lmeval)
	}

	w = newLeftoverWorld(t, []string{msgStoredTypes}, nil, storedFalse())
	schema := lmevalSchema(false)
	lm := schema["properties"].(map[string]any)["spec"].(map[string]any)["properties"].(map[string]any)["eval"].(map[string]any)["properties"].(map[string]any)["lmeval"].(map[string]any)
	lm["required"] = []any{"permitOnline"}
	w.serveCRD("platform.opendatahub.io", schema)
	p = onlyProblem(t, w.scan())
	if p.AutoFixable || !strings.Contains(p.Fix, "required and the CRD gives no default") {
		t.Fatalf("%+v", p)
	}
	res, _ = ApplyFix(w.c, "fix-leftover-fields:TrustyAI/default-trustyai")
	if res.ErrorCode != "prerequisites" {
		t.Fatalf("%+v", res)
	}
	assertWrites(t, w.f)
}

// The CRD counts as current only when (a) the installed CSV owns it or (b)
// the RHOAI operator's field manager (platform.opendatahub.io) wrote its
// spec.versions last. Any other writer, two writers at the same time, or
// none means the tool cannot tell, and nothing is changed.
func TestLeftoverStoredFieldsCRDNotCurrent(t *testing.T) {
	for manager, want := range map[string]string{
		"catalog":                   "last written by catalog",
		"kubectl-client-side-apply": "last written by kubectl-client-side-apply",
		// Formerly taken as "the running operator": any manager that is
		// not OLM or oc. Nothing says who that is.
		"manager":                    "last written by manager",
		"platform.opendatahub.io|oc": "last written by oc and platform.opendatahub.io at the same time",
		"":                           "do not say who wrote its schema",
	} {
		w := newLeftoverWorld(t, []string{msgStoredTypes}, nil, storedFalse())
		w.serveCRDManagers(strings.Split(manager, "|"), lmevalSchema(true))
		serveInstalledOperator(w.f)
		p := onlyProblem(t, w.scan())
		if p.AutoFixable || !strings.Contains(p.Fix, want) || !strings.Contains(p.Fix, "can't tell that CRD "+trustyCRD+" is the newer side") ||
			!strings.Contains(p.Fix, "the installed CSV rhods-operator.3.6.0 does not own it") {
			t.Errorf("%q: %+v", manager, p.Fix)
		}
		res, _ := ApplyFixAcknowledged(w.c, "fix-leftover-fields:TrustyAI/default-trustyai", AckBinding{Hash: "x"})
		if res.ErrorCode != "prerequisites" {
			t.Errorf("%q: %+v", manager, res)
		}
		assertWrites(t, w.f)
	}
	// The installed CSV owns it: current, whoever wrote it last.
	w := newLeftoverWorld(t, []string{msgStoredTypes}, nil, storedFalse())
	w.serveCRD("kubectl-edit", lmevalSchema(true))
	w.f.json("GET", namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName), 200, `{"status":{"installedCSV":"rhods-operator.3.6.0"}}`)
	w.f.json("GET", namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "rhods-operator.3.6.0"), 200,
		`{"spec":{"customresourcedefinitions":{"owned":[{"name":"`+trustyCRD+`"}]}}}`)
	if p := onlyProblem(t, w.scan()); !p.AutoFixable || !strings.Contains(strings.Join(p.Evidence, "\n"), "the installed CSV rhods-operator.3.6.0 owns it") {
		t.Errorf("owned CRD: %+v", p)
	}
}

// The API server refuses: a dry-run failure changes nothing, a 409 is a
// conflict. A bare 422 (the API server's whole answer to a failed JSON
// Patch test) on an object whose uid and resourceVersion did not move is
// the server's refusal, shown as is; a failed test on an object that moved
// is a conflict (below).
func TestLeftoverStoredFieldsWriteRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, dryBody, code string
		dryStatus           int
	}{
		{"invalid", `{"kind":"Status","status":"Failure","code":422,"reason":"Invalid","message":"TrustyAI.components.platform.opendatahub.io \"default-trustyai\" is invalid: spec.x: Invalid value"}`, "validation", 422},
		{"bare 422, object unchanged", apiServerPatchTestFailed, "validation", 422},
		{"conflict", `{"kind":"Status","status":"Failure","code":409,"reason":"Conflict","message":"Operation cannot be fulfilled"}`, "conflict", 409},
		{"forbidden", `{"kind":"Status","status":"Failure","code":403,"reason":"Forbidden","message":"trustyais is forbidden"}`, "forbidden", 403},
	} {
		w := newLeftoverWorld(t, []string{msgStoredTypes}, nil, storedFalse())
		w.f.json("PATCH", trustyList+"/default-trustyai", tc.dryStatus, tc.dryBody)
		res, _ := ApplyFixAcknowledged(w.c, "fix-leftover-fields:TrustyAI/default-trustyai", *scannedWithBinding(t, w).AcknowledgeBinding)
		if res.Success || res.ErrorCode != tc.code || !strings.Contains(res.Message, "Nothing was changed") && tc.code != "conflict" {
			t.Errorf("%s: %+v", tc.name, res)
		}
		if len(w.f.realWrites()) != 0 {
			t.Errorf("%s: real writes %v", tc.name, w.f.realWrites())
		}
	}
	// The test op itself fails when the stored value moved after the read.
	w := newLeftoverWorld(t, []string{msgStoredTypes}, nil, storedFalse())
	ack := *scannedWithBinding(t, w).AcknowledgeBinding
	w.f.handle("PATCH", trustyList+"/default-trustyai", func(r *http.Request, body []byte) (int, string) {
		w.mu.Lock()
		w.cr["spec"].(map[string]any)["eval"].(map[string]any)["lmeval"].(map[string]any)["permitOnline"] = true
		bumpResourceVersion(w.cr)
		w.mu.Unlock()
		return w.patchCR(r, body)
	})
	res, _ := ApplyFixAcknowledged(w.c, "fix-leftover-fields:TrustyAI/default-trustyai", ack)
	if res.ErrorCode != "conflict" || len(w.f.realWrites()) != 0 {
		t.Fatalf("%+v %v", res, w.f.realWrites())
	}
	// Bad targets.
	for _, id := range []string{"fix-leftover-fields:", "fix-leftover-fields:TrustyAI", "fix-leftover-fields:Trusty AI/x", "fix-leftover-fields:TrustyAI/$(id)"} {
		if res, _ := ApplyFix(w.c, id); res.ErrorCode != "validation" {
			t.Errorf("%s: %+v", id, res)
		}
	}
	// No longer reported.
	w = newLeftoverWorld(t, nil, nil, storedFalse())
	if res, _ := ApplyFix(w.c, "fix-leftover-fields:TrustyAI/default-trustyai"); res.ErrorCode != "nothing_to_do" {
		t.Fatalf("%+v", res)
	}
	if res, _ := ApplyFix(w.c, "retry-dsc-reconcile:default-dsc"); res.ErrorCode != "nothing_to_do" {
		t.Fatalf("%+v", res)
	}
	assertWrites(t, w.f)
}

// The live case (b): the module CR reports the Deployment it owns.
// TrustyAI has no in-use reader, so the fix needs an acknowledgement; then
// a dry run, a delete with UID and resourceVersion preconditions, and a
// wait until the operator created it again.
func TestLeftoverRecreateDeployment(t *testing.T) {
	w := newLeftoverWorld(t, nil, []string{msgTrustySelector}, storedFalse())
	p := onlyProblem(t, w.scan())
	id := "recreate-leftover:Deployment/redhat-ods-applications/trustyai-service-operator-controller-manager"
	if p.ID != "leftover-recreate-deployment-redhat-ods-applications-trustyai-service-operator-controller-manager" || !p.AutoFixable || p.AutoFixAction != id ||
		p.Severity != "critical" || !strings.Contains(p.Acknowledge, "cannot tell whether trustyai is in use") ||
		!strings.Contains(strings.Join(p.Evidence, "\n"), "owned by TrustyAI default-trustyai; the trustyai module operator redhat-ods-applications/trustyai-operator-module-controller-manager creates it again") {
		t.Fatalf("%+v", p)
	}
	res, _ := ApplyFix(w.c, id)
	if res.ErrorCode != "acknowledgement_required" || len(w.deletes) != 0 {
		t.Fatalf("%+v", res)
	}
	res, _ = ApplyFixAcknowledged(w.c, id, recreateAck(t, w))
	if !res.Success || !strings.HasPrefix(res.Message, "Recreated Deployment redhat-ods-applications/trustyai-service-operator-controller-manager") {
		t.Fatalf("%+v", res)
	}
	if len(w.deletes) != 2 || w.deletes[0].Query != "dryRun=All" || w.deletes[1].Query != "" ||
		!strings.Contains(w.deletes[1].Body, `"propagationPolicy":"Background"`) || !strings.Contains(w.deletes[1].Body, `"uid":"old-uid"`) || !strings.Contains(w.deletes[1].Body, `"resourceVersion":"7"`) {
		t.Fatalf("deletes %+v", w.deletes)
	}
	assertWrites(t, w.f, "DELETE "+serviceDep, "DELETE "+serviceDep)
}

func TestLeftoverRecreateNotBackInTime(t *testing.T) {
	w := newLeftoverWorld(t, nil, []string{msgTrustySelector}, storedFalse())
	w.recreate = false
	start := kube.Clock().Now()
	res, _ := ApplyFixAcknowledged(w.c, "recreate-leftover:Deployment/redhat-ods-applications/trustyai-service-operator-controller-manager", recreateAck(t, w))
	if res.Success || res.ErrorCode != "not_recreated" || kube.Clock().Since(start) < leftoverRecreateWait {
		t.Fatalf("%+v", res)
	}
	// Deleted already: the scan says it waits for the operator.
	p := onlyProblem(t, w.scan())
	if p.AutoFixable || !strings.Contains(p.Title, "waiting for the operator") {
		t.Fatalf("%+v", p)
	}
}

// Every refusal: not owned by a reporting module CR, another namespace,
// the recreating operator down, the delete refused or racing.
func TestLeftoverRecreateRefusals(t *testing.T) {
	id := "recreate-leftover:Deployment/redhat-ods-applications/trustyai-service-operator-controller-manager"
	for _, tc := range []struct {
		name  string
		setup func(w *leftoverWorld)
		want  string
	}{
		{"platform owner", func(w *leftoverWorld) {
			w.dep["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"apiVersion": "config.opendatahub.io/v1alpha1", "kind": "Platform", "name": "default", "uid": "p", "controller": true}}
		}, "controller owner Platform default"},
		{"other uid", func(w *leftoverWorld) {
			w.dep["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any)["uid"] = "someone-else"
		}, "not a module CR or the DataScienceCluster that reports the failure"},
		{"no owner", func(w *leftoverWorld) { delete(w.dep["metadata"].(map[string]any), "ownerReferences") }, "no controller owner"},
		{"namespace", func(w *leftoverWorld) {
			w.f.json("GET", dsciV2List, 200, `{"items":[{"spec":{"applicationsNamespace":"opendatahub"}}]}`)
		}, "not in the RHOAI applications namespace opendatahub"},
		{"no dsci", func(w *leftoverWorld) { w.f.json("GET", dsciV2List, 200, `{"items":[]}`) }, "applications namespace could not be read"},
		{"operator down", func(w *leftoverWorld) {
			w.f.json("GET", trustyOpDep, 200, `{"metadata":{"name":"trustyai-operator-module-controller-manager","namespace":"redhat-ods-applications"},"spec":{"replicas":1},"status":{"availableReplicas":0}}`)
		}, "has no available pod"},
		{"deleting", func(w *leftoverWorld) {
			w.dep["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-19T00:00:00Z"
		}, "already being deleted"},
	} {
		w := newLeftoverWorld(t, nil, []string{msgTrustySelector}, storedFalse())
		tc.setup(w)
		p := onlyProblem(t, w.scan())
		if p.AutoFixable || !strings.Contains(p.Fix, tc.want) {
			t.Errorf("%s: scan %+v", tc.name, p)
		}
		res, _ := ApplyFixAcknowledged(w.c, id, recreateAck(t, w))
		if res.ErrorCode != "prerequisites" || !strings.Contains(res.Message, tc.want) || len(w.deletes) != 0 {
			t.Errorf("%s: fix %+v", tc.name, res)
		}
	}
	for _, tc := range []struct {
		name   string
		status int
		body   string
		dry    bool
		code   string
	}{
		{"dry run forbidden", 403, `{"kind":"Status","status":"Failure","code":403,"reason":"Forbidden","message":"deployments.apps is forbidden"}`, true, "forbidden"},
		{"precondition", 409, `{"kind":"Status","status":"Failure","code":409,"reason":"Conflict","message":"Precondition failed: UID in precondition"}`, false, "conflict"},
	} {
		w := newLeftoverWorld(t, nil, []string{msgTrustySelector}, storedFalse())
		w.f.handle("DELETE", serviceDep, func(r *http.Request, _ []byte) (int, string) {
			if tc.dry || r.URL.Query().Get("dryRun") != "All" {
				return tc.status, tc.body
			}
			return 200, `{}`
		})
		res, _ := ApplyFixAcknowledged(w.c, id, recreateAck(t, w))
		if res.Success || res.ErrorCode != tc.code || w.dep == nil {
			t.Errorf("%s: %+v", tc.name, res)
		}
	}
	w := newLeftoverWorld(t, nil, nil, storedFalse())
	for _, bad := range []string{"recreate-leftover:Service/redhat-ods-applications/x", "recreate-leftover:Deployment/x", "recreate-leftover:Deployment/NS/x"} {
		if res, _ := ApplyFixAcknowledged(w.c, bad, AckBinding{Hash: "x"}); res.ErrorCode != "validation" {
			t.Errorf("%s: %+v", bad, res)
		}
	}
	if res, _ := ApplyFixAcknowledged(w.c, id, recreateAck(t, w)); res.ErrorCode != "nothing_to_do" || len(w.deletes) != 0 {
		t.Errorf("not reported: %+v", res)
	}
}

// A component with an in-use reader: nothing in use needs no
// acknowledgement; in use, or unreadable, does.
func TestLeftoverRecreateUsers(t *testing.T) {
	users := func(list []string, err error) usersReader {
		return func(*Client, string) ([]string, error) { return list, err }
	}
	rf := func(w *leftoverWorld) *reportedApplyFailure {
		s := scanApplyReporters(w.c, allApplyFailures)
		r := leftoverObject(s, "apps", "Deployment", "redhat-ods-applications", "trustyai-service-operator-controller-manager", applyImmutable)
		if r == nil {
			t.Fatal("not found")
		}
		return r
	}
	usersCheckedComponents["trustyai"] = true
	defer delete(usersCheckedComponents, "trustyai")
	w := newLeftoverWorld(t, nil, []string{msgTrustySelector}, storedFalse())
	if r := analyzeRecreate(w.c, rf(w), "redhat-ods-applications", readDeploymentForScan, users(nil, nil)); len(r.AckReasons) != 0 || len(r.Blockers) != 0 {
		t.Errorf("not in use: %+v", r)
	}
	r := analyzeRecreate(w.c, rf(w), "redhat-ods-applications", readDeploymentForScan, users([]string{"LMEvalJob p/x"}, nil))
	if !strings.Contains(strings.Join(r.AckReasons, ";"), "in use (LMEvalJob p/x)") {
		t.Errorf("in use: %+v", r)
	}
	r = analyzeRecreate(w.c, rf(w), "redhat-ods-applications", readDeploymentForScan, users(nil, context.DeadlineExceeded))
	if !strings.Contains(strings.Join(r.AckReasons, ";"), "could not check what uses trustyai") {
		t.Errorf("error: %+v", r)
	}
	// The gitops scan does not read users.
	if p := onlyProblem(t, checkLeftoversGitOps(w.c)); !strings.Contains(p.Acknowledge, "this scan does not check what uses trustyai") {
		t.Errorf("gitops: %+v", p)
	}
}

// A workload kind the ServiceAccount cannot read is checked by the fix,
// with the user's token, and always needs an acknowledgement.
func TestLeftoverRecreateStatefulSetNotReadInScan(t *testing.T) {
	msg := strings.ReplaceAll(strings.ReplaceAll(msgTrustySelector, "Kind=Deployment", "Kind=StatefulSet"), "Deployment.apps", "StatefulSet.apps")
	w := newLeftoverWorld(t, nil, []string{msg}, storedFalse())
	p := onlyProblem(t, w.scan())
	if !p.AutoFixable || !strings.Contains(strings.Join(p.Evidence, "\n"), "does not read StatefulSets") || !strings.Contains(p.Acknowledge, "could not read the StatefulSet") {
		t.Fatalf("%+v", p)
	}
	for _, r := range w.f.requests("GET", "/apis/apps/v1/namespaces/redhat-ods-applications/statefulsets/trustyai-service-operator-controller-manager") {
		t.Errorf("scan read a StatefulSet: %+v", r)
	}
	// The scan did not read it: the binding carries no UID or
	// resourceVersion, only the statement (which says so).
	if b := p.AcknowledgeBinding; b == nil || b.UID != "" || b.ResourceVersion != "" || b.Hash == "" {
		t.Fatalf("binding %+v", p.AcknowledgeBinding)
	}
	// The fix reads it with the user's token: it does not exist.
	res, _ := ApplyFixAcknowledged(w.c, p.AutoFixAction, *p.AcknowledgeBinding)
	if res.ErrorCode != "nothing_to_do" {
		t.Fatalf("%+v", res)
	}
	// It exists: the acknowledgement must still be the statement the scan
	// showed, and what the user's read adds (here: whether trustyai is in
	// use, which needs the owner the scan could not read, and the webhooks
	// its pods serve) is not covered, so nothing is deleted.
	sts := w.serviceDeployment("sts-uid")
	sts["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["labels"] = map[string]any{"control-plane": "controller-manager"}
	w.f.obj("/apis/apps/v1/namespaces/redhat-ods-applications/statefulsets/trustyai-service-operator-controller-manager", sts)
	forged := *p.AcknowledgeBinding
	forged.Hash = "0000"
	if res, _ := ApplyFixAcknowledged(w.c, p.AutoFixAction, forged); res.ErrorCode != "conflict" || !strings.Contains(res.Message, "what needs your acknowledgement") {
		t.Fatalf("forged: %+v", res)
	}
	w.f.obj("/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations", map[string]any{"items": []any{map[string]any{
		"metadata": map[string]any{"name": "trustyai-hooks"},
		"webhooks": []any{map[string]any{"name": "v.trustyai.opendatahub.io", "failurePolicy": "Fail",
			"clientConfig": map[string]any{"service": map[string]any{"namespace": "redhat-ods-applications", "name": "trusty-webhook"}}}},
	}}})
	w.f.obj("/api/v1/namespaces/redhat-ods-applications/services/trusty-webhook", map[string]any{"metadata": map[string]any{"name": "trusty-webhook"}, "spec": map[string]any{"selector": map[string]any{"control-plane": "controller-manager"}}})
	res, _ = ApplyFixAcknowledged(w.c, p.AutoFixAction, *p.AcknowledgeBinding)
	if res.ErrorCode != "conflict" || !strings.Contains(res.Message, "found what the scan could not show") || !strings.Contains(res.Message, "whether trustyai is in use") || len(w.f.writes()) != 0 {
		t.Fatalf("unseen reasons: %+v writes %v", res, w.f.writes())
	}
}

// waitRecreated polls on the fake clock; it does not sleep in real time.
func TestLeftoverWaitUsesTheClock(t *testing.T) {
	w := newLeftoverWorld(t, nil, nil, storedFalse())
	w.dep = nil
	start := time.Now()
	back, ready, err := waitRecreated(w.c, "Deployment", "redhat-ods-applications", "trustyai-service-operator-controller-manager", "old-uid")
	if back || ready || err != nil || time.Since(start) > 5*time.Second {
		t.Fatalf("%v %v %v %s", back, ready, err, time.Since(start))
	}
}
