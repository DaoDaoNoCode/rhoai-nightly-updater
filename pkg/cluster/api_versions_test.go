package cluster

import (
	"errors"
	"reflect"
	"sort"
	"testing"
)

// The API server's answers on 2026-10-07, RHOAI 3.6 nightly whose operator
// image was older than its CRDs: a list without limit (watch cache) and a
// list with ?limit=1 (read from etcd).
const (
	liveDSCv3Conversion429 = `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"storage is (re)initializing: failed to list datasciencecluster.opendatahub.io/v3, Kind=DataScienceCluster: conversion webhook for datasciencecluster.opendatahub.io/v2, Kind=DataScienceCluster failed: no kind \"DataScienceCluster\" is registered for version \"datasciencecluster.opendatahub.io/v3\" in scheme \"pkg/runtime/scheme.go:111\"","reason":"TooManyRequests","details":{"retryAfterSeconds":30},"code":429}`
	liveDSCv3Conversion500 = `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"conversion webhook for datasciencecluster.opendatahub.io/v2, Kind=DataScienceCluster failed: no kind \"DataScienceCluster\" is registered for version \"datasciencecluster.opendatahub.io/v3\" in scheme \"pkg/runtime/scheme.go:111\"","code":500}`
	livePlatformConversion = `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"conversion webhook for config.opendatahub.io/v1alpha1, Kind=Platform failed: no kind \"Platform\" is registered for version \"config.opendatahub.io/v1alpha2\" in scheme \"pkg/runtime/scheme.go:111\"","code":500}`
	liveDSCv3Message       = `storage is (re)initializing: failed to list datasciencecluster.opendatahub.io/v3, Kind=DataScienceCluster: conversion webhook for datasciencecluster.opendatahub.io/v2, Kind=DataScienceCluster failed: no kind "DataScienceCluster" is registered for version "datasciencecluster.opendatahub.io/v3" in scheme "pkg/runtime/scheme.go:111"`
)

// dscGroupDoc is the live discovery document of the DSC group: v3 preferred.
const dscGroupDoc = `{"kind":"APIGroup","apiVersion":"v1","name":"datasciencecluster.opendatahub.io","versions":[{"groupVersion":"datasciencecluster.opendatahub.io/v3","version":"v3"},{"groupVersion":"datasciencecluster.opendatahub.io/v2","version":"v2"}],"preferredVersion":{"groupVersion":"datasciencecluster.opendatahub.io/v3","version":"v3"}}`

