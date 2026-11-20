package cluster

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// live36Operator is one module operator as the RHOAI 3.6 platform applies
// it (rhods-operator rhoai-3.6 prefetched-manifests/<module>/rbac/role.yaml,
// role_binding.yaml, manager.yaml, and the prefetched-charts): a ClusterRole
// granting its ServiceAccount the module CR, <plural>/status and
// <plural>/finalizers, a ClusterRoleBinding, and a Deployment in the
// applications namespace owned by Platform/default. Deployment names are
// the live ones (ROSA, OpenShift AI 3.6.0).
type live36Operator struct {
	Module, Plural, Kind, Deployment, ServiceAccount, Role string
	ResourceNames                                          []string // kserve's rules name only default-kserve
}

var live36Operators = []live36Operator{
	{"aigateway", "aigateways", "AIGateway", "ai-gateway-operator", "ai-gateway-operator", "ai-gateway-manager-role", nil},
	{"aihub", "aihubs", "AIHub", "aihub-controller-manager", "aihub-controller-manager", "aihub-manager-role", nil},
	{"aipipelines", "aipipelines", "AIPipelines", "data-science-pipelines-operator-controller-manager", "data-science-pipelines-operator-controller-manager", "data-science-pipelines-operator-manager-role", nil},
	{"dashboard", "dashboards", "Dashboard", "dashboard-operator", "dashboard-operator", "dashboard-operator", nil},
	{"feastoperator", "feastoperators", "FeastOperator", "opendatahub-feast-operator", "opendatahub-feast-controller-manager", "opendatahub-feast-manager-role", nil},
	{"kserve", "kserves", "Kserve", "kserve-module-controller-manager", "kserve-module-controller-manager", "kserve-module-manager-role", []string{"default-kserve"}},
	{"mcplifecycleoperator", "mcplifecycleoperators", "MCPLifecycleOperator", "mcp-lifecycle-module-operator-controller-manager", "mcp-lifecycle-module-operator-controller-manager", "mcp-lifecycle-module-operator-manager-role", nil},
	{"mlflowoperator", "mlflowoperators", "MLflowOperator", "mlflow-operator-controller-manager", "mlflow-operator-controller-manager", "mlflow-operator-manager-role", nil},
	{"ogx", "ogxs", "OGX", "opendatahub-ogx-operator", "opendatahub-ogx-operator", "opendatahub-ogx-manager-cluster-role", nil},
	{"ray", "rays", "Ray", "ray-module-operator-controller-manager", "ray-module-operator-controller-manager", "ray-module-operator-manager-role", nil},
	{"trainer", "trainers", "Trainer", "trainer-operator-controller-manager", "trainer-operator-controller-manager", "trainer-operator-manager-role", nil},
	{"trustyai", "trustyais", "TrustyAI", "trustyai-operator-module-controller-manager", "trustyai-operator-module-controller-manager", "trustyai-operator-module-manager-role", nil},
	{"workbenches", "workbenches", "Workbenches", "workbenches-operator", "workbenches-operator", "workbenches-operator-manager-role", nil},
}

// liveOperator returns the live 3.6 operator of a module.
func liveOperator(module string) live36Operator {
	for _, o := range live36Operators {
		if o.Module == module {
			return o
		}
	}
	panic("no live operator for " + module)
}

const liveAppsNS = "redhat-ods-applications"

// moduleOperatorWorld is the RBAC and the applications namespace's
// Deployments of a live 3.6 cluster, editable before serve.
type moduleOperatorWorld struct {
	roles       []map[string]any
	bindings    []map[string]any
	deployments []map[string]any
	// pods of the applications namespace (none unless a test adds them).
	pods []map[string]any
}

// operatorPod is a Pod running as sa in the applications namespace, in
// phase, controlled by controllerKind (ReplicaSet, StatefulSet, ... or
// "" for none).
func operatorPod(name, sa, phase, controllerKind string) map[string]any {
	meta := map[string]any{"name": name, "namespace": liveAppsNS}
	if controllerKind != "" {
		meta["ownerReferences"] = []any{map[string]any{"apiVersion": "apps/v1", "kind": controllerKind, "name": name + "-owner", "controller": true}}
	}
	return map[string]any{"metadata": meta, "spec": map[string]any{"serviceAccountName": sa}, "status": map[string]any{"phase": phase}}
}

func rule(groups, resources, verbs []string, names ...string) map[string]any {
	r := map[string]any{"apiGroups": groups, "resources": resources, "verbs": verbs}
	if len(names) > 0 {
		r["resourceNames"] = names
	}
	return r
}

var componentsGroup = []string{componentAPIGroup}

