package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// githubAnchor is the anchor GitHub gives a Markdown heading: lower case,
// punctuation other than "-" dropped, spaces as "-".
func githubAnchor(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return b.String()
}

// The gitops profile's texts link the published GitOps guide, and each
// section they link is a heading of docs/GITOPS.md.
func TestGitOpsTextsLinkThePublishedGuide(t *testing.T) {
	raw, err := os.ReadFile("../../docs/GITOPS.md")
	if err != nil {
		t.Fatal(err)
	}
	anchors := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^#{2,4} (.+)$`).FindAllStringSubmatch(string(raw), -1) {
		anchors[githubAnchor(m[1])] = true
	}
	// Every anchor this tool links (backend and frontend).
	for _, a := range []string{"6-upgrading-and-rolling-back", "10-moving-an-install-from-installsh-to-argo-cd", "11-profiles", "the-admission-policy"} {
		if !anchors[a] {
			t.Errorf("docs/GITOPS.md has no heading for #%s", a)
		}
	}
	if !strings.HasPrefix(types.GitOpsDocURL, "https://github.com/") || !strings.HasSuffix(types.GitOpsDocURL, "/blob/main/docs/GITOPS.md") {
		t.Fatalf("GitOpsDocURL %q", types.GitOpsDocURL)
	}

	rec := httptest.NewRecorder()
	refusedInGitOpsProfile("Dashboard Dev")(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/x", nil))
	if !strings.Contains(rec.Body.String(), types.GitOpsDocURL+"#11-profiles") || strings.Contains(rec.Body.String(), "docs/GITOPS.md)") {
		t.Fatalf("403: %s", rec.Body.String())
	}
}
