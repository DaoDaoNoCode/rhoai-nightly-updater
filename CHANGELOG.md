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

## [2.2.0] - unreleased

### Added

- A Helm chart, `deploy/chart`, so Argo CD can deploy the updater from a
  GitOps repository at a release tag ([docs/GITOPS.md](docs/GITOPS.md)). It
  renders `deploy/template.yaml` itself (a copy in the chart, checked by
  `make check-chart`, CI and the release guard), so the template stays the
  only definition of the app's objects. Hooks replace `install.sh`'s
  imperative steps: a Sync hook Job creates the cookie Secret once and never
  replaces it, a PostSync hook Job adds the ConsoleLink. Every image is
  pinned by digest in Git (the updater, oauth-proxy and the hook image); an
  image without a digest, an unknown or misspelled value, or an image tag
  that names another release than the chart's fails the render. That last
  check compares the tag only: docs/GITOPS.md says how to check the digest's
  commit.
- Template parameter `INSTALL_METHOD` (empty by default; the chart sets
  `gitops`), passed to the app: on a GitOps install the "vX.Y.Z is
  available" and "deployment is out of date" notices and the help dialog say
  to change the chart's `targetRevision` and image in Git instead of running
  `install.sh`. `GET /api/version` reports it as `installMethod`. Older
  templates lack it and keep the `install.sh` wording, so
  `TEMPLATE_REVISION` stays 4.
- `make chart`, `make check-chart` and `make test-chart`
  (`scripts/test-chart.sh`), and a `chart` job in GitLab CI and GitHub
  Actions with Helm v3.22.1, checked by SHA-256.
- The release guard also checks that `deploy/chart/Chart.yaml` names the
  release (`version` X.Y.Z, `appVersion` vX.Y.Z), and the release notes name
  the chart.
