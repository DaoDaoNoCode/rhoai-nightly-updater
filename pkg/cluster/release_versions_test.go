package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"testing"
)

func TestUnnumberedEAReleaseTagsAndCSV(t *testing.T) {
	for _, tc := range []struct {
		tag, csv string
		wantEA   int
	}{
		{"rhoai-3.7-ea", "rhods-operator.v3.7.0-ea", 0},
		{"rhoai-3.7", "rhods-operator.v3.7.0", -1},
		{"rhoai-3.6-ea.2", "rhods-operator.v3.6.0-ea.2", 2},
		{"rhoai-4.12.5-ea", "rhods-operator.v4.12.5-ea+build.42", 0},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			if !cleanTagRegex.MatchString(tc.tag) {
				t.Fatal("release tag excluded")
			}
			tag, ok := parseTag(tc.tag)
			if !ok || tag.ea != tc.wantEA {
				t.Fatalf("tag=%+v, ok=%v", tag, ok)
			}
			csv, ok := parseCSVVersion(tc.csv)
			if !ok || csv.ea != tc.wantEA || csv.major != tag.major || csv.minor != tag.minor {
				t.Fatalf("CSV=%+v, ok=%v", csv, ok)
			}
		})
	}
	ea, _ := parseTag("rhoai-3.7-ea")
	ga, _ := parseTag("rhoai-3.7")
	if compareTags(ea, ga) >= 0 {
		t.Fatal("GA must rank after EA")
	}
	channels := []interface{}{
		map[string]interface{}{"name": "fast", "currentCSV": "rhods-operator.v3.7.0-ea"},
		map[string]interface{}{"name": "beta", "currentCSV": "rhods-operator.v3.7.0-ea"},
		map[string]interface{}{"name": "stable-3.6", "currentCSV": "rhods-operator.v3.6.0"},
	}
	channel, err := detectBestChannel(channels, "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.7-ea")
	if err != nil || channel != "beta" {
		t.Fatalf("unnumbered EA channel=%q, error=%v", channel, err)
	}
	bundles := []fbcBundleInfo{
		{name: "rhods-operator.v3.7.0-ea"},
		{name: "rhods-operator.v3.7.0"},
		{name: "rhods-operator.v3.7.9"},
		{name: "rhods-operator.v3.7.10"},
		{name: "rhods-operator.v3.70.0-ea"},
	}
	if bundle := findMatchingBundle(bundles, "rhoai-3.7-ea"); bundle == nil || bundle.name != "rhods-operator.v3.7.0-ea" {
		t.Fatalf("EA bundle=%+v", bundle)
	}
	if bundle := findMatchingBundle(bundles, "rhoai-3.7"); bundle == nil || bundle.name != "rhods-operator.v3.7.10" {
		t.Fatalf("GA bundle=%+v", bundle)
	}
}

func resetReleaseTagCache(t *testing.T) {
	t.Helper()
	reset := func() { tagScanCacheMu.Lock(); tagScanCache = nil; tagScanCacheMu.Unlock() }
	reset()
	t.Cleanup(reset)
}

func TestReleaseDiscoveryPaginatesWithoutVersionCheckpointsOrPageLimit(t *testing.T) {
	resetReleaseTagCache(t)
	calls := 0
	last := "rhoai-"
	client := &http.Client{Transport: dscSampleTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("last") != last {
			t.Fatalf("unexpected pagination cursor: %s", r.URL.Query().Get("last"))
		}
		calls++
		// Include more pages than the old per-scan limit; a registry can cap page size.
		tags := []string{fmt.Sprintf("rhoai-3.6-%03d-build", calls)}
		if calls == 32 {
			tags = []string{"rhoai-3.7", "rhoai-3.7-ea", "rhoai-3.9", "rhoai-8.12-ea", "unrelated-tag"}
		}
		last = tags[len(tags)-1]
		body, _ := json.Marshal(map[string]interface{}{"tags": tags})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}
	tags, err := fetchAndParseTags(context.Background(), client, "test")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tag := range tags {
		got = append(got, tag.raw)
	}
	if !reflect.DeepEqual(got, []string{"rhoai-3.7-ea", "rhoai-3.7", "rhoai-3.9", "rhoai-8.12-ea"}) || calls != 32 {
		t.Fatalf("tags=%v, calls=%d", got, calls)
	}
}
