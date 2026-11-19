package cluster

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	tDSC         = "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/default-dsc"
	tCertSub     = "/apis/operators.coreos.com/v1alpha1/namespaces/cert-manager-operator/subscriptions/openshift-cert-manager-operator"
	tCertGroups  = "/apis/operators.coreos.com/v1/namespaces/cert-manager-operator/operatorgroups"
	tCertNS      = "/api/v1/namespaces/cert-manager-operator"
	certMgrCSVNS = "cert-manager-operator"
)

// certManagerFakePackage is the cert-manager operator as redhat-operators
// offers it (live 2026-11-18: stable-v1 at 1.20.1; install modes Own,
// Single and All namespaces).
func certManagerFakePackage() fakePackage {
	return fakePackage{source: "redhat-operators", defaultChannel: "stable-v1", order: []string{"stable-v1"},
		installModes: []string{"OwnNamespace", "SingleNamespace", "AllNamespaces"},
		channels:     map[string]fakeChannel{"stable-v1": {head: "1.20.1", entries: []string{"1.20.1"}}}}
}

// prereqCluster is a cluster as fresh as RHOAI leaves it: a
// DataScienceCluster with every feature's component Removed, KServe
// reporting cert-manager missing, no cert-manager. RHOAI's operator reports
// a component Ready once it is Managed; KServe sees cert-manager once OLM
// installed it (after depReads more reads of the DataScienceCluster).
type prereqCluster struct {
	f   *resourceFake
	c   *Client
	sim *fakeOLMSim
	// depMissing is what KServe reports missing once cert-manager is in
	// ("" none: the condition turns True).
	depMissing string
	// depReads is how many DataScienceCluster reads after the install
	// KServe takes to notice it.
	depReads int
	reads    int
	certIn   bool
}

func newPrereqCluster(t *testing.T) *prereqCluster {
	t.Helper()
	f, c := newResourceFake(t)
	pc := &prereqCluster{f: f, c: c}
	f.putJSON(tDSC, `{"spec":{"components":{
		"aipipelines":{"managementState":"Removed"},"mlflowoperator":{"managementState":"Removed"},
		"kserve":{"managementState":"Removed"},"ogx":{"managementState":"Removed"},"dashboard":{"managementState":"Removed"}}},
		"status":{"conditions":[{"type":"KserveLLMInferenceServiceDependencies","status":"False","message":"cert-manager operator not installed"}]}}`)
	pc.sim = newFakeOLMSim(f)
	pc.sim.packages["openshift-cert-manager-operator"] = certManagerFakePackage()
	pc.putPackage("openshift-cert-manager-operator")
	pc.sim.onInstalled = func(pkg, ns, from, to string) {
		if pkg == "openshift-cert-manager-operator" {
			pc.certIn = true
		}
	}
	f.beforeServe = func(method, path string) {
		pc.sim.step(method, path)
		if method == http.MethodGet && strings.Contains(path, "/datascienceclusters") {
			pc.kserveChecks()
		}
	}
	f.onMergePatch = func(r *http.Request, k fakeKey, patch, next map[string]any) {
		// Under the fake's lock: the stored object is the one before the patch.
		updateFieldOwnership(r.URL.Query().Get("fieldManager"), pc.f.objects[k], patch, next)
		if k.plural != "datascienceclusters" || r.URL.Query().Get("dryRun") != "" {
			return
		}
		// RHOAI's operator: a component Managed reports <Key>Ready True.
		comps := ensureMap(ensureMap(next, "spec"), "components")
		status := ensureMap(next, "status")
		conds, _ := status["conditions"].([]any)
		for key, v := range comps {
			if m, _ := v.(map[string]any); m["managementState"] == "Managed" {
				conds = setFakeCondition(conds, key+"Ready", "True", "")
			}
		}
		status["conditions"] = conds
	}
	return pc
}

// kserveChecks is KServe's dependency check on a read of the
// DataScienceCluster: it sees cert-manager depReads reads after OLM
// installed it.
func (pc *prereqCluster) kserveChecks() {
	pc.f.mu.Lock()
	defer pc.f.mu.Unlock()
	if !pc.certIn {
		return
	}
	pc.reads++
	if pc.reads <= pc.depReads {
		return
	}
	k, _ := parseFakePath(tDSC)
	obj := pc.f.objects[k]
	if obj == nil {
		return
	}
	status := ensureMap(obj, "status")
	conds, _ := status["conditions"].([]any)
	if pc.depMissing == "" {
		status["conditions"] = setFakeCondition(conds, llmDependenciesCondition, "True", "")
	} else {
		status["conditions"] = setFakeCondition(conds, llmDependenciesCondition, "False", pc.depMissing)
	}
}

func (pc *prereqCluster) putPackage(name string) {
	p := pc.sim.packages[name]
	var status map[string]any
	_ = json.Unmarshal([]byte(packageManifestStatus(name, p)), &status)
	pc.f.put("/apis/packages.operators.coreos.com/v1/namespaces/openshift-marketplace/packagemanifests/"+name, map[string]any{
		"metadata": map[string]any{"labels": map[string]any{"catalog": p.source}}, "status": status})
}

