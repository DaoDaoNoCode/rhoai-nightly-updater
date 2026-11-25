package types

import "regexp"

// PublicRepoURL is this project's public GitHub mirror, which hosts its
// releases and its docs (the canonical GitLab project is not public).
const PublicRepoURL = "https://github.com/DaoDaoNoCode/rhoai-nightly-updater"

// releaseTag is a version the release jobs tag and publish on the mirror
// (scripts/release.sh: vMAJOR.MINOR.PATCH, pre-releases -alpha.N, -beta.N,
// -rc.N).
var releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+(-(alpha|beta|rc)\.\d+)?$`)

// docsRef is the mirror's ref the docs links point at: the running
// release's tag, else main (a dev build).
var docsRef = "main"

// SetDocsVersion makes DocURL link the docs of version v when it is a
// release tag, else main. The updater calls it once at startup.
func SetDocsVersion(v string) {
	docsRef = "main"
	if releaseTag.MatchString(v) {
		docsRef = v
	}
}

// DocURL is the published page of path (as "docs/GITOPS.md") on the mirror,
// at the running release's tag (main for a dev build), with anchor, GitHub's
// anchor of a heading ("11-profiles"), when not empty.
func DocURL(path, anchor string) string {
	u := PublicRepoURL + "/blob/" + docsRef + "/" + path
	if anchor != "" {
		u += "#" + anchor
	}
	return u
}
