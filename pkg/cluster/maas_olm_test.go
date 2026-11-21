package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// A small OLM for the MaaS tests: catalogs by package, and on every read of
// a Subscription OLM takes one step, as the real one would between polls:
// it installs the channel's head, or upgrades to it when the head has an
// edge from the installed CSV, else it stays (AtLatestKnown).

type fakeChannel struct {
	head    string          // version of the channel's head
	from    map[string]bool // versions the head upgrades from (replaces/skipRange)
	entries []string        // versions the channel lists
}

type fakePackage struct {
	source, defaultChannel string
	channels               map[string]fakeChannel
	order                  []string
	// installModes the channels' heads support (none: the catalog does
	// not say).
	installModes []string
}

// rhclFakePackage is the RHCL catalog as seen live on 2026-11-18
// (rh-api-management/rhcl-file-based-catalog): stable at 1.4.3; stable-1.4
// with 1.4.4 replacing 1.4.3; stable-1.5's 1.5.0 skipping ">=1.4.4 <1.5.0".
func rhclFakePackage() fakePackage {
	return fakePackage{source: "redhat-operators", defaultChannel: "stable", order: []string{"stable", "stable-1.4", "stable-1.5"}, channels: map[string]fakeChannel{
		"stable":     {head: "1.4.3", entries: []string{"1.4.0", "1.4.3"}, from: map[string]bool{"1.4.0": true}},
		"stable-1.4": {head: "1.4.4", entries: []string{"1.4.3", "1.4.4"}, from: map[string]bool{"1.4.3": true}},
		"stable-1.5": {head: "1.5.0", entries: []string{"1.5.0"}, from: map[string]bool{"1.4.4": true}},
	}}
}

func cnpgFakePackage() fakePackage {
	return fakePackage{source: "certified-operators", defaultChannel: "stable-v1", order: []string{"stable-v1"}, channels: map[string]fakeChannel{
		"stable-v1": {head: "1.27.1", entries: []string{"1.27.1"}},
	}}
}

// rhclCatalog is rhclFakePackage as the code reads it.
func rhclCatalog() operatorPackage {
	var out operatorPackage
	_ = json.Unmarshal([]byte(packageManifestStatus("rhcl-operator", rhclFakePackage())), &struct {
		DefaultChannel *string           `json:"defaultChannel"`
		Channels       *[]packageChannel `json:"channels"`
	}{&out.defaultChannel, &out.channels})
	return out
}

func packageManifestStatus(pkg string, p fakePackage) string {
	var chans []any
	for _, name := range p.order {
		ch := p.channels[name]
		var entries []any
		for _, v := range ch.entries {
			entries = append(entries, map[string]any{"name": pkg + ".v" + v, "version": v})
		}
		desc := map[string]any{"version": ch.head}
		if len(p.installModes) > 0 {
			var modes []any
			for _, m := range []string{"OwnNamespace", "SingleNamespace", "MultiNamespace", "AllNamespaces"} {
				modes = append(modes, map[string]any{"type": m, "supported": slices.Contains(p.installModes, m)})
			}
			desc["installModes"] = modes
		}
		chans = append(chans, map[string]any{"name": name, "currentCSV": pkg + ".v" + ch.head, "currentCSVDesc": desc, "entries": entries})
	}
	raw, _ := json.Marshal(map[string]any{"packageName": pkg, "catalogSource": p.source, "catalogSourceNamespace": "openshift-marketplace", "defaultChannel": p.defaultChannel, "channels": chans})
	return string(raw)
}

// fakeOLMSim simulates OLM on a resourceFake.
type fakeOLMSim struct {
	f        *resourceFake
	packages map[string]fakePackage
	// onInstalled runs (under the fake's lock) when a CSV of pkg is
	// installed or upgraded to.
	onInstalled func(pkg, ns, from, to string)
}

func newFakeOLMSim(f *resourceFake) *fakeOLMSim {
	s := &fakeOLMSim{f: f, packages: map[string]fakePackage{"rhcl-operator": rhclFakePackage(), "cloudnative-pg": cnpgFakePackage()}}
	for name, p := range s.packages {
		var status map[string]any
		_ = json.Unmarshal([]byte(packageManifestStatus(name, p)), &status)
		f.put("/apis/packages.operators.coreos.com/v1/namespaces/openshift-marketplace/packagemanifests/"+name, map[string]any{
			"metadata": map[string]any{"labels": map[string]any{"catalog": p.source}}, "status": status})
	}
	return s
}

// csvLocked stores an installed CSV of pkg at version in ns.
func (s *fakeOLMSim) csvLocked(pkg, ns, version string) {
	s.f.objects[fakeKey{gv: "operators.coreos.com/v1alpha1", ns: ns, plural: "clusterserviceversions", name: pkg + ".v" + version}] = map[string]any{
		"metadata": map[string]any{"name": pkg + ".v" + version, "namespace": ns, "uid": "csv-" + version, "labels": map[string]any{"operators.coreos.com/" + pkg + "." + ns: ""}},
		"spec": map[string]any{"install": map[string]any{"spec": map[string]any{"deployments": []any{map[string]any{
			"name": "kuadrant-operator-controller-manager",
			"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{
				"name": "manager", "resources": map[string]any{"limits": map[string]any{"cpu": "200m", "memory": "500Mi"}, "requests": map[string]any{"cpu": "200m", "memory": "350Mi"}},
			}}}}},
		}}}}},
		"status": map[string]any{"phase": "Succeeded"},
	}
}

// installed puts an installed operator in place: its CSV and a
// Subscription by name, not this tool's.
func (s *fakeOLMSim) installed(pkg, ns, subName, channel, version string) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	s.csvLocked(pkg, ns, version)
	p := s.packages[pkg]
	spec := map[string]any{"channel": channel, "name": pkg, "source": p.source, "sourceNamespace": "openshift-marketplace", "installPlanApproval": "Automatic"}
	if pkg == "rhcl-operator" {
		// Installed as the docs recommend: rate limits fail closed.
		spec["config"] = map[string]any{"env": []any{
			map[string]any{"name": "RATELIMIT_CHECK_SERVICE_FAILURE_MODE", "value": "deny"},
			map[string]any{"name": "RATELIMIT_REPORT_SERVICE_FAILURE_MODE", "value": "deny"},
		}}
	}
	s.f.objects[fakeKey{gv: "operators.coreos.com/v1alpha1", ns: ns, plural: "subscriptions", name: subName}] = map[string]any{
		"metadata": map[string]any{"name": subName, "namespace": ns, "uid": "sub-" + subName, "resourceVersion": "1"},
		"spec":     spec,
		"status":   map[string]any{"state": "AtLatestKnown", "installedCSV": pkg + ".v" + version, "currentCSV": pkg + ".v" + version},
	}
}

