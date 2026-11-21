package cluster

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/node"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// scopeIsNestedInsideScope is runtime-utils' ScopeIsNestedInsideScope
// (pkg/registries/registries.go @7bdb5b9c); these are the cases of its own
// tests that matter here, and the near misses.
func TestScopeIsNestedInsideScope(t *testing.T) {
	for _, tc := range []struct {
		sub, super string
		want       bool
	}{
		{"registry.redhat.io/rhoai", "registry.redhat.io/rhoai", true},
		{"registry.redhat.io/rhoai", "registry.redhat.io", true},
		{"registry.redhat.io/rhoai", "*.redhat.io", true},
		{"registry.redhat.io/rhoai", "*.io", true},
		{"registry.redhat.io/rhoai", "registry.redhat.io/rho", false},
		{"registry.redhat.io/rhoai", "registry.redhat.io/rhoai/odh-dashboard-rhel9", false},
		{"registry.redhat.io/rhoai", "redhat.io", false},
		{"registry.redhat.io/rhoai", "*.example.com", false},
		{"registry.redhat.io/rhoai", "quay.io", false},
		{"registry.redhat.io:443/rhoai", "*.redhat.io", true},
		{"foo.redhat.io/x:1/y", "*.redhat.io", false},
	} {
		if got := node.ScopeIsNestedInsideScope(tc.sub, tc.super); got != tc.want {
			t.Errorf("scopeIsNestedInsideScope(%q, %q) = %v, want %v", tc.sub, tc.super, got, tc.want)
		}
	}
}

// An existing mirror for a parent scope of registry.redhat.io/rhoai (the
// registry, a wildcard host) is what the nodes use for its images now. A
// mirror for exactly registry.redhat.io/rhoai would take those pulls away
// from it (the most specific source wins), so Create refuses and says
// which object maps what, and what to do instead. Nothing is written, not
// even a dry run.
func TestMirrorCreateRefusesOverAParentScope(t *testing.T) {
	for _, tc := range []struct {
		name, path, obj string
		want            []string
	}{
		{"an IDMS for the registry", nsIDMSList + "/corp-mirrors", `{"spec":{"imageDigestMirrors":[{"source":"registry.redhat.io","mirrors":["mirror.corp.example/redhat"]}]}}`,
			[]string{"ImageDigestMirrorSet corp-mirrors maps registry.redhat.io to mirror.corp.example/redhat", "pulls of registry.redhat.io/rhoai images by digest go to mirror.corp.example/redhat/rhoai now",
				"most specific source", "pulls by digest would go to quay.io/rhoai, then to registry.redhat.io itself",
				"in spec.imageDigestMirrors of ImageDigestMirrorSet corp-mirrors: source registry.redhat.io/rhoai, mirrors mirror.corp.example/redhat/rhoai, quay.io/rhoai (that also sets up the image mirror)"}},
		{"an ICSP for the registry", nsICSPList + "/legacy", `{"spec":{"repositoryDigestMirrors":[{"source":"registry.redhat.io","mirrors":["mirror.corp.example/redhat"]}]}}`,
			[]string{"ImageContentSourcePolicy legacy maps registry.redhat.io", "in spec.repositoryDigestMirrors of ImageContentSourcePolicy legacy"}},
		{"an IDMS for a wildcard host", nsIDMSList + "/wild", `{"spec":{"imageDigestMirrors":[{"source":"*.redhat.io","mirrors":["mirror.corp.example/redhat"]}]}}`,
			[]string{"ImageDigestMirrorSet wild maps *.redhat.io to mirror.corp.example/redhat", "go to mirror.corp.example/redhat/rhoai now"}},
		{"an ITMS for the registry: tag pulls", nsITMSList + "/tags", `{"spec":{"imageTagMirrors":[{"source":"registry.redhat.io","mirrors":["mirror.corp.example/redhat"]}]}}`,
			[]string{"ImageTagMirrorSet tags maps registry.redhat.io", "images by tag go to mirror.corp.example/redhat/rhoai now", "pulls by tag would go to registry.redhat.io itself",
				"in spec.imageTagMirrors of ImageTagMirrorSet tags: source registry.redhat.io/rhoai, mirrors mirror.corp.example/redhat/rhoai (then Create the image mirror works here)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := nodeSetupFake(t, "HighlyAvailable", false)
			f.putJSON(tc.path, tc.obj)
			plan, err := PlanNodeSetup(c, types.NodeSetupRequest{Action: node.MirrorCreate})
			if err != nil {
				t.Fatal(err)
			}
			if plan.Ready || plan.NothingToDo || plan.RefusalCode != "parent_scope" || !strings.Contains(plan.Refusal, "Nothing was changed") {
				t.Fatalf("plan %+v", plan)
			}
			for _, s := range tc.want {
				if !strings.Contains(plan.Refusal, s) {
					t.Errorf("refusal lacks %q:\n%s", s, plan.Refusal)
				}
			}
			res, _ := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: node.MirrorCreate})
			if res.Success || res.ErrorCode != "parent_scope" || len(writes(f)) != 0 {
				t.Fatalf("run %+v, writes %v", res, writes(f))
			}
			if n := len(f.callsTo(http.MethodPost, nsIDMSList)); n != 0 {
				t.Fatalf("%d creates (dry runs included) were sent", n)
			}
		})
	}
	t.Run("one refusal names every parent", func(t *testing.T) {
		f, c := nodeSetupFake(t, "HighlyAvailable", false)
		f.putJSON(nsIDMSList+"/corp-mirrors", `{"spec":{"imageDigestMirrors":[{"source":"registry.redhat.io","mirrors":["mirror.corp.example/redhat"]}]}}`)
		f.putJSON(nsITMSList+"/tags", `{"spec":{"imageTagMirrors":[{"source":"registry.redhat.io","mirrors":["mirror.corp.example/redhat"]}]}}`)
		plan, _ := PlanNodeSetup(c, types.NodeSetupRequest{Action: node.MirrorCreate})
		if plan.RefusalCode != "parent_scope" || !strings.Contains(plan.Refusal, "ImageDigestMirrorSet corp-mirrors") || !strings.Contains(plan.Refusal, "ImageTagMirrorSet tags") ||
			!strings.Contains(plan.Refusal, "by digest would go to quay.io/rhoai") || !strings.Contains(plan.Refusal, "by tag would go to registry.redhat.io itself") {
			t.Fatalf("plan %+v", plan)
		}
	})
}

