package cluster

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

// Tests for the one stale-webhook implementation (stale_webhooks.go). The
// diagnostics presentation is tested in diagnostics_webhooks_test.go and
// diagnostics_fix_test.go, the MinIO namespace guard in
// quick_resources_minio_test.go.

const epsPathFmt = "/apis/discovery.k8s.io/v1/namespaces/%s/endpointslices"

type whFixture struct {
	name     string
	kind     string // "v" or "m"
	labels   map[string]string
	owner    string // ownerReference "Kind" in components.platform.opendatahub.io
	services []string
	created  string
	ignore   bool     // failurePolicy Ignore
	urls     []string // webhooks called through clientConfig.url
}

func (w whFixture) object() map[string]interface{} {
	meta := map[string]interface{}{"name": w.name, "uid": "uid-" + w.name, "resourceVersion": "1"}
	if w.labels != nil {
		meta["labels"] = w.labels
	}
	if w.owner != "" {
		meta["ownerReferences"] = []interface{}{map[string]interface{}{"apiVersion": "components.platform.opendatahub.io/v1alpha1", "kind": w.owner, "name": "default-" + strings.ToLower(w.owner)}}
	}
	if w.created != "" {
		meta["creationTimestamp"] = w.created
	}
	var hooks []interface{}
	for i, s := range w.services {
		ns, name, _ := strings.Cut(s, "/")
		hook := map[string]interface{}{"name": fmt.Sprintf("hook%d", i), "clientConfig": map[string]interface{}{"service": map[string]interface{}{"namespace": ns, "name": name}}}
		if w.ignore {
			hook["failurePolicy"] = "Ignore"
		}
		hooks = append(hooks, hook)
	}
	for i, u := range w.urls {
		hooks = append(hooks, map[string]interface{}{"name": fmt.Sprintf("url%d", i), "clientConfig": map[string]interface{}{"url": u}})
	}
	return map[string]interface{}{"metadata": meta, "webhooks": hooks}
}

func olmOwned(csv string) map[string]string {
	return map[string]string{"olm.owner": csv, "olm.owner.namespace": SubNS, "olm.owner.kind": "ClusterServiceVersion"}
}

// serveWebhooks registers the configs, the CSV list, live Services (with a
// ready endpoint) and module CRs. Everything not registered is NotFound.
func serveWebhooks(f *fakeAPI, configs []whFixture, csvPhase string, liveServices []string, modules map[string]bool) {
	var v, m []interface{}
	for _, w := range configs {
		if w.kind == "m" {
			m = append(m, w.object())
		} else {
			v = append(v, w.object())
		}
	}
	f.obj("GET", vwcPath, map[string]interface{}{"items": v})
	f.obj("GET", mwcPath, map[string]interface{}{"items": m})
	f.json("GET", csvBase, http.StatusOK, fmt.Sprintf(`{"items":[{"metadata":{"name":"rhods-operator.3.6.0"},"status":{"phase":%q}}]}`, csvPhase))
	f.json("GET", csvBase+"/rhods-operator.3.6.0", http.StatusOK, `{"metadata":{"name":"rhods-operator.3.6.0"}}`)
	for _, s := range liveServices {
		ns, name, _ := strings.Cut(s, "/")
		f.json("GET", svcPath(ns, name), http.StatusOK, `{"metadata":{"creationTimestamp":"2026-01-01T00:00:00Z"},"spec":{"selector":{"app":"x"}}}`)
		f.json("GET", fmt.Sprintf(epsPathFmt, ns), http.StatusOK, `{"items":[{"endpoints":[{"conditions":{"ready":true}}]}]}`)
	}
	serveComponentGroup(f)
	var resources []string
	for kind := range modules {
		resources = append(resources, fmt.Sprintf(`{"name":%q,"kind":%q}`, strings.ToLower(kind)+"s", kind))
		items := `[]`
		if modules[kind] {
			items = `[{"metadata":{"name":"default"}}]`
		}
		f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1/"+strings.ToLower(kind)+"s", http.StatusOK, `{"items":`+items+`}`)
	}
	f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1", http.StatusOK, `{"resources":[`+strings.Join(resources, ",")+`]}`)
}

