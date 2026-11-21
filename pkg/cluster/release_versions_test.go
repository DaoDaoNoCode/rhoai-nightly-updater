package cluster

import (
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/catalog"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
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
			if !registry.CleanTagRegex.MatchString(tc.tag) {
				t.Fatal("release tag excluded")
			}
			tag, ok := registry.ParseTag(tc.tag)
			if !ok || tag.EA != tc.wantEA {
				t.Fatalf("tag=%+v, ok=%v", tag, ok)
			}
			csv, ok := registry.ParseCSVVersion(tc.csv)
			if !ok || csv.EA != tc.wantEA || csv.Major != tag.Major || csv.Minor != tag.Minor {
				t.Fatalf("CSV=%+v, ok=%v", csv, ok)
			}
		})
	}
	ea, _ := registry.ParseTag("rhoai-3.7-ea")
	ga, _ := registry.ParseTag("rhoai-3.7")
	if registry.CompareTags(ea, ga) >= 0 {
		t.Fatal("GA must rank after EA")
	}
	channels := []any{
		map[string]any{"name": "fast", "currentCSV": "rhods-operator.v3.7.0-ea"},
		map[string]any{"name": "beta", "currentCSV": "rhods-operator.v3.7.0-ea"},
		map[string]any{"name": "stable-3.6", "currentCSV": "rhods-operator.v3.6.0"},
	}
	channel, err := catalog.BestChannel(channels, "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.7-ea")
	if err != nil || channel != "beta" {
		t.Fatalf("unnumbered EA channel=%q, error=%v", channel, err)
	}
	bundles := []registry.FBCBundle{
		{Name: "rhods-operator.v3.7.0-ea"},
		{Name: "rhods-operator.v3.7.0"},
		{Name: "rhods-operator.v3.7.9"},
		{Name: "rhods-operator.v3.7.10"},
		{Name: "rhods-operator.v3.70.0-ea"},
	}
	if bundle := registry.FindMatchingBundle(bundles, "rhoai-3.7-ea"); bundle == nil || bundle.Name != "rhods-operator.v3.7.0-ea" {
		t.Fatalf("EA bundle=%+v", bundle)
	}
	if bundle := registry.FindMatchingBundle(bundles, "rhoai-3.7"); bundle == nil || bundle.Name != "rhods-operator.v3.7.10" {
		t.Fatalf("GA bundle=%+v", bundle)
	}
}

func resetReleaseTagCache(t *testing.T) {
	t.Helper()
	reset := registry.ResetTagScanForTest
	reset()
	t.Cleanup(reset)
}