// step is OLM's move on a Subscription read, by name or in a namespace's
// list (call from beforeServe).
func (s *fakeOLMSim) step(method, path string) {
	if method != http.MethodGet || !strings.Contains(path, "/subscriptions") {
		return
	}
	k, isList := parseFakePath(path)
	if isList && k.ns == "" {
		return
	}
	s.f.mu.Lock()
	var keys []fakeKey
	for key := range s.f.objects {
		if key.plural == "subscriptions" && key.ns == k.ns && (isList || key.name == k.name) {
			keys = append(keys, key)
		}
	}
	s.f.mu.Unlock()
	for _, key := range keys {
		s.stepOne(key)
	}
}

func (s *fakeOLMSim) stepOne(k fakeKey) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	sub := s.f.objects[k]
	if sub == nil {
		return
	}
	spec := ensureMap(sub, "spec")
	status := ensureMap(sub, "status")
	pkg, _ := spec["name"].(string)
	channel, _ := spec["channel"].(string)
	ch, ok := s.packages[pkg].channels[channel]
	if !ok {
		status["state"] = "UpgradePending"
		status["conditions"] = []any{map[string]any{"type": "ResolutionFailed", "status": "True", "message": "no channel " + channel}}
		return
	}
	installed, _ := status["installedCSV"].(string)
	from := csvVersion(installed)
	to := ""
	switch {
	case installed == "":
		to = ch.head
	case from != ch.head && ch.from[from]:
		to = ch.head
		delete(s.f.objects, fakeKey{gv: "operators.coreos.com/v1alpha1", ns: k.ns, plural: "clusterserviceversions", name: installed})
	}
	if to != "" {
		s.csvLocked(pkg, k.ns, to)
		status["installedCSV"] = pkg + ".v" + to
		if s.onInstalled != nil {
			s.onInstalled(pkg, k.ns, from, to)
		}
	}
	status["currentCSV"] = status["installedCSV"]
	status["state"] = "AtLatestKnown"
}

