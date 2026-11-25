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
	// Every section this tool links (backend and frontend), by file.
	for file, want := range map[string][]string{
		"docs/GITOPS.md":     {"6-upgrading-and-rolling-back", "10-moving-an-install-from-installsh-to-argo-cd", "11-profiles", "the-admission-policy"},
		"docs/UPGRADING.md":  {"patch-and-minor-releases"},
		"docs/QUICKSTART.md": {"the-image-mirror"},
	} {
		raw, err := os.ReadFile("../../" + file)
		if err != nil {
			t.Fatal(err)
		}
		anchors := map[string]bool{}
		for _, m := range regexp.MustCompile(`(?m)^#{2,4} (.+)$`).FindAllStringSubmatch(string(raw), -1) {
			anchors[githubAnchor(m[1])] = true
		}
		for _, a := range want {
			if !anchors[a] {
				t.Errorf("%s has no heading for #%s", file, a)
			}
		}
	}

	rec := httptest.NewRecorder()
	refusedInGitOpsProfile("Dashboard Dev")(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/x", nil))
	if !strings.Contains(rec.Body.String(), types.DocURL("docs/GITOPS.md", "11-profiles")) || strings.Contains(rec.Body.String(), "(docs/GITOPS.md") {
		t.Fatalf("403: %s", rec.Body.String())
	}
}

// A release links its own tag's docs on the mirror; a dev build, main.
func TestDocURLFollowsTheRunningRelease(t *testing.T) {
	defer types.SetDocsVersion(Version)
	for version, want := range map[string]string{
		"v5.0.0":         types.PublicRepoURL + "/blob/v5.0.0/docs/GITOPS.md#11-profiles",
		"v5.0.0-alpha.2": types.PublicRepoURL + "/blob/v5.0.0-alpha.2/docs/GITOPS.md#11-profiles",
		"dev":            types.PublicRepoURL + "/blob/main/docs/GITOPS.md#11-profiles",
		"v5.0.0-dirty":   types.PublicRepoURL + "/blob/main/docs/GITOPS.md#11-profiles",
		"":               types.PublicRepoURL + "/blob/main/docs/GITOPS.md#11-profiles",
	} {
		types.SetDocsVersion(version)
		if got := types.DocURL("docs/GITOPS.md", "11-profiles"); got != want {
			t.Errorf("%q: %s, want %s", version, got, want)
		}
	}
}