func verdictsByName(scan webhookScan) map[string]webhookVerdict {
	out := map[string]webhookVerdict{}
	for _, v := range scan.Verdicts {
		out[v.Config.Metadata.Name] = v
	}
	return out
}

// The configurations mirror the live RHOAI 3.6 cluster (OLM-owned operator
// webhooks, platform-applied module webhooks, odh-model-controller) plus
// leftovers. Ported from the Reinstall cleanup tests (B1) and checked
// against RHOAI_OPERATOR_NOTES §3.4.
func TestStaleWebhookScan_AppliesEveryRule(t *testing.T) {
	ops := "redhat-ods-operator/rhods-operator-service"
	apps := "redhat-ods-applications/"
	configs := []whFixture{
		// Live OLM config, Service serving: never considered.
		{name: "datasciencecluster-v2-validator.opendatahub.io-nfzvz", labels: olmOwned("rhods-operator.3.6.0"), services: []string{ops}},
		// Old CSV gone, but its Service is served again by the new operator:
		// rule 1 keeps it (B1 deleted it on the CSV rule alone).
		{name: "dscinitialization-v1-validator.opendatahub.io-old", labels: olmOwned("rhods-operator.3.5.0"), services: []string{ops}},
		// Old CSV gone and its Service gone: deletable.
		{name: "dscinitialization-v1-validator.opendatahub.io-gone", labels: olmOwned("rhods-operator.3.5.0"), services: []string{SubNS + "/old-operator-service"}},
		// CSV exists, Service gone: OLM heals it, guidance only (D2).
		{name: "dscinitialization-v2-validator.opendatahub.io-x", labels: olmOwned("rhods-operator.3.6.0"), services: []string{SubNS + "/gone-operator-service"}},
		// Module webhook whose module is installed: guidance only.
		{name: "inferenceservice.serving.kserve.io", labels: map[string]string{"platform.opendatahub.io/part-of": "kserve"}, owner: "Kserve", services: []string{apps + "gone-kserve-service"}},
		// Module removed (no CR) and Service gone: deletable.
		{name: "kuberay-mutating-webhook-configuration", kind: "m", labels: map[string]string{"platform.opendatahub.io/part-of": "ray"}, services: []string{apps + "kuberay-webhook-service"}},
		// Owned (ownerReference only) by a removed module CR: deletable.
		{name: "odh-notebook-controller-mutating-webhook-configuration", kind: "m", owner: "Workbenches", services: []string{apps + "odh-notebook-controller-webhook-service"}},
		// Serving module webhook (the one the old name matcher deleted).
		{name: "validating.odh-model-controller.opendatahub.io", labels: map[string]string{"platform.opendatahub.io/part-of": "kserve"}, owner: "Kserve", services: []string{apps + "odh-model-controller-webhook-service"}},
		// Another operator's OLM config: never selected.
		{name: "authorino.example.io", labels: map[string]string{"olm.owner": "authorino-operator.v1.4.3", "olm.owner.namespace": "openshift-operators"}, services: []string{"openshift-operators/gone-authorino"}},
		// RHOAI name, no owner: reported, never deleted.
		{name: "legacy.opendatahub.io", services: []string{apps + "gone-legacy"}},
		// One of two Services still serves: not stale.
		{name: "mixed.opendatahub.io", labels: olmOwned("rhods-operator.3.5.0"), services: []string{SubNS + "/gone", apps + "odh-model-controller-webhook-service"}},
		// Lookup error: not stale, reported as an error.
		{name: "broken-lookup.opendatahub.io", services: []string{apps + "error-service"}},
	}
	f, c := newFakeAPI(t)
	serveWebhooks(f, configs, "Succeeded", []string{ops, apps + "odh-model-controller-webhook-service"}, map[string]bool{"Kserve": true, "Ray": false, "Workbenches": false})
	f.status("GET", svcPath("redhat-ods-applications", "error-service"), http.StatusInternalServerError, "InternalError")

	scan := scanStaleWebhooks(c)
	got := verdictsByName(scan)
	var deletable, guidance []string
	for name, v := range got {
		if v.Deletable {
			deletable = append(deletable, name)
		} else {
			guidance = append(guidance, name)
		}
	}
	sort.Strings(deletable)
	sort.Strings(guidance)
	wantDeletable := []string{"dscinitialization-v1-validator.opendatahub.io-gone", "kuberay-mutating-webhook-configuration", "odh-notebook-controller-mutating-webhook-configuration"}
	wantGuidance := []string{"dscinitialization-v2-validator.opendatahub.io-x", "inferenceservice.serving.kserve.io", "legacy.opendatahub.io"}
	if fmt.Sprint(deletable) != fmt.Sprint(wantDeletable) || fmt.Sprint(guidance) != fmt.Sprint(wantGuidance) {
		t.Fatalf("deletable=%v guidance=%v\nwant %v / %v", deletable, guidance, wantDeletable, wantGuidance)
	}
	if len(scan.Errors) != 1 || !strings.Contains(scan.Errors[0], "error-service") {
		t.Fatalf("errors = %v", scan.Errors)
	}
	for name, want := range map[string]string{
		"dscinitialization-v1-validator.opendatahub.io-gone":     "its CSV rhods-operator.3.5.0 no longer exists",
		"dscinitialization-v2-validator.opendatahub.io-x":        "still exists, so OLM recreates it",
		"inferenceservice.serving.kserve.io":                     "module kserve is installed",
		"odh-notebook-controller-mutating-webhook-configuration": "module workbenches has no CR",
		"legacy.opendatahub.io":                                  "cannot tell which operator owns it",
	} {
		if !strings.Contains(got[name].Reason, want) {
			t.Errorf("%s: reason %q lacks %q", name, got[name].Reason, want)
		}
	}
	assertWrites(t, f)
}

