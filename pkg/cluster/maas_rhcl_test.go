package cluster

import (
	"strings"
	"testing"
)

// The RHCL catalog only upgrades to 1.5.0 from 1.4.4 (stable-1.5 entry
// skipRange ">=1.4.4 <1.5.0"; 1.4.4 is only in stable-1.4), so a switch to
// stable-1.5 from 1.4.3 or earlier leaves OLM at AtLatestKnown. The 1.4.4
// step recreates Authorino's Service, which drops its serving certificate.
func TestRHCLWarning(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    []string
		not     []string
	}{
		{version: "", want: nil},
		{version: "1.5.0", want: nil},
		{version: "1.6.1", want: nil},
		{version: "1.4.3", want: []string{"1.4.3", "stable-1.4", "then", "stable-1.5", "Authorino TLS"}},
		{version: "1.4.0", want: []string{"stable-1.4", "stable-1.5"}},
		{version: "1.4.4", want: []string{"1.4.4", "stable-1.5"}, not: []string{"stable-1.4"}},
	} {
		got := rhclWarning(tc.version)
		if tc.want == nil {
			if got != "" {
				t.Errorf("rhclWarning(%q) = %q, want none", tc.version, got)
			}
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("rhclWarning(%q) = %q, want it to mention %q", tc.version, got, w)
			}
		}
		for _, n := range tc.not {
			if strings.Contains(got, n) {
				t.Errorf("rhclWarning(%q) = %q, should not mention %q", tc.version, got, n)
			}
		}
	}
}