// newFreshMaaSCluster is a cluster that never had MaaS: RHOAI with its DSC
// (aigateway Removed, as the rhoai-3.6 sample has it), and nothing else:
// no Connectivity Link, no Kuadrant, no CloudNativePG, no infrastructure
// namespace. OLM installs what is subscribed; Kuadrant makes Authorino as
// RHCL does (without the TLS settings); the operator answers MaaS Managed.
func newFreshMaaSCluster(t *testing.T) (*modelsCluster, *fakeOLMSim) {
	t.Helper()
	mc := newMaaSCluster(t)
	f := mc.f
	f.mu.Lock()
	for k := range f.objects {
		if strings.HasPrefix(k.gv, "kuadrant.io") || k.gv == "operator.authorino.kuadrant.io/v1beta1" || k.ns == "kuadrant-system" || k.name == "redhat-ai-gateway-infra" ||
			k.name == kuadrantController || k.name == cnpgController {
			delete(f.objects, k)
		}
	}
	f.groups = map[string]string{}
	f.mu.Unlock()
	f.putJSON(tMaaSDSC, `{"spec":{"components":{"kserve":{"managementState":"Managed"},"aigateway":{"managementState":"Removed","modelsAsAService":{"managementState":"Removed"},"batchGateway":{"managementState":"Removed"}}}}}`)
	f.putJSON("/api/v1/namespaces/openshift-operators", `{}`)
	f.putJSON("/apis/operators.coreos.com/v1/namespaces/openshift-operators/operatorgroups/global-operators", `{"spec":{}}`)
	// Not served until installed: the API server answers 404.
	f.fail["GET /apis/kuadrant.io/v1beta1/kuadrants"] = 404
	sim := newFakeOLMSim(f)
	sim.onInstalled = func(pkg, ns, from, to string) {
		switch pkg {
		case "rhcl-operator":
			f.objects[fakeKey{gv: "apps/v1", ns: ns, plural: "deployments", name: kuadrantController}] = map[string]any{"metadata": map[string]any{"name": kuadrantController}}
			f.groups["/apis/kuadrant.io"] = `{"kind":"APIGroup","name":"kuadrant.io","versions":[{"groupVersion":"kuadrant.io/v1","version":"v1"},{"groupVersion":"kuadrant.io/v1beta1","version":"v1beta1"}],"preferredVersion":{"groupVersion":"kuadrant.io/v1","version":"v1"}}`
			delete(f.fail, "GET /apis/kuadrant.io/v1beta1/kuadrants")
		case "cloudnative-pg":
			f.objects[fakeKey{gv: "apps/v1", ns: ns, plural: "deployments", name: cnpgController}] = map[string]any{"metadata": map[string]any{"name": cnpgController}}
			f.groups["/apis/postgresql.cnpg.io"] = `{"kind":"APIGroup","name":"postgresql.cnpg.io","versions":[{"groupVersion":"postgresql.cnpg.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"postgresql.cnpg.io/v1","version":"v1"}}`
		}
	}
	f.beforeServe = func(method, path string) {
		sim.step(method, path)
		if method == http.MethodGet && strings.Contains(path, "/kuadrants") {
			kuadrantMakesAuthorino(f)
		}
	}
	// The operator answers the DataScienceCluster: MaaS Managed brings
	// maas-api, the tenant and the gateway's AuthPolicy; MaaS Removed takes
	// maas-api away.
	prevPatch := f.onMergePatch
	f.onMergePatch = func(r *http.Request, k fakeKey, patch, next map[string]any) {
		prevPatch(r, k, patch, next)
		if k.plural != "datascienceclusters" || r.URL.Query().Get("dryRun") != "" {
			return
		}
		ag := ensureMap(ensureMap(ensureMap(next, "spec"), "components"), "aigateway")
		maas, _ := ensureMap(ag, "modelsAsAService")["managementState"].(string)
		api := fakeKey{gv: "apps/v1", ns: "redhat-ai-gateway-infra", plural: "deployments", name: "maas-api"}
		if maas != "Managed" {
			delete(f.objects, api)
			return
		}
		f.objects[api] = map[string]any{"metadata": map[string]any{"name": "maas-api", "uid": "api"}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Available", "status": "True"}}}}
		f.objects[fakeKey{gv: "maas.opendatahub.io/v1alpha1", ns: "ai-tenants", plural: "aitenants", name: "models-as-a-service"}] = map[string]any{"metadata": map[string]any{"name": "models-as-a-service"}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}
		f.objects[fakeKey{gv: "maas.opendatahub.io/v1alpha1", ns: "models-as-a-service", plural: "maastenantconfigs", name: "default-tenant"}] = map[string]any{"metadata": map[string]any{"name": "default-tenant"}, "status": map[string]any{"infraNamespace": "redhat-ai-gateway-infra", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}
		f.objects[fakeKey{gv: "kuadrant.io/v1", ns: "openshift-ingress", plural: "authpolicies", name: "maas-gateway-auth"}] = map[string]any{"metadata": map[string]any{"name": "maas-gateway-auth", "labels": map[string]any{"app.kubernetes.io/managed-by": "maas-controller"}},
			"spec": map[string]any{"targetRef": map[string]any{"kind": "Gateway", "name": "maas-default-gateway"}}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Enforced", "status": "True"}}}}
	}
	return mc, sim
}

// kuadrantMakesAuthorino: a Kuadrant CR is Ready and has made Authorino's
// CR, Service and Deployment, as RHCL makes them (no TLS settings).
func kuadrantMakesAuthorino(f *resourceFake) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, obj := range f.objects {
		if k.plural != "kuadrants" {
			continue
		}
		ensureMap(obj, "status")["conditions"] = []any{map[string]any{"type": "Ready", "status": "True"}}
		cr := fakeKey{gv: "operator.authorino.kuadrant.io/v1beta1", ns: k.ns, plural: "authorinos", name: "authorino"}
		if f.objects[cr] != nil {
			continue
		}
		f.objects[cr] = map[string]any{"metadata": map[string]any{"name": "authorino", "uid": "authorino-cr", "resourceVersion": "1"}, "spec": map[string]any{"listener": map[string]any{}}}
		f.objects[fakeKey{gv: "v1", ns: k.ns, plural: "services", name: "authorino-authorino-authorization"}] = map[string]any{"metadata": map[string]any{"name": "authorino-authorino-authorization", "uid": "svc", "resourceVersion": "1"}}
		f.objects[fakeKey{gv: "apps/v1", ns: k.ns, plural: "deployments", name: "authorino"}] = map[string]any{"metadata": map[string]any{"name": "authorino", "uid": "dep", "resourceVersion": "1"},
			"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "authorino"}}}}}}
	}
}

// maasClusterWithModel is newMaaSCluster, set up, with the default model
// of this tool running in project proj.
func maasClusterWithModel(t *testing.T) *modelsCluster {
	t.Helper()
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("model: %+v", res)
	}
	return mc
}

func readRecord(t *testing.T, c *Client) maasRecord {
	t.Helper()
	rec, err := readMaaSRecord(c)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// A cluster that never had MaaS: Set up lists every change, then does them
// all in the docs' order: Connectivity Link and its Kuadrant CR,
// CloudNativePG, the infrastructure namespace and the database, the
// gateway, Authorino TLS, the DataScienceCluster.
func TestMaaSSetup_FreshClusterDoesEveryStep(t *testing.T) {
	mc, _ := newFreshMaaSCluster(t)
	st, err := GetMaaSStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	if st.Platform.SetupBlockedReason != "" || st.Blocked != nil {
		t.Fatalf("blocked: %+v %+v", st.Platform, st.Blocked)
	}
	// The dialog shows the chosen database's plan next to Set up's.
	plan := strings.Join(append(st.SetupPlan, st.Database.Choices[2].Plan...), "\n")
	for _, want := range []string{"Install Red Hat Connectivity Link 1.5.0", "channel stable-1.5", "Namespace kuadrant-system, OperatorGroup kuadrant-system/kuadrant-operator-group, Subscription kuadrant-system/kuadrant-operator",
		"Create the Kuadrant CR kuadrant-system/kuadrant", "Install CloudNativePG 1.27.1: Namespace cnpg-system, OperatorGroup cnpg-system/cloudnative-pg, Subscription cnpg-system/cloudnative-pg", "RATELIMIT_CHECK_SERVICE_FAILURE_MODE=deny and RATELIMIT_REPORT_SERVICE_FAILURE_MODE=deny", "Create namespace redhat-ai-gateway-infra",
		"maas-db", "Create Gateway openshift-ingress/maas-default-gateway", "Configure Authorino TLS", "aigateway.managementState Removed to Managed", "aigateway.modelsAsAService.managementState Removed to Managed"} {
		if !strings.Contains(plan, want) {
			t.Errorf("the plan lacks %q:\n%s", want, plan)
		}
	}
	for _, id := range []string{"rhcl", "kuadrant", "database", "gateway", "authorino-tls", "dsc"} {
		if p := maasPart(t, st, id); p.State != "missing" {
			t.Errorf("part %s = %+v", id, p)
		}
	}
	res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG})
	if !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	if got := finalSteps(events); got != "check=success rhcl=success kuadrant=success cnpg=success database=success gateway=success authorino-tls=success dsc=success ready=success" {
		t.Fatalf("steps = %s", got)
	}
	muts := mc.f.mutations()
	last := -1
	for _, want := range []string{
		"POST /api/v1/namespaces", "POST /apis/operators.coreos.com/v1/namespaces/kuadrant-system/operatorgroups", "POST /apis/operators.coreos.com/v1alpha1/namespaces/kuadrant-system/subscriptions",
		"POST /apis/kuadrant.io/v1beta1/namespaces/kuadrant-system/kuadrants", "POST /api/v1/namespaces", "POST /apis/operators.coreos.com/v1/namespaces/cnpg-system/operatorgroups",
		"POST /apis/operators.coreos.com/v1alpha1/namespaces/cnpg-system/subscriptions",
		"POST /apis/postgresql.cnpg.io/v1/namespaces/redhat-ai-gateway-infra/clusters", "POST /apis/gateway.networking.k8s.io/v1/namespaces/openshift-ingress/gateways",
		"PATCH /api/v1/namespaces/kuadrant-system/services/authorino-authorino-authorization", "PATCH /apis/operator.authorino.kuadrant.io/v1beta1/namespaces/kuadrant-system/authorinos/authorino",
		"PATCH /apis/apps/v1/namespaces/kuadrant-system/deployments/authorino", "PATCH " + tMaaSDSC,
	} {
		i := -1
		for j := last + 1; j < len(muts); j++ {
			if strings.HasPrefix(muts[j], want) {
				i = j
				break
			}
		}
		if i < 0 {
			t.Fatalf("%q not after the previous write in %v", want, muts)
		}
		last = i
	}
	// The docs' objects.
	sub := mc.f.get("/apis/operators.coreos.com/v1alpha1/namespaces/kuadrant-system/subscriptions/kuadrant-operator")
	if spec := sub["spec"].(map[string]any); spec["name"] != "rhcl-operator" || spec["channel"] != "stable-1.5" || spec["source"] != "redhat-operators" || spec["installPlanApproval"] != "Automatic" {
		t.Fatalf("subscription spec = %v", spec)
	}
	// The docs' fail-closed rate limiting, and nothing else in config.
	if raw, _ := json.Marshal(sub["spec"].(map[string]any)["config"]); string(raw) != `{"env":[{"name":"RATELIMIT_CHECK_SERVICE_FAILURE_MODE","value":"deny"},{"name":"RATELIMIT_REPORT_SERVICE_FAILURE_MODE","value":"deny"}]}` {
		t.Fatalf("subscription config = %s", raw)
	}
	// CloudNativePG in its own namespace, never openshift-operators (an
	// install there can pull an OSSM upgrade that breaks the gateway).
	cnpgSub := mc.f.get("/apis/operators.coreos.com/v1alpha1/namespaces/cnpg-system/subscriptions/cloudnative-pg")
	if spec := cnpgSub["spec"].(map[string]any); spec["name"] != "cloudnative-pg" || spec["source"] != "certified-operators" || spec["channel"] != "stable-v1" || spec["config"] != nil {
		t.Fatalf("cnpg subscription = %v", cnpgSub)
	}
	if og := mc.f.get("/apis/operators.coreos.com/v1/namespaces/cnpg-system/operatorgroups/cloudnative-pg"); og == nil || og["spec"] != nil {
		t.Fatalf("cnpg operator group = %v", og)
	}
	for _, m := range muts {
		if strings.Contains(m, "/namespaces/openshift-operators/") {
			t.Fatalf("wrote into openshift-operators: %s", m)
		}
	}
	if og := mc.f.get("/apis/operators.coreos.com/v1/namespaces/kuadrant-system/operatorgroups/kuadrant-operator-group"); og == nil || og["spec"] != nil {
		t.Fatalf("operator group = %v", og)
	}
	if ns := mc.f.get("/api/v1/namespaces/redhat-ai-gateway-infra"); ns == nil || ns["metadata"].(map[string]any)["labels"].(map[string]any)[kube.ManagedByLabelKey] != kube.ManagedByLabelValue {
		t.Fatalf("infra namespace = %v", ns)
	}
	svc := mc.f.get("/api/v1/namespaces/kuadrant-system/services/authorino-authorino-authorization")
	if svc["metadata"].(map[string]any)["annotations"].(map[string]any)[servingCertAnnotation] != "authorino-server-cert" {
		t.Fatalf("service = %v", svc)
	}
	dep, _ := json.Marshal(mc.f.get("/apis/apps/v1/namespaces/kuadrant-system/deployments/authorino"))
	if !strings.Contains(string(dep), `"SSL_CERT_FILE"`) || !strings.Contains(string(dep), components.RestartAnnotation) {
		t.Fatalf("authorino deployment = %s", dep)
	}
	ag := mc.f.get(tMaaSDSC)["spec"].(map[string]any)["components"].(map[string]any)["aigateway"].(map[string]any)
	if ag["managementState"] != "Managed" || ag["modelsAsAService"].(map[string]any)["managementState"] != "Managed" || ag["batchGateway"].(map[string]any)["managementState"] != "Removed" {
		t.Fatalf("aigateway = %v", ag)
	}
	rec := readRecord(t, mc.c)
	if !rec.AIGatewayChanged || rec.PreviousAIGateway != "Removed" || !rec.DSCChanged || rec.InfraNamespace != "redhat-ai-gateway-infra" {
		t.Fatalf("record = %+v", rec)
	}
	for _, k := range []string{"rhcl", "kuadrant", "cnpg", "authorino-tls"} {
		if rec.Shared[k] == "" {
			t.Errorf("record lacks shared %s: %+v", k, rec.Shared)
		}
	}
	st, _ = GetMaaSStatus(mc.c)
	if !st.Platform.Ready || len(st.SetupPlan) != 0 {
		t.Fatalf("after setup: platform %+v plan %v parts %+v", st.Platform, st.SetupPlan, st.Parts)
	}
	for _, id := range []string{"rhcl", "kuadrant", "cnpg", "authorino-tls", "database", "dsc"} {
		if p := maasPart(t, st, id); !p.ManagedByTool || p.NotManaged {
			t.Errorf("part %s is not shown as the tool's: %+v", id, p)
		}
	}

	// Remove: MaaS-only pieces go, the DSC is restored, the namespace this
	// tool created goes once empty; the shared pieces stay.
	res, _ = runMaaSOp(t, mc.c, types.MaaSRequest{Action: "teardown"})
	if !res.Success {
		t.Fatalf("teardown: %+v", res)
	}
	ag = mc.f.get(tMaaSDSC)["spec"].(map[string]any)["components"].(map[string]any)["aigateway"].(map[string]any)
	if ag["managementState"] != "Removed" || ag["modelsAsAService"].(map[string]any)["managementState"] != "Removed" {
		t.Fatalf("aigateway after removal = %v", ag)
	}
	if mc.f.has("/api/v1/namespaces/redhat-ai-gateway-infra") {
		t.Fatalf("the infrastructure namespace this tool created stays: %s", res.Message)
	}
	for _, keep := range []string{"/apis/operators.coreos.com/v1alpha1/namespaces/kuadrant-system/subscriptions/kuadrant-operator", "/apis/kuadrant.io/v1beta1/namespaces/kuadrant-system/kuadrants/kuadrant",
		"/apis/operators.coreos.com/v1alpha1/namespaces/cnpg-system/subscriptions/cloudnative-pg"} {
		if !mc.f.has(keep) {
			t.Errorf("removal deleted the shared %s", keep)
		}
	}
	if !strings.Contains(res.Message, "Connectivity Link") || !strings.Contains(res.Message, "CloudNativePG") {
		t.Fatalf("message = %s", res.Message)
	}
	rec = readRecord(t, mc.c)
	if rec.AIGatewayChanged || rec.DSCChanged || rec.InfraNamespace != "" || rec.Shared["rhcl"] == "" {
		t.Fatalf("record after removal = %+v", rec)
	}
}

// Already installed by someone else: adopted, nothing re-created, shown as
// not this tool's.
func TestMaaSSetup_AdoptsAnInstalledConnectivityLink(t *testing.T) {
	mc := newMaaSCluster(t)
	sim := newFakeOLMSim(mc.f)
	sim.installed("rhcl-operator", "kuadrant-system", "rhcl-operator", "stable-1.5", "1.5.0")
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG})
	if !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	for _, m := range mc.f.mutations() {
		if strings.Contains(m, "/subscriptions") || strings.Contains(m, "/operatorgroups") || strings.Contains(m, "/kuadrants") || strings.Contains(m, "/authorinos/") || m == "POST /api/v1/namespaces" {
			t.Fatalf("re-created or changed %s", m)
		}
	}
	st, _ := GetMaaSStatus(mc.c)
	if p := maasPart(t, st, "rhcl"); p.State != "ready" || !p.NotManaged || p.ManagedByTool || p.Warning != "" || len(p.Actions) != 0 {
		t.Fatalf("rhcl = %+v", p)
	}
	if rec := readRecord(t, mc.c); len(rec.Shared) != 0 {
		t.Fatalf("recorded someone else's pieces: %+v", rec.Shared)
	}
}