// No false positives during an operator install or upgrade: nothing is
// evaluated, so nothing is reported or deleted (§3.4 rule 3).
func TestStaleWebhookScan_NothingDuringAnInstall(t *testing.T) {
	for _, phase := range []string{"Pending", "InstallReady", "Installing", "Replacing"} {
		t.Run(phase, func(t *testing.T) {
			f, c := newFakeAPI(t)
			serveWebhooks(f, []whFixture{{name: "x.opendatahub.io-gone", labels: olmOwned("rhods-operator.3.5.0"), services: []string{SubNS + "/gone"}}}, phase, nil, nil)
			scan := scanStaleWebhooks(c)
			if len(scan.Verdicts) != 0 || !strings.Contains(scan.Upgrading, phase) {
				t.Fatalf("scan = %+v", scan)
			}
		})
	}
}

// An unreadable CSV list means an install may be running: report, but
// delete nothing.
func TestStaleWebhookScan_CSVListErrorMakesNothingDeletable(t *testing.T) {
	f, c := newFakeAPI(t)
	serveWebhooks(f, []whFixture{{name: "x.opendatahub.io-gone", labels: olmOwned("rhods-operator.3.5.0"), services: []string{SubNS + "/gone"}}}, "Succeeded", nil, nil)
	f.status("GET", csvBase, http.StatusForbidden, "Forbidden")
	scan := scanStaleWebhooks(c)
	if len(scan.Verdicts) != 1 || scan.Verdicts[0].Deletable || !strings.Contains(scan.Verdicts[0].Reason, "could not check whether an operator install is in progress") {
		t.Fatalf("scan = %+v", scan)
	}
}