// updateFieldOwnership records an update (a merge patch) by manager in
// next's managedFields as the API server does (Kubernetes docs,
// "Server-Side Apply", Field management: an update takes ownership of the
// fields it changes, from every other manager). A field the patch sets to
// the value it already had changes no ownership. Only spec is tracked.
func updateFieldOwnership(manager string, before, patch, next map[string]any) {
	if manager == "" {
		return
	}
	var leaves [][]string
	var walk func(v any, path []string)
	walk = func(v any, path []string) {
		if m, ok := v.(map[string]any); ok && len(m) > 0 {
			for key, child := range m {
				walk(child, append(append([]string(nil), path...), "f:"+key))
			}
			return
		}
		leaves = append(leaves, path)
	}
	if spec, ok := patch["spec"]; ok {
		walk(spec, []string{"f:spec"})
	}
	valueAt := func(obj map[string]any, path []string) any {
		var v any = obj
		for _, p := range path {
			m, _ := v.(map[string]any)
			v = m[strings.TrimPrefix(p, "f:")]
		}
		raw, _ := json.Marshal(v)
		return string(raw)
	}
	meta := ensureMap(next, "metadata")
	entries, _ := meta["managedFields"].([]any)
	var mine map[string]any
	for _, e := range entries {
		if em, _ := e.(map[string]any); em["manager"] == manager && em["operation"] == "Update" {
			mine = em
		}
	}
	for _, path := range leaves {
		if before != nil && valueAt(before, path) == valueAt(next, path) {
			continue
		}
		for _, e := range entries {
			em, _ := e.(map[string]any)
			if em["manager"] == manager && em["operation"] == "Update" {
				continue
			}
			node, _ := em["fieldsV1"].(map[string]any)
			for i, p := range path {
				if node == nil {
					break
				}
				if i == len(path)-1 {
					delete(node, p)
					break
				}
				node, _ = node[p].(map[string]any)
			}
		}
		if mine == nil {
			mine = map[string]any{"manager": manager, "operation": "Update", "fieldsV1": map[string]any{}}
			entries = append(entries, mine)
		}
		node := ensureMap(mine, "fieldsV1")
		for _, p := range path {
			node = ensureMap(node, p)
		}
	}
	meta["managedFields"] = entries
}

// someoneSets changes a component as someone else would, with an update of
// their own field manager (kubectl edit).
func someoneSets(t *testing.T, pc *prereqCluster, key, state string) {
	t.Helper()
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"components": map[string]any{key: map[string]any{"managementState": state}}}})
	if _, _, err := pc.c.do(http.MethodPatch, tDSC, "application/merge-patch+json", patch, url.Values{"fieldManager": {"kubectl-edit"}}); err != nil {
		t.Fatal(err)
	}
}

func setFakeCondition(conds []any, typ, status, msg string) []any {
	for _, c := range conds {
		if m, _ := c.(map[string]any); m["type"] == typ {
			m["status"], m["message"] = status, msg
			return conds
		}
	}
	return append(conds, map[string]any{"type": typ, "status": status, "message": msg})
}

func runPrereqs(t *testing.T, c *Client, feature string) (*types.OperationResponse, []UpdateStepEvent) {
	t.Helper()
	var events []UpdateStepEvent
	res, err := RunPrerequisites(c, types.PrerequisitesRequest{Feature: feature}, func(e UpdateStepEvent) { events = append(events, e) })
	if err != nil {
		t.Fatalf("RunPrerequisites: %v", err)
	}
	return res, events
}

func prereqStatus(t *testing.T, c *Client, feature string) types.FeaturePrerequisites {
	t.Helper()
	st, err := GetPrerequisitesStatusFor(c, "")
	if err != nil {
		t.Fatal(err)
	}
	return st.Features[feature]
}

func componentState(f *resourceFake, key string) string {
	comps := ensureMap(ensureMap(f.get(tDSC), "spec"), "components")
	s, _ := ensureMap(comps, key)["managementState"].(string)
	return s
}