// CloudNativePG installed as on the user's cluster (Subscription
// cnpg-system/cloudnative-pg, OperatorGroup cloudnative-pg for all
// namespaces) but its API not served yet: found by its CSV, waited for,
// never installed again.
func TestMaaSSetup_AdoptsCloudNativePGInItsOwnNamespace(t *testing.T) {
	mc, sim := newFreshMaaSCluster(t)
	sim.installed("cloudnative-pg", "cnpg-system", "cloudnative-pg", "stable-v1", "1.27.1")
	mc.f.putJSON("/apis/operators.coreos.com/v1/namespaces/cnpg-system/operatorgroups/cloudnative-pg", `{"spec":{}}`)
	st, _ := GetMaaSStatus(mc.c)
	if plan := strings.Join(st.Database.Choices[2].Plan, "\n"); !strings.Contains(plan, "Wait for OLM to finish installing CloudNativePG") || strings.Contains(plan, "Install CloudNativePG") {
		t.Fatalf("cnpg choice plan = %v", plan)
	}
	mc.f.mu.Lock()
	mc.f.groups["/apis/postgresql.cnpg.io"] = `{"kind":"APIGroup","name":"postgresql.cnpg.io","versions":[{"groupVersion":"postgresql.cnpg.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"postgresql.cnpg.io/v1","version":"v1"}}`
	mc.f.mu.Unlock()
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG})
	if !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	for _, m := range mc.f.mutations() {
		if strings.Contains(m, "/cnpg-system/") || strings.Contains(m, "/openshift-operators/") {
			t.Fatalf("changed CloudNativePG's install: %s", m)
		}
	}
	st, _ = GetMaaSStatus(mc.c)
	if p := maasPart(t, st, "cnpg"); p.State != "ready" || !p.NotManaged || !strings.Contains(p.Message, "cnpg-system") {
		t.Fatalf("cnpg = %+v", p)
	}
}