func platformDeployment(name, sa, ownerAPIVersion, ownerKind, ownerName string) map[string]any {
	meta := map[string]any{"name": name, "namespace": liveAppsNS, "labels": map[string]any{"platform.opendatahub.io/part-of": "platform", "app.kubernetes.io/name": name}}
	if ownerKind != "" {
		meta["ownerReferences"] = []any{map[string]any{"apiVersion": ownerAPIVersion, "kind": ownerKind, "name": ownerName, "controller": true}}
	}
	return map[string]any{"metadata": meta, "spec": map[string]any{"template": map[string]any{"spec": map[string]any{"serviceAccountName": sa}}}, "status": map[string]any{"availableReplicas": 1}}
}

func crb(name, role string, subjects ...map[string]any) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name}, "roleRef": map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": role}, "subjects": subjects}
}

func saSubject(ns, name string) map[string]any {
	return map[string]any{"kind": "ServiceAccount", "namespace": ns, "name": name}
}

func newLive36ModuleOperatorWorld() *moduleOperatorWorld {
	w := &moduleOperatorWorld{}
	all := []string{"create", "delete", "get", "list", "patch", "update", "watch"}
	// The RHOAI operator: specific finalizer rules for every module and
	// Kueue (internal/controller/modules and datasciencecluster
	// kubebuilder_rbac.go), "*" on RBAC objects; its Deployment is in
	// redhat-ods-operator, owned by the CSV.
	var opRules []any
	for _, o := range append(slices.Clone(live36Operators), live36Operator{Plural: "kueues"}) {
		opRules = append(opRules,
			rule(componentsGroup, []string{o.Plural}, all),
			rule(componentsGroup, []string{o.Plural + "/status"}, []string{"get", "update", "patch"}),
			rule(componentsGroup, []string{o.Plural + "/finalizers"}, []string{"update"}))
	}
	opRules = append(opRules, rule([]string{"rbac.authorization.k8s.io"}, []string{"clusterroles", "clusterrolebindings", "roles", "rolebindings"}, []string{"*"}))
	w.roles = append(w.roles,
		map[string]any{"metadata": map[string]any{"name": "rhods-operator.3.6.0-6kq8d"}, "rules": opRules},
		map[string]any{"metadata": map[string]any{"name": "cluster-admin"}, "rules": []any{rule([]string{"*"}, []string{"*"}, []string{"*"})}},
		// Aggregated user roles the modules ship: no finalizers.
		map[string]any{"metadata": map[string]any{"name": "admin"}, "rules": []any{
			rule(componentsGroup, []string{"aigateways", "rays", "trainers"}, all), rule(componentsGroup, []string{"aigateways/status", "rays/status"}, []string{"get"}),
			rule(componentsGroup, []string{"ogxs"}, []string{"*"}), rule(componentsGroup, []string{"ogxs/status"}, []string{"get"})}},
		map[string]any{"metadata": map[string]any{"name": "odh-observability"}, "rules": []any{rule([]string{"monitoring.coreos.com"}, []string{"*"}, []string{"*"})}},
	)
	w.bindings = append(w.bindings,
		crb("rhods-operator.3.6.0-6kq8d", "rhods-operator.3.6.0-6kq8d", saSubject(SubNS, "redhat-ods-operator-controller-manager")),
		crb("cluster-admin", "cluster-admin", map[string]any{"kind": "Group", "name": "system:masters"}, saSubject("openshift-monitoring", "cluster-monitoring-operator")),
		crb("odh-observability", "odh-observability", saSubject(liveAppsNS, "odh-observability")),
	)
	for _, o := range live36Operators {
		rules := []any{
			rule(componentsGroup, []string{o.Plural}, all, o.ResourceNames...),
			rule(componentsGroup, []string{o.Plural + "/finalizers"}, []string{"update"}, o.ResourceNames...),
			rule(componentsGroup, []string{o.Plural + "/status"}, []string{"get", "patch", "update"}, o.ResourceNames...),
		}
		switch o.Module {
		case "aigateway":
			rules = append(rules, rule(componentsGroup, []string{"aigateways/status", "modelsasservices/status"}, []string{"get", "patch", "update"}))
		case "dashboard":
			rules = append(rules, rule(componentsGroup, []string{"mcplifecycleoperators"}, []string{"get"}, "default"), rule(componentsGroup, []string{"aihubs"}, []string{"get"}, "default-aihub"))
		}
		w.roles = append(w.roles, map[string]any{"metadata": map[string]any{"name": o.Role}, "rules": rules})
		w.bindings = append(w.bindings, crb(o.Role+"binding", o.Role, saSubject(liveAppsNS, o.ServiceAccount)))
		w.deployments = append(w.deployments, platformDeployment(o.Deployment, o.ServiceAccount, "config.opendatahub.io/v1alpha1", "Platform", "default"))
	}
	w.deployments = append(w.deployments,
		platformDeployment("odh-observability", "odh-observability", "config.opendatahub.io/v1alpha1", "Platform", "default"),
		// Operands: owned by their module CR.
		platformDeployment("trustyai-service-operator-controller-manager", "trustyai-service-operator-controller-manager", componentAPIGroup+"/v1alpha1", "TrustyAI", "default-trustyai"),
		platformDeployment("kuberay-operator", "kuberay-operator", componentAPIGroup+"/v1alpha1", "Ray", "default-ray"),
		platformDeployment("rhods-dashboard", "rhods-dashboard", componentAPIGroup+"/v1alpha1", "Dashboard", "default-dashboard"),
	)
	return w
}