- Profiles ([docs/GITOPS.md, section 11](docs/GITOPS.md#11-profiles)):
  setting `PROFILE` (Template parameter, empty = `full`) picks what the
  updater may do; any other value than `full` or `gitops` stops it at
  startup, and `GET /api/version` reports it as `profile`. `full` is every
  feature, unchanged. `gitops`, for clusters where Argo CD manages RHOAI,
  allows only Update (its Argo CD path: install the build the Git-managed
  catalog serves; refused unless Argo CD manages CatalogSource
  `rhoai-catalog-dev`), its dry run, and the Status, Components and
  Diagnostics reads, which then need cluster-admin like every change. Every
  other route answers 403 `profile_gitops` before any API call; the UI hides
  those pages and actions and says why. In that profile Update never creates
  the operator namespace or OperatorGroup and never ends a Dashboard Dev
  session; it refuses instead.
- The chart's `profile` value, `gitops` by default: it renders the
  Template's objects without its RBAC plus `deploy/rbac-gitops.yaml`, the
  RBAC the gitops profile reaches and nothing more (cluster-wide reads only;
  no Secret, Deployment, namespace, webhook, CatalogSource or OperatorGroup
  writes), and a ValidatingAdmissionPolicy that lets the updater's
  ServiceAccount write only Subscription `rhods-operator` from
  `rhoai-catalog-dev` with at most the environment variables of
  `gitops.subscriptionEnvAllowlist` (default `DISABLE_DSC_CONFIG`) in
  `spec.config`, approve or delete only rhods-operator InstallPlans, delete
  only rhods-operator CSVs, and no CatalogSource or OperatorGroup. In that
  profile Update reads the build from the catalog Git manages instead of a
  temporary verification catalog, checks before deleting anything that the
  policy allows the Subscription it creates and the one it would restore,
  and runs only while the policy and its binding are exactly this
  release's, rendered with the pod's settings (checked at startup, every
  minute, before each Update and before its first deletion; 503
  `admission_policy_missing` otherwise). The gitops profile needs OpenShift
  4.17 or later. `profile: full` renders as before. Tests derive the RBAC
  from a call graph of the gitops routes, prove no catalog write is
  reachable, and evaluate the policy's CEL with cel-go (a new test-only
  dependency, `github.com/google/cel-go`) on shared cases next to the Go
  rule. `TEMPLATE_REVISION` stays 4: the full profile's template RBAC is
  unchanged, the new Template parameter `SUBSCRIPTION_ENV_ALLOWLIST` and
  variable `GITOPS_ADMISSION_POLICY` are read only in the gitops profile,
  and an install without `PROFILE` runs the full profile.
- A nightly catalog that an ACM policy enforces counts as managed, as one
  Argo CD tracks does: `config-policy-controller` owns its `f:spec` in
  `managedFields` (read from the same GET; no new RBAC). Both profiles leave
  it as the policy sets it and install the build it serves; the gitops
  profile runs on it; messages and the Status page name the policy.
  `GET /api/status` reports `catalogSource.managedByACM`.

### Changed

- The gitops profile never contacts Quay, another registry or GitHub, and
  reads no Secret: every page's build information comes from the cluster
  ([docs/GITOPS.md](docs/GITOPS.md#no-registry-access)). The dry run's
  version check uses the head the catalog serves (PackageManifest); the
  Status page's *Latest* row says a newer nightly arrives as a change in
  Git (no Quay lookup, no 401); Components shows the operator build from
  the installed CSV and no image labels, and takes DSC defaults from the
  CSV's `alm-examples` only; Diagnostics has no pull-secret check; the
  updater's own release check is off (`pinnedInGit`; the chart pins it in
  Git). Update and its dry run no longer read
  `kube-system/additional-pull-secret`: they say the cluster's own pull
  secrets are used, and the Status page shows the secret as *Not used*.
  `deploy/rbac-gitops.yaml` renders its Role in `kube-system` with no rules,
  under the name `install.sh` used, so moving an install to Argo CD removes
  `install.sh`'s Secret grant; Diagnostics reports (with
  SelfSubjectAccessReviews) when the ServiceAccount can still read Secrets. A test on the
  profile's reference graph fails when anything it reaches sends a request
  to another host than the API server. The full profile is unchanged.

### Fixed

- Recovery after a failed Update, Reinstall or Re-deploy deleted whatever
  operator Subscription it found. It now deletes only the one the operation
  applied (its UID, from the apply's answer), and leaves a replacement, and
  the CSVs installed for it, alone and reports it.
- Update's managed path (Argo CD or an ACM policy sets the catalog, both
  profiles) read the head to check from the catalog's PackageManifest,
  which can still describe the previous catalog right after Git changes the
  image. After a rollback in Git, the downgrade check could pass and OLM
  install the older build. The recreated Subscription now pins
  `startingCSV` to the checked head, and the restore pins the removed CSV,
  so OLM installs exactly those or nothing (`resolution_failed`).
- Update under Argo CD: when someone replaced the operator Subscription
  just before Update deleted it, Update left the replacement alone, but its
  automatic restore then deleted the replacement and re-applied the old
  Subscription. The restore now leaves it alone too and reports it.

## [2.1.0] - 2026-10-14

### Added

- Clusters where Argo CD (OpenShift GitOps) manages RHOAI. The tool notices
  Argo CD's tracking annotation on the nightly CatalogSource, the operator
  Subscription and the DataScienceCluster, and works with Git as the source
  of truth (RUNBOOK §13; no change to the Argo CD Application is needed):
  - Git picks the build: the catalog image stays as Git sets it. The Status
    page shows the build Git set, whether the operator runs it (**Install
    pending** when Git moved to a build of the same version, which OLM does
    not install by itself), and a newer nightly in Quay as the image to put
    in Git.
  - Update becomes *Install the build the catalog serves*: it keeps the
    channel Git sets, deletes the old CSV, and deletes the Subscription only
    right before recreating it with Argo CD's tracking and labels, so Argo
    CD does not recreate it from Git in between (a self-heal sync that
    follows finds it unchanged). Any other image is refused before anything
    changes.
  - Re-deploy, Reinstall and *Create the DataScienceCluster* are off while
    Argo CD manages the operator, and DSC repairs and Diagnostics' *disable
    component* while it manages the DSC: Argo CD would undo them.
    Diagnostics' prerequisite-operator guidance says to add the operator in
    Git instead.
  - Update checks, after OLM reports success, that the installed CSV is the
    build it verified (`containerImage` and `createdAt` annotations), and
    reports `install_mismatch` otherwise instead of success.
- `GET /api/status` reports `subscription.managedBy`,
  `catalogSource.managedBy`, `dscManagedBy` and, under Argo CD,
  `catalogServes` (the catalog's head CSV, its build, and whether it is
  installed).

### Security

- Go 1.27.2 (CI, the image build and the GitHub workflows): fixes the
  net/http, HTTP/2, crypto/tls and net/textproto advisories GO-2026-6603,
  6605, 6607 to 6613 and 6617 that govulncheck reports with 1.27.1.

## [2.0.1] - 2026-10-07

### Upgrade notes

- Upgrading from v2.0.0 or any earlier build: download and run the
  `install.sh` of v2.0.1 (commands at the end of these notes; pass the
  options your install was made with, such as `--namespace`, `--app-name`
  and `--image-repo`). From a clone: `git checkout v2.0.1 && make upgrade`.
- Reload open app pages after upgrading: a DSC repair from a page loaded
  before v2.0.1 is refused with a "refresh and review" message.
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
- DataScienceCluster v3 nests parts under a component (`dashboard.standard`,
  `kserve.nim`, `data.featureStore`). "Reset to version defaults" now lists
  each part it would switch to Removed, including the enabled parts of a
  component switched off, and both repairs refuse while the part's module
  could hang in deletion, as for whole components.
- A DSC repair runs only at the API version its preview was computed at; if
  the DSC is read at another version by then (for example after an operator
  update fixed the conversion), it asks to refresh and changes nothing.
  The reset dialog enables Confirm only when its defaults preview matches
  the version and operator the page compared with (otherwise it says the
  DataScienceCluster changed and offers Refresh), and a reset runs only if
  it would switch to Removed exactly the components the dialog listed.
- A part the installed CRD does not define (for example an old
  `dashboard.legacyPart`) is pruned by "Remove invalid fields" without the
  deletion checks of the component's module.

### Added

- Diagnostics "API versions": reports a RHOAI CRD version that the
  operator's conversion webhook cannot convert to (one problem per kind,
  with the API server's message, the versions that work, and read-only
  commands), as when a nightly's operator image is older than the CRDs of
  its bundle. Each served version is checked on its first object; with no
  object it could read, the check warns instead of passing. See
  [RUNBOOK §11.8](RUNBOOK.md#118-the-operator-cannot-convert-a-crd-version).
- The "vX.Y.Z is available" notice shows that release's installer as a
  command to copy (only for a plain https download URL, quoted), with the
  git route as the alternative. For this project
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