// An adopted Connectivity Link without the docs' deny-on-failure env: Set
// up leaves it alone and says so; the part offers Apply, which sets both
// and keeps the Subscription's other env entries.
func TestMaaSRHCLDenyOnFailure(t *testing.T) {
	mc := newMaaSCluster(t)
	sim := newFakeOLMSim(mc.f)
	sim.installed("rhcl-operator", "kuadrant-system", "kuadrant-operator", "stable-1.5", "1.5.0")
	subPath := "/apis/operators.coreos.com/v1alpha1/namespaces/kuadrant-system/subscriptions/kuadrant-operator"
	ensureMap(mc.f.get(subPath), "spec")["config"] = map[string]any{"env": []any{
		map[string]any{"name": "AUTH_SERVICE_TIMEOUT", "value": "2s"},
		map[string]any{"name": "RATELIMIT_CHECK_SERVICE_FAILURE_MODE", "value": "allow"},
	}}
	st, _ := GetMaaSStatus(mc.c)
	p := maasPart(t, st, "rhcl")
	if len(p.Actions) != 1 || p.Actions[0].ID != "rhcl-deny-on-failure" || !strings.Contains(p.Warning, "deny traffic on failure") || !strings.Contains(p.Commands, `"path":"/spec/config/env/-"`) {
		t.Fatalf("part = %+v", p)
	}
	if !strings.Contains(strings.Join(st.SetupPlan, "\n"), "Not changed: Subscription kuadrant-system/kuadrant-operator") {
		t.Fatalf("plan = %v", st.SetupPlan)
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	if indexOfPrefix(mc.f.mutations(), "PATCH "+subPath) >= 0 {
		t.Fatal("Set up changed a Subscription it did not create")
	}
	res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "rhcl-deny-on-failure"})
	if !res.Success || finalSteps(events) != "check=success env=success ready=success" {
		t.Fatalf("res = %+v", res)
	}
	raw, _ := json.Marshal(mc.f.get(subPath)["spec"].(map[string]any)["config"])
	if string(raw) != `{"env":[{"name":"AUTH_SERVICE_TIMEOUT","value":"2s"},{"name":"RATELIMIT_CHECK_SERVICE_FAILURE_MODE","value":"deny"},{"name":"RATELIMIT_REPORT_SERVICE_FAILURE_MODE","value":"deny"}]}` {
		t.Fatalf("config = %s", raw)
	}
	st, _ = GetMaaSStatus(mc.c)
	if p := maasPart(t, st, "rhcl"); len(p.Actions) != 0 || p.Warning != "" {
		t.Fatalf("after Apply: %+v", p)
	}
}

// RHCL 1.4.3 on channel stable: the upgrade switches to stable-1.5, sees
// OLM stay at 1.4.3 (AtLatestKnown), goes through stable-1.4 (1.4.4), then
// stable-1.5, and makes Authorino's certificate again, which the 1.4.4 step
// took by recreating Authorino's Service.
func TestMaaSUpgradeRHCL_HopsThroughTheChannelOLMOffers(t *testing.T) {
	mc := newMaaSCluster(t)
	sim := newFakeOLMSim(mc.f)
	sim.installed("rhcl-operator", "kuadrant-system", "kuadrant-operator", "stable", "1.4.3")
	sim.onInstalled = func(pkg, ns, from, to string) {
		if to == "1.4.4" {
			// authorino-operator recreates the Service; service-ca deletes
			// the old one's certificate.
			mc.f.objects[fakeKey{gv: "v1", ns: "kuadrant-system", plural: "services", name: "authorino-authorino-authorization"}] = map[string]any{"metadata": map[string]any{"name": "authorino-authorino-authorization", "uid": "svc-2", "resourceVersion": "1"}, "spec": map[string]any{"clusterIP": "None"}}
		}
	}
	mc.f.beforeServe = func(method, path string) { sim.step(method, path) }

	st, _ := GetMaaSStatus(mc.c)
	p := maasPart(t, st, "rhcl")
	if !strings.Contains(p.Warning, "1.4.3") || len(p.Actions) != 1 || p.Actions[0].ID != "upgrade-rhcl" {
		t.Fatalf("rhcl part = %+v", p)
	}
	changes := strings.Join(p.Actions[0].Changes, "\n")
	for _, want := range []string{"channel stable to stable-1.5 (version 1.5.0)", "channel stable-1.4 first, to 1.4.4", "Authorino TLS"} {
		if !strings.Contains(changes, want) {
			t.Fatalf("changes lack %q: %s", want, changes)
		}
	}
	if !strings.Contains(p.Commands, "stable-1.4") || !strings.Contains(p.Commands, "oc patch subscription") {
		t.Fatalf("commands = %s", p.Commands)
	}

	res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "upgrade-rhcl"})
	if !res.Success {
		t.Fatalf("upgrade: %+v", res)
	}
	if got := finalSteps(events); got != "check=success upgrade=success authorino-tls=success ready=success" {
		t.Fatalf("steps = %s", got)
	}
	var channels []string
	for _, r := range mc.f.requests {
		if strings.HasPrefix(r, "PATCH /apis/operators.coreos.com/v1alpha1/namespaces/kuadrant-system/subscriptions/kuadrant-operator") {
			channels = append(channels, r)
		}
	}
	sub := mc.f.get("/apis/operators.coreos.com/v1alpha1/namespaces/kuadrant-system/subscriptions/kuadrant-operator")
	if sub["spec"].(map[string]any)["channel"] != "stable-1.5" || sub["status"].(map[string]any)["installedCSV"] != "rhcl-operator.v1.5.0" || len(channels) != 3 {
		t.Fatalf("subscription = %v after %v", sub, channels)
	}
	svc := mc.f.get("/api/v1/namespaces/kuadrant-system/services/authorino-authorino-authorization")
	if ann, _ := svc["metadata"].(map[string]any)["annotations"].(map[string]any); ann[servingCertAnnotation] == nil {
		t.Fatalf("the certificate was not asked for again: %v", svc)
	}
	if !strings.Contains(res.Message, "1.5.0") {
		t.Fatalf("message = %s", res.Message)
	}
	st, _ = GetMaaSStatus(mc.c)
	if p := maasPart(t, st, "rhcl"); p.Warning != "" || len(p.Actions) != 0 || p.State != "ready" {
		t.Fatalf("after the upgrade: %+v", p)
	}
}