// A CSV that is being deleted is never reinstalled, so its configs count as
// orphaned (Reinstall cleans up while its own CSV deletion finishes).
func TestStaleWebhookScan_DeletingCSVCountsAsGone(t *testing.T) {
	f, c := newFakeAPI(t)
	serveWebhooks(f, []whFixture{{name: "dsci.opendatahub.io-a", labels: olmOwned("rhods-operator.3.6.0"), services: []string{SubNS + "/rhods-operator-service"}}}, "Succeeded", nil, nil)
	f.json("GET", csvBase+"/rhods-operator.3.6.0", http.StatusOK, `{"metadata":{"name":"rhods-operator.3.6.0","deletionTimestamp":"2026-10-05T10:00:00Z"}}`)
	scan := scanStaleWebhooks(c)
	if len(scan.Verdicts) != 1 || !scan.Verdicts[0].Deletable {
		t.Fatalf("scan = %+v", scan)
	}
}

// A config created moments ago may still be getting its Service.
func TestStaleWebhookScan_NewConfigIsNotDeletable(t *testing.T) {
	f, c := newFakeAPI(t)
	recent := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	serveWebhooks(f, []whFixture{{name: "x.opendatahub.io-gone", labels: olmOwned("rhods-operator.3.5.0"), services: []string{SubNS + "/gone"}, created: recent}}, "Succeeded", nil, nil)
	scan := scanStaleWebhooks(c)
	if len(scan.Verdicts) != 1 || scan.Verdicts[0].Deletable || !strings.Contains(scan.Verdicts[0].Reason, "created less than") {
		t.Fatalf("scan = %+v", scan)
	}
}

func TestServiceHealth(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour).Format(time.RFC3339)
	recent := now.Add(-time.Minute).Format(time.RFC3339)
	oldWebhookNow := webhookNow
	webhookNow = func() time.Time { return now }
	t.Cleanup(func() { webhookNow = oldWebhookNow })
	svc := `{"metadata":{"creationTimestamp":"` + old + `"},"spec":{"selector":{"app":"x"}}}`
	for _, tc := range []struct {
		name    string
		service string // "" = NotFound
		slices  string // "" = list fails
		grace   time.Duration
		want    serviceState
		exists  bool
	}{
		{name: "not found", want: serviceMissing},
		{name: "ready endpoint", service: svc, slices: `{"items":[{"endpoints":[{"conditions":{"ready":true}}]}]}`, grace: 5 * time.Minute, want: serviceServing, exists: true},
		{name: "nil ready means ready", service: svc, slices: `{"items":[{"endpoints":[{"conditions":{}}]}]}`, grace: 5 * time.Minute, want: serviceServing, exists: true},
		{name: "not ready for an hour", service: svc, slices: `{"items":[{"metadata":{"creationTimestamp":"` + old + `"},"endpoints":[{"conditions":{"ready":false}}]}]}`, grace: 5 * time.Minute, want: serviceNoEndpoints, exists: true},
		{name: "no slices for an hour", service: svc, slices: `{"items":[]}`, grace: 5 * time.Minute, want: serviceNoEndpoints, exists: true},
		{name: "endpoints changed a minute ago (restart)", service: svc, slices: `{"items":[{"metadata":{"creationTimestamp":"` + old + `","annotations":{"endpoints.kubernetes.io/last-change-trigger-time":"` + recent + `"}},"endpoints":[{"conditions":{"ready":false}}]}]}`, grace: 5 * time.Minute, want: serviceServing, exists: true},
		{name: "managedFields updated a minute ago", service: svc, slices: `{"items":[{"metadata":{"creationTimestamp":"` + old + `","managedFields":[{"time":"` + recent + `"}]},"endpoints":[]}]}`, grace: 5 * time.Minute, want: serviceServing, exists: true},
		{name: "no grace: down at once", service: svc, slices: `{"items":[{"metadata":{"managedFields":[{"time":"` + recent + `"}]},"endpoints":[{"conditions":{"ready":false}}]}]}`, want: serviceNoEndpoints, exists: true},
		{name: "unknown age within grace", service: `{"spec":{"selector":{"app":"x"}}}`, slices: `{"items":[]}`, grace: 5 * time.Minute, want: serviceServing, exists: true},
		{name: "ExternalName", service: `{"spec":{"type":"ExternalName"}}`, grace: 5 * time.Minute, want: serviceServing, exists: true},
		{name: "endpoints unreadable", service: svc, want: serviceUnknown, exists: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFakeAPI(t)
			if tc.service != "" {
				f.json("GET", svcPath("ns", "svc"), http.StatusOK, tc.service)
			}
			if tc.slices != "" {
				f.handle("GET", fmt.Sprintf(epsPathFmt, "ns"), func(r *http.Request, _ []byte) (int, string) {
					if r.URL.Query().Get("labelSelector") != "kubernetes.io/service-name=svc" {
						t.Errorf("endpoint slices listed with %q", r.URL.RawQuery)
					}
					return http.StatusOK, tc.slices
				})
			} else {
				f.status("GET", fmt.Sprintf(epsPathFmt, "ns"), http.StatusForbidden, "Forbidden")
			}
			h := checkServiceHealth(c, "ns/svc", tc.grace)
			if h.state != tc.want || h.exists != tc.exists {
				t.Fatalf("health = %+v, want state %v exists %v", h, tc.want, tc.exists)
			}
		})
	}
}

