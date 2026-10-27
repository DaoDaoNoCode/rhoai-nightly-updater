package cluster

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// noOutsideHosts makes every request through the registry and GitHub
// clients, and http.DefaultClient, fail the test: the gitops profile must
// not send one (the reference-graph test below proves it statically; these
// runtime checks cover the paths that replaced them).
func noOutsideHosts(t *testing.T) {
	t.Helper()
	installFakeRegistry(t, newFakeRegistry())
	fail := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("the gitops profile sent a request to %s", r.URL.Host)
		return nil, errors.New("no outside hosts in the gitops profile")
	})
	quayHTTPClient.Transport = fail
	orig := http.DefaultClient.Transport
	http.DefaultClient.Transport = fail
	t.Cleanup(func() { http.DefaultClient.Transport = orig })
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// requestedSecret reports whether the fake answered any Secret request.
func requestedSecret(f *fakeOLM) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if strings.Contains(r.Path, "/secrets") {
			return true
		}
	}
	return false
}

// Update and its dry run read no Secret, say that the cluster's own pull
// secrets are used, and take the build from the catalog's PackageManifest.
func TestGitOpsProfileUpdateReadsNoSecretAndNoRegistry(t *testing.T) {
	noOutsideHosts(t)
	orig := lookupTargetBundle
	t.Cleanup(func() { lookupTargetBundle = orig })
	lookupTargetBundle = func(*Client, string) (string, error) {
		t.Error("the gitops dry run read the bundle from the registry")
		return "", errors.New("registry")
	}

	f := servesGitOpsPolicy(t, gitOpsFake(t))
	emit, events := eventsRecorder()
	result, err := UpdateStreamGitOpsProfile(f.client(context.Background()), gitOpsImage, emit)
	if err != nil || !result.Success || indexOf(result.Logs, gitOpsProfileNoPullSecret) < 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	said := false
	for _, e := range events() {
		said = said || e.Message == gitOpsProfileNoPullSecret
	}
	if !said || requestedSecret(f) {
		t.Fatalf("said=%v, Secret requested=%v", said, requestedSecret(f))
	}

	f = servesGitOpsPolicy(t, gitOpsFake(t))
	dry, err := UpdateDryRunGitOpsProfile(f.client(context.Background()), gitOpsImage)
	if err != nil || !dry.Success || indexOf(dry.Logs, gitOpsProfileNoPullSecret) < 0 || requestedSecret(f) ||
		indexOf(dry.Logs, "OK: The catalog serves "+gitOpsCSV+" on channel "+gitOpsChannel) < 0 {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
}

// The dry run's downgrade check uses the head the catalog serves.
func TestGitOpsProfileDryRunVersionFromTheCatalog(t *testing.T) {
	noOutsideHosts(t)
	f := servesGitOpsPolicy(t, gitOpsFake(t))
	f.channels = `[{"name":"beta","currentCSV":"rhods-operator.3.5.0"}]`
	dry, err := UpdateDryRunGitOpsProfile(f.client(context.Background()), gitOpsImage)
	if err != nil || dry.Success || dry.ErrorCode != "validation" {
		t.Fatalf("an older head must be refused as a downgrade: %+v %v", dry, err)
	}
	// While the catalog does not serve the image yet, the release tag is
	// compared, and the log says why.
	f = servesGitOpsPolicy(t, gitOpsFake(t))
	f.gitOpsPods = false
	dry, err = UpdateDryRunGitOpsProfile(f.client(context.Background()), gitOpsImage)
	if err != nil || !dry.Success || !strings.Contains(strings.Join(dry.Logs, "\n"), "the catalog does not serve this build yet (OLM is replacing the catalog pod)") {
		t.Fatalf("catalog not serving yet: %+v %v", dry, err)
	}
}

// Status in the gitops profile: no Secret, no Quay; the installed build is
// the catalog image's reference, Latest is empty.
func TestGetStatusGitOps(t *testing.T) {
	noOutsideHosts(t)
	f := gitOpsFake(t)
	status, err := GetStatusGitOps(f.client(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	n := status.Nightly
	if n == nil || n.Installed == nil || n.Installed.Image != gitOpsImage || n.Installed.Tag != "rhoai-3.6" || n.Installed.Digest != "sha256:old" ||
		n.Latest != nil || n.UpdateAvailable != nil || n.Error != "" {
		t.Fatalf("nightly = %+v", n)
	}
	if status.PullSecret.Exists || requestedSecret(f) {
		t.Fatalf("the pull secret was read: %+v", status.PullSecret)
	}
	if status.CatalogServes == nil || status.CatalogServes.CSV != gitOpsCSV {
		t.Fatalf("catalogServes = %+v", status.CatalogServes)
	}
}

// Components in the gitops profile: no image labels (not even cached ones,
// which only the full profile fills), no GitHub sample; the DSC defaults
// come from the CSV's alm-examples or are reported missing.
func TestGetComponentsGitOps(t *testing.T) {
	noOutsideHosts(t)
	labelCache.Add("rhoai/odh-dashboard-rhel9@sha256:"+strings.Repeat("1", 64), labelCacheValue{labels: ImageLabels{GitCommit: "abc", BuildDate: "2026-10-01"}}, 0)
	dsc := mockResponse{body: `{"items":[{"apiVersion":"datasciencecluster.opendatahub.io/v2","metadata":{"name":"default-dsc"},"spec":{"components":{"dashboard":{"managementState":"Managed"}}}}]}`}
	c, _ := componentsAPI(t, 0, dsc, notFoundResp, "")
	resp, err := GetComponentsGitOps(c)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.ImageLabelsOff || resp.Deployments[0].GitCommit != "" {
		t.Fatalf("labels: off=%v %+v", resp.ImageLabelsOff, resp.Deployments)
	}
	if resp.DSCCompatibility == nil || !strings.Contains(resp.DSCCompatibility.DefaultsError, "the gitops profile does not read the GitHub sample") {
		t.Fatalf("compatibility = %+v", resp.DSCCompatibility)
	}
	c, _ = componentsAPI(t, 0, dsc, notFoundResp, almExamplesJSON)
	if resp, err := GetComponentsGitOps(c); err != nil || resp.DSCCompatibility == nil || resp.DSCCompatibility.DefaultsSource != "csv" {
		t.Fatalf("alm-examples: %+v %v", resp, err)
	}
}

// The operator build comes from the installed CSV's annotations.
func TestInstalledOperatorBuild(t *testing.T) {
	f, c := newFakeAPI(t)
	f.json("GET", namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName), 200, `{"status":{"installedCSV":"rhods-operator.3.6.0"}}`)
	f.json("GET", namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "rhods-operator.3.6.0"), 200,
		`{"metadata":{"annotations":{"containerImage":"registry.redhat.io/rhoai/odh-rhel9-operator@sha256:abc","createdAt":"2026-10-08T04:00:00Z"}},"spec":{"version":"3.6.0"},"status":{"phase":"Succeeded"}}`)
	op, err := getInstalledOperator(c)
	if err != nil || op.ContainerImage != "registry.redhat.io/rhoai/odh-rhel9-operator@sha256:abc" || op.CreatedAt != "2026-10-08T04:00:00Z" {
		t.Fatalf("%+v %v", op, err)
	}
}

// Diagnostics in the gitops profile: every check but the pull secret's, in
// the same order, then the Secret-access check, and no Secret read.
func TestGitOpsDiagnosticChecks(t *testing.T) {
	var want []string
	for _, chk := range diagnosticChecks {
		if chk.name == secretAccessCheckName {
			t.Fatal("the full profile reads the pull secret; the Secret-access check is the gitops profile's only")
		}
		if chk.name != "Pull secret" {
			want = append(want, chk.name)
		}
	}
	want = append(want, secretAccessCheckName)
	var got []string
	for _, chk := range gitOpsDiagnosticChecks {
		got = append(got, chk.name)
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("gitOpsDiagnosticChecks = %v, want diagnosticChecks without the pull secret: %v", got, want)
	}
	noOutsideHosts(t)
	f := gitOpsFake(t)
	resp, err := DiagnoseClusterGitOps(f.client(context.Background()))
	if err != nil || findCheck(resp, "Pull secret") != nil || requestedSecret(f) {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

// The gitops profile never talks to Quay or any other registry, or to any
// host but the API server: every page's build information comes from the
// cluster (docs/GITOPS.md, section 11). Checked on the same reference graph
// as the profile's RBAC (rbac_gitops_test.go): no function of the three
// packages that sends an HTTP request, other than the API client itself,
// may be reachable from the gitops profile's roots.

// networkSinks are the standard library calls that send a request.
var networkSinks = []string{
	"net/http.Client.Do", "net/http.Client.Get", "net/http.Client.Head", "net/http.Client.Post", "net/http.Client.PostForm",
	"net/http.Get", "net/http.Head", "net/http.Post", "net/http.PostForm",
	"net/http.Transport.RoundTrip", "net.Dial", "net.DialTimeout", "net.Dialer.Dial", "net.Dialer.DialContext",
}

// apiServerClient is the one function allowed to send requests in the
// gitops profile: the Kubernetes API client (client.go), with the
// ServiceAccount's token, to the API server only.
const apiServerClient = clusterPkg + ".Client.do"

// outboundFuncs are the functions of the three packages that send a
// request themselves, except the API client.
func outboundFuncs(pg *profileGraph) []string {
	var out []string
	for from, tos := range pg.graph {
		if from == apiServerClient || strings.HasPrefix(from, "var ") {
			continue
		}
		for _, sink := range networkSinks {
			if tos[sink] {
				out = append(out, from)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// pathTo is a shortest chain of references from the gitops roots to node.
func (pg *profileGraph) pathTo(node string) []string {
	pruned := apiPkg + ".Register"
	parent := map[string]string{}
	var queue []string
	for _, r := range append(pg.roots(), "var "+apiPkg+".gitOpsRoutes") {
		parent[r] = ""
		queue = append(queue, r)
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if n == node {
			var path []string
			for p := n; p != ""; p = parent[p] {
				path = append([]string{strings.TrimPrefix(strings.TrimPrefix(p, "var "), modulePath+"/pkg/")}, path...)
			}
			return path
		}
		next := make([]string, 0, len(pg.graph[n]))
		for to := range pg.graph[n] {
			next = append(next, to)
		}
		sort.Strings(next)
		for _, to := range next {
			if _, seen := parent[to]; !seen && to != pruned {
				parent[to] = n
				queue = append(queue, to)
			}
		}
	}
	return nil
}

func TestGitOpsProfileReachesNoRegistry(t *testing.T) {
	pg := loadProfileGraph(t)
	all := outboundFuncs(pg)
	// The check must see the registry clients of the full profile, or it
	// proves nothing.
	for _, want := range []string{clusterPkg + ".fetchQuayToken", apiPkg + ".registryGet"} {
		found := false
		for _, f := range all {
			found = found || f == want
		}
		if !found {
			t.Fatalf("outboundFuncs misses %s; found %v", want, all)
		}
	}
	reach := pg.gitOpsReach()
	var reached []string
	for _, f := range all {
		if reach[f] {
			reached = append(reached, strings.Join(pg.pathTo(f), " -> "))
		}
	}
	if len(reached) > 0 {
		t.Fatalf("the gitops profile reaches %d functions that send requests to other hosts than the API server:\n%s", len(reached), strings.Join(reached, "\n"))
	}
}