// An entry for exactly the source with mirrorSourcePolicy NeverContactSource
// (IDMS or ITMS): the setup's IDMS, with the default policy, would make the
// Machine Config Operator refuse the whole mirror configuration
// (helpers.go validateRegistriesConfScopes), so Create refuses first.
func TestMirrorCreateRefusesAConflictingSourcePolicy(t *testing.T) {
	for _, tc := range []struct{ name, path, obj, want string }{
		{"IDMS", nsIDMSList + "/strict", `{"spec":{"imageDigestMirrors":[{"source":"registry.redhat.io/rhoai","mirrors":["mirror.corp.example/rhoai"],"mirrorSourcePolicy":"NeverContactSource"}]}}`, "ImageDigestMirrorSet strict sets mirrorSourcePolicy NeverContactSource"},
		{"ITMS", nsITMSList + "/strict", `{"spec":{"imageTagMirrors":[{"source":"registry.redhat.io/rhoai","mirrors":["mirror.corp.example/rhoai"],"mirrorSourcePolicy":"NeverContactSource"}]}}`, "ImageTagMirrorSet strict sets mirrorSourcePolicy NeverContactSource"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := nodeSetupFake(t, "HighlyAvailable", false)
			f.putJSON(tc.path, tc.obj)
			plan, _ := PlanNodeSetup(c, types.NodeSetupRequest{Action: node.MirrorCreate})
			if plan.Ready || plan.RefusalCode != "policy_conflict" || !strings.Contains(plan.Refusal, tc.want) || !strings.Contains(plan.Refusal, "Nothing was changed") {
				t.Fatalf("plan %+v", plan)
			}
			res, _ := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: node.MirrorCreate})
			if res.Success || res.ErrorCode != "policy_conflict" || len(f.callsTo(http.MethodPost, nsIDMSList)) != 0 {
				t.Fatalf("run %+v", res)
			}
		})
	}
	t.Run("the default policy spelled out is no conflict", func(t *testing.T) {
		f, c := nodeSetupFake(t, "HighlyAvailable", false)
		f.putJSON(nsITMSList+"/tags", `{"spec":{"imageTagMirrors":[{"source":"registry.redhat.io/rhoai","mirrors":["mirror.corp.example/rhoai"],"mirrorSourcePolicy":"AllowContactingSource"}]}}`)
		if plan, _ := PlanNodeSetup(c, types.NodeSetupRequest{Action: node.MirrorCreate}); !plan.Ready {
			t.Fatalf("plan %+v", plan)
		}
	})
}