// AtLatestKnown with no channel leading on: the upgrade stops and says so.
func TestMaaSUpgradeRHCL_StopsWithoutAPath(t *testing.T) {
	mc := newMaaSCluster(t)
	sim := newFakeOLMSim(mc.f)
	p := sim.packages["rhcl-operator"]
	p.channels["stable-1.4"] = fakeChannel{head: "1.4.4", entries: []string{"1.4.4"}}
	sim.packages["rhcl-operator"] = p
	sim.installed("rhcl-operator", "kuadrant-system", "kuadrant-operator", "stable", "1.4.3")
	mc.f.beforeServe = func(method, path string) { sim.step(method, path) }
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "upgrade-rhcl"})
	if res.Success || !strings.Contains(res.Message, "stays at rhcl-operator.v1.4.3") {
		t.Fatalf("res = %+v", res)
	}
}

// The upgrade refuses, before any write, a Subscription it may not change.
func TestMaaSUpgradeRHCL_RefusesManualApprovalAndForeignNames(t *testing.T) {
	for _, tc := range []struct {
		name, sub string
		manual    bool
		want      string
	}{
		{"manual", "kuadrant-operator", true, "approves InstallPlans by hand"},
		{"other name", "my-rhcl", false, "not named as the MaaS docs name it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mc := newMaaSCluster(t)
			sim := newFakeOLMSim(mc.f)
			sim.installed("rhcl-operator", "kuadrant-system", tc.sub, "stable", "1.4.3")
			if tc.manual {
				ensureMap(mc.f.get("/apis/operators.coreos.com/v1alpha1/namespaces/kuadrant-system/subscriptions/"+tc.sub), "spec")["installPlanApproval"] = "Manual"
			}
			res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "upgrade-rhcl"})
			if res.Success || !strings.Contains(res.Message, tc.want) {
				t.Fatalf("res = %+v", res)
			}
			if muts := mc.f.mutations(); len(muts) != 0 {
				t.Fatalf("wrote %v", muts)
			}
		})
	}
}

// The memory limit is offered only once the operator was OOMKilled, and
// sets the CSV's resources with only the memory limit raised.
func TestMaaSRHCLMemory_OnlyWhenOOMKilled(t *testing.T) {
	mc := newMaaSCluster(t)
	sim := newFakeOLMSim(mc.f)
	sim.installed("rhcl-operator", "kuadrant-system", "kuadrant-operator", "stable-1.5", "1.5.0")
	st, _ := GetMaaSStatus(mc.c)
	if p := maasPart(t, st, "rhcl"); len(p.Actions) != 0 || p.Warning != "" {
		t.Fatalf("offered without an OOM kill: %+v", p)
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "rhcl-memory"}); res.Success || len(mc.f.mutations()) != 0 {
		t.Fatalf("ran without an OOM kill: %+v %v", res, mc.f.mutations())
	}
	mc.f.putJSON("/api/v1/namespaces/kuadrant-system/pods/kuadrant-operator-controller-manager-6d9f-x2x", `{"status":{"containerStatuses":[{"name":"manager","restartCount":3,"lastState":{"terminated":{"reason":"OOMKilled"}}}]}}`)
	st, _ = GetMaaSStatus(mc.c)
	p := maasPart(t, st, "rhcl")
	if len(p.Actions) != 1 || p.Actions[0].ID != "rhcl-memory" || !strings.Contains(p.Warning, "OOMKilled") || !strings.Contains(p.Actions[0].Changes[0], "limits cpu 200m, memory 1Gi; requests cpu 200m, memory 350Mi") {
		t.Fatalf("part = %+v", p)
	}
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "rhcl-memory"})
	if !res.Success {
		t.Fatalf("res = %+v", res)
	}
	sub := mc.f.get("/apis/operators.coreos.com/v1alpha1/namespaces/kuadrant-system/subscriptions/kuadrant-operator")
	raw, _ := json.Marshal(sub["spec"].(map[string]any)["config"])
	// The env stays: the merge patch sets resources only.
	if string(raw) != `{"env":[{"name":"RATELIMIT_CHECK_SERVICE_FAILURE_MODE","value":"deny"},{"name":"RATELIMIT_REPORT_SERVICE_FAILURE_MODE","value":"deny"}],"resources":{"limits":{"cpu":"200m","memory":"1Gi"},"requests":{"cpu":"200m","memory":"350Mi"}}}` {
		t.Fatalf("config = %s", raw)
	}
}

// Fail closed: setup and publish stop on a read that fails, before any write.
func TestMaaS_RefusesWhenAReadFails(t *testing.T) {
	for _, path := range []string{
		"GET /apis/operators.coreos.com/v1alpha1/clusterserviceversions",
		"GET /api/v1/namespaces/redhat-ai-gateway-infra",
	} {
		mc, _ := newFreshMaaSCluster(t)
		mc.f.fail[path] = 500
		res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG})
		if res.Success {
			t.Fatalf("%s: setup ran", path)
		}
		if muts := mc.f.mutations(); len(muts) != 0 {
			t.Fatalf("%s: wrote %v", path, muts)
		}
	}
	mc, _ := newFreshMaaSCluster(t)
	mc.f.fail["GET /apis/packages.operators.coreos.com/v1/namespaces/openshift-marketplace/packagemanifests"] = 500
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}); res.Success || len(mc.f.mutations()) != 0 {
		t.Fatalf("setup without the catalog: %+v %v", res, mc.f.mutations())
	}
}

// A namespace someone requested under the infrastructure namespace's name
// is still refused (the tool creates it only when it does not exist).
func TestMaaSSetup_StillRefusesAForeignInfraNamespace(t *testing.T) {
	mc, _ := newFreshMaaSCluster(t)
	mc.f.putJSON("/api/v1/namespaces/redhat-ai-gateway-infra", `{"metadata":{"annotations":{"openshift.io/requester":"mallory"}}}`)
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG})
	if res.Success || res.ErrorCode != "not_managed" || len(mc.f.mutations()) != 0 {
		t.Fatalf("res = %+v, wrote %v", res, mc.f.mutations())
	}
}

// Remove keeps the namespace this tool created while anything else is in it.
func TestMaaSTeardown_KeepsTheInfraNamespaceWhileItHoldsOthers(t *testing.T) {
	mc, _ := newFreshMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	mc.f.putJSON("/apis/apps/v1/namespaces/redhat-ai-gateway-infra/deployments/someone-elses", `{}`)
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "teardown"})
	if !res.Success || !mc.f.has("/api/v1/namespaces/redhat-ai-gateway-infra") || !strings.Contains(strings.Join(res.Logs, "\n"), "Deployment someone-elses") {
		t.Fatalf("res = %+v", res)
	}
	if rec := readRecord(t, mc.c); rec.InfraNamespace != "redhat-ai-gateway-infra" {
		t.Fatalf("the record forgot the kept namespace: %+v", rec)
	}
}

