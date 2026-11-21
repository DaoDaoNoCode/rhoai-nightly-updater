package cluster

import (
	"context"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// acmCatalogMetadata is the nightly CatalogSource's metadata where an ACM
// ConfigurationPolicy (musthave, enforce) applies it, as seen on a live
// cluster: no Argo CD annotation, config-policy-controller owns the spec and
// OLM's catalog operator the status.
func acmCatalogMetadata(specOwner string) map[string]any {
	return map[string]any{"managedFields": []any{
		map[string]any{"manager": specOwner, "operation": "Update", "apiVersion": "operators.coreos.com/v1alpha1", "fieldsType": "FieldsV1",
			"fieldsV1": map[string]any{"f:spec": map[string]any{"f:image": map[string]any{}, "f:sourceType": map[string]any{}}}},
		map[string]any{"manager": "catalog", "operation": "Update", "apiVersion": "operators.coreos.com/v1alpha1", "fieldsType": "FieldsV1",
			"fieldsV1": map[string]any{"f:status": map[string]any{}}},
	}}
}

// acmFake is gitOpsFake with the catalog an ACM policy enforces instead of
// Argo CD; Argo CD still manages the Subscription (the env-setup demo
// environment).
func acmFake(t *testing.T) *fakeOLM {
	f := gitOpsFake(t)
	f.catalog["metadata"] = acmCatalogMetadata(acmPolicyManager)
	f.argo = argoManagingAllBut(argoCatalogObject)
	return f
}

const acmManagerText = "ACM policy (config-policy-controller)"

func TestACMEnforcesSpec(t *testing.T) {
	for name, tc := range map[string]struct {
		fields []any
		want   bool
	}{
		"spec owned":                    {acmCatalogMetadata(acmPolicyManager)["managedFields"].([]any), true},
		"another manager owns the spec": {acmCatalogMetadata("kubectl-edit")["managedFields"].([]any), false},
		"status only": {[]any{map[string]any{"manager": acmPolicyManager, "operation": "Update",
			"fieldsV1": map[string]any{"f:status": map[string]any{}}}}, false},
		"through the status subresource": {[]any{map[string]any{"manager": acmPolicyManager, "operation": "Update", "subresource": "status",
			"fieldsV1": map[string]any{"f:spec": map[string]any{}}}}, false},
		"metadata only": {[]any{map[string]any{"manager": acmPolicyManager, "operation": "Update",
			"fieldsV1": map[string]any{"f:metadata": map[string]any{}}}}, false},
		"no managedFields": {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := gitOpsFake(t)
			f.catalog["metadata"] = map[string]any{"managedFields": tc.fields}
			f.argo = argoManagingAllBut(argoCatalogObject)
			g, err := readOperatorGitOps(f.client(context.Background()))
			if err != nil {
				t.Fatal(err)
			}
			if g.catalogACM != tc.want || g.catalogApp != "" {
				t.Fatalf("catalogACM = %v (app %q), want %v", g.catalogACM, g.catalogApp, tc.want)
			}
		})
	}
	// Argo CD wins when both manage the catalog.
	f := gitOpsFake(t)
	meta := acmCatalogMetadata(acmPolicyManager)
	meta["annotations"] = trackingID("CatalogSource", rhoai.CatalogNS, rhoai.CatalogName)
	f.catalog["metadata"] = meta
	g, err := readOperatorGitOps(f.client(context.Background()))
	if err != nil || g.catalogApp != testArgoApp || g.catalogACM || !strings.Contains(g.catalogManager(), testArgoApp) {
		t.Fatalf("both: %+v %v", g, err)
	}
}