// The docs mock shows the refusal in the backend's own words.
func TestDocsMockParentScopeRefusalIsTheBackends(t *testing.T) {
	raw, err := os.ReadFile("../../docs/tools/mock/fixtures.mjs")
	if err != nil {
		t.Fatal(err)
	}
	want := node.ParentScopeRefusal([]node.MirrorRule{{Kind: node.KindIDMS, Name: "corp-mirrors", Source: "registry.redhat.io", Mirrors: []string{"mirror.corp.example/redhat"}}})
	if !strings.Contains(string(raw), `export const MIRROR_PARENT_SCOPE_REFUSAL = "`+want+`";`) {
		t.Fatalf("docs/tools/mock/fixtures.mjs MIRROR_PARENT_SCOPE_REFUSAL is not:\n%s", want)
	}
}

// Mirror objects that a mirror for exactly registry.redhat.io/rhoai does not
// take anything away from: the setup goes ahead.
func TestMirrorCreateGoesAheadWithoutAParentScope(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
	}{
		{"a namespace that only starts the same", map[string]string{nsIDMSList + "/x": `{"spec":{"imageDigestMirrors":[{"source":"registry.redhat.io/rho","mirrors":["mirror.corp.example/rho"]}]}}`}},
		{"a more specific repository", map[string]string{nsIDMSList + "/x": `{"spec":{"imageDigestMirrors":[{"source":"registry.redhat.io/rhoai/odh-dashboard-rhel9","mirrors":["mirror.corp.example/dashboard"]}]}}`}},
		{"another registry", map[string]string{nsIDMSList + "/x": `{"spec":{"imageDigestMirrors":[{"source":"registry.access.redhat.com","mirrors":["mirror.corp.example/access"]}]}}`}},
		// No real mirror: the Machine Config Operator ignores it
		// (registries.go mirrorsContainsARealMirror).
		{"a parent without a real mirror", map[string]string{nsIDMSList + "/x": `{"spec":{"imageDigestMirrors":[{"source":"registry.redhat.io","mirrors":["registry.redhat.io"]}]}}`}},
		{"a parent with no mirrors", map[string]string{nsIDMSList + "/x": `{"spec":{"imageDigestMirrors":[{"source":"registry.redhat.io"}]}}`}},
		// The source has an entry of its own already (an ITMS's tag mirror):
		// the parent's mirrors are not used for it now either
		// (registries.go:331-345), so nothing is taken away.
		{"a parent, and the source's own tag mirror", map[string]string{
			nsIDMSList + "/corp":  `{"spec":{"imageDigestMirrors":[{"source":"registry.redhat.io","mirrors":["mirror.corp.example/redhat"]}]}}`,
			nsITMSList + "/rhoai": `{"spec":{"imageTagMirrors":[{"source":"registry.redhat.io/rhoai","mirrors":["mirror.corp.example/rhoai"]}]}}`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := nodeSetupFake(t, "HighlyAvailable", false)
			for p, obj := range tc.files {
				f.putJSON(p, obj)
			}
			plan, _ := PlanNodeSetup(c, types.NodeSetupRequest{Action: node.MirrorCreate})
			if !plan.Ready {
				t.Fatalf("plan %+v", plan)
			}
		})
	}
	t.Run("a parent, and the source already redirected to quay.io/rhoai", func(t *testing.T) {
		f, c := nodeSetupFake(t, "HighlyAvailable", false)
		f.putJSON(nsIDMSList+"/corp", `{"spec":{"imageDigestMirrors":[{"source":"registry.redhat.io","mirrors":["mirror.corp.example/redhat"]},{"source":"registry.redhat.io/rhoai","mirrors":["mirror.corp.example/redhat/rhoai","quay.io/rhoai"]}]}}`)
		if plan, _ := PlanNodeSetup(c, types.NodeSetupRequest{Action: node.MirrorCreate}); !plan.NothingToDo {
			t.Fatalf("the administrator's own entry sets it up: plan %+v", plan)
		}
	})
}

