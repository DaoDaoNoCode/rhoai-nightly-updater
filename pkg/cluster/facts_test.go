package cluster

// A change reads the cluster facts (the DSCInitialization, the
// DataScienceCluster, the control plane, the dashboard's and MLflow's
// addresses) with the ServiceAccount's read-only facts client, never with
// the signed-in user's token: a project user may read none of them, and
// the per-cluster caches all users share are filled by the ServiceAccount
// only.

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const dsciV2ListPath = "/apis/dscinitialization.opendatahub.io/v2/dscinitializations"

// tokensOf are the tokens of the GETs of path.
func tokensOf(f *resourceFake, path string) []string {
	var out []string
	for _, c := range f.callsTo(http.MethodGet, path) {
		out = append(out, c.Token)
	}
	return out
}

// withoutFacts is u without its facts client.
func withoutFacts(u *Client) *Client {
	n := *u
	editTestClient(&n, func(o *testClientOptions) { o.Facts = nil })
	return &n
}

// A project user (whose token may not list DSCInitializations) gets the
// applications namespace from a cold cache: the facts client reads it.
func TestNamespacesColdCacheNonAdminUsesFacts(t *testing.T) {
	f, c := newResourceFake(t)
	f.failAs[fakeUserToken+" GET "+dsciV2ListPath] = http.StatusForbidden
	u := userClientOf(c, fakeUserToken)
	ns, err := rhoai.DiscoverNamespaces(u)
	if err != nil || ns.Applications != "redhat-ods-applications" {
		t.Fatalf("namespaces %+v, %v", ns, err)
	}
	if got := tokensOf(f, dsciV2ListPath); len(got) != 1 || got[0] != fakeSAToken {
		t.Fatalf("DSCInitialization read with %v, want the ServiceAccount's token once", got)
	}
}

// A client of the user's token without a facts client is refused, cold
// cache or warm, and neither reads the DSCInitialization nor fills the
// cache: the answer never depends on what an earlier read cached.
func TestUserClientNeitherReadsNorFillsTheNamespaceCache(t *testing.T) {
	f, c := newResourceFake(t)
	u := withoutFacts(userClientOf(c, fakeUserToken))
	if _, err := rhoai.DiscoverNamespaces(u); !errors.Is(err, kube.ErrNoFactsClient) {
		t.Fatalf("cold cache: %v, want ErrNoFactsClient", err)
	}
	if n := len(f.callsTo(http.MethodGet, dsciV2ListPath)); n != 0 {
		t.Fatalf("the user's client read the DSCInitialization %d times", n)
	}
	if _, ok := rhoai.NamespacesCache.Get(c.BaseURL()); ok {
		t.Fatal("the user's client filled the namespace cache")
	}
	if _, err := rhoai.DiscoverNamespaces(c); err != nil {
		t.Fatal(err)
	}
	if _, err := rhoai.DiscoverNamespaces(u); !errors.Is(err, kube.ErrNoFactsClient) {
		t.Fatalf("warm cache: %v, want ErrNoFactsClient", err)
	}
}

// The control plane and the dashboard's address are cached per cluster:
// only a ServiceAccount (facts) read fills those caches, and a user's
// client without a facts client reads afresh and caches nothing.
func TestClusterFactCachesAreFilledOnlyThroughFacts(t *testing.T) {
	f, c := newResourceFake(t)
	f.putJSON("/apis/config.openshift.io/v1/infrastructures/cluster", `{"status":{"controlPlaneTopology":"External"}}`)
	f.failAs[fakeUserToken+" GET /apis/config.openshift.io/v1/infrastructures/cluster"] = http.StatusForbidden
	bare := withoutFacts(userClientOf(c, fakeUserToken))
	if got := rhoai.CachedControlPlane(bare); got != "" {
		t.Fatalf("a user's client without facts: %q, want unknown", got)
	}
	if _, ok := rhoai.ControlPlaneCache.Get(c.BaseURL()); ok {
		t.Fatal("a user's client filled the control plane cache")
	}
	if got := rhoai.CachedControlPlane(userClientOf(c, fakeUserToken)); got != rhoai.ControlPlaneHosted {
		t.Fatalf("through facts: %q", got)
	}
	if cp, ok := rhoai.ControlPlaneCache.Get(c.BaseURL()); !ok || cp != rhoai.ControlPlaneHosted {
		t.Fatalf("cache %q %v", cp, ok)
	}
	// One read with the user's token (the client without facts, refused),
	// one with the ServiceAccount's (through facts).
	if got := tokensOf(f, "/apis/config.openshift.io/v1/infrastructures/cluster"); !slices.Equal(got, []string{fakeUserToken, fakeSAToken}) {
		t.Fatalf("Infrastructure reads with %v", got)
	}
}