// Tier-era gateway policies: detected, the documented ones removed by name,
// maas-controller's and unknown ones kept; the gateway's authentication is
// judged by maas-controller's policy, not the first one.
func TestMaaSTierPolicies(t *testing.T) {
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	// TokenRateLimitPolicy is served at v1alpha1 only (RHCL 1.5).
	mc.f.fail["GET /apis/kuadrant.io/v1/namespaces/openshift-ingress/tokenratelimitpolicies"] = 404
	mc.f.fail["GET /apis/kuadrant.io/v1beta1/namespaces/openshift-ingress/tokenratelimitpolicies"] = 404
	target := `"spec":{"targetRef":{"kind":"Gateway","name":"maas-default-gateway"}}`
	mc.f.putJSON("/apis/kuadrant.io/v1/namespaces/openshift-ingress/authpolicies/gateway-auth-policy", `{`+target+`,"status":{"conditions":[{"type":"Enforced","status":"False"}]}}`)
	mc.f.putJSON("/apis/kuadrant.io/v1alpha1/namespaces/openshift-ingress/tokenratelimitpolicies/gateway-tier-rate-limits", `{`+target+`}`)
	mc.f.putJSON("/apis/kuadrant.io/v1/namespaces/openshift-ingress/ratelimitpolicies/custom-limit", `{`+target+`}`)
	mc.f.putJSON("/apis/kuadrant.io/v1alpha1/namespaces/openshift-ingress/tokenratelimitpolicies/gateway-default-deny", `{"metadata":{"labels":{"app.kubernetes.io/managed-by":"maas-controller"}},`+target+`}`)
	if got := maasGatewayAuth(mc.c); got != "enforced" {
		t.Fatalf("gateway auth = %s", got)
	}
	st, _ := GetMaaSStatus(mc.c)
	p := maasPart(t, st, "tier-policies")
	if st.Platform.Ready || p.State != "manual" || len(p.Actions) != 1 || len(p.Actions[0].Changes) != 2 || !strings.Contains(p.Message, "RateLimitPolicy custom-limit") || strings.Contains(p.Message, "gateway-default-deny") {
		t.Fatalf("part = %+v", p)
	}
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"})
	if res.Success || !strings.Contains(res.Message, "Old gateway policies") {
		t.Fatalf("publish = %+v", res)
	}
	res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "remove-tier-policies"})
	if !res.Success || finalSteps(events) != "check=success delete=success" {
		t.Fatalf("res = %+v", res)
	}
	if mc.f.has("/apis/kuadrant.io/v1/namespaces/openshift-ingress/authpolicies/gateway-auth-policy") || mc.f.has("/apis/kuadrant.io/v1alpha1/namespaces/openshift-ingress/tokenratelimitpolicies/gateway-tier-rate-limits") {
		t.Fatal("the old policies stay")
	}
	if !mc.f.has("/apis/kuadrant.io/v1/namespaces/openshift-ingress/ratelimitpolicies/custom-limit") || !mc.f.has("/apis/kuadrant.io/v1alpha1/namespaces/openshift-ingress/tokenratelimitpolicies/gateway-default-deny") {
		t.Fatal("deleted a policy that is not a documented tier-era one")
	}
}

// Publish: refuses on RHCL 1.4.0; with others' subscriptions, takes a
// priority below the lowest of theirs, and refuses below the docs' minimum.
func TestMaaSPublish_RHCLAndPriority(t *testing.T) {
	t.Run("1.4.0", func(t *testing.T) {
		mc := maasClusterWithModel(t)
		newFakeOLMSim(mc.f).installed("rhcl-operator", "kuadrant-system", "kuadrant-operator", "stable", "1.4.0")
		res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"})
		if res.Success || !strings.Contains(res.Message, "bypasses gateway authentication") {
			t.Fatalf("res = %+v", res)
		}
	})
	t.Run("below the lowest", func(t *testing.T) {
		mc := maasClusterWithModel(t)
		mc.f.putJSON("/apis/maas.opendatahub.io/v1alpha1/namespaces/models-as-a-service/maassubscriptions/premium", `{"spec":{"priority":10}}`)
		mc.f.putJSON("/apis/maas.opendatahub.io/v1alpha1/namespaces/models-as-a-service/maassubscriptions/free", `{"spec":{"priority":-3}}`)
		if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"}); !res.Success {
			t.Fatalf("publish: %+v", res)
		}
		if got := fmt.Sprint(mc.f.get(tMaaSSub)["spec"].(map[string]any)["priority"]); got != "-4" {
			t.Fatalf("priority = %s", got)
		}
	})
	t.Run("others and 1.4.3", func(t *testing.T) {
		mc := maasClusterWithModel(t)
		newFakeOLMSim(mc.f).installed("rhcl-operator", "kuadrant-system", "kuadrant-operator", "stable", "1.4.3")
		mc.f.putJSON("/apis/maas.opendatahub.io/v1alpha1/namespaces/models-as-a-service/maassubscriptions/premium", `{"spec":{"priority":10}}`)
		res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"})
		if res.Success || !strings.Contains(res.Message, "Upgrade Connectivity Link") || mc.f.has(tMaaSSub) {
			t.Fatalf("res = %+v", res)
		}
	})
	t.Run("alone", func(t *testing.T) {
		mc := maasClusterWithModel(t)
		if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"}); !res.Success {
			t.Fatalf("publish: %+v", res)
		}
		if got := fmt.Sprint(mc.f.get(tMaaSSub)["spec"].(map[string]any)["priority"]); got != "10" {
			t.Fatalf("priority = %s", got)
		}
	})
}

// A gateway someone else made that admits every namespace: Publish refuses
// and the part shows the patch to the documented list.
func TestMaaS_ForeignGatewayAdmittingAll(t *testing.T) {
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	mc.f.putJSON(tMaaSGateway, `{"spec":{"listeners":[{"name":"https","hostname":"maas.apps.example.com","allowedRoutes":{"namespaces":{"from":"All"}}}]},"status":{"conditions":[{"type":"Programmed","status":"True"}],"addresses":[{"value":"lb"}]}}`)
	st, _ := GetMaaSStatus(mc.c)
	p := maasPart(t, st, "gateway")
	if p.State != "problem" || !strings.Contains(p.Commands, `"from":"Selector"`) || !strings.Contains(p.Commands, "redhat-ai-gateway-infra") {
		t.Fatalf("part = %+v", p)
	}
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"})
	if res.Success || !strings.Contains(res.Message, "MaaS gateway") {
		t.Fatalf("res = %+v", res)
	}
}