// On a fresh cluster each tab's Set up lists every change, then makes
// them in order: the components, then (Models) cert-manager as odh-gitops
// installs it, then waits until RHOAI reports them ready and KServe sees
// cert-manager.
func TestPrerequisites_FreshClusterPerFeature(t *testing.T) {
	for _, tc := range []struct {
		feature    string
		components []string
		plan       []string
		writes     []string
		steps      string
	}{
		{feature: "pipelines", components: []string{"aipipelines"},
			plan:   []string{"DataScienceCluster default-dsc: set aipipelines to Managed (it is Removed"},
			writes: []string{"PATCH " + tDSC},
			steps:  "prerequisites=success dsc=success ready=success"},
		{feature: "mlflow", components: []string{"mlflowoperator"},
			plan:   []string{"set mlflowoperator to Managed"},
			writes: []string{"PATCH " + tDSC},
			steps:  "prerequisites=success dsc=success ready=success"},
		{feature: "playground", components: []string{"ogx", "dashboard"},
			plan:   []string{"set ogx to Managed", "set dashboard to Managed"},
			writes: []string{"PATCH " + tDSC, "PATCH " + tDSC},
			steps:  "prerequisites=success dsc=success ready=success"},
		{feature: "models", components: []string{"kserve"},
			plan: []string{"set kserve to Managed",
				"Install cert-manager Operator for Red Hat OpenShift 1.20.1: Namespace cert-manager-operator, OperatorGroup cert-manager-operator/cert-manager-operator, Subscription cert-manager-operator/openshift-cert-manager-operator (package openshift-cert-manager-operator, channel stable-v1, from redhat-operators"},
			writes: []string{"PATCH " + tDSC, "POST /api/v1/namespaces", "POST " + tCertGroups, "POST /apis/operators.coreos.com/v1alpha1/namespaces/cert-manager-operator/subscriptions"},
			steps:  "prerequisites=success dsc=success cert-manager=success ready=success"},
	} {
		t.Run(tc.feature, func(t *testing.T) {
			pc := newPrereqCluster(t)
			st := prereqStatus(t, pc.c, tc.feature)
			if st.Ready || st.SetupBlockedReason != "" {
				t.Fatalf("status = %+v", st)
			}
			plan := strings.Join(st.SetupPlan, "\n")
			for _, want := range tc.plan {
				if !strings.Contains(plan, want) {
					t.Errorf("plan lacks %q:\n%s", want, plan)
				}
			}
			if len(st.SetupPlan) != len(tc.plan) {
				t.Errorf("plan has %d changes, want %d:\n%s", len(st.SetupPlan), len(tc.plan), plan)
			}
			res, events := runPrereqs(t, pc.c, tc.feature)
			if !res.Success {
				t.Fatalf("setup: %+v", res)
			}
			if got := finalSteps(events); got != tc.steps {
				t.Errorf("steps = %s, want %s", got, tc.steps)
			}
			if muts := pc.f.mutations(); strings.Join(muts, "\n") != strings.Join(tc.writes, "\n") {
				t.Errorf("writes =\n%s\nwant\n%s", strings.Join(muts, "\n"), strings.Join(tc.writes, "\n"))
			}
			for _, key := range tc.components {
				if s := componentState(pc.f, key); s != "Managed" {
					t.Errorf("%s = %q", key, s)
				}
			}
			// Each component's patch names only it, under its own manager.
			rec, err := readPrereqRecord(pc.c)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range tc.components {
				if rc, ok := rec.Components[key]; !ok || rc.Previous != "Removed" || rc.Feature != tc.feature {
					t.Errorf("record of %s = %+v (%v)", key, rc, ok)
				}
			}
			after := prereqStatus(t, pc.c, tc.feature)
			if !after.Ready || len(after.SetupPlan) != 0 || len(after.Restorable) != len(tc.components) {
				t.Errorf("after setup = %+v", after)
			}
			// Nothing else of the cluster is touched: the other features'
			// components stay Removed.
			for _, other := range []string{"aipipelines", "mlflowoperator", "kserve", "ogx", "dashboard"} {
				if !slices.Contains(tc.components, other) && componentState(pc.f, other) != "Removed" {
					t.Errorf("%s changed", other)
				}
			}
		})
	}
}

// cert-manager goes where odh-gitops puts it: an OperatorGroup without a
// spec (all namespaces) and a Subscription with automatic approval, no
// config (no AWS role ARN: RHOAI's issuers need none).
func TestPrerequisites_CertManagerObjects(t *testing.T) {
	pc := newPrereqCluster(t)
	if res, _ := runPrereqs(t, pc.c, "models"); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	og := pc.f.get(tCertGroups + "/cert-manager-operator")
	if og == nil || og["spec"] != nil {
		t.Fatalf("operator group = %v", og)
	}
	sub := pc.f.get(tCertSub)
	spec, _ := sub["spec"].(map[string]any)
	if spec["name"] != "openshift-cert-manager-operator" || spec["channel"] != "stable-v1" || spec["source"] != "redhat-operators" || spec["installPlanApproval"] != "Automatic" || spec["config"] != nil {
		t.Fatalf("subscription spec = %v", spec)
	}
	if ensureMap(sub, "metadata")["labels"] == nil {
		t.Fatal("the subscription carries no tool label")
	}
	rec, _ := readPrereqRecord(pc.c)
	if rec.Shared["cert-manager"] == "" {
		t.Fatalf("record = %+v", rec)
	}
	// A second run changes nothing.
	before := len(pc.f.mutations())
	if res, _ := runPrereqs(t, pc.c, "models"); !res.Success {
		t.Fatalf("again: %+v", res)
	}
	if muts := pc.f.mutations(); len(muts) != before {
		t.Fatalf("a second run wrote %v", muts[before:])
	}
}

