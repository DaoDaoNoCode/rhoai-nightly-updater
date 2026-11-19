package cluster

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// The dashboard module's operator as the live 3.6 cluster runs it, under
// names nothing in the code knows (fixtureAppsNS, fixtureOperatorName).
const discoveredOperatorBody = `{"metadata":{"name":"dashboard-module-manager","uid":"operator-uid","resourceVersion":"1","annotations":{}},"spec":{"replicas":1,"selector":{"matchLabels":{"app":"dm"}},"template":{"spec":{"containers":[{"name":"manager","env":[{"name":"RELATED_IMAGE_ODH_DASHBOARD_IMAGE","value":"release-host"}]}]}}},"status":{"replicas":1,"readyReplicas":1,"updatedReplicas":1}}`

func (f *fakeAPI) allRequests() []fakeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeReq(nil), f.reqs...)
}

func rhodsDashboardItem(annotations map[string]any) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": dashboardDeploymentName, "uid": "dash-uid", "annotations": annotations,
		"ownerReferences": []any{map[string]any{"apiVersion": componentAPIGroup + "/v1alpha1", "kind": "Dashboard", "name": "default-dashboard", "uid": "dash-owner", "controller": true}}},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"serviceAccountName": "rhods-dashboard", "containers": []any{map[string]any{"name": "rhods-dashboard", "image": "release-host"}}}}}}
}

// serveModuleCluster is a 3.6-shaped cluster (dashboard module).
func serveModuleCluster(f *fakeAPI) {
	serveDashboardModule(f, fixtureAppsNS, fixtureOperatorName, discoveredOperatorBody, rhodsDashboardItem(nil))
}

// serveInTreeDashboardCluster is a 3.4-shaped cluster: Dashboard is served,
// but only the RHOAI operator (in SubNS) may update dashboards and their
// finalizers, and its installed CSV owns the Dashboard CRD.
func serveInTreeDashboardCluster(f *fakeAPI, dashboardAnnotations map[string]any) {
	serveComponentGroup(f)
	f.json("GET", "/apis/"+componentAPIGroup+"/v1alpha1", 200, `{"resources":[{"name":"dashboards","kind":"Dashboard","verbs":["get","list","update"]}]}`)
	serveDSCI(f, "redhat-ods-applications")
	f.obj("/apis/rbac.authorization.k8s.io/v1/clusterroles", map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"name": "rhods-operator.3.4.0-x"}, "rules": []any{
		rule(componentsGroup, []string{"dashboards"}, []string{"update"}), rule(componentsGroup, []string{"dashboards/finalizers"}, []string{"update"})}}}})
	f.obj("/apis/rbac.authorization.k8s.io/v1/clusterrolebindings", map[string]any{"items": []any{crb("rhods-operator.3.4.0-x", "rhods-operator.3.4.0-x", saSubject(SubNS, "redhat-ods-operator-controller-manager"))}})
	f.obj("/apis/apps/v1/namespaces/redhat-ods-applications/deployments", map[string]any{"items": []any{rhodsDashboardItem(dashboardAnnotations)}})
	f.obj(namespacedPath("apps/v1", "deployments", "redhat-ods-applications", dashboardDeploymentName), rhodsDashboardItem(dashboardAnnotations))
	f.json("GET", namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName), 200, `{"status":{"installedCSV":"rhods-operator.3.4.0"}}`)
	f.json("GET", namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "rhods-operator.3.4.0"), 200,
		`{"metadata":{"name":"rhods-operator.3.4.0"},"spec":{"version":"3.4.0","customresourcedefinitions":{"owned":[{"name":"dashboards.components.platform.opendatahub.io"}]},"install":{"spec":{"deployments":[{"name":"rhods-operator"}]}}},"status":{"phase":"Succeeded"}}`)
}