// The other mirror lists fail closed: one that cannot be read refuses; an
// API that is not served (404) has no objects.
func TestMirrorCreateListsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		path      string
		status    int
		wantReady bool
		wantCode  string
	}{
		{nsICSPList, 403, false, "forbidden"},
		{nsICSPList, 500, false, "unreadable"},
		{nsITMSList, 403, false, "forbidden"},
		{nsICSPList, 404, true, ""},
		{nsITMSList, 404, true, ""},
	} {
		t.Run(tc.path+" "+http.StatusText(tc.status), func(t *testing.T) {
			f, c := nodeSetupFake(t, "HighlyAvailable", false)
			f.fail["GET "+tc.path] = tc.status
			plan, _ := PlanNodeSetup(c, types.NodeSetupRequest{Action: node.MirrorCreate})
			if plan.Ready != tc.wantReady || plan.RefusalCode != tc.wantCode {
				t.Fatalf("plan %+v", plan)
			}
			if !tc.wantReady && (!strings.Contains(plan.Refusal, "Cannot list the") || len(f.callsTo(http.MethodPost, nsIDMSList)) != 0) {
				t.Fatalf("plan %+v, posts %d", plan, len(f.callsTo(http.MethodPost, nsIDMSList)))
			}
		})
	}
}

// The status's image mirror (getIDMS, read with the ServiceAccount) counts
// an ImageContentSourcePolicy for the source too, as the nodes do; its
// list is read only when no IDMS has the source, and fails closed.
func TestGetIDMSCountsAnImageContentSourcePolicy(t *testing.T) {
	t.Run("an ICSP", func(t *testing.T) {
		f, c := newResourceFake(t)
		f.putJSON(nsICSPList+"/legacy", `{"spec":{"repositoryDigestMirrors":[{"source":"registry.redhat.io/rhoai","mirrors":["quay.io/rhoai"]}]}}`)
		got, err := node.ImageMirror(c)
		if err != nil || !got.Exists || got.Kind != node.KindICSP || got.Name != "legacy" || got.Source != rhoai.IDMSSource {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("an IDMS: the ICSPs are not read", func(t *testing.T) {
		f, c := newResourceFake(t)
		f.putJSON(nsIDMSPath, `{"spec":{"imageDigestMirrors":[{"source":"registry.redhat.io/rhoai","mirrors":["quay.io/rhoai"]}]}}`)
		got, err := node.ImageMirror(c)
		if err != nil || !got.Exists || got.Kind != node.KindIDMS || got.Name != node.MirrorName {
			t.Fatalf("got %+v, %v", got, err)
		}
		if n := len(f.callsTo(http.MethodGet, nsICSPList)); n != 0 {
			t.Fatalf("ICSPs listed %d times", n)
		}
	})
	t.Run("ICSPs not served", func(t *testing.T) {
		f, c := newResourceFake(t)
		f.fail["GET "+nsICSPList] = 404
		if got, err := node.ImageMirror(c); err != nil || got.Exists {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("ICSPs unreadable", func(t *testing.T) {
		f, c := newResourceFake(t)
		f.fail["GET "+nsICSPList] = 403
		if got, err := node.ImageMirror(c); err == nil || got.Exists {
			t.Fatalf("an unreadable list must be an error: got %+v, %v", got, err)
		}
	})
}
