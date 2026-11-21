package cluster

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dsc"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	liveDSCUID   = "ba54ddbf-ad4a-4257-9ab4-977eb541dc1c"
	kueueCRPath  = "/apis/components.platform.opendatahub.io/v1alpha1/kueues/default-kueue"
	platformPath = "/apis/config.opendatahub.io/v1alpha1/platforms/default"
)

// teardownWorld is managerWorld with the Platform default and a Kueue CR
// owned by the DSC. The components group (serving Kueue) is served once
// serveModules is called: the plan's removal checks of every module run
// before the delete and are tested elsewhere, so the group appears only
// after the delete here.
func teardownWorld(t *testing.T) (*resourceFake, *Client) {
	t.Helper()
	f, c := managerWorld(t, nil)
	f.put(kueueCRPath, map[string]any{"metadata": map[string]any{"name": "default-kueue", "finalizers": []any{"platform.opendatahub.io/finalizer"},
		"ownerReferences": []any{map[string]any{"apiVersion": "datasciencecluster.opendatahub.io/v2", "kind": "DataScienceCluster", "name": "default-dsc", "uid": liveDSCUID, "blockOwnerDeletion": true, "controller": true}}}})
	setPlatform(f, 3, 3, "True", "", "")
	return f, c
}

// serveModules serves the components group with the Kueue kind.
func serveModules(f *resourceFake) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups = map[string]string{
		"/apis/datasciencecluster.opendatahub.io":           dscGroupDoc,
		"/apis/" + rhoai.ComponentAPIGroup:                  `{"kind":"APIGroup","name":"components.platform.opendatahub.io","preferredVersion":{"groupVersion":"components.platform.opendatahub.io/v1alpha1","version":"v1alpha1"}}`,
		"/apis/components.platform.opendatahub.io/v1alpha1": `{"resources":[{"name":"kueues","kind":"Kueue","verbs":["list","get","patch"]}]}`,
	}
}

// serveModulesOnceDeleted serves the module group after the real delete
// of the DSC.
func serveModulesOnceDeleted(f *resourceFake) {
	f.beforeRequest = func(r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Query().Get("dryRun") == "" {
			serveModules(f)
		}
	}
}

// setPlatform stores the Platform default at generation gen whose
// ProvisioningSucceeded observed observed, with ModulesReady status,
// reason and message ("" status: no ModulesReady).
func setPlatform(f *resourceFake, gen, observed int, status, reason, msg string) {
	conds := []any{map[string]any{"type": "ProvisioningSucceeded", "status": "True", "observedGeneration": observed}}
	if status != "" {
		conds = append(conds, map[string]any{"type": "ModulesReady", "status": status, "reason": reason, "message": msg})
	}
	f.put(platformPath, map[string]any{"metadata": map[string]any{"name": "default", "generation": gen}, "status": map[string]any{"conditions": conds}})
}

// The DSC goes at once (its finalizer only marks the modules Removed); the
// reset then waits for the Kueue CR it owned and for the Platform to report
// nothing pending, and creates the new DSC only after that.
func TestHardResetWaitsForTheModuleTeardown(t *testing.T) {
	f, c := teardownWorld(t)
	setPlatform(f, 4, 3, "True", "Removed", "Modules pending deletion: kueue")
	operatorRemovesDSCAfter(f, 1)
	serveModulesOnceDeleted(f)
	polls := 0
	prev := f.beforeServe
	f.beforeServe = func(method, path string) {
		prev(method, path)
		if method != http.MethodGet || path != dscV3Path {
			return
		}
		f.mu.Lock()
		k, _ := parseFakePath(dscV3Path)
		dscGone := f.objects[k] == nil
		f.mu.Unlock()
		if !dscGone {
			return
		}
		switch polls++; polls {
		case 2:
			f.mu.Lock()
			kk, _ := parseFakePath(kueueCRPath)
			delete(f.objects, kk)
			f.mu.Unlock()
		case 4:
			setPlatform(f, 4, 4, "True", "NoManagedModules", "All registered modules have ManagementState Removed or are not configured")
		}
	}
	res, steps := runHardReset(t, c, nil)
	if !res.Success {
		t.Fatalf("result = %+v", res)
	}
	text := stepsText(steps)
	for _, want := range []string{"DataScienceCluster default-dsc is gone", "Waiting for the module CRs the DataScienceCluster owned to be deleted: Kueue default-kueue",
		"Waiting for the Platform to report the modules removed: Platform default has not reported a reconcile of its generation 4 yet",
		"is gone and the platform has cleaned up its modules"} {
		if !strings.Contains(text, want) {
			t.Errorf("steps lack %q:\n%s", want, text)
		}
	}
	posts := f.callsTo(http.MethodPost, dscV3List)
	if len(posts) != 2 || polls < 4 {
		t.Fatalf("created %d times after %d polls", len(posts), polls)
	}
	for _, d := range f.callsTo(http.MethodDelete, dscV3Path) {
		if !strings.Contains(d.Body, `"propagationPolicy":"Background"`) {
			t.Fatalf("delete %s", d.Body)
		}
	}
}