// The live cluster's cert-manager (installed from the console: namespace
// cert-manager-operator, an own-namespace OperatorGroup named after the
// package) is adopted as it is: nothing is written.
func TestPrerequisites_AdoptsAnInstalledCertManager(t *testing.T) {
	pc := newPrereqCluster(t)
	pc.f.putJSON(tCertNS, `{}`)
	pc.f.putJSON(tCertGroups+"/openshift-cert-manager-operator", `{"spec":{"targetNamespaces":["cert-manager-operator"]}}`)
	pc.sim.installed("openshift-cert-manager-operator", certMgrCSVNS, "openshift-cert-manager-operator", "stable-v1", "1.20.1")
	pc.f.putJSON(tDSC, `{"spec":{"components":{"kserve":{"managementState":"Managed"}}},
		"status":{"conditions":[{"type":"KserveReady","status":"True"},{"type":"KserveLLMInferenceServiceDependencies","status":"True"}]}}`)
	st := prereqStatus(t, pc.c, "models")
	if !st.Ready || len(st.SetupPlan) != 0 || st.SetupBlockedReason != "" {
		t.Fatalf("status = %+v", st)
	}
	for _, p := range st.Parts {
		if p.ID == "cert-manager" && (p.State != "ready" || !p.NotManaged || !strings.Contains(p.Message, "1.20.1")) {
			t.Fatalf("part = %+v", p)
		}
	}
	res, events := runPrereqs(t, pc.c, "models")
	if !res.Success || finalSteps(events) != "prerequisites=success dsc=success cert-manager=success ready=success" {
		t.Fatalf("setup: %+v %s", res, finalSteps(events))
	}
	if muts := pc.f.mutations(); len(muts) != 0 {
		t.Fatalf("wrote %v", muts)
	}
}

// An install of the package elsewhere that OLM is still working on is
// waited for, never installed a second time.
func TestPrerequisites_WaitsForAPendingInstallElsewhere(t *testing.T) {
	pc := newPrereqCluster(t)
	pc.f.putJSON("/apis/operators.coreos.com/v1alpha1/namespaces/openshift-operators/subscriptions/my-cert-manager",
		`{"spec":{"name":"openshift-cert-manager-operator","channel":"stable-v1","source":"redhat-operators","sourceNamespace":"openshift-marketplace"},"status":{"state":"UpgradePending"}}`)
	st := prereqStatus(t, pc.c, "models")
	if strings.Contains(strings.Join(st.SetupPlan, "\n"), "Install cert-manager") {
		t.Fatalf("plan installs a second cert-manager: %v", st.SetupPlan)
	}
	res, _ := runPrereqs(t, pc.c, "models")
	if !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	for _, m := range pc.f.mutations() {
		if strings.HasPrefix(m, "POST ") {
			t.Fatalf("created %s", m)
		}
	}
	if !pc.f.has("/apis/operators.coreos.com/v1alpha1/namespaces/openshift-operators/clusterserviceversions/openshift-cert-manager-operator.v1.20.1") {
		t.Fatal("OLM was not waited for")
	}
}

// The namespace's one OperatorGroup is used as it is when it gives an
// install mode the package supports; with two, OLM installs nothing
// (TooManyOperatorGroups), so setup refuses before any write.
func TestPrerequisites_OperatorGroupReuse(t *testing.T) {
	t.Run("reuse an own-namespace group", func(t *testing.T) {
		pc := newPrereqCluster(t)
		pc.f.putJSON(tCertNS, `{}`)
		pc.f.putJSON(tCertGroups+"/openshift-cert-manager-operator", `{"spec":{"targetNamespaces":["cert-manager-operator"]}}`)
		st := prereqStatus(t, pc.c, "models")
		plan := strings.Join(st.SetupPlan, "\n")
		if !strings.Contains(plan, "using OperatorGroup cert-manager-operator/openshift-cert-manager-operator as it is") || strings.Contains(plan, "OperatorGroup cert-manager-operator/cert-manager-operator") {
			t.Fatalf("plan = %s", plan)
		}
		if res, _ := runPrereqs(t, pc.c, "models"); !res.Success {
			t.Fatalf("setup: %+v", res)
		}
		for _, m := range pc.f.mutations() {
			if strings.Contains(m, "/operatorgroups") || m == "POST /api/v1/namespaces" {
				t.Fatalf("created %s", m)
			}
		}
		if !pc.f.has(tCertSub) {
			t.Fatal("no subscription")
		}
	})
	t.Run("two groups", func(t *testing.T) {
		pc := newPrereqCluster(t)
		pc.f.putJSON(tCertNS, `{}`)
		pc.f.putJSON(tCertGroups+"/a", `{"spec":{}}`)
		pc.f.putJSON(tCertGroups+"/b", `{"spec":{}}`)
		st := prereqStatus(t, pc.c, "models")
		if !strings.Contains(st.SetupBlockedReason, "has 2 OperatorGroups") {
			t.Fatalf("status = %+v", st)
		}
		res, _ := runPrereqs(t, pc.c, "models")
		if res.Success || !strings.Contains(res.Message, "has 2 OperatorGroups") {
			t.Fatalf("setup: %+v", res)
		}
		if muts := pc.f.mutations(); len(muts) != 0 {
			t.Fatalf("wrote %v", muts)
		}
	})
	t.Run("a group the package does not support", func(t *testing.T) {
		pc := newPrereqCluster(t)
		p := pc.sim.packages["openshift-cert-manager-operator"]
		p.installModes = []string{"AllNamespaces"}
		pc.sim.packages["openshift-cert-manager-operator"] = p
		pc.putPackage("openshift-cert-manager-operator")
		pc.f.putJSON(tCertNS, `{}`)
		pc.f.putJSON(tCertGroups+"/mine", `{"spec":{"targetNamespaces":["cert-manager-operator"]}}`)
		res, _ := runPrereqs(t, pc.c, "models")
		if res.Success || !strings.Contains(res.Message, "OperatorGroup cert-manager-operator/mine targets only its own namespace") {
			t.Fatalf("setup: %+v", res)
		}
		if muts := pc.f.mutations(); len(muts) != 0 {
			t.Fatalf("wrote %v", muts)
		}
	})
	// A new OperatorGroup (namespace missing, or none in it) is planned only
	// for an install mode the channel's head supports.
	for name, prepare := range map[string]func(pc *prereqCluster){
		"no namespace":     func(*prereqCluster) {},
		"no OperatorGroup": func(pc *prereqCluster) { pc.f.putJSON(tCertNS, `{}`) },
	} {
		t.Run("a new group the package does not support: "+name, func(t *testing.T) {
			pc := newPrereqCluster(t)
			p := pc.sim.packages["openshift-cert-manager-operator"]
			p.installModes = []string{"OwnNamespace"}
			pc.sim.packages["openshift-cert-manager-operator"] = p
			pc.putPackage("openshift-cert-manager-operator")
			prepare(pc)
			before := len(pc.f.mutations())
			res, _ := runPrereqs(t, pc.c, "models")
			if res.Success || !strings.Contains(res.Message, "does not support being installed for all namespaces") {
				t.Fatalf("setup: %+v", res)
			}
			if muts := pc.f.mutations()[before:]; len(muts) != 0 {
				t.Fatalf("wrote %v", muts)
			}
		})
	}
}