// SetupComponentBlock reads the DataScienceCluster with the facts client:
// for a user whose token may not list it, a component that is off still
// blocks the setup (before, the refused read blocked nothing).
func TestSetupComponentBlockReadsThroughFacts(t *testing.T) {
	f, c := newResourceFake(t)
	putDSC(f, `{"aipipelines":{"managementState":"Removed"}}`)
	f.failAs[fakeUserToken+" GET "+wbDSCv2] = http.StatusForbidden
	reason, code := envkit.SetupComponentBlock(userClientOf(c, fakeUserToken), envkit.PipelinesComponent)
	if code != "prerequisites" || !strings.Contains(reason, "aipipelines is Removed") {
		t.Fatalf("reason %q code %q", reason, code)
	}
	for _, tok := range tokensOf(f, wbDSCv2) {
		if tok != fakeSAToken {
			t.Fatalf("the DataScienceCluster was read with %q", tok)
		}
	}
}

// A DataScienceCluster that cannot be read at any served version blocks
// the setups that check it (fail closed), with prerequisites_unknown, the
// read's HTTP status and not its text, and nothing is created.
func TestSetupComponentBlockFailsClosed(t *testing.T) {
	t.Run("pipeline server", func(t *testing.T) {
		f, c := newResourceFake(t)
		putReadyManagedS3(f)
		putDSProject(f, "p1")
		f.fail["GET "+wbDSCv2] = http.StatusInternalServerError
		f.failMessage["GET "+wbDSCv2] = "etcd says secret-detail"
		resp, _ := SetupPipelineServer(userClientOf(c, fakeUserToken), "p1")
		if resp.Success || resp.ErrorCode != envkit.ErrorCodePrerequisitesUnknown || !strings.Contains(resp.Message, "Cannot read whether Data Science Pipelines is turned on") ||
			!strings.Contains(resp.Message, "HTTP 500") || !strings.Contains(resp.Message, "Nothing was changed") || strings.Contains(resp.Message, "secret-detail") {
			t.Fatalf("got %+v", resp)
		}
		if m := f.mutations(); len(m) != 0 {
			t.Fatalf("changes made: %v", m)
		}
	})
	t.Run("MLflow", func(t *testing.T) {
		f, c := newResourceFake(t)
		f.fail["GET "+wbDSCv2] = http.StatusInternalServerError
		resp, _ := SetupMLflow(userClientOf(c, fakeUserToken))
		if resp.Success || resp.ErrorCode != envkit.ErrorCodePrerequisitesUnknown {
			t.Fatalf("got %+v", resp)
		}
		if f.get(mlflowPath) != nil {
			t.Error("the MLflow CR was created")
		}
	})
}

// A served version whose objects the conversion webhook cannot convert is
// skipped (ReadFirstDSC): the next version answers, so it blocks nothing.
// Only when every version fails is the setup refused.
func TestConversionErrorOnOneVersionDoesNotBlock(t *testing.T) {
	const v3 = "/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters"
	conversion := func(f *resourceFake, path string) {
		f.fail["GET "+path] = http.StatusInternalServerError
		f.failMessage["GET "+path] = "conversion webhook for datasciencecluster.opendatahub.io/v2, Kind=DataScienceCluster failed: no kind is registered for version v3"
	}
	for _, tc := range []struct {
		name, state, wantCode string
		v2Broken              bool
	}{
		{"v3 broken, v2 Managed", "Managed", "", false},
		{"v3 broken, v2 Removed", "Removed", "prerequisites", false},
		{"every version broken", "Managed", envkit.ErrorCodePrerequisitesUnknown, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newResourceFake(t)
			f.groups = map[string]string{}
			f.groups["/apis/datasciencecluster.opendatahub.io"] = `{"kind":"APIGroup","name":"datasciencecluster.opendatahub.io","versions":[{"groupVersion":"datasciencecluster.opendatahub.io/v3","version":"v3"},{"groupVersion":"datasciencecluster.opendatahub.io/v2","version":"v2"}],"preferredVersion":{"groupVersion":"datasciencecluster.opendatahub.io/v3","version":"v3"}}`
			putDSC(f, `{"aipipelines":{"managementState":"`+tc.state+`"}}`)
			conversion(f, v3)
			if tc.v2Broken {
				conversion(f, wbDSCv2)
			}
			_, code := envkit.SetupComponentBlock(userClientOf(c, fakeUserToken), envkit.PipelinesComponent)
			if code != tc.wantCode {
				t.Fatalf("code %q, want %q", code, tc.wantCode)
			}
			if len(f.callsTo(http.MethodGet, v3)) == 0 {
				t.Fatal("v3 was not tried first")
			}
		})
	}
}