// A Service whose endpoints cannot be listed (e.g. RBAC) keeps its
// configuration: nothing is deleted. The scan reports it, so the check
// says it could not verify the webhook instead of passing (R1-6).
func TestStaleWebhookScan_UnreadableEndpointsKeepTheConfig(t *testing.T) {
	f, c := newFakeAPI(t)
	serveWebhooks(f, []whFixture{{name: "x.opendatahub.io-a", labels: olmOwned("rhods-operator.3.5.0"), services: []string{SubNS + "/svc"}}}, "Succeeded", nil, nil)
	f.json("GET", svcPath(SubNS, "svc"), http.StatusOK, `{"spec":{"selector":{"app":"x"}}}`)
	f.status("GET", fmt.Sprintf(epsPathFmt, SubNS), http.StatusForbidden, "Forbidden")
	scan := scanStaleWebhooks(c)
	if len(scan.Verdicts) != 0 || len(scan.Errors) != 1 || !strings.Contains(scan.Errors[0], "list endpoints of Service "+SubNS+"/svc") {
		t.Fatalf("scan = %+v", scan)
	}
	out := checkStaleWebhooks(c)
	if out.check.Status != "warn" || !strings.Contains(out.check.Detail, "could not verify") {
		t.Fatalf("check = %+v", out.check)
	}
	assertWrites(t, f)
}

// Reinstall uses the same rules and deletes with preconditions; a kept
// config is logged with its reason, and the step never fails the reinstall.
func TestReinstallWebhookCleanup_DeletesOnlyStaleConfigsWithPreconditions(t *testing.T) {
	f, c := newFakeAPI(t)
	serveWebhooks(f, []whFixture{
		{name: "dsci.opendatahub.io-gone", labels: olmOwned("rhods-operator.3.5.0"), services: []string{SubNS + "/old-operator-service"}},
		{name: "legacy.opendatahub.io", services: []string{"redhat-ods-applications/gone-legacy"}},
		{name: "kuberay-mutating-webhook-configuration", kind: "m", labels: map[string]string{"platform.opendatahub.io/part-of": "ray"}, services: []string{"redhat-ods-applications/kuberay-webhook-service"}},
	}, "Succeeded", nil, map[string]bool{"Ray": false})
	f.json("DELETE", vwcPath+"/dsci.opendatahub.io-gone", http.StatusOK, `{}`)
	f.status("DELETE", mwcPath+"/kuberay-mutating-webhook-configuration", http.StatusInternalServerError, "InternalError")

	var logs []string
	removed := cleanupStaleWebhooksForReinstall(c, &logs)
	if removed != 1 {
		t.Fatalf("removed = %d, logs = %v", removed, logs)
	}
	assertWrites(t, f, "DELETE "+vwcPath+"/dsci.opendatahub.io-gone", "DELETE "+mwcPath+"/kuberay-mutating-webhook-configuration")
	body := f.requests("DELETE", vwcPath+"/dsci.opendatahub.io-gone")[0].Body
	if !strings.Contains(body, `"uid":"uid-dsci.opendatahub.io-gone"`) || !strings.Contains(body, `"resourceVersion":"1"`) {
		t.Fatalf("delete without preconditions: %s", body)
	}
	text := strings.Join(logs, "\n")
	for _, want := range []string{"Removed 1 stale webhook", "Kept ValidatingWebhookConfiguration legacy.opendatahub.io", "could not delete MutatingWebhookConfiguration kuberay"} {
		if !strings.Contains(text, want) {
			t.Errorf("logs lack %q:\n%s", want, text)
		}
	}
}

