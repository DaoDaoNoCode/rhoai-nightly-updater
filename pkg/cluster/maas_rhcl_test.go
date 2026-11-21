package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
)

// Below the docs' minimum the warning names the in-tool upgrade, never a
// channel (the path is read from the catalog); 1.4.0 says it bypasses
// gateway authentication.
func TestRHCLWarning(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    []string
	}{
		{version: "", want: nil},
		{version: "1.5.0", want: nil},
		{version: "1.6.1", want: nil},
		{version: "1.4.3", want: []string{"1.4.3", "1.5 or later", "Upgrade Connectivity Link", "Authorino TLS"}},
		{version: "1.4.0", want: []string{"bypasses gateway authentication", "publishes nothing", "Upgrade Connectivity Link"}},
		{version: "1.4.4", want: []string{"1.4.4", "Upgrade Connectivity Link"}},
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
		if strings.Contains(got, "stable-") {
			t.Errorf("rhclWarning(%q) = %q names a channel", tc.version, got)
		}
	}
}

// The channels are read from the catalog: the target is the lowest head
// that meets the docs' minimum; the hop lists the installed CSV and has a
// newer head below the target's (the RHCL catalog of 2026-11-18).
func TestRHCLChannelPicking(t *testing.T) {
	pkg := rhclCatalog()
	if got := rhclTargetChannel(pkg); got != "stable-1.5" {
		t.Fatalf("target = %q", got)
	}
	if got := hopChannel(pkg, "rhcl-operator.v1.4.3", "stable-1.5"); got != "stable-1.4" {
		t.Fatalf("hop = %q", got)
	}
	if got := hopChannel(pkg, "rhcl-operator.v1.4.4", "stable-1.5"); got != "" {
		t.Fatalf("hop from 1.4.4 = %q", got)
	}
	if got := rhclTargetChannel(envkit.OperatorPackage{DefaultChannel: "stable", Channels: pkg.Channels[:2]}); got != "" {
		t.Fatalf("target without a 1.5 channel = %q", got)
	}
}