// Which flow Dashboard Dev takes, for every shape of cluster: the module
// operator by discovery, the legacy flow only when the cluster definitely
// has no dashboard module, and an error (refuse) otherwise.
func TestDiscoverDashboardTarget(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      func(f *fakeAPI)
		operator   string // namespace/name, "" for none
		legacy     string // substring of the legacy reason
		ns         string
		errContain string
		noDSCI     bool
	}{
		{name: "3.6: the module operator, by RBAC and owner", setup: serveModuleCluster, operator: fixtureAppsNS + "/" + fixtureOperatorName, ns: fixtureAppsNS},
		{name: "3.4: the RHOAI operator reconciles Dashboard (CSV owns the CRD)", setup: func(f *fakeAPI) { serveInTreeDashboardCluster(f, nil) },
			legacy: "the RHOAI operator reconciles Dashboard itself", ns: "redhat-ods-applications"},
		{name: "component API not served", setup: func(f *fakeAPI) { serveDSCI(f, "old-apps") }, legacy: "API is not served", ns: "old-apps"},
		{name: "no Dashboard kind served", setup: func(f *fakeAPI) {
			serveComponentGroup(f)
			f.json("GET", "/apis/"+componentAPIGroup+"/v1alpha1", 200, `{"resources":[{"name":"rays","kind":"Ray"}]}`)
			serveDSCI(f, "old-apps")
		}, legacy: "no Dashboard kind", ns: "old-apps"},
		{name: "nothing installed: legacy without a namespace", setup: func(f *fakeAPI) {}, legacy: "API is not served"},
		{name: "component API unreadable", setup: func(f *fakeAPI) { f.status("GET", "/apis/"+componentAPIGroup, 503, "ServiceUnavailable") }, errContain: "503"},
		{name: "ClusterRoles unreadable", setup: func(f *fakeAPI) {
			serveModuleCluster(f)
			f.status("GET", "/apis/rbac.authorization.k8s.io/v1/clusterroles", 403, "Forbidden")
		}, errContain: "ClusterRoles"},
		{name: "DSCInitialization unreadable", setup: func(f *fakeAPI) {
			serveModuleCluster(f)
			f.status("GET", dsciV2List, 500, "InternalError")
		}, errContain: "DSCInitialization"},
		{name: "module served but no DSCInitialization", setup: func(f *fakeAPI) {
			serveModuleCluster(f)
			f.json("GET", dsciV2List, 200, `{"items":[]}`)
		}, errContain: "no DSCInitialization exists", noDSCI: true},
		{name: "operator Deployment gone (binding left)", setup: func(f *fakeAPI) {
			serveModuleCluster(f)
			f.obj("/apis/apps/v1/namespaces/"+fixtureAppsNS+"/deployments", map[string]any{"items": []any{rhodsDashboardItem(nil)}})
		}, errContain: "no Deployment owned by the Platform runs as"},
		{name: "two Deployments run as the operator's ServiceAccount", setup: func(f *fakeAPI) {
			serveModuleCluster(f)
			var a, b dashboardDeployment
			a.Metadata.Name, a.Metadata.UID = "dm-a", "a"
			b.Metadata.Name, b.Metadata.UID = "dm-b", "b"
			f.obj("/apis/apps/v1/namespaces/"+fixtureAppsNS+"/deployments", map[string]any{"items": []any{
				dashboardModuleOperatorListItem(a), dashboardModuleOperatorListItem(b)}})
		}, errContain: "several Deployments"},
		{name: "Dashboard served, nobody may update it, CSV does not own it", setup: func(f *fakeAPI) {
			serveInTreeDashboardCluster(f, nil)
			f.json("GET", namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "rhods-operator.3.4.0"), 200,
				`{"metadata":{"name":"rhods-operator.3.4.0"},"spec":{"version":"3.4.0","install":{"spec":{"deployments":[{"name":"rhods-operator"}]}}}}`)
		}, errContain: "does not own CRD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFakeAPI(t)
			tc.setup(f)
			target, err := discoverDashboardTarget(c)
			if tc.errContain != "" {
				var opErr *dashboardOperatorError
				if err == nil || !errors.As(err, &opErr) || !strings.Contains(err.Error(), tc.errContain) {
					t.Fatalf("target=%+v err=%v, want a dashboardOperatorError with %q", target, err, tc.errContain)
				}
				if errors.Is(err, errNoDSCInitialization) != tc.noDSCI {
					t.Fatalf("err=%v: errNoDSCInitialization=%v, want %v", err, !tc.noDSCI, tc.noDSCI)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if target.Operator != nil {
				got = target.Operator.ref()
			}
			if got != tc.operator || !strings.Contains(target.Legacy, tc.legacy) || (tc.legacy == "") != (target.Legacy == "") || target.Namespace != tc.ns {
				t.Fatalf("target=%+v (operator %q), want operator %q legacy %q namespace %q", target, got, tc.operator, tc.legacy, tc.ns)
			}
		})
	}
}