// An operator spec for an own-namespace operator (LeaderWorkerSet, JobSet)
// creates its OperatorGroup for its namespace only.
func TestOperatorObjects_OwnNamespaceGroup(t *testing.T) {
	op := operatorSpec{pkg: "leader-worker-set", subName: "leader-worker-set", namespace: "openshift-lws-operator", source: "redhat-operators", group: "leader-worker-set", ownNamespace: true}
	_, group, _ := operatorObjects(op, "stable-v1.0")
	spec, _ := group["spec"].(map[string]any)
	if targets, _ := spec["targetNamespaces"].([]any); len(targets) != 1 || targets[0] != "openshift-lws-operator" {
		t.Fatalf("group = %v", group)
	}
	if p := operatorPlanPrefix(&operatorInstallPlan{op: op, groupMissing: true}); !strings.Contains(p, "OperatorGroup openshift-lws-operator/leader-worker-set") {
		t.Fatalf("prefix = %q", p)
	}
}

// cert-manager's CRDs left by an uninstall (no controller) do not count:
// it is installed. A cert-manager that runs without OLM is not replaced or
// doubled: setup refuses before any write.
func TestPrerequisites_CertManagerWithoutOLM(t *testing.T) {
	t.Run("leftover CRDs", func(t *testing.T) {
		pc := newPrereqCluster(t)
		pc.f.groups = map[string]string{"/apis/cert-manager.io": `{"kind":"APIGroup","name":"cert-manager.io","versions":[{"groupVersion":"cert-manager.io/v1","version":"v1"}]}`}
		st := prereqStatus(t, pc.c, "models")
		if !strings.Contains(strings.Join(st.SetupPlan, "\n"), "Install cert-manager") {
			t.Fatalf("plan = %v (%s)", st.SetupPlan, st.SetupBlockedReason)
		}
		if res, _ := runPrereqs(t, pc.c, "models"); !res.Success || !pc.f.has(tCertSub) {
			t.Fatalf("setup: %+v", res)
		}
	})
	t.Run("a running cert-manager", func(t *testing.T) {
		pc := newPrereqCluster(t)
		pc.f.groups = map[string]string{"/apis/cert-manager.io": `{"kind":"APIGroup","name":"cert-manager.io","versions":[{"groupVersion":"cert-manager.io/v1","version":"v1"}]}`}
		pc.f.putJSON("/apis/apps/v1/namespaces/cert-manager/deployments/cert-manager", `{}`)
		st := prereqStatus(t, pc.c, "models")
		if !strings.Contains(st.SetupBlockedReason, "without being installed from OperatorHub") {
			t.Fatalf("status = %+v", st)
		}
		res, _ := runPrereqs(t, pc.c, "models")
		if res.Success || len(pc.f.mutations()) != 0 {
			t.Fatalf("setup: %+v, wrote %v", res, pc.f.mutations())
		}
	})
}