// A gateway someone else made that selects the namespaces it admits by
// label: Publish never writes it. When it admits the project, Publish goes
// on without a gateway write; when it does not, Publish refuses before
// changing anything.
func TestMaaSPublish_ForeignGatewaySelectingByLabel(t *testing.T) {
	const foreignGW = `{"metadata":{"name":"maas-default-gateway","namespace":"openshift-ingress"},"spec":{"listeners":[{"name":"https","hostname":"maas.apps.example.com","allowedRoutes":{"namespaces":{"from":"Selector","selector":{"matchLabels":{"maas.opendatahub.io/gateway-access":"true"}}}}}]},"status":{"conditions":[{"type":"Programmed","status":"True"}],"addresses":[{"value":"lb"}]}}`
	for _, tc := range []struct {
		name, nsLabels string
		admitted       bool
	}{
		{"labelled project", `{"opendatahub.io/dashboard":"true","maas.opendatahub.io/gateway-access":"true"}`, true},
		{"project without the label", `{"opendatahub.io/dashboard":"true"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mc := maasClusterWithModel(t)
			mc.f.putJSON(tMaaSGateway, foreignGW)
			mc.f.putJSON("/api/v1/namespaces/proj", `{"metadata":{"labels":`+tc.nsLabels+`}}`)
			mc.f.requests = nil
			res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"})
			if hasMutation(mc.f, "PUT "+tMaaSGateway) || hasMutation(mc.f, "PATCH "+tMaaSGateway) || hasMutation(mc.f, "DELETE "+tMaaSGateway) {
				t.Fatalf("the foreign gateway was written: %v", mc.f.mutations())
			}
			if !tc.admitted {
				if res.Success || res.ErrorCode != "prerequisites" || !strings.Contains(res.Message, "does not admit routes from proj") {
					t.Fatalf("res = %+v", res)
				}
				if len(mc.f.mutations()) != 0 {
					t.Fatalf("mutations = %v", mc.f.mutations())
				}
				return
			}
			if !res.Success {
				t.Fatalf("publish: %+v", res)
			}
			if got := finalSteps(events); got != "check=success gateway=success model=success maas=success ready=success" {
				t.Fatalf("steps = %s", got)
			}
			if !stepSaid(events, "gateway", "The gateway someone else made admits the project.") {
				t.Fatalf("events = %+v", events)
			}
			if !mc.f.has(tMaaSRef) || !readRecord(t, mc.c).published("proj", "qwen25-05b-instruct") {
				t.Fatalf("not published: record %+v", readRecord(t, mc.c))
			}
		})
	}
}

// stepSaid: a step event of id carried msg.
func stepSaid(events []UpdateStepEvent, id, msg string) bool {
	for _, e := range events {
		if e.Step == id && e.Message == msg {
			return true
		}
	}
	return false
}

// After Connectivity Link and CloudNativePG are uninstalled, OLM leaves their
// CRDs, so both APIs are still served with no operator to reconcile them
// (seen live 2026-11-18). A served API alone is not an installed operator:
// both parts read missing, Set up plans and does both installs.
func TestMaaSSetup_LeftoverCRDsWithoutOperatorsAreInstalled(t *testing.T) {
	mc, _ := newFreshMaaSCluster(t)
	f := mc.f
	f.mu.Lock()
	f.groups["/apis/kuadrant.io"] = `{"kind":"APIGroup","name":"kuadrant.io","versions":[{"groupVersion":"kuadrant.io/v1","version":"v1"},{"groupVersion":"kuadrant.io/v1beta1","version":"v1beta1"}],"preferredVersion":{"groupVersion":"kuadrant.io/v1","version":"v1"}}`
	f.groups["/apis/postgresql.cnpg.io"] = `{"kind":"APIGroup","name":"postgresql.cnpg.io","versions":[{"groupVersion":"postgresql.cnpg.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"postgresql.cnpg.io/v1","version":"v1"}}`
	delete(f.fail, "GET /apis/kuadrant.io/v1beta1/kuadrants")
	f.mu.Unlock()
	// As the API server does (seen live): a Kuadrant CR, dry run or not, in
	// a namespace that does not exist yet is refused with 404.
	const kuadrantsPath = "/apis/kuadrant.io/v1beta1/namespaces/kuadrant-system/kuadrants"
	prev := f.beforeServe
	f.beforeServe = func(method, path string) {
		prev(method, path)
		if method != http.MethodPost || path != kuadrantsPath {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.objects[fakeKey{gv: "v1", plural: "namespaces", name: "kuadrant-system"}] == nil {
			f.fail["POST "+kuadrantsPath], f.failDryRun["POST "+kuadrantsPath] = 404, 404
		} else {
			delete(f.fail, "POST "+kuadrantsPath)
			delete(f.failDryRun, "POST "+kuadrantsPath)
		}
	}
	st, err := GetMaaSStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	if p := maasPart(t, st, "rhcl"); p.State != "missing" {
		t.Errorf("part rhcl with only its CRDs left = %+v, want missing", p)
	}
	// CloudNativePG is in its database choice's plan, not a part, until chosen.
	plan := strings.Join(append(st.SetupPlan, st.Database.Choices[2].Plan...), "\n")
	for _, want := range []string{"Install Red Hat Connectivity Link", "Install CloudNativePG"} {
		if !strings.Contains(plan, want) {
			t.Errorf("the plan lacks %q:\n%s", want, plan)
		}
	}
	res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG})
	if !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	if got := finalSteps(events); !strings.Contains(got, "rhcl=success") || !strings.Contains(got, "cnpg=success") {
		t.Fatalf("steps = %s, want both installs", got)
	}
	muts := strings.Join(mc.f.mutations(), "\n")
	for _, want := range []string{"POST /apis/operators.coreos.com/v1alpha1/namespaces/kuadrant-system/subscriptions", "POST /apis/operators.coreos.com/v1alpha1/namespaces/cnpg-system/subscriptions"} {
		if !strings.Contains(muts, want) {
			t.Errorf("no %s in %s", want, muts)
		}
	}
}

// A Deployment list that cannot be read stops Set up before any change
// (fail closed): with only the API to go by, it cannot tell an installed
// operator from leftover CRDs.
func TestMaaSSetup_ControllerReadFailureChangesNothing(t *testing.T) {
	mc, _ := newFreshMaaSCluster(t)
	f := mc.f
	f.mu.Lock()
	f.groups["/apis/kuadrant.io"] = `{"kind":"APIGroup","name":"kuadrant.io","versions":[{"groupVersion":"kuadrant.io/v1","version":"v1"},{"groupVersion":"kuadrant.io/v1beta1","version":"v1beta1"}],"preferredVersion":{"groupVersion":"kuadrant.io/v1","version":"v1"}}`
	delete(f.fail, "GET /apis/kuadrant.io/v1beta1/kuadrants")
	f.fail["GET /apis/apps/v1/deployments"] = 403
	f.mu.Unlock()
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG})
	if res.Success {
		t.Fatal("setup succeeded without knowing whether Kuadrant's operator runs")
	}
	if m := mc.f.mutations(); len(m) != 0 {
		t.Fatalf("changes after a failed read: %v", m)
	}
}