func TestReinstallWebhookCleanup_SkippedDuringAnInstall(t *testing.T) {
	f, c := newFakeAPI(t)
	serveWebhooks(f, []whFixture{{name: "dsci.opendatahub.io-gone", labels: olmOwned("rhods-operator.3.5.0"), services: []string{SubNS + "/gone"}}}, "Installing", nil, nil)
	var logs []string
	if removed := cleanupStaleWebhooksForReinstall(c, &logs); removed != 0 || !strings.Contains(strings.Join(logs, "\n"), "install is in progress") {
		t.Fatalf("removed=%d logs=%v", removed, logs)
	}
	assertWrites(t, f)
}

func TestIsRHOAIWebhook(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]interface{}
		want   bool
	}{
		{"datasciencecluster-v2-validator.opendatahub.io-x", map[string]interface{}{"olm.owner": "rhods-operator.3.6.0", "olm.owner.namespace": SubNS}, true},
		{"anything", map[string]interface{}{"olm.owner": "rhods-operator.3.6.0"}, true},
		{"x.opendatahub.io", map[string]interface{}{"olm.owner": "rhods-operator.3.6.0", "olm.owner.namespace": "elsewhere"}, false},
		{"x.opendatahub.io", map[string]interface{}{"olm.owner": "authorino-operator.v1"}, false},
		{"rhods-thing", map[string]interface{}{"olm.owner": "not-rhods-operator.v1"}, false},
		{"legacy.opendatahub.io", nil, true},
		{"kuberay-mutating-webhook-configuration", nil, false},
	} {
		if got := isRHOAIWebhook(tc.name, tc.labels); got != tc.want {
			t.Errorf("isRHOAIWebhook(%q, %v) = %v, want %v", tc.name, tc.labels, got, tc.want)
		}
	}
}

// Live case: odh-observability-webhook (part-of=platform, owned by the
// Platform CR, failurePolicy Ignore) whose pods cannot start. It is reported
// as information with the real owner, never offered for deletion.
func TestStaleWebhookCheck_PlatformWebhookWithIgnoreIsInfo(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	oldNow := webhookNow
	webhookNow = func() time.Time { return now }
	t.Cleanup(func() { webhookNow = oldNow })
	f, c := newFakeAPI(t)
	serveWebhooks(f, []whFixture{{name: "odh-observability-webhook", kind: "m", labels: map[string]string{"platform.opendatahub.io/part-of": "platform"},
		services: []string{"redhat-ods-applications/odh-observability-webhook"}, ignore: true}}, "Succeeded", nil, nil)
	f.json("GET", svcPath("redhat-ods-applications", "odh-observability-webhook"), http.StatusOK, `{"metadata":{"creationTimestamp":"2026-10-03T00:00:00Z"},"spec":{"selector":{"app":"odh-observability"}}}`)
	f.json("GET", fmt.Sprintf(epsPathFmt, "redhat-ods-applications"), http.StatusOK, `{"items":[{"metadata":{"creationTimestamp":"2026-10-03T00:00:00Z"},"endpoints":[{"conditions":{"ready":false}}]}]}`)
	out := checkStaleWebhooks(c)
	if len(out.problems) != 1 {
		t.Fatalf("problems = %+v", out.problems)
	}
	p := out.problems[0]
	ev := strings.Join(p.Evidence, "\n")
	if p.ID != "webhook-service-missing" || p.Severity != "info" || p.AutoFixable || !strings.Contains(ev, "has no ready endpoints") ||
		!strings.Contains(ev, "failurePolicy Ignore") || !strings.Contains(ev, "for the whole platform") {
		t.Fatalf("problem = %+v", p)
	}
	assertWrites(t, f)
}

