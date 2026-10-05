package cluster

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// statusAPI is a fake API server for GetStatus that counts requests per path.
type statusAPI struct {
	mu       sync.Mutex
	counts   map[string]int
	queries  map[string][]string
	sub      string // Subscription body; "" = 404
	csvByNm  map[string]string
	csvList  string
	csImage  string
	failPath map[string]int
}

func (a *statusAPI) count(path string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.counts[path]
}

func (a *statusAPI) handler(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.counts[r.URL.Path]++
	a.queries[r.URL.Path] = append(a.queries[r.URL.Path], r.URL.RawQuery)
	fail := a.failPath[r.URL.Path]
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	notFound := func() {
		w.WriteHeader(404)
		fmt.Fprint(w, `{"kind":"Status","status":"Failure","reason":"NotFound","code":404}`)
	}
	if fail != 0 {
		w.WriteHeader(fail)
		fmt.Fprintf(w, `{"kind":"Status","status":"Failure","code":%d}`, fail)
		return
	}
	p := r.URL.Path
	csvPrefix := namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "")
	switch {
	case p == namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName):
		if a.sub == "" {
			notFound()
			return
		}
		fmt.Fprint(w, a.sub)
	case p == csvPrefix:
		fmt.Fprint(w, a.csvList)
	case strings.HasPrefix(p, csvPrefix+"/"):
		body, ok := a.csvByNm[strings.TrimPrefix(p, csvPrefix+"/")]
		if !ok {
			notFound()
			return
		}
		fmt.Fprint(w, body)
	case p == namespacedPath("packages.operators.coreos.com/v1", "packagemanifests", CatalogNS, ""):
		fmt.Fprint(w, stableManifestJSON("redhat-operators", "stable-3.5", []map[string]interface{}{
			{"name": "stable-3.5", "currentCSV": "rhods-operator.v3.5.0", "currentCSVDesc": map[string]string{"version": "3.5.0"}},
		}))
	case p == namespacedPath("operators.coreos.com/v1alpha1", "catalogsources", CatalogNS, CatalogName):
		if a.csImage == "" {
			notFound()
			return
		}
		fmt.Fprintf(w, `{"spec":{"image":%q},"status":{"connectionState":{"lastObservedState":"READY"}}}`, a.csImage)
	case p == "/apis/config.openshift.io/v1/consoles/cluster":
		fmt.Fprint(w, `{"status":{"consoleURL":"https://console.example"}}`)
	case strings.HasSuffix(p, "/secrets/additional-pull-secret"):
		cfg := base64.StdEncoding.EncodeToString([]byte(`{"auths":{"quay.io/rhoai":{"auth":"dXNlcjpwYXNz"}}}`))
		fmt.Fprintf(w, `{"data":{".dockerconfigjson":%q}}`, cfg)
	case strings.HasSuffix(p, "/datascienceclusters"):
		fmt.Fprint(w, `{"items":[{"metadata":{"name":"default-dsc"}}]}`)
	case strings.HasSuffix(p, "/configmaps/rhoai-updater-activity"):
		fmt.Fprint(w, `{"data":{}}`)
	default:
		fmt.Fprint(w, `{"items":[]}`)
	}
}

func newStatusAPI(t *testing.T, a *statusAPI) *Client {
	t.Helper()
	a.counts, a.queries = map[string]int{}, map[string][]string{}
	if a.failPath == nil {
		a.failPath = map[string]int{}
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(a.handler))
	t.Cleanup(srv.Close)
	stableTargetCache.Purge()
	consoleURLCache.Purge()
	t.Cleanup(stableTargetCache.Purge)
	t.Cleanup(consoleURLCache.Purge)
	return &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: context.Background(), username: "tester"}
}

const csv360 = `{"metadata":{"name":"rhods-operator.3.6.0"},"spec":{"version":"3.6.0","displayName":"Red Hat OpenShift AI"},"status":{"phase":"Succeeded"}}`

