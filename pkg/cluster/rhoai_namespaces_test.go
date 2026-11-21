package cluster

import (
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/diagnostics"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/mlflow"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
)

// Every feature that named redhat-ods-applications reads the
// DSCInitialization's namespace instead (custom-apps here), and none guesses
// a name when it cannot be read.

func TestDebugInfoUsesTheDiscoveredNamespace(t *testing.T) {
	f, c := newFakeAPI(t)
	serveDSCI(f, fixtureAppsNS)
	f.json("GET", "/api/v1/namespaces/"+rhoai.SubNS+"/pods", 200, `{"items":[]}`)
	f.json("GET", "/api/v1/namespaces/"+fixtureAppsNS+"/pods", 200, `{"items":[{"metadata":{"name":"app-pod","namespace":"`+fixtureAppsNS+`"}}]}`)
	f.json("GET", "/api/v1/namespaces/"+rhoai.CatalogNS+"/pods", 200, `{"items":[]}`)
	resp, err := GetDebugInfo(c)
	if err != nil || len(resp.ApplicationPods) != 1 || len(resp.Warnings) != 0 {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}

	f2, c2 := newFakeAPI(t)
	f2.status("GET", dsciV2List, 403, "Forbidden")
	f2.json("GET", "/api/v1/namespaces/"+rhoai.SubNS+"/pods", 200, `{"items":[]}`)
	f2.json("GET", "/api/v1/namespaces/"+rhoai.CatalogNS+"/pods", 200, `{"items":[]}`)
	resp, err = GetDebugInfo(c2)
	if err != nil || len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], "the applications namespace") {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	for _, r := range f2.allRequests() {
		if strings.Contains(r.Path, "redhat-ods-applications") {
			t.Fatalf("guessed the namespace: %s", r.Path)
		}
	}
}

// Assist-rollout's allowlist is the operator namespace and the discovered
// applications namespace; unknown, only the operator namespace (fail closed).
func TestRolloutNamespacesAreDiscovered(t *testing.T) {
	f, c := newFakeAPI(t)
	serveDSCI(f, fixtureAppsNS)
	if ok, why := components.IsRolloutNamespace(c, fixtureAppsNS); !ok {
		t.Fatalf("custom applications namespace refused: %s", why)
	}
	if ok, _ := components.IsRolloutNamespace(c, "redhat-ods-applications"); ok {
		t.Fatal("the default name is allowed on a cluster that does not use it")
	}

	f2, c2 := newFakeAPI(t)
	f2.status("GET", dsciV2List, 500, "InternalError")
	if ok, why := components.IsRolloutNamespace(c2, "redhat-ods-applications"); ok || !strings.Contains(why, "cannot be told") {
		t.Fatalf("ok=%v why=%q", ok, why)
	}
	if ok, _ := components.IsRolloutNamespace(c2, rhoai.SubNS); !ok {
		t.Fatal("the operator namespace needs no discovery")
	}
	r, err := AssistRolloutFor(c2, "redhat-ods-applications", "x")
	if err != nil || r.Success || r.ErrorCode != "validation" || len(f2.writes()) != 0 {
		t.Fatalf("result=%+v err=%v writes=%v", r, err, f2.writes())
	}
}

func TestMLflowStatusNamespaceIsDiscovered(t *testing.T) {
	f, c := newResourceFake(t)
	f.putJSON(dsciV2List+"/default-dsci", `{"metadata":{"name":"default-dsci"},"spec":{"applicationsNamespace":"`+fixtureAppsNS+`"}}`)
	putMLflow(f, toolLabelJSON, envkit.ToolFieldManager, "Apply", "", `{}`, `{"conditions":[{"type":"Ready","status":"True"}]}`)
	st := mlflow.Status(c)
	if st.Namespace != fixtureAppsNS {
		t.Fatalf("namespace %q, want %q", st.Namespace, fixtureAppsNS)
	}

	f2, c2 := newResourceFake(t)
	f2.fail["GET "+dsciV2List] = 403
	putMLflow(f2, toolLabelJSON, envkit.ToolFieldManager, "Apply", "", `{}`, `{"conditions":[{"type":"Ready","status":"True"}]}`)
	st = mlflow.Status(c2)
	if st.Namespace != "" || !st.Deployed || !strings.Contains(st.Message, "namespace it runs in is unknown") {
		t.Fatalf("state %+v", st)
	}
}

// The RHOAI pods check scans the applications and monitoring namespaces
// the DSCInitialization names; unknown, it warns instead of passing.
func TestRHOAIPodNamespacesAreDiscovered(t *testing.T) {
	f, c := newFakeAPI(t)
	f.json("GET", dsciV2List, 200, `{"items":[{"metadata":{"name":"d"},"spec":{"applicationsNamespace":"apps-x","monitoring":{"namespace":"mon-x"}}}]}`)
	got, err := diagnostics.RHOAIPodNamespaces(c)
	if err != nil || !slices.Equal(got, []string{"apps-x", "mon-x"}) {
		t.Fatalf("got %v, %v", got, err)
	}

	f2, c2 := newFakeAPI(t)
	f2.status("GET", dsciV2List, 403, "Forbidden")
	f2.json("GET", "/api/v1/namespaces", 200, `{"items":[]}`)
	out := diagnostics.CheckRHOAIPods(c2)
	if out.Check.Status != "warn" || !strings.Contains(out.Check.Detail, "could not tell the applications and monitoring namespaces") {
		t.Fatalf("check %+v", out.Check)
	}
}

// Without a known applications namespace, the snapshot fails (non-fatal to
// its callers) rather than recording a guessed namespace; with none yet it
// records the operator's Deployments only.
func TestSnapshotNamespaces(t *testing.T) {
	f, c := newFakeAPI(t)
	f.status("GET", dsciV2List, 500, "InternalError")
	if err := components.SaveDeploymentSnapshot(c); err == nil {
		t.Fatal("snapshot saved without the applications namespace")
	}
}

// MaaS's derived infrastructure namespace fails closed: setup refuses.
func TestMaaSInfraNamespaceUnknownRefuses(t *testing.T) {
	f, c := newFakeAPI(t)
	f.status("GET", dsciV2List, 403, "Forbidden")
	if ns, err := serving.MaaSInfraNamespace(c); err == nil || ns != "" {
		t.Fatalf("ns=%q err=%v", ns, err)
	}
	serveDSCI(f, "opendatahub")
	rhoai.NamespacesCache.Purge()
	if ns, err := serving.MaaSInfraNamespace(c); err != nil || ns != "odh-ai-gateway-infra" {
		t.Fatalf("ns=%q err=%v", ns, err)
	}
}