// A configuration that mixes a dead Service-backed webhook with a URL-backed
// one is never stale: the URL webhook may be serving, and deleting the
// configuration would remove it too (R1-1). The same configuration without
// the URL hook is deletable, so the URL hook is what keeps it.
func TestStaleWebhookScan_URLBackedHookKeepsTheConfig(t *testing.T) {
	gone := SubNS + "/old-operator-service"
	f, c := newFakeAPI(t)
	serveWebhooks(f, []whFixture{
		{name: "mixed-url.opendatahub.io-a", labels: olmOwned("rhods-operator.3.5.0"), services: []string{gone}, urls: []string{"https://validator.example.com/validate"}},
		{name: "url-only.opendatahub.io-a", labels: olmOwned("rhods-operator.3.5.0"), urls: []string{"https://validator.example.com/validate"}},
		{name: "service-only.opendatahub.io-a", labels: olmOwned("rhods-operator.3.5.0"), services: []string{gone}},
	}, "Succeeded", nil, nil)
	f.json("DELETE", vwcPath+"/service-only.opendatahub.io-a", http.StatusOK, `{}`)
	scan := scanStaleWebhooks(c)
	got := verdictsByName(scan)
	if _, ok := got["mixed-url.opendatahub.io-a"]; ok {
		t.Fatalf("a config with a URL-backed webhook must not be stale: %+v", got["mixed-url.opendatahub.io-a"])
	}
	if _, ok := got["url-only.opendatahub.io-a"]; ok {
		t.Fatal("a URL-only config must not be stale")
	}
	if v, ok := got["service-only.opendatahub.io-a"]; !ok || !v.Deletable {
		t.Fatalf("control: the Service-only config should be deletable: %+v", got)
	}
	d := deleteStaleWebhookConfigs(c, scan.Verdicts)
	if len(d.Deleted) != 1 || d.Deleted[0] != "ValidatingWebhookConfiguration service-only.opendatahub.io-a" {
		t.Fatalf("deleted = %v", d.Deleted)
	}
}

// A part-of value that maps to no served component kind, or a component API
// that is not served at all, means the producer is unknown: reported, never
// deletable (R5-F6).
func TestStaleWebhookScan_UnknownModuleIsNeverDeletable(t *testing.T) {
	cfg := whFixture{name: "mystery-webhook", kind: "m", labels: map[string]string{"platform.opendatahub.io/part-of": "notamodule"}, services: []string{"redhat-ods-applications/gone"}}
	t.Run("unmapped part-of", func(t *testing.T) {
		f, c := newFakeAPI(t)
		serveWebhooks(f, []whFixture{cfg}, "Succeeded", nil, map[string]bool{"Ray": false})
		v, ok := verdictsByName(scanStaleWebhooks(c))["mystery-webhook"]
		if !ok || v.Deletable || !strings.Contains(v.Reason, "cannot tell whether module notamodule is installed") {
			t.Fatalf("verdict = %+v", v)
		}
	})
	t.Run("component API not served", func(t *testing.T) {
		f, c := newFakeAPI(t)
		ray := cfg
		ray.labels = map[string]string{"platform.opendatahub.io/part-of": "ray"}
		serveWebhooks(f, []whFixture{ray}, "Succeeded", nil, nil)
		f.status("GET", "/apis/components.platform.opendatahub.io", http.StatusNotFound, "NotFound")
		v, ok := verdictsByName(scanStaleWebhooks(c))["mystery-webhook"]
		if !ok || v.Deletable {
			t.Fatalf("verdict = %+v", v)
		}
	})
}