func (w *moduleOperatorWorld) dropDeployment(name string) {
	w.deployments = slices.DeleteFunc(w.deployments, func(d map[string]any) bool { return d["metadata"].(map[string]any)["name"] == name })
}

func (f *fakeAPI) routed(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.routes["GET "+path]
	return ok
}

// serve serves the DSCInitialization (unless the test does), the
// ClusterRoles, ClusterRoleBindings and the applications namespace's
// Deployments.
func (w *moduleOperatorWorld) serve(f *fakeAPI) {
	if !f.routed(dsciV2List) {
		f.json("GET", dsciV2List, 200, `{"items":[{"metadata":{"name":"default-dsci"},"spec":{"applicationsNamespace":"`+liveAppsNS+`"}}]}`)
	}
	f.obj("/apis/rbac.authorization.k8s.io/v1/clusterroles", map[string]any{"items": w.roles})
	f.obj("/apis/rbac.authorization.k8s.io/v1/clusterrolebindings", map[string]any{"items": w.bindings})
	f.obj("/apis/apps/v1/namespaces/"+liveAppsNS+"/deployments", map[string]any{"items": w.deployments})
	if !f.routed("/api/v1/namespaces/" + liveAppsNS + "/pods") {
		pods := w.pods
		if pods == nil {
			pods = []map[string]any{}
		}
		f.obj("/api/v1/namespaces/"+liveAppsNS+"/pods", map[string]any{"items": pods})
	}
}

// serveLive36ModuleOperators serves the live 3.6 module operators' RBAC and
// Deployments.
func serveLive36ModuleOperators(f *fakeAPI) { newLive36ModuleOperatorWorld().serve(f) }

// serveLive36Kinds serves the component API with every live kind and Kueue,
// with the verbs the API server lists for a CRD.
func serveLive36Kinds(f *fakeAPI) {
	serveComponentGroup(f)
	verbs := []string{"delete", "deletecollection", "get", "list", "patch", "create", "update", "watch"}
	statusVerbs := []string{"get", "patch", "update"}
	var res []map[string]any
	for _, o := range live36Operators {
		res = append(res, map[string]any{"name": o.Plural, "kind": o.Kind, "verbs": verbs}, map[string]any{"name": o.Plural + "/status", "kind": o.Kind, "verbs": statusVerbs})
	}
	res = append(res, map[string]any{"name": "kueues", "kind": "Kueue", "verbs": verbs}, map[string]any{"name": "newmodules", "kind": "NewModule", "verbs": verbs})
	f.obj("/apis/components.platform.opendatahub.io/v1alpha1", map[string]any{"resources": res})
}

// serveInstalledOperatorOwningKueue serves the installed 3.6 CSV, which owns
// the Kueue CRD (the bundle ships it; config/rhoai/manifests/bases).
func serveInstalledOperatorOwningKueue(f *fakeAPI) {
	f.json("GET", namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName), 200, `{"status":{"installedCSV":"rhods-operator.3.6.0"}}`)
	f.json("GET", namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "rhods-operator.3.6.0"), 200,
		`{"metadata":{"name":"rhods-operator.3.6.0"},"spec":{"version":"3.6.0","customresourcedefinitions":{"owned":[{"name":"datascienceclusters.datasciencecluster.opendatahub.io"},{"name":"kueues.components.platform.opendatahub.io"}]},"install":{"spec":{"deployments":[{"name":"rhods-operator"}]}}},"status":{"phase":"Succeeded"}}`)
}

