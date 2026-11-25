package types

// PublicRepoURL is this project's public GitHub mirror, which hosts its
// releases and its docs (the canonical GitLab project is not public).
const PublicRepoURL = "https://github.com/DaoDaoNoCode/rhoai-nightly-updater"

// GitOpsDocURL is docs/GITOPS.md on the mirror's main branch, which moves
// only at releases: what an operator of the Helm chart follows. Its section
// anchors are GitHub's ("#11-profiles", "#the-admission-policy").
const GitOpsDocURL = PublicRepoURL + "/blob/main/docs/GITOPS.md"