// A DataScienceCluster that Argo CD manages is not changed: Set up refuses
// and says to change Git.
func TestPrerequisites_RefusesAGitOpsManagedDSC(t *testing.T) {
	pc := newPrereqCluster(t)
	dsc := pc.f.get(tDSC)
	ensureMap(dsc, "metadata")["annotations"] = map[string]any{argoTrackingAnnotation: "rhoai:datasciencecluster.opendatahub.io/DataScienceCluster:/default-dsc"}
	st := prereqStatus(t, pc.c, "pipelines")
	if !strings.Contains(st.SetupBlockedReason, "Argo CD application") {
		t.Fatalf("status = %+v", st)
	}
	res, _ := runPrereqs(t, pc.c, "pipelines")
	if res.Success || res.ErrorCode != ErrorCodeGitOpsManaged {
		t.Fatalf("setup: %+v", res)
	}
	if muts := pc.f.mutations(); len(muts) != 0 {
		t.Fatalf("wrote %v", muts)
	}
}

// Any read that fails stops Set up before it changes anything.
func TestPrerequisites_ReadFailuresChangeNothing(t *testing.T) {
	for _, fail := range []string{
		"GET /apis/datasciencecluster.opendatahub.io/v2/datascienceclusters",
		"GET /apis/operators.coreos.com/v1alpha1/subscriptions",
		"GET /apis/operators.coreos.com/v1alpha1/clusterserviceversions",
		"GET /apis/packages.operators.coreos.com/v1/namespaces/openshift-marketplace/packagemanifests",
		"GET " + tCertNS,
		"GET /api/v1/namespaces/rhoai-nightly-updater/configmaps/rhoai-nightly-updater-snapshot",
	} {
		t.Run(fail, func(t *testing.T) {
			pc := newPrereqCluster(t)
			t.Setenv("NAMESPACE", "rhoai-nightly-updater")
			pc.f.fail[fail] = 403
			res, _ := runPrereqs(t, pc.c, "models")
			if res.Success {
				t.Fatalf("setup succeeded: %+v", res)
			}
			if muts := pc.f.mutations(); len(muts) != 0 {
				t.Fatalf("wrote %v", muts)
			}
			if st := prereqStatus(t, pc.c, "models"); st.SetupBlockedReason == "" || st.Ready {
				t.Fatalf("status = %+v", st)
			}
		})
	}
}

// Models waits until KServe sees cert-manager before its setup goes on.
// What else KServe reports missing (Connectivity Link, a routing choice)
// is reported, not waited for.
func TestPrerequisites_ModelsWaitsForKServe(t *testing.T) {
	t.Run("waits", func(t *testing.T) {
		pc := newPrereqCluster(t)
		pc.depReads = 3
		res, events := runPrereqs(t, pc.c, "models")
		if !res.Success {
			t.Fatalf("setup: %+v", res)
		}
		waited := false
		for _, e := range events {
			if e.Step == "ready" && strings.Contains(e.Message, "KServe does not see cert-manager") {
				waited = true
			}
		}
		if !waited || pc.reads <= pc.depReads {
			t.Fatalf("did not wait for KServe (reads %d): %+v", pc.reads, events)
		}
	})
	t.Run("connectivity link is reported", func(t *testing.T) {
		pc := newPrereqCluster(t)
		pc.depMissing = "Red Hat Connectivity Link not installed"
		res, _ := runPrereqs(t, pc.c, "models")
		if !res.Success || !strings.Contains(res.Message, "KServe still reports missing: Red Hat Connectivity Link not installed") {
			t.Fatalf("setup: %+v", res)
		}
		st := prereqStatus(t, pc.c, "models")
		if !st.Ready {
			t.Fatalf("not ready: %+v", st)
		}
		for _, p := range st.Parts {
			if p.ID == "dependencies" && (p.State != "manual" || !strings.Contains(p.Message, "MaaS tab")) {
				t.Fatalf("part = %+v", p)
			}
		}
	})
	t.Run("kserve never sees it", func(t *testing.T) {
		pc := newPrereqCluster(t)
		pc.depReads = 1 << 30
		res, _ := runPrereqs(t, pc.c, "models")
		if res.Success || res.ErrorCode != "not_ready" || !strings.Contains(res.Message, "Diagnostics") {
			t.Fatalf("setup: %+v", res)
		}
	})
}