// A module CR that stays: the reset stops, creates nothing, names it and
// says what to check; Finish refuses while it is there.
func TestHardResetTimesOutOnAModuleCRAndFinishWaits(t *testing.T) {
	f, c := teardownWorld(t)
	operatorRemovesDSCAfter(f, 1)
	serveModulesOnceDeleted(f)
	obj := f.get(kueueCRPath)
	ensureMap(obj, "metadata")["deletionTimestamp"] = "2026-11-19T00:00:00Z"
	f.put(kueueCRPath, obj)
	res, _ := runHardReset(t, c, nil)
	if res.Success || res.ErrorCode != "timeout" || !strings.Contains(res.Message, "DataScienceCluster default-dsc is gone, but after 10m0s these module CRs it owned are still there: Kueue default-kueue (deleting since 2026-11-19T00:00:00Z, finalizers platform.opendatahub.io/finalizer)") ||
		!strings.Contains(res.Message, "Diagnostics (Platform modules)") || !strings.Contains(res.Message, "Finish the hard reset") {
		t.Fatalf("result = %+v", res)
	}
	if posts := f.callsTo(http.MethodPost, dscV3List); len(posts) != 0 {
		t.Fatalf("created: %+v", posts)
	}
	pending, err := dsc.PendingHardReset(c)
	if err != nil || pending == nil {
		t.Fatalf("pending %+v %v", pending, err)
	}
	f.beforeServe = nil
	f.put("/apis/dscinitialization.opendatahub.io/v2/dscinitializations/default-dsci", map[string]any{
		"metadata": map[string]any{"name": "default-dsci"}, "spec": map[string]any{"applicationsNamespace": "redhat-ods-applications"},
		"status": map[string]any{"release": map[string]any{"name": dsc.SelfManagedPlatform}}})
	fin, _ := FinishDSCHardReset(c, types.DSCFinishRequest{Name: pending.Name, Token: pending.Token})
	if fin.Success || fin.ErrorCode != "prerequisites" || !strings.Contains(fin.Message, "Kueue default-kueue") {
		t.Fatalf("finish = %+v", fin)
	}
	kk, _ := parseFakePath(kueueCRPath)
	delete(f.objects, kk)
	if fin, _ := FinishDSCHardReset(c, types.DSCFinishRequest{Name: pending.Name, Token: pending.Token}); !fin.Success {
		t.Fatalf("finish after the teardown = %+v", fin)
	}
}