func TestGetStatus_ReadsInstalledCSVByNameAndCachesStableTarget(t *testing.T) {
	t.Setenv("STABLE_SOURCE", "redhat-operators")
	t.Setenv("STABLE_CHANNEL", "")
	installFakeRegistry(t, newFakeRegistry())
	a := &statusAPI{
		sub:     `{"spec":{"source":"redhat-operators","channel":"stable"},"status":{"state":"AtLatestKnown","installedCSV":"rhods-operator.3.6.0"}}`,
		csvByNm: map[string]string{"rhods-operator.3.6.0": csv360},
		csvList: `{"items":[]}`,
	}
	c := newStatusAPI(t, a)
	for i := 0; i < 3; i++ {
		status, err := GetStatus(c)
		if err != nil {
			t.Fatal(err)
		}
		if status.CSV.Name != "rhods-operator.3.6.0" || status.CSV.Phase != "Succeeded" || status.CSV.Version != "3.6.0" {
			t.Fatalf("csv %+v", status.CSV)
		}
		if status.Subscription.State != "AtLatestKnown" || status.StableChannel != "stable-3.5" || status.ConsoleURL != "https://console.example" {
			t.Fatalf("status %+v", status)
		}
		if len(status.Errors) != 0 {
			t.Fatalf("errors %v", status.Errors)
		}
	}
	csvList := namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "")
	if a.count(csvList) != 0 {
		t.Fatalf("the CSV list must not be read when the Subscription names the CSV")
	}
	if n := a.count(namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName)); n != 3 {
		t.Fatalf("subscription read %d times for 3 polls, want 3 (no duplicate)", n)
	}
	if n := a.count(namespacedPath("packages.operators.coreos.com/v1", "packagemanifests", CatalogNS, "")); n != 1 {
		t.Fatalf("PackageManifest read %d times for 3 polls, want 1", n)
	}
	if n := a.count("/apis/config.openshift.io/v1/consoles/cluster"); n != 1 {
		t.Fatalf("console read %d times, want 1", n)
	}
}

func TestGetStatus_FreshClusterWithoutSubscription(t *testing.T) {
	t.Setenv("STABLE_SOURCE", "redhat-operators")
	t.Setenv("STABLE_CHANNEL", "")
	installFakeRegistry(t, newFakeRegistry())
	a := &statusAPI{csvList: `{"items":[]}`}
	c := newStatusAPI(t, a)
	status, err := GetStatus(c)
	if err != nil {
		t.Fatal(err)
	}
	if status.Subscription.State != "Not Installed" || status.CSV.Phase != "Not Found" || status.Nightly != nil || len(status.Errors) != 0 {
		t.Fatalf("status %+v", status)
	}
	if q := a.queries[namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "")]; len(q) != 1 || !strings.Contains(q[0], "labelSelector=%21olm.copiedFrom") {
		t.Fatalf("CSV list should exclude copied CSVs, queries %v", q)
	}
}

func TestGetStatus_StableDiscoveryErrorIsNotCached(t *testing.T) {
	t.Setenv("STABLE_SOURCE", "redhat-operators")
	t.Setenv("STABLE_CHANNEL", "")
	installFakeRegistry(t, newFakeRegistry())
	pm := namespacedPath("packages.operators.coreos.com/v1", "packagemanifests", CatalogNS, "")
	a := &statusAPI{csvList: `{"items":[]}`, failPath: map[string]int{pm: 503}}
	c := newStatusAPI(t, a)
	for i := 0; i < 2; i++ {
		status, _ := GetStatus(c)
		if status.StableDiscoveryError == "" {
			t.Fatal("expected discovery error")
		}
	}
	a.mu.Lock()
	delete(a.failPath, pm)
	a.mu.Unlock()
	status, _ := GetStatus(c)
	if status.StableChannel != "stable-3.5" || a.count(pm) != 3 {
		t.Fatalf("errors must be retried on the next poll: channel %q reads %d", status.StableChannel, a.count(pm))
	}
}