func TestCompareAPIVersionsOrdersLikeKubernetes(t *testing.T) {
	got := []string{"v1alpha1", "foo", "v1", "v2", "v1beta1", "v11alpha2", "v3", "v1alpha2", "v10", "v3beta1", "v1beta2", "bar"}
	sortAPIVersions(got)
	want := []string{"v10", "v3", "v2", "v1", "v3beta1", "v1beta2", "v1beta1", "v11alpha2", "v1alpha2", "v1alpha1", "bar", "foo"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestServedVersionsPreferredFirstThenNewest(t *testing.T) {
	apiVersionsCache.Purge()
	t.Cleanup(apiVersionsCache.Purge)
	f, c := newFakeAPI(t)
	f.json("GET", "/apis/datasciencecluster.opendatahub.io", 200,
		`{"name":"datasciencecluster.opendatahub.io","versions":[{"version":"v1"},{"version":"v2"},{"version":"v3"}],"preferredVersion":{"version":"v2"}}`)
	f.json("GET", "/apis/config.opendatahub.io", 200,
		`{"name":"config.opendatahub.io","versions":[{"version":"v1alpha1"},{"version":"v1alpha2"}],"preferredVersion":{"version":"v1alpha2"}}`)
	if got := servedVersions(c, dscGroup, dscFallbackVersions); !got.Discovered || !reflect.DeepEqual(got.Versions, []string{"v2", "v3", "v1"}) {
		t.Fatalf("dsc %+v", got)
	}
	if got := servedVersions(c, platformGroup, platformFallbackVersions); !reflect.DeepEqual(got.Versions, []string{"v1alpha2", "v1alpha1"}) {
		t.Fatalf("platform %+v", got)
	}
	// Cached: no second discovery read.
	servedVersions(c, dscGroup, dscFallbackVersions)
	if n := len(f.requests("GET", "/apis/datasciencecluster.opendatahub.io")); n != 1 {
		t.Fatalf("discovery reads: %d", n)
	}
}

func TestServedVersionsFallsBackWithoutDiscovery(t *testing.T) {
	apiVersionsCache.Purge()
	t.Cleanup(apiVersionsCache.Purge)
	f, c := newFakeAPI(t)
	// Group not served (no CRD yet): the old order, not cached.
	if got := servedVersions(c, dscGroup, dscFallbackVersions); got.Discovered || !reflect.DeepEqual(got.Versions, []string{"v2", "v1"}) {
		t.Fatalf("absent group %+v", got)
	}
	// Discovery failing for another reason: the old order too.
	f.status("GET", "/apis/datasciencecluster.opendatahub.io", 503, "ServiceUnavailable")
	if got := servedVersions(c, dscGroup, dscFallbackVersions); got.Discovered || !reflect.DeepEqual(got.Versions, []string{"v2", "v1"}) {
		t.Fatalf("failed discovery %+v", got)
	}
	// Once the group appears it is used right away.
	f.json("GET", "/apis/datasciencecluster.opendatahub.io", 200, dscGroupDoc)
	if got := servedVersions(c, dscGroup, dscFallbackVersions); !got.Discovered || !reflect.DeepEqual(got.Versions, []string{"v3", "v2"}) {
		t.Fatalf("after install %+v", got)
	}
}

func TestIsConversionWebhookError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"429 storage reinitializing (live)", parseK8sError([]byte(liveDSCv3Conversion429), 429), true},
		{"500 conversion webhook failed (live)", parseK8sError([]byte(liveDSCv3Conversion500), 500), true},
		{"429 throttled by priority and fairness", parseK8sError([]byte(`{"kind":"Status","status":"Failure","message":"Too many requests, please try again later.","reason":"TooManyRequests","code":429}`), 429), false},
		{"429 without a body", &K8sError{Status: 429}, false},
		{"500 other", parseK8sError([]byte(`{"kind":"Status","status":"Failure","message":"etcdserver: request timed out","code":500}`), 500), false},
		{"404", parseK8sError([]byte(k8sNotFound), 404), false},
		{"403 mentioning a conversion webhook", &K8sError{Status: 403, K8sMessage: "conversion webhook"}, false},
		{"network error", &NetworkError{Err: errors.New("conversion webhook")}, false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isConversionWebhookError(tc.err); got != tc.want {
				t.Fatalf("got %v", got)
			}
		})
	}
	if msg := conversionMessage(parseK8sError([]byte(liveDSCv3Conversion429), 429)); msg != liveDSCv3Message {
		t.Fatalf("message %q", msg)
	}
}