// When the operator cannot be identified, nothing is changed and the legacy
// flow is not taken: no write, and rhods-dashboard and the RHOAI operator's
// images are never read for a legacy patch.
func TestDashboardDevRefusesWhenTheOperatorIsUnknown(t *testing.T) {
	unknown := map[string]func(f *fakeAPI){
		"component API unreadable": func(f *fakeAPI) { f.status("GET", "/apis/"+componentAPIGroup, 503, "ServiceUnavailable") },
		"operator Deployment gone": func(f *fakeAPI) {
			serveModuleCluster(f)
			f.obj("/apis/apps/v1/namespaces/"+fixtureAppsNS+"/deployments", map[string]any{"items": []any{rhodsDashboardItem(map[string]any{"opendatahub.io/managed": "false"})}})
		},
		"RBAC unreadable": func(f *fakeAPI) {
			serveModuleCluster(f)
			f.status("GET", "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings", 403, "Forbidden")
		},
	}
	for name, setup := range unknown {
		t.Run(name, func(t *testing.T) {
			allPRImagesPublished(t)
			f, c := newFakeAPI(t)
			setup(f)
			if r, err := DeployPRImage(c, 7); err != nil || r.Success || !strings.Contains(r.Message, "cannot identify the dashboard module's operator") && !strings.Contains(r.Message, "Cannot identify the dashboard module's operator") {
				t.Fatalf("deploy = %+v, %v", r, err)
			}
			if r, err := RevertDashboardImage(c); err != nil || r.Success || !strings.Contains(r.Message, "No changes were made") {
				t.Fatalf("revert = %+v, %v", r, err)
			}
			if r, err := DeployDashboardMainWithFlavor(c, ""); err != nil || r.Success {
				t.Fatalf("main = %+v, %v", r, err)
			}
			if _, err := DashboardDevActive(c); err == nil {
				t.Fatal("DashboardDevActive: no error for an unknown operator")
			}
			if err := RevertDashboardDevForOperation(c); err == nil {
				t.Fatal("RevertDashboardDevForOperation: no error for an unknown operator")
			}
			if w := f.writes(); len(w) != 0 {
				t.Fatalf("writes: %v", w)
			}
			for _, r := range f.allRequests() {
				if strings.HasSuffix(r.Path, "/deployments/"+dashboardDeploymentName) || strings.HasSuffix(r.Path, "/deployments/rhods-operator") {
					t.Fatalf("the legacy flow read %s", r.Path)
				}
			}
		})
	}
}

// DashboardDevActive picks the legacy check only on definite absence, and
// reads rhods-dashboard in the discovered namespace then.
func TestDashboardDevActiveLegacySelection(t *testing.T) {
	notServed := func(dashboard string) func(f *fakeAPI) {
		return func(f *fakeAPI) {
			serveDSCI(f, "old-apps")
			if dashboard != "" {
				f.json("GET", namespacedPath("apps/v1", "deployments", "old-apps", dashboardDeploymentName), 200, dashboard)
			}
		}
	}
	for _, tc := range []struct {
		name   string
		setup  func(f *fakeAPI)
		active bool
	}{
		{"legacy clean", notServed(`{"metadata":{},"spec":{"template":{"spec":{"containers":[{"name":"rhods-dashboard","image":"registry.redhat.io/rhoai/odh-dashboard-rhel9@sha256:1"}]}}}}`), false},
		{"legacy unmanaged", notServed(`{"metadata":{"annotations":{"opendatahub.io/managed":"false"}}}`), true},
		{"legacy PR image", notServed(`{"metadata":{},"spec":{"template":{"spec":{"containers":[{"name":"gen-ai-ui","image":"quay.io/opendatahub/odh-mod-arch-gen-ai:pr-12"}]}}}}`), true},
		{"legacy, no dashboard", notServed(""), false},
		{"no DSCInitialization at all", func(f *fakeAPI) {}, false},
		{"module served, DSCInitialization gone (half uninstalled)", func(f *fakeAPI) {
			serveModuleCluster(f)
			f.json("GET", dsciV2List, 200, `{"items":[]}`)
		}, false},
		{"3.4 in-tree, unmanaged", func(f *fakeAPI) { serveInTreeDashboardCluster(f, map[string]any{"opendatahub.io/managed": "false"}) }, true},
		{"3.6 module, clean", serveModuleCluster, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFakeAPI(t)
			tc.setup(f)
			active, err := DashboardDevActive(c)
			if err != nil || active != tc.active {
				t.Fatalf("active=%v err=%v", active, err)
			}
		})
	}
}