// On the live 3.6 data every module resolves to its operator, including
// aigateway and mcplifecycleoperator (missing from the former fixed list),
// and Kueue to the RHOAI operator. The RBAC is read once.
func TestModuleOperatorFinderLive36(t *testing.T) {
	f, c := newFakeAPI(t)
	serveLive36Kinds(f)
	serveLive36ModuleOperators(f)
	serveInstalledOperatorOwningKueue(f)
	finder := newModuleOperatorFinder(c, nil)
	for _, o := range live36Operators {
		cr := "default-" + o.Module
		if o.Module == "mcplifecycleoperator" {
			cr = "default"
		}
		op, why, err := finder.find(o.Module, cr)
		if err != nil || op == nil || op.Namespace != liveAppsNS || op.Name != o.Deployment || op.RHOAI {
			t.Errorf("%s: op %+v, why %q, err %v", o.Module, op, why, err)
			continue
		}
		if !strings.Contains(op.Via, "Platform default") || !strings.Contains(op.Via, o.Plural+"/finalizers") {
			t.Errorf("%s via %q", o.Module, op.Via)
		}
	}
	op, why, err := finder.find("kueue", "default-kueue")
	if err != nil || op == nil || !op.RHOAI || op.ref() != SubNS+"/rhods-operator" || !strings.Contains(op.Via, "owns CRD kueues.components.platform.opendatahub.io") {
		t.Fatalf("kueue: %+v %q %v", op, why, err)
	}
	// A kind nothing in the namespace may update, and that the CSV does not
	// own, is unknown.
	if op, why, err := finder.find("newmodule", "default"); op != nil || err != nil || !strings.Contains(why, "does not own CRD newmodules") {
		t.Fatalf("newmodule: %+v %q %v", op, why, err)
	}
	for _, p := range []string{"/apis/rbac.authorization.k8s.io/v1/clusterroles", "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings", "/apis/apps/v1/namespaces/" + liveAppsNS + "/deployments"} {
		if n := len(f.requests("GET", p)); n != 1 {
			t.Errorf("%s read %d times", p, n)
		}
	}
}