func TestCSVForInstalledName(t *testing.T) {
	csvPath := namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "")
	cases := []struct {
		name      string
		installed string
		byName    map[string]string
		list      string
		failName  int
		want      types.CSVInfo
		wantErr   string
	}{
		{name: "by name", installed: "rhods-operator.3.6.0", byName: map[string]string{"rhods-operator.3.6.0": csv360}, want: types.CSVInfo{Name: "rhods-operator.3.6.0", Version: "3.6.0", Phase: "Succeeded"}},
		{name: "missing phase is Unknown", installed: "rhods-operator.3.6.0", byName: map[string]string{"rhods-operator.3.6.0": `{"metadata":{"name":"rhods-operator.3.6.0"},"spec":{"version":"3.6.0"}}`}, want: types.CSVInfo{Name: "rhods-operator.3.6.0", Version: "3.6.0", Phase: "Unknown"}},
		{name: "stale reference with surviving CSV", installed: "rhods-operator.3.5.0", list: `{"items":[` + csv360 + `]}`, wantErr: "subscription references missing CSV"},
		{name: "stale reference, nothing left", installed: "rhods-operator.3.5.0", list: `{"items":[]}`, want: types.CSVInfo{Phase: "Not Found"}},
		{name: "no installed name uses the list", list: `{"items":[` + csv360 + `]}`, want: types.CSVInfo{Name: "rhods-operator.3.6.0", Version: "3.6.0", Phase: "Succeeded"}},
		{name: "read error is not treated as missing", installed: "rhods-operator.3.6.0", failName: 503, wantErr: "request failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &statusAPI{csvByNm: tc.byName, csvList: tc.list, failPath: map[string]int{}}
			if tc.failName != 0 {
				a.failPath[csvPath+"/"+tc.installed] = tc.failName
			}
			c := newStatusAPI(t, a)
			got, err := csvForInstalledName(c, tc.installed)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %+v err %v, want %+v", got, err, tc.want)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

func TestGetNightlyStatus(t *testing.T) {
	nightlySub := types.SubscriptionInfo{Source: CatalogName}
	installedImg := quayImage + ":rhoai-3.6@" + digestOf(1)
	cases := []struct {
		name       string
		sub        types.SubscriptionInfo
		cs         types.CatalogSourceInfo
		tagDigest  string // current digest of rhoai-3.6 in Quay; "" = HEAD 404
		wantNil    bool
		wantUpdate *bool
		wantError  bool
	}{
		{name: "not on the nightly catalog", sub: types.SubscriptionInfo{Source: "redhat-operators"}, cs: types.CatalogSourceInfo{Exists: true, Image: installedImg}, wantNil: true},
		{name: "no catalog source", sub: nightlySub, wantNil: true},
		{name: "up to date", sub: nightlySub, cs: types.CatalogSourceInfo{Exists: true, Image: installedImg}, tagDigest: digestOf(1), wantUpdate: boolPtr(false)},
		{name: "update available", sub: nightlySub, cs: types.CatalogSourceInfo{Exists: true, Image: installedImg}, tagDigest: digestOf(2), wantUpdate: boolPtr(true)},
		{name: "tag-only install: no comparison", sub: nightlySub, cs: types.CatalogSourceInfo{Exists: true, Image: quayImage + ":rhoai-3.6"}, tagDigest: digestOf(2)},
		{name: "quay lookup fails", sub: nightlySub, cs: types.CatalogSourceInfo{Exists: true, Image: installedImg}, wantError: true},
		{name: "custom catalog image", sub: nightlySub, cs: types.CatalogSourceInfo{Exists: true, Image: "quay.io/rhoai/rhoai-fbc-fragment@" + digestOf(3)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeRegistry()
			if tc.tagDigest != "" {
				f.tagDigests["rhoai-3.6"] = tc.tagDigest
			}
			f.labels[digestOf(1)] = map[string]string{"build-date": "2026-10-01"}
			f.labels[digestOf(2)] = map[string]string{"build-date": "2026-10-05"}
			installFakeRegistry(t, f)
			got := getNightlyStatus(context.Background(), "auth", tc.sub, tc.cs)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("want nil, got %+v", got)
				}
				return
			}
			if got == nil || got.Installed == nil || got.Installed.Image != tc.cs.Image {
				t.Fatalf("installed missing: %+v", got)
			}
			if (got.UpdateAvailable == nil) != (tc.wantUpdate == nil) || (tc.wantUpdate != nil && *got.UpdateAvailable != *tc.wantUpdate) {
				t.Fatalf("updateAvailable %v, want %v (%+v)", got.UpdateAvailable, tc.wantUpdate, got)
			}
			if tc.wantError != (got.Error != "") {
				t.Fatalf("error %q", got.Error)
			}
			if tc.wantUpdate != nil {
				if got.Latest == nil || got.Latest.Digest != tc.tagDigest || got.Latest.Tag != "rhoai-3.6" || got.CheckedAt == "" {
					t.Fatalf("latest %+v", got.Latest)
				}
				if got.Installed.BuildDate != "2026-10-01" || got.Installed.Digest != digestOf(1) {
					t.Fatalf("installed %+v", got.Installed)
				}
			}
		})
	}
}