// The legacy flow (3.4 shape) reads the dashboard images from the
// Deployment the installed CSV installs, not from a fixed name.
func TestLegacyRevertReadsTheCSVsOperatorDeployment(t *testing.T) {
	f, c := newFakeAPI(t)
	serveInTreeDashboardCluster(f, map[string]any{"opendatahub.io/managed": "false", legacyOriginalManagedAnnotation: `{"present":false}`})
	f.json("GET", namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "rhods-operator.3.4.0"), 200,
		`{"metadata":{"name":"rhods-operator.3.4.0"},"spec":{"version":"3.4.0","customresourcedefinitions":{"owned":[{"name":"dashboards.components.platform.opendatahub.io"}]},"install":{"spec":{"deployments":[{"name":"renamed-operator"}]}}}}`)
	f.json("GET", namespacedPath("apps/v1", "deployments", SubNS, "renamed-operator"), 200,
		`{"spec":{"template":{"spec":{"containers":[{"env":[{"name":"RELATED_IMAGE_ODH_DASHBOARD_IMAGE","value":"release-host"}]}]}}}}`)
	f.json("PATCH", namespacedPath("apps/v1", "deployments", "redhat-ods-applications", dashboardDeploymentName), 200, `{}`)
	r, err := RevertDashboardImage(c)
	if err != nil || !r.Success {
		t.Fatalf("revert = %+v, %v", r, err)
	}
	if w := f.writes(); !slices.Equal(w, []string{"PATCH " + namespacedPath("apps/v1", "deployments", "redhat-ods-applications", dashboardDeploymentName)}) {
		t.Fatalf("writes = %v", w)
	}
}

// Pause and revert act on the discovered Deployment in the discovered
// namespace, and the state names it.
func TestDashboardDevPauseRevertRoundTripOnTheDiscoveredOperator(t *testing.T) {
	f, c := newDashboardDevFixture(t)
	mockDashboardTags(t, allFixtureTags("odh-pr-1")...)
	result := mustDeploy(t, c, "pr", 1, "")
	if *f.operator.Spec.Replicas != 0 || f.operator.Metadata.Annotations[dashboardDevAnnotation] == "" {
		t.Fatalf("not paused: replicas=%d annotations=%v", *f.operator.Spec.Replicas, f.operator.Metadata.Annotations)
	}
	if !strings.Contains(result.Message, fixtureOperatorName+" is paused") || !slices.Contains(result.Logs, "Dashboard Dev recovery information saved; "+fixtureOperatorName+" scaled to 0.") {
		t.Fatalf("deploy result does not name the operator: %+v", result)
	}
	wantPath := namespacedPath("apps/v1", "deployments", fixtureAppsNS, fixtureOperatorName)
	if !slices.Contains(f.patchPaths, wantPath) {
		t.Fatalf("operator patches went to %v, want %s", f.patchPaths, wantPath)
	}
	override, err := DashboardOverrideSummary(c)
	if err != nil || override.OperatorName != fixtureOperatorName || override.OperatorNamespace != fixtureAppsNS || !override.OperatorPaused {
		t.Fatalf("override = %+v, %v", override, err)
	}
	revert, err := RevertDashboardImage(c)
	if err != nil || !revert.Success || !strings.Contains(revert.Message, fixtureOperatorName+" restored to 2 replicas") {
		t.Fatalf("revert = %+v, %v", revert, err)
	}
	if *f.operator.Spec.Replicas != 2 || f.operator.Metadata.Annotations[dashboardDevAnnotation] != "" {
		t.Fatalf("not resumed: replicas=%d annotations=%v", *f.operator.Spec.Replicas, f.operator.Metadata.Annotations)
	}
	for _, p := range f.patchPaths {
		if !strings.Contains(p, "/namespaces/"+fixtureAppsNS+"/") {
			t.Fatalf("a patch outside the discovered namespace: %s", p)
		}
	}
}

