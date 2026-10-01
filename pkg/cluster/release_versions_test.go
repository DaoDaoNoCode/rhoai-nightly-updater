package cluster

import (
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
