package cluster

import (
	"strings"
	"testing"
)

// A channel name comes from a catalog's PackageManifest, which the cluster
// (not this tool) decides. In the commands a cluster-admin pastes, it is a
// quoted YAML scalar inside the heredoc (a newline cannot end the heredoc)
// and a shell-quoted JSON patch on the oc command line.
func TestMaaSCommands_QuoteTheCatalogChannel(t *testing.T) {
	evil := "stable\nEOF\ntouch /tmp/pwned\ncat <<'EOF'"
	for name, cmd := range map[string]string{
		"rhcl": rhclInstallCommands(evil),
		"cnpg": cnpgInstallCommands(evil),
	} {
		if strings.Contains(cmd, "\ntouch /tmp/pwned") || strings.Count(cmd, "\nEOF") != 1 {
			t.Errorf("%s: the channel breaks out of the heredoc:\n%s", name, cmd)
		}
		if !strings.Contains(cmd, `channel: "stable\nEOF\ntouch /tmp/pwned\ncat `+strings.Repeat(`\`+"u003c", 2)+`'EOF'"`) {
			t.Errorf("%s: channel not a quoted scalar:\n%s", name, cmd)
		}
	}
	if got, want := rhclChannelCommand("kuadrant-system", "kuadrant-operator", "x'$(id)"), `oc patch subscription kuadrant-operator -n kuadrant-system --type=merge -p '{"spec":{"channel":"x'\''$(id)"}}'`; got != want {
		t.Errorf("channel patch\n got %s\nwant %s", got, want)
	}
	if got := rhclChannelCommand("kuadrant-system", "kuadrant-operator", "stable-1.5"); got != `oc patch subscription kuadrant-operator -n kuadrant-system --type=merge -p '{"spec":{"channel":"stable-1.5"}}'` {
		t.Errorf("plain channel: %s", got)
	}
	if got := commentText("# to a\nrm -rf /"); strings.Contains(got, "\n") {
		t.Errorf("comment keeps a newline: %q", got)
	}
}