// Status reads reuse a discovery (the RBAC lists are large); every change
// discovers again.
func TestDashboardDiscoveryCachedForStatusOnly(t *testing.T) {
	f, c := newDashboardDevFixture(t)
	roleReads := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		n := 0
		for _, p := range f.reads {
			if strings.HasSuffix(p, "/clusterroles") {
				n++
			}
		}
		return n
	}
	for range 3 {
		if _, err := DashboardOverrideSummary(c); err != nil {
			t.Fatal(err)
		}
	}
	if n := roleReads(); n != 1 {
		t.Fatalf("three status reads listed ClusterRoles %d times, want 1", n)
	}
	if _, err := DashboardDevActive(c); err != nil {
		t.Fatal(err)
	}
	if n := roleReads(); n != 2 {
		t.Fatalf("the check before a change listed ClusterRoles %d times in all, want 2 (a fresh discovery)", n)
	}
}

// A cached operator whose Deployment is gone is discovered again, not
// reported as the legacy flow.
func TestDashboardStatusRediscoversAGoneOperator(t *testing.T) {
	f, c := newFakeAPI(t)
	dashboardTargetCache.Add(c.baseURL, dashboardTarget{Operator: &moduleOperator{Namespace: fixtureAppsNS, Name: "old-name"}, Namespace: fixtureAppsNS}, dashboardTargetTTL)
	serveModuleCluster(f)
	o, err := DashboardOverrideSummary(c)
	if err != nil || o == nil || o.OperatorName != fixtureOperatorName {
		t.Fatalf("override = %+v, %v", o, err)
	}
}

// The DSCInitialization's namespaces: cached when read, never guessed.
func TestDiscoverRHOAINamespaces(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(f *fakeAPI)
		want    rhoaiNamespaces
		wantErr string
		noDSCI  bool
	}{
		{"applications and monitoring", func(f *fakeAPI) {
			f.json("GET", dsciV2List, 200, `{"items":[{"metadata":{"name":"d"},"spec":{"applicationsNamespace":"apps-x","monitoring":{"namespace":"mon-x"}}}]}`)
		}, rhoaiNamespaces{Applications: "apps-x", Monitoring: "mon-x"}, "", false},
		{"no DSCInitialization API", func(f *fakeAPI) {}, rhoaiNamespaces{}, "no DSCInitialization exists", true},
		{"none created", func(f *fakeAPI) { f.json("GET", dsciV2List, 200, `{"items":[]}`) }, rhoaiNamespaces{}, "no DSCInitialization exists", true},
		{"two", func(f *fakeAPI) {
			f.json("GET", dsciV2List, 200, `{"items":[{"spec":{"applicationsNamespace":"a"}},{"spec":{"applicationsNamespace":"b"}}]}`)
		}, rhoaiNamespaces{}, "2 DSCInitializations", false},
		{"no namespace named", func(f *fakeAPI) { f.json("GET", dsciV2List, 200, `{"items":[{"metadata":{"name":"d"},"spec":{}}]}`) }, rhoaiNamespaces{}, "names no applications namespace", false},
		{"read refused", func(f *fakeAPI) { f.status("GET", dsciV2List, 403, "Forbidden") }, rhoaiNamespaces{}, "403", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFakeAPI(t)
			tc.setup(f)
			got, err := discoverRHOAINamespaces(c)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || errors.Is(err, errNoDSCInitialization) != tc.noDSCI || got != (rhoaiNamespaces{}) {
					t.Fatalf("got %+v, %v; want error %q", got, err, tc.wantErr)
				}
				if _, cached := rhoaiNamespacesCache.Get(c.baseURL); cached {
					t.Fatal("a failed read was cached")
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %+v, %v", got, err)
			}
			before := f.requestCount()
			if again, err := discoverRHOAINamespaces(c); err != nil || again != tc.want || f.requestCount() != before {
				t.Fatalf("second read %+v, %v, %d requests (want the cache)", again, err, f.requestCount()-before)
			}
		})
	}
}