// Workbench setup reads the DataScienceCluster with the facts client: a
// user whose token may not list it still gets the workbench (before, the
// setup stopped at "Cannot read the DataScienceCluster").
func TestWorkbenchSetupReadsTheDSCThroughFacts(t *testing.T) {
	f, c := newResourceFake(t)
	putWorkbenchCluster(t, f, true)
	f.failAs[fakeUserToken+" GET "+wbDSCv2] = http.StatusForbidden
	u := userClientOf(c, fakeUserToken)
	u.SetUsername("kube:admin")
	if res := wbSteps(t, u, types.WorkbenchRequest{Action: "setup", Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	if got := tokensOf(f, wbDSCv2); len(got) == 0 || slices.Contains(got, fakeUserToken) {
		t.Fatalf("DataScienceCluster reads with %v", got)
	}
}

// MLflow samples find MLflow (its cluster-scoped CR) with the facts
// client, and only after the project is found among the user's own.
func TestMLflowSamplesFindMLflowThroughFacts(t *testing.T) {
	env := newSamplesEnv(t)
	env.k8s.failAs[samplesUserToken+" GET "+mlflowPath] = http.StatusForbidden
	resp, _ := RunMLflowSamples(env.user, types.MLflowSamplesRequest{Project: "other", Action: "add", Samples: []string{SampleTraining}}, nil)
	if resp.Success || resp.ErrorCode != "validation" {
		t.Fatalf("other project: %+v", resp)
	}
	if n := len(env.k8s.callsTo(http.MethodGet, mlflowPath)); n != 0 {
		t.Fatalf("MLflow was read %d times for a project that is not the user's", n)
	}
	putMLflow(env.k8s, toolLabelJSON, envkit.ToolFieldManager, "Update", "", `{}`, `{"url":"https://x/mlflow","conditions":[{"type":"Available","status":"False"}]}`)
	resp, _ = RunMLflowSamples(env.user, mlflowRequest("add", SampleTraining), nil)
	if resp.Success || resp.ErrorCode != "prerequisites" || !strings.Contains(resp.Message, "not ready") {
		t.Fatalf("not ready: %+v", resp)
	}
	if got := tokensOf(env.k8s, mlflowPath); len(got) == 0 || slices.Contains(got, samplesUserToken) {
		t.Fatalf("MLflow CR reads with %v", got)
	}
}

// ReadProjectFacts gives the fixed facts from the ServiceAccount's reads,
// names what it could not read with fixed codes, and refuses a user's
// client without a facts client.
func TestReadProjectFacts(t *testing.T) {
	f, c := newResourceFake(t)
	putManagedDSC(f)
	f.putJSON("/apis/config.openshift.io/v1/infrastructures/cluster", `{"status":{"controlPlaneTopology":"External"}}`)
	u := userClientOf(c, fakeUserToken)
	f.failAs[fakeUserToken+" GET "+wbDSCv2] = http.StatusForbidden
	got, err := envkit.ReadProjectFacts(u)
	if err != nil || got.ApplicationsNamespace != "redhat-ods-applications" || got.DSC != types.ProjectFactsDSCPresent ||
		got.ControlPlane != types.ProjectFactsControlPlaneHosted || len(got.Problems) != 0 || len(got.Components) == 0 {
		t.Fatalf("facts %+v, %v", got, err)
	}
	if _, err := envkit.ReadProjectFacts(withoutFacts(u)); !errors.Is(err, kube.ErrNoFactsClient) {
		t.Fatalf("a user's client without facts: %v", err)
	}

	f2, c2 := newResourceFake(t)
	f2.fail["GET "+wbDSCv2] = http.StatusInternalServerError
	f2.fail["GET "+dsciV2ListPath] = http.StatusInternalServerError
	got, err = envkit.ReadProjectFacts(c2)
	if err != nil || got.DSC != "" || got.ApplicationsNamespace != "" || got.ControlPlane != "" ||
		!slices.Equal(got.Problems, []string{types.ProjectFactsProblemApplicationsNamespace, types.ProjectFactsProblemDSC, types.ProjectFactsProblemControlPlane}) {
		t.Fatalf("unreadable facts %+v, %v", got, err)
	}
}
