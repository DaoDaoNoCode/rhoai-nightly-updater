# Changelog

All notable changes to the RHOAI Nightly Updater. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
`vMAJOR.MINOR.PATCH`:

- **MAJOR**: the deploy contract changed (`TEMPLATE_REVISION` in
  `deploy/template.yaml`, RBAC, anything that needs the template re-applied).
  Upgrading needs a full redeploy: the release's `install.sh`, or
  `git checkout vX.Y.Z && make upgrade`.
- **MINOR**: features, same template.
- **PATCH**: fixes.

A section reads `- unreleased` until the release commit replaces it with the
release date (`YYYY-MM-DD`); the release guard (`scripts/release.sh check`)
only accepts a tag whose version is the top dated section. See
[CONTRIBUTING.md](CONTRIBUTING.md#releases).

## [2.0.1] - 2026-10-07

### Upgrade notes

- Upgrading from v2.0.0 or any earlier build: download and run the
  `install.sh` of v2.0.1 (commands at the end of these notes; add
  `--namespace` and `--app-name` if your install does not use the default
  names). From a clone: `git checkout v2.0.1 && make upgrade`.
- Same template as v2.0.0 (revision 4): no RBAC change, no full redeploy.

### Fixed

- RHOAI 3.6 nightlies serve DataScienceCluster v3: the Components page no
  longer fails with "GitHub returned HTTP 404" for
  `datasciencecluster_v1_datasciencecluster.yaml`. The DataScienceCluster,
  DSCInitialization, Platform and Dashboard CR API versions come from API
  discovery (preferred first, then newest first) instead of a fixed v2/v1
  list; DSC defaults accept an alm-examples example of any version, and the
  GitHub fallback tries the sample of each served version.
- A DSC is only compared with (and reset to, or created from) defaults of
  the version it is read at. When the operator's conversion webhook cannot
  convert it to the preferred version, it is read at the next version that
  works, the Components page says so, and a missing defaults version is
  explained instead of showing a sample URL.
- The Components page's "Edit in console" link uses the DSC's version
  instead of v2.

### Added

- Diagnostics "API versions": reports a RHOAI CRD version that the
  operator's conversion webhook cannot convert to (one problem per kind,
  with the API server's message, the versions that work, and read-only
  commands), as when a nightly's operator image is older than the CRDs of
  its bundle. See [RUNBOOK §11.8](RUNBOOK.md#118-the-operator-cannot-convert-a-crd-version).
- The "vX.Y.Z is available" notice shows that release's installer as a
  command to copy, with the git route as the alternative. For this project
  it downloads from the public GitHub mirror, because the GitLab project's
  release assets need a sign-in; the GitLab release notes do the same. See [docs/UPGRADING.md](docs/UPGRADING.md#patch-and-minor-releases).

## [2.0.0] - 2026-10-06

### Upgrade notes

- **Requires a full redeploy** (template revision 4; v1.0.0 has revision 2).
  Use the `install.sh` of v2.0.0, or `git checkout v2.0.0 && make upgrade`.
  Deployments that still pull `:latest` stay on v1.x: `:latest` no longer
  moves to a new major version by itself. See [docs/UPGRADING.md](docs/UPGRADING.md).
- **RBAC changes:** the ClusterRole adds `get` on `operator.openshift.io`
  objects named `cluster` (prerequisite operands), `list` on cert-manager
  `Certificates`, and the SeaweedFS objects (`seaweedfs` Deployment and
  Service, `seaweedfs-pvc`, `seaweedfs-ingress`); it keeps `delete` on the
  MinIO objects only to remove them.
- **S3 test storage moves from MinIO to SeaweedFS** on the next storage setup
  or repair. SeaweedFS starts empty; the old `minio-pvc` is kept until
  teardown. Rollback steps: [RUNBOOK §10.1](RUNBOOK.md#101-minio-to-seaweedfs-what-the-migration-does-and-how-to-roll-back).
- New template parameters `IMAGE_REPOSITORY` and `RELEASES_URL` (the update
  check); `install.sh` and `make` pass the repository they install from.

### Added

- Versioned releases: `vX.Y.Z` tags build an immutable `:vX.Y.Z` image, move
  `:vMAJOR`, and move `:latest` only within its major version (a manual CI job
  promotes it across majors). Pushes to `main` publish `:main` and the commit
  tag, never `:latest`. GitLab and GitHub Releases carry the notes,
  `install.sh` and `deploy/template.yaml`.
- `install.sh`, attached to every release: installs or upgrades exactly that
  release with only `oc` and `curl`, no clone. `make deploy`, `upgrade`,
  `rollback` and `resolve-image` run the same script.
- `make release VERSION=vX.Y.Z`, with the release guard (CHANGELOG entry, and
  a higher MAJOR whenever the template revision changes).
- The masthead shows the running release; a dismissible notice says when a
  newer release exists, and when it needs a full redeploy.
- Diagnostics explain why the DataScienceCluster is not ready (the DSC
  readiness doctor): classified causes per component, linked to the problems
  that fix them; a missing operand CR of a prerequisite operator; stale
  module roll-ups only across scans; Certificates that cert-manager has not
  issued, including a Ready Certificate whose Secret is gone.
- The Components page shows a not-ready component's classified cause.

### Changed

- The S3 test storage is SeaweedFS instead of MinIO, with recovery from
  interrupted setups; its status comes from the serving configuration, and
  an incomplete storage shows its reason as a warning.
- `TAG` defaults to the release tag HEAD is on; without one, `make deploy` and
  `make upgrade` ask for a release checkout or an explicit `TAG=main` /
  `TAG=<commit>`.
- The restart fix resolves `maxSurge` and `maxUnavailable` like the
  Deployment controller.

### Documentation

- A visual guide: README tour, a step-by-step QUICKSTART, UPGRADING (with the
  pre-v1 deployment variants verified live) and RUNBOOK troubleshooting, with
  annotated crops, light and dark screenshots, GIFs and Mermaid diagrams. All
  images come from a mock backend (`make docs-screenshots`), never a real cluster.

### Fixed

- Small UI text and layout issues found while capturing the docs: a missing
  full stop in the remote-operation banner, a sentence starting in lower case
  in the module back-off problem, a repeated tag in the Build Explorer compare
  header, the S3 storage row squeezing its text on mid-size windows, the S3
  storage title naming SeaweedFS while MinIO still runs, and "Not managed by
  this tool" shown for resources that do not exist.

### Security

- Test credentials are built at run time instead of being committed, so leak
  scanners do not flag them.
- SECURITY.md notes that SeaweedFS logs the S3 access key ID at startup.

## [1.0.0] - 2026-10-06

First release (template revision 2): the image is deployed by digest and
checked against the template of its own commit, the oauth-proxy cookie secret
lives in a Secret, RBAC is least-privilege with namespace-suffixed names,
cluster operations hold a cross-pod lock, and the UI covers Status,
Components, Build Explorer, Dashboard Dev, Test resources and Diagnostics.