func TestGetNightlyStatus_CachedPerInstalledImage(t *testing.T) {
	f := newFakeRegistry()
	f.tagDigests["rhoai-3.6"] = digestOf(2)
	installFakeRegistry(t, f)
	sub := types.SubscriptionInfo{Source: CatalogName}
	old := types.CatalogSourceInfo{Exists: true, Image: quayImage + ":rhoai-3.6@" + digestOf(1)}
	for i := 0; i < 5; i++ {
		if got := getNightlyStatus(context.Background(), "a", sub, old); got.UpdateAvailable == nil || !*got.UpdateAvailable {
			t.Fatalf("poll %d: %+v", i, got)
		}
	}
	if n := f.count("quay-head"); n != 1 {
		t.Fatalf("5 polls made %d Quay HEADs, want 1", n)
	}
	// After an update the CatalogSource points at the new digest: the very
	// next poll must reflect it, not the cached "update available".
	updated := types.CatalogSourceInfo{Exists: true, Image: quayImage + ":rhoai-3.6@" + digestOf(2)}
	if got := getNightlyStatus(context.Background(), "a", sub, updated); got.UpdateAvailable == nil || *got.UpdateAvailable {
		t.Fatalf("after update: %+v", got)
	}
	if n := f.count("quay-head"); n != 2 {
		t.Fatalf("a new installed image must be checked against Quay, HEADs %d", n)
	}
}

func TestGetNightlyStatus_StaleEntryRefreshesInBackground(t *testing.T) {
	f := newFakeRegistry()
	f.tagDigests["rhoai-3.6"] = digestOf(1)
	installFakeRegistry(t, f)
	sub := types.SubscriptionInfo{Source: CatalogName}
	cs := types.CatalogSourceInfo{Exists: true, Image: quayImage + ":rhoai-3.6@" + digestOf(1)}
	getNightlyStatus(context.Background(), "a", sub, cs)
	nightlyBackground.Wait()

	// A new nightly is pushed, and the cached comparison ages past freshness.
	f.mu.Lock()
	f.tagDigests["rhoai-3.6"] = digestOf(9)
	f.mu.Unlock()
	nightlyBackground.Wait()
	previous := nightlyStatusFresh
	nightlyStatusFresh = 0
	t.Cleanup(func() { nightlyBackground.Wait(); nightlyStatusFresh = previous })

	if got := getNightlyStatus(context.Background(), "a", sub, cs); *got.UpdateAvailable {
		t.Fatal("the stale value is served while refreshing")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := getNightlyStatus(context.Background(), "a", sub, cs)
		if got.UpdateAvailable != nil && *got.UpdateAvailable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background refresh did not pick up the new build")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDashboardCommitIsFilledFromCachedCatalog(t *testing.T) {
	f := newFakeRegistry()
	installed := digestOf(1)
	f.tagDigests["rhoai-3.6"] = installed
	dashDigest := "sha256:" + strings.Repeat("d", 64)
	f.fbcLayers[installed] = gzipTar(t, map[string][]byte{
		"configs/rhods-operator/catalog.json": []byte(fbcPackage + "\n" + fmt.Sprintf(
			`{"schema":"olm.bundle","name":"rhods-operator.3.6.0","package":"rhods-operator","relatedImages":[{"name":"odh_dashboard_image","image":"registry.redhat.io/rhoai/odh-dashboard-rhel9@%s"}]}`, dashDigest)),
	})
	f.labels[dashDigest] = map[string]string{"vcs-ref": "96088eb", "git.url": "https://github.com/red-hat-data-services/odh-dashboard"}
	installFakeRegistry(t, f)
	sub := types.SubscriptionInfo{Source: CatalogName}
	cs := types.CatalogSourceInfo{Exists: true, Image: quayImage + ":rhoai-3.6@" + installed}
	getNightlyStatus(context.Background(), "a", sub, cs)
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := getNightlyStatus(context.Background(), "a", sub, cs)
		if got.Installed.DashboardCommit == "96088eb" && got.Latest.DashboardCommit == "96088eb" {
			if got.Installed.DashboardGitURL == "" {
				t.Fatal("missing git URL")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("dashboard commit never filled: %+v", got.Installed)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
