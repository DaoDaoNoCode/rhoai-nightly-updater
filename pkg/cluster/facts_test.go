package cluster

// A change reads the cluster facts (the DSCInitialization, the
// DataScienceCluster, the control plane, the dashboard's and MLflow's
// addresses) with the ServiceAccount's read-only facts client, never with
// the signed-in user's token: a project user may read none of them, and
// the per-cluster caches all users share are filled by the ServiceAccount
// only.

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dashdev"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
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
	// Warm: the cached value is the ServiceAccount's; a user's client
	// without facts does not get it (it reads afresh and is refused).
	if got := rhoai.CachedControlPlane(bare); got != "" {
		t.Fatalf("a user's client without facts got the cached %q", got)
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

// The dashboard's address cache: filled only through facts; a user's
// client without facts neither fills it nor reads it when warm.
func TestDashboardURLCacheIsFilledOnlyThroughFacts(t *testing.T) {
	f, c := newResourceFake(t)
	const dashboards = "/apis/components.platform.opendatahub.io/v1alpha1/dashboards"
	f.putJSON(dashboards+"/default-dashboard", `{"status":{"url":"https://rh-ai.apps.example.com"}}`)
	f.failAs[fakeUserToken+" GET "+dashboards] = http.StatusForbidden
	bare := withoutFacts(userClientOf(c, fakeUserToken))
	if got := dashdev.CachedURL(bare); got != "" {
		t.Fatalf("cold, without facts: %q", got)
	}
	if _, ok := dashdev.URLCache.Get(c.BaseURL()); ok {
		t.Fatal("a user's client without facts filled the dashboard URL cache")
	}
	if got := dashdev.CachedURL(userClientOf(c, fakeUserToken)); got != "https://rh-ai.apps.example.com" {
		t.Fatalf("through facts: %q", got)
	}
	if u, ok := dashdev.URLCache.Get(c.BaseURL()); !ok || u != "https://rh-ai.apps.example.com" {
		t.Fatalf("cache %q %v", u, ok)
	}
	if got := dashdev.CachedURL(bare); got != "" {
		t.Fatalf("warm, without facts: got the cached %q", got)
	}
	if got := tokensOf(f, dashboards); !slices.Equal(got, []string{fakeUserToken, fakeSAToken, fakeUserToken}) {
		t.Fatalf("dashboard list reads with %v", got)
	}
}

// The signed-in user's client a ServiceAccount client of a read carries
// (withAdminRead's SetSignedInUser) reads the facts with that
// ServiceAccount client.
func TestSignedInUserKeepsTheFactsClient(t *testing.T) {
	f, sa := newResourceFake(t)
	u := sa.SignedInUser()
	if u == nil || !u.ActsAsUser() {
		t.Fatalf("signed-in user %v", u)
	}
	if fc := u.Facts(); fc.ActsAsUser() || fc.TestOptions().Token != fakeSAToken {
		t.Fatalf("facts as user %v with token %q", fc.ActsAsUser(), fc.TestOptions().Token)
	}
	f.failAs[fakeUserToken+" GET "+dsciV2ListPath] = http.StatusForbidden
	if ns, err := rhoai.DiscoverNamespaces(u); err != nil || ns.Applications != "redhat-ods-applications" {
		t.Fatalf("namespaces %+v, %v", ns, err)
	}
}

// Workbench start and delete read the DataScienceCluster through facts: a
// user whose token may not list it still gets MLflow linked on start, and
// a delete is still refused while the workbench component is off (before,
// both refused reads were skipped).
func TestWorkbenchStartAndTeardownReadTheDSCThroughFacts(t *testing.T) {
	stopped := `{"metadata":{"uid":"nb-1","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"},"annotations":{"kubeflow-resource-stopped":"2026-11-24T09:00:00Z"}},"spec":{"template":{"spec":{"containers":[{"name":"nightly-workbench","volumeMounts":[]}],"volumes":[]}}}}`
	t.Run("start", func(t *testing.T) {
		f, c := newResourceFake(t)
		putWorkbenchCluster(t, f, true)
		putToolWorkbench(t, f, c)
		f.putJSON(wbDSCv2+"/default-dsc", `{"metadata":{"name":"default-dsc"},"spec":{"components":{"workbenches":{"managementState":"Managed"},"mlflowoperator":{"managementState":"Managed"}}}}`)
		f.putJSON(wbNotebook, stopped)
		f.failAs[fakeUserToken+" GET "+wbDSCv2] = http.StatusForbidden
		if res := wbSteps(t, userClientOf(c, fakeUserToken), types.WorkbenchRequest{Action: "start", Project: "proj"}); !res.Success {
			t.Fatalf("start: %+v", res)
		}
		calls := f.callsTo("PATCH", wbNotebook)
		if len(calls) == 0 || !strings.Contains(calls[len(calls)-1].Body, "mlflow-instance") {
			t.Fatalf("start did not link MLflow: %+v", calls)
		}
	})
	t.Run("teardown", func(t *testing.T) {
		f, c := newResourceFake(t)
		putWorkbenchCluster(t, f, true)
		putToolWorkbench(t, f, c)
		f.putJSON(wbDSCv2+"/default-dsc", `{"metadata":{"name":"default-dsc"},"spec":{"components":{"workbenches":{"managementState":"Removed"}}}}`)
		f.failAs[fakeUserToken+" GET "+wbDSCv2] = http.StatusForbidden
		res := wbSteps(t, userClientOf(c, fakeUserToken), types.WorkbenchRequest{Action: "teardown", Project: "proj"})
		if res.Success || res.ErrorCode != "prerequisites" || len(f.callsTo("DELETE", wbNotebook)) != 0 {
			t.Fatalf("teardown: %+v", res)
		}
	})
}

// The playground finds the dashboard and reads the component's state and
// gen-ai-ui through facts, and calls the gen-ai API with the user's token.
// A not-ready operator is reported without the DataScienceCluster's
// condition message (the ServiceAccount's read).
func TestPlaygroundReadsClusterFactsThroughFacts(t *testing.T) {
	e := newPlaygroundEnv(t)
	var finder *Client
	t.Cleanup(features.SetGenAIForTest(func(c *Client) string { finder = c; return "https://dashboard.example.com" }, http.DefaultClient))
	u := e.userPlaygroundClient("user-token")
	api, problem, _ := features.PlaygroundAPI(u)
	if problem != "" || api.Token != "user-token" || finder == nil || finder.ActsAsUser() || !finder.ReadOnly() || finder.TestOptions().Token != testGenAIToken {
		t.Fatalf("api %+v problem %q; the dashboard found with %+v", api, problem, finder)
	}

	e2 := newPlaygroundEnv(t)
	e2.k8s.putJSON("/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/default-dsc", `{"spec":{"components":{"ogx":{"managementState":"Managed"}}},"status":{"conditions":[{"type":"OGXReady","status":"False","message":"secret-condition-detail"}]}}`)
	e2.k8s.failAs["user-token GET "+wbDSCv2] = http.StatusForbidden
	e2.k8s.failAs["user-token GET /apis/apps/v1/namespaces/redhat-ods-applications/deployments/gen-ai-ui"] = http.StatusForbidden
	var steps []UpdateStepEvent
	res, err := RunPlayground(e2.userPlaygroundClient("user-token"), setup(), func(s UpdateStepEvent) { steps = append(steps, s) })
	if err != nil || res.Success || res.ErrorCode != "component_not_ready" {
		t.Fatalf("got %+v, %v", res, err)
	}
	for _, s := range append(steps, UpdateStepEvent{Message: res.Message + strings.Join(res.Logs, " ")}) {
		if strings.Contains(s.Message, "secret-condition-detail") {
			t.Fatalf("the ServiceAccount's condition message reached the user: %q", s.Message)
		}
	}
}

// A failed fact read reaches the user and the log with its HTTP status and
// a fixed category only, never the API server's text.
func TestFactReadErrorsShowNoServerText(t *testing.T) {
	const detail = "system:serviceaccount:secret-ns:secret-sa cannot list"
	logs := captureSlog(t)
	f, c := newResourceFake(t)
	putWorkbenchCluster(t, f, true)
	f.fail["GET "+wbDSCv2] = http.StatusInternalServerError
	f.failMessage["GET "+wbDSCv2] = detail
	f.fail["GET "+mlflowPath] = http.StatusForbidden
	f.failMessage["GET "+mlflowPath] = detail
	u := userClientOf(c, fakeUserToken)
	var shown []string
	res, _ := RunWorkbench(u, types.WorkbenchRequest{Action: "setup", Project: "proj"}, func(e UpdateStepEvent) { shown = append(shown, e.Message) })
	shown = append(shown, res.Message)
	if !strings.Contains(res.Message, "HTTP 500") {
		t.Errorf("workbench: %q", res.Message)
	}
	reason, _ := envkit.SetupComponentBlock(u, envkit.PipelinesComponent)
	shown = append(shown, reason)
	_, _ = envkit.ReadProjectFacts(u)

	env := newSamplesEnv(t)
	env.k8s.fail["GET "+mlflowPath] = http.StatusForbidden
	env.k8s.failMessage["GET "+mlflowPath] = detail
	res, _ = RunMLflowSamples(env.user, mlflowRequest("add", SampleTraining), func(e UpdateStepEvent) { shown = append(shown, e.Message) })
	shown = append(shown, res.Message)
	if !strings.Contains(res.Message, "Cannot read MLflow's CR (HTTP 403)") {
		t.Errorf("samples: %q", res.Message)
	}
	for _, m := range append(shown, logs.String()) {
		if strings.Contains(m, "secret-ns") || strings.Contains(m, "secret-sa") {
			t.Fatalf("the API server's text was shown or logged: %q", m)
		}
	}
	if !strings.Contains(logs.String(), "fact=DataScienceCluster status=500 category=server_error") {
		t.Errorf("log lacks the fact, status and category:\n%s", logs.String())
	}
}

// ReadFirstDSC moves to the next served version only on a conversion
// failure (500, or 429 "storage is (re)initializing" whose Retry-After is
// not waited for) or a version that is not served; a generic 500 stops it,
// so the setup is refused rather than read at another version.
func TestDSCReadFallbackMovesOnOnlyForConversionFailures(t *testing.T) {
	const v3 = "/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters"
	for _, tc := range []struct {
		name, body string
		status     int
		wantCode   string
	}{
		{"429 watch cache, conversion", liveDSCv3Conversion429, http.StatusTooManyRequests, ""},
		{"500 conversion", liveDSCv3Conversion500, http.StatusInternalServerError, ""},
		{"generic 500", `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"InternalError","code":500,"message":"etcdserver: request timed out"}`, http.StatusInternalServerError, envkit.ErrorCodePrerequisitesUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFakeAPI(t)
			f.json("GET", "/apis/datasciencecluster.opendatahub.io", http.StatusOK, dscGroupDoc)
			f.json("GET", v3, tc.status, tc.body)
			if tc.status == http.StatusTooManyRequests {
				// As live: Retry-After 30, not waited for (NoRetryWhen).
				f.headers = map[string]http.Header{"GET " + v3: {"Retry-After": {"30"}}}
			}
			f.json("GET", wbDSCv2, http.StatusOK, `{"items":[{"metadata":{"name":"default-dsc"},"spec":{"components":{"aipipelines":{"managementState":"Managed"}}}}]}`)
			start := kube.Clock().Now()
			_, code := envkit.SetupComponentBlock(c.WithContext(context.Background()), envkit.PipelinesComponent)
			if code != tc.wantCode {
				t.Fatalf("code %q, want %q", code, tc.wantCode)
			}
			if n, waited := len(f.requests("GET", v3)), kube.Clock().Since(start); n != 1 || waited != 0 {
				t.Fatalf("v3 read %d times, waited %s", n, waited)
			}
			if v2 := len(f.requests("GET", wbDSCv2)); (tc.wantCode == "") != (v2 == 1) {
				t.Fatalf("v2 read %d times", v2)
			}
		})
	}
}

// MLflow samples ask MLflow with the token of the user whose projects were
// checked (SignedInUser), also when the client they get is a
// ServiceAccount client that carries that user: never with its own token.
func TestMLflowSamplesCallMLflowAsTheCheckedUser(t *testing.T) {
	env := newSamplesEnv(t)
	var asUser, asSA bool
	templ := env.mlflow.allow
	env.mlflow.allow = func(resource, verb, namespace, name string) bool {
		asSA = true
		return templ(resource, verb, namespace, name)
	}
	env.mlflow.userAllow = func(string, string, string, string) bool { asUser = true; return true }
	sa := *env.c
	sa.SetSignedInUser(env.user)
	res, _ := RunMLflowSamples(&sa, mlflowRequest("add", SampleTraining), nil)
	if !res.Success || !asUser || asSA {
		t.Fatalf("add: %+v; MLflow asked as the user %v, as the ServiceAccount %v", res, asUser, asSA)
	}
}