// Clean up turns a component back to its value before setup only while
// this tool owns the field and nothing uses it; operators always stay.
func TestRestoreComponents(t *testing.T) {
	setUp := func(t *testing.T, feature string) *prereqCluster {
		pc := newPrereqCluster(t)
		if res, _ := runPrereqs(t, pc.c, feature); !res.Success {
			t.Fatalf("setup: %+v", res)
		}
		return pc
	}
	t.Run("restores", func(t *testing.T) {
		pc := setUp(t, "models")
		pc.f.putJSON("/apis/serving.kserve.io/v1alpha2/namespaces/p/llminferenceservices/gone", `{"metadata":{"deletionTimestamp":"2026-11-18T00:00:00Z"}}`)
		n := len(pc.f.mutations())
		res, err := RestoreComponents(pc.c, []string{"kserve"})
		if err != nil || !res.Success || !strings.Contains(res.Message, "kserve is Removed again") {
			t.Fatalf("restore: %+v %v", res, err)
		}
		if componentState(pc.f, "kserve") != "Removed" {
			t.Fatal("kserve not restored")
		}
		for _, m := range pc.f.mutations()[n:] {
			if !strings.HasPrefix(m, "PATCH "+tDSC) {
				t.Fatalf("restore wrote %s (operators stay)", m)
			}
		}
		if rec, _ := readPrereqRecord(pc.c); len(rec.Components) != 0 || rec.Shared["cert-manager"] == "" {
			t.Fatalf("record = %+v", rec)
		}
		if st := prereqStatus(t, pc.c, "models"); len(st.Restorable) != 0 {
			t.Fatalf("still restorable: %+v", st.Restorable)
		}
	})
	t.Run("changed by someone since", func(t *testing.T) {
		pc := setUp(t, "pipelines")
		// They turned it off and on again: each change takes the field.
		someoneSets(t, pc, "aipipelines", "Removed")
		someoneSets(t, pc, "aipipelines", "Managed")
		n := len(pc.f.mutations())
		res, _ := RestoreComponents(pc.c, []string{"aipipelines"})
		if !res.Success || !strings.Contains(res.Message, "Kept Managed: aipipelines (changed by someone since") {
			t.Fatalf("restore: %+v", res)
		}
		if len(pc.f.mutations()) != n || componentState(pc.f, "aipipelines") != "Managed" {
			t.Fatal("changed a field someone else set")
		}
	})
	t.Run("set to Managed again by someone", func(t *testing.T) {
		// Setting the value it already has does not take the field (the
		// API server moves a field only when its value changes): the tool
		// still owns it, and the in-use check is what keeps a component
		// someone relies on.
		pc := setUp(t, "pipelines")
		someoneSets(t, pc, "aipipelines", "Managed")
		if !dscComponentOwned(pc.f.get(tDSC), "aipipelines") {
			t.Fatal("a same-value update took the field")
		}
		pc.f.putJSON("/apis/datasciencepipelinesapplications.opendatahub.io/v1/namespaces/team/datasciencepipelinesapplications/theirs", `{}`)
		res, _ := RestoreComponents(pc.c, []string{"aipipelines"})
		if !res.Success || !strings.Contains(res.Message, "in use: pipeline server team/theirs") || componentState(pc.f, "aipipelines") != "Managed" {
			t.Fatalf("restore: %+v", res)
		}
		k, _ := parseFakePath("/apis/datasciencepipelinesapplications.opendatahub.io/v1/namespaces/team/datasciencepipelinesapplications/theirs")
		delete(pc.f.objects, k)
		if res, _ := RestoreComponents(pc.c, []string{"aipipelines"}); !res.Success || componentState(pc.f, "aipipelines") != "Removed" {
			t.Fatalf("restore once unused: %+v", res)
		}
	})
	t.Run("the DataScienceCluster changes before the write", func(t *testing.T) {
		pc := setUp(t, "pipelines")
		writes := 0
		pc.f.beforeRequest = func(r *http.Request) {
			if r.Method == http.MethodPatch && r.URL.Path == tDSC && r.URL.Query().Get("dryRun") == "" && r.URL.Query().Get("fieldManager") != "kubectl-edit" {
				writes++
				if writes == 1 {
					// An unrelated edit: a new resourceVersion.
					pc.f.bumpRV(ensureMap(pc.f.get(tDSC), "metadata"))
				}
			}
		}
		res, _ := RestoreComponents(pc.c, []string{"aipipelines"})
		if !res.Success || componentState(pc.f, "aipipelines") != "Removed" || writes != 2 {
			t.Fatalf("restore: %+v (writes %d)", res, writes)
		}
	})
	t.Run("a change before the write fails a check", func(t *testing.T) {
		for name, change := range map[string]func(pc *prereqCluster){
			"someone changed it": func(pc *prereqCluster) {
				someoneSets(t, pc, "ogx", "Removed")
				someoneSets(t, pc, "ogx", "Managed")
			},
			"a playground server appears": func(pc *prereqCluster) {
				pc.f.putJSON("/apis/ogx.io/v1beta1/namespaces/team/ogxservers/theirs", `{}`)
				pc.f.bumpRV(ensureMap(pc.f.get(tDSC), "metadata"))
			},
			"Argo CD takes it over": func(pc *prereqCluster) {
				meta := ensureMap(pc.f.get(tDSC), "metadata")
				meta["annotations"] = map[string]any{argoTrackingAnnotation: "rhoai:datasciencecluster.opendatahub.io/DataScienceCluster:/default-dsc"}
				pc.f.bumpRV(meta)
			},
			"the second read fails": func(pc *prereqCluster) {
				pc.f.bumpRV(ensureMap(pc.f.get(tDSC), "metadata"))
				pc.f.fail["GET /apis/datasciencecluster.opendatahub.io/v2/datascienceclusters"] = 500
				pc.f.fail["GET /apis/datasciencecluster.opendatahub.io/v1/datascienceclusters"] = 500
			},
		} {
			t.Run(name, func(t *testing.T) {
				pc := setUp(t, "playground")
				changed := false
				pc.f.beforeRequest = func(r *http.Request) {
					if !changed && r.Method == http.MethodPatch && r.URL.Path == tDSC && r.URL.Query().Get("dryRun") == "" && r.URL.Query().Get("fieldManager") != "kubectl-edit" {
						changed = true
						change(pc)
					}
				}
				res, _ := RestoreComponents(pc.c, []string{"ogx", "dashboard"})
				if !changed {
					t.Fatal("no write was attempted")
				}
				if componentState(pc.f, "ogx") != "Managed" {
					t.Fatalf("ogx turned back after the check failed: %+v", res)
				}
				switch name {
				case "a playground server appears":
					// Both are checked again on the new read: both in use.
					if !res.Success || componentState(pc.f, "dashboard") != "Managed" || !strings.Contains(res.Message, "dashboard (in use") {
						t.Fatalf("restore: %+v", res)
					}
				case "someone changed it":
					if !res.Success || !strings.Contains(res.Message, "ogx (changed by someone since") || componentState(pc.f, "dashboard") != "Removed" {
						t.Fatalf("restore: %+v", res)
					}
				default:
					if res.Success || !strings.Contains(res.Message, "Nothing was changed") || componentState(pc.f, "dashboard") != "Managed" {
						t.Fatalf("restore: %+v", res)
					}
				}
			})
		}
	})
	t.Run("in use", func(t *testing.T) {
		pc := setUp(t, "pipelines")
		pc.f.putJSON("/apis/datasciencepipelinesapplications.opendatahub.io/v1/namespaces/team/datasciencepipelinesapplications/theirs", `{}`)
		res, _ := RestoreComponents(pc.c, []string{"aipipelines"})
		if !res.Success || !strings.Contains(res.Message, "in use: pipeline server team/theirs") || componentState(pc.f, "aipipelines") != "Managed" {
			t.Fatalf("restore: %+v", res)
		}
		// Recorded still: a later Clean up can turn it back.
		if st := prereqStatus(t, pc.c, "pipelines"); len(st.Restorable) != 1 {
			t.Fatalf("restorable = %+v", st.Restorable)
		}
	})
	t.Run("the check fails", func(t *testing.T) {
		pc := setUp(t, "playground")
		pc.f.fail["GET /apis/ogx.io/v1beta1/ogxservers"] = 403
		n := len(pc.f.mutations())
		res, _ := RestoreComponents(pc.c, []string{"ogx", "dashboard"})
		if res.Success || len(pc.f.mutations()) != n {
			t.Fatalf("restore: %+v, wrote %v", res, pc.f.mutations()[n:])
		}
	})
	t.Run("gitops", func(t *testing.T) {
		pc := setUp(t, "mlflow")
		ensureMap(pc.f.get(tDSC), "metadata")["annotations"] = map[string]any{argoTrackingAnnotation: "rhoai:datasciencecluster.opendatahub.io/DataScienceCluster:/default-dsc"}
		n := len(pc.f.mutations())
		res, _ := RestoreComponents(pc.c, []string{"mlflowoperator"})
		if res.Success || res.ErrorCode != ErrorCodeGitOpsManaged || len(pc.f.mutations()) != n {
			t.Fatalf("restore: %+v", res)
		}
	})
	t.Run("not turned on by this tool", func(t *testing.T) {
		pc := newPrereqCluster(t)
		res, _ := RestoreComponents(pc.c, []string{"kserve"})
		if !res.Success || len(pc.f.mutations()) != 0 || !strings.Contains(res.Message, "this tool did not turn it on") {
			t.Fatalf("restore: %+v", res)
		}
	})
}

// Clean up runs the restore last, after what used the components.
func TestCleanUp_RestoresComponentsLast(t *testing.T) {
	var order []string
	defer func(prev func(*Client, []string) (*types.OperationResponse, error)) { cleanupComponents = prev }(cleanupComponents)
	defer func(prev func(*Client) (*types.OperationResponse, error)) { cleanupMLflow = prev }(cleanupMLflow)
	cleanupComponents = func(_ *Client, keys []string) (*types.OperationResponse, error) {
		order = append(order, "components:"+strings.Join(keys, ","))
		return &types.OperationResponse{Success: true}, nil
	}
	cleanupMLflow = func(*Client) (*types.OperationResponse, error) {
		order = append(order, "mlflow")
		return &types.OperationResponse{Success: true}, nil
	}
	_, c := newResourceFake(t)
	var steps []string
	res, _ := CleanUp(c, types.CleanupRequest{MLflow: true, Components: []string{"mlflowoperator"}}, func(e UpdateStepEvent) { steps = append(steps, e.Step) })
	if !res.Success || strings.Join(order, " ") != "mlflow components:mlflowoperator" || steps[len(steps)-1] != "components" {
		t.Fatalf("res %+v order %v steps %v", res, order, steps)
	}
}