// A module CR the DSC owns whose operator the tool cannot identify (the CSV
// does not own the Kueue CRD here and no ServiceAccount may update it):
// refused before anything is deleted.
func TestHardResetRefusesAModuleCRWithAnUnknownOperator(t *testing.T) {
	f, c := teardownWorld(t)
	serveModules(f)
	ops, refusal, err := dsc.HardResetOperators(c, liveDSCUID)
	if err != nil || len(ops) != 0 || !strings.Contains(refusal, "Kueue default-kueue: the tool cannot identify its module operator") {
		t.Fatalf("ops %+v, refusal %q, err %v", ops, refusal, err)
	}
	// The installed CSV owns the Kueue CRD: the RHOAI operator reconciles
	// it, and there is no module operator to wait for.
	csv := f.get(rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", rhoai.SubNS, "rhods-operator.3.6.0"))
	ensureMap(csv, "spec")["customresourcedefinitions"] = map[string]any{"owned": []any{map[string]any{"name": "kueues.components.platform.opendatahub.io"}}}
	f.put(rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", rhoai.SubNS, "rhods-operator.3.6.0"), csv)
	if ops, refusal, err := dsc.HardResetOperators(c, liveDSCUID); err != nil || len(ops) != 0 || refusal != "" {
		t.Fatalf("ops %+v, refusal %q, err %v", ops, refusal, err)
	}
}

// Owner fixtures of the namespace guard's tests.
const (
	amwGV       = "work.open-cluster-management.io/v1"
	amwPath     = "/apis/" + amwGV + "/appliedmanifestworks/"
	rolesPath   = "/apis/rbac.authorization.k8s.io/v1/clusterroles/"
	dscOwnerRef = "datasciencecluster.opendatahub.io/v2"
)

// ref is an owner reference.
func ref(apiVersion, kind, name, uid string) map[string]any {
	return map[string]any{"apiVersion": apiVersion, "kind": kind, "name": name, "uid": uid}
}

// dscRef is an owner reference to the live DSC.
func dscRef() map[string]any {
	return ref(dscOwnerRef, "DataScienceCluster", "default-dsc", liveDSCUID)
}

// ownedNamespace stores namespace name with these owners.
func ownedNamespace(f *resourceFake, name string, owners ...map[string]any) {
	refs := make([]any, 0, len(owners))
	for _, o := range owners {
		refs = append(refs, o)
	}
	f.put("/api/v1/namespaces/"+name, map[string]any{"metadata": map[string]any{"name": name, "uid": "ns-" + name, "ownerReferences": refs}})
}

// serveOwnerKinds serves ClusterRoles and AppliedManifestWorks (ACM's,
// cluster-scoped) in API discovery.
func serveOwnerKinds(f *resourceFake) {
	f.groups["/apis/rbac.authorization.k8s.io/v1"] = `{"resources":[{"name":"clusterroles","kind":"ClusterRole","namespaced":false}]}`
	f.groups["/apis/"+amwGV] = `{"resources":[{"name":"appliedmanifestworks","kind":"AppliedManifestWork","namespaced":false},{"name":"appliedmanifestworks/status","kind":"AppliedManifestWork","namespaced":false}]}`
}

// putOwner stores the owner at path with uid, more metadata and owners.
func putOwner(f *resourceFake, path, uid string, meta map[string]any, owners ...map[string]any) {
	name := path[strings.LastIndex(path, "/")+1:]
	m := map[string]any{"name": name, "uid": uid}
	for k, v := range meta {
		m[k] = v
	}
	if len(owners) > 0 {
		refs := make([]any, 0, len(owners))
		for _, o := range owners {
			refs = append(refs, o)
		}
		m["ownerReferences"] = refs
	}
	f.put(path, map[string]any{"metadata": m})
}

// foregroundDeleting is the metadata of an object being deleted with
// propagationPolicy Foreground (waitingForDependentsDeletion).
var foregroundDeleting = map[string]any{"deletionTimestamp": "2026-11-20T10:00:00Z", "finalizers": []any{"foregroundDeletion"}}

// A4: the hard reset is refused when the garbage collector could delete a
// namespace with the DSC: no owner of it is solid and stays so (kubernetes
// v1.34 garbagecollector.go attemptToDeleteItem :565-644,
// classifyReferences :453-482), and one of them leads to an
// *.opendatahub.io object or cannot be resolved. The message names the
// owner chain and offers the soft reset or Fix, and never says to remove
// owner references; for an unknown owner it says what could not be read
// with whose sign-in and the oc command that shows its owners.
func TestHardResetRefusesNamespacesTheCollectorWouldDelete(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(f *resourceFake)
		want []string
	}{
		{"owned by the DSC", func(f *resourceFake) {
			ownedNamespace(f, "my-notebooks", dscRef())
		}, []string{"Deleting the DataScienceCluster would make the garbage collector delete namespace my-notebooks, owned by DataScienceCluster default-dsc (datasciencecluster.opendatahub.io/v2), with everything in it. Use the soft reset or Fix instead."}},
		{"owned by a ClusterRole the Platform owns", func(f *resourceFake) {
			ownedNamespace(f, "maas", ref("rbac.authorization.k8s.io/v1", "ClusterRole", "r", "role-uid"))
			putOwner(f, rolesPath+"r", "role-uid", nil, ref("config.opendatahub.io/v1alpha1", "Platform", "default", "p"))
		}, []string{"namespace maas, owned by ClusterRole r (rbac.authorization.k8s.io/v1), owned by Platform default (config.opendatahub.io/v1alpha1), with everything in it"}},
		{"an AppliedManifestWork owned by the DSC", func(f *resourceFake) {
			ownedNamespace(f, "acm", ref(amwGV, "AppliedManifestWork", "w", "w-uid"))
			putOwner(f, amwPath+"w", "w-uid", nil, dscRef())
		}, []string{"namespace acm, owned by AppliedManifestWork w (work.open-cluster-management.io/v1), owned by DataScienceCluster default-dsc"}},
		// Not solid: an owner being deleted in the foreground
		// (classifyReferences :475) does not keep the namespace.
		{"the other owner is being deleted in the foreground", func(f *resourceFake) {
			ownedNamespace(f, "fg", ref("rbac.authorization.k8s.io/v1", "ClusterRole", "going", "going-uid"), dscRef())
			putOwner(f, rolesPath+"going", "going-uid", foregroundDeleting)
		}, []string{"delete namespace fg, owned by DataScienceCluster default-dsc"}},
		// Solid for now (classifyReferences :477-478), but its deletion ends
		// in its removal: it does not keep the namespace after the DSC.
		{"the other owner is being deleted in the background", func(f *resourceFake) {
			ownedNamespace(f, "bg", dscRef(), ref("rbac.authorization.k8s.io/v1", "ClusterRole", "slow", "slow"))
			putOwner(f, rolesPath+"slow", "slow", map[string]any{"deletionTimestamp": "2026-11-20T10:00:00Z", "finalizers": []any{"example.com/cleanup"}})
		}, []string{"delete namespace bg, owned by DataScienceCluster default-dsc"}},
		// Solid for now, but the collector then removes its reference from
		// the namespace (orphanDependents :668-690): the DSC's remains.
		{"the other owner is being deleted with orphaning", func(f *resourceFake) {
			ownedNamespace(f, "orph", ref("rbac.authorization.k8s.io/v1", "ClusterRole", "orphaning", "o"), dscRef())
			putOwner(f, rolesPath+"orphaning", "o", map[string]any{"deletionTimestamp": "2026-11-20T10:00:00Z", "finalizers": []any{"orphan"}})
		}, []string{"delete namespace orph, owned by DataScienceCluster default-dsc"}},
		// Not solid: a gone owner, and an object of the name with another
		// UID (isDangling :431-449).
		{"the other owners are gone", func(f *resourceFake) {
			ownedNamespace(f, "gone", ref("rbac.authorization.k8s.io/v1", "ClusterRole", "nothing", "n"),
				ref("rbac.authorization.k8s.io/v1", "ClusterRole", "renamed", "old-uid"), dscRef())
			putOwner(f, rolesPath+"renamed", "new-uid", nil)
		}, []string{"delete namespace gone, owned by DataScienceCluster default-dsc"}},
		// The fake lists namespaces in no particular order.
		{"several namespaces", func(f *resourceFake) {
			ownedNamespace(f, "a", dscRef())
			ownedNamespace(f, "b", dscRef())
		}, []string{"namespace a, owned by DataScienceCluster default-dsc (datasciencecluster.opendatahub.io/v2)", "namespace b, owned by DataScienceCluster default-dsc (datasciencecluster.opendatahub.io/v2)",
			"(datasciencecluster.opendatahub.io/v2); namespace ", "(datasciencecluster.opendatahub.io/v2), with everything in them. Use the soft reset or Fix instead."}},
		{"an owner kind that is not served", func(f *resourceFake) {
			ownedNamespace(f, "x", ref("example.com/v1", "Thing", "t", "t"))
		}, []string{"The tool cannot tell whether the garbage collector would delete a namespace with the DataScienceCluster. Namespace x, owned by Thing t (example.com/v1): Thing t cannot be looked up: API discovery of apis/example.com/v1",
			"Check its owners with: oc get thing.example.com t -o jsonpath='{.metadata.ownerReferences}' Nothing was changed."}},
		{"an owner the user cannot read, and no solid owner", func(f *resourceFake) {
			ownedNamespace(f, "hidden", ref(amwGV, "AppliedManifestWork", "w", "w-uid"), ref("rbac.authorization.k8s.io/v1", "ClusterRole", "nothing", "n"))
			putOwner(f, amwPath+"w", "w-uid", nil)
			f.failAs[fakeUserToken+" GET "+amwPath+"w"] = http.StatusForbidden
		}, []string{"Namespace hidden, owned by AppliedManifestWork w (work.open-cluster-management.io/v1): AppliedManifestWork w could not be read with your sign-in: k8s API error 403",
			"Check its owners with: oc get appliedmanifestwork.work.open-cluster-management.io w -o jsonpath='{.metadata.ownerReferences}'"}},
		{"an unreadable owner further up the chain", func(f *resourceFake) {
			ownedNamespace(f, "deep", ref("rbac.authorization.k8s.io/v1", "ClusterRole", "r", "role-uid"))
			putOwner(f, rolesPath+"r", "role-uid", nil, ref(amwGV, "AppliedManifestWork", "w", "w-uid"))
			putOwner(f, amwPath+"w", "w-uid", nil)
			f.failAs[fakeUserToken+" GET "+amwPath+"w"] = http.StatusForbidden
		}, []string{"Namespace deep, owned by ClusterRole r (rbac.authorization.k8s.io/v1), owned by AppliedManifestWork w (work.open-cluster-management.io/v1): AppliedManifestWork w could not be read with your sign-in"}},
		{"owners in a loop", func(f *resourceFake) {
			ownedNamespace(f, "loop", ref("rbac.authorization.k8s.io/v1", "ClusterRole", "a", "a-uid"))
			putOwner(f, rolesPath+"a", "a-uid", nil, ref("rbac.authorization.k8s.io/v1", "ClusterRole", "b", "b-uid"))
			putOwner(f, rolesPath+"b", "b-uid", nil, ref("rbac.authorization.k8s.io/v1", "ClusterRole", "a", "a-uid"))
		}, []string{"Namespace loop, owned by ClusterRole a (rbac.authorization.k8s.io/v1), owned by ClusterRole b (rbac.authorization.k8s.io/v1), owned by ClusterRole a (rbac.authorization.k8s.io/v1): ClusterRole a is its own owner through the chain"}},
		{"namespaces unreadable with the user's sign-in", func(f *resourceFake) {
			f.failAs[fakeUserToken+" GET /api/v1/namespaces"] = http.StatusForbidden
		}, []string{"The namespaces could not be listed with your sign-in"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := managerWorld(t, nil)
			serveOwnerKinds(f)
			tc.edit(f)
			plan, err := PlanDSCChange(c, "hard-reset", "", "", false, false)
			if err != nil || !strings.HasPrefix(plan.Refusal, "A hard reset is not possible. ") || !strings.HasSuffix(plan.Refusal, " Nothing was changed.") ||
				strings.Contains(plan.Refusal, "oc edit") || strings.Contains(plan.Refusal, "Remove") {
				t.Fatalf("refusal %q, err %v", plan.Refusal, err)
			}
			for _, want := range tc.want {
				if !strings.Contains(plan.Refusal, want) {
					t.Fatalf("refusal %q\nwant %q", plan.Refusal, want)
				}
			}
			if d := f.callsTo(http.MethodDelete, dscV3Path); len(d) != 0 {
				t.Fatalf("deletes %+v", d)
			}
		})
	}
}

// A namespace the collector keeps whatever happens to the DSC is not a
// reason to refuse: one owner that is solid and does not lead to RHOAI is
// enough (attemptToDeleteItem :565-590), whatever the other owners are.
// Owners that are all gone or being deleted in the foreground, none of them
// RHOAI's, make the namespace the collector's already, reset or not.
func TestHardResetAllowsNamespacesTheCollectorKeeps(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(f *resourceFake)
	}{
		{"one solid unrelated owner next to the DSC", func(f *resourceFake) {
			ownedNamespace(f, "both", dscRef(), ref("rbac.authorization.k8s.io/v1", "ClusterRole", "plain", "pl"))
			putOwner(f, rolesPath+"plain", "pl", nil)
		}},
		{"a solid owner whose own owners include one that keeps it", func(f *resourceFake) {
			ownedNamespace(f, "chain", ref("rbac.authorization.k8s.io/v1", "ClusterRole", "mid", "mid"))
			putOwner(f, rolesPath+"mid", "mid", nil, dscRef(), ref("rbac.authorization.k8s.io/v1", "ClusterRole", "top", "top"))
			putOwner(f, rolesPath+"top", "top", nil)
		}},
		{"a gone owner and an unrelated one", func(f *resourceFake) {
			ownedNamespace(f, "a", ref("rbac.authorization.k8s.io/v1", "ClusterRole", "gone", "g"), ref("rbac.authorization.k8s.io/v1", "ClusterRole", "plain", "pl"))
			putOwner(f, rolesPath+"plain", "pl", nil)
		}},
		{"only gone or foreground-deleting owners", func(f *resourceFake) {
			ownedNamespace(f, "orphan", ref("rbac.authorization.k8s.io/v1", "ClusterRole", "gone", "g"), ref("rbac.authorization.k8s.io/v1", "ClusterRole", "going", "going"))
			putOwner(f, rolesPath+"going", "going", foregroundDeleting)
		}},
		{"an unreadable owner next to a solid one", func(f *resourceFake) {
			ownedNamespace(f, "mixed", ref("example.com/v1", "Thing", "t", "t"), ref("rbac.authorization.k8s.io/v1", "ClusterRole", "plain", "pl"))
			putOwner(f, rolesPath+"plain", "pl", nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := managerWorld(t, nil)
			serveOwnerKinds(f)
			tc.edit(f)
			if why := dsc.NamespaceOwnerBlockerForTest(c); why != "" {
				t.Fatalf("refused: %s", why)
			}
		})
	}
}

// The live case (ROSA HCP, an ACM spoke, 2026-11-20): four namespaces each
// owned by one AppliedManifestWork, which has no owners. The ServiceAccount
// may not read AppliedManifestWorks (and must not: the guard reads owners
// of any kind), the signed-in cluster-admin may. The plan (a read request:
// the ServiceAccount's client carrying the user's, pkg/api withAdminRead)
// and the reset itself (the user's client, withMutationAuth) both read the
// namespaces and their owners with the user's token only, and both pass.
func TestHardResetReadsNamespaceOwnersWithTheSignedInUsersToken(t *testing.T) {
	f, c := managerWorld(t, nil)
	serveOwnerKinds(f)
	operatorRemovesDSCAfter(f, 1)
	for i, ns := range []string{"juntao-cluster", "open-cluster-management-agent-addon", "open-cluster-management-policies", "submariner-operator"} {
		w := fmt.Sprintf("work-%d", i)
		ownedNamespace(f, ns, ref(amwGV, "AppliedManifestWork", w, w+"-uid"))
		putOwner(f, amwPath+w, w+"-uid", nil)
		f.failAs[fakeSAToken+" GET "+amwPath+w] = http.StatusForbidden
		f.failAs[fakeSAToken+" GET /apis/"+amwGV] = http.StatusForbidden
	}
	f.failAs[fakeSAToken+" GET /api/v1/namespaces"] = http.StatusForbidden
	saOwnerReads := func() []string {
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []string
		for _, call := range f.calls {
			if (call.Path == "/api/v1/namespaces" || strings.HasPrefix(call.Path, "/apis/"+amwGV)) && call.Token != fakeUserToken {
				out = append(out, call.Token+" "+call.Path)
			}
		}
		return out
	}

	plan, err := PlanDSCChange(c, "hard-reset", "", "", false, false)
	if err != nil || plan.Refusal != "" {
		t.Fatalf("plan refused: %q, %v", plan.Refusal, err)
	}
	if got := f.callsTo(http.MethodGet, amwPath+"work-0"); len(got) != 1 || got[0].Token != fakeUserToken {
		t.Fatalf("owner reads %+v", got)
	}
	if sa := saOwnerReads(); len(sa) != 0 {
		t.Fatalf("read with another token than the user's: %v", sa)
	}

	// The stream runs with the user's client itself (no ServiceAccount
	// client behind it) and runs the same guard before the delete.
	user := c.SignedInUser()
	editTestClient(user, func(o *testClientOptions) { o.SignedInUser = nil })
	res, err := HardResetDSC(user, types.DSCPlanRequest{Name: plan.Name, UID: plan.UID, Token: plan.Token, Confirm: plan.Name, KeepEnabled: new(false)}, func(UpdateStepEvent) {})
	if err != nil || !res.Success {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if got := f.callsTo(http.MethodGet, amwPath+"work-3"); len(got) != 2 || got[1].Token != fakeUserToken {
		t.Fatalf("the reset did not run the guard with the user's token: %+v", got)
	}
	if sa := saOwnerReads(); len(sa) != 0 {
		t.Fatalf("read with another token than the user's: %v", sa)
	}

	// A chain to the DSC refuses the reset with the user's client too,
	// before the delete (the reset plans again before it checks the token).
	f2, c2 := managerWorld(t, nil)
	serveOwnerKinds(f2)
	ownedNamespace(f2, "acm", ref(amwGV, "AppliedManifestWork", "w", "w-uid"))
	putOwner(f2, amwPath+"w", "w-uid", nil, dscRef())
	user2 := c2.SignedInUser()
	editTestClient(user2, func(o *testClientOptions) { o.SignedInUser = nil })
	res, _ = HardResetDSC(user2, types.DSCPlanRequest{Name: plan.Name, UID: plan.UID, Token: plan.Token, Confirm: plan.Name, KeepEnabled: new(false)}, func(UpdateStepEvent) {})
	if res.Success || res.ErrorCode != "prerequisites" || !strings.Contains(res.Message, "namespace acm, owned by AppliedManifestWork w") {
		t.Fatalf("result = %+v", res)
	}
	if got := f2.callsTo(http.MethodGet, amwPath+"w"); len(got) != 1 || got[0].Token != fakeUserToken {
		t.Fatalf("owner reads %+v", got)
	}
	if d := f2.callsTo(http.MethodDelete, dscV3Path); len(d) != 0 {
		t.Fatalf("deletes %+v", d)
	}
}

// Without the signed-in user's client (no token reached the request, or a
// client the hard reset is never run with) the guard refuses: the
// ServiceAccount does not read the owners instead.
func TestHardResetNamespaceGuardNeedsTheUsersSignIn(t *testing.T) {
	f, c := managerWorld(t, nil)
	c.SetSignedInUser(nil)
	plan, err := PlanDSCChange(c, "hard-reset", "", "", false, false)
	if err != nil || !strings.Contains(plan.Refusal, "reads the namespaces' owners with your own sign-in, and this request has none") {
		t.Fatalf("refusal %q, %v", plan.Refusal, err)
	}
	if got := f.callsTo(http.MethodGet, "/api/v1/namespaces"); len(got) != 0 {
		t.Fatalf("namespaces read with the ServiceAccount: %+v", got)
	}
}

// What the Platform default says decides (d), from the semantics of
// writeAggregateCondition (rhods-operator rhoai-3.6
// modules_controller_actions.go:763-806).
func TestPlatformModulesSettled(t *testing.T) {
	for _, tc := range []struct {
		name                string
		gen, observed       int
		status, reason, msg string
		settled             bool
		want                string
	}{
		{"nothing managed", 4, 4, "True", "NoManagedModules", "All registered modules have ManagementState Removed or are not configured", true, "no module pending deletion"},
		{"monitoring ready", 4, 4, "True", "", "", true, "no module pending deletion"},
		{"monitoring not ready, nothing pending", 4, 4, "False", "NotReady", "Some modules are not ready: monitoring", true, ""},
		{"pending (True, Removed)", 4, 4, "True", "Removed", "Modules pending deletion: kserve", false, "Modules pending deletion: kserve"},
		{"pending behind another error", 4, 4, "False", "NotReady", "Some modules are not ready: monitoring; pending deletion: ray", false, "pending deletion: ray"},
		{"stale generation", 5, 4, "True", "NoManagedModules", "", false, "generation 5"},
		{"reconcile stopped early", 4, 4, "False", "ConditionNotSet", "condition ModulesReady was not set during reconciliation", false, "stopped before it checked the modules"},
		{"no ModulesReady", 4, 4, "", "", "", false, "no ModulesReady"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newResourceFake(t)
			setPlatform(f, tc.gen, tc.observed, tc.status, tc.reason, tc.msg)
			settled, why, err := dsc.PlatformModulesSettled(c)
			if err != nil || settled != tc.settled || !strings.Contains(why, tc.want) {
				t.Fatalf("settled %v, %q, %v", settled, why, err)
			}
		})
	}
	f, c := newResourceFake(t)
	if settled, why, err := dsc.PlatformModulesSettled(c); !settled || err != nil || why != "no Platform default exists" {
		t.Fatalf("no Platform: %v %q %v", settled, why, err)
	}
	setPlatform(f, 1, 1, "True", "", "")
	p := f.get(platformPath)
	ensureMap(p, "metadata")["deletionTimestamp"] = "2026-11-19T00:00:00Z"
	f.put(platformPath, p)
	if settled, why, _ := dsc.PlatformModulesSettled(c); settled || !strings.Contains(why, "being deleted") {
		t.Fatalf("deleting Platform: %v %q", settled, why)
	}
}

// Phase 2: a recorded module operator Deployment that still exists with
// its UID is waited for; one deleted, or created again (another UID), is
// not.
func TestOperatorsLeft(t *testing.T) {
	f, c := newResourceFake(t)
	f.put("/apis/apps/v1/namespaces/redhat-ods-applications/deployments/kept", map[string]any{"metadata": map[string]any{"name": "kept", "uid": "u1"}})
	f.put("/apis/apps/v1/namespaces/redhat-ods-applications/deployments/again", map[string]any{"metadata": map[string]any{"name": "again", "uid": "new"}})
	left, err := dsc.OperatorsLeft(c, []dsc.HardResetOperator{
		{Namespace: "redhat-ods-applications", Name: "kept", UID: "u1"},
		{Namespace: "redhat-ods-applications", Name: "again", UID: "old"},
		{Namespace: "redhat-ods-applications", Name: "gone", UID: "u3"},
	})
	if err != nil || len(left) != 1 || left[0].Name != "kept" {
		t.Fatalf("left %+v %v", left, err)
	}
	st := dsc.TeardownState{Operators: left}
	if !strings.Contains(st.Stuck(), "Deployment redhat-ods-applications/kept") || !strings.Contains(st.Progress(), "module operators") {
		t.Fatalf("%q / %q", st.Stuck(), st.Progress())
	}
	if o, ok := dsc.ParseHardResetOperator(dsc.HardResetOperator{Namespace: "n", Name: "d", UID: "u"}.Record()); !ok || o.UID != "u" {
		t.Fatalf("record round trip %+v", o)
	}
}