// The gitops profile installs what an ACM-enforced catalog serves, as it
// does for an Argo CD managed one, writes nothing else, and says which
// manager sets the catalog.
func TestUpdateGitOpsProfile_ACMManagedCatalog(t *testing.T) {
	f := servesGitOpsPolicy(t, acmFake(t))
	result, err := UpdateStreamGitOpsProfile(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
	if err != nil || !result.Success {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, w := range f.writes() {
		if !gitOpsProfileWriteAllowed(w) {
			t.Errorf("the gitops profile wrote %s; writes: %v", w, f.writes())
		}
	}
	logs := strings.Join(result.Logs, "\n")
	if !strings.Contains(logs, "An "+acmManagerText+" sets the catalog image") || !strings.Contains(logs, "left as the ACM policy sets it") {
		t.Fatalf("logs do not name the ACM policy:\n%s", logs)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch := f.sub["spec"].(map[string]any)["channel"]; ch != gitOpsChannel {
		t.Fatalf("channel = %v, want Git's %s", ch, gitOpsChannel)
	}
}

// Only ACM manages the operator (the catalog; the Subscription is the
// updater's): the gitops profile runs, and refuses another image with the
// policy's wording.
func TestUpdateGitOpsProfile_ACMOnly(t *testing.T) {
	f := servesGitOpsPolicy(t, acmFake(t))
	delete(f.sub["metadata"].(map[string]any), "annotations")
	f.argo = argoManagingAllBut(argoCatalogObject, argoSubscriptionObject)
	result, err := UpdateStreamGitOpsProfile(f.client(context.Background()), testNightlyImage, func(UpdateStepEvent) {})
	if err != nil || result.Success || result.ErrorCode != kube.ErrorCodeGitOpsManaged ||
		!strings.Contains(result.Message, "An "+acmManagerText+" sets the image of CatalogSource "+rhoai.CatalogName) || strings.Contains(result.Message, "Argo CD") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
	result, err = UpdateStreamGitOpsProfile(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
	if err != nil || !result.Success {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, w := range f.writes() {
		if !gitOpsProfileWriteAllowed(w) {
			t.Errorf("the gitops profile wrote %s", w)
		}
	}
}

// A config-policy-controller entry that owns only the status, or nothing
// at all, does not count: the profile refuses, naming both managers.
func TestUpdateGitOpsProfile_RefusesACatalogNobodyManages(t *testing.T) {
	for name, meta := range map[string]map[string]any{
		"nothing": {},
		"status only": {"managedFields": []any{map[string]any{"manager": acmPolicyManager, "operation": "Update",
			"fieldsV1": map[string]any{"f:status": map[string]any{}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			f := servesGitOpsPolicy(t, gitOpsFake(t))
			f.catalog["metadata"] = meta
			f.argo = argoManagingAllBut(argoCatalogObject)
			result, err := UpdateStreamGitOpsProfile(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
			if err != nil || result.Success || result.ErrorCode != kube.ErrorCodeProfileGitOps || result.Message != gitOpsProfileUnmanagedCatalog ||
				!strings.Contains(result.Message, "Argo CD or an ACM policy") {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if w := f.writes(); len(w) != 0 {
				t.Fatalf("writes = %v", w)
			}
			dry, err := UpdateDryRunGitOpsProfile(f.client(context.Background()), gitOpsImage)
			if err != nil || dry.Success || dry.ErrorCode != kube.ErrorCodeProfileGitOps {
				t.Fatalf("dry run: %+v %v", dry, err)
			}
		})
	}
}

func TestUpdateDryRunGitOpsProfile_ACMManagedCatalog(t *testing.T) {
	orig := lookupTargetBundle
	t.Cleanup(func() { lookupTargetBundle = orig })
	lookupTargetBundle = func(*Client, string) (string, error) { return gitOpsCSV, nil }

	f := servesGitOpsPolicy(t, acmFake(t))
	result, err := UpdateDryRunGitOpsProfile(f.client(context.Background()), gitOpsImage)
	if err != nil || !result.Success {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	logs := strings.Join(result.Logs, "\n")
	if !strings.Contains(logs, acmManagerText) || !strings.Contains(logs, "left as the ACM policy sets it") {
		t.Fatalf("dry run logs do not name the ACM policy:\n%s", logs)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

// The full profile treats an ACM-enforced catalog like an Argo CD managed
// one: it never replaces the catalog (ACM would put its image back) and
// installs the build the catalog serves; the dry run does not apply it.
func TestUpdateFullProfile_ACMManagedCatalog(t *testing.T) {
	for name, setup := range map[string]func(*fakeOLM){
		"Subscription in Argo CD": func(*fakeOLM) {},
		"ACM only": func(f *fakeOLM) {
			delete(f.sub["metadata"].(map[string]any), "annotations")
			f.argo = argoManagingAllBut(argoCatalogObject, argoSubscriptionObject)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := acmFake(t)
			setup(f)
			csPath := rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "catalogsources", rhoai.CatalogNS, rhoai.CatalogName)
			result, err := UpdateStream(f.client(context.Background()), testNightlyImage, func(UpdateStepEvent) {})
			if err != nil || result.Success || result.ErrorCode != kube.ErrorCodeGitOpsManaged || !strings.Contains(result.Message, gitOpsImage) {
				t.Fatalf("another image: result=%+v err=%v", result, err)
			}
			if w := f.writes(); len(w) != 0 {
				t.Fatalf("writes = %v", w)
			}
			result, err = UpdateStream(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
			if err != nil || !result.Success {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			for _, w := range f.writes() {
				if strings.HasSuffix(w, " "+csPath) {
					t.Fatalf("the ACM-enforced catalog was written: %v", f.writes())
				}
			}
			if !strings.Contains(strings.Join(result.Logs, "\n"), acmManagerText) {
				t.Fatalf("logs do not name the ACM policy: %v", result.Logs)
			}

			orig := lookupTargetBundle
			t.Cleanup(func() { lookupTargetBundle = orig })
			lookupTargetBundle = func(*Client, string) (string, error) { return gitOpsCSV, nil }
			before := len(f.requests)
			dry, err := Update(f.client(context.Background()), gitOpsImage, true)
			if err != nil || !dry.Success || !strings.Contains(strings.Join(dry.Logs, "\n"), "left as the ACM policy sets it") {
				t.Fatalf("dry run: %+v %v", dry, err)
			}
			f.mu.Lock()
			for _, r := range f.requests[before:] {
				if r.Path == csPath && r.Method != "GET" {
					t.Errorf("the dry run sent %s %s", r.Method, r.Path)
				}
			}
			f.mu.Unlock()
		})
	}
}

// Reinstall and Re-deploy are off while ACM enforces the catalog, as under
// Argo CD, and say so without naming Argo CD.
func TestOperatorOperationsRefusedUnderACM(t *testing.T) {
	f := acmFake(t)
	delete(f.sub["metadata"].(map[string]any), "annotations")
	f.argo = argoManagingAllBut(argoCatalogObject, argoSubscriptionObject)
	r, _ := ReinstallStreamWithOptions(f.client(context.Background()), "stable", "", "", OperationOptions{}, func(UpdateStepEvent) {})
	if r.ErrorCode != kube.ErrorCodeGitOpsManaged || !strings.Contains(r.Message, "CatalogSource "+rhoai.CatalogName+" is managed by an "+acmManagerText) ||
		strings.Contains(r.Message, "Argo CD would") || !strings.Contains(r.Message, "Nothing was changed") {
		t.Fatalf("reinstall: %+v", r)
	}
	r, _ = RefreshOperatorStreamWithOptions(f.client(context.Background()), OperationOptions{}, func(UpdateStepEvent) {})
	if r.ErrorCode != kube.ErrorCodeGitOpsManaged || !strings.Contains(r.Message, acmManagerText) {
		t.Fatalf("re-deploy: %+v", r)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

// The image changes while Update waits: the message names the policy.
func TestCatalogImageChangedNamesTheManager(t *testing.T) {
	for g, want := range map[operatorGitOps]string{
		{catalogApp: "a", catalogACM: false}: `most likely by Argo CD application "a" syncing a change from Git`,
		{catalogACM: true}:                   "most likely by an " + acmManagerText + " enforcing a change",
		{subscriptionApp: "s"}:               `most likely by Argo CD application "s" syncing a change from Git`,
	} {
		if got := (&errCatalogImageChanged{by: g.catalogChanger(), image: "img"}).Error(); !strings.Contains(got, want) {
			t.Errorf("%+v: %q lacks %q", g, got, want)
		}
	}
}

// Status reports an ACM-enforced catalog, and what it serves.
func TestStatusReportsACMManagedCatalog(t *testing.T) {
	f := acmFake(t)
	c := f.client(context.Background())
	if cs := statusManagersOf(t, c).CatalogSource; !cs.ManagedByACM || cs.ManagedBy != "" {
		t.Fatalf("catalog = %+v", cs)
	}
	f.catalog["metadata"] = acmCatalogMetadata("kubectl-edit")
	if cs := statusManagersOf(t, c).CatalogSource; cs.ManagedByACM {
		t.Fatalf("another manager counted as ACM: %+v", cs)
	}
	meta := acmCatalogMetadata(acmPolicyManager)
	meta["annotations"] = trackingID("CatalogSource", rhoai.CatalogNS, rhoai.CatalogName)
	f.catalog["metadata"] = meta
	f.argo = argoManagingAll(argoGitOpsObjects...)
	if cs := statusManagersOf(t, c).CatalogSource; cs.ManagedByACM || cs.ManagedBy != testArgoApp {
		t.Fatalf("Argo CD must win: %+v", cs)
	}
	// A tracking annotation left over (no Argo CD here) does not hide the
	// policy.
	f.argo = &fakeArgoCD{}
	if cs := statusManagersOf(t, c).CatalogSource; !cs.ManagedByACM || cs.ManagedBy != "" || cs.ArgoCD == nil || cs.ArgoCD.State != types.ArgoCDStale {
		t.Fatalf("stale tracking and an ACM policy: %+v", cs)
	}
}