func TestListServedFallsBackOnlyOnConversionErrors(t *testing.T) {
	const items = `{"items":[{"apiVersion":"datasciencecluster.opendatahub.io/v2","metadata":{"name":"default-dsc"}}]}`
	for _, tc := range []struct {
		name        string
		v3          func(f *fakeAPI)
		wantVersion string
		wantSkipped []string
		wantErr     int
	}{
		{"v3 works", func(f *fakeAPI) {
			f.json("GET", "/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters", 200, `{"items":[]}`)
		}, "v3", nil, 0},
		{"v3 429 conversion: v2", func(f *fakeAPI) {
			f.json("GET", "/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters", 429, liveDSCv3Conversion429)
		}, "v2", []string{"v3"}, 0},
		{"v3 500 conversion: v2", func(f *fakeAPI) {
			f.json("GET", "/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters", 500, liveDSCv3Conversion500)
		}, "v2", []string{"v3"}, 0},
		{"v3 forbidden is returned", func(f *fakeAPI) {
			f.status("GET", "/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters", 403, "Forbidden")
		}, "", nil, 403},
		{"v3 throttled is returned", func(f *fakeAPI) {
			f.status("GET", "/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters", 429, "TooManyRequests")
		}, "", nil, 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			apiVersionsCache.Purge()
			t.Cleanup(apiVersionsCache.Purge)
			f, c := newFakeAPI(t)
			f.json("GET", "/apis/datasciencecluster.opendatahub.io", 200, dscGroupDoc)
			f.json("GET", "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters", 200, items)
			tc.v3(f)
			r, err := listServed(c, dscGroup, dscListFmt, nil, dscFallbackVersions)
			if tc.wantErr != 0 {
				if !IsK8sError(err, tc.wantErr) || len(f.requests("GET", "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters")) != 0 {
					t.Fatalf("want %d without falling back, got %+v %v", tc.wantErr, r, err)
				}
				return
			}
			if err != nil || r.Version != tc.wantVersion {
				t.Fatalf("got %+v %v", r, err)
			}
			var skipped []string
			for _, s := range r.Skipped {
				skipped = append(skipped, s.Version)
			}
			if !reflect.DeepEqual(skipped, tc.wantSkipped) {
				t.Fatalf("skipped %v", r.Skipped)
			}
		})
	}

	t.Run("every version fails to convert: the first error", func(t *testing.T) {
		apiVersionsCache.Purge()
		f, c := newFakeAPI(t)
		f.json("GET", "/apis/datasciencecluster.opendatahub.io", 200, dscGroupDoc)
		f.json("GET", "/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters", 429, liveDSCv3Conversion429)
		f.json("GET", "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters", 500, liveDSCv3Conversion500)
		r, err := listServed(c, dscGroup, dscListFmt, nil, dscFallbackVersions)
		if !IsK8sError(err, 429) || len(r.Skipped) != 2 {
			t.Fatalf("got %+v %v", r, err)
		}
	})
	t.Run("no version served: no API, no error", func(t *testing.T) {
		apiVersionsCache.Purge()
		_, c := newFakeAPI(t)
		r, err := listServed(c, dscGroup, dscListFmt, nil, dscFallbackVersions)
		if err != nil || r.Version != "" {
			t.Fatalf("got %+v %v", r, err)
		}
	})
	t.Run("named object missing at a served version is a 404", func(t *testing.T) {
		apiVersionsCache.Purge()
		f, c := newFakeAPI(t)
		f.json("GET", "/apis/datasciencecluster.opendatahub.io", 200, dscGroupDoc)
		_, err := getServed(c, dscGroup, dscListFmt+"/gone", dscFallbackVersions)
		if !IsK8sError(err, 404) || len(f.requests("GET", "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/gone")) != 0 {
			t.Fatalf("err %v", err)
		}
	})
}

func TestGroupDocOrderedVersions(t *testing.T) {
	g := apiGroupDoc{PreferredVersion: struct {
		Version string `json:"version"`
	}{Version: "v1beta1"}}
	for _, v := range []string{"v1alpha1", "v1", "v1beta1", "v2"} {
		g.Versions = append(g.Versions, struct {
			Version string `json:"version"`
		}{v})
	}
	got := g.orderedVersions()
	if !reflect.DeepEqual(got, []string{"v1beta1", "v2", "v1", "v1alpha1"}) {
		t.Fatal(got)
	}
	rest := append([]string{}, got[1:]...)
	if !sort.SliceIsSorted(rest, func(i, j int) bool { return compareAPIVersions(rest[i], rest[j]) < 0 }) {
		t.Fatal("rest not newest first")
	}
}