// Ambiguous, missing and over-broad grants leave the operator unknown.
func TestModuleOperatorFinderUnknown(t *testing.T) {
	trusty := liveOperator("trustyai")
	for _, tc := range []struct {
		name, module, cr, want, wantOp string
		edit                           func(w *moduleOperatorWorld)
	}{
		{"operator Deployment gone", "aigateway", "default-aigateway", "ai-gateway-operator may update aigateways and aigateways/finalizers, but no Deployment owned by the Platform runs as it", "",
			func(w *moduleOperatorWorld) { w.dropDeployment("ai-gateway-operator") }},
		{"two equally specific Deployments", "trustyai", "default-trustyai", "several Deployments in redhat-ods-applications may update trustyais/finalizers: trustyai-copy, trustyai-operator-module-controller-manager", "",
			func(w *moduleOperatorWorld) {
				w.deployments = append(w.deployments, platformDeployment("trustyai-copy", trusty.ServiceAccount, "config.opendatahub.io/v1alpha1", "Platform", "default"))
			}},
		{"not owned by the Platform", "mcplifecycleoperator", "default", "no Deployment owned by the Platform runs as it", "",
			func(w *moduleOperatorWorld) {
				o := liveOperator("mcplifecycleoperator")
				w.dropDeployment(o.Deployment)
				w.deployments = append(w.deployments, platformDeployment(o.Deployment, o.ServiceAccount, componentAPIGroup+"/v1alpha1", "MCPLifecycleOperator", "default"))
			}},
		{"cluster-admin Deployment does not stand in for a gone operator", "trustyai", "default-trustyai", "no Deployment owned by the Platform runs as it", "",
			func(w *moduleOperatorWorld) {
				w.dropDeployment(trusty.Deployment)
				w.bindings = append(w.bindings, crb("wild", "cluster-admin", saSubject(liveAppsNS, "wild")))
				w.deployments = append(w.deployments, platformDeployment("wild-operator", "wild", "config.opendatahub.io/v1alpha1", "Platform", "default"))
			}},
		{"a group binding identifies nothing", "trustyai", "default-trustyai", "no ServiceAccount in redhat-ods-applications may update trustyais", "",
			func(w *moduleOperatorWorld) {
				w.bindings = slices.DeleteFunc(w.bindings, func(b map[string]any) bool { return b["metadata"].(map[string]any)["name"] == trusty.Role+"binding" })
				w.bindings = append(w.bindings, crb("all-sas", trusty.Role, map[string]any{"kind": "Group", "name": "system:serviceaccounts"}))
			}},
		{"kserve rules name only default-kserve", "kserve", "other-kserve", "no ServiceAccount in redhat-ods-applications may update kserves", "", func(*moduleOperatorWorld) {}},
		// Resolved cases next to them.
		{"the specific grant beats a verb wildcard and cluster-admin", "trustyai", "default-trustyai", "", trusty.Deployment,
			func(w *moduleOperatorWorld) {
				w.roles = append(w.roles, map[string]any{"metadata": map[string]any{"name": "broad"}, "rules": []any{rule(componentsGroup, []string{"trustyais", "trustyais/finalizers"}, []string{"*"})}})
				w.bindings = append(w.bindings, crb("broad", "broad", saSubject(liveAppsNS, "broad")), crb("wild", "cluster-admin", saSubject(liveAppsNS, "wild")))
				w.deployments = append(w.deployments, platformDeployment("broad-operator", "broad", "config.opendatahub.io/v1alpha1", "Platform", "default"),
					platformDeployment("wild-operator", "wild", "config.opendatahub.io/v1alpha1", "Platform", "default"))
			}},
		{"owned by the DataScienceCluster", "ray", "default-ray", "", liveOperator("ray").Deployment,
			func(w *moduleOperatorWorld) {
				o := liveOperator("ray")
				w.dropDeployment(o.Deployment)
				w.deployments = append(w.deployments, platformDeployment(o.Deployment, o.ServiceAccount, "datasciencecluster.opendatahub.io/v2", "DataScienceCluster", "default-dsc"))
			}},
		{"status when no ServiceAccount may update finalizers", "trainer", "default-trainer", "", liveOperator("trainer").Deployment,
			func(w *moduleOperatorWorld) {
				o := liveOperator("trainer")
				for _, r := range w.roles {
					if r["metadata"].(map[string]any)["name"] == o.Role {
						r["rules"] = []any{rule(componentsGroup, []string{"trainers"}, []string{"update"}), rule(componentsGroup, []string{"trainers/status"}, []string{"update"})}
					}
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFakeAPI(t)
			serveLive36Kinds(f)
			serveInstalledOperatorOwningKueue(f)
			w := newLive36ModuleOperatorWorld()
			tc.edit(w)
			w.serve(f)
			op, why, err := newModuleOperatorFinder(c, nil).find(tc.module, tc.cr)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantOp != "" {
				if op == nil || op.Name != tc.wantOp {
					t.Fatalf("op %+v, why %q", op, why)
				}
				return
			}
			if op != nil || !strings.Contains(why, tc.want) {
				t.Fatalf("op %+v, why %q", op, why)
			}
		})
	}
}

// Read errors are errors, and the removal checks refuse on them (fail
// closed); an unknown operator gets the unchanged wording.
func TestModuleOperatorFinderFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		edit       func(f *fakeAPI)
	}{
		{"ClusterRoles forbidden", "list ClusterRoles", func(f *fakeAPI) {
			f.status("GET", "/apis/rbac.authorization.k8s.io/v1/clusterroles", http.StatusForbidden, "Forbidden")
		}},
		{"ClusterRoleBindings fail", "list ClusterRoleBindings", func(f *fakeAPI) {
			f.status("GET", "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings", http.StatusInternalServerError, "InternalError")
		}},
		{"Deployments forbidden", "list Deployments in redhat-ods-applications", func(f *fakeAPI) {
			f.status("GET", "/apis/apps/v1/namespaces/"+liveAppsNS+"/deployments", http.StatusForbidden, "Forbidden")
		}},
		{"no DSCInitialization", "no DSCInitialization exists", func(f *fakeAPI) {
			f.json("GET", dsciV2List, 200, `{"items":[]}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFakeAPI(t)
			serveInstalledOperator(f)
			serveLive36Kinds(f)
			f.json("GET", "/apis/apps/v1/namespaces/redhat-ods-operator/deployments/rhods-operator", http.StatusOK, `{"status":{"readyReplicas":1}}`)
			f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1/aigateways", http.StatusOK, `{"items":[{"metadata":{"name":"default-aigateway","finalizers":["platform.opendatahub.io/finalizer"]}}]}`)
			tc.edit(f) // registered first: serve keeps a DSCInitialization the test set
			w := newLive36ModuleOperatorWorld()
			if !f.routed("/apis/rbac.authorization.k8s.io/v1/clusterroles") {
				f.obj("/apis/rbac.authorization.k8s.io/v1/clusterroles", map[string]any{"items": w.roles})
			}
			if !f.routed("/apis/rbac.authorization.k8s.io/v1/clusterrolebindings") {
				f.obj("/apis/rbac.authorization.k8s.io/v1/clusterrolebindings", map[string]any{"items": w.bindings})
			}
			if !f.routed("/apis/apps/v1/namespaces/" + liveAppsNS + "/deployments") {
				f.obj("/apis/apps/v1/namespaces/"+liveAppsNS+"/deployments", map[string]any{"items": w.deployments})
			}
			if !f.routed(dsciV2List) {
				f.json("GET", dsciV2List, 200, `{"items":[{"metadata":{"name":"default-dsci"},"spec":{"applicationsNamespace":"`+liveAppsNS+`"}}]}`)
			}
			if _, _, err := newModuleOperatorFinder(c, nil).find("aigateway", "default-aigateway"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v", err)
			}
			got := strings.Join(newRemovalChecker(c).moduleBlockers("aigateway"), "; ")
			if !strings.Contains(got, "could not identify its operator") || !strings.Contains(got, tc.want) || !strings.Contains(got, "won't remove it") {
				t.Fatalf("blockers = %q", got)
			}
		})
	}
}

// The removal checks on the live 3.6 data: aigateway and
// mcplifecycleoperator (refused before with "can't confirm its operator is
// running") pass when their operators run and name them when not; an
// unknown operator keeps the old wording.
func TestRemovalChecksUseDiscoveredOperators(t *testing.T) {
	f, c := newFakeAPI(t)
	serveInstalledOperatorOwningKueue(f)
	serveLive36Kinds(f)
	w := newLive36ModuleOperatorWorld()
	w.dropDeployment(liveOperator("trustyai").Deployment)
	w.serve(f)
	f.json("GET", "/apis/apps/v1/namespaces/redhat-ods-operator/deployments/rhods-operator", http.StatusOK, `{"status":{"readyReplicas":1}}`)
	for _, m := range []string{"aigateway", "mcplifecycleoperator", "kueue", "trustyai"} {
		name := "default-" + m
		if m == "mcplifecycleoperator" {
			name = "default"
		}
		b, _ := json.Marshal(map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"name": name, "finalizers": []string{"platform.opendatahub.io/finalizer"}}}}})
		f.json("GET", "/apis/components.platform.opendatahub.io/v1alpha1/"+m+"s", http.StatusOK, string(b))
	}
	f.json("GET", "/apis/apps/v1/namespaces/"+liveAppsNS+"/deployments/ai-gateway-operator", http.StatusOK, `{"status":{"readyReplicas":1}}`)
	f.json("GET", "/apis/apps/v1/namespaces/"+liveAppsNS+"/deployments/mcp-lifecycle-module-operator-controller-manager", http.StatusOK, `{"status":{"readyReplicas":0}}`)
	rc := newRemovalChecker(c)
	if got := rc.moduleBlockers("aigateway"); len(got) != 0 {
		t.Errorf("aigateway: %q", got)
	}
	if got := strings.Join(rc.moduleBlockers("mcplifecycleoperator"), "; "); !strings.Contains(got, "its operator redhat-ods-applications/mcp-lifecycle-module-operator-controller-manager removes the finalizers of the module CR (finalizer platform.opendatahub.io/finalizer) and its operands, but Deployment redhat-ods-applications/mcp-lifecycle-module-operator-controller-manager has no ready pod") {
		t.Errorf("mcplifecycleoperator: %q", got)
	}
	if got := rc.moduleBlockers("kueue"); len(got) != 0 {
		t.Errorf("kueue (the RHOAI operator's own): %q", got)
	}
	if got := strings.Join(rc.moduleBlockers("trustyai"), "; "); got != unknownOperatorBlocker() {
		t.Errorf("trustyai without its operator: %q", got)
	}
	if got := strings.Join(rc.partBlockers("trustyai"), "; "); got != unknownOperatorBlocker() {
		t.Errorf("trustyai part: %q", got)
	}
}

// Diagnostics "Platform modules" names the discovered operator of a stuck
// module.
func TestStuckModuleNamesDiscoveredOperator(t *testing.T) {
	f, c := newFakeAPI(t)
	serveLive36Kinds(f)
	serveLive36ModuleOperators(f)
	serveInstalledOperator(f)
	f.json("GET", "/apis/apps/v1/namespaces/"+liveAppsNS+"/deployments/ai-gateway-operator", http.StatusOK, `{"status":{"readyReplicas":0}}`)
	p := stuckModuleProblem(c, newModuleOperatorFinder(c, nil), stuckModule{Module: "aigateway", Name: "default-aigateway", Finalizers: []string{"platform.opendatahub.io/finalizer"}})
	ev := strings.Join(p.Evidence, "\n")
	if !strings.Contains(ev, "owned by Platform default; its ServiceAccount ai-gateway-operator may update aigateways and aigateways/finalizers") ||
		!strings.Contains(ev, "has no ready pod") || p.Fix != "Start redhat-ods-applications/ai-gateway-operator again; it removes the finalizer." {
		t.Fatalf("%+v", p)
	}
	q := stuckModuleProblem(c, newModuleOperatorFinder(c, nil), stuckModule{Module: "newmodule", Name: "default"})
	if !strings.Contains(strings.Join(q.Evidence, "\n"), "Operator: not identified") {
		t.Fatalf("%+v", q)
	}
}

// The databaseservice module's CR is in services.platform.opendatahub.io
// (rhods-operator rhoai-3.6 pkg/cluster/gvk/gvk.go), its operator's
// ClusterRole grants databaseservices and databaseservices/finalizers in
// that group (prefetched-charts/databaseservice/templates/
// rbac.authorization.k8s.io_v1_clusterrole.yaml:204-221).
const servicesV1 = "/apis/services.platform.opendatahub.io/v1alpha1/"

func serveDatabaseService(f *fakeAPI, w *moduleOperatorWorld, deployment bool) {
	f.json("GET", "/apis/"+servicesAPIGroup, 200, `{"kind":"APIGroup","name":"services.platform.opendatahub.io","preferredVersion":{"groupVersion":"services.platform.opendatahub.io/v1alpha1","version":"v1alpha1"}}`)
	f.obj("/apis/services.platform.opendatahub.io/v1alpha1", map[string]any{"resources": []any{
		map[string]any{"name": "databaseservices", "kind": "DatabaseService", "verbs": []string{"get", "list", "patch", "update"}},
		map[string]any{"name": "databaseservices/status", "kind": "DatabaseService", "verbs": []string{"get", "patch", "update"}},
		map[string]any{"name": "auths", "kind": "Auth", "verbs": []string{"get", "list", "patch", "update"}},
	}})
	sg := []string{servicesAPIGroup}
	w.roles = append(w.roles, map[string]any{"metadata": map[string]any{"name": "databaseservice-manager-role"}, "rules": []any{
		rule(sg, []string{"databaseservices"}, []string{"get", "list", "update", "watch"}),
		rule(sg, []string{"databaseservices/finalizers"}, []string{"update"}),
		rule(sg, []string{"databaseservices/status"}, []string{"get", "patch", "update"}),
	}})
	w.bindings = append(w.bindings, crb("databaseservice-manager-rolebinding", "databaseservice-manager-role", saSubject(liveAppsNS, "databaseservice-operator")))
	if deployment {
		w.deployments = append(w.deployments, platformDeployment("databaseservice-operator", "databaseservice-operator", "config.opendatahub.io/v1alpha1", "Platform", "default"))
	}
}

// A14: the finder, the removal checks, the stuck-CR cleanup and the
// Platform modules check find the databaseservice module in the services
// group; a kind of that group that is not a module (Auth) stays unknown.
func TestDatabaseServiceModuleInTheServicesGroup(t *testing.T) {
	f, c := newFakeAPI(t)
	serveLive36Kinds(f)
	serveInstalledOperatorOwningKueue(f)
	w := newLive36ModuleOperatorWorld()
	serveDatabaseService(f, w, true)
	w.serve(f)
	finder := newModuleOperatorFinder(c, nil)
	op, why, err := finder.find("databaseservice", "default-databaseservice")
	if err != nil || op == nil || op.Name != "databaseservice-operator" || !strings.Contains(op.Via, "databaseservices/finalizers") {
		t.Fatalf("databaseservice: %+v %q %v", op, why, err)
	}
	if op, why, err := finder.find("auth", "auth"); op != nil || err != nil || !strings.Contains(why, "does not own CRD auths.services.platform.opendatahub.io") {
		t.Fatalf("auth: %+v %q %v", op, why, err)
	}
	// A rule for the same resource in the components group grants nothing.
	f.json("GET", servicesV1+"databaseservices", 200, `{"items":[{"metadata":{"name":"default-databaseservice","finalizers":["platform.opendatahub.io/finalizer"]}}]}`)
	f.json("GET", "/apis/apps/v1/namespaces/"+liveAppsNS+"/deployments/databaseservice-operator", 200, `{"status":{"readyReplicas":1}}`)
	f.json("GET", "/apis/apps/v1/namespaces/redhat-ods-operator/deployments/rhods-operator", 200, `{"status":{"readyReplicas":1}}`)
	if got := newRemovalChecker(c).moduleBlockers("databaseservice"); len(got) != 0 {
		t.Fatalf("removal of databaseservice refused: %q", got)
	}
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	f.json("GET", servicesV1+"databaseservices", 200, `{"items":[{"metadata":{"name":"default-databaseservice","uid":"u","resourceVersion":"3","deletionTimestamp":"`+old+`","finalizers":["platform.opendatahub.io/finalizer"]}}]}`)
	for _, o := range live36Operators {
		f.obj(componentsV1+o.Plural, map[string]any{"items": []any{}})
	}
	f.obj(componentsV1+"kueues", map[string]any{"items": []any{}})
	f.obj(componentsV1+"newmodules", map[string]any{"items": []any{}})
	f.obj(servicesV1+"auths", map[string]any{"items": []any{}})
	stuck, err := stuckModuleCRs(c, time.Now())
	if err != nil || len(stuck) != 1 || stuck[0].Module != "databaseservice" || stuck[0].CRD != "databaseservices.services.platform.opendatahub.io" {
		t.Fatalf("stuck %+v %v", stuck, err)
	}
	if p := stuckModuleProblem(c, finder, stuck[0]); p.TechnicalCmd != "oc get databaseservices.services.platform.opendatahub.io default-databaseservice -o yaml" {
		t.Fatalf("cmd %q", p.TechnicalCmd)
	}
	_, warnings := cleanupStuckComponentCRs(c)
	cleanupWarnings(t, warnings, "databaseservices/default-databaseservice has been deleting since "+old+", but its operator redhat-ods-applications/databaseservice-operator still exists")
	assertWrites(t, f)
}

// The services group's rule does not stand in for the components group:
// RBAC is matched in the kind's own group.
func TestModuleOperatorRBACIsMatchedInTheKindsGroup(t *testing.T) {
	f, c := newFakeAPI(t)
	serveLive36Kinds(f)
	serveInstalledOperatorOwningKueue(f)
	w := newLive36ModuleOperatorWorld()
	serveDatabaseService(f, w, true)
	for _, r := range w.roles {
		if r["metadata"].(map[string]any)["name"] == "databaseservice-manager-role" {
			r["rules"] = []any{rule(componentsGroup, []string{"databaseservices", "databaseservices/finalizers", "databaseservices/status"}, []string{"update"})}
		}
	}
	w.serve(f)
	if op, why, err := newModuleOperatorFinder(c, nil).find("databaseservice", "default-databaseservice"); op != nil || err != nil || !strings.Contains(why, "no ServiceAccount in redhat-ods-applications may update databaseservices") {
		t.Fatalf("%+v %q %v", op, why, err)
	}
}

// B(b): the operator is gone only when no Pod that may still run (whatever
// controls it) runs as its ServiceAccounts; Succeeded and Failed Pods do not
// count; a Pod list error is an error (callers leave the finalizer).
func TestFinalizerOwnerCountsPods(t *testing.T) {
	trusty := liveOperator("trustyai")
	for _, tc := range []struct {
		name     string
		pods     []map[string]any
		podsErr  bool
		wantGone bool
		wantPods []string
	}{
		{"no Pods", nil, false, true, nil},
		{"a ReplicaSet left by the deleted Deployment", []map[string]any{operatorPod("op-rs-1", trusty.ServiceAccount, "Running", "ReplicaSet")}, false, false, []string{liveAppsNS + "/op-rs-1"}},
		{"a bare Pod, Pending", []map[string]any{operatorPod("bare", trusty.ServiceAccount, "Pending", "")}, false, false, []string{liveAppsNS + "/bare"}},
		{"a StatefulSet Pod in an unknown phase", []map[string]any{operatorPod("sts-0", trusty.ServiceAccount, "", "StatefulSet")}, false, false, []string{liveAppsNS + "/sts-0"}},
		{"finished Pods", []map[string]any{operatorPod("done", trusty.ServiceAccount, "Succeeded", "Job"), operatorPod("failed", trusty.ServiceAccount, "Failed", "")}, false, true, nil},
		{"another ServiceAccount", []map[string]any{operatorPod("other", "someone-else", "Running", "ReplicaSet")}, false, true, nil},
		{"Pods unreadable", nil, true, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFakeAPI(t)
			serveLive36Kinds(f)
			w := newLive36ModuleOperatorWorld()
			w.dropDeployment(trusty.Deployment)
			w.pods = tc.pods
			if tc.podsErr {
				f.status("GET", "/api/v1/namespaces/"+liveAppsNS+"/pods", http.StatusForbidden, "Forbidden")
			}
			w.serve(f)
			owner, err := newModuleOperatorFinder(c, nil).finalizerOwner("trustyai", "default-trustyai")
			if tc.podsErr {
				if err == nil || !strings.Contains(err.Error(), "list Pods in redhat-ods-applications") {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil || owner.gone() != tc.wantGone || !slices.Equal(owner.Pods, tc.wantPods) {
				t.Fatalf("owner %+v gone=%v err %v", owner, owner.gone(), err)
			}
		})
	}
}
